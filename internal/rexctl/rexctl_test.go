package rexctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// recorder is a fake Runner. It records every call; reply maps the joined
// args (as in "block call -b block:1 program_status") to the output, and
// fail maps them to an error. ps is the process listing its procs returns
// (testdata/ps-claude.txt when empty), and psErr fails it.
type recorder struct {
	calls [][]string
	reply map[string]string
	fail  map[string]error
	ps    string
	psErr error

	// cwds are the working directories its procCwds reports, by pid; a pid
	// not in it is in /elsewhere. cwdsErr fails procCwds, and cwdPids
	// records the pids it was asked about.
	cwds    map[int]string
	cwdsErr error
	cwdPids [][]int
}

func (r *recorder) run(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	key := strings.Join(args, " ")
	if out, ok := r.reply[key]; ok || r.fail[key] != nil {
		return []byte(out), r.fail[key]
	}
	return []byte(defaults[key]), nil
}

// defaults answer the calls most tests don't care about: the block is on
// this machine.
var defaults = map[string]string{
	inspectCall: `{"session_id":"session:1","block":{"block_id":"block:1","host_id":"host-local"}}`,
	whoamiCall:  `{"endpoint":"unix:///Users/me/Library/Application%20Support/rex/server.sock","host_id":"host-local"}`,
}

func (r *recorder) count(call string) int {
	n := 0
	for _, c := range r.joined() {
		if c == call {
			n++
		}
	}
	return n
}

func (r *recorder) procs(context.Context) ([]byte, error) {
	if r.psErr != nil {
		return nil, r.psErr
	}
	name := r.ps
	if name == "" {
		name = "claude"
	}
	return os.ReadFile("testdata/ps-" + name + ".txt")
}

// procCwds answers as `lsof -a -d cwd -Fn -p` does.
func (r *recorder) procCwds(_ context.Context, pids []int) ([]byte, error) {
	r.cwdPids = append(r.cwdPids, pids)
	if r.cwdsErr != nil {
		return nil, r.cwdsErr
	}
	var b strings.Builder
	for _, pid := range pids {
		cwd, ok := r.cwds[pid]
		if !ok {
			cwd = "/elsewhere"
		}
		b.WriteString("p" + strconv.Itoa(pid) + "\nfcwd\nn" + cwd + "\n")
	}
	return []byte(b.String()), nil
}

// rex is a Rex that runs the recorder, with a short Send wait.
func (r *recorder) rex() Rex {
	return Rex{Run: r.run, Procs: r.procs, ProcCwds: r.procCwds, wait: 30 * time.Millisecond}
}

