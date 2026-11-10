package panel

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/tfcace/rexq/internal/agent"
	"github.com/tfcace/rexq/internal/store"
)

// hints are the footer's key hints in normal mode: only keys that do
// something now.
func (m model) hints() string {
	var h []string
	add := func(s string) { h = append(h, s) }
	n := len(m.list())
	if n > 1 {
		add("j/k move")
	}
	if it, ok := m.selected(); ok {
		switch r := min(len(it.Replies), 3); r {
		case 0:
		case 1:
			add("1 answer")
		default:
			add(fmt.Sprintf("1-%d answer", r))
		}
		add("r reply")
		add("z snooze")
		if it.State == store.StateSnoozed {
			add("u wake")
		}
		add("space context")
		add("enter jump")
	}
	if m.d.RexSession != "" {
		if m.all {
			add("a this session")
		} else {
			add("a all sessions")
		}
	}
	if n > 0 {
		add("d done")
		add("x not a question")
	}
	if m.d.InRex {
		add("q hide")
	} else {
		add("q quit")
	}
	return strings.Join(h, " · ")
}

// shortTitle names an item in a toast or prompt.
func shortTitle(it store.Item) string {
	return "“" + trunc(oneLine(it.Title), 40) + "”"
}

// View must not change state: Bubble Tea may call it more than once for a
// frame, and renders the last call.
func (m model) View() string {
	if m.w == 0 {
		return ""
	}
	head := m.header()
	rule := sFaint.Render(strings.Repeat("─", m.w))
	foot := m.footer()
	bodyH := m.h - lipgloss.Height(head) - 1 - lipgloss.Height(foot)
	if bodyH < 1 {
		bodyH = 1
	}
	body := m.empty(bodyH)
	if len(m.list()) > 0 {
		body = m.inbox(bodyH)
	}
	// Kitty sequences go at the end of the last line: they take no cells, and
	// the renderer drops lines from the top when a frame is too tall.
	// Quitting frees the images in the last frame, which Bubble Tea writes
	// before it leaves the alternate screen, the screen they belong to.
	frame := lipgloss.JoinVertical(lipgloss.Left, head, rule, body, foot) + m.apc
	if m.quitting {
		frame += m.im.free()
	}
	return frame
}

func (m model) header() string {
	blocked := 0
	for _, it := range m.g.open {
		if it.Kind == store.KindBlocker {
			blocked++
		}
	}
	left := sBold.Render("◇ Agent questions") + "  " +
		sText.Render(fmt.Sprintf("%d open", len(m.g.open))) + sFaint.Render(" · ") +
		fg(kindColor[store.KindBlocker]).Render(fmt.Sprintf("%d blocking", blocked)) + sFaint.Render(" · ") +
		sDim.Render(fmt.Sprintf("%d snoozed", len(m.g.snoozed)))
	scope := "this session"
	if m.showsAll() {
		scope = "all sessions"
	}
	right := sDim.Render(scope) + sFaint.Render("  "+m.d.Now().Format("Mon 15:04"))
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	head := trunc(" "+left+strings.Repeat(" ", gap)+right, m.w)
	if warn := m.warning(); warn != "" {
		head += "\n" + trunc(" "+fg(kindColor[store.KindDecision]).Render(warn), m.w)
	}
	return head
}

// warning says why questions may be missing: extraction failed last time
// (often a logged-out claude or a usage limit), or turns wait for a retry.
// "" when all is well. In a narrow panel the error is shortened, so the
// count and where to look stay in view.
func (m model) warning() string {
	h := m.health
	tail := "see hook.log"
	switch {
	case h.Failing() && h.Pending > 0:
		tail = plural(h.Pending, "turn") + " waiting · " + tail
	case h.Failing():
	case h.Pending > 0:
		return "⚠ " + plural(h.Pending, "turn") + " waiting for extraction · " + tail
	default:
		return ""
	}
	head := "⚠ extraction failing: "
	room := max(m.w-2-lipgloss.Width(head)-lipgloss.Width(" · "+tail), 12)
	return head + trunc(oneLine(h.LastError), room) + " · " + tail
}

