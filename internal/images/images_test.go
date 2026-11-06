package images

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindExistingImages(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.png")
	b := filepath.Join(dir, "b shot.jpg") // has a space: must be skipped, not mangled
	write(t, a)
	write(t, b)
	text := "| ![before](file://" + a + ") | see " + a + " again and /nope/missing.png and " + b
	got := Find(text)
	if len(got) != 1 || got[0] != a {
		t.Fatalf("got %v", got)
	}
}

func TestFindInBackticks(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a.png")
	write(t, a)
	if got := Find("Screenshot: `" + a + "`."); len(got) != 1 || got[0] != a {
		t.Fatalf("got %v", got)
	}
}

func TestFindSkipsNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "x.png")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "pipe.png")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Find("see " + d + " and " + fifo); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// "docs/var/.../a.png" is a relative path; its "/var/.../a.png" tail must not
// be read as an absolute path, even though that file exists.
func TestFindDoesNotMangleRelativePaths(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a.png")
	write(t, a)
	if got := Find("see docs" + a); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestFindExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	write(t, filepath.Join(home, "shot.png"))
	got := Find("see ~/shot.png")
	if len(got) != 1 || got[0] != filepath.Join(home, "shot.png") {
		t.Fatalf("got %v", got)
	}
}

func TestFindCapsAtMax(t *testing.T) {
	dir := t.TempDir()
	var text string
	var want []string
	for i := range Max + 2 {
		p := filepath.Join(dir, fmt.Sprintf("%d.png", i))
		write(t, p)
		text += p + "\n"
		want = append(want, p)
	}
	got := Find(text)
	if fmt.Sprint(got) != fmt.Sprint(want[:Max]) {
		t.Fatalf("got %v, want %v", got, want[:Max])
	}
}

func TestFindAfterPunctuation(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.png")
	b := filepath.Join(dir, "b.png")
	write(t, a)
	write(t, b)
	for text, want := range map[string][]string{
		"Saved **" + a + "**": {a},
		"--out=" + a:          {a},
		"Screenshot:" + a:     {a},
		a + "," + b:           {a, b},
		"see docs" + a:        nil,
		"https://host" + a:    nil,
		// A URL whose host is a's first directory: "//var/..." resolves to a.
		"https://" + a[1:]: nil,
	} {
		if got := Find(text); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("Find(%q) = %v, want %v", text, got, want)
		}
	}
}
