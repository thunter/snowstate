package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/pkg/snowstate/store"
	"github.com/thunter/snowstate/pkg/snowstate/store/memory"
)

func TestListStatesSplitsPrefixAndWorkspace(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t)
	for _, k := range []string{"platform/net/prod", "platform/net/dev", "other/x/default"} {
		_, err := s.Put(ctx, alice, k, "", stateBody(1), nil)
		require.NoError(t, err)
	}
	all, err := s.ListStates(ctx, alice, "")
	require.NoError(t, err)
	assert.Len(t, all, 3)

	some, err := s.ListStates(ctx, alice, "platform/net")
	require.NoError(t, err)
	require.Len(t, some, 2)
	assert.Equal(t, "platform/net", some[0].Prefix)
	assert.Equal(t, "dev", some[0].Workspace)
}

type onlyProject struct{ project string }

func (o onlyProject) Authorize(_ context.Context, _ Principal, action, key string) error {
	if action == ActionAdmin && (key == "" || len(key) > len(o.project) && key[:len(o.project)+1] == o.project+"/") {
		return nil
	}
	return store.ErrForbidden
}

func TestAdminListsAreFilteredByKey(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t, func(o *Options) { o.Authorizer = onlyProject{"platform"} })
	seed := New(Options{Store: st, InlineMaxBytes: 1 << 20})
	defer seed.Close()
	for _, k := range []string{"platform/a/b", "secret/a/b"} {
		_, err := seed.Put(ctx, alice, k, "", stateBody(1), nil)
		require.NoError(t, err)
		_, err = seed.Lock(ctx, alice, LockRequest{Key: k, Source: store.SourcePSS})
		require.NoError(t, err)
	}
	states, err := s.ListStates(ctx, alice, "")
	require.NoError(t, err)
	require.Len(t, states, 1)
	assert.Equal(t, "platform/a/b", states[0].Key)

	locks, err := s.ListLocks(ctx, alice)
	require.NoError(t, err)
	require.Len(t, locks, 1)
	assert.Equal(t, "platform/a/b", locks[0].Key)

	_, err = s.ListVersions(ctx, alice, "secret/a/b", 0)
	require.ErrorIs(t, err, store.ErrForbidden)
}

func TestVersionsAndGetVersion(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t)
	v1, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	v2, err := s.Put(ctx, alice, testKey, "", stateBody(2), nil)
	require.NoError(t, err)

	vs, err := s.ListVersions(ctx, alice, testKey, 0)
	require.NoError(t, err)
	require.Len(t, vs, 2)
	assert.Equal(t, v2.ID, vs[0].ID, "newest first")

	limited, err := s.ListVersions(ctx, alice, testKey, 1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)

	got, err := s.GetVersion(ctx, alice, testKey, v1.ID)
	require.NoError(t, err)
	assert.Equal(t, stateBody(1), got.Body)

	_, err = s.GetVersion(ctx, alice, testKey, "missing")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.ListVersions(ctx, alice, "bad", 0)
	require.ErrorIs(t, err, store.ErrInvalidKey)
	_, err = s.GetVersion(ctx, alice, "bad", "x")
	require.ErrorIs(t, err, store.ErrInvalidKey)
}

func TestRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	for _, inline := range []int64{8 << 20, 1} {
		t.Run(fmt.Sprintf("inline=%d", inline), func(t *testing.T) {
			s, st := newSvc(t, func(o *Options) { o.InlineMaxBytes = inline; o.RejectSerialRegression = true })
			v1, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
			require.NoError(t, err)
			_, err = s.Put(ctx, alice, testKey, "", stateBody(2), nil)
			require.NoError(t, err)

			restored, err := s.Restore(ctx, alice, testKey, v1.ID, false)
			require.NoError(t, err)
			assert.NotEqual(t, v1.ID, restored.ID, "a NEW version")
			assert.Equal(t, v1.ID, restored.RestoredFrom)

			got, err := s.Get(ctx, alice, testKey)
			require.NoError(t, err)
			assert.Equal(t, stateBody(1), got.Body)
			assert.Equal(t, restored.ID, got.VersionID)

			vs, _ := st.ListVersions(ctx, testKey, 0)
			assert.Len(t, vs, 3, "history is kept")
			e := auditFor(st, store.ActionRestore)
			require.NotNil(t, e)
			assert.Equal(t, v1.ID, e.Detail["restored_from"])
		})
	}
}

