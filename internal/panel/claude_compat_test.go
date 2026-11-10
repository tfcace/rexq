package panel

// Characterization tests: they pin how the panel shows and acts on a
// Claude item. They use only APIs that existed before Pi support, so they
// also run against the commit before it.

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/tfcace/rexq/internal/store"
)

// The SESSION section of a Claude item names it "claude <id>".
func TestClaudeCompatDetailNamesTheConversation(t *testing.T) {
	m, s, clk, _ := panelOn(t)
	it := store.Item{RexSession: "session:panel", ClaudeSession: "0123456789abcdef", SourceMsg: "m",
		Cwd: "/nonexistent/proj", Branch: "main", Project: "proj", Kind: store.KindDecision,
		Title: "Ship it?", Replies: []string{"Ship", "Wait"}, CreatedAt: clk.t.Add(-time.Hour)}
	if _, _, err := s.Insert(it); err != nil {
		t.Fatal(err)
	}
	m = load(t, m)
	m.expanded = true
	sel, _ := m.selected()
	detail := ansi.Strip(m.detail(sel, 100, 60))
	var session []string
	in := false
	for _, line := range strings.Split(detail, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "SESSION":
			in = true
		case in && line != "":
			session = append(session, line)
		}
	}
	want := []string{"main  ·  /nonexistent/proj", "claude 0123456789abcdef", "rex session:panel"}
	if strings.Join(session, "|") != strings.Join(want, "|") {
		t.Errorf("SESSION section\n got %q\nwant %q", session, want)
	}
}

// Outside Rex the panel says how to resume by hand, in claude's words.
func TestClaudeCompatHandResumeHints(t *testing.T) {
	f := goneBlock()
	m, s := testModel(t, f, "")
	it := waiting(t, s, t.TempDir(), true)
	if got := toastOf(t, m.answer(it, "yes")); got != "not in Rex: run claude --resume c1" {
		t.Errorf("answer: %q", got)
	}
	if got := toastOf(t, m.jump(it)); got != "run: claude --resume c1" {
		t.Errorf("jump: %q", got)
	}
}

// Answering a gone Claude resumes it with exactly this rex run.
func TestClaudeCompatResumeCommand(t *testing.T) {
	f := goneBlock()
	m, s := testModel(t, f, "session:panel")
	cwd := t.TempDir()
	it := waiting(t, s, cwd, true)
	if got := toastOf(t, m.answer(it, "yes")); got != "sent to proj: yes" {
		t.Fatalf("toast %q", got)
	}
	want := "run -s session:panel -c " + cwd + " --keep-open --label claude c1 -- claude --resume c1 -- yes"
	found := false
	for _, c := range f.calls {
		if strings.HasPrefix(c, "run ") {
			found = c == want
		}
	}
	if !found {
		t.Errorf("calls %q\nwant %q", f.calls, want)
	}
}

// A live Claude that can't take a reply: the panel's words.
func TestClaudeCompatAnswerWording(t *testing.T) {
	f := idleClaude(t)
	f.reply[captureCall] = strings.Repeat("─", 40) + "\n❯ half typed\n" + strings.Repeat("─", 40) + "\n"
	m, s := testModel(t, f, "session:a")
	it := waiting(t, s, t.TempDir(), true)
	if got := toastOf(t, m.answer(it, "yes")); got != `not sent to proj: you have a half-typed message there: "half typed"; enter jumps there` {
		t.Errorf("draft: %q", got)
	}

	f = idleClaude(t)
	f.reply[statusCall] = `{"records":[{"state":"working","app":"claude-code"}]}`
	m, s = testModel(t, f, "session:a")
	it = waiting(t, s, t.TempDir(), true)
	if got := toastOf(t, m.answer(it, "yes")); got != "not sent to proj: the agent is busy; enter jumps there" {
		t.Errorf("busy: %q", got)
	}
}
