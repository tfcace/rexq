package rexctl

// Characterization tests: they pin what rexctl says and runs for a Claude
// pane, word for word. They use only APIs that existed before Pi support,
// so they also run against the commit before it.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every reason a Claude reply is refused, as the panel shows it.
func TestClaudeCompatRefusalWording(t *testing.T) {
	empty, _ := os.ReadFile("testdata/box-empty.txt")
	draft, _ := os.ReadFile("testdata/box-draft.txt")
	status := func(s string) string { return `{"records":[` + s + `]}` }
	cases := []struct {
		name string
		rec  *recorder
		want Delivery
		why  string
	}{
		{"idle, empty box", &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: string(empty)}}, Type, ""},
		{"done, empty box", &recorder{reply: map[string]string{statusCall: status(`{"state":"done","app":"claude-code"}`), processCall: procLogin, captureCall: string(empty)}}, Type, ""},
		{"working", &recorder{reply: map[string]string{statusCall: status(`{"state":"working","app":"claude-code"}`), processCall: procLogin, captureCall: string(empty)}}, Refuse, "the agent is busy"},
		{"error state", &recorder{reply: map[string]string{statusCall: status(`{"state":"error","app":"claude-code"}`), processCall: procLogin, captureCall: string(empty)}}, Refuse, "the agent is busy"},
		{"blocked", &recorder{reply: map[string]string{statusCall: status(`{"state":"blocked","app":"claude-code"}`), processCall: procLogin, captureCall: string(empty)}}, Refuse, "the agent is waiting on a dialog"},
		{"blocked, kind", &recorder{reply: map[string]string{statusCall: status(`{"state":"blocked","kind":"permission","app":"claude-code"}`), processCall: procLogin, captureCall: string(empty)}}, Refuse, "the agent is waiting on a permission dialog"},
		{"subagent blocked", &recorder{reply: map[string]string{statusCall: status(`{"state":"idle","app":"claude-code"},{"id":"a","state":"blocked","kind":"question","app":"claude-code"}`), processCall: procLogin, captureCall: string(empty)}}, Refuse, "the agent is waiting on a question dialog"},
		{"draft", &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: string(draft)}}, Refuse, `you have a half-typed message there: "half typed"`},
		{"no box", &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: "nothing here\n", plainCall: "nothing here\n"}}, Refuse, "couldn't read the agent's input box"},
		{"suspended", &recorder{ps: "stopped", reply: map[string]string{statusCall: idleStatus, processCall: procLogin}}, Refuse, "Claude is suspended there"},
		{"no record yet", &recorder{reply: map[string]string{statusCall: noRecords, processCall: procLogin}}, Refuse, "the agent may still be starting; try again in a moment"},
		{"something else runs", &recorder{ps: "vim", reply: map[string]string{statusCall: idleStatus, processCall: procLogin}}, Refuse, "can't tell what's running there"},
		{"remote", &recorder{reply: map[string]string{inspectCall: `{"block":{"host_id":"host-other"}}`, statusCall: idleStatus, processCall: procLogin}}, Refuse, "that pane is on another machine"},
		{"rex down", &recorder{fail: map[string]error{statusCall: errors.New("no server")}}, Refuse, "couldn't reach Rex: no server"},
		{"mode switch", &recorder{}, Refuse, "replies starting with / or ! must be typed in the pane"},
		{"gone", &recorder{fail: map[string]error{statusCall: exitErr(3)}}, Resume, ""},
		{"shell left", &recorder{ps: "gone", reply: map[string]string{statusCall: idleStatus, processCall: procLogin}}, Resume, ""},
	}
	for _, c := range cases {
		reply := "yes"
		if c.name == "mode switch" {
			reply = "/clear"
		}
		got, why := c.rec.rex().Choose(context.Background(), "block:1", reply)
		if got != c.want || why != c.why {
			t.Errorf("%s: got %v %q, want %v %q", c.name, got, why, c.want, c.why)
		}
	}
}

