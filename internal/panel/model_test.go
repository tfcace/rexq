package panel

import (
	"strings"
	"testing"
	"time"

	"github.com/tfcace/rexq/internal/store"
)

func TestOrderPutsOpenFirstByUrgency(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	items := []store.Item{
		{ID: 1, Kind: store.KindLeftover, State: store.StateOpen, CreatedAt: now},
		{ID: 2, Kind: store.KindBlocker, State: store.StateOpen, CreatedAt: now.Add(-time.Hour)},
		{ID: 3, Kind: store.KindDecision, State: store.StateSnoozed, SnoozeUntil: now.Add(time.Hour), CreatedAt: now},
		{ID: 4, Kind: store.KindDecision, State: store.StateSnoozed, SnoozeUntil: now.Add(-time.Minute), CreatedAt: now}, // due
		{ID: 5, Kind: store.KindBlocker, State: store.StateOpen, CreatedAt: now.Add(-8 * 24 * time.Hour)},                // stale
	}
	g := order(items, now)
	if ids(g.open) != "2,4,1" || ids(g.snoozed) != "3" || ids(g.stale) != "5" {
		t.Fatalf("open=%s snoozed=%s stale=%s", ids(g.open), ids(g.snoozed), ids(g.stale))
	}
}

func ids(items []store.Item) string {
	s := ""
	for i, it := range items {
		if i > 0 {
			s += ","
		}
		s += string(rune('0' + it.ID))
	}
	return s
}

func TestSnoozeChoices(t *testing.T) {
	now := time.Date(2026, 11, 5, 22, 10, 0, 0, time.Local) // a Thursday
	got := map[string]time.Time{}
	for _, c := range snoozeChoices {
		until, _ := c.at(now)
		got[c.key] = until
	}
	if !got["1"].Equal(now.Add(time.Hour)) {
		t.Errorf("1 hour: %v", got["1"])
	}
	if got["3"] != time.Date(2026, 11, 6, 9, 0, 0, 0, time.Local) {
		t.Errorf("tomorrow: %v", got["3"])
	}
	if got["4"] != time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local) {
		t.Errorf("monday: %v", got["4"])
	}
	// Past 19:00, "this evening" is three hours on, and says when.
	if want := now.Add(3 * time.Hour); !got["2"].Equal(want) || snoozeChoices[1].name(now) != "until 01:10" {
		t.Errorf("evening at 22:10: %v %q", got["2"], snoozeChoices[1].name(now))
	}
	morning := time.Date(2026, 11, 5, 10, 0, 0, 0, time.Local)
	if e, _ := snoozeChoices[1].at(morning); e != time.Date(2026, 11, 5, 19, 0, 0, 0, time.Local) || snoozeChoices[1].name(morning) != "this evening" {
		t.Errorf("evening at 10:00: %v %q", e, snoozeChoices[1].name(morning))
	}
	if _, trig := snoozeChoices[4].at(now); trig != store.TriggerNextStop {
		t.Errorf("5 should be the next-stop trigger")
	}
}

// Fields the store keeps raw, like the working directory, must not reach
// the terminal with their control characters.
func TestDetailSanitizesRawFields(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	it := store.Item{ID: 1, Kind: store.KindBlocker, State: store.StateOpen, CreatedAt: now, Title: "Which?",
		Cwd: "/work/\x1b]0;pwned\x07proj", ClaudeSession: "c1\x1b[2J", RexSession: "session:\x9b31m"}
	m := model{d: Deps{Now: func() time.Time { return now }}, w: 120, h: 60, expanded: true, g: groups{open: []store.Item{it}}}
	for _, out := range []string{m.detail(it, 80, 60), m.View()} {
		for _, bad := range []string{"\x1b]0;", "\x07", "\x1b[2J", "\x9b"} {
			if strings.Contains(out, bad) {
				t.Errorf("output holds %q", bad)
			}
		}
	}
}

// Questions the user has written to the agent about since are set aside,
// after the snoozed ones; snoozed and old ones stay where they were.
func TestOrderSetsAsideRepliedQuestions(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	later := now.Add(time.Minute)
	items := []store.Item{
		{ID: 1, Kind: store.KindDecision, State: store.StateOpen, CreatedAt: now},
		{ID: 2, Kind: store.KindBlocker, State: store.StateOpen, CreatedAt: now, PromptedAt: later},
		{ID: 3, Kind: store.KindLeftover, State: store.StateOpen, CreatedAt: now.Add(-time.Hour), PromptedAt: later},
		{ID: 4, Kind: store.KindDecision, State: store.StateSnoozed, SnoozeUntil: now.Add(time.Hour), CreatedAt: now, PromptedAt: later},
		{ID: 5, Kind: store.KindBlocker, State: store.StateOpen, CreatedAt: now.Add(-8 * 24 * time.Hour), PromptedAt: later},
		{ID: 6, Kind: store.KindAction, State: store.StateOpen, CreatedAt: now, PromptedAt: now.Add(-time.Second)}, // written before
	}
	g := order(items, now)
	if ids(g.open) != "1,6" || ids(g.snoozed) != "4" || ids(g.replied) != "2,3" || ids(g.stale) != "5" {
		t.Fatalf("open=%s snoozed=%s replied=%s stale=%s", ids(g.open), ids(g.snoozed), ids(g.replied), ids(g.stale))
	}
	m := model{g: g, d: Deps{Now: func() time.Time { return now }}, w: 120, h: 40}
	if got := ids(m.list()); got != "1,6,4,2,3,5" {
		t.Fatalf("cursor order %s", got)
	}
	head := m.header()
	if !strings.Contains(head, "2 open") || !strings.Contains(head, "0 blocking") {
		t.Fatalf("header counts replied questions: %q", head)
	}
	view := m.View()
	snoozed, replied, older := strings.Index(view, "SNOOZED"), strings.Index(view, "YOU'VE REPLIED SINCE  2"), strings.Index(view, "OLDER THAN A WEEK")
	if snoozed < 0 || replied < snoozed || older < replied {
		t.Fatalf("sections out of order:\n%s", view)
	}
	if strings.Count(view, "·replied") != 4 {
		t.Fatalf("want the ·replied marker on the 4 replied rows:\n%s", view)
	}
}
