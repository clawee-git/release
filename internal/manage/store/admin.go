package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Admin is one operator account. Accounts are provisioned on the host with
// `clawee-release-manage admin add` and never self-registered
// (release-management.md §6).
type Admin struct {
	Name         string
	PasswordHash string
	// TOTPSecretEnc is the enrolled TOTP secret, sealed with the key derived
	// from the service's secret-key file. Nil until first login enrols. The
	// PLAINTEXT secret never reaches this struct and never reaches the
	// database: a catalog file copied off the host must not be a set of
	// working second factors.
	TOTPSecretEnc  []byte
	TOTPEnrolledAt time.Time
	// TOTPLastStep is the replay watermark: a code whose time step is at or
	// below it never verifies again, which is what stops a code intercepted
	// inside the ±1-step skew window from being reused.
	TOTPLastStep int64
	CreatedAt    time.Time
}

// Enrolled reports whether this admin has completed TOTP enrolment.
func (a Admin) Enrolled() bool { return len(a.TOTPSecretEnc) > 0 }

// CreateAdmin inserts an account. ErrAlreadyExists when the name is taken.
func (s *Store) CreateAdmin(name, passwordHash string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO admins (name, password_hash, created_at) VALUES (?, ?, ?)`,
		name, passwordHash, at.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: admin %q", ErrAlreadyExists, name)
		}
		return fmt.Errorf("store: create admin %q: %w", name, err)
	}
	return nil
}

// Admin returns one account, or ErrNotFound.
func (s *Store) Admin(name string) (*Admin, error) {
	var a Admin
	var enrolled, created int64
	var secret []byte
	err := s.db.QueryRow(`
		SELECT name, password_hash, totp_secret_enc, totp_enrolled_at, totp_last_step, created_at
		FROM admins WHERE name = ?`, name).
		Scan(&a.Name, &a.PasswordHash, &secret, &enrolled, &a.TOTPLastStep, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: admin %q", ErrNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read admin %q: %w", name, err)
	}
	a.TOTPSecretEnc = secret
	if enrolled != 0 {
		a.TOTPEnrolledAt = time.Unix(enrolled, 0).UTC()
	}
	a.CreatedAt = time.Unix(created, 0).UTC()
	return &a, nil
}

// ListAdmins returns every account, oldest first. `admin list` prints it.
func (s *Store) ListAdmins() ([]Admin, error) {
	rows, err := s.db.Query(`
		SELECT name, password_hash, totp_secret_enc, totp_enrolled_at, totp_last_step, created_at
		FROM admins ORDER BY created_at, name`)
	if err != nil {
		return nil, fmt.Errorf("store: list admins: %w", err)
	}
	defer rows.Close()
	var out []Admin
	for rows.Next() {
		var a Admin
		var enrolled, created int64
		var secret []byte
		if err := rows.Scan(&a.Name, &a.PasswordHash, &secret, &enrolled, &a.TOTPLastStep, &created); err != nil {
			return nil, fmt.Errorf("store: scan admin: %w", err)
		}
		a.TOTPSecretEnc = secret
		if enrolled != 0 {
			a.TOTPEnrolledAt = time.Unix(enrolled, 0).UTC()
		}
		a.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAdmin removes an account and, by cascade, its sessions and their CSRF
// tokens. ErrNotFound when the name is unknown — an operator who mistypes a
// name must not be told the account is gone.
func (s *Store) DeleteAdmin(name string) error {
	res, err := s.db.Exec(`DELETE FROM admins WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("store: delete admin %q: %w", name, err)
	}
	return requireOneRow(res, fmt.Errorf("%w: admin %q", ErrNotFound, name))
}

// EnrolTOTP records the sealed secret. It refuses to overwrite an existing
// enrolment: re-enrolment is a deliberate operator act (remove and re-add the
// account), never something a login flow can do, or an attacker who reached
// the password could enrol a second factor of their own.
func (s *Store) EnrolTOTP(name string, sealed []byte, at time.Time) error {
	res, err := s.db.Exec(`
		UPDATE admins SET totp_secret_enc = ?, totp_enrolled_at = ?
		WHERE name = ? AND totp_secret_enc IS NULL`, sealed, at.Unix(), name)
	if err != nil {
		return fmt.Errorf("store: enrol TOTP for %q: %w", name, err)
	}
	return requireOneRow(res, fmt.Errorf("%w: admin %q is unknown or already enrolled", ErrBadState, name))
}

// SetTOTPStep advances the replay watermark. It only ever moves FORWARD (the
// WHERE clause), so two concurrent logins cannot walk it backwards and reopen
// a step that has already been spent.
func (s *Store) SetTOTPStep(name string, step int64) error {
	_, err := s.db.Exec(`UPDATE admins SET totp_last_step = ? WHERE name = ? AND totp_last_step < ?`,
		step, name, step)
	if err != nil {
		return fmt.Errorf("store: advance TOTP watermark for %q: %w", name, err)
	}
	return nil
}

