package install

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/thunter/snowstate/deploy"
	"github.com/thunter/snowstate/internal/config"
)

// StateKey is where the Foundation's own state lives once it has moved.
const StateKey = "snowstate/foundation/default"

const (
	seedUser    = "SNOWSTATE_SEED"
	planFile    = "foundation.tfplan"
	varsFile    = "terraform.tfvars"
	backend     = "backend.tf"
	backendOff  = "backend.tf.disabled"
	localState  = "terraform.tfstate"
	localBackup = "terraform.tfstate.backup"
	envFile     = "snowstate.env"
)

// Outputs are the Foundation's names, read back after apply so overrides are
// honoured.
type Outputs struct {
	Database, Schema, Warehouse, AdminRole, RuntimeRole string
}

// Runner is everything outside the engine: the Terraform binary, Snowflake,
// and the sidecar. snowstate orchestrates the real Terraform and never
// reimplements it (D58).
type Runner interface {
	LookPath(name string) (string, error)
	// Terraform runs the binary in dir with local state, and returns its exit
	// code. An error means it could not run at all.
	Terraform(ctx context.Context, dir string, args ...string) (int, error)
	// Outputs reads the Foundation's outputs, from local state when cfg is
	// nil and through snowstate otherwise.
	Outputs(ctx context.Context, cfg *config.Config, dir string) (Outputs, error)
	Migrate(ctx context.Context, cfg *config.Config) error
	Doctor(ctx context.Context, cfg *config.Config) error
	// Sidecar runs the binary in dir through snowstate exec, with state under
	// key, and returns its exit code.
	Sidecar(ctx context.Context, cfg *config.Config, key, dir string, args ...string) (int, error)
}

// Options configures one install.
type Options struct {
	// Config holds the connection settings: account, auth and key. Role,
	// warehouse, database and schema are set per step.
	Config      *config.Config
	Database    string
	Dir         string
	Binary      string
	AutoApprove bool
	Stdin       io.Reader
	Stdout      io.Writer
}

// StepError says which step failed and what to do next.
type StepError struct {
	Step, Next string
	Err        error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("%s failed: %v\nnext: %s", e.Step, e.Err, e.Next)
}

func (e *StepError) Unwrap() error { return e.Err }

const rerun = "fix the problem above, then run snowstate install again"

// exitErr turns a non-zero exit into an error.
func exitErr(code int, err error) error {
	if err == nil && code != 0 {
		err = fmt.Errorf("exited with %d", code)
	}
	return err
}

// Run takes an Install from nothing to Foundation state held in snowstate.
// Running it again is safe at any point: before the move it finishes the
// first run, and after the move it plans and applies through snowstate.
func Run(ctx context.Context, opts Options, r Runner) error {
	base, vars, err := checkSettings(opts)
	if err != nil {
		return err
	}
	moved := exists(filepath.Join(opts.Dir, backend))
	if moved && hasLocalState(opts.Dir) {
		return &StepError{Step: "check " + opts.Dir, Err: errors.New("there is local state and an enabled backend, so the move may not have finished"),
			Next: "if `snowstate state pull " + StateKey + "` finds nothing, run mv backend.tf backend.tf.disabled in " + opts.Dir +
				" and run snowstate install again; otherwise compare the two before deleting terraform.tfstate"}
	}
	if _, err := r.LookPath(opts.Binary); err != nil {
		return &StepError{Step: "find " + opts.Binary, Err: err, Next: "install Terraform or OpenTofu, or pass --binary"}
	}

	say := func(step string) { fmt.Fprintf(opts.Stdout, "==> %s\n", step) }
	say("write the Foundation to " + opts.Dir)
	if err := writeFoundation(opts.Dir); err != nil {
		return &StepError{Step: "write the Foundation", Err: err, Next: "use an empty directory, or pass --dir"}
	}
	if err := writeIfAbsent(filepath.Join(opts.Dir, varsFile), []byte(vars)); err != nil {
		return &StepError{Step: "write " + varsFile, Err: err, Next: rerun}
	}
	if moved {
		return change(ctx, opts, base, r, say)
	}
	return firstRun(ctx, opts, base, r, say)
}

