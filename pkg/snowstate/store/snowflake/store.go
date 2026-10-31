// Package snowflake is the Store backed by Snowflake tables and a stage.
package snowflake

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	sf "github.com/snowflakedb/gosnowflake/v2"

	"github.com/thunter/snowstate/pkg/snowstate/keys"
	"github.com/thunter/snowstate/pkg/snowstate/store"
)

// Tenancy modes.
const (
	TenancyShared           = "shared"
	TenancySchemaPerProject = "schema_per_project"
)

// Lock modes.
const (
	LockHybrid   = "hybrid"
	LockStandard = "standard"
)

const (
	maxAttempts       = 3
	retryBackoff      = 200 * time.Millisecond
	contentionBudget  = 30 * time.Second // matches the default LOCK_TIMEOUT
	contentionBackoff = 150 * time.Millisecond
	auditChunk        = 200
)

// Options configures a Store.
type Options struct {
	DB            *sql.DB
	Database      string
	Schema        string
	ControlSchema string
	Tenancy       string
	LockMode      string
	QueryTag      string
	Observer      store.Observer
}

// Store implements store.Store.
type Store struct {
	opts Options
	db   *sql.DB
}

// New returns a Store over an open pool. Store takes ownership of the pool.
func New(opts Options) *Store {
	if opts.Tenancy == "" {
		opts.Tenancy = TenancyShared
	}
	if opts.LockMode == "" {
		opts.LockMode = LockHybrid
	}
	if opts.QueryTag == "" {
		opts.QueryTag = "snowstate"
	}
	if opts.ControlSchema == "" {
		opts.ControlSchema = "CONTROL"
	}
	return &Store{opts: opts, db: opts.DB}
}

// identRE is what an unquoted identifier from configuration must look like.
// Keeping config values to this set is what makes them safe to splice into SQL.
var identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

func validIdent(s string) error {
	if !identRE.MatchString(s) {
		return fmt.Errorf("snowflake: %q is not a plain identifier", s)
	}
	return nil
}

// Validate checks that configured names are safe to use in SQL.
func (s *Store) Validate() error {
	return errors.Join(validIdent(s.opts.Database), validIdent(s.opts.Schema), validIdent(s.opts.ControlSchema))
}

var projectSanitizer = regexp.MustCompile(`[^A-Z0-9]`)

// ProjectSchema returns the schema for a project: P_<PROJECT>, upper-cased with
// non-alphanumerics replaced by '_'.
func ProjectSchema(project string) string {
	return "P_" + projectSanitizer.ReplaceAllString(strings.ToUpper(project), "_")
}

// schemaFor returns the schema holding a state key.
func (s *Store) schemaFor(key string) string {
	if s.opts.Tenancy == TenancySchemaPerProject && key != "" {
		return ProjectSchema(keys.Project(key))
	}
	return s.controlSchema()
}

func (s *Store) controlSchema() string {
	if s.opts.Tenancy == TenancySchemaPerProject {
		return s.opts.ControlSchema
	}
	return s.opts.Schema
}

// table qualifies an object name. Both parts are validated identifiers.
func (s *Store) table(schema, name string) string {
	return s.opts.Database + "." + schema + "." + name
}

func (s *Store) dataTable(key, name string) string { return s.table(s.schemaFor(key), name) }

func (s *Store) controlTable(name string) string { return s.table(s.controlSchema(), name) }

