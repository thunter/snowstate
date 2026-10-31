// Package service holds every business rule: lock checks, write fencing, MD5,
// size limits, versioning and authorization hooks. It is frontend-neutral.
// Each frontend maps the typed errors in store to its own protocol.
package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/thunter/snowstate/pkg/snowstate/ids"
	"github.com/thunter/snowstate/pkg/snowstate/keys"
	"github.com/thunter/snowstate/pkg/snowstate/store"
)

// Re-exported so frontends need only this package for the common types.
type (
	// Principal identifies a caller.
	Principal = store.Principal
	// Lock is a held lock.
	Lock = store.Lock
	// Version is one immutable state version.
	Version = store.Version
)

// Authorization actions.
const (
	ActionRead  = "read"
	ActionWrite = "write"
	ActionLock  = "lock"
	ActionAdmin = "admin"
)

// pssLockInfo is the Info shown to HTTP clients for provider-held locks.
const pssLockInfo = "held via snowstate state_store"

const (
	maxLockInfoBytes = 65536
	maxLockIDBytes   = 128
	maxLineageBytes  = 64
	// Normalised lock column widths, from LOCKS in §6.1.
	maxOperationBytes = 64
	maxWhoBytes       = 512
	maxInfoBytes      = 4096
	maxTFVersionBytes = 32
	maxPathBytes      = 1024
)

// LockRequest asks for a lock.
type LockRequest struct {
	Key, Operation, Who, Info, TFVersion, Path string
	Created                                    time.Time
	// LockID is Terraform-supplied over HTTP. Empty means the service
	// generates one, which is how the provider frontend works.
	LockID string
	// RawJSON is the original LockInfo body, stored verbatim. HTTP only.
	RawJSON []byte
	// Source is store.SourceHTTP, SourcePSS or SourceCLI.
	Source string
}

// StateBlob is a state read back from the store.
type StateBlob struct {
	Body      []byte
	MD5       []byte // raw 16 bytes
	VersionID string
	Serial    int64
}

// StateService is the frontend-facing API (SPEC §3).
type StateService interface {
	Get(ctx context.Context, p Principal, key string) (*StateBlob, error)
	Put(ctx context.Context, p Principal, key, lockID string, body, md5sum []byte) (*Version, error)
	Delete(ctx context.Context, p Principal, key, lockID string) error
	Lock(ctx context.Context, p Principal, req LockRequest) (*Lock, error)
	Unlock(ctx context.Context, p Principal, key, lockID string, force bool) (*Lock, error)
	ListWorkspaces(ctx context.Context, p Principal, prefix string) ([]string, error)
	AdminService
}

// Authorizer decides whether a principal may act on a key.
type Authorizer interface {
	Authorize(ctx context.Context, p Principal, action, key string) error
}

// AllowAll is the direct-mode authorizer: Snowflake is the enforcement point.
type AllowAll struct{}

// Authorize implements Authorizer.
func (AllowAll) Authorize(context.Context, Principal, string, string) error { return nil }

// Hooks are optional callbacks for metrics.
type Hooks struct {
	LockConflict func()
	StateBytes   func(n int)
}

// Options configures a Service.
type Options struct {
	Store                  store.Store
	Authorizer             Authorizer
	MaxStateBytes          int64
	InlineMaxBytes         int64
	RequireLockForWrite    bool
	RejectSerialRegression bool
	AllowForceUnlock       bool
	LockTTL                time.Duration
	LogReads               bool
	Retention              Retention
	Logger                 *slog.Logger
	Hooks                  Hooks

	// AcquiredBy overrides ACQUIRED_BY on locks. The CI context uses it.
	AcquiredBy string
	// AuditDetail is merged into the DETAIL of every audit row.
	AuditDetail map[string]any

	// AfterInsertVersion runs between steps 3 and 4 of the write transaction.
	// It exists so tests can inject a failure there (SPEC §12.3).
	AfterInsertVersion func() error
}

// Service implements StateService.
type Service struct {
	opts  Options
	st    store.Store
	authz Authorizer
	log   *slog.Logger
	audit *auditBuffer
}

