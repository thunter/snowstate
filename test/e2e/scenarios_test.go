package e2e

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/pkg/snowstate/store"
	"github.com/thunter/snowstate/pkg/snowstate/store/memory"
)

const fixture = `
variable "name" {
  type    = string
  default = "one"
}
resource "random_pet" "p" {
  keepers = { name = var.name }
}
resource "terraform_data" "d" {
  input = var.name
}
resource "local_file" "f" {
  filename = "${path.module}/out.txt"
  content  = random_pet.p.id
}
output "name" {
  value = terraform_data.d.output
}
`

// slowFixture holds the lock long enough to race, kill or signal.
func slowFixture(seconds int) string {
	return fmt.Sprintf(`
resource "terraform_data" "slow" {
  provisioner "local-exec" {
    command = "sleep %d"
  }
}
`, seconds)
}

// 1. init → apply → plan (no changes) → apply (change) → state list/pull/push.
func TestScenario1Lifecycle(t *testing.T) {
	forEachClient(t, func(t *testing.T, c client) {
		s := startServer(t)
		key := "platform/lifecycle/prod"
		w := newWorkdir(t, c, httpBackend(s, key)+fixture)

		w.ok("init")
		w.ok("apply", "-auto-approve")
		assert.Nil(t, s.lockQuiet(key), "lock released after apply")

		out, err := w.command("plan", "-detailed-exitcode").CombinedOutput()
		require.NoError(t, err, "no changes expected\n%s", out)

		w.ok("apply", "-auto-approve", "-var", "name=two")
		list := w.ok("state", "list")
		assert.Contains(t, list, "terraform_data.d")
		assert.Contains(t, list, "random_pet.p")

		pulled := w.ok("state", "pull")
		var st struct {
			Serial  int    `json:"serial"`
			Lineage string `json:"lineage"`
		}
		require.NoError(t, json.Unmarshal([]byte(pulled), &st))
		assert.Positive(t, st.Serial)

		// state push: bump the serial and write it back.
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(pulled), &doc))
		doc["serial"] = st.Serial + 1
		raw, err := json.Marshal(doc)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(w.dir, "push.json"), raw, 0o600))
		w.ok("state", "push", "push.json")

		var after struct {
			Serial int `json:"serial"`
		}
		require.NoError(t, json.Unmarshal([]byte(w.ok("state", "pull")), &after))
		assert.GreaterOrEqual(t, after.Serial, st.Serial+1, "the client may bump the serial again on persist")

		assert.Contains(t, auditActions(s.audit(t)), store.ActionPut)
		assert.Contains(t, auditActions(s.audit(t)), store.ActionUnlock)
	})
}

func auditActions(events []store.AuditEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Action)
	}
	return out
}

// 2. Two concurrent applies: exactly one wins, the loser names the holder.
func TestScenario2ConcurrentApplies(t *testing.T) {
	forEachClient(t, func(t *testing.T, c client) {
		s := startServer(t)
		key := "platform/race/prod"
		a := newWorkdir(t, c, httpBackend(s, key)+slowFixture(4))
		b := newWorkdir(t, c, httpBackend(s, key)+slowFixture(4))
		a.ok("init")
		b.ok("init")

		first := a.command("apply", "-auto-approve", "-no-color")
		var firstOut strings.Builder
		first.Stdout, first.Stderr = &firstOut, &firstOut
		require.NoError(t, first.Start())
		holder := s.waitForLock(t, key)

		out, err := b.run("apply", "-auto-approve", "-no-color")
		require.Error(t, err, "second apply must lose\n%s", out)
		assert.Contains(t, out, "Error acquiring the state lock")
		assert.Contains(t, out, "HTTP remote state already locked")
		assert.Contains(t, out, holder.LockID, "the error names the lock")
		assert.Contains(t, out, holder.Who, "the error names the holder")
		assert.Contains(t, out, "OperationTypeApply")

		require.NoError(t, first.Wait(), firstOut.String())
		assert.Nil(t, s.lockQuiet(key))
	})
}

