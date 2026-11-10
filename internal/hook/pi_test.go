package hook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tfcace/rexq/internal/agent"
	"github.com/tfcace/rexq/internal/extract"
)

// piStop is the stop input rexq's Pi extension sends.
func piStop(session, promptID, prompt, msg string) string {
	b, _ := json.Marshal(map[string]any{
		"agent": "pi", "session_id": session, "prompt_id": promptID, "cwd": "/code/app",
		"transcript_path": "/nonexistent/pi/session.jsonl", "last_assistant_message": msg,
		"prompt": prompt, "extract_model": "", "provider": "anthropic", "model": "claude-opus-5",
	})
	return string(b)
}

// piDeps is deps whose Pi turns go to r, recording the model choice.
func piDeps(t *testing.T, env map[string]string, r extract.Runner) (Deps, *[]extract.ModelChoice) {
	d := deps(t, env)
	var choices []extract.ModelChoice
	claude := &promptRunner{err: errOffline}
	d.Runner = claude
	d.RunnerFor = func(a agent.Agent, c extract.ModelChoice) extract.Runner {
		if a.ID != agent.Pi.ID {
			t.Errorf("runner asked for %q", a.ID)
		}
		choices = append(choices, c)
		return r
	}
	return d, &choices
}

func TestPiHooksEndToEnd(t *testing.T) {
	r := &promptRunner{out: twoItems}
	d, choices := piDeps(t, map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:7", extract.PiModelEnv: "openai/gpt-5-nano"}, r)
	if err := Start(d, strings.NewReader(`{"agent":"pi","session_id":"01a1-pi"}`)); err != nil {
		t.Fatal(err)
	}
	if err := Prompt(d, strings.NewReader(`{"agent":"pi","session_id":"01a1-pi","prompt":"typed earlier"}`)); err != nil {
		t.Fatal(err)
	}
	stopOK(t, d, piStop("01a1-pi", "e1a4472c", "which file?", "Which file should I open?"))
	if len(r.prompts) != 1 || !strings.Contains(r.prompts[0], "<user_prompt>\nwhich file?\n</user_prompt>") {
		t.Fatalf("prompts %q", r.prompts)
	}
	want := extract.ModelChoice{Env: "openai/gpt-5-nano", Provider: "anthropic", Session: "claude-opus-5"}
	if len(*choices) != 1 || (*choices)[0] != want {
		t.Errorf("model choice %+v", *choices)
	}
	items := must(d.Store.List(""))
	if len(items) != 2 {
		t.Fatalf("%d items", len(items))
	}
	for _, it := range items {
		if it.Agent != "pi" || it.RexBlock != "block:7" || it.Cwd != "/code/app" || it.Prompt != "which file?" ||
			it.SourceMsg != SourceMsg("e1a4472c", "Which file should I open?") {
			t.Errorf("item %+v", it)
		}
	}
}

// A Pi turn that fails is queued with everything a retry needs, and the
// retry never reads the session file.
func TestPiQueuedTurnRetries(t *testing.T) {
	r := &promptRunner{err: errOffline}
	d, choices := piDeps(t, nil, r)
	stopFails(t, d, piStop("01a1-pi", "u1", "go on", "Want me to push?"))
	rows := must(d.Store.PendingFor("01a1-pi"))
	if len(rows) != 1 {
		t.Fatalf("%d queued", len(rows))
	}
	var payload map[string]any
	json.Unmarshal(rows[0].Payload, &payload)
	for k, v := range map[string]any{"agent": "pi", "prompt": "go on", "rex_prompt": "go on", "provider": "anthropic",
		"model": "claude-opus-5", "rex_launch_dir": "/code/app"} {
		if payload[k] != v {
			t.Errorf("payload %s = %v, want %v", k, payload[k], v)
		}
	}
	h := must(d.Store.Health())
	if !strings.Contains(h.LastError, "offline") || !h.Failing() {
		t.Errorf("health %+v", h)
	}
	r.err, r.out = nil, twoItems
	d.Now = later(d)
	stopOK(t, d, piStop("01a1-other", "u9", "", "Done."))
	if n := len(must(d.Store.List(""))); n != 2 {
		t.Fatalf("retried items: %d", n)
	}
	if len(*choices) < 2 || (*choices)[1].Session != "claude-opus-5" {
		t.Errorf("retry model choice %+v", *choices)
	}
	if must(d.Store.Health()).Failing() {
		t.Error("health not cleared")
	}
}

// Unknown agents are refused, not taken for Claude.
func TestHooksRefuseUnknownAgents(t *testing.T) {
	d := deps(t, map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"})
	d.Runner = &promptRunner{out: twoItems}
	in := `{"agent":"codex","session_id":"x","prompt":"p","last_assistant_message":"Want me to commit?"}`
	if err := Start(d, strings.NewReader(in)); err == nil {
		t.Error("start took it")
	}
	if err := Prompt(d, strings.NewReader(in)); err == nil {
		t.Error("prompt took it")
	}
	if err := stop(d, in); err == nil {
		t.Error("stop took it")
	}
	if owner := must(d.Store.PaneOwner("block:1")); owner != "" || len(must(d.Store.List(""))) != 0 {
		t.Error("an unknown agent changed the store")
	}
}

// Without RunnerFor, a Pi turn gets Pi's own runner, never Deps.Runner.
func TestPiTurnsDontUseTheClaudeRunner(t *testing.T) {
	d := deps(t, nil)
	claude := &promptRunner{out: twoItems}
	d.Runner = claude
	none := func(string) string { return "" }
	if r, ok := d.runner(agent.Pi, extract.ModelChoice{Provider: "anthropic", Session: "m"}, none).(extract.Pi); !ok ||
		len(r.Models) != 2 || r.Models[0] != "anthropic/claude-haiku-5-5" || r.Bin != "pi" {
		t.Fatalf("runner %#v", r)
	}
	pinned := func(k string) string { return map[string]string{extract.PiEnv: "/opt/pi"}[k] }
	if r := d.runner(agent.Pi, extract.ModelChoice{}, pinned).(extract.Pi); r.Bin != "/opt/pi" {
		t.Fatalf("REXQ_PI not used: %#v", r)
	}
	if d.runner(agent.Claude, extract.ModelChoice{}, none) != extract.Runner(claude) {
		t.Fatal("Claude's runner is not Deps.Runner")
	}
	d.Runner = nil
	if _, ok := d.runner(agent.Claude, extract.ModelChoice{}, none).(extract.Claude); !ok {
		t.Fatal("Claude's default runner is not claude -p")
	}
}
