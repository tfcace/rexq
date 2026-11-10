package inputbox

import (
	"os"
	"strings"
	"testing"
)

// Real captures of Pi 1.5.1 in a Rex pane (2026-11-10), in both formats
// rex capture writes.
var piFixtures = map[string]Box{
	"empty":   {Known: true},
	"cleared": {Known: true}, // ctrl+u after a draft
	"done":    {Known: true}, // after a turn, state done
	"draft":   {Known: true, Draft: "half typed"},
	"wrapped": {Known: true, Draft: "half typed and then a much longer continuation so that it wraps onto a second " +
		"line of the box, maybe even more"},
	"multiline":      {Known: true, Draft: "first line second line"}, // shift+enter
	"scrolled":       {},                                             // "↑ 5 more" in the top rule: part of the draft is hidden
	"working":        {},                                             // "── ⠴ Working ──" top rule
	"at-complete":    {},                                             // @ file autocomplete open under the box
	"slash-complete": {},                                             // / command autocomplete open
	"selector":       {},                                             // /model selector in place of the box
}

func TestPiFixtures(t *testing.T) {
	for name, want := range piFixtures {
		plain, err := os.ReadFile("testdata/pi-box-" + name + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		if got := Pi(Plain(string(plain))); got != want {
			t.Errorf("%s.txt: got %+v want %+v", name, got, want)
		}
		vt, err := os.ReadFile("testdata/pi-box-" + name + ".vt")
		if err != nil {
			t.Fatal(err)
		}
		lines, err := DecodeVT(string(vt))
		if err != nil {
			t.Errorf("%s.vt: %v", name, err)
			continue
		}
		if got := Pi(lines); got != want {
			t.Errorf("%s.vt: got %+v want %+v", name, got, want)
		}
	}
}

// Claude's box is not Pi's, and Pi's is not Claude's: each parser reads only
// its own agent's screen as a known box with a draft that could be typed on.
func TestPiAndClaudeBoxesDontMix(t *testing.T) {
	empty, _ := os.ReadFile("testdata/pi-box-empty.txt")
	if got := Claude(Plain(string(empty))); got.Known {
		t.Errorf("Claude read Pi's empty box: %+v", got)
	}
	rule := strings.Repeat("─", 40)
	claude := rule + "\n❯ \n" + rule + "\n  status\n"
	if got := Pi(Plain(claude)); got.Known && got.Draft == "" {
		t.Errorf("Pi read Claude's empty box as empty: %+v", got)
	}
}

func TestPiEdgeCases(t *testing.T) {
	rule := strings.Repeat("─", 40)
	footer := "~/code (main)\n$0.000 0.0%/200k   model\n"
	for name, c := range map[string]struct {
		screen string
		want   Box
	}{
		"empty":              {rule + "\n\n" + rule + "\n" + footer, Box{Known: true}},
		"draft":              {rule + "\nhi there\n" + rule + "\n" + footer, Box{Known: true, Draft: "hi there"}},
		"no footer":          {rule + "\nhi\n" + rule + "\n", Box{Known: true, Draft: "hi"}},
		"bottom scroll mark": {rule + "\nhi\n" + "─── ↓ 2 more " + rule + "\n" + footer, Box{}},
		"short rule":         {"────\nhi\n" + rule + "\n" + footer, Box{}},
		"one rule":           {"hi\n" + rule + "\n" + footer, Box{}},
		"no box":             {"$ ls\n", Box{}},
		"box too far up":     {rule + "\n\n" + rule + "\n" + strings.Repeat("output\n", PiTail+1), Box{}},
		"list under the box": {rule + "\n@x\n" + rule + "\n→ x.go\n  y.go\n" + footer, Box{}},
	} {
		if got := Pi(Plain(c.screen)); got != c.want {
			t.Errorf("%s: got %+v want %+v", name, got, c.want)
		}
	}
}
