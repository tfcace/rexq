package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tfcace/rexq/rex"
)

// DefaultKey toggles the panel unless the user chooses another.
const DefaultKey = "shift+cmd+i"

// RexInit is the config file Rex reads: $REX_CONFIG, or init.lua in
// $XDG_CONFIG_HOME/rex, or in ~/.config/rex.
func RexInit(getenv func(string) string, home string) string {
	if p := getenv("REX_CONFIG"); p != "" {
		return expandHome(p, home)
	}
	base := getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(expandHome(base, home), "rex", "init.lua")
}

// RexOptions says how to set the panel up in Rex.
type RexOptions struct {
	Init string // Rex's init.lua; the module goes beside it
	Bin  string // the rexq binary the panel runs
	Key  string // the key that toggles the panel; "" for none
	// Reload makes Rex read its config again. Its error is reported, not
	// returned: the files are in place either way.
	Reload func() error
}

const (
	rexMarker  = "-- rexq for Rex"
	rexComment = "-- rexq: the panel of questions agents left for you (added by rexq install)"
)

var (
	luaBin = regexp.MustCompile(`(?m)^local BIN = .*$`)
	// A line that loads the module, however it is written.
	luaRequire = regexp.MustCompile(`require\s*\(?\s*["']rexq["']`)
	// A whole line that only loads the module and sets it up: the one
	// rexq install adds, or the README's.
	luaOurLine = regexp.MustCompile(`^\s*require\s*\(?\s*["']rexq["']\s*\)?\s*(\.\s*setup\s*(\{.*\}|\(.*\)))?\s*;?\s*(--.*)?$`)
)

func rexModule(init string) string { return filepath.Join(filepath.Dir(init), "rexq.lua") }

// loadsRexq reports whether the Lua in src loads the module, outside
// comments.
func loadsRexq(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") && luaRequire.MatchString(line) {
			return true
		}
	}
	return false
}

// Rex writes the panel's module beside Rex's init.lua, with the binary's
// path in it, adds the line that loads it to init.lua unless one is
// there, and has Rex reload its config. It won't overwrite a rexq.lua
// that isn't rexq's.
func Rex(o RexOptions) ([]string, error) {
	module := rexModule(o.Init)
	if exists(module) && !strings.HasPrefix(firstLine(module), rexMarker) {
		return nil, fmt.Errorf("%s exists and isn't rexq's module; move it away first", module)
	}
	bin, err := quote(o.Bin)
	if err != nil {
		return nil, err
	}
	src, err := stamp(rex.Module, luaBin, "local BIN = "+bin)
	if err != nil {
		return nil, fmt.Errorf("rexq.lua: %v", err)
	}
	var msgs []string
	changed := false
	if cur, err := os.ReadFile(module); err == nil && string(cur) == src {
		msgs = append(msgs, module+" is already rexq's module")
	} else {
		if err := writeAtomic(module, []byte(src), 0o644, 0o755); err != nil {
			return nil, fmt.Errorf("can't write %s: %v", module, err)
		}
		changed = true
		msgs = append(msgs, "installed the panel's module in "+module)
	}

	cur, err := os.ReadFile(o.Init)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return msgs, fmt.Errorf("can't read %s: %v", o.Init, err)
	}
	if loadsRexq(string(cur)) {
		msgs = append(msgs, o.Init+" already loads rexq; its line stays as it is")
	} else {
		line := `require("rexq").setup{ key = false }`
		if o.Key != "" {
			key, err := quote(o.Key)
			if err != nil {
				return msgs, err
			}
			line = `require("rexq").setup{ key = ` + key + ` }`
		}
		text := string(cur)
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if text != "" {
			text += "\n"
		}
		text += rexComment + "\n" + line + "\n"
		if err := writeAtomic(o.Init, []byte(text), 0o644, 0o755); err != nil {
			return msgs, fmt.Errorf("can't write %s: %v", o.Init, err)
		}
		changed = true
		if o.Key != "" {
			msgs = append(msgs, fmt.Sprintf("added rexq to %s: %s toggles the panel", o.Init, o.Key))
		} else {
			msgs = append(msgs, fmt.Sprintf(`added rexq to %s, without a key: "Toggle Agent Questions" in the command palette shows the panel`, o.Init))
		}
	}
	if changed {
		msgs = append(msgs, reload(o.Reload))
	}
	return msgs, nil
}

func reload(f func() error) string {
	if f == nil {
		return "run `rex config reload` for Rex to pick the change up"
	}
	if err := f(); err != nil {
		return fmt.Sprintf("couldn't reload Rex's config (%v); run `rex config reload` once Rex is running", err)
	}
	return "reloaded Rex's config"
}

// RemoveRex deletes the module beside init.lua (only if it is rexq's)
// and takes the line that loads it out of init.lua, backing the file up
// first. A line that does more than load rexq stays, and is reported.
func RemoveRex(init string, reloadRex func() error) ([]string, error) {
	var msgs []string
	changed := false
	cur, err := os.ReadFile(init)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("can't read %s: %v", init, err)
	}
	if loadsRexq(string(cur)) {
		var kept []string
		removed := false
		for _, line := range strings.Split(string(cur), "\n") {
			if !luaOurLine.MatchString(line) {
				kept = append(kept, line)
				continue
			}
			removed = true
			// The comment above the line goes with it, and one of the
			// blank lines that set the two apart.
			if n := len(kept); n > 0 && strings.HasPrefix(kept[n-1], "-- rexq") {
				kept = kept[:n-1]
			}
			if n := len(kept); n > 0 && kept[n-1] == "" {
				kept = kept[:n-1]
			}
		}
		text := strings.Join(kept, "\n")
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if removed {
			to, err := backup(init)
			if err == nil {
				err = writeAtomic(init, []byte(text), 0o644, 0o755)
			}
			if err != nil {
				return nil, fmt.Errorf("can't write %s: %v", init, err)
			}
			changed = true
			msgs = append(msgs, "removed rexq from "+init+backedUp(to))
		}
		if loadsRexq(text) {
			msgs = append(msgs, init+" still loads rexq in a line rexq didn't write; remove it by hand")
		}
	} else {
		msgs = append(msgs, init+" doesn't load rexq")
	}

	module := rexModule(init)
	switch {
	case !exists(module):
		msgs = append(msgs, module+" doesn't exist")
	case !strings.HasPrefix(firstLine(module), rexMarker):
		return msgs, fmt.Errorf("%s isn't rexq's module; leaving it alone", module)
	default:
		if err := os.Remove(module); err != nil {
			return msgs, fmt.Errorf("can't remove %s: %v", module, err)
		}
		changed = true
		msgs = append(msgs, "removed "+module)
	}
	if changed {
		msgs = append(msgs, reload(reloadRex))
	}
	return msgs, nil
}
