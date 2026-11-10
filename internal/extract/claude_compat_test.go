package extract

// Characterization tests: they pin the exact claude -p command line and
// environment the Claude extractor builds. They use only APIs that existed
// before Pi support, so they also run against the commit before it.

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fakeClaudeEnv is fakeClaude that also saves its whole environment and
// working directory.
func fakeClaudeEnv(t *testing.T) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\0' \"$@\" > \"${0%/*}/args\"\n" +
		"/usr/bin/env > \"${0%/*}/env\"\n" +
		"/bin/pwd -P > \"${0%/*}/pwd\"\n" +
		`echo '{"is_error":false,"result":"","structured_output":{"items":[],"resolved":[]}}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func TestClaudeCompatArgvAndEnvironment(t *testing.T) {
	t.Setenv("REX_SESSION", "session:a")
	t.Setenv("REX_BLOCK", "block:1")
	t.Setenv("REXQ_COMPAT_MARK", "kept")
	t.Setenv(ClaudeEnv, "")
	bin, dir := fakeClaudeEnv(t)
	work := t.TempDir()
	if _, err := (Claude{Bin: bin, Dir: work}).Run(context.Background(), "the prompt", `{"s":1}`); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "args"))
	args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	want := []string{"-p", "--model", "haiku", "--no-session-persistence", "--safe-mode",
		"--settings", `{"disableAllHooks":true}`, "--strict-mcp-config", "--tools=",
		"--output-format", "json", "--json-schema", `{"s":1}`, "--", "the prompt"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("argv\n got %q\nwant %q", args, want)
	}

	envRaw, _ := os.ReadFile(filepath.Join(dir, "env"))
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(envRaw)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	if got[GuardEnv] != "1" {
		t.Errorf("%s=%q, want 1", GuardEnv, got[GuardEnv])
	}
	for _, k := range []string{"REX_SESSION", "REX_BLOCK"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s reached the extractor", k)
		}
	}
	if got["REXQ_COMPAT_MARK"] != "kept" || got["HOME"] != os.Getenv("HOME") {
		t.Errorf("the rest of the environment was not passed through")
	}
	// Every variable it sees is the parent's, plus the guard.
	var extra []string
	for k, v := range got {
		if k == GuardEnv || k == "PWD" || k == "SHLVL" || k == "_" || k == "OLDPWD" {
			continue
		}
		if pv, ok := os.LookupEnv(k); !ok || pv != v {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("variables not from the parent: %q", extra)
	}

	pwd, _ := os.ReadFile(filepath.Join(dir, "pwd"))
	real, _ := filepath.EvalSymlinks(work)
	if strings.TrimSpace(string(pwd)) != real {
		t.Errorf("ran in %q, want %q", strings.TrimSpace(string(pwd)), real)
	}
}

// Without a Dir it runs in an empty folder under the temp directory, with
// the model "haiku" and the binary from REXQ_CLAUDE.
func TestClaudeCompatDefaults(t *testing.T) {
	bin, dir := fakeClaudeEnv(t)
	t.Setenv(ClaudeEnv, bin)
	if _, err := (Claude{}).Run(context.Background(), "p", "{}"); err != nil {
		t.Fatal(err)
	}
	pwd, _ := os.ReadFile(filepath.Join(dir, "pwd"))
	want, _ := filepath.EvalSymlinks(filepath.Join(os.TempDir(), "rexq-extract"))
	if strings.TrimSpace(string(pwd)) != want {
		t.Errorf("ran in %q, want %q", strings.TrimSpace(string(pwd)), want)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.HasPrefix(string(raw), "-p\x00--model\x00haiku\x00") {
		t.Errorf("args %q", raw)
	}
}
