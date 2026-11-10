package agent

import (
	"reflect"
	"testing"
)

// Claude's entry is what rexq did before there were agents.
func TestClaudeEntry(t *testing.T) {
	c := Claude
	if c.ID != "" || c.Name != "Claude" || c.Command != "claude" || c.StatusApp != "claude-code" || c.StopSendsPrompt {
		t.Errorf("claude %+v", c)
	}
	if got := c.ResumeArgv("abc", "-yes"); !reflect.DeepEqual(got, []string{"claude", "--resume", "abc", "--", "-yes"}) {
		t.Errorf("argv %q", got)
	}
	if got := c.ResumeHint("abc"); got != "claude --resume abc" {
		t.Errorf("hint %q", got)
	}
	for state, want := range map[string]bool{"idle": true, "done": true, "error": false, "working": false, "blocked": false, "": false} {
		if c.Ready(state) != want {
			t.Errorf("ready(%q) = %v", state, !want)
		}
	}
	if !c.SameProject("/p/app-demo", "/p/app/demo") || c.SameProject("/p/a", "/p/b") {
		t.Error("same project")
	}
	if !c.Throwaway("-private-tmp-x") || c.Throwaway("-Users-me-x") {
		t.Error("throwaway")
	}
}

func TestPiEntry(t *testing.T) {
	p := Pi
	if got := p.ResumeArgv("01a1", ""); !reflect.DeepEqual(got, []string{"pi", "--session", "01a1"}) {
		t.Errorf("argv %q", got)
	}
	if got := p.ResumeArgv("01a1", "-yes"); !reflect.DeepEqual(got, []string{"pi", "--session", "01a1", "--", "-yes"}) {
		t.Errorf("argv %q", got)
	}
	if !p.Ready("error") || p.Ready("working") {
		t.Error("ready")
	}
	for comm, want := range map[string]bool{"pi": true, "/usr/local/bin/pi": true, "pi-rpc": false, "node": false, "claude": false} {
		if p.IsProcess(comm) != want {
			t.Errorf("%q: %v", comm, !want)
		}
	}
	if p.SameProject("/p/app-demo", "/p/app/demo") || !p.SameProject("/p/a", "/p/a") {
		t.Error("same project")
	}
}

func TestByID(t *testing.T) {
	for id, want := range map[string]string{"": "Claude", "pi": "Pi"} {
		if a, ok := ByID(id); !ok || a.Name != want {
			t.Errorf("%q: %v %v", id, a.Name, ok)
		}
	}
	if _, ok := ByID("codex"); ok {
		t.Error("unknown agent found")
	}
}
