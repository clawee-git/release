package auth

import (
	"sync"
	"time"
)

// Login attempts are limited per (client IP, account name).
//
// Per PAIR, not per account and not per IP alone. Per account alone lets
// anyone lock out a named admin from anywhere — a denial of service that needs
// no credential at all. Per IP alone lets one host walk the whole admin list.
// The pair bounds a guessing run against one account from one place, which is
// the shape the attack actually has.
const (
	loginWindow      = 15 * time.Minute
	loginMaxFailures = 5
)

type limiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{failures: make(map[string][]time.Time)} }

// allow reports whether another attempt may be made for key at now, pruning
// attempts that have aged out of the window.
func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key, now)) < loginMaxFailures
}

// fail records a failed attempt.
func (l *limiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[key] = append(l.prune(key, now), now)
}

// succeed clears the record: a correct credential is proof the run was not an
// attack, and leaving the counter armed would penalise an operator who
// mistyped a password four times.
func (l *limiter) succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

// prune must be called with the mutex held. It also deletes emptied keys, so
// the map does not grow without bound under a walk of made-up account names.
func (l *limiter) prune(key string, now time.Time) []time.Time {
	cutoff := now.Add(-loginWindow)
	kept := l.failures[key][:0]
	for _, t := range l.failures[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, key)
		return nil
	}
	l.failures[key] = kept
	return kept
}
