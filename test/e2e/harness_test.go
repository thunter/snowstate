// Package e2e drives real Terraform and OpenTofu binaries against snowstate.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/pkg/snowstate/store"
	"github.com/thunter/snowstate/pkg/snowstate/store/memory"
)

var (
	repoRoot string
	binary   string
	cacheDir string
)

type client struct {
	name string
	bin  string
}

var clients []client

func TestMain(m *testing.M) {
	code, err := setup(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func setup(m *testing.M) (int, error) {
	wd, err := os.Getwd()
	if err != nil {
		return 0, err
	}
	repoRoot = filepath.Dir(filepath.Dir(wd))
	tmp, err := os.MkdirTemp("", "snowstate-e2e")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmp)
	binary = filepath.Join(tmp, "snowstate")
	build := exec.Command("go", "build", "-o", binary, "./cmd/snowstate")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("build: %w\n%s", err, out)
	}
	cacheDir = filepath.Join(repoRoot, ".tools", "plugin-cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return 0, err
	}
	for _, c := range []client{
		{"terraform-1.5.7", ".tools/terraform-1.5.7/terraform"},
		{"terraform-1.17.1", ".tools/terraform-1.17.1/terraform"},
		{"tofu-1.13.2", ".tools/tofu-1.13.2/tofu"},
	} {
		c.bin = filepath.Join(repoRoot, c.bin)
		if _, err := os.Stat(c.bin); err == nil {
			clients = append(clients, c)
		}
	}
	if len(clients) == 0 {
		return 0, fmt.Errorf("no Terraform binaries: run `make tools`")
	}
	return m.Run(), nil
}

// forEachClient runs a scenario once per client binary.
func forEachClient(t *testing.T, fn func(t *testing.T, c client)) {
	t.Helper()
	for _, c := range clients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fn(t, c)
		})
	}
}

// server is a running `snowstate serve` with the memory store.
type server struct {
	url      string
	snapshot string
}

func startServer(t *testing.T, extraEnv ...string) *server {
	t.Helper()
	snapshot := filepath.Join(t.TempDir(), "store.json")
	return &server{url: startServerAt(t, snapshot, extraEnv...), snapshot: snapshot}
}

// startServerWithSnapshot starts a server on a chosen snapshot file. The test
// must have finished with it before the CLI opens the same file.
func startServerWithSnapshot(t *testing.T, snapshot string) string {
	t.Helper()
	return startServerAt(t, snapshot)
}

func startServerAt(t *testing.T, snapshot string, extraEnv ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, "serve", "--store=memory", "--listen=127.0.0.1:0", "--auth-mode=none")
	cmd.Env = append(baseEnv(), "SNOWSTATE_ALLOW_TEST_STORE=1", "SNOWSTATE_TEST_STORE_FILE="+snapshot)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	return "http://" + waitForAddr(t, stdout)
}

// waitForAddr reads the server's startup log line.
func waitForAddr(t *testing.T, stdout io.Reader) string {
	t.Helper()
	addr := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var line struct {
				Msg  string `json:"msg"`
				Addr string `json:"addr"`
			}
			if json.Unmarshal(scanner.Bytes(), &line) == nil && line.Msg == "snowstate listening" {
				addr <- line.Addr
			}
		}
	}()
	select {
	case a := <-addr:
		return a
	case <-time.After(30 * time.Second):
		t.Fatal("server did not start")
		return ""
	}
}

func (s *server) stateURL(key string) string { return s.url + "/v1/states/" + key }

func (s *server) waitForLock(t *testing.T, key string) *store.Lock {
	t.Helper()
	var l *store.Lock
	require.Eventually(t, func() bool {
		l = s.lockQuiet(key)
		return l != nil
	}, 60*time.Second, 50*time.Millisecond, "lock on %s never appeared", key)
	return l
}

func (s *server) lockQuiet(key string) *store.Lock {
	locks, err := memory.ReadSnapshotLocks(s.snapshot)
	if err != nil {
		return nil
	}
	if l, ok := locks[key]; ok {
		return &l
	}
	return nil
}

func (s *server) audit(t *testing.T) []store.AuditEvent {
	t.Helper()
	events, err := memory.ReadSnapshotAudit(s.snapshot)
	require.NoError(t, err)
	return events
}

func (s *server) rawState(t *testing.T, key string) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.Get(s.stateURL(key))
	require.NoError(t, err)
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, buf.Bytes()
}

func baseEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TF_") || strings.HasPrefix(kv, "SNOWSTATE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// workdir is one root module.
type workdir struct {
	t   *testing.T
	c   client
	dir string
	env []string
}

func newWorkdir(t *testing.T, c client, tf string) *workdir {
	t.Helper()
	dir := t.TempDir()
	w := &workdir{t: t, c: c, dir: dir}
	w.write(tf)
	w.env = append(baseEnv(), "TF_IN_AUTOMATION=1", "TF_INPUT=0", "CHECKPOINT_DISABLE=1",
		"TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE=1", "TF_PLUGIN_CACHE_DIR="+cacheDir)
	return w
}

func (w *workdir) write(tf string) {
	w.t.Helper()
	require.NoError(w.t, os.WriteFile(filepath.Join(w.dir, "main.tf"), []byte(tf), 0o600))
}

func (w *workdir) command(args ...string) *exec.Cmd {
	cmd := exec.Command(w.c.bin, args...)
	cmd.Dir = w.dir
	cmd.Env = w.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// run executes a command and returns combined output and error.
func (w *workdir) run(args ...string) (string, error) {
	w.t.Helper()
	out, err := w.command(args...).CombinedOutput()
	return string(out), err
}

// ok runs a command that must succeed.
func (w *workdir) ok(args ...string) string {
	w.t.Helper()
	out, err := w.run(args...)
	require.NoError(w.t, err, "%s %s\n%s", w.c.name, strings.Join(args, " "), out)
	return out
}

func httpBackend(s *server, key string) string {
	u := s.stateURL(key)
	return fmt.Sprintf(`terraform {
  backend "http" {
    address        = %q
    lock_address   = %q
    unlock_address = %q
  }
}
`, u, u, u)
}
