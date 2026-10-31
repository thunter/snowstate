package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/pkg/snowstate/service"
	"github.com/thunter/snowstate/pkg/snowstate/store"
	"github.com/thunter/snowstate/pkg/snowstate/store/memory"
)

var pepper = []byte(strings.Repeat("p", 32))

func TestGenerateTokenFormat(t *testing.T) {
	tok, err := GenerateToken()
	require.NoError(t, err)
	assert.Regexp(t, `^sst_[A-Za-z0-9_-]{43}$`, tok)
	assert.True(t, wellFormed(tok))
	assert.False(t, wellFormed("sst_short"))
	assert.False(t, wellFormed("xxx_"+tok[4:]))
}

func TestHMACIsKeyed(t *testing.T) {
	a := HMAC(pepper, "sst_x")
	assert.Len(t, a, 64)
	assert.NotEqual(t, a, HMAC([]byte("other"), "sst_x"))
}

type counting struct {
	store.Store
	lookups int
	touches int
	err     error
}

func (c *counting) GetTokenByHMAC(ctx context.Context, h string) (*store.Token, error) {
	c.lookups++
	if c.err != nil {
		return nil, c.err
	}
	return c.Store.GetTokenByHMAC(ctx, h)
}

func (c *counting) TouchToken(ctx context.Context, id string, at time.Time) error {
	c.touches++
	return c.Store.TouchToken(ctx, id, at)
}

func seed(t *testing.T, mutate ...func(*store.Token)) (*TokenAuth, *counting, string) {
	t.Helper()
	tok, err := GenerateToken()
	require.NoError(t, err)
	st := &counting{Store: memory.MustNew()}
	rec := &store.Token{ID: "t1", Principal: "ci", HMAC: HMAC(pepper, tok),
		Scopes: []store.Scope{{KeyPattern: "platform/*", Permissions: []string{"read"}}}}
	for _, m := range mutate {
		m(rec)
	}
	require.NoError(t, st.CreateToken(context.Background(), rec))
	return NewTokenAuth(pepper, st, slog.New(slog.DiscardHandler)), st, tok
}

func basicReq(user, pass string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetBasicAuth(user, pass)
	return r
}

func TestTokenAuthenticate(t *testing.T) {
	a, _, tok := seed(t)

	p, err := a.Authenticate(basicReq("ci", tok))
	require.NoError(t, err)
	assert.Equal(t, "ci", p.Name)
	assert.Equal(t, "token", p.Source)
	require.Len(t, p.Scopes, 1)

	_, err = a.Authenticate(basicReq("someone-else", tok))
	require.ErrorIs(t, err, ErrUnauthenticated, "username must match the principal")
	_, err = a.Authenticate(basicReq("ci", "sst_"+strings.Repeat("a", 43)))
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = a.Authenticate(basicReq("ci", "junk"))
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = a.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	require.ErrorIs(t, err, ErrUnauthenticated)
}

func TestExpiredAndRevokedTokens(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	a, _, tok := seed(t, func(r *store.Token) { r.ExpiresAt = &past })
	_, err := a.Authenticate(basicReq("ci", tok))
	require.ErrorIs(t, err, ErrUnauthenticated)

	a, _, tok = seed(t, func(r *store.Token) { r.RevokedAt = &past })
	_, err = a.Authenticate(basicReq("ci", tok))
	require.ErrorIs(t, err, ErrUnauthenticated)
}

func TestCacheAvoidsLookupsAndRevocationTakesEffectWithinTTL(t *testing.T) {
	a, st, tok := seed(t)
	now := time.Now()
	a.now = func() time.Time { return now }

	for range 3 {
		_, err := a.Authenticate(basicReq("ci", tok))
		require.NoError(t, err)
	}
	assert.Equal(t, 1, st.lookups)

	require.NoError(t, st.RevokeToken(context.Background(), "t1"))
	now = now.Add(59 * time.Second)
	_, err := a.Authenticate(basicReq("ci", tok))
	require.NoError(t, err, "still cached")

	now = now.Add(2 * time.Second)
	_, err = a.Authenticate(basicReq("ci", tok))
	require.ErrorIs(t, err, ErrUnauthenticated, "revoked after the 60 s TTL")
}

func TestCacheExpiryAppliesToTokenExpiresAt(t *testing.T) {
	exp := time.Now().Add(10 * time.Second)
	a, _, tok := seed(t, func(r *store.Token) { r.ExpiresAt = &exp })
	now := time.Now()
	a.now = func() time.Time { return now }
	_, err := a.Authenticate(basicReq("ci", tok))
	require.NoError(t, err)
	now = now.Add(11 * time.Second)
	_, err = a.Authenticate(basicReq("ci", tok))
	require.ErrorIs(t, err, ErrUnauthenticated, "a cached token still expires on time")
}

func TestCacheIsBounded(t *testing.T) {
	a := NewTokenAuth(pepper, memory.MustNew(), slog.New(slog.DiscardHandler))
	for i := range cacheSize + 10 {
		a.remember(strings.Repeat("h", 10)+string(rune(i+65)), &store.Token{})
	}
	assert.LessOrEqual(t, a.order.Len(), cacheSize)
	assert.Len(t, a.byKey, a.order.Len())
}

func TestTouchIsThrottled(t *testing.T) {
	a, st, tok := seed(t)
	now := time.Now()
	a.now = func() time.Time { return now }

	for range 3 {
		_, err := a.Authenticate(basicReq("ci", tok))
		require.NoError(t, err)
	}
	assert.Equal(t, 1, st.touches, "at most once per 5 minutes")

	now = now.Add(5*time.Minute + time.Second)
	_, err := a.Authenticate(basicReq("ci", tok))
	require.NoError(t, err)
	assert.Equal(t, 2, st.touches)
}

