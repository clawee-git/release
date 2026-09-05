// Package store is the manage service's SQLite catalog: release rows, admins,
// sessions, CSRF tokens, register nonces and invites.
//
// Three properties are deliberate and load-bearing:
//
//   - The driver is PURE GO (modernc.org/sqlite). The service is built and
//     cross-compiled by the same kit that builds the components; a cgo driver
//     would make the manage binary the one artifact in this repo that cannot
//     be built without a C toolchain on the build host.
//   - The store is CLOCK-FREE. Every method that records a time takes it from
//     the caller, so a retention or expiry test is arithmetic on values the
//     test chose rather than a sleep. The one exception is nothing: even
//     nonce issue and expiry take `now`.
//   - Migrations are FORWARD-ONLY and ledgered. `migrations` is an append-only
//     table of applied rungs; Open applies every rung above the recorded high
//     water mark, each in its own transaction. There is no down-migration,
//     because a manage database that has served a promote cannot be rolled
//     back to a schema that cannot express what it recorded.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// Errors the handlers map onto status codes. They are values, not strings, so
// a handler distinguishes "no such row" from "this row already exists" without
// matching on driver text that changes between driver versions.
var (
	// ErrNotFound: the addressed row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrAlreadyExists: a row with the same natural key already exists. The
	// register endpoint turns this into 409 — a re-registered stamp is a
	// re-run of a cut whose row is already recorded, not a new release.
	ErrAlreadyExists = errors.New("store: already exists")
	// ErrBadState: the requested transition is not legal from the row's
	// current state.
	ErrBadState = errors.New("store: illegal state transition")
	// ErrBadValue: a field outside the closed catalog vocabulary.
	ErrBadValue = errors.New("store: value outside the catalog vocabulary")
)

// DBFile is the catalog's filename under the data dir. It is a CONSTANT, not a
// flag: --data-dir is the one steerable root (privilege.md — a root validated
// at its own writer), and a second flag naming the file inside it buys nothing
// but a way for two processes to open two catalogs in one directory.
const DBFile = "catalog.db"

// Store is the catalog handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if absent) the catalog under dataDir and applies every
// pending migration.
//
// The pragmas are set in the DSN rather than as post-open statements because
// database/sql pools connections: a PRAGMA run once after Open applies to
// whichever single connection served it, and every later connection silently
// runs with the defaults. That is how a "WAL mode" database ends up in
// rollback-journal mode under load.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("store: data dir is empty")
	}
	path := filepath.Join(dataDir, DBFile)
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	// SQLite tolerates concurrent readers in WAL but serialises writers; a
	// bounded pool plus busy_timeout is the difference between a queued write
	// and a "database is locked" error surfacing to an operator.
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for the few places that need a transaction the store
// does not already wrap. It exists for batch B's promote, which must flip a
// row and write an audit line in one transaction with work this package does
// not own.
func (s *Store) DB() *sql.DB { return s.db }

// migration is one rung of the forward-only ladder. Version numbers are dense
// and never reused; the name is what the ledger records for a human reading
// the table.
type migration struct {
	version int
	name    string
	stmts   []string
}

