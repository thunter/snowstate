// Package memory is an in-process Store with the same semantics as Snowflake.
// One mutex serialises every transaction, which gives lock atomicity. It backs
// the unit tests and the e2e suite, and is never used in production.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thunter/snowstate/pkg/snowstate/store"
)

// Options configures a Store.
type Options struct {
	// SnapshotFile, when set, is loaded at start and rewritten after every
	// change. This is what lets test-only sidecars share state across
	// processes (e2e scenarios 9 and 10).
	SnapshotFile string
}

// Store is the in-memory implementation.
type Store struct {
	mu   sync.Mutex
	opts Options
	data snapshot
	// failBlob, when set, makes PutBlob fail. Tests use it.
	failBlob error
}

// snapshot is everything the store holds. Fields are exported for JSON.
type snapshot struct {
	States   map[string]store.StateMeta
	Versions map[string]store.Version
	Locks    map[string]store.Lock
	Tokens   map[string]store.Token
	Blobs    map[string][]byte
	Audit    []store.AuditEvent
}

// New returns an empty store, or one loaded from opts.SnapshotFile.
func New(opts Options) (*Store, error) {
	s := &Store{opts: opts, data: snapshot{
		States:   map[string]store.StateMeta{},
		Versions: map[string]store.Version{},
		Locks:    map[string]store.Lock{},
		Tokens:   map[string]store.Token{},
		Blobs:    map[string][]byte{},
	}}
	if opts.SnapshotFile == "" {
		return s, nil
	}
	data, err := readSnapshot(opts.SnapshotFile)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", opts.SnapshotFile, err)
	}
	s.data = *data
	return s, nil
}

// MustNew returns an empty store with no snapshot file.
func MustNew() *Store {
	s, err := New(Options{})
	if err != nil {
		panic(err)
	}
	return s
}

// ReadSnapshotAudit returns the audit rows in a snapshot file. E2E tests use
// it to inspect a server running in another process.
func ReadSnapshotAudit(path string) ([]store.AuditEvent, error) {
	data, err := readSnapshot(path)
	if err != nil {
		return nil, err
	}
	return data.Audit, nil
}

// ReadSnapshotLocks returns the locks in a snapshot file.
func ReadSnapshotLocks(path string) (map[string]store.Lock, error) {
	data, err := readSnapshot(path)
	if err != nil {
		return nil, err
	}
	return data.Locks, nil
}

func readSnapshot(path string) (*snapshot, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a test-only path chosen by the operator
	if err != nil {
		return nil, err
	}
	var data snapshot
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

// save persists the snapshot. Callers hold s.mu.
func (s *Store) save() error {
	if s.opts.SnapshotFile == "" {
		return nil
	}
	raw, err := json.Marshal(&s.data)
	if err != nil {
		return err
	}
	tmp := s.opts.SnapshotFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.opts.SnapshotFile)
}

// tx records undo steps so a failed transaction leaves no trace.
type tx struct {
	s    *Store
	undo []func()
}

