package store

import (
	"fmt"
	"time"
)

// Invite is one minted install link (release-management.md §4). Batch B owns
// the minting; the table and these two methods land with the schema so the
// manage page's invites listing has a real surface to read from, and so the
// audit shape is fixed before the code that writes it exists.
type Invite struct {
	ID        int64
	RowID     int64  // the release_versions row the link installs
	MintedBy  string // the admin who minted it
	ScriptKey string // the random single-use staging key install.sh was uploaded to
	URL       string // the presigned URL handed to the operator
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Live reports whether the link is still usable at now. The listing serves
// copy-again ONLY for a live link: offering the command for a dead link is an
// operator handing someone a URL that answers 403.
func (i Invite) Live(now time.Time) bool { return now.Before(i.ExpiresAt) }

// CreateInvite records a mint. The audit row IS the invite row: there is no
// per-link revocation, so the time bound and this record are the entire
// control surface, and a mint whose record fails to write must fail outright
// rather than hand out an unaudited link.
func (s *Store) CreateInvite(inv Invite) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO invites (release_version_id, minted_by, script_key, url, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		inv.RowID, inv.MintedBy, inv.ScriptKey, inv.URL, inv.CreatedAt.Unix(), inv.ExpiresAt.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: record invite for row %d: %w", inv.RowID, err)
	}
	return res.LastInsertId()
}

// ListInvites returns every mint, newest first.
func (s *Store) ListInvites() ([]Invite, error) {
	rows, err := s.db.Query(`
		SELECT id, release_version_id, minted_by, script_key, url, created_at, expires_at
		FROM invites ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list invites: %w", err)
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var inv Invite
		var created, expires int64
		if err := rows.Scan(&inv.ID, &inv.RowID, &inv.MintedBy, &inv.ScriptKey, &inv.URL, &created, &expires); err != nil {
			return nil, fmt.Errorf("store: scan invite: %w", err)
		}
		inv.CreatedAt = time.Unix(created, 0).UTC()
		inv.ExpiresAt = time.Unix(expires, 0).UTC()
		out = append(out, inv)
	}
	return out, rows.Err()
}
