package service

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/pkg/snowstate/store"
	"github.com/thunter/snowstate/pkg/snowstate/store/memory"
)

const testKey = "platform/net/prod"

var alice = Principal{Name: "alice", Source: "token"}

func stateBody(serial int) []byte {
	return []byte(fmt.Sprintf(`{"version":4,"terraform_version":"1.9.0","serial":%d,"lineage":"line-1","outputs":{},"resources":[]}`, serial))
}

func newSvc(t *testing.T, mutate ...func(*Options)) (*Service, *memory.Store) {
	t.Helper()
	st := memory.MustNew()
	opts := Options{Store: st, InlineMaxBytes: 8 << 20, AllowForceUnlock: true, LogReads: true}
	for _, m := range mutate {
		m(&opts)
	}
	s := New(opts)
	t.Cleanup(s.Close)
	return s, st
}

func httpLock(id string) LockRequest {
	return LockRequest{
		Key: testKey, LockID: id, Who: "bob@ci", Operation: "OperationTypeApply", Source: store.SourceHTTP,
		RawJSON: []byte(fmt.Sprintf(`{"ID":%q,"Who":"bob@ci","Operation":"OperationTypeApply"}`, id)),
	}
}

func pssLock() LockRequest {
	return LockRequest{Key: testKey, Who: "carol@laptop", Operation: "apply", Source: store.SourcePSS}
}

func TestGetMissingIsNilNil(t *testing.T) {
	s, _ := newSvc(t)
	b, err := s.Get(context.Background(), alice, testKey)
	require.NoError(t, err)
	assert.Nil(t, b)
}

func TestPutGetRoundTripIsByteExact(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	body := []byte("{\n  \"version\": 4, \"serial\": 7,\n  \"z_key\": 1.10, \"a\": \"\\u00e9\",\n \"lineage\": \"l\"\n}\n")
	sum := md5.Sum(body)

	v, err := s.Put(ctx, alice, testKey, "", body, sum[:])
	require.NoError(t, err)
	assert.Equal(t, int64(7), v.Serial)
	assert.Nil(t, v.Content)

	got, err := s.Get(ctx, alice, testKey)
	require.NoError(t, err)
	assert.Equal(t, body, got.Body)
	assert.Equal(t, sum[:], got.MD5)
	assert.Equal(t, v.ID, got.VersionID)
}

func TestPutValidation(t *testing.T) {
	s, _ := newSvc(t, func(o *Options) { o.MaxStateBytes = 200 })
	ctx := context.Background()
	wrong := make([]byte, 16)

	_, err := s.Put(ctx, alice, "nope", "", stateBody(1), nil)
	require.ErrorIs(t, err, store.ErrInvalidKey)
	_, err = s.Put(ctx, alice, testKey, "", stateBody(1), wrong)
	require.ErrorIs(t, err, store.ErrMD5Mismatch)
	_, err = s.Put(ctx, alice, testKey, "", []byte(strings.Repeat("x", 201)), nil)
	require.ErrorIs(t, err, store.ErrTooLarge)

	for _, bad := range []string{
		`not json`, `[1]`,
		`{"serial":1,"lineage":"l"}`, `{"version":4,"lineage":"l"}`, `{"version":4,"serial":1}`,
		`{"version":4,"serial":"x","lineage":"l"}`,
		`{"version":4,"serial":1,"lineage":"` + strings.Repeat("l", 65) + `"}`,
	} {
		_, err = s.Put(ctx, alice, testKey, "", []byte(bad), nil)
		require.ErrorIs(t, err, store.ErrInvalidState, bad)
	}
}

func TestEncryptedOpenTofuStateIsAccepted(t *testing.T) {
	s, _ := newSvc(t)
	body := []byte(`{"serial":3,"lineage":"l","meta":{},"encrypted_data":"abc=","encryption_version":"v0"}`)
	v, err := s.Put(context.Background(), alice, testKey, "", body, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(3), v.Serial)
}

