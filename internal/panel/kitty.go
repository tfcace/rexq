package panel

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Screenshots are shown with kitty graphics Unicode placeholders: the
// terminal gets each image once, by id, and the panel draws it as ordinary
// text cells whose foreground color names the image.
//
// Every sequence goes through Bubble Tea's renderer, at the end of the View,
// for a moment (see queue). Writing it to the terminal from a command's
// goroutine would race the renderer's own writes and could split an escape
// sequence.

// placeholder is the kitty graphics Unicode placeholder character.
const placeholder = rune(0x10EEEE)

// diacritics encode a placeholder cell's row and column.
var diacritics = []rune{
	0x0305, 0x030D, 0x030E, 0x0310, 0x0312, 0x033D, 0x033E, 0x033F, 0x0346, 0x034A,
	0x034B, 0x034C, 0x0350, 0x0351, 0x0352, 0x0357, 0x035B, 0x0363, 0x0364, 0x0365,
	0x0366, 0x0367, 0x0368, 0x0369, 0x036A, 0x036B, 0x036C, 0x036D, 0x036E, 0x036F,
}

const thumbCols, thumbRows = 10, 11

// apcHold is how long a sequence stays in the View. The renderer flushes at
// 60 fps, so it is written with at least one frame.
const apcHold = 250 * time.Millisecond

// images assigns kitty image ids to screenshot paths and remembers which
// were sent.
type images struct {
	ids  map[string]int
	next int
}

// pngSig starts every PNG file. Rex only decodes PNG (f=100); anything else
// would stay blank cells.
const pngSig = "\x89PNG\r\n\x1a\n"

// send returns the kitty sequence for the paths not sent yet, or "". A path
// that is not a PNG regular file now is skipped, and tried again next time.
// q=2 means the terminal never replies, so nothing lands in the panel's input.
func (im *images) send(paths []string) string {
	if im.ids == nil {
		im.ids = map[string]int{}
	}
	var b strings.Builder
	for _, p := range paths {
		if _, ok := im.ids[p]; ok {
			continue
		}
		if !isPNG(p) {
			continue
		}
		im.next++
		im.ids[p] = im.next
		fmt.Fprintf(&b, "\x1b_Ga=T,U=1,q=2,f=100,t=f,i=%d,c=%d,r=%d;%s\x1b\\",
			im.next, thumbCols, thumbRows, base64.StdEncoding.EncodeToString([]byte(p)))
	}
	return b.String()
}

// isPNG reports whether p is a regular file that starts like a PNG.
func isPNG(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	sig := make([]byte, len(pngSig))
	_, err = io.ReadFull(f, sig)
	return err == nil && string(sig) == pngSig
}

// id is the image id path was sent as.
func (im *images) id(path string) (int, bool) {
	if im == nil {
		return 0, false
	}
	id, ok := im.ids[path]
	return id, ok
}

// free is the sequence that deletes every image sent, data included. It
// changes nothing: View calls it, maybe more than once.
func (im *images) free() string {
	if im == nil {
		return ""
	}
	var all []int
	for _, id := range im.ids {
		all = append(all, id)
	}
	return deletes(all)
}

// drop forgets the paths keep refuses, and returns the sequence that deletes
// their images. A path dropped and shown again is sent as a new image. Only
// Update may call it (through prune), never View.
func (im *images) drop(keep func(path string) bool) string {
	var gone []int
	for p, id := range im.ids {
		if !keep(p) {
			gone = append(gone, id)
			delete(im.ids, p)
		}
	}
	return deletes(gone)
}

// deletes is the sequence that deletes images ids, data included (d=I).
func deletes(ids []int) string {
	slices.Sort(ids)
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id)
	}
	return b.String()
}

type apcSentMsg struct{ seq int }

// sendImages queues the selected item's screenshots that weren't sent yet.
func (m *model) sendImages() tea.Cmd {
	it, ok := m.selected()
	if !ok || m.im == nil || m.w == 0 {
		return nil // with no window size the View is empty: it would be lost
	}
	return m.queue(m.im.send(it.Images))
}

// prune frees the images no listed item shows anymore. Each holds a full
// decoded screenshot in the terminal, and the panel runs for days.
func (m *model) prune() tea.Cmd {
	if m.im == nil {
		return nil
	}
	listed := map[string]bool{}
	for _, it := range m.list() {
		for _, p := range it.Images {
			listed[p] = true
		}
	}
	return m.queue(m.im.drop(func(p string) bool { return listed[p] }))
}

// queue puts seq in the View until its tick, and returns that tick. The
// renderer only rewrites lines that changed, so seq is written once; if its
// line is redrawn while it is pending, it is written again, which is
// harmless: it sends or deletes the same image id again.
func (m *model) queue(seq string) tea.Cmd {
	if seq == "" {
		return nil
	}
	// Append: an earlier sequence may still be waiting for its frame.
	m.apc += seq
	m.apcSeq++
	n := m.apcSeq
	return tea.Tick(apcHold, func(time.Time) tea.Msg { return apcSentMsg{n} })
}

// thumb draws image id as placeholder cells.
func thumb(id int) string {
	var b strings.Builder
	for r := range thumbRows {
		fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm", id>>16&0xff, id>>8&0xff, id&0xff)
		for c := range thumbCols {
			b.WriteRune(placeholder)
			b.WriteRune(diacritics[r])
			b.WriteRune(diacritics[c])
		}
		b.WriteString("\x1b[39m")
		if r < thumbRows-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// thumbs lays the images out side by side, in rows that fit w cells.
func thumbs(ids []int, w int) string {
	per := max((w+1)/(thumbCols+1), 1)
	var rows []string
	for len(ids) > 0 {
		n := min(per, len(ids))
		var parts []string
		for i, id := range ids[:n] {
			if i > 0 {
				parts = append(parts, " ")
			}
			parts = append(parts, thumb(id))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, parts...))
		ids = ids[n:]
	}
	return strings.Join(rows, "\n\n")
}
