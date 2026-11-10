// Package agent describes each coding agent rexq works with, in one place:
// how to recognize its process and its status in Rex, read its input box,
// resume a conversation, extract from a turn and read its transcripts. The
// hooks, rexctl and the panel ask an Agent instead of knowing agents
// themselves. Claude Code is the one rexq started with; its entry is that
// behaviour unchanged.
package agent

import (
	"path"
	"strings"

	"github.com/tfcace/rexq/internal/extract"
	"github.com/tfcace/rexq/internal/inputbox"
	"github.com/tfcace/rexq/internal/transcript"
)

// Agent is one coding agent.
type Agent struct {
	// ID is what the store and the hooks' input call the agent. Claude's
	// is "", so rows and hook input from before there were agents are
	// Claude's.
	ID string
	// Name names the agent in messages: "Claude is suspended there".
	Name string
	// Command is the agent's command, which names it to the user: the
	// panel's "claude <id>" and a resumed tab's label.
	Command string
	// StatusApp is the app its Rex program status records carry.
	StatusApp string
	// IsProcess reports whether a process, as ps prints its command, is
	// the agent.
	IsProcess func(comm string) bool
	// ParseBox reads its input box off the screen.
	ParseBox func([]inputbox.Line) inputbox.Box
	// ReadyStates are the program states, besides idle and done, in which
	// it takes a typed reply.
	ReadyStates []string
	// ResumeArgv is the command that resumes conversation id, with reply
	// as its first message unless reply is empty.
	ResumeArgv func(id, reply string) []string
	// SameProject reports whether a running agent in directory running
	// may be running the conversation of directory dir, so resuming it in
	// dir must wait. Both are canonical paths.
	SameProject func(running, dir string) bool
	// LaunchDir returns the directory a conversation belongs to, from its
	// transcript and the directory the agent was in.
	LaunchDir func(transcriptPath, cwd string) string
	// LastTurn reads a transcript's last finished turn, for backfill.
	LastTurn func(path string) (transcript.Turn, bool, error)
	// Throwaway reports whether a transcript folder, by its name, holds
	// conversations of a temp directory (/private/tmp, /private/var/
	// folders), which backfill skips.
	Throwaway func(folder string) bool
	// StopSendsPrompt is true when the agent's stop input carries the
	// turn's prompt itself; otherwise it is the last one the prompt hook
	// recorded.
	StopSendsPrompt bool
	// TurnEnv are the environment variables its runner reads, which a
	// turn keeps from where it ended: a retry runs in another hook's
	// environment, which may not have them. Claude's runner reads the
	// hook's own environment, as it always did.
	TurnEnv []string
	// ModelEnv is the variable among TurnEnv that names the extraction
	// model, if any.
	ModelEnv string
	// ModelSetting is the rexq setting that names the extraction model,
	// read when a turn is extracted, if any.
	ModelSetting string
	// ShortID abbreviates a conversation id for labels, keeping the part
	// that tells conversations apart.
	ShortID func(id string) string
	// Runner returns the extraction runner for a turn, given what the turn
	// says about models and its environment.
	Runner func(c extract.ModelChoice, getenv func(string) string) extract.Runner
}

// ResumeHint is the command to resume conversation id by hand.
func (a Agent) ResumeHint(id string) string {
	return strings.Join(a.ResumeArgv(id, ""), " ")
}

// Ready reports whether state is one in which a reply may be typed.
func (a Agent) Ready(state string) bool {
	if state == "idle" || state == "done" {
		return true
	}
	for _, s := range a.ReadyStates {
		if s == state {
			return true
		}
	}
	return false
}

// Claude is Claude Code.
var Claude = Agent{
	ID:        "",
	Name:      "Claude",
	Command:   "claude",
	StatusApp: "claude-code",
	// An executable named claude, or the versioned binary it runs as when
	// it's a block's own command (~/.local/share/claude/versions/2.1.319).
	IsProcess: func(comm string) bool {
		return path.Base(comm) == "claude" || strings.Contains(comm, "/claude/versions/")
	},
	ParseBox: inputbox.Claude,
	ResumeArgv: func(id, reply string) []string {
		args := []string{"claude", "--resume", id}
		if reply != "" {
			args = append(args, "--", reply) // a reply starting with "-" isn't a flag
		}
		return args
	},
	// claude --resume finds a conversation by the name of its folder under
	// ~/.claude/projects. /p/app-demo, /p/app/demo and /p/app.demo share
	// one, so a Claude in any of them counts as running in the others.
	SameProject: func(running, dir string) bool {
		return transcript.ProjectDirName(running) == transcript.ProjectDirName(dir)
	},
	LaunchDir: transcript.LaunchDir,
	LastTurn:  transcript.LastTurn,
	Throwaway: func(folder string) bool { return strings.HasPrefix(folder, "-private") },
	Runner:    func(extract.ModelChoice, func(string) string) extract.Runner { return extract.Claude{} },
	// Claude's ids are random from the start: the first 8 characters.
	ShortID: func(id string) string {
		if len(id) > 8 {
			return id[:8]
		}
		return id
	},
}

// Pi is the Pi coding agent, through rexq's Pi extension.
var Pi = Agent{
	ID:        "pi",
	Name:      "Pi",
	Command:   "pi",
	StatusApp: "pi",
	// Pi sets its process title to "pi" (and "pi-rpc" in RPC mode, which
	// rexq leaves alone).
	IsProcess: func(comm string) bool { return path.Base(comm) == "pi" },
	ParseBox:  inputbox.Pi,
	// After a failed run Pi reports "error", with its box idle.
	ReadyStates: []string{"error"},
	ResumeArgv: func(id, reply string) []string {
		args := []string{"pi", "--session", id}
		if reply != "" {
			args = append(args, "--", reply)
		}
		return args
	},
	// pi --session finds a session by id wherever it is stored, and runs it
	// in the session's own directory.
	SameProject: func(running, dir string) bool { return running == dir },
	LaunchDir:   transcript.PiLaunchDir,
	LastTurn:    transcript.PiLastTurn,
	// Pi names a session folder "--" + the path without its leading "/",
	// with "/" turned into "-", + "--".
	Throwaway:       func(folder string) bool { return strings.HasPrefix(folder, "--private-") },
	StopSendsPrompt: true,
	TurnEnv:         []string{extract.PiModelEnv, extract.PiEnv},
	ModelEnv:        extract.PiModelEnv,
	ModelSetting:    "pi-model",
	// Pi's ids are UUIDv7, whose first 8 characters are a timestamp shared
	// by sessions started within a minute or so: the last 8, from the
	// random tail.
	ShortID: func(id string) string {
		if len(id) > 8 {
			return id[len(id)-8:]
		}
		return id
	},
	Runner: func(c extract.ModelChoice, getenv func(string) string) extract.Runner {
		bin := getenv(extract.PiEnv)
		if bin == "" {
			bin = "pi"
		}
		return extract.Pi{Bin: bin, Models: extract.PiModels(c)}
	},
}

// All lists the agents, Claude first.
var All = []Agent{Claude, Pi}

// ByID returns the agent called id; ok is false for an unknown one.
func ByID(id string) (Agent, bool) {
	for _, a := range All {
		if a.ID == id {
			return a, true
		}
	}
	return Agent{}, false
}
