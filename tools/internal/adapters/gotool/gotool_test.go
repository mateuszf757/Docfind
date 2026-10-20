package gotool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// govulncheckJSON — strumień `govulncheck -format json` (v1.8.0): ustalenia
// na poziomie modułu, pakietu i symbolu dla jednej podatności i ustalenie
// symbolu bez poprawki dla drugiej.
const govulncheckJSON = `{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck","scanner_version":"v1.8.0","db":"https://vuln.go.dev","db_last_modified":"2026-10-08T22:31:09Z"}}
{"osv":{"id":"GO-2026-0001","aliases":["CVE-2026-22222"]}}
{"osv":{"id":"GO-2026-0002","aliases":[]}}
{"finding":{"osv":"GO-2026-0001","fixed_version":"v0.36.6","trace":[{"module":"k8s.io/client-go","version":"v0.36.5"}]}}
{"finding":{"osv":"GO-2026-0001","fixed_version":"v0.36.6","trace":[{"module":"k8s.io/client-go","version":"v0.36.5","package":"k8s.io/client-go/rest"}]}}
{"finding":{"osv":"GO-2026-0001","fixed_version":"v0.36.6","trace":[{"module":"k8s.io/client-go","version":"v0.36.5","package":"k8s.io/client-go/rest","function":"Do"},{"module":"github.com/mateuszf757/Docfind/tools","function":"main"}]}}
{"finding":{"osv":"GO-2026-0001","fixed_version":"v0.36.6","trace":[{"module":"k8s.io/client-go","version":"v0.36.5","package":"k8s.io/client-go/rest","function":"DoRaw"}]}}
{"finding":{"osv":"GO-2026-0002","trace":[{"module":"golang.org/x/net","version":"v0.50.0","package":"golang.org/x/net/http2","function":"Serve"}]}}
{"finding":{"osv":"GO-2026-0003","fixed_version":"v1.2.3","trace":[{"module":"example.com/niewolany","version":"v1.2.0"}]}}
`

func TestParseGovulncheck(t *testing.T) {
	findings, db, err := ParseGovulncheck(strings.NewReader(govulncheckJSON))
	if err != nil {
		t.Fatal(err)
	}
	if db.Ecosystem != "Go (https://vuln.go.dev)" || db.Newest != "2026-10-08T22:31:09Z" {
		t.Errorf("migawka bazy: %+v", db)
	}
	if len(findings) != 2 {
		t.Fatalf("ustalenia osiągalne: %+v — tylko poziom symbolu, bez powtórzeń", findings)
	}
	net, client := findings[0], findings[1]
	if client.ID != "GO-2026-0001" || client.Package != "k8s.io/client-go" || strings.Join(client.Fixed, ",") != "v0.36.6" || strings.Join(client.Aliases, ",") != "CVE-2026-22222" {
		t.Errorf("client-go: %+v", client)
	}
	if net.ID != "GO-2026-0002" || len(net.Fixed) != 0 {
		t.Errorf("x/net: %+v", net)
	}
	got, _ := vuln.Policy{Today: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}.Evaluate(findings)
	if !got[1].Blocking || got[0].Blocking {
		t.Errorf("osiągalna z poprawką blokuje, bez poprawki nie: %+v", got)
	}
}

// TestParseGovulncheckWithoutDatabase: bez daty bazy wyniku nie da się
// porównać z punktem odniesienia — to awaria, a nie pusty wynik.
func TestParseGovulncheckWithoutDatabase(t *testing.T) {
	if _, _, err := ParseGovulncheck(strings.NewReader(`{"config":{"scanner_name":"govulncheck"}}`)); err == nil {
		t.Error("wynik bez config.db_last_modified przyjęty")
	}
}

// reachable — strumień z ustaleniem na poziomie symbolu, z poprawką.
const reachable = `{"config":{"db":"https://vuln.go.dev","db_last_modified":"2026-10-08T22:31:09Z"}}
{"finding":{"osv":"GO-2026-0001","fixed_version":"v0.36.6","trace":[{"module":"k8s.io/client-go","version":"v0.36.5","function":"Do"}]}}`

// TestVulncheckBaselineDir: kod z innego commita skanuje govulncheck
// z bieżącego modułu (go tool w ModuleDir), a moduł dostaje przez -C.
func TestVulncheckBaselineDir(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"go tool govulncheck -C /tmp/baza -format json ./...": {Stdout: reachable},
	}}
	findings, _, err := Go{ModuleDir: "/m", Runner: f}.Vulncheck(context.Background(), "/tmp/baza")
	if err != nil || len(findings) != 1 {
		t.Fatalf("ustalenia %+v, %v", findings, err)
	}
	if f.Calls[0].Dir != "/m" {
		t.Errorf("govulncheck z katalogu %q, oczekiwano bieżącego modułu", f.Calls[0].Dir)
	}
}

