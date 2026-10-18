package github

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// WorkflowRuns zwraca zakończone biegi workflowu na gałęzi, od najnowszego.
func (c CLI) WorkflowRuns(ctx context.Context, workflow, branch string, limit int) ([]ci.Run, error) {
	var runs struct {
		Runs []ci.Run `json:"workflow_runs"`
	}
	path := fmt.Sprintf("repos/{owner}/{repo}/actions/workflows/%s/runs?branch=%s&status=completed&per_page=%d", workflow, branch, limit)
	if err := c.api(ctx, &runs, path); err != nil {
		return nil, err
	}
	return runs.Runs, nil
}

// MergedPullRequests zwraca scalone PR-y do gałęzi base, od ostatnio
// scalonego; limit — ile ostatnio zamkniętych PR-ów przejrzeć.
func (c CLI) MergedPullRequests(ctx context.Context, base string, limit int) ([]ci.PullRequest, error) {
	var closed []struct {
		Number   int     `json:"number"`
		MergedAt *string `json:"merged_at"`
		URL      string  `json:"html_url"`
		Head     struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	path := fmt.Sprintf("repos/{owner}/{repo}/pulls?state=closed&base=%s&sort=updated&direction=desc&per_page=%d", base, limit)
	if err := c.api(ctx, &closed, path); err != nil {
		return nil, err
	}
	var merged []ci.PullRequest
	for _, pr := range closed {
		if pr.MergedAt != nil {
			merged = append(merged, ci.PullRequest{Number: pr.Number, HeadSHA: pr.Head.SHA, MergedAt: *pr.MergedAt, URL: pr.URL})
		}
	}
	// „updated” to nie kolejność scalenia: komentarz po scaleniu przesuwa PR.
	slices.SortStableFunc(merged, func(a, b ci.PullRequest) int { return strings.Compare(b.MergedAt, a.MergedAt) })
	return merged, nil
}

// RunsForCommit zwraca zakończone biegi workflowu dla commita i zdarzenia,
// od najnowszego.
func (c CLI) RunsForCommit(ctx context.Context, workflow, sha, event string) ([]ci.Run, error) {
	var runs struct {
		Runs []ci.Run `json:"workflow_runs"`
	}
	path := fmt.Sprintf("repos/{owner}/{repo}/actions/workflows/%s/runs?head_sha=%s&event=%s&status=completed&per_page=10", workflow, sha, event)
	if err := c.api(ctx, &runs, path); err != nil {
		return nil, err
	}
	return runs.Runs, nil
}

// RunArtifacts zwraca artefakty biegu ze skrótami SHA-256 archiwów, które
// liczy GitHub — także artefaktów po retencji (expired), bez treści.
func (c CLI) RunArtifacts(ctx context.Context, runID int64) ([]artifact.EvidenceArtifact, error) {
	var list struct {
		Artifacts []artifact.EvidenceArtifact `json:"artifacts"`
	}
	if err := c.api(ctx, &list, fmt.Sprintf("repos/{owner}/{repo}/actions/runs/%d/artifacts?per_page=100", runID)); err != nil {
		return nil, err
	}
	return list.Artifacts, nil
}

// RunsByEvent zwraca zakończone biegi workflowu dla zdarzenia, od
// najnowszego, ze wszystkich gałęzi.
func (c CLI) RunsByEvent(ctx context.Context, workflow, event string, limit int) ([]ci.Run, error) {
	var runs struct {
		Runs []ci.Run `json:"workflow_runs"`
	}
	path := fmt.Sprintf("repos/{owner}/{repo}/actions/workflows/%s/runs?event=%s&status=completed&per_page=%d", workflow, event, min(limit, 100))
	if err := c.api(ctx, &runs, path); err != nil {
		return nil, err
	}
	return runs.Runs, nil
}

// Jobs zwraca zadania biegu.
func (c CLI) Jobs(ctx context.Context, runID int64) ([]ci.Job, error) {
	var jobs struct {
		Jobs []ci.Job `json:"jobs"`
	}
	if err := c.api(ctx, &jobs, fmt.Sprintf("repos/{owner}/{repo}/actions/runs/%d/jobs", runID)); err != nil {
		return nil, err
	}
	return jobs.Jobs, nil
}

// FindIssue zwraca numer otwartego issue o dokładnie tym tytule; 0 — nie ma.
func (c CLI) FindIssue(ctx context.Context, title string) (int, error) {
	out, err := c.gh(ctx, "issue", "list", "--state", "open", "--search", title+" in:title", "--json", "number,title")
	if err != nil {
		return 0, err
	}
	var issues []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(out, &issues); err != nil {
		return 0, fmt.Errorf("gh issue list: %w", err)
	}
	for _, i := range issues {
		if i.Title == title {
			return i.Number, nil
		}
	}
	return 0, nil
}

// CreateIssue otwiera issue.
func (c CLI) CreateIssue(ctx context.Context, title, body string) error {
	_, err := c.gh(ctx, "issue", "create", "--title", title, "--body", body)
	return err
}

// EditIssue podmienia treść issue.
func (c CLI) EditIssue(ctx context.Context, number int, body string) error {
	_, err := c.gh(ctx, "issue", "edit", strconv.Itoa(number), "--body", body)
	return err
}

// CloseIssue zamyka issue z komentarzem.
func (c CLI) CloseIssue(ctx context.Context, number int, comment string) error {
	_, err := c.gh(ctx, "issue", "close", strconv.Itoa(number), "--comment", comment)
	return err
}

// ReleaseTag zwraca commit, na który wskazuje tag z adnotacją, i treść tagu
// — z GitHuba, a nie z lokalnego klona: checkout na tagu potrafi zastąpić tag
// z adnotacją lekkim, a liczy się to, co jest w repozytorium. Lekki tag to
// warunek niespełniony: wydanie to tag z adnotacją z `dft release`.
func (c CLI) ReleaseTag(ctx context.Context, name string) (commit, message string, err error) {
	var ref struct {
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := c.api(ctx, &ref, "repos/{owner}/{repo}/git/ref/tags/"+name); err != nil {
		return "", "", err
	}
	if ref.Object.Type != "tag" {
		return "", "", cli.Unmet("tag %s jest lekki (%s) — wydanie to tag z adnotacją z ./bin/mise run release", name, ref.Object.Type)
	}
	var tag struct {
		Message string `json:"message"`
		Object  struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := c.api(ctx, &tag, "repos/{owner}/{repo}/git/tags/"+ref.Object.SHA); err != nil {
		return "", "", err
	}
	if tag.Object.Type != "commit" {
		return "", "", cli.Unmet("tag %s wskazuje na %s, a nie na commit", name, tag.Object.Type)
	}
	return tag.Object.SHA, tag.Message, nil
}
