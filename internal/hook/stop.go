package hook

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tfcace/rexq/internal/extract"
	"github.com/tfcace/rexq/internal/filter"
	"github.com/tfcace/rexq/internal/images"
	"github.com/tfcace/rexq/internal/store"
	"github.com/tfcace/rexq/internal/transcript"
)

type stopInput struct {
	SessionID            string `json:"session_id"`
	PromptID             string `json:"prompt_id"`
	Cwd                  string `json:"cwd"`
	TranscriptPath       string `json:"transcript_path"`
	AgentID              string `json:"agent_id"`
	LastAssistantMessage string `json:"last_assistant_message"`
	// Captured when the hook fires, so a queued retry still knows where the
	// agent was and what the user had asked in that turn.
	RexSession string `json:"rex_session"`
	RexBlock   string `json:"rex_block"`
	Prompt     string `json:"rex_prompt"`
	// EndedAt is when the hook fired, in Unix seconds: when the agent
	// stopped and asked. Items are dated by it, not by when extraction
	// finished, so a reply typed meanwhile counts as written after them.
	EndedAt int64 `json:"rex_ended_at"`
	// Looked up when there are items to store, or when the turn is queued,
	// so a retry hours later still names the branch of its turn.
	HaveRepo bool   `json:"rex_have_repo"`
	Project  string `json:"rex_project"`
	Branch   string `json:"rex_branch"`
	// LaunchDir is where the conversation was started, worked out with
	// the repository; see transcript.LaunchDir. Cwd is only where the agent
	// was when it stopped.
	LaunchDir string `json:"rex_launch_dir"`
}

const (
	// retryTimeout bounds each retry of a queued turn.
	retryTimeout = 60 * time.Second
	// otherRetries is how many other sessions' queued turns one Stop retries.
	otherRetries = 3
)

// Stop handles the Stop hook. Register it with "async": true, so Claude
// never waits for the model.
//
// Under the session's lock it first retries the session's own queued turns,
// oldest first, then runs the turn that just ended, so turns are extracted
// in the order they ended, as long as they succeed. A turn that fails for any reason is queued. Then
// it retries a few queued turns of other sessions that aren't busy.
func Stop(ctx context.Context, d Deps, r io.Reader) error {
	if d.skip() {
		return nil
	}
	var in stopInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return err
	}
	if in.AgentID != "" || in.SessionID == "" {
		return nil
	}
	in.EndedAt = d.Now().Unix()
	in.RexSession, in.RexBlock = d.Env("REX_SESSION"), d.Env("REX_BLOCK")
	run := &stopRun{d: d}
	// A session started before the hooks were installed never claimed its
	// pane at SessionStart. It may take the pane now if nobody owns it, so
	// its items can be answered in place. Only the live turn: a retry's pane
	// may belong to someone else by now.
	if in.RexBlock != "" {
		_, err := d.Store.ClaimUnownedPane(in.RexBlock, in.RexSession, in.SessionID, d.Now())
		run.note(err)
	}
	prompt, err := d.Store.LastPrompt(in.SessionID)
	in.Prompt = prompt
	run.note(err)
	run.session(ctx, &in)
	run.others(ctx, in.SessionID)
	return errors.Join(run.errs...)
}

// stopRun is the work of one Stop hook. After the first turn that fails it
// retries nothing more: the model or the store is likely down, and each
// retry would only wait for its timeout.
type stopRun struct {
	d      Deps
	failed bool
	errs   []error
}

// note keeps err, if any, for the log.
func (r *stopRun) note(err error) {
	if err != nil {
		r.errs = append(r.errs, err)
	}
}

// session runs the live turn and its session's queued turns, holding the
// session's lock.
func (r *stopRun) session(ctx context.Context, in *stopInput) {
	unlock, err := r.d.Locks.Lock(ctx, in.SessionID)
	if err != nil {
		r.failed = true
		r.note(r.queue(in, fmt.Errorf("locking session %s: %w", in.SessionID, err)))
		return
	}
	defer unlock()
	// Due or not: holding the lock, this hook is the one to run them, and
	// in order. After a failure it leaves the rest queued but still runs
	// the live turn, so a turn that always fails can't hold up the session.
	rows, err := r.d.Store.PendingFor(in.SessionID)
	r.note(err)
	for _, row := range rows {
		if r.failed {
			break
		}
		r.retry(ctx, row, r.d.Store.ClaimPendingEarly)
	}
	note, err := r.d.process(ctx, in, true)
	r.note(note)
	if err != nil {
		r.failed = true
		r.note(r.queue(in, err))
	}
}

// others retries a few queued turns of other sessions, each session's oldest
// due one. It skips a session whose lock is taken: that session's own hook
// will get to them.
func (r *stopRun) others(ctx context.Context, self string) {
	if r.failed {
		return
	}
	rows, err := r.d.Store.Pending(10, r.d.Now())
	r.note(err)
	tried := 0
	for _, row := range rows {
		if r.failed || tried == otherRetries {
			return
		}
		if row.ClaudeSession == self {
			continue
		}
		unlock, ok, err := r.d.Locks.TryLock(row.ClaudeSession)
		r.note(err)
		if !ok {
			continue
		}
		if r.retry(ctx, row, r.d.Store.ClaimPending) {
			tried++
		}
		unlock()
	}
}

