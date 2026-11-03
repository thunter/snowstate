package install

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/internal/config"
)

// fakeRunner records every call the engine makes, so tests see the commands
// an operator would see, with the role each one used.
type fakeRunner struct {
	calls []string
	fail  map[string]error // keyed by the start of a call
	path  string
	// remoteChanges makes a plan through snowstate show changes.
	remoteChanges bool
}

func (f *fakeRunner) record(call string) error {
	f.calls = append(f.calls, call)
	for prefix, err := range f.fail {
		if strings.HasPrefix(call, prefix) {
			return err
		}
	}
	return nil
}

// exits records a command and reports whether it should exit non-zero, as a
// failed Terraform run does.
func (f *fakeRunner) exits(call string) bool { return f.record(call) != nil }

func (f *fakeRunner) LookPath(name string) (string, error) {
	if f.path == "" {
		return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
	}
	return f.path, nil
}

func (f *fakeRunner) Terraform(_ context.Context, dir string, args ...string) (int, error) {
	if f.exits("terraform " + strings.Join(args, " ")) {
		return 1, nil
	}
	if args[0] == "apply" {
		return 0, os.WriteFile(filepath.Join(dir, "terraform.tfstate"), []byte("{}"), 0o600)
	}
	if args[0] == "plan" && hasState(dir) {
		return 0, nil
	}
	if args[0] == "plan" {
		return 2, nil // -detailed-exitcode: there are changes
	}
	return 0, nil
}

// hasState is true once an apply has written local state.
func hasState(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "terraform.tfstate"))
	return err == nil && info.Size() > 0
}

func (f *fakeRunner) Outputs(_ context.Context, cfg *config.Config, _ string) (Outputs, error) {
	call := "outputs"
	if cfg != nil {
		call = "outputs through snowstate " + where(cfg)
	}
	return Outputs{
		Database: "TF_STATE", Schema: "BACKEND", Warehouse: "TF_STATE_WH",
		AdminRole: "TF_STATE_ADMIN", RuntimeRole: "TF_STATE_SVC",
	}, f.record(call)
}

func (f *fakeRunner) Migrate(_ context.Context, cfg *config.Config) error {
	return f.record("migrate " + where(cfg))
}

func (f *fakeRunner) Doctor(_ context.Context, cfg *config.Config) error {
	return f.record("doctor " + where(cfg))
}

func (f *fakeRunner) Sidecar(_ context.Context, cfg *config.Config, key, dir string, args ...string) (int, error) {
	if f.exits("snowstate exec " + where(cfg) + " key=" + key + " -- terraform " + strings.Join(args, " ")) {
		return 1, nil
	}
	if args[0] == "plan" && f.remoteChanges {
		return 2, nil
	}
	if args[0] == "init" && slices.Contains(args, "-migrate-state") {
		// -migrate-state empties the local file and keeps a backup.
		_ = os.WriteFile(filepath.Join(dir, "terraform.tfstate"), nil, 0o600)
		return 0, os.WriteFile(filepath.Join(dir, "terraform.tfstate.backup"), []byte("{}"), 0o600)
	}
	return 0, nil
}

func where(cfg *config.Config) string {
	s := cfg.Snowflake
	return fmt.Sprintf("user=%s role=%s wh=%s db=%s schema=%s", s.User, s.Role, s.Warehouse, s.Database, s.Schema)
}

func baseConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Snowflake.Account = "myorg-myaccount"
	cfg.Snowflake.Auth = "keypair"
	cfg.Snowflake.PrivateKeyPath = "/keys/seed.p8"
	return &cfg
}

func newOptions(t *testing.T, answer string) (Options, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	return Options{
		Config:   baseConfig(),
		Database: "TF_STATE",
		Dir:      filepath.Join(t.TempDir(), "snowstate-foundation"),
		Binary:   "terraform",
		Stdin:    strings.NewReader(answer),
		Stdout:   &out,
	}, &out
}

