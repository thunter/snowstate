//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/pkg/snowstate/ids"
	"github.com/thunter/snowstate/pkg/snowstate/store"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Dir(filepath.Dir(wd))
}

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "snowstate")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/snowstate")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return bin
}

// snowEnv points a snowstate process at the test schema. The key path is passed
// through; its contents are never read here.
func snowEnv() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TF_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"SNOWSTATE_SNOWFLAKE__ACCOUNT="+os.Getenv("SNOWSTATE_IT_ACCOUNT"),
		"SNOWSTATE_SNOWFLAKE__USER="+os.Getenv("SNOWSTATE_IT_USER"),
		"SNOWSTATE_SNOWFLAKE__ROLE="+os.Getenv("SNOWSTATE_IT_ROLE"),
		"SNOWSTATE_SNOWFLAKE__WAREHOUSE="+os.Getenv("SNOWSTATE_IT_WAREHOUSE"),
		"SNOWSTATE_SNOWFLAKE__DATABASE="+os.Getenv("SNOWSTATE_IT_DATABASE"),
		"SNOWSTATE_SNOWFLAKE__SCHEMA="+schema,
		"SNOWSTATE_SNOWFLAKE__AUTH="+os.Getenv("SNOWSTATE_IT_AUTH"),
		"SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH="+os.Getenv("SNOWSTATE_IT_PRIVATE_KEY_PATH"),
	)
}

func startSnowServer(t *testing.T, bin string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "serve", "--listen=127.0.0.1:0", "--auth-mode=none")
	cmd.Env = snowEnv()
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	addr := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var line struct {
				Msg, Addr string
			}
			if json.Unmarshal(scanner.Bytes(), &line) == nil && line.Addr != "" {
				addr <- line.Addr
			}
		}
	}()
	select {
	case a := <-addr:
		return "http://" + a
	case <-time.After(60 * time.Second):
		t.Fatal("server did not start")
		return ""
	}
}

type tf struct {
	t   *testing.T
	bin string
	dir string
	env []string
}

func newTF(t *testing.T, bin, url, key, body string) *tf {
	t.Helper()
	dir := t.TempDir()
	u := url + "/v1/states/" + key
	src := fmt.Sprintf("terraform {\n  backend \"http\" {\n    address = %q\n    lock_address = %q\n    unlock_address = %q\n  }\n}\n%s", u, u, u, body)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(src), 0o600))
	return &tf{t: t, bin: bin, dir: dir, env: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TF_IN_AUTOMATION=1", "TF_INPUT=0",
		"CHECKPOINT_DISABLE=1", "TF_HTTP_RETRY_WAIT_MIN=1",
	}}
}

func (w *tf) cmd(args ...string) *exec.Cmd {
	c := exec.Command(w.bin, args...)
	c.Dir, c.Env = w.dir, w.env
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return c
}

func (w *tf) ok(args ...string) string {
	w.t.Helper()
	out, err := w.cmd(args...).CombinedOutput()
	require.NoError(w.t, err, "%s\n%s", strings.Join(args, " "), out)
	return string(out)
}

func waitLock(t *testing.T, st store.Store, k string) *store.Lock {
	t.Helper()
	var l *store.Lock
	require.Eventually(t, func() bool {
		var err error
		l, err = st.GetLock(context.Background(), k)
		return err == nil && l != nil
	}, 2*time.Minute, 300*time.Millisecond, "lock never appeared")
	return l
}

func clientBinaries(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	out := map[string]string{}
	for name, rel := range map[string]string{
		"terraform": ".tools/terraform-1.17.1/terraform",
		"tofu":      ".tools/tofu-1.13.2/tofu",
	} {
		p := filepath.Join(root, rel)
		if _, err := os.Stat(p); err == nil {
			out[name] = p
		}
	}
	if len(out) == 0 {
		t.Skip("no client binaries: run make tools")
	}
	return out
}

