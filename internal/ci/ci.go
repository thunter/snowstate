// Package ci detects the CI system a sidecar runs under. GitHub Actions is the
// only implementation, and others add a Detect case and a Context.
package ci

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Context describes one CI run.
type Context interface {
	// AuditDetail goes into the DETAIL of every audit row.
	AuditDetail() map[string]any
	// AcquiredBy names the run on locks it takes.
	AcquiredBy() string
	// RunURLFor returns a holder's run URL from its ACQUIRED_BY, when known.
	RunURLFor(acquiredBy string) string
	// WriteSummary publishes Markdown for the run. It does nothing where the
	// system has no such feature.
	WriteSummary(markdown string) error
}

// Detect returns the current CI context, or nil outside CI.
func Detect(getenv func(string) string) Context {
	if getenv("GITHUB_ACTIONS") == "true" {
		return newGitHub(getenv)
	}
	return nil
}

type github struct {
	repository, workflow, job, runID, attempt, actor, sha, ref, server, summaryPath string
}

func newGitHub(getenv func(string) string) *github {
	server := getenv("GITHUB_SERVER_URL")
	if server == "" {
		server = "https://github.com"
	}
	return &github{
		repository:  getenv("GITHUB_REPOSITORY"),
		workflow:    getenv("GITHUB_WORKFLOW"),
		job:         getenv("GITHUB_JOB"),
		runID:       getenv("GITHUB_RUN_ID"),
		attempt:     getenv("GITHUB_RUN_ATTEMPT"),
		actor:       getenv("GITHUB_ACTOR"),
		sha:         getenv("GITHUB_SHA"),
		ref:         getenv("GITHUB_REF"),
		server:      server,
		summaryPath: getenv("GITHUB_STEP_SUMMARY"),
	}
}

func (g *github) runURL(repository, runID string) string {
	return fmt.Sprintf("%s/%s/actions/runs/%s", g.server, repository, runID)
}

func (g *github) AuditDetail() map[string]any {
	return map[string]any{
		"repository": g.repository, "workflow": g.workflow, "job": g.job, "run_id": g.runID,
		"run_attempt": g.attempt, "actor": g.actor, "sha": g.sha, "ref": g.ref,
		"run_url": g.runURL(g.repository, g.runID),
	}
}

func (g *github) AcquiredBy() string {
	return fmt.Sprintf("github:%s#%s/%s", g.repository, g.runID, g.attempt)
}

var acquiredByRE = regexp.MustCompile(`^github:([^#]+)#(\d+)/\d+$`)

func (g *github) RunURLFor(acquiredBy string) string {
	m := acquiredByRE.FindStringSubmatch(acquiredBy)
	if m == nil {
		return ""
	}
	return g.runURL(m[1], m[2])
}

func (g *github) WriteSummary(markdown string) error {
	if g.summaryPath == "" {
		return nil
	}
	f, err := os.OpenFile(g.summaryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // set by the runner
	if err != nil {
		return err
	}
	_, werr := f.WriteString(strings.TrimRight(markdown, "\n") + "\n")
	if err := f.Close(); werr == nil {
		werr = err
	}
	return werr
}