// plural returns n and noun, with an s unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (m model) footer() string {
	w := max(m.w-2, 10)
	var line string
	switch {
	case m.mode == snoozing:
		parts := []string{sDim.Render("snooze until")}
		for _, c := range snoozeChoices {
			parts = append(parts, chip(c.key, c.label, cAccent))
		}
		parts = append(parts, sFaint.Render("esc cancel"))
		line = wrapChips(parts, w)
	case m.mode == replying:
		// The question it answers on one line, the reply on the next.
		to := sAcc.Render("reply to a resolved question")
		if it, ok := m.byID(m.target); ok {
			to = sAcc.Render("reply to "+project(it)+": ") + sText.Render(oneLine(it.Title))
		}
		if m.restored {
			to = sFaint.Render("kept reply restored · esc clears  ") + to
		}
		// The key hint is pinned to the right end of the question line, so it
		// stays put while the reply grows on the line below.
		hint := sFaint.Render("enter send · esc cancel")
		to = trunc(to, max(w-lipgloss.Width(hint)-3, 1))
		gap := max(w-lipgloss.Width(to)-lipgloss.Width(hint), 1)
		prompt := sAcc.Render("❯ ")
		input := oneLine(m.input)
		room := w - lipgloss.Width(prompt) - 1
		if over := lipgloss.Width(input) - room; over > 0 {
			input = ansi.TruncateLeft(input, over+1, "…")
		}
		line = to + strings.Repeat(" ", gap) + hint + "\n" + prompt + sText.Render(input) + sAcc.Render("▏")
	case m.busy != nil && m.busy.verb != "":
		line = sAcc.Render(m.busy.verb + " " + m.busy.project + "…")
	case m.toast != "":
		line = sAcc.Width(w).Render(store.Sanitize(m.toast))
	default:
		line = sFaint.Render(wrapHints(m.hints(), w))
	}
	return lipgloss.NewStyle().PaddingLeft(1).Render(line)
}

// wrapHints breaks a " · " separated list of hints into lines of w cells,
// between hints.
func wrapHints(hints string, w int) string {
	var lines []string
	cur := ""
	for _, h := range strings.Split(hints, " · ") {
		switch {
		case cur == "":
			cur = h
		case lipgloss.Width(cur)+3+lipgloss.Width(h) > w:
			lines = append(lines, cur)
			cur = h
		default:
			cur += " · " + h
		}
	}
	return strings.Join(append(lines, cur), "\n")
}

// wrapChips joins parts with spaces, breaking lines to fit w cells.
func wrapChips(parts []string, w int) string {
	var lines []string
	cur := ""
	for _, p := range parts {
		if cur != "" && lipgloss.Width(cur)+1+lipgloss.Width(p) > w {
			lines = append(lines, cur)
			cur = p
			continue
		}
		if cur != "" {
			cur += " "
		}
		cur += p
	}
	return strings.Join(append(lines, cur), "\n")
}

// empty is the body when no item waits.
func (m model) empty(h int) string {
	msg := sBold.Render("Nothing waiting on you.")
	if !m.showsAll() {
		msg = lipgloss.JoinVertical(lipgloss.Center, msg, "", sFaint.Render("a  show all sessions"))
	}
	return lipgloss.Place(m.w, h, lipgloss.Center, lipgloss.Center, msg)
}

// inbox is the list on the left and the selected item on the right.
func (m model) inbox(bodyH int) string {
	listW := m.w * 38 / 100
	if listW < 34 {
		listW = 34
	}
	detW := max(m.w-listW-3, 12)
	now := m.d.Now()

	var rows []string
	selTop, selBottom := 0, 0
	i := -1 // the item's index in m.list()
	for _, sec := range m.sections() {
		if len(rows) > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, sFaint.Render(fmt.Sprintf(" %s  %d", sec.label, len(sec.items))), "")
		for _, it := range sec.items {
			i++
			dim := sec.dim
			sel := i == m.cur
			bar := "  "
			bg := lipgloss.NewStyle()
			if sel {
				bar = fg(kindColor[it.Kind]).Render("▌ ")
				bg = bg.Background(cSel)
			}
			right := sFaint.Render(ago(now, it.CreatedAt))
			switch {
			case it.Snoozed(now):
				right = sDim.Render("z " + snoozeLabel(now, it))
			case dim:
				right = sDim.Render(ago(now, it.CreatedAt))
			}
			projW := listW - 2 - 2 - lipgloss.Width(right) - 2
			l1 := glyph(it.Kind) + " " + sDim.Render(trunc(project(it), projW))
			gap := listW - 2 - lipgloss.Width(l1) - lipgloss.Width(right) - 1
			if gap < 1 {
				gap = 1
			}
			l1 = l1 + strings.Repeat(" ", gap) + right
			ts := sText
			if dim {
				ts = sDim
			}
			if sel {
				ts = sBold
			}
			// Written to since: answering is refused, so the title dims and a
			// marker says why.
			mark := ""
			if it.WrittenSince() {
				ts = sDim
				mark = sFaint.Render(" ·replied")
			}
			l2 := "  " + ts.Render(trunc(oneLine(it.Title), listW-5-lipgloss.Width(mark))) + mark
			block := bg.Width(listW - 2).Render(lipgloss.JoinVertical(lipgloss.Left, l1, l2))
			if sel {
				selTop = len(rows)
			}
			rows = append(rows, strings.Split(lipgloss.JoinHorizontal(lipgloss.Top, bar, block), "\n")...)
			if sel {
				selBottom = len(rows)
			}
		}
	}
	// Scroll just enough to keep the selection in view.
	if off := selBottom - bodyH; off > 0 {
		rows = rows[min(off, selTop):]
	}
	list := lipgloss.NewStyle().Width(listW).Height(bodyH).MaxHeight(bodyH).Render(strings.Join(rows, "\n"))

	sep := sFaint.Render(strings.TrimRight(strings.Repeat("│\n", bodyH), "\n"))

	det := ""
	if it, ok := m.selected(); ok {
		det = m.detail(it, detW, bodyH)
	}
	det = lipgloss.NewStyle().Width(detW).Height(bodyH).MaxHeight(bodyH).PaddingLeft(3).Render(det)

	return lipgloss.JoinHorizontal(lipgloss.Top, list, " ", sep, det)
}