// dataSchemas lists every schema that holds data objects.
func (s *Store) dataSchemas(ctx context.Context) ([]string, error) {
	if s.opts.Tenancy != TenancySchemaPerProject {
		return []string{s.opts.Schema}, nil
	}
	rows, err := s.query(ctx, s.db, "list_projects", "SELECT SCHEMA_NAME FROM "+s.controlTable("PROJECTS")+" ORDER BY 1")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, classify(err)
		}
		if err := validIdent(name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, classify(rows.Err())
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

// tagged sets QUERY_TAG to snowstate:<op>:<request_id> and collects query IDs.
func (s *Store) tagged(ctx context.Context, op string) (context.Context, func()) {
	meta := store.RequestMetaFrom(ctx)
	ctx = sf.WithQueryTag(ctx, s.opts.QueryTag+":"+op+":"+meta.RequestID)
	if meta.QueryIDs == nil {
		return ctx, func() {}
	}
	ch := make(chan string, 4)
	return sf.WithQueryIDChan(ctx, ch), func() {
		select {
		case id := <-ch:
			meta.QueryIDs.Add(id)
		default:
		}
	}
}

func (s *Store) observe(op string, start time.Time, err error) {
	if s.opts.Observer != nil {
		s.opts.Observer.ObserveQuery(op, time.Since(start), err)
	}
}

func (s *Store) exec(ctx context.Context, q querier, op, stmt string, args ...any) (sql.Result, error) {
	ctx, collect := s.tagged(ctx, op)
	defer collect()
	start := time.Now()
	res, err := q.ExecContext(ctx, stmt, args...)
	s.observe(op, start, err)
	return res, err
}

func (s *Store) query(ctx context.Context, q querier, op, stmt string, args ...any) (*sql.Rows, error) {
	ctx, collect := s.tagged(ctx, op)
	defer collect()
	start := time.Now()
	rows, err := q.QueryContext(ctx, stmt, args...) //nolint:sqlclosecheck // the caller closes
	s.observe(op, start, err)
	return rows, err
}

// retry runs an idempotent operation, backing off on transient errors.
func retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := range maxAttempts {
		if attempt > 0 {
			wait := retryBackoff<<attempt + time.Duration(rand.Int64N(int64(retryBackoff))) //nolint:gosec // jitter
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return classify(ctx.Err())
			}
		}
		if err = fn(); !transient(err) {
			break
		}
	}
	return classify(err)
}

// InTx implements store.Store. Row contention on a hybrid table aborts the
// statement rather than waiting, and a row's wait queue is capped, so the whole
// transaction is retried. Each party holds the row for one short transaction.
func (s *Store) InTx(ctx context.Context, _ string, fn func(store.Tx) error) error {
	deadline := time.Now().Add(contentionBudget)
	for {
		err := s.runTx(ctx, fn)
		if !isLockContention(err) {
			return err
		}
		if time.Now().After(deadline) {
			return classify(err)
		}
		wait := contentionBackoff + time.Duration(rand.Int64N(int64(contentionBackoff))) //nolint:gosec // jitter
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return classify(ctx.Err())
		}
	}
}

// runTx runs one transaction. A transient failure before COMMIT retries it.
// Once COMMIT is sent the outcome may be unknown, and it is never retried
// (SPEC §7).
func (s *Store) runTx(ctx context.Context, fn func(store.Tx) error) error {
	var commitErr error
	err := retry(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		t := &txn{s: s, tx: tx}
		if err := fn(t); err != nil {
			if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
				return errors.Join(err, rerr)
			}
			return err
		}
		start := time.Now()
		err = tx.Commit()
		s.observe("commit", start, err)
		commitErr = err
		return nil
	})
	if err != nil {
		return err
	}
	if commitErr != nil {
		return fmt.Errorf("%w: %w", store.ErrCommitUnknown, classify(commitErr))
	}
	return nil
}

func nullStr(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }

const stateColumns = `STATE_KEY, PROJECT, CURRENT_VERSION_ID, SERIAL, LINEAGE, TF_VERSION,
  STATE_MD5, SIZE_BYTES, CREATED_AT, UPDATED_AT, UPDATED_BY, DELETED_AT`

type scanner interface{ Scan(dest ...any) error }

func scanState(r scanner) (*store.StateMeta, error) {
	var m store.StateMeta
	var cur, lineage, tf, md5, by sql.NullString
	var serial, size sql.NullInt64
	var deleted sql.NullTime
	if err := r.Scan(&m.Key, &m.Project, &cur, &serial, &lineage, &tf, &md5, &size,
		&m.CreatedAt, &m.UpdatedAt, &by, &deleted); err != nil {
		return nil, err
	}
	m.CurrentVersionID, m.Lineage, m.TFVersion, m.MD5, m.UpdatedBy = cur.String, lineage.String, tf.String, md5.String, by.String
	m.Serial, m.SizeBytes = serial.Int64, size.Int64
	if deleted.Valid {
		m.DeletedAt = &deleted.Time
	}
	return &m, nil
}

const lockColumns = `STATE_KEY, LOCK_ID, SOURCE, OPERATION, WHO, INFO, TF_VERSION, LOCK_PATH,
  LOCK_CREATED, LOCK_INFO, ACQUIRED_AT, ACQUIRED_BY`

func scanLock(r scanner) (*store.Lock, error) {
	var l store.Lock
	var op, who, info, tf, path, raw, by sql.NullString
	var created sql.NullTime
	if err := r.Scan(&l.Key, &l.LockID, &l.Source, &op, &who, &info, &tf, &path,
		&created, &raw, &l.AcquiredAt, &by); err != nil {
		return nil, err
	}
	l.Operation, l.Who, l.Info, l.TFVersion, l.Path, l.AcquiredBy = op.String, who.String, info.String, tf.String, path.String, by.String
	if created.Valid {
		l.Created = created.Time
	}
	if raw.Valid {
		l.RawJSON = []byte(raw.String)
	}
	return &l, nil
}

