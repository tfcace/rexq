package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tfcace/rexq/internal/extract"
	"github.com/tfcace/rexq/internal/install"
	"github.com/tfcace/rexq/internal/store"
)

const installUsage = `usage:
  rexq install [--key KEY | --no-key]
  rexq install claude [settings.json]
  rexq install rex [--key KEY | --no-key] [--dir DIR]
  rexq install pi [--model provider/model|auto] [extension file]
  rexq uninstall [claude [settings.json] | rex [--dir DIR] | pi [extension file]]

install sets up everything rexq needs, and is safe to run again, as after
an upgrade:

  claude  adds the SessionStart, UserPromptSubmit and Stop hooks to Claude
          Code's settings ($CLAUDE_CONFIG_DIR/settings.json, or
          ~/.claude/settings.json), backing the file up and leaving other
          hooks alone.
  rex     writes the panel's module, rexq.lua, next to Rex's init.lua
          ($REX_CONFIG, or init.lua in $XDG_CONFIG_HOME/rex or ~/.config/rex,
          or in DIR), adds the line that loads it, and reloads Rex's config.
          KEY toggles the panel (shift+cmd+i unless given).
  pi      writes the Pi extension to $PI_CODING_AGENT_DIR/extensions/rexq.ts
          (~/.pi/agent/extensions/rexq.ts). --model also sets the model Pi
          extractions run with, as rexq config set pi-model does. Pi
          support is experimental, so only install pi adds it.

Without a name it does rex, and claude when Claude Code is here (the
command on PATH, or its configuration directory). It doesn't add pi, but
brings an extension that is already installed up to date.

Every one of them runs this binary, where it is now. --bin PATH names
another rexq.

uninstall takes the same things out again, and only what rexq put there.`

// targetFlags are the flags of install and uninstall.
type targetFlags struct {
	fs    *flag.FlagSet
	bin   *string
	key   *string
	noKey *bool
	dir   *string
	model *string
}

// parseTarget reads the flags that target takes ("" is everything) and
// returns what follows them.
func parseTarget(verb, target string, args []string) (*targetFlags, []string, error) {
	f := &targetFlags{fs: flag.NewFlagSet(verb, flag.ContinueOnError)}
	f.fs.SetOutput(io.Discard)
	none := ""
	no := false
	f.bin, f.key, f.noKey, f.dir, f.model = &none, &none, &no, &none, &none
	if verb == "install" {
		f.bin = f.fs.String("bin", "", "")
		if target == "" || target == "rex" {
			f.key = f.fs.String("key", "", "")
			f.noKey = f.fs.Bool("no-key", false, "")
		}
		if target == "pi" {
			f.model = f.fs.String("model", "", "")
		}
	}
	if target == "rex" {
		f.dir = f.fs.String("dir", "", "")
	}
	if err := f.fs.Parse(args); err != nil {
		return nil, nil, fmt.Errorf("%v\n%s", err, installUsage)
	}
	rest := f.fs.Args()
	max := 1
	if target == "" || target == "rex" {
		max = 0
	}
	if len(rest) > max {
		return nil, nil, errors.New(installUsage)
	}
	if *f.key != "" && *f.noKey {
		return nil, nil, errors.New("--key and --no-key don't go together")
	}
	return f, rest, nil
}

func (f *targetFlags) given(name string) bool {
	found := false
	f.fs.Visit(func(fl *flag.Flag) { found = found || fl.Name == name })
	return found
}

// rexKey is the key the Rex line binds: the default unless one was
// given, "" for none.
func (f *targetFlags) rexKey() (string, error) {
	switch {
	case *f.noKey:
		return "", nil
	case f.given("key") && *f.key == "":
		return "", errors.New("--key needs a key, as in shift+cmd+i; use --no-key for none")
	case *f.key != "":
		return *f.key, nil
	}
	return install.DefaultKey, nil
}

// selfPath is the binary the hooks, the panel and the extension will
// run: this one, or the one --bin names, checked.
func selfPath(bin string) (string, error) {
	if bin != "" {
		abs, err := filepath.Abs(bin)
		if err != nil {
			return "", err
		}
		if err := install.CheckBinary(abs); err != nil {
			return "", fmt.Errorf("%v; pass a working rexq to --bin", err)
		}
		return abs, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("can't tell where this rexq is (%v); pass it with --bin", err)
	}
	// go run builds into a directory it deletes on exit.
	if strings.Contains(p, string(filepath.Separator)+"go-build") {
		return "", errors.New("this rexq is a `go run` build that won't be here later; go install it first, or pass an installed one with --bin")
	}
	return p, nil
}

// reloadRex runs rex config reload.
func reloadRex() error {
	rex, err := exec.LookPath("rex")
	if err != nil {
		return errors.New("no rex on PATH")
	}
	out, err := exec.Command(rex, "config", "reload").CombinedOutput()
	if err != nil {
		if line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]); line != "" {
			return errors.New(line)
		}
	}
	return err
}

