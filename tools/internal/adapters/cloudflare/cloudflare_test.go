package cloudflare

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const token = "sekretny-token-123"

func server(t *testing.T, status string, zones int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"result":null}`))
			return
		}
		switch r.URL.Path {
		case "/user/tokens/verify":
			_, _ = w.Write([]byte(`{"success":true,"result":{"status":"` + status + `"}}`))
		case "/zones":
			items := strings.TrimSuffix(strings.Repeat(`{"id":"z"},`, zones), ",")
			_, _ = w.Write([]byte(`{"success":true,"result":[` + items + `]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestTokenStatusAndZone(t *testing.T) {
	ctx := context.Background()
	s := server(t, "active", 1)
	api := API{BaseURL: s.URL}
	if status, err := api.TokenStatus(ctx, token); err != nil || status != "active" {
		t.Errorf("aktywny: %q, %v", status, err)
	}
	if status, err := api.TokenStatus(ctx, "inny"); err != nil || status != "" {
		t.Errorf("odrzucony token: %q, %v", status, err)
	}
	if ok, err := api.ZoneVisible(ctx, token, "docfind.lol"); err != nil || !ok {
		t.Errorf("strefa: %v, %v", ok, err)
	}
	if ok, err := (API{BaseURL: server(t, "active", 0).URL}).ZoneVisible(ctx, token, "docfind.lol"); err != nil || ok {
		t.Errorf("strefa poza zakresem: %v, %v", ok, err)
	}
	if status, _ := (API{BaseURL: server(t, "disabled", 1).URL}).TokenStatus(ctx, token); status != "disabled" {
		t.Errorf("wyłączony: %q", status)
	}
	// Awaria transportu nie niesie tokenu w komunikacie.
	_, err := API{BaseURL: "http://127.0.0.1:1"}.TokenStatus(ctx, token)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("awaria transportu: %v", err)
	}
}
