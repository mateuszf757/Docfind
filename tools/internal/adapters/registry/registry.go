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

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Adnotacje, którymi BuildKit wiąże manifest atestacji z manifestem obrazu,
// którego dotyczy (konwencja vnd.docker.reference.*).
const (
	ReferenceTypeAnnotation   = "vnd.docker.reference.type"
	ReferenceDigestAnnotation = "vnd.docker.reference.digest"
	AttestationType           = "attestation-manifest"
)

// PredicateTypeAnnotation — adnotacja, którą BuildKit opisuje typ predykatu
// in-toto w każdej warstwie manifestu atestacji.
const PredicateTypeAnnotation = "in-toto.io/predicate-type"

// Client — rejestr obrazów przez go-containerregistry.
type Client struct {
	// Options — dodatkowe opcje dostępu (testy); kontekst i uwierzytelnienie
	// z konfiguracji Dockera są zawsze, chyba że Anonymous.
	Options []remote.Option
	// Anonymous — bez poświadczeń, tak jak Argo CD pobiera publiczny pakiet:
	// odpowiedź mówi, czy pakiet jest publiczny, a nie, czy autor ma do
	// niego dostęp.
	Anonymous bool
}

func (c Client) opts(ctx context.Context) []remote.Option {
	if c.Anonymous {
		return append([]remote.Option{remote.WithContext(ctx)}, c.Options...)
	}
	return append(Options(ctx), c.Options...)
}

// Options — opcje dostępu do rejestru, domyślnie z uwierzytelnieniem
// z konfiguracji Dockera. Rejestry na loopbacku (127.0.0.1:5000) go-containerregistry
// woła po HTTP sam.
func Options(ctx context.Context) []remote.Option {
	return []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)}
}