func (r *recorder) joined() []string {
	var out []string
	for _, c := range r.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

// exitErr stands in for *exec.ExitError: it has the same ExitCode method.
type exitErr int

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

const (
	statusCall  = "block call -b block:1 program_status"
	processCall = "block call -b block:1 process"
	captureCall = "capture -b block:1 --format=vt --trim" // keeps styles: faint text isn't typed
	plainCall   = "capture -b block:1 --trim"
	inspectCall = "block inspect block:1 --json"
	whoamiCall  = "whoami --json"
	idleStatus  = `{"records":[{"state":"idle","app":"claude-code"}]}`
	noRecords   = `{"records":[{"state":"idle","app":"other-app"}]}`
	// What Rex reports for an ordinary tab: login is both child and
	// foreground, whatever runs under it.
	procLogin = `{"child":{"pid":3356,"name":"login","path":"/usr/bin/login"},"foreground":{"pid":3356,"name":"login","path":"/usr/bin/login"}}`

	starting  = "the agent may still be starting; try again in a moment"
	suspended = "Claude is suspended there"
	cantTell  = "can't tell what's running there"
	remote    = "that pane is on another machine"
)

func TestDecide(t *testing.T) {
	empty := Box{Known: true}
	cases := []struct {
		st   Status
		ok   bool
		box  Box
		want Delivery
	}{
		{Status{State: "idle"}, true, empty, Type},
		{Status{State: "done"}, true, empty, Type},
		{Status{State: "working"}, true, empty, Refuse}, // typing mid-turn: only idle and done are allowed
		{Status{State: ""}, true, empty, Refuse},
		{Status{State: "some-future-state"}, true, empty, Refuse},
		{Status{State: "blocked", Kind: "permission"}, true, empty, Refuse},
		{Status{State: "blocked", Kind: "auth"}, true, empty, Refuse},
		{Status{State: "blocked", Kind: "question"}, true, empty, Refuse}, // a dialog: Enter would pick an option
		{Status{State: "idle", Hold: suspended}, true, empty, Refuse},
		{Status{State: "idle"}, true, Box{Known: true, Draft: "half typed"}, Refuse},
		{Status{State: "idle"}, true, Box{}, Refuse}, // couldn't read the box
		{Status{}, false, Box{}, Resume},
	}
	for _, c := range cases {
		got, why := Decide(c.st, c.ok, c.box)
		if got != c.want {
			t.Errorf("%+v ok=%v box=%+v: got %v want %v", c.st, c.ok, c.box, got, c.want)
		}
		if got == Refuse && why == "" {
			t.Errorf("%+v: refused without a reason", c.st)
		}
	}
	if _, why := Decide(Status{State: "idle", Hold: suspended}, true, empty); why != suspended {
		t.Errorf("hold reason: got %q", why)
	}
}

func TestParseBox(t *testing.T) {
	for file, want := range map[string]Box{
		"testdata/box-draft.txt":     {Known: true, Draft: "half typed"},
		"testdata/box-empty.txt":     {Known: true},
		"testdata/box-try-draft.txt": {Known: true, Draft: `Try "npm test" first`}, // not the placeholder
		"testdata/box-wrapped.txt": {Known: true, Draft: "please go ahead and commit the change, then run the full " +
			"test suite with the race detector and tell me if anything fails"},
		"testdata/box-stale.txt":   {},            // an old box above a shell prompt
		"testdata/box-labeled.txt": {Known: true}, // a named session's top rule; no placeholder
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := parseBox(string(b)); got != want {
			t.Errorf("%s: got %+v want %+v", file, got, want)
		}
	}
	labeled := strings.Repeat("─", 30) + " notes ─\n❯\u00a0hi\n" + strings.Repeat("─", 30) + "\n"
	if got := parseBox(labeled); got != (Box{Known: true, Draft: "hi"}) {
		t.Errorf("labeled rule: got %+v", got)
	}
	for _, notRule := range []string{"─── short ─", "text " + strings.Repeat("─", 30), strings.Repeat("─", 30) + " text"} {
		if got := parseBox(notRule + "\n❯\u00a0hi\n" + strings.Repeat("─", 30) + "\n"); got.Known {
			t.Errorf("%q read as a rule: %+v", notRule, got)
		}
	}
	if got := parseBox("no box on this screen\n"); got.Known {
		t.Errorf("found a box where there is none: %+v", got)
	}
}

// Real VT captures of Claude Code 2.1.319's box (2026-11-06). The
// placeholder, and the suggested next prompt Claude offers after a turn, are
// faint (SGR 2): nothing the user typed.
func TestParseBoxVT(t *testing.T) {
	for file, want := range map[string]Box{
		"testdata/box-empty.vt":   {Known: true}, // ❯ ESC[2mTry "edit <filepath> to..."
		"testdata/box-cleared.vt": {Known: true}, // the placeholder back after ctrl+u
		"testdata/box-draft.vt":   {Known: true, Draft: "half typed"},
		"testdata/box-wrapped.vt": {Known: true, Draft: "half typed and then a much longer continuation " +
			"so that it wraps onto a second line of the box"}, // spaces sent as ESC[1C
		"testdata/box-paste.vt":            {Known: true, Draft: "[Pasted text #1]"}, // not faint: a draft
		"testdata/box-suggestion.vt":       {Known: true},                            // faint suggestion, labeled rule
		"testdata/box-typed-then-faint.vt": {Known: true, Draft: "tried"},
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseBoxVT(string(b))
		if err != nil || got != want {
			t.Errorf("%s: got %+v err=%v, want %+v", file, got, err, want)
		}
	}
}

func TestParseBoxVTKeepsTheBoxRules(t *testing.T) {
	rule := "\x1b[0m\x1b[38;2;136;136;136m" + strings.Repeat("─", 40) + "\x1b[0m\r\n"
	for name, c := range map[string]struct {
		screen string
		want   Box
	}{
		"faint not matching the placeholder": {rule + "❯\u00a0\x1b[2manything at all\x1b[0m\r\n" + rule, Box{Known: true}},
		"bold is typed":                      {rule + "❯\u00a0\x1b[1mbold\x1b[0m\r\n" + rule, Box{Known: true, Draft: "bold"}},
		"faint ends at SGR 22":               {rule + "❯\u00a0\x1b[2mghost\x1b[22mtyped\r\n" + rule, Box{Known: true, Draft: "typed"}},
		"faint ends at SGR 0":                {rule + "❯\u00a0\x1b[2;3mghost\x1b[mtyped\r\n" + rule, Box{Known: true, Draft: "typed"}},
		"faint in a combined SGR":            {rule + "❯\u00a0\x1b[0;2mghost\x1b[0m\r\n" + rule, Box{Known: true}},
		"grey but not faint is typed":        {rule + "❯\u00a0\x1b[38;2;153;153;153mgrey\x1b[0m\r\n" + rule, Box{Known: true, Draft: "grey"}},
		"hyperlink is typed":                 {rule + "❯\u00a0\x1b]8;;file:///x\x1b\\x\x1b]8;;\x07 y\r\n" + rule, Box{Known: true, Draft: "x y"}},
		"cursor forward is spaces":           {rule + "❯\u00a0a\x1b[3Cb\r\n" + rule, Box{Known: true, Draft: "a b"}},
		"no box":                             {"hello\r\n", Box{}},
		"box too far up":                     {rule + "❯\u00a0\r\n" + rule + strings.Repeat("$ ls\r\n", boxTail+1), Box{}},
		"indent by cursor forward":           {rule + "❯\u00a0typed\r\n\x1b[2Cmore\r\n" + rule, Box{Known: true, Draft: "typed more"}},
		"faint continuation":                 {rule + "❯\u00a0typed\r\n  \x1b[2mghost\x1b[0m\r\n" + rule, Box{Known: true, Draft: "typed"}},
		"unindented line":                    {rule + "❯\u00a0typed\r\nmore\r\n" + rule, Box{}},
	} {
		got, err := parseBoxVT(c.screen)
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v err=%v, want %+v", name, got, err, c.want)
		}
	}
}

func TestParseBoxVTRejectsWhatItCantRead(t *testing.T) {
	rule := strings.Repeat("─", 40) + "\r\n"
	for name, screen := range map[string]string{
		"cursor up":        rule + "❯\u00a0a\x1b[1Ab\r\n" + rule,
		"erase":            rule + "❯\u00a0a\x1b[2Kb\r\n" + rule,
		"bare escape":      rule + "❯\u00a0a\x1b(Bb\r\n" + rule,
		"cut-off CSI":      rule + "❯\u00a0a\x1b[3",
		"unterminated OSC": rule + "❯\u00a0\x1b]8;;file:///x",
		"stray control":    rule + "❯\u00a0a\bb\r\n" + rule,
	} {
		if got, err := parseBoxVT(screen); err == nil {
			t.Errorf("%s: no error, got %+v", name, got)
		}
	}
}

