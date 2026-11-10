package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tfcace/rexq/internal/rexenv"
)

// PiEnv names the pi binary the Pi extractor runs, for when the hooks'
// environment has no pi on PATH.
const PiEnv = "REXQ_PI"

// PiModelEnv names a model for every Pi extraction, for scripts; it wins
// over the pi-model setting and the cheap model for the session's provider.
const PiModelEnv = "REXQ_PI_MODEL"

// Pi runs extractions with `pi -p`, on whatever provider and login Pi uses.
// Claude Code need not be installed.
type Pi struct {
	Bin string // default $REXQ_PI, else "pi" on PATH
	// Models are tried in order, each as "provider/model" or a bare model
	// id. A later one runs only when pi rejected the one before it as a
	// model it doesn't know or can't use.
	Models []string
	Dir    string // working directory; default an empty temp directory
}

// ModelChoice is what a Pi turn says about models, for PiModels.
type ModelChoice struct {
	Env        string // $REXQ_PI_MODEL
	Configured string // the pi-model setting (rexq config set pi-model, /rexq-model in Pi)
	Provider   string // the session's provider
	Session    string // the session's model id
}

// cheapModels are cheap models of some providers, for extraction. They go
// stale as providers retire models; Pi falls back to the session's model
// when one is rejected.
//
// Anthropic's is the model claude -p's "haiku" runs, which the Claude
// extractor uses. On rexq's eval cases (2026-11-10, 12 cases, three runs
// each) it passed 34 and 35 of 36 through pi -p with low thinking, against
// 27 of 36 with thinking off and 26 to 29 of 36 for claude-haiku-4-5; the
// Claude extractor passed 36 of 36.
var cheapModels = map[string]string{
	"anthropic":      "claude-haiku-5-5",
	"openai":         "gpt-5-mini",
	"google":         "gemini-2.5-flash",
	"openrouter":     "anthropic/claude-haiku-5.5",
	"github-copilot": "claude-haiku-5.5",
}

// PiAutomatic is the model a Pi extraction uses with no model chosen: the
// cheap model of the session's provider, else the session's own model.
func PiAutomatic(provider, session string) string {
	if m := PiModels(ModelChoice{Provider: provider, Session: session}); len(m) > 0 {
		return m[0]
	}
	return ""
}

// piModel is the shape of a model name: provider/model, where the model id
// may hold more slashes (openrouter/anthropic/claude-haiku-5.5), without
// spaces or quotes. rexq install pi --model takes the same.
var piModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._:@+~-][A-Za-z0-9._:@+~/-]*$`)

// ValidPiModel reports whether s names a model as provider/model.
func ValidPiModel(s string) bool { return piModel.MatchString(s) && !strings.ContainsAny(s, "\n\r") }

// PiModels lists the models to try for a Pi extraction, best first:
// $REXQ_PI_MODEL, else the pi-model setting, else the cheap model for the
// session's provider, else the session's own model. The session's model
// always comes last as the fallback, and nothing is listed twice.
func PiModels(c ModelChoice) []string {
	session := ""
	if c.Session != "" {
		session = c.Session
		if c.Provider != "" {
			session = c.Provider + "/" + c.Session
		}
	}
	first := strings.TrimSpace(c.Env)
	if first == "" {
		first = strings.TrimSpace(c.Configured)
	}
	if first == "" && c.Provider != "" {
		if m, ok := cheapModels[c.Provider]; ok {
			first = c.Provider + "/" + m
		}
	}
	var out []string
	for _, m := range []string{first, session} {
		if m != "" && (len(out) == 0 || out[0] != m) {
			out = append(out, m)
		}
	}
	return out
}

// rejected matches pi's errors for a model it can't use: one it doesn't
// know, or one the provider refuses.
var rejected = regexp.MustCompile(`(?i)model\b.*\b(not found|unknown|invalid|not supported|not available|does not exist|no access)|not_found_error|model_not_found|unknown model|invalid model`)

// Run implements Runner. pi -p has no JSON schema option, so the schema is
// part of the prompt and the JSON is read out of the reply.
func (p Pi) Run(ctx context.Context, prompt, schema string) ([]byte, error) {
	if len(p.Models) == 0 {
		return nil, errors.New("pi -p: no model to extract with: the Pi session named none and REXQ_PI_MODEL is unset")
	}
	var errs []string
	for i, model := range p.Models {
		out, err := p.run(ctx, model, prompt+piSchemaNote+schema+"\n")
		if err == nil {
			return jsonIn(out)
		}
		errs = append(errs, err.Error())
		if i == len(p.Models)-1 || !rejected.MatchString(err.Error()) || ctx.Err() != nil {
			break
		}
	}
	return nil, errors.New(strings.Join(errs, "; then "))
}

const piSchemaNote = "\n\nAnswer with only a JSON object that follows this JSON schema, and no other text:\n"

func (p Pi) run(ctx context.Context, model, prompt string) ([]byte, error) {
	bin, dir := p.Bin, p.Dir
	if bin == "" {
		bin = os.Getenv(PiEnv)
	}
	if bin == "" {
		bin = "pi"
	}
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "rexq-extract")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	args := []string{"-p",
		"--no-session",    // keep extractions out of Pi's sessions, and backfill
		"--no-extensions", // rexq's own extension included
		"--no-skills",     // nothing of the user's setup reaches the model
		"--no-prompt-templates",
		"--no-context-files", // no AGENTS.md or CLAUDE.md
		"--no-approve",       // no project configuration
		"--no-mcp",
		"--no-tools",
		"--no-themes",
		"--offline", // no catalog refresh; the model call still goes out
		// Low, not off: see cheapModels. Pi clamps it to what the model
		// can do.
		"--thinking", "low",
	}
	if provider, id, ok := strings.Cut(model, "/"); ok && provider != "" && id != "" {
		args = append(args, "--provider", provider, "--model", id)
	} else {
		args = append(args, "--model", model)
	}
	// "--" ends the options, so a prompt that starts with "-" can't be read
	// as a flag.
	args = append(args, "--", prompt)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(rexenv.Without(os.Environ()), GuardEnv+"=1")
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// cmd.Stdin stays nil, which is /dev/null: pi -p would prepend piped input.
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		if msg == "" {
			return nil, fmt.Errorf("pi -p (%s): %w", model, err)
		}
		return nil, fmt.Errorf("pi -p (%s): %v: %s", model, err, cut(msg, maxStderr))
	}
	return out, nil
}

// fence matches a Markdown code fence line.
var fence = regexp.MustCompile("(?m)^```[a-zA-Z]*[ \t]*$")

// jsonIn returns the JSON object in a model's reply: the reply itself, or
// what a code fence holds, or the text from the first "{" to the last "}".
func jsonIn(out []byte) ([]byte, error) {
	s := strings.TrimSpace(fence.ReplaceAllString(string(out), ""))
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return nil, fmt.Errorf("pi -p: no JSON in the reply %q", head(s, 200))
	}
	return []byte(s[i : j+1]), nil
}
