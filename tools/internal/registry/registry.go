// Package registry sprawdza opublikowany obraz w rejestrze, czyli w systemie,
// który go przechowuje — a nie w metadanych BuildKitu, które mówią, co BuildKit
// zamierzał wysłać. Decyzja 21 twierdziła, że opublikowane obrazy mają
// atestację, bo build jej nie wyłączał; rejestr trzymał sam manifest, bez
// indeksu i bez atestacji, dopóki nikt go nie zapytał.
//
// go-containerregistry zamiast `docker buildx imagetools inspect --raw`:
// typowane indeksy i manifesty, uwierzytelnienie z konfiguracji Dockera
// (to samo, które zapisuje docker/login-action) i weryfikacja bez Dockera —
// zadanie promocji nie musi go mieć.
package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
)

// Adnotacje, którymi BuildKit wiąże manifest atestacji z manifestem obrazu,
// którego dotyczy (konwencja vnd.docker.reference.*).
const (
	ReferenceTypeAnnotation   = "vnd.docker.reference.type"
	ReferenceDigestAnnotation = "vnd.docker.reference.digest"
	AttestationType           = "attestation-manifest"
)

// Typy predykatów in-toto, którymi BuildKit opisuje warstwy manifestu
// atestacji (adnotacja in-toto.io/predicate-type).
const (
	PredicateTypeAnnotation = "in-toto.io/predicate-type"
	SBOMPredicate           = "https://spdx.dev/Document"
)

// ProvenancePredicates — pochodzenie SLSA w wersjach, które wystawia BuildKit.
var ProvenancePredicates = []string{"https://slsa.dev/provenance/v0.2", "https://slsa.dev/provenance/v1"}

// Published opisuje obraz zweryfikowany w rejestrze.
type Published struct {
	// Index — digest indeksu, czyli tożsamość publikacji.
	Index string `json:"index"`
	// Image — digest manifestu obrazu platformy.
	Image string `json:"image"`
	// Attestation — digest manifestu atestacji.
	Attestation string `json:"attestation"`
	// Config — digest konfiguracji obrazu (rozpakowane warstwy).
	Config string `json:"config"`
	// Predicates — typy predykatów w manifeście atestacji (pochodzenie, SBOM).
	Predicates []string `json:"predicates"`
}

// HasProvenance mówi, czy atestacja niesie pochodzenie SLSA.
func (p Published) HasProvenance() bool {
	return slices.ContainsFunc(p.Predicates, func(t string) bool { return slices.Contains(ProvenancePredicates, t) })
}

// HasSBOM mówi, czy atestacja niesie SBOM (SPDX).
func (p Published) HasSBOM() bool {
	return slices.Contains(p.Predicates, SBOMPredicate)
}

// Options — opcje dostępu do rejestru, domyślnie z uwierzytelnieniem
// z konfiguracji Dockera. Rejestry na loopbacku (127.0.0.1:5000) go-containerregistry
// woła po HTTP sam.
func Options(ctx context.Context) []remote.Option {
	return []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)}
}

// Inspected — publikacja odczytana z rejestru razem z etykietami obrazu.
type Inspected struct {
	Published
	// Labels — etykiety z konfiguracji obrazu (org.opencontainers.image.*).
	Labels map[string]string
}

// Etykiety OCI, którymi build opisuje obraz (services/api/Dockerfile).
const (
	VersionLabel  = "org.opencontainers.image.version"
	RevisionLabel = "org.opencontainers.image.revision"
)

