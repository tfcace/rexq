package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(kv ...string) func(string) string {
	return func(k string) string {
		for i := 0; i < len(kv); i += 2 {
			if kv[i] == k {
				return kv[i+1]
			}
		}
		return ""
	}
}

func TestRexInit(t *testing.T) {
	for _, c := range []struct {
		env  func(string) string
		want string
	}{
		{env(), "/home/me/.config/rex/init.lua"},
		{env("XDG_CONFIG_HOME", "/xdg"), "/xdg/rex/init.lua"},
		{env("XDG_CONFIG_HOME", "/xdg", "REX_CONFIG", "/etc/rex/mine.lua"), "/etc/rex/mine.lua"},
	} {
		if got := RexInit(c.env, "/home/me"); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRex(t *testing.T) {
	dir := t.TempDir()
	init := filepath.Join(dir, "rex", "init.lua")
	module := filepath.Join(dir, "rex", "rexq.lua")
	reloads := 0
	o := RexOptions{Init: init, Bin: `/Users/my "name"/rexq`, Key: DefaultKey, Reload: func() error { reloads++; return nil }}

	// Nothing there yet: the directory, the module and init.lua appear.
	msgs, err := Rex(o)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, module); !strings.HasPrefix(got, rexMarker) ||
		!strings.Contains(got, "\nlocal BIN = \"/Users/my \\\"name\\\"/rexq\"\n") ||
		strings.Count(got, "\n") != strings.Count(read(t, "../../rex/rexq.lua"), "\n") {
		t.Errorf("the module is not the repository's with the binary written in:\n%s", got)
	}
	want := rexComment + "\n" + `require("rexq").setup{ key = "shift+cmd+i" }` + "\n"
	if got := read(t, init); got != want {
		t.Errorf("init.lua:\n%s", got)
	}
	if reloads != 1 || !strings.Contains(strings.Join(msgs, "\n"), "reloaded Rex's config") {
		t.Errorf("reloads %d: %q", reloads, msgs)
	}

	// Again: nothing changes, and Rex isn't reloaded.
	msgs, err = Rex(o)
	if err != nil || reloads != 1 || read(t, init) != want || len(msgs) != 2 {
		t.Errorf("second run: %v, reloads %d, %q", err, reloads, msgs)
	}

	// A new binary rewrites the module only; the line in init.lua, with
	// whatever the user made of it, stays.
	mine := "-- my config\nrequire('rexq').setup{ key = \"cmd+j\", bounds = {0, 0, 1, 1} }\n"
	os.WriteFile(init, []byte(mine), 0o644)
	o.Bin, o.Key = "/opt/rexq", "cmd+k"
	if _, err = Rex(o); err != nil || read(t, init) != mine || reloads != 2 ||
		!strings.Contains(read(t, module), `local BIN = "/opt/rexq"`) {
		t.Errorf("upgrade: %v, reloads %d", err, reloads)
	}

	// A config with other things in it gets the line at its end, after a
	// blank line; a commented-out require doesn't count as loading it.
	other := "rex.bind(\"cmd+x\", \"y\")\n-- require(\"rexq\").setup{}"
	os.WriteFile(init, []byte(other), 0o600)
	os.Chmod(init, 0o600)
	o.Key = ""
	o.Reload = func() error { return errors.New("no server") }
	msgs, err = Rex(o)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, init); got != other+"\n\n"+rexComment+"\n"+`require("rexq").setup{ key = false }`+"\n" {
		t.Errorf("init.lua:\n%s", got)
	}
	if fi, _ := os.Stat(init); fi.Mode().Perm() != 0o600 {
		t.Errorf("init.lua's mode changed to %v", fi.Mode().Perm())
	}
	if all := strings.Join(msgs, "\n"); !strings.Contains(all, "no server") || !strings.Contains(all, "rex config reload") {
		t.Errorf("a failed reload isn't explained: %q", msgs)
	}

	// Uninstall takes the line, its comment and the blank line out, backs
	// init.lua up, and deletes the module.
	msgs, err = RemoveRex(init, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, init); got != other+"\n" {
		t.Errorf("init.lua after uninstall:\n%q", got)
	}
	if backups, _ := filepath.Glob(init + ".bak-*"); len(backups) != 1 {
		t.Errorf("backups: %q", backups)
	}
	if exists(module) {
		t.Error("the module is still there")
	}
	// Nothing left: says so, changes nothing.
	msgs, err = RemoveRex(init, func() error { t.Error("reloaded for nothing"); return nil })
	if err != nil || len(msgs) != 2 {
		t.Errorf("second uninstall: %v %q", err, msgs)
	}
	if backups, _ := filepath.Glob(init + ".bak-*"); len(backups) != 1 {
		t.Errorf("second uninstall backed up: %q", backups)
	}
}

// Someone else's rexq.lua, and a line that does more than load rexq, are
// left alone.
func TestRexLeavesForeignFilesAlone(t *testing.T) {
	dir := t.TempDir()
	init := filepath.Join(dir, "init.lua")
	module := filepath.Join(dir, "rexq.lua")
	os.WriteFile(module, []byte("-- mine\nreturn {}\n"), 0o644)
	if _, err := Rex(RexOptions{Init: init, Bin: "/rexq", Key: "k"}); err == nil || !strings.Contains(err.Error(), "isn't rexq's") {
		t.Errorf("overwrote a foreign module: %v", err)
	}
	if exists(init) {
		t.Error("init.lua written despite the refusal")
	}
	custom := "local q = require(\"rexq\")\nq.setup{}\n"
	os.WriteFile(init, []byte(custom), 0o644)
	msgs, err := RemoveRex(init, nil)
	if err == nil || !strings.Contains(err.Error(), "leaving it alone") {
		t.Errorf("removed a foreign module: %v", err)
	}
	if read(t, init) != custom || read(t, module) != "-- mine\nreturn {}\n" {
		t.Error("a foreign file changed")
	}
	if !strings.Contains(strings.Join(msgs, "\n"), "by hand") {
		t.Errorf("the line left behind isn't mentioned: %q", msgs)
	}
	if _, err := Rex(RexOptions{Init: filepath.Join(t.TempDir(), "init.lua"), Bin: "/a\nb/rexq"}); err == nil {
		t.Error("a path with a newline was written into the module")
	}
}