// When the VT can't be read, InputBox falls back to the plain capture.
func TestInputBoxFallsBackToPlainCapture(t *testing.T) {
	draft, _ := os.ReadFile("testdata/box-draft.txt")
	rec := &recorder{reply: map[string]string{captureCall: "❯\x1b[5Agarbled", plainCall: string(draft)}}
	box, err := rec.rex().InputBox(context.Background(), "block:1")
	if err != nil || box != (Box{Known: true, Draft: "half typed"}) {
		t.Fatalf("got %+v err=%v", box, err)
	}
	if got := rec.joined(); strings.Join(got, "|") != captureCall+"|"+plainCall {
		t.Fatalf("calls %q", got)
	}

	// A readable VT capture is enough.
	vt, _ := os.ReadFile("testdata/box-suggestion.vt")
	rec = &recorder{reply: map[string]string{captureCall: string(vt)}}
	if box, err := rec.rex().InputBox(context.Background(), "block:1"); err != nil || box != (Box{Known: true}) || len(rec.calls) != 1 {
		t.Fatalf("got %+v err=%v calls=%q", box, err, rec.joined())
	}

	// A failing capture is an error, with no second try.
	rec = &recorder{fail: map[string]error{captureCall: exitErr(4)}}
	if _, err := rec.rex().InputBox(context.Background(), "block:1"); err == nil || len(rec.calls) != 1 {
		t.Fatalf("err=%v calls=%q", err, rec.joined())
	}
}

// The user's case: Claude suggested a next prompt in the box, faint, and the
// plain capture read it as a draft.
func TestChooseTypesUnderASuggestedPrompt(t *testing.T) {
	vt, _ := os.ReadFile("testdata/box-suggestion.vt")
	rec := &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: string(vt)}}
	if d, why := rec.rex().Choose(context.Background(), "block:1", "1"); d != Type {
		t.Fatalf("got %v %q", d, why)
	}
}

func TestRefusalsQuoteTheDraft(t *testing.T) {
	for draft, want := range map[string]string{
		"half typed": `you have a half-typed message there: "half typed"`,
		"tried typing 1 in the panel and then some more": `you have a half-typed message there: "tried typing 1 in the panel an…"`,
		"say \"hi\"\tnow": `you have a half-typed message there: "say \"hi\" now"`,
		"héllo ❯ wörld":   `you have a half-typed message there: "héllo ❯ wörld"`,
	} {
		_, why := Decide(Status{State: "idle"}, true, Box{Known: true, Draft: draft})
		if why != want {
			t.Errorf("%q: got %s\nwant %s", draft, why, want)
		}
	}
}

func TestClaudeStatusFindsClaudeRecord(t *testing.T) {
	rec := &recorder{reply: map[string]string{
		statusCall:  `{"records":[{"state":"blocked","kind":"permission","app":"claude-code"}]}`,
		processCall: procLogin,
	}}
	st, ok, err := rec.rex().ClaudeStatus(context.Background(), "block:1")
	if err != nil || !ok || st.Kind != "permission" || st.Hold != "" {
		t.Fatalf("got %+v ok=%v err=%v", st, ok, err)
	}
}

func TestClaudeStatusBlockedSubagentBlocksThePane(t *testing.T) {
	rec := &recorder{reply: map[string]string{
		statusCall: `{"records":[
			{"state":"working","app":"claude-code"},
			{"id":"agent-1","state":"blocked","kind":"permission","app":"claude-code"},
			{"state":"idle","app":"other-app"}]}`,
		processCall: procLogin,
	}}
	st, ok, err := rec.rex().ClaudeStatus(context.Background(), "block:1")
	if err != nil || !ok || st.State != "blocked" || st.Kind != "permission" || st.ID != "agent-1" {
		t.Fatalf("got %+v ok=%v err=%v", st, ok, err)
	}
}

func TestClaudeStatusTakesStateFromRoot(t *testing.T) {
	rec := &recorder{reply: map[string]string{
		statusCall: `{"records":[
			{"id":"agent-1","state":"done","app":"claude-code"},
			{"state":"working","app":"claude-code"}]}`,
		processCall: procLogin,
	}}
	st, ok, err := rec.rex().ClaudeStatus(context.Background(), "block:1")
	if err != nil || !ok || st.State != "working" || st.ID != "" {
		t.Fatalf("got %+v ok=%v err=%v", st, ok, err)
	}
}

// The pane's process tree, read from ps, says whether Claude runs there;
// Rex's records then say what it's doing.
func TestClaudeStatusFromProcessTree(t *testing.T) {
	cases := []struct {
		name, ps, status, process string
		ok                        bool
		hold, state               string
	}{
		{"claude running", "claude", idleStatus, procLogin, true, "", "idle"},
		{"claude suspended", "stopped", idleStatus, procLogin, true, suspended, ""},
		{"claude gone, leftover record", "gone", idleStatus, procLogin, false, "", ""},
		{"claude gone, no record", "gone", noRecords, procLogin, false, "", ""},
		{"claude is the root", "root", idleStatus, procLogin, true, "", "idle"},
		{"claude has no record yet", "claude", noRecords, procLogin, true, starting, ""},
		{"claude root, no record yet", "root", `{"records":[]}`, procLogin, true, starting, ""},
		{"vim, no claude", "vim", noRecords, procLogin, true, cantTell, ""},
		{"vim, leftover record", "vim", idleStatus, procLogin, true, cantTell, ""},
		{"a bare shell as root", "shell-root", noRecords, procLogin, false, "", ""},
		{"a bare shell root with an unknown child", "shell-child", noRecords, procLogin, true, cantTell, ""},
		{"root not in ps", "claude", idleStatus, `{"child":{"pid":4242}}`, true, cantTell, ""},
		{"no child reported", "claude", idleStatus, `{"last_exit":null}`, true, cantTell, ""},
	}
	for _, c := range cases {
		rec := &recorder{ps: c.ps, reply: map[string]string{statusCall: c.status, processCall: c.process}}
		st, ok, err := rec.rex().ClaudeStatus(context.Background(), "block:1")
		if err != nil || ok != c.ok || st.Hold != c.hold || (c.state != "" && st.State != c.state) {
			t.Errorf("%s: got %+v ok=%v err=%v, want ok=%v hold=%q state=%q", c.name, st, ok, err, c.ok, c.hold, c.state)
		}
		if c.hold != "" {
			if d, why := Decide(st, ok, Box{Known: true}); d != Refuse || why != c.hold {
				t.Errorf("%s: Decide gave %v %q", c.name, d, why)
			}
		}
	}
}