func TestIdempotentPut(t *testing.T) {
	s, st := newSvc(t)
	ctx := context.Background()
	first, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	second, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID, "no new version")

	vs, err := st.ListVersions(ctx, testKey, 0)
	require.NoError(t, err)
	assert.Len(t, vs, 1)
	s.FlushAudit()
	assert.Contains(t, actions(st), store.ActionPutNoop)
}

func actions(st *memory.Store) []string {
	var out []string
	for _, e := range st.Audit() {
		out = append(out, e.Action)
	}
	return out
}

func results(st *memory.Store, action string) []string {
	var out []string
	for _, e := range st.Audit() {
		if e.Action == action {
			out = append(out, e.Result)
		}
	}
	return out
}

func auditFor(st *memory.Store, action string) *store.AuditEvent {
	for _, e := range st.Audit() {
		if e.Action == action {
			return &e
		}
	}
	return nil
}

func TestPutFence(t *testing.T) {
	ctx := context.Background()

	t.Run("lock held, ID missing", func(t *testing.T) {
		s, _ := newSvc(t)
		_, err := s.Lock(ctx, alice, httpLock("L1"))
		require.NoError(t, err)
		_, err = s.Put(ctx, alice, testKey, "", stateBody(1), nil)
		var locked *store.ErrLocked
		require.ErrorAs(t, err, &locked)
		assert.Equal(t, "L1", locked.Current.LockID)
	})
	t.Run("lock held, ID different", func(t *testing.T) {
		s, _ := newSvc(t)
		_, err := s.Lock(ctx, alice, httpLock("L1"))
		require.NoError(t, err)
		_, err = s.Put(ctx, alice, testKey, "L2", stateBody(1), nil)
		var locked *store.ErrLocked
		require.ErrorAs(t, err, &locked)
	})
	t.Run("lock held, ID matches", func(t *testing.T) {
		s, _ := newSvc(t)
		_, err := s.Lock(ctx, alice, httpLock("L1"))
		require.NoError(t, err)
		_, err = s.Put(ctx, alice, testKey, "L1", stateBody(1), nil)
		require.NoError(t, err)
	})
	t.Run("no lock, ID supplied", func(t *testing.T) {
		s, _ := newSvc(t)
		_, err := s.Put(ctx, alice, testKey, "L1", stateBody(1), nil)
		require.ErrorIs(t, err, store.ErrLockNotHeld)
	})
	t.Run("no lock, lock required", func(t *testing.T) {
		s, _ := newSvc(t, func(o *Options) { o.RequireLockForWrite = true })
		_, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
		require.ErrorIs(t, err, store.ErrLockRequired)
	})
}

// Runs the same scenario through both caller styles, checking G7: a lock taken
// through one frontend blocks the other's writes.
func TestFrontendsBlockEachOther(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t)

	// HTTP lock blocks a PSS-style caller, whose lock attempt and write both fail.
	_, err := s.Lock(ctx, alice, httpLock("http-lock"))
	require.NoError(t, err)
	_, err = s.Lock(ctx, alice, pssLock())
	var locked *store.ErrLocked
	require.ErrorAs(t, err, &locked)
	assert.Equal(t, "http-lock", locked.Current.LockID)
	_, err = s.Put(ctx, alice, testKey, "held-in-provider-process", stateBody(1), nil)
	require.ErrorAs(t, err, &locked)
	_, err = s.Unlock(ctx, alice, testKey, "http-lock", false)
	require.NoError(t, err)

	// PSS lock generates its own ID and blocks an HTTP-style caller.
	pss, err := s.Lock(ctx, alice, pssLock())
	require.NoError(t, err)
	require.NotEmpty(t, pss.LockID)
	_, err = s.Lock(ctx, alice, httpLock("http-2"))
	require.ErrorAs(t, err, &locked)

	info := map[string]any{}
	require.NoError(t, json.Unmarshal(LockInfoJSON(locked.Current), &info))
	assert.Equal(t, pss.LockID, info["ID"])
	assert.Equal(t, "held via snowstate state_store", info["Info"])
	assert.Equal(t, "carol@laptop", info["Who"])

	_, err = s.Put(ctx, alice, testKey, "http-2", stateBody(2), nil)
	require.ErrorAs(t, err, &locked)
	_, err = s.Put(ctx, alice, testKey, pss.LockID, stateBody(2), nil)
	require.NoError(t, err)
	_, err = s.Unlock(ctx, alice, testKey, pss.LockID, false)
	require.NoError(t, err)
}

