#!/usr/bin/env python3
"""Add rexq's SessionStart, UserPromptSubmit and Stop hooks to Claude Code's settings,
or take them out again.

usage: install-hooks.py [settings.json [rexq binary]]
       install-hooks.py --uninstall [settings.json]

The settings file defaults to $CLAUDE_CONFIG_DIR/settings.json, or
~/.claude/settings.json without CLAUDE_CONFIG_DIR. The binary defaults to the
rexq on PATH, then $(go env GOBIN)/rexq, then $(go env GOPATH)/bin/rexq.

Other hooks stay as they are. A hook from an earlier rexq install (any
command running "rexq hook <name>") is replaced in place; otherwise each rexq
hook goes in a group of its own. --uninstall removes every rexq hook, and
drops the groups and events that held nothing else. Nothing is written when
nothing changes. The file is backed up first and written atomically, through
a symlink if it is one, keeping its mode. A missing file is created, with its
directory, readable only by you.
"""
import json, os, re, shlex, shutil, stat, subprocess, sys, tempfile, time


def fail(msg):
    print(f"install-hooks: {msg}", file=sys.stderr)
    sys.exit(1)


def check_binary(binary):
    """Refuse a binary that is missing or can't run a hook cleanly: an old
    or broken rexq would fail in every Claude session."""
    if not (os.path.isfile(binary) and os.access(binary, os.X_OK)):
        fail(f"{binary} is missing or not executable; install rexq first, or pass its path")
    with tempfile.TemporaryDirectory() as d:
        # A scratch store, so the check doesn't touch the real one.
        env = dict(os.environ, REXQ_DB=os.path.join(d, "q.db"))
        try:
            r = subprocess.run([binary, "hook", "bogus"], stdin=subprocess.DEVNULL,
                               capture_output=True, env=env, timeout=10)
        except (OSError, subprocess.TimeoutExpired) as e:
            fail(f"{binary} hook bogus didn't run: {e}")
    if r.returncode != 0:
        fail(f"`{binary} hook bogus` exited {r.returncode}, want 0; is it an old rexq?")


def ours(name):
    """Matches a command running rexq's hook name: the old form
    (/path/rexq hook stop) and the quoted new one."""
    return re.compile(r"rexq['\"]?\s+hook\s+" + re.escape(name) + r"\b")


def install(groups, hook, name):
    """Put hook into groups, replacing earlier rexq hooks for name."""
    ours_re = ours(name)
    placed = False
    emptied = []
    for g in groups:
        if not isinstance(g, dict) or not isinstance(g.get("hooks"), list):
            continue
        kept = []
        for h in g["hooks"]:
            if isinstance(h, dict) and isinstance(h.get("command"), str) and ours_re.search(h["command"]):
                if not placed:
                    kept.append(dict(hook))
                    placed = True
                continue
            kept.append(h)
        if g["hooks"] and not kept:
            emptied.append(g)
        g["hooks"] = kept
    groups[:] = [g for g in groups if not any(g is e for e in emptied)]
    if not placed:
        groups.append({"hooks": [dict(hook)]})


def write_atomic(path, settings):
    real = os.path.realpath(path)
    try:
        mode = stat.S_IMODE(os.stat(real).st_mode)
    except FileNotFoundError:
        # A new file: settings can hold tokens, so only the user reads it.
        mode = 0o600
        os.makedirs(os.path.dirname(real), mode=0o700, exist_ok=True)
    tmp = real + ".tmp"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, mode)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            # ensure_ascii=False keeps the file's other text as it was, not \u-escaped.
            json.dump(settings, f, indent=2, ensure_ascii=False)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, mode)
        os.replace(tmp, real)
    except BaseException:
        if os.path.exists(tmp):
            os.unlink(tmp)
        raise


def uninstall(groups, name):
    """Take rexq's hooks for name out of groups, dropping the groups this
    empties. Returns whether it removed any."""
    ours_re = ours(name)
    removed = False
    emptied = []
    for g in groups:
        if not isinstance(g, dict) or not isinstance(g.get("hooks"), list):
            continue
        kept = [h for h in g["hooks"]
                if not (isinstance(h, dict) and isinstance(h.get("command"), str) and ours_re.search(h["command"]))]
        if len(kept) != len(g["hooks"]):
            removed = True
            if not kept:
                emptied.append(g)
        g["hooks"] = kept
    groups[:] = [g for g in groups if not any(g is e for e in emptied)]
    return removed


EVENTS = [("SessionStart", "start"), ("UserPromptSubmit", "prompt"), ("Stop", "stop")]