// Session is a browser session. It exists from the password step; MFAOK flips
// at the TOTP step, and only an MFAOK session reaches anything under /manage.
type Session struct {
	ID        string
	Admin     string
	MFAOK     bool
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateSession inserts a session in the pre-MFA state.
func (s *Store) CreateSession(id, admin string, createdAt, expiresAt time.Time) error {
	_, err := s.db.Exec(`INSERT INTO sessions (id, admin, mfa_ok, created_at, expires_at) VALUES (?, ?, 0, ?, ?)`,
		id, admin, createdAt.Unix(), expiresAt.Unix())
	if err != nil {
		return fmt.Errorf("store: create session for %q: %w", admin, err)
	}
	return nil
}

// Session returns a session that is present AND unexpired at now; an expired
// one is ErrNotFound, so no caller can forget to compare the expiry.
func (s *Store) Session(id string, now time.Time) (*Session, error) {
	var sess Session
	var created, expires int64
	err := s.db.QueryRow(`SELECT id, admin, mfa_ok, created_at, expires_at FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.Admin, &sess.MFAOK, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: session", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read session: %w", err)
	}
	sess.CreatedAt = time.Unix(created, 0).UTC()
	sess.ExpiresAt = time.Unix(expires, 0).UTC()
	if !now.Before(sess.ExpiresAt) {
		return nil, fmt.Errorf("%w: session expired", ErrNotFound)
	}
	return &sess, nil
}

// MarkSessionMFA flips a session to fully authenticated.
func (s *Store) MarkSessionMFA(id string) error {
	res, err := s.db.Exec(`UPDATE sessions SET mfa_ok = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: mark session MFA: %w", err)
	}
	return requireOneRow(res, fmt.Errorf("%w: session", ErrNotFound))
}

// DeleteSession ends a session; its CSRF tokens cascade away with it.
func (s *Store) DeleteSession(id string) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// CreateCSRF issues a CSRF token bound to a session.
func (s *Store) CreateCSRF(token, sessionID string, expiresAt time.Time) error {
	if _, err := s.db.Exec(`INSERT INTO csrf (token, session_id, expires_at) VALUES (?, ?, ?)`,
		token, sessionID, expiresAt.Unix()); err != nil {
		return fmt.Errorf("store: issue CSRF token: %w", err)
	}
	return nil
}

// CSRFValid reports whether token is live AND belongs to sessionID. The
// session binding is the whole check: a token that is merely well-formed, or
// merely present in the table, is one an attacker can mint from their own
// session and replay against a victim's.
func (s *Store) CSRFValid(token, sessionID string, now time.Time) (bool, error) {
	var expires int64
	err := s.db.QueryRow(`SELECT expires_at FROM csrf WHERE token = ? AND session_id = ?`, token, sessionID).
		Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: check CSRF token: %w", err)
	}
	return now.Before(time.Unix(expires, 0).UTC()), nil
}

// RecordLoginFailure appends one failed attempt for key.
func (s *Store) RecordLoginFailure(key string, at time.Time) error {
	if _, err := s.db.Exec(`INSERT INTO login_failures (key, at) VALUES (?, ?)`, key, at.Unix()); err != nil {
		return fmt.Errorf("store: record login failure: %w", err)
	}
	return nil
}

// LoginFailures counts the attempts for key at or after since.
func (s *Store) LoginFailures(key string, since time.Time) (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM login_failures WHERE key = ? AND at > ?`,
		key, since.Unix()).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count login failures: %w", err)
	}
	return n, nil
}

// ClearLoginFailures forgets key's attempts. Only a success on the SAME stage
// calls this: a correct password must never clear the second factor's counter
// (see internal/manage/auth).
func (s *Store) ClearLoginFailures(key string) error {
	if _, err := s.db.Exec(`DELETE FROM login_failures WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: clear login failures: %w", err)
	}
	return nil
}

// loginFailureRetention is how long a failure row is kept. It is far longer
// than any rate-limit window — the rows are cheap, and keeping them past the
// window means an operator can still see a brute-force run in the table after
// the limit has aged out.
const loginFailureRetention = 30 * 24 * time.Hour

// PurgeExpired drops sessions, CSRF tokens and nonces that are past their
// expiry, spent nonces and login-failure rows past their retention.
//
// An UNUSED expired nonce goes immediately, but a SPENT one is kept for a
// month first: a replay must stay distinguishable from an unknown nonce in the
// logs while anyone might still be reading them. Keeping it forever was the
// other mistake — the table only ever grew.
func (s *Store) PurgeExpired(now time.Time) error {
	ts := now.Unix()
	stale := now.Add(-loginFailureRetention).Unix()
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{`DELETE FROM sessions WHERE expires_at <= ?`, ts},
		{`DELETE FROM csrf WHERE expires_at <= ?`, ts},
		{`DELETE FROM nonces WHERE expires_at <= ? AND used_at = 0`, ts},
		{`DELETE FROM nonces WHERE used_at != 0 AND used_at <= ?`, stale},
		{`DELETE FROM login_failures WHERE at <= ?`, stale},
	} {
		if _, err := s.db.Exec(q.sql, q.arg); err != nil {
			return fmt.Errorf("store: purge expired: %w", err)
		}
	}
	return nil
}

// requireOneRow turns "the UPDATE/DELETE matched nothing" into the caller's
// error. Without it every such statement succeeds against a row that is not
// there, which is how a mistyped name reads as a completed operation.
func requireOneRow(res sql.Result, missing error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return missing
	}
	return nil
}