// 3. SIGKILL mid-apply leaves the lock. force-unlock clears it and is audited.
func TestScenario3KillAndForceUnlock(t *testing.T) {
	forEachClient(t, func(t *testing.T, c client) {
		s := startServer(t)
		key := "platform/kill/prod"
		w := newWorkdir(t, c, httpBackend(s, key)+slowFixture(30))
		w.ok("init")

		cmd := w.command("apply", "-auto-approve")
		require.NoError(t, cmd.Start())
		held := s.waitForLock(t, key)
		require.NoError(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL))
		_ = cmd.Wait()

		assert.NotNil(t, s.lockQuiet(key), "SIGKILL leaves the lock held")

		out := w.ok("force-unlock", "-force", held.LockID)
		assert.Contains(t, out, "successfully unlocked")
		assert.Nil(t, s.lockQuiet(key))

		var row *store.AuditEvent
		for _, e := range s.audit(t) {
			if e.Action == store.ActionForceUnlock {
				row = &e
			}
		}
		require.NotNil(t, row, "audit shows FORCE_UNLOCK")
		assert.Equal(t, held.LockID, row.LockID)
		assert.Equal(t, store.ResultOK, row.Result)
		assert.Contains(t, row.Detail["removed_lock"], held.LockID)
	})
}

// 4. terraform_remote_state reading another key.
func TestScenario4RemoteState(t *testing.T) {
	forEachClient(t, func(t *testing.T, c client) {
		s := startServer(t)
		source := newWorkdir(t, c, httpBackend(s, "platform/source/prod")+`output "greeting" { value = "hello" }`)
		source.ok("init")
		source.ok("apply", "-auto-approve")

		reader := newWorkdir(t, c, httpBackend(s, "platform/reader/prod")+fmt.Sprintf(`
data "terraform_remote_state" "src" {
  backend = "http"
  config  = { address = %q }
}
output "got" { value = data.terraform_remote_state.src.outputs.greeting }
`, s.stateURL("platform/source/prod")))
		reader.ok("init")
		reader.ok("apply", "-auto-approve")
		assert.Contains(t, reader.ok("output", "-raw", "got"), "hello")
	})
}

// 5. Alternate /lock path with POST and DELETE.
func TestScenario5AlternateLockPath(t *testing.T) {
	forEachClient(t, func(t *testing.T, c client) {
		s := startServer(t)
		key := "platform/altlock/prod"
		u := s.stateURL(key)
		tf := fmt.Sprintf(`terraform {
  backend "http" {
    address        = %q
    lock_address   = %q
    unlock_address = %q
    lock_method    = "POST"
    unlock_method  = "DELETE"
  }
}
`, u, u+"/lock", u+"/lock")
		w := newWorkdir(t, c, tf+slowFixture(3))
		w.ok("init")
		cmd := w.command("apply", "-auto-approve")
		require.NoError(t, cmd.Start())
		s.waitForLock(t, key)
		require.NoError(t, cmd.Wait())
		assert.Nil(t, s.lockQuiet(key))
	})
}

// 7. Migrating from local state.
func TestScenario7MigrateFromLocal(t *testing.T) {
	forEachClient(t, func(t *testing.T, c client) {
		s := startServer(t)
		key := "platform/migrate/prod"
		body := `resource "terraform_data" "d" { input = "x" }`
		w := newWorkdir(t, c, body)
		w.ok("init")
		w.ok("apply", "-auto-approve")

		w.write(httpBackend(s, key) + body)
		w.ok("init", "-migrate-state", "-force-copy")

		status, _, raw := s.rawState(t, key)
		require.Equal(t, 200, status)
		assert.Contains(t, string(raw), "terraform_data")

		out, err := w.command("plan", "-detailed-exitcode").CombinedOutput()
		require.NoError(t, err, string(out))
	})
}

