// Package inputbox reads an agent's input box off a captured screen. It
// knows the screen formats `rex capture` writes and one box layout per
// agent; it runs nothing.
package inputbox

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Box is what an agent's input box holds. Known is false when the box
// couldn't be found on screen, in which case nothing should be typed.
type Box struct {
	Known bool
	Draft string
}

// Line is one screen line: Text is everything shown, Typed leaves out
// faint text (each faint rune becomes a space).
type Line struct{ Text, Typed string }

// Plain splits a plain capture into lines. Nothing is faint in it.
func Plain(screen string) []Line {
	var lines []Line
	for _, l := range strings.Split(strings.TrimRight(screen, "\n"), "\n") {
		lines = append(lines, Line{l, l})
	}
	return lines
}

// DecodeVT turns a capture made with --format=vt into lines. It knows SGR
// (ESC[...m, tracking faint: SGR 2 on, 0 or 22 off), cursor forward (ESC[nC,
// which Rex writes for runs of spaces) and OSC strings such as hyperlinks
// (skipped). It fails on any escape it doesn't know, rather than guess where
// text sits.
func DecodeVT(screen string) ([]Line, error) {
	screen = strings.TrimRight(screen, "\r\n")
	var lines []Line
	var text, typed strings.Builder
	faint := false
	rs := []rune(screen)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == '\n':
			lines = append(lines, Line{text.String(), typed.String()})
			text.Reset()
			typed.Reset()
		case r == '\r':
		case r == 0x1b && i+1 < len(rs) && rs[i+1] == '[':
			j := i + 2
			for j < len(rs) && rs[j] >= 0x30 && rs[j] <= 0x3f {
				j++
			}
			if j >= len(rs) || rs[j] < 0x40 || rs[j] > 0x7e {
				return nil, fmt.Errorf("vt: unfinished escape at %d", i)
			}
			params := string(rs[i+2 : j])
			switch rs[j] {
			case 'm':
				faint = applySGR(params, faint)
			case 'C':
				n := 1
				if params != "" {
					v, err := strconv.Atoi(params)
					if err != nil || v < 0 || v > 1000 {
						return nil, fmt.Errorf("vt: bad cursor move %q", params)
					}
					n = max(v, 1)
				}
				text.WriteString(strings.Repeat(" ", n))
				typed.WriteString(strings.Repeat(" ", n))
			default:
				return nil, fmt.Errorf("vt: unexpected escape %q", "ESC["+params+string(rs[j]))
			}
			i = j
		case r == 0x1b && i+1 < len(rs) && rs[i+1] == ']':
			j := i + 2
			for ; j < len(rs); j++ {
				if rs[j] == 0x07 {
					break
				}
				if rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\' {
					j++
					break
				}
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("vt: unfinished OSC at %d", i)
			}
			i = j
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return nil, fmt.Errorf("vt: unexpected control %q at %d", r, i)
		default:
			text.WriteRune(r)
			if faint {
				typed.WriteRune(' ')
			} else {
				typed.WriteRune(r)
			}
		}
	}
	return append(lines, Line{text.String(), typed.String()}), nil
}

// applySGR returns whether text is faint after the SGR parameters params.
func applySGR(params string, faint bool) bool {
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		switch ps[i] {
		case "", "0", "22":
			faint = false
		case "2":
			faint = true
		case "38", "48", "58": // colors: 5;n or 2;r;g;b follow
			if i+1 < len(ps) && ps[i+1] == "5" {
				i += 2
			} else if i+1 < len(ps) && ps[i+1] == "2" {
				i += 4
			}
		}
	}
	return faint
}

// claudePlaceholder matches the whole text Claude shows in an empty box, as
// in `❯ Try "edit <filepath> to..."` (Claude Code 2.1.319, 2026-11-06). Only
// a whole match counts, so a draft like `Try "npm test" first` stays a
// draft. An unknown placeholder reads as a draft too, which is safe. The VT
// reader drops it anyway, since it's faint; this catches it in a plain
// capture.
var claudePlaceholder = regexp.MustCompile(`^Try "[^"]*"$`)

// ClaudeTail is how many non-blank lines may follow Claude's closing rule.
// Claude draws a status line and a mode line under it; an old box from an
// exited Claude ends up above a shell prompt and its output, further up.
const ClaudeTail = 10

