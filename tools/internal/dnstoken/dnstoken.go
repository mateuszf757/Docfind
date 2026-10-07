// Package dnstoken zapisuje token API Cloudflare w klastrze dla wydawców
// Let's Encrypt (DNS-01), po sprawdzeniu go w API Cloudflare (dawniej
// ci/set-dns-token.sh).
//
// Token nie trafia do gita, historii powłoki, argumentów procesów ani
// zmiennych środowiskowych dzieci — w tym pakiecie w ogóle nie ma
// podprocesów: weryfikacja to żądanie HTTPS z tokenem w nagłówku, a Secret
// powstaje przez API Kubernetesa. Argumenty procesu widzi każdy użytkownik
// systemu w `ps`; skrypt bashowy musiał na to uważać (printf jako wbudowane
// polecenie, curl --config -), tu nie ma czego pilnować.
package dnstoken

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
)

// Miejsce tokenu w klastrze — tam szuka go wydawca z charta platformy.
const (
	SecretName      = "cloudflare-api-token"
	SecretNamespace = "cert-manager"
	SecretKey       = "api-token"
	// DefaultAPI — API Cloudflare; testy podstawiają serwer lokalny.
	DefaultAPI = "https://api.cloudflare.com/client/v4"
)

// ReadToken czyta token z wejścia: jedna linia, bez końcowych białych znaków.
// Z terminala — przez readSecret bez echa; z potoku (menedżer haseł) — wprost.
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

// Verifier sprawdza token w API Cloudflare.
type Verifier struct {
	BaseURL string
	Client  *http.Client
}

type envelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
}

func (v Verifier) get(ctx context.Context, token, path string) (envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(v.BaseURL, "/")+"/"+path, nil)
	if err != nil {
		return envelope{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := v.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// Błąd transportu nie zawiera nagłówków żądania, więc tokenu też nie.
		return envelope{}, fmt.Errorf("API Cloudflare: %w", err)
	}
	defer resp.Body.Close()
	var e envelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e); err != nil {
		return envelope{}, fmt.Errorf("API Cloudflare (HTTP %d): nieczytelna odpowiedź: %w", resp.StatusCode, err)
	}
	return e, nil
}

// Verify sprawdza, że token jest aktywny, a jeśli podano strefę — że ją
// widzi. Zły albo wygasły token wyszedłby inaczej dopiero jako wyzwanie
// DNS-01 wiszące bez końca, z błędem widocznym tylko w statusie Challenge.
func (v Verifier) Verify(ctx context.Context, token, zone string) error {
	e, err := v.get(ctx, token, "user/tokens/verify")
	if err != nil {
		return err
	}
	var result struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(e.Result, &result)
	if !e.Success || result.Status != "active" {
		return cli.Unmet("Cloudflare nie potwierdził tokenu (status: %q)", result.Status)
	}
	cli.Step("token aktywny")
	if zone == "" {
		return nil
	}
	e, err = v.get(ctx, token, "zones?name="+url.QueryEscape(zone))
	if err != nil {
		return err
	}
	var zones []json.RawMessage
	_ = json.Unmarshal(e.Result, &zones)
	if !e.Success || len(zones) != 1 {
		return cli.Unmet("token nie widzi strefy %s — brakuje Zone → Zone → Read albo strefa jest spoza zakresu tokenu", zone)
	}
	cli.Step("token widzi strefę %s", zone)
	return nil
}

// Store zapisuje token jako Secret (tworzy albo podmienia), w przestrzeni
// nazw cert-managera. Na Etapie 4 przejmie go Vault przez External Secrets
// Operator (decyzja 5).
func Store(ctx context.Context, cs kubernetes.Interface, token string) error {
	if _, err := cs.CoreV1().Namespaces().Get(ctx, SecretNamespace, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: SecretNamespace}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: SecretNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{SecretKey: []byte(token)},
	}
	secrets := cs.CoreV1().Secrets(SecretNamespace)
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	} else if err != nil {
		return err
	}
	return nil
}
