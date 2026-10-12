// Package dnstoken prowadzi token API Cloudflare dla wydawców Let's Encrypt
// (DNS-01): sprawdzenie w API Cloudflare, zapis w OpenBao i Secret od
// External Secrets Operator (decyzja 43; dawniej ci/set-dns-token.sh
// i Secret zapisywany wprost).
//
// Token nie trafia do gita, historii powłoki, argumentów procesów ani
// zmiennych środowiskowych dzieci: weryfikacja to żądanie HTTPS z tokenem
// w nagłówku (adapters/cloudflare), zapis — żądanie do OpenBao przez
// port-forward rolą, która token może zapisać, ale nie odczytać. W pamięci
// żyje jako sensitive.Value.
package dnstoken

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

// Miejsce tokenu: źródło w OpenBao i Secret od ESO, którego szuka wydawca
// z charta platformy (ExternalSecret o tej samej nazwie).
const (
	SecretName      = "cloudflare-api-token"
	SecretNamespace = "cert-manager"
	SecretKey       = "api-token"
	// VaultPath — ścieżka w KV docfind; WriterAccount i WriterRole — konto
	// w przestrzeni openbao i rola, która ten jeden sekret zapisuje.
	VaultPath     = "platform/cloudflare"
	WriterAccount = "openbao-dns-writer"
	WriterRole    = "dns-token-writer"
)

// DNS — dostawca DNS strefy wyzwań (adapters/cloudflare).
type DNS interface {
	TokenStatus(ctx context.Context, token string) (string, error)
	ZoneVisible(ctx context.Context, token, zone string) (bool, error)
}

// Vault — sekrety w OpenBao (stages/secrets), rolą wąską.
type Vault interface {
	PutSecret(ctx context.Context, account, role, path string, data map[string]sensitive.Value) (int, error)
	SecretVersion(ctx context.Context, account, role, path string) (int, bool, error)
}

// Cluster — Secret od ESO i jego ExternalSecret (adapters/kube).
type Cluster interface {
	SecretData(ctx context.Context, namespace, name string) (map[string][]byte, error)
	ExternalSecret(ctx context.Context, namespace, name string) (cluster.ExternalSecret, error)
	ForceSync(ctx context.Context, namespace, name string, at time.Time) error
}

// ReadToken czyta token z wejścia: jedna linia, bez końcowych białych znaków.
// Z terminala — bez echa (czyta cmd); z potoku (menedżer haseł) — wprost.
func ReadToken(in io.Reader) (string, error) {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return "", cli.Unmet("pusty token")
	}
	return token, nil
}

// Verify sprawdza, że token jest aktywny, a jeśli podano strefę — że ją
// widzi. Zły albo wygasły token wyszedłby inaczej dopiero jako wyzwanie
// DNS-01 wiszące bez końca, z błędem widocznym tylko w statusie Challenge.
func Verify(ctx context.Context, dns DNS, token, zone string) error {
	status, err := dns.TokenStatus(ctx, token)
	if err != nil {
		return err
	}
	if status != "active" {
		return cli.Unmet("Cloudflare nie potwierdził tokenu (status: %q)", status)
	}
	cli.Step("token aktywny")
	if zone == "" {
		return nil
	}
	visible, err := dns.ZoneVisible(ctx, token, zone)
	if err != nil {
		return err
	}
	if !visible {
		return cli.Unmet("token nie widzi strefy %s — brakuje Zone → Zone → Read albo strefa jest spoza zakresu tokenu", zone)
	}
	cli.Step("token widzi strefę %s", zone)
	return nil
}

// Sync — token w OpenBao i jego Secret od ESO.
type Sync struct {
	Vault   Vault
	Cluster Cluster
	// Timeout — na synchronizację ESO; zero — 2 minuty.
	Timeout time.Duration
	// Now — zegar; nil oznacza time.Now.
	Now func() time.Time
}

func (s Sync) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Store zapisuje nową wersję tokenu w OpenBao i czeka, aż ESO przeniesie ją
// do Secreta — wynik `dns-token` to token u wydawców, a nie tylko w OpenBao.
func (s Sync) Store(ctx context.Context, token sensitive.Value) error {
	since := s.now().Truncate(time.Second)
	version, err := s.Vault.PutSecret(ctx, WriterAccount, WriterRole, VaultPath, map[string]sensitive.Value{SecretKey: token})
	if err != nil {
		return err
	}
	cli.Step("token zapisany w OpenBao (%s, wersja %d)", VaultPath, version)
	return s.sync(ctx, since)
}

// Ensure — przy `cluster up` w środowisku z Let's Encrypt: token jest
// w OpenBao i ESO zrobił z niego Secret. Secret sprzed OpenBao (zapisany
// wprost przez dawne `dns-token`) przenosi do OpenBao — wartość idzie
// z Secreta do OpenBao w pamięci, a ESO przejmuje Secret, bo nie ma on
// właściciela. Ta sama droga odtwarza token w OpenBao, gdyby jego dane
// przepadły, a Secret został. hint — jak zapisać token, gdy nie ma go nigdzie.
func (s Sync) Ensure(ctx context.Context, hint string) error {
	_, exists, err := s.Vault.SecretVersion(ctx, WriterAccount, WriterRole, VaultPath)
	if err != nil {
		return err
	}
	since := s.now().Truncate(time.Second)
	if !exists {
		data, err := s.Cluster.SecretData(ctx, SecretNamespace, SecretName)
		var token sensitive.Value
		if err == nil {
			token = sensitive.New(string(data[SecretKey]))
		}
		if token.Empty() {
			return cli.Unmet("brak tokenu Cloudflare w OpenBao (%s) — wydawca Let's Encrypt nie przejdzie wyzwania DNS-01; %s", VaultPath, hint)
		}
		version, err := s.Vault.PutSecret(ctx, WriterAccount, WriterRole, VaultPath, map[string]sensitive.Value{SecretKey: token})
		if err != nil {
			return err
		}
		cli.Step("token Cloudflare przeniesiony z Secreta %s/%s do OpenBao (wersja %d) — Secret przejmuje ESO", SecretNamespace, SecretName, version)
	}
	return s.sync(ctx, since)
}

// sync wymusza synchronizację ExternalSecret i czeka na udaną po since.
func (s Sync) sync(ctx context.Context, since time.Time) error {
	if err := s.Cluster.ForceSync(ctx, SecretNamespace, SecretName, s.now()); err != nil {
		return err
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	var last cluster.ExternalSecret
	err := poll.Until(ctx, timeout, 2*time.Second, func(ctx context.Context) (bool, error) {
		es, err := s.Cluster.ExternalSecret(ctx, SecretNamespace, SecretName)
		if err != nil {
			return false, nil
		}
		last = es
		return es.Ready && !es.RefreshTime.Before(since), nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		// Bez żadnego stanu kontroler ESO nie dotarł do obiektu — przyczyna
		// jest w nim, nie w OpenBao.
		why := strings.TrimSpace(last.Reason + " " + last.Message)
		if why == "" {
			why = "brak stanu od kontrolera ESO — kubectl -n external-secrets logs deploy/external-secrets"
		}
		return cli.Unmet("ExternalSecret %s/%s nie zsynchronizował się w %v: %s — kubectl -n %s describe externalsecret %s", SecretNamespace, SecretName, timeout, why, SecretNamespace, SecretName)
	}
	if err != nil {
		return err
	}
	cli.Step("Secret %s/%s od ESO zsynchronizowany z OpenBao", SecretNamespace, SecretName)
	return nil
}