func TestFirstInstallRunsEachStepWithItsRole(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	runner := &fakeRunner{path: "/usr/bin/terraform"}

	require.NoError(t, Run(context.Background(), opts, runner))

	admin := "user=SNOWSTATE_SEED role=TF_STATE_ADMIN wh=TF_STATE_WH db=TF_STATE schema=BACKEND"
	svc := "user=SNOWSTATE_SEED role=TF_STATE_SVC wh=TF_STATE_WH db=TF_STATE schema=BACKEND"
	require.Equal(t, []string{
		"terraform init -input=false",
		"terraform plan -input=false -detailed-exitcode -out=foundation.tfplan",
		"terraform apply -input=false foundation.tfplan",
		"outputs",
		"migrate " + admin,
		"doctor " + svc,
		"snowstate exec " + svc + " key=snowstate/foundation/default -- terraform init -input=false -migrate-state -force-copy",
		"snowstate exec " + svc + " key=snowstate/foundation/default -- terraform plan -input=false -detailed-exitcode",
	}, runner.calls)

	require.FileExists(t, filepath.Join(opts.Dir, "main.tf"))
	require.FileExists(t, filepath.Join(opts.Dir, "backend.tf"))
	require.NoFileExists(t, filepath.Join(opts.Dir, "backend.tf.disabled"))
	require.NoFileExists(t, filepath.Join(opts.Dir, "terraform.tfstate"))
	require.NoFileExists(t, filepath.Join(opts.Dir, "terraform.tfstate.backup"))
	require.NoFileExists(t, filepath.Join(opts.Dir, "foundation.tfplan"))

	vars, err := os.ReadFile(filepath.Join(opts.Dir, "terraform.tfvars"))
	require.NoError(t, err)
	require.Equal(t, `organization_name = "myorg"
account_name      = "myaccount"
private_key_path  = "/keys/seed.p8"
database_name     = "TF_STATE"
`, string(vars))
}

func TestNothingIsAppliedWithoutApproval(t *testing.T) {
	opts, out := newOptions(t, "no\n")
	runner := &fakeRunner{path: "/usr/bin/terraform"}

	err := Run(context.Background(), opts, runner)

	require.ErrorContains(t, err, "approve the plan failed: not approved, so nothing was applied")
	require.Contains(t, out.String(), "Apply this plan? Only 'yes' will be accepted:")
	require.Equal(t, []string{"terraform init -input=false", "terraform plan -input=false -detailed-exitcode -out=foundation.tfplan"}, runner.calls)
	require.NoFileExists(t, filepath.Join(opts.Dir, "foundation.tfplan"))
}

func TestAutoApproveSkipsThePrompt(t *testing.T) {
	opts, out := newOptions(t, "")
	opts.AutoApprove = true
	runner := &fakeRunner{path: "/usr/bin/terraform"}

	require.NoError(t, Run(context.Background(), opts, runner))
	require.NotContains(t, out.String(), "Apply this plan?")
	require.Contains(t, runner.calls, "terraform apply -input=false foundation.tfplan")
}

func TestMissingBinaryFailsBeforeAnythingChanges(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	opts.Binary = "tofu"
	runner := &fakeRunner{}

	err := Run(context.Background(), opts, runner)

	require.ErrorContains(t, err, "find tofu failed")
	require.ErrorContains(t, err, "next: install Terraform or OpenTofu, or pass --binary")
	require.Empty(t, runner.calls)
	require.NoDirExists(t, opts.Dir)
}

func TestStateOnlyMovesAfterDoctorPasses(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	runner := &fakeRunner{path: "/usr/bin/terraform", fail: map[string]error{"doctor": fmt.Errorf("doctor found problems")}}

	err := Run(context.Background(), opts, runner)

	require.ErrorContains(t, err, "doctor failed")
	require.ErrorContains(t, err, "the state has not moved")
	for _, call := range runner.calls {
		require.NotContains(t, call, "snowstate exec")
	}
	require.FileExists(t, filepath.Join(opts.Dir, "backend.tf.disabled"))
	require.FileExists(t, filepath.Join(opts.Dir, "terraform.tfstate"))
}

