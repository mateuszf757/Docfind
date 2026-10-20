package main

import (
	"context"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
	"github.com/mateuszf757/Docfind/tools/internal/stages/plan"
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

func TestSourceArgs(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	tests := []struct {
		args []string
		want string
	}{
		// Bez flag źródło wybiera definicja: build z drzewa albo Argo CD.
		{nil, ""},
		{[]string{"--image", "ghcr.io/x/api@" + digest}, environment.SourceRegistry},
		{[]string{"--image-archive", "obraz.tar", "--config-digest", digest}, environment.SourceArchive},
		{[]string{"--image", "ghcr.io/x/api@" + digest, "--chart-package"}, environment.SourceRegistry},
	}
	for _, tt := range tests {
		got, err := sourceArgs(tt.args)
		if err != nil || got.Kind != tt.want {
			t.Errorf("%v: %+v, %v", tt.args, got, err)
		}
	}
	for _, bad := range [][]string{
		{"--image-archive", "obraz.tar"},
		{"--image", "x@" + digest, "--image-archive", "obraz.tar", "--config-digest", digest},
		{"pozycyjny"},
		{"--nieznana"},
		// Spakowany chart tylko z obrazem z rejestru.
		{"--chart-package"},
		{"--image-archive", "obraz.tar", "--config-digest", digest, "--chart-package"},
	} {
		if _, err := sourceArgs(bad); cli.ExitCode(err) != cli.ExitFailure {
			t.Errorf("%v: przyjęte (%v)", bad, err)
		}
	}
}

func TestEnvFlag(t *testing.T) {
	for value, want := range map[string]bool{"": false, "0": false, "1": true} {
		t.Setenv("DFT_TEST_FLAG", value)
		if got, err := envFlag("DFT_TEST_FLAG"); err != nil || got != want {
			t.Errorf("%q: %v, %v", value, got, err)
		}
	}
	t.Setenv("DFT_TEST_FLAG", "tak")
	if _, err := envFlag("DFT_TEST_FLAG"); cli.ExitCode(err) != cli.ExitFailure {
		t.Errorf("wartość spoza 0/1 przyjęta: %v", err)
	}
}

// TestUsageErrors: złe wywołania odpadają przed uruchomieniem czegokolwiek —
// Fake bez nagranych odpowiedzi zawiódłby przy pierwszym poleceniu.
func TestUsageErrors(t *testing.T) {
	quiet(t)
	w := &workspace{root: t.TempDir(), runner: &proc.Fake{}}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"nieznane polecenie":            func() error { return run(ctx, []string{"nieznane"}) },
		"bez polecenia":                 func() error { return run(ctx, nil) },
		"nieznana bramka":               func() error { return runCheck(ctx, w, []string{"nieznana"}) },
		"bramka bez nazwy":              func() error { return runCheck(ctx, w, nil) },
		"próg rozmiaru nie liczbą":      func() error { return runCheckRuntime(ctx, w, []string{"api", "--max-image-bytes", "dużo"}) },
		"próg rozmiaru zerowy":          func() error { return runCheckRuntime(ctx, w, []string{"--max-image-bytes", "0"}) },
		"dwa źródła skanu":              func() error { return runCheckVulns(ctx, w, []string{"--image", "x", "--image-archive", "y"}) },
		"digest konfiguracji bez pliku": func() error { return runCheckVulns(ctx, w, []string{"--config-digest", "sha256:x"}) },
		"klaster bez podpolecenia":      func() error { return runCluster(ctx, w, nil) },
		"down z argumentem":             func() error { return runCluster(ctx, w, []string{"down", "teraz"}) },
		"okres próbny bez N":            func() error { return runTrial(ctx, w, []string{"cluster"}) },
		"okres próbny z N=0":            func() error { return runTrial(ctx, w, []string{"0", "cluster"}) },
		"PR-y zadania spoza listy":      func() error { return runTrial(ctx, w, []string{"--pull-requests", "vulns", "10", "cluster"}) },
		"ustawienia z nieznaną flagą":   func() error { return runRepoSettings(ctx, w, []string{"--force"}) },
		"ustawienia z dwoma trybami":    func() error { return runRepoSettings(ctx, w, []string{"--check", "--dry-run"}) },
		"ci z nieznanym poleceniem":     func() error { return runCI(ctx, w, []string{"koszt"}) },
		"budżet z nieznaną flagą":       func() error { return runCI(ctx, w, []string{"budget", "--force"}) },
		"tożsamość bez pola":            func() error { return runIdentity(ctx, w, nil) },
		"promocja bez numeru wydania":   func() error { return runPromote(ctx, w, []string{"ghcr.io/x/api@sha256:abc"}) },
		"wydanie bez numeru":            func() error { return runRelease(ctx, w, nil) },
		"wydanie w CI bez numeru":       func() error { return runReleaseImage(ctx, w, nil) },
		"tag obrazu z wersji":           func() error { return runIdentity(ctx, w, []string{"docker-tag"}) },
		"test z argumentem":             func() error { return runTest(ctx, w, []string{"go"}) },
		"test na PR-ze bez bazy":        func() error { return runTest(ctx, w, []string{"--event", "pull_request"}) },
		"test na PR-ze z trybem raportu": func() error {
			return runTest(ctx, w, []string{"--event", "pull_request", "--base", "abc", "--vulns-report-only"})
		},
		"test na push z bazą":               func() error { return runTest(ctx, w, []string{"--event", "push", "--base", "abc"}) },
		"test z dwoma punktami odniesienia": func() error { return runTest(ctx, w, []string{"--base", "a", "--vulns-baseline", "b"}) },
		"kontrakt bez podpolecenia":         func() error { return runContract(ctx, w, nil) },
		"ci bez podpolecenia":               func() error { return runCI(ctx, w, nil) },
		"kontrakt z nieznaną flagą":         func() error { return runContract(ctx, w, []string{"thresholds", "--force"}) },
		"bramka kontraktu z argumentem":     func() error { return runCheckContract(ctx, w, []string{"origin/main"}) },
		"bramka Go z nieznaną flagą":        func() error { return runCheckGo(ctx, w, []string{"--baseline-commit", "abc"}) },
		"issue z pliku zamiast katalogu": func() error {
			return runVulnsIssue(ctx, w, []string{filepath.Join(w.root, "vulns-api.json")})
		},
	} {
		if err := call(); cli.ExitCode(err) != cli.ExitFailure {
			t.Errorf("%s: kod %d (%v), oczekiwano złego wywołania", name, cli.ExitCode(err), err)
		}
	}
	if calls := w.runner.(*proc.Fake).Calls; len(calls) > 0 {
		t.Errorf("przed odmową uruchomiono %v", calls)
	}
}

