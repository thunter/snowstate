//go:build integration

// Package integration runs against a real Snowflake account (SPEC §12.3). It
// needs the SNOWSTATE_IT_* variables and skips itself when they are unset.
// Each run works in a throwaway schema BACKEND_IT_<random>.
package integration

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/md5"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/pkg/snowstate/ids"
	"github.com/thunter/snowstate/pkg/snowstate/service"
	"github.com/thunter/snowstate/pkg/snowstate/sfconn"
	"github.com/thunter/snowstate/pkg/snowstate/store"
	"github.com/thunter/snowstate/pkg/snowstate/store/snowflake"
)

var (
	schema string
	admin  *sql.DB
	alice  = service.Principal{Name: "alice", Source: "snowflake"}
)

func connConfig(schemaName string) sfconn.Config {
	return sfconn.Config{
		Account:              os.Getenv("SNOWSTATE_IT_ACCOUNT"),
		User:                 os.Getenv("SNOWSTATE_IT_USER"),
		Role:                 os.Getenv("SNOWSTATE_IT_ROLE"),
		Warehouse:            os.Getenv("SNOWSTATE_IT_WAREHOUSE"),
		Database:             os.Getenv("SNOWSTATE_IT_DATABASE"),
		Schema:               schemaName,
		Auth:                 os.Getenv("SNOWSTATE_IT_AUTH"),
		PrivateKeyPath:       os.Getenv("SNOWSTATE_IT_PRIVATE_KEY_PATH"),
		PrivateKeyPassphrase: os.Getenv("SNOWSTATE_IT_PRIVATE_KEY_PASSPHRASE"),
		QueryTag:             "snowstate_it",
		LockTimeout:          30 * time.Second,
		Pool:                 sfconn.Pool{MaxOpen: 30, MaxIdle: 30, ConnMaxLifetime: time.Hour},
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("SNOWSTATE_IT_ACCOUNT") == "" {
		fmt.Println("SNOWSTATE_IT_* not set, skipping integration tests")
		os.Exit(0)
	}
	schema = "BACKEND_IT_" + strings.ToUpper(strings.ReplaceAll(ids.V4(), "-", "")[:12])
	ctx := context.Background()
	var err error
	admin, err = sfconn.Open(ctx, connConfig(""))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	db := os.Getenv("SNOWSTATE_IT_DATABASE")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+db+"."+schema); err != nil {
		fmt.Fprintln(os.Stderr, "create schema:", err)
		os.Exit(1)
	}
	code := m.Run()
	if _, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+db+"."+schema); err != nil {
		fmt.Fprintln(os.Stderr, "drop schema:", err)
		code = 1
	}
	_ = admin.Close()
	os.Exit(code)
}

func newStore(t *testing.T) *snowflake.Store {
	t.Helper()
	db, err := sfconn.Open(context.Background(), connConfig(schema))
	require.NoError(t, err)
	st := snowflake.New(snowflake.Options{
		DB: db, Database: os.Getenv("SNOWSTATE_IT_DATABASE"), Schema: schema, QueryTag: "snowstate_it",
	})
	t.Cleanup(func() { _ = st.Close() })
	_, err = st.Migrate(context.Background(), false)
	require.NoError(t, err)
	return st
}

func newService(t *testing.T, st store.Store, mutate ...func(*service.Options)) *service.Service {
	t.Helper()
	opts := service.Options{Store: st, InlineMaxBytes: 8 << 20, AllowForceUnlock: true, LogReads: true}
	for _, m := range mutate {
		m(&opts)
	}
	s := service.New(opts)
	t.Cleanup(s.Close)
	return s
}

func key(t *testing.T) string {
	t.Helper()
	return "it/" + strings.ReplaceAll(t.Name(), "/", "-") + "/" + ids.V4()[:8]
}

func body(serial int) []byte {
	return []byte(fmt.Sprintf(`{"version":4,"terraform_version":"1.9.0","serial":%d,"lineage":"l-1","resources":[{"mode":"managed","type":"aws_s3_bucket","name":"b","provider":"provider[\"x\"]","instances":[{"attributes":{"secret":"nope"}}]}]}`, serial))
}