// Inspect sprawdza budowę publikacji ref (repozytorium@digest): indeks
// z dokładnie jednym manifestem obrazu i z manifestem atestacji, który na
// niego wskazuje. Zwraca digesty i etykiety konfiguracji; z niczym ich nie
// porównuje — to robi wywołujący (VerifyPublished, wdrożenie po digeście).
//
// Wynik negatywny (cli.Unmet): w rejestrze nie ma publikacji w tej postaci.
// Awaria (zwykły błąd): rejestr nieosiągalny albo odpowiedź nieczytelna.
func Inspect(ref string, opts ...remote.Option) (Inspected, error) {
	digestRef, err := name.NewDigest(ref)
	if err != nil {
		return Inspected{}, fmt.Errorf("referencja %q: oczekiwano repozytorium@sha256:…: %w", ref, err)
	}
	desc, err := remote.Get(digestRef, opts...)
	if err != nil {
		return Inspected{}, fmt.Errorf("pobranie %s z rejestru: %w", ref, err)
	}
	if !desc.MediaType.IsIndex() {
		return Inspected{}, cli.Unmet("w rejestrze jest %s, a nie indeks — obraz bez atestacji pochodzenia", desc.MediaType)
	}
	index, err := desc.ImageIndex()
	if err != nil {
		return Inspected{}, fmt.Errorf("indeks %s: %w", ref, err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return Inspected{}, fmt.Errorf("indeks %s: %w", ref, err)
	}

	var images []v1.Descriptor
	attested := map[string]string{}
	for _, d := range manifest.Manifests {
		if d.Annotations[ReferenceTypeAnnotation] == AttestationType {
			attested[d.Annotations[ReferenceDigestAnnotation]] = d.Digest.String()
			continue
		}
		images = append(images, d)
	}
	if len(images) != 1 {
		return Inspected{}, cli.Unmet("indeks %s ma %d manifestów obrazu, oczekiwano jednego", ref, len(images))
	}
	image := images[0]
	attestation, ok := attested[image.Digest.String()]
	if !ok {
		return Inspected{}, cli.Unmet("indeks %s nie ma manifestu atestacji dla obrazu %s", ref, image.Digest)
	}

	predicates, err := attestationPredicates(index, attestation)
	if err != nil {
		return Inspected{}, err
	}
	img, err := index.Image(image.Digest)
	if err != nil {
		return Inspected{}, fmt.Errorf("manifest %s: %w", image.Digest, err)
	}
	config, err := img.ConfigName()
	if err != nil {
		return Inspected{}, fmt.Errorf("konfiguracja %s: %w", image.Digest, err)
	}
	file, err := img.ConfigFile()
	if err != nil {
		return Inspected{}, fmt.Errorf("konfiguracja %s: %w", image.Digest, err)
	}
	return Inspected{
		Published: Published{Index: desc.Digest.String(), Image: image.Digest.String(), Attestation: attestation, Config: config.String(), Predicates: predicates},
		Labels:    file.Config.Labels,
	}, nil
}

// attestationPredicates czyta typy predykatów z warstw manifestu atestacji.
func attestationPredicates(index v1.ImageIndex, attestation string) ([]string, error) {
	h, err := v1.NewHash(attestation)
	if err != nil {
		return nil, fmt.Errorf("digest atestacji %q: %w", attestation, err)
	}
	att, err := index.Image(h)
	if err != nil {
		return nil, fmt.Errorf("manifest atestacji %s: %w", attestation, err)
	}
	m, err := att.Manifest()
	if err != nil {
		return nil, fmt.Errorf("manifest atestacji %s: %w", attestation, err)
	}
	var predicates []string
	for _, l := range m.Layers {
		if t := l.Annotations[PredicateTypeAnnotation]; t != "" && !slices.Contains(predicates, t) {
			predicates = append(predicates, t)
		}
	}
	slices.Sort(predicates)
	return predicates, nil
}

// RequireAttestations odmawia publikacji bez pochodzenia SLSA albo bez SBOM
// w atestacji BuildKitu.
func RequireAttestations(p Published) error {
	switch {
	case !p.HasProvenance():
		return cli.Unmet("atestacja %s bez pochodzenia SLSA (predykaty: %v)", p.Attestation, p.Predicates)
	case !p.HasSBOM():
		return cli.Unmet("atestacja %s bez SBOM (predykaty: %v)", p.Attestation, p.Predicates)
	}
	return nil
}

// VerifyPublished sprawdza publikację jak Inspect i dodatkowo, że
// konfiguracja obrazu to wantConfig — konfiguracja obrazu, który przeszedł
// sprawdzenia przed publikacją — a atestacja niesie pochodzenie i SBOM.
func VerifyPublished(ref, wantConfig string, opts ...remote.Option) (Published, error) {
	inspected, err := Inspect(ref, opts...)
	if err != nil {
		return Published{}, err
	}
	published := inspected.Published
	if published.Config != wantConfig {
		return published, cli.Unmet("w rejestrze obraz z konfiguracją %s, a sprawdzony miał %s", published.Config, wantConfig)
	}
	return published, RequireAttestations(published)
}

// ErrNotFound — w rejestrze nie ma obrazu pod tą referencją.
var ErrNotFound = errors.New("obrazu nie ma w rejestrze")

// SaveImage zapisuje obraz linux/amd64 spod ref (tag albo digest) do
// archiwum w formacie `docker save` — do skanu bez demona Dockera.
func SaveImage(ref, path string, opts ...remote.Option) error {
	r, err := name.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("referencja %q: %w", ref, err)
	}
	img, err := remote.Image(r, append(opts, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))...)
	var terr *transport.Error
	if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", ref, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("pobranie %s: %w", ref, err)
	}
	tag, err := name.NewTag(r.Context().Name() + ":skan")
	if err != nil {
		return err
	}
	if err := tarball.WriteToFile(path, tag, img); err != nil {
		return fmt.Errorf("archiwum %s: %w", ref, err)
	}
	return nil
}
