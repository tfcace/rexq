package panel

import (
	"errors"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tfcace/rexq/internal/store"
)

// These tests drive model.Update with messages and keys, as Bubble Tea
// would, and look at the commands and state it returns.

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// panelOn is a panel in session:panel over a scratch store, with a clock
// the test moves. Its agents' panes are gone and their directories don't
// exist, so nothing is ever delivered.
func panelOn(t *testing.T) (model, *store.Store, *clock, *fakeRex) {
	t.Helper()
	f := goneBlock()
	m, s := testModel(t, f, "session:panel")
	clk := &clock{time.Now()}
	m.d.Now = clk.now
	m.w, m.h = 120, 40
	return m, s, clk, f
}

// ask stores an open question in its own project and conversation.
func ask(t *testing.T, s *store.Store, proj, kind string, age time.Duration, now time.Time) store.Item {
	t.Helper()
	it := store.Item{RexSession: "session:panel", ClaudeSession: "c-" + proj, SourceMsg: "m", Cwd: "/nonexistent/" + proj,
		Project: proj, Kind: kind, Title: "Question for " + proj, Replies: []string{"yes", "no"}, CreatedAt: now.Add(-age)}
	id, _, err := s.Insert(it)
	if err != nil {
		t.Fatal(err)
	}
	it.ID, it.State = id, store.StateOpen
	return it
}

func send(t *testing.T, m model, msg tea.Msg) (model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	return next.(model), cmd
}

func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func press(t *testing.T, m model, keys ...string) (model, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, k := range keys {
		m, cmd = send(t, m, keyMsg(k))
	}
	return m, cmd
}

// load runs a load to the end, as a tick finding new data would.
func load(t *testing.T, m model) model {
	t.Helper()
	cmd := m.reload()
	m, _ = send(t, m, cmd())
	return m
}

func selectedID(m model) int64 {
	it, _ := m.selected()
	return it.ID
}

// selectB loads A (most urgent) and B, and selects B.
func selectB(t *testing.T) (model, *store.Store, *clock, store.Item) {
	m, s, clk, _ := panelOn(t)
	ask(t, s, "alpha", store.KindBlocker, time.Hour, clk.t)
	b := ask(t, s, "bravo", store.KindDecision, time.Hour, clk.t)
	m = load(t, m)
	m, _ = press(t, m, "j")
	if selectedID(m) != b.ID {
		t.Fatalf("selected %d, want bravo %d", selectedID(m), b.ID)
	}
	return m, s, clk, b
}

// A question arriving above the selection moves the list, not the target.
func TestAnswerActsOnTheSelectionAfterAReload(t *testing.T) {
	m, s, clk, b := selectB(t)
	ask(t, s, "charlie", store.KindBlocker, 0, clk.t) // sorts above bravo
	m = load(t, m)
	if selectedID(m) != b.ID {
		t.Fatalf("selection moved to %d", selectedID(m))
	}
	m, cmd := press(t, m, "1")
	if cmd == nil || m.busy == nil || m.busy.id != b.ID {
		t.Fatalf("busy %+v cmd %v", m.busy, cmd != nil)
	}
	done := doneOf(t, cmd)
	if done.id != b.ID || !strings.Contains(done.toast, "bravo") {
		t.Fatalf("answered %+v", done)
	}
}

func TestReplyActsOnItsQuestionAfterAReload(t *testing.T) {
	m, s, clk, b := selectB(t)
	m, _ = press(t, m, "r")
	ask(t, s, "charlie", store.KindBlocker, 0, clk.t)
	m = load(t, m)
	if f := m.footer(); !strings.Contains(f, "reply to bravo: Question for bravo") {
		t.Fatalf("prompt doesn't name the target: %q", f)
	}
	m, cmd := press(t, m, "o", "k", "enter")
	if cmd == nil || m.busy == nil || m.busy.id != b.ID {
		t.Fatalf("busy %+v", m.busy)
	}
	if done := doneOf(t, cmd); done.id != b.ID {
		t.Fatalf("answered %d", done.id)
	}
}

