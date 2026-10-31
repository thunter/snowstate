package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/thunter/snowstate/pkg/snowstate/service"
	"github.com/thunter/snowstate/pkg/snowstate/store"
)

// LoopbackSecret accepts any username with a one-time password.
type LoopbackSecret struct {
	secret []byte
	source string
}

// NewLoopbackSecret generates the one-time password. source is the principal
// source recorded for callers: "loopback" normally.
func NewLoopbackSecret() (*LoopbackSecret, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return &LoopbackSecret{secret: []byte(base64.RawURLEncoding.EncodeToString(b[:])), source: "loopback"}, nil
}

// Secret returns the password to hand to the child process.
func (l *LoopbackSecret) Secret() string { return string(l.secret) }

// Authenticate implements Authenticator.
func (l *LoopbackSecret) Authenticate(r *http.Request) (service.Principal, error) {
	user, pass, ok := r.BasicAuth()
	if !ok || subtle.ConstantTimeCompare([]byte(pass), l.secret) != 1 {
		return service.Principal{}, ErrUnauthenticated
	}
	return service.Principal{Name: user, Source: l.source}, nil
}

// None accepts every request. It exists for tests and is loopback-only.
type None struct{}

// Authenticate implements Authenticator.
func (None) Authenticate(*http.Request) (service.Principal, error) {
	return service.Principal{
		Name: "anonymous", Source: "loopback",
		Scopes: []store.Scope{{KeyPattern: "*", Permissions: []string{
			service.ActionRead, service.ActionWrite, service.ActionLock, service.ActionAdmin,
		}}},
	}, nil
}

// MTLS maps the client certificate CN to a principal, whose scopes are the
// union of that principal's live tokens. Basic auth may be omitted.
type MTLS struct {
	Tokens store.Store
	Next   Authenticator
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]mtlsEntry
}

type mtlsEntry struct {
	scopes []store.Scope
	at     time.Time
}

// Authenticate implements Authenticator. Basic credentials win when present.
func (m *MTLS) Authenticate(r *http.Request) (service.Principal, error) {
	if m.Next != nil {
		if _, _, ok := r.BasicAuth(); ok {
			return m.Next.Authenticate(r)
		}
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return service.Principal{}, ErrUnauthenticated
	}
	cn := r.TLS.VerifiedChains[0][0].Subject.CommonName
	if cn == "" {
		return service.Principal{}, ErrUnauthenticated
	}
	scopes, err := m.scopes(r, cn)
	if err != nil {
		return service.Principal{}, err
	}
	return service.Principal{Name: cn, Source: "mtls", Scopes: scopes}, nil
}

func (m *MTLS) scopes(r *http.Request, cn string) ([]store.Scope, error) {
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	m.mu.Lock()
	if e, ok := m.cache[cn]; ok && now.Sub(e.at) < cacheTTL {
		m.mu.Unlock()
		return e.scopes, nil
	}
	m.mu.Unlock()

	tokens, err := m.Tokens.ListTokens(r.Context())
	if err != nil {
		return nil, errors.Join(store.ErrUnavailable, err)
	}
	var scopes []store.Scope
	for i := range tokens {
		t := &tokens[i]
		if t.Principal != cn || t.RevokedAt != nil || (t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)) {
			continue
		}
		scopes = append(scopes, t.Scopes...)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cache == nil {
		m.cache = map[string]mtlsEntry{}
	}
	m.cache[cn] = mtlsEntry{scopes: scopes, at: now}
	return scopes, nil
}
