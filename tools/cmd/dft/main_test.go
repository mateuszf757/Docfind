package main

import (
	"context"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
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
		{nil, environment.SourceLocal},
		{[]string{"--image", "ghcr.io/x/api@" + digest}, environment.SourceRegistry},
		{[]string{"--image-archive", "obraz.tar", "--config-digest", digest}, environment.SourceArchive},
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
		"ustawienia z nieznaną flagą":   func() error { return runRepoSettings(ctx, w, []string{"--force"}) },
		"tożsamość bez pola":            func() error { return runIdentity(ctx, w, nil) },
		"promocja bez numeru wydania":   func() error { return runPromote(ctx, w, []string{"ghcr.io/x/api@sha256:abc"}) },
		"wydanie bez numeru":            func() error { return runRelease(ctx, w, nil) },
		"wydanie w CI bez numeru":       func() error { return runReleaseImage(ctx, w, nil) },
		"tag obrazu z wersji":           func() error { return runIdentity(ctx, w, []string{"docker-tag"}) },
	} {
		if err := call(); cli.ExitCode(err) != cli.ExitFailure {
			t.Errorf("%s: kod %d (%v), oczekiwano złego wywołania", name, cli.ExitCode(err), err)
		}
	}
	if calls := w.runner.(*proc.Fake).Calls; len(calls) > 0 {
		t.Errorf("przed odmową uruchomiono %v", calls)
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