// TestParseTestFlags: zdarzenie z CI wybiera tryb, lokalnie decydują flagi.
func TestParseTestFlags(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want testMode
	}{
		{[]string{"--event", "pull_request", "--base", "abc"}, testMode{vulns: goVulnMode{baseline: "abc"}, base: "abc"}},
		{[]string{"--event", "push", "--base", ""}, testMode{vulns: goVulnMode{reportOnly: true}}},
		{[]string{"--event", "schedule"}, testMode{vulns: goVulnMode{reportOnly: true}}},
		{nil, testMode{}},
		{[]string{"--base", "origin/main"}, testMode{vulns: goVulnMode{baseline: "origin/main"}, base: "origin/main"}},
		{[]string{"--vulns-baseline", "origin/main"}, testMode{vulns: goVulnMode{baseline: "origin/main"}}},
		{[]string{"--vulns-report-only"}, testMode{vulns: goVulnMode{reportOnly: true}}},
		{[]string{"--part", "lint", "--event", "pull_request", "--base", "abc"}, testMode{vulns: goVulnMode{baseline: "abc"}, base: "abc", part: "lint"}},
		{[]string{"--part", "go", "--event", "push", "--base", ""}, testMode{vulns: goVulnMode{reportOnly: true}, part: "go"}},
	} {
		got, err := parseTestFlags(tt.args)
		if err != nil || got != tt.want {
			t.Errorf("parseTestFlags(%q) = %+v, %v; oczekiwano %+v", tt.args, got, err, tt.want)
		}
	}
	for _, args := range [][]string{{"--part", "python"}, {"--part", "go", "--event", "push", "--base", "abc"}} {
		if _, err := parseTestFlags(args); cli.ExitCode(err) != cli.ExitFailure {
			t.Errorf("parseTestFlags(%q) przyjęte: %v", args, err)
		}
	}
}

