// Package rexctl drives Rex through the rex CLI.
package rexctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tfcace/rexq/internal/rexenv"
)

// Runner runs `rex` with args and returns stdout.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Exec runs the real rex binary. It never starts a Rex server: a stray empty
// server would report every block as missing, and a missing block means
// "resume the conversation in a new tab". Errors keep rex's exit code
// (errors.As to *exec.ExitError) and carry its stderr.
func Exec(ctx context.Context, args ...string) ([]byte, error) {
	return execRex(ctx, "rex", args...)
}

func execRex(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, append([]string{"--autostart=false"}, args...)...)
	// Inside Rex, REX_SESSION scopes `-b BLOCK` to the caller's own session,
	// so the panel couldn't reach an agent's pane in another session. Every
	// call here names its targets.
	cmd.Env = rexenv.Without(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		name := "rex"
		if len(args) > 0 {
			name += " " + args[0] // not all args: they can hold the reply text
		}
		msg := strings.TrimPrefix(strings.TrimSpace(stderr.String()), "Error: ")
		if msg == "" {
			return out, fmt.Errorf("%s: %w", name, err)
		}
		return out, fmt.Errorf("%s: %s (%w)", name, msg, err)
	}
	return out, nil
}

// notFound reports whether err is rex exiting with code 3, which it does
// when the target block doesn't exist (or isn't a valid block id). An
// unreachable server exits 4, so it never reads as a missing block.
func notFound(err error) bool {
	var ec interface{ ExitCode() int }
	return errors.As(err, &ec) && ec.ExitCode() == 3
}

// Rex talks to the Rex server.
type Rex struct {
	Run Runner

	// Procs lists this machine's processes as `ps -A -o
	// pid=,ppid=,stat=,comm=` prints them; nil runs ps.
	Procs func(ctx context.Context) ([]byte, error)

	// LocalHost returns the host id of this machine's Rex server; nil asks
	// `rex whoami`. A pane whose block has another host id is on another
	// machine, where ps can't see it. An empty id means the server isn't on
	// this machine; an error means the lookup failed.
	LocalHost func(ctx context.Context) (string, error)

	// ProcCwds lists the working directories of pids as `lsof -a -d cwd
	// -Fn -p PID,...` prints them; nil runs lsof.
	ProcCwds func(ctx context.Context, pids []int) ([]byte, error)

	// wait is how long Send waits for the typed reply to show in the
	// input box before giving up; zero means 500ms.
	wait time.Duration

	hosts *hostCache // shared by copies; nil caches nothing
}

// New returns a Rex that runs rex with run and caches host lookups: the
// local host id and each block's host, neither of which changes.
func New(run Runner) Rex {
	return Rex{Run: run, hosts: &hostCache{blocks: map[string]string{}}}
}

type hostCache struct {
	mu     sync.Mutex
	local  string
	blocks map[string]string
}

// onThisMachine reports whether block runs on this machine's Rex server.
// It's false when block has no host id or the server isn't on this machine.
// An error means a host couldn't be read; rex's exit code 3 on the block's
// lookup says the block is gone.
func (r Rex) onThisMachine(ctx context.Context, block string) (bool, error) {
	host, err := r.blockHost(ctx, block)
	if err != nil {
		return false, err
	}
	local, err := r.localHost(ctx)
	switch {
	case errors.Is(err, errNotLocal):
		return false, nil
	case err != nil:
		// %v, not %w: whoami's exit code says nothing about the block, so
		// it must never read as a missing one.
		return false, fmt.Errorf("finding this machine's Rex host: %v", err)
	}
	return host != "" && host == local, nil
}

// errNotLocal says the Rex server isn't on this machine, so no pane is.
var errNotLocal = errors.New("the Rex server isn't on this machine")

