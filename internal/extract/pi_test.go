package extract

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakePi writes a stand-in for pi: it appends its NUL-separated arguments,
// one run per line, to dir/args, saves its guard and Rex variables, and
// runs body with the model in $MODEL.
func fakePi(t *testing.T, body string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "pi")
	script := "#!/bin/sh\nd=\"${0%/*}\"\nprintf '%s\\0' \"$@\" >> \"$d/args\"\necho >> \"$d/args\"\n" +
		"printf '%s|%s|%s' \"$REXQ_EXTRACTOR\" \"${REX_SESSION-unset}\" \"${REX_BLOCK-unset}\" > \"$d/env\"\n" +
		"MODEL=''\nwhile [ $# -gt 0 ]; do case \"$1\" in --model) MODEL=\"$2\";; --) break;; esac; shift; done\n" +
		body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func runs(t *testing.T, dir string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\x00\n") {
		if line != "" {
			out = append(out, strings.Split(strings.TrimSuffix(line, "\x00"), "\x00"))
		}
	}
	return out
}

func TestPiRunArgsAndEnvironment(t *testing.T) {
	t.Setenv("REX_SESSION", "session:a")
	t.Setenv("REX_BLOCK", "block:1")
	bin, dir := fakePi(t, `echo '{"items":[],"resolved":[]}'`)
	got, err := Pi{Bin: bin, Models: []string{"anthropic/claude-haiku-4-5"}, Dir: t.TempDir()}.Run(context.Background(), "-the prompt", `{"s":1}`)
	if err != nil || string(got) != `{"items":[],"resolved":[]}` {
		t.Fatalf("got %s, %v", got, err)
	}
	r := runs(t, dir)
	if len(r) != 1 {
		t.Fatalf("%d runs", len(r))
	}
	want := []string{"-p", "--no-session", "--no-extensions", "--no-skills", "--no-prompt-templates",
		"--no-context-files", "--no-approve", "--no-mcp", "--no-tools", "--no-themes", "--offline",
		"--thinking", "low", "--provider", "anthropic", "--model", "claude-haiku-4-5", "--",
		"-the prompt" + piSchemaNote + `{"s":1}` + "\n"}
	if !reflect.DeepEqual(r[0], want) {
		t.Errorf("argv\n got %q\nwant %q", r[0], want)
	}
	if env, _ := os.ReadFile(filepath.Join(dir, "env")); string(env) != "1|unset|unset" {
		t.Errorf("guard|REX_SESSION|REX_BLOCK = %q", env)
	}
}

func TestPiRunBareModelAndREXQ_PI(t *testing.T) {
	bin, dir := fakePi(t, `echo '{}'`)
	t.Setenv(PiEnv, bin)
	if _, err := (Pi{Models: []string{"haiku"}}).Run(context.Background(), "p", "{}"); err != nil {
		t.Fatal(err)
	}
	r := runs(t, dir)
	if len(r) != 1 || slicesIndex(r[0], "--provider") >= 0 || r[0][slicesIndex(r[0], "--model")+1] != "haiku" {
		t.Fatalf("args %q", r)
	}
	// A model id with a slash after the provider keeps it.
	bin, dir = fakePi(t, `echo '{}'`)
	if _, err := (Pi{Bin: bin, Models: []string{"openrouter/anthropic/claude-haiku-4.5"}}).Run(context.Background(), "p", "{}"); err != nil {
		t.Fatal(err)
	}
	a := runs(t, dir)[0]
	if a[slicesIndex(a, "--provider")+1] != "openrouter" || a[slicesIndex(a, "--model")+1] != "anthropic/claude-haiku-4.5" {
		t.Fatalf("args %q", a)
	}
}

