// Package github to adapter GitHuba przez `gh` z mise: atestacje
// (Sigstore), ustawienia repozytorium, środowiska, biegi workflowów
// i issue. gh trzyma uwierzytelnienie autora (lokalnie) albo GITHUB_TOKEN
// zadania (w CI) — dft nie dotyka tokenu.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// CLI — program gh.
type CLI struct {
	Runner proc.Runner
}

func (c CLI) gh(ctx context.Context, args ...string) ([]byte, error) {
	res, err := c.Runner.Run(ctx, proc.Cmd{Name: "gh", Args: args})
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args[:min(3, len(args))], " "), err)
	}
	return res.Stdout, nil
}

// api woła `gh api` i dekoduje JSON do v.
func (c CLI) api(ctx context.Context, v any, args ...string) error {
	out, err := c.gh(ctx, append([]string{"api"}, args...)...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("gh api %s: nieczytelna odpowiedź: %w", args[0], err)
	}
	return nil
}