func TestMigrationsAreIdempotent(t *testing.T) {
	st := newStore(t)
	res, err := st.Migrate(context.Background(), false)
	require.NoError(t, err)
	assert.Empty(t, res.Applied, "second run does nothing")

	dry, err := st.Migrate(context.Background(), true)
	require.NoError(t, err)
	assert.Empty(t, dry.Applied)

	applied, latest, err := st.SchemaVersion(context.Background())
	require.NoError(t, err)
	assert.Equal(t, latest, applied)

	hybrid, err := st.LocksAreHybrid(context.Background())
	require.NoError(t, err)
	assert.True(t, hybrid)

	supported, err := st.HybridSupported(context.Background())
	require.NoError(t, err)
	assert.True(t, supported)
}

func storageCases() []struct {
	name   string
	inline int64
	want   string
} {
	return []struct {
		name   string
		inline int64
		want   string
	}{{"inline", 8 << 20, store.StorageInline}, {"stage", 1, store.StorageStage}}
}

func TestRoundTripInlineAndStage(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, tc := range storageCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t, st, func(o *service.Options) { o.InlineMaxBytes = tc.inline })
			k := key(t)

			got, err := s.Get(ctx, alice, k)
			require.NoError(t, err)
			assert.Nil(t, got)

			v1, err := s.Put(ctx, alice, k, "", body(1), nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, v1.Storage)

			got, err = s.Get(ctx, alice, k)
			require.NoError(t, err)
			assert.Equal(t, body(1), got.Body)
			assert.Equal(t, v1.ID, got.VersionID)

			noop, err := s.Put(ctx, alice, k, "", body(1), nil)
			require.NoError(t, err)
			assert.Equal(t, v1.ID, noop.ID, "identical bytes make no version")

			v2, err := s.Put(ctx, alice, k, "", body(2), nil)
			require.NoError(t, err)
			assert.NotEqual(t, v1.ID, v2.ID)

			vs, err := st.ListVersions(ctx, k, 0)
			require.NoError(t, err)
			require.Len(t, vs, 2)
			assert.Equal(t, v2.ID, vs[0].ID, "newest first")

			old, err := st.GetVersion(ctx, k, v1.ID)
			require.NoError(t, err)
			assert.Equal(t, int64(1), old.Serial)

			_, err = st.GetVersion(ctx, k, "missing")
			require.ErrorIs(t, err, store.ErrNotFound)

			require.NoError(t, s.Delete(ctx, alice, k, ""))
			got, err = s.Get(ctx, alice, k)
			require.NoError(t, err)
			assert.Nil(t, got, "soft deleted")

			meta, err := st.GetState(ctx, k)
			require.NoError(t, err)
			assert.True(t, meta.Deleted())
			assert.NotEmpty(t, meta.CurrentVersionID)

			listed, err := st.ListStates(ctx, store.ListOptions{Prefix: k})
			require.NoError(t, err)
			assert.Empty(t, listed)
			listed, err = st.ListStates(ctx, store.ListOptions{Prefix: k, IncludeDeleted: true})
			require.NoError(t, err)
			assert.Len(t, listed, 1)
		})
	}
}

// §6.3 VERIFY: the largest inline payload must bind. This body deflates to just
// under the default inline_max_bytes of 8 MiB.
func TestInlineMaxBinds(t *testing.T) {
	st := newStore(t)
	s := newService(t, st)
	ctx := context.Background()
	k := key(t)

	build := func(n int) []byte {
		blob := make([]byte, n)
		_, err := rand.Read(blob)
		require.NoError(t, err)
		return []byte(`{"version":4,"serial":1,"lineage":"l","pad":"` + hex.EncodeToString(blob) + `"}`)
	}
	const target = 8<<20 - 64<<10
	n := 4 << 20
	n = n * target / deflatedSize(t, build(n))
	large := build(n)
	size := deflatedSize(t, large)
	require.LessOrEqual(t, size, 8<<20)
	require.Greater(t, size, 7<<20)
	t.Logf("inline payload: %d bytes", size)

	v, err := s.Put(ctx, alice, k, "", large, nil)
	require.NoError(t, err)
	assert.Equal(t, store.StorageInline, v.Storage)

	got, err := s.Get(ctx, alice, k)
	require.NoError(t, err)
	assert.Equal(t, md5.Sum(large), md5.Sum(got.Body))
}

func deflatedSize(t *testing.T, body []byte) int {
	t.Helper()
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	require.NoError(t, err)
	_, err = w.Write(body)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Len()
}