func TestSnoozeActsOnItsQuestionAfterAReload(t *testing.T) {
	m, s, clk, b := selectB(t)
	m, _ = press(t, m, "z")
	c := ask(t, s, "charlie", store.KindBlocker, 0, clk.t)
	m = load(t, m)
	m, cmd := press(t, m, "3")
	if cmd == nil {
		t.Fatal("no command")
	}
	m, _ = send(t, m, cmd())
	for id, want := range map[int64]string{b.ID: store.StateSnoozed, c.ID: store.StateOpen} {
		if it, _, _ := s.Item(id); it.State != want {
			t.Errorf("item %d is %s, want %s", id, it.State, want)
		}
	}
	if !strings.Contains(m.toast, "Question for bravo") {
		t.Errorf("toast %q", m.toast)
	}
}

// When the selected question goes away, the cursor stays put and keys wait
// a moment, so a key meant for it can't land on its neighbour.
func TestResolvedSelectionHoldsActions(t *testing.T) {
	m, s, clk, b := selectB(t)
	m.expanded = true
	if err := s.Dismiss(b.ID); err != nil { // as from another panel
		t.Fatal(err)
	}
	m = load(t, m)
	if m.toast != "that question was resolved" || m.expanded {
		t.Fatalf("toast %q expanded %v", m.toast, m.expanded)
	}
	for _, k := range []string{"1", "d", "x", "z", "r"} {
		if next, cmd := press(t, m, k); cmd != nil || next.mode != normal {
			t.Fatalf("%s acted right away", k)
		}
	}
	clk.t = clk.t.Add(settle)
	if _, cmd := press(t, m, "1"); cmd == nil {
		t.Fatal("still held after a second")
	}
	m.frozen = clk.t.Add(time.Hour)
	m, _ = press(t, m, "k")
	if _, cmd := press(t, m, "1"); cmd == nil {
		t.Fatal("still held after moving")
	}
}

func TestReplyIsKeptWhenItsQuestionGoes(t *testing.T) {
	m, s, _, b := selectB(t)
	m, _ = press(t, m, "r", "o", "k")
	if err := s.Answer(b.ID, "elsewhere"); err != nil {
		t.Fatal(err)
	}
	m = load(t, m)
	m, cmd := press(t, m, "enter")
	if cmd != nil || m.toast != "that question is gone; your reply is kept" || m.input != "ok" || m.mode != normal {
		t.Fatalf("cmd %v toast %q input %q mode %v", cmd != nil, m.toast, m.input, m.mode)
	}
}

// One action at a time: a second press while the first runs does nothing.
func TestActionKeysWaitForTheActionInFlight(t *testing.T) {
	m, _, _, b := selectB(t)
	m, cmd := press(t, m, "1")
	if cmd == nil || m.busy == nil {
		t.Fatal("first press did nothing")
	}
	if f := m.footer(); !strings.Contains(f, "sending to bravo…") {
		t.Fatalf("footer %q", f)
	}
	for _, k := range []string{"1", "2", "enter", "d", "x", "z", "u"} {
		if next, cmd := press(t, m, k); cmd != nil || next.mode != normal {
			t.Fatalf("%s while busy: acted", k)
		}
	}
	m, _ = press(t, m, "r", "h", "i")
	if next, cmd := press(t, m, "enter"); cmd != nil || next.input != "hi" {
		t.Fatalf("reply sent while busy")
	}
	m, _ = press(t, m, "esc")
	m, reload := send(t, m, doneMsg{id: b.ID, toast: "sent"})
	// Keys still wait for the list that shows the result.
	if next, cmd := press(t, m, "1"); cmd != nil || next.toast != "sent" {
		t.Fatalf("acted before the reload, or lost the toast %q", next.toast)
	}
	m, _ = send(t, m, reload())
	if m.busy != nil {
		t.Fatal("still busy")
	}
	if _, cmd := press(t, m, "1"); cmd == nil {
		t.Fatal("no action after the first finished")
	}
}

