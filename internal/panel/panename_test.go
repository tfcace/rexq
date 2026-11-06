package panel

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tfcace/rexq/internal/store"
)

const titleCall = "block call -b block:1 title"

// paneOn is a panel with one question from an agent live in block:1, whose
// terminal title is title, loaded and selected.
func paneOn(t *testing.T, title string) (model, *fakeRex, *clock) {
	t.Helper()
	m, s, clk, f := panelOn(t)
	f.reply = map[string]string{titleCall: `{"title":"` + title + `","owner":{"pid":1}}`}
	it := store.Item{RexSession: "session:panel", RexBlock: "block:1", ClaudeSession: "c1", SourceMsg: "m",
		Cwd: "/nonexistent/proj", Project: "proj", Kind: store.KindDecision, Title: "Which?",
		Replies: []string{"yes"}, CreatedAt: clk.t.Add(-time.Hour)}
	if _, _, err := s.Insert(it); err != nil {
		t.Fatal(err)
	}
	return m, f, clk
}

// loaded delivers a load, and returns the pane lookup it asked for.
func loaded(t *testing.T, m model) (model, tea.Cmd) {
	t.Helper()
	cmd := m.reload()
	return send(t, m, cmd())
}

func sourceLine(m model) string {
	it, _ := m.selected()
	return strings.Split(m.detail(it, 76, 40), "\n")[1]
}

func TestSourceLineNamesTheLivePane(t *testing.T) {
	m, _, _ := paneOn(t, "Open questions tracker for Rex")
	m, lookup := loaded(t, m)
	if got := sourceLine(m); !strings.Contains(got, "● live in a pane") {
		t.Fatalf("before the name arrives: %q", got)
	}
	if lookup == nil {
		t.Fatal("no lookup")
	}
	m, _ = send(t, m, lookup())
	if got := sourceLine(m); !strings.Contains(got, "● live in “Open questions tracker for Rex”") {
		t.Fatalf("got %q", got)
	}
}

func TestSourceLineWithoutAName(t *testing.T) {
	m, f, _ := paneOn(t, "")
	f.fail[titleCall] = exitErr(4)
	m, lookup := loaded(t, m)
	m, _ = send(t, m, lookup())
	if got := sourceLine(m); !strings.Contains(got, "● live in a pane") {
		t.Fatalf("lookup failed: %q", got)
	}
}

func TestSourceLineWhenNotLive(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	ask(t, s, "proj", store.KindDecision, time.Hour, clk.t)
	m, lookup := loaded(t, m)
	if lookup != nil {
		t.Fatal("looked up a pane it doesn't have")
	}
	if got := sourceLine(m); !strings.Contains(got, "○ not open · replying resumes it") {
		t.Fatalf("got %q", got)
	}
}

// Names are cached per block: one lookup, then another only after 10 s.
func TestPaneNameIsCached(t *testing.T) {
	m, f, clk := paneOn(t, "Title")
	m, lookup := loaded(t, m)
	if _, again := send(t, m, tickMsg{version: m.version}); again != nil && hasLookup(again) {
		t.Fatal("a second lookup while the first runs")
	}
	m, _ = send(t, m, lookup())
	calls := len(f.calls)
	m, _ = press(t, m, " ")
	m, cmd := send(t, m, tickMsg{version: m.version})
	if hasLookup(cmd) || len(f.calls) != calls {
		t.Fatal("looked up again within 10 s")
	}
	clk.t = clk.t.Add(paneNameTTL)
	if _, cmd := send(t, m, tickMsg{version: m.version}); !hasLookup(cmd) {
		t.Fatal("no refresh after 10 s")
	}
}

// hasLookup reports whether cmd, or a command batched in it, is a pane
// name lookup. It runs only commands that return at once.
func hasLookup(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		switch msg := msg.(type) {
		case paneNameMsg:
			return true
		case tea.BatchMsg:
			for _, c := range msg {
				if hasLookup(c) {
					return true
				}
			}
		}
	case <-time.After(100 * time.Millisecond): // a tick
	}
	return false
}

// Drawing never asks Rex anything.
func TestViewNeverCallsRex(t *testing.T) {
	m, f, _ := paneOn(t, "Title")
	m, lookup := loaded(t, m)
	before := len(f.calls)
	for range 5 {
		m.View()
		m.expanded = !m.expanded
	}
	if len(f.calls) != before {
		t.Fatalf("View called rex: %q", f.calls[before:])
	}
	m, _ = send(t, m, lookup())
	before = len(f.calls)
	m.View()
	if len(f.calls) != before {
		t.Fatalf("View called rex: %q", f.calls[before:])
	}
}

// A long name is cut before the age is dropped, and the line fits.
func TestSourceLineFits(t *testing.T) {
	m, _, _ := paneOn(t, strings.Repeat("Long conversation title ", 3))
	m, lookup := loaded(t, m)
	m, _ = send(t, m, lookup())
	it, _ := m.selected()
	line := strings.Split(m.detail(it, 40, 40), "\n")[1]
	if w := len([]rune(line)); w > 36 {
		t.Fatalf("%d wide: %q", w, line)
	}
	if !strings.Contains(line, "● live in “") || !strings.Contains(line, "…”") {
		t.Fatalf("got %q", line)
	}
}

// Narrow, a pane that isn't open still says that replying resumes it.
func TestNotLiveLineFits(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	ask(t, s, "a-rather-long-project-name", store.KindDecision, time.Hour, clk.t)
	m = load(t, m)
	it, _ := m.selected()
	if got := m.sourceLine(it, 35); got != "○ not open · replying resumes it" {
		t.Fatalf("got %q", got)
	}
}
