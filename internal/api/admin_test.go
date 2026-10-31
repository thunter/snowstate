package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/internal/auth"
	"github.com/thunter/snowstate/pkg/snowstate/service"
	"github.com/thunter/snowstate/pkg/snowstate/store"
)

func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	return out
}

func TestAdminStatesList(t *testing.T) {
	e := newEnv(t)
	e.ok(t, http.MethodPost, statePath, stateBody)
	e.ok(t, http.MethodPost, "/v1/states/other/x/default", stateBody)

	w := e.do(http.MethodGet, "/v1/admin/states?prefix=platform/net", "")
	require.Equal(t, 200, w.Code)
	rows := decode[[]map[string]any](t, w.Body.Bytes())
	require.Len(t, rows, 1)
	assert.Equal(t, "platform/net/prod", rows[0]["key"])
	assert.Equal(t, "platform/net", rows[0]["prefix"])
	assert.Equal(t, "prod", rows[0]["workspace"])

	assert.Equal(t, 405, e.do(http.MethodPost, "/v1/admin/states", "").Code)
}

func TestAdminVersionsAndRestoreRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.ok(t, http.MethodPost, statePath, stateBody)
	second := `{"version":4,"serial":2,"lineage":"l"}`
	e.ok(t, http.MethodPost, statePath, second)

	base := "/v1/admin/states/platform/net/prod"
	w := e.do(http.MethodGet, base+"/versions", "")
	require.Equal(t, 200, w.Code)
	versions := decode[[]map[string]any](t, w.Body.Bytes())
	require.Len(t, versions, 2)
	oldest := versions[1]["id"].(string)

	w = e.do(http.MethodGet, base+"/versions?limit=1", "")
	assert.Len(t, decode[[]map[string]any](t, w.Body.Bytes()), 1)
	assert.Equal(t, 400, e.do(http.MethodGet, base+"/versions?limit=x", "").Code)

	w = e.do(http.MethodGet, base+"/versions/"+oldest, "")
	require.Equal(t, 200, w.Code)
	assert.Equal(t, stateBody, w.Body.String(), "raw content")
	assert.Equal(t, 404, e.do(http.MethodGet, base+"/versions/missing", "").Code)

	w = e.do(http.MethodPost, base+"/restore", `{"version_id":"`+oldest+`"}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, oldest, decode[map[string]any](t, w.Body.Bytes())["restored_from"])
	assert.Equal(t, stateBody, e.do(http.MethodGet, statePath, "").Body.String())

	assert.Equal(t, 400, e.do(http.MethodPost, base+"/restore", `{}`).Code)
	assert.Equal(t, 400, e.do(http.MethodPost, base+"/restore", `junk`).Code)
	assert.Equal(t, 404, e.do(http.MethodPost, base+"/restore", `{"version_id":"nope"}`).Code)
	assert.Equal(t, 405, e.do(http.MethodGet, base+"/restore", "").Code)
	assert.Equal(t, 405, e.do(http.MethodPost, base+"/versions", "").Code)
	assert.Equal(t, 404, e.do(http.MethodGet, base, "").Code)

	e.ok(t, "LOCK", statePath, lockBody("L1"))
	assert.Equal(t, 423, e.do(http.MethodPost, base+"/restore", `{"version_id":"`+oldest+`"}`).Code)
}

func TestAdminKeyNamedVersions(t *testing.T) {
	e := newEnv(t)
	e.ok(t, http.MethodPost, "/v1/states/a/versions", stateBody)
	w := e.do(http.MethodGet, "/v1/admin/states/a/versions/versions", "")
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Len(t, decode[[]map[string]any](t, w.Body.Bytes()), 1)
}

func TestAdminLocks(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "LOCK", statePath, lockBody("L1"))

	w := e.do(http.MethodGet, "/v1/admin/locks", "")
	require.Equal(t, 200, w.Code)
	rows := decode[[]map[string]any](t, w.Body.Bytes())
	require.Len(t, rows, 1)
	assert.Equal(t, "L1", rows[0]["id"])
	assert.Equal(t, "bob@ci", rows[0]["who"])

	e.ok(t, http.MethodDelete, "/v1/admin/locks/platform/net/prod", "")
	assert.Nil(t, lockOf(t, e))
	assert.Len(t, auditActions(e, store.ActionForceUnlock), 1)

	assert.Equal(t, 405, e.do(http.MethodGet, "/v1/admin/locks/platform/net/prod", "").Code)
	assert.Equal(t, 405, e.do(http.MethodPost, "/v1/admin/locks", "").Code)
	assert.Equal(t, 404, e.do(http.MethodGet, "/v1/admin/nope", "").Code)
}

func TestAdminNeedsAdminPermission(t *testing.T) {
	e := newEnv(t, func(o *service.Options, a *Options) {
		o.Authorizer = auth.ScopeAuthorizer{}
		a.Auth = fixedAuth{scopes: []store.Scope{{KeyPattern: "*", Permissions: []string{"read", "write", "lock"}}}}
	})
	basic := []string{"Authorization", "Basic Y2k6eA=="}
	assert.Equal(t, 401, e.do(http.MethodGet, "/v1/admin/states", "").Code)
	for _, target := range []string{
		"/v1/admin/states", "/v1/admin/locks", "/v1/admin/states/a/b/versions", "/v1/admin/states/a/b/versions/x",
	} {
		assert.Equal(t, 403, e.do(http.MethodGet, target, "", basic...).Code, target)
	}
	assert.Equal(t, 403, e.do(http.MethodPost, "/v1/admin/states/a/b/restore", `{"version_id":"x"}`, basic...).Code)
	assert.Equal(t, 403, e.do(http.MethodDelete, "/v1/admin/locks/a/b", "", basic...).Code)
}

func TestAdminErrorsOnBadKey(t *testing.T) {
	e := newEnv(t)
	assert.Equal(t, 400, e.do(http.MethodGet, "/v1/admin/states/bad/versions", "").Code)
	assert.Equal(t, 400, e.do(http.MethodGet, "/v1/admin/states/bad/versions/x", "").Code)
	assert.Equal(t, 400, e.do(http.MethodDelete, "/v1/admin/locks/bad", "").Code)
}