func (s *Store) getState(ctx context.Context, q querier, key string) (*store.StateMeta, error) {
	rows, err := s.query(ctx, q, "get_state",
		"SELECT "+stateColumns+" FROM "+s.dataTable(key, "STATES")+" WHERE STATE_KEY = ?", key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanState(rows)
}

// GetState implements store.Store.
func (s *Store) GetState(ctx context.Context, key string) (*store.StateMeta, error) {
	var out *store.StateMeta
	err := retry(ctx, func() error {
		var err error
		out, err = s.getState(ctx, s.db, key)
		return err
	})
	return out, err
}

func (s *Store) getLock(ctx context.Context, q querier, key, suffix string) (*store.Lock, error) {
	rows, err := s.query(ctx, q, "get_lock",
		"SELECT "+lockColumns+" FROM "+s.dataTable(key, "LOCKS")+
			" WHERE STATE_KEY = ? AND LOCK_ID IS NOT NULL"+suffix, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanLock(rows)
}

// GetLock implements store.Store.
func (s *Store) GetLock(ctx context.Context, key string) (*store.Lock, error) {
	var out *store.Lock
	err := retry(ctx, func() error {
		var err error
		out, err = s.getLock(ctx, s.db, key, "")
		return err
	})
	return out, err
}

const versionColumns = `VERSION_ID, STATE_KEY, SERIAL, LINEAGE, TF_VERSION, STATE_MD5, SIZE_BYTES,
  STORAGE, STAGE_PATH, LOCK_ID, CREATED_AT, CREATED_BY, RESTORED_FROM`

func scanVersion(r scanner, extra ...any) (*store.Version, error) {
	var v store.Version
	var tf, stage, lockID, by, restored sql.NullString
	dest := []any{&v.ID, &v.Key, &v.Serial, &v.Lineage, &tf, &v.MD5, &v.SizeBytes,
		&v.Storage, &stage, &lockID, &v.CreatedAt, &by, &restored}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	v.TFVersion, v.StagePath, v.LockID, v.CreatedBy, v.RestoredFrom = tf.String, stage.String, lockID.String, by.String, restored.String
	return &v, nil
}

// GetVersion implements store.Store, including inline content.
func (s *Store) GetVersion(ctx context.Context, key, id string) (*store.Version, error) {
	var out *store.Version
	err := retry(ctx, func() error {
		rows, err := s.query(ctx, s.db, "get_version",
			"SELECT "+versionColumns+", CONTENT_ZLIB FROM "+s.dataTable(key, "STATE_VERSIONS")+
				" WHERE VERSION_ID = ? AND STATE_KEY = ?", id, key)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return err
			}
			return store.ErrNotFound
		}
		var content []byte
		out, err = scanVersion(rows, &content)
		if err != nil {
			return err
		}
		out.Content = content
		return nil
	})
	return out, err
}

// ListVersions implements store.Store, newest first.
func (s *Store) ListVersions(ctx context.Context, key string, limit int) ([]store.Version, error) {
	var out []store.Version
	err := retry(ctx, func() error {
		out = nil
		stmt := "SELECT " + versionColumns + " FROM " + s.dataTable(key, "STATE_VERSIONS") +
			" WHERE STATE_KEY = ? ORDER BY VERSION_ID DESC"
		args := []any{key}
		if limit > 0 {
			stmt += " LIMIT ?"
			args = append(args, limit)
		}
		rows, err := s.query(ctx, s.db, "list_versions", stmt, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			v, err := scanVersion(rows)
			if err != nil {
				return err
			}
			out = append(out, *v)
		}
		return rows.Err()
	})
	return out, err
}