// sourceLine says where the question came from and where a reply goes:
// the live pane, by name when Rex told it, or that replying resumes the
// conversation. It fits in w cells: a long name is cut first, then the age
// goes.
func (m model) sourceLine(it store.Item, w int) string {
	proj := sDim.Render(project(it))
	age := sFaint.Render("  ·  " + when(m.d.Now(), it.CreatedAt))
	sep := sFaint.Render("  ·  ")
	if it.RexBlock == "" {
		live := sFaint.Render("○ not open · replying resumes it")
		// Narrow, the project goes before the point: replying resumes it.
		for _, line := range []string{proj + age + sep + live, proj + sep + live} {
			if lipgloss.Width(line) <= w {
				return line
			}
		}
		return trunc(live, w)
	}
	name := m.paneNameOf(it.RexBlock)
	live := func(name string) string {
		if name == "" {
			return fg(cLive).Render("● live in a pane")
		}
		return fg(cLive).Render("● live in “" + name + "”")
	}
	for _, pre := range []string{proj + age + sep, proj + sep} {
		if line := pre + live(name); lipgloss.Width(line) <= w {
			return line
		}
		if name == "" {
			continue
		}
		// Cut the name to the room left, if enough is left to read.
		if room := w - lipgloss.Width(pre) - lipgloss.Width("● live in “”"); room >= 6 {
			return pre + live(trunc(name, room))
		}
	}
	return trunc(proj+sep+live(name), w)
}

// detail is laid out top to bottom in the order you need it: what is being
// asked, how you can answer, and only then (folded) why. When the unfolded
// context doesn't fit in h lines beside the answers, it takes their place.
func (m model) detail(it store.Item, w, h int) string {
	inner := max(w-4, 8)
	wrap := func(s lipgloss.Style, t string) string { return s.Width(inner).Render(t) }
	now := m.d.Now()

	// 1. where it came from, quiet
	where := m.sourceLine(it, inner)

	// 2. the question, loud
	kind := fg(kindColor[it.Kind]).Render(kindGlyph[it.Kind] + " " + kindLabel[it.Kind])
	title := lipgloss.NewStyle().Foreground(cText).Bold(true).Width(inner).Render(store.Sanitize(it.Title))

	head := []string{"", where, "", "", kind, "", title}
	if it.WrittenSince() {
		head = append(head, "", wrap(sDim, "you've written to this agent since this was asked; d marks it done"))
	}
	if it.Snoozed(now) {
		head = append(head, "", sDim.Render("z  snoozed "+snoozeLabel(now, it)+"   u wake now"))
	}

	// 3. the answers, as a vertical menu
	menu := []string{"", ""}
	kc := fg(kindColor[it.Kind]).Bold(true)
	for i, r := range it.Replies {
		if i == 3 {
			break // only 1-3 answer
		}
		line := "  " + kc.Render(fmt.Sprint(i+1)) + "   " + sText.Render(oneLine(r))
		if i == it.Suggested {
			line += sFaint.Render("   suggested")
		}
		menu = append(menu, line)
	}
	menu = append(menu,
		"  "+sDim.Render("r")+"   "+sDim.Render("Write a reply…"),
		"  "+sDim.Render("z")+"   "+sDim.Render("Snooze"),
	)

	// 4. the why, folded
	extras := "context"
	if n := len(it.Images); n == 1 {
		extras += ", 1 screenshot"
	} else if n > 1 {
		extras += fmt.Sprintf(", %d screenshots", n)
	}
	if !m.expanded {
		return strings.Join(append(append(head, menu...), "", "", sFaint.Render("▸ space  "+extras)), "\n")
	}
	ctx := []string{"", "", sFaint.Render("▾ space  hide"), ""}
	if m.mode == replying {
		ctx[2] = sFaint.Render("▾ context")
	}
	section := func(label, body string) {
		ctx = append(ctx, sFaint.Render(label), body, "")
	}
	if it.Body != "" {
		quote := lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).
			BorderForeground(kindColor[it.Kind]).PaddingLeft(1).Foreground(cText).Width(inner - 2).
			Render(store.Sanitize(it.Body))
		section("THE AGENT WROTE", quote)
	}
	if it.Context != "" {
		section("AROUND IT", wrap(sDim, store.Sanitize(it.Context)))
	}
	if it.Prompt != "" {
		section("YOU HAD ASKED", wrap(sDim.Italic(true), "“"+store.Sanitize(it.Prompt)+"”"))
	}
	if len(it.Images) > 0 {
		section("SCREENSHOTS", m.screenshots(it, inner))
	}
	var sess, place []string
	for _, s := range []string{oneLine(it.Branch), oneLine(it.Cwd)} {
		if s != "" {
			place = append(place, s)
		}
	}
	if len(place) > 0 {
		sess = append(sess, strings.Join(place, "  ·  "))
	}
	sess = append(sess, agentCommand(it)+" "+oneLine(it.ClaudeSession))
	if it.RexSession != "" {
		sess = append(sess, "rex "+oneLine(it.RexSession))
	}
	if n := m.siblings(it); n == 1 {
		sess = append(sess, "1 more question from the same message")
	} else if n > 1 {
		sess = append(sess, fmt.Sprintf("%d more questions from the same message", n))
	}
	section("SESSION", wrap(sFaint, strings.Join(sess, "\n")))

	full := strings.Join(append(append(head, menu...), ctx...), "\n")
	if lipgloss.Height(full) <= h {
		return full
	}
	hidden := "r reply hidden"
	switch r := min(len(it.Replies), 3); r {
	case 0:
	case 1:
		hidden = "1 answer hidden"
	default:
		hidden = fmt.Sprintf("1-%d answers hidden", r)
	}
	// While replying, space types a space, so it isn't offered.
	if m.mode != replying {
		hidden += " · space folds"
	}
	return strings.Join(append(append(head, "", "", sFaint.Render(hidden)), ctx...), "\n")
}

