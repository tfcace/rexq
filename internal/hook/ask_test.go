package hook

import (
	"strings"
	"testing"
	"time"
)

// While the first-run picker is open in Pi, no Pi turn is extracted with a
// model the user hasn't seen: it waits in the queue, without counting as a
// failed extraction, and runs once the choice is made.
func TestPiTurnsWaitForTheFirstChoice(t *testing.T) {
	r := &promptRunner{out: twoItems}
	d, choices := piDeps(t, nil, r)
	if ok, err := d.Store.ClaimAsk("pi-model", d.Now()); err != nil || !ok {
		t.Fatal(err)
	}
	err := stop(d, piStop("pi-a", "u1", "go on", "Want me to push?"))
	if err == nil || !strings.Contains(err.Error(), "waiting for the extraction model") {
		t.Errorf("note: %v", err)
	}
	if len(*choices) != 0 {
		t.Fatalf("extracted while the picker was open: %+v", *choices)
	}
	if queued(d, "pi-a") != 1 {
		t.Fatal("turn not queued")
	}
	if h := must(d.Store.Health()); h.Failing() {
		t.Errorf("counted as a failed extraction: %+v", h)
	}
	// Claude turns don't wait.
	d.RunnerFor = nil
	d.Runner = r
	stopOK(t, d, turn("c-1", "p1", "Want me to commit?"))
	if len(r.prompts) != 1 {
		t.Errorf("Claude turn waited: %d calls", len(r.prompts))
	}

	// Chosen: the session's next stop runs its queued turn first.
	d2, choices2 := piDeps(t, nil, r)
	d2.Store, d2.Now = d.Store, d.Now
	if err := d.Store.SetChoice("pi-model", "openai/gpt-5-mini"); err != nil {
		t.Fatal(err)
	}
	stopOK(t, d2, piStop("pi-a", "u2", "", "Done."))
	if len(*choices2) != 2 || (*choices2)[0].Configured != "openai/gpt-5-mini" || queued(d, "pi-a") != 0 {
		t.Errorf("after the choice: %+v, %d queued", *choices2, queued(d, "pi-a"))
	}
}

// A picker that never closed (Pi crashed) holds turns back only for a while.
func TestAnAbandonedPickerStopsHoldingTurns(t *testing.T) {
	r := &promptRunner{out: twoItems}
	d, choices := piDeps(t, nil, r)
	d.Store.ClaimAsk("pi-model", d.Now().Add(-time.Hour))
	stopOK(t, d, piStop("pi-a", "u1", "go on", "Want me to push?"))
	if len(*choices) != 1 {
		t.Errorf("held back by an abandoned picker")
	}
}
