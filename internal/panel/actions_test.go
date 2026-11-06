package panel

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tfcace/rexq/internal/hook"
	"github.com/tfcace/rexq/internal/rexctl"
	"github.com/tfcace/rexq/internal/store"
)

// fakeRex stands in for the rex CLI. reply maps the joined args to the
// output and fail to an error; anything else answers "" and no error.
type fakeRex struct {
	mu    sync.Mutex
	calls []string
	reply map[string]string
	fail  map[string]error

	claudeIn string // a Claude the hooks never saw runs here
}

func (f *fakeRex) run(_ context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)
	return []byte(f.reply[key]), f.fail[key]
}

// called reports whether any call starts with prefix.
func (f *fakeRex) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

type exitErr int

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

const (
	inspectCall = "block inspect block:1 --json"
	statusCall  = "block call -b block:1 program_status"
	processCall = "block call -b block:1 process"
	captureCall = "capture -b block:1 --format=vt --trim"
)

// idleClaude is block:1 running an idle Claude with an empty input box, on
// this machine: rexctl would type there.
func idleClaude(t *testing.T) *fakeRex {
	box, err := os.ReadFile("../rexctl/testdata/box-empty.txt")
	if err != nil {
		t.Fatal(err)
	}
	return &fakeRex{reply: map[string]string{
		inspectCall: `{"block":{"block_id":"block:1","host_id":"host-local"}}`,
		statusCall:  `{"records":[{"state":"idle","app":"claude-code"}]}`,
		processCall: `{"child":{"pid":3356,"name":"login","path":"/usr/bin/login"}}`,
		captureCall: string(box),
	}, fail: map[string]error{}}
}

// goneBlock is a block:1 that no longer exists.
func goneBlock() *fakeRex {
	return &fakeRex{fail: map[string]error{inspectCall: exitErr(3)}}
}

func testModel(t *testing.T, f *fakeRex, rexSession string) (model, *store.Store) {
	s, err := store.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r := rexctl.New(f.run)
	r.Procs = func(context.Context) ([]byte, error) { return os.ReadFile("../rexctl/testdata/ps-claude.txt") }
	r.LocalHost = func(context.Context) (string, error) { return "host-local", nil }
	r.ProcCwds = func(_ context.Context, pids []int) ([]byte, error) { return lsofOut(pids, f.claudeIn), nil }
	return newModel(Deps{Store: s, Rex: r, RexSession: rexSession, InRex: rexSession != "", Now: time.Now}), s
}

// lsofOut answers as `lsof -a -d cwd -Fn -p` does: every Claude is in
// /elsewhere except pid 3487, which is in claudeIn when that's set.
func lsofOut(pids []int, claudeIn string) []byte {
	var b strings.Builder
	for _, pid := range pids {
		cwd := "/elsewhere"
		if pid == 3487 && claudeIn != "" {
			cwd = claudeIn
		}
		b.WriteString("p" + strconv.Itoa(pid) + "\nfcwd\nn" + cwd + "\n")
	}
	return []byte(b.String())
}

