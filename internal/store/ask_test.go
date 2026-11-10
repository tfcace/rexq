package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A choice the user is asked for once per store: never asked, asked (the
// picker is open somewhere), and answered are told apart.
func TestAskState(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_000_000, 0)
	state := func(at time.Time) string {
		t.Helper()
		st, err := s.AskState("pi-model", at)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if got := state(now); got != AskNever {
		t.Fatalf("new store: %q", got)
	}
	// Only the first claim shows the picker.
	if ok, err := s.ClaimAsk("pi-model", now); err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if ok, _ := s.ClaimAsk("pi-model", now); ok {
		t.Fatal("second claim won too")
	}
	if got := state(now.Add(time.Minute)); got != AskOpen {
		t.Errorf("while open: %q", got)
	}
	// A picker that never answered (Pi crashed) stops counting as open.
	if got := state(now.Add(AskWait + time.Second)); got != AskDone {
		t.Errorf("after the wait: %q", got)
	}
	if ok, _ := s.ClaimAsk("pi-model", now.Add(time.Hour)); ok {
		t.Error("asked again after a crash")
	}
	// Esc: answered, model still automatic.
	if err := s.DismissAsk("pi-model"); err != nil {
		t.Fatal(err)
	}
	if got := state(now); got != AskDone {
		t.Errorf("dismissed: %q", got)
	}
	if v, _ := s.Setting("pi-model"); v != "" {
		t.Errorf("dismiss set %q", v)
	}
}

// Choosing, Automatic included, answers it, also when nobody was asked.
func TestSetChoiceAnswers(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_000_000, 0)
	if err := s.SetChoice("pi-model", ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.AskState("pi-model", now); st != AskDone {
		t.Errorf("automatic chosen: %q", st)
	}
	if ok, _ := s.ClaimAsk("pi-model", now); ok {
		t.Error("asked after a choice")
	}
	if err := s.SetChoice("pi-model", "openai/gpt-5-mini"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Setting("pi-model"); v != "openai/gpt-5-mini" {
		t.Errorf("value %q", v)
	}
}

// Sessions starting together, each its own process with its own
// connection: exactly one claims the picker.
func TestClaimAskOnceAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	must(Open(path)).Close()
	var wg sync.WaitGroup
	won := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(path)
			if err != nil {
				t.Error(err)
				return
			}
			defer s.Close()
			ok, err := s.ClaimAsk("pi-model", time.Now())
			if err != nil {
				t.Error(err)
			}
			won <- ok
		}()
	}
	wg.Wait()
	close(won)
	n := 0
	for ok := range won {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d claims won, want 1", n)
	}
}
