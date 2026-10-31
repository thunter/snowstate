package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/thunter/snowstate/internal/app"
	"github.com/thunter/snowstate/internal/auth"
	"github.com/thunter/snowstate/internal/config"
	"github.com/thunter/snowstate/internal/obs"
	"github.com/thunter/snowstate/pkg/snowstate/ids"
	"github.com/thunter/snowstate/pkg/snowstate/service"
	"github.com/thunter/snowstate/pkg/snowstate/store"
	"github.com/thunter/snowstate/pkg/snowstate/store/snowflake"
)

// withStore loads config and runs fn with a store for provisioning commands
// (migrate, project). Admin commands talk to Snowflake directly, not through
// the HTTP API (SPEC §8).
func withStore(cmd *cobra.Command, fn func(ctx context.Context, cfg *config.Config, st *snowflake.Store) error) error {
	cfg, err := loadConfig(cmd, nil, config.ProfileAdmin)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	st, err := app.OpenForProvisioning(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return fn(ctx, cfg, st)
}

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Create or upgrade the schema",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			return withStore(cmd, func(ctx context.Context, cfg *config.Config, st *snowflake.Store) error {
				res, err := st.Migrate(ctx, dryRun)
				if err != nil {
					return err
				}
				verb := "applied"
				if dryRun {
					verb = "would apply"
				}
				if res.HybridChecked && res.HybridAvailable && cfg.Locking.Mode == snowflake.LockStandard {
					fmt.Fprintln(cmd.OutOrStdout(), "hybrid tables are available: consider locking.mode=hybrid")
				} else if res.HybridChecked {
					fmt.Fprintln(cmd.OutOrStdout(), "hybrid tables: available =", res.HybridAvailable)
				}
				if len(res.Applied) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "schema is up to date")
				}
				for _, m := range res.Applied {
					fmt.Fprintln(cmd.OutOrStdout(), verb, m)
				}
				return nil
			})
		},
	}
	cmd.Flags().Bool("dry-run", false, "list migrations without running them")
	return cmd
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check connectivity, grants, hybrid support and a round trip",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd, nil, config.ProfileAdmin)
			if err != nil {
				return err
			}
			if !app.Doctor(cmd.Context(), cfg, cmd.OutOrStdout()) {
				return errors.New("doctor found problems")
			}
			return nil
		},
	}
}

// adminService runs fn with a direct-mode service: Snowflake grants decide.
func adminService(cmd *cobra.Command, fn func(ctx context.Context, svc *service.Service, p service.Principal, st store.Store) error) error {
	cfg, err := loadConfig(cmd, nil, config.ProfileAdmin)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	log := obs.NewLogger(cmd.ErrOrStderr(), "warn", "text")
	testStore, _ := cmd.Flags().GetString("store")
	st, err := app.OpenStore(ctx, cfg, app.StoreOptions{TestStore: testStore}, log)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	opts := app.ServiceOptions(cfg, st, service.AllowAll{}, log)
	// Operators need a way out of a stuck lock whatever the API policy is.
	opts.AllowForceUnlock = true
	svc := service.New(opts)
	defer svc.Close()
	return fn(ctx, svc, service.Principal{Name: operator(cfg), Source: "snowflake"}, st)
}