func TestStoreOutageIsNotUnauthenticated(t *testing.T) {
	a, st, tok := seed(t)
	st.err = store.ErrUnavailable
	_, err := a.Authenticate(basicReq("ci", tok))
	require.ErrorIs(t, err, store.ErrUnavailable)
	require.NotErrorIs(t, err, ErrUnauthenticated)
}

func TestParseScope(t *testing.T) {
	s, err := ParseScope("platform/*:read,write,lock")
	require.NoError(t, err)
	assert.Equal(t, "platform/*", s.KeyPattern)
	assert.Equal(t, []string{"read", "write", "lock"}, s.Permissions)

	for _, bad := range []string{"", "noperm", ":read", "x:fly"} {
		_, err := ParseScope(bad)
		require.Error(t, err, bad)
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, key string
		want         bool
	}{
		{"*", "platform/net/prod", true},
		{"platform/*", "platform/net/prod", true},
		{"platform/*", "platform2/net", false},
		{"platform/*", "other/platform/x", false},
		{"platform/*/prod", "platform/net/prod", true},
		{"platform/*/prod", "platform/a/b/prod", true},
		{"platform/*/prod", "platform/net/dev", false},
		{"platform/net/prod", "platform/net/prod", true},
		{"platform/net/prod", "platform/net/prod2", false},
		{"platform/?et/prod", "platform/net/prod", true},
		{"platform/?net", "platform//net", false},
		{"*/prod", "a/prod", true},
		{"**", "a/b", true},
		{"", "", true},
		{"", "a", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, MatchGlob(tt.pattern, tt.key), "%s vs %s", tt.pattern, tt.key)
	}
}

func TestScopeAuthorizerDeniesByDefault(t *testing.T) {
	a := ScopeAuthorizer{}
	ctx := context.Background()
	p := service.Principal{Name: "ci", Scopes: []store.Scope{
		{KeyPattern: "platform/*", Permissions: []string{"read", "lock"}},
		{KeyPattern: "shared/*", Permissions: []string{"write"}},
	}}
	require.NoError(t, a.Authorize(ctx, p, "read", "platform/x/y"))
	require.NoError(t, a.Authorize(ctx, p, "lock", "platform/x/y"))
	require.NoError(t, a.Authorize(ctx, p, "write", "shared/x"))
	require.ErrorIs(t, a.Authorize(ctx, p, "write", "platform/x/y"), store.ErrForbidden)
	require.ErrorIs(t, a.Authorize(ctx, p, "read", "shared/x"), store.ErrForbidden)
	require.ErrorIs(t, a.Authorize(ctx, p, "admin", ""), store.ErrForbidden)
	require.ErrorIs(t, a.Authorize(ctx, service.Principal{Name: "none"}, "read", "platform/x/y"), store.ErrForbidden)
	require.NoError(t, a.Authorize(ctx, p, "read", ""), "list endpoints ask about any key")
}

func TestLoopbackSecret(t *testing.T) {
	l, err := NewLoopbackSecret()
	require.NoError(t, err)
	assert.Len(t, l.Secret(), 43)

	p, err := l.Authenticate(basicReq("anything", l.Secret()))
	require.NoError(t, err)
	assert.Equal(t, "anything", p.Name)
	assert.Equal(t, "loopback", p.Source)

	_, err = l.Authenticate(basicReq("anything", "wrong"))
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = l.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	require.ErrorIs(t, err, ErrUnauthenticated)
}

func TestNoneGrantsEverything(t *testing.T) {
	p, err := None{}.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.NoError(t, ScopeAuthorizer{}.Authorize(context.Background(), p, "admin", "any/key"))
}

func mtlsRequest(cn string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{Subject: pkix.Name{CommonName: cn}}}}}
	return r
}

func TestMTLS(t *testing.T) {
	ctx := context.Background()
	st := memory.MustNew()
	past := time.Now().Add(-time.Hour)
	require.NoError(t, st.CreateToken(ctx, &store.Token{ID: "a", Principal: "svc", Scopes: []store.Scope{{KeyPattern: "a/*", Permissions: []string{"read"}}}}))
	require.NoError(t, st.CreateToken(ctx, &store.Token{ID: "b", Principal: "svc", RevokedAt: &past, Scopes: []store.Scope{{KeyPattern: "b/*", Permissions: []string{"read"}}}}))
	require.NoError(t, st.CreateToken(ctx, &store.Token{ID: "c", Principal: "other", Scopes: []store.Scope{{KeyPattern: "c/*", Permissions: []string{"read"}}}}))

	m := &MTLS{Tokens: st}
	p, err := m.Authenticate(mtlsRequest("svc"))
	require.NoError(t, err)
	assert.Equal(t, "mtls", p.Source)
	require.Len(t, p.Scopes, 1)
	assert.Equal(t, "a/*", p.Scopes[0].KeyPattern)

	_, err = m.Authenticate(mtlsRequest(""))
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = m.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	require.ErrorIs(t, err, ErrUnauthenticated)

	withBasic := &MTLS{Tokens: st, Next: None{}}
	p, err = withBasic.Authenticate(basicReq("x", "y"))
	require.NoError(t, err)
	assert.Equal(t, "anonymous", p.Name, "Basic credentials win when present")
}

type failingList struct{ store.Store }

func (failingList) ListTokens(context.Context) ([]store.Token, error) {
	return nil, errors.New("down")
}

func TestMTLSOutage(t *testing.T) {
	m := &MTLS{Tokens: failingList{memory.MustNew()}}
	_, err := m.Authenticate(mtlsRequest("svc"))
	require.ErrorIs(t, err, store.ErrUnavailable)
}