// New returns a running Service. Call Close to flush the audit buffer.
func New(opts Options) *Service {
	if opts.Authorizer == nil {
		opts.Authorizer = AllowAll{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MaxStateBytes == 0 {
		opts.MaxStateBytes = 256 << 20
	}
	s := &Service{opts: opts, st: opts.Store, authz: opts.Authorizer, log: opts.Logger}
	s.audit = newAuditBuffer(opts.Store, opts.Logger)
	return s
}

// Close flushes the audit buffer. The store stays open.
func (s *Service) Close() { s.audit.close() }

// FlushAudit writes buffered audit rows now.
func (s *Service) FlushAudit() { s.audit.flush() }

// AuditDepth returns the number of audit rows waiting to be written.
func (s *Service) AuditDepth() int { return s.audit.depth() }

func (s *Service) authorize(ctx context.Context, p Principal, action, auditAction, key string) error {
	err := s.authz.Authorize(ctx, p, action, key)
	if err != nil {
		s.record(ctx, p, auditAction, key, "", "", err)
	}
	return err
}

// event builds an audit row.
func (s *Service) event(ctx context.Context, p Principal, action, key, versionID, lockID string, err error, detail map[string]any) store.AuditEvent {
	meta := store.RequestMetaFrom(ctx)
	merged := make(map[string]any, len(detail)+len(s.opts.AuditDetail))
	for k, v := range s.opts.AuditDetail {
		merged[k] = v
	}
	for k, v := range detail {
		merged[k] = v
	}
	if len(merged) == 0 {
		merged = nil
	}
	return store.AuditEvent{
		ID:         ids.V4(),
		Time:       time.Now().UTC(),
		RequestID:  meta.RequestID,
		Principal:  p.Name,
		RemoteAddr: meta.RemoteAddr,
		Action:     action,
		Key:        key,
		VersionID:  versionID,
		LockID:     lockID,
		Result:     resultOf(err),
		HTTPStatus: statusOf(err),
		Detail:     merged,
	}
}

// record queues an audit row outside any transaction.
func (s *Service) record(ctx context.Context, p Principal, action, key, versionID, lockID string, err error) {
	s.audit.add(s.event(ctx, p, action, key, versionID, lockID, err, nil))
}

// Get returns the current state, or nil, nil when there is none.
func (s *Service) Get(ctx context.Context, p Principal, key string) (*StateBlob, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, p, ActionRead, store.ActionGet, key); err != nil {
		return nil, err
	}
	m, err := s.st.GetState(ctx, key)
	if err != nil {
		return nil, err
	}
	if m == nil || m.Deleted() {
		s.recordRead(ctx, p, key, "")
		return nil, nil
	}
	v, err := s.st.GetVersion(ctx, key, m.CurrentVersionID)
	if err != nil {
		return nil, fmt.Errorf("current version of %s: %w", key, err)
	}
	blob, err := s.load(ctx, v)
	if err != nil {
		return nil, err
	}
	s.recordRead(ctx, p, key, v.ID)
	return blob, nil
}

func (s *Service) recordRead(ctx context.Context, p Principal, key, versionID string) {
	if s.opts.LogReads {
		s.record(ctx, p, store.ActionGet, key, versionID, "", nil)
	}
}

// load decompresses a version and verifies it against its recorded MD5.
func (s *Service) load(ctx context.Context, v *store.Version) (*StateBlob, error) {
	gz := v.Content
	if v.Storage == store.StorageStage {
		var err error
		if gz, err = s.st.GetBlob(ctx, v.Key, v.StagePath); err != nil {
			return nil, fmt.Errorf("blob for version %s: %w", v.ID, err)
		}
	}
	body, err := gunzip(gz)
	if err != nil {
		return nil, fmt.Errorf("version %s: %w", v.ID, err)
	}
	sum := md5.Sum(body)
	if hex.EncodeToString(sum[:]) != v.MD5 {
		return nil, fmt.Errorf("version %s: stored bytes fail MD5 check", v.ID)
	}
	return &StateBlob{Body: body, MD5: sum[:], VersionID: v.ID, Serial: v.Serial}, nil
}

// stateHeader is the part of a state file the service reads. The body itself
// is never re-serialized.
type stateHeader struct {
	Version           *json.Number `json:"version"`
	TerraformVersion  string       `json:"terraform_version"`
	Serial            *int64       `json:"serial"`
	Lineage           *string      `json:"lineage"`
	EncryptionVersion string       `json:"encryption_version"`
}

func parseState(body []byte) (*stateHeader, error) {
	var h stateHeader
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, fmt.Errorf("%w: not a JSON object: %w", store.ErrInvalidState, err)
	}
	// OpenTofu's client-side encrypted states have no "version" field.
	if h.Version == nil && h.EncryptionVersion == "" {
		return nil, fmt.Errorf("%w: missing version", store.ErrInvalidState)
	}
	if h.Serial == nil {
		return nil, fmt.Errorf("%w: missing serial", store.ErrInvalidState)
	}
	if h.Lineage == nil {
		return nil, fmt.Errorf("%w: missing lineage", store.ErrInvalidState)
	}
	if len(*h.Lineage) > maxLineageBytes {
		return nil, fmt.Errorf("%w: lineage is longer than %d bytes", store.ErrInvalidState, maxLineageBytes)
	}
	return &h, nil
}

