package rexctl

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Box is what Claude Code's input box holds. Known is false when the box
// couldn't be found on screen, in which case nothing should be typed.
type Box struct {
	Known bool
	Draft string
}

// placeholder matches the whole text Claude shows in an empty box, as in
// `❯ Try "edit <filepath> to..."` (Claude Code 2.1.319, 2026-11-06). Only a
// whole match counts, so a draft like `Try "npm test" first` stays a draft.
// An unknown placeholder reads as a draft too, which is safe. The VT reader
// drops it anyway, since it's faint; this catches it in a plain capture.
var placeholder = regexp.MustCompile(`^Try "[^"]*"$`)

// boxTail is how many non-blank lines may follow the box's closing rule.
// Claude draws a status line and a mode line under it; an old box from an
// exited Claude ends up above a shell prompt and its output, further up.
const boxTail = 10

// InputBox reads the input box of the Claude pane in block. It reads the
// screen with its styles, so faint text, which is Claude's placeholder or a
// suggested next prompt and nothing the user typed, doesn't count as a
// draft. If that capture can't be decoded, it falls back to plain text, where
// ghost text reads as a draft (and the reply is refused).
func (r Rex) InputBox(ctx context.Context, block string) (Box, error) {
	out, err := r.Run(ctx, "capture", "-b", block, "--format=vt", "--trim")
	if err != nil {
		return Box{}, err
	}
	if box, err := parseBoxVT(string(out)); err == nil {
		return box, nil
	}
	out, err = r.Run(ctx, "capture", "-b", block, "--trim")
	if err != nil {
		return Box{}, err
	}
	return parseBox(string(out)), nil
}

// boxLine is one screen line: text is everything shown, typed leaves out
// faint text (each faint rune becomes a space).
type boxLine struct{ text, typed string }

// parseBox reads the box from a plain capture, where nothing is faint.
func parseBox(screen string) Box {
	var lines []boxLine
	for _, l := range strings.Split(strings.TrimRight(screen, "\n"), "\n") {
		lines = append(lines, boxLine{l, l})
	}
	return parseLines(lines)
}

// parseBoxVT reads the box from a capture made with --format=vt. It fails on
// any escape it doesn't know, rather than guess where text sits.
func parseBoxVT(screen string) (Box, error) {
	lines, err := decodeVT(strings.TrimRight(screen, "\r\n"))
	if err != nil {
		return Box{}, err
	}
	return parseLines(lines), nil
}

// decodeVT turns VT output into lines. It knows SGR (ESC[...m, tracking
// faint: SGR 2 on, 0 or 22 off), cursor forward (ESC[nC, which Rex writes
// for runs of spaces) and OSC strings such as hyperlinks (skipped).
func decodeVT(screen string) ([]boxLine, error) {
	var lines []boxLine
	var text, typed strings.Builder
	faint := false
	rs := []rune(screen)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == '\n':
			lines = append(lines, boxLine{text.String(), typed.String()})
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
	return append(lines, boxLine{text.String(), typed.String()}), nil
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

// parseLines reads Claude Code's input box: two horizontal rules near the
// bottom of the screen, with a first line starting "❯" between them and any
// further (wrapped or multi-line) draft lines indented by two spaces. The
// draft is the typed text of those lines joined by single spaces. Anything
// else is unknown.
func parseLines(lines []boxLine) Box {
	// A rule is a line of "─", at least 20 long. In a named session the
	// top rule carries the name near its end: "──────── app-docs ─".
	isRule := func(s string) bool {
		s = strings.TrimSpace(s)
		lead := len([]rune(s)) - len([]rune(strings.TrimLeft(s, "─")))
		return lead >= 20 && strings.HasSuffix(s, "─")
	}

	// The closing rule is the last rule, within boxTail non-blank lines
	// of the bottom.
	end, seen := -1, 0
	for i := len(lines) - 1; i >= 0 && seen <= boxTail; i-- {
		if isRule(lines[i].text) {
			end = i
			break
		}
		if strings.TrimSpace(lines[i].text) != "" {
			seen++
		}
	}
	if end < 2 {
		return Box{}
	}

	var parts []string
	for i := end - 1; i >= 1; i-- {
		line := strings.TrimRight(lines[i].text, " ")
		if isRule(lines[i-1].text) {
			if !strings.HasPrefix(strings.TrimSpace(line), "❯") {
				return Box{}
			}
			parts = append(parts, strings.TrimPrefix(strings.TrimSpace(lines[i].typed), "❯"))
			for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
				parts[l], parts[r] = parts[r], parts[l]
			}
			draft := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
			if placeholder.MatchString(draft) {
				draft = ""
			}
			return Box{Known: true, Draft: draft}
		}
		if line != "" && !strings.HasPrefix(line, "  ") {
			return Box{}
		}
		parts = append(parts, lines[i].typed)
	}
	return Box{}
}

// quoteDraft shows what an input box held, as one quoted line of at most 30
// runes.
func quoteDraft(draft string) string {
	draft = typedLine(draft)
	if rs := []rune(draft); len(rs) > 30 {
		draft = string(rs[:30]) + "…"
	}
	return strconv.Quote(draft)
}