func TestCodeInTheDirectoryIsNeverOverwritten(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	require.NoError(t, os.MkdirAll(opts.Dir, 0o750))
	mine := []byte("# my own change\n")
	require.NoError(t, os.WriteFile(filepath.Join(opts.Dir, "main.tf"), mine, 0o600))
	runner := &fakeRunner{path: "/usr/bin/terraform"}

	err := Run(context.Background(), opts, runner)

	require.ErrorContains(t, err, "main.tf differs from the embedded Foundation")
	require.Empty(t, runner.calls)
	have, readErr := os.ReadFile(filepath.Join(opts.Dir, "main.tf"))
	require.NoError(t, readErr)
	require.Equal(t, mine, have)
}

func TestAFailedStepSaysWhichStepAndWhatNext(t *testing.T) {
	tests := []struct{ fail, step, next string }{
		{"terraform init", "terraform init failed", "fix the problem above, then run snowstate install again"},
		{"terraform plan", "terraform plan failed", "fix the problem above, then run snowstate install again"},
		{"terraform apply", "terraform apply failed", "fix the problem above, then run snowstate install again"},
		{"outputs", "read the Foundation outputs failed", "fix the problem above, then run snowstate install again"},
		{"migrate", "migrate failed", "check that TF_STATE_ADMIN can create tables"},
		{"snowstate exec user=SNOWSTATE_SEED role=TF_STATE_SVC wh=TF_STATE_WH db=TF_STATE schema=BACKEND key=snowstate/foundation/default -- terraform init",
			"move the state failed", "the state is still local"},
		{"snowstate exec user=SNOWSTATE_SEED role=TF_STATE_SVC wh=TF_STATE_WH db=TF_STATE schema=BACKEND key=snowstate/foundation/default -- terraform plan",
			"check the moved state failed", "the state is in snowstate and the local copy is kept"},
	}
	for _, tc := range tests {
		t.Run(tc.step, func(t *testing.T) {
			opts, _ := newOptions(t, "yes\n")
			runner := &fakeRunner{path: "/usr/bin/terraform", fail: map[string]error{tc.fail: fmt.Errorf("boom")}}

			err := Run(context.Background(), opts, runner)

			require.ErrorContains(t, err, tc.step+": ")
			require.ErrorContains(t, err, "next: "+tc.next)
		})
	}
}

func TestSettingsAreCheckedBeforeAnythingRuns(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(*config.Config)
		wants string
	}{
		{"account without org", func(c *config.Config) { c.Snowflake.Account = "justanaccount" }, "is not <org>-<account>"},
		{"no key path", func(c *config.Config) { c.Snowflake.PrivateKeyPath = "" }, "no private key path"},
		{"not key-pair auth", func(c *config.Config) { c.Snowflake.Auth = "externalbrowser" }, "needs key-pair auth"},
		{"encrypted key", func(c *config.Config) { c.Snowflake.PrivateKeyPassphraseEnv = "TEST_SEED_PASSPHRASE" }, "encrypted"},
	}
	t.Setenv("TEST_SEED_PASSPHRASE", "secret")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts, _ := newOptions(t, "yes\n")
			tc.edit(opts.Config)
			runner := &fakeRunner{path: "/usr/bin/terraform"}

			require.ErrorContains(t, Run(context.Background(), opts, runner), tc.wants)
			require.Empty(t, runner.calls)
			require.NoDirExists(t, opts.Dir)
		})
	}
}

func TestEmbeddedFoundationMatchesItsSource(t *testing.T) {
	src := "../../deploy/terraform/foundation"
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, writeFoundation(dir))
	for _, e := range entries {
		want, err := os.ReadFile(filepath.Join(src, e.Name()))
		require.NoError(t, err)
		have, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err, "%s is not embedded", e.Name())
		require.Equal(t, string(want), string(have), e.Name())
	}
	written, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, written, len(entries))
}

