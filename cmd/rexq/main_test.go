package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tfcace/rexq/internal/store"
)

// rexqBin is the binary built once for the tests that run it.
var rexqBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rexq-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	rexqBin = filepath.Join(dir, "rexq")
	if out, err := exec.Command("go", "build", "-o", rexqBin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// A hook that exits non-zero disturbs the agent, and exit 2 from Stop makes
// Claude keep working. Whatever arrives on stdin, every hook must exit 0.
// Hooks print nothing either: stdout from SessionStart and UserPromptSubmit
// goes into the agent's context.
func TestHooksAlwaysExitZero(t *testing.T) {
	inputs := []string{"", "not json", `{"session_id":`, `{"session_id":"x","last_assistant_message":"Should I?"}`}
	working := filepath.Join(t.TempDir(), "q.db")
	dbs := map[string]string{
		"broken store":  "/dev/null/q.db", // can't be created
		"working store": working,
	}
	for label, db := range dbs {
		for _, name := range []string{"prompt", "start", "stop", "bogus"} {
			for _, in := range inputs {
				cmd := exec.Command(rexqBin, "hook", name)
				cmd.Stdin = strings.NewReader(in)
				var stdout bytes.Buffer
				cmd.Stdout = &stdout
				// No claude on PATH, so a stop that gets as far as extracting fails there.
				cmd.Env = []string{"REXQ_DB=" + db, "PATH=/nonexistent", "HOME=" + t.TempDir(), "REX_BLOCK=block:1"}
				if err := cmd.Run(); err != nil {
					t.Errorf("%s: hook %s with %q: %v", label, name, in, err)
				}
				if stdout.Len() > 0 {
					t.Errorf("%s: hook %s with %q printed %q", label, name, in, stdout.String())
				}
			}
		}
	}
	// The stop with a real message got past the lock and the model call and
	// was queued; a panic on the way (a missing dependency, say) would exit
	// 0 too, so check the log for one.
	log, err := os.ReadFile(filepath.Join(filepath.Dir(working), "hook.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "panic") {
		t.Errorf("a hook panicked:\n%s", log)
	}
	if !strings.Contains(string(log), "stop: ") {
		t.Errorf("the failed stop wasn't logged:\n%s", log)
	}
}

// Closing the pane signals the hook while the model runs. The turn must be
// queued for a retry, not lost, and the hook still exits 0.
func TestStopQueuesItsTurnWhenSignalled(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			started := filepath.Join(dir, "started")
			fake := fmt.Sprintf("#!/bin/sh\n: > '%s'\nexec /bin/sleep 30\n", started)
			if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(dir, "state", "q.db")
			cmd := exec.Command(rexqBin, "hook", "stop")
			cmd.Stdin = strings.NewReader(`{"session_id":"x","last_assistant_message":"Should I?"}`)
			cmd.Env = []string{"REXQ_DB=" + db, "PATH=" + dir, "HOME=" + dir}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if _, err := os.Stat(started); err == nil {
					break
				}
				if time.Now().After(deadline) {
					cmd.Process.Kill()
					t.Fatal("the model never started")
				}
			}
			cmd.Process.Signal(sig)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("hook exited with %v", err)
				}
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				t.Fatal("hook didn't stop on the signal")
			}
			s, err := store.Open(db)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if rows, err := s.PendingFor("x"); err != nil || len(rows) != 1 {
				t.Fatalf("turn not queued: %d rows, %v", len(rows), err)
			}
		})
	}
}

// The log stays small (one rotation past maxLog) and one entry stays one
// line, whatever an error message holds.
func TestHookLogRotatesAndFlattens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook.log")
	big := strings.Repeat("x", maxLog)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	logTo(path)("stop: %v", "first\nsecond\r\nthird")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(got), "\n"); lines != 1 || !strings.HasSuffix(string(got), " stop: first | second | third\n") {
		t.Errorf("entry not one flat line: %q", got)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil || string(old) != big {
		t.Errorf("old log not kept as hook.log.1: %v", err)
	}
	// Below the limit, entries append.
	logTo(path)("next")
	if got, _ := os.ReadFile(path); strings.Count(string(got), "\n") != 2 {
		t.Errorf("entry not appended: %q", got)
	}
}

