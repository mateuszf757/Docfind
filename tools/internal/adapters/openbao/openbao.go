// Package openbao to adapter API OpenBao (zgodnego z API Vaulta): cienki
// klient HTTP na tych kilkanaście wywołań, których potrzebuje
// stages/secretstore — inicjalizacja, stan pieczęci, logowanie kontem
// Kubernetesa i konfiguracja jako kod. Bez SDK: to kilka zapytań JSON,
// a SDK wciągnąłby kilkadziesiąt modułów w łańcuch dostaw narzędzi.
//
// Połączenie zawsze TLS, weryfikowane wewnętrznym CA platformy, z nazwą
// serwera z certyfikatu — także przez port-forward na 127.0.0.1. Token idzie
// tylko w nagłówku, a w błędach nie ma treści odpowiedzi poza listą
// errors z API: odpowiedź na zapis może nieść wartości, odpowiedź na
// błąd — nie.
package openbao

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/secretstore"
	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

// Client — klient jednego serwera OpenBao.
type Client struct {
	addr  string
	http  *http.Client
	token sensitive.Value
}

// New łączy się z addr (host:port) po TLS, ufając tylko certyfikatom
// z caPEM i sprawdzając nazwę serverName.
func New(addr string, caPEM []byte, serverName string) (*Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA OpenBao: brak certyfikatu PEM")
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS13},
		// Bez proxy z otoczenia: adres to port-forward na pętli zwrotnej.
		Proxy: nil,
	}
	return &Client{addr: "https://" + addr, http: &http.Client{Transport: transport, Timeout: 30 * time.Second}}, nil
}

// WithToken zwraca klienta z tokenem.
func (c *Client) WithToken(token sensitive.Value) *Client {
	cp := *c
	cp.token = token
	return &cp
}

// APIError — odpowiedź spoza 2xx.
type APIError struct {
	Method, Path string
	Status       int
	Errors       []string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("OpenBao %s %s: HTTP %d %s", e.Method, e.Path, e.Status, strings.Join(e.Errors, "; "))
}

// Status zwraca kod HTTP z błędu API albo 0.
func Status(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.addr+"/v1/"+path, body)
	if err != nil {
		return err
	}
	if !c.token.Empty() {
		req.Header.Set("X-Vault-Token", c.token.Reveal())
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("OpenBao %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Method: method, Path: path, Status: resp.StatusCode}
		var e struct {
			Errors []string `json:"errors"`
		}
		if json.Unmarshal(raw, &e) == nil {
			apiErr.Errors = e.Errors
		}
		return apiErr
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("OpenBao %s %s: odpowiedź: %w", method, path, err)
	}
	return nil
}

// SealStatus — stan pieczęci serwera (bez tokenu).
func (c *Client) SealStatus(ctx context.Context) (secretstore.SealStatus, error) {
	var s struct {
		Initialized bool   `json:"initialized"`
		Sealed      bool   `json:"sealed"`
		Type        string `json:"type"`
		Version     string `json:"version"`
	}
	if err := c.do(ctx, http.MethodGet, "sys/seal-status", nil, &s); err != nil {
		return secretstore.SealStatus{}, err
	}
	return secretstore.SealStatus{Initialized: s.Initialized, Sealed: s.Sealed, Type: s.Type, Version: s.Version}, nil
}

// Init inicjalizuje serwer z automatycznym odpieczętowaniem: shares kluczy
// odzyskiwania, wszystkie potrzebne (próg = shares).
func (c *Client) Init(ctx context.Context, shares int) (secretstore.Init, error) {
	var r struct {
		RecoveryKeys []string `json:"recovery_keys_base64"`
		RootToken    string   `json:"root_token"`
	}
	if err := c.do(ctx, http.MethodPut, "sys/init", map[string]int{"recovery_shares": shares, "recovery_threshold": shares}, &r); err != nil {
		return secretstore.Init{}, err
	}
	if r.RootToken == "" || len(r.RecoveryKeys) != shares {
		return secretstore.Init{}, errors.New("OpenBao sys/init: odpowiedź bez tokenu root albo kluczy odzyskiwania")
	}
	init := secretstore.Init{RootToken: sensitive.New(r.RootToken)}
	for _, k := range r.RecoveryKeys {
		init.RecoveryKeys = append(init.RecoveryKeys, sensitive.New(k))
	}
	return init, nil
}

// KubernetesLogin wymienia token konta Kubernetesa na token OpenBao roli
// role w metodzie z mount.
func (c *Client) KubernetesLogin(ctx context.Context, mount, role string, jwt sensitive.Value) (sensitive.Value, error) {
	var r struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := c.do(ctx, http.MethodPost, "auth/"+mount+"/login", map[string]string{"role": role, "jwt": jwt.Reveal()}, &r); err != nil {
		return sensitive.Value{}, err
	}
	if r.Auth.ClientToken == "" {
		return sensitive.Value{}, errors.New("OpenBao logowanie: odpowiedź bez tokenu")
	}
	return sensitive.New(r.Auth.ClientToken), nil
}

// RevokeSelf odwołuje token klienta.
func (c *Client) RevokeSelf(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "auth/token/revoke-self", nil, nil)
}

// Audits — ścieżka → rodzaj urządzenia audytu.
func (c *Client) Audits(ctx context.Context) (map[string]string, error) {
	return c.typesAt(ctx, "sys/audit")
}

// Mounts — ścieżka → rodzaj silnika sekretów.
func (c *Client) Mounts(ctx context.Context) (map[string]string, error) {
	return c.typesAt(ctx, "sys/mounts")
}

