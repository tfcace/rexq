package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tfcace/rexq/internal/extract"
	"github.com/tfcace/rexq/internal/filter"
	"github.com/tfcace/rexq/internal/hook"
	"github.com/tfcace/rexq/internal/images"
	"github.com/tfcace/rexq/internal/rexctl"
	"github.com/tfcace/rexq/internal/store"
	"github.com/tfcace/rexq/internal/transcript"
)

func backfillMain(args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	since := fs.Duration("since", 7*24*time.Hour, "only sessions whose last turn is newer than this")
	dry := fs.Bool("dry-run", false, "list what would be extracted, call nothing")
	fs.Parse(args)

	root, err := projectsDir(os.Getenv)
	if err != nil {
		return err
	}
	s := openStore()
	defer s.Close()
	return backfill(backfillDeps{
		Root: root, Store: s, Runner: extract.Claude{},
		Repo: hook.Repo, Now: time.Now, Out: os.Stdout,
	}, *since, *dry)
}

// projectsDir is Claude Code's projects directory: under CLAUDE_CONFIG_DIR
// when it is set, as Claude Code itself does, else under ~/.claude.
func projectsDir(getenv func(string) string) (string, error) {
	if dir := getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// backfillDeps are backfill's collaborators, replaceable in tests.
type backfillDeps struct {
	Root   string // Claude Code's projects directory
	Store  *store.Store
	Runner extract.Runner
	Repo   func(cwd string) (project, branch string)
	Now    func() time.Time
	Out    io.Writer
}

// backfillTimeout bounds one session's extraction.
const backfillTimeout = 2 * time.Minute

// maxFailures is how many extractions in a row may fail before backfill
// gives up: by then the model or the login is likely down.
const maxFailures = 2

// backfill extracts the last turn of each recent session the store doesn't
// know, one session at a time. Earlier turns were followed by a reply from
// the user, so they were not left hanging. With dry it only lists them.
//
// It returns an error if any extraction failed, and stops at once on a
// store error. A failed session stays unknown, so the next run retries it.
func backfill(d backfillDeps, since time.Duration, dry bool) error {
	// One level down only: subagent transcripts live deeper.
	files, err := filepath.Glob(filepath.Join(d.Root, "*", "*.jsonl"))
	if err != nil {
		return err
	}
	cutoff := d.Now().Add(-since)
	var sessions, items, failed, inARow int
	var lastErr error
	skippedDirs := map[string]bool{}
	for _, f := range files {
		// Transcripts only grow, so one untouched since the cutoff can't
		// end in a newer turn. Saves reading a gigabyte of old sessions.
		if fi, err := os.Stat(f); err != nil || fi.ModTime().Before(cutoff) {
			continue
		}
		turn, ok, err := transcript.LastTurn(f)
		if err != nil {
			fmt.Fprintf(d.Out, "skipped %s: %v\n", f, err)
			continue
		}
		if !ok || turn.Time.Before(cutoff) || !filter.LooksOpen(turn.Final) {
			continue
		}
		// Headless runs (claude -p, the SDK) are scripts and benchmarks; the
		// hooks skip them too.
		if strings.HasPrefix(turn.Entrypoint, "sdk-") {
			continue
		}
		// The hooks or an earlier backfill already saw this session.
		known, err := d.Store.Known(turn.SessionID)
		if err != nil {
			return err
		}
		if known {
			continue
		}
		// Sessions in temp dirs (/private/var/folders, /private/tmp) are
		// throwaway runs whose directory may be gone by now. Said once per
		// directory, so a session that matters can be resumed by hand.
		if dir := filepath.Base(filepath.Dir(f)); strings.HasPrefix(dir, "-private") {
			if !skippedDirs[dir] {
				skippedDirs[dir] = true
				fmt.Fprintf(d.Out, "skipped %s (temp dir)\n", dir)
			}
			continue
		}
		sessions++
		// Where the conversation started, as the Stop hook works it out:
		// claude --resume needs it, and the agent may have moved since.
		launch := transcript.LaunchDir(f, turn.Cwd)
		project, branch := d.Repo(launch)
		fmt.Fprintf(d.Out, "%s  %-14s %s\n", turn.Time.Local().Format("Jan 02"), project, rexctl.Short(turn.SessionID))
		if dry {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), backfillTimeout)
		res, err := extract.Extract(ctx, d.Runner, extract.Input{Message: turn.Final, Prompt: turn.Prompt})
		cancel()
		if err != nil {
			fmt.Fprintln(d.Out, "   skipped:", err)
			failed++
			inARow++
			lastErr = err
			if inARow == maxFailures {
				return fmt.Errorf("stopped after %d failed extractions in a row: %w", inARow, err)
			}
			continue
		}
		inARow = 0
		// The transcript names the branch the turn ran on; the repository
		// only knows the branch it is on now. A detached head says nothing,
		// and neither does a branch of a directory outside the launch one.
		if turn.Branch != "" && turn.Branch != "HEAD" && within(turn.Cwd, launch) {
			branch = turn.Branch
		}
		// Keyed like the hook's items, so the same turn can't be stored
		// twice. Old transcripts lack the prompt id.
		source := turn.MsgUUID
		if turn.PromptID != "" {
			source = hook.SourceMsg(turn.PromptID, turn.Final)
		}
		shots := images.Find(turn.Final)
		var found []store.Item
		for i, it := range res.Items {
			// No Rex session or pane: List shows the item in every Rex
			// session's panel, and answering it resumes the conversation in
			// a new tab (or types into its pane, once SessionStart claims one).
			found = append(found, store.Item{
				ClaudeSession: turn.SessionID, SourceMsg: source, Seq: i,
				Cwd: launch, Branch: branch, Project: project,
				Kind: it.Kind, Title: it.Title, Body: it.Quote, Context: it.Context, Prompt: turn.Prompt,
				Replies: it.Replies, Suggested: it.Suggested, Images: shots, CreatedAt: turn.Time,
			})
		}
		// Marked even with no items, so the next run doesn't pay again.
		added, err := d.Store.InsertBackfill(turn.SessionID, found, d.Now())
		if err != nil {
			return err
		}
		items += added
		fmt.Fprintf(d.Out, "   %s\n", plural(added, "item"))
	}
	if dry {
		fmt.Fprintf(d.Out, "%s to extract, one claude -p call each\n", plural(sessions, "session"))
	} else {
		fmt.Fprintf(d.Out, "%s, %s\n", plural(sessions, "session"), plural(items, "item"))
	}
	if failed > 0 {
		return fmt.Errorf("%s failed, the last with: %w", plural(failed, "extraction"), lastErr)
	}
	return nil
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// plural returns n and noun, with an s unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