// runRexq runs the rexq at bin with args. A non-nil env replaces the
// environment.
func runRexq(t *testing.T, bin string, env []string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

// installer runs rexq install claude on settings, for hooks that run
// binary.
func installer(t *testing.T, settings, binary string) (stdout, stderr string, err error) {
	t.Helper()
	return runRexq(t, rexqBin, nil, "install", "claude", "--bin", binary, settings)
}

// hookCommands returns the commands of the hooks in the settings at path,
// by event.
func hookCommands(t *testing.T, path string) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Hooks map[string][]struct {
			Hooks []map[string]any
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for event, groups := range got.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				out[event] = append(out[event], fmt.Sprint(h["command"]))
			}
		}
	}
	return out
}

// linkRexq puts a rexq in dir, linked to the test binary, and returns its path.
func linkRexq(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "rexq")
	if err := os.Symlink(rexqBin, p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Without arguments install claude writes $CLAUDE_CONFIG_DIR/settings.json,
// or ~/.claude/settings.json, creating it when it's missing: no backup of
// nothing, and only the user may read it. The binary is the rexq that ran.
func TestInstallHooksDefaults(t *testing.T) {
	for _, configDir := range []bool{false, true} {
		dir := t.TempDir()
		home := filepath.Join(dir, "home")
		bin := linkRexq(t, filepath.Join(dir, "bin"))
		env := []string{"HOME=" + home, "PATH=" + filepath.Dir(bin)}
		settings := filepath.Join(home, ".claude", "settings.json")
		if configDir {
			settings = filepath.Join(dir, "config", "claude", "settings.json")
			env = append(env, "CLAUDE_CONFIG_DIR="+filepath.Dir(settings))
		}
		out, errOut, err := runRexq(t, bin, env, "install", "claude")
		if err != nil {
			t.Fatalf("CLAUDE_CONFIG_DIR %v: %v: %s%s", configDir, err, out, errOut)
		}
		fi, err := os.Stat(settings)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("CLAUDE_CONFIG_DIR %v: settings not created 0600: %v", configDir, err)
		}
		if fi, err := os.Stat(filepath.Dir(settings)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("CLAUDE_CONFIG_DIR %v: directory not created 0700: %v", configDir, err)
		}
		if cs := hookCommands(t, settings)["Stop"]; len(cs) != 1 || cs[0] != bin+" hook stop || true" {
			t.Errorf("CLAUDE_CONFIG_DIR %v: Stop %q, want the rexq that ran", configDir, cs)
		}
		if backups, _ := filepath.Glob(settings + ".bak-*"); len(backups) != 0 {
			t.Errorf("CLAUDE_CONFIG_DIR %v: backed up a missing file: %q", configDir, backups)
		}
		if configDir {
			if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
				t.Errorf("~/.claude touched despite CLAUDE_CONFIG_DIR: %v", err)
			}
		}
		// Uninstalling leaves the hooks object, and backs up what it changes.
		if out, errOut, err := runRexq(t, bin, env, "uninstall", "claude"); err != nil {
			t.Fatalf("uninstall: %v: %s%s", err, out, errOut)
		}
		if cs := hookCommands(t, settings); len(cs) != 0 {
			t.Errorf("uninstall left %q", cs)
		}
		if backups, _ := filepath.Glob(settings + ".bak-*"); len(backups) != 1 {
			t.Errorf("uninstall backups: %q", backups)
		}
	}
}

const rexGroup = `{"hooks":[{"type":"command","command":"/bin/sh -c 'rex __internal session-start || true'","timeout":5}]}`

