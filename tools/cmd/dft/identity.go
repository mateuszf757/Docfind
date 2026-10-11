package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

const identityUsage = "dft identity <version|commit|source-date-epoch|source-date [epoch]|image <usługa>|require-clean>"

func runVersion(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft version"); err != nil {
		return err
	}
	return printIdentity(ctx, w, []string{"version"})
}

// runIdentity wypisuje jedno pole tożsamości. Jedno pole na wywołanie,
// bez JSON-a: skrypt bashowy bierze wynik w całości do zmiennej i nie musi
// niczego parsować.
func runIdentity(ctx context.Context, w *workspace, args []string) error {
	if len(args) == 0 {
		return cli.Usage("użycie: %s", identityUsage)
	}
	return printIdentity(ctx, w, args)
}

func printIdentity(ctx context.Context, w *workspace, args []string) error {
	repo := w.git()
	field, rest := args[0], args[1:]
	var value string
	switch field {
	case "version":
		if err := expectArgs(rest, 0, 0, identityUsage); err != nil {
			return err
		}
		v, err := repo.Version(ctx)
		if err != nil {
			return err
		}
		value = v
	case "commit":
		if err := expectArgs(rest, 0, 0, identityUsage); err != nil {
			return err
		}
		c, err := repo.Commit(ctx)
		if err != nil {
			return err
		}
		value = c
	case "source-date-epoch":
		if err := expectArgs(rest, 0, 0, identityUsage); err != nil {
			return err
		}
		epoch, err := repo.SourceDateEpoch(ctx)
		if err != nil {
			return err
		}
		value = strconv.FormatInt(epoch, 10)
	case "source-date":
		if err := expectArgs(rest, 0, 1, identityUsage); err != nil {
			return err
		}
		var epoch int64
		if len(rest) == 1 {
			e, err := strconv.ParseInt(rest[0], 10, 64)
			if err != nil {
				return cli.Usage("source-date: %q nie jest liczbą sekund", rest[0])
			}
			epoch = e
		} else {
			e, err := repo.SourceDateEpoch(ctx)
			if err != nil {
				return err
			}
			epoch = e
		}
		value = artifact.SourceDate(epoch)
	case "image":
		if err := expectArgs(rest, 1, 1, identityUsage); err != nil {
			return err
		}
		value = imageName(rest[0])
	case "require-clean":
		if err := expectArgs(rest, 0, 0, identityUsage); err != nil {
			return err
		}
		return repo.RequireClean(ctx)
	default:
		return cli.Usage("nieznane pole %q — użycie: %s", field, identityUsage)
	}
	_, err := fmt.Fprintln(cli.Out, value)
	return err
}
