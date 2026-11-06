// Package hook implements the Claude Code hooks that feed the store.
package hook

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/tfcace/rexq/internal/extract"
	"github.com/tfcace/rexq/internal/store"
)

// Deps are the hook's collaborators, replaceable in tests.
type Deps struct {
	Store  *store.Store
	Runner extract.Runner
	Now    func() time.Time
	Env    func(string) string
	Repo   func(cwd string) (project, branch string)
	// Locks serializes the work for one Claude session across processes;
	// see FileLock.
	Locks Locker
}

// skip reports whether the hooks should ignore this Claude process: the
// extractor's own run, or any headless run. Claude Code sets
// CLAUDE_CODE_ENTRYPOINT to "cli" in an interactive session and to "sdk-cli"
// under claude -p; scripts and benchmarks must not fill the inbox or spend
// extraction calls. Unset means an interactive session.
func (d Deps) skip() bool {
	return d.Env(extract.GuardEnv) != "" || strings.HasPrefix(d.Env("CLAUDE_CODE_ENTRYPOINT"), "sdk-")
}

// Prompt handles UserPromptSubmit: it remembers what the user just said, so
// the next extraction can tell which earlier items it answered.
func Prompt(d Deps, r io.Reader) error {
	if d.skip() {
		return nil
	}
	var in struct {
		SessionID string `json:"session_id"`
		Prompt    string `json:"prompt"`
	}
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return err
	}
	if in.SessionID == "" {
		return nil
	}
	return d.Store.RecordPrompt(in.SessionID, in.Prompt, d.Now())
}

// Start handles SessionStart: the Claude session in this Rex pane may have
// changed (new session, resume, /clear), so the pane now belongs to it. It
// is the only hook that changes who owns a pane.
func Start(d Deps, r io.Reader) error {
	if d.skip() {
		return nil
	}
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return err
	}
	block := d.Env("REX_BLOCK")
	if in.SessionID == "" || block == "" {
		return nil
	}
	return d.Store.ClaimPane(block, d.Env("REX_SESSION"), in.SessionID, d.Now())
}
