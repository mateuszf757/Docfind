package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/mateuszf757/Docfind/tools/internal/attest"
	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/env"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
	"github.com/mateuszf757/Docfind/tools/internal/registry"
)

const repoRoot = "../../.."

func TestK3dConfig(t *testing.T) {
	e := env.Environment{Cluster: env.Cluster{Name: "docfind-ci", Ports: env.Ports{API: 6551, HTTP: 8081, HTTPS: 8444}}}
	raw, err := K3dConfig(filepath.Join(repoRoot, "deploy", "k3d", "cluster.yaml"), e)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{`"name": "docfind-ci"`, `"hostPort": "6551"`, `"127.0.0.1:8081:80"`, `"127.0.0.1:8444:443"`, `"--disable=traefik"`} {
		if !strings.Contains(out, want) {
			t.Errorf("konfiguracja bez %s:\n%s", want, out)
		}
	}
}

// TestK3dConfigRejectsWildcard: mapowanie na 0.0.0.0 to klaster osiągalny
// z sieci — odrzucone, a nie przepisane po cichu (decyzja 17).
func TestK3dConfigRejectsWildcard(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "k3d", "cluster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ from, to string }{
		{"port: 127.0.0.1:8080:80", "port: 0.0.0.0:8080:80"},
		{"  hostIP: 127.0.0.1", "  hostIP: 0.0.0.0"},
	} {
		path := filepath.Join(t.TempDir(), "cluster.yaml")
		bad := strings.Replace(string(src), mutation.from, mutation.to, 1)
		if bad == string(src) {
			t.Fatalf("podmiana %q nie zadziałała", mutation.from)
		}
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := K3dConfig(path, env.Environment{Cluster: env.Cluster{Name: "x", Ports: env.Ports{API: 6550, HTTP: 8080, HTTPS: 8443}}})
		if cli.ExitCode(err) != cli.ExitUnmet {
			t.Errorf("%s: przyjęte (%v)", mutation.to, err)
		}
	}
}

const apiImage = "ghcr.io/mateuszf757/docfind-api"

