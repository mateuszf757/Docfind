package dnstoken

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
)

const token = "sekretny-token-123"

func cloudflare(t *testing.T, status string, zones int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(server.Close)
	return server
}

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = &bytes.Buffer{}, &bytes.Buffer{}
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

func TestVerify(t *testing.T) {
	quiet(t)
	tests := []struct {
		name     string
		status   string
		zones    int
		token    string
		zone     string
		wantCode int
	}{
		{"aktywny, widzi strefę", "active", 1, token, "docfind.lol", cli.ExitOK},
		{"aktywny, bez sprawdzania strefy", "active", 0, token, "", cli.ExitOK},
		{"nieaktywny", "disabled", 1, token, "", cli.ExitUnmet},
		{"nie widzi strefy", "active", 0, token, "docfind.lol", cli.ExitUnmet},
		{"zły token", "active", 1, "inny", "", cli.ExitUnmet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := cloudflare(t, tt.status, tt.zones)
			err := Verifier{BaseURL: server.URL}.Verify(context.Background(), tt.token, tt.zone)
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			if err != nil && strings.Contains(err.Error(), tt.token) {
				t.Errorf("token w komunikacie błędu: %v", err)
			}
		})
	}
}

func TestReadToken(t *testing.T) {
	got, err := ReadToken(strings.NewReader("  " + token + "  \nreszta"))
	if err != nil || got != token {
		t.Errorf("ReadToken = %q, %v", got, err)
	}
	if _, err := ReadToken(strings.NewReader("\n")); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("pusty token przyjęty: %v", err)
	}
}

func TestStore(t *testing.T) {
	cs := fake.NewClientset()
	ctx := context.Background()
	for _, value := range []string{token, token + "-nowy"} {
		if err := Store(ctx, cs, value); err != nil {
			t.Fatal(err)
		}
		s, err := cs.CoreV1().Secrets(SecretNamespace).Get(ctx, SecretName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if string(s.Data[SecretKey]) != value {
			t.Errorf("Secret ma %q, oczekiwano %q", s.Data[SecretKey], value)
		}
	}
}
