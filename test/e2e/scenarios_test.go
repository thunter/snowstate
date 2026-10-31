package e2e

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/pkg/snowstate/store"
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