func TestClaudeStatusAbsence(t *testing.T) {
	for name, rec := range map[string]*recorder{
		"block gone":            {fail: map[string]error{statusCall: exitErr(3)}},
		"block gone on inspect": {fail: map[string]error{inspectCall: exitErr(3)}},
		"block gone before process": {
			reply: map[string]string{statusCall: idleStatus},
			fail:  map[string]error{processCall: exitErr(3)},
		},
	} {
		st, ok, err := rec.rex().ClaudeStatus(context.Background(), "block:1")
		if err != nil || ok {
			t.Errorf("%s: got %+v ok=%v err=%v, want confirmed absence", name, st, ok, err)
		}
	}
}

func TestClaudeStatusErrorsAreNotAbsence(t *testing.T) {
	for name, rec := range map[string]*recorder{
		"server down":  {fail: map[string]error{statusCall: exitErr(4)}},
		"inspect down": {fail: map[string]error{inspectCall: exitErr(4)}},
		"bad inspect":  {reply: map[string]string{inspectCall: `{"block":`}},
		"other error":  {fail: map[string]error{statusCall: errors.New("exec: rex: not found")}},
		"bad json":     {reply: map[string]string{statusCall: `{"records":`}},
		"process down": {reply: map[string]string{statusCall: idleStatus}, fail: map[string]error{processCall: exitErr(1)}},
		"bad process":  {reply: map[string]string{statusCall: idleStatus, processCall: `nope`}},
		"ps fails":     {reply: map[string]string{statusCall: idleStatus, processCall: procLogin}, psErr: errors.New("ps: boom")},
		"ps garbage":   {reply: map[string]string{statusCall: idleStatus, processCall: procLogin}, ps: "garbage"},
	} {
		if _, ok, err := rec.rex().ClaudeStatus(context.Background(), "block:1"); err == nil {
			t.Errorf("%s: got ok=%v and no error", name, ok)
		}
	}
}

// ps describes only this machine, so a pane on another host is neither
// typed into nor resumed.
func TestClaudeStatusRefusesRemotePanes(t *testing.T) {
	for name, rec := range map[string]*recorder{
		"other host":    {reply: map[string]string{inspectCall: `{"block":{"host_id":"host-other"}}`}},
		"no host id":    {reply: map[string]string{inspectCall: `{"block":{"block_id":"block:1"}}`}},
		"remote server": {reply: map[string]string{whoamiCall: `{"endpoint":"https://remote.example:7297","host_id":"host-local"}`}},
		"server host":   {reply: map[string]string{whoamiCall: `{"endpoint":"unix:///tmp/rex.sock"}`}},
	} {
		for k, v := range map[string]string{statusCall: idleStatus, processCall: procLogin} {
			if _, set := rec.reply[k]; !set {
				if rec.reply == nil {
					rec.reply = map[string]string{}
				}
				rec.reply[k] = v
			}
		}
		st, ok, err := rec.rex().ClaudeStatus(context.Background(), "block:1")
		if err != nil || !ok || st.Hold != remote {
			t.Errorf("%s: got %+v ok=%v err=%v", name, st, ok, err)
		}
		if d, why := rec.rex().Choose(context.Background(), "block:1", "yes"); d != Refuse || why != remote {
			t.Errorf("%s: Choose gave %v %q", name, d, why)
		}
	}
}

// When this machine's host can't be looked up, the pane isn't known to be
// remote: the reason is the failed lookup. Its exit code never reads as a
// missing block.
func TestClaudeStatusLocalHostLookupFails(t *testing.T) {
	for name, rec := range map[string]*recorder{
		"whoami fails":     {fail: map[string]error{whoamiCall: exitErr(4)}},
		"whoami exits 3":   {fail: map[string]error{whoamiCall: exitErr(3)}},
		"whoami bad json":  {reply: map[string]string{whoamiCall: `nope`}},
		"whoami other err": {fail: map[string]error{whoamiCall: errors.New("rex whoami: boom")}},
	} {
		if rec.reply == nil {
			rec.reply = map[string]string{}
		}
		rec.reply[statusCall], rec.reply[processCall] = idleStatus, procLogin
		st, ok, err := rec.rex().ClaudeStatus(context.Background(), "block:1")
		if err == nil {
			t.Errorf("%s: got %+v ok=%v and no error", name, st, ok)
		}
		d, why := rec.rex().Choose(context.Background(), "block:1", "yes")
		if d != Refuse || !strings.HasPrefix(why, "couldn't reach Rex: ") {
			t.Errorf("%s: Choose gave %v %q", name, d, why)
		}
	}

	r := (&recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin}}).rex()
	r.LocalHost = func(context.Context) (string, error) { return "", errors.New("lookup broke") }
	if d, why := r.Choose(context.Background(), "block:1", "yes"); d != Refuse || !strings.Contains(why, "lookup broke") {
		t.Errorf("injected lookup: Choose gave %v %q", d, why)
	}
}

