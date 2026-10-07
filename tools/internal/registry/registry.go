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
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
)

// Adnotacje, którymi BuildKit wiąże manifest atestacji z manifestem obrazu,
// którego dotyczy (konwencja vnd.docker.reference.*).
const (
	ReferenceTypeAnnotation   = "vnd.docker.reference.type"
	ReferenceDigestAnnotation = "vnd.docker.reference.digest"
	AttestationType           = "attestation-manifest"
)

// Published opisuje obraz zweryfikowany w rejestrze.
type Published struct {
	// Index — digest indeksu, czyli tożsamość publikacji.
	Index string
	// Image — digest manifestu obrazu platformy.
	Image string
	// Attestation — digest manifestu atestacji pochodzenia.
	Attestation string
	// Config — digest konfiguracji obrazu (rozpakowane warstwy).
	Config string
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
		Published: Published{Index: desc.Digest.String(), Image: image.Digest.String(), Attestation: attestation, Config: config.String()},
		Labels:    file.Config.Labels,
	}, nil
}

// VerifyPublished sprawdza publikację jak Inspect i dodatkowo, że
// konfiguracja obrazu to wantConfig — konfiguracja obrazu, który przeszedł
// sprawdzenia przed publikacją.
func VerifyPublished(ref, wantConfig string, opts ...remote.Option) (Published, error) {
	inspected, err := Inspect(ref, opts...)
	if err != nil {
		return Published{}, err
	}
	published := inspected.Published
	if published.Config != wantConfig {
		return published, cli.Unmet("w rejestrze obraz z konfiguracją %s, a sprawdzony miał %s", published.Config, wantConfig)
	}
	return published, nil
}
