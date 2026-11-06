package rexenv

import (
	"strings"
	"testing"
)

func TestWithoutDropsThePaneIdentity(t *testing.T) {
	got := Without([]string{"HOME=/h", "REX_SESSION=session:1", "REX_BLOCK=block:1", "REX_SOCKET=/s", "REX_SESSIONX=keep"})
	want := []string{"HOME=/h", "REX_SOCKET=/s", "REX_SESSIONX=keep"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %q want %q", got, want)
	}
}
