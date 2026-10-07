// Package github stosuje ustawienia repozytorium zapisane jako kod:
// rulesety z .github/rulesets, auto-merge i wymóg przypinania akcji pełnym
// SHA (dawniej ci/apply-repo-settings.sh).
//
// Przez `gh api` z mise: gh trzyma uwierzytelnienie autora (z uprawnieniami
// admina repozytorium), a dft nie dotyka tokenu. Reguła gałęzi main jest
// warunkiem bezpieczeństwa auto-merge łatek od Dependabota: `gh pr merge
// --auto` czeka tylko na sprawdzenia wymagane przez regułę, więc bez niej
// scaliłby PR natychmiast, bez żadnego CI (decyzja 22).
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// Settings stosuje ustawienia.
type Settings struct {
	Root   string
	Runner proc.Runner
	// DryRun — tylko wypisuje, co by zrobił; niczego nie zmienia.
	DryRun bool
}

func (s Settings) gh(ctx context.Context, args ...string) ([]byte, error) {
	res, err := s.Runner.Run(ctx, proc.Cmd{Name: "gh", Args: args})
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args[:min(3, len(args))], " "), err)
	}
	return res.Stdout, nil
}

// write wykonuje zmianę albo, w trybie próbnym, tylko ją opisuje.
func (s Settings) write(ctx context.Context, what string, args ...string) error {
	if s.DryRun {
		cli.Step("[próba] %s: gh %s", what, strings.Join(args, " "))
		return nil
	}
	if _, err := s.gh(ctx, args...); err != nil {
		return err
	}
	cli.Step("%s", what)
	return nil
}

// Apply stosuje wszystkie ustawienia. Idempotentne: istniejąca reguła
// o tej samej nazwie jest aktualizowana, a nie dublowana.
func (s Settings) Apply(ctx context.Context) error {
	if _, err := s.gh(ctx, "auth", "status"); err != nil {
		return fmt.Errorf("gh nie jest zalogowany — ./bin/mise exec -- gh auth login (%w)", err)
	}
	out, err := s.gh(ctx, "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		return err
	}
	repo := strings.TrimSpace(string(out))
	cli.Step("repozytorium: %s", repo)

	// Auto-merge musi być włączony, inaczej `gh pr merge --auto` kończy się
	// błędem.
	if err := s.write(ctx, "auto-merge włączony", "api", "--method", "PATCH", "repos/"+repo, "-F", "allow_auto_merge=true", "--silent"); err != nil {
		return err
	}

	// Przypinanie akcji pełnym SHA wymuszane przez GitHuba, a nie tylko przez
	// dyscyplinę: workflow z akcją przypiętą tagiem nie ruszy. Pozostałe pola
	// polityki są przepisywane z bieżących ustawień, żeby ich nie zmienić.
	out, err = s.gh(ctx, "api", "repos/"+repo+"/actions/permissions")
	if err != nil {
		return err
	}
	var permissions struct {
		Enabled        bool   `json:"enabled"`
		AllowedActions string `json:"allowed_actions"`
	}
	if err := json.Unmarshal(out, &permissions); err != nil || permissions.AllowedActions == "" {
		return fmt.Errorf("nieczytelne uprawnienia akcji: %q", out)
	}
	if err := s.write(ctx, fmt.Sprintf("akcje: wymagane przypięcie pełnym SHA (allowed_actions=%s)", permissions.AllowedActions),
		"api", "--method", "PUT", "repos/"+repo+"/actions/permissions",
		"-F", fmt.Sprintf("enabled=%t", permissions.Enabled), "-f", "allowed_actions="+permissions.AllowedActions,
		"-F", "sha_pinning_required=true", "--silent"); err != nil {
		return err
	}

	out, err = s.gh(ctx, "api", "repos/"+repo+"/rulesets")
	if err != nil {
		return err
	}
	var existing []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &existing); err != nil {
		return fmt.Errorf("nieczytelna lista reguł: %w", err)
	}
	files, err := filepath.Glob(filepath.Join(s.Root, ".github", "rulesets", "*.json"))
	if err != nil {
		return err
	}
	slices.Sort(files)
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var ruleset struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &ruleset); err != nil || ruleset.Name == "" {
			return cli.Unmet("%s: brak nazwy reguły", file)
		}
		idx := slices.IndexFunc(existing, func(r struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}) bool {
			return r.Name == ruleset.Name
		})
		if idx >= 0 {
			id := existing[idx].ID
			err = s.write(ctx, fmt.Sprintf("reguła '%s' zaktualizowana (id %d)", ruleset.Name, id),
				"api", "--method", "PUT", fmt.Sprintf("repos/%s/rulesets/%d", repo, id), "--input", file, "--silent")
		} else {
			err = s.write(ctx, fmt.Sprintf("reguła '%s' utworzona", ruleset.Name),
				"api", "--method", "POST", "repos/"+repo+"/rulesets", "--input", file, "--silent")
		}
		if err != nil {
			return err
		}
	}
	return nil
}
