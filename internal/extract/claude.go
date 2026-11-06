package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/tfcace/rexq/internal/rexenv"
)

// ClaudeEnv names the claude binary the extractor runs, for when the
// hooks' environment has no claude on PATH.
const ClaudeEnv = "REXQ_CLAUDE"

// Claude runs extractions with `claude -p` on the user's own login.
type Claude struct {
	Bin   string // default $REXQ_CLAUDE, else "claude" on PATH
	Model string // default "haiku"
	Dir   string // working directory; default an empty temp directory
}

// Run implements Runner.
func (c Claude) Run(ctx context.Context, prompt, schema string) ([]byte, error) {
	bin, model, dir := c.Bin, c.Model, c.Dir
	if bin == "" {
		bin = os.Getenv(ClaudeEnv)
	}
	if bin == "" {
		bin = "claude"
	}
	if model == "" {
		model = "haiku"
	}
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "rexq-extract")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// --no-session-persistence keeps these runs out of ~/.claude/projects,
	// so backfill never reads the extractor's own transcripts. --bare is not
	// usable: it skips hooks but only accepts an API key.
	//
	// The empty working directory and disableAllHooks keep the project's
	// CLAUDE.md and the user's SessionStart hooks out of the extractor's
	// context (measured 2026-11-05: about 2.2k extra tokens of unrelated
	// instructions otherwise). disableAllHooks also makes the extractor's
	// run unable to fire rexq's own hooks; GuardEnv stays as a second line.
	//
	// --safe-mode turns off the user's customizations (CLAUDE.md, skills,
	// plugins, hooks, MCP servers) but keeps the login. Without it the user's
	// global ~/.claude/CLAUDE.md reached the extractor: one eval run invented
	// an item from the user's own rules about reports. The flags around it
	// stay as defense in depth.
	//
	// --strict-mcp-config without --mcp-config loads no MCP servers, so the
	// user's servers don't start for every extraction.
	//
	// --tools takes several values and would swallow the next plain argument,
	// so it is passed as one "--tools=" argument. "--" ends the options, so a
	// prompt that starts with "-" can't be read as a flag.
	cmd := exec.CommandContext(ctx, bin, "-p",
		"--model", model,
		"--no-session-persistence",
		"--safe-mode",
		"--settings", `{"disableAllHooks":true}`,
		"--strict-mcp-config",
		"--tools=",
		"--output-format", "json",
		"--json-schema", schema,
		"--", prompt)
	cmd.Dir = dir
	// Without REX_SESSION and REX_BLOCK: the extractor isn't the agent in
	// that pane, and nothing it starts should act as if it were.
	cmd.Env = append(rexenv.Without(os.Environ()), GuardEnv+"=1")
	// If claude leaves a child holding stdout open after the context ends,
	// stop waiting for it after this long.
	cmd.WaitDelay = 5 * time.Second
	// cmd.Stdin stays nil, which is /dev/null: claude -p waits 3 s for stdin otherwise.
	out, err := cmd.Output()
	if err != nil {
		// On failure claude -p usually exits 1 with an error envelope on
		// stdout, which says more than stderr.
		var env envelope
		if json.Unmarshal(out, &env) == nil && env.IsError {
			return nil, env.err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("claude -p: %v: %s", err, cut(string(ee.Stderr), maxStderr))
		}
		return nil, fmt.Errorf("claude -p: %w", err)
	}
	return Envelope(out)
}

// maxStderr is how much of claude's stderr an error keeps: its start,
// which usually says what went wrong, without flooding the hook log.
const maxStderr = 500

// cut returns s, or its first n bytes and a marker if it is longer.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return head(s, n) + " [cut]"
}

// envelope is the JSON that `claude -p --output-format json` prints.
type envelope struct {
	IsError    bool            `json:"is_error"`
	Subtype    string          `json:"subtype"`
	Result     string          `json:"result"`
	Structured json.RawMessage `json:"structured_output"`
}

// err describes an error envelope.
func (e envelope) err() error {
	msg := e.Result
	if msg == "" {
		msg = e.Subtype
	}
	if msg == "" {
		msg = "unknown error"
	}
	return fmt.Errorf("claude -p: %s", msg)
}

// Envelope unwraps the JSON that `claude -p --output-format json` prints.
func Envelope(out []byte) ([]byte, error) {
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("claude -p: unreadable output: %w", err)
	}
	if env.IsError {
		return nil, env.err()
	}
	if len(env.Structured) > 0 && string(env.Structured) != "null" {
		return env.Structured, nil
	}
	return []byte(env.Result), nil
}
