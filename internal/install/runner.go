package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"

	"github.com/thunter/snowstate/internal/app"
	"github.com/thunter/snowstate/internal/config"
	"github.com/thunter/snowstate/internal/sidecar"
)

// Exec is the real Runner: the Terraform or OpenTofu binary, Snowflake, and
// the sidecar, all writing to the operator's terminal.
type Exec struct {
	Binary         string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Logger         *slog.Logger
}

var _ Runner = (*Exec)(nil)

// LookPath finds the binary.
func (e *Exec) LookPath(name string) (string, error) { return exec.LookPath(name) }

// Terraform runs the binary in dir with local state.
func (e *Exec) Terraform(ctx context.Context, dir string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, e.Binary, append([]string{"-chdir=" + dir}, args...)...) //nolint:gosec // running the operator's Terraform is the feature
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", e.Binary, args[0], err)
	}
	return 0, nil
}

// Outputs reads the Foundation's names, from local state when cfg is nil and
// through snowstate otherwise.
func (e *Exec) Outputs(ctx context.Context, cfg *config.Config, dir string) (Outputs, error) {
	var raw bytes.Buffer
	if cfg == nil {
		cmd := exec.CommandContext(ctx, e.Binary, "-chdir="+dir, "output", "-json") //nolint:gosec // running the operator's Terraform is the feature
		cmd.Stdout, cmd.Stderr = &raw, e.Stderr
		if err := cmd.Run(); err != nil {
			return Outputs{}, fmt.Errorf("%s output: %w", e.Binary, err)
		}
	} else {
		if err := exitErr(e.sidecar(ctx, cfg, StateKey, dir, &raw, "output", "-json")); err != nil {
			return Outputs{}, fmt.Errorf("%s output: %w", e.Binary, err)
		}
	}
	var values map[string]struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal(raw.Bytes(), &values); err != nil {
		return Outputs{}, fmt.Errorf("%s output: %w", e.Binary, err)
	}
	text := func(name string) string { s, _ := values[name].Value.(string); return s }
	out := Outputs{
		Database: text("database"), Schema: text("schema"), Warehouse: text("warehouse"),
		AdminRole: text("admin_role"), RuntimeRole: text("runtime_role"),
	}
	if out.Database == "" || out.Schema == "" || out.Warehouse == "" || out.AdminRole == "" || out.RuntimeRole == "" {
		return Outputs{}, errors.New("the Foundation's outputs are incomplete: was it applied?")
	}
	return out, nil
}

// Migrate runs snowstate migrate in process.
func (e *Exec) Migrate(ctx context.Context, cfg *config.Config) error {
	st, err := app.OpenForProvisioning(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	res, err := st.Migrate(ctx, false)
	if err != nil {
		return err
	}
	if len(res.Applied) == 0 {
		fmt.Fprintln(e.Stdout, "schema is up to date")
	}
	for _, m := range res.Applied {
		fmt.Fprintln(e.Stdout, "applied", m)
	}
	return nil
}

// Doctor runs snowstate doctor in process.
func (e *Exec) Doctor(ctx context.Context, cfg *config.Config) error {
	if !app.Doctor(ctx, cfg, e.Stdout) {
		return errors.New("doctor found problems")
	}
	return nil
}

// Sidecar runs the binary in dir through snowstate exec.
func (e *Exec) Sidecar(ctx context.Context, cfg *config.Config, key, dir string, args ...string) (int, error) {
	return e.sidecar(ctx, cfg, key, dir, e.Stdout, args...)
}

func (e *Exec) sidecar(ctx context.Context, cfg *config.Config, key, dir string, stdout io.Writer, args ...string) (int, error) {
	return sidecar.Run(ctx, sidecar.Options{
		Config: cfg, Key: key, Args: append([]string{e.Binary, "-chdir=" + dir}, args...),
		ShutdownGrace: sidecar.DefaultShutdownGrace, Logger: e.Logger,
		Stdin: e.Stdin, Stdout: stdout, Stderr: e.Stderr,
	})
}