func TestLongestKey(t *testing.T) {
	st := newStore(t)
	s := newService(t, st)
	ctx := context.Background()
	k := "it/" + strings.Repeat("k", 509)
	require.Len(t, k, 512)
	_ = s.Delete(ctx, alice, k, "")
	lock, err := s.Lock(ctx, alice, service.LockRequest{Key: k, Who: "w", Source: store.SourcePSS})
	require.NoError(t, err)
	_, err = s.Unlock(ctx, alice, k, lock.LockID, false)
	require.NoError(t, err)
}

func httpLock(k, id string) service.LockRequest {
	return service.LockRequest{
		Key: k, LockID: id, Who: "w@h", Operation: "OperationTypeApply", Source: store.SourceHTTP,
		RawJSON: []byte(`{"ID":"` + id + `","Who":"w@h","Operation":"OperationTypeApply"}`),
	}
}

func TestLockLifecycle(t *testing.T) {
	st := newStore(t)
	s := newService(t, st)
	ctx := context.Background()
	k := key(t)

	l, err := s.Lock(ctx, alice, httpLock(k, "L1"))
	require.NoError(t, err)
	assert.Equal(t, "alice", l.AcquiredBy)

	again, err := s.Lock(ctx, alice, httpLock(k, "L1"))
	require.NoError(t, err, "same ID is idempotent")
	assert.Equal(t, l.LockID, again.LockID)

	_, err = s.Lock(ctx, alice, httpLock(k, "L2"))
	var locked *store.ErrLocked
	require.ErrorAs(t, err, &locked)
	assert.Equal(t, `{"ID":"L1","Who":"w@h","Operation":"OperationTypeApply"}`, string(service.LockInfoJSON(locked.Current)))

	_, err = s.Put(ctx, alice, k, "", body(1), nil)
	require.ErrorAs(t, err, &locked, "fenced without an ID")
	_, err = s.Put(ctx, alice, k, "L1", body(1), nil)
	require.NoError(t, err)

	listed, err := st.ListLocks(ctx)
	require.NoError(t, err)
	found := false
	for _, x := range listed {
		found = found || x.Key == k
	}
	assert.True(t, found)

	_, err = s.Unlock(ctx, alice, k, "L2", false)
	var mismatch *store.ErrLockMismatch
	require.ErrorAs(t, err, &mismatch)

	_, err = s.Unlock(ctx, alice, k, "L1", false)
	require.NoError(t, err)
	cur, err := st.GetLock(ctx, k)
	require.NoError(t, err)
	assert.Nil(t, cur)

	_, err = s.Put(ctx, alice, k, "L1", body(2), nil)
	require.ErrorIs(t, err, store.ErrLockNotHeld)

	_, err = s.Lock(ctx, alice, httpLock(k, "L3"))
	require.NoError(t, err)
	removed, err := s.Unlock(ctx, alice, k, "", true)
	require.NoError(t, err)
	assert.Equal(t, "L3", removed.LockID)
}

func TestProviderStyleLocksBlockHTTP(t *testing.T) {
	st := newStore(t)
	s := newService(t, st)
	ctx := context.Background()
	k := key(t)

	pss, err := s.Lock(ctx, alice, service.LockRequest{Key: k, Who: "c@l", Operation: "apply", Source: store.SourcePSS})
	require.NoError(t, err)
	cur, err := st.GetLock(ctx, k)
	require.NoError(t, err)
	assert.Empty(t, cur.RawJSON)
	assert.Contains(t, string(service.LockInfoJSON(cur)), "held via snowstate state_store")

	_, err = s.Lock(ctx, alice, httpLock(k, "H"))
	var locked *store.ErrLocked
	require.ErrorAs(t, err, &locked)
	_, err = s.Put(ctx, alice, k, pss.LockID, body(1), nil)
	require.NoError(t, err)
}

// Release blocker for hybrid mode (§12.3): 25 goroutines on separate
// connections race for one key, 10 rounds, exactly one winner each round.
func TestConcurrentLockHasExactlyOneWinner(t *testing.T) {
	st := newStore(t)
	s := newService(t, st)
	ctx := context.Background()
	k := key(t)

	for round := range 10 {
		var wins atomic.Int32
		var wrong atomic.Int32
		var firstErr atomic.Value
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := range 25 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := s.Lock(ctx, alice, httpLock(k, fmt.Sprintf("r%d-g%d", round, g)))
				var locked *store.ErrLocked
				switch {
				case err == nil:
					wins.Add(1)
				case errors.As(err, &locked):
				default:
					wrong.Add(1)
					firstErr.Store(err.Error())
				}
			}()
		}
		close(start)
		wg.Wait()
		require.Equal(t, int32(0), wrong.Load(), "round %d: unexpected error %v", round, firstErr.Load())
		require.Equal(t, int32(1), wins.Load(), "round %d", round)
		_, err := s.Unlock(ctx, alice, k, "", true)
		require.NoError(t, err)
	}
}