func TestRestoreRefusesWhileLocked(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t)
	v1, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	_, err = s.Put(ctx, alice, testKey, "", stateBody(2), nil)
	require.NoError(t, err)
	_, err = s.Lock(ctx, alice, httpLock("L1"))
	require.NoError(t, err)

	_, err = s.Restore(ctx, alice, testKey, v1.ID, false)
	var locked *store.ErrLocked
	require.ErrorAs(t, err, &locked)

	_, err = s.Restore(ctx, alice, testKey, v1.ID, true)
	require.NoError(t, err)
}

func TestRestoreCurrentContentIsNoop(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t)
	v1, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	v, err := s.Restore(ctx, alice, testKey, v1.ID, false)
	require.NoError(t, err)
	assert.Equal(t, v1.ID, v.ID)
	vs, _ := st.ListVersions(ctx, testKey, 0)
	assert.Len(t, vs, 1)
}

func TestRestoreRevivesDeletedState(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t)
	v1, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	require.NoError(t, s.Delete(ctx, alice, testKey, ""))
	_, err = s.Restore(ctx, alice, testKey, v1.ID, false)
	require.NoError(t, err)
	got, err := s.Get(ctx, alice, testKey)
	require.NoError(t, err)
	assert.NotNil(t, got)
}

func TestReleaseLockWorksWithForceUnlockDisabled(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t, func(o *Options) { o.AllowForceUnlock = false })
	_, err := s.Lock(ctx, alice, httpLock("L1"))
	require.NoError(t, err)

	removed, err := s.ReleaseLock(ctx, alice, testKey)
	require.NoError(t, err)
	assert.Equal(t, "L1", removed.LockID)
	assert.NotNil(t, auditFor(st, store.ActionForceUnlock))

	_, err = s.ReleaseLock(ctx, alice, "bad")
	require.ErrorIs(t, err, store.ErrInvalidKey)
}

func TestGCKeepsNewestVersionsAndCurrent(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t, func(o *Options) {
		o.Retention = Retention{KeepVersions: 2}
		o.InlineMaxBytes = 1
	})
	var last *Version
	for i := 1; i <= 5; i++ {
		var err error
		last, err = s.Put(ctx, alice, testKey, "", stateBody(i), nil)
		require.NoError(t, err)
	}

	dry, err := s.GC(ctx, alice, true)
	require.NoError(t, err)
	assert.Equal(t, 3, dry.VersionsRemoved)
	vs, _ := st.ListVersions(ctx, testKey, 0)
	assert.Len(t, vs, 5, "a dry run changes nothing")

	r, err := s.GC(ctx, alice, false)
	require.NoError(t, err)
	assert.Equal(t, 3, r.VersionsRemoved)
	vs, _ = st.ListVersions(ctx, testKey, 0)
	assert.Len(t, vs, 2)
	assert.Equal(t, last.ID, vs[0].ID)
	blobs, _ := st.ListBlobs(ctx)
	assert.Len(t, blobs, 2, "blobs of removed versions go too")

	got, err := s.Get(ctx, alice, testKey)
	require.NoError(t, err)
	assert.Equal(t, stateBody(5), got.Body)
	s.FlushAudit()
	assert.Contains(t, actions(st), store.ActionGC)
}

func TestGCNeverDropsCurrentAfterRestore(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t, func(o *Options) { o.Retention = Retention{KeepVersions: 1} })
	_, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	_, err = s.GC(ctx, alice, false)
	require.NoError(t, err)
	got, err := s.Get(ctx, alice, testKey)
	require.NoError(t, err)
	assert.NotNil(t, got)
}

func TestGCKeepDaysProtectsRecentVersions(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t, func(o *Options) { o.Retention = Retention{KeepVersions: 1, KeepDays: 7} })
	for i := 1; i <= 3; i++ {
		_, err := s.Put(ctx, alice, testKey, "", stateBody(i), nil)
		require.NoError(t, err)
	}
	r, err := s.GC(ctx, alice, false)
	require.NoError(t, err)
	assert.Zero(t, r.VersionsRemoved)
	vs, _ := st.ListVersions(ctx, testKey, 0)
	assert.Len(t, vs, 3)
}

func TestGCUnlimitedWhenKeepVersionsIsZero(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t, func(o *Options) { o.Retention = Retention{KeepVersions: 0} })
	for i := 1; i <= 3; i++ {
		_, err := s.Put(ctx, alice, testKey, "", stateBody(i), nil)
		require.NoError(t, err)
	}
	r, err := s.GC(ctx, alice, false)
	require.NoError(t, err)
	assert.Zero(t, r.VersionsRemoved)
}

