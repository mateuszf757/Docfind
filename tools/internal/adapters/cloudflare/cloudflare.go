// Package cloudflare to adapter API Cloudflare: sprawdzenie tokenu, którym
// wydawca Let's Encrypt rozwiązuje wyzwania DNS-01.
//
// Token idzie tylko w nagłówku żądania HTTPS — nie w argumentach procesu,
// zmiennych środowiskowych dzieci ani komunikatach: błąd transportu nie
// zawiera nagłówków żądania.
package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// DefaultAPI — API Cloudflare; testy podstawiają serwer lokalny.
const DefaultAPI = "https://api.cloudflare.com/client/v4"

// API — klient API Cloudflare.
type API struct {
	BaseURL string
	Client  *http.Client
}

type envelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
}

func (a API) get(ctx context.Context, token, path string) (envelope, error) {
	base := a.BaseURL
	if base == "" {
		base = DefaultAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/"+path, nil)
	if err != nil {
		return envelope{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return envelope{}, fmt.Errorf("API Cloudflare: %w", err)
	}
	defer resp.Body.Close()
	var e envelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e); err != nil {
		return envelope{}, fmt.Errorf("API Cloudflare (HTTP %d): nieczytelna odpowiedź: %w", resp.StatusCode, err)
	}
	return e, nil
}

// TokenStatus zwraca stan tokenu według Cloudflare („active", „disabled"…);
// token odrzucony w całości — pusty stan.
func (a API) TokenStatus(ctx context.Context, token string) (string, error) {
	e, err := a.get(ctx, token, "user/tokens/verify")
	if err != nil {
		return "", err
	}
	var result struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(e.Result, &result)
	if !e.Success {
		return "", nil
	}
	return result.Status, nil
}

// ZoneVisible mówi, czy token widzi strefę DNS (uprawnienie Zone → Read
// w jej zakresie).
func (a API) ZoneVisible(ctx context.Context, token, zone string) (bool, error) {
	e, err := a.get(ctx, token, "zones?name="+url.QueryEscape(zone))
	if err != nil {
		return false, err
	}
	var zones []json.RawMessage
	_ = json.Unmarshal(e.Result, &zones)
	return e.Success && len(zones) == 1, nil
}
