package snowflake

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql migrations/control/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations(dir string) ([]migration, error) {
	entries, err := fs.Glob(migrationFS, dir+"/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	out := make([]migration, 0, len(entries))
	for _, path := range entries {
		base := path[strings.LastIndexByte(path, '/')+1:]
		num, _, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("migration %s has no number", base)
		}
		version, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", base, err)
		}
		raw, err := migrationFS.ReadFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: base, sql: string(raw)})
	}
	return out, nil
}

// statements splits a migration on semicolons at the end of a line. Migrations
// contain no procedural blocks, so this is sufficient.
func statements(script string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		if strings.HasSuffix(strings.TrimRight(line, " \t"), ";") {
			if stmt := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(cur.String()), ";")); stmt != "" {
				out = append(out, stmt)
			}
			cur.Reset()
		}
	}
	if stmt := strings.TrimSpace(cur.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
}

func (s *Store) render(script string) string {
	kind, nullability := "HYBRID TABLE", "NOT NULL"
	if s.opts.LockMode == LockStandard {
		kind, nullability = "TABLE", ""
	}
	return strings.NewReplacer("{{LOCKS_KIND}}", kind, "{{LOCK_ID_NULL}}", nullability).Replace(script)
}

// MigrateResult lists what ran, or would run in a dry run.
type MigrateResult struct {
	Applied []string
	// HybridChecked and HybridAvailable report the §6.5 detection. It needs a
	// schema to work in, so a dry run on an empty account does not do it.
	HybridChecked   bool
	HybridAvailable bool
}

// ErrNoHybridTables explains what to do when hybrid tables are unavailable.
var ErrNoHybridTables = errors.New("hybrid tables are not available in this account or region. " +
	"Locking needs them for correctness. Check the cloud and region (they are GA on AWS and Azure, preview on GCP), " +
	"or accept the risk of the experimental fallback by setting locking.mode=standard and " +
	"locking.standard_mode_acknowledged=true")

// Migrate creates and upgrades every schema. It is idempotent and forward-only.
func (s *Store) Migrate(ctx context.Context, dryRun bool) (*MigrateResult, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	res := &MigrateResult{}
	control := s.controlSchema()
	if !dryRun {
		if err := s.ensureSchema(ctx, control); err != nil {
			return nil, err
		}
		if err := s.ensureLedger(ctx); err != nil {
			return nil, err
		}
		if err := s.checkHybrid(ctx, res); err != nil {
			return nil, err
		}
	}
	if err := s.applyDir(ctx, "migrations/control", control, "control", dryRun, res); err != nil {
		return nil, err
	}
	if s.opts.Tenancy == TenancySchemaPerProject {
		schemas, err := s.dataSchemas(ctx)
		if err != nil {
			if dryRun {
				return res, nil // CONTROL may not exist yet
			}
			return nil, err
		}
		for _, schema := range schemas {
			if err := s.applyDir(ctx, "migrations", schema, schema, dryRun, res); err != nil {
				return nil, err
			}
		}
		return res, nil
	}
	if err := s.applyDir(ctx, "migrations", s.opts.Schema, s.opts.Schema, dryRun, res); err != nil {
		return nil, err
	}
	return res, nil
}

// checkHybrid probes for hybrid tables on every run, including with standard
// locking, so an operator on the fallback learns when they could leave it.
func (s *Store) checkHybrid(ctx context.Context, res *MigrateResult) error {
	ok, err := s.HybridSupported(ctx)
	if err != nil {
		return err
	}
	res.HybridChecked, res.HybridAvailable = true, ok
	if !ok && s.opts.LockMode == LockHybrid {
		return ErrNoHybridTables
	}
	return nil
}

// MigrateSchema creates one project schema and upgrades it.
func (s *Store) MigrateSchema(ctx context.Context, schema string) error {
	if err := validIdent(schema); err != nil {
		return err
	}
	if err := s.ensureSchema(ctx, schema); err != nil {
		return err
	}
	return s.applyDir(ctx, "migrations", schema, schema, false, &MigrateResult{})
}

// ensureSchema creates a schema only when it is missing, because a bootstrapped
// role need not hold CREATE SCHEMA.
func (s *Store) ensureSchema(ctx context.Context, schema string) error {
	rows, err := s.query(ctx, s.db, "schema_check",
		"SELECT COUNT(*) FROM "+s.opts.Database+".INFORMATION_SCHEMA.SCHEMATA WHERE SCHEMA_NAME = ?", schema)
	if err != nil {
		return classify(err)
	}
	defer func() { _ = rows.Close() }()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return classify(err)
		}
	}
	if err := rows.Err(); err != nil {
		return classify(err)
	}
	if n > 0 {
		return nil
	}
	_, err = s.exec(ctx, s.db, "schema_create", "CREATE SCHEMA IF NOT EXISTS "+s.opts.Database+"."+schema)
	return classify(err)
}

func (s *Store) ensureLedger(ctx context.Context) error {
	_, err := s.exec(ctx, s.db, "migrate", "CREATE TABLE IF NOT EXISTS "+s.controlTable("SCHEMA_MIGRATIONS")+
		` (SCOPE VARCHAR NOT NULL, VERSION NUMBER NOT NULL, NAME VARCHAR,
		APPLIED_AT TIMESTAMP_TZ DEFAULT CURRENT_TIMESTAMP(), PRIMARY KEY (SCOPE, VERSION))`)
	return classify(err)
}

