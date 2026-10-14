// Package artifact to rdzeń zasady „build once": obraz budowany raz na
// commit, sprawdzany i promowany między środowiskami po digeście — nigdy
// budowany od nowa dla kolejnego środowiska. Wydanie to numer nadany
// digestowi, który przeszedł akceptację, a nie osobny build (decyzja 40).
//
// Tu są typy, którymi etapy (build, wdrożenie, skan, wydanie, promocja)
// rozmawiają z adapterami (git, rejestr, GitHub), i reguły tożsamości
// z decyzji 30 i 40 — bez wywołań gita, Dockera ani rejestru.
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
	Config string `json:"config,omitempty"`
	// Commit — commit, z którego zbudowano obraz (etykieta revision). Jedyna
	// tożsamość źródła w obrazie: wersja nie jest jego treścią (decyzja 40).
	Commit string `json:"commit"`
	// Release — numer wydania X.Y.Z, gdy digest ma atestację wydania; pusty
	// — obraz commita.
	Release string `json:"release,omitempty"`
}

// Tag — tag obrazu w rejestrze: numer wydania albo skrócony commit
// (etykieta dla czytelnika; obraz wybiera digest).
func (a Artifact) Tag() string {
	if a.Release != "" {
		return a.Release
	}
	return CommitTag(a.Commit)
}

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

// Blob — zawartość artefaktu wysyłana do rejestru przed manifestem, który
// na nią wskazuje (chart Helma: konfiguracja i archiwum).
type Blob struct {
	Data      []byte
	MediaType string
}

// ErrNotFound — w rejestrze nie ma obrazu pod tą referencją. Adapter
// rejestru owija nim odpowiedź 404, a etapy rozróżniają nim brak obrazu
// (np. obrazu bazowego do porównania) od awarii rejestru.
var ErrNotFound = errors.New("obrazu nie ma w rejestrze")

// ErrUnknownCommit — w repozytorium nie ma tego commita (płytki klon,
// gałąź spoza pobranych). Adapter gita owija nim odpowiedź rev-parse,
// a etapy rozróżniają nim brak kodu do porównania od awarii gita.
var ErrUnknownCommit = errors.New("commitu nie ma w repozytorium")

// Expected — tożsamość, której oczekuje się na każdym podzie: obraz
// w specyfikacji i commit z /version, a do tego wersja wdrożenia, którą
// /version podaje ze zmiennej DOCFIND_VERSION. Pusta wersja — wdrożenie jej
// nie ustawia (sprzed decyzji 40) i nie jest sprawdzana.
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

// CommitTag — tag obrazu commita w rejestrze: 12 znaków commita. Ten sam
// commit to ten sam obraz (decyzja 21), więc tag nigdy nie przeskakuje na
// inną treść.
func CommitTag(commit string) string { return ShortCommit(commit) }

// LocalTag — tag kopii lokalnej: obraz z brudnego drzewa nie odpowiada
// żadnemu commitowi i nie może nosić tagu obrazu commita.
func LocalTag(commit string, dirty bool) string {
	if dirty {
		return CommitTag(commit) + "-dirty"
	}
	return CommitTag(commit)
}