func TestWriteTransactionIsAtomic(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	k := key(t)
	boom := errors.New("injected between steps 3 and 4")
	s := newService(t, st, func(o *service.Options) { o.AfterInsertVersion = func() error { return boom } })

	_, err := s.Put(ctx, alice, k, "", body(1), nil)
	require.ErrorIs(t, err, boom)

	meta, err := st.GetState(ctx, k)
	require.NoError(t, err)
	assert.Nil(t, meta, "no visible current version")

	vs, err := st.ListVersions(ctx, k, 0)
	require.NoError(t, err)
	assert.Empty(t, vs, "version row rolled back")

	var ok int
	require.NoError(t, admin.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM %s.%s.AUDIT_LOG WHERE STATE_KEY = ? AND RESULT = 'OK'",
		os.Getenv("SNOWSTATE_IT_DATABASE"), schema), k).Scan(&ok))
	assert.Zero(t, ok, "no audit row")
}

func TestAuditRowsAreWritten(t *testing.T) {
	st := newStore(t)
	s := newService(t, st, func(o *service.Options) { o.AuditDetail = map[string]any{"run_id": "42"} })
	ctx := store.WithRequestMeta(context.Background(), store.RequestMeta{RequestID: "req-1", RemoteAddr: "10.0.0.1"})
	k := key(t)

	_, err := s.Lock(ctx, alice, httpLock(k, "L1"))
	require.NoError(t, err)
	_, err = s.Put(ctx, alice, k, "L1", body(1), nil)
	require.NoError(t, err)
	_, err = s.Get(ctx, alice, k)
	require.NoError(t, err)
	_, err = s.Unlock(ctx, alice, k, "", true)
	require.NoError(t, err)
	s.FlushAudit()

	rows, err := admin.QueryContext(ctx, fmt.Sprintf(
		`SELECT ACTION, DETAIL:run_id::string, REQUEST_ID FROM %s.%s.AUDIT_LOG WHERE STATE_KEY = ?`,
		os.Getenv("SNOWSTATE_IT_DATABASE"), schema), k)
	require.NoError(t, err)
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var action, run, req string
		require.NoError(t, rows.Scan(&action, &run, &req))
		assert.Equal(t, "42", run)
		assert.Equal(t, "req-1", req)
		seen[action] = true
	}
	require.NoError(t, rows.Err())
	for _, want := range []string{"LOCK", "PUT", "GET", "FORCE_UNLOCK"} {
		assert.True(t, seen[want], want)
	}
}

func TestInventoryView(t *testing.T) {
	st := newStore(t)
	s := newService(t, st)
	ctx := context.Background()
	k := key(t)
	_, err := s.Put(ctx, alice, k, "", body(1), nil)
	require.NoError(t, err)

	var rType, name, mode string
	var count int
	require.NoError(t, admin.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT RESOURCE_TYPE, RESOURCE_NAME, MODE, INSTANCE_COUNT FROM %s.%s.STATE_RESOURCES WHERE STATE_KEY = ?",
		os.Getenv("SNOWSTATE_IT_DATABASE"), schema), k).Scan(&rType, &name, &mode, &count))
	assert.Equal(t, "aws_s3_bucket", rType)
	assert.Equal(t, "b", name)
	assert.Equal(t, "managed", mode)
	assert.Equal(t, 1, count)
}