// TestExitCodes: niezerowy kod narzędzia jakości to warunek niespełniony,
// a niemożność uruchomienia — awaria; dla govulncheck (-format json)
// niezerowy kod to zawsze awaria, a wynik niesie strumień.
func TestExitCodes(t *testing.T) {
	vet := func(g Go, ctx context.Context) error { return g.Vet(ctx) }
	vulncheck := func(g Go, ctx context.Context) error { _, _, err := g.Vulncheck(ctx, ""); return err }
	tests := []struct {
		name     string
		resp     proc.FakeResponse
		run      func(Go, context.Context) error
		wantCode int
	}{
		{"vet bez uwag", proc.FakeResponse{}, vet, cli.ExitOK},
		{"vet z uwagami", proc.FakeResponse{Code: 1}, vet, cli.ExitUnmet},
		{"go nie uruchamia się", proc.FakeResponse{Err: errors.New("brak go")}, vet, cli.ExitFailure},
		{"govulncheck: brak bazy", proc.FakeResponse{Code: 1}, vulncheck, cli.ExitFailure},
		{"govulncheck: nieczytelny wynik", proc.FakeResponse{Stdout: "nie JSON"}, vulncheck, cli.ExitFailure},
		{"govulncheck: wynik", proc.FakeResponse{Stdout: reachable}, vulncheck, cli.ExitOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &proc.Fake{Responses: map[string]proc.FakeResponse{
				"go vet ./...":                           tt.resp,
				"go tool govulncheck -format json ./...": tt.resp,
			}}
			err := tt.run(Go{ModuleDir: "/m", Runner: f}, context.Background())
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Errorf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
		})
	}
}

func TestTestRaceAndBuild(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"go test -race -coverpkg=./... -coverprofile=/tmp/c.out ./...": {},
		"go env CC": {Stdout: "gcc -m64\n"},
		"go build -mod=readonly -trimpath -o /out/drain-probe ./cmd/drain-probe": {},
		"go test -tags cluster -count=1 -run Rejected$ ./...":                    {Code: 1},
	}}
	g := Go{ModuleDir: "/m", Runner: f, LookPath: func(name string) (string, error) {
		if name != "gcc" {
			return "", errors.New("brak")
		}
		return "/usr/bin/gcc", nil
	}}
	ctx := context.Background()
	if err := g.Test(ctx, true, "/tmp/c.out"); err != nil || !strings.Contains(strings.Join(f.Calls[0].Env, " "), "CGO_ENABLED=1") {
		t.Errorf("-race bez CGO_ENABLED=1: %v, %v", f.Calls[0].Env, err)
	}
	if cc, ok := g.CCompiler(ctx); !ok || cc != "gcc -m64" {
		t.Errorf("kompilator C: %q, %v", cc, ok)
	}
	if err := g.Build(ctx, "./cmd/drain-probe", "/out/drain-probe", "GOOS=linux"); err != nil {
		t.Fatal(err)
	}
	if env := strings.Join(f.Calls[2].Env, " "); env != "CGO_ENABLED=0 GOOS=linux" {
		t.Errorf("środowisko buildu: %q", env)
	}
	if err := g.TestTagged(ctx, "cluster", "Rejected$"); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("wariant negatywny przepuszczony: %v", err)
	}
}

func TestCompileTests(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"go test -race -coverpkg=./... -c -o /tmp/w/race/ ./...": {},
		"go test -tags docker -c -o /tmp/w/docker/ ./...":        {},
	}}
	g := Go{ModuleDir: "/m", Runner: f}
	ctx := context.Background()
	if err := g.CompileTests(ctx, "/tmp/w/race", true, true, ""); err != nil || !strings.Contains(strings.Join(f.Calls[0].Env, " "), "CGO_ENABLED=1") {
		t.Errorf("testy z -race: %v, %v", f.Calls[0].Env, err)
	}
	if err := g.CompileTests(ctx, "/tmp/w/docker/", false, false, "docker"); err != nil || len(f.Calls[1].Env) != 0 {
		t.Errorf("testy z tagiem: %v, %v", f.Calls[1].Env, err)
	}
}