func TestHostLookupIsInjectableAndCached(t *testing.T) {
	rec := &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin,
		inspectCall: `{"block":{"host_id":"host-a"}}`}}
	r := New(rec.run)
	r.Procs = rec.procs
	lookups := 0
	r.LocalHost = func(context.Context) (string, error) { lookups++; return "host-a", nil }
	for range 3 {
		if st, ok, err := r.ClaudeStatus(context.Background(), "block:1"); err != nil || !ok || st.Hold != "" {
			t.Fatalf("got %+v ok=%v err=%v", st, ok, err)
		}
	}
	if lookups != 1 || rec.count(inspectCall) != 1 || rec.count(whoamiCall) != 0 {
		t.Fatalf("lookups=%d inspects=%d whoamis=%d, want 1 1 0", lookups, rec.count(inspectCall), rec.count(whoamiCall))
	}

	// The default lookup asks whoami, once per Rex value and its copies.
	rec = &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin}}
	r = New(rec.run)
	r.Procs = rec.procs
	copied := r
	r.ClaudeStatus(context.Background(), "block:1")
	copied.ClaudeStatus(context.Background(), "block:1")
	if rec.count(whoamiCall) != 1 || rec.count(inspectCall) != 1 {
		t.Fatalf("whoamis=%d inspects=%d, want 1 1", rec.count(whoamiCall), rec.count(inspectCall))
	}

	// A failed lookup isn't cached.
	rec = &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin}, fail: map[string]error{whoamiCall: exitErr(4)}}
	r = New(rec.run)
	r.Procs = rec.procs
	r.ClaudeStatus(context.Background(), "block:1")
	r.ClaudeStatus(context.Background(), "block:1")
	if rec.count(whoamiCall) != 2 {
		t.Fatalf("whoamis=%d, want 2", rec.count(whoamiCall))
	}
}

func TestParseProcs(t *testing.T) {
	out, _ := os.ReadFile("testdata/ps-claude.txt")
	ps, err := parseProcs(out)
	if err != nil {
		t.Fatal(err)
	}
	if p := ps[2151]; p.ppid != 1478 || !strings.HasSuffix(p.comm, "/Example Helper.app/Contents/MacOS/Example Helper") {
		t.Errorf("command with spaces: got %+v", p)
	}
	if p := ps[3487]; p.ppid != 3357 || p.stat != "Ss+" || p.comm != "/Users/me/.local/bin/claude" {
		t.Errorf("got %+v", p)
	}
	for _, bad := range []string{"12 1\n", "x 1 S /bin/sh\n", "12 y S /bin/sh\n"} {
		if _, err := parseProcs([]byte(bad)); err == nil {
			t.Errorf("parseProcs(%q): no error", bad)
		}
	}
}

func TestClaudeLike(t *testing.T) {
	for comm, want := range map[string]bool{
		"/Users/me/.local/bin/claude":                    true,
		"/Users/me/.local/share/claude/versions/2.1.319": true,
		"claude": true,
		"/opt/homebrew/lib/node_modules/x/claude-agent-sdk-darwin-arm64/claude": true,
		"-zsh":                   false,
		"/usr/bin/login":         false,
		"/Users/me/bin/claude-x": false,
		"/Users/me/claude/notes": false,
	} {
		if got := claudeLike(comm); got != want {
			t.Errorf("claudeLike(%q) = %v", comm, got)
		}
	}
}

// The real ps, run without a shell, lists this test process under its parent.
func TestListProcs(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("no ps on PATH")
	}
	out, err := listProcs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ps, err := parseProcs(out)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := ps[os.Getpid()]; !ok || p.ppid != os.Getppid() {
		t.Fatalf("own process: got %+v ok=%v", p, ok)
	}
}

func TestChoose(t *testing.T) {
	emptyBox, _ := os.ReadFile("testdata/box-empty.txt")
	cases := map[string]struct {
		rec  *recorder
		want Delivery
		why  string
	}{
		"type":       {rec: &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: string(emptyBox)}}, want: Type},
		"gone":       {rec: &recorder{fail: map[string]error{statusCall: exitErr(3)}}, want: Resume},
		"shell only": {rec: &recorder{ps: "gone", reply: map[string]string{statusCall: idleStatus, processCall: procLogin}}, want: Resume},
		"starting": {
			rec:  &recorder{reply: map[string]string{statusCall: noRecords, processCall: procLogin}},
			want: Refuse, why: starting,
		},
		"suspended": {
			rec:  &recorder{ps: "stopped", reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: string(emptyBox)}},
			want: Refuse, why: suspended,
		},
		"rex down": {
			rec:  &recorder{fail: map[string]error{statusCall: errors.New("no Rex server")}},
			want: Refuse, why: "couldn't reach Rex: no Rex server",
		},
		"ps fails": {
			rec:  &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin}, psErr: errors.New("ps: boom")},
			want: Refuse, why: "couldn't reach Rex: ps: boom",
		},
		"capture fails": {
			rec:  &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin}, fail: map[string]error{captureCall: errors.New("boom")}},
			want: Refuse, why: "couldn't reach Rex: boom",
		},
	}
	for name, c := range cases {
		got, why := c.rec.rex().Choose(context.Background(), "block:1", "yes")
		if got != c.want || (c.why != "" && why != c.why) {
			t.Errorf("%s: got %v %q, want %v %q", name, got, why, c.want, c.why)
		}
	}
}

var modeSwitching = []string{"/compact", "!rm -rf build", "  /clear", "\n!ls", "\x1b/model"}

const modeWhy = "replies starting with / or ! must be typed in the pane"

func TestChooseRefusesModeSwitchingReplies(t *testing.T) {
	emptyBox, _ := os.ReadFile("testdata/box-empty.txt")
	for _, reply := range modeSwitching {
		for name, rec := range map[string]*recorder{
			"live": {reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: string(emptyBox)}},
			"gone": {fail: map[string]error{statusCall: exitErr(3)}},
		} {
			d, why := rec.rex().Choose(context.Background(), "block:1", reply)
			if d != Refuse || why != modeWhy || len(rec.calls) != 0 {
				t.Errorf("%s %q: got %v %q calls=%q", name, reply, d, why, rec.joined())
			}
		}
	}
}

