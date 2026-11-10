// Package install sets rexq up where the agents and Rex look for it:
// hooks in Claude Code's settings, the module beside Rex's init.lua, and
// the extension among Pi's. It also takes each of them out again.
//
// Every function returns the lines to tell the user, and changes nothing
// it doesn't recognize as its own.
package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// realPath follows path when it is a symlink, so a linked file (as in a
// dotfiles checkout) is written through and the link stays.
func realPath(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return path
}

// writeAtomic replaces the file at path with data, keeping its mode. A
// new file gets newMode, and missing directories dirMode.
func writeAtomic(path string, data []byte, newMode, dirMode os.FileMode) error {
	real := realPath(path)
	mode := newMode
	if fi, err := os.Stat(real); err == nil {
		mode = fi.Mode().Perm()
	} else if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(real), dirMode); err != nil {
			return err
		}
	} else {
		return err
	}
	tmp := real + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, real)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// backup copies the file at path beside it and returns the copy's path,
// or "" when there is no file to copy.
func backup(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	// Nanoseconds, so a second run never overwrites the first backup.
	to := fmt.Sprintf("%s.bak-%d", path, time.Now().UnixNano())
	if err := os.WriteFile(to, data, mode); err != nil {
		return "", err
	}
	return to, nil
}

func backedUp(to string) string {
	if to == "" {
		return ""
	}
	return " (backup at " + to + ")"
}

// CheckBinary refuses a rexq that is missing or can't run a hook cleanly:
// an old or broken one would fail in every agent session.
func CheckBinary(bin string) error {
	fi, err := os.Stat(bin)
	if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is missing or not executable", bin)
	}
	dir, err := os.MkdirTemp("", "rexq-check")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "hook", "bogus")
	// A scratch store, so the check doesn't touch the real one.
	cmd.Env = append(os.Environ(), "REXQ_DB="+filepath.Join(dir, "q.db"))
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return fmt.Errorf("`%s hook bogus` exited %d, want 0; is it an old rexq?", bin, ee.ExitCode())
	}
	if err != nil {
		return fmt.Errorf("%s hook bogus didn't run: %v", bin, err)
	}
	return nil
}

// firstLine is the first line of the file at path, or "" when it can't
// be read.
func firstLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return line
}

// stamp writes value into the one line of src that re matches, as the
// line the stamped file keeps the binary's path on.
func stamp(src string, re *regexp.Regexp, line string) (string, error) {
	if len(re.FindAllString(src, -1)) != 1 {
		return "", fmt.Errorf("no single line matching %s", re)
	}
	return re.ReplaceAllLiteralString(src, line), nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// quote writes s as a double-quoted string that Lua and TypeScript both
// read as s.
func quote(s string) (string, error) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%q has a control character in it", s)
		}
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
}
