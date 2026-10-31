// Package store defines the storage interface and the types shared by every
// frontend. Implementations live in store/memory and store/snowflake.
//
// Business rules belong in pkg/snowstate/service. A store only provides
// atomic primitives; Tx is the seam that lets the service run its rules
// inside one storage transaction.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Blob storage kinds, as stored in STATE_VERSIONS.STORAGE.
const (
	StorageInline = "INLINE"
	StorageStage  = "STAGE"
)

// Lock sources, as stored in LOCKS.SOURCE.
const (
	SourceHTTP = "HTTP"
	SourcePSS  = "PSS"
	SourceCLI  = "CLI"
)

// Audit results, as stored in AUDIT_LOG.RESULT.
const (
	ResultOK       = "OK"
	ResultDenied   = "DENIED"
	ResultConflict = "CONFLICT"
	ResultError    = "ERROR"
)

// Audit actions, as stored in AUDIT_LOG.ACTION.
const (
	ActionGet         = "GET"
	ActionPut         = "PUT"
	ActionPutNoop     = "PUT_NOOP"
	ActionDelete      = "DELETE"
	ActionLock        = "LOCK"
	ActionUnlock      = "UNLOCK"
	ActionForceUnlock = "FORCE_UNLOCK"
	ActionRestore     = "RESTORE"
	ActionTokenCreate = "TOKEN_CREATE"
	ActionTokenRevoke = "TOKEN_REVOKE"
	ActionGC          = "GC"
	ActionAdminRead   = "ADMIN_READ"
)

// Sentinel errors. ErrLocked and ErrLockMismatch are struct types below
// because they carry the current lock.
var (
	ErrNotFound         = errors.New("not found")
	ErrLockNotHeld      = errors.New("lock not held")
	ErrLockRequired     = errors.New("lock required")
	ErrInvalidKey       = errors.New("invalid key")
	ErrInvalidState     = errors.New("invalid state")
	ErrInvalidLock      = errors.New("invalid lock")
	ErrMD5Mismatch      = errors.New("md5 mismatch")
	ErrTooLarge         = errors.New("state too large")
	ErrForbidden        = errors.New("forbidden")
	ErrUnavailable      = errors.New("storage unavailable")
	ErrSerialRegression = errors.New("serial regression")
	// ErrCommitUnknown means a COMMIT was sent but its outcome is not known.
	// The caller must re-read before deciding what happened (SPEC §7).
	ErrCommitUnknown = errors.New("commit outcome unknown")
)

// ErrLocked reports that a lock is held by someone else.
type ErrLocked struct{ Current *Lock }

func (e *ErrLocked) Error() string {
	if e.Current == nil {
		return "state is locked"
	}
	return fmt.Sprintf("state is locked by %s (ID %s)", e.Current.Who, e.Current.LockID)
}

// ErrLockMismatch reports an unlock with an ID that is not the held lock's.
type ErrLockMismatch struct{ Current *Lock }

func (e *ErrLockMismatch) Error() string {
	if e.Current == nil {
		return "lock ID mismatch"
	}
	return fmt.Sprintf("lock ID mismatch: held lock is %s", e.Current.LockID)
}

// Principal identifies a caller. Scopes is empty in direct mode, where
// Snowflake grants are the enforcement point.
type Principal struct {
	Name   string
	Source string // "token" | "mtls" | "loopback" | "snowflake"
	Scopes []Scope
}

// Scope grants permissions on keys matching a glob.
type Scope struct {
	KeyPattern  string   `json:"key_pattern"`
	Permissions []string `json:"permissions"`
}

// Lock is a held lock. The normalised columns are authoritative; RawJSON is
// only set for locks taken over HTTP.
type Lock struct {
	Key        string
	LockID     string
	Operation  string
	Who        string
	Info       string
	TFVersion  string
	Path       string
	Source     string
	AcquiredBy string
	Created    time.Time
	AcquiredAt time.Time
	RawJSON    []byte
}

// StateMeta is the row in STATES.
type StateMeta struct {
	Key              string
	Project          string
	CurrentVersionID string
	Serial           int64
	Lineage          string
	TFVersion        string
	MD5              string
	SizeBytes        int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
	UpdatedBy        string
	DeletedAt        *time.Time
}

// Deleted reports whether the state has been soft deleted.
func (m *StateMeta) Deleted() bool { return m != nil && m.DeletedAt != nil }

// Version is a row in STATE_VERSIONS. Content holds gzip bytes for INLINE
// versions when the store was asked for them.
type Version struct {
	ID           string
	Key          string
	Serial       int64
	Lineage      string
	TFVersion    string
	MD5          string
	SizeBytes    int64
	Storage      string
	Content      []byte
	StagePath    string
	LockID       string
	CreatedAt    time.Time
	CreatedBy    string
	RestoredFrom string
}

