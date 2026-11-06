package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLastTurn(t *testing.T) {
	turn, ok, err := LastTurn("testdata/session.jsonl")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if turn.Prompt != "keep going" {
		t.Errorf("prompt %q (a tool result is not a prompt)", turn.Prompt)
	}
	if turn.Final != "Done. Should I commit this?" {
		t.Errorf("final %q (text before the last tool call must not count)", turn.Final)
	}
	if turn.MsgUUID != "a3" || turn.SessionID != "s1" || turn.Branch != "main" || turn.PromptID != "p1" {
		t.Errorf("metadata %+v", turn)
	}
}

// Real transcripts write each content block of one API message as its own
// entry (thinking, text, tool_use), all with the same message id, and
// interleave entries of other types.
func TestLastTurnJoinsAMessageSplitAcrossEntries(t *testing.T) {
	turn, ok, err := LastTurn("testdata/split.jsonl")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if turn.Final != "I tidied it.\n\nWant me to push?" {
		t.Errorf("final %q", turn.Final)
	}
	want := time.Date(2026, 10, 15, 13, 2, 2, 0, time.UTC)
	if turn.MsgUUID != "a6" || !turn.Time.Equal(want) || turn.Cwd != "/p" || turn.Entrypoint != "cli" {
		t.Errorf("metadata %+v", turn)
	}
	if turn.Prompt != "tidy the README" || turn.SessionID != "s2" || turn.PromptID != "p1" {
		t.Errorf("prompt %q session %q prompt id %q", turn.Prompt, turn.SessionID, turn.PromptID)
	}
}

// A session that ends in a tool call was still working when it ended, even
// if the same message held text before the call.
func TestLastTurnEndingInAToolCallHasNone(t *testing.T) {
	_, ok, err := LastTurn("testdata/toolcall.jsonl")
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v, want no turn", ok, err)
	}
}

// A compaction summary and meta entries are not the user's prompt, and a
// task notification starts no turn. The final message is the session's last
// one, as the Stop hook would have seen it, and so is the prompt id: the
// notification's.
func TestLastTurnSkipsCompactionAndInjectedEntries(t *testing.T) {
	turn, ok, err := LastTurn("testdata/compact.jsonl")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if turn.Prompt != "port the parser" || turn.PromptID != "p2" {
		t.Errorf("prompt %q id %q", turn.Prompt, turn.PromptID)
	}
	if turn.Final != "The background build passed. Still waiting on you: delete the old parser?" || turn.MsgUUID != "a3" {
		t.Errorf("final %q from %s", turn.Final, turn.MsgUUID)
	}
}

// The user interrupting the agent is an answer of sorts: nothing is left
// hanging.
func TestLastTurnInterruptedHasNone(t *testing.T) {
	_, ok, err := LastTurn("testdata/interrupted.jsonl")
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v, want no turn", ok, err)
	}
}

// Claude Code writes a synthetic "No response requested." when a session is
// resumed; it must not hide the real last message. Local commands start no
// turn, a prompt with an image keeps its text, and a line that isn't JSON
// is skipped.
func TestLastTurnIgnoresSyntheticMessages(t *testing.T) {
	turn, ok, err := LastTurn("testdata/synthetic.jsonl")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if turn.Final != "Not quite. Do you want me to fix it?" || turn.MsgUUID != "a1" {
		t.Errorf("final %q from %s", turn.Final, turn.MsgUUID)
	}
	if turn.Prompt != "[Image #1] is this aligned?" || turn.Entrypoint != "sdk-cli" || turn.PromptID != "p1" {
		t.Errorf("prompt %q entrypoint %q prompt id %q", turn.Prompt, turn.Entrypoint, turn.PromptID)
	}
}

// Local commands (/model, /exit) and ! shell commands get no answer from the
// agent, so they leave its question standing, with its prompt id.
func TestLastTurnLocalCommandsLeaveTheTurn(t *testing.T) {
	turn, ok, err := LastTurn("testdata/localcmd.jsonl")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if turn.Final != "Drafted. Should I tag v0.2.0?" || turn.Prompt != "draft the release notes" || turn.PromptID != "p1" {
		t.Errorf("turn %+v", turn)
	}
}