// operator names the person at the CLI for audit rows.
func operator(cfg *config.Config) string {
	if cfg.Snowflake.User != "" {
		return cfg.Snowflake.User
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "cli"
}

func newStateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "state", Short: "Inspect and restore states"}

	list := &cobra.Command{
		Use: "list", Short: "List states, showing the prefix/workspace split", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			prefix, _ := cmd.Flags().GetString("prefix")
			return adminService(cmd, func(ctx context.Context, svc *service.Service, p service.Principal, _ store.Store) error {
				states, err := svc.ListStates(ctx, p, prefix)
				if err != nil {
					return err
				}
				w := table(cmd)
				defer func() { _ = w.Flush() }()
				fmt.Fprintln(w, "KEY\tPREFIX\tWORKSPACE\tSERIAL\tSIZE\tUPDATED\tBY")
				for i := range states {
					s := &states[i]
					fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n", s.Key, s.Prefix, s.Workspace, s.Serial,
						s.SizeBytes, s.UpdatedAt.Format(time.RFC3339), s.UpdatedBy)
				}
				return nil
			})
		},
	}
	list.Flags().String("prefix", "", "only keys under this prefix")

	versions := &cobra.Command{
		Use: "versions <key>", Short: "List versions, newest first", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			limit, _ := cmd.Flags().GetInt("limit")
			return adminService(cmd, func(ctx context.Context, svc *service.Service, p service.Principal, _ store.Store) error {
				vs, err := svc.ListVersions(ctx, p, args[0], limit)
				if err != nil {
					return err
				}
				w := table(cmd)
				defer func() { _ = w.Flush() }()
				fmt.Fprintln(w, "VERSION\tSERIAL\tSIZE\tSTORAGE\tCREATED\tBY\tRESTORED_FROM")
				for i := range vs {
					v := &vs[i]
					fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\t%s\n", v.ID, v.Serial, v.SizeBytes, v.Storage,
						v.CreatedAt.Format(time.RFC3339), v.CreatedBy, v.RestoredFrom)
				}
				return nil
			})
		},
	}
	versions.Flags().Int("limit", 0, "maximum versions to show (0 for all)")

	pull := &cobra.Command{
		Use: "pull <key>", Short: "Download a state", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version, _ := cmd.Flags().GetString("version")
			output, _ := cmd.Flags().GetString("output")
			return adminService(cmd, func(ctx context.Context, svc *service.Service, p service.Principal, _ store.Store) error {
				var blob *service.StateBlob
				var err error
				if version != "" {
					blob, err = svc.GetVersion(ctx, p, args[0], version)
				} else {
					blob, err = svc.Get(ctx, p, args[0])
				}
				if err != nil {
					return err
				}
				if blob == nil {
					return fmt.Errorf("no state at %s", args[0])
				}
				if output == "" {
					_, err = cmd.OutOrStdout().Write(blob.Body)
					return err
				}
				return os.WriteFile(output, blob.Body, 0o600) // state contains secrets
			})
		},
	}
	pull.Flags().String("version", "", "version ID (default current)")
	pull.Flags().StringP("output", "o", "", "write to a file instead of stdout")

	restore := &cobra.Command{
		Use: "restore <key>", Short: "Make a new version from an old one", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version, _ := cmd.Flags().GetString("version")
			force, _ := cmd.Flags().GetBool("force")
			return adminService(cmd, func(ctx context.Context, svc *service.Service, p service.Principal, _ store.Store) error {
				v, err := svc.Restore(ctx, p, args[0], version, force)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "restored as version", v.ID)
				return nil
			})
		},
	}
	restore.Flags().String("version", "", "version ID to restore")
	restore.Flags().Bool("force", false, "restore even while a lock is held")
	_ = restore.MarkFlagRequired("version")

	for _, c := range []*cobra.Command{list, versions, pull, restore} {
		addTestStoreFlag(c)
	}
	cmd.AddCommand(list, versions, pull, restore)
	return cmd
}

func newLockCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "lock", Short: "Inspect and release locks"}

	list := &cobra.Command{
		Use: "list", Short: "List held locks", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return adminService(cmd, func(ctx context.Context, svc *service.Service, p service.Principal, _ store.Store) error {
				locks, err := svc.ListLocks(ctx, p)
				if err != nil {
					return err
				}
				w := table(cmd)
				defer func() { _ = w.Flush() }()
				fmt.Fprintln(w, "KEY\tID\tOPERATION\tWHO\tSOURCE\tACQUIRED\tBY")
				for i := range locks {
					l := &locks[i]
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", l.Key, l.LockID, l.Operation, l.Who,
						l.Source, l.AcquiredAt.Format(time.RFC3339), l.AcquiredBy)
				}
				return nil
			})
		},
	}

	release := &cobra.Command{
		Use: "release <key>", Short: "Release a stuck lock", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			return adminService(cmd, func(ctx context.Context, svc *service.Service, p service.Principal, st store.Store) error {
				if !force {
					if err := confirmRelease(ctx, cmd, st, args[0]); err != nil {
						return err
					}
				}
				removed, err := svc.ReleaseLock(ctx, p, args[0])
				if err != nil {
					return err
				}
				if removed == nil {
					fmt.Fprintln(cmd.OutOrStdout(), "no lock held on", args[0])
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "released lock %s on %s\n", removed.LockID, args[0])
				return nil
			})
		},
	}
	release.Flags().Bool("force", false, "do not ask for confirmation")

	for _, c := range []*cobra.Command{list, release} {
		addTestStoreFlag(c)
	}
	cmd.AddCommand(list, release)
	return cmd
}