func TestGCPurgesDeletedStatesPastRetention(t *testing.T) {
	ctx := context.Background()
	s, st := newSvc(t, func(o *Options) { o.Retention = Retention{DeletedDays: 0} })
	_, err := s.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	require.NoError(t, s.Delete(ctx, alice, testKey, ""))

	r, err := s.GC(ctx, alice, false)
	require.NoError(t, err)
	assert.Equal(t, 1, r.StatesPurged)
	m, _ := st.GetState(ctx, testKey)
	assert.Nil(t, m)
	vs, _ := st.ListVersions(ctx, testKey, 0)
	assert.Empty(t, vs)
}

func TestGCLeavesRecentlyDeletedAndLockedStates(t *testing.T) {
	ctx := context.Background()
	recent, st := newSvc(t, func(o *Options) { o.Retention = Retention{DeletedDays: 30} })
	_, err := recent.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	require.NoError(t, recent.Delete(ctx, alice, testKey, ""))
	r, err := recent.GC(ctx, alice, false)
	require.NoError(t, err)
	assert.Zero(t, r.StatesPurged)
	m, _ := st.GetState(ctx, testKey)
	assert.NotNil(t, m)

	locked, _ := newSvc(t, func(o *Options) { o.Retention = Retention{DeletedDays: 0} })
	_, err = locked.Put(ctx, alice, testKey, "", stateBody(1), nil)
	require.NoError(t, err)
	require.NoError(t, locked.Delete(ctx, alice, testKey, ""))
	_, err = locked.Lock(ctx, alice, httpLock("L1"))
	require.NoError(t, err)
	r, err = locked.GC(ctx, alice, false)
	require.NoError(t, err)
	assert.Zero(t, r.StatesPurged, "a locked state is left alone")
}

func TestGCSweepsOrphansOnlyWhenOldEnough(t *testing.T) {
	ctx := context.Background()
	st := memory.MustNew()
	_, err := st.PutBlob(ctx, testKey, "orphan", []byte("x"))
	require.NoError(t, err)

	young := New(Options{Store: st, InlineMaxBytes: 1 << 20})
	defer young.Close()
	r, err := young.GC(ctx, alice, false)
	require.NoError(t, err)
	assert.Zero(t, r.BlobsRemoved, "in-flight uploads are protected by age")

	s := New(Options{Store: st, InlineMaxBytes: 1 << 20, Retention: Retention{OrphanMinAge: -1}})
	defer s.Close()
	dry, err := s.GC(ctx, alice, true)
	require.NoError(t, err)
	assert.Equal(t, 1, dry.BlobsRemoved)
	blobs, _ := st.ListBlobs(ctx)
	assert.Len(t, blobs, 1)

	r, err = s.GC(ctx, alice, false)
	require.NoError(t, err)
	assert.Equal(t, 1, r.BlobsRemoved)
	blobs, _ = st.ListBlobs(ctx)
	assert.Empty(t, blobs)
}

type failList struct{ store.Store }

func (failList) ListStates(context.Context, store.ListOptions) ([]store.StateMeta, error) {
	return nil, errors.New("down")
}

func TestAdminErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	s := New(Options{Store: failList{memory.MustNew()}})
	defer s.Close()
	_, err := s.ListStates(ctx, alice, "")
	require.Error(t, err)
	_, err = s.GC(ctx, alice, false)
	require.Error(t, err)

	denied, _ := newSvc(t, func(o *Options) { o.Authorizer = denyAll{} })
	_, err = denied.ListStates(ctx, alice, "")
	require.ErrorIs(t, err, store.ErrForbidden)
	_, err = denied.ListLocks(ctx, alice)
	require.ErrorIs(t, err, store.ErrForbidden)
	_, err = denied.GC(ctx, alice, false)
	require.ErrorIs(t, err, store.ErrForbidden)
	_, err = denied.Restore(ctx, alice, testKey, "x", false)
	require.ErrorIs(t, err, store.ErrForbidden)
	_, err = denied.ReleaseLock(ctx, alice, testKey)
	require.ErrorIs(t, err, store.ErrForbidden)
	_, err = denied.GetVersion(ctx, alice, testKey, "x")
	require.ErrorIs(t, err, store.ErrForbidden)
}
