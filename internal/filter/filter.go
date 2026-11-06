// Package filter decides cheaply whether an agent message may leave
// something for the user, before any model is called.
package filter

import (
	"regexp"
	"strings"
)

// Apostrophes accept both ' and the typographic ’ that agents often write.
var phrases = regexp.MustCompile(`(?i)(open questions?|questions? for you|left for you|your (call|decision|input|review|go-ahead)|for you to decide|decisions? (for you|needed)|needs? your (input|decision)|up to you|want me to|would you like|do you want|should i\b|shall i\b|unresolved|before i (proceed|continue)|which (do you|would you) prefer|let me know|please (confirm|check|review|run|restart)|veto|stopped for|when you(['’]re| are) ready|you(['’]ll| will) need to|still rough|known limits|leftovers?)`)

// LooksOpen reports whether text may contain a question, decision or to-do
// for the user.
func LooksOpen(text string) bool {
	if phrases.MatchString(text) {
		return true
	}
	// A question in the last paragraph is usually addressed to the user.
	tail := strings.TrimSpace(text)
	if i := strings.LastIndex(tail, "\n\n"); i >= 0 {
		tail = tail[i:]
	}
	return strings.Contains(tail, "?")
}
