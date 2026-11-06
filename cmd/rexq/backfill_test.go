package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tfcace/rexq/internal/hook"
	"github.com/tfcace/rexq/internal/store"
	"github.com/tfcace/rexq/internal/transcript"
)

const twoItems = `{"items":[{"kind":"decision","title":"Commit this?","quote":"Should I commit this?","context":"The fix is done.","replies":["Yes","No"],"suggested":0},
	{"kind":"leftover","title":"Docs not updated?","quote":"Docs are still rough.","context":"","replies":[],"suggested":-1}],"resolved":[]}`

// fakeRunner stands in for claude -p and counts its calls. The nth call
// (from 1) fails if fail[n] is set; every call fails if err is set.
type fakeRunner struct {
	calls   int
	prompts []string
	err     error
	fail    map[int]bool
	out     string // default twoItems
	onRun   func()
}

func (r *fakeRunner) Run(_ context.Context, prompt, _ string) ([]byte, error) {
	r.calls++
	r.prompts = append(r.prompts, prompt)
	if r.onRun != nil {
		r.onRun()
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.fail[r.calls] {
		return nil, fmt.Errorf("claude -p: overloaded")
	}
	if r.out != "" {
		return []byte(r.out), nil
	}
	return []byte(twoItems), nil
}

var backfillNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// writeSession writes a one-turn transcript for session id under the
// project directory dir of root, its last message sent at ended. Its
// prompt has the id "p-"+id and the branch is main.
func writeSession(t *testing.T, root, dir, id string, ended time.Time, entrypoint, final string) string {
	t.Helper()
	ts := func(d time.Duration) string { return ended.Add(d).UTC().Format(time.RFC3339) }
	lines := []string{
		fmt.Sprintf(`{"type":"user","sessionId":%q,"uuid":"u-%s","promptId":"p-%s","timestamp":%q,"cwd":"/work/%s","gitBranch":"main","entrypoint":%q,"message":{"role":"user","content":"fix the bug"}}`,
			id, id, id, ts(-time.Minute), id, entrypoint),
		fmt.Sprintf(`{"type":"assistant","sessionId":%q,"uuid":"a-%s","timestamp":%q,"cwd":"/work/%s","gitBranch":"main","entrypoint":%q,"message":{"id":"msg-%s","role":"assistant","content":[{"type":"text","text":%q}]}}`,
			id, id, ts(0), id, entrypoint, id, final),
	}
	path := filepath.Join(root, dir, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// rewrite replaces old with new throughout the file at path.
func rewrite(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(b), old, new)), 0o644); err != nil {
		t.Fatal(err)
	}
}

type backfillEnv struct {
	root   string
	store  *store.Store
	runner *fakeRunner
	out    bytes.Buffer
}

func newBackfillEnv(t *testing.T) *backfillEnv {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &backfillEnv{root: t.TempDir(), store: s, runner: &fakeRunner{}}
}

func (e *backfillEnv) runErr(since time.Duration, dry bool) error {
	return backfill(backfillDeps{
		Root: e.root, Store: e.store, Runner: e.runner, Now: func() time.Time { return backfillNow }, Out: &e.out,
		Repo: func(cwd string) (string, string) { return "proj-" + filepath.Base(cwd), "repo-branch" },
	}, since, dry)
}

func (e *backfillEnv) run(t *testing.T, since time.Duration, dry bool) {
	t.Helper()
	if err := e.runErr(since, dry); err != nil {
		t.Fatalf("%v\n%s", err, e.out.String())
	}
}

const asks = "Done. Should I commit this?"

var recent = backfillNow.Add(-2 * time.Hour)