func TestJumpShowsWhileInFlight(t *testing.T) {
	m, _, _, _ := selectB(t)
	m, cmd := press(t, m, "enter")
	if cmd == nil || m.busy == nil || !strings.Contains(m.footer(), "jumping to bravo…") {
		t.Fatalf("footer %q", m.footer())
	}
}

// A failed load shows why and leaves the retry to the tick.
func TestFailedLoadDoesNotReloadAtOnce(t *testing.T) {
	m, s, _, _ := panelOn(t)
	cmd := m.reload()
	s.Close()
	m, next := send(t, m, cmd())
	if next != nil || !strings.HasPrefix(m.toast, "couldn't read the store: ") || !m.loadFailed {
		t.Fatalf("cmd %v toast %q", next != nil, m.toast)
	}
	seq := m.loadSeq
	m, _ = send(t, m, tickMsg{version: m.version})
	if m.loadSeq != seq+1 {
		t.Fatal("the tick didn't retry")
	}
}

func TestOlderLoadIsIgnored(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	ask(t, s, "alpha", store.KindBlocker, time.Hour, clk.t)
	older := m.reload()
	olderMsg := older()
	ask(t, s, "bravo", store.KindBlocker, time.Hour, clk.t)
	newer := m.reload()
	m, _ = send(t, m, newer())
	m, _ = send(t, m, olderMsg)
	if n := len(m.list()); n != 2 {
		t.Fatalf("an older load replaced the list: %d items", n)
	}
}

// d, x and u say what they did and can be undone once.
func TestDoneDismissWakeUndo(t *testing.T) {
	for _, c := range []struct{ key, said, state string }{
		{"d", "done: “Question for bravo” · u undo", store.StateAnswered},
		{"x", "dismissed: “Question for bravo” · u undo", store.StateDismissed},
		{"u", "woke: “Question for bravo” · u undo", store.StateOpen},
	} {
		m, s, clk, b := selectB(t)
		before := store.StateOpen
		if c.key == "u" {
			if err := s.Snooze(b.ID, clk.t.Add(time.Hour), ""); err != nil {
				t.Fatal(err)
			}
			m = load(t, m)
			before = store.StateSnoozed
		}
		m, cmd := press(t, m, c.key)
		if cmd == nil {
			t.Fatalf("%s: no command", c.key)
		}
		m, cmd = send(t, m, cmd())
		if m.toast != c.said {
			t.Errorf("%s: toast %q", c.key, m.toast)
		}
		if it, _, _ := s.Item(b.ID); it.State != c.state {
			t.Errorf("%s: state %s", c.key, it.State)
		}
		m, _ = send(t, m, cmd())
		m, cmd = press(t, m, "U")
		if cmd == nil {
			t.Fatalf("%s: U did nothing", c.key)
		}
		m, _ = send(t, m, cmd())
		if it, _, _ := s.Item(b.ID); it.State != before {
			t.Errorf("%s: undo left %s, want %s", c.key, it.State, before)
		}
		if m.toast != "undone: “Question for bravo”" {
			t.Errorf("%s: undo toast %q", c.key, m.toast)
		}
		if _, cmd := press(t, m, "U"); cmd != nil {
			t.Errorf("%s: undid twice", c.key)
		}
	}
}

// The undo is only offered while its toast shows.
func TestUndoGoesWithItsToast(t *testing.T) {
	m, _, _, _ := selectB(t)
	m, cmd := press(t, m, "d")
	m, _ = send(t, m, cmd())
	m, _ = press(t, m, "k")
	if _, cmd := press(t, m, "U"); cmd != nil {
		t.Fatal("undid after another key")
	}
}

