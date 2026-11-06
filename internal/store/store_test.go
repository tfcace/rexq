package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCreatesSchemaTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		s.Close()
	}
}

func sample(session, msg string, seq int) Item {
	return Item{
		RexSession: "session:a", ClaudeSession: session, SourceMsg: msg, Seq: seq,
		Kind: KindDecision, Title: "Commit this?", Replies: []string{"Commit", "Not yet"},
		Suggested: 0, CreatedAt: time.Unix(1000, 0),
	}
}

func TestInsertIsIdempotent(t *testing.T) {
	s := openTemp(t)
	if _, added, err := s.Insert(sample("c1", "m1", 0)); err != nil || !added {
		t.Fatalf("first insert: added=%v err=%v", added, err)
	}
	if _, added, err := s.Insert(sample("c1", "m1", 0)); err != nil || added {
		t.Fatalf("second insert: added=%v err=%v", added, err)
	}
	items, err := s.List("")
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %d items, err=%v", len(items), err)
	}
	got := items[0]
	if got.Title != "Commit this?" || len(got.Replies) != 2 || got.Suggested != 0 {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

func TestListScopesByRexSession(t *testing.T) {
	s := openTemp(t)
	a := sample("c1", "m1", 0)
	b := sample("c2", "m2", 0)
	b.RexSession = "session:b"
	s.Insert(a)
	s.Insert(b)
	if items, _ := s.List("session:a"); len(items) != 1 || items[0].ClaudeSession != "c1" {
		t.Fatalf("scoped list: %+v", items)
	}
	if items, _ := s.List(""); len(items) != 2 {
		t.Fatalf("all: %d", len(items))
	}
	// An item from no Rex session (backfilled, or an agent outside Rex)
	// belongs to every session's list.
	c := sample("c3", "m3", 0)
	c.RexSession = ""
	s.Insert(c)
	for _, scope := range []string{"session:a", "session:b"} {
		items, _ := s.List(scope)
		if len(items) != 2 || items[0].ClaudeSession != "c3" && items[1].ClaudeSession != "c3" {
			t.Errorf("%s: %+v", scope, items)
		}
	}
}

// Agent text can carry escape sequences echoed from tool output. Stored
// as-is, the panel would render them and an OSC sequence could retitle the
// terminal or write the clipboard.
func TestInsertStripsControlCharacters(t *testing.T) {
	s := openTemp(t)
	it := sample("c1", "m1", 0)
	it.Title = "Commit\x1b]52;c;aGk=\x07 this?"
	it.Body = "line one\nline\ttwo\u009b2J" // U+009B is CSI as a C1 control
	it.Replies = []string{"\x1b[31mYes\x1b[0m"}
	s.Insert(it)
	got := must(s.List(""))[0]
	if got.Title != "Commit]52;c;aGk= this?" || got.Body != "line one\nline\ttwo2J" || got.Replies[0] != "[31mYes[0m" {
		t.Fatalf("not sanitized: %q %q %q", got.Title, got.Body, got.Replies)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestSnoozeWakeAnswer(t *testing.T) {
	s := openTemp(t)
	id, _, _ := s.Insert(sample("c1", "m1", 0))
	now := time.Unix(5000, 0)

	if err := s.Snooze(id, now.Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	items, _ := s.List("")
	if !items[0].Snoozed(now) || items[0].Snoozed(now.Add(2*time.Hour)) {
		t.Fatalf("snooze window wrong: %+v", items[0])
	}
	if open, _ := s.OpenFor("c1"); len(open) != 0 {
		t.Fatalf("snoozed item counted as open")
	}
	s.Wake(id)
	if open, _ := s.OpenFor("c1"); len(open) != 1 {
		t.Fatalf("wake did not reopen")
	}
	s.Answer(id, "Commit")
	if items, _ := s.List(""); len(items) != 0 {
		t.Fatalf("answered item still listed")
	}
}

func TestWakeTriggerOnlyTouchesThatSession(t *testing.T) {
	s := openTemp(t)
	a, _, _ := s.Insert(sample("c1", "m1", 0))
	b, _, _ := s.Insert(sample("c2", "m2", 0))
	s.Snooze(a, time.Time{}, TriggerNextStop)
	s.Snooze(b, time.Time{}, TriggerNextStop)
	if err := s.WakeTrigger("c1", TriggerNextStop); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenFor("c1"); len(open) != 1 {
		t.Fatalf("c1 not woken")
	}
	if open, _ := s.OpenFor("c2"); len(open) != 0 {
		t.Fatalf("c2 woken by c1's stop")
	}
}

// The claiming session's own items in the pane keep it; only other
// sessions' items lose it.
func TestClaimPaneKeepsTheClaimersItems(t *testing.T) {
	s := openTemp(t)
	old := sample("c-old", "m1", 0)
	old.RexBlock = "block:1"
	cur := sample("c-new", "m2", 0)
	cur.RexBlock = "block:1"
	must2(s.Insert(old))
	must2(s.Insert(cur))
	if err := s.ClaimPane("block:1", "session:a", "c-new", time.Unix(5, 0)); err != nil {
		t.Fatal(err)
	}
	items := must(s.List(""))
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	for _, it := range items {
		want := ""
		if it.ClaudeSession == "c-new" {
			want = "block:1"
		}
		if it.RexBlock != want {
			t.Fatalf("%s: block %q, want %q", it.ClaudeSession, it.RexBlock, want)
		}
	}
}

// After a resume in another Rex session, the session's waiting items follow
// it; finished ones stay where they were.
func TestClaimPaneMovesOnlyWaitingItems(t *testing.T) {
	s := openTemp(t)
	open := sample("c1", "m1", 0)
	open.RexBlock = "block:old"
	done := sample("c1", "m1", 1)
	done.RexBlock = "block:old"
	must2(s.Insert(open))
	doneID := must2(s.Insert(done))
	ok(t, s.Answer(doneID, "yes"))
	ok(t, s.ClaimPane("block:new", "session:b", "c1", time.Unix(5, 0)))
	got := must(s.List(""))
	if len(got) != 1 || got[0].RexSession != "session:b" || got[0].RexBlock != "block:new" {
		t.Fatalf("not moved: %+v", got)
	}
	if it := itemByID(t, s, doneID); it.RexBlock != "block:old" {
		t.Errorf("answered item moved: %+v", it)
	}
}

func TestPromptsKeepLatest(t *testing.T) {
	s := openTemp(t)
	s.RecordPrompt("c1", "first", time.Unix(1, 0))
	s.RecordPrompt("c1", "second", time.Unix(2, 0))
	if p, _ := s.LastPrompt("c1"); p != "second" {
		t.Fatalf("got %q", p)
	}
	if p, _ := s.LastPrompt("nobody"); p != "" {
		t.Fatalf("unknown session returned %q", p)
	}
	if !must(s.Known("c1")) || must(s.Known("nobody")) {
		t.Fatalf("Known wrong")
	}
}

// The queue's whole life: the turn failed once when it was queued, each
// failed retry waits longer, and the sixth failure drops it.
func TestPendingScheduleEndToEnd(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1000, 0)
	ok(t, s.Enqueue("c1", []byte(`{}`), now))
	waits := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	for failure, wait := range waits {
		due := now.Add(wait)
		if rows := must(s.Pending(10, due.Add(-time.Second))); len(rows) != 0 {
			t.Fatalf("after failure %d: due before %v", failure+1, wait)
		}
		rows := must(s.Pending(10, due))
		if len(rows) != 1 {
			t.Fatalf("after failure %d: not due after %v", failure+1, wait)
		}
		if !must(s.ClaimPending(rows[0].ID, due)) {
			t.Fatalf("after failure %d: claim failed", failure+1)
		}
		err := s.FailPending(rows[0].ID)
		if failure < len(waits)-1 {
			ok(t, err)
		} else {
			wantDropped(t, err, rows[0].ID)
		}
		now = due
	}
	if n := countPending(t, s); n != 0 {
		t.Fatalf("not dropped after the sixth failure: %d rows", n)
	}
}

// wantDropped checks that err reports dropping row id of session c1 after
// all its retries.
func wantDropped(t *testing.T, err error, id int64) {
	t.Helper()
	var d *DroppedError
	if !errors.As(err, &d) {
		t.Fatalf("want a DroppedError, got %v", err)
	}
	if d.ID != id || d.ClaudeSession != "c1" || d.Attempts != MaxAttempts {
		t.Fatalf("got %+v", d)
	}
	if msg := err.Error(); !strings.Contains(msg, "dropped") || !strings.Contains(msg, "c1") {
		t.Fatalf("unhelpful message %q", msg)
	}
}

// A retry whose process dies never calls FailPending; the claim alone uses
// up the attempt, so the row still goes away in the end.
func TestClaimPendingCountsTheAttempt(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1000, 0)
	ok(t, s.Enqueue("c1", []byte(`{}`), now))
	for i := 0; i < MaxAttempts; i++ {
		now = now.Add(3 * time.Hour)
		rows := must(s.Pending(10, now))
		if len(rows) != 1 || !must(s.ClaimPending(rows[0].ID, now)) {
			t.Fatalf("claim %d failed", i+1)
		}
	}
	now = now.Add(3 * time.Hour)
	rows := must(s.Pending(10, now))
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	claimed, err := s.ClaimPending(rows[0].ID, now)
	if claimed {
		t.Fatalf("claimed past %d attempts", MaxAttempts)
	}
	wantDropped(t, err, rows[0].ID)
	if n := countPending(t, s); n != 0 {
		t.Fatalf("row kept after %d crashed retries", MaxAttempts)
	}
}

func countPending(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM pending`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A claimed row is out of the queue until its backoff ends, so two hook
// processes never retry the same turn at once.
func TestClaimPendingIsExclusive(t *testing.T) {
	s := openTemp(t)
	ok(t, s.Enqueue("c1", []byte(`{}`), time.Unix(1, 0)))
	now := time.Unix(100, 0)
	id := must(s.Pending(10, now))[0].ID
	if !must(s.ClaimPending(id, now)) {
		t.Fatal("first claim failed")
	}
	if must(s.ClaimPending(id, now)) {
		t.Fatal("second claim succeeded")
	}
	if rows := must(s.Pending(10, now)); len(rows) != 0 {
		t.Fatalf("claimed row still listed: %+v", rows)
	}
}

// The hook holding a session's lock runs the session's turns in order, so
// it takes them whether or not their backoff has ended.
func TestClaimPendingEarlyIgnoresTheBackoff(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(100, 0)
	ok(t, s.Enqueue("c1", []byte(`{}`), now))
	id := must(s.PendingFor("c1"))[0].ID
	if must(s.ClaimPending(id, now)) {
		t.Fatal("claimed before its backoff ended")
	}
	if !must(s.ClaimPendingEarly(id, now)) {
		t.Fatal("early claim failed")
	}
	if !must(s.ClaimPendingEarly(id, now)) {
		t.Fatal("second early claim failed")
	}
	ok(t, s.DonePending(id))
	if must(s.ClaimPendingEarly(id, now)) {
		t.Fatal("claimed a row that is gone")
	}
}

func TestPendingForListsOneSessionOldestFirst(t *testing.T) {
	s := openTemp(t)
	for _, e := range []struct{ session, payload string }{{"c1", "a"}, {"c2", "b"}, {"c1", "c"}} {
		ok(t, s.Enqueue(e.session, []byte(e.payload), time.Unix(1, 0)))
	}
	// Not due yet, but listed: the session's own hook runs them in order.
	rows := must(s.PendingFor("c1"))
	if len(rows) != 2 || string(rows[0].Payload) != "a" || string(rows[1].Payload) != "c" {
		t.Fatalf("got %+v", rows)
	}
}

// For other sessions' turns, Pending offers only each session's oldest row,
// and only once it is due, so a later turn never overtakes an earlier one.
func TestPendingOffersEachSessionsOldestRow(t *testing.T) {
	s := openTemp(t)
	for _, e := range []struct{ session, payload string }{{"c1", "a"}, {"c2", "b"}, {"c1", "c"}} {
		ok(t, s.Enqueue(e.session, []byte(e.payload), time.Unix(1, 0)))
	}
	now := time.Unix(1000, 0)
	rows := must(s.Pending(10, now))
	if len(rows) != 2 || string(rows[0].Payload) != "a" || string(rows[1].Payload) != "b" {
		t.Fatalf("got %+v", rows)
	}
	// c1's oldest row backs off; its newer one must still wait.
	if !must(s.ClaimPending(rows[0].ID, now)) {
		t.Fatal("claim failed")
	}
	rows = must(s.Pending(10, now))
	if len(rows) != 1 || string(rows[0].Payload) != "b" {
		t.Fatalf("got %+v", rows)
	}
}

// A session resumed in another pane leaves the old one: a late item of it
// goes to the pane it runs in now.
func TestInsertFromPaneFollowsAResume(t *testing.T) {
	s := openTemp(t)
	ok(t, s.ClaimPane("block:1", "session:a", "c1", time.Unix(5, 0)))
	ok(t, s.ClaimPane("block:2", "session:b", "c1", time.Unix(6, 0)))
	it := sample("c1", "late", 0)
	it.RexSession, it.RexBlock = "session:a", "block:1"
	id := must2(s.InsertFromPane(it))
	got := itemByID(t, s, id)
	if got.RexBlock != "block:2" || got.RexSession != "session:b" {
		t.Fatalf("late item went to %s %s", got.RexSession, got.RexBlock)
	}
	// A session with no pane keeps the Rex session it captured.
	other := sample("c9", "x", 0)
	other.RexSession, other.RexBlock = "session:z", "block:2"
	if got := itemByID(t, s, must2(s.InsertFromPane(other))); got.RexBlock != "" || got.RexSession != "session:z" {
		t.Fatalf("paneless item went to %s %q", got.RexSession, got.RexBlock)
	}
	// Nobody owns block:1 now.
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM panes WHERE block = 'block:1'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old pane row kept: n=%d err=%v", n, err)
	}
}

func itemByID(t *testing.T, s *Store, id int64) Item {
	t.Helper()
	items, err := s.query(`SELECT `+columns+` FROM items WHERE id = ?`, id)
	if err != nil || len(items) != 1 {
		t.Fatalf("item %d: %v", id, err)
	}
	return items[0]
}

func TestClaimPaneReleasesOthersAndMovesTheSession(t *testing.T) {
	s := openTemp(t)
	old := sample("c-old", "m1", 0)
	old.RexBlock = "block:1"
	resumed := sample("c-new", "m2", 0)
	resumed.RexBlock = "block:9"
	must2(s.Insert(old))
	must2(s.Insert(resumed))
	if err := s.ClaimPane("block:1", "session:b", "c-new", time.Unix(5, 0)); err != nil {
		t.Fatal(err)
	}
	for _, it := range must(s.List("")) {
		switch it.ClaudeSession {
		case "c-old":
			if it.RexBlock != "" {
				t.Errorf("old session still points at the pane: %+v", it)
			}
		case "c-new":
			if it.RexBlock != "block:1" || it.RexSession != "session:b" {
				t.Errorf("new session not moved: %+v", it)
			}
		}
	}
}

// InsertFromPane gives an item the pane its Claude session claimed last, if
// no other session has claimed that pane since.
func TestInsertFromPaneChecksTheOwner(t *testing.T) {
	s := openTemp(t)
	if err := s.ClaimPane("block:1", "session:a", "c-owner", time.Unix(5, 0)); err != nil {
		t.Fatal(err)
	}
	// The block an item carries doesn't matter: it goes to the pane its
	// session owns now, or to none.
	cases := []struct {
		session, block, want string
	}{
		{"c-owner", "block:1", "block:1"},
		{"c-other", "block:1", ""},
		{"c-owner", "block:unclaimed", "block:1"},
		{"c-owner", "", "block:1"},
		{"c-other", "", ""},
	}
	for i, c := range cases {
		it := sample(c.session, fmt.Sprint("m", i), 0)
		it.RexBlock = c.block
		id, added, err := s.InsertFromPane(it)
		if err != nil || !added {
			t.Fatalf("case %d: added=%v err=%v", i, added, err)
		}
		var got string
		if err := s.db.QueryRow(`SELECT rex_block FROM items WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("case %d (%s in %q): block %q, want %q", i, c.session, c.block, got, c.want)
		}
	}
	// Sanitized and idempotent, like Insert.
	it := sample("c-owner", "m0", 0)
	it.RexBlock = "block:1"
	if _, added, err := s.InsertFromPane(it); err != nil || added {
		t.Fatalf("duplicate: added=%v err=%v", added, err)
	}
	it = sample("c-owner", "dirty", 0)
	it.Title = "Commit\x1b[31m?"
	id := must2(s.InsertFromPane(it))
	var title string
	s.db.QueryRow(`SELECT title FROM items WHERE id = ?`, id).Scan(&title)
	if title != "Commit[31m?" {
		t.Fatalf("not sanitized: %q", title)
	}
}

