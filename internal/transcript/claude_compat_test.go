package transcript

// Characterization tests: they pin how a Claude conversation's launch
// directory is worked out. They use only APIs that existed before Pi
// support, so they also run against the commit before it.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeCompatLaunchDir(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "code", "rex.app")
	write := func(dir, name, body string) string {
		p := filepath.Join(root, "projects", dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	named := write(ProjectDirName(repo), "a.jsonl", `{"type":"user","cwd":"/first/cwd"}`+"\n")
	short := write("-shortened", "b.jsonl", `{"type":"mode"}`+"\n"+`{"type":"user","cwd":"/first/cwd"}`+"\n")
	empty := write("-empty", "c.jsonl", `{"type":"mode"}`+"\n")
	for name, c := range map[string]struct{ path, cwd, want string }{
		"no transcript":             {"", "/x/y", "/x/y"},
		"the launch dir itself":     {named, repo, repo},
		"a subdirectory":            {named, filepath.Join(repo, "internal", "hook"), repo},
		"left the launch dir":       {named, "/somewhere/else", "/first/cwd"},
		"no cwd: the first entry's": {named, "", "/first/cwd"},
		"folder name doesn't match": {short, filepath.Join(repo, "x"), "/first/cwd"},
		"nothing to go on":          {empty, "/x/y", "/x/y"},
		"unreadable transcript":     {filepath.Join(root, "gone.jsonl"), "/x/y", "/x/y"},
	} {
		if got := LaunchDir(c.path, c.cwd); got != c.want {
			t.Errorf("%s: got %q want %q", name, got, c.want)
		}
	}
}

func TestClaudeCompatProjectDirName(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/me/code/rex.app": "-Users-me-code-rex-app",
		"/p/app_demo":            "-p-app-demo",
		"/p/ünï":                 "-p--n-",
	} {
		if got := ProjectDirName(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
