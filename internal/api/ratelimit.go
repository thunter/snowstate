package api

import (
	"sync"
	"time"
)

// RateLimiter blocks an IP after too many failed logins in one minute.
type RateLimiter struct {
	limit int
	now   func() time.Time

	mu     sync.Mutex
	counts map[string]*window
}

type window struct {
	start time.Time
	fails int
}

// maxTrackedIPs bounds memory when an attacker sprays source addresses.
const maxTrackedIPs = 100000

// NewRateLimiter allows limit failures per minute per IP. Zero disables it.
func NewRateLimiter(limit int) *RateLimiter {
	return &RateLimiter{limit: limit, now: time.Now, counts: map[string]*window{}}
}

// Allow reports whether the IP may attempt authentication.
func (l *RateLimiter) Allow(ip string) bool {
	if l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.counts[ip]
	if w == nil {
		return true
	}
	if l.now().Sub(w.start) >= time.Minute {
		delete(l.counts, ip)
		return true
	}
	return w.fails < l.limit
}

// Fail records a failed attempt.
func (l *RateLimiter) Fail(ip string) {
	if l.limit <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.counts[ip]
	if w == nil || now.Sub(w.start) >= time.Minute {
		if len(l.counts) >= maxTrackedIPs {
			for k, v := range l.counts {
				if now.Sub(v.start) >= time.Minute {
					delete(l.counts, k)
				}
			}
		}
		if len(l.counts) >= maxTrackedIPs {
			return
		}
		w = &window{start: now}
		l.counts[ip] = w
	}
	w.fails++
}