// Settings as an earlier install left them: old-style rexq commands, one in
// a group of its own and one in a group it shares, beside Rex's hook.
var oldSettings = `{
  "model": "opus",
  "note": "keeps — non-ASCII",
  "hooks": {
    "SessionStart": [` + rexGroup + `],
    "UserPromptSubmit": [{"hooks":[{"type":"command","command":"/old/path/rexq hook prompt","timeout":5}]}],
    "Stop": [{"hooks":[{"type":"command","command":"/Users/me/go/bin/rexq hook stop","async":true},{"type":"command","command":"say done"}]}]
  }
}
`

func TestInstallHooks(t *testing.T) {
	dir := t.TempDir()
	// The settings file is a symlink, as in a dotfiles checkout.
	real := filepath.Join(dir, "dotfiles", "settings.json")
	link := filepath.Join(dir, "settings.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(oldSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := installer(t, link, rexqBin)
	if err != nil {
		t.Fatalf("%v: %s%s", err, out, errOut)
	}

	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("settings link replaced by a file: %v", err)
	}
	fi, err := os.Stat(real)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed: %v %v", fi.Mode(), err)
	}
	raw, _ := os.ReadFile(real)
	if !strings.Contains(string(raw), "keeps — non-ASCII") {
		t.Errorf("other text rewritten: %s", raw)
	}
	var got struct {
		Model string
		Hooks map[string][]struct {
			Hooks []map[string]any
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "opus" {
		t.Errorf("other settings lost: %s", raw)
	}
	commands := func(event string) []string {
		var cs []string
		for _, g := range got.Hooks[event] {
			for _, h := range g.Hooks {
				cs = append(cs, fmt.Sprint(h["command"]))
			}
		}
		return cs
	}
	want := func(name string) string { return rexqBin + " hook " + name + " || true" }
	if cs := commands("SessionStart"); len(cs) != 2 || !strings.HasPrefix(cs[0], "/bin/sh -c 'rex ") || cs[1] != want("start") {
		t.Errorf("SessionStart: %q", cs)
	}
	if cs := commands("UserPromptSubmit"); len(cs) != 1 || cs[0] != want("prompt") {
		t.Errorf("UserPromptSubmit: old command not replaced: %q", cs)
	}
	if cs := commands("Stop"); len(cs) != 2 || cs[0] != want("stop") || cs[1] != "say done" {
		t.Errorf("Stop: %q", cs)
	}
	if async := got.Hooks["Stop"][0].Hooks[0]["async"]; async != true {
		t.Errorf("Stop hook not async: %v", async)
	}
	backups, _ := filepath.Glob(link + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("want one backup, got %q", backups)
	}
	if b, _ := os.ReadFile(backups[0]); string(b) != oldSettings {
		t.Errorf("backup isn't the old file")
	}
	if _, err := os.Stat(real + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind: %v", err)
	}

	// A second run changes nothing.
	out, errOut, err = installer(t, link, rexqBin)
	if err != nil || !strings.Contains(out, "already") {
		t.Errorf("second run: %v %s%s", err, out, errOut)
	}
	if again, _ := os.ReadFile(real); string(again) != string(raw) {
		t.Errorf("second run changed the file")
	}
	if backups, _ := filepath.Glob(link + ".bak-*"); len(backups) != 1 {
		t.Errorf("second run made a backup: %q", backups)
	}
}

// The installer refuses, with one line and no change, when it can't do the
// job safely.
func TestInstallHooksRefuses(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.json", oldSettings, 0o600)
	exits2 := write("rexq-old", "#!/bin/sh\necho usage >&2\nexit 2\n", 0o755)
	cases := []struct {
		name, settings, binary string
	}{
		{"malformed JSON", write("bad.json", `{"hooks": {`, 0o600), rexqBin},
		{"hooks not an object", write("list.json", `{"hooks": []}`, 0o600), rexqBin},
		{"event not a list", write("event.json", `{"hooks": {"Stop": {}}}`, 0o600), rexqBin},
		{"no binary", good, filepath.Join(dir, "nope")},
		{"binary fails its hook", good, exits2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, _ := os.ReadFile(c.settings)
			out, errOut, err := installer(t, c.settings, c.binary)
			if err == nil {
				t.Fatalf("succeeded: %s", out)
			}
			if strings.Contains(errOut, "Traceback") || strings.Count(strings.TrimSpace(errOut), "\n") != 0 {
				t.Errorf("want one line, got %q", errOut)
			}
			if after, _ := os.ReadFile(c.settings); string(after) != string(before) {
				t.Errorf("settings changed")
			}
		})
	}
	if backups, _ := filepath.Glob(filepath.Join(dir, "*.bak-*")); len(backups) != 0 {
		t.Errorf("refused runs left backups: %q", backups)
	}
}

