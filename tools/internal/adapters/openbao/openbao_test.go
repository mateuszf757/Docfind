package openbao

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

// server — OpenBao na niby: TLS z certyfikatem httptest (example.com).
func server(t *testing.T, handler http.HandlerFunc) (*httptest.Server, []byte) {
	t.Helper()
	s := httptest.NewTLSServer(handler)
	t.Cleanup(s.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	return s, ca
}

func client(t *testing.T, s *httptest.Server, ca []byte) *Client {
	t.Helper()
	c, err := New(strings.TrimPrefix(s.URL, "https://"), ca, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestTokenAndErrors: token tylko w nagłówku; błąd niesie listę errors
// z API, a nie treść odpowiedzi.
func TestTokenAndErrors(t *testing.T) {
	s, ca := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "t-123" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"errors":["permission denied"],"data":{"value":"tajne"}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"keys":["a","b"]}}`)
	})
	c := client(t, s, ca)
	_, err := c.Policies(context.Background())
	if Status(err) != http.StatusForbidden || !strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "tajne") {
		t.Errorf("błąd bez tokenu: %v", err)
	}
	keys, err := c.WithToken(sensitive.New("t-123")).Policies(context.Background())
	if err != nil || strings.Join(keys, ",") != "a,b" {
		t.Errorf("polityki: %v, %v", keys, err)
	}
}

// foreignCA — certyfikat samopodpisany spoza serwera testowego (httptest
// ma jeden wbudowany certyfikat dla wszystkich serwerów).
func foreignCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "obce CA"}, DNSNames: []string{"example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestRejectsForeignCA: serwer z certyfikatem spoza CA platformy jest
// odrzucany, zanim cokolwiek do niego trafi.
func TestRejectsForeignCA(t *testing.T) {
	s, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("żądanie dotarło do serwera z obcym certyfikatem")
	})
	if _, err := client(t, s, foreignCA(t)).SealStatus(context.Background()); err == nil {
		t.Error("obcy certyfikat przyjęty")
	}
	if _, err := New("127.0.0.1:1", []byte("nie PEM"), "x"); err == nil {
		t.Error("CA bez certyfikatu przyjęte")
	}
}

// TestTypesAndLists: ścieżki pod "data" i na najwyższym poziomie, puste
// LIST jako 404, Init z kluczem i tokenem jako wartościami poufnymi.
func TestTypesAndLists(t *testing.T) {
	s, ca := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/sys/mounts":
			fmt.Fprint(w, `{"data":{"docfind/":{"type":"kv"},"sys/":{"type":"system"}},"docfind/":{"type":"kv"}}`)
		case r.URL.Path == "/v1/sys/audit":
			fmt.Fprint(w, `{"stdout/":{"type":"file"}}`)
		case r.Method == "LIST":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"errors":[]}`)
		case r.URL.Path == "/v1/sys/init" && r.Method == http.MethodPut:
			fmt.Fprint(w, `{"recovery_keys_base64":["a2V5"],"root_token":"s.root"}`)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	})
	c := client(t, s, ca)
	ctx := context.Background()
	if m, err := c.Mounts(ctx); err != nil || m["docfind"] != "kv" || m["sys"] != "system" {
		t.Errorf("montowania: %v, %v", m, err)
	}
	if a, err := c.Audits(ctx); err != nil || a["stdout"] != "file" {
		t.Errorf("audyt: %v, %v", a, err)
	}
	if roles, err := c.Roles(ctx, "kubernetes"); err != nil || len(roles) != 0 {
		t.Errorf("puste role: %v, %v", roles, err)
	}
	init, err := c.Init(ctx, 1)
	if err != nil || init.RootToken.Reveal() != "s.root" || init.RecoveryKeys[0].Reveal() != "a2V5" {
		t.Fatalf("init: %v", err)
	}
	if out := fmt.Sprintf("%+v", init); strings.Contains(out, "s.root") || strings.Contains(out, "a2V5") {
		t.Errorf("wynik inicjalizacji wypisuje sekrety: %s", out)
	}
}

// TestKV: zapis w KV v2 idzie pod data/ z wartościami w treści żądania,
// a wersja z metadata/ — 404 to brak sekretu, nie błąd.
func TestKV(t *testing.T) {
	var body string
	s, ca := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/docfind/data/platform/cloudflare":
			raw, _ := io.ReadAll(r.Body)
			body = string(raw)
			fmt.Fprint(w, `{"data":{"version":3}}`)
		case r.URL.Path == "/v1/docfind/metadata/platform/cloudflare":
			fmt.Fprint(w, `{"data":{"current_version":3}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"errors":[]}`)
		}
	})
	c := client(t, s, ca)
	ctx := context.Background()
	v, err := c.KVPut(ctx, "docfind", "platform/cloudflare", map[string]sensitive.Value{"api-token": sensitive.New("cf-1")})
	if err != nil || v != 3 || body != `{"data":{"api-token":"cf-1"}}` {
		t.Errorf("zapis: wersja %d, treść %s, %v", v, body, err)
	}
	if v, ok, err := c.KVVersion(ctx, "docfind", "platform/cloudflare"); err != nil || !ok || v != 3 {
		t.Errorf("wersja: %d, %v, %v", v, ok, err)
	}
	if _, ok, err := c.KVVersion(ctx, "docfind", "platform/brak"); err != nil || ok {
		t.Errorf("brak sekretu: %v, %v", ok, err)
	}
}