// confirmRelease shows the holder and asks. Without a terminal to ask on, it
// refuses, so a script has to say --force on purpose.
func confirmRelease(ctx context.Context, cmd *cobra.Command, st store.Store, key string) error {
	lock, err := st.GetLock(ctx, key)
	if err != nil || lock == nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Lock %s on %s\n  who: %s\n  operation: %s\n  acquired: %s by %s\n",
		lock.LockID, key, lock.Who, lock.Operation, lock.AcquiredAt.Format(time.RFC3339), lock.AcquiredBy)
	fmt.Fprint(cmd.OutOrStdout(), "Release it? Check for a partial apply first. [y/N] ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(line), "y") {
		return nil
	}
	return errors.New("not released: confirm at the prompt, or use --force")
}

func newGCCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "gc", Short: "Apply retention and sweep orphan blobs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			return adminService(cmd, func(ctx context.Context, svc *service.Service, p service.Principal, _ store.Store) error {
				r, err := svc.GC(ctx, p, dryRun)
				if err != nil {
					return err
				}
				prefix := "removed"
				if dryRun {
					prefix = "would remove"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %d versions, %d states, %d orphan blobs\n",
					prefix, r.VersionsRemoved, r.StatesPurged, r.BlobsRemoved)
				return nil
			})
		},
	}
	cmd.Flags().Bool("dry-run", false, "report without deleting")
	addTestStoreFlag(cmd)
	return cmd
}

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage API tokens"}

	create := &cobra.Command{
		Use: "create", Short: "Create a token. It is printed once.", Args: cobra.NoArgs,
		RunE: runTokenCreate,
	}
	create.Flags().String("principal", "", "principal name, used as the HTTP username")
	create.Flags().StringArray("scope", nil, "'<key pattern>:<perm>[,<perm>]', repeatable")
	create.Flags().String("expires", "", "lifetime such as 90d or 12h")
	create.Flags().String("description", "", "free text")
	_ = create.MarkFlagRequired("principal")
	_ = create.MarkFlagRequired("scope")

	list := &cobra.Command{
		Use: "list", Short: "List tokens", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return adminService(cmd, func(ctx context.Context, _ *service.Service, _ service.Principal, st store.Store) error {
				tokens, err := st.ListTokens(ctx)
				if err != nil {
					return err
				}
				w := table(cmd)
				defer func() { _ = w.Flush() }()
				fmt.Fprintln(w, "ID\tPRINCIPAL\tSCOPES\tEXPIRES\tREVOKED\tLAST_USED\tDESCRIPTION")
				for i := range tokens {
					t := &tokens[i]
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Principal, scopesString(t.Scopes),
						timeOrDash(t.ExpiresAt), timeOrDash(t.RevokedAt), timeOrDash(t.LastUsedAt), t.Description)
				}
				return nil
			})
		},
	}

	revoke := &cobra.Command{
		Use: "revoke <token-id>", Short: "Revoke a token (effective within 60 s)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return adminService(cmd, func(ctx context.Context, _ *service.Service, p service.Principal, st store.Store) error {
				err := st.RevokeToken(ctx, args[0])
				tokenAudit(ctx, st, p, store.ActionTokenRevoke, args[0], err, cmd)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "revoked", args[0])
				return nil
			})
		},
	}

	for _, c := range []*cobra.Command{create, list, revoke} {
		addTestStoreFlag(c)
	}
	cmd.AddCommand(create, list, revoke)
	return cmd
}

