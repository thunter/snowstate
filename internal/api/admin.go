package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thunter/snowstate/pkg/snowstate/service"
	"github.com/thunter/snowstate/pkg/snowstate/store"
)

const (
	adminStates = adminPrefix + "states"
	adminLocks  = adminPrefix + "locks"
)

type stateJSON struct {
	Key       string    `json:"key"`
	Prefix    string    `json:"prefix"`
	Workspace string    `json:"workspace"`
	VersionID string    `json:"version_id"`
	Serial    int64     `json:"serial"`
	Lineage   string    `json:"lineage"`
	TFVersion string    `json:"terraform_version"`
	MD5       string    `json:"md5"`
	SizeBytes int64     `json:"size_bytes"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

type versionJSON struct {
	ID           string    `json:"id"`
	Serial       int64     `json:"serial"`
	Lineage      string    `json:"lineage"`
	TFVersion    string    `json:"terraform_version"`
	MD5          string    `json:"md5"`
	SizeBytes    int64     `json:"size_bytes"`
	Storage      string    `json:"storage"`
	LockID       string    `json:"lock_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	CreatedBy    string    `json:"created_by"`
	RestoredFrom string    `json:"restored_from,omitempty"`
}

type lockJSON struct {
	Key        string    `json:"key"`
	ID         string    `json:"id"`
	Operation  string    `json:"operation"`
	Who        string    `json:"who"`
	Source     string    `json:"source"`
	AcquiredBy string    `json:"acquired_by"`
	AcquiredAt time.Time `json:"acquired_at"`
}

func versionToJSON(v *service.Version) versionJSON {
	return versionJSON{
		ID: v.ID, Serial: v.Serial, Lineage: v.Lineage, TFVersion: v.TFVersion, MD5: v.MD5,
		SizeBytes: v.SizeBytes, Storage: v.Storage, LockID: v.LockID, CreatedAt: v.CreatedAt,
		CreatedBy: v.CreatedBy, RestoredFrom: v.RestoredFrom,
	}
}

func (h *Handler) writeJSON(w http.ResponseWriter, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		h.log.Error("encoding response", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (h *Handler) admin(w http.ResponseWriter, r *http.Request, p service.Principal, info *reqInfo) {
	path := r.URL.Path
	switch {
	case path == adminStates:
		h.onlyGet(w, r, func() { h.adminListStates(w, r, p) })
	case strings.HasPrefix(path, adminStates+"/"):
		h.adminState(w, r, p, strings.TrimPrefix(path, adminStates+"/"), info)
	case path == adminLocks:
		h.onlyGet(w, r, func() { h.adminListLocks(w, r, p) })
	case strings.HasPrefix(path, adminLocks+"/"):
		key := strings.TrimPrefix(path, adminLocks+"/")
		info.key = key
		if r.Method != http.MethodDelete {
			h.notAllowed(w, r, http.MethodDelete)
			return
		}
		if _, err := h.opts.Service.ReleaseLock(r.Context(), p, key); err != nil {
			h.writeServiceError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		h.writeError(w, r, http.StatusNotFound, "not found")
	}
}

func (h *Handler) onlyGet(w http.ResponseWriter, r *http.Request, fn func()) {
	if r.Method != http.MethodGet {
		h.notAllowed(w, r, http.MethodGet)
		return
	}
	fn()
}

// adminState handles {key...}/versions, {key...}/versions/{id} and
// {key...}/restore. Matching from the right keeps keys with a segment named
// "versions" working.
func (h *Handler) adminState(w http.ResponseWriter, r *http.Request, p service.Principal, rest string, info *reqInfo) {
	if key, ok := strings.CutSuffix(rest, "/restore"); ok {
		info.key = key
		if r.Method != http.MethodPost {
			h.notAllowed(w, r, http.MethodPost)
			return
		}
		h.adminRestore(w, r, p, key)
		return
	}
	if key, ok := strings.CutSuffix(rest, "/versions"); ok {
		info.key = key
		h.onlyGet(w, r, func() { h.adminListVersions(w, r, p, key) })
		return
	}
	if i := strings.LastIndex(rest, "/versions/"); i > 0 {
		key, id := rest[:i], rest[i+len("/versions/"):]
		info.key = key
		h.onlyGet(w, r, func() { h.adminGetVersion(w, r, p, key, id, info) })
		return
	}
	h.writeError(w, r, http.StatusNotFound, "not found")
}

func (h *Handler) adminListStates(w http.ResponseWriter, r *http.Request, p service.Principal) {
	states, err := h.opts.Service.ListStates(r.Context(), p, r.URL.Query().Get("prefix"))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	out := make([]stateJSON, 0, len(states))
	for i := range states {
		s := &states[i]
		out = append(out, stateJSON{
			Key: s.Key, Prefix: s.Prefix, Workspace: s.Workspace, VersionID: s.CurrentVersionID,
			Serial: s.Serial, Lineage: s.Lineage, TFVersion: s.TFVersion, MD5: s.MD5,
			SizeBytes: s.SizeBytes, UpdatedAt: s.UpdatedAt, UpdatedBy: s.UpdatedBy,
		})
	}
	h.writeJSON(w, out)
}

func (h *Handler) adminListVersions(w http.ResponseWriter, r *http.Request, p service.Principal, key string) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			h.writeError(w, r, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	vs, err := h.opts.Service.ListVersions(r.Context(), p, key, limit)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	out := make([]versionJSON, 0, len(vs))
	for i := range vs {
		out = append(out, versionToJSON(&vs[i]))
	}
	h.writeJSON(w, out)
}

func (h *Handler) adminGetVersion(w http.ResponseWriter, r *http.Request, p service.Principal, key, id string, info *reqInfo) {
	blob, err := h.opts.Service.GetVersion(r.Context(), p, key, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	info.bytes = len(blob.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob.Body) //nolint:gosec // served as application/json
}

func (h *Handler) adminRestore(w http.ResponseWriter, r *http.Request, p service.Principal, key string) {
	var req struct {
		VersionID string `json:"version_id"`
		Force     bool   `json:"force"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxLockBody))
	if err == nil {
		err = json.Unmarshal(raw, &req)
	}
	if err != nil || req.VersionID == "" {
		h.writeError(w, r, http.StatusBadRequest, "body must be {\"version_id\":...}")
		return
	}
	v, err := h.opts.Service.Restore(r.Context(), p, key, req.VersionID, req.Force)
	if errors.Is(err, store.ErrNotFound) {
		h.writeError(w, r, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, versionToJSON(v))
}

func (h *Handler) adminListLocks(w http.ResponseWriter, r *http.Request, p service.Principal) {
	locks, err := h.opts.Service.ListLocks(r.Context(), p)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	out := make([]lockJSON, 0, len(locks))
	for i := range locks {
		l := &locks[i]
		out = append(out, lockJSON{
			Key: l.Key, ID: l.LockID, Operation: l.Operation, Who: l.Who,
			Source: l.Source, AcquiredBy: l.AcquiredBy, AcquiredAt: l.AcquiredAt,
		})
	}
	h.writeJSON(w, out)
}
