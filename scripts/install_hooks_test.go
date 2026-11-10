package scripts

// Characterization tests for install-hooks.py without --pi: for each case it
// runs the script and compares what it printed and the settings.json it
// left, byte for byte, with golden files made by the installer from before
// Pi support. Run with -update only against that installer.

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// otherHooks is a settings file with someone else's hooks, other keys and
// non-ASCII text, in an order json.dump keeps.
const otherHooks = `{
  "model": "opus",
  "statusLine": {"type": "command", "command": "echo é—ok"},
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "say done"}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard"}]}]
  }
}
`

// oldInstall has rexq hooks in the form an early installer wrote, sharing a
// group with another hook.
const oldInstall = `{"hooks": {"Stop": [{"hooks": [
  {"type": "command", "command": "/old/path/rexq hook stop", "async": true},
  {"type": "command", "command": "say done"}]}],
 "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "/old/path/rexq hook prompt"}]}]}}
`

type installCase struct {
	name     string
	settings string // "" for no file
	args     []string
	env      []string
	steps    int // how many times to run it (a second run must change nothing)
}

var installCases = []installCase{
	{name: "fresh", args: []string{"$SETTINGS", "$BIN"}},
	{name: "others", settings: otherHooks, args: []string{"$SETTINGS", "$BIN"}},
	{name: "old-install", settings: oldInstall, args: []string{"$SETTINGS", "$BIN"}},
	{name: "twice", settings: otherHooks, args: []string{"$SETTINGS", "$BIN"}, steps: 2},
	{name: "config-dir", settings: otherHooks, args: []string{"", "$BIN"}, env: []string{"CLAUDE_CONFIG_DIR=$DIR"}},
	{name: "uninstall-none", settings: otherHooks, args: []string{"--uninstall", "$SETTINGS"}},
	{name: "uninstall-old", settings: oldInstall, args: []string{"--uninstall", "$SETTINGS"}},
	{name: "broken-json", settings: "{nope", args: []string{"$SETTINGS", "$BIN"}},
	{name: "hooks-not-object", settings: `{"hooks": []}`, args: []string{"$SETTINGS", "$BIN"}},
}

func TestInstallHooksWithoutPiIsUnchanged(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	for _, c := range installCases {
		t.Run(c.name, func(t *testing.T) {
			got := runInstall(t, c)
			golden := filepath.Join("testdata", "install-hooks", c.name+".golden")
			if *update {
				os.MkdirAll(filepath.Dir(golden), 0o755)
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("output differs from the installer before Pi support\n--- got\n%s\n--- want\n%s", got, want)
			}
		})
	}
	// Install then uninstall leaves the other hooks byte for byte as an
	// uninstall always left them.
	t.Run("roundtrip", func(t *testing.T) {
		out := runRoundTrip(t)
		golden := filepath.Join("testdata", "install-hooks", "roundtrip.golden")
		if *update {
			os.WriteFile(golden, []byte(out), 0o644)
			return
		}
		want, _ := os.ReadFile(golden)
		if out != string(want) {
			t.Errorf("round trip differs\n--- got\n%s\n--- want\n%s", out, want)
		}
	})
}

// fakeRexq writes a rexq that passes the installer's check.
func fakeRexq(t *testing.T, dir string) string {
	bin := filepath.Join(dir, "bin", "rexq")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

var backupName = regexp.MustCompile(`\.bak-[0-9]+`)

// runInstall runs the case and describes everything it did, with the
// temporary paths and backup times replaced by placeholders.
func runInstall(t *testing.T, c installCase) string {
	t.Helper()
	dir := t.TempDir()
	bin := fakeRexq(t, dir)
	settings := filepath.Join(dir, "settings.json")
	if c.settings != "" {
		if err := os.WriteFile(settings, []byte(c.settings), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sub := strings.NewReplacer("$SETTINGS", settings, "$BIN", bin, "$DIR", dir)
	var args []string
	for _, a := range c.args {
		if a != "" {
			args = append(args, sub.Replace(a))
		}
	}
	steps := c.steps
	if steps == 0 {
		steps = 1
	}
	var b strings.Builder
	for i := 0; i < steps; i++ {
		cmd := exec.Command("python3", append([]string{"install-hooks.py"}, args...)...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		cmd.Env = append(os.Environ(), "HOME="+dir, "CLAUDE_CONFIG_DIR=")
		for _, e := range c.env {
			cmd.Env = append(cmd.Env, sub.Replace(e))
		}
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "== run %d: exit %d\n-- stdout\n%s-- stderr\n%s", i+1, code, stdout.String(), stderr.String())
	}
	fmt.Fprintf(&b, "-- settings.json\n")
	if data, err := os.ReadFile(settings); err == nil {
		fi, _ := os.Stat(settings)
		fmt.Fprintf(&b, "mode %v\n%s", fi.Mode().Perm(), data)
	} else {
		b.WriteString("(none)\n")
	}
	backups, _ := filepath.Glob(settings + ".bak-*")
	fmt.Fprintf(&b, "-- %d backups\n", len(backups))
	for _, p := range backups {
		data, _ := os.ReadFile(p)
		fmt.Fprintf(&b, "backup equals the input: %v\n", string(data) == c.settings)
	}
	out := strings.NewReplacer(bin, "$BIN", dir, "$DIR").Replace(b.String())
	return backupName.ReplaceAllString(out, ".bak-N")
}

// runRoundTrip installs into otherHooks, uninstalls, and shows the result.
func runRoundTrip(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := fakeRexq(t, dir)
	settings := filepath.Join(dir, "settings.json")
	os.WriteFile(settings, []byte(otherHooks), 0o644)
	var b strings.Builder
	for _, args := range [][]string{{settings, bin}, {"--uninstall", settings}} {
		cmd := exec.Command("python3", append([]string{"install-hooks.py"}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+dir, "CLAUDE_CONFIG_DIR=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		b.Write(out)
	}
	data, _ := os.ReadFile(settings)
	b.Write(data)
	return backupName.ReplaceAllString(strings.NewReplacer(bin, "$BIN", dir, "$DIR").Replace(b.String()), ".bak-N")
}