func TestSendRefusesModeSwitchingReplies(t *testing.T) {
	for _, reply := range modeSwitching {
		rec := &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: boxWith(reply)}}
		err := rec.rex().Send(context.Background(), "block:1", reply)
		if err == nil || err.Error() != modeWhy || len(rec.calls) != 0 {
			t.Errorf("%q: err=%v calls=%q", reply, err, rec.joined())
		}
	}
	// Elsewhere in the reply they're ordinary characters.
	rec := &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: boxWith("yes /tmp is fine!")}}
	if err := rec.rex().Send(context.Background(), "block:1", "yes /tmp is fine!"); err != nil {
		t.Fatal(err)
	}
}

func TestResumeRefusesModeSwitchingReplies(t *testing.T) {
	for _, reply := range modeSwitching {
		rec := &recorder{}
		err := Rex{Run: rec.run}.Resume(context.Background(), "session:1", t.TempDir(), "abc", reply)
		if err == nil || err.Error() != modeWhy || len(rec.calls) != 0 {
			t.Errorf("%q: err=%v calls=%q", reply, err, rec.joined())
		}
	}
}

func boxWith(draft string) string {
	rule := strings.Repeat("─", 80)
	return rule + "\n❯ " + draft + "\n" + rule + "\n  status line\n"
}

func TestSendTypesVerifiesThenPressesEnter(t *testing.T) {
	rec := &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: boxWith("yes, commit")}}
	if err := rec.rex().Send(context.Background(), "block:1", "yes, commit"); err != nil {
		t.Fatal(err)
	}
	got := rec.joined()
	if len(got) != 7 {
		t.Fatalf("want 7 calls (send, inspect, whoami, status, process, capture, enter), got %q", got)
	}
	if got[0] != "send -b block:1 -- yes, commit" || got[6] != "send-key -b block:1 enter" {
		t.Fatalf("got %q", got)
	}
}

func TestSendDoesNotPressEnterUnlessVerified(t *testing.T) {
	blocked := `{"records":[{"state":"idle","app":"claude-code"},` +
		`{"id":"a","state":"blocked","kind":"question","app":"claude-code"}]}`
	typed := boxWith("yes, commit")
	cases := map[string]*recorder{
		"box differs":   {reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: boxWith("half typed yes, commit")}},
		"box unknown":   {reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: "no box\n"}},
		"now blocked":   {reply: map[string]string{statusCall: blocked, processCall: procLogin, captureCall: typed}},
		"now working":   {reply: map[string]string{statusCall: strings.Replace(idleStatus, "idle", "working", 1), processCall: procLogin, captureCall: typed}},
		"now suspended": {ps: "stopped", reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: typed}},
		"claude exited": {ps: "gone", reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: typed}},
		"pane gone":     {reply: map[string]string{captureCall: typed}, fail: map[string]error{statusCall: exitErr(3)}},
		"status fails":  {reply: map[string]string{captureCall: typed}, fail: map[string]error{statusCall: exitErr(4)}},
		"ps fails":      {reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: typed}, psErr: errors.New("boom")},
	}
	for name, rec := range cases {
		err := rec.rex().Send(context.Background(), "block:1", "yes, commit")
		var ns *NotSentError
		if !errors.As(err, &ns) || ns.Why == "" || !strings.HasPrefix(err.Error(), "typed but not sent: ") {
			t.Errorf("%s: got err %v", name, err)
		}
		for _, c := range rec.joined() {
			if strings.HasPrefix(c, "send-key") {
				t.Errorf("%s: pressed a key: %q", name, c)
			}
		}
	}
}

func TestSendSaysWhatTheBoxHeld(t *testing.T) {
	for box, want := range map[string]string{
		boxWith("half typed yes, commit"): `typed but not sent: the input box shows "half typed yes, commit", not the reply`,
		boxWith(""):                       `typed but not sent: the input box is empty`,
		boxWith("an extremely long draft that goes on and on"): `typed but not sent: the input box shows "an extremely long draft that g…", not the reply`,
	} {
		rec := &recorder{reply: map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: box}}
		err := rec.rex().Send(context.Background(), "block:1", "yes, commit")
		if err == nil || err.Error() != want {
			t.Errorf("got %v\nwant %s", err, want)
		}
	}
}

func TestSendStopsWhenTypingFails(t *testing.T) {
	rec := &recorder{fail: map[string]error{"send -b block:1 -- yes": errors.New("boom")}}
	if err := rec.rex().Send(context.Background(), "block:1", "yes"); err == nil {
		t.Fatal("want an error")
	}
	if got := rec.joined(); len(got) != 1 {
		t.Fatalf("want only the send call, got %q", got)
	}
}

func TestSendRejectsEmptyReply(t *testing.T) {
	rec := &recorder{}
	if err := rec.rex().Send(context.Background(), "block:1", " \n\x1b "); err == nil || len(rec.calls) != 0 {
		t.Fatalf("err=%v calls=%q", err, rec.joined())
	}
}