// What Send says when it typed but didn't press Enter.
func TestClaudeCompatNotSentWording(t *testing.T) {
	typed := boxWith("yes")
	for name, c := range map[string]struct {
		rec *recorder
		why string
	}{
		"claude exited": {&recorder{ps: "gone", reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: typed}}, "Claude is no longer running there"},
		"now working":   {&recorder{reply: map[string]string{statusCall: `{"records":[{"state":"working","app":"claude-code"}]}`, processCall: procLogin, captureCall: typed}}, "the agent is busy"},
		"box empty":     {&recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: boxWith("")}}, "the input box is empty"},
		"box unknown":   {&recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: "x\n", plainCall: "x\n"}}, "couldn't read the agent's input box"},
	} {
		err := c.rec.rex().Send(context.Background(), "block:1", "yes")
		if err == nil || err.Error() != "typed but not sent: "+c.why {
			t.Errorf("%s: got %v, want %q", name, err, c.why)
		}
	}
	rec := &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: typed}}
	if err := rec.rex().Send(context.Background(), "block:1", "yes"); err != nil {
		t.Fatal(err)
	}
	want := []string{"send -b block:1 -- yes", inspectCall, whoamiCall, statusCall, processCall, captureCall, "send-key -b block:1 enter"}
	if got := rec.joined(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("calls\n got %q\nwant %q", got, want)
	}
}

// The claude --resume command line and every way Resume refuses.
func TestClaudeCompatResume(t *testing.T) {
	dir := t.TempDir()
	rec := &recorder{}
	if err := rec.rex().Resume(context.Background(), "session:1", dir, "0123456789abcdef", "go ahead"); err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "-s", "session:1", "-c", dir, "--keep-open", "--label", "claude 01234567",
		"--", "claude", "--resume", "0123456789abcdef", "--", "go ahead"}
	if len(rec.calls) != 1 || strings.Join(rec.calls[0], "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv\n got %q\nwant %q", rec.calls, want)
	}

	file := filepath.Join(dir, "f")
	os.WriteFile(file, nil, 0o600)
	running := t.TempDir()
	for name, c := range map[string]struct {
		rec       *recorder
		cwd, id   string
		reply     string
		wantError string
	}{
		"no id":       {&recorder{}, dir, "", "", "no Claude session to resume"},
		"no cwd":      {&recorder{}, "", "abc", "", "no project directory for that conversation"},
		"gone dir":    {&recorder{}, filepath.Join(dir, "gone"), "abc", "", "the project directory is gone"},
		"file":        {&recorder{}, file, "abc", "", "the project directory is gone"},
		"mode switch": {&recorder{}, dir, "abc", "!ls", "replies starting with / or ! must be typed in the pane"},
		"running":     {&recorder{cwds: map[int]string{3487: running}}, running, "abc", "", "a Claude is already running in that project; jump there or answer in it"},
		"can't tell":  {&recorder{cwdsErr: errors.New("lsof: boom")}, dir, "abc", "", "couldn't check for a running Claude: lsof: boom"},
		"ps fails":    {&recorder{psErr: errors.New("ps: boom")}, dir, "abc", "", "couldn't check for a running Claude: ps: boom"},
	} {
		err := c.rec.rex().Resume(context.Background(), "session:1", c.cwd, c.id, c.reply)
		if err == nil || err.Error() != c.wantError || len(c.rec.calls) != 0 {
			t.Errorf("%s: err=%v calls=%q", name, err, c.rec.joined())
		}
	}
}

// A Claude counts as running in a directory whose ~/.claude/projects folder
// name is the same, since claude --resume would find the conversation there.
func TestClaudeCompatRunningInSharesProjectFolders(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "app-demo")
	b := filepath.Join(root, "app", "demo")
	for _, d := range []string{a, b} {
		os.MkdirAll(d, 0o755)
	}
	rec := &recorder{cwds: map[int]string{9002: b}}
	running, err := rec.rex().ClaudeRunningIn(context.Background(), a)
	if err != nil || !running {
		t.Fatalf("running=%v err=%v", running, err)
	}
	rec = &recorder{ps: "noclaude"}
	if running, err := rec.rex().ClaudeRunningIn(context.Background(), a); err != nil || running || len(rec.cwdPids) != 0 {
		t.Fatalf("no claude: running=%v err=%v lsof=%v", running, err, rec.cwdPids)
	}
}

// The process names that count as Claude, and the ones that don't.
func TestClaudeCompatProcessNames(t *testing.T) {
	for comm, want := range map[string]bool{
		"/Users/me/.local/bin/claude": true,
		"claude":                      true,
		"/Users/me/.local/share/claude/versions/2.1.3": true,
		"pi":                           false,
		"node":                         false,
		"/usr/bin/login":               false,
		"-zsh":                         false,
		"/opt/claude-tools/bin/helper": false,
	} {
		if got := claudeLike(comm); got != want {
			t.Errorf("%q: got %v want %v", comm, got, want)
		}
	}
}