// checkSettings checks the connection settings before anything runs, and
// returns the Seed user's connection and the Foundation's variables.
func checkSettings(opts Options) (*config.Config, string, error) {
	settings := func(err error, next string) error { return &StepError{Step: "check settings", Err: err, Next: next} }
	org, account, found := strings.Cut(opts.Config.Snowflake.Account, "-")
	if !found || org == "" || account == "" {
		return nil, "", settings(fmt.Errorf("snowflake.account %q is not <org>-<account>", opts.Config.Snowflake.Account),
			"set SNOWSTATE_SNOWFLAKE__ACCOUNT to your account identifier")
	}
	if opts.Config.Snowflake.Auth != "keypair" {
		return nil, "", settings(fmt.Errorf("install needs key-pair auth, not %q, because the Foundation logs in with the Seed key", opts.Config.Snowflake.Auth),
			"set SNOWSTATE_SNOWFLAKE__AUTH=keypair")
	}
	if env := opts.Config.Snowflake.PrivateKeyPassphraseEnv; env != "" && os.Getenv(env) != "" {
		return nil, "", settings(errors.New("the Seed key is encrypted, and the Foundation can't pass a passphrase to Terraform"),
			"use an unencrypted key for SNOWSTATE_SEED, kept readable only by you")
	}
	if opts.Config.Snowflake.PrivateKeyPath == "" {
		return nil, "", settings(errors.New("no private key path for the Seed user"), "set SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH to the SNOWSTATE_SEED key")
	}
	keyPath, err := filepath.Abs(opts.Config.Snowflake.PrivateKeyPath)
	if err != nil {
		return nil, "", settings(err, "set SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH to an absolute path")
	}
	base := *opts.Config
	// Terraform runs in the Foundation directory, so a relative path would
	// point somewhere else for it.
	base.Snowflake.PrivateKeyPath = keyPath
	base.Snowflake.User = seedUser
	vars := fmt.Sprintf("organization_name = %q\naccount_name      = %q\nprivate_key_path  = %q\ndatabase_name     = %q\n",
		org, account, keyPath, opts.Database)
	return &base, vars, nil
}

// firstRun works with local state until doctor passes, then moves it. A plan
// with no changes means an earlier run already applied, so resuming repeats
// no changes.
func firstRun(ctx context.Context, opts Options, base *config.Config, r Runner, say func(string)) error {
	say("terraform init")
	if err := exitErr(r.Terraform(ctx, opts.Dir, "init", "-input=false")); err != nil {
		return &StepError{Step: "terraform init", Err: err, Next: rerun}
	}
	if err := planAndApply(opts, say, func(args ...string) (int, error) { return r.Terraform(ctx, opts.Dir, args...) }); err != nil {
		return err
	}
	out, err := r.Outputs(ctx, nil, opts.Dir)
	if err != nil {
		return &StepError{Step: "read the Foundation outputs", Err: err, Next: rerun}
	}
	if err := writeEnv(opts.Dir, base, out); err != nil {
		return &StepError{Step: "write " + envFile, Err: err, Next: rerun}
	}
	runtime, err := migrateAndCheck(ctx, base, out, r, say, false)
	if err != nil {
		return err
	}

	say("move the state into snowstate")
	if err := os.Rename(filepath.Join(opts.Dir, backendOff), filepath.Join(opts.Dir, backend)); err != nil {
		return &StepError{Step: "enable the backend", Err: err, Next: rerun}
	}
	if err := exitErr(r.Sidecar(ctx, runtime, StateKey, opts.Dir, "init", "-input=false", "-migrate-state", "-force-copy")); err != nil {
		// Back to the starting point, so running install again is safe.
		_ = os.Rename(filepath.Join(opts.Dir, backend), filepath.Join(opts.Dir, backendOff))
		return &StepError{Step: "move the state", Err: err, Next: "the state is still local in " + opts.Dir + "; " + rerun}
	}
	if code, err := r.Sidecar(ctx, runtime, StateKey, opts.Dir, "plan", "-input=false", "-detailed-exitcode"); err != nil || code != 0 {
		if err == nil {
			err = errors.New("the plan through snowstate shows changes, or failed")
		}
		return &StepError{Step: "check the moved state", Err: err,
			Next: "the state is in snowstate and the local copy is kept in " + opts.Dir + "; compare them with snowstate state pull " + StateKey + ", then run snowstate install again"}
	}
	return finish(opts, say)
}

