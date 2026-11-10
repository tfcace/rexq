package panel

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/tfcace/rexq/internal/store"
)

// waitingPi stores an open item from Pi session pi-1, claimed in block:1.
func waitingPi(t *testing.T, s *store.Store, cwd string) store.Item {
	return waitingPiIn(t, s, cwd, "session:a")
}

func waitingPiIn(t *testing.T, s *store.Store, cwd, rexSession string) store.Item {
	t.Helper()
	if err := s.ClaimPane("block:1", rexSession, "pi-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	it := store.Item{RexSession: rexSession, RexBlock: "block:1", ClaudeSession: "pi-1", SourceMsg: "m", Cwd: cwd,
		Project: "proj", Kind: store.KindDecision, Title: "Which?", Replies: []string{"yes", "no"}, Agent: "pi"}
	if _, _, err := s.Insert(it); err != nil {
		t.Fatal(err)
	}
	return must(s.List(""))[0]
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestPiItemDetailNamesPi(t *testing.T) {
	m, s, _, _ := panelOn(t)
	waitingPiIn(t, s, "/nonexistent/proj", "session:panel")
	m = load(t, m)
	m.expanded = true
	sel, _ := m.selected()
	if d := ansi.Strip(m.detail(sel, 100, 60)); !strings.Contains(d, "pi pi-1") || strings.Contains(d, "claude pi-1") {
		t.Errorf("detail:\n%s", d)
	}
}

func TestPiAnswerResumesWithPi(t *testing.T) {
	f := goneBlock()
	m, s := testModel(t, f, "session:panel")
	cwd := t.TempDir()
	it := waitingPi(t, s, cwd)
	if got := toastOf(t, m.answer(it, "yes")); got != "sent to proj: yes" {
		t.Fatalf("toast %q", got)
	}
	want := "run -s session:panel -c " + cwd + " --keep-open --label pi pi-1 -- pi --session pi-1 -- yes"
	if !f.called(want) {
		t.Fatalf("calls %q", f.calls)
	}
	m.d.RexSession = ""
	if got := toastOf(t, m.jump(it)); got != "run: pi --session pi-1" {
		t.Errorf("jump outside Rex: %q", got)
	}
}

// idlePi is block:1 running an idle Pi with box screen on this machine.
func idlePi(t *testing.T, box string) *fakeRex {
	b, err := os.ReadFile("../inputbox/testdata/pi-box-" + box + ".vt")
	if err != nil {
		t.Fatal(err)
	}
	return &fakeRex{reply: map[string]string{
		inspectCall: `{"block":{"block_id":"block:1","host_id":"host-local"}}`,
		statusCall:  `{"records":[{"state":"idle","app":"pi"}]}`,
		processCall: `{"child":{"pid":3356,"name":"login","path":"/usr/bin/login"}}`,
		captureCall: string(b),
	}, fail: map[string]error{}}
}

func piModel(t *testing.T, f *fakeRex, session string) (model, *store.Store) {
	m, s := testModel(t, f, session)
	m.d.Rex.Procs = func(context.Context) ([]byte, error) { return os.ReadFile("../rexctl/testdata/ps-pi.txt") }
	return m, s
}

func TestPiAnswerRefusesAnOpenAutocomplete(t *testing.T) {
	f := idlePi(t, "at-complete")
	m, s := piModel(t, f, "session:a")
	it := waitingPi(t, s, t.TempDir())
	if got := toastOf(t, m.answer(it, "yes")); got != "not sent to proj: couldn't read the agent's input box; enter jumps there" {
		t.Errorf("toast %q", got)
	}
	if f.called("send") {
		t.Error("typed")
	}
}

func TestPiAnswerTypesIntoAnIdleBox(t *testing.T) {
	f := idlePi(t, "empty")
	m, s := piModel(t, f, "session:a")
	it := waitingPi(t, s, t.TempDir())
	// The box shows the reply once typed; this fake shows it at once, so
	// typing is refused as a draft, and the decision to type is what this
	// checks: no resume, no refusal for the box.
	got := toastOf(t, m.answer(it, "yes"))
	if !f.called("send -b block:1 -- yes") || f.called("run") {
		t.Errorf("toast %q calls %q", got, f.calls)
	}
}

func TestUnknownAgentIsRefused(t *testing.T) {
	f := goneBlock()
	m, s := testModel(t, f, "session:panel")
	it := store.Item{RexSession: "session:a", ClaudeSession: "x-1", SourceMsg: "m", Cwd: t.TempDir(),
		Project: "proj", Kind: store.KindDecision, Title: "Which?", Agent: "codex"}
	if _, _, err := s.Insert(it); err != nil {
		t.Fatal(err)
	}
	it = must(s.List(""))[0]
	want := "not sent to proj: it is from agent codex, which this rexq doesn't know; update rexq"
	if got := toastOf(t, m.answer(it, "yes")); got != want {
		t.Errorf("answer %q", got)
	}
	if got := toastOf(t, m.jump(it)); !strings.Contains(got, "update rexq") || f.called("run") {
		t.Errorf("jump %q calls %q", got, f.calls)
	}
}
