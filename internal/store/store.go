// Package store keeps the questions agents leave for the user, in one
// SQLite file shared by the hooks and every panel.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const schema = `
CREATE TABLE IF NOT EXISTS items (
  id             INTEGER PRIMARY KEY,
  rex_session    TEXT NOT NULL DEFAULT '',
  rex_block      TEXT NOT NULL DEFAULT '',
  claude_session TEXT NOT NULL,
  source_msg     TEXT NOT NULL,
  seq            INTEGER NOT NULL,
  cwd            TEXT NOT NULL DEFAULT '',
  branch         TEXT NOT NULL DEFAULT '',
  project        TEXT NOT NULL DEFAULT '',
  kind           TEXT NOT NULL,
  title          TEXT NOT NULL,
  body           TEXT NOT NULL DEFAULT '',
  context        TEXT NOT NULL DEFAULT '',
  prompt         TEXT NOT NULL DEFAULT '',
  replies        TEXT NOT NULL DEFAULT '[]',
  suggested      INTEGER NOT NULL DEFAULT -1,
  images         TEXT NOT NULL DEFAULT '[]',
  state          TEXT NOT NULL DEFAULT 'open',
  snooze_until   INTEGER NOT NULL DEFAULT 0,
  snooze_trigger TEXT NOT NULL DEFAULT '',
  answered_with  TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  agent          TEXT NOT NULL DEFAULT '',
  UNIQUE (claude_session, source_msg, seq)
);
CREATE INDEX IF NOT EXISTS items_by_session ON items (rex_session, state);
CREATE TABLE IF NOT EXISTS last_prompts (
  claude_session TEXT PRIMARY KEY,
  prompt         TEXT NOT NULL,
  updated_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pending (
  id             INTEGER PRIMARY KEY,
  claude_session TEXT NOT NULL DEFAULT '',
  payload        TEXT NOT NULL,
  attempts       INTEGER NOT NULL DEFAULT 0,
  next_at        INTEGER NOT NULL DEFAULT 0,
  created_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS backfilled (
  claude_session TEXT PRIMARY KEY,
  at             INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS health (
  id             INTEGER PRIMARY KEY CHECK (id = 1),
  last_error     TEXT NOT NULL DEFAULT '',
  last_error_at  INTEGER NOT NULL DEFAULT 0,
  last_ok_at     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
  key            TEXT PRIMARY KEY,
  value          TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS panes (
  block          TEXT PRIMARY KEY,
  claude_session TEXT NOT NULL,
  rex_session    TEXT NOT NULL,
  claimed_at     INTEGER NOT NULL
);`

// schemaVersion is the PRAGMA user_version of the schema above. A change
// to the schema that older binaries can't use raises it.
//
// Version 2 added items.agent: "" for Claude Code, "pi" for Pi, and the
// settings table. A version 1
// store is migrated by adding the column, so its rows are Claude's, and by
// renaming the prompts table to last_prompts. Every item query of a
// version 1 rexq reads prompts, so a version 1 panel that had the store
// open when it moved on fails to load, rather than show Pi's questions as
// Claude's and answer them with claude --resume. A version 1 rexq that
// opens the store anew refuses it for its version.
const schemaVersion = 2

// ErrNewerStore is returned by Open for a store a newer rexq wrote.
var ErrNewerStore = errors.New("store was written by a newer rexq")

// Store is the shared questions database.
type Store struct{ db *sql.DB }

// DefaultPath is where the store lives unless REXQ_DB says otherwise.
func DefaultPath() string {
	if p := os.Getenv("REXQ_DB"); p != "" {
		return p
	}
	return defaultLocation()
}

// defaultLocation is the store's place when REXQ_DB is unset.
func defaultLocation() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "rex-questions", "questions.db")
	}
	return filepath.Join(home, ".local", "state", "rex-questions", "questions.db")
}

// DefaultBusyTimeout is how long Open waits for a lock another process
// holds before a write fails with SQLITE_BUSY.
const DefaultBusyTimeout = 5 * time.Second

// Open opens or creates the store at path.
func Open(path string) (*Store, error) {
	return OpenWith(path, DefaultBusyTimeout)
}

// OpenWith is Open with another busy timeout. The synchronous hooks use a
// short one: Claude waits for them, and kills them after their own timeout.
func OpenWith(path string, busyTimeout time.Duration) (*Store, error) {
	// Absolute, because a relative path in a file URL is read as a host.
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := private(path); err != nil {
		return nil, err
	}
	// A URL, so a path holding ?, # or % is not read as query or fragment.
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: fmt.Sprintf("_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)", busyTimeout.Milliseconds())}).String()
	// Many processes creating a new file at once can see SQLITE_BUSY while
	// one of them switches it to WAL, before busy_timeout applies. Retry.
	delay := 20 * time.Millisecond
	for attempt := 1; ; attempt++ {
		s, err := open(dsn)
		if err == nil || !isBusy(err) || attempt == openAttempts {
			return s, err
		}
		time.Sleep(delay)
		delay *= 2
	}
}

// private makes sure only the user can read the store at path: what agents
// and the user wrote is in it. It creates the directory 0700 and the
// database 0600 (SQLite gives its -wal and -shm files the database's mode),
// and tightens them if an earlier version left them looser. An existing
// directory is tightened only if it is the default one: REXQ_DB may point
// into a shared directory such as /tmp.
func private(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if dir == filepath.Dir(defaultLocation()) {
		if err := tighten(dir, 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := tighten(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// tighten drops the permission bits of path that perm doesn't allow.
func tighten(path string, perm os.FileMode) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&^perm == 0 {
		return nil
	}
	return os.Chmod(path, fi.Mode().Perm()&perm)
}

// openAttempts is how often Open tries a busy database before giving up.
const openAttempts = 5

func open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: PRAGMA data_version is per connection, and the panel
	// relies on it to notice writes from the hooks.
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > schemaVersion {
		db.Close()
		return nil, fmt.Errorf("%w (schema version %d; this one knows up to %d): update rexq", ErrNewerStore, version, schemaVersion)
	}
	if version < schemaVersion {
		if err := migrate(db); err != nil {
			db.Close()
			return nil, err
		}
	} else if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate brings a new store, or one of an older version, to this schema,
// in one write transaction, so two processes opening an old store at once
// migrate it once. It sets the version only then: every write wakes the
// panels.
func migrate(db *sql.DB) error {
	// One connection (see open), so BEGIN IMMEDIATE and what follows run
	// on it. IMMEDIATE takes the write lock first, so the version read
	// below is the one this migration changes.
	if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			db.Exec("ROLLBACK")
		}
	}()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("%w (schema version %d; this one knows up to %d): update rexq", ErrNewerStore, version, schemaVersion)
	}
	if version < schemaVersion {
		// Stores of version 0 (from before versioning) and 1 have the
		// version 1 schema, or none yet. CREATE TABLE IF NOT EXISTS would
		// leave a version 1 table as it is, so its changes come first.
		has := func(q string) (bool, error) {
			var n int
			err := db.QueryRow(q).Scan(&n)
			return n > 0, err
		}
		old, err := has(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'prompts'`)
		if err != nil {
			return err
		}
		if old {
			if _, err := db.Exec(`ALTER TABLE prompts RENAME TO last_prompts`); err != nil {
				return err
			}
		}
		items, err := has(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'items'`)
		if err != nil {
			return err
		}
		agent, err := has(`SELECT count(*) FROM pragma_table_info('items') WHERE name = 'agent'`)
		if err != nil {
			return err
		}
		if items && !agent {
			if _, err := db.Exec(`ALTER TABLE items ADD COLUMN agent TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(schema); err != nil {
			return err
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return err
		}
	}
	if _, err := db.Exec("COMMIT"); err != nil {
		return err
	}
	done = true
	return nil
}

// isBusy reports whether err is SQLITE_BUSY or one of its extended codes.
func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DataVersion changes whenever another connection writes to the database.
func (s *Store) DataVersion() (int64, error) {
	var v int64
	err := s.db.QueryRow("PRAGMA data_version").Scan(&v)
	return v, err
}