// ListStates implements store.Store, sorted by key.
func (s *Store) ListStates(ctx context.Context, opts store.ListOptions) ([]store.StateMeta, error) {
	schemas, err := s.dataSchemas(ctx)
	if err != nil {
		return nil, err
	}
	var out []store.StateMeta
	for _, schema := range schemas {
		stmt := "SELECT " + stateColumns + " FROM " + s.table(schema, "STATES") + " WHERE 1=1"
		var args []any
		if !opts.IncludeDeleted {
			stmt += " AND DELETED_AT IS NULL"
		}
		if opts.Prefix != "" {
			stmt += " AND (STATE_KEY = ? OR STARTSWITH(STATE_KEY, ?))"
			args = append(args, opts.Prefix, opts.Prefix+"/")
		}
		err := retry(ctx, func() error {
			rows, err := s.query(ctx, s.db, "list_states", stmt+" ORDER BY STATE_KEY", args...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				m, err := scanState(rows)
				if err != nil {
					return err
				}
				out = append(out, *m)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListLocks implements store.Store, sorted by key.
func (s *Store) ListLocks(ctx context.Context) ([]store.Lock, error) {
	schemas, err := s.dataSchemas(ctx)
	if err != nil {
		return nil, err
	}
	var out []store.Lock
	for _, schema := range schemas {
		err := retry(ctx, func() error {
			rows, err := s.query(ctx, s.db, "list_locks",
				"SELECT "+lockColumns+" FROM "+s.table(schema, "LOCKS")+" WHERE LOCK_ID IS NOT NULL ORDER BY STATE_KEY")
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				l, err := scanLock(rows)
				if err != nil {
					return err
				}
				out = append(out, *l)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func detailJSON(d map[string]any) (sql.NullString, error) {
	if len(d) == 0 {
		return sql.NullString{}, nil
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(raw), Valid: true}, nil
}

const auditColumns = `EVENT_ID, EVENT_TIME, REQUEST_ID, PRINCIPAL, REMOTE_ADDR, ACTION, STATE_KEY,
  VERSION_ID, LOCK_ID, RESULT, HTTP_STATUS, DETAIL`

// auditArgs returns the binds for one row. Times are bound as RFC 3339 text and
// cast by the statement, which avoids the driver's per-value type markers.
func auditArgs(e *store.AuditEvent) ([]any, error) {
	detail, err := detailJSON(e.Detail)
	if err != nil {
		return nil, err
	}
	status := sql.NullInt64{Int64: int64(e.HTTPStatus), Valid: e.HTTPStatus != 0}
	return []any{e.ID, e.Time.UTC().Format(time.RFC3339Nano), nullStr(e.RequestID), nullStr(e.Principal),
		nullStr(e.RemoteAddr), e.Action, nullStr(e.Key), nullStr(e.VersionID), nullStr(e.LockID),
		e.Result, status, detail}, nil
}

const auditValues = "(?,?,?,?,?,?,?,?,?,?,?,?)"

const auditSelect = `SELECT column1, TO_TIMESTAMP_TZ(column2), column3, column4, column5, column6, column7,
  column8, column9, column10, column11, PARSE_JSON(column12) FROM VALUES `

func (s *Store) insertAudit(ctx context.Context, q querier, schema string, events []store.AuditEvent) error {
	var args []any
	values := make([]string, 0, len(events))
	for i := range events {
		a, err := auditArgs(&events[i])
		if err != nil {
			return err
		}
		args = append(args, a...)
		values = append(values, auditValues)
	}
	stmt := "INSERT INTO " + s.table(schema, "AUDIT_LOG") + " (" + auditColumns + ") " +
		auditSelect + strings.Join(values, ",")
	_, err := s.exec(ctx, q, "audit", stmt, args...)
	return err
}

// AppendAudit implements store.Store.
func (s *Store) AppendAudit(ctx context.Context, events []store.AuditEvent) error {
	bySchema := map[string][]store.AuditEvent{}
	for _, e := range events {
		schema := s.schemaFor(e.Key)
		bySchema[schema] = append(bySchema[schema], e)
	}
	for schema, rows := range bySchema {
		for len(rows) > 0 {
			n := min(auditChunk, len(rows))
			chunk := rows[:n]
			rows = rows[n:]
			// EVENT_ID has no uniqueness guarantee, so a retried batch could
			// duplicate rows. Audit batches are advisory, and the buffer has
			// its own retry, so a failure here is simply returned.
			if err := s.insertAudit(ctx, s.db, schema, chunk); err != nil {
				return classify(err)
			}
		}
	}
	return nil
}

// CreateToken implements store.Store.
func (s *Store) CreateToken(ctx context.Context, t *store.Token) error {
	scopes, err := json.Marshal(t.Scopes)
	if err != nil {
		return err
	}
	var expires sql.NullString
	if t.ExpiresAt != nil {
		expires = sql.NullString{String: t.ExpiresAt.UTC().Format(time.RFC3339Nano), Valid: true}
	}
	_, err = s.exec(ctx, s.db, "token_create",
		"INSERT INTO "+s.controlTable("TOKENS")+
			" (TOKEN_ID, PRINCIPAL, TOKEN_HMAC, SCOPES, DESCRIPTION, CREATED_BY, EXPIRES_AT)"+
			" SELECT ?, ?, ?, PARSE_JSON(?), ?, ?, TO_TIMESTAMP_TZ(?)",
		t.ID, t.Principal, t.HMAC, string(scopes), nullStr(t.Description), nullStr(t.CreatedBy), expires)
	return classify(err)
}

//nolint:gosec // column names
const tokenColumns = `TOKEN_ID, PRINCIPAL, TOKEN_HMAC, TO_JSON(SCOPES), DESCRIPTION, CREATED_AT,
  CREATED_BY, EXPIRES_AT, REVOKED_AT, LAST_USED_AT`

func scanToken(r scanner) (*store.Token, error) {
	var t store.Token
	var scopes string
	var desc, by sql.NullString
	var expires, revoked, used sql.NullTime
	if err := r.Scan(&t.ID, &t.Principal, &t.HMAC, &scopes, &desc, &t.CreatedAt, &by, &expires, &revoked, &used); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
		return nil, fmt.Errorf("token %s has bad scopes: %w", t.ID, err)
	}
	t.Description, t.CreatedBy = desc.String, by.String
	if expires.Valid {
		t.ExpiresAt = &expires.Time
	}
	if revoked.Valid {
		t.RevokedAt = &revoked.Time
	}
	if used.Valid {
		t.LastUsedAt = &used.Time
	}
	return &t, nil
}

// GetTokenByHMAC implements store.Store.
func (s *Store) GetTokenByHMAC(ctx context.Context, hmac string) (*store.Token, error) {
	var out *store.Token
	err := retry(ctx, func() error {
		rows, err := s.query(ctx, s.db, "token_get",
			"SELECT "+tokenColumns+" FROM "+s.controlTable("TOKENS")+" WHERE TOKEN_HMAC = ?", hmac)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return err
			}
			return store.ErrNotFound
		}
		out, err = scanToken(rows)
		return err
	})
	return out, err
}

// ListTokens implements store.Store.
func (s *Store) ListTokens(ctx context.Context) ([]store.Token, error) {
	var out []store.Token
	err := retry(ctx, func() error {
		out = nil
		rows, err := s.query(ctx, s.db, "token_list",
			"SELECT "+tokenColumns+" FROM "+s.controlTable("TOKENS")+" ORDER BY CREATED_AT, TOKEN_ID")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			t, err := scanToken(rows)
			if err != nil {
				return err
			}
			out = append(out, *t)
		}
		return rows.Err()
	})
	return out, err
}

// RevokeToken implements store.Store.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	res, err := s.exec(ctx, s.db, "token_revoke",
		"UPDATE "+s.controlTable("TOKENS")+" SET REVOKED_AT = CURRENT_TIMESTAMP() WHERE TOKEN_ID = ?", id)
	if err != nil {
		return classify(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// TouchToken implements store.Store.
func (s *Store) TouchToken(ctx context.Context, id string, at time.Time) error {
	_, err := s.exec(ctx, s.db, "token_touch",
		"UPDATE "+s.controlTable("TOKENS")+" SET LAST_USED_AT = TO_TIMESTAMP_TZ(?) WHERE TOKEN_ID = ?",
		at.UTC().Format(time.RFC3339Nano), id)
	return classify(err)
}

// DeleteVersions implements store.Store.
func (s *Store) DeleteVersions(ctx context.Context, key string, ids []string) error {
	for len(ids) > 0 {
		n := min(1000, len(ids))
		chunk := ids[:n]
		ids = ids[n:]
		args := make([]any, 0, n+1)
		args = append(args, key)
		for _, id := range chunk {
			args = append(args, id)
		}
		_, err := s.exec(ctx, s.db, "gc_versions",
			"DELETE FROM "+s.dataTable(key, "STATE_VERSIONS")+" WHERE STATE_KEY = ? AND VERSION_ID IN ("+
				strings.TrimSuffix(strings.Repeat("?,", n), ",")+")", args...)
		if err != nil {
			return classify(err)
		}
	}
	return nil
}

// PurgeState implements store.Store.
func (s *Store) PurgeState(ctx context.Context, key string) error {
	_, err := s.exec(ctx, s.db, "gc_state", "DELETE FROM "+s.dataTable(key, "STATES")+" WHERE STATE_KEY = ?", key)
	return classify(err)
}

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error {
	return retry(ctx, func() error {
		rows, err := s.query(ctx, s.db, "ping", "SELECT 1")
		if err != nil {
			return err
		}
		_, err = anyRow(rows)
		return err
	})
}

// Close implements store.Store.
func (s *Store) Close() error { return s.db.Close() }

var _ store.Store = (*Store)(nil)
