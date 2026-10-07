package registry

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
)

// testRegistry to rejestr OCI w pamięci procesu — warianty, których nie
// da się łatwo wywołać prawdziwym BuildKitem (dwa obrazy w indeksie,
// atestacja wskazująca na inny obraz), bez Dockera i bez sieci.
func testRegistry(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	// Adres na loopbacku — go-containerregistry wybiera wtedy HTTP sam.
	return strings.TrimPrefix(server.URL, "http://") + "/docfind/docfind-api"
}

func randomImage(t *testing.T) v1.Image {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func digestOf(t *testing.T, img v1.Image) v1.Hash {
	t.Helper()
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func configOf(t *testing.T, img v1.Image) string {
	t.Helper()
	c, err := img.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	return c.String()
}

// attestedIndex buduje indeks taki, jaki publikuje BuildKit z
// --provenance: manifest obrazu i manifest atestacji z adnotacją
// wskazującą na digest obrazu, któremu ma ją przypisać.
func attestedIndex(t *testing.T, img v1.Image, attestationFor v1.Hash) v1.ImageIndex {
	t.Helper()
	return mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: randomImage(t), Descriptor: v1.Descriptor{
			Platform: &v1.Platform{OS: "unknown", Architecture: "unknown"},
			Annotations: map[string]string{
				ReferenceTypeAnnotation:   AttestationType,
				ReferenceDigestAnnotation: attestationFor.String(),
			},
		}},
	)
}

func pushIndex(t *testing.T, repo string, idx v1.ImageIndex) string {
	t.Helper()
	ref, err := name.ParseReference(repo + ":test")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	d, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return repo + "@" + d.String()
}

func pushImage(t *testing.T, repo string, img v1.Image) string {
	t.Helper()
	ref, err := name.ParseReference(repo + ":test")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	return repo + "@" + digestOf(t, img).String()
}

func TestVerifyPublished(t *testing.T) {
	ctx := context.Background()
	repo := testRegistry(t)
	img := randomImage(t)
	other := randomImage(t)

	tests := []struct {
		name     string
		ref      func(t *testing.T) string
		config   string
		wantCode int
		wantMsg  string
	}{
		{
			name:     "indeks z atestacją, konfiguracja sprawdzona",
			ref:      func(t *testing.T) string { return pushIndex(t, repo, attestedIndex(t, img, digestOf(t, img))) },
			config:   configOf(t, img),
			wantCode: cli.ExitOK,
		},
		{
			// Tak wyglądała publikacja przez `docker push` przed PR #18.
			name:     "sam manifest, bez indeksu",
			ref:      func(t *testing.T) string { return pushImage(t, repo, img) },
			config:   configOf(t, img),
			wantCode: cli.ExitUnmet,
			wantMsg:  "a nie indeks",
		},
		{
			name: "indeks bez atestacji",
			ref: func(t *testing.T) string {
				return pushIndex(t, repo, mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img}))
			},
			config:   configOf(t, img),
			wantCode: cli.ExitUnmet,
			wantMsg:  "nie ma manifestu atestacji",
		},
		{
			name:     "atestacja wskazuje na inny obraz",
			ref:      func(t *testing.T) string { return pushIndex(t, repo, attestedIndex(t, img, digestOf(t, other))) },
			config:   configOf(t, img),
			wantCode: cli.ExitUnmet,
			wantMsg:  "nie ma manifestu atestacji",
		},
		{
			name: "dwa obrazy w indeksie",
			ref: func(t *testing.T) string {
				idx := mutate.AppendManifests(attestedIndex(t, img, digestOf(t, img)), mutate.IndexAddendum{Add: other})
				return pushIndex(t, repo, idx)
			},
			config:   configOf(t, img),
			wantCode: cli.ExitUnmet,
			wantMsg:  "ma 2 manifestów obrazu",
		},
		{
			name:     "inna konfiguracja niż w obrazie sprawdzonym",
			ref:      func(t *testing.T) string { return pushIndex(t, repo, attestedIndex(t, other, digestOf(t, other))) },
			config:   configOf(t, img),
			wantCode: cli.ExitUnmet,
			wantMsg:  "a sprawdzony miał",
		},
		{
			// Brak obiektu w rejestrze to awaria (nie wiadomo, co jest
			// opublikowane), a nie wynik.
			name:     "digest, którego nie ma w rejestrze",
			ref:      func(t *testing.T) string { return repo + "@sha256:" + strings.Repeat("0", 64) },
			config:   configOf(t, img),
			wantCode: cli.ExitFailure,
		},
		{
			name:     "rejestr nieosiągalny",
			ref:      func(t *testing.T) string { return "127.0.0.1:1/docfind/docfind-api@sha256:" + strings.Repeat("0", 64) },
			config:   configOf(t, img),
			wantCode: cli.ExitFailure,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := VerifyPublished(tt.ref(t), tt.config, Options(ctx)...)
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			if tt.wantMsg != "" && (err == nil || !strings.Contains(err.Error(), tt.wantMsg)) {
				t.Errorf("błąd %v nie zawiera %q", err, tt.wantMsg)
			}
		})
	}
}

// TestInspectLabels: wdrożenie po digeście bierze wersję i commit z etykiet
// konfiguracji obrazu w rejestrze — z tego, co tam leży, a nie z gita.
func TestInspectLabels(t *testing.T) {
	repo := testRegistry(t)
	img, err := mutate.Config(randomImage(t), v1.Config{Labels: map[string]string{
		VersionLabel:  "0.3.0",
		RevisionLabel: strings.Repeat("a", 40),
	}})
	if err != nil {
		t.Fatal(err)
	}
	ref := pushIndex(t, repo, attestedIndex(t, img, digestOf(t, img)))
	got, err := Inspect(ref, remote.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels[VersionLabel] != "0.3.0" || got.Labels[RevisionLabel] != strings.Repeat("a", 40) {
		t.Errorf("etykiety: %v", got.Labels)
	}
	if got.Config != configOf(t, img) || got.Index != ref[strings.Index(ref, "@")+1:] {
		t.Errorf("digesty: %+v (ref %s)", got.Published, ref)
	}
}
