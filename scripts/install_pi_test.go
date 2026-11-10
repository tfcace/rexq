package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func installPi(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("python3", append([]string{"install-hooks.py", "--pi"}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// recordingRexq writes a rexq that passes the installer's check and
// appends each command line it gets to calls.
func recordingRexq(t *testing.T, dir string) (bin, calls string) {
	bin = filepath.Join(dir, "bin", "rexq")
	calls = filepath.Join(dir, "calls")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$*\" >> \""+calls+"\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func TestInstallPi(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	bin, calls := recordingRexq(t, dir)
	ext := filepath.Join(dir, "agent", "extensions", "rexq.ts")
	env := []string{"PI_CODING_AGENT_DIR=" + filepath.Join(dir, "agent"), "HOME=" + dir}
	configCalls := func() []string {
		b, _ := os.ReadFile(calls)
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.HasPrefix(l, "config ") {
				out = append(out, l)
			}
		}
		return out
	}

	out, err := installPi(t, env, "--model", "openrouter/anthropic/claude-haiku-5.5", "", bin)
	if err == nil || !strings.Contains(out, "not a file path") {
		t.Fatalf("an empty extension path was taken: %s", out)
	}
	if leftovers, _ := filepath.Glob("*.tmp"); len(leftovers) > 0 {
		t.Fatalf("left %q", leftovers)
	}
	out, err = installPi(t, append(env, "PATH="+filepath.Dir(bin)+":"+os.Getenv("PATH")), "--model", "openrouter/anthropic/claude-haiku-5.5")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, want := range []string{"installed rexq's Pi extension in " + ext, "Pi extractions use openrouter/anthropic/claude-haiku-5.5",
		"Every Pi extraction is a pi -p call, billed to the provider", "/rexq-model"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if got := configCalls(); len(got) != 1 || got[0] != "config set pi-model openrouter/anthropic/claude-haiku-5.5" {
		t.Errorf("config calls %q", got)
	}
	src, err := os.ReadFile(ext)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(src), "// rexq Pi extension: installed by scripts/install-hooks.py --pi.\n") ||
		!strings.Contains(string(src), "\nconst REXQ_BIN = \""+bin+"\";\n") {
		t.Error("installed extension lacks the marker or the binary")
	}
	if strings.Contains(string(src), "EXTRACT_MODEL") || strings.Contains(string(src), "claude-haiku-5.5") {
		t.Error("the model is baked into the extension")
	}
	repo, _ := os.ReadFile("../pi/rexq.ts")
	if strings.Count(string(src), "\n") != strings.Count(string(repo), "\n") {
		t.Error("the installed extension is not the repository's with one line changed")
	}

	// Again with another model only changes the setting; with auto it
	// clears it; with none it leaves it alone.
	if out, err := installPi(t, env, "--model=openai/gpt-5-mini", ext, bin); err != nil || !strings.Contains(out, "already has") {
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := installPi(t, env, "--model", "auto", ext, bin); err != nil || !strings.Contains(out, "automatic") {
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := installPi(t, env, ext, bin); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := []string{"config set pi-model openrouter/anthropic/claude-haiku-5.5", "config set pi-model openai/gpt-5-mini", "config set pi-model auto"}
	if got := configCalls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("config calls %q", got)
	}

	// Values that aren't provider/model are refused before anything runs.
	before, _ := os.ReadFile(ext)
	for _, bad := range []string{"haiku", "anthropic/", "/haiku", "anthropic/claude haiku", `a/b"c`, `a/b\c`, "a b/c", "-x/y", "anthropic/claude-haiku-5-5\n"} {
		out, err := installPi(t, env, "--model", bad, ext, bin)
		if err == nil || !strings.Contains(out, "is not provider/model") {
			t.Errorf("%q: %v %s", bad, err, out)
		}
	}
	if after, _ := os.ReadFile(ext); string(after) != string(before) || len(configCalls()) != 3 {
		t.Error("a refused install changed something")
	}
	if out, err := installPi(t, env, "--model"); err == nil || !strings.Contains(out, "needs a value") {
		t.Errorf("--model without a value: %v %s", err, out)
	}

	// A failing rexq config fails the install loudly.
	failing := filepath.Join(dir, "failing-rexq")
	os.WriteFile(failing, []byte("#!/bin/sh\n[ \"$1\" = config ] && { echo 'store is broken' >&2; exit 1; }\nexit 0\n"), 0o755)
	if out, err := installPi(t, env, "--model", "openai/gpt-5-mini", ext, failing); err == nil || !strings.Contains(out, "store is broken") {
		t.Errorf("failing config: %v %s", err, out)
	}

	// A broken binary is refused.
	if out, err := installPi(t, env, ext, filepath.Join(dir, "nope")); err == nil || !strings.Contains(out, "missing or not executable") {
		t.Errorf("missing binary: %v %s", err, out)
	}

	// Someone else's rexq.ts is left alone, by install and uninstall.
	foreign := filepath.Join(dir, "other", "rexq.ts")
	os.MkdirAll(filepath.Dir(foreign), 0o755)
	os.WriteFile(foreign, []byte("// mine\n"), 0o644)
	if out, err := installPi(t, env, foreign, bin); err == nil || !strings.Contains(out, "isn't rexq's") {
		t.Errorf("overwrote a foreign file: %v %s", err, out)
	}
	if out, err := installPi(t, env, "--uninstall", foreign); err == nil || !strings.Contains(out, "leaving it alone") {
		t.Errorf("removed a foreign file: %v %s", err, out)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "// mine\n" {
		t.Error("foreign file changed")
	}

	// Uninstall removes ours, from the default place.
	if out, err := installPi(t, env, "--uninstall"); err != nil || !strings.Contains(out, "removed") {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Stat(ext); !os.IsNotExist(err) {
		t.Error("still there")
	}
	if out, err := installPi(t, env, "--uninstall"); err != nil || !strings.Contains(out, "isn't installed") {
		t.Errorf("second uninstall: %v %s", err, out)
	}
}

// The installed extension, its binary pointing at nothing, loads and runs
// every handler without throwing or waiting.
func TestInstalledExtensionWithAMissingBinaryLoads(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	dir := t.TempDir()
	bin := fakeRexq(t, dir)
	ext := filepath.Join(dir, "rexq.ts")
	if out, err := installPi(t, nil, ext, bin); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	os.Remove(bin) // the binary goes away after the install
	script := `
const mod = await import(process.argv[1]);
const handlers = new Map();
const { execFile } = await import("node:child_process");
mod.default({ on: (n, h) => handlers.set(n, h), registerCommand: () => {},
  exec: (cmd, args) => new Promise((resolve) => execFile(cmd, args, (err, stdout, stderr) =>
    resolve({ stdout, stderr: stderr || String(err?.message ?? ""), code: err ? 1 : 0 }))) });
const sm = { getSessionId: () => "s", getCwd: () => "/c", getSessionFile: () => "/f", getBranch: () => [
  { type: "message", id: "u", message: { role: "user", content: "q" } },
  { type: "message", id: "a", message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: "Ship it?" }] } }] };
let shown = 0;
const ctx = { mode: "tui", hasUI: true, sessionManager: sm, model: { provider: "anthropic", id: "m" },
  modelRegistry: { getAvailable: () => [] }, ui: { select: async () => { shown++; }, notify: () => { shown++; } } };
let errors = 0;
process.on("uncaughtException", () => errors++);
process.on("unhandledRejection", () => errors++);
const t0 = Date.now();
await handlers.get("session_start")({ reason: "startup" }, ctx);
await handlers.get("input")({ text: "q", source: "interactive" }, ctx);
await handlers.get("message_end")({ message: { role: "user", content: "q" } }, ctx);
await handlers.get("agent_settled")({ aborted: false }, ctx);
const took = Date.now() - t0;
await new Promise(r => setTimeout(r, 300));
if (handlers.size !== 4 || errors || took > 200 || shown) { console.log("size", handlers.size, "errors", errors, "took", took, "shown", shown); process.exit(1); }
console.log("ok");
`
	cmd := exec.Command(node, "--input-type=module", "-e", script, ext)
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), "ERR_UNKNOWN_FILE_EXTENSION") {
		t.Skip("this node can't run TypeScript")
	}
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("%v: %s", err, out)
	}
}