func TestStepsAlwaysRunAsTheSeedUser(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	opts.Config.Snowflake.User = "SOMEONE_ELSE"
	runner := &fakeRunner{path: "/usr/bin/terraform"}

	require.NoError(t, Run(context.Background(), opts, runner))
	for _, call := range runner.calls {
		require.NotContains(t, call, "SOMEONE_ELSE")
	}
	require.Contains(t, runner.calls, "doctor user=SNOWSTATE_SEED role=TF_STATE_SVC wh=TF_STATE_WH db=TF_STATE schema=BACKEND")
}

func TestARelativeKeyPathIsMadeAbsolute(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	work := t.TempDir()
	t.Chdir(work)
	opts.Config.Snowflake.PrivateKeyPath = "keys/seed.p8"
	runner := &fakeRunner{path: "/usr/bin/terraform"}

	require.NoError(t, Run(context.Background(), opts, runner))
	vars, err := os.ReadFile(filepath.Join(opts.Dir, "terraform.tfvars"))
	require.NoError(t, err)
	require.Contains(t, string(vars), `private_key_path  = "`+filepath.Join(work, "keys", "seed.p8")+`"`)
}

const svcExec = "snowstate exec user=SNOWSTATE_SEED role=TF_STATE_SVC wh=TF_STATE_WH db=TF_STATE schema=BACKEND key=snowstate/foundation/default -- terraform "

func installed(t *testing.T) Options {
	t.Helper()
	opts, _ := newOptions(t, "yes\n")
	require.NoError(t, Run(context.Background(), opts, &fakeRunner{path: "/usr/bin/terraform"}))
	return opts
}

func TestARerunOnAFinishedInstallGoesThroughSnowstate(t *testing.T) {
	opts := installed(t)
	var out bytes.Buffer
	opts.Stdout, opts.Stdin = &out, strings.NewReader("")
	runner := &fakeRunner{path: "/usr/bin/terraform"}

	require.NoError(t, Run(context.Background(), opts, runner))

	require.Equal(t, []string{
		svcExec + "init -input=false",
		svcExec + "plan -input=false -detailed-exitcode -out=foundation.tfplan",
		"outputs through snowstate user=SNOWSTATE_SEED role=TF_STATE_SVC wh=TF_STATE_WH db=TF_STATE schema=BACKEND",
		"migrate user=SNOWSTATE_SEED role=TF_STATE_ADMIN wh=TF_STATE_WH db=TF_STATE schema=BACKEND",
		"doctor user=SNOWSTATE_SEED role=TF_STATE_SVC wh=TF_STATE_WH db=TF_STATE schema=BACKEND",
	}, runner.calls)
	require.Contains(t, out.String(), "no changes to apply")
	require.NotContains(t, out.String(), "Apply this plan?")
}

func TestALaterChangeIsAppliedThroughSnowstateAfterApproval(t *testing.T) {
	opts := installed(t)
	opts.Stdin = strings.NewReader("yes\n")
	runner := &fakeRunner{path: "/usr/bin/terraform", remoteChanges: true}

	require.NoError(t, Run(context.Background(), opts, runner))
	require.Contains(t, runner.calls, svcExec+"apply -input=false foundation.tfplan")
	for _, call := range runner.calls {
		require.False(t, strings.HasPrefix(call, "terraform "), "nothing runs with local state: %s", call)
	}

	opts.Stdin = strings.NewReader("no\n")
	declined := &fakeRunner{path: "/usr/bin/terraform", remoteChanges: true}
	require.ErrorContains(t, Run(context.Background(), opts, declined), "not approved")
	require.NotContains(t, declined.calls, svcExec+"apply -input=false foundation.tfplan")
}

func TestARerunAfterTheMoveCheckFailedTidiesUp(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	first := &fakeRunner{path: "/usr/bin/terraform", fail: map[string]error{svcExec + "plan": fmt.Errorf("boom")}}
	require.ErrorContains(t, Run(context.Background(), opts, first), "check the moved state failed")
	require.FileExists(t, filepath.Join(opts.Dir, "terraform.tfstate.backup"))

	require.NoError(t, Run(context.Background(), opts, &fakeRunner{path: "/usr/bin/terraform"}))
	require.NoFileExists(t, filepath.Join(opts.Dir, "terraform.tfstate"))
	require.NoFileExists(t, filepath.Join(opts.Dir, "terraform.tfstate.backup"))
}

