package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/reposettings"
)

// Repo zwraca repozytorium bieżącego katalogu (właściciel/nazwa); wymaga
// zalogowanego gh.
func (c CLI) Repo(ctx context.Context) (string, error) {
	if _, err := c.gh(ctx, "auth", "status"); err != nil {
		return "", fmt.Errorf("gh nie jest zalogowany — ./bin/mise exec -- gh auth login (%w)", err)
	}
	out, err := c.gh(ctx, "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// EnableAutoMerge włącza auto-merge — bez niego `gh pr merge --auto` kończy
// się błędem.
func (c CLI) EnableAutoMerge(ctx context.Context, repo string) error {
	_, err := c.gh(ctx, "api", "--method", "PATCH", "repos/"+repo, "-F", "allow_auto_merge=true", "--silent")
	return err
}

// EnableSecretScanning włącza skanowanie sekretów i blokadę pushu z sekretem
// o znanym kształcie (push protection). W publicznym repozytorium oba są
// domyślne, ale wyłącza je jedno kliknięcie w ustawieniach — stąd kod.
func (c CLI) EnableSecretScanning(ctx context.Context, repo string) error {
	_, err := c.gh(ctx, "api", "--method", "PATCH", "repos/"+repo,
		"-f", "security_and_analysis[secret_scanning][status]=enabled",
		"-f", "security_and_analysis[secret_scanning_push_protection][status]=enabled",
		"--silent")
	return err
}

// ActionsPermissions zwraca politykę akcji repozytorium.
func (c CLI) ActionsPermissions(ctx context.Context, repo string) (enabled bool, allowed string, err error) {
	var p struct {
		Enabled        bool   `json:"enabled"`
		AllowedActions string `json:"allowed_actions"`
	}
	if err := c.api(ctx, &p, "repos/"+repo+"/actions/permissions"); err != nil {
		return false, "", err
	}
	if p.AllowedActions == "" {
		return false, "", errors.New("nieczytelne uprawnienia akcji: brak allowed_actions")
	}
	return p.Enabled, p.AllowedActions, nil
}

// RequireSHAPinning wymusza przypinanie akcji pełnym SHA; pozostałe pola
// polityki są przepisywane z podanych, żeby ich nie zmienić.
func (c CLI) RequireSHAPinning(ctx context.Context, repo string, enabled bool, allowed string) error {
	_, err := c.gh(ctx, "api", "--method", "PUT", "repos/"+repo+"/actions/permissions",
		"-F", fmt.Sprintf("enabled=%t", enabled), "-f", "allowed_actions="+allowed,
		"-F", "sha_pinning_required=true", "--silent")
	return err
}

// Rulesets zwraca reguły repozytorium: nazwa → identyfikator.
func (c CLI) Rulesets(ctx context.Context, repo string) (map[string]int64, error) {
	var list []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := c.api(ctx, &list, "repos/"+repo+"/rulesets"); err != nil {
		return nil, err
	}
	ids := map[string]int64{}
	for _, r := range list {
		ids[r.Name] = r.ID
	}
	return ids, nil
}

// CreateRuleset tworzy regułę z pliku.
func (c CLI) CreateRuleset(ctx context.Context, repo, file string) error {
	_, err := c.gh(ctx, "api", "--method", "POST", "repos/"+repo+"/rulesets", "--input", file, "--silent")
	return err
}

// UpdateRuleset zastępuje regułę id treścią pliku.
func (c CLI) UpdateRuleset(ctx context.Context, repo string, id int64, file string) error {
	_, err := c.gh(ctx, "api", "--method", "PUT", fmt.Sprintf("repos/%s/rulesets/%d", repo, id), "--input", file, "--silent")
	return err
}

// UserID zwraca identyfikator użytkownika — API środowisk przyjmuje
// recenzentów po identyfikatorze, nie po loginie.
func (c CLI) UserID(ctx context.Context, login string) (int64, error) {
	out, err := c.gh(ctx, "api", "users/"+login, "--jq", ".id")
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("identyfikator użytkownika %s: %q", login, out)
	}
	return id, nil
}

// PutEnvironment tworzy albo aktualizuje środowisko: recenzenci (po
// identyfikatorach), opóźnienie i zasada wyboru gałęzi.
func (c CLI) PutEnvironment(ctx context.Context, repo string, e reposettings.Environment, reviewerIDs []int64) (err error) {
	type reviewer struct {
		Type string `json:"type"`
		ID   int64  `json:"id"`
	}
	body := struct {
		WaitTimer         int                       `json:"wait_timer"`
		PreventSelfReview bool                      `json:"prevent_self_review"`
		Reviewers         []reviewer                `json:"reviewers"`
		BranchPolicy      reposettings.BranchPolicy `json:"deployment_branch_policy"`
	}{WaitTimer: e.WaitTimer, PreventSelfReview: e.PreventSelfReview, Reviewers: []reviewer{}, BranchPolicy: e.BranchPolicy}
	for _, id := range reviewerIDs {
		body.Reviewers = append(body.Reviewers, reviewer{Type: "User", ID: id})
	}
	input, err := os.CreateTemp("", "dft-env-*.json")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.Remove(input.Name())) }()
	err = json.NewEncoder(input).Encode(body)
	if cerr := input.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_, err = c.gh(ctx, "api", "--method", "PUT", "repos/"+repo+"/environments/"+e.Name, "--input", input.Name(), "--silent")
	return err
}

