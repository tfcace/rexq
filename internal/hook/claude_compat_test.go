package hook

// Characterization tests: they pin how the hooks treat Claude Code's own
// hook input, which carries no agent field. They use only APIs that existed
// before Pi support, so they also run against the commit before it.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tfcace/rexq/internal/store"
	transcriptpkg "github.com/tfcace/rexq/internal/transcript"
)

// promptRunner records the prompts it is asked to run.
type promptRunner struct {
	mu      sync.Mutex
	out     string
	err     error
	prompts []string
}

func (r *promptRunner) Run(_ context.Context, prompt, _ string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, prompt)
	return []byte(r.out), r.err
}

// claudeTranscript writes a Claude transcript in the projects folder named
// for launch and returns its path.
func claudeTranscript(t *testing.T, launch string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "projects", transcriptpkg.ProjectDirName(launch), "c1.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"user","cwd":"`+launch+`"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Claude Code's real hook payloads (SessionStart, UserPromptSubmit, Stop),
// with every field it sends and no agent field, go through start, prompt and
// stop as they always did.
func TestClaudeCompatHooksEndToEnd(t *testing.T) {
	launch := filepath.Join(t.TempDir(), "code", "app")
	transcript := claudeTranscript(t, launch)
	d := deps(t, map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1", "CLAUDE_CODE_ENTRYPOINT": "cli"})
	r := &promptRunner{out: twoItems}
	d.Runner = r
	var looked []string
	d.Repo = func(dir string) (string, string) { looked = append(looked, dir); return "app", "main" }

	start := `{"session_id":"c1","transcript_path":"` + transcript + `","cwd":"` + launch + `","hook_event_name":"SessionStart","source":"startup"}`
	if err := Start(d, strings.NewReader(start)); err != nil {
		t.Fatal(err)
	}
	if owner := must(d.Store.PaneOwner("block:1")); owner != "c1" {
		t.Fatalf("pane owner %q, want c1", owner)
	}

	prompt := `{"session_id":"c1","transcript_path":"` + transcript + `","cwd":"` + launch + `","permission_mode":"default","hook_event_name":"UserPromptSubmit","prompt":"tidy the README"}`
	if err := Prompt(d, strings.NewReader(prompt)); err != nil {
		t.Fatal(err)
	}
	if p := must(d.Store.LastPrompt("c1")); p != "tidy the README" {
		t.Fatalf("prompt %q", p)
	}

	msg := "Done. Want me to commit?"
	stopIn, _ := json.Marshal(map[string]any{
		"session_id": "c1", "prompt_id": "p1", "transcript_path": transcript,
		"cwd": filepath.Join(launch, "internal"), "permission_mode": "default",
		"hook_event_name": "Stop", "stop_hook_active": false, "last_assistant_message": msg,
	})
	stopOK(t, d, string(stopIn))

	if len(r.prompts) != 1 {
		t.Fatalf("model called %d times, want 1", len(r.prompts))
	}
	for _, want := range []string{"<user_prompt>\ntidy the README\n</user_prompt>", "<agent_message>\n" + msg + "\n</agent_message>"} {
		if !strings.Contains(r.prompts[0], want) {
			t.Errorf("extraction prompt lacks %q", want)
		}
	}
	if !reflect.DeepEqual(looked, []string{launch}) {
		t.Errorf("Repo looked in %q, want the launch directory %q", looked, launch)
	}
	items := must(d.Store.List(""))
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
	first := items[0]
	if items[1].Seq == 0 {
		first = items[1]
	}
	want := store.Item{
		ID: first.ID, RexSession: "session:a", RexBlock: "block:1", ClaudeSession: "c1",
		SourceMsg: SourceMsg("p1", msg), Seq: 0, Cwd: launch, Branch: "main", Project: "app",
		Kind: store.KindDecision, Title: "Commit this?", Body: "Want me to commit?",
		Prompt: "tidy the README", Replies: []string{"Commit", "Not yet"}, Suggested: 0,
		Images: []string{}, State: store.StateOpen, CreatedAt: time.Unix(10_000, 0),
		PromptedAt: time.Unix(10_000, 0),
	}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("stored item:\n got %+v\nwant %+v", first, want)
	}
}

// A failed Claude turn is queued with exactly the payload keys it always
// had, so queued turns written before and after Pi support read the same.
func TestClaudeCompatQueuedPayload(t *testing.T) {
	launch := filepath.Join(t.TempDir(), "app")
	transcript := claudeTranscript(t, launch)
	d := deps(t, map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"})
	d.Runner = &promptRunner{err: errOffline}
	in, _ := json.Marshal(map[string]any{
		"session_id": "c1", "prompt_id": "p1", "transcript_path": transcript, "cwd": launch,
		"hook_event_name": "Stop", "last_assistant_message": "Want me to commit?",
	})
	stopFails(t, d, string(in))
	rows := must(d.Store.PendingFor("c1"))
	if len(rows) != 1 {
		t.Fatalf("%d queued", len(rows))
	}
	var got map[string]any
	if err := json.Unmarshal(rows[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wantKeys := []string{"agent_id", "cwd", "last_assistant_message", "prompt_id", "rex_block", "rex_branch",
		"rex_ended_at", "rex_have_repo", "rex_launch_dir", "rex_project", "rex_prompt", "rex_session",
		"session_id", "transcript_path"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("payload keys\n got %q\nwant %q", keys, wantKeys)
	}
	if got["rex_launch_dir"] != launch || got["rex_project"] != "proj" || got["rex_block"] != "block:1" {
		t.Errorf("payload %v", got)
	}
}

// The hooks skip the extractor and headless Claude runs, and a subagent's
// Stop, before reading anything.
func TestClaudeCompatSkips(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"extractor": {"REXQ_EXTRACTOR": "1"},
		"sdk-cli":   {"CLAUDE_CODE_ENTRYPOINT": "sdk-cli"},
		"sdk-ts":    {"CLAUDE_CODE_ENTRYPOINT": "sdk-ts"},
	} {
		d := deps(t, env)
		r := &promptRunner{out: twoItems}
		d.Runner = r
		if err := Stop(context.Background(), d, strings.NewReader("not json")); err != nil {
			t.Errorf("%s: skipped hook read its input: %v", name, err)
		}
		if len(r.prompts) != 0 {
			t.Errorf("%s: model called", name)
		}
	}
	d := deps(t, nil)
	r := &promptRunner{out: twoItems}
	d.Runner = r
	stopOK(t, d, `{"session_id":"c1","agent_id":"sub-1","last_assistant_message":"Want me to commit?"}`)
	if len(r.prompts) != 0 || len(must(d.Store.List(""))) != 0 {
		t.Error("a subagent's stop was extracted")
	}
}