func TestTypedLine(t *testing.T) {
	for in, want := range map[string]string{
		"yes, commit":             "yes, commit",
		"a\r\nb\rc\nd":            "a b c d",
		"tab\there":               "tab here",
		"red\x1b[31m\x07bell\x00": "red[31mbell",
		"del\x7f c1\u009bx":       "del c1x",
		"  padded  ":              "padded",
		"héllo ❯ ok":              "héllo ❯ ok",
	} {
		if got := typedLine(in); got != want {
			t.Errorf("typedLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResumeArgs(t *testing.T) {
	dir := t.TempDir()
	rec := &recorder{}
	if err := rec.rex().Resume(context.Background(), "session:1", dir, "0123456789ab", "-yes"); err != nil {
		t.Fatal(err)
	}
	want := "run -s session:1 -c " + dir + " --keep-open --label claude 01234567 -- claude --resume 0123456789ab -- -yes"
	if got := rec.joined(); len(got) != 1 || got[0] != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}

	rec = &recorder{}
	if err := rec.rex().Resume(context.Background(), "", dir, "abc", ""); err != nil {
		t.Fatal(err)
	}
	want = "run -c " + dir + " --keep-open --label claude abc -- claude --resume abc"
	if got := rec.joined(); len(got) != 1 || got[0] != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestResumeRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	os.WriteFile(file, nil, 0o600)
	for name, c := range map[string]struct{ cwd, id, want string }{
		"no id":       {dir, "", "no Claude session to resume"},
		"no cwd":      {"", "abc", "no project directory for that conversation"},
		"missing dir": {filepath.Join(dir, "gone"), "abc", "the project directory is gone"},
		"not a dir":   {file, "abc", "the project directory is gone"},
	} {
		rec := &recorder{}
		err := Rex{Run: rec.run}.Resume(context.Background(), "session:1", c.cwd, c.id, "hi")
		if err == nil || err.Error() != c.want || len(rec.calls) != 0 {
			t.Errorf("%s: err=%v calls=%q", name, err, rec.joined())
		}
	}
}

// fixtureClaudes are the Claude processes in testdata/ps-claude.txt.
var fixtureClaudes = []int{1881, 3255, 3487, 9002}

func TestClaudeRunningIn(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		cwds  map[int]string
		query string
		want  bool
	}{
		"same cwd":          {map[int]string{3487: dir}, dir, true},
		"headless too":      {map[int]string{9002: dir}, dir, true},
		"suspended too":     {map[int]string{3255: dir}, dir, true},
		"unclean query":     {map[int]string{3487: dir}, dir + "/./", true},
		"query is symlink":  {map[int]string{3487: dir}, link, true},
		"claude in symlink": {map[int]string{3487: link}, dir, true},
		"another cwd":       {map[int]string{3487: filepath.Join(dir, "sub")}, dir, false},
		"a parent cwd":      {map[int]string{3487: filepath.Dir(dir)}, dir, false},
		"nowhere near":      {nil, dir, false},
		// claude --resume finds a conversation by the encoded folder name, so
		// directories that encode alike are the same project to it.
		"encodes alike":    {map[int]string{3487: filepath.Join(dir, "app-demo")}, filepath.Join(dir, "app", "demo"), true},
		"dotted twin":      {map[int]string{3487: filepath.Join(dir, "app.demo")}, filepath.Join(dir, "app-demo"), true},
		"subdir of a twin": {map[int]string{3487: filepath.Join(dir, "app-demo", "x")}, filepath.Join(dir, "app", "demo"), false},
	} {
		rec := &recorder{cwds: c.cwds}
		got, err := rec.rex().ClaudeRunningIn(context.Background(), c.query)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v %v, want %v", name, got, err, c.want)
		}
		if len(rec.cwdPids) != 1 || fmt.Sprint(rec.cwdPids[0]) != fmt.Sprint(fixtureClaudes) {
			t.Errorf("%s: asked lsof about %v, want %v", name, rec.cwdPids, fixtureClaudes)
		}
	}
}

func TestClaudeRunningInWithoutClaudeSkipsLsof(t *testing.T) {
	rec := &recorder{ps: "noclaude"}
	if got, err := rec.rex().ClaudeRunningIn(context.Background(), t.TempDir()); err != nil || got {
		t.Fatalf("got %v %v", got, err)
	}
	if len(rec.cwdPids) != 0 {
		t.Fatalf("ran lsof: %v", rec.cwdPids)
	}
}

func TestClaudeRunningInErrors(t *testing.T) {
	if _, err := (&recorder{psErr: errors.New("ps: boom")}).rex().ClaudeRunningIn(context.Background(), "/x"); err == nil {
		t.Error("ps failing: no error")
	}
	if _, err := (&recorder{cwdsErr: errors.New("lsof: boom")}).rex().ClaudeRunningIn(context.Background(), "/x"); err == nil {
		t.Error("lsof failing: no error")
	}
}

// lsof exits 1 when a pid it was given is gone, and still reports the rest.
// That's fine for a Claude that exited meanwhile, but one still running
// whose directory it couldn't read is an error.
func TestClaudeRunningInPartialLsof(t *testing.T) {
	dir := t.TempDir()
	partial := func(_ context.Context, pids []int) ([]byte, error) {
		return []byte("p1881\nfcwd\nn/elsewhere\np3487\nfcwd\nn/elsewhere\n"), exitErr(1)
	}
	rec := &recorder{}
	r := rec.rex()
	r.ProcCwds = partial
	if _, err := r.ClaudeRunningIn(context.Background(), dir); err == nil {
		t.Error("3255 and 9002 still run, unread: no error")
	}

	// By the second listing, the Claudes lsof skipped have exited.
	lists := 0
	r.Procs = func(ctx context.Context) ([]byte, error) {
		lists++
		if lists == 1 {
			return os.ReadFile("testdata/ps-claude.txt")
		}
		return []byte(" 1881  1478 S    /Users/me/.local/bin/claude\n 3487  3357 Ss+  /Users/me/.local/bin/claude\n"), nil
	}
	if got, err := r.ClaudeRunningIn(context.Background(), dir); err != nil || got {
		t.Errorf("skipped Claudes exited: got %v %v", got, err)
	}
	r.ProcCwds = func(_ context.Context, pids []int) ([]byte, error) {
		return []byte("p3487\nfcwd\nn" + dir + "\n"), exitErr(1)
	}
	lists = 0
	if got, err := r.ClaudeRunningIn(context.Background(), dir); err != nil || !got {
		t.Errorf("a match in partial output: got %v %v", got, err)
	}
}