// Put writes a new version. md5sum is the raw digest the client claims, or nil.
func (s *Service) Put(ctx context.Context, p Principal, key, lockID string, body, md5sum []byte) (*Version, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, p, ActionWrite, store.ActionPut, key); err != nil {
		return nil, err
	}
	if int64(len(body)) > s.opts.MaxStateBytes {
		return nil, store.ErrTooLarge
	}
	sum := md5.Sum(body)
	if md5sum != nil && !bytes.Equal(md5sum, sum[:]) {
		return nil, store.ErrMD5Mismatch
	}
	h, err := parseState(body)
	if err != nil {
		return nil, err
	}
	gz, err := gzipBytes(body)
	if err != nil {
		return nil, err
	}

	v := &store.Version{
		ID:        ids.V7(),
		Key:       key,
		Serial:    *h.Serial,
		Lineage:   *h.Lineage,
		TFVersion: truncate(h.TerraformVersion, maxTFVersionBytes),
		MD5:       hex.EncodeToString(sum[:]),
		SizeBytes: int64(len(body)),
		LockID:    lockID,
		CreatedBy: p.Name,
	}
	if err := s.placeBlob(ctx, v, gz); err != nil {
		return nil, err
	}

	result, err := s.commitVersion(ctx, p, v, store.ActionPut, nil)
	if err != nil {
		s.removeOrphan(ctx, v, err)
		s.recordWriteFailure(ctx, p, store.ActionPut, key, lockID, err)
		return nil, err
	}
	if result.ID != v.ID {
		s.removeOrphan(ctx, v, nil)
		s.record(ctx, p, store.ActionPutNoop, key, result.ID, lockID, nil)
	}
	if s.opts.Hooks.StateBytes != nil {
		s.opts.Hooks.StateBytes(len(body))
	}
	return result, nil
}

// placeBlob decides INLINE or STAGE, uploading before the transaction (§6.3).
func (s *Service) placeBlob(ctx context.Context, v *store.Version, gz []byte) error {
	if int64(len(gz)) <= s.opts.InlineMaxBytes {
		v.Storage, v.Content = store.StorageInline, gz
		return nil
	}
	path, err := s.st.PutBlob(ctx, v.Key, v.ID, gz)
	if err != nil {
		return fmt.Errorf("upload blob: %w", err)
	}
	v.Storage, v.StagePath = store.StorageStage, path
	return nil
}

// removeOrphan best-effort deletes a blob whose version never became visible.
func (s *Service) removeOrphan(ctx context.Context, v *store.Version, cause error) {
	if v.Storage != store.StorageStage || errors.Is(cause, store.ErrCommitUnknown) {
		return
	}
	if err := s.st.RemoveBlob(context.WithoutCancel(ctx), v.Key, v.StagePath); err != nil {
		s.log.Warn("orphan blob left for gc", "state_key", v.Key, "version_id", v.ID, "error", err)
	}
}

func (s *Service) recordWriteFailure(ctx context.Context, p Principal, action, key, lockID string, err error) {
	var locked *store.ErrLocked
	if errors.As(err, &locked) {
		s.conflict()
	}
	s.record(ctx, p, action, key, "", lockID, err)
}

func (s *Service) conflict() {
	if s.opts.Hooks.LockConflict != nil {
		s.opts.Hooks.LockConflict()
	}
}

