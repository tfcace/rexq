package install

// For each case these run the installer and compare what it said and the
// settings.json it left, byte for byte, with golden files. The settings
// in them were first written by the Python installer this package
// replaced, so they also pin the file format: key order, indentation and
// text outside ASCII all stay as they were.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// otherHooks is a settings file with someone else's hooks, other keys and
// non-ASCII text.
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

type claudeCase struct {
	name      string
	settings  string // "" for no file
	uninstall bool
	configDir bool // find the file through CLAUDE_CONFIG_DIR
	steps     int  // how many times to run it (a second run must change nothing)
}

var claudeCases = []claudeCase{
	{name: "fresh"},
	{name: "others", settings: otherHooks},
	{name: "old-install", settings: oldInstall},
	{name: "twice", settings: otherHooks, steps: 2},
	{name: "config-dir", settings: otherHooks, configDir: true},
	{name: "uninstall-none", settings: otherHooks, uninstall: true},
	{name: "uninstall-old", settings: oldInstall, uninstall: true},
	{name: "broken-json", settings: "{nope"},
	{name: "hooks-not-object", settings: `{"hooks": []}`},
}

func TestClaudeGolden(t *testing.T) {
	check := func(t *testing.T, name, got string) {
		golden := filepath.Join("testdata", "claude", name+".golden")
		if *update {
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
			t.Errorf("differs from the golden file\n--- got\n%s\n--- want\n%s", got, want)
		}
	}
	for _, c := range claudeCases {
		t.Run(c.name, func(t *testing.T) { check(t, c.name, runClaude(t, c)) })
	}
	// Install then uninstall leaves the other hooks as an uninstall
	// always left them.
	t.Run("roundtrip", func(t *testing.T) {
		dir := t.TempDir()
		settings := filepath.Join(dir, "settings.json")
		os.WriteFile(settings, []byte(otherHooks), 0o644)
		var b strings.Builder
		for _, f := range []func() ([]string, error){
			func() ([]string, error) { return Claude(settings, "/bin/rexq") },
			func() ([]string, error) { return RemoveClaude(settings) },
		} {
			msgs, err := f()
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString(strings.Join(msgs, "\n") + "\n")
		}
		data, _ := os.ReadFile(settings)
		b.Write(data)
		check(t, "roundtrip", scrub(b.String(), dir))
	})
}

var backupName = regexp.MustCompile(`\.bak-[0-9]+`)

func scrub(out, dir string) string {
	out = strings.NewReplacer("/bin/rexq", "$BIN", dir, "$DIR").Replace(out)
	return backupName.ReplaceAllString(out, ".bak-N")
}

// runClaude runs the case and describes everything it did, with the
// temporary paths and backup times replaced by placeholders.
func runClaude(t *testing.T, c claudeCase) string {
	t.Helper()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	if c.settings != "" {
		if err := os.WriteFile(settings, []byte(c.settings), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := settings
	if c.configDir {
		path = ClaudeSettings(func(k string) string {
			if k == "CLAUDE_CONFIG_DIR" {
				return dir
			}
			return ""
		}, "/nowhere")
	}
	steps := max(c.steps, 1)
	var b strings.Builder
	for i := range steps {
		var msgs []string
		var err error
		if c.uninstall {
			msgs, err = RemoveClaude(path)
		} else {
			msgs, err = Claude(path, "/bin/rexq")
		}
		fmt.Fprintf(&b, "== run %d\n", i+1)
		for _, m := range msgs {
			fmt.Fprintf(&b, "%s\n", m)
		}
		if err != nil {
			fmt.Fprintf(&b, "error: %v\n", err)
		}
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
	return scrub(b.String(), dir)
}

// A file is written back with nothing but the hooks changed: key order,
// number spelling, escapes and empty containers stay.
func TestJSONRoundTrip(t *testing.T) {
	in := "{\n  \"z\": 1.50,\n  \"a\": [\n    1e3,\n    null,\n    true,\n    {}\n  ],\n" +
		"  \"s\": \"tab\\t quote\\\" back\\\\ é— \\u0001 <&>\",\n  \"e\": [],\n  \"z2\": {\n    \"k\": false\n  }\n}\n"
	v, err := parseJSON([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(encodeJSON(v)); got != in {
		t.Errorf("got\n%s\nwant\n%s", got, in)
	}
	for _, bad := range []string{"", "{", `{"a":1} x`, `{"a":1,}`, "[1 2]", "\xff"} {
		if _, err := parseJSON([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/me/go/bin/rexq": "/Users/me/go/bin/rexq",
		"/Users/my name/rexq":   "'/Users/my name/rexq'",
		"/it's/rexq":            `'/it'"'"'s/rexq'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
