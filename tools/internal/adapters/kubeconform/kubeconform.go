// Package kubeconform to adapter kubeconform: walidacja wyrenderowanych
// manifestów schematami zasobów Kubernetesa z przypiętego commita
// repozytorium schematów i schematami CRD wygenerowanymi z przypiętych
// chartów (decyzja 28).
package kubeconform

import (
	"context"
	"io"
	"os"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Kubeconform — program kubeconform z ustawieniami walidacji.
type Kubeconform struct {
	Runner proc.Runner
	Out    io.Writer
	Err    io.Writer
	// KubernetesVersion — wersja X.Y.Z klastra (DF_KUBERNETES_VERSION).
	KubernetesVersion string
	// SchemaCommit — commit repozytorium yannh/kubernetes-json-schema
	// (DF_KUBECONFORM_SCHEMA_COMMIT) zamiast domyślnej gałęzi master.
	SchemaCommit string
	// CacheDir — pamięć podręczna pobranych schematów.
	CacheDir string
}

// Validate sprawdza manifesty z pliku. Schematy CRD leżą w crdSchemas
// w układzie <grupa>/<rodzaj>_<wersja>.json (core/crdschema).
//
// -strict odrzuca pola, których nie ma w schemacie: literówka w nazwie pola
// manifestu jest inaczej po cichu ignorowana przez API server. Nigdy
// -ignore-missing-schemas: przy CRD wyłączyłoby walidację bez słowa. -skip
// CustomResourceDefinition: repozytorium schematów nie ma schematu dla
// samego rodzaju CRD; pominięty jest dokładnie ten jeden rodzaj, a definicje
// CRD przychodzą wyłącznie z przypiętych chartów.
func (k Kubeconform) Validate(ctx context.Context, manifests, crdSchemas string) error {
	// {{ … }} rozwija kubeconform.
	schemas := "https://raw.githubusercontent.com/yannh/kubernetes-json-schema/" + k.SchemaCommit +
		"/{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json"
	crd := crdSchemas + "/{{ .Group }}/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json"
	if err := os.MkdirAll(k.CacheDir, 0o755); err != nil {
		return err
	}
	_, err := k.Runner.Run(ctx, proc.Cmd{Name: "kubeconform", Args: []string{
		"-strict", "-summary", "-kubernetes-version", k.KubernetesVersion,
		"-schema-location", schemas, "-schema-location", crd,
		"-skip", "CustomResourceDefinition", "-cache", k.CacheDir, manifests,
	}, Stdout: k.Out, Stderr: k.Err})
	if code := proc.ExitCode(err); code > 0 {
		return cli.Unmet("kubeconform %s: kod %d", manifests, code)
	}
	return err
}
