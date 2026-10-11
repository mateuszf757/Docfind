package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
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
