package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Kinds of item, in the order the panel ranks them.
const (
	KindBlocker  = "blocker"  // the agent stopped and is waiting
	KindDecision = "decision" // the agent asks you to choose
	KindVeto     = "veto"     // already done, you may undo it
	KindAction   = "action"   // you have to do something by hand
	KindLeftover = "leftover" // a known limit, nothing is waiting
)

// KindRank orders kinds from most to least urgent.
var KindRank = map[string]int{KindBlocker: 0, KindDecision: 1, KindVeto: 2, KindAction: 3, KindLeftover: 4}

// States.
const (
	StateOpen      = "open"
	StateSnoozed   = "snoozed"
	StateAnswered  = "answered"
	StateDismissed = "dismissed"
)

// TriggerNextStop wakes a snoozed item the next time its agent stops.
const TriggerNextStop = "next-stop"

// Item is one question left for the user.
type Item struct {
	ID            int64
	RexSession    string
	RexBlock      string
	ClaudeSession string
	SourceMsg     string
	Seq           int
	// Cwd is stored raw, since claude --resume needs the real path. Pass it
	// through Sanitize before displaying it.
	Cwd           string
	Branch        string
	Project       string
	Kind          string
	Title         string
	Body          string
	Context       string
	Prompt        string
	Replies       []string
	Suggested     int // index into Replies, -1 when the agent suggested none
	Images        []string
	State         string
	SnoozeUntil   time.Time
	SnoozeTrigger string
	AnsweredWith  string
	CreatedAt     time.Time
	// PromptedAt is when the user last wrote to the item's Claude session,
	// as the prompt hook saw it; zero if never. Read only: it comes from
	// the prompts table.
	PromptedAt time.Time
}

// WrittenSince reports whether the user wrote to the agent after it asked,
// so the conversation has moved on. Both times are in seconds, and a prompt
// in the same second as the question counts as after it.
func (it Item) WrittenSince() bool {
	return !it.PromptedAt.IsZero() && !it.PromptedAt.Before(it.CreatedAt)
}

// Snoozed reports whether the item should stay out of the open list at now.
func (it Item) Snoozed(now time.Time) bool {
	return it.State == StateSnoozed && (it.SnoozeTrigger != "" || it.SnoozeUntil.After(now))
}

const columns = `id, rex_session, rex_block, claude_session, source_msg, seq, cwd, branch, project,
  kind, title, body, context, prompt, replies, suggested, images, state, snooze_until,
  snooze_trigger, answered_with, created_at,
  COALESCE((SELECT updated_at FROM prompts WHERE prompts.claude_session = items.claude_session), 0)`

// Insert adds an item. It reports false, without error, when an item with
// the same session, message and position is already stored.
func (s *Store) Insert(it Item) (int64, bool, error) {
	return insert(s.db, it, "?, ?", it.RexSession, it.RexBlock)
}

// InsertFromPane is Insert for an item from a running agent. It ignores
// it.RexBlock: the item goes to the pane its Claude session claimed last,
// and gets none if the session owns no pane (it never claimed one, or
// another session claimed it since). Its Rex session comes from that pane
// too, or stays it.RexSession without one. The lookup is part of the
// insert, so a ClaimPane can't slip in between.
func (s *Store) InsertFromPane(it Item) (int64, bool, error) {
	return insert(s.db, it, `COALESCE((SELECT rex_session FROM panes WHERE claude_session = ? ORDER BY claimed_at DESC LIMIT 1), ?),
		COALESCE((SELECT block FROM panes WHERE claude_session = ? ORDER BY claimed_at DESC LIMIT 1), '')`,
		it.ClaudeSession, it.RexSession, it.ClaudeSession)
}