// Inspect sprawdza budowę publikacji ref (repozytorium@digest): indeks
// z dokładnie jednym manifestem obrazu i z manifestem atestacji, który na
// niego wskazuje. Zwraca publikację z commitem z etykiety obrazu; z niczym
// jej nie porównuje — to robi wywołujący (Verify, wdrożenie po digeście,
// wydanie, promocja).
//
// Wynik negatywny (cli.Unmet): w rejestrze nie ma publikacji w tej postaci.
// Awaria (zwykły błąd): rejestr nieosiągalny albo odpowiedź nieczytelna.
func (c Client) Inspect(ctx context.Context, ref string) (artifact.Publication, error) {
	digestRef, err := name.NewDigest(ref)
	if err != nil {
		return artifact.Publication{}, fmt.Errorf("referencja %q: oczekiwano repozytorium@sha256:…: %w", ref, err)
	}
	desc, err := remote.Get(digestRef, c.opts(ctx)...)
	var terr *transport.Error
	if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
		// Najczęstsza przyczyna: digest konfiguracji wzięty zamiast digestu
		// indeksu — oba są w podsumowaniu zadania build, a pod digestem
		// konfiguracji rejestr ma blob, nie manifest.
		return artifact.Publication{}, fmt.Errorf("%s: %w — do wdrożenia i promocji służy digest indeksu (wyjście digest zadania build, kolumna „Obraz do wdrożenia”), nie digest konfiguracji", ref, artifact.ErrNotFound)
	}
	if err != nil {
		return artifact.Publication{}, fmt.Errorf("pobranie %s z rejestru: %w", ref, err)
	}
	if !desc.MediaType.IsIndex() {
		return artifact.Publication{}, cli.Unmet("w rejestrze jest %s, a nie indeks — obraz bez atestacji pochodzenia", desc.MediaType)
	}
	index, err := desc.ImageIndex()
	if err != nil {
		return artifact.Publication{}, fmt.Errorf("indeks %s: %w", ref, err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return artifact.Publication{}, fmt.Errorf("indeks %s: %w", ref, err)
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
		return artifact.Publication{}, cli.Unmet("indeks %s ma %d manifestów obrazu, oczekiwano jednego", ref, len(images))
	}
	image := images[0]
	attestation, ok := attested[image.Digest.String()]
	if !ok {
		return artifact.Publication{}, cli.Unmet("indeks %s nie ma manifestu atestacji dla obrazu %s", ref, image.Digest)
	}

	predicates, err := attestationPredicates(index, attestation)
	if err != nil {
		return artifact.Publication{}, err
	}
	img, err := index.Image(image.Digest)
	if err != nil {
		return artifact.Publication{}, fmt.Errorf("manifest %s: %w", image.Digest, err)
	}
	config, err := img.ConfigName()
	if err != nil {
		return artifact.Publication{}, fmt.Errorf("konfiguracja %s: %w", image.Digest, err)
	}
	file, err := img.ConfigFile()
	if err != nil {
		return artifact.Publication{}, fmt.Errorf("konfiguracja %s: %w", image.Digest, err)
	}
	return artifact.Publication{
		Artifact: artifact.Artifact{
			Repository: digestRef.Context().Name(),
			Digest:     desc.Digest.String(),
			Config:     config.String(),
			Commit:     file.Config.Labels[artifact.RevisionLabel],
		},
		Image:       image.Digest.String(),
		Attestation: attestation,
		Predicates:  predicates,
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

// Verify sprawdza publikację jak Inspect i dodatkowo, że konfiguracja
// obrazu to wantConfig — konfiguracja obrazu, który przeszedł sprawdzenia
// przed publikacją — a atestacja niesie pochodzenie i SBOM.
func (c Client) Verify(ctx context.Context, ref, wantConfig string) (artifact.Publication, error) {
	p, err := c.Inspect(ctx, ref)
	if err != nil {
		return artifact.Publication{}, err
	}
	if p.Config != wantConfig {
		return p, cli.Unmet("w rejestrze obraz z konfiguracją %s, a sprawdzony miał %s", p.Config, wantConfig)
	}
	return p, p.RequireAttestations()
}

// Tag dopisuje w rejestrze tag do opublikowanego indeksu ref
// (repozytorium@digest): ten sam manifest pod nową nazwą, bez ponownej
// publikacji — digest się nie zmienia (wydanie, decyzja 40). Tag, który już
// wskazuje na inny digest, to warunek niespełniony: tagi wydań się nie
// przesuwają.
func (c Client) Tag(ctx context.Context, ref, tag string) error {
	digestRef, err := name.NewDigest(ref)
	if err != nil {
		return fmt.Errorf("referencja %q: oczekiwano repozytorium@sha256:…: %w", ref, err)
	}
	tagRef := digestRef.Context().Tag(tag)
	existing, err := remote.Head(tagRef, c.opts(ctx)...)
	var terr *transport.Error
	switch {
	case err == nil && existing.Digest.String() == digestRef.DigestStr():
		return nil
	case err == nil:
		return cli.Unmet("tag %s wskazuje na %s, a wydanie to %s — tagi wydań się nie przesuwają", tagRef, existing.Digest, digestRef.DigestStr())
	case !errors.As(err, &terr) || terr.StatusCode != http.StatusNotFound:
		return fmt.Errorf("odczyt %s: %w", tagRef, err)
	}
	desc, err := remote.Get(digestRef, c.opts(ctx)...)
	if err != nil {
		return fmt.Errorf("pobranie %s z rejestru: %w", ref, err)
	}
	if err := remote.Tag(tagRef, desc, c.opts(ctx)...); err != nil {
		return fmt.Errorf("tag %s: %w", tagRef, err)
	}
	return nil
}

// Save zapisuje obraz linux/amd64 spod ref (tag albo digest) do
// archiwum w formacie `docker save` — do skanu bez demona Dockera.
func (c Client) Save(ctx context.Context, ref, path string) error {
	r, err := name.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("referencja %q: %w", ref, err)
	}
	img, err := remote.Image(r, append(c.opts(ctx), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))...)
	var terr *transport.Error
	if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", ref, artifact.ErrNotFound)
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
