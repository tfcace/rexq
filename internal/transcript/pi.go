package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// piEntry is one line of a Pi session file (format version 3).
type piEntry struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	ParentID  *string   `json:"parentId"`
	Timestamp time.Time `json:"timestamp"`
	Cwd       string    `json:"cwd"` // the header's
	Message   struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stopReason"`
		Provider   string          `json:"provider"`
		Model      string          `json:"model"`
	} `json:"message"`
}

// PiLastTurn returns the final turn of the Pi session file at path, on the
// branch the session is at: Pi's entries form a tree, and the session
// continues from its last entry. ok is false when that branch has no
// finished turn: the user had the last word, or the agent's last answer
// ended in a tool call, an error or an interruption.
//
// The prompt is the user's message that started the turn, and PromptID
// its entry id; MsgUUID is the final assistant message's entry id.
// Messages extensions inject (custom messages) are not the user's and
// don't start a turn.
func PiLastTurn(path string) (Turn, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Turn{}, false, err
	}
	defer f.Close()
	var header piEntry
	byID := map[string]*piEntry{}
	var last *piEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var e piEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Type == "session" {
			header = e
			continue
		}
		if e.ID == "" {
			continue
		}
		byID[e.ID] = &e
		last = &e
	}
	if err := sc.Err(); err != nil {
		return Turn{}, false, err
	}
	var branch []*piEntry
	for e, seen := last, map[string]bool{}; e != nil && !seen[e.ID]; {
		seen[e.ID] = true
		branch = append(branch, e)
		if e.ParentID == nil {
			break
		}
		e = byID[*e.ParentID]
	}
	cur := Turn{SessionID: header.ID, Cwd: header.Cwd}
	for i := len(branch) - 1; i >= 0; i-- {
		e := branch[i]
		if e.Type != "message" {
			continue
		}
		switch e.Message.Role {
		case "user":
			cur = Turn{SessionID: header.ID, Cwd: header.Cwd, Prompt: piText(e.Message.Content), PromptID: e.ID}
		case "assistant":
			cur.Final, cur.MsgUUID = "", ""
			if e.Message.StopReason == "stop" || e.Message.StopReason == "length" {
				if text := piText(e.Message.Content); text != "" {
					cur.Final, cur.MsgUUID, cur.Time = text, e.ID, e.Timestamp
					cur.Provider, cur.Model = e.Message.Provider, e.Message.Model
				}
			}
		}
	}
	return cur, cur.Final != "", nil
}

// piText joins the text blocks of a message's content, which is a string or
// a list of blocks.
func piText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// PiLaunchDir returns the directory a Pi session belongs to, which pi
// --session resumes it in: cwd when the extension sent it (the session's
// own), else the cwd in the session file's header.
func PiLaunchDir(path, cwd string) string {
	if cwd != "" || path == "" {
		return cwd
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	if sc.Scan() {
		var h piEntry
		if json.Unmarshal(sc.Bytes(), &h) == nil && h.Type == "session" {
			return h.Cwd
		}
	}
	return ""
}