// seed writes one session of each kind backfill must skip, and one it
// must extract ("want").
func seed(t *testing.T, e *backfillEnv) {
	t.Helper()
	writeSession(t, e.root, "-Users-me-app", "want0001-aaaa", recent, "cli", asks)
	writeSession(t, e.root, "-Users-me-app", "known001-aaaa", recent, "cli", asks)
	writeSession(t, e.root, "-Users-me-app", "old00001-aaaa", backfillNow.Add(-8*24*time.Hour), "cli", asks)
	writeSession(t, e.root, "-Users-me-app", "quiet001-aaaa", recent, "cli", "Done. All tests pass.")
	writeSession(t, e.root, "-Users-me-app", "headless-aaaa", recent, "sdk-cli", asks)
	writeSession(t, e.root, "-Users-me-app", "sdkts001-aaaa", recent, "sdk-ts", asks)
	writeSession(t, e.root, "-private-tmp-x", "tmp00001-aaaa", recent, "cli", asks)
	writeSession(t, e.root, "-private-tmp-x", "tmp00002-aaaa", recent, "cli", asks)
	// Benchmarks run headless, and are skipped for that, not for their name.
	writeSession(t, e.root, "-Users-me-app-bench-A", "bench001-aaaa", recent, "sdk-cli", asks)
	// Subagent transcripts sit a level deeper and are not sessions.
	writeSession(t, e.root, filepath.Join("-Users-me-app", "want0001-aaaa", "subagents"), "agent001-aaaa", recent, "cli", asks)
	// The hooks saw this one.
	if err := e.store.RecordPrompt("known001-aaaa", "hi", recent); err != nil {
		t.Fatal(err)
	}
}

func listed(id string) string {
	return fmt.Sprintf("%s  %-14s %s\n", recent.Local().Format("Jan 02"), "proj-"+id, id[:8])
}

func TestBackfillDryRunListsAndCallsNothing(t *testing.T) {
	e := newBackfillEnv(t)
	seed(t, e)
	e.run(t, 7*24*time.Hour, true)
	if e.runner.calls != 0 {
		t.Errorf("dry run made %d model calls", e.runner.calls)
	}
	want := listed("want0001-aaaa") + "skipped -private-tmp-x (temp dir)\n" + "1 session to extract, one claude -p call each\n"
	if out := e.out.String(); out != want {
		t.Errorf("output:\n%s\nwant:\n%s", out, want)
	}
	items, err := e.store.List("")
	if err != nil || len(items) != 0 {
		t.Errorf("dry run stored %d items (err %v)", len(items), err)
	}
	if known, _ := e.store.Known("want0001-aaaa"); known {
		t.Error("a dry run marked the session")
	}
}

func TestBackfillExtractsOnlyUnseenRecentInteractiveSessions(t *testing.T) {
	e := newBackfillEnv(t)
	seed(t, e)
	e.run(t, 7*24*time.Hour, false)
	if e.runner.calls != 1 {
		t.Fatalf("%d model calls, want 1:\n%s", e.runner.calls, e.out.String())
	}
	if !strings.Contains(e.runner.prompts[0], asks) || !strings.Contains(e.runner.prompts[0], "fix the bug") {
		t.Errorf("prompt lacks the final message or the user's prompt:\n%s", e.runner.prompts[0])
	}
	if want := listed("want0001-aaaa") + "   2 items\nskipped -private-tmp-x (temp dir)\n1 session, 2 items\n"; e.out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", e.out.String(), want)
	}
	items, err := e.store.List("")
	if err != nil || len(items) != 2 {
		t.Fatalf("%d items, err %v", len(items), err)
	}
	for i, it := range items {
		// Keyed by prompt id and message exactly like the hook's items; the
		// transcript's branch wins over the repository's current one.
		if it.ClaudeSession != "want0001-aaaa" || it.SourceMsg != hook.SourceMsg("p-want0001-aaaa", asks) ||
			it.RexSession != "" || it.RexBlock != "" || it.Cwd != "/work/want0001-aaaa" ||
			it.Project != "proj-want0001-aaaa" || it.Branch != "main" || it.Prompt != "fix the bug" ||
			!it.CreatedAt.Equal(recent) {
			t.Errorf("item %d: %+v", i, it)
		}
	}
	// A second run finds the session known and calls nothing.
	e.runner.calls = 0
	e.out.Reset()
	e.run(t, 7*24*time.Hour, false)
	if e.runner.calls != 0 || e.out.String() != "skipped -private-tmp-x (temp dir)\n0 sessions, 0 items\n" {
		t.Errorf("second run: %d calls, output %q", e.runner.calls, e.out.String())
	}
}

// Transcripts older than promptId fall back to the message's uuid, and a
// detached head in the transcript falls back to the repository's branch.
func TestBackfillFallbacks(t *testing.T) {
	e := newBackfillEnv(t)
	path := writeSession(t, e.root, "-Users-me-app", "want0001-aaaa", recent, "cli", asks)
	rewrite(t, path, `"promptId":"p-want0001-aaaa",`, "")
	rewrite(t, path, `"gitBranch":"main"`, `"gitBranch":"HEAD"`)
	e.run(t, 7*24*time.Hour, false)
	items, err := e.store.List("")
	if err != nil || len(items) != 2 {
		t.Fatalf("%d items, err %v", len(items), err)
	}
	if it := items[0]; it.SourceMsg != "a-want0001-aaaa" || it.Branch != "repo-branch" {
		t.Errorf("source %q branch %q", it.SourceMsg, it.Branch)
	}
}