func TestUndoRefusesAChangedItem(t *testing.T) {
	m, s, _, b := selectB(t)
	m, cmd := press(t, m, "x")
	m, cmd = send(t, m, cmd())
	m, _ = send(t, m, cmd())
	if _, err := s.Restore(b, store.StateDismissed); err != nil {
		t.Fatal(err)
	}
	if err := s.Answer(b.ID, "elsewhere"); err != nil {
		t.Fatal(err)
	}
	_, cmd = press(t, m, "U")
	msg := cmd().(wroteMsg)
	if !errors.Is(msg.err, errNoUndo) {
		t.Fatalf("err %v", msg.err)
	}
	if it, _, _ := s.Item(b.ID); it.State != store.StateAnswered {
		t.Fatalf("undo overwrote a newer change: %s", it.State)
	}
}

// enter right after "enter jumps there" jumps to the question that toast
// was about, whatever is selected now.
func TestEnterJumpsToTheToastsQuestion(t *testing.T) {
	m, s, clk, b := selectB(t)
	c := ask(t, s, "charlie", store.KindLeftover, 0, clk.t)
	m, _ = send(t, m, doneMsg{id: c.ID, toast: "not sent to charlie: busy; enter jumps there", jump: &c})
	m = load(t, m)
	next, cmd := press(t, m, "enter")
	if cmd == nil || next.busy == nil || next.busy.id != c.ID {
		t.Fatalf("jumped to %+v", next.busy)
	}
	// After another key, enter is back to the selection.
	m, _ = press(t, m, " ")
	next, _ = press(t, m, "enter")
	if next.busy == nil || next.busy.id != b.ID {
		t.Fatalf("jumped to %+v", next.busy)
	}
}

func TestQQuitsOnlyOutsideRex(t *testing.T) {
	m, _, _, f := panelOn(t)
	m.d.InRex = false
	_, cmd := press(t, m, "q")
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("q didn't quit outside Rex")
	}
	m.d.InRex = true
	t.Setenv("REX_SESSION", "session:x") // HideSelf names its own block
	t.Setenv("REX_BLOCK", "block:x")
	f.fail[`api call session.detach_block {"session_id":"session:x","block_id":"block:x"}`] = errors.New("no server")
	_, cmd = press(t, m, "q")
	if msg, ok := cmd().(toastMsg); !ok || !strings.Contains(string(msg), "couldn't hide the panel: ") {
		t.Fatalf("got %v", msg)
	}
}

func TestHintsOnlyOfferWhatWorks(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	if h := m.hints(); h != "a all sessions · q hide" {
		t.Errorf("empty, in Rex: %q", h)
	}
	m.d.InRex = false
	if h := m.hints(); h != "a all sessions · q quit" {
		t.Errorf("empty, Rex block unknown: %q", h)
	}
	it := ask(t, s, "alpha", store.KindAction, 0, clk.t)
	m = load(t, m)
	if h := m.hints(); !strings.Contains(h, "1-2 answer") || strings.Contains(h, "j/k") {
		t.Errorf("two replies, one item: %q", h)
	}
	it.ID, it.SourceMsg, it.Replies = 0, "m2", nil
	if _, _, err := s.Insert(it); err != nil {
		t.Fatal(err)
	}
	m = load(t, m)
	m, _ = press(t, m, "j")
	if h := m.hints(); strings.Contains(h, "answer") || !strings.Contains(h, "j/k move") {
		t.Errorf("no replies: %q", h)
	}
	m.d.RexSession = ""
	if h := m.hints(); strings.Contains(h, "a all") {
		t.Errorf("outside Rex: %q", h)
	}
}

func TestAgoSaysNow(t *testing.T) {
	now := time.Now()
	if got := ago(now, now.Add(-20*time.Second)); got != "now" {
		t.Fatalf("got %q", got)
	}
}