// uninstaller runs rexq uninstall claude on settings.
func uninstaller(t *testing.T, settings string) (stdout, stderr string, err error) {
	t.Helper()
	return runRexq(t, rexqBin, nil, "uninstall", "claude", settings)
}

func TestUninstallHooks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	link := filepath.Join(dir, "settings.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(oldSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// Install first, so both the quoted new form and a shared group are there.
	if out, errOut, err := installer(t, link, rexqBin); err != nil {
		t.Fatalf("%v: %s%s", err, out, errOut)
	}
	installed, _ := os.ReadFile(real)
	for _, b := range must(filepath.Glob(link + ".bak-*")) {
		os.Remove(b)
	}

	out, errOut, err := uninstaller(t, link)
	if err != nil {
		t.Fatalf("%v: %s%s", err, out, errOut)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("settings link replaced by a file: %v", err)
	}
	if fi, err := os.Stat(real); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed: %v", err)
	}
	raw, _ := os.ReadFile(real)
	if strings.Contains(string(raw), "rexq") {
		t.Errorf("rexq hooks left:\n%s", raw)
	}
	var got struct {
		Model string
		Note  string
		Hooks map[string][]struct {
			Hooks []map[string]any
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "opus" || got.Note != "keeps — non-ASCII" {
		t.Errorf("other settings lost:\n%s", raw)
	}
	// Rex's own hook and the Stop group's other hook stay; the groups that
	// held only rexq hooks go, and so does the event they emptied.
	if g := got.Hooks["SessionStart"]; len(g) != 1 || len(g[0].Hooks) != 1 || !strings.Contains(fmt.Sprint(g[0].Hooks[0]["command"]), "rex __internal") {
		t.Errorf("SessionStart: %+v", g)
	}
	if g := got.Hooks["Stop"]; len(g) != 1 || len(g[0].Hooks) != 1 || g[0].Hooks[0]["command"] != "say done" {
		t.Errorf("Stop: %+v", g)
	}
	if _, ok := got.Hooks["UserPromptSubmit"]; ok {
		t.Errorf("emptied event kept: %s", raw)
	}
	backups := must(filepath.Glob(link + ".bak-*"))
	if len(backups) != 1 {
		t.Fatalf("want one backup, got %q", backups)
	}
	if b, _ := os.ReadFile(backups[0]); string(b) != string(installed) {
		t.Errorf("backup isn't the installed file")
	}

	// Nothing left to remove: no change, no backup.
	out, errOut, err = uninstaller(t, link)
	if err != nil || !strings.Contains(out, "no rexq hooks") {
		t.Errorf("second run: %v %s%s", err, out, errOut)
	}
	if again, _ := os.ReadFile(real); string(again) != string(raw) {
		t.Errorf("second run changed the file")
	}
	if backups := must(filepath.Glob(link + ".bak-*")); len(backups) != 1 {
		t.Errorf("second run made a backup: %q", backups)
	}
}

// The log quotes agents' errors, so terminal escapes in them are dropped:
// reading the log with cat or tail must not run them.
func TestHookLogDropsControlCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook.log")
	logTo(path)("claude -p: %s", "\x1b]52;c;cGF5bG9hZA==\x07red \x1b[31mtext\u009b2J")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSuffix(string(got), "\n")
	if strings.ContainsAny(line, "\x1b\x07\u009b\n") || !strings.HasSuffix(line, "claude -p: ]52;c;cGF5bG9hZA==red [31mtext2J") {
		t.Errorf("logged %q", got)
	}
}