// InTx implements store.Store.
func (s *Store) InTx(ctx context.Context, _ string, fn func(store.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", store.ErrUnavailable, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &tx{s: s}
	if err := fn(t); err != nil {
		for i := len(t.undo) - 1; i >= 0; i-- {
			t.undo[i]()
		}
		return err
	}
	if len(t.undo) == 0 {
		return nil
	}
	return s.save()
}

func (t *tx) GetLock(_ context.Context, key string) (*store.Lock, error) {
	return t.s.lock(key), nil
}

func (s *Store) lock(key string) *store.Lock {
	l, ok := s.data.Locks[key]
	if !ok {
		return nil
	}
	return &l
}

func (t *tx) InsertLock(_ context.Context, l *store.Lock) (bool, error) {
	if _, held := t.s.data.Locks[l.Key]; held {
		return false, nil
	}
	cp := *l
	if cp.AcquiredAt.IsZero() {
		cp.AcquiredAt = time.Now().UTC()
	}
	t.s.data.Locks[l.Key] = cp
	t.undo = append(t.undo, func() { delete(t.s.data.Locks, l.Key) })
	return true, nil
}

func (t *tx) DeleteLock(_ context.Context, key, lockID string) (bool, error) {
	cur, held := t.s.data.Locks[key]
	if !held || (lockID != "" && cur.LockID != lockID) {
		return false, nil
	}
	delete(t.s.data.Locks, key)
	t.undo = append(t.undo, func() { t.s.data.Locks[key] = cur })
	return true, nil
}

func (t *tx) GetState(_ context.Context, key string) (*store.StateMeta, error) {
	return t.s.state(key), nil
}

func (s *Store) state(key string) *store.StateMeta {
	m, ok := s.data.States[key]
	if !ok {
		return nil
	}
	return &m
}

func (t *tx) InsertVersion(_ context.Context, v *store.Version) error {
	cp := *v
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	t.s.data.Versions[v.ID] = cp
	t.undo = append(t.undo, func() { delete(t.s.data.Versions, v.ID) })
	return nil
}

func (t *tx) PutState(_ context.Context, m *store.StateMeta) error {
	prev, had := t.s.data.States[m.Key]
	cp := *m
	cp.DeletedAt = nil
	cp.UpdatedAt = time.Now().UTC()
	cp.CreatedAt = cp.UpdatedAt
	if had {
		cp.CreatedAt = prev.CreatedAt
	}
	t.s.data.States[m.Key] = cp
	t.undo = append(t.undo, func() {
		if had {
			t.s.data.States[m.Key] = prev
		} else {
			delete(t.s.data.States, m.Key)
		}
	})
	return nil
}

func (t *tx) SoftDeleteState(_ context.Context, key, by string) error {
	prev, had := t.s.data.States[key]
	if !had {
		return nil
	}
	now := time.Now().UTC()
	cp := prev
	cp.DeletedAt, cp.UpdatedAt, cp.UpdatedBy = &now, now, by
	t.s.data.States[key] = cp
	t.undo = append(t.undo, func() { t.s.data.States[key] = prev })
	return nil
}

func (t *tx) InsertAudit(_ context.Context, e *store.AuditEvent) error {
	n := len(t.s.data.Audit)
	t.s.data.Audit = append(t.s.data.Audit, *e)
	t.undo = append(t.undo, func() { t.s.data.Audit = t.s.data.Audit[:n] })
	return nil
}

// GetState implements store.Store.
func (s *Store) GetState(ctx context.Context, key string) (*store.StateMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrUnavailable, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state(key), nil
}

// GetLock implements store.Store.
func (s *Store) GetLock(_ context.Context, key string) (*store.Lock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lock(key), nil
}

// GetVersion implements store.Store.
func (s *Store) GetVersion(_ context.Context, key, id string) (*store.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data.Versions[id]
	if !ok || v.Key != key {
		return nil, store.ErrNotFound
	}
	return &v, nil
}

// ListVersions implements store.Store, newest first.
func (s *Store) ListVersions(_ context.Context, key string, limit int) ([]store.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Version, 0)
	for _, v := range s.data.Versions {
		if v.Key == key {
			v.Content = nil
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListStates implements store.Store, sorted by key.
func (s *Store) ListStates(_ context.Context, opts store.ListOptions) ([]store.StateMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.StateMeta, 0, len(s.data.States))
	for _, m := range s.data.States {
		if m.Deleted() && !opts.IncludeDeleted {
			continue
		}
		if opts.Prefix != "" && m.Key != opts.Prefix && !strings.HasPrefix(m.Key, opts.Prefix+"/") {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// ListLocks implements store.Store, sorted by key.
func (s *Store) ListLocks(_ context.Context) ([]store.Lock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Lock, 0, len(s.data.Locks))
	for _, l := range s.data.Locks {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// PutBlob implements store.Store.
func (s *Store) PutBlob(_ context.Context, key, versionID string, gz []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failBlob != nil {
		return "", s.failBlob
	}
	path := key + "/" + versionID + ".json.gz"
	s.data.Blobs[path] = append([]byte(nil), gz...)
	return path, s.save()
}

// FailBlobs makes PutBlob return err until it is called with nil.
func (s *Store) FailBlobs(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failBlob = err
}

// GetBlob implements store.Store.
func (s *Store) GetBlob(_ context.Context, _, path string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.data.Blobs[path]
	if !ok {
		return nil, store.ErrNotFound
	}
	return b, nil
}

// RemoveBlob implements store.Store.
func (s *Store) RemoveBlob(_ context.Context, _, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Blobs, path)
	return s.save()
}

// ListBlobs implements store.Store.
func (s *Store) ListBlobs(_ context.Context) ([]store.Blob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Blob, 0, len(s.data.Blobs))
	for p := range s.data.Blobs {
		out = append(out, store.Blob{Path: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// AppendAudit implements store.Store.
func (s *Store) AppendAudit(_ context.Context, events []store.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Audit = append(s.data.Audit, events...)
	return s.save()
}

// Audit returns a copy of the audit log. Tests use it.
func (s *Store) Audit() []store.AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.AuditEvent(nil), s.data.Audit...)
}

// CreateToken implements store.Store.
func (s *Store) CreateToken(_ context.Context, t *store.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Tokens[t.ID] = *t
	return s.save()
}

// GetTokenByHMAC implements store.Store.
func (s *Store) GetTokenByHMAC(_ context.Context, hmac string) (*store.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.data.Tokens {
		if t.HMAC == hmac {
			return &t, nil
		}
	}
	return nil, store.ErrNotFound
}

// ListTokens implements store.Store.
func (s *Store) ListTokens(_ context.Context) ([]store.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Token, 0, len(s.data.Tokens))
	for _, t := range s.data.Tokens {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// RevokeToken implements store.Store.
func (s *Store) RevokeToken(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.data.Tokens[id]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	t.RevokedAt = &now
	s.data.Tokens[id] = t
	return s.save()
}

// TouchToken implements store.Store.
func (s *Store) TouchToken(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.data.Tokens[id]; ok {
		t.LastUsedAt = &at
		s.data.Tokens[id] = t
	}
	return s.save()
}

// DeleteVersions implements store.Store.
func (s *Store) DeleteVersions(_ context.Context, _ string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.data.Versions, id)
	}
	return s.save()
}

// PurgeState implements store.Store.
func (s *Store) PurgeState(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.States, key)
	return s.save()
}

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", store.ErrUnavailable, err)
	}
	return nil
}

// Close implements store.Store.
func (s *Store) Close() error { return nil }

var _ store.Store = (*Store)(nil)
