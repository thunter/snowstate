// Package sidecar implements `snowstate exec` (SPEC §8.1): an ephemeral
// loopback server that lives exactly as long as one Terraform command.
package sidecar

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thunter/snowstate/internal/app"
	"github.com/thunter/snowstate/internal/auth"
	"github.com/thunter/snowstate/internal/ci"
	"github.com/thunter/snowstate/internal/config"
	"github.com/thunter/snowstate/internal/obs"
	"github.com/thunter/snowstate/pkg/snowstate/keys"
	"github.com/thunter/snowstate/pkg/snowstate/service"
	"github.com/thunter/snowstate/pkg/snowstate/store"
)

// KeyEnv names the variable that can supply the state key.
const KeyEnv = "SNOWSTATE_KEY"

// DefaultShutdownGrace is how long the server outlives a second signal.
const DefaultShutdownGrace = 20 * time.Second

// Options configures one run.
type Options struct {
	Config           *config.Config
	Key              string
	Args             []string
	ReleaseStaleLock bool
	ShutdownGrace    time.Duration
	TestStore        string
	Logger           *slog.Logger
	Stdin            io.Reader
	Stdout, Stderr   io.Writer
}

// lockTracker remembers which locks this sidecar's child took and released,
// and what blocked it.
type lockTracker struct {
	mu       sync.Mutex
	held     map[string]bool
	acquired []string
	released []string
	blockers []*service.Lock
}

func (t *lockTracker) locked(l *service.Lock) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.held[l.LockID] = true
	t.acquired = append(t.acquired, l.LockID)
}

func (t *lockTracker) unlocked(l *service.Lock) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.held, l.LockID)
	t.released = append(t.released, l.LockID)
}

func (t *lockTracker) blocked(l *service.Lock) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.blockers = append(t.blockers, l)
}

func (t *lockTracker) holds(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.held[id]
}

// Run executes the child and returns its exit code. An error means the child
// never ran or the sidecar failed.
func Run(ctx context.Context, opts Options) (int, error) {
	if len(opts.Args) == 0 {
		return 0, errors.New("nothing to run: use snowstate exec -- <command> [args]")
	}
	if opts.Key == "" {
		opts.Key = os.Getenv(KeyEnv)
	}
	if opts.Key == "" {
		return 0, fmt.Errorf("no state key: pass --key or set %s", KeyEnv)
	}
	if err := keys.Validate(opts.Key); err != nil {
		return 0, err
	}
	if opts.ShutdownGrace == 0 {
		opts.ShutdownGrace = DefaultShutdownGrace
	}
	log := opts.Logger
	cfg := opts.Config
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Auth.Mode = "loopback-secret"

	st, err := app.OpenStore(ctx, cfg, app.StoreOptions{TestStore: opts.TestStore}, log)
	if err != nil {
		return 0, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Ping(ctx); err != nil {
		return 0, fmt.Errorf("connect: %w", err)
	}

	tracker := &lockTracker{held: map[string]bool{}}
	// Sidecar is direct mode: Snowflake grants decide, not token scopes (§5.3).
	svcOpts := app.ServiceOptions(cfg, st, service.AllowAll{}, log)
	svcOpts.Hooks = service.Hooks{Locked: tracker.locked, Unlocked: tracker.unlocked, Blocked: tracker.blocked}
	identity := cfg.Snowflake.User
	if identity == "" {
		identity = "sidecar"
	}
	run := ci.Detect(os.Getenv)
	if run != nil {
		svcOpts.AuditDetail = run.AuditDetail()
		svcOpts.AcquiredBy = run.AcquiredBy()
	}
	svc := service.New(svcOpts)
	defer svc.Close()

	before := currentState(ctx, st, opts.Key, log)

	secret, err := auth.NewLoopbackSecret()
	if err != nil {
		return 0, err
	}
	secret.Identity = identity
	handler, err := app.Handler(cfg, svc, secret, obs.NewMetrics(svc.AuditDepth), log)
	if err != nil {
		return 0, err
	}
	srv, err := app.Listen(cfg, handler, nil, log)
	if err != nil {
		return 0, err
	}
	srv.Start()

	code, err := runChild(opts, childEnv(srv.Addr().String(), opts.Key, secret.Secret()), log)

	if serr := srv.Shutdown(context.Background()); serr != nil {
		log.Warn("sidecar shutdown", "error", serr)
	}
	stale := reportStaleLock(context.Background(), svc, st, opts, tracker, log)
	if run != nil {
		after := currentState(context.Background(), st, opts.Key, log)
		if serr := run.WriteSummary(summary(opts, run, before, after, tracker, stale)); serr != nil {
			log.Warn("could not write the step summary", "error", serr)
		}
	}
	return code, err
}

func currentState(ctx context.Context, st store.Store, key string, log *slog.Logger) *store.StateMeta {
	m, err := st.GetState(ctx, key)
	if err != nil {
		log.Warn("could not read the state for the summary", "error", err)
		return nil
	}
	if m.Deleted() {
		return nil
	}
	return m
}

func stateLine(m *store.StateMeta) string {
	if m == nil {
		return "none"
	}
	return fmt.Sprintf("serial %d, version `%s`", m.Serial, m.CurrentVersionID)
}

// summary is the Markdown for $GITHUB_STEP_SUMMARY. State contents never
// appear here: only the key, serials, IDs and who held a lock.
func summary(opts Options, run ci.Context, before, after *store.StateMeta, t *lockTracker, stale *store.Lock) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "### snowstate: `%s`\n\n", opts.Key)
	fmt.Fprintf(&b, "- Command: `%s`\n", commandName(opts.Args))
	fmt.Fprintf(&b, "- Before: %s\n- After: %s\n", stateLine(before), stateLine(after))
	for _, id := range t.acquired {
		fmt.Fprintf(&b, "- Lock acquired: `%s`\n", id)
	}
	for _, id := range t.released {
		fmt.Fprintf(&b, "- Lock released: `%s`\n", id)
	}
	for _, l := range t.blockers {
		fmt.Fprintf(&b, "- **Lock conflict** with `%s`, held by %s", l.LockID, l.Who)
		if url := run.RunURLFor(l.AcquiredBy); url != "" {
			fmt.Fprintf(&b, " in [this run](%s)", url)
		}
		b.WriteString("\n")
	}
	if stale != nil {
		fmt.Fprintf(&b, "- **Lock still held**: `%s`. Check for a partial apply, then run `snowstate lock release %s`.\n",
			stale.LockID, opts.Key)
	}
	return b.String()
}

