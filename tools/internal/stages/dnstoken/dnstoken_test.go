package dnstoken

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
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

// vault — OpenBao: wersje sekretu i role, którymi zapisywano.
type vault struct {
	versions []string
	roles    []string
}

func (v *vault) PutSecret(_ context.Context, account, role, path string, data map[string]sensitive.Value) (int, error) {
	if path != VaultPath || account != WriterAccount {
		return 0, errors.New("nie ta ścieżka albo konto")
	}
	v.roles = append(v.roles, role)
	v.versions = append(v.versions, data[SecretKey].Reveal())
	return len(v.versions), nil
}
func (v *vault) SecretVersion(context.Context, string, string, string) (int, bool, error) {
	return len(v.versions), len(v.versions) > 0, nil
}

// kluster — Secret sprzed OpenBao (legacy) i ExternalSecret, który ESO
// synchronizuje po force-sync, o ile w OpenBao jest wartość (synced).
type kluster struct {
	legacy  string
	synced  func() bool
	forced  []time.Time
	refresh time.Time
	// staleStore — SecretStore zwalidowany przed OpenBao: niegotowy, dopóki
	// ktoś nie wymusi walidacji; do tego czasu force-sync nic nie daje.
	staleStore  bool
	revalidated int
}

func (k *kluster) SecretStore(context.Context, string, string) (cluster.SecretStore, error) {
	return cluster.SecretStore{Ready: !k.staleStore}, nil
}
func (k *kluster) RevalidateStore(context.Context, string, string, time.Time) error {
	k.revalidated++
	k.staleStore = false
	return nil
}

func (k *kluster) SecretData(context.Context, string, string) (map[string][]byte, error) {
	if k.legacy == "" {
		return nil, errors.New("not found")
	}
	return map[string][]byte{SecretKey: []byte(k.legacy)}, nil
}
func (k *kluster) ForceSync(_ context.Context, _, _ string, at time.Time) error {
	k.forced = append(k.forced, at)
	if k.synced() && !k.staleStore {
		k.refresh = at
	}
	return nil
}
func (k *kluster) ExternalSecret(context.Context, string, string) (cluster.ExternalSecret, error) {
	if !k.synced() || k.refresh.IsZero() {
		return cluster.ExternalSecret{Reason: "SecretSyncedError", Message: "secret not found"}, nil
	}
	return cluster.ExternalSecret{Ready: true, RefreshTime: k.refresh}, nil
}

func sync(v *vault, k *kluster) Sync {
	clock := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	return Sync{Vault: v, Cluster: k, Timeout: time.Second, Now: func() time.Time { clock = clock.Add(time.Second); return clock }}
}

// TestStore: nowa wersja w OpenBao rolą zapisu, potem wymuszona
// synchronizacja i czekanie na Secret.
func TestStore(t *testing.T) {
	quiet(t)
	v := &vault{}
	k := &kluster{}
	k.synced = func() bool { return len(v.versions) > 0 }
	if err := sync(v, k).Store(context.Background(), sensitive.New(token)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.versions, ",") != token || strings.Join(v.roles, ",") != WriterRole || len(k.forced) != 1 {
		t.Errorf("wersje %v, role %v, wymuszenia %d", v.versions, v.roles, len(k.forced))
	}
}

// TestEnsure: Secret sprzed OpenBao przeniesiony do OpenBao (raz), brak
// tokenu w obu miejscach — warunek niespełniony z instrukcją, ESO bez
// synchronizacji — warunek niespełniony z powodem od ESO.
func TestEnsure(t *testing.T) {
	quiet(t)
	ctx := context.Background()
	v := &vault{}
	k := &kluster{legacy: "token-sprzed-openbao"}
	k.synced = func() bool { return len(v.versions) > 0 }
	if err := sync(v, k).Ensure(ctx, "dns-token"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.versions, ",") != "token-sprzed-openbao" {
		t.Fatalf("migracja: %v", v.versions)
	}
	if err := sync(v, k).Ensure(ctx, "dns-token"); err != nil || len(v.versions) != 1 {
		t.Errorf("drugie wdrożenie przeniosło token jeszcze raz: %v, %v", v.versions, err)
	}

	empty := &vault{}
	none := &kluster{synced: func() bool { return false }}
	if err := sync(empty, none).Ensure(ctx, "./bin/mise run dns-token"); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "./bin/mise run dns-token") {
		t.Errorf("brak tokenu: %v", err)
	}
	stuck := &kluster{synced: func() bool { return false }}
	if err := sync(v, stuck).Ensure(ctx, "dns-token"); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "SecretSyncedError") {
		t.Errorf("ESO bez synchronizacji: %v", err)
	}
}

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

// TestEnsureRevalidatesStaleStore: SecretStore z charta platformy powstał
// przed OpenBao i jest „niegotowy" do następnej próby ESO (narastająca
// przerwa) — Ensure wymusza jego walidację i ponawia synchronizację, zamiast
// czekać, aż ESO sam wróci (staging: przekroczony czas).
func TestEnsureRevalidatesStaleStore(t *testing.T) {
	quiet(t)
	v := &vault{}
	k := &kluster{legacy: "token-sprzed-openbao", staleStore: true}
	k.synced = func() bool { return len(v.versions) > 0 }
	sy := sync(v, k)
	sy.Timeout = 5 * time.Second
	if err := sy.Ensure(context.Background(), "dns-token"); err != nil {
		t.Fatal(err)
	}
	if k.revalidated != 1 {
		t.Errorf("wymuszeń walidacji magazynu: %d", k.revalidated)
	}
}
