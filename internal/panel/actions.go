package panel

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tfcace/rexq/internal/agent"
	"github.com/tfcace/rexq/internal/rexctl"
	"github.com/tfcace/rexq/internal/store"
)

// errNoUndo says an undo found the item changed since.
var errNoUndo = errors.New("it changed since; nothing to undo")

type snoozeChoice struct {
	key, label string
	at         func(now time.Time) (time.Time, string)
}

// name is the choice's label as it applies at now.
func (c snoozeChoice) name(now time.Time) string {
	if c.key == "2" && now.Hour() >= 19 {
		until, _ := c.at(now)
		return "until " + until.Format("15:04")
	}
	return c.label
}

func at9(now time.Time, days int) time.Time {
	d := now.AddDate(0, 0, days)
	return time.Date(d.Year(), d.Month(), d.Day(), 9, 0, 0, 0, now.Location())
}

var snoozeChoices = []snoozeChoice{
	{"1", "1 hour", func(n time.Time) (time.Time, string) { return n.Add(time.Hour), "" }},
	{"2", "this evening", func(n time.Time) (time.Time, string) {
		e := time.Date(n.Year(), n.Month(), n.Day(), 19, 0, 0, 0, n.Location())
		if !e.After(n) {
			e = n.Add(3 * time.Hour)
		}
		return e, ""
	}},
	{"3", "tomorrow 9:00", func(n time.Time) (time.Time, string) { return at9(n, 1), "" }},
	{"4", "Monday 9:00", func(n time.Time) (time.Time, string) {
		days := (8 - int(n.Weekday())) % 7
		if days == 0 {
			days = 7
		}
		return at9(n, days), ""
	}},
	{"5", "next time this agent stops", func(time.Time) (time.Time, string) { return time.Time{}, store.TriggerNextStop }},
}

// answer delivers reply to the item's agent, and marks the item answered
// only once it was delivered. First it rereads the item: it must still be
// waiting, and the user mustn't have written to that agent since it was
// asked, or the reply would answer a conversation that moved on. Then
// rexctl decides whether typing into the live pane is safe, and before
// typing, the pane must still belong to the item's conversation.
func (m model) answer(asked store.Item, reply string) tea.Cmd {
	return bg(asked.ID, func(ctx context.Context) (string, *store.Item) {
		proj := project(asked)
		refuse := func(why string) (string, *store.Item) {
			return "not sent to " + proj + ": " + why + "; enter jumps there", &asked
		}
		it, found, err := m.d.Store.Item(asked.ID)
		switch {
		case err != nil:
			return "not sent to " + proj + ": couldn't read the store: " + err.Error(), nil
		case !found || (it.State != store.StateOpen && it.State != store.StateSnoozed):
			return "not sent to " + proj + ": that question was already resolved", nil
		}
		if it.WrittenSince() {
			return refuse("you've written to that agent since")
		}
		a, ok := agent.ByID(it.Agent)
		if !ok {
			return "not sent to " + proj + ": " + unknownAgent(it), nil
		}

		how, why := rexctl.Resume, ""
		if it.RexBlock != "" {
			how, why = m.d.Rex.ChooseFor(ctx, a, it.RexBlock, reply)
		}
		switch how {
		case rexctl.Refuse:
			return refuse(why)
		case rexctl.Type:
			owner, err := m.d.Store.PaneOwner(it.RexBlock)
			switch {
			case err != nil:
				return "not sent to " + proj + ": couldn't read the store: " + err.Error(), nil
			case owner == "":
				return "not sent to " + proj + ": that pane isn't linked to this conversation", nil
			case owner != it.ClaudeSession:
				return "not sent to " + proj + ": that pane now runs a different conversation", nil
			}
			if err := m.d.Rex.SendTo(ctx, a, it.RexBlock, reply); err != nil {
				if ns := (*rexctl.NotSentError)(nil); errors.As(err, &ns) {
					return proj + ": " + err.Error() + "; enter jumps there", &it
				}
				return "couldn't type into " + proj + ": " + err.Error(), nil
			}
		case rexctl.Resume:
			if m.d.RexSession == "" {
				return "not in Rex: run " + oneLine(a.ResumeHint(it.ClaudeSession)), nil
			}
			if err := m.d.Rex.ResumeAgent(ctx, a, m.d.RexSession, it.Cwd, it.ClaudeSession, reply); err != nil {
				return "couldn't resume " + proj + ": " + err.Error(), nil
			}
		}
		if err := m.d.Store.Answer(it.ID, reply); err != nil {
			return "sent to " + proj + ", but couldn't mark it answered: " + err.Error(), nil
		}
		return "sent to " + proj + ": " + oneLine(reply), nil
	})
}

// jump shows the item's agent: its live pane, or a resumed session when no
// agent runs there any more. A live agent is never resumed a second time:
// when Rex can't tell, or focusing fails while the agent still runs there,
// it says so instead.
func (m model) jump(it store.Item) tea.Cmd {
	return bg(it.ID, func(ctx context.Context) (string, *store.Item) {
		a, ok := agent.ByID(it.Agent)
		if !ok {
			return unknownAgent(it), nil
		}
		if it.RexBlock != "" {
			if toast, resume := m.focus(ctx, a, it); !resume {
				return toast, nil
			}
		}
		if m.d.RexSession == "" {
			return "run: " + oneLine(a.ResumeHint(it.ClaudeSession)), nil
		}
		if err := m.d.Rex.ResumeAgent(ctx, a, m.d.RexSession, it.Cwd, it.ClaudeSession, ""); err != nil {
			return "couldn't resume " + project(it) + ": " + err.Error(), nil
		}
		m.d.Rex.HideSelf(ctx)
		return "", nil
	})
}

// focus shows the item's pane and hides the panel. It returns the toast,
// or resume when the conversation no longer runs there.
func (m model) focus(ctx context.Context, a agent.Agent, it store.Item) (toast string, resume bool) {
	owner, err := m.d.Store.PaneOwner(it.RexBlock)
	if err != nil {
		return "couldn't read the store: " + err.Error(), false
	}
	if owner != "" && owner != it.ClaudeSession {
		return "", true // the pane runs a different conversation now
	}
	_, ok, err := m.d.Rex.AgentStatus(ctx, a, it.RexBlock)
	switch {
	case err != nil:
		return "couldn't reach Rex: " + err.Error(), false
	case !ok:
		return "", true
	}
	ferr := m.d.Rex.Focus(ctx, m.d.RexSession, it.RexSession, it.RexBlock)
	if ferr == nil {
		m.d.Rex.HideSelf(ctx)
		return "", false
	}
	// Resume only if the pane went away in the meantime.
	if _, ok, err := m.d.Rex.AgentStatus(ctx, a, it.RexBlock); err == nil && !ok {
		return "", true
	}
	return "couldn't jump to " + project(it) + ": " + ferr.Error(), false
}

// unknownAgent says an item names an agent this rexq doesn't know, which
// only a newer rexq writes.
func unknownAgent(it store.Item) string {
	return "it is from agent " + oneLine(it.Agent) + ", which this rexq doesn't know; update rexq"
}
