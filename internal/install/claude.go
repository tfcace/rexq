package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// claudeEvents are the Claude Code hook events rexq listens to, with the
// rexq hook each runs.
var claudeEvents = []struct{ event, name string }{
	{"SessionStart", "start"},
	{"UserPromptSubmit", "prompt"},
	{"Stop", "stop"},
}

// ClaudeSettings is the settings file Claude Code reads:
// $CLAUDE_CONFIG_DIR/settings.json, or ~/.claude/settings.json.
func ClaudeSettings(getenv func(string) string, home string) string {
	base := getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		base = filepath.Join(home, ".claude")
	}
	return filepath.Join(expandHome(base, home), "settings.json")
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return path
}

// ours matches a command running rexq's hook name: the form an early
// installer wrote (/path/rexq hook stop) and the quoted one.
func ours(name string) *regexp.Regexp {
	return regexp.MustCompile(`rexq['"]?\s+hook\s+` + regexp.QuoteMeta(name) + `\b`)
}

func isOurs(h any, re *regexp.Regexp) bool {
	o, ok := h.(*object)
	if !ok {
		return false
	}
	cmd, ok := o.vals["command"].(string)
	return ok && re.MatchString(cmd)
}

// hooksOf returns the hooks of a group, or false for a group rexq
// doesn't understand and leaves alone.
func hooksOf(g any) (*object, []any, bool) {
	o, ok := g.(*object)
	if !ok {
		return nil, nil, false
	}
	hooks, ok := o.vals["hooks"].([]any)
	return o, hooks, ok
}

// place puts hook into groups, replacing earlier rexq hooks for name in
// place. Without one to replace, the hook goes in a group of its own.
func place(groups []any, hook *object, name string) []any {
	re := ours(name)
	placed := false
	out := []any{}
	for _, g := range groups {
		o, hooks, ok := hooksOf(g)
		if !ok {
			out = append(out, g)
			continue
		}
		kept := []any{}
		for _, h := range hooks {
			if !isOurs(h, re) {
				kept = append(kept, h)
			} else if !placed {
				kept = append(kept, hook)
				placed = true
			}
		}
		o.set("hooks", kept)
		// A group that held only rexq hooks already placed goes.
		if len(hooks) == 0 || len(kept) > 0 {
			out = append(out, g)
		}
	}
	if !placed {
		out = append(out, obj("hooks", []any{hook}))
	}
	return out
}

// remove takes rexq's hooks for name out of groups, dropping the groups
// this empties, and reports whether it removed any.
func remove(groups []any, name string) ([]any, bool) {
	re := ours(name)
	removed := false
	out := []any{}
	for _, g := range groups {
		o, hooks, ok := hooksOf(g)
		if !ok {
			out = append(out, g)
			continue
		}
		kept := []any{}
		for _, h := range hooks {
			if !isOurs(h, re) {
				kept = append(kept, h)
			}
		}
		if len(kept) != len(hooks) {
			removed = true
			o.set("hooks", kept)
			if len(kept) == 0 {
				continue
			}
		}
		out = append(out, g)
	}
	return out, removed
}

// loadSettings reads the settings at path. A missing file reads as {}.
func loadSettings(path string) (*object, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return obj(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("can't read %s: %v", path, err)
	}
	v, err := parseJSON(data)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%v); fix it first", path, err)
	}
	settings, ok := v.(*object)
	if !ok {
		return nil, fmt.Errorf("%s doesn't hold a JSON object", path)
	}
	if hooks, ok := settings.get("hooks"); ok {
		if _, ok := hooks.(*object); !ok {
			return nil, fmt.Errorf(`"hooks" in %s is not an object`, path)
		}
	}
	return settings, nil
}

// saveSettings backs path up, then writes settings to it. A new file is
// readable only by the user: settings can hold tokens.
func saveSettings(path string, settings *object) (string, error) {
	to, err := backup(path)
	if err == nil {
		err = writeAtomic(path, encodeJSON(settings), 0o600, 0o700)
	}
	if err != nil {
		return "", fmt.Errorf("can't write %s: %v", path, err)
	}
	return to, nil
}

// shellQuote quotes s for the shell Claude Code runs hooks with.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r))
	}) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Claude adds rexq's SessionStart, UserPromptSubmit and Stop hooks,
// running bin, to the Claude Code settings at path. Other hooks stay as
// they are, and a hook from an earlier install is replaced in place.
// Nothing is written when nothing changes; otherwise the file is backed
// up first.
func Claude(path, bin string) ([]string, error) {
	settings, err := loadSettings(path)
	if err != nil {
		return nil, err
	}
	before := string(encodeJSON(settings))
	hooks, _ := settings.vals["hooks"].(*object)
	if hooks == nil {
		hooks = obj()
		settings.set("hooks", hooks)
	}
	for _, e := range claudeEvents {
		// "|| true": exit 2 from UserPromptSubmit blocks and erases the
		// user's prompt, and from Stop makes Claude keep working. rexq
		// exits 0 itself; this covers a broken or outdated binary.
		hook := obj("type", "command", "command", shellQuote(bin)+" hook "+e.name+" || true")
		if e.name == "stop" {
			hook.set("async", true)
		} else {
			hook.set("timeout", json.Number("5"))
		}
		var groups []any
		if v, ok := hooks.get(e.event); ok {
			if groups, ok = v.([]any); !ok {
				return nil, fmt.Errorf(`"hooks.%s" in %s is not a list`, e.event, path)
			}
		}
		hooks.set(e.event, place(groups, hook, e.name))
	}
	if string(encodeJSON(settings)) == before {
		return []string{path + " already has rexq's hooks"}, nil
	}
	to, err := saveSettings(path, settings)
	if err != nil {
		return nil, err
	}
	return []string{"installed rexq's hooks in " + path + backedUp(to)}, nil
}

// RemoveClaude takes rexq's hooks out of the settings at path, with the
// groups and events that held nothing else.
func RemoveClaude(path string) ([]string, error) {
	settings, err := loadSettings(path)
	if err != nil {
		return nil, err
	}
	hooks, _ := settings.vals["hooks"].(*object)
	changed := false
	for _, e := range claudeEvents {
		if hooks == nil {
			break
		}
		v, ok := hooks.get(e.event)
		if !ok {
			continue
		}
		groups, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf(`"hooks.%s" in %s is not a list`, e.event, path)
		}
		groups, removed := remove(groups, e.name)
		if !removed {
			continue
		}
		changed = true
		if len(groups) == 0 {
			hooks.del(e.event)
		} else {
			hooks.set(e.event, groups)
		}
	}
	if !changed {
		return []string{path + " has no rexq hooks"}, nil
	}
	to, err := saveSettings(path, settings)
	if err != nil {
		return nil, err
	}
	return []string{"removed rexq's hooks from " + path + backedUp(to)}, nil
}
