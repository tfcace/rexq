package agent

import "testing"

// Pi's ids are UUIDv7: their first 8 characters are a timestamp, the same
// for sessions started within about a minute. Pi's short id is the random
// tail; Claude's stays the head.
func TestShortID(t *testing.T) {
	a, b := "01a1c34d-e5e0-765d-9c1a-3b5f7e2d8a41", "01a1c34d-f012-7a3e-8d22-91ce04b7c6f0"
	if Pi.ShortID(a) == Pi.ShortID(b) {
		t.Errorf("Pi: %q and %q both %q", a, b, Pi.ShortID(a))
	}
	if got := Pi.ShortID(a); got != "7e2d8a41" {
		t.Errorf("Pi: got %q", got)
	}
	for id, want := range map[string]string{"0123456789abcdef": "01234567", "abc": "abc", "": ""} {
		if got := Claude.ShortID(id); got != want {
			t.Errorf("Claude %q: got %q", id, got)
		}
	}
	for id, want := range map[string]string{"my-session": "-session", "short": "short"} {
		if got := Pi.ShortID(id); got != want {
			t.Errorf("Pi %q: got %q", id, got)
		}
	}
}
