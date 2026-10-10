// Package artifact to rdzeń zasady „build once": obraz budowany raz,
// sprawdzany i promowany między środowiskami po digeście — nigdy budowany od
// nowa dla kolejnego środowiska.
//
// Tu są typy, którymi etapy (build, wdrożenie, skan, promocja) rozmawiają
// z adapterami (git, rejestr, GitHub), i reguły tożsamości z decyzji 30 —
// bez wywołań gita, Dockera ani rejestru.
package artifact

import (
	"errors"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
)

// Artifact — obraz usługi zidentyfikowany treścią.
type Artifact struct {
	// Repository — nazwa obrazu bez tagu, np. ghcr.io/mateuszf757/docfind-api.
	Repository string `json:"repository"`
	// Digest — sha256 indeksu w rejestrze: tożsamość publikacji, to, co
	// pobierają węzły i co się promuje. Pusty, dopóki obraz nie jest
	// opublikowany.
	Digest string `json:"digest,omitempty"`
	// Config — sha256 konfiguracji obrazu: tożsamość zawartości (warstw po
	// rozpakowaniu), niezależna od kompresji i atestacji. Nią porównuje się
	// kopię lokalną, archiwum z CI i publikację. Pod tym digestem rejestr nie
	// ma manifestu — do pobrania służy Digest.
	Config  string `json:"config,omitempty"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Tag — tag obrazu z wersji (etykieta dla czytelnika; wybiera digest).
func (a Artifact) Tag() string { return DockerTag(a.Version) }

// Ref — repozytorium@digest: referencja do pobrania.
func (a Artifact) Ref() string { return a.Repository + "@" + a.Digest }

// Pinned — repozytorium:tag@digest, jak w specyfikacji poda: tag dla
// czytelnika, digest dla węzła.
func (a Artifact) Pinned() string { return a.Repository + ":" + a.Tag() + "@" + a.Digest }

// ParseRef rozkłada referencję repozytorium[:tag]@sha256:… na repozytorium
// i digest. Tag w referencji niczego nie wybiera, więc jest pomijany.
func ParseRef(ref string) (repository, digest string, err error) {
	d, err := name.NewDigest(ref)
	if err != nil {
		return "", "", fmt.Errorf("obraz %q: oczekiwano repozytorium@sha256:…: %w", ref, err)
	}
	return d.Context().Name(), d.DigestStr(), nil
}

// ErrNotFound — w rejestrze nie ma obrazu pod tą referencją. Adapter
// rejestru owija nim odpowiedź 404, a etapy rozróżniają nim brak obrazu
// (np. obrazu bazowego do porównania) od awarii rejestru.
var ErrNotFound = errors.New("obrazu nie ma w rejestrze")

// Expected — tożsamość, której oczekuje się na każdym podzie: obraz
// w specyfikacji, wersja i commit z /version.
type Expected struct {
	Image   string `json:"image"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// ShortCommit — commit skrócony do 12 znaków, do komunikatów.
func ShortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
