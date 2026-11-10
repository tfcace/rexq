package rexctl

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/tfcace/rexq/internal/agent"
)

// What Rex reports for Pi (1.5.1), which writes its own status records.
const (
	piIdle    = `{"records":[{"state":"idle","app":"pi","effective_app":"pi","owner":{"pid":3487,"name":"node"}}]}`
	piDone    = `{"records":[{"state":"done","app":"pi"}]}`
	piWorking = `{"records":[{"state":"working","app":"pi"}]}`
	piError   = `{"records":[{"state":"error","app":"pi"}]}`
	piBlocked = `{"records":[{"state":"blocked","kind":"auth","app":"pi"}]}`
)

func piBox(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../inputbox/testdata/pi-box-" + name + ".vt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPiStatus(t *testing.T) {
	cases := []struct {
		name, ps, status string
		ok               bool
		hold, state      string
		ready            bool
	}{
		{"idle under a shell", "pi", piIdle, true, "", "idle", false},
		{"pi is the block's command", "pi-root", piIdle, true, "", "idle", false},
		{"error", "pi", piError, true, "", "error", true},
		{"working", "pi", piWorking, true, "", "working", false},
		{"suspended", "pi-stopped", piIdle, true, "Pi is suspended there", "", false},
		{"no record yet", "pi", `{"records":[]}`, true, starting, "", false},
		{"only Claude's record", "pi", idleStatus, true, starting, "", false},
		{"pi gone, shell left", "gone", piIdle, false, "", "", false},
		{"claude runs there, not pi", "claude", piIdle, true, cantTell, "", false},
	}
	for _, c := range cases {
		rec := &recorder{ps: c.ps, reply: map[string]string{statusCall: c.status, processCall: procLogin}}
		st, ok, err := rec.rex().AgentStatus(context.Background(), agent.Pi, "block:1")
		if err != nil || ok != c.ok || st.Hold != c.hold || (c.state != "" && st.State != c.state) || st.Ready != c.ready {
			t.Errorf("%s: got %+v ok=%v err=%v", c.name, st, ok, err)
		}
	}
	// Claude's status never reads a Pi pane as Claude's.
	rec := &recorder{ps: "pi", reply: map[string]string{statusCall: piIdle, processCall: procLogin}}
	if st, ok, err := rec.rex().ClaudeStatus(context.Background(), "block:1"); err != nil || !ok || st.Hold != cantTell {
		t.Errorf("Claude status of a Pi pane: %+v ok=%v err=%v", st, ok, err)
	}
}

func TestChooseForPi(t *testing.T) {
	for name, c := range map[string]struct {
		status, box, ps string
		want            Delivery
		why             string
	}{
		"idle, empty":          {piIdle, "empty", "", Type, ""},
		"done, empty":          {piDone, "done", "", Type, ""},
		"error, empty":         {piError, "empty", "", Type, ""},
		"draft":                {piIdle, "draft", "", Refuse, `you have a half-typed message there: "half typed"`},
		"multi-line draft":     {piIdle, "multiline", "", Refuse, `you have a half-typed message there: "first line second line"`},
		"scrolled draft":       {piIdle, "scrolled", "", Refuse, "couldn't read the agent's input box"},
		"file autocomplete":    {piIdle, "at-complete", "", Refuse, "couldn't read the agent's input box"},
		"command autocomplete": {piIdle, "slash-complete", "", Refuse, "couldn't read the agent's input box"},
		"selector open":        {piIdle, "selector", "", Refuse, "couldn't read the agent's input box"},
		"working":              {piWorking, "working", "", Refuse, "the agent is busy"},
		"blocked":              {piBlocked, "empty", "", Refuse, "the agent is waiting on a auth dialog"},
		"suspended":            {piIdle, "empty", "pi-stopped", Refuse, "Pi is suspended there"},
	} {
		ps := c.ps
		if ps == "" {
			ps = "pi"
		}
		rec := &recorder{ps: ps, reply: map[string]string{statusCall: c.status, processCall: procLogin, captureCall: piBox(t, c.box)}}
		got, why := rec.rex().ChooseFor(context.Background(), agent.Pi, "block:1", "yes")
		if got != c.want || why != c.why {
			t.Errorf("%s: got %v %q, want %v %q", name, got, why, c.want, c.why)
		}
	}
	// A gone Pi is resumed.
	rec := &recorder{ps: "gone", reply: map[string]string{statusCall: piIdle, processCall: procLogin}}
	if got, _ := rec.rex().ChooseFor(context.Background(), agent.Pi, "block:1", "yes"); got != Resume {
		t.Errorf("gone: got %v", got)
	}
}

// pibox is Pi's box holding draft, as a plain capture.
func pibox(draft string) string {
	rule := strings.Repeat("─", 80)
	return rule + "\n" + draft + "\n" + rule + "\n~/code (main)\n$0.000 0.0%/200k   model\n"
}

func TestSendToPi(t *testing.T) {
	rec := &recorder{ps: "pi", reply: map[string]string{statusCall: piIdle, processCall: procLogin, captureCall: pibox("yes, commit")}}
	if err := rec.rex().SendTo(context.Background(), agent.Pi, "block:1", "yes, commit"); err != nil {
		t.Fatal(err)
	}
	if got := rec.joined(); got[0] != "send -b block:1 -- yes, commit" || got[len(got)-1] != "send-key -b block:1 enter" {
		t.Fatalf("calls %q", got)
	}
	for name, c := range map[string]struct {
		rec *recorder
		why string
	}{
		"autocomplete opened": {&recorder{ps: "pi", reply: map[string]string{statusCall: piIdle, processCall: procLogin,
			captureCall: pibox("see @READ") + "→ README.md\n"}}, "couldn't read the agent's input box"},
		"pi exited": {&recorder{ps: "gone", reply: map[string]string{statusCall: piIdle, processCall: procLogin, captureCall: pibox("yes")}},
			"Pi is no longer running there"},
		"box differs": {&recorder{ps: "pi", reply: map[string]string{statusCall: piIdle, processCall: procLogin, captureCall: pibox("x yes")}},
			`the input box shows "x yes", not the reply`},
	} {
		err := c.rec.rex().SendTo(context.Background(), agent.Pi, "block:1", "yes")
		var ns *NotSentError
		if !errors.As(err, &ns) || ns.Why != c.why {
			t.Errorf("%s: err %v", name, err)
		}
		for _, call := range c.rec.joined() {
			if strings.HasPrefix(call, "send-key") {
				t.Errorf("%s: pressed enter", name)
			}
		}
	}
}

func TestResumePi(t *testing.T) {
	dir := t.TempDir()
	rec := &recorder{ps: "noclaude"}
	// Pi's ids are UUIDv7: the label takes the random tail, not the
	// timestamp head two sessions of the same minute share.
	if err := rec.rex().ResumeAgent(context.Background(), agent.Pi, "session:1", dir, "01a1c30f-ea0a-763c-8a30-17d795acac59", "-go"); err != nil {
		t.Fatal(err)
	}
	want := "run -s session:1 -c " + dir + " --keep-open --label pi 95acac59 -- pi --session 01a1c30f-ea0a-763c-8a30-17d795acac59 -- -go"
	if got := rec.joined(); len(got) != 1 || got[0] != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	for name, c := range map[string]struct {
		rec       *recorder
		id, reply string
		want      string
	}{
		"no id":       {&recorder{}, "", "", "no Pi session to resume"},
		"mode switch": {&recorder{}, "abc", "/model", "replies starting with / or ! must be typed in the pane"},
		"running":     {&recorder{ps: "pi", cwds: map[int]string{3487: dir}}, "abc", "", "a Pi is already running in that project; jump there or answer in it"},
		"can't tell":  {&recorder{ps: "pi", cwdsErr: errors.New("lsof: boom")}, "abc", "", "couldn't check for a running Pi: lsof: boom"},
	} {
		err := c.rec.rex().ResumeAgent(context.Background(), agent.Pi, "session:1", dir, c.id, c.reply)
		if err == nil || err.Error() != c.want || len(c.rec.calls) != 0 {
			t.Errorf("%s: err=%v calls=%q", name, err, c.rec.joined())
		}
	}
}

// Only Pi processes count for a Pi, and only in that very directory.
func TestRunningInForPi(t *testing.T) {
	dir := t.TempDir()
	rec := &recorder{ps: "pi", cwds: map[int]string{4100: dir}}
	if running, err := rec.rex().RunningIn(context.Background(), agent.Pi, dir); err != nil || !running {
		t.Fatalf("running=%v err=%v", running, err)
	}
	if got := rec.cwdPids; len(got) != 1 || len(got[0]) != 3 { // 3487, 4100, 9002; not pi-rpc or claude
		t.Errorf("asked lsof about %v", got)
	}
	// A Pi next door, in a directory with the same ~/.claude/projects name,
	// doesn't count.
	rec = &recorder{ps: "pi", cwds: map[int]string{4100: dir + "-x"}}
	if running, err := rec.rex().RunningIn(context.Background(), agent.Pi, dir+".x"); err != nil || running {
		t.Fatalf("running=%v err=%v", running, err)
	}
	// A Claude in the directory doesn't stop a Pi resume.
	rec = &recorder{ps: "claude", cwds: map[int]string{3487: dir}}
	if running, err := rec.rex().RunningIn(context.Background(), agent.Pi, dir); err != nil || running {
		t.Fatalf("claude counted: running=%v err=%v", running, err)
	}
}