def load(path):
    """Read the settings at path. A missing file reads as {}."""
    try:
        with open(path, encoding="utf-8") as f:
            settings = json.load(f)
    except FileNotFoundError:
        return {}
    except json.JSONDecodeError as e:
        fail(f"{path} is not valid JSON ({e}); fix it first")
    except (OSError, UnicodeDecodeError) as e:
        fail(f"can't read {path}: {e}")
    if not isinstance(settings, dict):
        fail(f"{path} doesn't hold a JSON object")
    hooks = settings.get("hooks", {})
    if not isinstance(hooks, dict):
        fail(f'"hooks" in {path} is not an object')
    return settings


def save(path, settings):
    """Back up path, then write settings to it. Returns the backup's path,
    or None when there was no file to back up."""
    # Nanoseconds, so a second run never overwrites the first backup.
    backup = f"{path}.bak-{time.time_ns()}"
    try:
        if os.path.exists(path):
            shutil.copy2(path, backup)
        else:
            backup = None
        write_atomic(path, settings)
    except OSError as e:
        fail(f"can't write {path}: {e}")
    return backup


def default_settings():
    """$CLAUDE_CONFIG_DIR/settings.json, as Claude Code reads it, or
    ~/.claude/settings.json."""
    base = os.environ.get("CLAUDE_CONFIG_DIR") or os.path.expanduser("~/.claude")
    return os.path.join(os.path.expanduser(base), "settings.json")


def go_env(name):
    """`go env name`, or "" when go isn't there or fails."""
    go = shutil.which("go")
    if not go:
        return ""
    try:
        r = subprocess.run([go, "env", name], stdin=subprocess.DEVNULL,
                           capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.TimeoutExpired):
        return ""
    return r.stdout.strip() if r.returncode == 0 else ""


def default_binary():
    """The rexq on PATH, else where `go install` puts it."""
    found = shutil.which("rexq")
    if found:
        return os.path.abspath(found)
    gobin = go_env("GOBIN")
    if gobin:
        return os.path.join(gobin, "rexq")
    # GOPATH may list several directories; go install uses the first.
    gopath = go_env("GOPATH").split(os.pathsep)[0]
    if gopath:
        return os.path.join(gopath, "bin", "rexq")
    fail("can't find rexq on PATH or through `go env`; pass its path")


def saved(backup):
    return f" (backup at {backup})" if backup else ""


def main_uninstall(args):
    path = args[0] if args else default_settings()
    settings = load(path)
    hooks = settings.get("hooks", {})
    changed = False
    for event, name in EVENTS:
        groups = hooks.get(event)
        if groups is None:
            continue
        if not isinstance(groups, list):
            fail(f'"hooks.{event}" in {path} is not a list')
        if uninstall(groups, name):
            changed = True
            if not groups:
                del hooks[event]
    if not changed:
        print(f"{path} has no rexq hooks")
        return
    backup = save(path, settings)
    print(f"removed rexq's hooks from {path}{saved(backup)}")


def main():
    if len(sys.argv) > 1 and sys.argv[1] == "--uninstall":
        main_uninstall(sys.argv[2:])
        return
    path = sys.argv[1] if len(sys.argv) > 1 else default_settings()
    binary = sys.argv[2] if len(sys.argv) > 2 else default_binary()
    check_binary(binary)

    # "|| true": exit 2 from UserPromptSubmit blocks and erases the user's
    # prompt, and from Stop makes Claude keep working. rexq exits 0 itself;
    # this covers a broken or outdated binary.
    def command(name):
        return f"{shlex.quote(binary)} hook {name} || true"

    wanted = [
        ("SessionStart", "start", {"type": "command", "command": command("start"), "timeout": 5}),
        ("UserPromptSubmit", "prompt", {"type": "command", "command": command("prompt"), "timeout": 5}),
        ("Stop", "stop", {"type": "command", "command": command("stop"), "async": True}),
    ]

    settings = load(path)
    hooks = settings.setdefault("hooks", {})
    before = json.dumps(settings, sort_keys=True)
    for event, name, hook in wanted:
        groups = hooks.setdefault(event, [])
        if not isinstance(groups, list):
            fail(f'"hooks.{event}" in {path} is not a list')
        install(groups, hook, name)
    if json.dumps(settings, sort_keys=True) == before:
        print(f"{path} already has rexq's hooks")
        return

    backup = save(path, settings)
    print(f"installed rexq's hooks in {path}{saved(backup)}")


main()
