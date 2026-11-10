package rexctl

import (
	"context"
	"strconv"

	"github.com/tfcace/rexq/internal/agent"
	"github.com/tfcace/rexq/internal/inputbox"
)

// Box is what an agent's input box holds. Known is false when the box
// couldn't be found on screen, in which case nothing should be typed.
type Box = inputbox.Box

// boxTail is how many non-blank lines may follow Claude's closing rule.
const boxTail = inputbox.ClaudeTail

// InputBox reads the input box of the Claude pane in block. See InputBoxOf.
func (r Rex) InputBox(ctx context.Context, block string) (Box, error) {
	return r.InputBoxOf(ctx, agent.Claude, block)
}

// InputBoxOf reads the input box of a's pane in block. It reads the screen
// with its styles, so faint text, which is Claude's placeholder or a
// suggested next prompt and nothing the user typed, doesn't count as a
// draft. If that capture can't be decoded, it falls back to plain text,
// where ghost text reads as a draft (and the reply is refused).
func (r Rex) InputBoxOf(ctx context.Context, a agent.Agent, block string) (Box, error) {
	out, err := r.Run(ctx, "capture", "-b", block, "--format=vt", "--trim")
	if err != nil {
		return Box{}, err
	}
	if lines, err := inputbox.DecodeVT(string(out)); err == nil {
		return a.ParseBox(lines), nil
	}
	out, err = r.Run(ctx, "capture", "-b", block, "--trim")
	if err != nil {
		return Box{}, err
	}
	return a.ParseBox(inputbox.Plain(string(out))), nil
}

// parseBox reads Claude's box from a plain capture, where nothing is faint.
func parseBox(screen string) Box {
	return inputbox.Claude(inputbox.Plain(screen))
}

// parseBoxVT reads Claude's box from a capture made with --format=vt. It
// fails on any escape it doesn't know, rather than guess where text sits.
func parseBoxVT(screen string) (Box, error) {
	lines, err := inputbox.DecodeVT(screen)
	if err != nil {
		return Box{}, err
	}
	return inputbox.Claude(lines), nil
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