// 8. A large state round-trips with an identical MD5.
func TestScenario8LargeState(t *testing.T) {
	forEachClient(t, func(t *testing.T, c client) {
		s := startServer(t)
		key := "platform/large/prod"
		w := newWorkdir(t, c, httpBackend(s, key)+`output "pad" { value = "unused" }`)
		w.ok("init")
		w.ok("apply", "-auto-approve")

		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(w.ok("state", "pull")), &doc))
		blob := make([]byte, 11<<20) // 22 MiB of hex that gzip can only halve
		_, err := rand.Read(blob)
		require.NoError(t, err)
		doc["outputs"] = map[string]any{"pad": map[string]any{"value": hex.EncodeToString(blob), "type": "string"}}
		doc["serial"] = 100
		raw, err := json.Marshal(doc)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(raw), 20<<20)
		require.NoError(t, os.WriteFile(filepath.Join(w.dir, "big.json"), raw, 0o600))
		w.ok("state", "push", "big.json")

		status, header, stored := s.rawState(t, key)
		require.Equal(t, 200, status)
		require.GreaterOrEqual(t, len(stored), 20<<20)
		sum := md5.Sum(stored)
		assert.Equal(t, base64.StdEncoding.EncodeToString(sum[:]), header.Get("Content-MD5"))

		pulled := w.command("state", "pull")
		out, err := pulled.Output()
		require.NoError(t, err)
		// The CLI prints the state through a helper that appends one newline.
		require.Len(t, out, len(stored)+1)
		assert.Equal(t, sum, md5.Sum(out[:len(stored)]), "pull returns identical bytes")

	})
}

// sidecarWorkdir is a root module with an empty backend block, run through
// `snowstate exec` with a store shared across processes.
type sidecarWorkdir struct {
	*workdir
	key      string
	snapshot string
}

func newSidecarWorkdir(t *testing.T, c client, key, tf string) *sidecarWorkdir {
	t.Helper()
	w := newWorkdir(t, c, "terraform {\n  backend \"http\" {}\n}\n"+tf)
	snapshot := filepath.Join(t.TempDir(), "store.json")
	w.env = append(w.env, "SNOWSTATE_ALLOW_TEST_STORE=1", "SNOWSTATE_TEST_STORE_FILE="+snapshot, "SNOWSTATE_KEY="+key)
	return &sidecarWorkdir{workdir: w, key: key, snapshot: snapshot}
}

// sidecar builds `snowstate exec ... -- <client> args`.
func (w *sidecarWorkdir) sidecar(execFlags []string, args ...string) *exec.Cmd {
	full := append([]string{"exec", "--store=memory"}, execFlags...)
	full = append(full, "--", w.c.bin)
	full = append(full, args...)
	cmd := exec.Command(binary, full...)
	cmd.Dir = w.dir
	cmd.Env = w.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func (w *sidecarWorkdir) okSidecar(args ...string) string {
	w.t.Helper()
	out, err := w.sidecar(nil, args...).CombinedOutput()
	require.NoError(w.t, err, "%s\n%s", strings.Join(args, " "), out)
	return string(out)
}

// 6. snowstate exec -- terraform apply, end to end.
func TestScenario6Sidecar(t *testing.T) {
	forEachClient(t, func(t *testing.T, c client) {
		key := "platform/sidecar/prod"
		w := newSidecarWorkdir(t, c, key, `resource "terraform_data" "d" { input = "x" }
output "o" { value = "side" }
`)
		w.okSidecar("init", "-input=false")
		w.okSidecar("apply", "-auto-approve", "-input=false")

		out, err := w.sidecar(nil, "plan", "-detailed-exitcode", "-input=false").CombinedOutput()
		require.NoError(t, err, "no changes expected\n%s", out)

		locks, err := memory.ReadSnapshotLocks(w.snapshot)
		require.NoError(t, err)
		assert.Empty(t, locks)

		events, err := memory.ReadSnapshotAudit(w.snapshot)
		require.NoError(t, err)
		assert.Contains(t, auditActions(events), store.ActionPut)

		// Nothing about the sidecar may reach the working directory.
		raw, err := os.ReadFile(filepath.Join(w.dir, ".terraform", "terraform.tfstate"))
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "127.0.0.1")
	})
}

