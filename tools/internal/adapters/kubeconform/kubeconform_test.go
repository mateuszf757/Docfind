package kubeconform

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

func TestValidate(t *testing.T) {
	var args string
	code := 0
	runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
		args = strings.Join(c.Args, " ")
		if code != 0 {
			return proc.Result{}, &proc.ExitError{Cmd: c.String(), Code: code}
		}
		return proc.Result{}, nil
	})
	k := Kubeconform{Runner: runner, KubernetesVersion: "1.36.4", SchemaCommit: "c9452f", CacheDir: filepath.Join(t.TempDir(), "cache")}
	if err := k.Validate(context.Background(), "render.yaml", "/tmp/crd"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-strict", "-kubernetes-version 1.36.4", "kubernetes-json-schema/c9452f/", "/tmp/crd/{{ .Group }}", "-skip CustomResourceDefinition", "render.yaml"} {
		if !strings.Contains(args, want) {
			t.Errorf("kubeconform bez %q: %s", want, args)
		}
	}
	if strings.Contains(args, "-ignore-missing-schemas") {
		t.Error("-ignore-missing-schemas wyłącza walidację CRD")
	}
	code = 1
	if err := k.Validate(context.Background(), "render.yaml", "/tmp/crd"); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("manifest odrzucony: %v", err)
	}
}