// The log quotes agents' errors: only the user may read it.
func TestHookLogIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook.log")
	logTo(path)("new")
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("new log: %v %v", fi.Mode(), err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	logTo(path)("again")
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("old log left %v %v", fi.Mode(), err)
	}
}

// q hides the panel only where hiding works: in the floating layer the Rex
// action starts with --layer. Run by hand in a Rex pane, q quits.
func TestPanelHidesOnlyAsTheLayer(t *testing.T) {
	inRex := map[string]string{"REX_SESSION": "session:a", "REX_BLOCK": "block:1"}
	cases := []struct {
		args []string
		env  map[string]string
		want bool
	}{
		{[]string{"--layer"}, inRex, true},
		{nil, inRex, false},
		{[]string{"--layer"}, map[string]string{"REX_SESSION": "session:a"}, false},
		{[]string{"--layer"}, nil, false},
	}
	for _, c := range cases {
		got, err := panelLayer(c.args, func(k string) string { return c.env[k] })
		if err != nil || got != c.want {
			t.Errorf("%q %v: %v %v, want %v", c.args, c.env, got, err, c.want)
		}
	}
	if _, err := panelLayer([]string{"--bogus"}, func(string) string { return "" }); err == nil {
		t.Error("unknown flag accepted")
	}
}

