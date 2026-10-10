// Package reposync doprowadza ustawienia repozytorium na GitHubie do tych
// zapisanych jako kod (core/reposettings): rulesety, auto-merge, wymóg
// przypinania akcji pełnym SHA i środowiska GitHuba (`dft repo-settings`,
// dawniej ci/apply-repo-settings.sh).
//
// Reguła gałęzi main jest warunkiem bezpieczeństwa auto-merge łatek od
// Dependabota: `gh pr merge --auto` czeka tylko na sprawdzenia wymagane przez
// regułę, więc bez niej scaliłby PR natychmiast, bez żadnego CI (decyzja 22).
package reposync

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/reposettings"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// GitHub — ustawienia repozytorium (adapters/github).
type GitHub interface {
	Repo(ctx context.Context) (string, error)
	EnableAutoMerge(ctx context.Context, repo string) error
	ActionsPermissions(ctx context.Context, repo string) (enabled bool, allowed string, err error)
	RequireSHAPinning(ctx context.Context, repo string, enabled bool, allowed string) error
	Rulesets(ctx context.Context, repo string) (map[string]int64, error)
	CreateRuleset(ctx context.Context, repo, file string) error
	UpdateRuleset(ctx context.Context, repo string, id int64, file string) error
	UserID(ctx context.Context, login string) (int64, error)
	PutEnvironment(ctx context.Context, repo string, e reposettings.Environment, reviewerIDs []int64) error
	BranchPolicies(ctx context.Context, repo, env string) (map[reposettings.Policy]int64, error)
	AddBranchPolicy(ctx context.Context, repo, env string, p reposettings.Policy) error
	DeleteBranchPolicy(ctx context.Context, repo, env string, id int64) error
}

// Settings stosuje ustawienia repozytorium w Root.
type Settings struct {
	Root   string
	GitHub GitHub
	// DryRun — tylko wypisuje, co by zrobił; niczego nie zmienia.
	DryRun bool
}

// write wykonuje zmianę albo, w trybie próbnym, tylko ją opisuje.
func (s Settings) write(what string, change func() error) error {
	if s.DryRun {
		cli.Step("[próba] %s", what)
		return nil
	}
	if err := change(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	cli.Step("%s", what)
	return nil
}

// Apply stosuje wszystkie ustawienia. Idempotentne: istniejąca reguła
// o tej samej nazwie jest aktualizowana, a nie dublowana.
func (s Settings) Apply(ctx context.Context) error {
	rulesets, err := reposettings.LoadRulesets(s.Root)
	if err != nil {
		return err
	}
	envs, err := reposettings.LoadEnvironments(s.Root)
	if err != nil {
		return err
	}
	repo, err := s.GitHub.Repo(ctx)
	if err != nil {
		return err
	}
	cli.Step("repozytorium: %s", repo)

	if err := s.write("auto-merge włączony", func() error { return s.GitHub.EnableAutoMerge(ctx, repo) }); err != nil {
		return err
	}
	// Przypinanie akcji pełnym SHA wymuszane przez GitHuba, a nie tylko przez
	// dyscyplinę: workflow z akcją przypiętą tagiem nie ruszy.
	enabled, allowed, err := s.GitHub.ActionsPermissions(ctx, repo)
	if err != nil {
		return err
	}
	if err := s.write(fmt.Sprintf("akcje: wymagane przypięcie pełnym SHA (allowed_actions=%s)", allowed), func() error {
		return s.GitHub.RequireSHAPinning(ctx, repo, enabled, allowed)
	}); err != nil {
		return err
	}

	existing, err := s.GitHub.Rulesets(ctx, repo)
	if err != nil {
		return err
	}
	for _, r := range rulesets {
		var err error
		if id, ok := existing[r.Name]; ok {
			err = s.write(fmt.Sprintf("reguła '%s' zaktualizowana (id %d)", r.Name, id), func() error { return s.GitHub.UpdateRuleset(ctx, repo, id, r.File) })
		} else {
			err = s.write(fmt.Sprintf("reguła '%s' utworzona", r.Name), func() error { return s.GitHub.CreateRuleset(ctx, repo, r.File) })
		}
		if err != nil {
			return err
		}
	}
	for _, e := range envs {
		if err := s.environment(ctx, repo, e); err != nil {
			return err
		}
	}
	return nil
}

// environment stosuje środowisko i doprowadza listę gałęzi i tagów do tej
// z pliku: brakujące dodaje, nadmiarowe usuwa.
func (s Settings) environment(ctx context.Context, repo string, e reposettings.Environment) error {
	var ids []int64
	for _, login := range e.Reviewers {
		id, err := s.GitHub.UserID(ctx, login)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := s.write(fmt.Sprintf("środowisko '%s': recenzenci %v, tylko %v", e.Name, e.Reviewers, e.Policies), func() error {
		return s.GitHub.PutEnvironment(ctx, repo, e, ids)
	}); err != nil {
		return err
	}
	existing, err := s.GitHub.BranchPolicies(ctx, repo, e.Name)
	switch {
	case err != nil && s.DryRun:
		// Środowiska jeszcze nie ma — w trybie próbnym nie powstało.
		existing = nil
	case err != nil:
		return err
	}
	for _, want := range e.Policies {
		if _, ok := existing[want]; ok {
			continue
		}
		if err := s.write(fmt.Sprintf("środowisko '%s': dopuszczony %s %s", e.Name, want.Type, want.Name), func() error {
			return s.GitHub.AddBranchPolicy(ctx, repo, e.Name, want)
		}); err != nil {
			return err
		}
	}
	for _, p := range slices.SortedFunc(maps.Keys(existing), func(a, b reposettings.Policy) int {
		return strings.Compare(a.Type+" "+a.Name, b.Type+" "+b.Name)
	}) {
		if slices.Contains(e.Policies, p) {
			continue
		}
		id := existing[p]
		if err := s.write(fmt.Sprintf("środowisko '%s': usunięty %s %s", e.Name, p.Type, p.Name), func() error {
			return s.GitHub.DeleteBranchPolicy(ctx, repo, e.Name, id)
		}); err != nil {
			return err
		}
	}
	return nil
}