// Token is a row in TOKENS. Only the HMAC of the token is ever stored.
type Token struct {
	ID          string
	Principal   string
	HMAC        string
	Scopes      []Scope
	Description string
	CreatedAt   time.Time
	CreatedBy   string
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
	LastUsedAt  *time.Time
}

// AuditEvent is a row in AUDIT_LOG.
type AuditEvent struct {
	ID         string
	Time       time.Time
	RequestID  string
	Principal  string
	RemoteAddr string
	Action     string
	Key        string
	VersionID  string
	LockID     string
	Result     string
	HTTPStatus int
	Detail     map[string]any
}

// Blob describes an object in the stage, used for the orphan sweep.
type Blob struct {
	Path string
	// Key selects the schema in schema_per_project tenancy.
	Key string
}

// ListOptions narrows state listings.
type ListOptions struct {
	Prefix         string
	IncludeDeleted bool
}

// Tx is the set of operations that run inside one storage transaction.
type Tx interface {
	GetLock(ctx context.Context, key string) (*Lock, error)
	// InsertLock atomically acquires a lock. It returns false when a lock
	// already exists for the key, without error.
	InsertLock(ctx context.Context, l *Lock) (bool, error)
	// DeleteLock removes the lock. An empty lockID removes whatever is held.
	// It returns false when nothing matched.
	DeleteLock(ctx context.Context, key, lockID string) (bool, error)
	GetState(ctx context.Context, key string) (*StateMeta, error)
	InsertVersion(ctx context.Context, v *Version) error
	// PutState moves the current pointer, clearing any soft delete.
	PutState(ctx context.Context, m *StateMeta) error
	SoftDeleteState(ctx context.Context, key, by string) error
	InsertAudit(ctx context.Context, e *AuditEvent) error
}

// Store is the persistence interface. Every method is safe for concurrent use.
type Store interface {
	// InTx runs fn in one transaction. An error from fn rolls back. key routes
	// the transaction to the right schema in schema_per_project tenancy.
	InTx(ctx context.Context, key string, fn func(Tx) error) error

	// Reads outside a transaction. GetState and GetLock return nil, nil when
	// there is no row. GetVersion returns ErrNotFound.
	GetState(ctx context.Context, key string) (*StateMeta, error)
	GetLock(ctx context.Context, key string) (*Lock, error)
	GetVersion(ctx context.Context, key, id string) (*Version, error)
	ListVersions(ctx context.Context, key string, limit int) ([]Version, error)
	ListStates(ctx context.Context, opts ListOptions) ([]StateMeta, error)
	ListLocks(ctx context.Context) ([]Lock, error)

	// Blobs. PutBlob is called before the metadata transaction.
	PutBlob(ctx context.Context, key, versionID string, gz []byte) (string, error)
	GetBlob(ctx context.Context, key, path string) ([]byte, error)
	RemoveBlob(ctx context.Context, key, path string) error
	ListBlobs(ctx context.Context) ([]Blob, error)

	// AppendAudit writes a batch outside any transaction.
	AppendAudit(ctx context.Context, events []AuditEvent) error

	// Tokens.
	CreateToken(ctx context.Context, t *Token) error
	GetTokenByHMAC(ctx context.Context, hmac string) (*Token, error)
	ListTokens(ctx context.Context) ([]Token, error)
	RevokeToken(ctx context.Context, id string) error
	TouchToken(ctx context.Context, id string, at time.Time) error

	// GC. DeleteVersions removes rows only; blobs go through RemoveBlob.
	DeleteVersions(ctx context.Context, key string, ids []string) error
	PurgeState(ctx context.Context, key string) error

	// Ping checks connectivity.
	Ping(ctx context.Context) error
	Close() error
}

// Observer receives timing for storage operations. It exists so that stores,
// which a provider can reuse, need not import a metrics library.
type Observer interface {
	ObserveQuery(op string, d time.Duration, err error)
}

type requestMetaKey struct{}

// RequestMeta carries per-request facts for audit rows and query tags.
type RequestMeta struct {
	RequestID  string
	RemoteAddr string
	// QueryIDs, when set, collects Snowflake query IDs for the request log.
	QueryIDs *QueryIDs
}

// WithRequestMeta attaches request metadata to ctx.
func WithRequestMeta(ctx context.Context, m RequestMeta) context.Context {
	return context.WithValue(ctx, requestMetaKey{}, m)
}

// RequestMetaFrom returns the metadata attached to ctx, if any.
func RequestMetaFrom(ctx context.Context) RequestMeta {
	m, _ := ctx.Value(requestMetaKey{}).(RequestMeta)
	return m
}
