package api

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/internal/auth"
	"github.com/thunter/snowstate/internal/obs"
	"github.com/thunter/snowstate/pkg/snowstate/service"
	"github.com/thunter/snowstate/pkg/snowstate/store"
	"github.com/thunter/snowstate/pkg/snowstate/store/memory"
)

const (
	statePath = "/v1/states/platform/net/prod"
	stateBody = `{"version":4,"terraform_version":"1.9.0","serial":1,"lineage":"l","resources":[]}`
)

func lockBody(id string) string {
	return `{"ID":"` + id + `","Operation":"OperationTypeApply","Info":"","Who":"bob@ci","Version":"1.9.0","Created":"2026-10-31T00:00:00Z","Path":""}`
}

type env struct {
	h   *Handler
	st  *memory.Store
	svc *service.Service
}

func newEnv(t *testing.T, mutate ...func(*service.Options, *Options)) *env {
	t.Helper()
	st := memory.MustNew()
	so := service.Options{Store: st, InlineMaxBytes: 8 << 20, AllowForceUnlock: true, MaxStateBytes: 1 << 20}
	ao := Options{Auth: auth.None{}, MaxStateBytes: 1 << 20, RequestTimeout: time.Minute, ServeMetrics: true,
		Metrics: obs.NewMetrics(nil)}
	for _, m := range mutate {
		m(&so, &ao)
	}
	svc := service.New(so)
	t.Cleanup(svc.Close)
	ao.Service = svc
	return &env{h: New(ao), st: st, svc: svc}
}

