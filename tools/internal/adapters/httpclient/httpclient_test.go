package httpclient

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/loopback"
	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

func port(t *testing.T, s *httptest.Server) int {
	t.Helper()
	_, p, err := net.SplitHostPort(s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

// TestGetTLS: żądanie idzie na 127.0.0.1 z nazwą hosta w SNI i nagłówku
// Host, certyfikat jest weryfikowany względem podanego CA, a przekierowanie
// wraca jako odpowiedź.
func TestGetTLS(t *testing.T) {
	var gotHost string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		if r.URL.Path == "/stary" {
			http.Redirect(w, r, "https://example.com/nowy", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("X-Request-Id", "abc")
	}))
	defer srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	req := loopback.Request{Port: port(t, srv), Host: "example.com", Path: "/version", TLS: &loopback.Trust{RootCAs: ca}, Timeout: 3 * time.Second}
	ctx := context.Background()
	c := Client{}

	resp, err := c.Get(ctx, req)
	if err != nil || resp.Status != http.StatusOK || resp.Header["X-Request-Id"] != "abc" {
		t.Fatalf("odpowiedź %+v, %v", resp, err)
	}
	if gotHost != req.Host+":"+strconv.Itoa(req.Port) {
		t.Errorf("nagłówek Host %q", gotHost)
	}
	req.Path = "/stary"
	if resp, err := c.Get(ctx, req); err != nil || resp.Status != http.StatusMovedPermanently || resp.Header["Location"] != "https://example.com/nowy" {
		t.Errorf("przekierowanie: %+v, %v", resp, err)
	}
	cert, err := c.Certificate(ctx, req)
	if err != nil || cert.SerialNumber.Cmp(srv.Certificate().SerialNumber) != 0 {
		t.Errorf("certyfikat: %v", err)
	}

	// Bez CA serwera weryfikacja ma się nie udać — klient niczego nie
	// przepuszcza po cichu.
	req.TLS = &loopback.Trust{}
	if _, err := c.Get(ctx, req); err == nil {
		t.Error("certyfikat spoza magazynu zaufania przyjęty")
	}
	req.TLS = &loopback.Trust{SkipVerify: true}
	if _, err := c.Get(ctx, req); err != nil {
		t.Errorf("staging bez weryfikacji łańcucha: %v", err)
	}
}

func TestGetHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	resp, err := Client{}.Get(context.Background(), loopback.Request{Port: port(t, srv), Path: "/healthz", Timeout: time.Second})
	if err != nil || resp.Status != http.StatusServiceUnavailable {
		t.Errorf("HTTP: %+v, %v", resp, err)
	}
	if _, err := (Client{}).Certificate(context.Background(), loopback.Request{Port: port(t, srv)}); err == nil {
		t.Error("certyfikat bez TLS")
	}
}

// TestBearer: token idzie w nagłówku Authorization tylko wtedy, gdy jest.
func TestBearer(t *testing.T) {
	var got []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
	}))
	t.Cleanup(s.Close)
	port := s.Listener.Addr().(*net.TCPAddr).Port
	for _, token := range []sensitive.Value{sensitive.New("t-1"), {}} {
		if _, err := (Client{}).Get(context.Background(), loopback.Request{Port: port, Path: "/metrics", Timeout: time.Second, Bearer: token}); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0] != "Bearer t-1" || got[1] != "" {
		t.Errorf("nagłówki: %q", got)
	}
}