// migrations is APPEND-ONLY. Editing a landed rung changes the schema of hosts
// that have not applied it while leaving hosts that have applied it untouched,
// and the ledger records the same version for both — the divergence is
// undetectable afterwards (migrations.md). New work is a new rung.
var migrations = []migration{
	{
		version: 1,
		name:    "initial catalog",
		stmts: []string{
			// release_versions is the catalog. artifacts_json is the register
			// payload's Artifacts array verbatim: the store is deliberately
			// opaque to it, because the key layout is the CUT's contract with
			// the staging bucket and a store that re-derived it could disagree
			// with where the bytes actually are.
			`CREATE TABLE release_versions (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				component       TEXT    NOT NULL,
				channel         TEXT    NOT NULL,
				version         TEXT    NOT NULL,
				stamp           TEXT    NOT NULL,
				artifacts_json  TEXT    NOT NULL,
				sums_key        TEXT    NOT NULL,
				minisig_key     TEXT    NOT NULL,
				state           TEXT    NOT NULL,
				is_current      INTEGER NOT NULL DEFAULT 0,
				created_at      INTEGER NOT NULL,
				promoted_at     INTEGER NOT NULL DEFAULT 0,
				yanked_at       INTEGER NOT NULL DEFAULT 0
			)`,
			// One row per cut. The register endpoint's 409 is this index: a
			// re-run of --distribute-only must not produce a second row for
			// bytes that already have one.
			`CREATE UNIQUE INDEX release_versions_stamp
				ON release_versions (component, channel, stamp)`,
			// At most ONE current row per (component, channel). is_current is
			// what the channel manifest names, so two of them is two answers
			// to "what does this channel serve" — an invariant worth having
			// the database refuse rather than a code path remember.
			`CREATE UNIQUE INDEX release_versions_current
				ON release_versions (component, channel) WHERE is_current = 1`,
			`CREATE INDEX release_versions_lookup
				ON release_versions (component, channel, state, created_at DESC)`,

			// Admins are provisioned on the host, never self-registered
			// (release-management.md §6). totp_secret_enc is NULL until first
			// login enrols; totp_last_step is the replay watermark.
			`CREATE TABLE admins (
				name             TEXT    PRIMARY KEY,
				password_hash    TEXT    NOT NULL,
				totp_secret_enc  BLOB,
				totp_enrolled_at INTEGER NOT NULL DEFAULT 0,
				totp_last_step   INTEGER NOT NULL DEFAULT 0,
				created_at       INTEGER NOT NULL
			)`,

			// A session exists from the password step; mfa_ok flips at the
			// TOTP step. It is one row rather than two tables because the
			// half-authenticated state is exactly what the TOTP page needs to
			// address, and a separate "pending" table is the same row with a
			// second name and a second expiry to keep in step.
			`CREATE TABLE sessions (
				id         TEXT    PRIMARY KEY,
				admin      TEXT    NOT NULL REFERENCES admins(name) ON DELETE CASCADE,
				mfa_ok     INTEGER NOT NULL DEFAULT 0,
				created_at INTEGER NOT NULL,
				expires_at INTEGER NOT NULL
			)`,
			`CREATE INDEX sessions_admin ON sessions (admin)`,

			// CSRF tokens are their own rows so a token can be rotated without
			// re-issuing the session cookie, and so a token cannot outlive the
			// session it belongs to (ON DELETE CASCADE).
			`CREATE TABLE csrf (
				token      TEXT    PRIMARY KEY,
				session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
				expires_at INTEGER NOT NULL
			)`,
			`CREATE INDEX csrf_session ON csrf (session_id)`,

			// A register nonce is single-use: used_at is set in the same
			// transaction that consumes it, and the endpoint refuses a nonce
			// whose used_at is non-zero. Rows are kept after use rather than
			// deleted so a replay is DISTINGUISHABLE from an unknown nonce in
			// the logs — deleting makes an attack look like a typo.
			`CREATE TABLE nonces (
				nonce      TEXT    PRIMARY KEY,
				created_at INTEGER NOT NULL,
				expires_at INTEGER NOT NULL,
				used_at    INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX nonces_expiry ON nonces (expires_at)`,

			// Invites are batch B's surface; the table lands here so the
			// schema is one ledger rung rather than two, and so the manage
			// page's invites tab has something to list against from day one.
			`CREATE TABLE invites (
				id                 INTEGER PRIMARY KEY AUTOINCREMENT,
				release_version_id INTEGER NOT NULL REFERENCES release_versions(id),
				minted_by          TEXT    NOT NULL,
				script_key         TEXT    NOT NULL,
				url                TEXT    NOT NULL,
				created_at         INTEGER NOT NULL,
				expires_at         INTEGER NOT NULL
			)`,
			`CREATE INDEX invites_row ON invites (release_version_id)`,
			`CREATE INDEX invites_expiry ON invites (expires_at)`,
		},
	},
	{
		version: 2,
		name:    "persisted login failure counters",
		stmts: []string{
			// The login rate limit lives here rather than in process memory:
			// unpersisted, a restart cleared every counter, so a brute-force
			// run could be resumed by whatever made the service restart. One
			// row per failed attempt, swept by PurgeExpired.
			`CREATE TABLE login_failures (
				id      INTEGER PRIMARY KEY AUTOINCREMENT,
				key     TEXT    NOT NULL,
				at      INTEGER NOT NULL
			)`,
			`CREATE INDEX login_failures_key ON login_failures (key, at)`,
		},
	},
}

// migrate applies every rung above the recorded high water mark.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT    NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create migrations ledger: %w", err)
	}
	var applied int
	// COALESCE, not sql.NullInt64: an empty ledger is version 0, and the
	// scan-into-null dance is one more shape to get wrong for no gain.
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&applied); err != nil {
		return fmt.Errorf("store: read migrations ledger: %w", err)
	}
	for _, m := range migrations {
		if m.version <= applied {
			continue
		}
		if err := s.applyMigration(m); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(m migration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin migration %d: %w", m.version, err)
	}
	defer tx.Rollback()
	for i, stmt := range m.stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("store: migration %d (%s) statement %d: %w", m.version, m.name, i+1, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO migrations (version, name, applied_at) VALUES (?, ?, unixepoch())`,
		m.version, m.name); err != nil {
		return fmt.Errorf("store: record migration %d: %w", m.version, err)
	}
	return tx.Commit()
}

// AppliedMigrations returns the ledger, oldest first. The `version` verb
// prints it, so an operator can tell which rungs a host has run without
// opening the database.
func (s *Store) AppliedMigrations() ([]string, error) {
	rows, err := s.db.Query(`SELECT version, name FROM migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("store: list migrations: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v int
		var name string
		if err := rows.Scan(&v, &name); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%d %s", v, name))
	}
	return out, rows.Err()
}
