package rexctl

// Characterization tests through the agent-aware entry points: with
// agent.Claude they must do exactly what the Claude entry points always
// did. They touch no Pi code paths.

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tfcace/rexq/internal/agent"
)

// Every Claude box fixture reads the same through InputBoxOf as through the
// old parser.
func TestClaudeAgentBoxFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/box-*")
	if len(files) < 10 {
		t.Fatalf("only %d fixtures", len(files))
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var want Box
		if filepath.Ext(f) == ".vt" {
			var err error
			if want, err = parseBoxVT(string(b)); err != nil {
				t.Fatal(err)
			}
		} else {
			want = parseBox(string(b))
		}
		key := captureCall
		if filepath.Ext(f) == ".txt" {
			// A plain capture as the VT one fails to decode, falling back.
			key = plainCall
		}
		rec := &recorder{reply: map[string]string{key: string(b)}}
		if key == plainCall {
			rec.reply[captureCall] = "\x1b[5A"
		}
		got, err := rec.rex().InputBoxOf(context.Background(), agent.Claude, "block:1")
		if err != nil || got != want {
			t.Errorf("%s: got %+v %v, want %+v", f, got, err, want)
		}
	}
}

// The Claude status and decision tables give the same answers, calls and
// wording through AgentStatus, ChooseFor and SendTo as through ClaudeStatus,
// Choose and Send.
func TestClaudeAgentEntryPointsMatch(t *testing.T) {
	empty, _ := os.ReadFile("testdata/box-empty.txt")
	draft, _ := os.ReadFile("testdata/box-draft.vt")
	type setup struct {
		ps     string
		reply  map[string]string
		failOn string
	}
	setups := map[string]setup{
		"idle":       {"claude", map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: string(empty)}, ""},
		"error":      {"claude", map[string]string{statusCall: `{"records":[{"state":"error","app":"claude-code"}]}`, processCall: procLogin, captureCall: string(empty)}, ""},
		"draft":      {"claude", map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: string(draft)}, ""},
		"stopped":    {"stopped", map[string]string{statusCall: idleStatus, processCall: procLogin}, ""},
		"root":       {"root", map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: string(empty)}, ""},
		"vim":        {"vim", map[string]string{statusCall: idleStatus, processCall: procLogin}, ""},
		"shell":      {"gone", map[string]string{statusCall: idleStatus, processCall: procLogin}, ""},
		"no records": {"claude", map[string]string{statusCall: noRecords, processCall: procLogin}, ""},
		"pi pane":    {"pi", map[string]string{statusCall: `{"records":[{"state":"idle","app":"pi"}]}`, processCall: procLogin}, ""},
		"typed":      {"claude", map[string]string{statusCall: idleStatus, processCall: procLogin, captureCall: boxWith("yes")}, ""},
	}
	for name, s := range setups {
		oldRec := &recorder{ps: s.ps, reply: s.reply}
		newRec := &recorder{ps: s.ps, reply: s.reply}
		st1, ok1, err1 := oldRec.rex().ClaudeStatus(context.Background(), "block:1")
		st2, ok2, err2 := newRec.rex().AgentStatus(context.Background(), agent.Claude, "block:1")
		if st1 != st2 || ok1 != ok2 || (err1 == nil) != (err2 == nil) {
			t.Errorf("%s: status %+v %v %v vs %+v %v %v", name, st1, ok1, err1, st2, ok2, err2)
		}
		d1, w1 := oldRec.rex().Choose(context.Background(), "block:1", "yes")
		d2, w2 := newRec.rex().ChooseFor(context.Background(), agent.Claude, "block:1", "yes")
		if d1 != d2 || w1 != w2 {
			t.Errorf("%s: choose %v %q vs %v %q", name, d1, w1, d2, w2)
		}
		e1 := oldRec.rex().Send(context.Background(), "block:1", "yes")
		e2 := newRec.rex().SendTo(context.Background(), agent.Claude, "block:1", "yes")
		if (e1 == nil) != (e2 == nil) || (e1 != nil && e1.Error() != e2.Error()) {
			t.Errorf("%s: send %v vs %v", name, e1, e2)
		}
		if !reflect.DeepEqual(oldRec.calls, newRec.calls) {
			t.Errorf("%s: calls differ\n old %q\n new %q", name, oldRec.joined(), newRec.joined())
		}
	}
}

func TestClaudeAgentResumeMatches(t *testing.T) {
	dir := t.TempDir()
	running := t.TempDir()
	for name, c := range map[string]struct {
		cwds    map[int]string
		cwd, id string
		reply   string
	}{
		"reply":    {nil, dir, "0123456789abcdef", "go ahead"},
		"no reply": {nil, dir, "abc", ""},
		"running":  {map[int]string{3487: running}, running, "abc", ""},
		"no id":    {nil, dir, "", ""},
	} {
		oldRec := &recorder{cwds: c.cwds}
		newRec := &recorder{cwds: c.cwds}
		e1 := oldRec.rex().Resume(context.Background(), "session:1", c.cwd, c.id, c.reply)
		e2 := newRec.rex().ResumeAgent(context.Background(), agent.Claude, "session:1", c.cwd, c.id, c.reply)
		if (e1 == nil) != (e2 == nil) || (e1 != nil && e1.Error() != e2.Error()) || !reflect.DeepEqual(oldRec.calls, newRec.calls) {
			t.Errorf("%s: %v %q vs %v %q", name, e1, oldRec.joined(), e2, newRec.joined())
		}
		r1, err1 := oldRec.rex().ClaudeRunningIn(context.Background(), c.cwd)
		r2, err2 := newRec.rex().RunningIn(context.Background(), agent.Claude, c.cwd)
		if r1 != r2 || (err1 == nil) != (err2 == nil) {
			t.Errorf("%s: running %v %v vs %v %v", name, r1, err1, r2, err2)
		}
	}
}
