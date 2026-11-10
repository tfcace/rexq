// Package transcript reads Claude Code session transcripts.
package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"time"
)

// Turn is one user prompt and the agent's final message for it.
type Turn struct {
	SessionID, MsgUUID, Cwd, Branch string
	Time                            time.Time
	Prompt, Final                   string
	// PromptID is Claude Code's id for the prompt the final message answers,
	// the prompt_id the Stop hook gets. Empty in transcripts older than the
	// field.
	PromptID string
	// Entrypoint is how Claude Code was started for the final message:
	// "cli" when interactive, "sdk-cli" under claude -p, "sdk-ts" from the
	// TypeScript SDK. Empty in transcripts older than the field.
	Entrypoint string
	// Provider and Model answered the final message; only Pi's
	// transcripts say.
	Provider, Model string

	msgID string // API message id of Final, to join a message split across entries
}

type entry struct {
	Type             string    `json:"type"`
	SessionID        string    `json:"sessionId"`
	UUID             string    `json:"uuid"`
	PromptID         string    `json:"promptId"`
	Timestamp        time.Time `json:"timestamp"`
	Cwd              string    `json:"cwd"`
	GitBranch        string    `json:"gitBranch"`
	Entrypoint       string    `json:"entrypoint"`
	IsSidechain      bool      `json:"isSidechain"`
	IsMeta           bool      `json:"isMeta"`
	IsCompactSummary bool      `json:"isCompactSummary"`
	Message          struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// synthetic is the model name of messages Claude Code writes itself, such as
// "No response requested." when a session is resumed, or an API error.
const synthetic = "<synthetic>"

// LastTurn returns the session's final turn. ok is false when the
// transcript has no finished turn: it is empty, the user had the last word,
// or the agent was still working (its last message ends in a tool call).
//
// The final message is the text of the session's last assistant message,
// which is what the Stop hook would have seen. Claude Code writes each
// content block of a message as its own entry, all with the message's id.
//
// A slash command starts a turn only once the agent answers it: a local
// command such as /model or /exit gets no answer and leaves the turn before
// it standing.
func LastTurn(path string) (Turn, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Turn{}, false, err
	}
	defer f.Close()
	var cur Turn
	var command *Turn // a slash command's turn, until the agent answers it
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.IsSidechain {
			continue
		}
		switch e.Type {
		case "user":
			// Compaction summaries and meta entries are injected, not typed.
			if e.IsMeta || e.IsCompactSummary {
				continue
			}
			switch p, kind := promptText(e.Message.Content); kind {
			case prompt:
				cur = Turn{SessionID: e.SessionID, Prompt: p, PromptID: e.PromptID}
				command = nil
			case slashCommand:
				command = &Turn{SessionID: e.SessionID, Prompt: p, PromptID: e.PromptID}
			case interrupt:
				cur.Final, cur.MsgUUID, cur.msgID = "", "", ""
				command = nil
			case other:
				// Tool results and task notifications belong to the turn.
				// A notification gets a prompt id of its own, and the Stop
				// hook after the agent's answer to it gets that id.
				command = nil
				if e.PromptID != "" {
					cur.PromptID = e.PromptID
				}
			}
		case "assistant":
			if e.Message.Model == synthetic {
				continue
			}
			if command != nil {
				cur, command = *command, nil
			}
			var blocks []block
			json.Unmarshal(e.Message.Content, &blocks)
			if e.Message.ID == "" || e.Message.ID != cur.msgID {
				cur.Final, cur.MsgUUID, cur.msgID = "", "", e.Message.ID
			}
			for _, b := range blocks {
				switch b.Type {
				case "tool_use":
					cur.Final, cur.MsgUUID = "", ""
				case "text":
					if b.Text == "" {
						continue
					}
					if cur.Final != "" {
						cur.Final += "\n\n"
					}
					cur.Final += b.Text
					cur.MsgUUID, cur.Time, cur.Cwd, cur.Branch, cur.Entrypoint = e.UUID, e.Timestamp, e.Cwd, e.GitBranch, e.Entrypoint
					if cur.SessionID == "" {
						cur.SessionID = e.SessionID
					}
				}
			}
		}
	}
	return cur, cur.Final != "" && cur.MsgUUID != "", sc.Err()
}

// Kinds of user entry.
const (
	other        = iota // a tool result or injected text, part of the turn
	prompt              // the user's own words
	slashCommand        // the user ran a slash command
	interrupt           // the user pressed Esc
	local               // output of a local command or a ! shell command
)

// interrupted starts the text Claude Code writes when the user interrupts
// the agent, with or without " for tool use]".
const interrupted = "[Request interrupted by user"

var (
	commandName = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
)

// classify says what kind of user entry text is, and returns the prompt
// for a prompt or a slash command.
func classify(s string) (string, int) {
	switch {
	case strings.HasPrefix(s, interrupted):
		return "", interrupt
	case strings.HasPrefix(s, "<command-name>") || strings.HasPrefix(s, "<command-message>"):
		var name, args string
		if m := commandName.FindStringSubmatch(s); m != nil {
			name = strings.TrimSpace(m[1])
		}
		if m := commandArgs.FindStringSubmatch(s); m != nil {
			args = strings.TrimSpace(m[1])
		}
		return strings.TrimSpace(name + " " + args), slashCommand
	case strings.HasPrefix(s, "<local-command-") || strings.HasPrefix(s, "<bash-"):
		return "", local
	case strings.HasPrefix(s, "<"):
		return "", other
	}
	return s, prompt
}

// promptText says what kind of user entry raw, a message's content, is, and
// returns the prompt for a prompt or a slash command.
func promptText(raw json.RawMessage) (string, int) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return classify(s)
	}
	var blocks []block
	json.Unmarshal(raw, &blocks)
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return "", other
		}
	}
	for _, b := range blocks {
		if b.Type == "text" {
			if p, kind := classify(b.Text); kind != other {
				return p, kind
			}
		}
	}
	return "", other
}
