// Program dft — narzędzia i bramki DOCFIND (decyzja 23).
//
// Uruchamiany przez ci/dft, który buduje go z bieżącego drzewa i przekazuje
// argumenty; zadania mise i workflowy wołają ci/dft. Kody wyjścia: 0 — wynik
// pozytywny, 1 — warunek niespełniony, 2 — awaria narzędzia albo złe
// wywołanie.
package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/identity"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// command to jedno polecenie dft.
type command struct {
	usage string
	help  string
	run   func(ctx context.Context, env *environment, args []string) error
}

// environment — to, czego potrzebuje każde polecenie: korzeń repozytorium
// i sposób uruchamiania programów.
type environment struct {
	root   string
	runner proc.Runner
}

var commands = map[string]command{
	"version": {
		usage: "dft version",
		help:  "wersja artefaktu z gita (decyzja 30)",
		run:   runVersion,
	},
	"identity": {
		usage: "dft identity <version|docker-tag [wersja]|commit|source-date-epoch|source-date [epoch]|image <usługa>|require-clean>",
		help:  "jedno pole tożsamości artefaktu — dla skryptów, bez parsowania",
		run:   runIdentity,
	},
	"pins": {
		usage: "dft pins github-output",
		help:  "przypięte wersje dla workflowów (uv, BuildKit) do $GITHUB_OUTPUT albo na ekran",
		run:   runPins,
	},
	"check": {
		usage: "dft check <versions|go|reproducible-tools|runtime|base|reproducible> [usługa]",
		help:  "bramki: wersje, kod Go, powtarzalność narzędzi; obraz: Etap 1, baza, powtarzalność",
		run:   runCheck,
	},
	"build": {
		usage: "dft build [usługa]",
		help:  "build obrazu z wersją z gita; RELEASE=1 wydanie, PUSH=1 publikacja, NO_CACHE=1 od zera",
		run:   runBuild,
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
	root, err := identity.Root(ctx, runner, wd)
	if err != nil {
		return err
	}
	return cmd.run(ctx, &environment{root: root, runner: runner}, args[1:])
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
