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
	// Terraform runs the binary in dir with local state.
	Terraform(ctx context.Context, dir string, args ...string) error
	Outputs(ctx context.Context, dir string) (Outputs, error)
	Migrate(ctx context.Context, cfg *config.Config) error
	Doctor(ctx context.Context, cfg *config.Config) error
	// Sidecar runs the binary in dir through snowstate exec, with state under key.
	Sidecar(ctx context.Context, cfg *config.Config, key, dir string, args ...string) error
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

// Run takes an Install from nothing to Foundation state held in snowstate.
func Run(ctx context.Context, opts Options, r Runner) error {
	org, account, ok := strings.Cut(opts.Config.Snowflake.Account, "-")
	if !ok || org == "" || account == "" {
		return &StepError{Step: "check settings", Err: fmt.Errorf("snowflake.account %q is not <org>-<account>", opts.Config.Snowflake.Account),
			Next: "set SNOWSTATE_SNOWFLAKE__ACCOUNT to your account identifier"}
	}
	settings := func(err error, next string) error { return &StepError{Step: "check settings", Err: err, Next: next} }
	if opts.Config.Snowflake.Auth != "keypair" {
		return settings(fmt.Errorf("install needs key-pair auth, not %q, because the Foundation logs in with the Seed key", opts.Config.Snowflake.Auth),
			"set SNOWSTATE_SNOWFLAKE__AUTH=keypair")
	}
	if env := opts.Config.Snowflake.PrivateKeyPassphraseEnv; env != "" && os.Getenv(env) != "" {
		return settings(errors.New("the Seed key is encrypted, and the Foundation can't pass a passphrase to Terraform"),
			"use an unencrypted key for SNOWSTATE_SEED, kept readable only by you")
	}
	if opts.Config.Snowflake.PrivateKeyPath == "" {
		return settings(errors.New("no private key path for the Seed user"), "set SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH to the SNOWSTATE_SEED key")
	}
	keyPath, err := filepath.Abs(opts.Config.Snowflake.PrivateKeyPath)
	if err != nil {
		return settings(err, "set SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH to an absolute path")
	}
	base := *opts.Config
	// Terraform runs in the Foundation directory, so a relative path would
	// point somewhere else for it.
	base.Snowflake.PrivateKeyPath = keyPath
	base.Snowflake.User = seedUser
	if _, err := os.Stat(filepath.Join(opts.Dir, backend)); err == nil {
		return &StepError{Step: "check " + opts.Dir, Err: errors.New("this Foundation's state is already in snowstate"),
			Next: "to change it, run snowstate exec -- terraform apply in " + opts.Dir + "; re-running install is not supported yet"}
	}
	if _, err := r.LookPath(opts.Binary); err != nil {
		return &StepError{Step: "find " + opts.Binary, Err: err, Next: "install Terraform or OpenTofu, or pass --binary"}
	}

	say := func(step string) { fmt.Fprintf(opts.Stdout, "==> %s\n", step) }
	say("write the Foundation to " + opts.Dir)
	if err := writeFoundation(opts.Dir); err != nil {
		return &StepError{Step: "write the Foundation", Err: err, Next: "use an empty directory, or pass --dir"}
	}
	vars := fmt.Sprintf("organization_name = %q\naccount_name      = %q\nprivate_key_path  = %q\ndatabase_name     = %q\n",
		org, account, keyPath, opts.Database)
	if err := writeIfAbsent(filepath.Join(opts.Dir, varsFile), []byte(vars)); err != nil {
		return &StepError{Step: "write " + varsFile, Err: err, Next: rerun}
	}

	say("terraform init")
	if err := r.Terraform(ctx, opts.Dir, "init", "-input=false"); err != nil {
		return &StepError{Step: "terraform init", Err: err, Next: rerun}
	}
	say("terraform plan")
	if err := r.Terraform(ctx, opts.Dir, "plan", "-input=false", "-out="+planFile); err != nil {
		return &StepError{Step: "terraform plan", Err: err, Next: rerun}
	}
	if !opts.AutoApprove && !approved(opts) {
		_ = os.Remove(filepath.Join(opts.Dir, planFile))
		return &StepError{Step: "approve the plan", Err: errors.New("not approved, so nothing was applied"),
			Next: "run snowstate install again and answer yes, or pass --auto-approve"}
	}
	say("terraform apply")
	err = r.Terraform(ctx, opts.Dir, "apply", "-input=false", planFile)
	_ = os.Remove(filepath.Join(opts.Dir, planFile))
	if err != nil {
		return &StepError{Step: "terraform apply", Err: err, Next: rerun}
	}

	out, err := r.Outputs(ctx, opts.Dir)
	if err != nil {
		return &StepError{Step: "read the Foundation outputs", Err: err, Next: rerun}
	}
	say("migrate as " + out.AdminRole)
	if err := r.Migrate(ctx, stepConfig(&base, out, out.AdminRole)); err != nil {
		return &StepError{Step: "migrate", Err: err, Next: "check that " + out.AdminRole + " can create tables in the schema, then run snowstate install again"}
	}
	runtime := stepConfig(&base, out, out.RuntimeRole)
	say("doctor as " + out.RuntimeRole)
	if err := r.Doctor(ctx, runtime); err != nil {
		return &StepError{Step: "doctor", Err: err, Next: "the state has not moved; fix the failed checks, then run snowstate install again"}
	}

	say("move the state into snowstate")
	if err := os.Rename(filepath.Join(opts.Dir, backendOff), filepath.Join(opts.Dir, backend)); err != nil {
		return &StepError{Step: "enable the backend", Err: err, Next: rerun}
	}
	if err := r.Sidecar(ctx, runtime, StateKey, opts.Dir, "init", "-input=false", "-migrate-state", "-force-copy"); err != nil {
		// Back to the starting point, so running install again is safe.
		_ = os.Rename(filepath.Join(opts.Dir, backend), filepath.Join(opts.Dir, backendOff))
		return &StepError{Step: "move the state", Err: err, Next: "the state is still local in " + opts.Dir + "; " + rerun}
	}
	if err := r.Sidecar(ctx, runtime, StateKey, opts.Dir, "plan", "-input=false", "-detailed-exitcode"); err != nil {
		return &StepError{Step: "check the moved state", Err: err,
			Next: "the state is in snowstate and the local copy is kept in " + opts.Dir + "; compare them with snowstate state pull " + StateKey + " before deleting the local files"}
	}
	for _, name := range []string{localState, localBackup} {
		if err := os.Remove(filepath.Join(opts.Dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return &StepError{Step: "remove the local state", Err: err, Next: "delete " + name + " by hand"}
		}
	}
	say("done: the Foundation's state is in snowstate under " + StateKey)
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
		have, err := os.ReadFile(path) //nolint:gosec // inside the operator's chosen directory
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, name)
		case err != nil:
			return err
		case !bytes.Equal(have, want):
			return fmt.Errorf("%s differs from the embedded Foundation", path)
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
