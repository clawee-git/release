package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// IssueNonce records a freshly minted register nonce.
func (s *Store) IssueNonce(nonce string, createdAt, expiresAt time.Time) error {
	if _, err := s.db.Exec(`INSERT INTO nonces (nonce, created_at, expires_at) VALUES (?, ?, ?)`,
		nonce, createdAt.Unix(), expiresAt.Unix()); err != nil {
		return fmt.Errorf("store: issue nonce: %w", err)
	}
	return nil
}

// ConsumeNonce spends a nonce, in one transaction, and reports why it could
// not be spent when it could not.
//
// The check and the spend MUST be one transaction: two cuts racing the same
// captured nonce through a read-then-write would both see used_at = 0 and both
// register a row, which is exactly the replay the nonce exists to prevent. The
// UPDATE carries its own predicate so the database, not this function's
// reasoning, is what enforces single use.
//
// Unknown, expired and already-used are three different errors on purpose. The
// endpoint answers 403 to all three — an attacker learns nothing — but the log
// line an operator reads afterwards has to tell a clock-skewed build host from
// a replayed capture.
func (s *Store) ConsumeNonce(nonce string, now time.Time) error {
	return s.tx(func(tx *sql.Tx) error {
		var expires, used int64
		err := tx.QueryRow(`SELECT expires_at, used_at FROM nonces WHERE nonce = ?`, nonce).
			Scan(&expires, &used)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: nonce", ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: consume nonce: %w", err)
		}
		if used != 0 {
			return fmt.Errorf("%w: nonce was already used at %s", ErrBadState, time.Unix(used, 0).UTC().Format(time.RFC3339))
		}
		if !now.Before(time.Unix(expires, 0).UTC()) {
			return fmt.Errorf("%w: nonce expired at %s", ErrBadState, time.Unix(expires, 0).UTC().Format(time.RFC3339))
		}
		res, err := tx.Exec(`UPDATE nonces SET used_at = ? WHERE nonce = ? AND used_at = 0`, now.Unix(), nonce)
		if err != nil {
			return fmt.Errorf("store: consume nonce: %w", err)
		}
		return requireOneRow(res, fmt.Errorf("%w: nonce was used concurrently", ErrBadState))
	})
}
