package hook

// Characterization tests through the agent-aware hooks: Claude Code's hook
// input, which has no agent field, is Claude's from start to stop. They
// touch no Pi code paths.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tfcace/rexq/internal/agent"
	"github.com/tfcace/rexq/internal/extract"
)

func TestClaudeAgentInputWithoutAgentIsClaude(t *testing.T) {
	d := deps(t, map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"})
	r := &promptRunner{out: twoItems}
	var asked []string
	d.RunnerFor = func(a agent.Agent, c extract.ModelChoice) extract.Runner {
		asked = append(asked, a.Name)
		if c != (extract.ModelChoice{}) {
			t.Errorf("a Claude turn carried a model choice: %+v", c)
		}
		return r
	}
	claim(t, d, "c1")
	if err := Prompt(d, strings.NewReader(`{"session_id":"c1","prompt":"recorded"}`)); err != nil {
		t.Fatal(err)
	}
	// A "prompt" key in a Claude stop, should Claude Code ever send one, is
	// not the turn's prompt: Claude's comes from the prompt hook.
	in, _ := json.Marshal(map[string]any{"session_id": "c1", "prompt_id": "p1", "cwd": "/tmp/proj",
		"last_assistant_message": "Want me to commit?", "prompt": "not this"})
	stopOK(t, d, string(in))
	if strings.Join(asked, ",") != "Claude" {
		t.Errorf("runner asked for %q", asked)
	}
	if !strings.Contains(r.prompts[0], "<user_prompt>\nrecorded\n</user_prompt>") {
		t.Errorf("prompt %q", r.prompts[0])
	}
	for _, it := range must(d.Store.List("")) {
		if it.Agent != agent.Claude.ID || it.Agent != "" {
			t.Errorf("item agent %q", it.Agent)
		}
	}
}