func runTokenCreate(cmd *cobra.Command, _ []string) error {
	principal, _ := cmd.Flags().GetString("principal")
	rawScopes, _ := cmd.Flags().GetStringArray("scope")
	expires, _ := cmd.Flags().GetString("expires")
	description, _ := cmd.Flags().GetString("description")

	scopes := make([]store.Scope, 0, len(rawScopes))
	for _, raw := range rawScopes {
		s, err := auth.ParseScope(raw)
		if err != nil {
			return err
		}
		scopes = append(scopes, s)
	}
	var expiresAt *time.Time
	if expires != "" {
		d, err := parseLifetime(expires)
		if err != nil {
			return err
		}
		at := time.Now().Add(d).UTC()
		expiresAt = &at
	}
	cfg, err := loadConfig(cmd, nil, config.ProfileAdmin)
	if err != nil {
		return err
	}
	pepper := os.Getenv(cfg.Auth.PepperEnv)
	if len(pepper) < 32 {
		return fmt.Errorf("%s must hold the server's 32+ byte pepper", cfg.Auth.PepperEnv)
	}
	token, err := auth.GenerateToken()
	if err != nil {
		return err
	}
	return adminService(cmd, func(ctx context.Context, _ *service.Service, p service.Principal, st store.Store) error {
		rec := &store.Token{
			ID: ids.V4(), Principal: principal, HMAC: auth.HMAC([]byte(pepper), token), Scopes: scopes,
			Description: description, CreatedBy: p.Name, ExpiresAt: expiresAt,
		}
		err := st.CreateToken(ctx, rec)
		tokenAudit(ctx, st, p, store.ActionTokenCreate, rec.ID, err, cmd)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "id:    %s\ntoken: %s\nStore it now: it will not be shown again.\n", rec.ID, token)
		return nil
	})
}

func tokenAudit(ctx context.Context, st store.Store, p service.Principal, action, id string, opErr error, cmd *cobra.Command) {
	result := store.ResultOK
	if opErr != nil {
		result = store.ResultError
	}
	err := st.AppendAudit(ctx, []store.AuditEvent{{
		ID: ids.V4(), Time: time.Now().UTC(), Principal: p.Name, Action: action, Result: result,
		Detail: map[string]any{"token_id": id},
	}})
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: audit row not written:", err)
	}
}

// parseLifetime extends time.ParseDuration with days.
func parseLifetime(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("bad lifetime %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad lifetime %q", s)
	}
	return d, nil
}

func scopesString(scopes []store.Scope) string {
	parts := make([]string, 0, len(scopes))
	for _, s := range scopes {
		parts = append(parts, s.KeyPattern+":"+strings.Join(s.Permissions, ","))
	}
	return strings.Join(parts, " ")
}

func timeOrDash(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func table(cmd *cobra.Command) *tabwriter.Writer {
	return tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
}

func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Manage schema_per_project tenancy"}

	create := &cobra.Command{
		Use: "create <name>", Short: "Create a project schema and grant a role", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			role, _ := cmd.Flags().GetString("role")
			planRole, _ := cmd.Flags().GetString("plan-role")
			perms, _ := cmd.Flags().GetStringSlice("permissions")
			return withStore(cmd, func(ctx context.Context, cfg *config.Config, st *snowflake.Store) error {
				p, err := st.CreateProject(ctx, snowflake.ProjectRequest{
					Name: args[0], Role: role, PlanRole: planRole, Permissions: perms, CreatedBy: operator(cfg),
				})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "project %s uses schema %s\n", p.Name, p.Schema)
				return nil
			})
		},
	}
	create.Flags().String("role", "", "Snowflake role for this project")
	create.Flags().String("plan-role", "", "optional second role with read,lock for plan jobs")
	create.Flags().StringSlice("permissions", []string{"read", "write", "lock"}, "permissions for --role")
	_ = create.MarkFlagRequired("role")

	list := &cobra.Command{
		Use: "list", Short: "List projects", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(cmd, func(ctx context.Context, _ *config.Config, st *snowflake.Store) error {
				projects, err := st.ListProjects(ctx)
				if err != nil {
					return err
				}
				w := table(cmd)
				defer func() { _ = w.Flush() }()
				fmt.Fprintln(w, "PROJECT\tSCHEMA\tROLE\tPLAN_ROLE\tPERMISSIONS")
				for i := range projects {
					p := &projects[i]
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.Schema, p.Role, p.PlanRole, p.Permissions)
				}
				return nil
			})
		},
	}
	cmd.AddCommand(create, list)
	return cmd
}