func TestLockSemantics(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t, func(o *Options) { o.AcquiredBy = "github:o/r#1/1" })

	l, err := s.Lock(ctx, alice, httpLock("L1"))
	require.NoError(t, err)
	assert.Equal(t, "github:o/r#1/1", l.AcquiredBy)

	again, err := s.Lock(ctx, alice, httpLock("L1"))
	require.NoError(t, err, "re-lock with the same ID is idempotent")
	assert.Equal(t, "L1", again.LockID)

	_, err = s.Lock(ctx, alice, httpLock("L2"))
	var locked *store.ErrLocked
	require.ErrorAs(t, err, &locked)
	assert.Equal(t, locked.Current.RawJSON, LockInfoJSON(locked.Current), "HTTP locks return their raw JSON")
	assert.Contains(t, locked.Error(), "bob@ci")

	bad := httpLock("")
	_, err = s.Lock(ctx, alice, bad)
	require.ErrorIs(t, err, store.ErrInvalidLock)
	_, err = s.Lock(ctx, alice, httpLock(strings.Repeat("x", 129)))
	require.ErrorIs(t, err, store.ErrInvalidLock)
	big := httpLock("L9")
	big.RawJSON = make([]byte, 65537)
	_, err = s.Lock(ctx, alice, big)
	require.ErrorIs(t, err, store.ErrInvalidLock)

	_, err = s.Lock(ctx, alice, LockRequest{Key: "bad", Source: store.SourcePSS})
	require.ErrorIs(t, err, store.ErrInvalidKey)

	s.FlushAudit()
	assert.Contains(t, results(st, store.ActionLock), store.ResultConflict)
}

func TestLockColumnsAreTruncatedToFit(t *testing.T) {
	s, _ := newSvc(t)
	req := httpLock("L1")
	req.Who = strings.Repeat("é", 400) // 800 bytes
	l, err := s.Lock(context.Background(), alice, req)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(l.Who), 512)
	assert.True(t, strings.HasSuffix(l.Who, "é"), "must not split a rune")
}

func TestConcurrentLockHasOneWinner(t *testing.T) {
	s, _ := newSvc(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Lock(context.Background(), alice, httpLock(fmt.Sprintf("L%d", i))); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), wins.Load())
}

func TestUnlockSemantics(t *testing.T) {
	ctx := context.Background()

	t.Run("matching ID", func(t *testing.T) {
		s, st := newSvc(t)
		_, _ = s.Lock(ctx, alice, httpLock("L1"))
		removed, err := s.Unlock(ctx, alice, testKey, "L1", false)
		require.NoError(t, err)
		assert.Equal(t, "L1", removed.LockID)
		cur, _ := st.GetLock(ctx, testKey)
		assert.Nil(t, cur)
		assert.Contains(t, actions(st), store.ActionUnlock)
	})
	t.Run("different ID", func(t *testing.T) {
		s, st := newSvc(t)
		_, _ = s.Lock(ctx, alice, httpLock("L1"))
		_, err := s.Unlock(ctx, alice, testKey, "L2", false)
		var mismatch *store.ErrLockMismatch
		require.ErrorAs(t, err, &mismatch)
		assert.Equal(t, "L1", mismatch.Current.LockID)
		assert.Contains(t, mismatch.Error(), "L1")
		cur, _ := st.GetLock(ctx, testKey)
		assert.NotNil(t, cur, "lock survives")
	})
	t.Run("nothing held", func(t *testing.T) {
		s, _ := newSvc(t)
		removed, err := s.Unlock(ctx, alice, testKey, "L1", false)
		require.NoError(t, err)
		assert.Nil(t, removed)
	})
	t.Run("force", func(t *testing.T) {
		s, st := newSvc(t)
		_, _ = s.Lock(ctx, alice, httpLock("L1"))
		_, err := s.Unlock(ctx, alice, testKey, "", true)
		require.NoError(t, err)
		e := auditFor(st, store.ActionForceUnlock)
		require.NotNil(t, e)
		assert.Contains(t, e.Detail["removed_lock"], `"ID":"L1"`)
	})
	t.Run("force with ID only removes that lock", func(t *testing.T) {
		s, st := newSvc(t)
		_, _ = s.Lock(ctx, alice, httpLock("L1"))
		_, err := s.Unlock(ctx, alice, testKey, "other", true)
		var mismatch *store.ErrLockMismatch
		require.ErrorAs(t, err, &mismatch)
		_, err = s.Unlock(ctx, alice, testKey, "L1", true)
		require.NoError(t, err)
		assert.NotNil(t, auditFor(st, store.ActionForceUnlock))
	})
	t.Run("force disabled", func(t *testing.T) {
		s, _ := newSvc(t, func(o *Options) { o.AllowForceUnlock = false })
		_, _ = s.Lock(ctx, alice, httpLock("L1"))
		_, err := s.Unlock(ctx, alice, testKey, "", true)
		require.ErrorIs(t, err, store.ErrForbidden)
	})
}

