package filter

import (
	"strings"
	"testing"
)

func TestLooksOpen(t *testing.T) {
	yes := []string{
		"Nothing is committed. Want me to commit the feature on a branch?",
		"**Decisions for you:**\n1. Check the hook rewrite.",
		"**A decision you may want to veto:** I raised the iOS deployment target from 17 to 18.",
		"It touches a core path and the banner contract, so I stopped for your call.",
		"Should I start on 1? And for the `zero=` question, guide or new key?",
		"Your installed binary is still behind HEAD; reinstall when you are ready.",
		"**Questions for you:**\n1. Keep the old flag or drop it?\n2. Ship now?\n\nEverything else is done and pushed.",
		"Would you like me to open a PR? Tests are green.\n\nThe branch is pushed.",
		"You’ll need to restart the app to pick up the new entitlement.",
		"Let me know what you think and I'll adjust.",
		"Please confirm the bundle id before I push.",
	}
	no := []string{
		"Done. All 42 tests pass and the branch is pushed.",
		"Fixed the nil check in parse.go:88. `go test ./...` is green.",
		// A question in an early paragraph, after enough leading whitespace
		// that an offset taken from the trimmed text, applied to the untrimmed
		// text, would reach back past the "?".
		strings.Repeat(" ", 40) + "Why did it fail? The cache was stale.\n\nCleared it; the build is green again.",
	}
	for _, s := range yes {
		if !LooksOpen(s) {
			t.Errorf("missed: %q", s)
		}
	}
	for _, s := range no {
		if LooksOpen(s) {
			t.Errorf("false positive: %q", s)
		}
	}
}

// A question in the last paragraph counts even when no phrase matches.
func TestLooksOpenTailQuestion(t *testing.T) {
	s := "All green.\n\nShip it to TestFlight now, or wait for the icon?"
	if phrases.MatchString(s) {
		t.Fatalf("a phrase matches %q, so it does not test the tail rule", s)
	}
	if !LooksOpen(s) {
		t.Errorf("missed: %q", s)
	}
}
