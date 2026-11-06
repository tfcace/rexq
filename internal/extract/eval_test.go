//go:build eval

package extract

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tfcace/rexq/internal/filter"
)

// evalCase is one testdata/eval file: an agent's final message and what
// extracting it should give.
type evalCase struct {
	Name, Prompt, Message string
	// Min and Max bound the number of items.
	Min, Max int
	// Kinds must each appear among the items.
	Kinds []string
	// Must are substrings that must each appear, ignoring case, in some
	// item's title or quote: the questions that must not be missed.
	Must []string
	// Hard marks a negative the cheap filter lets through, so only the
	// model can drop it.
	Hard bool
	// Open are the session's earlier open items. Superseded must each be
	// listed as asked again, and Kept must not be.
	Open       []OpenItem
	Superseded []int64
	Kept       []int64
}

// TestEval runs the real model on final messages shaped like real sessions and
// checks it finds what a person would. Each case costs a `claude -p` call
// per run. EVAL_RUNS repeats every case and reports pass rates.
//
// Run with: go test -tags eval ./internal/extract/ -run Eval -v
func TestEval(t *testing.T) {
	files, _ := filepath.Glob("testdata/eval/*.json")
	if len(files) == 0 {
		t.Skip("no eval cases")
	}
	runs := 1
	if s := os.Getenv("EVAL_RUNS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("EVAL_RUNS=%q: want a positive number", s)
		}
		runs = n
	}
	var cases []evalCase
	for _, f := range files {
		var c evalCase
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if c.Name == "" || c.Message == "" {
			t.Fatalf("%s: no name or message", f)
		}
		if c.Hard && !filter.LooksOpen(c.Message) {
			t.Fatalf("%s: marked hard, but the filter already drops it", f)
		}
		cases = append(cases, c)
	}
	passed := map[string]int{}
	total := 0
	for run := 1; run <= runs; run++ {
		for _, c := range cases {
			name := c.Name
			if runs > 1 {
				name += "/run" + strconv.Itoa(run)
			}
			if t.Run(name, func(t *testing.T) { evalOne(t, c) }) {
				passed[c.Name]++
				total++
			}
		}
	}
	if runs > 1 {
		for _, c := range cases {
			t.Logf("%-10s passed %d of %d", c.Name, passed[c.Name], runs)
		}
		t.Logf("all cases: passed %d of %d", total, runs*len(cases))
	}
}

func evalOne(t *testing.T, c evalCase) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := Extract(ctx, Claude{}, Input{Message: c.Message, Prompt: c.Prompt, Open: c.Open})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		t.Logf("%-8s %s  %v (suggested %d)", it.Kind, it.Title, it.Replies, it.Suggested)
		for _, r := range it.Replies {
			if n := len(strings.Fields(r)); n > 6 {
				t.Errorf("reply too long (%d words): %q", n, r)
			}
		}
	}
	if n := len(res.Items); n < c.Min || n > c.Max {
		t.Errorf("got %d items, want %d to %d", n, c.Min, c.Max)
	}
	for _, k := range c.Kinds {
		found := false
		for _, it := range res.Items {
			found = found || it.Kind == k
		}
		if !found {
			t.Errorf("no item of kind %q", k)
		}
	}
	t.Logf("superseded %v, resolved %v", res.Superseded, res.Resolved)
	for _, id := range c.Superseded {
		if !slices.Contains(res.Superseded, id) {
			t.Errorf("item %d asked again, not superseded", id)
		}
	}
	for _, id := range c.Kept {
		if slices.Contains(res.Superseded, id) || slices.Contains(res.Resolved, id) {
			t.Errorf("item %d closed, but this message doesn't ask it again", id)
		}
	}
	for _, m := range c.Must {
		found := false
		for _, it := range res.Items {
			text := strings.ToLower(it.Title + "\n" + it.Quote)
			found = found || strings.Contains(text, strings.ToLower(m))
		}
		if !found {
			t.Errorf("no item mentions %q", m)
		}
	}
}