// When the context pushes the answers out, one line says so.
func TestFoldedAnswersLeaveALine(t *testing.T) {
	m, _, _, b := selectB(t)
	b.Body = strings.Repeat("a long explanation ", 40)
	m.expanded = true
	if out := m.detail(b, 60, 20); !strings.Contains(out, "1-2 answers hidden · space folds") {
		t.Fatalf("got %q", out)
	}
}

// runInit runs Init's commands as Bubble Tea would and returns the first
// load's message. The tick also runs, and its message is dropped.
func runInit(t *testing.T, m model) tea.Msg {
	t.Helper()
	batch, ok := m.Init()().(tea.BatchMsg)
	if !ok {
		t.Fatal("Init returned no batch")
	}
	msgs := make(chan tea.Msg, len(batch))
	for _, cmd := range batch {
		go func() { msgs <- cmd() }()
	}
	for range batch {
		if msg := <-msgs; func() bool { _, ok := msg.(loadedMsg); return ok }() {
			return msg
		}
	}
	t.Fatal("Init loaded nothing")
	return nil
}

// The first load counts: what follows it isn't dropped as older.
func TestFirstLoadIsCounted(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	ask(t, s, "alpha", store.KindBlocker, time.Hour, clk.t)
	other := ask(t, s, "bravo", store.KindBlocker, time.Hour, clk.t)
	if err := s.Dismiss(other.ID); err != nil {
		t.Fatal(err)
	}
	it := other
	it.ID, it.SourceMsg, it.RexSession = 0, "m2", "session:other"
	if _, _, err := s.Insert(it); err != nil {
		t.Fatal(err)
	}
	m, _ = send(t, m, runInit(t, m))
	if n := len(m.list()); n != 1 {
		t.Fatalf("first load: %d items", n)
	}
	m, cmd := press(t, m, "a")
	m, _ = send(t, m, cmd())
	if n := len(m.list()); n != 2 {
		t.Fatalf("a after opening: %d items, want 2", n)
	}
}

func TestFirstActionAfterOpeningShows(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	ask(t, s, "alpha", store.KindBlocker, time.Hour, clk.t)
	m, _ = send(t, m, runInit(t, m))
	m, cmd := press(t, m, "x")
	m, cmd = send(t, m, cmd())
	m, _ = send(t, m, cmd())
	if n := len(m.list()); n != 0 {
		t.Fatalf("x as the first action: still %d listed", n)
	}
}

// Fast triage: the panel's own changes don't hold the keys that follow.
func TestFastTriage(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	for _, p := range []string{"alpha", "bravo", "charlie", "delta"} {
		ask(t, s, p, store.KindDecision, time.Hour, clk.t)
	}
	m = load(t, m)
	for i := range 3 {
		var cmd tea.Cmd
		m, cmd = press(t, m, "d")
		if cmd == nil {
			t.Fatalf("d #%d did nothing", i+1)
		}
		m, cmd = send(t, m, cmd())
		m, _ = send(t, m, cmd())
		if !strings.HasPrefix(m.toast, "done: ") {
			t.Fatalf("d #%d: toast %q", i+1, m.toast)
		}
	}
	if n := len(m.list()); n != 1 {
		t.Fatalf("%d left, want 1", n)
	}
	// The undo of the last one is still offered.
	if _, cmd := press(t, m, "U"); cmd == nil {
		t.Fatal("undo lost")
	}
}

// A key ignored while the selection settles leaves the toast alone.
func TestHeldKeyKeepsTheToast(t *testing.T) {
	m, s, _, b := selectB(t)
	if err := s.Dismiss(b.ID); err != nil {
		t.Fatal(err)
	}
	m = load(t, m)
	m, _ = press(t, m, "1")
	if m.toast != "that question was resolved" {
		t.Fatalf("toast %q", m.toast)
	}
}