// commitVersion is the transaction in §6.4. It returns the new version, or
// the current one when the bytes are unchanged.
func (s *Service) commitVersion(ctx context.Context, p Principal, v *store.Version, action string, detail map[string]any) (*Version, error) {
	var noop *Version
	err := s.st.InTx(ctx, v.Key, func(tx store.Tx) error {
		lock, err := tx.GetLock(ctx, v.Key)
		if err != nil {
			return err
		}
		if err := s.checkFence(lock, v.LockID); err != nil {
			return err
		}
		cur, err := tx.GetState(ctx, v.Key)
		if err != nil {
			return err
		}
		if cur != nil && !cur.Deleted() && cur.MD5 == v.MD5 {
			noop = &Version{ID: cur.CurrentVersionID, Key: v.Key, Serial: cur.Serial, MD5: cur.MD5, SizeBytes: cur.SizeBytes}
			return nil
		}
		if err := s.checkSerial(cur, v); err != nil {
			return err
		}
		if err := tx.InsertVersion(ctx, v); err != nil {
			return err
		}
		if s.opts.AfterInsertVersion != nil {
			if err := s.opts.AfterInsertVersion(); err != nil {
				return err
			}
		}
		if err := tx.PutState(ctx, &store.StateMeta{
			Key: v.Key, Project: keys.Project(v.Key), CurrentVersionID: v.ID,
			Serial: v.Serial, Lineage: v.Lineage, TFVersion: v.TFVersion,
			MD5: v.MD5, SizeBytes: v.SizeBytes, UpdatedBy: p.Name,
		}); err != nil {
			return err
		}
		e := s.event(ctx, p, action, v.Key, v.ID, v.LockID, nil, detail)
		return tx.InsertAudit(ctx, &e)
	})
	if errors.Is(err, store.ErrCommitUnknown) {
		// §7: never retry; re-read. If the version exists the write landed.
		if _, rerr := s.st.GetVersion(ctx, v.Key, v.ID); rerr == nil {
			return stripContent(v), nil
		}
		return nil, fmt.Errorf("%w: %w", store.ErrUnavailable, err)
	}
	if err != nil {
		return nil, err
	}
	if noop != nil {
		return noop, nil
	}
	return stripContent(v), nil
}

func stripContent(v *Version) *Version {
	out := *v
	out.Content = nil
	return &out
}

// checkFence applies the write rules in §4.3, whichever frontend took the lock.
func (s *Service) checkFence(lock *store.Lock, lockID string) error {
	switch {
	case lock != nil && lock.LockID != lockID:
		return &store.ErrLocked{Current: lock}
	case lock == nil && lockID != "":
		return store.ErrLockNotHeld
	case lock == nil && s.opts.RequireLockForWrite:
		return store.ErrLockRequired
	}
	return nil
}

func (s *Service) checkSerial(cur *store.StateMeta, v *store.Version) error {
	if cur == nil || cur.Deleted() {
		return nil
	}
	if cur.Lineage != v.Lineage {
		s.log.Warn("state lineage changed", "state_key", v.Key)
		return nil
	}
	if v.Serial < cur.Serial {
		s.log.Warn("state serial regression", "state_key", v.Key, "current", cur.Serial, "new", v.Serial)
		if s.opts.RejectSerialRegression {
			return fmt.Errorf("%w: %d is below current %d", store.ErrSerialRegression, v.Serial, cur.Serial)
		}
	}
	return nil
}

// Delete soft deletes a state. Deleting a missing state succeeds.
func (s *Service) Delete(ctx context.Context, p Principal, key, lockID string) error {
	if err := validKey(key); err != nil {
		return err
	}
	if err := s.authorize(ctx, p, ActionWrite, store.ActionDelete, key); err != nil {
		return err
	}
	err := s.st.InTx(ctx, key, func(tx store.Tx) error {
		lock, err := tx.GetLock(ctx, key)
		if err != nil {
			return err
		}
		if lock != nil && lock.LockID != lockID {
			return &store.ErrLocked{Current: lock}
		}
		if err := tx.SoftDeleteState(ctx, key, p.Name); err != nil {
			return err
		}
		e := s.event(ctx, p, store.ActionDelete, key, "", lockID, nil, nil)
		return tx.InsertAudit(ctx, &e)
	})
	if err != nil {
		s.recordWriteFailure(ctx, p, store.ActionDelete, key, lockID, err)
	}
	return err
}

