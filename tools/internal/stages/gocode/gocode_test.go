package gocode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/coverage"
	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

func silence(t *testing.T) *bytes.Buffer {
	t.Helper()
	var errOut bytes.Buffer
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = &bytes.Buffer{}, &errOut
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
	return &errOut
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGofmt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dobry.go", "package x\n\nfunc A() {}\n")
	// testdata i katalogi ukryte nie są kodem modułu.
	write(t, dir, "testdata/zly.go", "package x\nfunc  B(){}\n")
	write(t, dir, ".cache/zly.go", "package x\nfunc  B(){}\n")

	c := Checker{ModuleDir: dir}
	if err := c.Gofmt(); err != nil {
		t.Fatalf("sformatowany moduł odrzucony: %v", err)
	}
	write(t, dir, "pakiet/zly.go", "package pakiet\nfunc  B(){}\n")
	err := c.Gofmt()
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), filepath.Join("pakiet", "zly.go")) {
		t.Errorf("oczekiwano niespełnionego z nazwą pliku, jest %v", err)
	}
}

// TestLayers: moduł z etapem na portach przechodzi; etap importujący
// adapter — wariant negatywny — jest odrzucany, a ten sam import w teście
// etapu nie.
func TestLayers(t *testing.T) {
	errOut := silence(t)
	dir := t.TempDir()
	const m = "example.com/narzedzia"
	write(t, dir, "go.mod", "module "+m+"\n\ngo 1.27\n")
	write(t, dir, "internal/core/a/a.go", "package a\n")
	write(t, dir, "internal/adapters/b/b.go", "package b\n\nimport (\n\t\"os/exec\"\n\n\t\""+m+"/internal/core/a\"\n)\n\nvar _ = exec.Command\nvar _ = a.X\n")
	write(t, dir, "internal/stages/c/c.go", "package c\n\nimport \""+m+"/internal/core/a\"\n\nvar _ = a.X\n")
	write(t, dir, "internal/stages/c/c_test.go", "package c\n\nimport \""+m+"/internal/adapters/b\"\n\nvar _ = b.X\n")
	write(t, dir, "cmd/x/main.go", "package main\n\nimport (\n\t\""+m+"/internal/adapters/b\"\n\t\""+m+"/internal/stages/c\"\n)\n\nfunc main() {}\n")
	c := Checker{ModuleDir: dir}
	if err := c.Layers(); err != nil {
		t.Fatalf("poprawny moduł odrzucony: %v\n%s", err, errOut)
	}

	write(t, dir, "internal/stages/c/zly.go", "package c\n\nimport \""+m+"/internal/adapters/b\"\n\nvar _ = b.X\n")
	err := c.Layers()
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(errOut.String(), "internal/stages/c (stages) importuje "+m+"/internal/adapters/b") {
		t.Errorf("etap importujący adapter przyjęty: %v\n%s", err, errOut)
	}
}

// TestRepositoryLayers: kod narzędzi w repozytorium spełnia reguły warstw.
func TestRepositoryLayers(t *testing.T) {
	errOut := silence(t)
	if err := (Checker{ModuleDir: "../../.."}).Layers(); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
}

// toolchain — atrapa toolchainu Go.
type toolchain struct {
	cc       bool
	tested   []bool
	findings []vuln.Finding
	// baseline — ustalenia w kodzie z punktu odniesienia, baselineDB — data
	// bazy w jego skanie (pusta — ta sama co w bieżącym).
	baseline   []vuln.Finding
	baselineDB string
	// builds — zawartość binarki dft po GOCACHE (od zera) i bez.
	cached, fresh string
	// profile — profil pokrycia zapisywany przez Test.
	profile string
	// compiled — wywołania CompileTests: race, pokrycie, tag.
	compiled []string
}

func (f *toolchain) Vet(context.Context) error         { return nil }
func (f *toolchain) Staticcheck(context.Context) error { return nil }
func (f *toolchain) Test(_ context.Context, race bool, coverprofile string) error {
	f.tested = append(f.tested, race)
	if coverprofile != "" {
		return os.WriteFile(coverprofile, []byte(f.profile), 0o644)
	}
	return nil
}
func (f *toolchain) CCompiler(context.Context) (string, bool) { return "gcc", f.cc }
func (f *toolchain) CompileTests(_ context.Context, out string, race, cover bool, tag string) error {
	f.compiled = append(f.compiled, fmt.Sprintf("%s race=%v cover=%v tag=%s", filepath.Base(out), race, cover, tag))
	return nil
}

const vulnDB = "2026-10-08T22:31:09Z"

func (f *toolchain) Vulncheck(_ context.Context, dir string) ([]vuln.Finding, vuln.Database, error) {
	if dir == "" {
		return f.findings, vuln.Database{Ecosystem: "Go", Newest: vulnDB}, nil
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return nil, vuln.Database{}, fmt.Errorf("punkt odniesienia bez modułu: %w", err)
	}
	db := vuln.Database{Ecosystem: "Go", Newest: vulnDB}
	if f.baselineDB != "" {
		db.Newest = f.baselineDB
	}
	return f.baseline, db, nil
}