// change plans and applies through snowstate, for a later Foundation change
// or a re-run after the move. It also brings the schema up to date.
func change(ctx context.Context, opts Options, base *config.Config, r Runner, say func(string)) error {
	saved := readEnv(opts.Dir, opts.Database)
	runtime := stepConfig(base, saved, saved.RuntimeRole)
	say("terraform init through snowstate")
	if err := exitErr(r.Sidecar(ctx, runtime, StateKey, opts.Dir, "init", "-input=false")); err != nil {
		return &StepError{Step: "terraform init", Err: err, Next: rerun}
	}
	terraform := func(args ...string) (int, error) { return r.Sidecar(ctx, runtime, StateKey, opts.Dir, args...) }
	if exists(filepath.Join(opts.Dir, localBackup)) {
		// An earlier run moved the state but couldn't check it. The backup
		// goes only once a plan through snowstate shows no changes (D76).
		say("check the moved state")
		if code, err := terraform("plan", "-input=false", "-detailed-exitcode"); err != nil || code != 0 {
			if err == nil {
				err = errors.New("the state in snowstate differs from the local copy, or the plan failed")
			}
			return &StepError{Step: "check the moved state", Err: err,
				Next: "compare " + localBackup + " in " + opts.Dir + " with snowstate state pull " + StateKey + ", and delete the backup once they match"}
		}
	}
	if err := planAndApply(opts, say, terraform); err != nil {
		return err
	}
	out, err := r.Outputs(ctx, runtime, opts.Dir)
	if err != nil {
		return &StepError{Step: "read the Foundation outputs", Err: err, Next: rerun}
	}
	if err := writeEnv(opts.Dir, base, out); err != nil {
		return &StepError{Step: "write " + envFile, Err: err, Next: rerun}
	}
	if _, err := migrateAndCheck(ctx, base, out, r, say, true); err != nil {
		return err
	}
	return finish(opts, say)
}

// migrateAndCheck runs migrate as the admin role and doctor as the runtime
// role, and returns the runtime role's connection.
func migrateAndCheck(ctx context.Context, base *config.Config, out Outputs, r Runner, say func(string), moved bool) (*config.Config, error) {
	say("migrate as " + out.AdminRole)
	if err := r.Migrate(ctx, stepConfig(base, out, out.AdminRole)); err != nil {
		return nil, &StepError{Step: "migrate", Err: err, Next: "check that " + out.AdminRole + " can create tables in the schema, then run snowstate install again"}
	}
	runtime := stepConfig(base, out, out.RuntimeRole)
	say("doctor as " + out.RuntimeRole)
	if err := r.Doctor(ctx, runtime); err != nil {
		next := "the state has not moved; fix the failed checks, then run snowstate install again"
		if moved {
			next = "the state is in snowstate; fix the failed checks with the runbook, then run snowstate install again"
		}
		return nil, &StepError{Step: "doctor", Err: err, Next: next}
	}
	return runtime, nil
}

// finish removes the local state files, once the state is known to be in
// snowstate.
func finish(opts Options, say func(string)) error {
	for _, name := range []string{localState, localBackup} {
		if err := os.Remove(filepath.Join(opts.Dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return &StepError{Step: "remove the local state", Err: err, Next: "delete " + name + " by hand"}
		}
	}
	say("done: the Foundation's state is in snowstate under " + StateKey)
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// hasLocalState is true when terraform.tfstate holds state. A move leaves it
// empty, with the state in the backup.
func hasLocalState(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, localState))
	return err == nil && info.Size() > 0
}

// writeEnv saves the settings for snowstate commands, so a re-run, or an
// operator during an outage, needs no terraform output. It holds names and a
// key path only.
func writeEnv(dir string, base *config.Config, out Outputs) error {
	s := base.Snowflake
	var text strings.Builder
	fmt.Fprintf(&text, "# Written by snowstate install, which rewrites it each run. Load with: source %s\n", envFile)
	for _, kv := range [][2]string{
		{"SNOWSTATE_SNOWFLAKE__ACCOUNT", s.Account}, {"SNOWSTATE_SNOWFLAKE__USER", s.User},
		{"SNOWSTATE_SNOWFLAKE__ROLE", out.RuntimeRole}, {"SNOWSTATE_SNOWFLAKE__WAREHOUSE", out.Warehouse},
		{"SNOWSTATE_SNOWFLAKE__DATABASE", out.Database}, {"SNOWSTATE_SNOWFLAKE__SCHEMA", out.Schema},
		{"SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH", s.PrivateKeyPath}, {"SNOWSTATE_KEY", StateKey},
	} {
		fmt.Fprintf(&text, "export %s=%s\n", kv[0], shellQuote(kv[1]))
	}
	fmt.Fprintf(&text, "%s%s\n", adminComment, out.AdminRole)
	return os.WriteFile(filepath.Join(dir, envFile), []byte(text.String()), 0o600)
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func shellUnquote(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], `'\''`, "'")
	}
	return s
}

const adminComment = "# The admin role, for migrate: "

