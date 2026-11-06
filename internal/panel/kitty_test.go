package panel

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/tfcace/rexq/internal/store"
)

const apcStart = "\x1b_G"

// sent is the transmit sequence's payload for path: its base64 name.
func sent(path string) string { return base64.StdEncoding.EncodeToString([]byte(path)) }

// withShots stores an open question whose screenshots are paths, and loads it.
func withShots(t *testing.T, paths ...string) (model, store.Item) {
	t.Helper()
	m, s, clk, _ := panelOn(t)
	it := store.Item{RexSession: "session:panel", ClaudeSession: "c-shots", SourceMsg: "m", Cwd: "/nonexistent/shots",
		Project: "shots", Kind: store.KindDecision, Title: "Keep this layout?", Replies: []string{"yes"},
		Images: paths, CreatedAt: clk.t.Add(-time.Minute)}
	id, _, err := s.Insert(it)
	if err != nil {
		t.Fatal(err)
	}
	it.ID = id
	return load(t, m), it
}

func png(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The transmit sequence rides in the View for one frame, and only once per
// path: folding and unfolding again sends nothing.
func TestExpandSendsScreenshotsOnce(t *testing.T) {
	dir := t.TempDir()
	shot := png(t, dir, "grid.png")
	m, _ := withShots(t, shot)
	if strings.Contains(m.View(), apcStart) {
		t.Fatal("sent before the context was opened")
	}

	m, cmd := press(t, m, " ")
	v := m.View()
	if n := strings.Count(v, apcStart); n != 1 || !strings.Contains(v, sent(shot)) {
		t.Fatalf("want one transmit of %s, got %d in view", shot, n)
	}
	if !strings.Contains(v, "\x1b[38;2;0;0;1m"+string(placeholder)) {
		t.Error("no thumbnail for image 1 under SCREENSHOTS")
	}
	if cmd == nil {
		t.Fatal("no command to end the transmit frame")
	}
	m, _ = send(t, m, cmd())
	if strings.Contains(m.View(), apcStart) {
		t.Fatal("transmit sequence still in the view after its frame")
	}
	if !strings.Contains(m.View(), string(placeholder)) {
		t.Error("thumbnail gone after the transmit frame")
	}

	m, _ = press(t, m, " ")
	m, cmd = press(t, m, " ")
	if strings.Contains(m.View(), apcStart) || cmd != nil {
		t.Fatal("sent the same screenshot again")
	}
}

// A screenshot that is gone, or isn't a regular file, is not sent; its name
// is shown instead.
func TestMissingScreenshotsAreSkipped(t *testing.T) {
	dir := t.TempDir()
	shot := png(t, dir, "grid.png")
	gone := filepath.Join(dir, "gone.png")
	folder := filepath.Join(dir, "folder.png")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	m, _ := withShots(t, gone, folder, shot)
	m, _ = press(t, m, " ")
	v := m.View()
	if strings.Count(v, apcStart) != 1 || !strings.Contains(v, sent(shot)) {
		t.Fatalf("want only %s sent", shot)
	}
	if strings.Contains(v, sent(gone)) || strings.Contains(v, sent(folder)) {
		t.Fatal("sent a path that isn't a regular file")
	}
	for _, name := range []string{"gone.png", "folder.png"} {
		if !strings.Contains(v, name) {
			t.Errorf("%s not named", name)
		}
	}
}

// A second item opened while the first's frame is pending keeps both
// sequences until the later one's frame ends.
func TestPendingTransmitsAccumulate(t *testing.T) {
	dir := t.TempDir()
	a, b := png(t, dir, "a.png"), png(t, dir, "b.png")
	m, s, clk, _ := panelOn(t)
	for i, p := range []string{a, b} {
		it := store.Item{RexSession: "session:panel", ClaudeSession: "c", SourceMsg: "m" + p, Cwd: "/nonexistent/x",
			Project: "x", Kind: store.KindDecision, Title: "Q", Images: []string{p},
			CreatedAt: clk.t.Add(-time.Duration(i+1) * time.Minute)}
		if _, _, err := s.Insert(it); err != nil {
			t.Fatal(err)
		}
	}
	m = load(t, m)
	m, _ = press(t, m, " ")
	first := m.apcSeq
	m, _ = press(t, m, "j", " ")
	v := m.View()
	if strings.Count(v, apcStart) != 2 || !strings.Contains(v, sent(a)) || !strings.Contains(v, sent(b)) {
		t.Fatalf("want both pending, got %d", strings.Count(v, apcStart))
	}
	m, _ = send(t, m, apcSentMsg{first})
	if strings.Count(m.View(), apcStart) != 2 {
		t.Fatal("the first frame's end cleared the second's sequence")
	}
	m, _ = send(t, m, apcSentMsg{m.apcSeq})
	if strings.Contains(m.View(), apcStart) {
		t.Fatal("sequence kept after the last frame")
	}
}

// A path that wasn't a file is tried again the next time.
func TestImagesRetryAPathThatAppears(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "later.png")
	im := &images{}
	if seq := im.send([]string{p}); seq != "" {
		t.Fatalf("sent a missing file: %q", seq)
	}
	png(t, dir, "later.png")
	if seq := im.send([]string{p}); !strings.Contains(seq, sent(p)) || !strings.Contains(seq, "i=1,") {
		t.Fatalf("got %q", seq)
	}
	if id, ok := im.id(p); !ok || id != 1 {
		t.Fatalf("id %d %v", id, ok)
	}
}

