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

	"github.com/tfcace/rexq/internal/agent"
	"github.com/tfcace/rexq/internal/extract"
	"github.com/tfcace/rexq/internal/filter"
	"github.com/tfcace/rexq/internal/images"
	"github.com/tfcace/rexq/internal/store"
)

type stopInput struct {
	SessionID            string `json:"session_id"`
	PromptID             string `json:"prompt_id"`
	Cwd                  string `json:"cwd"`
	TranscriptPath       string `json:"transcript_path"`
	AgentID              string `json:"agent_id"`
	LastAssistantMessage string `json:"last_assistant_message"`
	// Agent is the agent's id (see package agent); Claude Code sends none.
	// The rest of this block only comes from agents other than Claude:
	// the turn's prompt as the agent saw it, and the session's provider
	// and model.
	Agent      string `json:"agent,omitempty"`
	TurnPrompt string `json:"prompt,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	// Env holds the agent's TurnEnv as they were when the turn ended, so
	// a retry from another hook's environment runs as the turn would have.
	Env map[string]string `json:"rex_env,omitempty"`
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
	a, ok := agent.ByID(in.Agent)
	if !ok {
		return fmt.Errorf("stop: unknown agent %q", in.Agent)
	}
	in.EndedAt = d.Now().Unix()
	in.RexSession, in.RexBlock = d.Env("REX_SESSION"), d.Env("REX_BLOCK")
	for _, k := range a.TurnEnv {
		if v := d.Env(k); v != "" {
			if in.Env == nil {
				in.Env = map[string]string{}
			}
			in.Env[k] = v
		}
	}
	run := &stopRun{d: d}
	if a.ModelSetting != "" {
		// Pi's first-run picker may be open: the user hasn't seen the
		// model yet, so nothing of this agent is extracted until they
		// answer it (or it is abandoned; see store.AskWait).
		st, err := d.Store.AskState(a.ModelSetting, d.Now())
		run.note(err)
		run.waiting = st == store.AskOpen
	}
	// A session started before the hooks were installed never claimed its
	// pane at SessionStart. It may take the pane now if nobody owns it, so
	// its items can be answered in place. Only the live turn: a retry's pane
	// may belong to someone else by now.
	if in.RexBlock != "" {
		_, err := d.Store.ClaimUnownedPane(in.RexBlock, in.RexSession, in.SessionID, d.Now())
		run.note(err)
	}
	if a.StopSendsPrompt {
		in.Prompt = in.TurnPrompt
	} else {
		prompt, err := d.Store.LastPrompt(in.SessionID)
		in.Prompt = prompt
		run.note(err)
	}
	run.session(ctx, &in)
	run.others(ctx, in.SessionID, a)
	return errors.Join(run.errs...)
}

// stopRun is the work of one Stop hook. After the first turn that fails it
// retries nothing more: the model or the store is likely down, and each
// retry would only wait for its timeout.
type stopRun struct {
	d      Deps
	failed bool
	errs   []error
	// waiting is set while the user is being asked for the agent's
	// extraction model: the live turn is queued, nothing is extracted.
	waiting bool
}

// errWaitingForModel queues a turn while the model picker is open.
var errWaitingForModel = errors.New("waiting for the extraction model to be chosen in Pi (/rexq-model); the turn is queued")

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
	if r.waiting {
		r.failed = true // retry nothing else either
		r.note(r.queue(in, errWaitingForModel))
		return
	}
	// Due or not: holding the lock, this hook is the one to run them, and
	// in order. After a failure it leaves the rest queued but still runs
	// the live turn, so a turn that always fails can't hold up the session.
	rows, err := r.d.Store.PendingFor(in.SessionID)
	r.note(err)
	for _, row := range rows {
		if r.failed {
			break
		}
		if !r.mayRetry(row, in.agent()) {
			continue
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

// mayRetry reports whether this hook, which runs for agent a, may retry the
// queued turn row: only turns of a, whose runner needs a's environment. A
// turn of an agent this rexq doesn't know is left queued, with a note. An
// unreadable turn counts as Claude's, as before there were agents; retry
// drops it.
func (r *stopRun) mayRetry(row store.PendingRow, a agent.Agent) bool {
	var p struct {
		Agent string `json:"agent"`
	}
	if json.Unmarshal(row.Payload, &p) != nil {
		return a.ID == agent.Claude.ID
	}
	if _, ok := agent.ByID(p.Agent); !ok {
		r.note(fmt.Errorf("leaving queued turn %d alone: unknown agent %q (written by a newer rexq?)", row.ID, p.Agent))
		return false
	}
	return p.Agent == a.ID
}

// others retries a few queued turns of other sessions of agent a, each
// session's oldest due one. It skips a session whose lock is taken: that
// session's own hook will get to them.
func (r *stopRun) others(ctx context.Context, self string, a agent.Agent) {
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
		if row.ClaudeSession == self || !r.mayRetry(row, a) {
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
		if a, ok := agent.ByID(in.Agent); ok {
			in.LaunchDir = a.LaunchDir(in.TranscriptPath, in.Cwd)
		}
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
	a, ok := agent.ByID(in.Agent)
	if !ok { // mayRetry and Stop keep these out
		return errors.Join(notes...), fmt.Errorf("unknown agent %q", in.Agent)
	}
	getenv := in.getenv(a, d.Env)
	choice := in.modelChoice(a, getenv)
	if a.ModelSetting != "" {
		// Read now, not when the turn ended: a retry uses the model
		// chosen since, as every hook and backfill do.
		v, err := s.Setting(a.ModelSetting)
		notes = append(notes, err)
		choice.Configured = v
	}
	res, err := extract.Extract(ctx, d.runner(a, choice, getenv), extract.Input{Message: msg, Prompt: in.Prompt, Open: earlier})
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
			CreatedAt: asked, Agent: a.ID,
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

// agent is the live turn's agent, which Stop checked is known.
func (in *stopInput) agent() agent.Agent {
	a, _ := agent.ByID(in.Agent)
	return a
}

// getenv is the environment the turn's runner reads: the agent's TurnEnv
// as kept when the turn ended, or for Claude, which keeps none, the hook's.
func (in *stopInput) getenv(a agent.Agent, env func(string) string) func(string) string {
	if len(a.TurnEnv) == 0 {
		return env
	}
	return func(k string) string { return in.Env[k] }
}

// modelChoice is what the turn says about extraction models.
func (in *stopInput) modelChoice(a agent.Agent, getenv func(string) string) extract.ModelChoice {
	c := extract.ModelChoice{Provider: in.Provider, Session: in.Model}
	if a.ModelEnv != "" {
		c.Env = getenv(a.ModelEnv)
	}
	return c
}

// runner returns the extraction runner for agent a's turns.
func (d Deps) runner(a agent.Agent, c extract.ModelChoice, getenv func(string) string) extract.Runner {
	switch {
	case d.RunnerFor != nil:
		return d.RunnerFor(a, c)
	case a.ID == agent.Claude.ID && d.Runner != nil:
		return d.Runner // set before there were agents: Claude's
	}
	return a.Runner(c, getenv)
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