func (e *env) do(method, target, body string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *env) ok(t *testing.T, method, target, body string) {
	t.Helper()
	w := e.do(method, target, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestEveryResponseHasRequestID(t *testing.T) {
	e := newEnv(t)
	for _, target := range []string{"/healthz", statePath, "/nope", "/v1/states/bad"} {
		w := e.do(http.MethodGet, target, "")
		assert.NotEmpty(t, w.Header().Get("X-Request-Id"), target)
	}
}

func TestGet(t *testing.T) {
	e := newEnv(t)

	w := e.do(http.MethodGet, statePath, "")
	assert.Equal(t, http.StatusNoContent, w.Code, "no state")

	e.ok(t, http.MethodPost, statePath, stateBody)
	w = e.do(http.MethodGet, statePath, "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, stateBody, w.Body.String(), "exact stored bytes")
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	sum := md5.Sum([]byte(stateBody))
	assert.Equal(t, base64.StdEncoding.EncodeToString(sum[:]), w.Header().Get("Content-MD5"))
	assert.Regexp(t, `^"[0-9a-f-]{36}"$`, w.Header().Get("ETag"))

	e.ok(t, http.MethodDelete, statePath, "")
	assert.Equal(t, http.StatusNoContent, e.do(http.MethodGet, statePath, "").Code, "soft deleted")
}

func TestWriteRows(t *testing.T) {
	sum := md5.Sum([]byte(stateBody))
	goodMD5 := base64.StdEncoding.EncodeToString(sum[:])
	wrong := md5.Sum([]byte("other"))
	badMD5 := base64.StdEncoding.EncodeToString(wrong[:])

	tests := []struct {
		name    string
		method  string
		query   string
		body    string
		headers []string
		locked  string
		require bool
		want    int
		errText string
	}{
		{name: "not json", body: "nope", want: 400},
		{name: "missing version", body: `{"serial":1,"lineage":"l"}`, want: 400},
		{name: "missing serial", body: `{"version":4,"lineage":"l"}`, want: 400},
		{name: "missing lineage", body: `{"version":4,"serial":1}`, want: 400},
		{name: "md5 mismatch", body: stateBody, headers: []string{"Content-MD5", badMD5}, want: 400},
		{name: "md5 unparsable", body: stateBody, headers: []string{"Content-MD5", "%%%"}, want: 400},
		{name: "md5 good", body: stateBody, headers: []string{"Content-MD5", goodMD5}, want: 200},
		{name: "too large", body: `{"version":4,"pad":"` + strings.Repeat("x", 1<<20) + `"}`, want: 413},
		{name: "locked, no id", body: stateBody, locked: "L1", want: 423},
		{name: "locked, other id", query: "?ID=L2", body: stateBody, locked: "L1", want: 423},
		{name: "locked, our id", query: "?ID=L1", body: stateBody, locked: "L1", want: 200},
		{name: "put verb", method: http.MethodPut, query: "?ID=L1", body: stateBody, locked: "L1", want: 200},
		{name: "id without lock", query: "?ID=L1", body: stateBody, want: 409, errText: "lock not held"},
		{name: "lock required", body: stateBody, require: true, want: 423, errText: "lock required"},
		{name: "plain write", body: stateBody, want: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, func(o *service.Options, _ *Options) { o.RequireLockForWrite = tt.require })
			if tt.locked != "" {
				e.ok(t, "LOCK", statePath, lockBody(tt.locked))
			}
			method := tt.method
			if method == "" {
				method = http.MethodPost
			}
			w := e.do(method, statePath+tt.query, tt.body, tt.headers...)
			require.Equal(t, tt.want, w.Code, w.Body.String())
			if tt.errText != "" {
				var body map[string]string
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				assert.Equal(t, tt.errText, body["error"])
				assert.Equal(t, w.Header().Get("X-Request-Id"), body["request_id"])
			}
			if tt.want == 423 && tt.locked != "" {
				assert.JSONEq(t, lockBody(tt.locked), w.Body.String(), "body is the current lock")
			}
		})
	}
}

func TestIdempotentWriteMakesNoVersion(t *testing.T) {
	e := newEnv(t)
	e.ok(t, http.MethodPost, statePath, stateBody)
	e.ok(t, http.MethodPost, statePath, stateBody)
	vs, err := e.st.ListVersions(context.Background(), "platform/net/prod", 0)
	require.NoError(t, err)
	assert.Len(t, vs, 1)
}

func TestSerialRegressionIs409(t *testing.T) {
	e := newEnv(t, func(o *service.Options, _ *Options) { o.RejectSerialRegression = true })
	e.ok(t, http.MethodPost, statePath, strings.Replace(stateBody, `"serial":1`, `"serial":5`, 1))
	assert.Equal(t, 409, e.do(http.MethodPost, statePath, stateBody).Code)
}

func TestDeleteRows(t *testing.T) {
	e := newEnv(t)
	e.ok(t, http.MethodDelete, statePath, "") // missing state is idempotent
	e.ok(t, http.MethodPost, statePath, stateBody)
	e.ok(t, "LOCK", statePath, lockBody("L1"))

	w := e.do(http.MethodDelete, statePath, "")
	require.Equal(t, 423, w.Code)
	assert.JSONEq(t, lockBody("L1"), w.Body.String())
	assert.Equal(t, 423, e.do(http.MethodDelete, statePath+"?ID=L2", "").Code)
	e.ok(t, http.MethodDelete, statePath+"?ID=L1", "")
}

func TestLockRows(t *testing.T) {
	e := newEnv(t)

	w := e.do("LOCK", statePath, lockBody("L1"))
	require.Equal(t, 200, w.Code)
	assert.Equal(t, lockBody("L1"), w.Body.String(), "returned verbatim")

	w = e.do("LOCK", statePath, lockBody("L1"))
	require.Equal(t, 200, w.Code, "idempotent re-lock")
	assert.Equal(t, lockBody("L1"), w.Body.String())

	w = e.do("LOCK", statePath, lockBody("L2"))
	require.Equal(t, 423, w.Code)
	assert.Equal(t, lockBody("L1"), w.Body.String())

	assert.Equal(t, 400, e.do("LOCK", "/v1/states/a/b", "nope").Code)
	assert.Equal(t, 400, e.do("LOCK", "/v1/states/a/b", `{"Who":"x"}`).Code)
	assert.Equal(t, 400, e.do("LOCK", "/v1/states/a/b", "").Code)
	assert.Equal(t, 413, e.do("LOCK", "/v1/states/a/b", `{"ID":"`+strings.Repeat("x", 1<<20+1)+`"}`).Code)
}

func TestUnlockRows(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "LOCK", statePath, lockBody("L1"))

	w := e.do("UNLOCK", statePath, lockBody("L2"))
	require.Equal(t, 409, w.Code, "different ID")
	assert.Equal(t, lockBody("L1"), w.Body.String())

	e.ok(t, "UNLOCK", statePath, lockBody("L1"))
	assert.Nil(t, lockOf(t, e), "removed")
	e.ok(t, "UNLOCK", statePath, lockBody("L1")) // idempotent with nothing held
	assert.Empty(t, auditActions(e, store.ActionForceUnlock), "normal unlocks are not force")

	assert.Equal(t, 400, e.do("UNLOCK", statePath, "not json").Code)
}