func (r Rex) blockHost(ctx context.Context, block string) (string, error) {
	if c := r.hosts; c != nil {
		c.mu.Lock()
		host, ok := c.blocks[block]
		c.mu.Unlock()
		if ok {
			return host, nil
		}
	}
	out, err := r.Run(ctx, "block", "inspect", block, "--json")
	if err != nil {
		return "", err
	}
	var res struct {
		Block struct {
			HostID string `json:"host_id"`
		} `json:"block"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("reading block inspect: %w", err)
	}
	if c := r.hosts; c != nil && res.Block.HostID != "" {
		c.mu.Lock()
		c.blocks[block] = res.Block.HostID
		c.mu.Unlock()
	}
	return res.Block.HostID, nil
}

func (r Rex) localHost(ctx context.Context) (string, error) {
	if c := r.hosts; c != nil {
		c.mu.Lock()
		local := c.local
		c.mu.Unlock()
		if local != "" {
			return local, nil
		}
	}
	lookup := r.LocalHost
	if lookup == nil {
		lookup = r.whoamiHost
	}
	local, err := lookup(ctx)
	if err != nil {
		return "", err
	}
	if local == "" {
		return "", errNotLocal
	}
	if c := r.hosts; c != nil {
		c.mu.Lock()
		c.local = local
		c.mu.Unlock()
	}
	return local, nil
}

// whoamiHost reads the server's host id from `rex whoami`. It counts as
// this machine's only when rex reached the server over a unix socket.
func (r Rex) whoamiHost(ctx context.Context) (string, error) {
	out, err := r.Run(ctx, "whoami", "--json")
	if err != nil {
		return "", err
	}
	var res struct {
		Endpoint string `json:"endpoint"`
		HostID   string `json:"host_id"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("reading whoami: %w", err)
	}
	if !strings.HasPrefix(res.Endpoint, "unix:") || res.HostID == "" {
		return "", errNotLocal
	}
	return res.HostID, nil
}

// Status is a program's reported status (OSC 7501).
type Status struct {
	ID    string `json:"id"` // empty for Claude itself, set for subagents
	App   string `json:"app"`
	State string `json:"state"`
	Kind  string `json:"kind"`

	// Hold, when set, is why the pane's process tree rules out typing,
	// whatever the records say: Claude is suspended, hasn't reported a
	// status yet, or something else runs there.
	Hold string `json:"-"`
}

const (
	holdSuspended = "Claude is suspended there"
	holdStarting  = "the agent may still be starting; try again in a moment"
	holdUnknown   = "can't tell what's running there"
	holdRemote    = "that pane is on another machine"
)

