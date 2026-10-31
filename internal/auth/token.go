// Package auth verifies client credentials and scope permissions (SPEC §5.1).
package auth

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/thunter/snowstate/pkg/snowstate/service"
	"github.com/thunter/snowstate/pkg/snowstate/store"
)

const (
	// TokenPrefix starts every token.
	TokenPrefix = "sst_"
	// tokenBodyLen is the base64url length of 32 random bytes.
	tokenBodyLen = 43
	cacheTTL     = 60 * time.Second
	cacheSize    = 4096
	touchEvery   = 5 * time.Minute
)

// ErrUnauthenticated means credentials are missing or wrong.
var ErrUnauthenticated = errors.New("unauthenticated")

// Authenticator turns a request into a Principal.
type Authenticator interface {
	Authenticate(r *http.Request) (service.Principal, error)
}

// GenerateToken returns a new token.
func GenerateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HMAC returns the stored form of a token.
func HMAC(pepper []byte, token string) string {
	m := hmac.New(sha256.New, pepper)
	m.Write([]byte(token))
	return hex.EncodeToString(m.Sum(nil))
}

// wellFormed rejects junk before it costs a Snowflake query.
func wellFormed(token string) bool {
	return strings.HasPrefix(token, TokenPrefix) && len(token) == len(TokenPrefix)+tokenBodyLen
}

type cacheEntry struct {
	hmac      string
	token     store.Token
	verified  time.Time
	touchedAt time.Time
}

// TokenAuth verifies Basic credentials against the TOKENS table. Successful
// lookups are cached for 60 s, which is the bound on revocation latency.
type TokenAuth struct {
	pepper []byte
	st     store.Store
	log    *slog.Logger
	now    func() time.Time

	mu    sync.Mutex
	order *list.List // front is most recently used
	byKey map[string]*list.Element
}

// NewTokenAuth returns a TokenAuth.
func NewTokenAuth(pepper []byte, st store.Store, log *slog.Logger) *TokenAuth {
	return &TokenAuth{
		pepper: pepper, st: st, log: log, now: time.Now,
		order: list.New(), byKey: map[string]*list.Element{},
	}
}

// Authenticate implements Authenticator.
func (a *TokenAuth) Authenticate(r *http.Request) (service.Principal, error) {
	user, pass, ok := r.BasicAuth()
	if !ok || !wellFormed(pass) {
		return service.Principal{}, ErrUnauthenticated
	}
	return a.Verify(r.Context(), user, pass)
}

// Verify checks a username and token.
func (a *TokenAuth) Verify(ctx context.Context, user, token string) (service.Principal, error) {
	h := HMAC(a.pepper, token)
	entry := a.cached(h)
	if entry == nil {
		t, err := a.st.GetTokenByHMAC(ctx, h)
		if errors.Is(err, store.ErrNotFound) {
			return service.Principal{}, ErrUnauthenticated
		}
		if err != nil {
			return service.Principal{}, err
		}
		entry = a.remember(h, t)
	}
	t := &entry.token
	if subtle.ConstantTimeCompare([]byte(h), []byte(t.HMAC)) != 1 ||
		subtle.ConstantTimeCompare([]byte(user), []byte(t.Principal)) != 1 {
		return service.Principal{}, ErrUnauthenticated
	}
	now := a.now()
	if t.RevokedAt != nil || (t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)) {
		return service.Principal{}, ErrUnauthenticated
	}
	a.touch(ctx, entry, now)
	return service.Principal{Name: t.Principal, Source: "token", Scopes: t.Scopes}, nil
}

func (a *TokenAuth) cached(h string) *cacheEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	el, ok := a.byKey[h]
	if !ok {
		return nil
	}
	e, _ := el.Value.(*cacheEntry)
	if a.now().Sub(e.verified) >= cacheTTL {
		a.order.Remove(el)
		delete(a.byKey, h)
		return nil
	}
	a.order.MoveToFront(el)
	return e
}

func (a *TokenAuth) remember(h string, t *store.Token) *cacheEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := &cacheEntry{hmac: h, token: *t, verified: a.now()}
	if t.LastUsedAt != nil {
		e.touchedAt = *t.LastUsedAt
	}
	a.byKey[h] = a.order.PushFront(e)
	if a.order.Len() > cacheSize {
		oldest := a.order.Back()
		old, _ := oldest.Value.(*cacheEntry)
		a.order.Remove(oldest)
		delete(a.byKey, old.hmac)
	}
	return e
}

// touch updates LAST_USED_AT at most once per 5 minutes per token.
func (a *TokenAuth) touch(ctx context.Context, e *cacheEntry, now time.Time) {
	a.mu.Lock()
	due := now.Sub(e.touchedAt) >= touchEvery
	if due {
		e.touchedAt = now
	}
	a.mu.Unlock()
	if !due {
		return
	}
	if err := a.st.TouchToken(context.WithoutCancel(ctx), e.token.ID, now.UTC()); err != nil {
		a.log.Warn("could not update token last-used time", "token_id", e.token.ID, "error", err)
	}
}

// ParseScope parses the CLI form 'platform/*:read,write,lock'.
func ParseScope(s string) (store.Scope, error) {
	i := strings.LastIndexByte(s, ':')
	if i <= 0 {
		return store.Scope{}, fmt.Errorf("scope %q: want <pattern>:<perm>[,<perm>]", s)
	}
	scope := store.Scope{KeyPattern: s[:i]}
	for _, p := range strings.Split(s[i+1:], ",") {
		switch p {
		case service.ActionRead, service.ActionWrite, service.ActionLock, service.ActionAdmin:
			scope.Permissions = append(scope.Permissions, p)
		default:
			return store.Scope{}, fmt.Errorf("scope %q: unknown permission %q", s, p)
		}
	}
	return scope, nil
}

// MatchGlob matches a key pattern. '*' matches any run of characters, including
// '/', so 'platform/*' covers 'platform/network/prod'. '?' matches one
// character other than '/'.
func MatchGlob(pattern, key string) bool {
	px, kx := 0, 0
	star, mark := -1, 0
	for kx < len(key) {
		switch {
		case px < len(pattern) && pattern[px] == '*':
			star, mark = px, kx
			px++
		case px < len(pattern) && (pattern[px] == key[kx] || (pattern[px] == '?' && key[kx] != '/')):
			px++
			kx++
		case star >= 0:
			mark++
			px, kx = star+1, mark
		default:
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// ScopeAuthorizer enforces token scopes (gateway mode). It denies by default.
type ScopeAuthorizer struct{}

// Authorize implements service.Authorizer. An empty key asks whether the
// principal has the permission on any key, for list endpoints which then
// filter per key.
func (ScopeAuthorizer) Authorize(_ context.Context, p service.Principal, action, key string) error {
	for _, s := range p.Scopes {
		if !contains(s.Permissions, action) {
			continue
		}
		if key == "" || MatchGlob(s.KeyPattern, key) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s may not %s %s", store.ErrForbidden, p.Name, action, key)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

var _ service.Authorizer = ScopeAuthorizer{}
