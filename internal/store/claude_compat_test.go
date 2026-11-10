package store

// Characterization tests: a store written by the rexq before Pi support
// (schema version 1) opens with every row reading as it did. They use only
// APIs that existed then, so they also run against the commit before it.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// schemaV1 is the schema of rexq before Pi support, verbatim.
const schemaV1 = `
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
  UNIQUE (claude_session, source_msg, seq)
);
CREATE INDEX IF NOT EXISTS items_by_session ON items (rex_session, state);
CREATE TABLE IF NOT EXISTS prompts (
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
CREATE TABLE IF NOT EXISTS panes (
  block          TEXT PRIMARY KEY,
  claude_session TEXT NOT NULL,
  rex_session    TEXT NOT NULL,
  claimed_at     INTEGER NOT NULL
);
PRAGMA user_version = 1;`

// v1Tables are the tables of schemaV1 and their columns, in order.
var v1Tables = map[string]string{
	"items": `id, rex_session, rex_block, claude_session, source_msg, seq, cwd, branch, project, kind, title,
		body, context, prompt, replies, suggested, images, state, snooze_until, snooze_trigger, answered_with, created_at`,
	"prompts":    "claude_session, prompt, updated_at",
	"pending":    "id, claude_session, payload, attempts, next_at, created_at",
	"backfilled": "claude_session, at",
	"health":     "id, last_error, last_error_at, last_ok_at",
	"panes":      "block, claude_session, rex_session, claimed_at",
}

// writeV1Store makes a version 1 store at path, as the old rexq left it,
// with rows in every table.
func writeV1Store(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := schemaV1 + `
INSERT INTO items (id, rex_session, rex_block, claude_session, source_msg, seq, cwd, branch, project, kind, title,
  body, context, prompt, replies, suggested, images, state, snooze_until, snooze_trigger, answered_with, created_at) VALUES
 (1, 'session:a', 'block:1', 'c1', 'p1:abcd', 0, '/code/app', 'main', 'app', 'decision', 'Commit this?',
  'Want me to commit?', 'ctx', 'tidy up', '["Commit","Not yet"]', 0, '[]', 'open', 0, '', '', 1000),
 (2, 'session:a', 'block:1', 'c1', 'p1:abcd', 1, '/code/app', 'main', 'app', 'veto', 'Keep iOS 18?',
  'I raised it.', '', 'tidy up', '["Keep","Revert"]', -1, '["/tmp/a.png"]', 'snoozed', 5000, '', '', 1000),
 (3, '', '', 'c2', 'u-9', 0, '/code/lib', '', 'lib', 'action', 'Run the migration?',
  '', '', '', '[]', -1, '[]', 'snoozed', 0, 'next-stop', '', 900),
 (4, 'session:b', '', 'c3', 'p7:ffff', 0, '/code/x', 'dev', 'x', 'blocker', 'Which key?',
  '', '', '', '["A"]', 0, '[]', 'answered', 0, '', 'A', 800);
INSERT INTO prompts VALUES ('c1', 'tidy up', 990), ('c3', 'A', 850);
INSERT INTO pending VALUES (1, 'c2', '{"session_id":"c2","last_assistant_message":"x?"}', 2, 7000, 950);
INSERT INTO backfilled VALUES ('c2', 960);
INSERT INTO health VALUES (1, 'claude -p: usage limit', 970, 940);
INSERT INTO panes VALUES ('block:1', 'c1', 'session:a', 995);`
	if _, err := db.Exec(stmts); err != nil {
		t.Fatal(err)
	}
}

// dumpV1 returns every row of every version 1 table, by the version 1
// columns, as text.
func dumpV1(t *testing.T, path string) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := map[string][]string{}
	for table, cols := range v1Tables {
		// A version 2 store keeps the prompts table as last_prompts.
		from := table
		var renamed int
		db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'last_prompts'`).Scan(&renamed)
		if table == "prompts" && renamed > 0 {
			from = "last_prompts"
		}
		rows, err := db.Query(fmt.Sprintf("SELECT %s FROM %s ORDER BY 1", cols, from))
		if err != nil {
			t.Fatal(err)
		}
		n := len(strings.Split(cols, ","))
		for rows.Next() {
			vals := make([]any, n)
			ptrs := make([]any, n)
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], fmt.Sprintf("%q", vals))
		}
		rows.Close()
	}
	return out
}

func TestClaudeCompatVersion1StoreReadsTheSame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	writeV1Store(t, path)
	before := dumpV1(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.query(`SELECT ` + columns + ` FROM items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Item{
		{ID: 1, RexSession: "session:a", RexBlock: "block:1", ClaudeSession: "c1", SourceMsg: "p1:abcd", Seq: 0,
			Cwd: "/code/app", Branch: "main", Project: "app", Kind: "decision", Title: "Commit this?",
			Body: "Want me to commit?", Context: "ctx", Prompt: "tidy up", Replies: []string{"Commit", "Not yet"},
			Suggested: 0, Images: []string{}, State: "open", CreatedAt: time.Unix(1000, 0), PromptedAt: time.Unix(990, 0)},
		{ID: 2, RexSession: "session:a", RexBlock: "block:1", ClaudeSession: "c1", SourceMsg: "p1:abcd", Seq: 1,
			Cwd: "/code/app", Branch: "main", Project: "app", Kind: "veto", Title: "Keep iOS 18?", Body: "I raised it.",
			Prompt: "tidy up", Replies: []string{"Keep", "Revert"}, Suggested: -1, Images: []string{"/tmp/a.png"},
			State: "snoozed", SnoozeUntil: time.Unix(5000, 0), CreatedAt: time.Unix(1000, 0), PromptedAt: time.Unix(990, 0)},
		{ID: 3, ClaudeSession: "c2", SourceMsg: "u-9", Cwd: "/code/lib", Project: "lib", Kind: "action",
			Title: "Run the migration?", Replies: []string{}, Suggested: -1, Images: []string{}, State: "snoozed",
			SnoozeTrigger: "next-stop", CreatedAt: time.Unix(900, 0)},
		{ID: 4, RexSession: "session:b", ClaudeSession: "c3", SourceMsg: "p7:ffff", Cwd: "/code/x", Branch: "dev",
			Project: "x", Kind: "blocker", Title: "Which key?", Replies: []string{"A"}, Suggested: 0, Images: []string{},
			State: "answered", AnsweredWith: "A", CreatedAt: time.Unix(800, 0), PromptedAt: time.Unix(850, 0)},
	}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("items\n got %+v\nwant %+v", all, want)
	}
	if owner, err := s.PaneOwner("block:1"); err != nil || owner != "c1" {
		t.Errorf("pane owner %q %v", owner, err)
	}
	if known, err := s.Known("c2"); err != nil || !known {
		t.Errorf("backfilled session forgotten: %v %v", known, err)
	}
	if h, err := s.Health(); err != nil || h.LastError != "claude -p: usage limit" {
		t.Errorf("health %+v %v", h, err)
	}
	s.Close()

	// Opening wrote nothing to the old columns, and opening again changes
	// nothing either.
	if after := dumpV1(t, path); !reflect.DeepEqual(after, before) {
		t.Errorf("rows changed\nbefore %v\n after %v", before, after)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if after := dumpV1(t, path); !reflect.DeepEqual(after, before) {
		t.Errorf("rows changed on a second open")
	}
}
