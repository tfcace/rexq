// Package panel is the questions panel: a Bubble Tea TUI over the store.
package panel

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tfcace/rexq/internal/rexctl"
	"github.com/tfcace/rexq/internal/store"
)

// Deps are the panel's collaborators.
type Deps struct {
	Store      *store.Store
	Rex        rexctl.Rex
	RexSession string // the session this panel lives in; "" outside Rex
	InRex      bool   // the panel runs in a Rex block it can hide
	Now        func() time.Time
}

type mode int

const (
	normal mode = iota
	snoozing
	replying
)

// groups are the panel's sections, in display order.
type groups struct {
	open    []store.Item // waiting on you, most urgent first
	snoozed []store.Item // soonest to wake first
	replied []store.Item // open, but you've written to the agent since; dimmed
	stale   []store.Item // open but older than StaleAfter, shown dimmed last
}

// StaleAfter is when an open item stops competing for attention: a session
// you never went back to shouldn't crowd the list.
const StaleAfter = 7 * 24 * time.Hour

// settle is how long actions wait after the selected question disappeared
// under the cursor, so a key meant for it can't land on its neighbour.
const settle = time.Second

type model struct {
	d    Deps
	w, h int
	all  bool // show every Rex session
	g    groups

	// The selection is an item, not a position: background loads move
	// items around, and keys must act on the question on screen.
	cur      int
	sel      int64     // the selected item's ID, 0 before the first load
	frozen   time.Time // actions wait until then; see settle
	expanded bool

	mode     mode
	input    string
	restored bool  // input is a reply kept from a question that went away
	target   int64 // the item a reply or snooze is for
	// folded is the context's fold before r unfolded it, put back when the
	// reply is sent or cancelled.
	folded bool

	busy   *busyOp // an answer or jump in flight
	mine   int64   // the item the panel itself last resolved
	toast  string
	jumpTo *store.Item // enter right after the toast jumps here
	undo   *undoOp     // U right after the toast undoes this

	health     store.Health // how extraction is going, from the last load
	version    int64
	loadSeq    int  // the last load issued
	shownSeq   int  // the last load shown
	loadFailed bool // the tick retries

	names    *paneNames // shared by the model's copies
	im       *images    // shared by the model's copies
	apc      string     // kitty sequences waiting for their frame
	apcSeq   int        // the last queue
	quitting bool       // the last frame frees the images
}

// busyOp is an action in flight. While one runs, action keys do nothing,
// so a double press can't send twice.
type busyOp struct {
	id      int64
	verb    string // "sending to", "jumping to", or "" for store writes
	project string
	// settle, once the action is done, is the load that shows its result.
	// Keys wait for it, so they never act on a list from before.
	settle int
}

// undoOp puts an item back as it was before the panel changed it.
type undoOp struct {
	prev store.Item
	from string // the state the change left it in
}

type tickMsg struct {
	version int64
	err     error
}
type loadedMsg struct {
	seq     int
	g       groups
	health  store.Health
	version int64
}
type loadErrMsg struct {
	seq int
	err error
}

// doneMsg ends an answer or a jump.
type doneMsg struct {
	id    int64
	toast string
	jump  *store.Item // set when the toast offers "enter jumps there"
}

// wroteMsg ends a store write: done, dismiss, wake, snooze or undo.
type wroteMsg struct {
	id    int64
	toast string
	undo  *undoOp
	err   error
}

type toastMsg string

// Run starts the panel.
func Run(d Deps) error {
	_, err := tea.NewProgram(newModel(d), tea.WithAltScreen()).Run()
	return err
}

// newModel is a panel whose first load, the one Init runs, is load 1.
// Init can't count it itself: it gets a copy of the model.
func newModel(d Deps) model {
	return model{d: d, loadSeq: 1, im: &images{}, names: &paneNames{m: map[string]paneName{}}}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.load(m.loadSeq), m.tick())
}

// tick checks the store's data version off the UI goroutine.
func (m model) tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg {
		v, err := m.d.Store.DataVersion()
		return tickMsg{v, err}
	})
}

// showsAll reports whether the panel lists every Rex session. Outside Rex
// there is no session of its own to scope to.
func (m model) showsAll() bool { return m.all || m.d.RexSession == "" }

func (m model) scope() string {
	if m.showsAll() {
		return ""
	}
	return m.d.RexSession
}

// reload issues a load. Loads are numbered, so a slow one can't replace a
// newer list.
func (m *model) reload() tea.Cmd {
	m.loadSeq++
	return m.load(m.loadSeq)
}

// load reads the list as load number seq.
func (m model) load(seq int) tea.Cmd {
	s, scope, now := m.d.Store, m.scope(), m.d.Now
	return func() tea.Msg {
		v, _ := s.DataVersion()
		items, err := s.List(scope)
		if err != nil {
			return loadErrMsg{seq, err}
		}
		// Without it the panel only can't warn; the list still shows.
		h, _ := s.Health()
		return loadedMsg{seq, order(items, now()), h, v}
	}
}

