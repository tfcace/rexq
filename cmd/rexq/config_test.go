package main

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func rexqConfig(t *testing.T, db string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(rexqBin, append([]string{"config"}, args...)...)
	cmd.Env = append([]string{"REXQ_DB=" + db, "HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestConfigPiModel(t *testing.T) {
	db := filepath.Join(t.TempDir(), "q.db")
	if out, err := rexqConfig(t, db, nil, "get", "pi-model"); err != nil || out != "\n" {
		t.Fatalf("unset: %q %v", out, err)
	}
	if out, err := rexqConfig(t, db, nil, "set", "pi-model", "openai/gpt-5-mini"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if out, _ := rexqConfig(t, db, nil, "get", "pi-model"); out != "openai/gpt-5-mini\n" {
		t.Fatalf("got %q", out)
	}
	out, err := rexqConfig(t, db, []string{"REXQ_PI_MODEL=google/gemini-2.5-flash"}, "get", "pi-model", "--json",
		"--provider", "anthropic", "--session-model", "claude-opus-5")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := map[string]string{"pi-model": "openai/gpt-5-mini", "automatic": "anthropic/claude-haiku-5-5",
		"override": "google/gemini-2.5-flash", "effective": "google/gemini-2.5-flash"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all %v)", k, got[k], v, got)
		}
	}
	for _, auto := range []string{"auto", "automatic", ""} {
		rexqConfig(t, db, nil, "set", "pi-model", "x/y")
		if out, err := rexqConfig(t, db, nil, "set", "pi-model", auto); err != nil {
			t.Fatalf("%q: %v: %s", auto, err, out)
		}
		if out, _ := rexqConfig(t, db, nil, "get", "pi-model"); out != "\n" {
			t.Errorf("%q didn't clear: %q", auto, out)
		}
	}
	out, _ = rexqConfig(t, db, nil, "get", "pi-model", "--json", "--provider", "anthropic", "--session-model", "claude-opus-5")
	json.Unmarshal([]byte(out), &got)
	if got["effective"] != "anthropic/claude-haiku-5-5" || got["pi-model"] != "" {
		t.Errorf("automatic: %v", got)
	}
	for _, bad := range [][]string{{"set", "pi-model", "haiku"}, {"set", "pi-model", "a/b c"}, {"set", "nope", "x"}, {"get", "nope"}, {"set", "pi-model"}, {"frob"}} {
		if out, err := rexqConfig(t, db, nil, bad...); err == nil {
			t.Errorf("%q taken: %s", bad, out)
		} else if strings.Contains(out, "panic") {
			t.Errorf("%q: %s", bad, out)
		}
	}
}

// config ask claims the first-run picker once per store; set and dismiss
// answer it.
func TestConfigAsk(t *testing.T) {
	db := filepath.Join(t.TempDir(), "q.db")
	ask := func() map[string]any {
		t.Helper()
		out, err := rexqConfig(t, db, nil, "ask", "pi-model", "--provider", "anthropic", "--session-model", "claude-opus-5")
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return got
	}
	state := func() string {
		out, _ := rexqConfig(t, db, nil, "get", "pi-model", "--json")
		var got map[string]any
		json.Unmarshal([]byte(out), &got)
		s, _ := got["asked"].(string)
		return s
	}
	if s := state(); s != "never" {
		t.Fatalf("new store: %q", s)
	}
	first := ask()
	if first["ask"] != true || first["automatic"] != "anthropic/claude-haiku-5-5" {
		t.Fatalf("first: %v", first)
	}
	if second := ask(); second["ask"] != false {
		t.Fatalf("asked twice: %v", second)
	}
	if s := state(); s != "open" {
		t.Errorf("while open: %q", s)
	}
	if out, err := rexqConfig(t, db, nil, "dismiss", "pi-model"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if s := state(); s != "done" {
		t.Errorf("dismissed: %q", s)
	}

	// A model set from outside Pi counts as the choice.
	db = filepath.Join(t.TempDir(), "q.db")
	rexqConfig(t, db, nil, "set", "pi-model", "openai/gpt-5-mini")
	if got := ask(); got["ask"] != false {
		t.Errorf("asked after config set: %v", got)
	}
	db = filepath.Join(t.TempDir(), "q.db")
	rexqConfig(t, db, nil, "set", "pi-model", "auto")
	if got := ask(); got["ask"] != false {
		t.Errorf("asked after choosing auto: %v", got)
	}
}