// A new owner claiming the pane wins over an older one, whatever the order
// of the inserts around it.
func TestClaimPaneReplacesTheOwner(t *testing.T) {
	s := openTemp(t)
	ok(t, s.ClaimPane("block:1", "session:a", "c1", time.Unix(5, 0)))
	ok(t, s.ClaimPane("block:1", "session:a", "c2", time.Unix(6, 0)))
	it := sample("c1", "m", 0)
	it.RexBlock = "block:1"
	id := must2(s.InsertFromPane(it))
	var got string
	s.db.QueryRow(`SELECT rex_block FROM items WHERE id = ?`, id).Scan(&got)
	if got != "" {
		t.Fatalf("c1 kept a pane c2 claimed: %q", got)
	}
}

// must2 is must for calls that return two values and an error.
func must2[T, U any](v T, _ U, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func stateOf(t *testing.T, s *Store, id int64) string {
	t.Helper()
	var state string
	if err := s.db.QueryRow(`SELECT state FROM items WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestFinishedItemsIgnoreStateChanges(t *testing.T) {
	s := openTemp(t)
	answered := must2(s.Insert(sample("c1", "m1", 0)))
	dismissed := must2(s.Insert(sample("c1", "m1", 1)))
	if err := s.Answer(answered, "Commit"); err != nil {
		t.Fatal(err)
	}
	if err := s.Snooze(answered, time.Unix(9000, 0), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Dismiss(answered); err != nil {
		t.Fatal(err)
	}
	if err := s.Dismiss(dismissed); err != nil {
		t.Fatal(err)
	}
	if err := s.Wake(dismissed); err != nil {
		t.Fatal(err)
	}
	if err := s.Answer(dismissed, "Commit"); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, s, answered); got != StateAnswered {
		t.Fatalf("answered item now %q", got)
	}
	if got := stateOf(t, s, dismissed); got != StateDismissed {
		t.Fatalf("dismissed item now %q", got)
	}
	if items := must(s.List("")); len(items) != 0 {
		t.Fatalf("finished items listed: %+v", items)
	}
}

func TestInsertNormalizesSuggestedAndKind(t *testing.T) {
	s := openTemp(t)
	cases := []struct {
		suggested int
		want      int
	}{{0, 0}, {1, 1}, {2, -1}, {-1, -1}, {-7, -1}}
	for i, c := range cases {
		it := sample("c1", "m1", i)
		it.Suggested = c.suggested
		must2(s.Insert(it))
	}
	bogus := sample("c1", "m2", 0)
	bogus.Kind = "urgent!!"
	bogus.Images = []string{"shot\x1b[2J.png"}
	must2(s.Insert(bogus))

	items := must(s.OpenFor("c1"))
	if len(items) != len(cases)+1 {
		t.Fatalf("got %d items, want %d", len(items), len(cases)+1)
	}
	for _, it := range items {
		if it.SourceMsg == "m2" {
			if it.Kind != KindLeftover {
				t.Fatalf("unknown kind stored as %q", it.Kind)
			}
			if it.Images[0] != "shot[2J.png" {
				t.Fatalf("image not sanitized: %q", it.Images[0])
			}
			continue
		}
		if want := cases[it.Seq].want; it.Suggested != want {
			t.Fatalf("suggested %d stored as %d, want %d", cases[it.Seq].suggested, it.Suggested, want)
		}
	}
}

func TestWakeDue(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(5000, 0)
	due := must2(s.Insert(sample("c1", "m1", 0)))
	later := must2(s.Insert(sample("c1", "m1", 1)))
	trigger := must2(s.Insert(sample("c1", "m1", 2)))
	if err := s.Snooze(due, now.Add(-time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Snooze(later, now.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Snooze(trigger, time.Time{}, TriggerNextStop); err != nil {
		t.Fatal(err)
	}
	if err := s.WakeDue(now); err != nil {
		t.Fatal(err)
	}
	open := must(s.OpenFor("c1"))
	if len(open) != 1 || open[0].ID != due {
		t.Fatalf("open after WakeDue: %+v", open)
	}
	if open[0].SnoozeUntil != (time.Time{}) {
		t.Fatalf("woken item kept snooze time %v", open[0].SnoozeUntil)
	}
}

func TestInsertRoundTripsEveryField(t *testing.T) {
	s := openTemp(t)
	want := Item{
		RexSession:    "session:x",
		RexBlock:      "block:7",
		ClaudeSession: "c9",
		SourceMsg:     "msg-9",
		Seq:           3,
		// Cwd is a real path for claude --resume, so it is stored raw.
		Cwd:       "/home/me/odd\x01dir",
		Branch:    "feature/x",
		Project:   "proj",
		Kind:      KindBlocker,
		Title:     "Title",
		Body:      "Body\nmore",
		Context:   "Context",
		Prompt:    "Prompt",
		Replies:   []string{"a", "b"},
		Suggested: -1,
		Images:    []string{"/tmp/a.png", "/tmp/b.png"},
		State:     StateOpen,
		CreatedAt: time.Unix(1234, 0),
	}
	id := must2(s.Insert(want))
	want.ID = id
	items := must(s.List(""))
	if len(items) != 1 {
		t.Fatalf("got %d items", len(items))
	}
	if !reflect.DeepEqual(items[0], want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", items[0], want)
	}
}

func TestInsertDefaultsCreatedAt(t *testing.T) {
	s := openTemp(t)
	it := sample("c1", "m1", 0)
	it.CreatedAt = time.Time{}
	before := time.Now().Add(-time.Second)
	must2(s.Insert(it))
	got := must(s.List(""))[0].CreatedAt
	if got.Before(before) || got.After(time.Now().Add(time.Second)) {
		t.Fatalf("created_at %v, want about now", got)
	}
}

func TestDataVersionSeesOtherConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	a := must(Open(path))
	defer a.Close()
	b := must(Open(path))
	defer b.Close()

	v0 := must(a.DataVersion())
	must2(a.Insert(sample("c1", "m1", 0)))
	if v := must(a.DataVersion()); v != v0 {
		t.Fatalf("own write changed data_version %d -> %d", v0, v)
	}
	must2(b.Insert(sample("c1", "m1", 1)))
	if v := must(a.DataVersion()); v == v0 {
		t.Fatalf("write from another connection not seen (still %d)", v)
	}
}

func TestOpenPathWithURLCharacters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "we#ird?na%20me.db")
	s := must(Open(path))
	must2(s.Insert(sample("c1", "m1", 0)))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("store not at %q: %v (dir has %q)", path, err, names)
	}
}

func TestDefaultPathWithoutHome(t *testing.T) {
	t.Setenv("REXQ_DB", "")
	t.Setenv("HOME", "")
	want := filepath.Join(os.TempDir(), "rex-questions", "questions.db")
	if got := DefaultPath(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// REXQ_DB may hold a relative path.
func TestOpenRelativePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	s := must(Open(filepath.Join("rel", "q.db")))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "rel", "q.db")); err != nil {
		t.Fatal(err)
	}
}

// A snooze with neither a time nor a trigger would never end.
func TestSnoozeNeedsTimeOrTrigger(t *testing.T) {
	s := openTemp(t)
	id := must2(s.Insert(sample("c1", "m1", 0)))
	if err := s.Snooze(id, time.Time{}, ""); err == nil {
		t.Fatal("empty snooze accepted")
	}
	if got := stateOf(t, s, id); got != StateOpen {
		t.Fatalf("item now %q", got)
	}
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// The sync hooks get a short busy timeout, so a busy store can't hold the
// user's prompt until Claude kills the hook; everything else waits 5 s.
func TestOpenWithSetsTheBusyTimeout(t *testing.T) {
	busy := func(s *Store) int {
		t.Helper()
		var ms int
		if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&ms); err != nil {
			t.Fatal(err)
		}
		return ms
	}
	path := filepath.Join(t.TempDir(), "q.db")
	s, err := OpenWith(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ms := busy(s); ms != 1000 {
		t.Errorf("OpenWith(1s): busy_timeout %d ms", ms)
	}
	d := openTemp(t)
	if ms := busy(d); ms != 5000 {
		t.Errorf("Open: busy_timeout %d ms", ms)
	}
}

func TestPaneOwner(t *testing.T) {
	s := openTemp(t)
	if got, err := s.PaneOwner("block:1"); err != nil || got != "" {
		t.Fatalf("no row: got %q %v", got, err)
	}
	ok(t, s.ClaimPane("block:1", "session:a", "c1", time.Unix(5, 0)))
	ok(t, s.ClaimPane("block:2", "session:a", "c2", time.Unix(6, 0)))
	if got, err := s.PaneOwner("block:1"); err != nil || got != "c1" {
		t.Fatalf("got %q %v, want c1", got, err)
	}
	ok(t, s.ClaimPane("block:1", "session:a", "c3", time.Unix(7, 0)))
	if got, err := s.PaneOwner("block:1"); err != nil || got != "c3" {
		t.Fatalf("after a new claim: got %q %v, want c3", got, err)
	}
	// c2 resumed in block:3, so block:2 no longer belongs to anyone.
	ok(t, s.ClaimPane("block:3", "session:a", "c2", time.Unix(8, 0)))
	if got, err := s.PaneOwner("block:2"); err != nil || got != "" {
		t.Fatalf("after c2 moved: got %q %v, want none", got, err)
	}
}

func TestItemByID(t *testing.T) {
	s := openTemp(t)
	id := must2(s.Insert(sample("c1", "m", 0)))
	it, found, err := s.Item(id)
	if err != nil || !found || it.ID != id || it.Title != "Commit this?" || it.State != StateOpen {
		t.Fatalf("got %+v %v %v", it, found, err)
	}
	ok(t, s.Dismiss(id))
	if it, found, err := s.Item(id); err != nil || !found || it.State != StateDismissed {
		t.Fatalf("dismissed: got %+v %v %v", it, found, err)
	}
	if _, found, err := s.Item(id + 100); err != nil || found {
		t.Fatalf("missing: found=%v err=%v", found, err)
	}
}

func TestRestore(t *testing.T) {
	s := openTemp(t)
	id := must2(s.Insert(sample("c1", "m", 0)))
	ok(t, s.Snooze(id, time.Unix(5000, 0), ""))
	prev, _, _ := s.Item(id)

	ok(t, s.Dismiss(id))
	if done, err := s.Restore(prev, StateDismissed); err != nil || !done {
		t.Fatalf("restore: %v %v", done, err)
	}
	got, _, _ := s.Item(id)
	if got.State != StateSnoozed || !got.SnoozeUntil.Equal(time.Unix(5000, 0)) || got.AnsweredWith != "" {
		t.Fatalf("restored to %+v", got)
	}

	// Answered since: the undo of a dismiss no longer applies.
	ok(t, s.Answer(id, "yes"))
	if done, err := s.Restore(prev, StateDismissed); err != nil || done {
		t.Fatalf("restore over a newer change: %v %v", done, err)
	}
	if stateOf(t, s, id) != StateAnswered {
		t.Fatal("restore overwrote a newer change")
	}
	// Undo of "done" clears the answer.
	if done, err := s.Restore(prev, StateAnswered); err != nil || !done {
		t.Fatalf("restore: %v %v", done, err)
	}
	if got, _, _ := s.Item(id); got.State != StateSnoozed || got.AnsweredWith != "" {
		t.Fatalf("restored to %+v", got)
	}
}

// A backfilled session is known even when it left nothing, so the next
// backfill doesn't pay to extract it again.
func TestInsertBackfillMarksTheSession(t *testing.T) {
	s := openTemp(t)
	if n, err := s.InsertBackfill("empty", nil, time.Unix(5, 0)); err != nil || n != 0 {
		t.Fatalf("no items: n=%d err=%v", n, err)
	}
	if !must(s.Known("empty")) {
		t.Error("a session backfilled with no items is not known")
	}
	items := []Item{sample("c1", "m1", 0), sample("c1", "m1", 1)}
	items[0].RexSession = ""
	if n, err := s.InsertBackfill("c1", items, time.Unix(5, 0)); err != nil || n != 2 {
		t.Fatalf("two items: n=%d err=%v", n, err)
	}
	// Again: nothing new is added, and it is no error.
	if n, err := s.InsertBackfill("c1", items, time.Unix(6, 0)); err != nil || n != 0 {
		t.Fatalf("again: n=%d err=%v", n, err)
	}
	got, err := s.List("")
	if err != nil || len(got) != 2 {
		t.Fatalf("list: %d items, err=%v", len(got), err)
	}
}

// If marking the session fails, its items are not stored either: the
// session stays unknown and complete for the next run.
func TestInsertBackfillIsAllOrNothing(t *testing.T) {
	s := openTemp(t)
	if _, err := s.db.Exec(`CREATE TRIGGER fail BEFORE INSERT ON backfilled BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertBackfill("c1", []Item{sample("c1", "m1", 0)}, time.Unix(5, 0)); err == nil {
		t.Fatal("no error")
	}
	if must(s.Known("c1")) {
		t.Error("a failed backfill left the session known")
	}
}

func TestHealth(t *testing.T) {
	s := openTemp(t)
	h := must(s.Health())
	if h.Failing() || h.Pending != 0 || h.LastError != "" {
		t.Fatalf("new store: %+v", h)
	}
	ok(t, s.RecordExtractionError("claude -p: Invalid API key · Please run /login", time.Unix(100, 0)))
	ok(t, s.Enqueue("c1", []byte("{}"), time.Unix(100, 0)))
	ok(t, s.Enqueue("c2", []byte("{}"), time.Unix(100, 0)))
	h = must(s.Health())
	if !h.Failing() || h.Pending != 2 || h.LastError != "claude -p: Invalid API key · Please run /login" ||
		!h.LastErrorAt.Equal(time.Unix(100, 0)) {
		t.Fatalf("after a failure: %+v", h)
	}
	ok(t, s.RecordExtractionOK(time.Unix(200, 0)))
	h = must(s.Health())
	if h.Failing() || !h.LastOKAt.Equal(time.Unix(200, 0)) || h.LastError == "" {
		t.Fatalf("after a success: %+v", h)
	}
	// A long error is kept short.
	ok(t, s.RecordExtractionError(strings.Repeat("x", 5000), time.Unix(300, 0)))
	if h := must(s.Health()); !h.Failing() || len(h.LastError) > maxHealthError+10 {
		t.Fatalf("long error: %d bytes", len(h.LastError))
	}
}

// An item knows when its agent was last written to, so the panel can tell
// the conversation moved on. A prompt in the same second as the question
// counts as after it: the second can't order them.
func TestItemWrittenSince(t *testing.T) {
	s := openTemp(t)
	id := must2(s.Insert(sample("c1", "m1", 0))) // asked at 1000
	if it := itemByID(t, s, id); it.WrittenSince() || !it.PromptedAt.IsZero() {
		t.Fatalf("never prompted: %+v", it)
	}
	for at, want := range map[int64]bool{999: false, 1000: true, 1001: true} {
		ok(t, s.RecordPrompt("c1", "hi", time.Unix(at, 0)))
		if got := must(s.List(""))[0]; got.WrittenSince() != want || !got.PromptedAt.Equal(time.Unix(at, 0)) {
			t.Errorf("prompt at %d: written since %v, want %v", at, got.WrittenSince(), want)
		}
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// The store holds what agents and you wrote: only you may read it.
func TestOpenKeepsTheStorePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "q.db")
	s := must(Open(path))
	must2(s.Insert(sample("c1", "m1", 0)))
	s.Close()
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("new directory %v", m)
	}
	if m := mode(t, path); m != 0o600 {
		t.Errorf("new database %v", m)
	}
	// A store from before is tightened when opened.
	ok(t, os.Chmod(path, 0o644))
	ok(t, os.WriteFile(path+"-wal", nil, 0o644))
	s = must(Open(path))
	defer s.Close() // closing removes the -wal file
	for _, p := range []string{path, path + "-wal"} {
		if m := mode(t, p); m != 0o600 {
			t.Errorf("%s left %v", filepath.Base(p), m)
		}
	}
}

// The default state directory is tightened too; a directory REXQ_DB points
// into may be shared, like /tmp, and is left alone.
func TestOpenTightensTheDefaultDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("REXQ_DB", "")
	state := filepath.Join(home, ".local", "state", "rex-questions")
	ok(t, os.MkdirAll(state, 0o755))
	ok(t, os.Chmod(state, 0o755))
	must(Open(DefaultPath())).Close()
	if m := mode(t, state); m != 0o700 {
		t.Errorf("default directory left %v", m)
	}
	shared := t.TempDir()
	ok(t, os.Chmod(shared, 0o755))
	must(Open(filepath.Join(shared, "q.db"))).Close()
	if m := mode(t, shared); m != 0o755 {
		t.Errorf("shared directory changed to %v", m)
	}
}