// A state too large for DECOMPRESS_STRING must not break the view for everyone.
func TestInventoryViewSurvivesLargeState(t *testing.T) {
	st := newStore(t)
	s := newService(t, st)
	ctx := context.Background()
	small, big := key(t), key(t)
	_, err := s.Put(ctx, alice, small, "", body(1), nil)
	require.NoError(t, err)

	large := []byte(`{"version":4,"serial":1,"lineage":"l","resources":[{"mode":"managed","type":"big","name":"` +
		strings.Repeat("x", 120<<20) + `","instances":[]}]}`)
	v, err := s.Put(ctx, alice, big, "", large, nil)
	require.NoError(t, err)
	require.Equal(t, store.StorageInline, v.Storage, "this compressible body stays inline")

	got, err := s.Get(ctx, alice, big)
	require.NoError(t, err)
	assert.Equal(t, md5.Sum(large), md5.Sum(got.Body))

	db := os.Getenv("SNOWSTATE_IT_DATABASE")
	var n int
	require.NoError(t, admin.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM %s.%s.STATE_RESOURCES WHERE STATE_KEY = ?", db, schema), small).Scan(&n))
	assert.Equal(t, 1, n, "the small state is still listed")
	require.NoError(t, admin.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM %s.%s.STATE_RESOURCES WHERE STATE_KEY = ?", db, schema), big).Scan(&n))
	t.Logf("large state rows in view: %d", n)
}

func TestTokens(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	id := ids.V4()
	exp := time.Now().Add(time.Hour).UTC()
	tok := &store.Token{ID: id, Principal: "ci", HMAC: strings.Repeat("a", 60) + id[:4],
		Scopes: []store.Scope{{KeyPattern: "platform/*", Permissions: []string{"read", "lock"}}}, ExpiresAt: &exp}
	require.NoError(t, st.CreateToken(ctx, tok))

	got, err := st.GetTokenByHMAC(ctx, tok.HMAC)
	require.NoError(t, err)
	assert.Equal(t, tok.Scopes, got.Scopes)
	assert.WithinDuration(t, exp, *got.ExpiresAt, time.Second)
	assert.Nil(t, got.RevokedAt)

	require.NoError(t, st.TouchToken(ctx, id, time.Now()))
	require.NoError(t, st.RevokeToken(ctx, id))
	got, err = st.GetTokenByHMAC(ctx, tok.HMAC)
	require.NoError(t, err)
	assert.NotNil(t, got.RevokedAt)
	assert.NotNil(t, got.LastUsedAt)

	all, err := st.ListTokens(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, all)

	require.ErrorIs(t, st.RevokeToken(ctx, "missing"), store.ErrNotFound)
	_, err = st.GetTokenByHMAC(ctx, "missing")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestBlobPrimitives(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	k := key(t)
	p, err := st.PutBlob(ctx, k, "v1", []byte("\x1f\x8bnot really gzip"))
	require.NoError(t, err)
	got, err := st.GetBlob(ctx, k, p)
	require.NoError(t, err)
	assert.Equal(t, []byte("\x1f\x8bnot really gzip"), got)

	blobs, err := st.ListBlobs(ctx)
	require.NoError(t, err)
	found := false
	for _, b := range blobs {
		found = found || b.Path == p
	}
	assert.True(t, found, "listed %v", blobs)

	require.NoError(t, st.RemoveBlob(ctx, k, p))
	_, err = st.GetBlob(ctx, k, p)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestRestoreRoundTrip(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, tc := range storageCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t, st, func(o *service.Options) { o.InlineMaxBytes = tc.inline })
			k := key(t)
			v1, err := s.Put(ctx, alice, k, "", body(1), nil)
			require.NoError(t, err)
			_, err = s.Put(ctx, alice, k, "", body(2), nil)
			require.NoError(t, err)

			restored, err := s.Restore(ctx, alice, k, v1.ID, false)
			require.NoError(t, err)
			assert.Equal(t, v1.ID, restored.RestoredFrom)

			got, err := s.Get(ctx, alice, k)
			require.NoError(t, err)
			assert.Equal(t, body(1), got.Body)
			assert.Equal(t, restored.ID, got.VersionID)

			vs, err := st.ListVersions(ctx, k, 0)
			require.NoError(t, err)
			assert.Len(t, vs, 3)
			assert.Equal(t, v1.ID, vs[0].RestoredFrom)
		})
	}
}

