package hook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tfcace/rexq/internal/agent"
	"github.com/tfcace/rexq/internal/extract"
)

// A queued Pi turn is retried with the model override of the environment
// it ended in, not that of the hook that retries it.
func TestPiRetryUsesTheEnvironmentOfItsTurn(t *testing.T) {
	env := map[string]string{extract.PiModelEnv: "openai/gpt-5-nano"}
	r := &promptRunner{err: errOffline}
	d, choices := piDeps(t, env, r)
	stopFails(t, d, piStop("pi-a", "u1", "go on", "Want me to push?"))

	// Another Pi session, started without the override, retries it.
	delete(env, extract.PiModelEnv)
	r.err, r.out = nil, noItems
	d.Now = later(d)
	stopOK(t, d, piStop("pi-b", "u9", "", "Want me to tag it?"))
	if len(*choices) != 3 {
		t.Fatalf("%d runner calls, want the failed turn, the live turn and the retry", len(*choices))
	}
	// A hook runs its live turn before other sessions' queued ones.
	if got := (*choices)[1].Env; got != "" {
		t.Errorf("live turn ran with REXQ_PI_MODEL=%q, want its own (unset)", got)
	}
	if got := (*choices)[2].Env; got != "openai/gpt-5-nano" {
		t.Errorf("retry ran with REXQ_PI_MODEL=%q, want the turn's own", got)
	}
}

// A hook retries only its own agent's queued turns: the other agent's need
// that agent's environment.
func TestStopRetriesOnlyItsAgentsTurns(t *testing.T) {
	r := &promptRunner{err: errOffline}
	d, _ := piDeps(t, nil, r)
	stopFails(t, d, piStop("pi-a", "u1", "go on", "Want me to push?"))
	claude := &promptRunner{err: errOffline}
	d.RunnerFor = func(a agent.Agent, c extract.ModelChoice) extract.Runner {
		if a.ID == agent.Pi.ID {
			return r
		}
		return claude
	}
	stopFails(t, d, turn("c-1", "p1", "Want me to commit?"))
	if n := queued(d, "pi-a", "c-1"); n != 2 {
		t.Fatalf("%d queued", n)
	}

	r.err, r.out = nil, noItems
	claude.err, claude.out = nil, noItems
	d.Now = later(d)
	// A Claude stop leaves the Pi turn queued.
	stopOK(t, d, turn("c-2", "p2", "Done."))
	if n := queued(d, "pi-a"); n != 1 {
		t.Errorf("a Claude hook retried a Pi turn")
	}
	if n := queued(d, "c-1"); n != 0 {
		t.Errorf("a Claude hook skipped a Claude turn")
	}
	// And a Pi stop leaves Claude turns alone.
	claude.err = errOffline
	stopFails(t, d, turn("c-3", "p3", "Want me to merge?"))
	d.Now = later(d)
	stopOK(t, d, piStop("pi-b", "u9", "", "Done."))
	if n := queued(d, "c-3"); n != 1 {
		t.Errorf("a Pi hook retried a Claude turn")
	}
	if n := queued(d, "pi-a"); n != 0 {
		t.Errorf("a Pi hook skipped a Pi turn")
	}
}

// A queued turn of an agent this rexq doesn't know is left queued, with a
// note, and never extracted as Claude's.
func TestRetryLeavesUnknownAgentsAlone(t *testing.T) {
	d := deps(t, nil)
	r := &promptRunner{out: twoItems}
	d.Runner = r
	payload, _ := json.Marshal(map[string]any{"agent": "codex", "session_id": "x-1",
		"last_assistant_message": "Want me to commit?"})
	if err := d.Store.Enqueue("x-1", payload, d.Now()); err != nil {
		t.Fatal(err)
	}
	d.Now = later(d)
	err := stop(d, turn("c-1", "p1", "Done."))
	if err == nil || !strings.Contains(err.Error(), `unknown agent "codex"`) {
		t.Errorf("no note: %v", err)
	}
	if len(r.prompts) != 0 {
		t.Errorf("extracted the unknown agent's turn: %d calls", len(r.prompts))
	}
	rows := must(d.Store.PendingFor("x-1"))
	if len(rows) != 1 {
		t.Fatalf("row gone: %d", len(rows))
	}
	if len(must(d.Store.List(""))) != 0 {
		t.Error("stored items")
	}
	// Not even its own session's stop runs it.
	err = stop(d, `{"agent":"codex","session_id":"x-1","last_assistant_message":"x?"}`)
	if len(r.prompts) != 0 || len(must(d.Store.PendingFor("x-1"))) != 1 {
		t.Errorf("ran it: %v", err)
	}
}