func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// report prints what one target did, and returns its error labelled.
func report(label string, msgs []string, err error) error {
	for _, m := range msgs {
		fmt.Printf("%s: %s\n", label, m)
	}
	if err != nil {
		return fmt.Errorf("%s: %v", label, err)
	}
	return nil
}

func rexInit(f *targetFlags, home string) string {
	if *f.dir != "" {
		return filepath.Join(*f.dir, "init.lua")
	}
	return install.RexInit(os.Getenv, home)
}

func installMain(args []string) error {
	target := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target, args = args[0], args[1:]
	}
	switch target {
	case "", "claude", "rex", "pi":
	default:
		return errors.New(installUsage)
	}
	f, rest, err := parseTarget("install", target, args)
	if err != nil {
		return err
	}
	key, err := f.rexKey()
	if err != nil {
		return err
	}
	model := *f.model
	if model != "" && model != "auto" && !extract.ValidPiModel(model) {
		return fmt.Errorf("--model %q is not provider/model, as in anthropic/claude-haiku-5-5 or openai/gpt-5-mini, or auto", model)
	}
	if f.given("model") && model == "" {
		return errors.New("--model needs a value: provider/model, as in anthropic/claude-haiku-5-5, or auto")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	bin, err := selfPath(*f.bin)
	if err != nil {
		return err
	}

	claude := func(path string) error {
		msgs, err := install.Claude(path, bin)
		return report("Claude Code", msgs, err)
	}
	rex := func() error {
		msgs, err := install.Rex(install.RexOptions{Init: rexInit(f, home), Bin: bin, Key: key, Reload: reloadRex})
		return report("Rex", msgs, err)
	}
	pi := func(path string) error {
		msgs, err := install.Pi(path, bin)
		if err == nil && model != "" {
			if err = configSet(model); err == nil && model == "auto" {
				msgs = append(msgs, "extractions use a cheap model of each session's provider (automatic)")
			} else if err == nil {
				msgs = append(msgs, "extractions use "+model)
			}
		}
		if err == nil {
			msgs = append(msgs, install.PiBilling)
		}
		return report("Pi", msgs, err)
	}

	arg := func(def string) string {
		if len(rest) > 0 {
			return rest[0]
		}
		return def
	}
	switch target {
	case "claude":
		return claude(arg(install.ClaudeSettings(os.Getenv, home)))
	case "rex":
		return rex()
	case "pi":
		return pi(arg(install.PiExtension(os.Getenv, home)))
	}

	// Everything but Pi, which is experimental and asked for by name:
	// Rex always, Claude Code when it is here.
	var errs []error
	settings := install.ClaudeSettings(os.Getenv, home)
	claudeHere := onPath("claude") || isDir(filepath.Dir(settings))
	if claudeHere {
		errs = append(errs, claude(settings))
	} else {
		fmt.Println("Claude Code: not found, skipped; `rexq install claude` adds its hooks anyway")
	}
	// An extension that is already there follows the binary, as after
	// an upgrade.
	ext := install.PiExtension(os.Getenv, home)
	piInstalled := install.IsPiExtension(ext)
	if piInstalled {
		errs = append(errs, pi(ext))
	} else if onPath("pi") || isDir(install.PiDir(os.Getenv, home)) {
		fmt.Println("Pi: not set up; its support is experimental, and `rexq install pi` adds it")
	}
	errs = append(errs, rex())
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if claudeHere || piInstalled {
		fmt.Println("\nNew agent sessions leave their questions in the panel from now on. `rexq backfill --dry-run` lists earlier ones to pick up.")
	} else {
		fmt.Println("\nClaude Code isn't here yet: run `rexq install` again once it is.")
	}
	return nil
}

func uninstallMain(args []string) error {
	target := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target, args = args[0], args[1:]
	}
	switch target {
	case "", "claude", "rex", "pi":
	default:
		return errors.New(installUsage)
	}
	f, rest, err := parseTarget("uninstall", target, args)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	arg := func(def string) string {
		if len(rest) > 0 {
			return rest[0]
		}
		return def
	}
	claude := func() error {
		msgs, err := install.RemoveClaude(arg(install.ClaudeSettings(os.Getenv, home)))
		return report("Claude Code", msgs, err)
	}
	rex := func() error {
		msgs, err := install.RemoveRex(rexInit(f, home), reloadRex)
		return report("Rex", msgs, err)
	}
	pi := func() error {
		msgs, err := install.RemovePi(arg(install.PiExtension(os.Getenv, home)))
		return report("Pi", msgs, err)
	}
	switch target {
	case "claude":
		return claude()
	case "rex":
		return rex()
	case "pi":
		return pi()
	}
	if err := errors.Join(claude(), pi(), rex()); err != nil {
		return err
	}
	fmt.Printf("\nStill here, for you to delete: the questions in %s", filepath.Dir(store.DefaultPath()))
	if p, err := os.Executable(); err == nil {
		fmt.Printf(", and this binary, %s", p)
	}
	fmt.Println(".")
	return nil
}