// screenshots shows the item's sent images as thumbnails, when they fit in
// w cells, and names the rest (gone, or not a file).
func (m model) screenshots(it store.Item, w int) string {
	var ids []int
	var names []string
	for _, p := range it.Images {
		if id, ok := m.im.id(p); ok && w >= thumbCols {
			ids = append(ids, id)
			continue
		}
		names = append(names, oneLine(filepath.Base(p)))
	}
	var parts []string
	if len(ids) > 0 {
		parts = append(parts, thumbs(ids, w))
	}
	if len(names) > 0 {
		parts = append(parts, sDim.Width(w).Render(strings.Join(names, "\n")))
	}
	return strings.Join(parts, "\n\n")
}

// siblings counts the other listed items from it's message.
func (m model) siblings(it store.Item) int {
	n := 0
	for _, o := range m.list() {
		if o.ID != it.ID && o.ClaudeSession == it.ClaudeSession && o.SourceMsg == it.SourceMsg {
			n++
		}
	}
	return n
}

// project names the item's project, falling back to its directory.
func project(it store.Item) string {
	if p := oneLine(it.Project); p != "" {
		return p
	}
	if it.Cwd != "" {
		return oneLine(filepath.Base(it.Cwd))
	}
	return "?"
}

// oneLine makes s safe to show on one line: no control characters, and
// runs of whitespace (newlines too) collapsed to one space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(store.Sanitize(s)), " ")
}

// trunc cuts s, which may hold styles, to w cells, ending in "…" when cut.
func trunc(s string, w int) string {
	if w < 1 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// when says how long ago t was, as a phrase.
func when(now, t time.Time) string {
	if now.Sub(t) < time.Minute {
		return "just now"
	}
	return ago(now, t) + " ago"
}

func ago(now, t time.Time) string {
	d := max(now.Sub(t), 0)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// snoozeLabel says when a snoozed item wakes, briefly.
func snoozeLabel(now time.Time, it store.Item) string {
	switch it.SnoozeTrigger {
	case "":
	case store.TriggerNextStop:
		return "till next stop"
	default:
		return oneLine(it.SnoozeTrigger)
	}
	u := it.SnoozeUntil.In(now.Location())
	d := u.Sub(now)
	switch {
	case d < 3*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours())+1)
	case sameDay(u, now) && u.Hour() >= 17:
		return "tonight " + u.Format("15:04")
	case sameDay(u, now):
		return "today " + u.Format("15:04")
	case sameDay(u, now.AddDate(0, 0, 1)):
		return "tomorrow " + u.Format("15:04")
	case d < 6*24*time.Hour:
		return u.Format("Mon 15:04")
	default:
		return u.Format("Jan 2")
	}
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// agentCommand names the item's agent by its command, as "claude" names
// Claude Code.
func agentCommand(it store.Item) string {
	if a, ok := agent.ByID(it.Agent); ok {
		return a.Command
	}
	return oneLine(it.Agent)
}
