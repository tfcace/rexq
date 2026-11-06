package hook

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Repo names the project and branch for cwd. It understands jj and git and
// falls back to the directory name.
func Repo(cwd string) (project, branch string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run := func(name string, args ...string) string {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	// --ignore-working-copy: a hook must not snapshot the agent's working
	// copy or wait on its lock.
	jj := func(args ...string) string {
		return run("jj", append([]string{"--ignore-working-copy"}, args...)...)
	}
	root := run("git", "rev-parse", "--show-toplevel")
	if root == "" {
		root = jj("root")
	}
	if root == "" {
		root = cwd
	}
	branch = run("git", "branch", "--show-current")
	if branch == "" {
		// The working copy is usually a new commit on top of the bookmark,
		// so name the nearest bookmarked ancestor.
		branch = jj("log", "--no-graph", "-r", "latest(heads(::@ & bookmarks()))",
			"-T", `local_bookmarks.map(|b| b.name()).join(",")`)
	}
	if root == "" {
		return "", branch
	}
	return filepath.Base(root), branch
}