// readEnv reads the names writeEnv saved, or derives them from the database
// name when the file is missing.
func readEnv(dir, database string) Outputs {
	out := Outputs{
		Database: database, Schema: "BACKEND", Warehouse: database + "_WH",
		AdminRole: database + "_ADMIN", RuntimeRole: database + "_SVC",
	}
	data, err := os.ReadFile(filepath.Join(dir, envFile)) //nolint:gosec // inside the operator's chosen directory
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		if admin, ok := strings.CutPrefix(line, adminComment); ok {
			out.AdminRole = admin
			continue
		}
		name, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		value = shellUnquote(value)
		switch name {
		case "SNOWSTATE_SNOWFLAKE__ROLE":
			out.RuntimeRole = value
		case "SNOWSTATE_SNOWFLAKE__WAREHOUSE":
			out.Warehouse = value
		case "SNOWSTATE_SNOWFLAKE__DATABASE":
			out.Database = value
		case "SNOWSTATE_SNOWFLAKE__SCHEMA":
			out.Schema = value
		}
	}
	return out
}

// planAndApply plans, and applies only when the plan has changes and the
// operator approves. A plan with no changes means an earlier run finished the
// apply, which is how a re-run avoids repeating it.
func planAndApply(opts Options, say func(string), terraform func(args ...string) (int, error)) error {
	say("terraform plan")
	code, err := terraform("plan", "-input=false", "-detailed-exitcode", "-out="+planFile)
	defer func() { _ = os.Remove(filepath.Join(opts.Dir, planFile)) }()
	switch {
	case err != nil:
		return &StepError{Step: "terraform plan", Err: err, Next: rerun}
	case code == 0:
		say("no changes to apply")
		return nil
	case code != 2:
		return &StepError{Step: "terraform plan", Err: fmt.Errorf("exited with %d", code), Next: rerun}
	}
	if !opts.AutoApprove && !approved(opts) {
		return &StepError{Step: "approve the plan", Err: errors.New("not approved, so nothing was applied"),
			Next: "run snowstate install again and answer yes, or pass --auto-approve"}
	}
	say("terraform apply")
	if err := exitErr(terraform("apply", "-input=false", planFile)); err != nil {
		return &StepError{Step: "terraform apply", Err: err, Next: rerun}
	}
	return nil
}

func approved(opts Options) bool {
	fmt.Fprint(opts.Stdout, "Apply this plan? Only 'yes' will be accepted: ")
	line, _ := bufio.NewReader(opts.Stdin).ReadString('\n')
	return strings.TrimSpace(line) == "yes"
}

// stepConfig is the Seed user's connection with one step's role and the
// Foundation's names. The Seed user has secondary roles off, so the session
// has only this role (D66).
func stepConfig(base *config.Config, out Outputs, role string) *config.Config {
	cfg := *base
	cfg.Snowflake.Role = role
	cfg.Snowflake.Warehouse = out.Warehouse
	cfg.Snowflake.Database = out.Database
	cfg.Snowflake.Schema = out.Schema
	return &cfg
}

// writeFoundation writes the embedded code into dir. Files already there must
// match, and every file is checked before any is written, so the operator's
// code is never overwritten or mixed with new files.
func writeFoundation(dir string) error {
	var missing []string
	err := fs.WalkDir(deploy.Foundation(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, err := fs.ReadFile(deploy.Foundation(), name)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, name)
		if name == backendOff && exists(filepath.Join(dir, backend)) {
			// The enabled backend block counts as its disabled form (D65).
			path = filepath.Join(dir, backend)
		}
		have, err := os.ReadFile(path) //nolint:gosec // inside the operator's chosen directory
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, name)
		case err != nil:
			return err
		case !bytes.Equal(have, want):
			return fmt.Errorf("%s differs from the embedded Foundation (- embedded, + yours):\n%s", path, lineDiff(string(want), string(have)))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, name := range missing {
		data, err := fs.ReadFile(deploy.Foundation(), name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func writeIfAbsent(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // inside the operator's chosen directory
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// lineDiff lists the lines that differ between a and b, marked - for a and
// + for b, from their longest common subsequence. The files are small, so the
// quadratic table is fine.
func lineDiff(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			i, j = i+1, j+1
		case j < len(y) && (i == len(x) || lcs[i][j+1] >= lcs[i+1][j]):
			fmt.Fprintf(&out, "+ %s\n", y[j])
			j++
		default:
			fmt.Fprintf(&out, "- %s\n", x[i])
			i++
		}
	}
	return strings.TrimSuffix(out.String(), "\n")
}