func TestLockTTLSteal(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t, func(o *Options) { o.LockTTL = time.Millisecond })
	_, err := s.Lock(ctx, alice, httpLock("old"))
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	l, err := s.Lock(ctx, alice, httpLock("new"))
	require.NoError(t, err)
	assert.Equal(t, "new", l.LockID)
	e := auditFor(st, store.ActionForceUnlock)
	require.NotNil(t, e)
	assert.Equal(t, "ttl", e.Detail["reason"])
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t)

	require.NoError(t, s.Delete(ctx, alice, testKey, ""), "missing state is idempotent")

	_, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)

	_, _ = s.Lock(ctx, alice, httpLock("L1"))
	var locked *store.ErrLocked
	require.ErrorAs(t, s.Delete(ctx, alice, testKey, ""), &locked)
	require.ErrorAs(t, s.Delete(ctx, alice, testKey, "L2"), &locked)
	require.NoError(t, s.Delete(ctx, alice, testKey, "L1"))
	_, _ = s.Unlock(ctx, alice, testKey, "L1", false)

	got, err := s.Get(ctx, alice, testKey)
	require.NoError(t, err)
	assert.Nil(t, got, "soft deleted reads as no state")

	vs, _ := st.ListVersions(ctx, testKey, 0)
	assert.Len(t, vs, 1, "versions are kept")

	// Writing identical bytes after a delete must revive the state.
	_, err = s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	got, err = s.Get(ctx, alice, testKey)
	require.NoError(t, err)
	assert.NotNil(t, got)
}

func TestSerialGuard(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t, func(o *Options) { o.RejectSerialRegression = true })
	_, err := s.Put(ctx, alice, testKey, "", stateBody(5), nil)
	require.NoError(t, err)
	_, err = s.Put(ctx, alice, testKey, "", stateBody(4), nil)
	require.ErrorIs(t, err, store.ErrSerialRegression)

	other := []byte(`{"version":4,"serial":1,"lineage":"line-2"}`)
	_, err = s.Put(ctx, alice, testKey, "", other, nil)
	require.NoError(t, err, "a lineage change is not a regression")

	off, _ := newSvc(t)
	_, err = off.Put(ctx, alice, testKey, "", stateBody(5), nil)
	require.NoError(t, err)
	_, err = off.Put(ctx, alice, testKey, "", stateBody(4), nil)
	require.NoError(t, err, "the guard is off by default")
}

