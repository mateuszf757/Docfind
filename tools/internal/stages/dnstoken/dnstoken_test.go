package dnstoken

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

const token = "sekretny-token-123"

type dns struct {
	status string
	zones  map[string]bool
}

func (d dns) TokenStatus(_ context.Context, t string) (string, error) {
	if t != token {
		return "", nil
	}
	return d.status, nil
}

func (d dns) ZoneVisible(_ context.Context, _, zone string) (bool, error) { return d.zones[zone], nil }

func TestVerify(t *testing.T) {
	oldOut := cli.Out
	cli.Out = io.Discard
	defer func() { cli.Out = oldOut }()
	tests := []struct {
		name     string
		dns      dns
		token    string
		zone     string
		wantCode int
	}{
		{"aktywny, widzi strefę", dns{"active", map[string]bool{"docfind.lol": true}}, token, "docfind.lol", cli.ExitOK},
		{"aktywny, bez sprawdzania strefy", dns{"active", nil}, token, "", cli.ExitOK},
		{"nieaktywny", dns{"disabled", nil}, token, "", cli.ExitUnmet},
		{"nie widzi strefy", dns{"active", nil}, token, "docfind.lol", cli.ExitUnmet},
		{"zły token", dns{"active", nil}, "inny", "", cli.ExitUnmet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(context.Background(), tt.dns, tt.token, tt.zone)
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

type secrets map[string]map[string][]byte

func (s secrets) StoreSecret(_ context.Context, namespace, name string, data map[string][]byte) error {
	s[namespace+"/"+name] = data
	return nil
}

func TestStore(t *testing.T) {
	s := secrets{}
	if err := Store(context.Background(), s, token); err != nil {
		t.Fatal(err)
	}
	if got := string(s[SecretNamespace+"/"+SecretName][SecretKey]); got != token {
		t.Errorf("Secret ma %q", got)
	}
}