func TestGCOnSnowflake(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	k := key(t)
	// A negative OrphanMinAge skips the age check: this test owns its orphan.
	s := newService(t, st, func(o *service.Options) {
		o.InlineMaxBytes = 1
		o.Retention = service.Retention{KeepVersions: 1, OrphanMinAge: -1}
	})
	v1, err := s.Put(ctx, alice, k, "", body(1), nil)
	require.NoError(t, err)
	v2, err := s.Put(ctx, alice, k, "", body(2), nil)
	require.NoError(t, err)
	old, err := st.GetVersion(ctx, k, v1.ID)
	require.NoError(t, err)

	orphan, err := st.PutBlob(ctx, k, "orphan", []byte("\x1f\x8b"))
	require.NoError(t, err)

	dry, err := s.GC(ctx, alice, true)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, dry.VersionsRemoved, 1)
	assert.GreaterOrEqual(t, dry.BlobsRemoved, 1)

	_, err = s.GC(ctx, alice, false)
	require.NoError(t, err)

	vs, err := st.ListVersions(ctx, k, 0)
	require.NoError(t, err)
	require.Len(t, vs, 1)
	assert.Equal(t, v2.ID, vs[0].ID)

	_, err = st.GetBlob(ctx, k, old.StagePath)
	require.ErrorIs(t, err, store.ErrNotFound, "the removed version's blob is gone")
	_, err = st.GetBlob(ctx, k, orphan)
	require.ErrorIs(t, err, store.ErrNotFound, "the orphan is gone")

	got, err := s.Get(ctx, alice, k)
	require.NoError(t, err)
	assert.Equal(t, body(2), got.Body)

	blobs, err := st.ListBlobs(ctx)
	require.NoError(t, err)
	for _, b := range blobs {
		if b.Path == vs[0].StagePath {
			assert.False(t, b.LastModified.IsZero(), "LIST timestamps must parse, or orphans never age out")
		}
	}

	require.NoError(t, s.Delete(ctx, alice, k, ""))
	purge := newService(t, st, func(o *service.Options) { o.Retention = service.Retention{DeletedDays: 0} })
	r, err := purge.GC(ctx, alice, false)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, r.StatesPurged, 1)
	meta, err := st.GetState(ctx, k)
	require.NoError(t, err)
	assert.Nil(t, meta)
}

// Routing in schema_per_project, without any grants. The role-based isolation
// checks are in TestSchemaPerProjectIsolation.
func TestSchemaPerProjectRouting(t *testing.T) {
	ctx := context.Background()
	db, err := sfconn.Open(ctx, connConfig(""))
	require.NoError(t, err)
	suffix := strings.ToUpper(ids.V4()[:8])
	control := "BACKEND_IT_CTL_" + suffix
	st := snowflake.New(snowflake.Options{
		DB: db, Database: os.Getenv("SNOWSTATE_IT_DATABASE"), Schema: "UNUSED", ControlSchema: control,
		Tenancy: snowflake.TenancySchemaPerProject, QueryTag: "snowstate_it",
	})
	t.Cleanup(func() { _ = st.Close() })

	projA, projB := "it"+suffix+"a", "it"+suffix+"b"
	t.Cleanup(func() {
		for _, schema := range []string{control, snowflake.ProjectSchema(projA), snowflake.ProjectSchema(projB)} {
			_, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+os.Getenv("SNOWSTATE_IT_DATABASE")+"."+schema)
			assert.NoError(t, err)
		}
	})

	res, err := st.Migrate(ctx, false)
	require.NoError(t, err)
	assert.NotEmpty(t, res.Applied)

	for _, name := range []string{projA, projB} {
		_, err := st.CreateProject(ctx, snowflake.ProjectRequest{Name: name})
		require.NoError(t, err)
	}
	projects, err := st.ListProjects(ctx)
	require.NoError(t, err)
	assert.Len(t, projects, 2)

	s := newService(t, st)
	keyA, keyB := projA+"/net/prod", projB+"/net/prod"
	_, err = s.Put(ctx, alice, keyA, "", body(1), nil)
	require.NoError(t, err)
	_, err = s.Put(ctx, alice, keyB, "", body(2), nil)
	require.NoError(t, err)

	got, err := s.Get(ctx, alice, keyA)
	require.NoError(t, err)
	assert.Equal(t, body(1), got.Body)

	var n int
	dbName := os.Getenv("SNOWSTATE_IT_DATABASE")
	require.NoError(t, admin.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM %s.%s.STATES", dbName, snowflake.ProjectSchema(projA))).Scan(&n))
	assert.Equal(t, 1, n, "each schema holds only its own project")

	all, err := st.ListStates(ctx, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, all, 2, "listing spans projects")

	_, err = s.Lock(ctx, alice, service.LockRequest{Key: keyB, Source: store.SourcePSS})
	require.NoError(t, err)
	locks, err := st.ListLocks(ctx)
	require.NoError(t, err)
	assert.Len(t, locks, 1)

	require.NoError(t, admin.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM %s.%s.STATE_RESOURCES", dbName, control)).Scan(&n))
	assert.Equal(t, 2, n, "the union view spans projects")

	// "<name>_" and "<name>." sanitise to the same schema, which must be refused.
	underscored, dotted := projA+"_", projA+"."
	t.Cleanup(func() {
		_, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+dbName+"."+snowflake.ProjectSchema(underscored))
		assert.NoError(t, err)
	})
	_, err = st.CreateProject(ctx, snowflake.ProjectRequest{Name: underscored})
	require.NoError(t, err)
	_, err = st.CreateProject(ctx, snowflake.ProjectRequest{Name: dotted})
	require.ErrorContains(t, err, "already used", "sanitising must not merge two projects")
}