// A session started before the hooks were installed never ran SessionStart.
// Its Stop may take the pane it runs in, if nobody owns that pane and the
// session owns none, and its earlier pane-less items follow.
func TestClaimUnownedPane(t *testing.T) {
	s := openTemp(t)
	early := sample("c1", "m1", 0)
	early.RexSession = ""
	id := must2(s.Insert(early))
	claimed, err := s.ClaimUnownedPane("block:1", "session:a", "c1", time.Unix(5, 0))
	if err != nil || !claimed {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	if owner := must(s.PaneOwner("block:1")); owner != "c1" {
		t.Errorf("owner %q", owner)
	}
	if it := itemByID(t, s, id); it.RexBlock != "block:1" || it.RexSession != "session:a" {
		t.Errorf("earlier item not moved: %+v", it)
	}
	// Again: it owns the pane now, so nothing changes.
	if claimed, err := s.ClaimUnownedPane("block:1", "session:a", "c1", time.Unix(6, 0)); err != nil || claimed {
		t.Errorf("second claim: %v %v", claimed, err)
	}
}

func TestClaimUnownedPaneNeverTakesAnOwnedPane(t *testing.T) {
	s := openTemp(t)
	ok(t, s.ClaimPane("block:1", "session:a", "c-owner", time.Unix(5, 0)))
	mine := sample("c1", "m1", 0)
	id := must2(s.Insert(mine))
	if claimed, err := s.ClaimUnownedPane("block:1", "session:a", "c1", time.Unix(6, 0)); err != nil || claimed {
		t.Fatalf("took an owned pane: %v %v", claimed, err)
	}
	if owner := must(s.PaneOwner("block:1")); owner != "c-owner" {
		t.Errorf("owner %q", owner)
	}
	if it := itemByID(t, s, id); it.RexBlock != "" {
		t.Errorf("item moved: %+v", it)
	}
}

func TestClaimUnownedPaneTakesOnePaneASession(t *testing.T) {
	s := openTemp(t)
	ok(t, s.ClaimPane("block:1", "session:a", "c1", time.Unix(5, 0)))
	if claimed, err := s.ClaimUnownedPane("block:2", "session:a", "c1", time.Unix(6, 0)); err != nil || claimed {
		t.Fatalf("claimed a second pane: %v %v", claimed, err)
	}
	if owner := must(s.PaneOwner("block:2")); owner != "" {
		t.Errorf("block:2 owner %q", owner)
	}
	if owner := must(s.PaneOwner("block:1")); owner != "c1" {
		t.Errorf("block:1 owner %q", owner)
	}
}

// A new store records its schema version, and a store from a newer rexq is
// refused rather than misread.
func TestOpenChecksTheSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version %d, %v; want %d", v, err, schemaVersion)
	}
	// Reopening the same version works.
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := Open(path); !errors.Is(err, ErrNewerStore) || !strings.Contains(err.Error(), "newer rexq") {
		if s != nil {
			s.Close()
		}
		t.Fatalf("got %v, want ErrNewerStore", err)
	}
}

// Stores from before versioning (user_version 0) open and are stamped.
func TestOpenStampsAnUnversionedStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = must(Open(path))
	defer s.Close()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version %d, %v", v, err)
	}
}
