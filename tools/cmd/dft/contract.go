package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/mateuszf757/Docfind/tools/internal/core/contract"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

const contractUsage = "dft contract thresholds [--write]"

// dftCommands, dftChecks — nazwy poleceń i bramek, na które może się
// powołać kontrakt. Ustawiane w init: mapy poleceń zawierają bramkę
// kontraktu, która potrzebuje ich nazw — w inicjalizatorze byłby cykl.
var dftCommands, dftChecks []string

func init() {
	dftCommands = slices.Sorted(maps.Keys(commands))
	dftChecks = slices.Sorted(maps.Keys(checks))
}

// runContract — tabela progów kontraktu z kodu (core/contract): na wyjście
// albo, z --write, wprost w docs/engineering-contracts.md między znaczniki.
// Próg zmienia się w kodzie, a kontrakt dostaje jego tabelę — nigdy
// odwrotnie (decyzja 50).
func runContract(_ context.Context, w *workspace, args []string) error {
	if len(args) == 0 || args[0] != "thresholds" {
		return cli.Usage("użycie: %s", contractUsage)
	}
	fs := flag.NewFlagSet("dft contract thresholds", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	write := fs.Bool("write", false, "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		return cli.Usage("użycie: %s", contractUsage)
	}
	if !*write {
		_, err := fmt.Fprint(os.Stdout, contract.Markdown())
		return err
	}
	file := filepath.Join(w.root, filepath.FromSlash(contract.Doc))
	text, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	updated, err := contract.ReplaceTable(string(text))
	if err != nil {
		return err
	}
	if updated == string(text) {
		cli.Step("tabela progów w %s równa kodowi — bez zmian", contract.Doc)
		return nil
	}
	if err := os.WriteFile(file, []byte(updated), 0o644); err != nil {
		return err
	}
	cli.Step("tabela progów w %s przepisana z %s", contract.Doc, contract.File)
	return nil
}

func runCheckContract(ctx context.Context, w *workspace, args []string) error {
	const usage = "dft check contract [--base <commit>]"
	fs := flag.NewFlagSet("dft check contract", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("base", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return cli.Usage("użycie: %s", usage)
	}
	return checkContract(ctx, w, *base)
}

// checkContract — kontrakt inżynierski (decyzja 50): tabela progów równa
// kodowi, każda klauzula z polami, strażnicy i decyzje, na które się
// powołuje, istnieją. Z commitem bazowym także pola chronione: zmienione
// tylko razem z docs/DECYZJE.md.
func checkContract(ctx context.Context, w *workspace, base string) error {
	text, err := os.ReadFile(filepath.Join(w.root, filepath.FromSlash(contract.Doc)))
	if err != nil {
		return err
	}
	known, err := w.contractKnown()
	if err != nil {
		return err
	}
	v := &cli.Verdict{}
	found := contract.CheckDocument(string(text), known)
	for _, f := range found {
		v.Fail("%s", f)
	}
	if len(found) == 0 {
		cli.Step("kontrakt: tabela progów równa kodowi, każda klauzula z polami, strażnicy i decyzje istnieją")
	}
	if base == "" {
		return v.Err("kontrakt inżynierski")
	}
	repo := w.git()
	changed, err := repo.ChangedFiles(ctx, base)
	if err != nil {
		return err
	}
	found, err = contract.CheckChange(changed, func(file string) (before, after []byte, err error) {
		if before, err = repo.FileAt(ctx, base, file); err != nil {
			return nil, nil, err
		}
		after, err = repo.FileAt(ctx, "HEAD", file)
		return before, after, err
	})
	if err != nil {
		return err
	}
	for _, f := range found {
		v.Fail("%s", f)
	}
	if len(found) == 0 {
		cli.Step("pola chronione względem %.12s: bez zmian albo razem z %s (decyzja 50)", base, contract.Decisions)
	}
	return v.Err("kontrakt inżynierski")
}

// contractKnown — to, na co kontrakt może się powołać: zadania z mise.toml,
// polecenia i bramki dft, pakiety tools/, decyzje z docs/DECYZJE.md.
func (w *workspace) contractKnown() (contract.Known, error) {
	mise, err := os.ReadFile(filepath.Join(w.root, "mise.toml"))
	if err != nil {
		return contract.Known{}, err
	}
	decisions, err := os.ReadFile(filepath.Join(w.root, filepath.FromSlash(contract.Decisions)))
	if err != nil {
		return contract.Known{}, err
	}
	known := contract.Known{
		Tasks:     contract.MiseTasks(string(mise)),
		Commands:  dftCommands,
		Checks:    dftChecks,
		Decisions: contract.DecisionNumbers(string(decisions)),
	}
	for _, dir := range []string{"tools/internal/stages", "tools/internal/core", "tools/internal/adapters", "tools/internal/kit", "tools/cmd"} {
		entries, err := os.ReadDir(filepath.Join(w.root, filepath.FromSlash(dir)))
		if err != nil {
			return contract.Known{}, err
		}
		for _, e := range entries {
			if e.IsDir() {
				known.Packages = append(known.Packages, path.Base(dir)+"/"+e.Name())
			}
		}
	}
	if len(known.Tasks) == 0 || len(known.Decisions) == 0 {
		return contract.Known{}, errors.New("mise.toml bez zadań albo docs/DECYZJE.md bez decyzji — zmienił się format, a kontrakt nie miałby się do czego odwołać")
	}
	return known, nil
}