// Lock acquires a lock, or returns *ErrLocked with the current holder.
func (s *Service) Lock(ctx context.Context, p Principal, req LockRequest) (*Lock, error) {
	if err := validKey(req.Key); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, p, ActionLock, store.ActionLock, req.Key); err != nil {
		return nil, err
	}
	l, err := s.newLock(p, req)
	if err != nil {
		return nil, err
	}

	var held *Lock
	err = s.st.InTx(ctx, l.Key, func(tx store.Tx) error {
		ok, err := tx.InsertLock(ctx, l)
		if err != nil {
			return err
		}
		if !ok {
			cur, err := tx.GetLock(ctx, l.Key)
			if err != nil {
				return err
			}
			if cur == nil { // released between the two statements
				return fmt.Errorf("%w: lock vanished during acquire", store.ErrUnavailable)
			}
			if cur.LockID == l.LockID {
				held = cur
				return nil
			}
			if !s.lockExpired(cur) {
				return &store.ErrLocked{Current: cur}
			}
			if err := s.stealLock(ctx, tx, p, cur, l); err != nil {
				return err
			}
		}
		e := s.event(ctx, p, store.ActionLock, l.Key, "", l.LockID, nil, nil)
		return tx.InsertAudit(ctx, &e)
	})
	if err != nil {
		s.recordWriteFailure(ctx, p, store.ActionLock, l.Key, l.LockID, err)
		return nil, err
	}
	if held != nil {
		return held, nil
	}
	return l, nil
}

func (s *Service) lockExpired(cur *Lock) bool {
	return s.opts.LockTTL > 0 && time.Since(cur.AcquiredAt) > s.opts.LockTTL
}

// stealLock replaces an expired lock, audited as FORCE_UNLOCK with reason ttl.
func (s *Service) stealLock(ctx context.Context, tx store.Tx, p Principal, cur, l *Lock) error {
	removed, err := tx.DeleteLock(ctx, cur.Key, cur.LockID)
	if err != nil {
		return err
	}
	if !removed {
		return &store.ErrLocked{Current: cur}
	}
	e := s.event(ctx, p, store.ActionForceUnlock, cur.Key, "", cur.LockID, nil, map[string]any{
		"reason": "ttl", "removed_lock": string(LockInfoJSON(cur)),
	})
	if err := tx.InsertAudit(ctx, &e); err != nil {
		return err
	}
	ok, err := tx.InsertLock(ctx, l)
	if err != nil {
		return err
	}
	if !ok {
		return &store.ErrLocked{Current: cur}
	}
	return nil
}

func (s *Service) newLock(p Principal, req LockRequest) (*Lock, error) {
	l := &Lock{
		Key:        req.Key,
		LockID:     req.LockID,
		Operation:  truncate(req.Operation, maxOperationBytes),
		Who:        truncate(req.Who, maxWhoBytes),
		Info:       truncate(req.Info, maxInfoBytes),
		TFVersion:  truncate(req.TFVersion, maxTFVersionBytes),
		Path:       truncate(req.Path, maxPathBytes),
		Source:     req.Source,
		AcquiredBy: s.opts.AcquiredBy,
		Created:    req.Created,
		AcquiredAt: time.Now().UTC(),
		RawJSON:    req.RawJSON,
	}
	if l.AcquiredBy == "" {
		l.AcquiredBy = p.Name
	}
	if l.LockID == "" {
		if req.Source == store.SourceHTTP {
			return nil, fmt.Errorf("%w: missing ID", store.ErrInvalidLock)
		}
		l.LockID = ids.V4()
	}
	if len(l.LockID) > maxLockIDBytes {
		return nil, fmt.Errorf("%w: ID is longer than %d bytes", store.ErrInvalidLock, maxLockIDBytes)
	}
	if len(l.RawJSON) > maxLockInfoBytes {
		return nil, fmt.Errorf("%w: body is larger than %d bytes", store.ErrInvalidLock, maxLockInfoBytes)
	}
	if l.Created.IsZero() {
		l.Created = l.AcquiredAt
	}
	return l, nil
}