func definition(t *testing.T, name string) env.Environment {
	t.Helper()
	for _, key := range []string{"DOCFIND_HOSTNAME", "DOCFIND_TLS_ISSUER", "DOCFIND_ACME_EMAIL", "DOCFIND_ACME_ZONE"} {
		t.Setenv(key, "")
	}
	e, err := env.Load(repoRoot, name)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func labelled(t *testing.T, labels map[string]string) v1.Image {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Config(img, v1.Config{Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// saved zapisuje obraz jak `docker save` i zwraca ścieżkę i digest konfiguracji.
func saved(t *testing.T, img v1.Image) (string, string) {
	t.Helper()
	tag, err := name.NewTag(apiImage + ":0.0.0-dev.1-abc1234")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "image.tar")
	if err := tarball.WriteToFile(path, tag, img); err != nil {
		t.Fatal(err)
	}
	config, err := img.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	return path, config.String()
}

var commit = strings.Repeat("c", 40)

func TestArchiveImage(t *testing.T) {
	d := Deployer{Env: definition(t, "ci"), Image: apiImage}
	img := labelled(t, map[string]string{registry.VersionLabel: "0.0.0-dev.1+abc1234", registry.RevisionLabel: commit})
	path, config := saved(t, img)

	got, err := d.archiveImage(Artifact{Source: env.SourceArchive, Archive: path, ConfigDigest: config}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wantRef := apiImage + ":local-" + strings.TrimPrefix(config, "sha256:")[:12]
	if got.want.Image != wantRef || got.want.Version != "0.0.0-dev.1+abc1234" || got.want.Commit != commit || got.digest != "" {
		t.Errorf("obraz z archiwum: %+v, oczekiwano %s", got, wantRef)
	}
	// Archiwum do importu niesie nowy tag, a obraz zostaje ten sam.
	loaded, err := tarball.ImageFromPath(got.load, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := loaded.ConfigName(); c.String() != config {
		t.Errorf("archiwum do importu: konfiguracja %s, oczekiwano %s", c, config)
	}

	t.Run("inna konfiguracja niż sprawdzona w zadaniu build", func(t *testing.T) {
		_, err := d.archiveImage(Artifact{Source: env.SourceArchive, Archive: path, ConfigDigest: "sha256:" + strings.Repeat("0", 64)}, t.TempDir())
		if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "zadanie build sprawdziło") {
			t.Errorf("podmienione archiwum przyjęte: %v", err)
		}
	})
	t.Run("obraz bez etykiet", func(t *testing.T) {
		path, config := saved(t, labelled(t, nil))
		_, err := d.archiveImage(Artifact{Source: env.SourceArchive, Archive: path, ConfigDigest: config}, t.TempDir())
		if cli.ExitCode(err) != cli.ExitUnmet {
			t.Errorf("obraz bez wersji przyjęty: %v", err)
		}
	})
}

func TestRegistryImage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	attested := []string{registry.ProvenancePredicates[0], registry.SBOMPredicate}
	inspect := func(version string, predicates []string) func(context.Context, string) (registry.Inspected, error) {
		return func(_ context.Context, ref string) (registry.Inspected, error) {
			if ref != apiImage+"@"+digest {
				return registry.Inspected{}, fmt.Errorf("nieoczekiwany ref %s", ref)
			}
			return registry.Inspected{
				Published: registry.Published{Index: digest, Predicates: predicates},
				Labels:    map[string]string{registry.VersionLabel: version, registry.RevisionLabel: commit},
			}, nil
		}
	}
	// signed — atestacja GitHuba z podanego źródła; zapamiętuje wymagany tag.
	var askedSourceRef string
	signed := func(sourceCommit string, refuse bool) func(context.Context, string, string) (attest.Result, error) {
		return func(_ context.Context, ref, sourceRef string) (attest.Result, error) {
			askedSourceRef = sourceRef
			if refuse {
				return attest.Result{}, cli.Unmet("atestacja GitHuba dla %s nie przeszła weryfikacji", ref)
			}
			return attest.Result{PredicateType: attest.ProvenanceV1, SourceRef: sourceRef, SourceCommit: sourceCommit}, nil
		}
	}
	tests := []struct {
		name, env, ref, version string
		predicates              []string
		attest                  func(context.Context, string, string) (attest.Result, error)
		wantCode                int
		wantImage               string
		wantSourceRef           string
	}{
		{"obraz z main na staging", "staging", apiImage + "@" + digest, "0.3.1-dev.4+abc1234", attested, signed(commit, false), cli.ExitOK, apiImage + ":0.3.1-dev.4-abc1234@" + digest, ""},
		{"tag w referencji niczego nie wybiera", "staging", apiImage + ":latest@" + digest, "0.3.1-dev.4+abc1234", attested, signed(commit, false), cli.ExitOK, apiImage + ":0.3.1-dev.4-abc1234@" + digest, ""},
		{"wydanie na produkcję z tagu swojej wersji", "prod", apiImage + "@" + digest, "0.3.1", attested, signed(commit, false), cli.ExitOK, apiImage + ":0.3.1@" + digest, "refs/tags/v0.3.1"},
		{"obraz z main na produkcję", "prod", apiImage + "@" + digest, "0.3.1-dev.4+abc1234", attested, signed(commit, false), cli.ExitUnmet, "", ""},
		{"obcy obraz", "staging", "docker.io/library/nginx@" + digest, "0.3.1", attested, signed(commit, false), cli.ExitUnmet, "", ""},
		{"tag zamiast digestu", "staging", apiImage + ":0.3.1", "0.3.1", attested, signed(commit, false), cli.ExitFailure, "", ""},
		// Publikacje sprzed decyzji 37: pochodzenie bez SBOM.
		{"atestacja bez SBOM na staging", "staging", apiImage + "@" + digest, "0.3.0", attested[:1], signed(commit, false), cli.ExitUnmet, "", ""},
		{"bez atestacji GitHuba na staging", "staging", apiImage + "@" + digest, "0.3.1-dev.4+abc1234", attested, signed(commit, true), cli.ExitUnmet, "", ""},
		{"podpis z innego commita niż etykieta", "staging", apiImage + "@" + digest, "0.3.1-dev.4+abc1234", attested, signed(strings.Repeat("e", 40), false), cli.ExitUnmet, "", ""},
		// dev nie wymaga atestacji — autor może postawić wydanie sprzed niej.
		{"wydanie sprzed SBOM w dev", "dev", apiImage + "@" + digest, "0.3.0", attested[:1], nil, cli.ExitOK, apiImage + ":0.3.0@" + digest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			askedSourceRef = ""
			d := Deployer{Env: definition(t, tt.env), Image: apiImage, Inspect: inspect(tt.version, tt.predicates), Attest: tt.attest}
			got, err := d.registryImage(context.Background(), tt.ref)
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			if tt.wantImage != "" && (got.want.Image != tt.wantImage || got.digest != digest || got.load != "") {
				t.Errorf("obraz %+v, oczekiwano %s", got, tt.wantImage)
			}
			if askedSourceRef != tt.wantSourceRef {
				t.Errorf("wymagany tag źródła %q, oczekiwano %q", askedSourceRef, tt.wantSourceRef)
			}
		})
	}
}

// TestUpRefusesBeforeTouchingAnything: odmowy z definicji padają, zanim
// cokolwiek zostanie uruchomione — Fake bez nagranych odpowiedzi zawiódłby
// przy pierwszym poleceniu.
func TestUpRefusesBeforeTouchingAnything(t *testing.T) {
	tests := []struct {
		name, env string
		art       Artifact
		want      string
	}{
		{"build z drzewa na ci", "ci", Artifact{Source: env.SourceLocal}, "nie przyjmuje obrazu ze źródła local"},
		{"archiwum na staging", "staging", Artifact{Source: env.SourceArchive, Archive: "x.tar", ConfigDigest: "sha256:x"}, "nie przyjmuje obrazu ze źródła archive"},
		{"Let's Encrypt bez adresu ACME", "staging", Artifact{Source: env.SourceRegistry, Ref: apiImage + "@sha256:" + strings.Repeat("d", 64)}, "DOCFIND_ACME_EMAIL"},
		{"produkcja bez klastra", "prod", Artifact{Source: env.SourceRegistry}, "tylko klastry k3d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &proc.Fake{}
			d := Deployer{Root: repoRoot, Env: definition(t, tt.env), Runner: fake, Image: apiImage}
			err := d.Up(context.Background(), tt.art)
			if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("oczekiwano odmowy z %q, jest %v", tt.want, err)
			}
			if len(fake.Calls) > 0 {
				t.Errorf("przed odmową uruchomiono %v", fake.Calls)
			}
		})
	}
}

