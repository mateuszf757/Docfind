package artifact

import "io"

// BuildRequest — co zbudować. Etap decyduje o tagach, argumentach,
// atestacjach i publikacji; adapter Dockera zamienia to na wywołanie buildx
// i dobiera wyjście do sterownika BuildKitu.
type BuildRequest struct {
	ContextDir string
	// Target — etap Dockerfile do zbudowania; pusty — ostatni (obraz).
	Target string
	// Contexts — nazwane konteksty builda (--build-context nazwa=katalog):
	// pliki spoza katalogu usługi, których potrzebuje tylko wskazany etap.
	Contexts []BuildContext
	// Tags — pełne referencje obraz:tag.
	Tags []string
	Args []BuildArg
	// NoCache — build od zera (sprawdzenie powtarzalności).
	NoCache bool
	// CacheOnly — wynik tylko w pamięci podręcznej BuildKitu, bez obrazu
	// (etap testów: liczy się, czy się zbudował).
	CacheOnly bool
	// Publish — publikacja do rejestru z atestacjami; nil — kopia lokalna
	// bez atestacji (atestacja opisuje przebieg budowania, więc różni się
	// między buildami i zmieniałaby digest przy każdym).
	Publish *PublishRequest
	// Out i Err — wyjście BuildKitu na bieżąco.
	Out, Err io.Writer
}

// BuildContext — nazwany kontekst builda.
type BuildContext struct {
	Name, Dir string
}

// BuildArg — argument builda (--build-arg).
type BuildArg struct {
	Name, Value string
}

// PublishRequest — publikacja z atestacją pochodzenia (mode=max) i SBOM.
type PublishRequest struct {
	// SBOMGenerator — obraz skanera SBOM przypięty digestem (decyzja 37).
	SBOMGenerator string
}
