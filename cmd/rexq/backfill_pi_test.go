package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tfcace/rexq/internal/agent"
	"github.com/tfcace/rexq/internal/extract"
	"github.com/tfcace/rexq/internal/hook"
	"github.com/tfcace/rexq/internal/store"
)

// writePiSession writes a one-turn Pi session file under root, in folder.
func writePiSession(t *testing.T, root, folder, id, cwd string, ended time.Time, final string) {
	t.Helper()
	ts := func(d time.Duration) string { return ended.Add(d).UTC().Format(time.RFC3339) }
	lines := []string{
		fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":%q,"cwd":%q}`, id, ts(-time.Minute), cwd),
		fmt.Sprintf(`{"type":"message","id":"u-%s","parentId":null,"timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":"fix the bug"}]}}`, id, ts(-time.Second)),
		fmt.Sprintf(`{"type":"message","id":"a-%s","parentId":"u-%s","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":%q}],"provider":"anthropic","model":"claude-opus-5","stopReason":"stop"}}`, id, id, ts(0), final),
	}
	p := filepath.Join(root, folder, "2026-11-06_"+id+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, ended, ended)
}

func TestBackfillPi(t *testing.T) {
	root := t.TempDir()
	s, err := store.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recent := backfillNow.Add(-time.Hour)
	writePiSession(t, root, "--work-app--", "pi-1", "/work/app", recent, "Should I commit this?")
	writePiSession(t, root, "--private-tmp-x--", "pi-tmp", "/private/tmp/x", recent, "Should I commit this?")
	writePiSession(t, root, "--work-old--", "pi-old", "/work/old", backfillNow.Add(-30*24*time.Hour), "Should I commit this?")
	writePiSession(t, root, "--work-quiet--", "pi-quiet", "/work/quiet", recent, "Done.")
	r := &fakeRunner{}
	if err := s.SetSetting("pi-model", "openai/gpt-5-mini"); err != nil {
		t.Fatal(err)
	}
	var choices []extract.ModelChoice
	var out bytes.Buffer
	d := backfillDeps{Root: root, Store: s, Agent: agent.Pi,
		RunnerFor: func(c extract.ModelChoice) extract.Runner { choices = append(choices, c); return r },
		Repo:      func(dir string) (string, string) { return filepath.Base(dir), "main" },
		Now:       func() time.Time { return backfillNow }, Out: &out}

	if err := backfill(d, 7*24*time.Hour, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 session to extract, one pi -p call each") || !strings.Contains(out.String(), "skipped --private-tmp-x-- (temp dir)") {
		t.Fatalf("dry run:\n%s", out.String())
	}
	if err := backfill(d, 7*24*time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 || len(choices) != 1 || choices[0] != (extract.ModelChoice{Configured: "openai/gpt-5-mini", Provider: "anthropic", Session: "claude-opus-5"}) {
		t.Fatalf("calls %d choices %+v", r.calls, choices)
	}
	items, _ := s.List("")
	if len(items) != 2 {
		t.Fatalf("%d items", len(items))
	}
	for _, it := range items {
		if it.Agent != "pi" || it.ClaudeSession != "pi-1" || it.Cwd != "/work/app" || it.Project != "app" ||
			it.SourceMsg != hook.SourceMsg("u-pi-1", "Should I commit this?") {
			t.Errorf("item %+v", it)
		}
	}
	// A second run finds the session known.
	r.calls = 0
	if err := backfill(d, 7*24*time.Hour, false); err != nil || r.calls != 0 {
		t.Fatalf("second run: %v, %d calls", err, r.calls)
	}
}

func TestPiDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if d, _ := piSessionsDir(getenv); d != filepath.Join(home, ".pi", "agent", "sessions") {
		t.Errorf("default %q", d)
	}
	env["PI_CODING_AGENT_DIR"] = "/x/agent"
	if d, _ := piSessionsDir(getenv); d != "/x/agent/sessions" {
		t.Errorf("agent dir %q", d)
	}
	env["PI_CODING_AGENT_SESSION_DIR"] = "/y/s"
	if d, _ := piSessionsDir(getenv); d != "/y/s" {
		t.Errorf("session dir %q", d)
	}
}