// rexq install, by itself, sets up Rex and Claude Code when it is here,
// with this binary's path in both. Pi is added by name only, and kept up
// to date from then on. rexq uninstall takes all of them out.
func TestInstallEverything(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	bin := linkRexq(t, filepath.Join(dir, "bin"))
	// A rex that records its arguments, and no claude or pi on PATH.
	calls := filepath.Join(dir, "rex-calls")
	if err := os.WriteFile(filepath.Join(dir, "bin", "rex"), []byte("#!/bin/sh\necho \"$*\" >> \""+calls+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "PATH=" + filepath.Dir(bin), "REXQ_DB=" + filepath.Join(dir, "q.db")}
	settings := filepath.Join(home, ".claude", "settings.json")
	ext := filepath.Join(home, ".pi", "agent", "extensions", "rexq.ts")
	module := filepath.Join(home, ".config", "rex", "rexq.lua")
	initLua := filepath.Join(home, ".config", "rex", "init.lua")

	// Claude Code isn't here: only Rex is set up, and it says so.
	out, errOut, err := runRexq(t, bin, env, "install", "--key", "cmd+j")
	if err != nil {
		t.Fatalf("%v: %s%s", err, out, errOut)
	}
	for _, p := range []string{settings, ext} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s written for an agent that isn't here", p)
		}
	}
	if !strings.Contains(out, "Claude Code: not found") || strings.Contains(out, "Pi") || !strings.Contains(out, "isn't here yet") {
		t.Errorf("skipped agents not reported:\n%s", out)
	}
	if b, _ := os.ReadFile(module); !strings.Contains(string(b), "local BIN = \""+bin+"\"\n") {
		t.Errorf("module lacks the binary:\n%.300s", b)
	}
	if b, _ := os.ReadFile(initLua); !strings.Contains(string(b), `require("rexq").setup{ key = "cmd+j" }`) {
		t.Errorf("init.lua: %s", b)
	}
	if b, _ := os.ReadFile(calls); string(b) != "config reload\n" {
		t.Errorf("rex calls: %q", b)
	}

	// Both agents' directories exist: Claude Code is set up, Pi is only
	// pointed at, and Rex is left as it is.
	os.MkdirAll(filepath.Dir(settings), 0o755)
	os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0o755)
	out, errOut, err = runRexq(t, bin, env, "install")
	if err != nil {
		t.Fatalf("%v: %s%s", err, out, errOut)
	}
	if cs := hookCommands(t, settings)["Stop"]; len(cs) != 1 || cs[0] != bin+" hook stop || true" {
		t.Errorf("Stop %q", cs)
	}
	if _, err := os.Stat(ext); !os.IsNotExist(err) {
		t.Errorf("the Pi extension was installed without being asked for")
	}
	if !strings.Contains(out, "`rexq install pi`") || strings.Contains(out, "billed") || !strings.Contains(out, "already loads rexq") {
		t.Errorf("output:\n%s", out)
	}
	if b, _ := os.ReadFile(calls); string(b) != "config reload\n" {
		t.Errorf("reloaded Rex for nothing: %q", b)
	}

	// The Pi model goes into the store; a bad one is refused before
	// anything is written.
	if out, errOut, err := runRexq(t, bin, env, "install", "pi", "--model", "openai/gpt-5-mini"); err != nil || !strings.Contains(out, "extractions use openai/gpt-5-mini") {
		t.Errorf("%v: %s%s", err, out, errOut)
	}
	if out, _, _ := runRexq(t, bin, env, "config", "get", "pi-model"); strings.TrimSpace(out) != "openai/gpt-5-mini" {
		t.Errorf("pi-model %q", out)
	}
	if b, _ := os.ReadFile(ext); !strings.Contains(string(b), "const REXQ_BIN = \""+bin+"\";") {
		t.Errorf("extension lacks the binary")
	}
	// Once it is installed, a plain install keeps it current: here, after
	// the binary moved.
	moved := linkRexq(t, filepath.Join(dir, "bin2"))
	if out, errOut, err := runRexq(t, moved, env, "install"); err != nil || !strings.Contains(out, "billed") {
		t.Errorf("%v: %s%s", err, out, errOut)
	}
	if b, _ := os.ReadFile(ext); !strings.Contains(string(b), "const REXQ_BIN = \""+moved+"\";") {
		t.Errorf("a plain install left the installed extension on the old binary")
	}
	for _, bad := range [][]string{{"pi", "--model", "haiku"}, {"pi", "--model", ""}, {"--key", "a", "--no-key"}, {"rex", "--key", ""}, {"nope"}, {"claude", "a", "b"}} {
		if out, errOut, err := runRexq(t, bin, env, append([]string{"install"}, bad...)...); err == nil || out != "" {
			t.Errorf("install %q: %v %s%s", bad, err, out, errOut)
		}
	}

	out, errOut, err = runRexq(t, bin, env, "uninstall")
	if err != nil {
		t.Fatalf("%v: %s%s", err, out, errOut)
	}
	if cs := hookCommands(t, settings); len(cs) != 0 {
		t.Errorf("hooks left: %q", cs)
	}
	for _, p := range []string{ext, module} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left", p)
		}
	}
	if b, _ := os.ReadFile(initLua); strings.Contains(string(b), "rexq") {
		t.Errorf("init.lua still has rexq: %s", b)
	}
	if !strings.Contains(out, bin) || !strings.Contains(out, dir) {
		t.Errorf("what's left isn't named:\n%s", out)
	}
}