// retry runs one queued turn, with its session's lock held, after taking
// it with claim. It reports whether it got to run it, rather than finding it
// claimed or gone.
func (r *stopRun) retry(ctx context.Context, row store.PendingRow, claim func(int64, time.Time) (bool, error)) bool {
	s := r.d.Store
	claimed, err := claim(row.ID, r.d.Now())
	if err != nil || !claimed {
		r.note(err)
		return false
	}
	var in stopInput
	if err := json.Unmarshal(row.Payload, &in); err != nil {
		r.note(fmt.Errorf("dropping unreadable queued turn %d: %w", row.ID, err))
		r.note(s.DonePending(row.ID))
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, retryTimeout)
	defer cancel()
	note, err := r.d.process(ctx, &in, false)
	r.note(note)
	if err != nil {
		r.failed = true
		r.note(fmt.Errorf("retrying queued turn %d: %w", row.ID, err))
		r.note(s.FailPending(row.ID))
		return true
	}
	r.note(s.DonePending(row.ID))
	return true
}

// queue stores a failed live turn for a later retry. It returns cause,
// joined with the reason if the turn couldn't be queued.
func (r *stopRun) queue(in *stopInput, cause error) error {
	r.d.lookUpRepo(in)
	payload, err := json.Marshal(in)
	if err == nil {
		err = r.d.Store.Enqueue(in.SessionID, payload, r.d.Now())
	}
	if err != nil {
		return errors.Join(cause, fmt.Errorf("queueing the turn: %w", err))
	}
	return cause
}

// lookUpRepo works out the turn's launch directory, and its project and
// branch from there.
func (d Deps) lookUpRepo(in *stopInput) {
	if in.LaunchDir == "" {
		in.LaunchDir = transcript.LaunchDir(in.TranscriptPath, in.Cwd)
	}
	if !in.HaveRepo {
		in.Project, in.Branch = d.Repo(in.LaunchDir)
		in.HaveRepo = true
	}
}

// process extracts one turn and stores what it finds. err means the turn is
// not done and must run again; Insert and Answer make that safe. note holds
// problems worth logging that don't call for running it again. live is false
// for a queued turn retried later.
func (d Deps) process(ctx context.Context, in *stopInput, live bool) (note, err error) {
	s := d.Store
	var notes []error
	if live {
		// A next-stop snooze ends when its agent stops again, not when a
		// later hook retries one of its old turns.
		notes = append(notes, s.WakeTrigger(in.SessionID, store.TriggerNextStop))
	}
	// Expired time snoozes become open again, so this turn can resolve them.
	notes = append(notes, s.WakeDue(d.Now()))

	open, err := s.OpenFor(in.SessionID)
	if err != nil {
		return errors.Join(notes...), err
	}
	msg := in.LastAssistantMessage
	if !filter.LooksOpen(msg) && len(open) == 0 {
		return errors.Join(notes...), nil
	}
	var earlier []extract.OpenItem
	for _, it := range open {
		earlier = append(earlier, extract.OpenItem{ID: it.ID, Title: it.Title})
	}
	res, err := extract.Extract(ctx, d.Runner, extract.Input{Message: msg, Prompt: in.Prompt, Open: earlier})
	// Recorded for the panel, which warns while extraction fails. A hook
	// stopped by its pane closing says nothing about extraction.
	switch {
	case err == nil:
		notes = append(notes, s.RecordExtractionOK(d.Now()))
	case !errors.Is(err, context.Canceled):
		notes = append(notes, s.RecordExtractionError(err.Error(), d.Now()))
	}
	if err != nil {
		return errors.Join(notes...), err
	}
	var errs []error
	for _, id := range res.Resolved {
		errs = append(errs, s.Answer(id, "(answered in the session)"))
	}
	// The agent asked these again; this turn's items replace them.
	// Extract keeps an id in one list only.
	for _, id := range res.Superseded {
		errs = append(errs, s.Answer(id, "(asked again)"))
	}
	if len(res.Items) == 0 {
		return errors.Join(notes...), errors.Join(errs...)
	}
	d.lookUpRepo(in)
	source := SourceMsg(in.PromptID, msg)
	shots := images.Find(msg)
	asked := d.Now()
	if in.EndedAt > 0 { // turns queued before EndedAt existed lack it
		asked = time.Unix(in.EndedAt, 0)
	}
	skipped := 0
	for i, f := range res.Items {
		// The item goes to the pane its session owns as it is stored, not
		// the one captured when the turn ended: the agent may have moved
		// (resume) or lost it (/clear) since.
		_, added, err := s.InsertFromPane(store.Item{
			RexSession: in.RexSession, RexBlock: in.RexBlock,
			ClaudeSession: in.SessionID, SourceMsg: source, Seq: i,
			Cwd: in.LaunchDir, Branch: in.Branch, Project: in.Project,
			Kind: f.Kind, Title: f.Title, Body: f.Quote, Context: f.Context, Prompt: in.Prompt,
			Replies: f.Replies, Suggested: f.Suggested, Images: shots,
			CreatedAt: asked,
		})
		if err == nil && !added {
			skipped++
		}
		errs = append(errs, err)
	}
	// Expected when a turn runs twice (a retry after a partial insert);
	// otherwise a sign that two turns got the same key.
	if skipped > 0 {
		notes = append(notes, fmt.Errorf("skipped %d duplicate items of session %s", skipped, in.SessionID))
	}
	return errors.Join(notes...), errors.Join(errs...)
}

// SourceMsg keys a turn's items: the prompt id and the start of the final
// message's SHA-1. The prompt id alone is not enough: after a compaction,
// Claude Code gives many turns the same one. A retry of the same turn gets
// the same key, so its items are not stored twice. Without a prompt id it
// is a longer hash of the message alone. Backfill uses the same formula,
// but the final message it reads from the transcript need not be byte for
// byte the hook's last_assistant_message; what keeps backfill from storing
// a turn the hook stored is that it skips sessions the store knows.
func SourceMsg(promptID, msg string) string {
	sum := sha1.Sum([]byte(msg))
	if promptID == "" {
		return hex.EncodeToString(sum[:8])
	}
	return promptID + ":" + hex.EncodeToString(sum[:4])
}