// TestCIRequire: zadanie zbierające zielone tylko przy samych sukcesach;
// brak NEEDS, zły JSON i puste needs to złe wywołanie, a nie wynik.
func TestCIRequire(t *testing.T) {
	quiet(t)
	w := &workspace{root: t.TempDir(), runner: &proc.Fake{}}
	ctx := context.Background()
	for needs, want := range map[string]int{
		`{"test":{"result":"success","outputs":{}},"build":{"result":"success"}}`: cli.ExitOK,
		`{"test":{"result":"success"},"build":{"result":"skipped"}}`:              cli.ExitUnmet,
		`{"test":{"result":"failure"}}`:                                           cli.ExitUnmet,
		`{}`:                                                                      cli.ExitFailure,
		`nie JSON`:                                                                cli.ExitFailure,
		``:                                                                        cli.ExitFailure,
	} {
		t.Setenv("NEEDS", needs)
		if code := cli.ExitCode(runCI(ctx, w, []string{"require"})); code != want {
			t.Errorf("NEEDS=%s: kod %d, oczekiwano %d", needs, code, want)
		}
	}
}

// TestRepositoryContract: kontrakt w repozytorium przechodzi własne
// sprawdzenie — z prawdziwymi poleceniami, bramkami, zadaniami i decyzjami.
func TestRepositoryContract(t *testing.T) {
	quiet(t)
	w := &workspace{root: filepath.Join("..", "..", ".."), runner: &proc.Fake{}}
	if err := checkContract(context.Background(), w, ""); err != nil {
		t.Fatal(err)
	}
}

// TestReleaseBuildRefused: build wydania nie istnieje od decyzji 40 —
// RELEASE=1 kończy się złym wywołaniem z instrukcją, zanim cokolwiek ruszy.
func TestReleaseBuildRefused(t *testing.T) {
	quiet(t)
	t.Setenv("RELEASE", "1")
	w := &workspace{root: t.TempDir(), runner: &proc.Fake{}}
	err := runBuild(context.Background(), w, nil)
	if cli.ExitCode(err) != cli.ExitFailure || !strings.Contains(err.Error(), "./bin/mise run release") {
		t.Errorf("RELEASE=1: %v", err)
	}
}

// TestHelpCoversChecks: każda bramka `dft check` jest w podpowiedzi.
func TestHelpCoversChecks(t *testing.T) {
	for _, name := range slices.Sorted(maps.Keys(checks)) {
		if !strings.Contains(checkUsage, name) {
			t.Errorf("bramka %s bez opisu w checkUsage", name)
		}
	}
	for name, c := range commands {
		if c.usage == "" || c.help == "" || !strings.HasPrefix(c.usage, "dft "+name) {
			t.Errorf("polecenie %s: użycie %q, opis %q", name, c.usage, c.help)
		}
	}
}

// Graf pakietów klastrowych repozytorium: bramki klastrowe w nim, bramki
// obrazu i budżety poza nim.
func TestRepositoryClusterDirs(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	w := &workspace{root: root, runner: proc.Exec{}}
	dirs, err := plan.Planner{ModuleDir: filepath.Join(root, "tools"), Packages: w.gotool()}.ClusterDirs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"tools/internal/stages/drain", "tools/internal/stages/deploy", "tools/internal/adapters/kube", "tools/internal/adapters/k3d", "tools/internal/adapters/helm", "tools/internal/core/environment"} {
		if !dirs[dir] {
			t.Errorf("%s poza grafem pakietów klastrowych", dir)
		}
	}
	for _, dir := range []string{"tools/internal/stages/budget", "tools/internal/stages/lifecycle", "tools/internal/stages/gocode"} {
		if dirs[dir] {
			t.Errorf("%s w grafie pakietów klastrowych", dir)
		}
	}
}