// AuthMethods — ścieżka → rodzaj metody uwierzytelniania.
func (c *Client) AuthMethods(ctx context.Context) (map[string]string, error) {
	return c.typesAt(ctx, "sys/auth")
}

// typesAt czyta listę ścieżek z rodzajami. API zwraca je pod "data"
// (nowsze wersje) albo na najwyższym poziomie; ścieżki bez końcowego „/".
func (c *Client) typesAt(ctx context.Context, path string) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	entries := raw
	if data, ok := raw["data"]; ok {
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, err
		}
	}
	out := map[string]string{}
	for k, v := range entries {
		var e struct {
			Type string `json:"type"`
		}
		if !strings.HasSuffix(k, "/") || json.Unmarshal(v, &e) != nil || e.Type == "" {
			continue
		}
		out[strings.TrimSuffix(k, "/")] = e.Type
	}
	return out, nil
}

// EnableMount włącza silnik sekretów.
func (c *Client) EnableMount(ctx context.Context, m secretstore.Mount) error {
	return c.do(ctx, http.MethodPost, "sys/mounts/"+m.Path, map[string]any{"type": m.Type, "description": m.Description, "options": m.Options}, nil)
}

// EnableAuth włącza metodę uwierzytelniania.
func (c *Client) EnableAuth(ctx context.Context, a secretstore.Auth) error {
	return c.do(ctx, http.MethodPost, "sys/auth/"+a.Path, map[string]string{"type": a.Type}, nil)
}

// Write zapisuje dane pod ścieżką API (konfiguracja metody, rola).
func (c *Client) Write(ctx context.Context, path string, data any) error {
	return c.do(ctx, http.MethodPost, path, data, nil)
}

// Policies — nazwy polityk ACL.
func (c *Client) Policies(ctx context.Context) ([]string, error) {
	return c.list(ctx, "sys/policies/acl")
}

// Policy — treść polityki ACL; pusta, gdy jej nie ma.
func (c *Client) Policy(ctx context.Context, name string) (string, error) {
	var r struct {
		Data struct {
			Policy string `json:"policy"`
		} `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, "sys/policies/acl/"+name, nil, &r)
	if Status(err) == http.StatusNotFound {
		return "", nil
	}
	return r.Data.Policy, err
}

// PutPolicy zapisuje politykę ACL.
func (c *Client) PutPolicy(ctx context.Context, name, hcl string) error {
	return c.do(ctx, http.MethodPut, "sys/policies/acl/"+name, map[string]string{"policy": hcl}, nil)
}

// DeletePolicy usuwa politykę ACL.
func (c *Client) DeletePolicy(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "sys/policies/acl/"+name, nil, nil)
}

// Roles — nazwy ról metody kubernetes w mount.
func (c *Client) Roles(ctx context.Context, mount string) ([]string, error) {
	return c.list(ctx, "auth/"+mount+"/role")
}

// DeleteRole usuwa rolę metody kubernetes.
func (c *Client) DeleteRole(ctx context.Context, mount, role string) error {
	return c.do(ctx, http.MethodDelete, "auth/"+mount+"/role/"+role, nil, nil)
}

// list — LIST ścieżki; brak wpisów to 404 i pusta lista.
func (c *Client) list(ctx context.Context, path string) ([]string, error) {
	var r struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	err := c.do(ctx, "LIST", path, nil, &r)
	if Status(err) == http.StatusNotFound {
		return nil, nil
	}
	return r.Data.Keys, err
}

// KVPut zapisuje nową wersję sekretu w silniku KV v2 i zwraca jej numer.
// Wartości odsłaniane dopiero przy kodowaniu żądania.
func (c *Client) KVPut(ctx context.Context, mount, path string, data map[string]sensitive.Value) (int, error) {
	plain := make(map[string]string, len(data))
	for k, v := range data {
		plain[k] = v.Reveal()
	}
	var r struct {
		Data struct {
			Version int `json:"version"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, mount+"/data/"+path, map[string]any{"data": plain}, &r); err != nil {
		return 0, err
	}
	return r.Data.Version, nil
}

// KVVersion zwraca bieżącą wersję sekretu z metadanych KV v2 — bez
// wartości; exists == false, gdy sekretu nie ma.
func (c *Client) KVVersion(ctx context.Context, mount, path string) (version int, exists bool, err error) {
	var r struct {
		Data struct {
			CurrentVersion int `json:"current_version"`
		} `json:"data"`
	}
	err = c.do(ctx, http.MethodGet, mount+"/metadata/"+path, nil, &r)
	if Status(err) == http.StatusNotFound {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return r.Data.CurrentVersion, r.Data.CurrentVersion > 0, nil
}

// RaftServers — członkowie klastra Raft.
func (c *Client) RaftServers(ctx context.Context) ([]secretstore.RaftServer, error) {
	var r struct {
		Data struct {
			Config struct {
				Servers []struct {
					NodeID  string `json:"node_id"`
					Address string `json:"address"`
					Leader  bool   `json:"leader"`
					Voter   bool   `json:"voter"`
				} `json:"servers"`
			} `json:"config"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "sys/storage/raft/configuration", nil, &r); err != nil {
		return nil, err
	}
	var out []secretstore.RaftServer
	for _, s := range r.Data.Config.Servers {
		out = append(out, secretstore.RaftServer{NodeID: s.NodeID, Address: s.Address, Leader: s.Leader, Voter: s.Voter})
	}
	return out, nil
}