func TestAnInterruptedMoveStopsRatherThanGuess(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	first := &fakeRunner{path: "/usr/bin/terraform", fail: map[string]error{svcExec + "init": fmt.Errorf("boom")}}
	require.Error(t, Run(context.Background(), opts, first))
	// As if the process died after enabling the backend, before the move.
	require.NoError(t, os.Rename(filepath.Join(opts.Dir, "backend.tf.disabled"), filepath.Join(opts.Dir, "backend.tf")))
	runner := &fakeRunner{path: "/usr/bin/terraform"}

	err := Run(context.Background(), opts, runner)

	require.ErrorContains(t, err, "local state and an enabled backend")
	require.ErrorContains(t, err, "mv backend.tf backend.tf.disabled")
	require.Empty(t, runner.calls)
}

func TestARerunWithoutSavedNamesUsesTheDerivedOnes(t *testing.T) {
	opts := installed(t)
	require.NoError(t, os.Remove(filepath.Join(opts.Dir, "snowstate.env")))
	runner := &fakeRunner{path: "/usr/bin/terraform"}

	require.NoError(t, Run(context.Background(), opts, runner))
	require.Equal(t, svcExec+"init -input=false", runner.calls[0])
}

func TestAFailedMoveLeavesTheBackendDisabled(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	runner := &fakeRunner{path: "/usr/bin/terraform", fail: map[string]error{
		"snowstate exec user=SNOWSTATE_SEED role=TF_STATE_SVC wh=TF_STATE_WH db=TF_STATE schema=BACKEND key=snowstate/foundation/default -- terraform init": fmt.Errorf("boom"),
	}}

	require.ErrorContains(t, Run(context.Background(), opts, runner), "move the state failed")
	require.FileExists(t, filepath.Join(opts.Dir, "backend.tf.disabled"))
	require.NoFileExists(t, filepath.Join(opts.Dir, "backend.tf"))
}

