package store

import (
	"path/filepath"
	"testing"
)

func TestSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	s := must(Open(path))
	if v, err := s.Setting("pi-model"); err != nil || v != "" {
		t.Fatalf("unset: %q %v", v, err)
	}
	if err := s.SetSetting("pi-model", "openai/gpt-5-mini"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Another process sees it.
	s = must(Open(path))
	defer s.Close()
	if v, _ := s.Setting("pi-model"); v != "openai/gpt-5-mini" {
		t.Fatalf("got %q", v)
	}
	if err := s.SetSetting("pi-model", "anthropic/claude-haiku-5-5"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Setting("pi-model"); v != "anthropic/claude-haiku-5-5" {
		t.Fatalf("got %q", v)
	}
	// Empty clears it.
	if err := s.SetSetting("pi-model", ""); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM settings`).Scan(&n)
	if v, _ := s.Setting("pi-model"); v != "" || n != 0 {
		t.Fatalf("not cleared: %q, %d rows", v, n)
	}
}

// A version 1 store gets the settings table when it migrates.
func TestMigrateAddsSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	writeV1Store(t, path)
	s := must(Open(path))
	defer s.Close()
	if err := s.SetSetting("pi-model", "a/b"); err != nil {
		t.Fatal(err)
	}
}