func TestStageStorageAndOrphanCleanup(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t, func(o *Options) { o.InlineMaxBytes = 1 })

	v, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	assert.Equal(t, store.StorageStage, v.Storage)
	got, err := s.Get(ctx, alice, testKey)
	require.NoError(t, err)
	assert.Equal(t, stateBody(1), got.Body)

	// A no-op write uploads, then removes the duplicate.
	_, err = s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	blobs, _ := st.ListBlobs(ctx)
	assert.Len(t, blobs, 1)

	// A rejected write leaves no blob behind.
	_, _ = s.Lock(ctx, alice, httpLock("L1"))
	_, err = s.Put(ctx, alice, testKey, "", stateBody(2), nil)
	require.Error(t, err)
	blobs, _ = st.ListBlobs(ctx)
	assert.Len(t, blobs, 1)

	st.FailBlobs(errors.New("stage down"))
	_, err = s.Put(ctx, alice, testKey, "L1", stateBody(2), nil)
	require.ErrorContains(t, err, "upload blob")
}

func TestWriteIsAtomicOnFailure(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	s, st := newSvc(t, func(o *Options) { o.AfterInsertVersion = func() error { return boom } })

	_, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.ErrorIs(t, err, boom)

	m, _ := st.GetState(ctx, testKey)
	assert.Nil(t, m, "no current version")
	vs, _ := st.ListVersions(ctx, testKey, 0)
	assert.Empty(t, vs)
	assert.NotContains(t, results(st, store.ActionPut), store.ResultOK, "no audit row committed")
}

func TestCorruptStoredBytesAreDetected(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t)
	_, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	m, _ := st.GetState(ctx, testKey)

	gz, err := gzipBytes([]byte(`{"tampered":true}`))
	require.NoError(t, err)
	v, err := st.GetVersion(ctx, testKey, m.CurrentVersionID)
	require.NoError(t, err)
	v.Content = gz
	bad, err := s.load(ctx, v)
	require.ErrorContains(t, err, "MD5")
	assert.Nil(t, bad)

	v.Content = []byte("not gzip")
	_, err = s.load(ctx, v)
	require.ErrorContains(t, err, "gunzip")
}

type commitUnknown struct {
	store.Store
	landed bool
}

func (c *commitUnknown) InTx(ctx context.Context, key string, fn func(store.Tx) error) error {
	if c.landed {
		if err := c.Store.InTx(ctx, key, fn); err != nil {
			return err
		}
	}
	return store.ErrCommitUnknown
}

func TestCommitOutcomeUnknownIsResolvedByRereading(t *testing.T) {
	ctx := context.Background()

	landed := New(Options{Store: &commitUnknown{Store: memory.MustNew(), landed: true}, InlineMaxBytes: 1 << 20})
	defer landed.Close()
	v, err := landed.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	assert.NotEmpty(t, v.ID)

	lost := New(Options{Store: &commitUnknown{Store: memory.MustNew()}, InlineMaxBytes: 1 << 20})
	defer lost.Close()
	_, err = lost.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.ErrorIs(t, err, store.ErrUnavailable)
}

func TestAuditCarriesRequestContext(t *testing.T) {
	s, st := newSvc(t, func(o *Options) { o.AuditDetail = map[string]any{"run_id": "42"} })
	ctx := store.WithRequestMeta(context.Background(), store.RequestMeta{RequestID: "r1", RemoteAddr: "10.0.0.1"})
	_, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)

	e := auditFor(st, store.ActionPut)
	require.NotNil(t, e)
	assert.Equal(t, "r1", e.RequestID)
	assert.Equal(t, "10.0.0.1", e.RemoteAddr)
	assert.Equal(t, "alice", e.Principal)
	assert.Equal(t, "42", e.Detail["run_id"])
	assert.Equal(t, 200, e.HTTPStatus)

	sum := md5.Sum(stateBody(1))
	assert.Equal(t, hex.EncodeToString(sum[:]), mustState(t, st).MD5)
}

func mustState(t *testing.T, st *memory.Store) *store.StateMeta {
	t.Helper()
	m, err := st.GetState(context.Background(), testKey)
	require.NoError(t, err)
	return m
}

func TestReadsAreAuditedOnlyWhenConfigured(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t)
	_, _ = s.Get(ctx, alice, testKey)
	s.FlushAudit()
	assert.Contains(t, actions(st), store.ActionGet)

	off, st2 := newSvc(t, func(o *Options) { o.LogReads = false })
	_, _ = off.Get(ctx, alice, testKey)
	off.FlushAudit()
	assert.NotContains(t, actions(st2), store.ActionGet)
}

