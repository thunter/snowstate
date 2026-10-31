// Package api is the HTTP frontend: routing, protocol encoding and middleware.
// Routing is manual because LOCK and UNLOCK are non-standard verbs.
package api

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/thunter/snowstate/internal/auth"
	"github.com/thunter/snowstate/internal/obs"
	"github.com/thunter/snowstate/pkg/snowstate/service"
	"github.com/thunter/snowstate/pkg/snowstate/store"
)

const (
	statesPrefix  = "/v1/states/"
	adminPrefix   = "/v1/admin/"
	lockSuffix    = "/lock"
	maxLockBody   = 1 << 20
	readyCacheTTL = 10 * time.Second
	retryAfter    = "2"
)

// errBadBody means the client stopped sending part way through a request.
var errBadBody = errors.New("bad request body")

// Options configures a Handler.
type Options struct {
	Service        service.StateService
	Auth           auth.Authenticator
	Metrics        *obs.Metrics
	Logger         *slog.Logger
	MaxStateBytes  int64
	RequestTimeout time.Duration
	TrustedProxies []*net.IPNet
	Limiter        *RateLimiter
	// ServeMetrics exposes /metrics here. It is off when metrics.listen is set.
	ServeMetrics bool
}

// Handler is the root http.Handler.
type Handler struct {
	opts Options
	log  *slog.Logger

	readyMu  sync.Mutex
	readyAt  time.Time
	readyErr error
}

// New returns a Handler.
func New(opts Options) *Handler {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = time.Minute
	}
	return &Handler{opts: opts, log: opts.Logger}
}

