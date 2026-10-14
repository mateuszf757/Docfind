// Program dft — narzędzia i bramki DOCFIND (decyzja 23).
//
// Uruchamiany przez ci/dft, który buduje go z bieżącego drzewa i przekazuje
// argumenty; zadania mise i workflowy wołają ci/dft. Kody wyjścia: 0 — wynik
// pozytywny, 1 — warunek niespełniony, 2 — awaria narzędzia albo złe
// wywołanie.
//
// cmd/dft to korzeń kompozycji (decyzja 39): czyta argumenty i zmienne,
// składa etapy (internal/stages) z adapterami (internal/adapters)
// i zapisuje raporty. Decyzje bramek należą do etapów.
package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/git"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// command to jedno polecenie dft.
type command struct {
	usage string
	help  string
	run   func(ctx context.Context, w *workspace, args []string) error
}

var commands = map[string]command{
	"version": {
		usage: "dft version",
		help:  "wersja drzewa z gita (decyzja 30) — wersja wdrożenia, obraz jej nie niesie (decyzja 40)",
		run:   runVersion,
	},
	"identity": {
		usage: identityUsage,
		help:  "jedno pole tożsamości drzewa i obrazu — dla skryptów, bez parsowania",
		run:   runIdentity,
	},
	"pins": {
		usage: "dft pins github-output",
		help:  "przypięte wersje dla workflowów (uv, BuildKit) do $GITHUB_OUTPUT albo na ekran",
		run:   runPins,
	},
	"check": {
		usage: "dft check <bramka> …",
		help:  "bramki: versions, go, reproducible-tools, workflows, python, charts; obraz: runtime, base, reproducible, negatives, published, attestation, vulns; klaster: drain, tls, identity, cluster-negatives",
		run:   runCheck,
	},
	"build": {
		usage: "dft build [usługa]",
		help:  "build obrazu commita (decyzja 40); PUSH=1 publikacja, NO_CACHE=1 od zera, ARCHIVE=<plik> archiwum",
		run:   runBuild,
	},
	"release": {
		usage: `dft release <vX.Y.Z> ["tytuł"]`,
		help:  "wydanie: akceptacja na stagingu obrazu, który tam stoi (tożsamość, drain, TLS), i tag z adnotacją na jego commicie — bez builda",
		run:   runRelease,
	},
	"release-image": {
		usage: "dft release-image <vX.Y.Z>",
		help:  "w CI na tagu: zapis wydania z tagu, pochodzenie obrazu z main, tag z numerem w rejestrze; wyjścia dla atestacji i promocji",
		run:   runReleaseImage,
	},
	"test": {
		usage: "dft test [--vulns-baseline <commit>] [--vulns-report-only]",
		help:  "zadanie test z CI: wersje, workflowy, Python, charty, kod Go; flagi — tryb govulncheck (decyzja 37)",
		run:   runTest,
	},
	"cluster": {
		usage: "dft cluster <up [--image <obraz>@sha256:… | --image-archive <plik> --config-digest sha256:…]|down|evidence>",
		help:  "klaster środowiska (DOCFIND_ENV, domyślnie dev): utworzenie, platforma, obraz, chart, tożsamość; dowody do .cache/reports",
		run:   runCluster,
	},
	"promote": {
		usage: "dft promote <obraz@sha256:…> <vX.Y.Z>",
		help:  "promocja wydania na prod: zgoda w Environment production, pochodzenie z main, atestacja wydania, zapis i instalacja dla klienta",
		run:   runPromote,
	},
	"vulns-issue": {
		usage: "dft vulns-issue <katalog raportów>",
		help:  "wynik nocnych skanów podatności (obraz, kod Go) jako jedno issue: otwiera, aktualizuje albo zamyka (gh)",
		run:   runVulnsIssue,
	},
	"trial": {
		usage: "dft trial <N> <zadanie>…",
		help:  "okres próbny zadań CI: seria zielonych biegów na main (push, nocą) za pierwszym podejściem; kod 0, gdy każde ma N",
		run:   runTrial,
	},
	"chart": {
		usage: "dft chart publish --image <obraz@sha256:…>",
		help:  "chart aplikacji z tym obrazem w rejestrze OCI: wersja z commita, powtarzalny artefakt, sprawdzony po wysłaniu (decyzja 45)",
		run:   runChart,
	},
	"dns-token": {
		usage: "dft dns-token",
		help:  "token API Cloudflare do klastra (DNS-01), sprawdzony w API; bez echa, nigdy w argumentach",
		run:   runDNSToken,
	},
	"repo-settings": {
		usage: "dft repo-settings [--dry-run]",
		help:  "rulesety, środowiska GitHuba, auto-merge i przypinanie akcji SHA w ustawieniach repozytorium (admin)",
		run:   runRepoSettings,
	},
	"test-image": {
		usage: "dft test-image [usługa]",
		help:  "testy jednostkowe w obrazie na musl — ta sama baza co produkcja",
		run:   runTestImage,
	},
}

func main() {
	ctx, stop := cli.SignalContext()
	err := run(ctx, os.Args[1:])
	stop()
	cli.Report(err)
	os.Exit(cli.ExitCode(err))
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printUsage()
		if len(args) == 0 {
			return cli.Usage("podaj polecenie")
		}
		return nil
	}
	cmd, ok := commands[args[0]]
	if !ok {
		printUsage()
		return cli.Usage("nieznane polecenie %q", args[0])
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	runner := proc.Exec{}
	root, err := git.Root(ctx, runner, wd)
	if err != nil {
		return err
	}
	return cmd.run(ctx, &workspace{root: root, runner: runner}, args[1:])
}

func printUsage() {
	fmt.Fprintln(cli.Err, "dft — narzędzia i bramki DOCFIND. Polecenia:")
	for _, name := range slices.Sorted(maps.Keys(commands)) {
		c := commands[name]
		fmt.Fprintf(cli.Err, "  %-60s %s\n", c.usage, c.help)
	}
}

// expectArgs sprawdza liczbę argumentów polecenia.
func expectArgs(args []string, min, max int, usage string) error {
	if len(args) < min || len(args) > max {
		return cli.Usage("użycie: %s", usage)
	}
	return nil
}

// service zwraca usługę z argumentów; domyślnie api, jak w zadaniach mise.
func service(args []string, usage string) (string, error) {
	if err := expectArgs(args, 0, 1, usage); err != nil {
		return "", err
	}
	if len(args) == 1 {
		return args[0], nil
	}
	return "api", nil
}