// waiting stores an open item from Claude session c1, in block:1 when
// claimed says c1 claimed the pane, and returns it as the panel lists it.
func waiting(t *testing.T, s *store.Store, cwd string, claimed bool) store.Item {
	t.Helper()
	if claimed {
		if err := s.ClaimPane("block:1", "session:a", "c1", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	it := store.Item{RexSession: "session:a", RexBlock: "block:1", ClaudeSession: "c1", SourceMsg: "m", Cwd: cwd,
		Project: "proj", Kind: store.KindDecision, Title: "Which?", Replies: []string{"yes", "no"}}
	if _, _, err := s.Insert(it); err != nil {
		t.Fatal(err)
	}
	items, err := s.List("")
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %v %v", items, err)
	}
	return items[0]
}

// toastOf runs an answer or jump command and returns its toast.
func toastOf(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	return doneOf(t, cmd).toast
}

func doneOf(t *testing.T, cmd tea.Cmd) doneMsg {
	t.Helper()
	msg, ok := cmd().(doneMsg)
	if !ok {
		t.Fatalf("got %T, want doneMsg", msg)
	}
	return msg
}

func stillOpen(t *testing.T, s *store.Store) bool {
	t.Helper()
	items, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	return len(items) == 1 && items[0].State == store.StateOpen
}

func TestAnswerRefusesAPaneAnotherConversationOwns(t *testing.T) {
	for owner, want := range map[string]string{
		"c2": "not sent to proj: that pane now runs a different conversation",
		"":   "not sent to proj: that pane isn't linked to this conversation",
	} {
		f := idleClaude(t)
		m, s := testModel(t, f, "session:a")
		if owner != "" {
			if err := s.ClaimPane("block:1", "session:a", owner, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		it := waiting(t, s, t.TempDir(), false) // stored in block:1 all the same
		if got := toastOf(t, m.answer(it, "yes")); got != want {
			t.Fatalf("owner %q: toast %q", owner, got)
		}
		if f.called("send") || f.called("run") {
			t.Fatalf("delivered anyway: %q", f.calls)
		}
		if !stillOpen(t, s) {
			t.Fatal("marked answered")
		}
	}
}

// Before delivering, the item is read again: answered elsewhere, or moved
// on because the user wrote to the agent, it's not sent.
func TestAnswerRereadsTheItem(t *testing.T) {
	f := goneBlock()
	m, s := testModel(t, f, "session:panel")
	it := waiting(t, s, t.TempDir(), true)
	if err := s.Answer(it.ID, "from the pane"); err != nil {
		t.Fatal(err)
	}
	if got := toastOf(t, m.answer(it, "yes")); got != "not sent to proj: that question was already resolved" {
		t.Fatalf("resolved: toast %q", got)
	}

	f = goneBlock()
	m, s = testModel(t, f, "session:panel")
	it = waiting(t, s, t.TempDir(), true)
	if err := s.RecordPrompt("c1", "never mind, do X", it.CreatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	done := doneOf(t, m.answer(it, "yes"))
	if done.toast != "not sent to proj: you've written to that agent since; enter jumps there" || done.jump == nil || done.jump.ID != it.ID {
		t.Fatalf("written since: %+v", done)
	}
	if f.called("run") || !stillOpen(t, s) {
		t.Fatalf("delivered or answered: %q", f.calls)
	}

	// A prompt from before the question doesn't count.
	if err := s.RecordPrompt("c1", "earlier", it.CreatedAt.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := toastOf(t, m.answer(it, "yes")); got != "sent to proj: yes" {
		t.Fatalf("earlier prompt: toast %q", got)
	}
}

func TestAnswerShowsWhyItRefused(t *testing.T) {
	f := idleClaude(t)
	f.reply[statusCall] = `{"records":[{"state":"busy","app":"claude-code"}]}`
	m, s := testModel(t, f, "session:a")
	it := waiting(t, s, t.TempDir(), true)
	got := toastOf(t, m.answer(it, "yes"))
	if got != "not sent to proj: the agent is busy; enter jumps there" {
		t.Fatalf("toast %q", got)
	}
	if f.called("send") || !stillOpen(t, s) {
		t.Fatalf("delivered or answered: %q", f.calls)
	}
}

func TestAnswerShowsTypedButNotSent(t *testing.T) {
	f := idleClaude(t) // the box never shows the reply, so Send won't press Enter
	m, s := testModel(t, f, "session:a")
	it := waiting(t, s, t.TempDir(), true)
	got := toastOf(t, m.answer(it, "yes"))
	if !strings.HasPrefix(got, "proj: typed but not sent: ") || !strings.HasSuffix(got, "; enter jumps there") {
		t.Fatalf("toast %q", got)
	}
	if f.called("send-key") || !stillOpen(t, s) {
		t.Fatalf("sent or answered: %q", f.calls)
	}
}

func TestAnswerResumesAGoneAgent(t *testing.T) {
	f := goneBlock()
	m, s := testModel(t, f, "session:panel")
	cwd := t.TempDir()
	it := waiting(t, s, cwd, true)
	got := toastOf(t, m.answer(it, "yes"))
	if got != "sent to proj: yes" {
		t.Fatalf("toast %q", got)
	}
	if !f.called("run -s session:panel -c " + cwd) {
		t.Fatalf("no resume in the panel's session: %q", f.calls)
	}
	if stillOpen(t, s) {
		t.Fatal("not marked answered")
	}
}

func TestAnswerFailedResumeLeavesItOpen(t *testing.T) {
	f := goneBlock()
	m, s := testModel(t, f, "session:panel")
	it := waiting(t, s, "/nonexistent/proj", true)
	got := toastOf(t, m.answer(it, "yes"))
	if got != "couldn't resume proj: the project directory is gone" {
		t.Fatalf("toast %q", got)
	}
	if f.called("run") || !stillOpen(t, s) {
		t.Fatalf("ran or answered: %q", f.calls)
	}

	m.d.RexSession = ""
	if got := toastOf(t, m.answer(it, "yes")); got != "not in Rex: run claude --resume c1" {
		t.Fatalf("outside Rex: toast %q", got)
	}
}

func TestJumpFocusesALiveAgent(t *testing.T) {
	f := idleClaude(t)
	m, s := testModel(t, f, "session:a")
	it := waiting(t, s, t.TempDir(), true)
	toastOf(t, m.jump(it))
	if !f.called("focus -s session:a block:1") || f.called("run") {
		t.Fatalf("calls %q", f.calls)
	}
}

func TestJumpResumesAGoneAgent(t *testing.T) {
	f := goneBlock()
	m, s := testModel(t, f, "session:a")
	cwd := t.TempDir()
	it := waiting(t, s, cwd, true)
	toastOf(t, m.jump(it))
	if f.called("focus") || !f.called("run -s session:a -c "+cwd) {
		t.Fatalf("calls %q", f.calls)
	}
}

// A live agent is never resumed a second time, even when focusing fails.
func TestJumpNeverResumesALiveAgent(t *testing.T) {
	f := idleClaude(t)
	f.fail["focus -s session:a block:1"] = exitErr(1)
	m, s := testModel(t, f, "session:a")
	it := waiting(t, s, t.TempDir(), true)
	if got := toastOf(t, m.jump(it)); !strings.HasPrefix(got, "couldn't jump to proj: ") {
		t.Fatalf("toast %q", got)
	}
	f.fail[statusCall] = exitErr(4) // and when Rex can't say
	if got := toastOf(t, m.jump(it)); !strings.HasPrefix(got, "couldn't reach Rex: ") {
		t.Fatalf("toast %q", got)
	}
	if f.called("run") {
		t.Fatalf("resumed: %q", f.calls)
	}
}

// An item from a Claude the hooks never saw has no pane, but that Claude
// may still run: answering must not start a second one.
func TestAnswerNeverResumesNextToARunningClaude(t *testing.T) {
	cwd := t.TempDir()
	f := &fakeRex{claudeIn: cwd}
	m, s := testModel(t, f, "session:panel")
	if _, _, err := s.Insert(store.Item{ClaudeSession: "c1", SourceMsg: "m", Cwd: cwd, Project: "proj",
		Kind: store.KindDecision, Title: "Which?", Replies: []string{"yes"}}); err != nil {
		t.Fatal(err)
	}
	items, _ := s.List("")
	it := items[0]
	want := "couldn't resume proj: a Claude is already running in that project; jump there or answer in it"
	if got := toastOf(t, m.answer(it, "yes")); got != want {
		t.Fatalf("answer: toast %q", got)
	}
	if got := toastOf(t, m.jump(it)); got != want {
		t.Fatalf("jump: toast %q", got)
	}
	if f.called("run") || !stillOpen(t, s) {
		t.Fatalf("resumed or answered: %q", f.calls)
	}
}

// promptingRunner is an extraction during which the user writes to the
// agent: the prompt hook runs while the model is still working.
type promptingRunner struct {
	d   hook.Deps
	clk *clock
	mu  *sync.Mutex
}

func (r promptingRunner) Run(context.Context, string, string) ([]byte, error) {
	r.mu.Lock()
	r.clk.t = r.clk.t.Add(5 * time.Second)
	r.mu.Unlock()
	if err := hook.Prompt(r.d, strings.NewReader(`{"session_id":"c1","prompt":"actually, do it the other way"}`)); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.clk.t = r.clk.t.Add(30 * time.Second)
	r.mu.Unlock()
	return []byte(`{"items":[{"kind":"decision","title":"Commit this?","quote":"Want me to commit?","context":"",` +
		`"replies":["Commit","Not yet"],"suggested":0}],"resolved":[]}`), nil
}

// A reply typed while extraction still ran is written after the question,
// so the panel doesn't answer a conversation that moved on.
func TestAnswerRefusesWhenTheUserWroteDuringExtraction(t *testing.T) {
	f := goneBlock()
	m, s := testModel(t, f, "session:panel")
	clk, mu := &clock{time.Unix(10_000, 0)}, &sync.Mutex{}
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clk.t }
	d := hook.Deps{Store: s, Now: now, Env: func(string) string { return "" },
		Repo: func(string) (string, string) { return "proj", "main" }, Locks: hook.FileLock(t.TempDir())}
	d.Runner = promptingRunner{d, clk, mu}
	cwd := t.TempDir()
	in := `{"session_id":"c1","prompt_id":"p1","cwd":"` + cwd + `","last_assistant_message":"Want me to commit?"}`
	if err := hook.Stop(context.Background(), d, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	items, err := s.List("")
	if err != nil || len(items) != 1 {
		t.Fatalf("items %v %v", items, err)
	}
	done := doneOf(t, m.answer(items[0], "Commit"))
	if done.toast != "not sent to proj: you've written to that agent since; enter jumps there" {
		t.Fatalf("toast %q", done.toast)
	}
	if f.called("run") || !stillOpen(t, s) {
		t.Fatalf("delivered or answered: %q", f.calls)
	}
}