// sources — repozytorium z commitami po nazwie; Export zapisuje go.mod.
type sources map[string]bool

func (s sources) Export(_ context.Context, rev, dir string) error {
	if !s[rev] {
		return fmt.Errorf("%s: %w", rev, artifact.ErrUnknownCommit)
	}
	return os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n"), 0o644)
}
func (f *toolchain) Build(_ context.Context, pkg, output string, env ...string) error {
	content := f.cached
	if len(env) > 0 && strings.HasPrefix(env[0], "GOCACHE=") {
		content = f.fresh
	}
	return os.WriteFile(filepath.Join(output, "dft"), []byte(content), 0o755)
}

// TestRace: testy z -race tam, gdzie jest kompilator C; w CI jego brak to
// awaria, a lokalnie ostrzeżenie i testy bez -race.
func TestRace(t *testing.T) {
	silence(t)
	tests := []struct {
		name        string
		cc          bool
		requireRace bool
		wantRace    []bool
		wantCode    int
	}{
		{"kompilator jest", true, true, []bool{true}, cli.ExitOK},
		{"brak kompilatora lokalnie", false, false, []bool{false}, cli.ExitOK},
		{"brak kompilatora w CI", false, true, nil, cli.ExitFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := &toolchain{cc: tt.cc}
			err := Checker{Go: tc, RequireRace: tt.requireRace}.Test(context.Background())
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			if len(tc.tested) != len(tt.wantRace) || (len(tt.wantRace) > 0 && tc.tested[0] != tt.wantRace[0]) {
				t.Errorf("testy z -race: %v, oczekiwano %v", tc.tested, tt.wantRace)
			}
		})
	}
}

// TestVulncheckException: ważny wyjątek z zakresu go zdejmuje blokadę,
// przeterminowany — nie.
func TestVulncheckException(t *testing.T) {
	silence(t)
	reachable := []vuln.Finding{{ID: "GO-2026-0001", Package: "k8s.io/client-go", Severity: vuln.SeverityUnknown, Fixed: []string{"v0.36.6"}}}
	today := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for expires, want := range map[string]int{"2026-10-20": cli.ExitOK, "2026-10-07": cli.ExitUnmet} {
		c := Checker{Go: &toolchain{findings: reachable}, Today: today,
			Exceptions: []vuln.Exception{{ID: "GO-2026-0001", Package: "k8s.io/client-go", Expires: expires, Reason: "aktualizacja client-go czeka na k3s 1.37", Scope: vuln.ScopeGo}}}
		if code := cli.ExitCode(c.Vulncheck(context.Background())); code != want {
			t.Errorf("wyjątek do %s: kod %d, oczekiwano %d", expires, code, want)
		}
	}
}

// TestVulncheckModes: tryby z decyzji 37. Z punktem odniesienia blokuje
// tylko to, czego nie było w kodzie z commita bazowego, w trybie raportu
// nic, a raport dostaje Publish także przy wyniku negatywnym — przy
// awarii raportu nie ma.
func TestVulncheckModes(t *testing.T) {
	silence(t)
	old := vuln.Finding{ID: "GO-2026-0001", Package: "stdlib", Version: "v1.27.1", Severity: vuln.SeverityUnknown, Fixed: []string{"v1.27.2"}}
	added := vuln.Finding{ID: "GO-2026-0002", Package: "golang.org/x/net", Version: "v0.59.0", Severity: vuln.SeverityUnknown, Fixed: []string{"v0.60.0"}}
	tests := []struct {
		name         string
		tc           toolchain
		baseline     string
		reportOnly   bool
		wantCode     int
		wantBlocking int
		wantBaseline string
		wantReport   bool
	}{
		{"bez punktu odniesienia", toolchain{findings: []vuln.Finding{old, added}}, "", false, cli.ExitUnmet, 2, "", true},
		{"nowa podatność względem bazy", toolchain{findings: []vuln.Finding{old, added}, baseline: []vuln.Finding{old}}, "abc", false, cli.ExitUnmet, 1, "tools/ z abc", true},
		{"tylko podatności z bazy", toolchain{findings: []vuln.Finding{old}, baseline: []vuln.Finding{old}}, "abc", false, cli.ExitOK, 0, "tools/ z abc", true},
		{"tylko raport", toolchain{findings: []vuln.Finding{old, added}}, "", true, cli.ExitOK, 2, "", true},
		{"brak commita bazowego", toolchain{findings: []vuln.Finding{old}}, "nieznany", false, cli.ExitUnmet, 1, "", true},
		{"baza zmieniła się między skanami", toolchain{findings: []vuln.Finding{old}, baseline: []vuln.Finding{old}, baselineDB: "2026-10-09T08:00:00Z"}, "abc", false, cli.ExitFailure, 0, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var published []vuln.Report
			c := Checker{Go: &tt.tc, Today: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC),
				VulnBaseline: tt.baseline, Sources: sources{"abc": true}, VulnReportOnly: tt.reportOnly,
				Publish: func(rep vuln.Report) error { published = append(published, rep); return nil }}
			err := c.Vulncheck(context.Background())
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			if !tt.wantReport {
				if len(published) != 0 {
					t.Errorf("raport po awarii: %+v", published)
				}
				return
			}
			if len(published) != 1 {
				t.Fatalf("raporty: %+v", published)
			}
			rep := published[0]
			if rep.Blocking != tt.wantBlocking || rep.Baseline != tt.wantBaseline || rep.ReportOnly != tt.reportOnly || len(rep.Databases) != 1 {
				t.Errorf("raport: %+v", rep)
			}
		})
	}
}