// Rex only decodes PNG: anything else would stay blank cells, so it's named.
func TestOnlyPNGIsSent(t *testing.T) {
	dir := t.TempDir()
	jpg := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(jpg, []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake.png") // a .png that isn't one
	if err := os.WriteFile(fake, []byte("GIF89a"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := withShots(t, jpg, fake)
	m, cmd := press(t, m, " ")
	v := m.View()
	if strings.Contains(v, apcStart) || cmd != nil {
		t.Fatal("sent a file that isn't a PNG")
	}
	for _, name := range []string{"photo.jpg", "fake.png"} {
		if !strings.Contains(v, name) {
			t.Errorf("%s not named", name)
		}
	}
}

// Quitting frees the images in the last frame, which Bubble Tea writes
// before it leaves the alternate screen, where they live.
func TestQuitFreesImagesInTheLastFrame(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		m, _ := withShots(t, png(t, t.TempDir(), "grid.png"))
		m.d.InRex = false
		m, _ = press(t, m, " ")
		var msg tea.Msg = keyMsg(k)
		if k == "ctrl+c" {
			msg = tea.KeyMsg{Type: tea.KeyCtrlC}
		}
		m, cmd := send(t, m, msg)
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s didn't quit", k)
		}
		// Bubble Tea renders after the Update and again for the final frame:
		// both must free.
		for i := range 2 {
			if !strings.Contains(m.View(), "\x1b_Ga=d,d=I,i=1,q=2\x1b\\") {
				t.Errorf("%s: frame %d doesn't free image 1", k, i+1)
			}
		}
	}
}

// Hiding keeps the panel running, and its images.
func TestHideKeepsImages(t *testing.T) {
	m, _ := withShots(t, png(t, t.TempDir(), "grid.png"))
	m, _ = press(t, m, " ", "q")
	if strings.Contains(m.View(), "a=d") {
		t.Fatal("hiding freed the images")
	}
}

// An image whose item left the list is freed, and sent again (as a new
// image) if it comes back.
func TestImagesOfItemsThatLeaveAreFreed(t *testing.T) {
	dir := t.TempDir()
	a, b := png(t, dir, "a.png"), png(t, dir, "b.png")
	m, s, clk, _ := panelOn(t)
	var ids []int64
	for i, p := range []string{a, b} {
		it := store.Item{RexSession: "session:panel", ClaudeSession: "c", SourceMsg: "m" + p, Cwd: "/nonexistent/x",
			Project: "x", Kind: store.KindDecision, Title: "Q", Images: []string{p},
			CreatedAt: clk.t.Add(-time.Duration(i+1) * time.Minute)}
		id, _, err := s.Insert(it)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	m = load(t, m)
	m, _ = press(t, m, " ", "j", " ") // sends a as 1, b as 2
	m, _ = send(t, m, apcSentMsg{m.apcSeq})

	if err := s.Dismiss(ids[0]); err != nil {
		t.Fatal(err)
	}
	m = load(t, m)
	v := m.View()
	if !strings.Contains(v, "\x1b_Ga=d,d=I,i=1,q=2\x1b\\") {
		t.Fatal("image 1 not freed after its item left")
	}
	if strings.Contains(v, "i=2,q=2") {
		t.Fatal("freed image 2, whose item is still listed")
	}
	if _, ok := m.im.id(a); ok {
		t.Fatal("a still counts as sent")
	}
	if id, ok := m.im.id(b); !ok || id != 2 {
		t.Fatalf("b is %d %v", id, ok)
	}
	if seq := m.im.send([]string{a}); !strings.Contains(seq, "i=3,") {
		t.Fatalf("a not sent again as a new image: %q", seq)
	}
}

// The pending sequence rides at the end of the last line, which the
// renderer never drops, and takes no room there.
func TestPendingSequenceIsOnTheLastLine(t *testing.T) {
	m, _ := withShots(t, png(t, t.TempDir(), "grid.png"))
	m, _ = press(t, m, " ")
	lines := strings.Split(m.View(), "\n")
	last := lines[len(lines)-1]
	if strings.Contains(lines[0], apcStart) || !strings.Contains(last, apcStart) {
		t.Fatal("sequence not on the last line")
	}
	pending := m.apc
	m.apc = ""
	plain := strings.Split(m.View(), "\n")
	if got, want := ansi.StringWidth(last), ansi.StringWidth(plain[len(plain)-1]); got != want {
		t.Fatalf("last line is %d cells with the sequence, %d without", got, want)
	}
	if last != plain[len(plain)-1]+pending {
		t.Fatal("the sequence isn't simply appended")
	}
}

// Before the first WindowSizeMsg the View is empty, so a sequence would be
// lost: nothing is marked sent.
func TestNothingIsSentBeforeTheWindowSize(t *testing.T) {
	shot := png(t, t.TempDir(), "grid.png")
	m, _ := withShots(t, shot)
	m.w, m.h = 0, 0
	m, cmd := press(t, m, " ")
	if m.apc != "" || cmd != nil {
		t.Fatal("queued a sequence with no window")
	}
	if _, ok := m.im.id(shot); ok {
		t.Fatal("marked sent with no window")
	}
}

// A column too narrow for a thumbnail names the screenshot instead.
func TestNarrowColumnNamesScreenshots(t *testing.T) {
	shot := png(t, t.TempDir(), "grid.png")
	m, it := withShots(t, shot)
	m, _ = press(t, m, " ")
	out := m.screenshots(store.Item{Images: it.Images}, thumbCols-1)
	if strings.ContainsRune(out, placeholder) || !strings.Contains(out, "grid.png") {
		t.Fatalf("got %q", out)
	}
	if !strings.ContainsRune(m.screenshots(store.Item{Images: it.Images}, thumbCols), placeholder) {
		t.Fatal("no thumbnail at exactly its width")
	}
}

func TestFreeDeletesEveryImageSent(t *testing.T) {
	dir := t.TempDir()
	im := &images{}
	im.send([]string{png(t, dir, "a.png"), png(t, dir, "b.png")})
	want := "\x1b_Ga=d,d=I,i=1,q=2\x1b\\\x1b_Ga=d,d=I,i=2,q=2\x1b\\"
	for range 2 { // free changes nothing
		if got := im.free(); got != want {
			t.Fatalf("got %q", got)
		}
	}
	if (*images)(nil).free() != "" {
		t.Fatal("a nil images freed something")
	}
}