// order splits items into the panel's sections. Due snoozes count as open.
func order(items []store.Item, now time.Time) groups {
	var g groups
	for _, it := range items {
		switch {
		case it.Snoozed(now):
			g.snoozed = append(g.snoozed, it)
		case now.Sub(it.CreatedAt) > StaleAfter:
			g.stale = append(g.stale, it)
		case it.WrittenSince():
			g.replied = append(g.replied, it)
		default:
			g.open = append(g.open, it)
		}
	}
	byUrgency := func(l []store.Item) func(i, j int) bool {
		return func(i, j int) bool {
			a, b := l[i], l[j]
			if store.KindRank[a.Kind] != store.KindRank[b.Kind] {
				return store.KindRank[a.Kind] < store.KindRank[b.Kind]
			}
			return a.CreatedAt.After(b.CreatedAt)
		}
	}
	sort.SliceStable(g.open, byUrgency(g.open))
	sort.SliceStable(g.replied, byUrgency(g.replied))
	sort.SliceStable(g.stale, byUrgency(g.stale))
	sort.SliceStable(g.snoozed, func(i, j int) bool {
		a, b := g.snoozed[i], g.snoozed[j]
		if (a.SnoozeTrigger == "") != (b.SnoozeTrigger == "") {
			return a.SnoozeTrigger == ""
		}
		return a.SnoozeUntil.Before(b.SnoozeUntil)
	})
	return g
}

// list is the cursor order: the sections, top to bottom.
func (m model) list() []store.Item {
	var l []store.Item
	for _, sec := range m.sections() {
		l = append(l, sec.items...)
	}
	return l
}

// section is one part of the list, under its label.
type section struct {
	label string
	items []store.Item
	dim   bool // not waiting on you now
}

// sections are the list's parts in display order. Waiting on you always
// shows; the others only when they hold something.
func (m model) sections() []section {
	all := []section{
		{"WAITING ON YOU", m.g.open, false},
		{"SNOOZED", m.g.snoozed, true},
		{"YOU'VE REPLIED SINCE", m.g.replied, true},
		{"OLDER THAN A WEEK", m.g.stale, true},
	}
	out := all[:1]
	for _, sec := range all[1:] {
		if len(sec.items) > 0 {
			out = append(out, sec)
		}
	}
	return out
}

// byID finds a listed item.
func (m model) byID(id int64) (store.Item, bool) {
	for _, it := range m.list() {
		if it.ID == id {
			return it, true
		}
	}
	return store.Item{}, false
}

// selected is the item under the cursor.
func (m model) selected() (store.Item, bool) {
	if m.sel == 0 {
		return store.Item{}, false
	}
	return m.byID(m.sel)
}

// actionable is the selected item when keys may act on it: nothing is in
// flight, and the selection didn't just change under the user.
func (m model) actionable() (store.Item, bool) {
	if m.busy != nil || m.d.Now().Before(m.frozen) {
		return store.Item{}, false
	}
	return m.selected()
}

// move puts the cursor on index i.
func (m *model) move(i int) {
	l := m.list()
	m.cur = max(min(i, len(l)-1), 0)
	m.sel = 0
	if len(l) > 0 {
		m.sel = l[m.cur].ID
	}
	m.frozen = time.Time{}
	m.expanded = false
}

// follow keeps the cursor on the selected item after a load. If it's gone,
// the cursor stays where it was and actions wait a moment.
func (m *model) follow() {
	l := m.list()
	for i, it := range l {
		if it.ID == m.sel {
			m.cur = i
			return
		}
	}
	gone := m.sel
	m.cur = max(min(m.cur, len(l)-1), 0)
	m.sel = 0
	if len(l) > 0 {
		m.sel = l[m.cur].ID
	}
	if gone == 0 {
		return
	}
	m.expanded = false
	if gone == m.mine {
		return // the panel's own doing: no surprise, and fast triage stays fast
	}
	m.frozen = m.d.Now().Add(settle)
	m.toast, m.jumpTo, m.undo = "that question was resolved", nil, nil
}

// settled ends the action in flight once load seq shows its result. From
// then on, the item vanishing is no longer the panel's doing.
func (m *model) settled(seq int) {
	if m.busy != nil && m.busy.settle > 0 && seq >= m.busy.settle {
		m.busy, m.mine = nil, 0
	}
}

// finished marks the action in flight as done, and reloads to show it.
func (m *model) finished() tea.Cmd {
	cmd := m.reload()
	if m.busy != nil {
		m.busy.verb, m.busy.settle = "", m.loadSeq
	}
	return cmd
}