func lockOf(t *testing.T, e *env) *store.Lock {
	t.Helper()
	l, err := e.st.GetLock(context.Background(), "platform/net/prod")
	require.NoError(t, err)
	return l
}

func auditActions(e *env, action string) []store.AuditEvent {
	var out []store.AuditEvent
	for _, a := range e.st.Audit() {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

func TestForceUnlockTerraformEmptyBody(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "LOCK", statePath, lockBody("L1"))
	e.ok(t, "UNLOCK", statePath, "")
	assert.Nil(t, lockOf(t, e))

	rows := auditActions(e, store.ActionForceUnlock)
	require.Len(t, rows, 1)
	assert.Equal(t, lockBody("L1"), rows[0].Detail["removed_lock"])
}

// OpenTofu sends the user's ID with every other field zero (decisions.md D1).
func TestForceUnlockOpenTofuBody(t *testing.T) {
	body := func(id string) string {
		return `{"ID":"` + id + `","Operation":"","Info":"","Who":"","Version":"","Created":"0001-01-01T00:00:00Z","Path":""}`
	}
	e := newEnv(t)
	e.ok(t, "LOCK", statePath, lockBody("L1"))

	assert.Equal(t, 409, e.do("UNLOCK", statePath, body("wrong")).Code)
	assert.NotNil(t, lockOf(t, e), "a wrong ID must not unlock")

	e.ok(t, "UNLOCK", statePath, body("L1"))
	assert.Nil(t, lockOf(t, e))
	assert.Len(t, auditActions(e, store.ActionForceUnlock), 1)
}

func TestForceUnlockDisabled(t *testing.T) {
	e := newEnv(t, func(o *service.Options, _ *Options) { o.AllowForceUnlock = false })
	e.ok(t, "LOCK", statePath, lockBody("L1"))
	assert.Equal(t, 403, e.do("UNLOCK", statePath, "").Code)
	assert.Equal(t, 403, e.do("UNLOCK", statePath, `{"ID":"L1"}`).Code)
	assert.NotNil(t, lockOf(t, e))
	e.ok(t, "UNLOCK", statePath, lockBody("L1"))
}

func TestAlternateLockPath(t *testing.T) {
	e := newEnv(t)
	lockPath := statePath + "/lock"
	for _, m := range []string{http.MethodPost, http.MethodPut, "LOCK"} {
		e.ok(t, m, lockPath, lockBody("L1"))
		assert.NotNil(t, lockOf(t, e), m)
		e.ok(t, http.MethodDelete, lockPath, lockBody("L1"))
		assert.Nil(t, lockOf(t, e), m)
	}
	e.ok(t, http.MethodPost, lockPath, lockBody("L1"))
	e.ok(t, "UNLOCK", lockPath, lockBody("L1"))

	w := e.do(http.MethodGet, lockPath, "")
	assert.Equal(t, 405, w.Code)
	assert.Contains(t, w.Header().Get("Allow"), "UNLOCK")
}

func TestInvalidKeys(t *testing.T) {
	e := newEnv(t)
	for _, target := range []string{
		"/v1/states/single", "/v1/states/a//b", "/v1/states/a/b/", "/v1/states/a/.b", "/v1/states/a/b%20c",
		"/v1/states/a/%2e%2e/b",
	} {
		assert.Equal(t, 400, e.do(http.MethodGet, target, "").Code, target)
	}
	assert.Equal(t, 400, e.do("LOCK", "/v1/states/single/lock", lockBody("L")).Code)
}

func TestUnknownMethodAndPath(t *testing.T) {
	e := newEnv(t)
	w := e.do(http.MethodPatch, statePath, "")
	assert.Equal(t, 405, w.Code)
	assert.Equal(t, "GET, POST, PUT, DELETE, LOCK, UNLOCK", w.Header().Get("Allow"))
	assert.Equal(t, 404, e.do(http.MethodGet, "/nope", "").Code)
}

type fixedAuth struct{ scopes []store.Scope }

func (f fixedAuth) Authenticate(r *http.Request) (service.Principal, error) {
	if _, _, ok := r.BasicAuth(); !ok {
		return service.Principal{}, auth.ErrUnauthenticated
	}
	return service.Principal{Name: "ci", Source: "token", Scopes: f.scopes}, nil
}

func TestAuthRows(t *testing.T) {
	e := newEnv(t, func(o *service.Options, a *Options) {
		o.Authorizer = auth.ScopeAuthorizer{}
		a.Auth = fixedAuth{scopes: []store.Scope{{KeyPattern: "platform/*", Permissions: []string{"read"}}}}
	})
	w := e.do(http.MethodGet, statePath, "")
	assert.Equal(t, 401, w.Code)
	assert.Equal(t, `Basic realm="snowstate"`, w.Header().Get("WWW-Authenticate"))

	basic := []string{"Authorization", "Basic Y2k6eA=="}
	assert.Equal(t, 204, e.do(http.MethodGet, statePath, "", basic...).Code, "read allowed")
	assert.Equal(t, 403, e.do(http.MethodGet, "/v1/states/other/net/prod", "", basic...).Code, "other project")
	assert.Equal(t, 403, e.do(http.MethodPost, statePath, stateBody, basic...).Code, "write denied")
	assert.Equal(t, 403, e.do("LOCK", statePath, lockBody("L"), basic...).Code, "lock denied")
	assert.Equal(t, 403, e.do("UNLOCK", statePath, "", basic...).Code, "force-unlock needs lock")
}

type errStore struct {
	store.Store
	err error
}

func (s errStore) GetState(context.Context, string) (*store.StateMeta, error) { return nil, s.err }
func (s errStore) InTx(context.Context, string, func(store.Tx) error) error   { return s.err }
func (s errStore) Ping(context.Context) error                                 { return s.err }

func TestSnowflakeUnavailableIs503(t *testing.T) {
	e := newEnv(t, func(o *service.Options, _ *Options) {
		o.Store = errStore{Store: memory.MustNew(), err: store.ErrUnavailable}
	})
	for _, tc := range []struct{ method, body string }{
		{http.MethodGet, ""}, {http.MethodPost, stateBody}, {"LOCK", lockBody("L")}, {http.MethodDelete, ""},
	} {
		w := e.do(tc.method, statePath, tc.body)
		assert.Equal(t, 503, w.Code, tc.method)
		assert.Equal(t, "2", w.Header().Get("Retry-After"), tc.method)
	}
}

func TestUnexpectedErrorIs500WithoutDetail(t *testing.T) {
	e := newEnv(t, func(o *service.Options, _ *Options) {
		o.Store = errStore{Store: memory.MustNew(), err: errors.New("SQL compilation error: secret detail")}
	})
	w := e.do(http.MethodGet, statePath, "")
	assert.Equal(t, 500, w.Code)
	assert.NotContains(t, w.Body.String(), "secret detail")
}

func TestHealthAndReadiness(t *testing.T) {
	e := newEnv(t)
	assert.Equal(t, 200, e.do(http.MethodGet, "/healthz", "").Code)
	assert.Equal(t, 200, e.do(http.MethodGet, "/readyz", "").Code)

	bad := newEnv(t, func(o *service.Options, _ *Options) {
		o.Store = errStore{Store: memory.MustNew(), err: store.ErrUnavailable}
	})
	assert.Equal(t, 200, bad.do(http.MethodGet, "/healthz", "").Code, "liveness never touches storage")
	assert.Equal(t, 503, bad.do(http.MethodGet, "/readyz", "").Code)
}

func TestReadinessIsCached(t *testing.T) {
	counting := &countPing{Store: memory.MustNew()}
	e := newEnv(t, func(o *service.Options, _ *Options) { o.Store = counting })
	for range 5 {
		e.do(http.MethodGet, "/readyz", "")
	}
	assert.Equal(t, 1, counting.n)
}

type countPing struct {
	store.Store
	n int
}

func (c *countPing) Ping(context.Context) error { c.n++; return nil }

func TestMetricsEndpoint(t *testing.T) {
	e := newEnv(t)
	e.ok(t, http.MethodPost, statePath, stateBody)
	body := e.do(http.MethodGet, "/metrics", "").Body.String()
	for _, want := range []string{
		`snowstate_http_requests_total{method="POST",route="state",status="200"} 1`,
		"snowstate_http_request_duration_seconds",
		"snowstate_audit_buffer_depth",
	} {
		assert.Contains(t, body, want)
	}
}

func TestMetricsOffWhenSeparateListener(t *testing.T) {
	e := newEnv(t, func(_ *service.Options, a *Options) { a.ServeMetrics = false })
	assert.Equal(t, 404, e.do(http.MethodGet, "/metrics", "").Code)
}

func TestRequestLogNeverContainsSecrets(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := newEnv(t, func(o *service.Options, a *Options) { a.Logger = log; o.Logger = log })

	secret := `{"version":4,"serial":1,"lineage":"l","password":"hunter2-state-secret"}`
	w := e.do(http.MethodPost, statePath, secret, "Authorization", "Basic c2VjcmV0LWhlYWRlci12YWx1ZQ==")
	require.Equal(t, 200, w.Code)

	out := logs.String()
	assert.NotContains(t, out, "hunter2-state-secret")
	assert.NotContains(t, out, "c2VjcmV0LWhlYWRlci12YWx1ZQ")
	assert.Contains(t, out, `"state_key":"platform/net/prod"`)
	assert.Contains(t, out, `"principal":"anonymous"`)
	assert.Contains(t, out, `"duration_ms"`)
}

func TestRateLimitFailedAuth(t *testing.T) {
	e := newEnv(t, func(_ *service.Options, a *Options) {
		a.Auth = fixedAuth{}
		a.Limiter = NewRateLimiter(3)
	})
	for range 3 {
		assert.Equal(t, 401, e.do(http.MethodGet, statePath, "").Code)
	}
	w := e.do(http.MethodGet, statePath, "", "Authorization", "Basic Y2k6eA==")
	assert.Equal(t, 429, w.Code, "even good credentials wait out the window")
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
}

func TestRateLimiterWindow(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewRateLimiter(1)
	l.now = func() time.Time { return now }
	require.True(t, l.Allow("ip"))
	l.Fail("ip")
	assert.False(t, l.Allow("ip"))
	assert.True(t, l.Allow("other"))
	now = now.Add(time.Minute)
	assert.True(t, l.Allow("ip"))

	off := NewRateLimiter(0)
	off.Fail("ip")
	assert.True(t, off.Allow("ip"))
}

func TestClientIPOnlyTrustsConfiguredProxies(t *testing.T) {
	cidrs, err := ParseCIDRs([]string{"10.0.0.0/8"})
	require.NoError(t, err)
	h := New(Options{TrustedProxies: cidrs})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = net.JoinHostPort("10.1.1.1", "5")
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.2.2.2")
	assert.Equal(t, "1.2.3.4", h.clientIP(r))

	r.Header.Set("X-Forwarded-For", "not-an-ip")
	assert.Equal(t, "10.1.1.1", h.clientIP(r))

	r.RemoteAddr = net.JoinHostPort("8.8.8.8", "5")
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	assert.Equal(t, "8.8.8.8", h.clientIP(r), "untrusted peers cannot spoof")

	_, err = ParseCIDRs([]string{"nope"})
	require.Error(t, err)
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestBrokenClientBodyIs400(t *testing.T) {
	e := newEnv(t)
	for _, m := range []string{http.MethodPost, "LOCK", "UNLOCK"} {
		r := httptest.NewRequest(m, statePath, brokenBody{})
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		assert.Equal(t, 400, w.Code, m)
	}
}
