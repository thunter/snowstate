package sfconn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitHubToken(t *testing.T) {
	var audience, auth string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		audience, auth = r.URL.Query().Get("audience"), r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"value":"jwt-1"}`))
	}))
	defer srv.Close()
	t.Setenv(githubRequestURL, srv.URL+"/token?api-version=2.0")
	t.Setenv(githubRequestToken, "req-token")

	fetch := oidcTokenFetcher(WIF{TokenSource: "github"})
	got, err := fetch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "jwt-1", got)
	assert.Equal(t, DefaultWIFAudience, audience)
	assert.Equal(t, "bearer req-token", auth)

	_, err = fetch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "each new connection fetches afresh")

	got, err = oidcTokenFetcher(WIF{TokenSource: "github", Audience: "custom"})(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "custom", audience)
	assert.Equal(t, "jwt-1", got)
}

func TestGitHubTokenNeedsThePermission(t *testing.T) {
	t.Setenv(githubRequestURL, "")
	t.Setenv(githubRequestToken, "")
	_, err := oidcTokenFetcher(WIF{TokenSource: "github"})(context.Background())
	require.ErrorContains(t, err, "add `permissions: id-token: write` to the job")
}

func TestGitHubTokenBadResponses(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"status": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		"json":   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nope")) },
		"empty":  func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"value":""}`)) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			t.Setenv(githubRequestURL, srv.URL+"/?x=1")
			t.Setenv(githubRequestToken, "secret-req-token")
			_, err := oidcTokenFetcher(WIF{TokenSource: "github"})(context.Background())
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "secret-req-token")
		})
	}
}

func TestFileAndEnvTokens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jwt")
	require.NoError(t, os.WriteFile(path, []byte("jwt-file\n"), 0o600))
	got, err := oidcTokenFetcher(WIF{TokenSource: "file", TokenFilePath: path})(ctx)
	require.NoError(t, err)
	assert.Equal(t, "jwt-file", got)

	t.Setenv("MY_JWT", "jwt-env")
	got, err = oidcTokenFetcher(WIF{TokenSource: "env", TokenEnv: "MY_JWT"})(ctx)
	require.NoError(t, err)
	assert.Equal(t, "jwt-env", got)

	for _, wif := range []WIF{
		{TokenSource: "file"}, {TokenSource: "env"}, {TokenSource: "env", TokenEnv: "UNSET_JWT_VAR"},
		{TokenSource: "file", TokenFilePath: "/nonexistent/jwt"}, {TokenSource: "magic"},
	} {
		_, err := oidcTokenFetcher(wif)(ctx)
		require.Error(t, err, wif.TokenSource)
	}
}

func TestWorkloadIdentityConfig(t *testing.T) {
	c, err := driverConfig(Config{Account: "a", Auth: AuthWorkloadIdentity, WIF: WIF{Provider: "oidc"}})
	require.NoError(t, err)
	assert.Equal(t, "OIDC", c.WorkloadIdentityProvider)
}