const alreadyRunning = "a Claude is already running in that project; jump there or answer in it"

func TestResumeRefusesWhileClaudeRunsInTheProject(t *testing.T) {
	dir := t.TempDir()
	rec := &recorder{cwds: map[int]string{3487: dir}}
	err := rec.rex().Resume(context.Background(), "session:1", dir, "abc", "yes")
	if err == nil || err.Error() != alreadyRunning {
		t.Fatalf("err=%v", err)
	}
	rec2 := &recorder{cwdsErr: errors.New("lsof: boom")}
	err = rec2.rex().Resume(context.Background(), "session:1", dir, "abc", "yes")
	if err == nil || err.Error() != "couldn't check for a running Claude: lsof: boom" {
		t.Fatalf("err=%v", err)
	}
	for _, r := range []*recorder{rec, rec2} {
		if len(r.calls) != 0 {
			t.Fatalf("ran rex: %q", r.joined())
		}
	}
}

func TestFocus(t *testing.T) {
	rec := &recorder{reply: map[string]string{
		"client ls --json": `{"clients":[{"client_id":"client:cli","info":{"kind":"cli"}},{"client_id":"client:app","info":{"kind":"app"}}]}`,
	}}
	if err := (Rex{Run: rec.run}).Focus(context.Background(), "session:me", "session:other", "block:1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"client ls --json", "-C client:app do session.select session_id=session:other", "focus -s session:other block:1"}
	if got := rec.joined(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}

	rec = &recorder{}
	if err := (Rex{Run: rec.run}).Focus(context.Background(), "session:me", "", "block:1"); err != nil {
		t.Fatal(err)
	}
	if got := rec.joined(); len(got) != 1 || got[0] != "focus block:1" {
		t.Fatalf("got %q", got)
	}
}

func TestHideSelfEncodesJSON(t *testing.T) {
	t.Setenv("REX_SESSION", `session:"1"`)
	t.Setenv("REX_BLOCK", "block:1")
	rec := &recorder{}
	if err := (Rex{Run: rec.run}).HideSelf(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := `api call session.detach_block {"session_id":"session:\"1\"","block_id":"block:1"}`
	if got := rec.joined(); len(got) != 1 || got[0] != want {
		t.Fatalf("got %q", got)
	}
}

// execRex runs a stand-in for rex that echoes its args and environment and
// fails the way rex does when a block is gone.
func TestExecPassesNoAutostartAndKeepsExitCode(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "rex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" \"session=$REX_SESSION\"\necho 'Error: block target \"block:1\" not found' >&2\nexit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_SESSION", "session:mine")
	out, err := execRex(context.Background(), bin, "capture", "-b", "block:1")
	if got := string(out); got != "--autostart=false\ncapture\n-b\nblock:1\nsession=\n" {
		t.Errorf("stdout %q", got)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("err %v does not unwrap to exit code 3", err)
	}
	if !notFound(err) {
		t.Errorf("notFound(%v) = false", err)
	}
	if !strings.Contains(err.Error(), `block target "block:1" not found`) {
		t.Errorf("stderr missing from %q", err)
	}
}

const (
	titleCall = "block call -b block:1 title"
	viewCall  = `api call session.view {"session_id":"session:1"}`
	// session.view with block:1 in a tab labeled label, beside another tab.
	viewTmpl = `{"session_id":"session:1","windows":[` +
		`{"window_id":"window:a","label":"elsewhere","layers":[{"blocks":[{"block_id":"block:9"}]}]},` +
		`{"window_id":"window:b","label":"LABEL","layers":[{"blocks":[{"block_id":"block:2"}]},{"kind":"floating","blocks":[{"block_id":"block:1"}]}]}]}`
)

func TestPaneName(t *testing.T) {
	for name, c := range map[string]struct {
		title, label, want string
	}{
		"title":              {title: "Open questions tracker for Rex", label: "rexq", want: "Open questions tracker for Rex"},
		"named tab":          {title: "", label: "rexq work", want: "rexq work"},
		"blank title":        {title: "  \t ", label: "rexq work", want: "rexq work"},
		"default tab":        {title: "", label: "Window 9de6e29d", want: ""},
		"control characters": {title: "evil\x1b]0;x\x07 title", want: "evil]0;x title"},
		"long title":         {title: strings.Repeat("abcdefghij", 6), want: strings.Repeat("abcdefghij", 4)[:39] + "…"},
	} {
		title, _ := json.Marshal(map[string]any{"title": c.title, "owner": map[string]any{"pid": 1}})
		rec := &recorder{reply: map[string]string{
			titleCall: string(title),
			viewCall:  strings.Replace(viewTmpl, "LABEL", c.label, 1),
		}}
		got, err := rec.rex().PaneName(context.Background(), "block:1")
		if err != nil || got != c.want {
			t.Errorf("%s: got %q %v, want %q", name, got, err, c.want)
		}
		if c.title != "" && strings.TrimSpace(c.title) != "" && rec.count(viewCall) != 0 {
			t.Errorf("%s: looked up the tab despite a title", name)
		}
	}
}

func TestPaneNameErrors(t *testing.T) {
	for name, rec := range map[string]*recorder{
		"title fails":   {fail: map[string]error{titleCall: exitErr(4)}},
		"bad title":     {reply: map[string]string{titleCall: `nope`}},
		"inspect fails": {reply: map[string]string{titleCall: `{"title":""}`}, fail: map[string]error{inspectCall: exitErr(3)}},
		"view fails":    {reply: map[string]string{titleCall: `{"title":""}`}, fail: map[string]error{viewCall: exitErr(4)}},
	} {
		if got, err := rec.rex().PaneName(context.Background(), "block:1"); err == nil {
			t.Errorf("%s: got %q and no error", name, got)
		}
	}
}
