package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/clawee-git/release/internal/manage/catalog"
)

// ReleaseVersion is one cut, as the catalog records it (release-management.md
// §2). One row per (component, channel, stamp).
type ReleaseVersion struct {
	ID        int64
	Component string
	Channel   string
	Version   string // human semver, e.g. 0.2.28
	Stamp     string // the full cut stamp
	// ArtifactsJSON is the register payload's artifacts array VERBATIM. The
	// store never parses it: the object keys in it are the cut's contract with
	// the staging bucket, and a store that re-derived them could disagree with
	// where the bytes actually are.
	ArtifactsJSON string
	SumsKey       string
	MinisigKey    string
	State         string
	IsCurrent     bool
	CreatedAt     time.Time
	PromotedAt    time.Time // zero until promoted
	YankedAt      time.Time // zero until yanked
}

// Stage inserts a new row in state `staged`. The caller supplies CreatedAt.
//
// Returns ErrAlreadyExists when (component, channel, stamp) is already
// catalogued — a re-run of the cut's distribute step, not a new release — and
// ErrBadValue for a component or channel outside the vocabulary, or a stamp
// that contradicts the claimed channel.
func (s *Store) Stage(rv ReleaseVersion) (int64, error) {
	if !catalog.ValidComponent(rv.Component) {
		return 0, fmt.Errorf("%w: component %q", ErrBadValue, rv.Component)
	}
	if !catalog.ValidChannel(rv.Channel) {
		return 0, fmt.Errorf("%w: channel %q", ErrBadValue, rv.Channel)
	}
	if !catalog.StampMatchesChannel(rv.Stamp, rv.Channel) {
		return 0, fmt.Errorf("%w: stamp %q is not a %s stamp", ErrBadValue, rv.Stamp, rv.Channel)
	}
	res, err := s.db.Exec(`
		INSERT INTO release_versions
			(component, channel, version, stamp, artifacts_json, sums_key, minisig_key,
			 state, is_current, created_at, promoted_at, yanked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, 0)`,
		rv.Component, rv.Channel, rv.Version, rv.Stamp, rv.ArtifactsJSON,
		rv.SumsKey, rv.MinisigKey, catalog.StateStaged, rv.CreatedAt.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w: %s %s on %s", ErrAlreadyExists, rv.Component, rv.Stamp, rv.Channel)
		}
		return 0, fmt.Errorf("store: stage %s %s: %w", rv.Component, rv.Stamp, err)
	}
	return res.LastInsertId()
}

// Promote flips id to `public`, sets is_current, and clears the previous
// current row for the same (component, channel) — in ONE transaction, with the
// clear happening first, because the partial unique index refuses two current
// rows and the whole point of the transaction is that there is never a moment
// with none.
//
// It refuses a row that is not `staged`: promote is the go-live, and a public,
// yanked or expired row reaching it again means something upstream lost track
// of the row's state.
//
// It does NOT touch any bucket. Everything remote is batch B's, and this
// method is deliberately the LAST step of that sequence (verify → copy →
// GitHub → manifest → flip), so a failure anywhere before it leaves the row
// `staged` and the public surface untouched.
func (s *Store) Promote(id int64, at time.Time) error {
	return s.tx(func(tx *sql.Tx) error {
		var component, channel, state string
		err := tx.QueryRow(`SELECT component, channel, state FROM release_versions WHERE id = ?`, id).
			Scan(&component, &channel, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: release row %d", ErrNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("store: promote %d: %w", id, err)
		}
		if state != catalog.StateStaged {
			return fmt.Errorf("%w: row %d is %s, only a staged row can be promoted", ErrBadState, id, state)
		}
		if _, err := tx.Exec(`
			UPDATE release_versions SET is_current = 0
			WHERE component = ? AND channel = ? AND is_current = 1`, component, channel); err != nil {
			return fmt.Errorf("store: promote %d: clear previous current: %w", id, err)
		}
		if _, err := tx.Exec(`
			UPDATE release_versions SET state = ?, is_current = 1, promoted_at = ?
			WHERE id = ?`, catalog.StatePublic, at.Unix(), id); err != nil {
			return fmt.Errorf("store: promote %d: %w", id, err)
		}
		return nil
	})
}