func TestReproducible(t *testing.T) {
	silence(t)
	binaries, err := Checker{Go: &toolchain{cached: "z-pamieci", fresh: "od-zera"}}.Reproducible(context.Background(), t.TempDir())
	if cli.ExitCode(err) != cli.ExitUnmet {
		t.Fatalf("różne binarki: kod %d (%v)", cli.ExitCode(err), err)
	}
	if len(binaries) != 1 || binaries[0].Name != "dft" {
		t.Errorf("binarki: %+v", binaries)
	}
	if _, err := (Checker{Go: &toolchain{cached: "ta-sama", fresh: "ta-sama"}}).Reproducible(context.Background(), t.TempDir()); err != nil {
		t.Errorf("te same bajty: %v", err)
	}
}

// TestAll: niespełnione bramki zgłaszane razem, awaria przerywa od razu.
func TestAll(t *testing.T) {
	silence(t)
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/m\n")
	write(t, dir, "internal/core/a/a.go", "package a\nfunc  B(){}\n")
	err := Checker{ModuleDir: dir, Go: &toolchain{cc: true}}.All(context.Background())
	if cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("niesformatowany plik: %v", err)
	}
	broken := &brokenToolchain{toolchain{cc: true}}
	write(t, dir, "internal/core/a/a.go", "package a\n")
	if err := (Checker{ModuleDir: dir, Go: broken}).All(context.Background()); cli.ExitCode(err) != cli.ExitFailure || !strings.Contains(err.Error(), "go vet") {
		t.Errorf("awaria go vet: %v", err)
	}
}

type brokenToolchain struct{ toolchain }

func (*brokenToolchain) Vet(context.Context) error { return errors.New("brak go") }

// TestCoverage: pokrycie z profilu trafia do raportu, a pakiet poniżej
// minimum swojej warstwy to NIESPEŁNIONE (decyzja 62).
func TestCoverage(t *testing.T) {
	silence(t)
	dir := t.TempDir()
	const m = "example.com/narzedzia"
	write(t, dir, "go.mod", "module "+m+"\n\ngo 1.27\n")
	profile := "mode: atomic\n" +
		m + "/internal/stages/a/a.go:1.1,2.2 10 1\n" +
		m + "/internal/kit/b/b.go:1.1,2.2 10 0\n" +
		m + "/cmd/x/main.go:1.1,2.2 10 0\n"
	var got map[string]float64
	c := Checker{ModuleDir: dir, Go: &toolchain{cc: true, profile: profile}, Coverage: func(packages map[string]coverage.Stat, total coverage.Stat) error {
		got = map[string]float64{"total": total.Percent()}
		for pkg, s := range packages {
			got[pkg] = s.Percent()
		}
		return nil
	}}
	err := c.Test(context.Background())
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "pokrycie pakietów poniżej minimum") {
		t.Fatalf("kit z 0%%: %v", err)
	}
	if got["internal/stages/a"] != 100 || got["cmd/x"] != 0 || got["total"] < 33 || got["total"] > 34 {
		t.Errorf("raport pokrycia: %v", got)
	}
	c.Go = &toolchain{cc: true, profile: "mode: atomic\n" + m + "/internal/kit/b/b.go:1.1,2.2 10 1\n"}
	if err := c.Test(context.Background()); err != nil {
		t.Errorf("pokrycie powyżej minimum: %v", err)
	}
}

func TestWarm(t *testing.T) {
	silence(t)
	tc := &toolchain{cc: true}
	c := Checker{ModuleDir: "../../..", Go: tc, RequireRace: true, Coverage: func(map[string]coverage.Stat, coverage.Stat) error { return nil }}
	if err := c.Warm(context.Background(), "docker"); err != nil {
		t.Fatal(err)
	}
	want := []string{"test race=true cover=true tag=", "docker race=false cover=false tag=docker"}
	if !slices.Equal(tc.compiled, want) {
		t.Errorf("kompilacje: %q, oczekiwano %q", tc.compiled, want)
	}
	if len(tc.tested) != 0 {
		t.Errorf("Warm uruchomił testy: %v", tc.tested)
	}
	// W CI bez kompilatora C pisarz nie może zapisać pamięci bez -race —
	// bramki szukałyby wpisów, których nie ma.
	if err := (Checker{Go: &toolchain{}, RequireRace: true}).Warm(context.Background()); err == nil {
		t.Error("brak kompilatora C w CI przepuszczony")
	}
}
