package snowflake

import (
	"context"
	"database/sql"
	"time"

	sf "github.com/snowflakedb/gosnowflake"

	"github.com/thunter/snowstate/pkg/snowstate/store"
)

// txn implements store.Tx over one Snowflake transaction.
type txn struct {
	s  *Store
	tx *sql.Tx
}

// GetLock reads the lock inside the transaction. In hybrid mode the row is
// locked FOR UPDATE, so a holder's write cannot interleave with a force-unlock
// and a new acquire: those wait until this transaction ends.
func (t *txn) GetLock(ctx context.Context, key string) (*store.Lock, error) {
	suffix := ""
	if t.s.opts.LockMode == LockHybrid {
		suffix = " FOR UPDATE"
	}
	return t.s.getLock(ctx, t.tx, key, suffix)
}

// PeekLock implements store.Tx.
func (t *txn) PeekLock(ctx context.Context, key string) (*store.Lock, error) {
	return t.s.getLock(ctx, t.tx, key, "")
}

func lockArgs(l *store.Lock) []any {
	var raw sql.NullString
	if len(l.RawJSON) > 0 {
		raw = sql.NullString{String: string(l.RawJSON), Valid: true}
	}
	var created sql.NullString
	if !l.Created.IsZero() {
		created = sql.NullString{String: l.Created.UTC().Format(time.RFC3339Nano), Valid: true}
	}
	return []any{l.Key, l.LockID, l.Source, nullStr(l.Operation), nullStr(l.Who), nullStr(l.Info),
		nullStr(l.TFVersion), nullStr(l.Path), created, raw,
		l.AcquiredAt.UTC().Format(time.RFC3339Nano), nullStr(l.AcquiredBy)}
}

// InsertLock implements store.Tx. The primary key on the hybrid LOCKS table
// makes this the atomic acquire.
func (t *txn) InsertLock(ctx context.Context, l *store.Lock) (bool, error) {
	if t.s.opts.LockMode == LockStandard {
		return t.insertLockStandard(ctx, l)
	}
	_, err := t.s.exec(ctx, t.tx, "lock_insert",
		"INSERT INTO "+t.s.dataTable(l.Key, "LOCKS")+" ("+lockColumns+") "+
			"SELECT ?, ?, ?, ?, ?, ?, ?, ?, TO_TIMESTAMP_TZ(?), ?, TO_TIMESTAMP_TZ(?), ?", lockArgs(l)...)
	if isPrimaryKeyViolation(err) {
		return false, nil
	}
	return err == nil, err
}

// DeleteLock implements store.Tx.
func (t *txn) DeleteLock(ctx context.Context, key, lockID string) (bool, error) {
	if t.s.opts.LockMode == LockStandard {
		return t.deleteLockStandard(ctx, key, lockID)
	}
	stmt := "DELETE FROM " + t.s.dataTable(key, "LOCKS") + " WHERE STATE_KEY = ?"
	args := []any{key}
	if lockID != "" {
		stmt += " AND LOCK_ID = ?"
		args = append(args, lockID)
	}
	res, err := t.s.exec(ctx, t.tx, "lock_delete", stmt, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// GetState implements store.Tx.
func (t *txn) GetState(ctx context.Context, key string) (*store.StateMeta, error) {
	return t.s.getState(ctx, t.tx, key)
}

// InsertVersion implements store.Tx.
func (t *txn) InsertVersion(ctx context.Context, v *store.Version) error {
	_, err := t.s.exec(ctx, t.tx, "version_insert",
		"INSERT INTO "+t.s.dataTable(v.Key, "STATE_VERSIONS")+
			" (VERSION_ID, STATE_KEY, SERIAL, LINEAGE, TF_VERSION, STATE_MD5, SIZE_BYTES, STORAGE,"+
			" CONTENT_ZLIB, STAGE_PATH, LOCK_ID, CREATED_BY, RESTORED_FROM)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		v.ID, v.Key, v.Serial, v.Lineage, nullStr(v.TFVersion), v.MD5, v.SizeBytes, v.Storage,
		sf.DataTypeBinary, v.Content,
		nullStr(v.StagePath), nullStr(v.LockID), nullStr(v.CreatedBy), nullStr(v.RestoredFrom))
	return err
}

// PutState implements store.Tx.
func (t *txn) PutState(ctx context.Context, m *store.StateMeta) error {
	_, err := t.s.exec(ctx, t.tx, "state_merge",
		"MERGE INTO "+t.s.dataTable(m.Key, "STATES")+` t USING (SELECT
			?::VARCHAR AS STATE_KEY, ?::VARCHAR AS PROJECT, ?::VARCHAR AS CURRENT_VERSION_ID,
			?::NUMBER(38,0) AS SERIAL, ?::VARCHAR AS LINEAGE, ?::VARCHAR AS TF_VERSION,
			?::VARCHAR AS STATE_MD5, ?::NUMBER(38,0) AS SIZE_BYTES, ?::VARCHAR AS UPDATED_BY) s
		ON t.STATE_KEY = s.STATE_KEY
		WHEN MATCHED THEN UPDATE SET CURRENT_VERSION_ID = s.CURRENT_VERSION_ID, SERIAL = s.SERIAL,
			LINEAGE = s.LINEAGE, TF_VERSION = s.TF_VERSION, STATE_MD5 = s.STATE_MD5,
			SIZE_BYTES = s.SIZE_BYTES, UPDATED_AT = CURRENT_TIMESTAMP(), UPDATED_BY = s.UPDATED_BY,
			DELETED_AT = NULL
		WHEN NOT MATCHED THEN INSERT (STATE_KEY, PROJECT, CURRENT_VERSION_ID, SERIAL, LINEAGE,
			TF_VERSION, STATE_MD5, SIZE_BYTES, UPDATED_BY)
		VALUES (s.STATE_KEY, s.PROJECT, s.CURRENT_VERSION_ID, s.SERIAL, s.LINEAGE, s.TF_VERSION,
			s.STATE_MD5, s.SIZE_BYTES, s.UPDATED_BY)`,
		m.Key, m.Project, m.CurrentVersionID, m.Serial, m.Lineage, nullStr(m.TFVersion),
		m.MD5, m.SizeBytes, nullStr(m.UpdatedBy))
	return err
}

// SoftDeleteState implements store.Tx.
func (t *txn) SoftDeleteState(ctx context.Context, key, by string) error {
	_, err := t.s.exec(ctx, t.tx, "state_delete",
		"UPDATE "+t.s.dataTable(key, "STATES")+
			" SET DELETED_AT = CURRENT_TIMESTAMP(), UPDATED_AT = CURRENT_TIMESTAMP(), UPDATED_BY = ?"+
			" WHERE STATE_KEY = ?", nullStr(by), key)
	return err
}

// InsertAudit implements store.Tx.
func (t *txn) InsertAudit(ctx context.Context, e *store.AuditEvent) error {
	return t.s.insertAudit(ctx, t.tx, t.s.schemaFor(e.Key), []store.AuditEvent{*e})
}

// Standard mode is added in M5.
func (t *txn) insertLockStandard(context.Context, *store.Lock) (bool, error) {
	return false, errStandardUnsupported
}

func (t *txn) deleteLockStandard(context.Context, string, string) (bool, error) {
	return false, errStandardUnsupported
}
