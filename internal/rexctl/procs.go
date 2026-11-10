package rexctl

import (
	"context"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/tfcace/rexq/internal/agent"
)

// process is one line of `ps -A -o pid=,ppid=,stat=,comm=`.
type process struct {
	pid, ppid int
	stat      string // "T" in it means stopped
	comm      string // the executable's path, or argv[0] for login shells ("-zsh")
}

type processes map[int]process

// listProcs runs ps directly, without a shell.
func listProcs(ctx context.Context) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,stat=,comm=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return out, nil
}

// parseProcs reads listProcs' output. A command may contain spaces, so it's
// everything after the third field.
func parseProcs(out []byte) (processes, error) {
	ps := processes{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 4 {
			return nil, fmt.Errorf("ps: unexpected line %q", line)
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("ps: unexpected line %q", line)
		}
		ps[pid] = process{pid: pid, ppid: ppid, stat: f[2], comm: strings.Join(f[3:], " ")}
	}
	return ps, nil
}

// tree returns root and its descendants, nearest first. It's empty when
// root isn't running.
func (ps processes) tree(root int) []process {
	r, ok := ps[root]
	if !ok {
		return nil
	}
	children := map[int][]int{}
	for _, p := range ps {
		children[p.ppid] = append(children[p.ppid], p.pid)
	}
	for _, c := range children {
		slices.Sort(c)
	}
	out := []process{r}
	seen := map[int]bool{root: true}
	for i := 0; i < len(out); i++ {
		for _, c := range children[out[i].pid] {
			if !seen[c] {
				seen[c] = true
				out = append(out, ps[c])
			}
		}
	}
	return out
}

// atPrompt reports whether tree (from processes.tree) is only a shell
// waiting at its prompt: the root alone, or login with exactly one child
// and nothing under it.
func atPrompt(tree []process) bool {
	switch len(tree) {
	case 1:
		return true
	case 2:
		return path.Base(tree[0].comm) == "login"
	}
	return false
}

// listCwds runs lsof directly, without a shell, for pids' working
// directories. lsof exits 1 when a pid is gone, so its output comes back
// with the error.
func listCwds(ctx context.Context, pids []int) ([]byte, error) {
	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}
	out, err := exec.CommandContext(ctx, "lsof", "-a", "-d", "cwd", "-Fn", "-p", strings.Join(list, ",")).Output()
	if err != nil {
		return out, fmt.Errorf("lsof: %w", err)
	}
	return out, nil
}

// parseCwds reads `lsof -Fn` output: a "p<pid>" line, then the pid's files
// as "n<name>" lines (here only its cwd).
func parseCwds(out []byte) map[int]string {
	cwds := map[int]string{}
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid > 0:
			cwds[pid] = line[1:]
		}
	}
	return cwds
}

// samePath canonicalizes a path for comparison: cleaned, with symlinks
// resolved when it can be.
func samePath(p string) string {
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// ClaudeRunningIn reports whether any Claude Code process on this machine
// runs in cwd. See RunningIn.
func (r Rex) ClaudeRunningIn(ctx context.Context, cwd string) (bool, error) {
	return r.RunningIn(ctx, agent.Claude, cwd)
}

// RunningIn reports whether any process of agent a on this machine,
// interactive or headless, runs in cwd. Only a's processes count. When it
// can't tell, it returns an error.
//
// Directories match the way the agent resumes a conversation (see
// Agent.SameProject): for Claude, by the name of their folder under
// ~/.claude/projects, so /p/app-demo, /p/app/demo and /p/app.demo count as
// one.
func (r Rex) RunningIn(ctx context.Context, a agent.Agent, cwd string) (bool, error) {
	list := r.Procs
	if list == nil {
		list = listProcs
	}
	claudes, err := agentPids(ctx, list, a)
	if err != nil || len(claudes) == 0 {
		return false, err
	}
	lsof := r.ProcCwds
	if lsof == nil {
		lsof = listCwds
	}
	out, lerr := lsof(ctx, claudes)
	cwds := parseCwds(out)
	want := samePath(cwd)
	for _, c := range cwds {
		if a.SameProject(samePath(c), want) {
			return true, nil
		}
	}
	if lerr == nil {
		return false, nil
	}
	// lsof failed: fine only if every Claude it skipped has exited since.
	again, err := agentPids(ctx, list, a)
	if err != nil {
		return false, err
	}
	for _, pid := range again {
		if _, read := cwds[pid]; !read && slices.Contains(claudes, pid) {
			return false, lerr
		}
	}
	return false, nil
}

// agentPids lists the pids of a's processes, in order.
func agentPids(ctx context.Context, list func(context.Context) ([]byte, error), a agent.Agent) ([]int, error) {
	out, err := list(ctx)
	if err != nil {
		return nil, err
	}
	ps, err := parseProcs(out)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, p := range ps {
		if a.IsProcess(p.comm) {
			pids = append(pids, p.pid)
		}
	}
	slices.Sort(pids)
	return pids, nil
}