// snapshot describes every file under dir: its path, mode and contents.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if fi.IsDir() {
			fmt.Fprintf(&b, "%s/ %v\n", rel, fi.Mode().Perm())
			return nil
		}
		data, err := os.ReadFile(p)
		fmt.Fprintf(&b, "%s %v\n%s\n", rel, fi.Mode().Perm(), data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Running install again, or uninstall again, changes nothing on disk:
// no file rewritten, no backup made, Rex not reloaded. A further install
// and uninstall round ends where the first did, so repeating them never
// piles anything up.
func TestInstallAndUninstallAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	bin := linkRexq(t, filepath.Join(dir, "bin"))
	calls := filepath.Join(dir, "rex-calls")
	if err := os.WriteFile(filepath.Join(dir, "bin", "rex"), []byte("#!/bin/sh\necho \"$*\" >> \""+calls+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "PATH=" + filepath.Dir(bin), "REXQ_DB=" + filepath.Join(dir, "state", "q.db")}
	// Files with things of the user's own in them, in a format that isn't
	// the installer's.
	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude/settings.json", oldSettings)
	write(".config/rex/init.lua", "rex.bind(\"cmd+x\", \"y\")") // no final newline
	reloads := func() int {
		b, _ := os.ReadFile(calls)
		return strings.Count(string(b), "config reload\n")
	}
	run := func(args ...string) string {
		t.Helper()
		out, errOut, err := runRexq(t, bin, env, args...)
		if err != nil {
			t.Fatalf("rexq %q: %v: %s%s", args, err, out, errOut)
		}
		return out
	}
	both := func(verb string) {
		run(verb)
		if verb == "install" {
			run("install", "pi", "--model", "openai/gpt-5-mini")
		}
	}
	// repeat runs verb again twice and wants nothing to have changed.
	repeat := func(verb string) {
		t.Helper()
		want, wantReloads := snapshot(t, home), reloads()
		for i := range 2 {
			both(verb)
			if got := snapshot(t, home); got != want {
				t.Errorf("%s, run %d after the first, changed the files:\n--- before\n%s\n--- after\n%s", verb, i+1, want, got)
			}
			if reloads() != wantReloads {
				t.Errorf("%s, run %d after the first, reloaded Rex", verb, i+1)
			}
		}
	}
	// state is the files without the backups: those record each real
	// change, and are the one thing a new round adds.
	state := func() string {
		return dropBackups(backupSuffix.ReplaceAllString(snapshot(t, home), ".bak-N"))
	}

	// The first install upgrades the old hooks in place.
	both("install")
	repeat("install")
	if out := run("config", "get", "pi-model"); strings.TrimSpace(out) != "openai/gpt-5-mini" {
		t.Errorf("pi-model %q", out)
	}
	both("uninstall")
	repeat("uninstall")
	clean := state()
	for _, rel := range []string{".claude/settings.json", ".config/rex/init.lua"} {
		if b, _ := os.ReadFile(filepath.Join(home, rel)); strings.Contains(string(b), "rexq") {
			t.Errorf("%s still has rexq in it:\n%s", rel, b)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".config/rex/init.lua")); string(b) != "rex.bind(\"cmd+x\", \"y\")\n" {
		t.Errorf("init.lua after uninstall: %q", b)
	}

	// From there, every round lands on the same files, both ways.
	installed := ""
	for round := 2; round <= 3; round++ {
		both("install")
		repeat("install")
		if got := state(); installed == "" {
			installed = got
		} else if got != installed {
			t.Errorf("install round %d differs from the one before:\n--- before\n%s\n--- now\n%s", round, installed, got)
		}
		both("uninstall")
		repeat("uninstall")
		if got := state(); got != clean {
			t.Errorf("uninstall round %d differs from the first:\n--- first\n%s\n--- now\n%s", round, clean, got)
		}
	}
}

var backupSuffix = regexp.MustCompile(`\.bak-[0-9]+`)

// dropBackups removes the backup files' entries from a snapshot.
func dropBackups(snap string) string {
	var kept []string
	skip := false
	for _, line := range strings.Split(snap, "\n") {
		if isEntry := entryLine.MatchString(line); isEntry {
			skip = strings.Contains(line, ".bak-N")
		}
		if !skip {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// entryLine matches a snapshot line that starts a file or directory.
var entryLine = regexp.MustCompile(`^\S+ -[rwx-]{9}$`)
