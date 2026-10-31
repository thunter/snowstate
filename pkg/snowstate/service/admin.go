package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/thunter/snowstate/pkg/snowstate/ids"
	"github.com/thunter/snowstate/pkg/snowstate/keys"
	"github.com/thunter/snowstate/pkg/snowstate/store"
)

// defaultOrphanMinAge keeps GC away from blobs whose metadata transaction may
// still be running, because blobs are uploaded first.
const defaultOrphanMinAge = time.Hour

// Retention configures GC.
type Retention struct {
	KeepVersions int
	KeepDays     int
	DeletedDays  int
	// OrphanMinAge overrides how old an unreferenced blob must be to go.
	OrphanMinAge time.Duration
}

// StateInfo is one row of a state listing.
type StateInfo struct {
	store.StateMeta
	Prefix    string
	Workspace string
}

// GCReport says what GC removed, or would remove in a dry run.
type GCReport struct {
	DryRun          bool
	VersionsRemoved int
	StatesPurged    int
	BlobsRemoved    int
}

// AdminService is the admin half of StateService.
type AdminService interface {
	Ping(ctx context.Context) error
	ListStates(ctx context.Context, p Principal, prefix string) ([]StateInfo, error)
	ListVersions(ctx context.Context, p Principal, key string, limit int) ([]Version, error)
	GetVersion(ctx context.Context, p Principal, key, id string) (*StateBlob, error)
	Restore(ctx context.Context, p Principal, key, id string, force bool) (*Version, error)
	ListLocks(ctx context.Context, p Principal) ([]Lock, error)
	ReleaseLock(ctx context.Context, p Principal, key string) (*Lock, error)
	GC(ctx context.Context, p Principal, dryRun bool) (*GCReport, error)
}

// Ping checks that storage is reachable.
func (s *Service) Ping(ctx context.Context) error { return s.st.Ping(ctx) }

func (s *Service) adminRead(ctx context.Context, p Principal, key string) {
	s.record(ctx, p, store.ActionAdminRead, key, "", "", nil)
}

// ListStates returns the states under prefix that the principal may administer.
func (s *Service) ListStates(ctx context.Context, p Principal, prefix string) ([]StateInfo, error) {
	if err := s.authorize(ctx, p, ActionAdmin, store.ActionAdminRead, ""); err != nil {
		return nil, err
	}
	states, err := s.st.ListStates(ctx, store.ListOptions{Prefix: prefix})
	if err != nil {
		return nil, err
	}
	out := make([]StateInfo, 0, len(states))
	for _, m := range states {
		if s.authz.Authorize(ctx, p, ActionAdmin, m.Key) != nil {
			continue
		}
		info := StateInfo{StateMeta: m}
		if pre, w, err := keys.Split(m.Key); err == nil {
			info.Prefix, info.Workspace = pre, w
		}
		out = append(out, info)
	}
	s.adminRead(ctx, p, "")
	return out, nil
}

// ListVersions returns a state's versions, newest first.
func (s *Service) ListVersions(ctx context.Context, p Principal, key string, limit int) ([]Version, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, p, ActionAdmin, store.ActionAdminRead, key); err != nil {
		return nil, err
	}
	vs, err := s.st.ListVersions(ctx, key, limit)
	if err != nil {
		return nil, err
	}
	s.adminRead(ctx, p, key)
	return vs, nil
}

// GetVersion returns the content of one version.
func (s *Service) GetVersion(ctx context.Context, p Principal, key, id string) (*StateBlob, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, p, ActionAdmin, store.ActionAdminRead, key); err != nil {
		return nil, err
	}
	v, err := s.st.GetVersion(ctx, key, id)
	if err != nil {
		return nil, err
	}
	blob, err := s.load(ctx, v)
	if err != nil {
		return nil, err
	}
	s.record(ctx, p, store.ActionAdminRead, key, id, "", nil)
	return blob, nil
}

// Restore makes a NEW version with the bytes of an old one. History is never
// rewritten. It refuses while a lock is held unless force is set.
func (s *Service) Restore(ctx context.Context, p Principal, key, id string, force bool) (*Version, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, p, ActionAdmin, store.ActionRestore, key); err != nil {
		return nil, err
	}
	old, err := s.st.GetVersion(ctx, key, id)
	if err != nil {
		return nil, err
	}
	blob, err := s.load(ctx, old)
	if err != nil {
		return nil, err
	}
	lock, err := s.st.GetLock(ctx, key)
	if err != nil {
		return nil, err
	}
	v := &store.Version{
		ID: ids.V7(), Key: key, Serial: old.Serial, Lineage: old.Lineage, TFVersion: old.TFVersion,
		MD5: old.MD5, SizeBytes: old.SizeBytes, CreatedBy: p.Name, RestoredFrom: old.ID,
	}
	if lock != nil && force {
		v.LockID = lock.LockID
	}
	if err := s.placeBlob(ctx, v, blob.Body); err != nil {
		return nil, err
	}
	result, err := s.commitVersion(ctx, p, v, store.ActionRestore, map[string]any{"restored_from": old.ID})
	if err != nil {
		s.removeOrphan(ctx, v, err)
		s.recordWriteFailure(ctx, p, store.ActionRestore, key, v.LockID, err)
		return nil, err
	}
	if result.ID != v.ID {
		s.removeOrphan(ctx, v, nil)
	}
	return result, nil
}