// Role isolation (§12.3). It needs two pre-created roles, each granted to the
// test user with USAGE on the warehouse (dev_setup.md §3.2):
//
//	SNOWSTATE_IT_ROLE_A, SNOWSTATE_IT_ROLE_B
func TestSchemaPerProjectIsolation(t *testing.T) {
	roleA, roleB := os.Getenv("SNOWSTATE_IT_ROLE_A"), os.Getenv("SNOWSTATE_IT_ROLE_B")
	if roleA == "" || roleB == "" {
		t.Skip("set SNOWSTATE_IT_ROLE_A and SNOWSTATE_IT_ROLE_B to run the role isolation test")
	}
	ctx := context.Background()
	dbName := os.Getenv("SNOWSTATE_IT_DATABASE")
	suffix := strings.ToUpper(ids.V4()[:8])
	control := "BACKEND_IT_CTL_" + suffix
	projA, projB := "it"+suffix+"a", "it"+suffix+"b"

	open := func(role string) *snowflake.Store {
		cfg := connConfig("")
		cfg.Role = role
		db, err := sfconn.Open(ctx, cfg)
		require.NoError(t, err)
		st := snowflake.New(snowflake.Options{
			DB: db, Database: dbName, Schema: "UNUSED", ControlSchema: control,
			Tenancy: snowflake.TenancySchemaPerProject, QueryTag: "snowstate_it",
		})
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	owner := open(os.Getenv("SNOWSTATE_IT_ROLE"))
	t.Cleanup(func() {
		for _, schema := range []string{control, snowflake.ProjectSchema(projA), snowflake.ProjectSchema(projB)} {
			_, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+dbName+"."+schema)
			assert.NoError(t, err)
		}
	})
	_, err := owner.Migrate(ctx, false)
	require.NoError(t, err)
	perms := []string{"read", "write", "lock"}
	_, err = owner.CreateProject(ctx, snowflake.ProjectRequest{Name: projA, Role: roleA, Permissions: perms})
	require.NoError(t, err)
	_, err = owner.CreateProject(ctx, snowflake.ProjectRequest{Name: projB, Role: roleB, Permissions: perms})
	require.NoError(t, err)

	asA := newService(t, open(roleA))
	keyA, keyB := projA+"/net/prod", projB+"/net/prod"

	// Role A works in its own project, through a full write and read.
	lock, err := asA.Lock(ctx, alice, httpLock(keyA, "A1"))
	require.NoError(t, err)
	_, err = asA.Put(ctx, alice, keyA, lock.LockID, body(1), nil)
	require.NoError(t, err)
	_, err = asA.Get(ctx, alice, keyA)
	require.NoError(t, err)
	_, err = asA.Unlock(ctx, alice, keyA, lock.LockID, false)
	require.NoError(t, err)

	// Role A cannot read, write or lock project B.
	_, err = asA.Get(ctx, alice, keyB)
	require.ErrorIs(t, err, store.ErrForbidden)
	_, err = asA.Put(ctx, alice, keyB, "", body(1), nil)
	require.ErrorIs(t, err, store.ErrForbidden)
	_, err = asA.Lock(ctx, alice, httpLock(keyB, "A2"))
	require.ErrorIs(t, err, store.ErrForbidden)

	// History is append-only for project roles.
	cfg := connConfig("")
	cfg.Role = roleA
	raw, err := sfconn.Open(ctx, cfg)
	require.NoError(t, err)
	defer raw.Close()
	schemaA := dbName + "." + snowflake.ProjectSchema(projA)
	for _, stmt := range []string{
		"DELETE FROM " + schemaA + ".STATE_VERSIONS",
		"UPDATE " + schemaA + ".STATE_VERSIONS SET SERIAL = 0",
		"DELETE FROM " + schemaA + ".AUDIT_LOG",
		"UPDATE " + schemaA + ".AUDIT_LOG SET RESULT = 'X'",
	} {
		_, err := raw.ExecContext(ctx, stmt)
		require.Error(t, err, stmt)
	}
}