func bg(id int64, f func(ctx context.Context) (string, *store.Item)) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		toast, jump := f(ctx)
		return doneMsg{id, toast, jump}
	}
}

// Update handles msg, then looks up the selected pane's name if it's due.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	nm, ok := next.(model)
	if !ok {
		return next, cmd
	}
	return nm, tea.Batch(cmd, nm.lookUpPane())
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case paneNameMsg:
		if m.names != nil {
			m.names.m[msg.block] = paneName{name: msg.name, at: m.d.Now()}
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		if (msg.err == nil && msg.version != m.version) || m.snoozeEnded() || m.loadFailed {
			return m, tea.Batch(m.reload(), m.tick())
		}
		return m, m.tick()
	case loadedMsg:
		if msg.seq <= m.shownSeq {
			return m, nil
		}
		m.shownSeq, m.loadFailed = msg.seq, false
		m.g, m.health, m.version = msg.g, msg.health, msg.version
		m.follow()
		m.settled(msg.seq)
		return m, m.prune()
	case loadErrMsg:
		if msg.seq <= m.shownSeq {
			return m, nil
		}
		m.shownSeq, m.loadFailed = msg.seq, true
		m.toast = "couldn't read the store: " + msg.err.Error()
		m.settled(msg.seq)
		return m, nil
	case doneMsg:
		m.toast, m.jumpTo, m.undo = msg.toast, msg.jump, nil
		return m, m.finished()
	case wroteMsg:
		m.toast, m.jumpTo, m.undo = msg.toast, nil, msg.undo
		switch {
		case errors.Is(msg.err, errNoUndo):
			m.toast = "can't undo: " + msg.err.Error()
		case msg.err != nil:
			m.toast, m.undo = "couldn't update the store: "+msg.err.Error(), nil
		}
		return m, m.finished()
	case toastMsg:
		m.toast = string(msg)
		return m, nil
	case apcSentMsg:
		if msg.seq == m.apcSeq {
			m.apc = ""
		}
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

// snoozeEnded reports whether a listed snooze has run out, so the item
// moves back to the open list.
func (m model) snoozeEnded() bool {
	now := m.d.Now()
	for _, it := range m.g.snoozed {
		if !it.Snoozed(now) {
			return true
		}
	}
	return false
}

// start marks an action on it as in flight and returns its command.
func (m model) start(it store.Item, verb string, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	m.busy = &busyOp{id: it.ID, verb: verb, project: project(it)}
	m.mine = it.ID
	return m, cmd
}

// write changes an item in the store off the UI goroutine.
func (m model) write(it store.Item, toast string, undo *undoOp, f func(*store.Store) error) (tea.Model, tea.Cmd) {
	s := m.d.Store
	return m.start(it, "", func() tea.Msg {
		return wroteMsg{it.ID, toast, undo, f(s)}
	})
}

func (m model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		m.quitting = true
		return m, tea.Quit
	}
	switch m.mode {
	case replying:
		return m.replyKey(msg)
	case snoozing:
		m.mode = normal
		for _, c := range snoozeChoices {
			if k != c.key || m.busy != nil {
				continue
			}
			it, ok := m.byID(m.target)
			if !ok {
				m.toast = "that question is gone"
				return m, nil
			}
			until, trig := c.at(m.d.Now())
			return m.write(it, "snoozed "+c.name(m.d.Now())+": "+shortTitle(it), nil,
				func(s *store.Store) error { return s.Snooze(it.ID, until, trig) })
		}
		return m, nil
	}

	if m.held(k) {
		return m, nil // and the toast stays: the key was ignored
	}
	jumpTo, undo := m.jumpTo, m.undo
	m.toast, m.jumpTo, m.undo = "", nil, nil
	switch k {
	case "q", "esc":
		if !m.d.InRex {
			m.quitting = true
			return m, tea.Quit
		}
		return m, m.hide()
	case "j", "down":
		m.move(m.cur + 1)
	case "k", "up":
		m.move(m.cur - 1)
	case " ", "space":
		m.expanded = !m.expanded
		if m.expanded {
			return m, m.sendImages()
		}
	case "a":
		if m.d.RexSession != "" {
			m.all = !m.all
			m.cur, m.sel, m.expanded = 0, 0, false
			return m, m.reload()
		}
	case "enter":
		if m.busy != nil {
			break
		}
		if jumpTo != nil {
			return m.start(*jumpTo, "jumping to", m.jump(*jumpTo))
		}
		if it, ok := m.selected(); ok {
			return m.start(it, "jumping to", m.jump(it))
		}
	case "r":
		// Writing may start while an action runs; sending waits for it.
		if it, ok := m.selected(); ok && !m.d.Now().Before(m.frozen) {
			m.mode, m.target = replying, it.ID
			m.restored = m.input != ""
			// Space types a space now, so show the context for them: the
			// agent's words stay in view while they write.
			m.folded = !m.expanded
			if m.folded {
				m.expanded = true
				return m, m.sendImages()
			}
		}
	case "z":
		if it, ok := m.actionable(); ok {
			m.mode, m.target = snoozing, it.ID
		}
	case "d":
		if it, ok := m.actionable(); ok {
			return m.write(it, "done: "+shortTitle(it)+" · u undo", &undoOp{it, store.StateAnswered},
				func(s *store.Store) error { return s.Answer(it.ID, "(done)") })
		}
	case "x":
		if it, ok := m.actionable(); ok {
			return m.write(it, "dismissed: "+shortTitle(it)+" · u undo", &undoOp{it, store.StateDismissed},
				func(s *store.Store) error { return s.Dismiss(it.ID) })
		}
	case "U", "u":
		// While an undo is offered, u means it too: the item it would wake
		// was just done or dismissed, and isn't listed.
		if k == "u" && undo == nil {
			if it, ok := m.actionable(); ok && it.State == store.StateSnoozed {
				return m.write(it, "woke: "+shortTitle(it)+" · u undo", &undoOp{it, store.StateOpen},
					func(s *store.Store) error { return s.Wake(it.ID) })
			}
			break
		}
		if undo != nil && m.busy == nil {
			it := undo.prev
			return m.write(it, "undone: "+shortTitle(it), nil, func(s *store.Store) error {
				if ok, err := s.Restore(undo.prev, undo.from); err != nil || ok {
					return err
				}
				return errNoUndo
			})
		}
	case "1", "2", "3":
		i := int(k[0] - '1')
		if it, ok := m.actionable(); ok && i < len(it.Replies) {
			return m.start(it, "sending to", m.answer(it, it.Replies[i]))
		}
	}
	return m, nil
}

