package hook

import (
	"strings"
	"testing"

	"github.com/tfcace/rexq/internal/extract"
)

// The Pi extraction model is read from the store when a turn is
// extracted, so live turns and retries from any hook agree on it.
func TestPiExtractionReadsTheSetting(t *testing.T) {
	r := &promptRunner{err: errOffline}
	d, choices := piDeps(t, nil, r)
	if err := d.Store.SetSetting("pi-model", "openai/gpt-5-mini"); err != nil {
		t.Fatal(err)
	}
	stopFails(t, d, piStop("pi-a", "u1", "go on", "Want me to push?"))
	if (*choices)[0].Configured != "openai/gpt-5-mini" {
		t.Errorf("live turn: %+v", (*choices)[0])
	}
	// Changed before the retry: the retry uses the new one.
	d.Store.SetSetting("pi-model", "google/gemini-2.5-flash")
	r.err, r.out = nil, noItems
	d.Now = later(d)
	stopOK(t, d, piStop("pi-b", "u9", "", "Done?"))
	for i, c := range (*choices)[1:] {
		if c.Configured != "google/gemini-2.5-flash" {
			t.Errorf("call %d: %+v", i+1, c)
		}
	}
	// A stop payload's own extract_model, from an older extension, is
	// ignored.
	in := strings.Replace(piStop("pi-c", "u3", "", "Ship?"), `"agent":"pi"`, `"agent":"pi","extract_model":"x/old"`, 1)
	stopOK(t, d, in)
	if c := (*choices)[len(*choices)-1]; c.Configured != "google/gemini-2.5-flash" {
		t.Errorf("payload model used: %+v", c)
	}
	_ = extract.PiModelEnv
}