// ListLocks returns the locks the principal may administer.
func (s *Service) ListLocks(ctx context.Context, p Principal) ([]Lock, error) {
	if err := s.authorize(ctx, p, ActionAdmin, store.ActionAdminRead, ""); err != nil {
		return nil, err
	}
	locks, err := s.st.ListLocks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Lock, 0, len(locks))
	for i := range locks {
		if s.authz.Authorize(ctx, p, ActionAdmin, locks[i].Key) == nil {
			out = append(out, locks[i])
		}
	}
	s.adminRead(ctx, p, "")
	return out, nil
}

// ReleaseLock removes whatever lock is held. It needs admin rather than lock,
// and works even with locking.allow_force_unlock off, because operators need a
// way out of a stuck lock.
func (s *Service) ReleaseLock(ctx context.Context, p Principal, key string) (*Lock, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, p, ActionAdmin, store.ActionForceUnlock, key); err != nil {
		return nil, err
	}
	return s.removeLock(ctx, p, key, "", true)
}

// GC applies retention, purges long-deleted states and sweeps orphan blobs.
func (s *Service) GC(ctx context.Context, p Principal, dryRun bool) (*GCReport, error) {
	if err := s.authorize(ctx, p, ActionAdmin, store.ActionGC, ""); err != nil {
		return nil, err
	}
	states, err := s.st.ListStates(ctx, store.ListOptions{IncludeDeleted: true})
	if err != nil {
		return nil, err
	}
	report := &GCReport{DryRun: dryRun}
	referenced := map[string]bool{}
	for i := range states {
		if err := s.gcState(ctx, &states[i], dryRun, report, referenced); err != nil {
			return report, fmt.Errorf("gc %s: %w", states[i].Key, err)
		}
	}
	if err := s.sweepBlobs(ctx, dryRun, report, referenced); err != nil {
		return report, err
	}
	if !dryRun {
		s.record(ctx, p, store.ActionGC, "", "", "", nil)
	}
	return report, nil
}

func (s *Service) gcState(ctx context.Context, m *store.StateMeta, dryRun bool, r *GCReport, referenced map[string]bool) error {
	versions, err := s.st.ListVersions(ctx, m.Key, 0)
	if err != nil {
		return err
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].ID > versions[j].ID })

	purge, err := s.purgeable(ctx, m)
	if err != nil {
		return err
	}
	var drop []store.Version
	for i := range versions {
		v := &versions[i]
		if purge || s.expired(m, v, i) {
			drop = append(drop, *v)
			continue
		}
		if v.StagePath != "" {
			referenced[v.StagePath] = true
		}
	}
	r.VersionsRemoved += len(drop)
	if purge {
		r.StatesPurged++
	}
	if dryRun {
		return nil
	}

	// Pointer first, then rows, then blobs: a crash leaves only orphans, which
	// the next run sweeps, and never a row pointing at a missing blob.
	if purge {
		if err := s.st.PurgeState(ctx, m.Key); err != nil {
			return err
		}
	}
	dropIDs := make([]string, 0, len(drop))
	for i := range drop {
		dropIDs = append(dropIDs, drop[i].ID)
	}
	if err := s.st.DeleteVersions(ctx, m.Key, dropIDs); err != nil {
		return err
	}
	for i := range drop {
		if drop[i].StagePath == "" {
			continue
		}
		if err := s.st.RemoveBlob(ctx, m.Key, drop[i].StagePath); err != nil {
			s.log.Warn("blob left for the next gc", "state_key", m.Key, "version_id", drop[i].ID, "error", err)
		}
	}
	return nil
}

// purgeable reports whether a deleted state is past retention and unlocked.
func (s *Service) purgeable(ctx context.Context, m *store.StateMeta) (bool, error) {
	if !m.Deleted() {
		return false, nil
	}
	cutoff := time.Duration(s.opts.Retention.DeletedDays) * 24 * time.Hour
	if time.Since(*m.DeletedAt) < cutoff {
		return false, nil
	}
	lock, err := s.st.GetLock(ctx, m.Key)
	return lock == nil, err
}

// expired applies keep_versions and keep_days. The current version always
// stays, and a version must fail both rules to go.
func (s *Service) expired(m *store.StateMeta, v *store.Version, newestIndex int) bool {
	r := s.opts.Retention
	if v.ID == m.CurrentVersionID || r.KeepVersions == 0 || newestIndex < r.KeepVersions {
		return false
	}
	return r.KeepDays == 0 || time.Since(v.CreatedAt) > time.Duration(r.KeepDays)*24*time.Hour
}

func (s *Service) sweepBlobs(ctx context.Context, dryRun bool, r *GCReport, referenced map[string]bool) error {
	blobs, err := s.st.ListBlobs(ctx)
	if err != nil {
		return err
	}
	minAge := s.opts.Retention.OrphanMinAge
	if minAge == 0 {
		minAge = defaultOrphanMinAge
	}
	for i := range blobs {
		b := &blobs[i]
		if referenced[b.Path] {
			continue
		}
		if minAge > 0 && (b.LastModified.IsZero() || time.Since(b.LastModified) < minAge) {
			continue
		}
		r.BlobsRemoved++
		if dryRun {
			continue
		}
		if err := s.st.RemoveBlob(ctx, b.Key, b.Path); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}
