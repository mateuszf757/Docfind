// Package dnstoken zapisuje token API Cloudflare w klastrze dla wydawców
// Let's Encrypt (DNS-01), po sprawdzeniu go w API Cloudflare (dawniej
// ci/set-dns-token.sh).
//
// Token nie trafia do gita, historii powłoki, argumentów procesów ani
// zmiennych środowiskowych dzieci: weryfikacja to żądanie HTTPS z tokenem
// w nagłówku (adapters/cloudflare), a Secret powstaje przez API Kubernetesa
// (adapters/kube). Argumenty procesu widzi każdy użytkownik systemu w `ps`;
// skrypt bashowy musiał na to uważać, tu nie ma czego pilnować.
package dnstoken

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Miejsce tokenu w klastrze — tam szuka go wydawca z charta platformy.
const (
	SecretName      = "cloudflare-api-token"
	SecretNamespace = "cert-manager"
	SecretKey       = "api-token"
)

// DNS — dostawca DNS strefy wyzwań (adapters/cloudflare).
type DNS interface {
	TokenStatus(ctx context.Context, token string) (string, error)
	ZoneVisible(ctx context.Context, token, zone string) (bool, error)
}

// Secrets — Secrety klastra (adapters/kube).
type Secrets interface {
	StoreSecret(ctx context.Context, namespace, name string, data map[string][]byte) error
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

// Store zapisuje token jako Secret (tworzy albo podmienia), w przestrzeni
// nazw cert-managera. Na Etapie 4 przejmie go Vault przez External Secrets
// Operator (decyzja 5).
func Store(ctx context.Context, s Secrets, token string) error {
	return s.StoreSecret(ctx, SecretNamespace, SecretName, map[string][]byte{SecretKey: []byte(token)})
}
