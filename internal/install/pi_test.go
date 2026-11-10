package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPi(t *testing.T) {
	dir := t.TempDir()
	ext := PiExtension(env("PI_CODING_AGENT_DIR", filepath.Join(dir, "agent")), "/nowhere")
	if ext != filepath.Join(dir, "agent", "extensions", "rexq.ts") {
		t.Fatalf("extension path %s", ext)
	}
	if got := PiExtension(env(), "/home/me"); got != "/home/me/.pi/agent/extensions/rexq.ts" {
		t.Errorf("default extension path %s", got)
	}
	for _, bad := range []string{"", dir} {
		if _, err := Pi(bad, "/rexq"); err == nil || !strings.Contains(err.Error(), "not a file path") {
			t.Errorf("%q taken as the extension's path: %v", bad, err)
		}
	}

	msgs, err := Pi(ext, "/opt/rexq")
	if err != nil || !strings.Contains(msgs[0], "installed rexq's Pi extension in "+ext) {
		t.Fatalf("%v %q", err, msgs)
	}
	src := read(t, ext)
	repo := read(t, "../../pi/rexq.ts")
	if !strings.HasPrefix(src, piMarker) || !strings.Contains(src, "\nconst REXQ_BIN = \"/opt/rexq\";\n") ||
		strings.Count(src, "\n") != strings.Count(repo, "\n") {
		t.Error("the installed extension is not the repository's with the binary written in")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(ext), "*.tmp")); len(leftovers) > 0 {
		t.Errorf("left %q", leftovers)
	}
	if msgs, err := Pi(ext, "/opt/rexq"); err != nil || !strings.Contains(msgs[0], "already has") {
		t.Errorf("second run: %v %q", err, msgs)
	}

	// An extension the Python installer wrote is rexq's too, and is
	// replaced.
	os.WriteFile(ext, []byte("// rexq Pi extension: installed by scripts/install-hooks.py --pi.\nold\n"), 0o644)
	if _, err := Pi(ext, "/opt/rexq"); err != nil || read(t, ext) != src {
		t.Errorf("an earlier install wasn't replaced: %v", err)
	}

	// Someone else's rexq.ts is left alone, by install and uninstall.
	foreign := filepath.Join(dir, "other", "rexq.ts")
	os.MkdirAll(filepath.Dir(foreign), 0o755)
	os.WriteFile(foreign, []byte("// mine\n"), 0o644)
	if _, err := Pi(foreign, "/opt/rexq"); err == nil || !strings.Contains(err.Error(), "isn't rexq's") {
		t.Errorf("overwrote a foreign file: %v", err)
	}
	if _, err := RemovePi(foreign); err == nil || !strings.Contains(err.Error(), "leaving it alone") {
		t.Errorf("removed a foreign file: %v", err)
	}
	if read(t, foreign) != "// mine\n" {
		t.Error("foreign file changed")
	}

	if msgs, err := RemovePi(ext); err != nil || !strings.Contains(msgs[0], "removed") || exists(ext) {
		t.Errorf("uninstall: %v %q", err, msgs)
	}
	if msgs, err := RemovePi(ext); err != nil || !strings.Contains(msgs[0], "isn't installed") {
		t.Errorf("second uninstall: %v %q", err, msgs)
	}
}

// The installed extension, its binary pointing at nothing, loads and runs
// every handler without throwing or waiting.
func TestInstalledExtensionWithAMissingBinaryLoads(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	dir := t.TempDir()
	ext := filepath.Join(dir, "rexq.ts")
	if _, err := Pi(ext, filepath.Join(dir, "gone", "rexq")); err != nil {
		t.Fatal(err)
	}
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
	out, err := exec.Command(node, "--input-type=module", "-e", script, ext).CombinedOutput()
	if strings.Contains(string(out), "ERR_UNKNOWN_FILE_EXTENSION") {
		t.Skip("this node can't run TypeScript")
	}
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("%v: %s", err, out)
	}
}
