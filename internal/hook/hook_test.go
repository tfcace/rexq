package hook

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tfcace/rexq/internal/extract"
	"github.com/tfcace/rexq/internal/store"
	transcriptpkg "github.com/tfcace/rexq/internal/transcript"
)

func deps(t *testing.T, env map[string]string) Deps {
	t.Helper()
	return depsAt(t, env, filepath.Join(t.TempDir(), "q.db"))
}

func depsAt(t *testing.T, env map[string]string, path string) Deps {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return Deps{
		Store: s,
		Now:   func() time.Time { return time.Unix(10_000, 0) },
		Env:   func(k string) string { return env[k] },
		Repo:  func(string) (string, string) { return "proj", "main" },
		Locks: FileLock(t.TempDir()),
	}
}

func TestPromptRecordsLatest(t *testing.T) {
	d := deps(t, nil)
	in := `{"session_id":"c1","hook_event_name":"UserPromptSubmit","prompt":"yes, commit"}`
	if err := Prompt(d, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	if p, err := d.Store.LastPrompt("c1"); err != nil || p != "yes, commit" {
		t.Fatalf("got %q, %v", p, err)
	}
}

func TestStartClaimsThePane(t *testing.T) {
	d := deps(t, map[string]string{"REX_SESSION": "session:b", "REX_BLOCK": "block:1"})
	old := store.Item{ClaudeSession: "c-old", SourceMsg: "m", Kind: store.KindDecision, Title: "Old?",
		RexSession: "session:a", RexBlock: "block:1", CreatedAt: time.Unix(1, 0)}
	resumed := store.Item{ClaudeSession: "c-resumed", SourceMsg: "m", Kind: store.KindDecision, Title: "Resumed?",
		RexSession: "session:a", RexBlock: "block:9", CreatedAt: time.Unix(1, 0)}
	insert(t, d, old)
	insert(t, d, resumed)

	in := `{"session_id":"c-resumed","hook_event_name":"SessionStart","source":"resume"}`
	if err := Start(d, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	for _, it := range must(d.Store.List("")) {
		switch it.ClaudeSession {
		case "c-old":
			if it.RexBlock != "" {
				t.Errorf("old session still points at the pane")
			}
		case "c-resumed":
			if it.RexBlock != "block:1" || it.RexSession != "session:b" {
				t.Errorf("resumed session not moved: %+v", it)
			}
		}
	}
}

// The extractor's own claude process must leave the store alone.
func TestPromptAndStartIgnoreTheExtractor(t *testing.T) {
	d := deps(t, map[string]string{extract.GuardEnv: "1", "REX_SESSION": "session:a", "REX_BLOCK": "block:1"})
	insert(t, d, store.Item{ClaudeSession: "c-old", SourceMsg: "m", Kind: store.KindDecision, Title: "Old?",
		RexSession: "session:a", RexBlock: "block:1", CreatedAt: time.Unix(1, 0)})
	if err := Prompt(d, strings.NewReader(`{"session_id":"x","prompt":"extract this"}`)); err != nil {
		t.Fatal(err)
	}
	if err := Start(d, strings.NewReader(`{"session_id":"x","source":"startup"}`)); err != nil {
		t.Fatal(err)
	}
	if p := must(d.Store.LastPrompt("x")); p != "" {
		t.Errorf("prompt recorded: %q", p)
	}
	if it := must(d.Store.List(""))[0]; it.RexBlock != "block:1" {
		t.Errorf("extractor took the pane: %+v", it)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func insert(t *testing.T, d Deps, it store.Item) int64 {
	t.Helper()
	id, _, err := d.Store.Insert(it)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// claim runs SessionStart for session in the pane d.Env names.
func claim(t *testing.T, d Deps, session string) {
	t.Helper()
	if err := Start(d, strings.NewReader(fmt.Sprintf(`{"session_id":%q,"source":"startup"}`, session))); err != nil {
		t.Fatal(err)
	}
}

// stubRunner returns out and err, and counts its calls. It is safe to share
// between goroutines.
type stubRunner struct {
	mu    sync.Mutex
	out   string
	err   error
	calls int
}

func (s *stubRunner) Run(context.Context, string, string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return []byte(s.out), s.err
}

func (s *stubRunner) set(out string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out, s.err = out, err
}

func (s *stubRunner) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

var errOffline = errors.New("offline")

const twoItems = `{"items":[
 {"kind":"decision","title":"Commit this?","quote":"Want me to commit?","context":"","replies":["Commit","Not yet"],"suggested":0},
 {"kind":"veto","title":"Keep iOS 18?","quote":"I raised the target to 18.","context":"","replies":["Keep 18","Revert"],"suggested":-1}],
 "resolved":[]}`

const noItems = `{"items":[],"resolved":[]}`

func stopJSON(msg string) string { return turn("c1", "p1", msg) }

func turn(session, promptID, msg string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": session, "prompt_id": promptID, "cwd": "/tmp/proj",
		"hook_event_name": "Stop", "last_assistant_message": msg,
	})
	return string(b)
}

func stop(d Deps, in string) error {
	return Stop(context.Background(), d, strings.NewReader(in))
}

func stopOK(t *testing.T, d Deps, in string) {
	t.Helper()
	if err := stop(d, in); err != nil {
		t.Fatal(err)
	}
}

func stopFails(t *testing.T, d Deps, in string) {
	t.Helper()
	if err := stop(d, in); err == nil {
		t.Fatal("want an error")
	}
}

func TestStopStoresItemsWithRexLocation(t *testing.T) {
	r := &stubRunner{out: twoItems}
	d := deps(t, map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"})
	d.Runner = r
	claim(t, d, "c1")
	stopOK(t, d, stopJSON("Want me to commit?"))
	items := must(d.Store.List("session:a"))
	if len(items) != 2 || items[0].RexBlock != "block:1" || items[0].Project != "proj" {
		t.Fatalf("got %+v", items)
	}
	// Same turn again (a retry): no duplicates, and the log says so.
	err := stop(d, stopJSON("Want me to commit?"))
	if err == nil || !strings.Contains(err.Error(), "skipped 2 duplicate items") {
		t.Errorf("retry note: %v", err)
	}
	if items := must(d.Store.List("")); len(items) != 2 {
		t.Fatalf("duplicated: %d", len(items))
	}
}

// After compaction Claude Code reuses one prompt id for many turns. Each
// turn's items must still be stored, keyed by the message too.
func TestStopKeepsTurnsThatShareAPromptID(t *testing.T) {
	d := deps(t, nil)
	d.Runner = &stubRunner{out: twoItems}
	stopOK(t, d, turn("c1", "p1", "Want me to commit?"))
	stopOK(t, d, turn("c1", "p1", "Should I push too?"))
	items := must(d.Store.List(""))
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4", len(items))
	}
	keys := map[string]bool{}
	for _, it := range items {
		keys[it.SourceMsg] = true
	}
	want := map[string]bool{SourceMsg("p1", "Want me to commit?"): true, SourceMsg("p1", "Should I push too?"): true}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("keys %v, want %v", keys, want)
	}
}

func TestSourceMsg(t *testing.T) {
	sum := sha1.Sum([]byte("Want me to commit?"))
	h := hex.EncodeToString(sum[:])
	if got := SourceMsg("p1", "Want me to commit?"); got != "p1:"+h[:8] {
		t.Errorf("with a prompt id: %q", got)
	}
	if got := SourceMsg("", "Want me to commit?"); got != h[:16] {
		t.Errorf("without one: %q, want the old 16-digit hash", got)
	}
}

// A session started before the hooks were installed never ran SessionStart.
// Its Stop takes the pane it runs in, since nobody owns it, and its earlier
// pane-less items (an earlier turn's, say) follow.
func TestStopClaimsAnUnownedPane(t *testing.T) {
	d := deps(t, map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"})
	insert(t, d, store.Item{ClaudeSession: "c1", SourceMsg: "m0", Kind: store.KindDecision, Title: "Earlier?",
		CreatedAt: time.Unix(1, 0)})
	d.Runner = &stubRunner{out: twoItems}
	stopOK(t, d, stopJSON("Want me to commit?"))
	if owner := must(d.Store.PaneOwner("block:1")); owner != "c1" {
		t.Fatalf("pane owner %q", owner)
	}
	items := must(d.Store.List("session:a"))
	if len(items) != 3 {
		t.Fatalf("got %d items", len(items))
	}
	for _, it := range items {
		if it.RexBlock != "block:1" || it.RexSession != "session:a" {
			t.Errorf("item %d has no pane: %+v", it.ID, it)
		}
	}
}

// The Stop hook never takes a pane another conversation claimed.
func TestStopNeverTakesAnOwnedPane(t *testing.T) {
	d := deps(t, map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"})
	claim(t, d, "c-other")
	d.Runner = &stubRunner{out: twoItems}
	stopOK(t, d, stopJSON("Want me to commit?"))
	if owner := must(d.Store.PaneOwner("block:1")); owner != "c-other" {
		t.Fatalf("pane owner %q", owner)
	}
	for _, it := range must(d.Store.List("")) {
		if it.RexBlock != "" {
			t.Errorf("item took the other conversation's pane: %+v", it)
		}
	}
}

// A session that owns a pane keeps that one: its Stop doesn't claim the pane
// its environment names.
func TestStopDoesNotClaimASecondPane(t *testing.T) {
	d := deps(t, map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:2"})
	claim(t, d, "c1") // in block:2
	d.Env = func(k string) string {
		return map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"}[k]
	}
	d.Runner = &stubRunner{out: twoItems}
	stopOK(t, d, stopJSON("Want me to commit?"))
	if owner := must(d.Store.PaneOwner("block:1")); owner != "" {
		t.Fatalf("claimed a second pane for %q", owner)
	}
	for _, it := range must(d.Store.List("")) {
		if it.RexBlock != "block:2" {
			t.Errorf("item not in the session's pane: %+v", it)
		}
	}
}

// The race the panes table exists for: turn N's hook is still extracting
// when the user types /clear and a new session claims the pane. Turn N's
// items must not point at the pane, and the new session keeps it.
func TestStopDuringClearLeavesThePaneToTheNewSession(t *testing.T) {
	g := &gateRunner{release: make(chan struct{})}
	d := deps(t, map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"})
	d.Runner = g
	claim(t, d, "c1")

	errc := make(chan error, 1)
	go func() { errc <- stop(d, stopJSON("Want me to commit?")) }()
	waitFor(t, func() bool { return g.count() >= 1 })
	claim(t, d, "c2") // /clear
	close(g.release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	items := must(d.Store.List(""))
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
	for _, it := range items {
		if it.RexBlock != "" {
			t.Errorf("c1's item took c2's pane: %+v", it)
		}
	}

	// c2 still owns the pane: its own items get it.
	d.Runner = &stubRunner{out: twoItems}
	stopOK(t, d, turn("c2", "q1", "Want me to commit?"))
	for _, it := range must(d.Store.List("")) {
		if it.ClaudeSession == "c2" && it.RexBlock != "block:1" {
			t.Errorf("c2 lost its pane: %+v", it)
		}
	}
}

// queued counts the queued turns of sessions, due or not.
func queued(d Deps, sessions ...string) int {
	n := 0
	for _, s := range sessions {
		n += len(must(d.Store.PendingFor(s)))
	}
	return n
}

// later returns a clock past the first backoff of turns queued by d.Now.
func later(d Deps) func() time.Time {
	t := d.Now().Add(store.Backoff[0])
	return func() time.Time { return t }
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStopSkipsQuietTurnsWithoutCallingTheModel(t *testing.T) {
	r := &stubRunner{out: twoItems}
	d := deps(t, nil)
	d.Runner = r
	repoCalls := 0
	d.Repo = func(string) (string, string) { repoCalls++; return "proj", "main" }
	stopOK(t, d, stopJSON("Done. Tests pass."))
	if r.count() != 0 || repoCalls != 0 {
		t.Fatalf("quiet turn called the model %d times and Repo %d times", r.count(), repoCalls)
	}
}

func TestStopIgnoresSubagentsAndTheExtractor(t *testing.T) {
	r := &stubRunner{out: twoItems}
	d := deps(t, map[string]string{extract.GuardEnv: "1"})
	d.Runner = r
	stopOK(t, d, stopJSON("Want me to commit?"))
	d.Env = func(string) string { return "" }
	sub := strings.Replace(stopJSON("Want me to commit?"), `"cwd"`, `"agent_id":"a1","cwd"`, 1)
	stopOK(t, d, sub)
	if r.count() != 0 {
		t.Fatalf("model called %d times", r.count())
	}
}

func TestStopQueuesFailures(t *testing.T) {
	r := &stubRunner{err: errOffline}
	d := deps(t, nil)
	d.Runner = r
	stopFails(t, d, stopJSON("Want me to commit?"))
	if n := queued(d, "c1"); n != 1 {
		t.Fatalf("failure not queued: %d rows", n)
	}
	// The retry runs on another branch, but stores the turn's own.
	d.Repo = func(string) (string, string) { return "proj", "later" }
	d.Runner = &seqRunner{outs: []string{twoItems}} // the live turn finds nothing
	stopOK(t, d, stopJSON("Done."))
	if n := queued(d, "c1"); n != 0 {
		t.Fatalf("queue not drained")
	}
	items := must(d.Store.List(""))
	if len(items) != 2 {
		t.Fatalf("retried items missing: %d", len(items))
	}
	for _, it := range items {
		if it.Branch != "main" {
			t.Errorf("retry stored branch %q, want the turn's own", it.Branch)
		}
	}
}

// Any failure, not only the model's, leaves the turn queued.
func TestStopQueuesTurnsWhoseInsertFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	d := depsAt(t, nil, path)
	d.Runner = &stubRunner{out: twoItems}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER broken BEFORE INSERT ON items BEGIN SELECT RAISE(ABORT, 'disk on fire'); END`); err != nil {
		t.Fatal(err)
	}
	stopFails(t, d, stopJSON("Want me to commit?"))
	if n := queued(d, "c1"); n != 1 {
		t.Fatalf("turn lost: %d queued", n)
	}
	if _, err := raw.Exec(`DROP TRIGGER broken`); err != nil {
		t.Fatal(err)
	}
	d.Runner = &seqRunner{outs: []string{twoItems}}
	stopOK(t, d, turn("c1", "p2", "Done."))
	if n := len(must(d.Store.List(""))); n != 2 {
		t.Fatalf("retried items missing: %d", n)
	}
}

func TestStopResolvesAnsweredItems(t *testing.T) {
	d := deps(t, nil)
	d.Runner = &stubRunner{out: twoItems}
	stopOK(t, d, stopJSON("Want me to commit?"))
	open := must(d.Store.OpenFor("c1"))
	d.Runner = &stubRunner{out: fmt.Sprintf(`{"items":[],"resolved":[%d]}`, open[0].ID)}
	stopOK(t, d, turn("c1", "p2", "Committed."))
	if open := must(d.Store.OpenFor("c1")); len(open) != 1 {
		t.Fatalf("want 1 left open, got %d", len(open))
	}
}

// The answered-in-the-flow case: turn N's extraction is still running (about
// 14 s) when the user answers and turn N+1 ends. Unless the two run in order,
// turn N+1 sees no open items, and turn N's items stay open although the user
// answered them.
func TestStopSerializesTurnsOfOneSession(t *testing.T) {
	g := &gateRunner{release: make(chan struct{})}
	d := deps(t, nil)
	d.Runner = g

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = stop(d, stopJSON("Want me to commit?"))
	}()
	waitFor(t, func() bool { return g.count() >= 1 })
	go func() { defer wg.Done(); errs[1] = stop(d, turn("c1", "p2", "Committed.")) }()
	time.Sleep(50 * time.Millisecond) // give turn N+1 a chance to run early
	close(g.release)
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if g.count() != 2 || !strings.Contains(g.prompt(1), "Commit this?") {
		t.Fatalf("turn N+1 didn't see turn N's items: calls=%d", g.count())
	}
}

// gateRunner holds its first call until release is closed.
type gateRunner struct {
	mu      sync.Mutex
	prompts []string
	release chan struct{}
}

func (g *gateRunner) Run(_ context.Context, prompt, _ string) ([]byte, error) {
	g.mu.Lock()
	g.prompts = append(g.prompts, prompt)
	n := len(g.prompts)
	g.mu.Unlock()
	if n == 1 {
		<-g.release
		return []byte(twoItems), nil
	}
	return []byte(noItems), nil
}

func (g *gateRunner) count() int          { g.mu.Lock(); defer g.mu.Unlock(); return len(g.prompts) }
func (g *gateRunner) prompt(i int) string { g.mu.Lock(); defer g.mu.Unlock(); return g.prompts[i] }

// seqRunner returns outs in order, then no items, and records its prompts.
type seqRunner struct {
	mu      sync.Mutex
	outs    []string
	prompts []string
}

func (s *seqRunner) Run(_ context.Context, prompt, _ string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, prompt)
	if len(s.outs) == 0 {
		return []byte(noItems), nil
	}
	out := s.outs[0]
	s.outs = s.outs[1:]
	return []byte(out), nil
}

// A queued turn N runs before turn N+1 of its session, so N+1 sees N's items,
// even while N is backing off (the clock hasn't moved).
func TestStopRunsItsSessionsQueuedTurnsFirst(t *testing.T) {
	d := deps(t, nil)
	d.Runner = &stubRunner{err: errOffline}
	stopFails(t, d, stopJSON("Want me to commit?"))
	r := &seqRunner{outs: []string{twoItems}}
	d.Runner = r
	stopOK(t, d, turn("c1", "p2", "Committed."))
	if len(r.prompts) != 2 || !strings.Contains(r.prompts[0], "Want me to commit?") ||
		!strings.Contains(r.prompts[1], "Commit this?") {
		t.Fatalf("turns out of order: %q", r.prompts)
	}
}

// A queued turn that fails again doesn't hold up its session: the turn that
// just ended still runs, and the old one stays queued.
func TestStopRunsTheLiveTurnWhenItsQueuedTurnFailsAgain(t *testing.T) {
	d := deps(t, nil)
	d.Runner = &stubRunner{err: errOffline}
	stopFails(t, d, stopJSON("Want me to commit?"))
	d.Runner = failOn{"Want me to commit?"}
	stopFails(t, d, turn("c1", "p2", "Should I push?"))
	items := must(d.Store.List(""))
	if len(items) != 2 || items[0].SourceMsg != SourceMsg("p2", "Should I push?") {
		t.Fatalf("live turn didn't run: %+v", items)
	}
	if rows := must(d.Store.PendingFor("c1")); len(rows) != 1 {
		t.Fatalf("want the old turn still queued, got %d rows", len(rows))
	}
}

// failOn fails for a message containing text and finds two items in any
// other.
type failOn struct{ text string }

func (f failOn) Run(_ context.Context, prompt, _ string) ([]byte, error) {
	if strings.Contains(prompt, f.text) {
		return nil, errOffline
	}
	return []byte(twoItems), nil
}

// A session resumed in another pane takes its queued turn's items along.
func TestRetryFollowsAResumedSession(t *testing.T) {
	env := map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"}
	d := deps(t, env)
	r := &stubRunner{err: errOffline}
	d.Runner = r
	claim(t, d, "c1")
	stopFails(t, d, stopJSON("Want me to commit?"))
	env["REX_SESSION"], env["REX_BLOCK"] = "session:b", "block:2"
	claim(t, d, "c1") // claude --resume in another pane
	r.set(twoItems, nil)
	d.Now = later(d)
	stopOK(t, d, turn("c3", "x1", "Done."))
	items := must(d.Store.List(""))
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
	for _, it := range items {
		if it.RexBlock != "block:2" || it.RexSession != "session:b" {
			t.Errorf("item went to the pane c1 left: %+v", it)
		}
	}
}

// The schedule as hooks see it: other sessions' Stops retry a queued turn
// only once its backoff is over, and the sixth failure drops it.
func TestRetryScheduleEndToEnd(t *testing.T) {
	r := &stubRunner{err: errOffline}
	d := deps(t, nil)
	d.Runner = r
	now := d.Now()
	d.Now = func() time.Time { return now }
	stopFails(t, d, stopJSON("Want me to commit?"))
	for i, wait := range store.Backoff {
		now = now.Add(wait - time.Second)
		stopOK(t, d, turn("c3", "x1", "Done."))
		if r.count() != i+1 {
			t.Fatalf("retried before the backoff ended, after failure %d", i+1)
		}
		now = now.Add(time.Second)
		err := stop(d, turn("c3", "x1", "Done."))
		if err == nil {
			t.Fatal("want an error")
		}
		if r.count() != i+2 {
			t.Fatalf("not retried %v after failure %d", wait, i+1)
		}
		last := i == len(store.Backoff)-1
		if dropped := strings.Contains(err.Error(), "dropped queued turn"); dropped != last {
			t.Fatalf("after failure %d: dropped=%v in %q", i+2, dropped, err)
		}
	}
	if rows := must(d.Store.PendingFor("c1")); len(rows) != 0 {
		t.Fatalf("not dropped after the sixth failure")
	}
}

// A queued turn whose retries all died before reporting back is dropped by
// the next claim, and the Stop that dropped it says so for the log.
func TestStopReportsATurnDroppedAfterCrashedRetries(t *testing.T) {
	r := &stubRunner{err: errOffline}
	d := deps(t, nil)
	d.Runner = r
	stopFails(t, d, stopJSON("Want me to commit?"))
	id := must(d.Store.PendingFor("c1"))[0].ID
	for i := 0; i < store.MaxAttempts; i++ {
		if !must(d.Store.ClaimPendingEarly(id, d.Now())) {
			t.Fatalf("claim %d failed", i+1)
		}
	}
	r.set(twoItems, nil)
	err := stop(d, turn("c1", "p2", "Should I push?"))
	var dropped *store.DroppedError
	if !errors.As(err, &dropped) || dropped.ID != id || dropped.ClaudeSession != "c1" {
		t.Fatalf("drop not reported: %v", err)
	}
	if n := queued(d, "c1"); n != 0 {
		t.Fatalf("spent turn still queued: %d", n)
	}
	if n := len(must(d.Store.List(""))); n != 2 {
		t.Fatalf("live turn didn't run: %d items", n)
	}
}

// A retry must use the prompt from its own turn, not whatever the user typed
// since.
func TestRetryKeepsItsOwnPrompt(t *testing.T) {
	r := &stubRunner{err: errOffline}
	d := deps(t, nil)
	d.Runner = r
	if err := d.Store.RecordPrompt("c1", "the original prompt", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	stopFails(t, d, stopJSON("Want me to commit?"))
	if err := d.Store.RecordPrompt("c1", "a much later prompt", time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	r.set(twoItems, nil)
	stopOK(t, d, turn("c1", "p9", "Done."))
	retried := 0
	for _, it := range must(d.Store.List("")) {
		if !strings.HasPrefix(it.SourceMsg, "p1:") { // the current turn (p9) correctly uses the later prompt
			continue
		}
		retried++
		if it.Prompt != "the original prompt" {
			t.Fatalf("retry stored prompt %q", it.Prompt)
		}
	}
	if retried == 0 {
		t.Fatal("retried turn stored nothing")
	}
}

// A retry can run long after its turn, from another agent's Stop, when a new
// Claude session may own the pane (after /clear, say). Its items must not
// point at that pane, and the new owner keeps it.
func TestRetryDoesNotStealThePane(t *testing.T) {
	r := &stubRunner{err: errOffline}
	env := map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"}
	d := deps(t, env)
	d.Runner = r
	claim(t, d, "c1")
	stopFails(t, d, stopJSON("Want me to commit?"))

	// /clear in block:1: c2 runs there now and has an item.
	insert(t, d, store.Item{ClaudeSession: "c2", SourceMsg: "m", Kind: store.KindDecision, Title: "New?",
		RexSession: "session:a", RexBlock: "block:1", CreatedAt: time.Unix(1, 0)})
	claim(t, d, "c2")

	// Another agent, in another pane, stops and triggers the retry.
	env["REX_BLOCK"] = "block:2"
	r.set(twoItems, nil)
	d.Now = later(d)
	stopOK(t, d, turn("c3", "x1", "Done."))

	retried := 0
	for _, it := range must(d.Store.List("")) {
		switch it.ClaudeSession {
		case "c2":
			if it.RexBlock != "block:1" {
				t.Errorf("retry took c2's pane: %+v", it)
			}
		case "c1":
			retried++
			if it.RexBlock != "" {
				t.Errorf("retried item points at c2's pane: %+v", it)
			}
		}
	}
	if retried == 0 {
		t.Fatal("retried turn stored nothing")
	}
}

// A retry whose agent still owns the pane keeps it.
func TestRetryInItsOwnPaneKeepsThePane(t *testing.T) {
	r := &stubRunner{err: errOffline}
	d := deps(t, map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"})
	d.Runner = r
	claim(t, d, "c1")
	stopFails(t, d, stopJSON("Want me to commit?"))
	r.set(twoItems, nil)
	stopOK(t, d, turn("c1", "p2", "Done."))
	items := must(d.Store.List(""))
	if len(items) == 0 {
		t.Fatal("nothing stored")
	}
	for _, it := range items {
		if it.RexBlock != "block:1" {
			t.Errorf("item lost its pane: %+v", it)
		}
	}
}

// Only the agent's own next stop ends a next-stop snooze; another agent's
// Stop retrying one of its queued turns does not.
func TestRetryDoesNotEndNextStopSnoozes(t *testing.T) {
	r := &stubRunner{err: errOffline}
	d := deps(t, nil)
	d.Runner = r
	stopFails(t, d, stopJSON("Want me to commit?"))
	id := insert(t, d, store.Item{ClaudeSession: "c1", SourceMsg: "m", Kind: store.KindDecision, Title: "Later?",
		CreatedAt: time.Unix(1, 0)})
	if err := d.Store.Snooze(id, time.Time{}, store.TriggerNextStop); err != nil {
		t.Fatal(err)
	}
	r.set(twoItems, nil)
	d.Now = later(d)
	stopOK(t, d, turn("c3", "x1", "Done."))
	if n := queued(d, "c1"); n != 0 {
		t.Fatalf("retry didn't run")
	}
	if st := stateOf(t, d, id); st != store.StateSnoozed {
		t.Fatalf("retry woke the item: %s", st)
	}
	stopOK(t, d, turn("c1", "p2", "Done."))
	if st := stateOf(t, d, id); st != store.StateOpen {
		t.Fatalf("live stop didn't wake the item: %s", st)
	}
}

func stateOf(t *testing.T, d Deps, id int64) string {
	t.Helper()
	for _, it := range must(d.Store.List("")) {
		if it.ID == id {
			return it.State
		}
	}
	t.Fatalf("item %d not waiting", id)
	return ""
}

// A Stop leaves another session's queued turn alone while that session is
// busy, without waiting for it.
func TestRetrySkipsBusySessions(t *testing.T) {
	r := &stubRunner{err: errOffline}
	d := deps(t, nil)
	d.Runner = r
	stopFails(t, d, stopJSON("Want me to commit?"))
	unlock, err := d.Locks.Lock(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	r.set(twoItems, nil)
	done := make(chan error, 1)
	d.Now = later(d)
	go func() { done <- stop(d, turn("c3", "x1", "Done.")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop waited for a busy session")
	}
	if n := queued(d, "c1"); n != 1 {
		t.Fatalf("busy session's turn touched: %d queued", n)
	}
	if r.count() != 1 {
		t.Fatalf("model called %d times", r.count())
	}
}

// After one failure the hook stops retrying: the model is likely down, and
// every retry would wait for its timeout.
func TestRetryStopsAfterTheFirstFailure(t *testing.T) {
	r := &stubRunner{err: errOffline}
	d := deps(t, nil)
	d.Runner = r
	for _, s := range []string{"c1", "c2", "c3"} {
		stopFails(t, d, turn(s, "p1", "Want me to commit?"))
	}
	before := r.count()
	d.Now = later(d)
	stopFails(t, d, turn("c4", "p1", "Done."))
	if n := r.count() - before; n != 1 {
		t.Fatalf("model called %d times after it failed", n)
	}
}

// Two agents stopping at once, each with a queued turn of the other's
// session, both finish and every turn is stored.
func TestConcurrentStopsWithEachOthersQueuedTurns(t *testing.T) {
	r := &stubRunner{err: errOffline}
	d := deps(t, nil)
	d.Runner = r
	stopFails(t, d, turn("c1", "p1", "Want me to commit?"))
	stopFails(t, d, turn("c2", "p1", "Want me to commit?"))
	d.Runner = byMessage{}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, s := range []string{"c2", "c1"} {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = stop(d, turn(s, "p2", "Done.")) }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock")
	}
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if n := queued(d, "c1", "c2"); n != 0 {
		t.Fatalf("%d turns still queued", n)
	}
	for _, s := range []string{"c1", "c2"} {
		if n := len(must(d.Store.OpenFor(s))); n != 2 {
			t.Errorf("%s: %d items, want 2", s, n)
		}
	}
}

// byMessage finds two items in a message that asks to commit, and nothing
// in any other.
type byMessage struct{}

func (byMessage) Run(_ context.Context, prompt, _ string) ([]byte, error) {
	if strings.Contains(prompt, "Want me to commit?") {
		return []byte(twoItems), nil
	}
	return []byte(noItems), nil
}

func TestFileLockTryLock(t *testing.T) {
	l := FileLock(t.TempDir())
	unlock, ok, err := l.TryLock("c1")
	if err != nil || !ok {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	if _, ok, err := l.TryLock("c1"); err != nil || ok {
		t.Fatalf("second: ok=%v err=%v", ok, err)
	}
	unlock()
	unlock2, ok, err := l.TryLock("c1")
	if err != nil || !ok {
		t.Fatalf("after unlock: ok=%v err=%v", ok, err)
	}
	unlock2()
}

func TestFileLockWaitEndsWithTheContext(t *testing.T) {
	l := FileLock(t.TempDir())
	unlock, err := l.Lock(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := l.Lock(ctx, "c1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestFileLockUsesABoundedSetOfFiles(t *testing.T) {
	dir := t.TempDir()
	l := FileLock(dir)
	for i := 0; i < 500; i++ {
		unlock, ok, err := l.TryLock(fmt.Sprint("session-", i))
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			unlock()
		}
	}
	if n := len(must(os.ReadDir(dir))); n > LockShards {
		t.Fatalf("%d lock files", n)
	}
}

func TestRepoFallsBackToTheDirectoryName(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "plain")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// TMPDIR may sit inside a repository: keep git from looking above base.
	t.Setenv("GIT_CEILING_DIRECTORIES", base)
	// jj has no such setting; if it finds a repository, Repo rightly names it.
	if jj, err := exec.LookPath("jj"); err == nil {
		cmd := exec.Command(jj, "--ignore-working-copy", "root")
		cmd.Dir = dir
		if cmd.Run() == nil {
			t.Skip("the temp dir is inside a jj repository")
		}
	}
	if p, b := Repo(dir); p != "plain" || b != "" {
		t.Fatalf("got %q %q", p, b)
	}
}

// In jj the working copy is usually a new commit on top of the bookmark.
func TestRepoFindsTheNearestJJBookmark(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	cfg := filepath.Join(t.TempDir(), "jj.toml")
	if err := os.WriteFile(cfg, []byte("user.name = \"t\"\nuser.email = \"t@example.com\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	dir := filepath.Join(t.TempDir(), "proj")
	for _, args := range [][]string{
		{"git", "init", dir},
		{"-R", dir, "commit", "-m", "first"},
		{"-R", dir, "bookmark", "create", "feature", "-r", "@-"},
		{"-R", dir, "new"},
	} {
		if out, err := exec.Command("jj", args...).CombinedOutput(); err != nil {
			t.Fatalf("jj %v: %v\n%s", args, err, out)
		}
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if p, b := Repo(sub); p != "proj" || b != "feature" {
		t.Fatalf("got %q %q", p, b)
	}
}

// Headless runs (claude -p from scripts and benchmarks) must not fill the
// inbox or spend extraction calls. Interactive sessions say "cli".
func TestHooksSkipHeadlessRuns(t *testing.T) {
	env := map[string]string{"CLAUDE_CODE_ENTRYPOINT": "sdk-cli", "REX_SESSION": "session:a", "REX_BLOCK": "block:1"}
	d := deps(t, env)
	r := &stubRunner{out: twoItems}
	d.Runner = r
	if err := Prompt(d, strings.NewReader(`{"session_id":"c1","prompt":"go"}`)); err != nil {
		t.Fatal(err)
	}
	if p := must(d.Store.LastPrompt("c1")); p != "" {
		t.Errorf("headless prompt recorded: %q", p)
	}
	claim(t, d, "c1")
	if owner := must(d.Store.PaneOwner("block:1")); owner != "" {
		t.Errorf("headless start claimed the pane for %q", owner)
	}
	stopOK(t, d, stopJSON("Want me to commit?"))
	if owner := must(d.Store.PaneOwner("block:1")); owner != "" {
		t.Errorf("headless stop claimed the pane for %q", owner)
	}
	if r.count() != 0 {
		t.Errorf("model called %d times for a headless stop", r.count())
	}
	if n := queued(d, "c1"); n != 0 {
		t.Errorf("headless stop queued %d turns", n)
	}

	env["CLAUDE_CODE_ENTRYPOINT"] = "cli"
	stopOK(t, d, stopJSON("Want me to commit?"))
	items := must(d.Store.List(""))
	if r.count() != 1 || len(items) != 2 {
		t.Fatalf("interactive stop: %d model calls, %d items", r.count(), len(items))
	}
}

// blockRunner waits for its context to end, as a model call does when the
// hook is told to stop (the pane closed, say).
type blockRunner struct{ started chan struct{} }

func (b blockRunner) Run(ctx context.Context, _, _ string) ([]byte, error) {
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

// A Stop interrupted mid-extraction queues its turn instead of losing it.
func TestStopQueuesATurnInterruptedMidExtraction(t *testing.T) {
	d := deps(t, nil)
	b := blockRunner{started: make(chan struct{})}
	d.Runner = b
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-b.started; cancel() }()
	if err := Stop(ctx, d, strings.NewReader(stopJSON("Want me to commit?"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("want the cancellation reported, got %v", err)
	}
	if n := queued(d, "c1"); n != 1 {
		t.Fatalf("interrupted turn lost: %d queued", n)
	}
}

// clockRunner returns out, moving the clock on by took first: extraction
// that takes a while.
type clockRunner struct {
	mu   sync.Mutex
	now  time.Time
	took time.Duration
	out  string
	err  error
}

func (c *clockRunner) Run(context.Context, string, string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.took)
	return []byte(c.out), c.err
}

func (c *clockRunner) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

// Items are dated when the agent stopped, not when extraction finished, so
// a prompt typed in between counts as written after the question.
func TestStopDatesItemsWhenTheAgentStopped(t *testing.T) {
	r := &clockRunner{now: time.Unix(10_000, 0), took: 40 * time.Second, out: twoItems}
	d := deps(t, nil)
	d.Runner, d.Now = r, r.Now
	stopOK(t, d, stopJSON("Want me to commit?"))
	items := must(d.Store.List(""))
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
	for _, it := range items {
		if !it.CreatedAt.Equal(time.Unix(10_000, 0)) {
			t.Errorf("dated %v, want the hook's start", it.CreatedAt)
		}
	}
}

func TestRetryKeepsTheTurnsTime(t *testing.T) {
	r := &clockRunner{now: time.Unix(10_000, 0), err: errOffline}
	d := deps(t, nil)
	d.Runner, d.Now = r, r.Now
	stopFails(t, d, stopJSON("Want me to commit?"))
	r.mu.Lock()
	r.now, r.err, r.out = time.Unix(50_000, 0), nil, twoItems
	r.mu.Unlock()
	stopOK(t, d, turn("c1", "p9", "Done."))
	retried := 0
	for _, it := range must(d.Store.List("")) {
		if !strings.HasPrefix(it.SourceMsg, "p1:") {
			continue
		}
		retried++
		if !it.CreatedAt.Equal(time.Unix(10_000, 0)) {
			t.Errorf("retry dated %v, want the turn's own time", it.CreatedAt)
		}
	}
	if retried == 0 {
		t.Fatal("retried turn stored nothing")
	}
}

// stopIn is a Stop hook input with a transcript path and a cwd.
func stopIn(session, promptID, cwd, transcriptPath, msg string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": session, "prompt_id": promptID, "cwd": cwd, "transcript_path": transcriptPath,
		"hook_event_name": "Stop", "last_assistant_message": msg,
	})
	return string(b)
}

// Items keep the directory the conversation was started in, which
// claude --resume and the running-Claude check need, not wherever the agent
// had wandered to. Project and branch come from it too.
func TestStopStoresTheLaunchDirectory(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "code", "rex.app")
	other := filepath.Join(root, "code", "other")
	transcript := filepath.Join(root, "projects", transcriptpkg.ProjectDirName(repo), "c1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	head := `{"type":"mode"}` + "\n" + `{"type":"user","cwd":"` + repo + `"}` + "\n"
	if err := os.WriteFile(transcript, []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, cwd, want string }{
		{"subdirectory", filepath.Join(repo, "internal", "hook"), repo},
		{"another repo", other, repo},
		{"no transcript", other, other},
	}
	for i, c := range cases {
		d := deps(t, nil)
		d.Runner = &stubRunner{out: twoItems}
		var looked []string
		d.Repo = func(dir string) (string, string) { looked = append(looked, dir); return filepath.Base(dir), "main" }
		path := transcript
		if c.name == "no transcript" {
			path = filepath.Join(root, "projects", "-gone", "c9.jsonl")
		}
		stopOK(t, d, stopIn(fmt.Sprint("c", i), "p1", c.cwd, path, "Want me to commit?"))
		items := must(d.Store.List(""))
		if len(items) != 2 || items[0].Cwd != c.want || items[0].Project != filepath.Base(c.want) {
			t.Errorf("%s: %+v", c.name, items)
		}
		if len(looked) != 1 || looked[0] != c.want {
			t.Errorf("%s: Repo looked in %q, want %q", c.name, looked, c.want)
		}
	}
}

// A queued turn keeps the launch directory worked out when it ended.
func TestRetryKeepsTheLaunchDirectory(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	transcript := filepath.Join(root, "projects", "x", "c1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"type":"user","cwd":"`+repo+`"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &stubRunner{err: errOffline}
	d := deps(t, nil)
	d.Runner = r
	stopFails(t, d, stopIn("c1", "p1", filepath.Join(root, "elsewhere"), transcript, "Want me to commit?"))
	os.Remove(transcript) // gone by the time of the retry
	d.Runner = &seqRunner{outs: []string{twoItems}}
	stopOK(t, d, turn("c1", "p2", "Done."))
	items := must(d.Store.List(""))
	if len(items) != 2 || items[0].Cwd != repo {
		t.Fatalf("retry lost the launch directory: %+v", items)
	}
}

// A failing extraction (logged out, usage limit) is recorded for the panel,
// and the next one that works clears it. Quiet turns, which call no model,
// say nothing either way.
func TestStopRecordsExtractionHealth(t *testing.T) {
	d := deps(t, nil)
	d.Runner = &stubRunner{err: errors.New("claude -p: Not logged in · Please run /login")}
	stopFails(t, d, stopJSON("Want me to commit?"))
	h := must(d.Store.Health())
	if !h.Failing() || h.LastError != "claude -p: Not logged in · Please run /login" || h.Pending != 1 {
		t.Fatalf("after a failure: %+v", h)
	}
	// Same clock: c1's queued turn isn't due, so only c2's turns run.
	d.Runner = &stubRunner{out: noItems}
	stopOK(t, d, turn("c2", "p1", "Done. All tests pass."))
	if h := must(d.Store.Health()); !h.Failing() {
		t.Fatalf("a quiet turn cleared the failure: %+v", h)
	}
	// A second on, still before c1's retry is due.
	d.Now = func() time.Time { return time.Unix(10_001, 0) }
	stopOK(t, d, turn("c2", "p2", "Want me to push?"))
	h = must(d.Store.Health())
	if h.Failing() || !h.LastOKAt.Equal(time.Unix(10_001, 0)) || h.Pending != 1 {
		t.Fatalf("after a success: %+v", h)
	}
}

func TestFileLockFilesArePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	unlock, ok, err := FileLock(dir).TryLock("c1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	unlock()
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("lock directory: %v %v", fi.Mode(), err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.lock"))
	for _, f := range files {
		if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi.Mode(), err)
		}
	}
	if len(files) != 1 {
		t.Errorf("lock files: %q", files)
	}
}

// repeatOut is a turn that asks open item id again, and lists unknown ids.
func repeatOut(id int64) string {
	return fmt.Sprintf(`{"items":[{"kind":"decision","title":"Commit this now?","quote":"Commit now?","context":"","replies":["Commit"],"suggested":0}],
	 "resolved":[],"superseded":[%d, 99999]}`, id)
}

func answeredWith(t *testing.T, d Deps, id int64) string {
	t.Helper()
	it, found, err := d.Store.Item(id)
	if err != nil || !found {
		t.Fatalf("item %d: %v %v", id, found, err)
	}
	return it.AnsweredWith
}

func TestStopClosesRepeatedQuestions(t *testing.T) {
	d := deps(t, nil)
	d.Runner = &stubRunner{out: twoItems}
	stopOK(t, d, stopJSON("Want me to commit?"))
	open := must(d.Store.OpenFor("c1"))
	d.Runner = &stubRunner{out: repeatOut(open[0].ID)}
	stopOK(t, d, turn("c1", "p2", "Commit now?"))
	if got := answeredWith(t, d, open[0].ID); got != "(asked again)" {
		t.Fatalf("repeat answered with %q", got)
	}
	titles := map[string]bool{}
	for _, it := range must(d.Store.OpenFor("c1")) {
		titles[it.Title] = true
	}
	if len(titles) != 2 || !titles[open[1].Title] || !titles["Commit this now?"] {
		t.Fatalf("open after the repeat: %v", titles)
	}
}

// An id the extractor says is both answered and asked again is answered.
func TestStopCountsAnIDInBothListsOnce(t *testing.T) {
	d := deps(t, nil)
	d.Runner = &stubRunner{out: twoItems}
	stopOK(t, d, stopJSON("Want me to commit?"))
	id := must(d.Store.OpenFor("c1"))[0].ID
	d.Runner = &stubRunner{out: fmt.Sprintf(`{"items":[{"kind":"decision","title":"Push it?","quote":"","context":"","replies":[],"suggested":-1}],
	 "resolved":[%d],"superseded":[%d]}`, id, id)}
	stopOK(t, d, turn("c1", "p2", "Committed. Push it?"))
	if got := answeredWith(t, d, id); got != "(answered in the session)" {
		t.Fatalf("answered with %q", got)
	}
}

func TestRetriedTurnClosesRepeatedQuestions(t *testing.T) {
	d := deps(t, nil)
	d.Runner = &stubRunner{out: twoItems}
	stopOK(t, d, stopJSON("Want me to commit?"))
	id := must(d.Store.OpenFor("c1"))[0].ID
	d.Runner = &stubRunner{err: errOffline}
	stopFails(t, d, turn("c1", "p2", "Commit now?"))
	if got := answeredWith(t, d, id); got != "" {
		t.Fatalf("closed by a failed turn: %q", got)
	}
	d.Runner = &seqRunner{outs: []string{repeatOut(id)}} // the retry, then the live turn finds nothing
	stopOK(t, d, turn("c1", "p3", "Done."))
	if got := answeredWith(t, d, id); got != "(asked again)" {
		t.Fatalf("retry answered with %q", got)
	}
}
