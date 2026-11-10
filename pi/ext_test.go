// Package pi holds rexq's Pi extension (rexq.ts). This test runs the
// extension's own tests (rexq.test.ts) with node, which strips the types.
package pi

import (
	"os/exec"
	"strings"
	"testing"
)

func TestExtension(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	out, err := exec.Command(node, "--test", "rexq.test.ts").CombinedOutput()
	if err != nil && strings.Contains(string(out), "ERR_UNKNOWN_FILE_EXTENSION") {
		t.Skip("this node can't run TypeScript; node 23.6 or newer can")
	}
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}
