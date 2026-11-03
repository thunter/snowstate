//go:build integration

package integration

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/pkg/snowstate/ids"
)

// TestInstallFromNothing runs the real binary and the real Terraform through a
// first install that is killed partway, a re-run that finishes it, and a later
// Foundation change, on a new Install that it then removes. It needs a Seed on the
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

	install := func() (string, error) {
		cmd := exec.Command(bin, "install", "--database", database, "--dir", dir, "--binary", tf, "--auto-approve")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Kill the first run once it has applied and migrated.
	first := exec.Command(bin, "install", "--database", database, "--dir", dir, "--binary", tf, "--auto-approve")
	first.Env = env
	pipe, err := first.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, first.Start())
	scanner := bufio.NewScanner(pipe)
	killed := false
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "==> doctor as") {
			require.NoError(t, first.Process.Kill())
			killed = true
			break
		}
	}
	_ = first.Wait()
	require.True(t, killed, "the first run never reached doctor")

	out, err := install()
	require.NoError(t, err, out)
	require.Contains(t, out, "no changes to apply", "the re-run must not apply again")
	require.NotContains(t, out, "==> terraform apply")
	require.Contains(t, out, "PASS  session                user=SNOWSTATE_SEED role="+database+"_SVC")
	require.NotContains(t, out, "WARN  secondary roles")
	require.Contains(t, out, "done: the Foundation's state is in snowstate under snowstate/foundation/default")
	require.NoFileExists(t, filepath.Join(dir, "terraform.tfstate"))
	require.FileExists(t, filepath.Join(dir, "backend.tf"))

	env2 := append(append([]string{}, env...),
		"SNOWSTATE_SNOWFLAKE__USER=SNOWSTATE_SEED",
		"SNOWSTATE_SNOWFLAKE__ROLE="+database+"_SVC",
		"SNOWSTATE_SNOWFLAKE__WAREHOUSE="+database+"_WH",
		"SNOWSTATE_SNOWFLAKE__DATABASE="+database,
		"SNOWSTATE_SNOWFLAKE__SCHEMA=BACKEND",
	)
	countVersions := func() int {
		versions := exec.Command(bin, "state", "versions", "snowstate/foundation/default")
		versions.Env = env2
		listing, err := versions.CombinedOutput()
		require.NoError(t, err, string(listing))
		return strings.Count(strings.TrimSpace(string(listing)), "\n") // lines after the header
	}
	require.Equal(t, 1, countVersions())

	// A later change: add the service user. It applies through snowstate.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	vars, err := os.OpenFile(filepath.Join(dir, "terraform.tfvars"), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = fmt.Fprintf(vars, "service_public_key = %q\n", base64.StdEncoding.EncodeToString(der))
	require.NoError(t, err)
	require.NoError(t, vars.Close())

	out, err = install()
	require.NoError(t, err, out)
	require.Contains(t, out, "==> terraform apply")
	require.Contains(t, out, "==> done")
	require.Equal(t, 2, countVersions(), "the change wrote a second version")
}

// dropInstall removes an Install's objects. Dropping the database removes
// every state in it, which is what a test Install needs.
func dropInstall(t *testing.T, database string) {
	ctx := context.Background()
	for _, stmt := range []string{
		"DROP DATABASE IF EXISTS " + database,
		"DROP USER IF EXISTS " + database + "_SERVICE",
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