func TestNothingIsWrittenWhenAFileDiffers(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	require.NoError(t, os.MkdirAll(opts.Dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(opts.Dir, "versions.tf"), []byte("# mine\n"), 0o600))

	require.ErrorContains(t, Run(context.Background(), opts, &fakeRunner{path: "/usr/bin/terraform"}), "versions.tf differs")
	entries, err := os.ReadDir(opts.Dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the operator's file is there")
}

func TestARerunAfterAFailureFinishesWithoutApplyingAgain(t *testing.T) {
	svc := svcExec
	tests := []struct{ failed string }{
		{"terraform init"}, {"terraform plan"}, {"terraform apply"}, {"outputs"}, {"migrate"}, {"doctor"}, {svc + "init"},
	}
	for _, tc := range tests {
		t.Run(tc.failed, func(t *testing.T) {
			opts, _ := newOptions(t, "yes\n")
			first := &fakeRunner{path: "/usr/bin/terraform", fail: map[string]error{tc.failed: fmt.Errorf("boom")}}
			require.Error(t, Run(context.Background(), opts, first))

			opts.Stdin = strings.NewReader("yes\n")
			second := &fakeRunner{path: "/usr/bin/terraform"}
			require.NoError(t, Run(context.Background(), opts, second))

			applied := slices.Contains([]string{"terraform init", "terraform plan", "terraform apply"}, tc.failed)
			require.Equal(t, applied, slices.Contains(second.calls, "terraform apply -input=false foundation.tfplan"),
				"apply on the second run only if the first never applied")
			require.Equal(t, svc+"plan -input=false -detailed-exitcode", second.calls[len(second.calls)-1])
			require.NoFileExists(t, filepath.Join(opts.Dir, "terraform.tfstate"))
			require.FileExists(t, filepath.Join(opts.Dir, "backend.tf"))
		})
	}
}

func TestADifferenceIsShownLineByLine(t *testing.T) {
	opts := installed(t)
	path := filepath.Join(opts.Dir, "versions.tf")
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	edited := strings.Replace(string(src), `version = "~> 2.22"`, `version = "~> 2.30"`, 1)
	require.NotEqual(t, string(src), edited)
	require.NoError(t, os.WriteFile(path, []byte(edited), 0o600))

	err = Run(context.Background(), opts, &fakeRunner{path: "/usr/bin/terraform"})

	require.ErrorContains(t, err, "versions.tf differs from the embedded Foundation")
	require.ErrorContains(t, err, `- `+`      version = "~> 2.22"`)
	require.ErrorContains(t, err, `+ `+`      version = "~> 2.30"`)
	have, _ := os.ReadFile(path)
	require.Equal(t, edited, string(have))
}

func TestAnEditedBackendBlockCountsAsADifference(t *testing.T) {
	opts := installed(t)
	path := filepath.Join(opts.Dir, "backend.tf")
	require.NoError(t, os.WriteFile(path, []byte("terraform {\n  backend \"s3\" {}\n}\n"), 0o600))

	err := Run(context.Background(), opts, &fakeRunner{path: "/usr/bin/terraform"})

	require.ErrorContains(t, err, "backend.tf differs from the embedded Foundation")
	require.NoFileExists(t, filepath.Join(opts.Dir, "backend.tf.disabled"))
}

func TestGeneratedAndOperatorFilesAreIgnored(t *testing.T) {
	opts := installed(t)
	for name, body := range map[string]string{
		".terraform.lock.hcl": "# generated\n",
		"terraform.tfvars":    "database_name = \"TF_STATE\"\nservice_public_key = \"MII...\"\n",
		"notes.md":            "mine\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(opts.Dir, name), []byte(body), 0o600))
	}

	require.NoError(t, Run(context.Background(), opts, &fakeRunner{path: "/usr/bin/terraform"}))
}

func TestALeftoverBackupIsOnlyRemovedAfterACleanPlan(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	first := &fakeRunner{path: "/usr/bin/terraform", fail: map[string]error{svcExec + "plan": fmt.Errorf("boom")}}
	require.Error(t, Run(context.Background(), opts, first))

	opts.AutoApprove = true
	runner := &fakeRunner{path: "/usr/bin/terraform", remoteChanges: true}
	err := Run(context.Background(), opts, runner)

	require.ErrorContains(t, err, "differs from the local copy")
	require.NotContains(t, runner.calls, svcExec+"apply -input=false foundation.tfplan")
	require.FileExists(t, filepath.Join(opts.Dir, "terraform.tfstate.backup"))
}

func TestDoctorFailingAfterTheMoveGivesAdviceForThatRoute(t *testing.T) {
	opts := installed(t)
	runner := &fakeRunner{path: "/usr/bin/terraform", fail: map[string]error{"doctor": fmt.Errorf("doctor found problems")}}

	err := Run(context.Background(), opts, runner)

	require.ErrorContains(t, err, "doctor failed")
	require.ErrorContains(t, err, "the state is in snowstate")
	require.NotContains(t, err.Error(), "has not moved")
}

func TestSavedSettingsCanBeSourcedWithSpacesInThePath(t *testing.T) {
	opts, _ := newOptions(t, "yes\n")
	opts.Config.Snowflake.PrivateKeyPath = "/keys/my keys/seed.p8"
	require.NoError(t, Run(context.Background(), opts, &fakeRunner{path: "/usr/bin/terraform"}))

	env, err := os.ReadFile(filepath.Join(opts.Dir, "snowstate.env"))
	require.NoError(t, err)
	require.Contains(t, string(env), "export SNOWSTATE_SNOWFLAKE__PRIVATE_KEY_PATH='/keys/my keys/seed.p8'\n")
	require.Contains(t, string(env), "export SNOWSTATE_SNOWFLAKE__ROLE='TF_STATE_SVC'\n")

	// The saved names still drive a re-run.
	runner := &fakeRunner{path: "/usr/bin/terraform"}
	require.NoError(t, Run(context.Background(), opts, runner))
	require.Equal(t, svcExec+"init -input=false", runner.calls[0])
}