// commandName shows the program and subcommand, never arguments, which can
// carry variable values.
func commandName(args []string) string {
	name := filepath.Base(args[0])
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		name += " " + args[1]
	}
	return name
}

// childEnv passes the backend settings only through the environment. Nothing
// is written to .terraform/ or the backend block, which is what keeps saved
// plans portable across sidecars (§8.1 step 7).
func childEnv(addr, key, secret string) []string {
	url := "http://" + addr + "/v1/states/" + key
	return append(os.Environ(),
		"TF_HTTP_ADDRESS="+url,
		"TF_HTTP_LOCK_ADDRESS="+url,
		"TF_HTTP_UNLOCK_ADDRESS="+url,
		"TF_HTTP_USERNAME=sidecar",
		"TF_HTTP_PASSWORD="+secret,
	)
}

// runChild runs the command, forwarding signals until it exits. The sidecar
// keeps serving meanwhile, so Terraform can still write state and unlock.
func runChild(opts Options, env []string, log *slog.Logger) (int, error) {
	cmd := exec.Command(opts.Args[0], opts.Args[1:]...) //nolint:gosec // running the user's command is the feature
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.Stdin, opts.Stdout, opts.Stderr
	isolateChild(cmd, opts.Stdin)

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, forwardedSignals...)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", opts.Args[0], err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var grace <-chan time.Time
	received := 0
	for {
		select {
		case err := <-done:
			return exitCode(err)
		case sig := <-sigs:
			received++
			log.Warn("forwarding signal to child", "signal", sig.String())
			if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
				log.Warn("could not forward signal", "error", err)
			}
			if received == 2 {
				grace = time.After(opts.ShutdownGrace)
			}
		case <-grace:
			log.Warn("child still running after the shutdown grace period, stopping it")
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				log.Warn("could not kill child", "error", err)
			}
			grace = nil
		}
	}
}

func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return codeFromExit(exit), nil
	}
	return 0, err
}

// reportStaleLock handles a lock this sidecar's child left behind (§8.1 step
// 6) and returns it if it is still held. It stays held unless the user opted
// in: an unknown partial apply must not be silently unlocked.
func reportStaleLock(ctx context.Context, svc *service.Service, st store.Store, opts Options, t *lockTracker, log *slog.Logger) *store.Lock {
	lock, err := st.GetLock(ctx, opts.Key)
	if err != nil {
		log.Warn("could not check for a stale lock", "error", err)
		return nil
	}
	if lock == nil || !t.holds(lock.LockID) {
		return nil
	}
	if opts.ReleaseStaleLock {
		principal := service.Principal{Name: lock.AcquiredBy, Source: "snowflake"}
		if _, err := svc.Unlock(ctx, principal, opts.Key, lock.LockID, true); err != nil {
			log.Warn("could not release the stale lock", "error", err)
			return lock
		}
		fmt.Fprintf(opts.Stderr, "snowstate: released stale lock %s on %s\n", lock.LockID, opts.Key)
		return nil
	}
	fmt.Fprintf(opts.Stderr, "snowstate: the command exited while still holding lock %s on %s.\n"+
		"Check for a partial apply, then release it with:\n  snowstate lock release %s\n",
		lock.LockID, opts.Key, opts.Key)
	return lock
}