// A session that leaves nothing is still remembered, so the next run
// doesn't pay to extract it again.
func TestBackfillRemembersSessionsWithNoItems(t *testing.T) {
	e := newBackfillEnv(t)
	writeSession(t, e.root, "-Users-me-app", "want0001-aaaa", recent, "cli", asks)
	e.runner.out = `{"items":[],"resolved":[]}`
	e.run(t, 7*24*time.Hour, false)
	if !strings.HasSuffix(e.out.String(), "   0 items\n1 session, 0 items\n") {
		t.Errorf("output:\n%s", e.out.String())
	}
	e.out.Reset()
	e.run(t, 7*24*time.Hour, true)
	if e.runner.calls != 1 || e.out.String() != "0 sessions to extract, one claude -p call each\n" {
		t.Errorf("%d calls; second run output %q", e.runner.calls, e.out.String())
	}
}

func TestBackfillSinceWidensTheCutoff(t *testing.T) {
	e := newBackfillEnv(t)
	seed(t, e)
	e.run(t, 9*24*time.Hour, true)
	out := e.out.String()
	if !strings.Contains(out, "old00001") || !strings.Contains(out, "want0001") || !strings.HasSuffix(out, "2 sessions to extract, one claude -p call each\n") {
		t.Errorf("output:\n%s", out)
	}
	e.out.Reset()
	e.run(t, time.Hour, true)
	if e.out.String() != "0 sessions to extract, one claude -p call each\n" {
		t.Errorf("1h output:\n%s", e.out.String())
	}
}

// A transcript untouched since the cutoff is not read, whatever it holds.
func TestBackfillSkipsFilesUntouchedSinceTheCutoff(t *testing.T) {
	e := newBackfillEnv(t)
	path := writeSession(t, e.root, "-Users-me-app", "want0001-aaaa", recent, "cli", asks)
	old := backfillNow.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	e.run(t, 7*24*time.Hour, true)
	if e.out.String() != "0 sessions to extract, one claude -p call each\n" {
		t.Errorf("output:\n%s", e.out.String())
	}
	// Touched again, it is read.
	if err := os.Chtimes(path, recent, recent); err != nil {
		t.Fatal(err)
	}
	e.out.Reset()
	e.run(t, 7*24*time.Hour, true)
	if !strings.Contains(e.out.String(), "want0001") {
		t.Errorf("output:\n%s", e.out.String())
	}
}

func TestBackfillReportsUnreadableTranscripts(t *testing.T) {
	e := newBackfillEnv(t)
	path := writeSession(t, e.root, "-Users-me-app", "want0001-aaaa", recent, "cli", asks)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	e.run(t, 7*24*time.Hour, true)
	if out := e.out.String(); !strings.HasPrefix(out, "skipped "+path+": ") || !strings.Contains(out, "permission denied") {
		t.Errorf("output:\n%s", out)
	}
}

// A failed extraction is reported, stores nothing and leaves the session
// unknown, so the next run tries it again. The run still ends in an error.
func TestBackfillReportsAFailedExtraction(t *testing.T) {
	e := newBackfillEnv(t)
	writeSession(t, e.root, "-Users-me-app", "a0000001-aaaa", recent, "cli", asks)
	writeSession(t, e.root, "-Users-me-app", "b0000001-aaaa", recent, "cli", asks)
	e.runner.fail = map[int]bool{1: true}
	err := e.runErr(7*24*time.Hour, false)
	if err == nil || !strings.Contains(err.Error(), "1 extraction failed") {
		t.Errorf("err %v", err)
	}
	if !strings.Contains(e.out.String(), "   skipped: claude -p: overloaded\n") || !strings.HasSuffix(e.out.String(), "2 sessions, 2 items\n") {
		t.Errorf("output:\n%s", e.out.String())
	}
	if known, _ := e.store.Known("a0000001-aaaa"); known {
		t.Error("a failed session became known")
	}
	if known, _ := e.store.Known("b0000001-aaaa"); !known || e.runner.calls != 2 {
		t.Errorf("the next session: known %v after %d calls", known, e.runner.calls)
	}
}