func (s *Store) applied(ctx context.Context, scope string) (map[int]bool, error) {
	rows, err := s.query(ctx, s.db, "migrate",
		"SELECT VERSION FROM "+s.controlTable("SCHEMA_MIGRATIONS")+" WHERE SCOPE = ?", scope)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func (s *Store) applyDir(ctx context.Context, dir, schema, scope string, dryRun bool, res *MigrateResult) error {
	list, err := loadMigrations(dir)
	if err != nil {
		return err
	}
	done, err := s.applied(ctx, scope)
	if err != nil {
		if !dryRun {
			return classify(err)
		}
		done = map[int]bool{} // no ledger yet: everything is pending
	}
	for _, m := range list {
		if done[m.version] {
			continue
		}
		res.Applied = append(res.Applied, scope+"/"+m.name)
		if dryRun {
			continue
		}
		for _, stmt := range statements(s.render(m.sql)) {
			if err := s.execInSchema(ctx, schema, stmt); err != nil {
				return fmt.Errorf("migration %s in %s: %w", m.name, schema, err)
			}
		}
		if _, err := s.exec(ctx, s.db, "migrate",
			"INSERT INTO "+s.controlTable("SCHEMA_MIGRATIONS")+" (SCOPE, VERSION, NAME) VALUES (?, ?, ?)",
			scope, m.version, m.name); err != nil {
			return classify(err)
		}
	}
	return nil
}

// execInSchema runs DDL on one connection with the schema selected, because
// migrations use unqualified names.
func (s *Store) execInSchema(ctx context.Context, schema, stmt string) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return classify(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "USE SCHEMA "+s.opts.Database+"."+schema); err != nil {
		return classify(err)
	}
	_, err = conn.ExecContext(ctx, stmt)
	return classify(err)
}

// SchemaVersion reports the applied and latest data migration for the default
// scope, for `doctor` and the provider's "run snowstate migrate" check.
func (s *Store) SchemaVersion(ctx context.Context) (applied, latest int, err error) {
	list, err := loadMigrations("migrations")
	if err != nil {
		return 0, 0, err
	}
	latest = list[len(list)-1].version
	scopes, err := s.dataSchemas(ctx)
	if err != nil {
		return 0, 0, err
	}
	applied = latest
	for _, scope := range scopes {
		done, err := s.applied(ctx, scope)
		if err != nil {
			return 0, latest, classify(err)
		}
		high := 0
		for v := range done {
			high = max(high, v)
		}
		applied = min(applied, high)
	}
	return applied, latest, nil
}

// Session describes the current Snowflake session.
type Session struct {
	User, Role, Warehouse, Database, Schema string
	// SecondaryRoles is CURRENT_SECONDARY_ROLES(), a JSON object.
	SecondaryRoles string
}

func (s Session) String() string {
	return fmt.Sprintf("user=%s role=%s", s.User, s.Role)
}

// Session reports who and where the connection is.
func (s *Store) Session(ctx context.Context) (Session, error) {
	var out Session
	rows, err := s.query(ctx, s.db, "session",
		`SELECT COALESCE(CURRENT_USER(), ''), COALESCE(CURRENT_ROLE(), ''), COALESCE(CURRENT_WAREHOUSE(), ''),
		COALESCE(CURRENT_DATABASE(), ''), COALESCE(CURRENT_SCHEMA(), ''), COALESCE(CURRENT_SECONDARY_ROLES(), '')`)
	if err != nil {
		return out, classify(err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		if err := rows.Scan(&out.User, &out.Role, &out.Warehouse, &out.Database, &out.Schema, &out.SecondaryRoles); err != nil {
			return out, classify(err)
		}
	}
	return out, classify(rows.Err())
}

func anyRow(rows *sql.Rows) (bool, error) {
	defer func() { _ = rows.Close() }()
	found := rows.Next()
	return found, rows.Err()
}

// LocksAreHybrid reports whether LOCKS is a hybrid table.
func (s *Store) LocksAreHybrid(ctx context.Context) (bool, error) {
	schemas, err := s.dataSchemas(ctx)
	if err != nil {
		return false, err
	}
	for _, schema := range schemas {
		rows, err := s.query(ctx, s.db, "hybrid_check",
			"SHOW HYBRID TABLES LIKE 'LOCKS' IN SCHEMA "+s.opts.Database+"."+schema)
		if err != nil {
			return false, classify(err)
		}
		found, err := anyRow(rows)
		if err != nil || !found {
			return false, classify(err)
		}
	}
	return true, nil
}

// HybridSupported tries to create a temporary hybrid table (SPEC §6.5).
func (s *Store) HybridSupported(ctx context.Context) (bool, error) {
	schema := s.controlSchema()
	name := "SNOWSTATE_HYBRID_PROBE"
	err := s.execInSchema(ctx, schema, "CREATE OR REPLACE HYBRID TABLE "+name+" (ID VARCHAR(16) PRIMARY KEY)")
	if err != nil {
		if errorNumber(err) == codeInsufficientPrivs {
			return false, err
		}
		return false, nil //nolint:nilerr // the failure is the answer to the question
	}
	return true, s.execInSchema(ctx, schema, "DROP TABLE IF EXISTS "+name)
}
