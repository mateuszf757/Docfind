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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	return s.environments(ctx, repo)
}

// Environment — środowisko GitHuba z .github/environments/<nazwa>.json.
type Environment struct {
	Name              string `json:"name"`
	WaitTimer         int    `json:"wait_timer"`
	PreventSelfReview bool   `json:"prevent_self_review"`
	// Reviewers — loginy użytkowników, których zgoda odblokowuje zadanie.
	Reviewers    []string `json:"reviewers"`
	BranchPolicy struct {
		ProtectedBranches    bool `json:"protected_branches"`
		CustomBranchPolicies bool `json:"custom_branch_policies"`
	} `json:"deployment_branch_policy"`
	// Policies — gałęzie (branch) i tagi (tag), z których wolno wdrażać.
	Policies []Policy `json:"policies"`
}

// Policy — wzorzec gałęzi albo tagu dopuszczonych do środowiska.
type Policy struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// LoadEnvironments czyta definicje środowisk GitHuba ściśle: nieznane pole to
// literówka, która inaczej po cichu zostawiłaby środowisko bez ochrony.
func LoadEnvironments(root string) ([]Environment, error) {
	files, err := filepath.Glob(filepath.Join(root, ".github", "environments", "*.json"))
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	var envs []Environment
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var e Environment
		if err := dec.Decode(&e); err != nil {
			return nil, cli.Unmet("%s: %v", file, err)
		}
		if e.Name+".json" != filepath.Base(file) {
			return nil, cli.Unmet("%s: name=%q, a plik nazywa się %s", file, e.Name, filepath.Base(file))
		}
		if !e.BranchPolicy.CustomBranchPolicies || e.BranchPolicy.ProtectedBranches || len(e.Policies) == 0 {
			return nil, cli.Unmet("%s: środowisko bez własnej listy gałęzi i tagów przyjmuje wdrożenie z dowolnej gałęzi", file)
		}
		for _, p := range e.Policies {
			if p.Name == "" || (p.Type != "branch" && p.Type != "tag") {
				return nil, cli.Unmet("%s: polityka %+v — nazwa i typ branch albo tag", file, p)
			}
		}
		envs = append(envs, e)
	}
	return envs, nil
}

func (s Settings) environments(ctx context.Context, repo string) error {
	envs, err := LoadEnvironments(s.Root)
	if err != nil {
		return err
	}
	for _, e := range envs {
		type reviewer struct {
			Type string `json:"type"`
			ID   int64  `json:"id"`
		}
		body := struct {
			WaitTimer         int        `json:"wait_timer"`
			PreventSelfReview bool       `json:"prevent_self_review"`
			Reviewers         []reviewer `json:"reviewers"`
			BranchPolicy      any        `json:"deployment_branch_policy"`
		}{WaitTimer: e.WaitTimer, PreventSelfReview: e.PreventSelfReview, Reviewers: []reviewer{}, BranchPolicy: e.BranchPolicy}
		for _, login := range e.Reviewers {
			out, err := s.gh(ctx, "api", "users/"+login, "--jq", ".id")
			if err != nil {
				return err
			}
			id, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			if err != nil {
				return fmt.Errorf("identyfikator użytkownika %s: %q", login, out)
			}
			body.Reviewers = append(body.Reviewers, reviewer{Type: "User", ID: id})
		}
		input, err := os.CreateTemp("", "dft-env-*.json")
		if err != nil {
			return err
		}
		err = json.NewEncoder(input).Encode(body)
		_ = input.Close()
		defer os.Remove(input.Name())
		if err != nil {
			return err
		}
		if err := s.write(ctx, fmt.Sprintf("środowisko '%s': recenzenci %v, tylko %v", e.Name, e.Reviewers, e.Policies),
			"api", "--method", "PUT", "repos/"+repo+"/environments/"+e.Name, "--input", input.Name(), "--silent"); err != nil {
			return err
		}
		if err := s.syncPolicies(ctx, repo, e); err != nil {
			return err
		}
	}
	return nil
}

// syncPolicies doprowadza listę gałęzi i tagów środowiska do tej z pliku:
// brakujące dodaje, nadmiarowe usuwa.
func (s Settings) syncPolicies(ctx context.Context, repo string, e Environment) error {
	path := "repos/" + repo + "/environments/" + e.Name + "/deployment-branch-policies"
	var existing struct {
		Policies []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"branch_policies"`
	}
	out, err := s.gh(ctx, "api", path)
	switch {
	case err != nil && s.DryRun:
		// Środowiska jeszcze nie ma — w trybie próbnym nie powstało.
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(out, &existing); err != nil {
			return fmt.Errorf("nieczytelne polityki środowiska %s: %w", e.Name, err)
		}
	}
	for _, want := range e.Policies {
		if slices.ContainsFunc(existing.Policies, func(p struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		}) bool {
			return p.Name == want.Name && p.Type == want.Type
		}) {
			continue
		}
		if err := s.write(ctx, fmt.Sprintf("środowisko '%s': dopuszczony %s %s", e.Name, want.Type, want.Name),
			"api", "--method", "POST", path, "-f", "name="+want.Name, "-f", "type="+want.Type, "--silent"); err != nil {
			return err
		}
	}
	for _, p := range existing.Policies {
		if slices.Contains(e.Policies, Policy{Name: p.Name, Type: p.Type}) {
			continue
		}
		if err := s.write(ctx, fmt.Sprintf("środowisko '%s': usunięty %s %s", e.Name, p.Type, p.Name),
			"api", "--method", "DELETE", fmt.Sprintf("%s/%d", path, p.ID), "--silent"); err != nil {
			return err
		}
	}
	return nil
}

// RequireApproval odmawia, gdy środowisko GitHuba name nie wymaga zgody
// człowieka. GitHub tworzy środowisko bez żadnych reguł przy pierwszym
// zadaniu, które go użyje — zanim autor zastosuje .github/environments/,
// zadanie z `environment: production` przeszłoby bez nikogo.
func RequireApproval(ctx context.Context, r proc.Runner, repo, name string) error {
	res, err := r.Run(ctx, proc.Cmd{Name: "gh", Args: []string{"api", "repos/" + repo + "/environments/" + name}})
	if err != nil {
		return fmt.Errorf("środowisko %s: %w", name, err)
	}
	var env struct {
		ProtectionRules []struct {
			Type      string            `json:"type"`
			Reviewers []json.RawMessage `json:"reviewers"`
		} `json:"protection_rules"`
	}
	if err := json.Unmarshal(res.Stdout, &env); err != nil {
		return fmt.Errorf("środowisko %s: nieczytelna odpowiedź: %w", name, err)
	}
	for _, rule := range env.ProtectionRules {
		if rule.Type == "required_reviewers" && len(rule.Reviewers) > 0 {
			return nil
		}
	}
	return cli.Unmet("środowisko GitHuba %s nie wymaga zgody — zastosuj .github/environments/ (./bin/mise run repo:settings, konto admina)", name)
}