// Yank marks id `yanked` and clears is_current. Re-pointing the channel
// manifest at the newest remaining public row is the CALLER's step — this
// method reports that row so the caller has it without a second query.
//
// Only a public row can be yanked: yanking a staged row would be a no-op with
// a state change, and yanking an already-yanked one hides a double-click.
func (s *Store) Yank(id int64, at time.Time) (successor *ReleaseVersion, err error) {
	err = s.tx(func(tx *sql.Tx) error {
		var component, channel, state string
		e := tx.QueryRow(`SELECT component, channel, state FROM release_versions WHERE id = ?`, id).
			Scan(&component, &channel, &state)
		if errors.Is(e, sql.ErrNoRows) {
			return fmt.Errorf("%w: release row %d", ErrNotFound, id)
		}
		if e != nil {
			return fmt.Errorf("store: yank %d: %w", id, e)
		}
		if state != catalog.StatePublic {
			return fmt.Errorf("%w: row %d is %s, only a public row can be yanked", ErrBadState, id, state)
		}
		if _, e := tx.Exec(`
			UPDATE release_versions SET state = ?, is_current = 0, yanked_at = ?
			WHERE id = ?`, catalog.StateYanked, at.Unix(), id); e != nil {
			return fmt.Errorf("store: yank %d: %w", id, e)
		}
		rows, e := queryVersions(tx, `
			SELECT `+versionCols+` FROM release_versions
			WHERE component = ? AND channel = ? AND state = ? AND id != ?
			ORDER BY created_at DESC, id DESC LIMIT 1`,
			component, channel, catalog.StatePublic, id)
		if e != nil {
			return e
		}
		if len(rows) == 1 {
			successor = &rows[0]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return successor, nil
}

// ExpireOldVersions runs retention for one (component, channel): it orders that
// channel's promoted rows newest first, keeps `keep` of them, and marks the
// rest `expired`. It returns the rows it newly expired so the caller can prune
// their bytes.
//
// Three scoping rules, each of which was a real defect somewhere:
//
//   - Per CHANNEL. A component's beta rows never count toward its stable
//     keep-window; unscoped, a busy channel pushes the other channel's rows
//     out of their own window.
//   - The CURRENT row is never expired, however old it is
//     (release-management.md §2). The manifest names it.
//   - `staged` rows are not counted and not expired. The guideline keeps every
//     staged row: a staged row is the only thing an invite can point at, and
//     it is the row an operator has not decided about yet.
func (s *Store) ExpireOldVersions(component, channel string, keep int, at time.Time) ([]ReleaseVersion, error) {
	if !catalog.ValidComponent(component) || !catalog.ValidChannel(channel) {
		return nil, fmt.Errorf("%w: %s/%s", ErrBadValue, component, channel)
	}
	if keep < 1 {
		return nil, fmt.Errorf("store: retention keep must be >= 1, got %d", keep)
	}
	var expired []ReleaseVersion
	err := s.tx(func(tx *sql.Tx) error {
		candidates, e := queryVersions(tx, `
			SELECT `+versionCols+` FROM release_versions
			WHERE component = ? AND channel = ? AND state IN (?, ?)
			ORDER BY created_at DESC, id DESC`,
			component, channel, catalog.StatePublic, catalog.StateYanked)
		if e != nil {
			return e
		}
		kept := 0
		for _, rv := range candidates {
			if rv.IsCurrent {
				// Never expired, and never counted against the window either:
				// the current row is not one of the N most recent releases,
				// it is the one the channel serves.
				continue
			}
			if kept < keep {
				kept++
				continue
			}
			if _, e := tx.Exec(`UPDATE release_versions SET state = ? WHERE id = ?`,
				catalog.StateExpired, rv.ID); e != nil {
				return fmt.Errorf("store: expire %d: %w", rv.ID, e)
			}
			rv.State = catalog.StateExpired
			expired = append(expired, rv)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return expired, nil
}

// CurrentPublic returns the one public, is_current row for (component,
// channel), or ErrNotFound.
func (s *Store) CurrentPublic(component, channel string) (*ReleaseVersion, error) {
	rows, err := queryVersions(s.db, `
		SELECT `+versionCols+` FROM release_versions
		WHERE component = ? AND channel = ? AND state = ? AND is_current = 1`,
		component, channel, catalog.StatePublic)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: no current %s release on %s", ErrNotFound, component, channel)
	}
	return &rows[0], nil
}

// Unpromoted returns the newest `staged` row for (component, channel), or
// ErrNotFound. It is the row the manage card offers promote and mint on.
func (s *Store) Unpromoted(component, channel string) (*ReleaseVersion, error) {
	rows, err := queryVersions(s.db, `
		SELECT `+versionCols+` FROM release_versions
		WHERE component = ? AND channel = ? AND state = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`,
		component, channel, catalog.StateStaged)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: no staged %s release on %s", ErrNotFound, component, channel)
	}
	return &rows[0], nil
}

// ListByComponent returns every row for (component, channel), all states,
// newest first. It backs the history page.
func (s *Store) ListByComponent(component, channel string) ([]ReleaseVersion, error) {
	return queryVersions(s.db, `
		SELECT `+versionCols+` FROM release_versions
		WHERE component = ? AND channel = ?
		ORDER BY created_at DESC, id DESC`, component, channel)
}

// Get returns one row by id, or ErrNotFound.
func (s *Store) Get(id int64) (*ReleaseVersion, error) {
	rows, err := queryVersions(s.db, `SELECT `+versionCols+` FROM release_versions WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: release row %d", ErrNotFound, id)
	}
	return &rows[0], nil
}

// versionCols is the column list every ReleaseVersion query selects, in the
// order scanVersion reads them. One string, one scan function: a column added
// to one and not the other is a compile-time-invisible mis-scan, and this is
// the cheapest way to make them impossible to write down separately.
const versionCols = `id, component, channel, version, stamp, artifacts_json,
	sums_key, minisig_key, state, is_current, created_at, promoted_at, yanked_at`

// querier is the shared surface of *sql.DB and *sql.Tx, so the same read runs
// inside a transaction or outside one.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func queryVersions(q querier, query string, args ...any) ([]ReleaseVersion, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query release rows: %w", err)
	}
	defer rows.Close()
	var out []ReleaseVersion
	for rows.Next() {
		var rv ReleaseVersion
		var created, promoted, yanked int64
		if err := rows.Scan(&rv.ID, &rv.Component, &rv.Channel, &rv.Version, &rv.Stamp,
			&rv.ArtifactsJSON, &rv.SumsKey, &rv.MinisigKey, &rv.State, &rv.IsCurrent,
			&created, &promoted, &yanked); err != nil {
			return nil, fmt.Errorf("store: scan release row: %w", err)
		}
		rv.CreatedAt = time.Unix(created, 0).UTC()
		if promoted != 0 {
			rv.PromotedAt = time.Unix(promoted, 0).UTC()
		}
		if yanked != 0 {
			rv.YankedAt = time.Unix(yanked, 0).UTC()
		}
		out = append(out, rv)
	}
	return out, rows.Err()
}

// tx runs fn in a transaction, rolling back on error.
func (s *Store) tx(fn func(*sql.Tx) error) error {
	t, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(t); err != nil {
		t.Rollback()
		return err
	}
	return t.Commit()
}

// isUniqueViolation recognises a UNIQUE constraint failure without depending
// on the driver's error type. The driver's own error struct is not part of its
// documented surface and its numeric codes moved between releases; the message
// prefix SQLite itself emits has not.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ByVersion resolves the newest row for (component, channel, version).
//
// Newest rather than "the one", because `version` is the human semver and a
// re-cut of the same semver is a second row with a different stamp. The invite
// mint addresses a build by the version an operator reads off the page, so it
// gets the most recent one — and the mint records the row ID, so what was
// minted stays unambiguous afterwards.
func (s *Store) ByVersion(component, channel, version string) (*ReleaseVersion, error) {
	rows, err := queryVersions(s.db, `
		SELECT `+versionCols+` FROM release_versions
		WHERE component = ? AND channel = ? AND version = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, component, channel, version)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: %s %s on %s", ErrNotFound, component, version, channel)
	}
	return &rows[0], nil
}
