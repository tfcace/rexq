// Command rexq tracks the questions coding agents leave for you.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tfcace/rexq/internal/extract"
	"github.com/tfcace/rexq/internal/hook"
	"github.com/tfcace/rexq/internal/panel"
	"github.com/tfcace/rexq/internal/rexctl"
	"github.com/tfcace/rexq/internal/store"
)

const usage = `usage:
  rexq install                  set rexq up: Claude Code's hooks and the Rex panel; install pi adds Pi (see rexq install -h)
  rexq uninstall                take them out again
  rexq hook prompt|start|stop   run as a Claude Code hook, or for the Pi extension (reads hook JSON on stdin)
  rexq panel [--layer]          open the questions panel (--layer: as the Rex floating layer)
  rexq list [-all]              print open questions
  rexq backfill [flags]         extract questions from past transcripts (--pi: Pi's)
  rexq config get|set pi-model  the model Pi extractions run with (see rexq config)`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "hook":
		hookMain(os.Args[2:]) // never returns an error: hooks must exit 0
		return
	case "install", "uninstall":
		if len(os.Args) > 2 && (os.Args[2] == "-h" || os.Args[2] == "--help" || os.Args[2] == "help") {
			fmt.Println(installUsage)
			return
		}
		if os.Args[1] == "install" {
			err = installMain(os.Args[2:])
		} else {
			err = uninstallMain(os.Args[2:])
		}
	case "list":
		err = listMain(os.Args[2:])
	case "backfill":
		err = backfillMain(os.Args[2:])
	case "config":
		err = configMain(os.Args[2:])
	case "panel":
		var layer bool
		if layer, err = panelLayer(os.Args[2:], os.Getenv); err != nil {
			break
		}
		s := openStore()
		defer s.Close()
		err = panel.Run(panel.Deps{Store: s, Rex: rexctl.New(rexctl.Exec), RexSession: os.Getenv("REX_SESSION"),
			InRex: layer, Now: time.Now})
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rexq:", err)
		os.Exit(1)
	}
}

// panelLayer reads the panel's flags and reports whether it runs as the
// Rex floating layer, which q hides rather than quits. Only the Rex action
// passes --layer; a panel started by hand in a Rex pane quits on q, since
// hiding would detach the pane it runs in.
func panelLayer(args []string, getenv func(string) string) (bool, error) {
	fs := flag.NewFlagSet("panel", flag.ContinueOnError)
	layer := fs.Bool("layer", false, "run as the Rex floating layer (the rexq.lua action passes it)")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	return *layer && getenv("REX_SESSION") != "" && getenv("REX_BLOCK") != "", nil
}

func openStore() *store.Store {
	s, err := store.Open(store.DefaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "rexq:", err)
		os.Exit(1)
	}
	return s
}

// hookMain always exits 0. A Stop hook that exits 2 makes Claude keep
// working, and a hook error should never disturb the agent.
func hookMain(args []string) {
	logf := hookLog()
	defer func() {
		if r := recover(); r != nil {
			logf("panic: %v", r)
		}
		os.Exit(0)
	}()
	if len(args) != 1 {
		logf("usage: rexq hook prompt|start|stop")
		return
	}
	// Claude waits for the synchronous hooks and kills them after their
	// timeout (5 s), so they must not spend it waiting on a busy store.
	busy := syncBusyTimeout
	if args[0] == "stop" {
		busy = store.DefaultBusyTimeout
	}
	s, err := store.OpenWith(store.DefaultPath(), busy)
	if err != nil {
		logf("open store: %v", err)
		return
	}
	defer s.Close()
	state := filepath.Dir(store.DefaultPath())
	d := hook.Deps{
		Store: s, Runner: extract.Claude{}, Now: time.Now, Env: os.Getenv, Repo: hook.Repo,
		Locks: hook.FileLock(filepath.Join(state, "locks")),
	}
	switch args[0] {
	case "prompt":
		err = hook.Prompt(d, os.Stdin)
	case "start":
		err = hook.Start(d, os.Stdin)
	case "stop":
		// Closing the pane signals the hook. The signal ends the extraction
		// instead of the process, so the turn is queued for a retry (one
		// quick write) rather than lost.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		err = hook.Stop(ctx, d, os.Stdin)
	default:
		err = fmt.Errorf("unknown hook %q", args[0])
	}
	if err != nil {
		logf("%s: %v", args[0], err)
	}
}

// syncBusyTimeout is the busy timeout of the synchronous hooks, well within
// the 5 s Claude gives them.
const syncBusyTimeout = time.Second

// hookLog returns a logger that appends to hook.log beside the store.
func hookLog() func(string, ...any) {
	return logTo(filepath.Join(filepath.Dir(store.DefaultPath()), "hook.log"))
}

// maxLog is the size past which the hook log moves to hook.log.1 and starts
// over, so it never holds much more than twice this.
const maxLog = 1 << 20

// flatten keeps a log entry on one line.
var flatten = strings.NewReplacer("\r\n", " | ", "\n", " | ", "\r", " | ")

// logTo returns a logger that appends one line per entry to path. It drops
// what it can't write: a hook has nowhere else to report. Two hooks rotating
// at once can lose the older half of the log, which is fine for a log.
func logTo(path string) func(string, ...any) {
	return func(format string, a ...any) {
		if fi, err := os.Stat(path); err == nil && fi.Size() >= maxLog {
			os.Rename(path, path+".1")
		}
		// 0600: the log quotes agents' errors. A log from before is
		// tightened.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o077 != 0 {
			f.Chmod(fi.Mode().Perm() &^ 0o077)
		}
		// Sanitize: the entry may quote an agent's or claude's output, and
		// escape sequences in it would act on the terminal of whoever reads
		// the log.
		fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), store.Sanitize(flatten.Replace(fmt.Sprintf(format, a...))))
	}
}

func listMain(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	all := fs.Bool("all", false, "every Rex session, not just this one")
	fs.Parse(args)
	s := openStore()
	defer s.Close()
	scope := os.Getenv("REX_SESSION")
	if *all {
		scope = ""
	}
	items, err := s.List(scope)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, it := range items {
		mark := " "
		if it.Snoozed(now) {
			mark = "z"
		}
		fmt.Printf("%s %-4d %-9s %-12s %s\n", mark, it.ID, it.Kind, it.Project, it.Title)
	}
	return nil
}
