package ci

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDetect(t *testing.T) {
	assert.Nil(t, Detect(env(nil)))
	assert.Nil(t, Detect(env(map[string]string{"GITHUB_ACTIONS": "false"})))
	assert.NotNil(t, Detect(env(map[string]string{"GITHUB_ACTIONS": "true"})))
}

func TestGitHubContext(t *testing.T) {
	summary := filepath.Join(t.TempDir(), "summary.md")
	c := Detect(env(map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "o/r", "GITHUB_WORKFLOW": "tf", "GITHUB_JOB": "apply",
		"GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_ACTOR": "me", "GITHUB_SHA": "abc",
		"GITHUB_REF": "refs/heads/main", "GITHUB_STEP_SUMMARY": summary,
	}))
	assert.Equal(t, "github:o/r#42/2", c.AcquiredBy())

	d := c.AuditDetail()
	for _, k := range []string{"repository", "workflow", "job", "run_id", "run_attempt", "actor", "sha", "ref", "run_url"} {
		assert.NotEmpty(t, d[k], k)
	}
	assert.Equal(t, "https://github.com/o/r/actions/runs/42", d["run_url"])

	assert.Equal(t, "https://github.com/x/y/actions/runs/9", c.RunURLFor("github:x/y#9/1"))
	assert.Empty(t, c.RunURLFor("TODD"))

	require.NoError(t, c.WriteSummary("one"))
	require.NoError(t, c.WriteSummary("two"))
	raw, err := os.ReadFile(summary)
	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\n", string(raw))
}

func TestNoSummaryFile(t *testing.T) {
	c := Detect(env(map[string]string{"GITHUB_ACTIONS": "true"}))
	require.NoError(t, c.WriteSummary("x"))
}