// replyKey edits the reply, and sends it to the item it was started for.
func (m model) replyKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode, m.input, m.restored = normal, "", false
		m.refold()
	case "enter":
		if m.busy != nil {
			return m, nil // the last action is still running; keep the reply
		}
		it, ok := m.byID(m.target)
		if !ok {
			m.mode = normal
			m.refold()
			m.toast, m.jumpTo, m.undo = "that question is gone; your reply is kept", nil, nil
			return m, nil
		}
		if strings.TrimSpace(m.input) == "" {
			return m, nil
		}
		text := m.input
		m.mode, m.input, m.restored = normal, "", false
		m.refold()
		return m.start(it, "sending to", m.answer(it, text))
	case "backspace":
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			m.input += string(msg.Runes)
		}
	}
	return m, nil
}

// refold folds the context again if r unfolded it. A question selected
// meanwhile was already folded when the selection moved.
func (m *model) refold() {
	if m.folded {
		m.expanded = false
	}
	m.folded = false
}

// held reports whether key k must wait: an action is in flight, or the
// selection just changed under the user.
func (m model) held(k string) bool {
	frozen := m.d.Now().Before(m.frozen)
	switch k {
	case "1", "2", "3", "d", "x", "z", "u":
		return m.busy != nil || frozen
	case "enter", "U":
		return m.busy != nil
	case "r":
		return frozen
	}
	return false
}

// hide detaches the panel's Rex block. The panel keeps running, and its
// images stay sent.
func (m model) hide() tea.Cmd {
	hideSelf := m.d.Rex.HideSelf
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := hideSelf(ctx); err != nil {
			return toastMsg("couldn't hide the panel: " + err.Error())
		}
		return nil
	}
}

// paneNameTTL is how long a pane's name is kept before it's looked up
// again: Claude retitles its pane as the conversation goes.
const paneNameTTL = 10 * time.Second

// paneNames caches the names of agents' panes, by block. Only Update
// touches it; View only reads it, so drawing never waits on Rex.
type paneNames struct{ m map[string]paneName }

type paneName struct {
	name    string // "" when it has none, or the lookup failed
	at      time.Time
	pending bool
}

type paneNameMsg struct {
	block, name string
	err         error
}

// lookUpPane asks Rex for the selected item's pane name when it has none
// cached, or an old one.
func (m model) lookUpPane() tea.Cmd {
	it, ok := m.selected()
	if !ok || it.RexBlock == "" || m.names == nil {
		return nil
	}
	block := it.RexBlock
	e, seen := m.names.m[block]
	if e.pending || (seen && m.d.Now().Sub(e.at) < paneNameTTL) {
		return nil
	}
	e.pending = true
	m.names.m[block] = e
	rex := m.d.Rex
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		name, err := rex.PaneName(ctx, block)
		return paneNameMsg{block, name, err}
	}
}

// paneNameOf is the cached name of block, or "".
func (m model) paneNameOf(block string) string {
	if m.names == nil {
		return ""
	}
	return m.names.m[block].name
}