// Two failures in a row mean the model or the login is down: stop instead
// of paying for every remaining session.
func TestBackfillStopsAfterTwoFailuresInARow(t *testing.T) {
	e := newBackfillEnv(t)
	for _, id := range []string{"a0000001-aaaa", "b0000001-aaaa", "c0000001-aaaa", "d0000001-aaaa"} {
		writeSession(t, e.root, "-Users-me-app", id, recent, "cli", asks)
	}
	e.runner.fail = map[int]bool{1: true, 3: true, 4: true}
	err := e.runErr(7*24*time.Hour, false)
	if err == nil || !strings.Contains(err.Error(), "2 failed extractions in a row") {
		t.Errorf("err %v", err)
	}
	if e.runner.calls != 4 {
		t.Errorf("%d calls, want 4 (a fails, b works, c and d fail)", e.runner.calls)
	}
	e.runner = &fakeRunner{err: errors.New("down")}
	e.out.Reset()
	if err := e.runErr(7*24*time.Hour, false); err == nil || e.runner.calls != 2 {
		t.Errorf("all failing: %d calls, err %v", e.runner.calls, err)
	}
}

// A store error ends the run at once.
func TestBackfillStopsOnAStoreError(t *testing.T) {
	e := newBackfillEnv(t)
	writeSession(t, e.root, "-Users-me-app", "a0000001-aaaa", recent, "cli", asks)
	writeSession(t, e.root, "-Users-me-app", "b0000001-aaaa", recent, "cli", asks)
	e.runner.onRun = func() { e.store.Close() }
	if err := e.runErr(7*24*time.Hour, false); err == nil {
		t.Error("no error")
	}
	if e.runner.calls != 1 {
		t.Errorf("%d calls after the store failed", e.runner.calls)
	}
}

// Backfilled items keep the directory the conversation started in, found
// the way the Stop hook finds it. The transcript's branch only counts while
// the agent was still inside that directory.
func TestBackfillStoresTheLaunchDirectory(t *testing.T) {
	cases := []struct{ name, last, branch string }{
		{"subdirectory", "/work/app/cmd/x", "main"},
		{"another repo", "/work/other", "repo-branch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newBackfillEnv(t)
			path := writeSession(t, e.root, transcript.ProjectDirName("/work/app"), "want0001-aaaa", recent, "cli", asks)
			// The prompt was typed in the launch directory, the answer came
			// from wherever the agent had moved to.
			rewrite(t, path, `"cwd":"/work/want0001-aaaa"`, `"cwd":"/work/app"`)
			b, _ := os.ReadFile(path)
			lines := strings.SplitN(string(b), "\n", 2)
			rewrite(t, path, lines[1], strings.Replace(lines[1], `"cwd":"/work/app"`, `"cwd":"`+c.last+`"`, 1))
			e.run(t, 7*24*time.Hour, false)
			items := must(e.store.List(""))
			if len(items) != 2 {
				t.Fatalf("%d items", len(items))
			}
			if it := items[0]; it.Cwd != "/work/app" || it.Project != "proj-app" || it.Branch != c.branch {
				t.Errorf("cwd %q project %q branch %q", it.Cwd, it.Project, it.Branch)
			}
		})
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Claude Code keeps its projects under CLAUDE_CONFIG_DIR when it is set.
func TestProjectsDirHonorsClaudeConfigDir(t *testing.T) {
	t.Setenv("HOME", "/Users/me")
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got, err := projectsDir(getenv); err != nil || got != "/Users/me/.claude/projects" {
		t.Errorf("default: %q %v", got, err)
	}
	env["CLAUDE_CONFIG_DIR"] = "/tmp/claude-config"
	if got, err := projectsDir(getenv); err != nil || got != "/tmp/claude-config/projects" {
		t.Errorf("CLAUDE_CONFIG_DIR: %q %v", got, err)
	}
}

// A directory named like a benchmark is skipped only when its sessions are
// headless: an interactive one there is the user's.
func TestBackfillKeepsInteractiveSessionsInBenchDirs(t *testing.T) {
	e := newBackfillEnv(t)
	writeSession(t, e.root, "-Users-me-workbench", "bench001-aaaa", recent, "cli", asks)
	e.run(t, 7*24*time.Hour, true)
	if want := listed("bench001-aaaa") + "1 session to extract, one claude -p call each\n"; e.out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", e.out.String(), want)
	}
}