func slicesIndex(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func TestPiRunReadsTheJSONOutOfTheReply(t *testing.T) {
	for reply, want := range map[string]string{
		`{"items":[]}`:                                `{"items":[]}`,
		"```json\n{\"items\":[]}\n```":                `{"items":[]}`,
		"Here you go:\n```\n{\"a\":{\"b\":1}}\n```\n": `{"a":{"b":1}}`,
		"Sure. {\"items\":[]} Done.":                  `{"items":[]}`,
	} {
		bin, _ := fakePi(t, "cat <<'EOF'\n"+reply+"\nEOF")
		got, err := Pi{Bin: bin, Models: []string{"m"}}.Run(context.Background(), "p", "{}")
		if err != nil || string(got) != want {
			t.Errorf("%q: got %s %v", reply, got, err)
		}
	}
	bin, _ := fakePi(t, `echo 'I cannot help with that.'`)
	if _, err := (Pi{Bin: bin, Models: []string{"m"}}).Run(context.Background(), "p", "{}"); err == nil || !strings.Contains(err.Error(), "no JSON") {
		t.Fatalf("err %v", err)
	}
}

// A model pi rejects is retried once with the next, the session's own.
func TestPiRunFallsBackWhenTheModelIsRejected(t *testing.T) {
	bin, dir := fakePi(t, `if [ "$MODEL" = gone-model ]; then
  echo 'Warning: Model "gone-model" not found for provider "anthropic". Using custom model id.' >&2
  echo '404 {"type":"error","error":{"type":"not_found_error","message":"model: gone-model"}}' >&2
  exit 1
fi
echo '{"items":[],"resolved":[]}'`)
	got, err := Pi{Bin: bin, Models: []string{"anthropic/gone-model", "anthropic/claude-opus-5"}}.Run(context.Background(), "p", "{}")
	if err != nil || string(got) != `{"items":[],"resolved":[]}` {
		t.Fatalf("got %s %v", got, err)
	}
	if r := runs(t, dir); len(r) != 2 {
		t.Fatalf("%d runs, want 2", len(r))
	}
}

// Any other failure is not retried with another model, and a persistent
// rejection reports both attempts.
func TestPiRunErrors(t *testing.T) {
	bin, dir := fakePi(t, `echo 'fetch failed: ENOTFOUND api.example' >&2; exit 1`)
	_, err := Pi{Bin: bin, Models: []string{"a/m1", "a/m2"}}.Run(context.Background(), "p", "{}")
	if err == nil || !strings.Contains(err.Error(), "pi -p (a/m1)") || !strings.Contains(err.Error(), "ENOTFOUND") {
		t.Fatalf("err %v", err)
	}
	if r := runs(t, dir); len(r) != 1 {
		t.Fatalf("retried a non-model failure: %d runs", len(r))
	}

	bin, dir = fakePi(t, `echo "Model \"$MODEL\" not found" >&2; exit 1`)
	_, err = Pi{Bin: bin, Models: []string{"a/m1", "a/m2"}}.Run(context.Background(), "p", "{}")
	if err == nil || !strings.Contains(err.Error(), "pi -p (a/m1)") || !strings.Contains(err.Error(), "; then pi -p (a/m2)") {
		t.Fatalf("err %v", err)
	}
	if r := runs(t, dir); len(r) != 2 {
		t.Fatalf("%d runs, want 2", len(r))
	}

	if _, err := (Pi{Bin: bin}).Run(context.Background(), "p", "{}"); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("no models: %v", err)
	}
	if _, err := (Pi{Bin: filepath.Join(t.TempDir(), "missing"), Models: []string{"m"}}).Run(context.Background(), "p", "{}"); err == nil {
		t.Fatal("missing binary: no error")
	}
}

// The precedence: REXQ_PI_MODEL, then the installed setting, then the cheap
// model for the session's provider, then the session's model, which is
// always the fallback.
func TestPiModels(t *testing.T) {
	for name, c := range map[string]struct {
		in   ModelChoice
		want []string
	}{
		"env wins":           {ModelChoice{Env: "openai/gpt-5-nano", Configured: "x/y", Provider: "anthropic", Session: "claude-opus-5"}, []string{"openai/gpt-5-nano", "anthropic/claude-opus-5"}},
		"then the setting":   {ModelChoice{Configured: "google/gemini-2.5-flash", Provider: "anthropic", Session: "claude-opus-5"}, []string{"google/gemini-2.5-flash", "anthropic/claude-opus-5"}},
		"then cheap":         {ModelChoice{Provider: "anthropic", Session: "claude-opus-5"}, []string{"anthropic/claude-haiku-5-5", "anthropic/claude-opus-5"}},
		"then the session":   {ModelChoice{Provider: "some-local", Session: "qwen-coder"}, []string{"some-local/qwen-coder"}},
		"cheap is the model": {ModelChoice{Provider: "anthropic", Session: "claude-haiku-5-5"}, []string{"anthropic/claude-haiku-5-5"}},
		"blank env ignored":  {ModelChoice{Env: "  ", Configured: "a/b", Provider: "p", Session: "s"}, []string{"a/b", "p/s"}},
		"nothing known":      {ModelChoice{}, nil},
		"no session model":   {ModelChoice{Provider: "openai"}, []string{"openai/gpt-5-mini"}},
	} {
		if got := PiModels(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q want %q", name, got, c.want)
		}
	}
}

// Extract works the same through the Pi runner.
func TestExtractThroughPi(t *testing.T) {
	bin, _ := fakePi(t, "cat <<'EOF'\n```json\n"+`{"items":[{"kind":"decision","title":" Ship it? ","quote":"Ship?","context":"","replies":["Ship"],"suggested":0}],"resolved":[],"superseded":[]}`+"\n```\nEOF")
	res, err := Extract(context.Background(), Pi{Bin: bin, Models: []string{"m"}}, Input{Message: "Ship?"})
	if err != nil || len(res.Items) != 1 || res.Items[0].Title != "Ship it?" {
		t.Fatalf("got %+v %v", res, err)
	}
}