func TestSidecarExitCodeIsForwarded(t *testing.T) {
	env := append(baseEnv(), "SNOWSTATE_ALLOW_TEST_STORE=1", "SNOWSTATE_KEY=a/b")

	cmd := exec.Command(binary, "exec", "--store=memory", "--", "sh", "-c", "exit 7")
	cmd.Env = env
	err := cmd.Run()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 7, exit.ExitCode())

	cmd = exec.Command(binary, "exec", "--store=memory", "--", "sh", "-c", "kill -KILL $$")
	cmd.Env = env
	err = cmd.Run()
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 137, exit.ExitCode(), "a signalled child exits 128+n")

	cmd = exec.Command(binary, "exec", "--store=memory", "--", "sh", "-c", `test -n "$TF_HTTP_PASSWORD" && echo "$TF_HTTP_ADDRESS"`)
	cmd.Env = env
	out, err := cmd.Output()
	require.NoError(t, err)
	assert.Regexp(t, `^http://127\.0\.0\.1:\d+/v1/states/a/b\n$`, string(out))
}

func TestSidecarRequiresKeyAndTestStoreGate(t *testing.T) {
	cmd := exec.Command(binary, "exec", "--store=memory", "--", "true")
	cmd.Env = append(baseEnv(), "SNOWSTATE_ALLOW_TEST_STORE=1")
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "SNOWSTATE_KEY")

	cmd = exec.Command(binary, "exec", "--store=memory", "--key=a/b", "--", "true")
	cmd.Env = baseEnv()
	out, err = cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "SNOWSTATE_ALLOW_TEST_STORE")
}

// A single SIGINT must reach the child exactly once, even when the signal is
// sent to the sidecar's whole process group as a CI runner might.
func TestSidecarSignalReachesChildOnce(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "count")
	script := fmt.Sprintf(`trap 'echo hit >> %s' INT TERM
echo ready
n=0
while [ $n -lt 40 ]; do sleep 0.1; n=$((n+1)); done
`, marker)

	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		require.NoError(t, os.RemoveAll(marker))
		cmd := exec.Command(binary, "exec", "--store=memory", "--key=a/b", "--", "sh", "-c", script)
		cmd.Env = append(baseEnv(), "SNOWSTATE_ALLOW_TEST_STORE=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		stdout, err := cmd.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())
		_, err = stdout.Read(make([]byte, 16))
		require.NoError(t, err)

		require.NoError(t, syscall.Kill(-cmd.Process.Pid, sig), "signal the group")
		require.NoError(t, cmd.Wait())

		raw, err := os.ReadFile(marker)
		require.NoError(t, err)
		assert.Equal(t, 1, strings.Count(string(raw), "hit"), sig.String())
	}
}