func policiesPath(repo, env string) string {
	return "repos/" + repo + "/environments/" + env + "/deployment-branch-policies"
}

// BranchPolicies zwraca gałęzie i tagi dopuszczone do środowiska:
// polityka → identyfikator.
func (c CLI) BranchPolicies(ctx context.Context, repo, env string) (map[reposettings.Policy]int64, error) {
	var existing struct {
		Policies []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"branch_policies"`
	}
	if err := c.api(ctx, &existing, policiesPath(repo, env)); err != nil {
		return nil, err
	}
	ids := map[reposettings.Policy]int64{}
	for _, p := range existing.Policies {
		ids[reposettings.Policy{Name: p.Name, Type: p.Type}] = p.ID
	}
	return ids, nil
}

// AddBranchPolicy dopuszcza gałąź albo tag do środowiska.
func (c CLI) AddBranchPolicy(ctx context.Context, repo, env string, p reposettings.Policy) error {
	_, err := c.gh(ctx, "api", "--method", "POST", policiesPath(repo, env), "-f", "name="+p.Name, "-f", "type="+p.Type, "--silent")
	return err
}

// DeleteBranchPolicy usuwa dopuszczenie id.
func (c CLI) DeleteBranchPolicy(ctx context.Context, repo, env string, id int64) error {
	_, err := c.gh(ctx, "api", "--method", "DELETE", fmt.Sprintf("%s/%d", policiesPath(repo, env), id), "--silent")
	return err
}

// RequiresApproval mówi, czy środowisko GitHuba wymaga zgody recenzenta.
// GitHub tworzy środowisko bez żadnych reguł przy pierwszym zadaniu, które go
// użyje — samo istnienie środowiska niczego nie gwarantuje.
func (c CLI) RequiresApproval(ctx context.Context, repo, env string) (bool, error) {
	var e struct {
		ProtectionRules []struct {
			Type      string            `json:"type"`
			Reviewers []json.RawMessage `json:"reviewers"`
		} `json:"protection_rules"`
	}
	if err := c.api(ctx, &e, "repos/"+repo+"/environments/"+env); err != nil {
		return false, fmt.Errorf("środowisko %s: %w", env, err)
	}
	for _, rule := range e.ProtectionRules {
		if rule.Type == "required_reviewers" && len(rule.Reviewers) > 0 {
			return true, nil
		}
	}
	return false, nil
}