// Unlock releases a lock and returns the lock it removed, or nil when none was
// held. With force set, lockID may be empty to remove whatever is held, or it
// makes the force conditional on that ID (OpenTofu's force-unlock).
func (s *Service) Unlock(ctx context.Context, p Principal, key, lockID string, force bool) (*Lock, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	action := store.ActionUnlock
	if force {
		action = store.ActionForceUnlock
	}
	if err := s.authorize(ctx, p, ActionLock, action, key); err != nil {
		return nil, err
	}
	if force && !s.opts.AllowForceUnlock {
		err := fmt.Errorf("%w: force unlock is disabled", store.ErrForbidden)
		s.record(ctx, p, action, key, "", lockID, err)
		return nil, err
	}

	var removed *Lock
	err := s.st.InTx(ctx, key, func(tx store.Tx) error {
		cur, err := tx.GetLock(ctx, key)
		if err != nil || cur == nil {
			return err
		}
		if lockID != "" && cur.LockID != lockID {
			return &store.ErrLockMismatch{Current: cur}
		}
		ok, err := tx.DeleteLock(ctx, key, cur.LockID)
		if err != nil || !ok {
			return err
		}
		var detail map[string]any
		if force {
			detail = map[string]any{"removed_lock": string(LockInfoJSON(cur))}
		}
		e := s.event(ctx, p, action, key, "", cur.LockID, nil, detail)
		if err := tx.InsertAudit(ctx, &e); err != nil {
			return err
		}
		removed = cur
		return nil
	})
	if err != nil {
		s.record(ctx, p, action, key, "", lockID, err)
		return nil, err
	}
	return removed, nil
}

// ListWorkspaces returns the workspaces of a prefix (§4.2). default is always
// included, because Terraform expects it to exist.
func (s *Service) ListWorkspaces(ctx context.Context, p Principal, prefix string) ([]string, error) {
	if err := keys.ValidatePrefix(prefix); err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalidKey, err)
	}
	if err := s.authz.Authorize(ctx, p, ActionRead, prefix+"/"+keys.DefaultWorkspace); err != nil {
		return nil, err
	}
	states, err := s.st.ListStates(ctx, store.ListOptions{Prefix: prefix})
	if err != nil {
		return nil, err
	}
	out := []string{}
	hasDefault := false
	for i := range states {
		if w, ok := keys.WorkspaceOf(prefix, states[i].Key); ok {
			out = append(out, w)
			hasDefault = hasDefault || w == keys.DefaultWorkspace
		}
	}
	if !hasDefault {
		out = append([]string{keys.DefaultWorkspace}, out...)
	}
	return out, nil
}

// LockInfoJSON renders a lock as Terraform LockInfo JSON. HTTP locks return
// their original bytes. Others are built from the normalised columns.
func LockInfoJSON(l *Lock) []byte {
	if len(l.RawJSON) > 0 {
		return l.RawJSON
	}
	raw, err := json.Marshal(map[string]any{
		"ID": l.LockID, "Operation": l.Operation, "Info": pssLockInfo, "Who": l.Who,
		"Version": l.TFVersion, "Created": l.Created.UTC().Format(time.RFC3339Nano), "Path": l.Path,
	})
	if err != nil {
		return []byte(`{}`) // unreachable: every value is a string
	}
	return raw
}

func validKey(key string) error {
	if err := keys.Validate(key); err != nil {
		return fmt.Errorf("%w: %w", store.ErrInvalidKey, err)
	}
	return nil
}

func gzipBytes(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(body); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzip(gz []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	defer func() { _ = r.Close() }()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	return out, nil
}

func truncate(v string, n int) string {
	if len(v) <= n {
		return v
	}
	for n > 0 && v[n]&0xC0 == 0x80 { // do not split a UTF-8 sequence
		n--
	}
	return v[:n]
}

func resultOf(err error) string {
	var locked *store.ErrLocked
	var mismatch *store.ErrLockMismatch
	switch {
	case err == nil:
		return store.ResultOK
	case errors.Is(err, store.ErrForbidden):
		return store.ResultDenied
	case errors.As(err, &locked), errors.As(err, &mismatch),
		errors.Is(err, store.ErrLockNotHeld), errors.Is(err, store.ErrLockRequired),
		errors.Is(err, store.ErrSerialRegression):
		return store.ResultConflict
	}
	return store.ResultError
}

// statusOf gives the protocol status an audit row records. Provider rows use
// the same numbers, which keeps AUDIT_LOG queries uniform across frontends.
func statusOf(err error) int {
	var locked *store.ErrLocked
	var mismatch *store.ErrLockMismatch
	switch {
	case err == nil:
		return 200
	case errors.Is(err, store.ErrForbidden):
		return 403
	case errors.As(err, &locked), errors.Is(err, store.ErrLockRequired):
		return 423
	case errors.As(err, &mismatch), errors.Is(err, store.ErrLockNotHeld), errors.Is(err, store.ErrSerialRegression):
		return 409
	case errors.Is(err, store.ErrUnavailable):
		return 503
	}
	return 500
}