// Claude reads Claude Code's input box: two horizontal rules near the
// bottom of the screen, with a first line starting "❯" between them and any
// further (wrapped or multi-line) draft lines indented by two spaces. The
// draft is the typed text of those lines joined by single spaces. Anything
// else is unknown.
func Claude(lines []Line) Box {
	// A rule is a line of "─", at least 20 long. In a named session the
	// top rule carries the name near its end: "──────── app-docs ─".
	isRule := func(s string) bool {
		s = strings.TrimSpace(s)
		lead := len([]rune(s)) - len([]rune(strings.TrimLeft(s, "─")))
		return lead >= 20 && strings.HasSuffix(s, "─")
	}

	// The closing rule is the last rule, within ClaudeTail non-blank lines
	// of the bottom.
	end, seen := -1, 0
	for i := len(lines) - 1; i >= 0 && seen <= ClaudeTail; i-- {
		if isRule(lines[i].Text) {
			end = i
			break
		}
		if strings.TrimSpace(lines[i].Text) != "" {
			seen++
		}
	}
	if end < 2 {
		return Box{}
	}

	var parts []string
	for i := end - 1; i >= 1; i-- {
		line := strings.TrimRight(lines[i].Text, " ")
		if isRule(lines[i-1].Text) {
			if !strings.HasPrefix(strings.TrimSpace(line), "❯") {
				return Box{}
			}
			parts = append(parts, strings.TrimPrefix(strings.TrimSpace(lines[i].Typed), "❯"))
			for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
				parts[l], parts[r] = parts[r], parts[l]
			}
			draft := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
			if claudePlaceholder.MatchString(draft) {
				draft = ""
			}
			return Box{Known: true, Draft: draft}
		}
		if line != "" && !strings.HasPrefix(line, "  ") {
			return Box{}
		}
		parts = append(parts, lines[i].Typed)
	}
	return Box{}
}

// PiTail is how many non-blank lines may follow Pi's closing rule: its
// footer (directory, then usage and model) and a few extension status
// lines. An autocomplete list or a selector shows there too; Pi reads its
// "→" marker as the one Enter picks, so a box with one is unknown.
const PiTail = 5

// piMaxLines bounds how many lines Pi's box may hold. Pi shows at most 30%
// of the screen's rows, and scrolls the rest.
const piMaxLines = 60

// Pi reads the input box of Pi (1.5.1): a plain rule of "─" across the
// screen, the text, and another plain rule, followed only by the footer.
// Pi has no prompt mark and no placeholder; the cursor is an inverse
// space. A rule carrying text (Pi's "── ⠴ Working ──" while it runs, or a
// "↑ 3 more" scroll mark when the text is longer than the box) makes the box
// unknown, as does an open autocomplete list or selector under it. The
// draft is the typed text of the lines between, joined by single spaces.
func Pi(lines []Line) Box {
	// ruleish is any line starting with a run of rule characters: Pi's
	// plain rule, or one with a label in it.
	ruleish := func(s string) bool {
		return strings.HasPrefix(strings.TrimSpace(s), "───")
	}
	plain := func(s string) bool {
		s = strings.TrimSpace(s)
		return len([]rune(s)) >= 20 && strings.Trim(s, "─") == ""
	}

	end, seen := -1, 0
	for i := len(lines) - 1; i >= 0 && seen <= PiTail; i-- {
		if ruleish(lines[i].Text) {
			end = i
			break
		}
		text := strings.TrimSpace(lines[i].Text)
		if strings.HasPrefix(text, "→") {
			return Box{} // an autocomplete list or a selector is open
		}
		if text != "" {
			seen++
		}
	}
	if end < 1 || !plain(lines[end].Text) {
		return Box{}
	}
	var parts []string
	for i := end - 1; i >= 0 && end-i <= piMaxLines; i-- {
		if ruleish(lines[i].Text) {
			if !plain(lines[i].Text) {
				return Box{}
			}
			for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
				parts[l], parts[r] = parts[r], parts[l]
			}
			return Box{Known: true, Draft: strings.Join(strings.Fields(strings.Join(parts, " ")), " ")}
		}
		parts = append(parts, lines[i].Typed)
	}
	return Box{}
}
