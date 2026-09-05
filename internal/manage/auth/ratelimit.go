package auth

import "time"

// Login attempts are limited per (stage, client IP, account), and the counters
// live in the CATALOG rather than in process memory.
//
// Per PAIR, not per account and not per IP alone. Per account alone lets anyone
// lock out a named admin from anywhere — a denial of service that needs no
// credential at all. Per IP alone lets one host walk the whole admin list. The
// pair bounds a guessing run against one account from one place, which is the
// shape the attack actually has.
//
// PERSISTED because the alternative was a limit a restart erased: an attacker
// who could provoke a crash, or who simply waited for a deploy, resumed their
// run with a clean counter. The rows are two integers and a string, swept by
// PurgeExpired, and the write happens only on a FAILED attempt — the success
// path pays one DELETE.
const (
	loginWindow      = 15 * time.Minute
	loginMaxFailures = 5
)

// allow reports whether another attempt may be made for key at now.
//
// A store error is reported as NOT allowed. That is the fail-closed direction:
// a catalog that cannot be read is a catalog that cannot prove this is not the
// hundredth guess, and an unreadable catalog makes signing in impossible
// anyway.
func (s *Service) allow(key string, now time.Time) bool {
	n, err := s.Store.LoginFailures(key, now.Add(-loginWindow))
	if err != nil {
		return false
	}
	return n < loginMaxFailures
}

// fail records a failed attempt.
func (s *Service) fail(key string, now time.Time) {
	if err := s.Store.RecordLoginFailure(key, now); err != nil {
		// Never silent: a limiter that stopped counting without saying so is a
		// limiter that is not there.
		s.Log.Warn("could not record a failed login attempt; the rate limit is not counting", "err", err)
	}
}

// succeed clears the record for THIS STAGE only. A correct credential is proof
// the run was not an attack, and leaving the counter armed would penalise an
// operator who mistyped four times — but see passwordKey/totpKey: the password
// stage clears only its own key.
func (s *Service) succeed(key string) {
	if err := s.Store.ClearLoginFailures(key); err != nil {
		s.Log.Warn("could not clear login failures", "err", err)
	}
}