// A reply kept from a question that went away comes back, said so, when r
// starts another.
func TestKeptReplyIsRestoredVisibly(t *testing.T) {
	m, s, _, b := selectB(t)
	m, _ = press(t, m, "r", "o", "k")
	if err := s.Answer(b.ID, "elsewhere"); err != nil {
		t.Fatal(err)
	}
	m = load(t, m)
	m, _ = press(t, m, "enter")
	m.frozen = time.Time{}
	m, _ = press(t, m, "r")
	if m.mode != replying || m.input != "ok" || !strings.Contains(m.footer(), "kept reply restored") {
		t.Fatalf("mode %v input %q footer %q", m.mode, m.input, m.footer())
	}
	m, _ = press(t, m, "esc", "r")
	if m.input != "" || strings.Contains(m.footer(), "kept reply") {
		t.Fatalf("esc didn't drop it: %q", m.input)
	}
}

func TestWhenLabel(t *testing.T) {
	now := time.Now()
	if got := when(now, now.Add(-20*time.Second)); got != "just now" {
		t.Errorf("got %q", got)
	}
	if got := when(now, now.Add(-12*time.Minute)); got != "12m ago" {
		t.Errorf("got %q", got)
	}
}

// The panel's own action on a question only spares the hold while it lands.
// Refused, it changed nothing, so a later removal from elsewhere still holds
// the keys.
func TestHoldAfterARefusedAnswer(t *testing.T) {
	m, s, _, b := selectB(t)
	m, cmd := press(t, m, "1")
	m, reload := send(t, m, cmd()) // refused: the directory is gone
	m, _ = send(t, m, reload())
	if m.busy != nil || selectedID(m) != b.ID {
		t.Fatalf("busy %+v selected %d", m.busy, selectedID(m))
	}
	if err := s.Dismiss(b.ID); err != nil { // from another panel
		t.Fatal(err)
	}
	m = load(t, m)
	if m.toast != "that question was resolved" {
		t.Fatalf("toast %q", m.toast)
	}
	if _, cmd := press(t, m, "1"); cmd != nil {
		t.Fatal("1 acted right after the selection slid")
	}
}

// After d or x the item isn't listed, so u can only mean undo.
func TestLowercaseUUndoes(t *testing.T) {
	for _, k := range []string{"d", "x"} {
		m, s, _, b := selectB(t)
		m, cmd := press(t, m, k)
		m, cmd = send(t, m, cmd())
		m, _ = send(t, m, cmd())
		m, cmd = press(t, m, "u")
		if cmd == nil {
			t.Fatalf("%s: u did nothing", k)
		}
		m, _ = send(t, m, cmd())
		if it, _, _ := s.Item(b.ID); it.State != store.StateOpen || m.toast != "undone: “Question for bravo”" {
			t.Fatalf("%s: state %s toast %q", k, it.State, m.toast)
		}
	}
}

// The key hint sits at a fixed place on the question line, so it doesn't
// move as the reply grows (it used to trail the cursor on the input line).
func TestReplyHintStaysPutWhileTyping(t *testing.T) {
	m, _, _, _ := selectB(t)
	m, _ = press(t, m, "r")
	hintAt := func(m model) (line, col int) {
		for i, l := range strings.Split(ansi.Strip(m.footer()), "\n") {
			if c := strings.Index(l, "enter send · esc cancel"); c >= 0 {
				return i, ansi.StringWidth(l[:c])
			}
		}
		t.Fatalf("no hint in %q", ansi.Strip(m.footer()))
		return
	}
	l0, c0 := hintAt(m)
	m, _ = press(t, m, strings.Split("I don't remember what this is", "")...)
	l1, c1 := hintAt(m)
	if l0 != l1 || c0 != c1 {
		t.Fatalf("hint moved from line %d col %d to line %d col %d", l0, c0, l1, c1)
	}
	if !strings.Contains(ansi.Strip(m.footer()), "❯ I don't remember what this is") {
		t.Fatalf("reply not shown: %q", ansi.Strip(m.footer()))
	}
}

