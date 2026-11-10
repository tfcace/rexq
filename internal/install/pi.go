package install

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tfcace/rexq/pi"
)

// PiExtension is where Pi finds rexq's extension:
// $PI_CODING_AGENT_DIR/extensions/rexq.ts, or under ~/.pi/agent.
func PiExtension(getenv func(string) string, home string) string {
	return filepath.Join(PiDir(getenv, home), "extensions", "rexq.ts")
}

// PiDir is Pi's agent directory.
func PiDir(getenv func(string) string, home string) string {
	base := getenv("PI_CODING_AGENT_DIR")
	if base == "" {
		base = filepath.Join(home, ".pi", "agent")
	}
	return expandHome(base, home)
}

// piMarker starts the first line of an installed extension, whichever
// installer wrote it.
const piMarker = "// rexq Pi extension:"

// PiBilling is what an install tells the user about cost.
const PiBilling = "every Pi extraction is a pi -p call, billed to the provider of the model it uses (with Anthropic" +
	" subscription auth, as extra usage); choose the model with /rexq-model in Pi"

var tsBin = regexp.MustCompile(`(?m)^const REXQ_BIN = .*;$`)

// IsPiExtension reports whether the file at path is rexq's extension.
func IsPiExtension(path string) bool { return strings.HasPrefix(firstLine(path), piMarker) }

// Pi writes rexq's Pi extension to path, with the binary's path in it.
// It won't overwrite a file that isn't rexq's.
func Pi(path, bin string) ([]string, error) {
	if fi, err := os.Stat(path); path == "" || err == nil && fi.IsDir() {
		return nil, fmt.Errorf("%q is not a file path for the extension", path)
	}
	if exists(path) && !IsPiExtension(path) {
		return nil, fmt.Errorf("%s exists and isn't rexq's Pi extension; move it away first", path)
	}
	quoted, err := quote(bin)
	if err != nil {
		return nil, err
	}
	src, err := stamp(pi.Extension, tsBin, "const REXQ_BIN = "+quoted+";")
	if err != nil {
		return nil, fmt.Errorf("rexq.ts: %v", err)
	}
	if cur, err := os.ReadFile(path); err == nil && string(cur) == src {
		return []string{path + " already has rexq's Pi extension"}, nil
	}
	if err := writeAtomic(path, []byte(src), 0o644, 0o755); err != nil {
		return nil, fmt.Errorf("can't write %s: %v", path, err)
	}
	return []string{"installed rexq's Pi extension in " + path + "; /reload running Pi sessions"}, nil
}

// RemovePi deletes the extension at path, only if it is rexq's.
func RemovePi(path string) ([]string, error) {
	if !exists(path) {
		return []string{path + " doesn't exist; rexq's Pi extension isn't installed there"}, nil
	}
	if !IsPiExtension(path) {
		return nil, fmt.Errorf("%s isn't rexq's Pi extension; leaving it alone", path)
	}
	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("can't remove %s: %v", path, err)
	}
	return []string{"removed rexq's Pi extension " + path + "; /reload running Pi sessions"}, nil
}