// The admin CLI against the test store, with a real token-mode server and a
// real client authenticating with a token from `token create`.
func TestTokensAndAdminCLI(t *testing.T) {
	const pepper = "e2e-pepper-e2e-pepper-e2e-pepper-1234"
	snapshot := filepath.Join(t.TempDir(), "store.json")
	env := append(baseEnv(), "SNOWSTATE_ALLOW_TEST_STORE=1", "SNOWSTATE_TEST_STORE_FILE="+snapshot,
		"SNOWSTATE_TOKEN_PEPPER="+pepper)
	cli := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(binary, append(args, "--store=memory")...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s\n%s", strings.Join(args, " "), out)
		return string(out)
	}

	out := cli("token", "create", "--principal", "ci", "--scope", "platform/*:read,write,lock", "--expires", "1d", "--description", "e2e")
	token := regexp.MustCompile(`sst_[A-Za-z0-9_-]{43}`).FindString(out)
	require.NotEmpty(t, token, out)
	id := regexp.MustCompile(`id:\s+(\S+)`).FindStringSubmatch(out)[1]

	listed := cli("token", "list")
	assert.Contains(t, listed, "platform/*:read,write,lock")
	assert.NotContains(t, listed, token, "the token is printed once")

	cmd := exec.Command(binary, "serve", "--store=memory", "--listen=127.0.0.1:0")
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	url := "http://" + waitForAddr(t, stdout)

	c := clients[len(clients)-1]
	u := url + "/v1/states/platform/app/prod"
	w := newWorkdir(t, c, fmt.Sprintf(`terraform {
  backend "http" {
    address = %q
    lock_address = %q
    unlock_address = %q
    username = "ci"
  }
}
resource "terraform_data" "d" { input = "x" }
`, u, u, u))

	denied, err := w.run("init")
	require.Error(t, err, "no password yet\n%s", denied)
	assert.Contains(t, denied, "requires auth")

	w.env = append(w.env, "TF_HTTP_PASSWORD="+token)
	w.ok("init")
	w.ok("apply", "-auto-approve")

	other := newWorkdir(t, c, fmt.Sprintf(`terraform {
  backend "http" {
    address = %q
    username = "ci"
  }
}
`, url+"/v1/states/other/app/prod"))
	other.env = append(other.env, "TF_HTTP_PASSWORD="+token)
	out2, err := other.run("init")
	require.Error(t, err, "scope denies another project\n%s", out2)
	assert.Contains(t, out2, "invalid auth")

	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	require.NoError(t, cmd.Wait(), "graceful exit")

	key := "platform/app/prod"
	assert.Contains(t, cli("state", "list"), "platform/app")
	versions := cli("state", "versions", key)
	first := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f-]{27}`).FindString(versions)
	require.NotEmpty(t, first, versions)

	pulled := filepath.Join(t.TempDir(), "state.json")
	cli("state", "pull", key, "-o", pulled, "--version", first)
	raw, err := os.ReadFile(pulled)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "terraform_data")
	assert.Contains(t, cli("state", "pull", key), "terraform_data")

	assert.Contains(t, cli("state", "restore", key, "--version", first), "restored as version")
	assert.Contains(t, cli("lock", "list"), "KEY")
	assert.Contains(t, cli("lock", "release", key), "no lock held")
	assert.Contains(t, cli("gc", "--dry-run"), "would remove")
	assert.Contains(t, cli("gc"), "removed")

	assert.Contains(t, cli("token", "revoke", id), "revoked")
	events, err := memory.ReadSnapshotAudit(snapshot)
	require.NoError(t, err)
	assert.Contains(t, auditActions(events), store.ActionTokenCreate)
	assert.Contains(t, auditActions(events), store.ActionTokenRevoke)
}

func TestLockReleaseAsksFirst(t *testing.T) {
	snapshot := filepath.Join(t.TempDir(), "store.json")
	env := append(baseEnv(), "SNOWSTATE_ALLOW_TEST_STORE=1", "SNOWSTATE_TEST_STORE_FILE="+snapshot)
	key := "platform/stuck/prod"

	s := startServerWithSnapshot(t, snapshot)
	req, err := http.NewRequest("LOCK", s+"/v1/states/"+key, strings.NewReader(`{"ID":"L1","Who":"bob","Operation":"apply"}`))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	run := func(stdin string, args ...string) (string, error) {
		cmd := exec.Command(binary, append([]string{"lock", "release", key, "--store=memory"}, args...)...)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run("")
	require.Error(t, err, "no answer means no")
	assert.Contains(t, out, "bob")
	assert.Contains(t, out, "--force")

	out, err = run("n\n")
	require.Error(t, err, out)

	out, err = run("y\n")
	require.NoError(t, err, out)
	assert.Contains(t, out, "released lock L1")
}

func TestStandardModeIsGated(t *testing.T) {
	base := append(baseEnv(), "SNOWSTATE_ALLOW_TEST_STORE=1", "SNOWSTATE_LOCKING__MODE=standard")

	cmd := exec.Command(binary, "serve", "--store=memory", "--listen=127.0.0.1:0", "--auth-mode=none")
	cmd.Env = base
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "refused without the acknowledgement")
	assert.Contains(t, string(out), "standard_mode_acknowledged")

	cmd = exec.Command(binary, "exec", "--store=memory", "--key=a/b", "--", "true")
	cmd.Env = base
	out, err = cmd.CombinedOutput()
	require.Error(t, err, "exec is gated too")
	assert.Contains(t, string(out), "standard_mode_acknowledged")

	cmd = exec.Command(binary, "exec", "--store=memory", "--key=a/b", "--", "true")
	cmd.Env = append(base, "SNOWSTATE_LOCKING__STANDARD_MODE_ACKNOWLEDGED=true")
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}
