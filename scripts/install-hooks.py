#!/usr/bin/env python3
"""Add rexq's SessionStart, UserPromptSubmit and Stop hooks to Claude Code's settings,
or take them out again. With --pi, install or remove rexq's Pi extension instead.

usage: install-hooks.py [settings.json [rexq binary]]
       install-hooks.py --uninstall [settings.json]
       install-hooks.py --pi [--model provider/model] [extension file [rexq binary]]
       install-hooks.py --pi --uninstall [extension file]

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

--pi writes pi/rexq.ts from this repository to $PI_CODING_AGENT_DIR/extensions/
rexq.ts (~/.pi/agent/extensions/rexq.ts without PI_CODING_AGENT_DIR), with the
rexq binary's path written into it. It won't overwrite a rexq.ts that isn't
rexq's. --model provider/model (or auto) also sets the model Pi extractions run
with, as `rexq config set pi-model` and /rexq-model in Pi do; without it the
setting stays as it is. --pi --uninstall deletes the file, only if it is rexq's.
Running Pi sessions load the change on /reload.
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


PI_MARKER = "// rexq Pi extension: installed by scripts/install-hooks.py --pi."

# provider/model: a provider id, a slash, and a model id that may hold more
# slashes (openrouter/anthropic/claude-haiku-5.5), without spaces or quotes.
PI_MODEL = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._:@+~-][A-Za-z0-9._:@+~/-]*")


def default_pi_extension():
    """$PI_CODING_AGENT_DIR/extensions/rexq.ts, where Pi finds global
    extensions, or ~/.pi/agent/extensions/rexq.ts."""
    base = os.environ.get("PI_CODING_AGENT_DIR") or os.path.expanduser("~/.pi/agent")
    return os.path.join(os.path.expanduser(base), "extensions", "rexq.ts")


def is_ours(path):
    """Whether the file at path is an installed rexq extension."""
    try:
        with open(path, encoding="utf-8") as f:
            return f.readline().rstrip("\n") == PI_MARKER
    except (OSError, UnicodeDecodeError):
        return False


def pi_source(binary):
    """pi/rexq.ts with the binary written in."""
    here = os.path.dirname(os.path.realpath(__file__))
    template = os.path.join(here, "..", "pi", "rexq.ts")
    try:
        with open(template, encoding="utf-8") as f:
            src = f.read()
    except OSError as e:
        fail(f"can't read the extension {template}: {e}")
    if not src.startswith(PI_MARKER + "\n"):
        fail(f"{template} doesn't look like rexq's Pi extension")
    for name, value in (("REXQ_BIN", binary),):
        line = re.compile(r"^const " + name + r" = .*;$", re.M)
        if len(line.findall(src)) != 1:
            fail(f"{template} has no single `const {name} = ...;` line")
        # json.dumps writes a valid TypeScript string literal.
        src = line.sub(lambda m: f"const {name} = {json.dumps(value)};", src)
    return src


def main_pi(args):
    uninstall = False
    model = ""
    rest = []
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--uninstall":
            uninstall = True
        elif a == "--model":
            if i + 1 >= len(args):
                fail("--model needs a value: provider/model, as in anthropic/claude-haiku-5-5, or auto")
            model = args[i + 1]
            i += 1
        elif a.startswith("--model="):
            model = a[len("--model="):]
        elif a.startswith("-"):
            fail(f"unknown option {a}")
        else:
            rest.append(a)
        i += 1
    if uninstall:
        if model:
            fail("--model has nothing to do with --uninstall")
        if len(rest) > 1:
            fail("usage: install-hooks.py --pi --uninstall [extension file]")
        path = rest[0] if rest else default_pi_extension()
        if not os.path.lexists(path):
            print(f"{path} doesn't exist; rexq's Pi extension isn't installed there")
            return
        if not is_ours(path):
            fail(f"{path} isn't rexq's Pi extension; leaving it alone")
        try:
            os.unlink(path)
        except OSError as e:
            fail(f"can't remove {path}: {e}")
        print(f"removed rexq's Pi extension {path}; /reload running Pi sessions")
        return
    if len(rest) > 2:
        fail("usage: install-hooks.py --pi [--model provider/model] [extension file [rexq binary]]")
    if model and model != "auto" and not PI_MODEL.fullmatch(model):
        fail(f"--model {model!r} is not provider/model, as in anthropic/claude-haiku-5-5 or openai/gpt-5-mini, or auto")
    path = rest[0] if rest else default_pi_extension()
    if not path or os.path.isdir(path):
        fail(f"{path!r} is not a file path for the extension")
    binary = rest[1] if len(rest) > 1 else default_binary()
    binary = os.path.abspath(binary)
    check_binary(binary)
    if os.path.lexists(path) and not is_ours(path):
        fail(f"{path} exists and isn't rexq's Pi extension; move it away first")
    src = pi_source(binary)
    try:
        with open(path, encoding="utf-8") as f:
            current = f.read() == src
    except (OSError, UnicodeDecodeError):
        current = False
    if current:
        print(f"{path} already has rexq's Pi extension")
    else:
        write_extension(path, src)
        print(f"installed rexq's Pi extension in {path}; /reload running Pi sessions")
    if model:
        set_pi_model(binary, model)
    print("Every Pi extraction is a pi -p call, billed to the provider of the model it uses (with Anthropic"
          " subscription auth, as extra usage). Choose the model with /rexq-model in Pi.")


def set_pi_model(binary, model):
    """Set the pi-model setting through rexq, which keeps it in its store."""
    try:
        r = subprocess.run([binary, "config", "set", "pi-model", model], stdin=subprocess.DEVNULL,
                           capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.TimeoutExpired) as e:
        fail(f"`{binary} config set pi-model` didn't run: {e}")
    if r.returncode != 0:
        fail(f"`{binary} config set pi-model {model}` failed: {(r.stderr or r.stdout).strip()}")
    if model == "auto":
        print("Pi extractions use a cheap model of each session's provider (automatic)")
    else:
        print(f"Pi extractions use {model}")


def write_extension(path, src):
    """Write src to path atomically."""
    real = os.path.realpath(path)
    tmp = real + ".tmp"
    try:
        os.makedirs(os.path.dirname(real), exist_ok=True)
        with open(tmp, "w", encoding="utf-8") as f:
            f.write(src)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, real)
    except OSError as e:
        if os.path.exists(tmp):
            os.unlink(tmp)
        fail(f"can't write {path}: {e}")


def main():
    if "--pi" in sys.argv[1:]:
        main_pi([a for a in sys.argv[1:] if a != "--pi"])
        return
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