// reqInfo is filled in as a request moves through the handler, for the log line.
type reqInfo struct {
	principal string
	key       string
	bytes     int
	route     string
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := obs.NewRequestID()
	w := &statusWriter{ResponseWriter: rw}
	w.Header().Set("X-Request-Id", reqID)

	info := &reqInfo{route: "other"}
	queryIDs := &store.QueryIDs{}
	ctx, cancel := context.WithTimeout(r.Context(), h.opts.RequestTimeout)
	defer cancel()
	ctx = store.WithRequestMeta(ctx, store.RequestMeta{
		RequestID: reqID, RemoteAddr: h.clientIP(r), QueryIDs: queryIDs,
	})
	r = r.WithContext(ctx)

	defer func() {
		status := w.status
		if status == 0 {
			status = http.StatusOK
		}
		d := time.Since(start)
		if h.opts.Metrics != nil {
			h.opts.Metrics.ObserveRequest(r.Method, info.route, status, d)
		}
		h.log.Info("request", "request_id", reqID, "principal", info.principal, "method", r.Method,
			"state_key", info.key, "status", status, "duration_ms", d.Milliseconds(),
			"sf_query_ids", queryIDs.List(), "bytes", info.bytes)
	}()

	h.route(w, r, info)
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request, info *reqInfo) {
	path := r.URL.Path
	switch {
	case path == "/healthz":
		info.route = "healthz"
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	case path == "/readyz":
		info.route = "readyz"
		h.ready(w, r)
	case path == "/metrics" && h.opts.ServeMetrics && h.opts.Metrics != nil:
		info.route = "metrics"
		h.opts.Metrics.Handler().ServeHTTP(w, r)
	case strings.HasPrefix(path, statesPrefix):
		p, ok := h.authenticate(w, r, info)
		if ok {
			h.state(w, r, p, info)
		}
	case strings.HasPrefix(path, adminPrefix):
		info.route = "admin"
		p, ok := h.authenticate(w, r, info)
		if ok {
			h.admin(w, r, p, info)
		}
	default:
		h.writeError(w, r, http.StatusNotFound, "not found")
	}
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	h.readyMu.Lock()
	defer h.readyMu.Unlock()
	if time.Since(h.readyAt) >= readyCacheTTL {
		h.readyErr = h.opts.Service.Ping(r.Context())
		h.readyAt = time.Now()
	}
	if h.readyErr != nil {
		h.log.Warn("readiness check failed", "error", h.readyErr)
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, info *reqInfo) (service.Principal, bool) {
	ip := store.RequestMetaFrom(r.Context()).RemoteAddr
	if h.opts.Limiter != nil && !h.opts.Limiter.Allow(ip) {
		w.Header().Set("Retry-After", "60")
		h.writeError(w, r, http.StatusTooManyRequests, "too many failed authentication attempts")
		return service.Principal{}, false
	}
	p, err := h.opts.Auth.Authenticate(r)
	switch {
	case err == nil:
		info.principal = p.Name
		return p, true
	case errors.Is(err, auth.ErrUnauthenticated):
		if h.opts.Limiter != nil {
			h.opts.Limiter.Fail(ip)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="snowstate"`)
		h.writeError(w, r, http.StatusUnauthorized, "missing or invalid credentials")
	default:
		h.writeServiceError(w, r, err)
	}
	return service.Principal{}, false
}

// state dispatches /v1/states/{key...}.
func (h *Handler) state(w http.ResponseWriter, r *http.Request, p service.Principal, info *reqInfo) {
	key := strings.TrimPrefix(r.URL.Path, statesPrefix)
	lockRoute := false
	if k, ok := strings.CutSuffix(key, lockSuffix); ok {
		key, lockRoute = k, true
	}
	info.key = key

	if lockRoute {
		info.route = "state_lock"
		switch r.Method {
		case http.MethodPost, http.MethodPut, "LOCK":
			h.lock(w, r, p, key)
		case http.MethodDelete, "UNLOCK":
			h.unlock(w, r, p, key)
		default:
			h.notAllowed(w, r, "POST, PUT, LOCK, DELETE, UNLOCK")
		}
		return
	}

	info.route = "state"
	switch r.Method {
	case http.MethodGet:
		h.get(w, r, p, key, info)
	case http.MethodPost, http.MethodPut:
		h.put(w, r, p, key, info)
	case http.MethodDelete:
		if err := h.opts.Service.Delete(r.Context(), p, key, r.URL.Query().Get("ID")); err != nil {
			h.writeServiceError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "LOCK":
		h.lock(w, r, p, key)
	case "UNLOCK":
		h.unlock(w, r, p, key)
	default:
		h.notAllowed(w, r, "GET, POST, PUT, DELETE, LOCK, UNLOCK")
	}
}

func (h *Handler) notAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	h.writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, p service.Principal, key string, info *reqInfo) {
	blob, err := h.opts.Service.Get(r.Context(), p, key)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	if blob == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-MD5", base64.StdEncoding.EncodeToString(blob.MD5))
	w.Header().Set("ETag", `"`+blob.VersionID+`"`)
	w.WriteHeader(http.StatusOK)
	info.bytes = len(blob.Body)
	if _, err := w.Write(blob.Body); err != nil { //nolint:gosec // served as application/json
		h.log.Warn("short write of state body", "state_key", key, "error", err)
	}
}

// readBody streams the request through a limit, hashing as it reads.
func readBody(r *http.Request, limit int64) ([]byte, hash.Hash, error) {
	if r.ContentLength > limit {
		return nil, nil, store.ErrTooLarge
	}
	var buf bytes.Buffer
	if r.ContentLength > 0 {
		buf.Grow(int(r.ContentLength) + bytes.MinRead)
	}
	sum := md5.New()
	if _, err := buf.ReadFrom(io.TeeReader(io.LimitReader(r.Body, limit+1), sum)); err != nil {
		return nil, nil, fmt.Errorf("%w: reading body: %w", errBadBody, err)
	}
	if int64(buf.Len()) > limit {
		return nil, nil, store.ErrTooLarge
	}
	return buf.Bytes(), sum, nil
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request, p service.Principal, key string, info *reqInfo) {
	body, sum, err := readBody(r, h.opts.MaxStateBytes)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	info.bytes = len(body)

	var claimed []byte
	if raw := r.Header.Get("Content-MD5"); raw != "" {
		claimed, err = base64.StdEncoding.DecodeString(raw)
		if err != nil {
			h.writeError(w, r, http.StatusBadRequest, "Content-MD5 is not valid base64")
			return
		}
	}
	v, err := h.opts.Service.Put(r.Context(), p, key, r.URL.Query().Get("ID"), body, claimed)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.log.Debug("state written", "state_key", key, "version_id", v.ID, "serial", v.Serial,
		"md5", hex.EncodeToString(sum.Sum(nil)), "bytes", len(body))
	w.Header().Set("ETag", `"`+v.ID+`"`)
	w.WriteHeader(http.StatusOK)
}

// lockInfo is the subset of Terraform's LockInfo the server reads.
type lockInfo struct {
	ID        string
	Operation string
	Info      string
	Who       string
	Version   string
	Created   string
	Path      string
}

func (h *Handler) lock(w http.ResponseWriter, r *http.Request, p service.Principal, key string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxLockBody+1))
	if err != nil {
		h.writeServiceError(w, r, fmt.Errorf("%w: %w", errBadBody, err))
		return
	}
	if len(raw) > maxLockBody {
		h.writeError(w, r, http.StatusRequestEntityTooLarge, "lock body too large")
		return
	}
	var li lockInfo
	if err := json.Unmarshal(raw, &li); err != nil {
		h.writeError(w, r, http.StatusBadRequest, "lock body is not valid JSON")
		return
	}
	if li.ID == "" {
		h.writeError(w, r, http.StatusBadRequest, "lock body has no ID")
		return
	}
	created, _ := time.Parse(time.RFC3339Nano, li.Created) // a client value, informational only
	l, err := h.opts.Service.Lock(r.Context(), p, service.LockRequest{
		Key: key, Operation: li.Operation, Who: li.Who, Info: li.Info, TFVersion: li.Version,
		Path: li.Path, Created: created, LockID: li.ID, RawJSON: raw, Source: store.SourceHTTP,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeLock(w, http.StatusOK, l)
}

// unlock handles the three body shapes clients send. A normal unlock carries
// the full LockInfo. Terraform's force-unlock has an empty body. OpenTofu's
// force-unlock has only the ID. See decisions.md D1.
func (h *Handler) unlock(w http.ResponseWriter, r *http.Request, p service.Principal, key string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxLockBody+1))
	if err != nil {
		h.writeServiceError(w, r, fmt.Errorf("%w: %w", errBadBody, err))
		return
	}
	var li lockInfo
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &li); err != nil {
			h.writeError(w, r, http.StatusBadRequest, "unlock body is not valid JSON")
			return
		}
	}
	force := li.ID == "" || (li.Who == "" && li.Operation == "")
	if _, err := h.opts.Service.Unlock(r.Context(), p, key, li.ID, force); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeLock(w http.ResponseWriter, status int, l *service.Lock) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(service.LockInfoJSON(l))
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	body, _ := json.Marshal(map[string]string{
		"error": msg, "request_id": store.RequestMetaFrom(r.Context()).RequestID,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeServiceError maps typed errors to the protocol in §4.3.
func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var locked *store.ErrLocked
	var mismatch *store.ErrLockMismatch
	switch {
	case errors.As(err, &locked):
		writeLock(w, http.StatusLocked, locked.Current)
	case errors.As(err, &mismatch):
		writeLock(w, http.StatusConflict, mismatch.Current)
	case errors.Is(err, errBadBody):
		h.writeError(w, r, http.StatusBadRequest, "could not read request body")
	case errors.Is(err, store.ErrLockRequired):
		h.writeError(w, r, http.StatusLocked, "lock required")
	case errors.Is(err, store.ErrLockNotHeld):
		h.writeError(w, r, http.StatusConflict, "lock not held")
	case errors.Is(err, store.ErrSerialRegression):
		h.writeError(w, r, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalidKey), errors.Is(err, store.ErrInvalidState),
		errors.Is(err, store.ErrInvalidLock):
		h.writeError(w, r, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrMD5Mismatch):
		h.writeError(w, r, http.StatusBadRequest, "Content-MD5 does not match the body")
	case errors.Is(err, store.ErrTooLarge):
		h.writeError(w, r, http.StatusRequestEntityTooLarge, "state exceeds limits.max_state_bytes")
	case errors.Is(err, store.ErrForbidden):
		h.writeError(w, r, http.StatusForbidden, "forbidden")
	case errors.Is(err, store.ErrNotFound):
		h.writeError(w, r, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		h.log.Warn("storage unavailable", "error", err)
		w.Header().Set("Retry-After", retryAfter)
		h.writeError(w, r, http.StatusServiceUnavailable, "storage unavailable")
	default:
		h.log.Error("request failed", "error", err)
		h.writeError(w, r, http.StatusInternalServerError, "internal error")
	}
}

// clientIP honours X-Forwarded-For only from configured proxies.
func (h *Handler) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !h.trusted(ip) {
		return host
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	// Walk from the right: the nearest untrusted hop is the real client.
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		parsed := net.ParseIP(candidate)
		if parsed == nil {
			break
		}
		if !h.trusted(parsed) {
			return candidate
		}
	}
	return host
}

func (h *Handler) trusted(ip net.IP) bool {
	for _, n := range h.opts.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ParseCIDRs converts trusted proxy configuration. Config validation has
// already rejected bad values, so an error here is unexpected.
func ParseCIDRs(in []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(in))
	for _, c := range in {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy: %w", err)
		}
		out = append(out, n)
	}
	return out, nil
}
