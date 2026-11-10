package extract

import "testing"

func TestPiAutomatic(t *testing.T) {
	for _, c := range []struct{ provider, session, want string }{
		{"anthropic", "claude-opus-5", "anthropic/claude-haiku-5-5"},
		{"openai", "gpt-5", "openai/gpt-5-mini"},
		{"some-local", "qwen", "some-local/qwen"},
		{"", "", ""},
	} {
		if got := PiAutomatic(c.provider, c.session); got != c.want {
			t.Errorf("%s/%s: got %q want %q", c.provider, c.session, got, c.want)
		}
	}
}

func TestValidPiModel(t *testing.T) {
	for _, ok := range []string{"anthropic/claude-haiku-5-5", "openrouter/anthropic/claude-haiku-5.5", "google/gemini-2.5-flash", "local/qwen:7b"} {
		if !ValidPiModel(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "haiku", "anthropic/", "/haiku", "a/b c", `a/b"c`, `a/b\c`, "-x/y", "a/b\n"} {
		if ValidPiModel(bad) {
			t.Errorf("%q taken", bad)
		}
	}
}