func TestDownRequiresPermission(t *testing.T) {
	fake := &proc.Fake{}
	d := Deployer{Root: repoRoot, Env: definition(t, "staging"), Runner: fake}
	if err := d.Down(context.Background()); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "cluster-delete") {
		t.Errorf("usunięcie stagingu: %v", err)
	}
	if len(fake.Calls) > 0 {
		t.Errorf("uruchomiono %v", fake.Calls)
	}
}

// TestVerifyRegistry: promocja na prod bez klastra — te same sprawdzenia co
// przed wdrożeniem, a środowisko bez źródła rejestru odmawia.
func TestVerifyRegistry(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	inspect := func(_ context.Context, _ string) (registry.Inspected, error) {
		return registry.Inspected{
			Published: registry.Published{Index: digest, Predicates: []string{registry.ProvenancePredicates[1], registry.SBOMPredicate}},
			Labels:    map[string]string{registry.VersionLabel: "0.4.0", registry.RevisionLabel: commit},
		}, nil
	}
	var sourceRef string
	attestFn := func(_ context.Context, _, ref string) (attest.Result, error) {
		sourceRef = ref
		return attest.Result{PredicateType: attest.ProvenanceV1, SourceRef: ref, SourceCommit: commit}, nil
	}
	d := Deployer{Env: definition(t, "prod"), Image: apiImage, Inspect: inspect, Attest: attestFn}
	got, err := d.VerifyRegistry(context.Background(), apiImage+"@"+digest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != apiImage+":0.4.0@"+digest || got.Version != "0.4.0" || sourceRef != "refs/tags/v0.4.0" {
		t.Errorf("wydanie: %+v, tag źródła %q", got, sourceRef)
	}
	ci := Deployer{Env: definition(t, "ci"), Image: apiImage, Inspect: inspect, Attest: attestFn}
	ci.Env.Artifact.Sources = []string{env.SourceArchive}
	if _, err := ci.VerifyRegistry(context.Background(), apiImage+"@"+digest); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("środowisko bez rejestru: %v", err)
	}
}
