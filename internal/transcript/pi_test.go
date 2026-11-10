package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testdata/pi-session.jsonl is a real Pi 1.5.1 session (system prompt and
// thinking trimmed): a plain turn, then a turn with a bash call.
func TestPiLastTurn(t *testing.T) {
	turn, ok, err := PiLastTurn("testdata/pi-session.jsonl")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if turn.SessionID != "01a1c30f-ea0a-763c-8a30-17d795acac59" || turn.Cwd != "/private/tmp/rexq-pi-fx" {
		t.Errorf("session %q cwd %q", turn.SessionID, turn.Cwd)
	}
	if !strings.HasPrefix(turn.Prompt, "Use the bash tool to run ls") || turn.PromptID != "e1a4472c" {
		t.Errorf("prompt %q id %q", turn.Prompt, turn.PromptID)
	}
	if !strings.HasSuffix(turn.Final, "Which file or directory would you like me to open?") || turn.MsgUUID != "694083d9" {
		t.Errorf("final %q id %q", turn.Final, turn.MsgUUID)
	}
	if turn.Provider != "anthropic" || turn.Model != "claude-haiku-4-5" {
		t.Errorf("provider %q model %q", turn.Provider, turn.Model)
	}
	if want := time.Date(2026, 11, 10, 1, 27, 17, 732e6, time.UTC); !turn.Time.Equal(want) {
		t.Errorf("time %v", turn.Time)
	}
}

// The same session after the user asked again and pressed Escape: the user
// had the last word.
func TestPiLastTurnAborted(t *testing.T) {
	if turn, ok, err := PiLastTurn("testdata/pi-aborted.jsonl"); err != nil || ok {
		t.Fatalf("got %+v ok=%v err=%v", turn, ok, err)
	}
}

func piLines(lines ...string) string {
	return `{"type":"session","version":3,"id":"s1","timestamp":"2026-11-10T00:00:00Z","cwd":"/p"}` + "\n" + strings.Join(lines, "\n") + "\n"
}

func writePi(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	piU1 = `{"type":"message","id":"u1","parentId":null,"timestamp":"2026-11-10T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"first"}]}}`
	piA1 = `{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-11-10T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"Q1?"}],"stopReason":"stop"}}`
	piU2 = `{"type":"message","id":"u2","parentId":"a1","timestamp":"2026-11-10T00:00:03Z","message":{"role":"user","content":"second"}}`
	piA2 = `{"type":"message","id":"a2","parentId":"u2","timestamp":"2026-11-10T00:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"Q2?"}],"stopReason":"stop"}}`
)

func TestPiLastTurnFollowsTheBranch(t *testing.T) {
	// /tree back to a1 with a summary: the session continues from a1.
	summary := `{"type":"branch_summary","id":"b1","parentId":"a1","timestamp":"2026-11-10T00:00:05Z","fromId":"a2","summary":"..."}`
	turn, ok, err := PiLastTurn(writePi(t, piLines(piU1, piA1, piU2, piA2, summary)))
	if err != nil || !ok || turn.Final != "Q1?" || turn.Prompt != "first" || turn.PromptID != "u1" {
		t.Fatalf("got %+v ok=%v err=%v", turn, ok, err)
	}
	turn, ok, _ = PiLastTurn(writePi(t, piLines(piU1, piA1, piU2, piA2)))
	if !ok || turn.Final != "Q2?" || turn.Prompt != "second" {
		t.Fatalf("straight line: %+v", turn)
	}
}

func TestPiLastTurnEndings(t *testing.T) {
	tool := `{"type":"message","id":"a2","parentId":"u2","timestamp":"2026-11-10T00:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"Let me look."},{"type":"toolCall","id":"c"}],"stopReason":"toolUse"}}`
	errored := `{"type":"message","id":"a2","parentId":"u2","timestamp":"2026-11-10T00:00:04Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"overloaded"}}`
	custom := `{"type":"custom_message","id":"c1","parentId":"a2","timestamp":"2026-11-10T00:00:05Z","customType":"x","content":"injected","display":false}`
	compaction := `{"type":"compaction","id":"k1","parentId":"a2","timestamp":"2026-11-10T00:00:05Z","summary":"s","firstKeptEntryId":"u2","tokensBefore":1}`
	for name, c := range map[string]struct {
		body  string
		ok    bool
		final string
	}{
		"ends in a tool call":      {piLines(piU1, piA1, piU2, tool), false, ""},
		"ends in an error":         {piLines(piU1, piA1, piU2, errored), false, ""},
		"user has the last word":   {piLines(piU1, piA1, piU2), false, ""},
		"custom message after":     {piLines(piU1, piA1, piU2, piA2, custom), true, "Q2?"},
		"compaction after":         {piLines(piU1, piA1, piU2, piA2, compaction), true, "Q2?"},
		"header only":              {piLines(), false, ""},
		"unreadable lines ignored": {piLines(piU1, "{nope", piA1), true, "Q1?"},
	} {
		turn, ok, err := PiLastTurn(writePi(t, c.body))
		if err != nil || ok != c.ok || turn.Final != c.final {
			t.Errorf("%s: got %+v ok=%v err=%v", name, turn, ok, err)
		}
	}
	if _, _, err := PiLastTurn(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Error("missing file: no error")
	}
}

func TestPiLaunchDir(t *testing.T) {
	p := writePi(t, piLines(piU1))
	for name, c := range map[string]struct{ path, cwd, want string }{
		"the extension's cwd": {p, "/given", "/given"},
		"the header's":        {p, "", "/p"},
		"no file":             {filepath.Join(t.TempDir(), "x.jsonl"), "", ""},
		"nothing":             {"", "", ""},
	} {
		if got := PiLaunchDir(c.path, c.cwd); got != c.want {
			t.Errorf("%s: got %q want %q", name, got, c.want)
		}
	}
}