// ClaudeStatus returns Claude Code's status in block. ok is false only when
// Claude is confirmed absent, so resuming the conversation elsewhere can't
// run it twice: the block doesn't exist (rex exits 3), or no Claude process
// runs under the block and nothing but a shell does (the block's root
// process alone, or login with the user's shell as its only descendant).
// Any other failure is an error. A pane on another machine is held: no
// typing, and no resuming either.
//
// Claude's presence comes from the local process tree under the block's
// root process: Rex's records outlive a killed or suspended Claude, and in
// an ordinary tab Rex reports login as the foreground whatever runs under
// it. The state comes from Claude's own record (the one without an id). If
// any claude-code record, a subagent's included, is blocked, so is the
// pane, and the result carries that record's id and kind.
func (r Rex) ClaudeStatus(ctx context.Context, block string) (Status, bool, error) {
	local, err := r.onThisMachine(ctx, block)
	if err != nil {
		if notFound(err) {
			return Status{}, false, nil
		}
		return Status{}, false, err
	}
	if !local {
		return Status{Hold: holdRemote}, true, nil
	}

	out, err := r.Run(ctx, "block", "call", "-b", block, "program_status")
	if err != nil {
		if notFound(err) {
			return Status{}, false, nil
		}
		return Status{}, false, err
	}
	var res struct {
		Records []Status `json:"records"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return Status{}, false, fmt.Errorf("reading program_status: %w", err)
	}

	out, err = r.Run(ctx, "block", "call", "-b", block, "process")
	if err != nil {
		if notFound(err) {
			return Status{}, false, nil
		}
		return Status{}, false, err
	}
	var proc struct {
		Child *struct {
			PID int `json:"pid"`
		} `json:"child"`
	}
	if err := json.Unmarshal(out, &proc); err != nil {
		return Status{}, false, fmt.Errorf("reading process: %w", err)
	}
	if proc.Child == nil || proc.Child.PID == 0 {
		return Status{Hold: holdUnknown}, true, nil
	}

	list := r.Procs
	if list == nil {
		list = listProcs
	}
	out, err = list(ctx)
	if err != nil {
		return Status{}, false, err
	}
	all, err := parseProcs(out)
	if err != nil {
		return Status{}, false, err
	}
	tree := all.tree(proc.Child.PID)
	var claudeProc *process
	for i := range tree {
		if claudeLike(tree[i].comm) {
			claudeProc = &tree[i] // the one nearest the root
			break
		}
	}
	switch {
	case len(tree) == 0:
		return Status{Hold: holdUnknown}, true, nil
	case claudeProc == nil && atPrompt(tree):
		return Status{}, false, nil
	case claudeProc == nil:
		return Status{Hold: holdUnknown}, true, nil
	case strings.Contains(claudeProc.stat, "T"):
		return Status{App: "claude-code", Hold: holdSuspended}, true, nil
	}

	var claude []Status
	for _, s := range res.Records {
		if s.App == "claude-code" {
			claude = append(claude, s)
		}
	}
	if len(claude) == 0 {
		return Status{Hold: holdStarting}, true, nil
	}
	st := Status{App: "claude-code"} // no root record: state "" refuses
	for _, s := range claude {
		if s.ID == "" {
			st = s
			break
		}
	}
	for _, s := range claude {
		if s.State == "blocked" {
			st.ID, st.State, st.Kind = s.ID, s.State, s.Kind
			break
		}
	}
	return st, true, nil
}

// Delivery is how a reply reaches the agent.
type Delivery int

const (
	Type   Delivery = iota // type it into the live pane
	Resume                 // open a tab with claude --resume
	Refuse                 // don't send; the user should look first
)

// Decide picks how to deliver a reply. It types only into a Claude pane that
// is idle or done, isn't blocked on any dialog, and whose input box is known
// to be empty: typing into a dialog would answer it, typing onto a draft
// would send the draft too, and typing mid-turn races the agent.
func Decide(st Status, ok bool, box Box) (Delivery, string) {
	if !ok {
		return Resume, ""
	}
	if why := statusRefusal(st); why != "" {
		return Refuse, why
	}
	if !box.Known {
		return Refuse, "couldn't read the agent's input box"
	}
	if box.Draft != "" {
		return Refuse, "you have a half-typed message there: " + quoteDraft(box.Draft)
	}
	return Type, ""
}

// statusRefusal says why st rules out typing, or "" when it doesn't.
func statusRefusal(st Status) string {
	switch {
	case st.Hold != "":
		return st.Hold
	case st.State == "blocked" && st.Kind == "":
		return "the agent is waiting on a dialog"
	case st.State == "blocked":
		return "the agent is waiting on a " + st.Kind + " dialog"
	case st.State != "idle" && st.State != "done":
		return "the agent is busy"
	}
	return ""
}

// Choose reads block's status and input box and decides how to deliver
// reply. When Rex can't be reached it refuses with the reason. A reply that
// would switch Claude's input mode is refused before asking Rex anything.
func (r Rex) Choose(ctx context.Context, block, reply string) (Delivery, string) {
	if switchesMode(reply) {
		return Refuse, errModeSwitch.Error()
	}
	st, ok, err := r.ClaudeStatus(ctx, block)
	if err != nil {
		return Refuse, "couldn't reach Rex: " + err.Error()
	}
	var box Box
	if ok {
		if box, err = r.InputBox(ctx, block); err != nil {
			return Refuse, "couldn't reach Rex: " + err.Error()
		}
	}
	return Decide(st, ok, box)
}

// NotSentError is Send's error when it typed the reply but didn't press
// Enter: the reply sits in the agent's input box.
type NotSentError struct{ Why string }

func (e *NotSentError) Error() string { return "typed but not sent: " + e.Why }

// Send types text into block and presses Enter, but only after it has seen
// the text land: Claude still idle or done and blocked on nothing, and the
// input box holding exactly the text. Otherwise the text stays typed but
// unsent, and the error is a *NotSentError.
//
// Replies are one line: \r\n, \r, \n and tabs become spaces, and other
// control characters (C0, DEL, C1, so ESC too) are dropped. A newline would
// submit early, and escapes could drive the terminal. A reply starting
// with "/" or "!" is refused before anything is typed (see switchesMode).
func (r Rex) Send(ctx context.Context, block, text string) error {
	text = typedLine(text)
	if switchesMode(text) {
		return errModeSwitch
	}
	if text == "" {
		return errors.New("the reply is empty")
	}
	if _, err := r.Run(ctx, "send", "-b", block, "--", text); err != nil {
		return err
	}
	if why := r.waitTyped(ctx, block, text); why != "" {
		return &NotSentError{why}
	}
	_, err := r.Run(ctx, "send-key", "-b", block, "enter")
	return err
}

// waitTyped polls until block's input box holds text with nothing in the
// way, and returns "" then, or why it gave up.
func (r Rex) waitTyped(ctx context.Context, block, text string) string {
	wait := r.wait
	if wait == 0 {
		wait = 500 * time.Millisecond
	}
	deadline := time.Now().Add(wait)
	want := strings.Join(strings.Fields(text), " ")
	for {
		st, ok, err := r.ClaudeStatus(ctx, block)
		switch {
		case err != nil:
			return "couldn't reach Rex: " + err.Error()
		case !ok:
			return "Claude is no longer running there"
		}
		if why := statusRefusal(st); why != "" {
			return why
		}
		var why string
		box, err := r.InputBox(ctx, block)
		switch {
		case err != nil:
			return "couldn't reach Rex: " + err.Error()
		case !box.Known:
			why = "couldn't read the agent's input box"
		case strings.Join(strings.Fields(box.Draft), " ") == want:
			return ""
		case box.Draft == "":
			why = "the input box is empty"
		default:
			why = "the input box shows " + quoteDraft(box.Draft) + ", not the reply"
		}
		if time.Now().After(deadline) {
			return why
		}
		select {
		case <-ctx.Done():
			return ctx.Err().Error()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// errModeSwitch refuses replies that would switch Claude's input mode:
// "/" opens slash commands and "!" runs a shell command.
var errModeSwitch = errors.New("replies starting with / or ! must be typed in the pane")

// switchesMode reports whether reply, as it would be typed, starts with
// "/" or "!".
func switchesMode(reply string) bool {
	reply = typedLine(reply)
	return strings.HasPrefix(reply, "/") || strings.HasPrefix(reply, "!")
}

// typedLine makes text safe to type as a single-line reply: line breaks and
// tabs become spaces, control characters go. See Send. (The panel's oneLine
// is for showing text, not typing it.)
func typedLine(text string) string {
	text = strings.ReplaceAll(text, "\r\n", " ")
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		}
		return r
	}, text)
	return strings.TrimSpace(text)
}

// Resume opens a tab that resumes a Claude conversation, sending reply as
// its first message when reply isn't empty. It refuses while any Claude
// runs in cwd, since that may be this conversation. It must run in the
// conversation's own directory, or claude can't find it. The tab stays open
// if claude exits, so a failed resume stays visible. An empty session means
// Rex's default session.
func (r Rex) Resume(ctx context.Context, session, cwd, claudeSession, reply string) error {
	if switchesMode(reply) {
		return errModeSwitch // as a first message, "/x" runs a slash command
	}
	if claudeSession == "" {
		return errors.New("no Claude session to resume")
	}
	if cwd == "" {
		return errors.New("no project directory for that conversation")
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return errors.New("the project directory is gone")
	}
	// A Claude the hooks never saw (started before they were installed, or
	// outside Rex) has no pane on record, so the caller may think the
	// conversation isn't running. Never start a second Claude next to one.
	running, err := r.ClaudeRunningIn(ctx, cwd)
	if err != nil {
		return fmt.Errorf("couldn't check for a running Claude: %v", err)
	}
	if running {
		return errors.New("a Claude is already running in that project; jump there or answer in it")
	}
	args := []string{"run"}
	if session != "" {
		args = append(args, "-s", session)
	}
	args = append(args, "-c", cwd, "--keep-open", "--label", "claude "+Short(claudeSession),
		"--", "claude", "--resume", claudeSession)
	if reply != "" {
		args = append(args, "--", reply) // a reply starting with "-" isn't a flag
	}
	_, err = r.Run(ctx, args...)
	return err
}

// Focus shows block. When it's in another Rex session, the app switches to
// that session first.
func (r Rex) Focus(ctx context.Context, current, session, block string) error {
	if session != "" && session != current {
		client, err := r.appClient(ctx)
		if err != nil {
			return err
		}
		if _, err := r.Run(ctx, "-C", client, "do", "session.select", "session_id="+session); err != nil {
			return err
		}
	}
	args := []string{"focus"}
	if session != "" {
		args = append(args, "-s", session)
	}
	_, err := r.Run(ctx, append(args, block)...)
	return err
}

func (r Rex) appClient(ctx context.Context) (string, error) {
	out, err := r.Run(ctx, "client", "ls", "--json")
	if err != nil {
		return "", err
	}
	var res struct {
		Clients []struct {
			ID   string `json:"client_id"`
			Info struct {
				Kind string `json:"kind"`
			} `json:"info"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", err
	}
	for _, c := range res.Clients {
		if c.Info.Kind == "app" {
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("no Rex app is connected")
}

// HideSelf detaches the panel's own block, so it keeps running out of view.
func (r Rex) HideSelf(ctx context.Context) error {
	sid, bid := os.Getenv("REX_SESSION"), os.Getenv("REX_BLOCK")
	if sid == "" || bid == "" {
		return fmt.Errorf("not running in Rex")
	}
	body, err := json.Marshal(struct {
		SessionID string `json:"session_id"`
		BlockID   string `json:"block_id"`
	}{sid, bid})
	if err != nil {
		return err
	}
	_, err = r.Run(ctx, "api", "call", "session.detach_block", string(body))
	return err
}

// Short abbreviates an id for display, safely for ids under 8 characters.
func Short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// maxPaneName is how many runes of a pane's name PaneName keeps.
const maxPaneName = 40

// defaultTab matches the labels Rex gives tabs the user never named.
var defaultTab = regexp.MustCompile(`^Window [0-9a-f]{8}$`)

// PaneName names block for people: its terminal title (Claude sets it to
// the conversation's title), else its tab's label if the user named the
// tab, else "". It is one line, without control characters, at most
// maxPaneName runes.
func (r Rex) PaneName(ctx context.Context, block string) (string, error) {
	out, err := r.Run(ctx, "block", "call", "-b", block, "title")
	if err != nil {
		return "", err
	}
	var t struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(out, &t); err != nil {
		return "", fmt.Errorf("reading title: %w", err)
	}
	if name := paneName(t.Title); name != "" {
		return name, nil
	}

	out, err = r.Run(ctx, "block", "inspect", block, "--json")
	if err != nil {
		return "", err
	}
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(out, &in); err != nil {
		return "", fmt.Errorf("reading block inspect: %w", err)
	}
	body, err := json.Marshal(struct {
		SessionID string `json:"session_id"`
	}{in.SessionID})
	if err != nil {
		return "", err
	}
	out, err = r.Run(ctx, "api", "call", "session.view", string(body))
	if err != nil {
		return "", err
	}
	var view struct {
		Windows []struct {
			Label  string `json:"label"`
			Layers []struct {
				Blocks []struct {
					ID string `json:"block_id"`
				} `json:"blocks"`
			} `json:"layers"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(out, &view); err != nil {
		return "", fmt.Errorf("reading session.view: %w", err)
	}
	for _, w := range view.Windows {
		for _, l := range w.Layers {
			for _, b := range l.Blocks {
				if b.ID == block && !defaultTab.MatchString(w.Label) {
					return paneName(w.Label), nil
				}
			}
		}
	}
	return "", nil
}

// paneName makes s fit to show: one line, no control characters, short.
func paneName(s string) string {
	s = strings.Join(strings.Fields(typedLine(s)), " ")
	if r := []rune(s); len(r) > maxPaneName {
		s = string(r[:maxPaneName-1]) + "…"
	}
	return s
}