func standardStore(t *testing.T) *snowflake.Store {
	t.Helper()
	ctx := context.Background()
	name := "BACKEND_IT_STD_" + strings.ToUpper(ids.V4()[:8])
	dbName := os.Getenv("SNOWSTATE_IT_DATABASE")
	_, err := admin.ExecContext(ctx, "CREATE SCHEMA "+dbName+"."+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+dbName+"."+name)
		assert.NoError(t, err)
	})
	db, err := sfconn.Open(ctx, connConfig(name))
	require.NoError(t, err)
	st := snowflake.New(snowflake.Options{
		DB: db, Database: dbName, Schema: name, QueryTag: "snowstate_it", LockMode: snowflake.LockStandard,
	})
	t.Cleanup(func() { _ = st.Close() })
	res, err := st.Migrate(ctx, false)
	require.NoError(t, err)
	assert.True(t, res.HybridChecked)
	return st
}

func TestStandardModeLockLifecycle(t *testing.T) {
	st := standardStore(t)
	s := newService(t, st)
	ctx := context.Background()
	k := key(t)

	hybrid, err := st.LocksAreHybrid(ctx)
	require.NoError(t, err)
	assert.False(t, hybrid)

	for range 2 { // the second pass reuses the cleared row
		l, err := s.Lock(ctx, alice, httpLock(k, "L1"))
		require.NoError(t, err)
		assert.Equal(t, "L1", l.LockID)

		_, err = s.Lock(ctx, alice, httpLock(k, "L1"))
		require.NoError(t, err, "same ID is idempotent")

		_, err = s.Lock(ctx, alice, httpLock(k, "L2"))
		var locked *store.ErrLocked
		require.ErrorAs(t, err, &locked)

		_, err = s.Put(ctx, alice, k, "", body(1), nil)
		require.ErrorAs(t, err, &locked)

		_, err = s.Unlock(ctx, alice, k, "L2", false)
		var mismatch *store.ErrLockMismatch
		require.ErrorAs(t, err, &mismatch)

		removed, err := s.Unlock(ctx, alice, k, "L1", false)
		require.NoError(t, err)
		require.NotNil(t, removed)

		cur, err := st.GetLock(ctx, k)
		require.NoError(t, err)
		assert.Nil(t, cur, "a cleared row reads as no lock")
	}

	listed, err := st.ListLocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, listed, "cleared rows are not listed")
}

// raceRound runs 25 racers and returns winners and unexpected errors.
func raceRound(s *service.Service, k string, round int) (wins int32, errs []string) {
	ctx := context.Background()
	var won atomic.Int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Lock(ctx, alice, httpLock(k, fmt.Sprintf("r%d-g%d", round, g)))
			var locked *store.ErrLocked
			switch {
			case err == nil:
				won.Add(1)
			case !errors.As(err, &locked):
				mu.Lock()
				errs = append(errs, err.Error())
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	return won.Load(), errs
}

// §6.5 and §12.3: the result of this test is recorded in decisions.md. It
// reports winners per round rather than stopping, because the point is to
// measure. Two winners in any round means standard mode is unsafe.
func TestStandardModeConcurrency(t *testing.T) {
	st := standardStore(t)
	s := newService(t, st)
	ctx := context.Background()
	reused := key(t)

	unsafe := 0
	for round := range 10 {
		for name, k := range map[string]string{"reused": reused, "fresh": key(t) + fmt.Sprint(round)} {
			wins, errs := raceRound(s, k, round)
			t.Logf("round %d %s: winners=%d errors=%d", round, name, wins, len(errs))
			if len(errs) > 0 {
				t.Logf("  first error: %s", errs[0])
			}
			if wins > 1 {
				unsafe++
			}
			_, err := s.Unlock(ctx, alice, k, "", true)
			require.NoError(t, err)
		}
	}
	assert.Zero(t, unsafe, "rounds with more than one winner: standard mode is unsafe")
}