// A slash command the agent answers is the turn's prompt: its name and
// arguments, and its prompt id.
func TestLastTurnSlashCommandPrompt(t *testing.T) {
	turn, ok, err := LastTurn("testdata/skillcmd.jsonl")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if turn.Prompt != "/code-critic review the parser\nfor speed" || turn.PromptID != "p2" {
		t.Errorf("prompt %q id %q", turn.Prompt, turn.PromptID)
	}
	if turn.Final != "The parser allocates per token. Want me to fix that?" || turn.MsgUUID != "a3" {
		t.Errorf("final %q from %s", turn.Final, turn.MsgUUID)
	}
}

func TestLastTurnEmptyAndMissing(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "e.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LastTurn(empty); ok || err != nil {
		t.Errorf("empty: ok=%v err=%v", ok, err)
	}
	if _, _, err := LastTurn(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Error("missing file: no error")
	}
}

// Real transcripts hold lines of a few MB (tool results, request blobs).
func TestLastTurnReadsLongLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.jsonl")
	long := `{"type":"user","sessionId":"s7","uuid":"u1","timestamp":"2026-10-15T13:00:00Z","message":{"role":"user","content":"` + strings.Repeat("x", 3<<20) + `"}}` + "\n" +
		`{"type":"assistant","sessionId":"s7","uuid":"a1","timestamp":"2026-10-15T13:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Ready?"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	turn, ok, err := LastTurn(path)
	if err != nil || !ok || turn.Final != "Ready?" || len(turn.Prompt) != 3<<20 {
		t.Errorf("ok=%v err=%v final %q prompt %d bytes", ok, err, turn.Final, len(turn.Prompt))
	}
}

// Claude Code names a launch directory's folder by turning every character
// but ASCII letters and digits into "-". Checked against real folders in
// ~/.claude/projects, 2026-11-06.
func TestProjectDirName(t *testing.T) {
	cases := map[string]string{
		"/Users/me/projects/rex-claude-tracker":         "-Users-me-projects-rex-claude-tracker",
		"/private/var/folders/d2/T/tmp.Ab3dEf9hIj":      "-private-var-folders-d2-T-tmp-Ab3dEf9hIj",
		"/Users/me/Library/Application Support/x":       "-Users-me-Library-Application-Support-x",
		"/Users/me/projects/worktrees/example__project": "-Users-me-projects-worktrees-example--project",
		"/tmp/agent-1/-Users-me-x":                      "-tmp-agent-1--Users-me-x",
	}
	for dir, want := range cases {
		if got := ProjectDirName(dir); got != want {
			t.Errorf("%s: %q, want %q", dir, got, want)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchDir(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "work", "my.repo")
	other := filepath.Join(root, "work", "other")
	projects := filepath.Join(root, "projects")
	inRepo := filepath.Join(projects, ProjectDirName(repo), "s1.jsonl")
	// The first entries carry no cwd, as in real transcripts.
	writeFile(t, inRepo, `{"type":"last-prompt"}`+"\n"+`{"type":"mode"}`+"\n"+
		`{"type":"user","cwd":"`+repo+`","sessionId":"s1"}`+"\n"+
		`{"type":"user","cwd":"`+other+`","sessionId":"s1"}`+"\n")

	cases := []struct {
		name, transcript, cwd, want string
	}{
		{"started here", inRepo, repo, repo},
		{"cd'd into a subdirectory", inRepo, filepath.Join(repo, "internal", "x"), repo},
		{"moved into another repo", inRepo, other, repo},
		{"no transcript file, no match", filepath.Join(projects, "-elsewhere", "s2.jsonl"), other, other},
		{"no transcript path", "", filepath.Join(repo, "sub"), filepath.Join(repo, "sub")},
	}
	for _, c := range cases {
		if got := LaunchDir(c.transcript, c.cwd); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// Only the head of a transcript is read for the launch directory.
func TestFirstCwdReadsOnlyTheStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, path, strings.Repeat(`{"type":"mode"}`+"\n", firstCwdLines)+`{"type":"user","cwd":"/late"}`+"\n")
	if got := FirstCwd(path); got != "" {
		t.Errorf("read past the head: %q", got)
	}
}