// A logged-out claude or a usage limit must not look like an empty inbox:
// the header warns while extraction fails or turns wait for it.
func TestHeaderWarnsWhileExtractionFails(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	m = load(t, m)
	if out := ansi.Strip(m.View()); strings.Contains(out, "⚠") {
		t.Fatalf("healthy panel warns:\n%s", out)
	}
	if err := s.RecordExtractionError("claude -p: Not logged in · Please run /login\nmore", clk.t); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"c1", "c2", "c3"} {
		if err := s.Enqueue(c, []byte("{}"), clk.t); err != nil {
			t.Fatal(err)
		}
	}
	m = load(t, m)
	out := ansi.Strip(m.View())
	if !strings.Contains(out, "⚠ extraction failing: claude -p: Not logged in · Please run /login more · 3 turns waiting · see hook.log") {
		t.Fatalf("no warning:\n%s", out)
	}
	if !strings.Contains(out, "Nothing waiting on you") {
		t.Errorf("empty body gone:\n%s", out)
	}
	// Recovered, but turns still wait for their retry.
	if err := s.RecordExtractionOK(clk.t.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	m = load(t, m)
	if out := ansi.Strip(m.View()); !strings.Contains(out, "⚠ 3 turns waiting for extraction · see hook.log") || strings.Contains(out, "failing") {
		t.Fatalf("pending only:\n%s", out)
	}
	// The line fits a narrow panel.
	m.w = 40
	for _, l := range strings.Split(m.View(), "\n") {
		if w := ansi.StringWidth(l); w > 40 {
			t.Errorf("line %d cells wide: %q", w, ansi.Strip(l))
		}
	}
}

