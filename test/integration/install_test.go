//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/pkg/snowstate/ids"
)

// TestInstallFromNothing runs the real binary and the real Terraform through a
// first install on a new Install, then removes it. It needs a Seed on the
// account, with its key in SNOWSTATE_IT_SEED_PRIVATE_KEY_PATH, and a
// SNOWSTATE_IT_ROLE that sits above the Seed role, such as SYSADMIN, so the
// cleanup can drop what the Seed role owns.
func TestInstallFromNothing(t *testing.T) {
	seedKey := os.Getenv("SNOWSTATE_IT_SEED_PRIVATE_KEY_PATH")
	if seedKey == "" {
		t.Skip("SNOWSTATE_IT_SEED_PRIVATE_KEY_PATH not set")
	}
	tf := filepath.Join(repoRoot(t), ".tools", "terraform-1.17.1", "terraform")
	if _, err := os.Stat(tf); err != nil {
		t.Skip("Terraform not downloaded: run make tools")
	}
	bin := buildBinary(t)
	database := "IT_INSTALL_" + strings.ToUpper(strings.ReplaceAll(ids.V4(), "-", "")[:8])
	t.Cleanup(func() { dropInstall(t, database) })

	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TF_") && !strings.HasPrefix(kv, "SNOWSTATE_") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"SNOWSTATE_SNOWFLAKE__ACCOUNT="+os.Getenv("SNOWSTATE_IT_ACCOUNT"),
		"SNOWSTATE_SNOWFLAKE__AUTH=keypair",
		"SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH="+seedKey,
	)
	dir := filepath.Join(t.TempDir(), "snowstate-foundation")

	cmd := exec.Command(bin, "install", "--database", database, "--dir", dir, "--binary", tf, "--auto-approve")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), "PASS  session                user=SNOWSTATE_SEED role="+database+"_SVC")
	require.NotContains(t, string(out), "WARN  secondary roles")
	require.Contains(t, string(out), "done: the Foundation's state is in snowstate under snowstate/foundation/default")
	require.NoFileExists(t, filepath.Join(dir, "terraform.tfstate"))
	require.FileExists(t, filepath.Join(dir, "backend.tf"))

	versions := exec.Command(bin, "state", "versions", "snowstate/foundation/default")
	versions.Env = append(env,
		"SNOWSTATE_SNOWFLAKE__USER=SNOWSTATE_SEED",
		"SNOWSTATE_SNOWFLAKE__ROLE="+database+"_SVC",
		"SNOWSTATE_SNOWFLAKE__WAREHOUSE="+database+"_WH",
		"SNOWSTATE_SNOWFLAKE__DATABASE="+database,
		"SNOWSTATE_SNOWFLAKE__SCHEMA=BACKEND",
	)
	listing, err := versions.CombinedOutput()
	require.NoError(t, err, string(listing))
	require.Equal(t, 2, strings.Count(strings.TrimSpace(string(listing)), "\n")+1, "a header and one version:\n%s", listing)
}

// dropInstall removes an Install's objects. Dropping the database removes
// every state in it, which is what a test Install needs.
func dropInstall(t *testing.T, database string) {
	ctx := context.Background()
	for _, stmt := range []string{
		"DROP DATABASE IF EXISTS " + database,
		"DROP WAREHOUSE IF EXISTS " + database + "_WH",
		"DROP ROLE IF EXISTS " + database + "_ADMIN",
		"DROP ROLE IF EXISTS " + database + "_SVC",
		"DROP ROLE IF EXISTS " + database + "_READER",
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Errorf("cleanup: %s: %v", stmt, err)
		}
	}
}
