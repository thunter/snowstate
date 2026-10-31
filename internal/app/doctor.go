package app

import (
	"context"
	"fmt"
	"io"

	"github.com/thunter/snowstate/internal/config"
	"github.com/thunter/snowstate/pkg/snowstate/ids"
	"github.com/thunter/snowstate/pkg/snowstate/store"
	"github.com/thunter/snowstate/pkg/snowstate/store/snowflake"
)

// Doctor runs the checks in SPEC §8.2, printing pass or fail for each. It
// returns false when any check failed.
func Doctor(ctx context.Context, cfg *config.Config, out io.Writer) bool {
	ok := true
	check := func(name string, err error, detail string) bool {
		if err != nil {
			ok = false
			fmt.Fprintf(out, "FAIL  %-22s %v\n", name, err)
			return false
		}
		fmt.Fprintf(out, "PASS  %-22s %s\n", name, detail)
		return true
	}
	check("config", nil, "parsed")

	st, err := OpenSnowflake(ctx, cfg, nil)
	if !check("connection config", err, "") {
		return false
	}
	defer func() { _ = st.Close() }()

	pingErr := st.Ping(ctx)
	if !check("snowflake auth", pingErr, "connected") {
		printConnectivityHint(pingErr, out)
		return false
	}

	session, err := st.Session(ctx)
	if check("session", err, session.String()) {
		check("role", required(session.Role, "role"), session.Role)
		check("warehouse", required(session.Warehouse, "warehouse"), session.Warehouse)
		check("database", required(session.Database, "database"), session.Database)
		check("schema", required(session.Schema, "schema"), session.Schema)
	}

	applied, latest, err := st.SchemaVersion(ctx)
	if err == nil && applied < latest {
		err = fmt.Errorf("schema is at %d, latest is %d: run snowstate migrate", applied, latest)
	}
	check("migrations", err, fmt.Sprintf("version %d", latest))

	if cfg.Locking.Mode == snowflake.LockHybrid {
		hybrid, err := st.LocksAreHybrid(ctx)
		if err == nil && !hybrid {
			err = fmt.Errorf("LOCKS is not a hybrid table")
		}
		check("hybrid tables", err, "LOCKS is hybrid")
	}

	check("privileges", roundTrip(ctx, st), "lock row written and deleted")
	return ok
}

func required(value, name string) error {
	if value == "" {
		return fmt.Errorf("no current %s: check grants and the configured name", name)
	}
	return nil
}

// roundTrip writes and deletes a row under a scratch key. The key is outside
// the key convention on purpose, so it can never collide with a real state.
func roundTrip(ctx context.Context, st *snowflake.Store) error {
	key := "__doctor__/" + ids.V4()
	return st.InTx(ctx, "", func(tx store.Tx) error {
		lock := &store.Lock{Key: key, LockID: ids.V4(), Source: store.SourceCLI, Operation: "doctor"}
		if _, err := tx.InsertLock(ctx, lock); err != nil {
			return err
		}
		_, err := tx.DeleteLock(ctx, key, lock.LockID)
		return err
	})
}

// printConnectivityHint explains the network-policy options (SPEC §8.4.2).
func printConnectivityHint(err error, out io.Writer) {
	if !snowflake.IsNetworkPolicyRejection(err) {
		return
	}
	fmt.Fprint(out, `
A Snowflake network policy rejected this IP. GitHub-hosted runners have no fixed
IPs, so choose one of these (docs/github-actions.md has the details):
  1. Allow GitHub's published Actions IP ranges (large, and they change).
  2. Use larger runners with static IPs (GitHub Enterprise Cloud).
  3. Use self-hosted runners inside the corporate network.
`)
}