// A question whose agent you've written to since is marked in the list and
// explained in the detail, before you try to answer it.
func TestWrittenSinceIsMarked(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	a := ask(t, s, "alpha", store.KindBlocker, time.Hour, clk.t)
	ask(t, s, "bravo", store.KindDecision, time.Hour, clk.t)
	if err := s.RecordPrompt(a.ClaudeSession, "never mind", clk.t.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	m = load(t, m)
	out := ansi.Strip(m.View())
	if strings.Count(out, "·replied") != 1 {
		t.Errorf("want alpha alone marked:\n%s", out)
	}
	// Set aside below bravo, which is still waiting and selected first.
	if strings.Contains(out, "you've written to this agent since") {
		t.Errorf("bravo explained as replied:\n%s", out)
	}
	m, _ = press(t, m, "j") // alpha
	if out := ansi.Strip(m.View()); !strings.Contains(out, "you've written to this agent since this was asked; d marks it done") {
		t.Errorf("detail doesn't explain:\n%s", out)
	}
}

// A prompt in the same second as the question may have come after it, so
// the panel refuses to answer.
func TestAnswerRefusesAPromptInTheSameSecond(t *testing.T) {
	m, s, clk, f := panelOn(t)
	it := ask(t, s, "alpha", store.KindBlocker, 0, clk.t.Truncate(time.Second))
	if err := s.RecordPrompt(it.ClaudeSession, "hm", it.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if got := toastOf(t, m.answer(it, "yes")); got != "not sent to alpha: you've written to that agent since; enter jumps there" {
		t.Fatalf("toast %q", got)
	}
	if f.called("run") {
		t.Fatal("resumed")
	}
}

// Space types a space while replying, so r shows the context itself: the
// agent's words stay in view while you write. Sending or cancelling puts
// the fold back as it was.
func TestReplyShowsTheContext(t *testing.T) {
	m, _, _, _ := selectB(t)
	m, _ = press(t, m, "r")
	if !m.expanded {
		t.Fatal("r didn't unfold the context")
	}
	m, _ = press(t, m, "esc")
	if m.expanded {
		t.Fatal("cancel didn't fold it back")
	}
	// Already unfolded: it stays so.
	m, _ = press(t, m, " ", "r", "esc")
	if !m.expanded {
		t.Fatal("cancel folded a context the user had open")
	}
	m, _ = press(t, m, " ", "r", "o", "k", "enter")
	if m.expanded || m.mode != normal {
		t.Fatalf("send left expanded=%v mode=%v", m.expanded, m.mode)
	}
}

// At 80x24 the reply footer and the unfolded question still fit, and the
// agent's words show.
func TestReplyFitsASmallPanel(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	if _, _, err := s.Insert(store.Item{RexSession: "session:panel", ClaudeSession: "c-alpha", SourceMsg: "m",
		Cwd: "/nonexistent/alpha", Project: "alpha", Kind: store.KindDecision, Title: "Question for alpha",
		Body: "Want me to switch the cache to LRU?", Prompt: "speed up the build",
		Replies: []string{"Switch", "Keep it", "Measure first"}, CreatedAt: clk.t.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	m.w, m.h = 80, 24
	m = load(t, m)
	m, _ = press(t, m, "r")
	m, _ = press(t, m, strings.Split("let me think about it", "")...)
	out := m.View()
	lines := strings.Split(out, "\n")
	if len(lines) > 24 {
		t.Errorf("%d lines", len(lines))
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 80 {
			t.Errorf("line %d cells wide: %q", w, ansi.Strip(l))
		}
	}
	plain := ansi.Strip(out)
	for _, want := range []string{"❯ let me think about it", "enter send · esc cancel", "Question for alpha",
		"THE AGENT WROTE", "Want me to switch the cache to LRU?"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "space folds") {
		t.Errorf("offers space, which types a space while replying:\n%s", plain)
	}
}

// In a narrow panel the error is shortened, not the count or the hint.
func TestWarningKeepsItsHintWhenNarrow(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	if err := s.RecordExtractionError("claude -p: Not logged in · Please run /login", clk.t); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("c1", []byte("{}"), clk.t); err != nil {
		t.Fatal(err)
	}
	m.w, m.h = 80, 24
	m = load(t, m)
	line := strings.Split(ansi.Strip(m.header()), "\n")[1]
	if !strings.HasSuffix(strings.TrimRight(line, " "), "· 1 turn waiting · see hook.log") ||
		!strings.Contains(line, "⚠ extraction failing: claude -p: Not") || ansi.StringWidth(line) > 80 {
		t.Fatalf("got %q", line)
	}
}

// A reply recorded after the list loaded moves its question aside at the
// next load; the selection, and the keys, follow it there.
func TestRepliedQuestionMovesAsideAndStaysSelected(t *testing.T) {
	m, s, clk, b := selectB(t)
	if err := s.RecordPrompt(b.ClaudeSession, "let's do something else", clk.t); err != nil {
		t.Fatal(err)
	}
	if len(m.g.replied) != 0 {
		t.Fatal("moved before a load")
	}
	m = load(t, m)
	if ids := len(m.g.replied); ids != 1 || m.g.replied[0].ID != b.ID {
		t.Fatalf("replied section %+v", m.g.replied)
	}
	if selectedID(m) != b.ID || m.toast != "" {
		t.Fatalf("selection %d toast %q", selectedID(m), m.toast)
	}
	m, cmd := press(t, m, "d")
	m, cmd = send(t, m, cmd())
	m, _ = send(t, m, cmd())
	if it, _, _ := s.Item(b.ID); it.State != store.StateAnswered {
		t.Fatalf("d left %s", it.State)
	}
	m, cmd = press(t, m, "u")
	m, cmd = send(t, m, cmd())
	m, _ = send(t, m, cmd())
	if len(m.g.replied) != 1 || m.g.replied[0].ID != b.ID {
		t.Fatalf("undo didn't bring it back aside: %+v", m.g.replied)
	}
}