// E2E scenarios 1-3 against real Snowflake (§12.3).
func TestE2EOverSnowflake(t *testing.T) {
	st := newStore(t)
	bin := buildBinary(t)
	url := startSnowServer(t, bin)
	slow := func(n int) string {
		return fmt.Sprintf(`resource "terraform_data" "slow" {
  provisioner "local-exec" {
    command = "sleep %d"
  }
}
`, n)
	}

	for name, client := range clientBinaries(t) {
		t.Run(name, func(t *testing.T) {
			t.Run("scenario1", func(t *testing.T) {
				k := "it/e2e1/" + ids.V4()[:8]
				w := newTF(t, client, url, k, `variable "n" {
  default = "a"
}
resource "terraform_data" "d" {
  input = var.n
}
`)
				w.ok("init")
				w.ok("apply", "-auto-approve")
				out, err := w.cmd("plan", "-detailed-exitcode").CombinedOutput()
				require.NoError(t, err, "no changes expected\n%s", out)
				w.ok("apply", "-auto-approve", "-var", "n=b")
				assert.Contains(t, w.ok("state", "list"), "terraform_data.d")

				pulled := w.ok("state", "pull")
				var doc map[string]any
				require.NoError(t, json.Unmarshal([]byte(pulled), &doc))
				doc["serial"] = doc["serial"].(float64) + 1
				raw, err := json.Marshal(doc)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(w.dir, "push.json"), raw, 0o600))
				w.ok("state", "push", "push.json")

				vs, err := st.ListVersions(context.Background(), k, 0)
				require.NoError(t, err)
				assert.GreaterOrEqual(t, len(vs), 3)
			})

			t.Run("scenario2", func(t *testing.T) {
				k := "it/e2e2/" + ids.V4()[:8]
				a := newTF(t, client, url, k, slow(20))
				b := newTF(t, client, url, k, slow(20))
				a.ok("init")
				b.ok("init")
				first := a.cmd("apply", "-auto-approve", "-no-color")
				var firstOut bytes.Buffer
				first.Stdout, first.Stderr = &firstOut, &firstOut
				require.NoError(t, first.Start())
				holder := waitLock(t, st, k)

				out, err := b.cmd("apply", "-auto-approve", "-no-color").CombinedOutput()
				require.Error(t, err, "second apply must lose\n%s", out)
				assert.Contains(t, string(out), "Error acquiring the state lock")
				assert.Contains(t, string(out), holder.LockID)
				assert.Contains(t, string(out), holder.Who)
				require.NoError(t, first.Wait(), firstOut.String())
			})

			t.Run("scenario3", func(t *testing.T) {
				k := "it/e2e3/" + ids.V4()[:8]
				w := newTF(t, client, url, k, slow(60))
				w.ok("init")
				c := w.cmd("apply", "-auto-approve")
				require.NoError(t, c.Start())
				held := waitLock(t, st, k)
				require.NoError(t, syscall.Kill(-c.Process.Pid, syscall.SIGKILL))
				_ = c.Wait()

				cur, err := st.GetLock(context.Background(), k)
				require.NoError(t, err)
				require.NotNil(t, cur, "SIGKILL leaves the lock held")

				assert.Contains(t, w.ok("force-unlock", "-force", held.LockID), "successfully unlocked")
				cur, err = st.GetLock(context.Background(), k)
				require.NoError(t, err)
				assert.Nil(t, cur)

				var n int
				require.NoError(t, admin.QueryRowContext(context.Background(), fmt.Sprintf(
					"SELECT COUNT(*) FROM %s.%s.AUDIT_LOG WHERE STATE_KEY = ? AND ACTION = 'FORCE_UNLOCK' AND RESULT = 'OK'",
					os.Getenv("SNOWSTATE_IT_DATABASE"), schema), k).Scan(&n))
				assert.Equal(t, 1, n)
			})
		})
	}
}

func TestCLIMigrateAndDoctor(t *testing.T) {
	newStore(t)
	bin := buildBinary(t)
	run := func(args ...string) (string, error) {
		c := exec.Command(bin, args...)
		c.Env = snowEnv()
		out, err := c.CombinedOutput()
		return string(out), err
	}
	out, err := run("migrate", "--dry-run")
	require.NoError(t, err, out)
	assert.Contains(t, out, "up to date")

	out, err = run("migrate")
	require.NoError(t, err, out)

	out, err = run("doctor")
	require.NoError(t, err, out)
	for _, want := range []string{"PASS  snowflake auth", "PASS  migrations", "PASS  hybrid tables", "PASS  privileges"} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, "FAIL")

	c := exec.Command(bin, "doctor")
	c.Env = append(snowEnv(), "SNOWSTATE_SNOWFLAKE__SCHEMA=NO_SUCH_SCHEMA_HERE")
	raw, err := c.CombinedOutput()
	require.Error(t, err, "doctor exits non-zero on failure")
	assert.Contains(t, string(raw), "FAIL")
}

// The first thing a new user runs: migrate into a schema that does not exist.
func TestCLIMigrateCreatesTheSchema(t *testing.T) {
	bin := buildBinary(t)
	fresh := "BACKEND_IT_NEW_" + strings.ToUpper(ids.V4()[:8])
	dbName := os.Getenv("SNOWSTATE_IT_DATABASE")
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+dbName+"."+fresh)
		assert.NoError(t, err)
	})
	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Env = append(snowEnv(), "SNOWSTATE_SNOWFLAKE__SCHEMA="+fresh)
		out, err := c.CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}
	assert.Contains(t, run("migrate", "--dry-run"), "would apply")
	assert.Contains(t, run("migrate"), "applied")
	assert.Contains(t, run("doctor"), "PASS  privileges")
}