// insert adds it through e, with where and its whereArgs giving the
// rex_session and rex_block values.
func insert(e execer, it Item, where string, whereArgs ...any) (int64, bool, error) {
	if it.State == "" {
		it.State = StateOpen
	}
	if it.CreatedAt.IsZero() {
		it.CreatedAt = time.Now()
	}
	it = normalizeItem(sanitizeItem(it))
	replies, _ := json.Marshal(nonNil(it.Replies))
	images, _ := json.Marshal(nonNil(it.Images))
	args := append(whereArgs, it.ClaudeSession, it.SourceMsg, it.Seq,
		it.Cwd, it.Branch, it.Project, it.Kind, it.Title, it.Body, it.Context, it.Prompt,
		string(replies), it.Suggested, string(images), it.State, it.CreatedAt.Unix())
	res, err := e.Exec(`INSERT INTO items (rex_session, rex_block, claude_session, source_msg, seq,
		cwd, branch, project, kind, title, body, context, prompt, replies, suggested, images,
		state, created_at)
		VALUES (`+where+`, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (claude_session, source_msg, seq) DO NOTHING`, args...)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, false, nil
	}
	id, err := res.LastInsertId()
	return id, true, err
}

// List returns open and snoozed items, newest first. An empty rexSession
// lists every session. A Rex session's list also holds the items of no
// session (backfilled, or from an agent outside Rex): they belong nowhere
// else, so they show everywhere.
func (s *Store) List(rexSession string) ([]Item, error) {
	q := `SELECT ` + columns + ` FROM items WHERE state IN ('open', 'snoozed')`
	args := []any{}
	if rexSession != "" {
		q += ` AND rex_session IN (?, '')`
		args = append(args, rexSession)
	}
	return s.query(q+` ORDER BY created_at DESC, seq`, args...)
}

// Item returns one item by id, whatever its state. found is false when
// there is no such item.
func (s *Store) Item(id int64) (it Item, found bool, err error) {
	items, err := s.query(`SELECT `+columns+` FROM items WHERE id = ?`, id)
	if err != nil || len(items) == 0 {
		return Item{}, false, err
	}
	return items[0], true, nil
}

// OpenFor returns the open (not snoozed) items of one Claude session.
func (s *Store) OpenFor(claudeSession string) ([]Item, error) {
	return s.query(`SELECT `+columns+` FROM items WHERE claude_session = ? AND state = 'open' ORDER BY seq`, claudeSession)
}

func (s *Store) query(q string, args ...any) ([]Item, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func scan(rows *sql.Rows) (Item, error) {
	var it Item
	var replies, images string
	var until, created, prompted int64
	err := rows.Scan(&it.ID, &it.RexSession, &it.RexBlock, &it.ClaudeSession, &it.SourceMsg, &it.Seq,
		&it.Cwd, &it.Branch, &it.Project, &it.Kind, &it.Title, &it.Body, &it.Context, &it.Prompt,
		&replies, &it.Suggested, &images, &it.State, &until, &it.SnoozeTrigger, &it.AnsweredWith, &created, &prompted)
	if err != nil {
		return it, err
	}
	// Errors are ignored: only Insert writes these columns, always as JSON.
	json.Unmarshal([]byte(replies), &it.Replies)
	json.Unmarshal([]byte(images), &it.Images)
	if until > 0 {
		it.SnoozeUntil = time.Unix(until, 0)
	}
	it.CreatedAt = time.Unix(created, 0)
	if prompted > 0 {
		it.PromptedAt = time.Unix(prompted, 0)
	}
	return it, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Sanitize drops C0 and C1 control characters except newline and tab, so
// agent text can't drive the terminal it is shown in.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

func sanitizeItem(it Item) Item {
	it.Title, it.Body, it.Context, it.Prompt = Sanitize(it.Title), Sanitize(it.Body), Sanitize(it.Context), Sanitize(it.Prompt)
	it.Project, it.Branch = Sanitize(it.Project), Sanitize(it.Branch)
	it.Replies = sanitizeAll(it.Replies)
	it.Images = sanitizeAll(it.Images)
	return it
}

func sanitizeAll(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = Sanitize(s)
	}
	return out
}

// normalizeItem keeps values from the extractor inside what the panel
// understands: a known kind, and a suggested reply that exists.
func normalizeItem(it Item) Item {
	if it.Suggested < 0 || it.Suggested >= len(it.Replies) {
		it.Suggested = -1
	}
	if _, ok := KindRank[it.Kind]; !ok {
		it.Kind = KindLeftover
	}
	return it
}