type denyAll struct{}

func (denyAll) Authorize(context.Context, Principal, string, string) error { return store.ErrForbidden }

func TestAuthorizerIsConsultedOnEveryOperation(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t, func(o *Options) { o.Authorizer = denyAll{} })

	_, err := s.Get(ctx, alice, testKey)
	require.ErrorIs(t, err, store.ErrForbidden)
	_, err = s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.ErrorIs(t, err, store.ErrForbidden)
	require.ErrorIs(t, s.Delete(ctx, alice, testKey, ""), store.ErrForbidden)
	_, err = s.Lock(ctx, alice, httpLock("L1"))
	require.ErrorIs(t, err, store.ErrForbidden)
	_, err = s.Unlock(ctx, alice, testKey, "L1", false)
	require.ErrorIs(t, err, store.ErrForbidden)
	_, err = s.ListWorkspaces(ctx, alice, "platform/net")
	require.ErrorIs(t, err, store.ErrForbidden)

	s.FlushAudit()
	e := auditFor(st, store.ActionPut)
	require.NotNil(t, e)
	assert.Equal(t, store.ResultDenied, e.Result)
	assert.Equal(t, 403, e.HTTPStatus)
}

func TestListWorkspaces(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t)

	got, err := s.ListWorkspaces(ctx, alice, "platform/net")
	require.NoError(t, err)
	assert.Equal(t, []string{"default"}, got, "default exists even with no state")

	for _, k := range []string{"platform/net/prod", "platform/net/dev", "platform/net/eu/prod", "platform/netx/prod"} {
		_, err := s.Put(ctx, alice, k, "", stateBody(1), nil)
		require.NoError(t, err)
	}
	require.NoError(t, s.Delete(ctx, alice, "platform/net/dev", ""))

	got, err = s.ListWorkspaces(ctx, alice, "platform/net")
	require.NoError(t, err)
	assert.Equal(t, []string{"default", "prod"}, got)

	_, err = s.Put(ctx, alice, "platform/net/default", "", stateBody(1), nil)
	require.NoError(t, err)
	got, err = s.ListWorkspaces(ctx, alice, "platform/net")
	require.NoError(t, err)
	assert.Equal(t, []string{"default", "prod"}, got)

	_, err = s.ListWorkspaces(ctx, alice, "bad//prefix")
	require.ErrorIs(t, err, store.ErrInvalidKey)
}

func TestAuditBufferFlushesOnSizeAndSurvivesFailure(t *testing.T) {
	st := &flakyAudit{Store: memory.MustNew()}
	st.fail.Store(true)
	s := New(Options{Store: st, InlineMaxBytes: 1 << 20, LogReads: true})
	for range 3 {
		_, _ = s.Get(context.Background(), alice, testKey)
	}
	s.FlushAudit()
	assert.Equal(t, 3, s.AuditDepth(), "rows are retained when a flush fails")

	st.fail.Store(false)
	for range 100 {
		_, _ = s.Get(context.Background(), alice, testKey)
	}
	require.Eventually(t, func() bool { return s.AuditDepth() == 0 }, 2*time.Second, 5*time.Millisecond)
	s.Close()
	assert.Len(t, st.Store.(*memory.Store).Audit(), 103)
}

type flakyAudit struct {
	store.Store
	fail atomic.Bool
}

func (f *flakyAudit) AppendAudit(ctx context.Context, e []store.AuditEvent) error {
	if f.fail.Load() {
		return errors.New("down")
	}
	return f.Store.AppendAudit(ctx, e)
}

func TestLargeBodyRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t)
	blob := make([]byte, 1<<20)
	_, _ = rand.Read(blob)
	body := []byte(`{"version":4,"serial":1,"lineage":"l","pad":"` + hex.EncodeToString(blob) + `"}`)
	_, err := s.Put(ctx, alice, testKey, "", body, nil)
	require.NoError(t, err)
	got, err := s.Get(ctx, alice, testKey)
	require.NoError(t, err)
	assert.Equal(t, md5.Sum(body), md5.Sum(got.Body))
}
