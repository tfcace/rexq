# rexq

rexq collects the questions Claude Code and [Pi](https://pi.dev) agents leave for you (decisions, vetoes, things to do by hand, leftovers) into one panel inside [Rex](https://www.superlogical.com/rex). A Claude `Stop` hook sends each final message to Haiku through `claude -p` on your own login (a Pi extension does the same through `pi -p`), stores the items it finds in SQLite, and a floating Rex panel lists them. From the panel you answer, snooze, mark done, or jump to the agent.

![The Agent questions panel floating over a Rex tab: questions from two agents, one snoozed, with quick answers for the selected one](assets/panel.png)

## Requirements

- macOS only: it is built and tested there.
- Go 1.26 or newer, to build it.
- Claude Code, tested with 2.1.319. rexq uses async `Stop` hooks, `SessionStart` and `UserPromptSubmit` hooks, and `claude -p --safe-mode --json-schema --strict-mcp-config`.
- Or Pi, tested with 1.5.1, instead of or beside Claude Code (see [Pi](#pi)). Pi sessions don't need Claude Code installed. Running the extension's tests needs Node 23.6 or newer.
- Rex, tested with build 0.0.1793645657 (2026-11-02). The panel module uses the Lua APIs `rex.action`, `rex.bind` and `rex.session` (`view`, `list_blocks`, `new_block`, `move_block`, `float_block`, `focus_block`, `detach_block`). The binary runs the `rex` CLI: `block inspect`, `block call` (`program_status`, `process`, `title`), `capture`, `send`, `send-key`, `run`, `focus`, `whoami`, `client ls`, `do session.select` and `api call` (`session.view`, `session.detach_block`).
- `python3` for the hook installer, and `ps` and `lsof` (part of macOS), which the panel uses to find running Claude processes.
- `git` or `jj`, optional: they name an item's project and branch. Without them it's the directory name.
- `sqlite3`, optional, for troubleshooting.

## Cost

Each agent turn whose final message may leave something for you (a cheap local filter decides) costs one Haiku call, billed to your Claude plan like any other `claude -p` run. Turns that plainly leave nothing cost nothing.

Every Pi extraction is a billed `pi -p` call, made the same way for each Pi turn the filter lets through, and charged to the provider of the model it runs: per token, and with Anthropic subscription auth as extra usage, not your plan's limits, as Pi warns at startup. Choose that model with `/rexq-model` in Pi (see [Pi](#pi)).

## Install

```sh
go install github.com/tfcace/rexq/cmd/rexq@latest
command -v rexq    # the path the Rex module needs below
```

The hook installer and the Rex module live in this repository:

```sh
git clone https://github.com/tfcace/rexq && cd rexq
python3 scripts/install-hooks.py      # adds the hooks to ~/.claude/settings.json
```

To build from that checkout instead of `@latest`, run `go install ./cmd/rexq` in it.

The installer backs up `settings.json` first (or creates it), leaves other hooks alone, and refuses a missing or broken binary. It writes `$CLAUDE_CONFIG_DIR/settings.json` when `CLAUDE_CONFIG_DIR` is set, and uses the `rexq` on `PATH`, else the one `go install` made. Both can be given: `install-hooks.py [settings.json [rexq binary]]`.

The hooks run `claude -p`, so `claude` must be on `PATH` in the environment Claude Code runs hooks in. If it isn't, set `REXQ_CLAUDE` to its full path there.

### The Rex panel

Copy the module next to the `init.lua` Rex reads: `~/.config/rex/`, or `$XDG_CONFIG_HOME/rex/` when that is set, or beside the file `REX_CONFIG` names.

```sh
mkdir -p ~/.config/rex
cp rex/rexq.lua ~/.config/rex/
```

Add this to `init.lua`, with the path `command -v rexq` printed, and reload the config (`rex config reload`):

```lua
require("rexq").setup{ key = "shift+cmd+i", bin = "/path/to/rexq" }
```

`shift+cmd+i` shows the panel over the current tab and hides it again. The action is also in the command palette as "Toggle Agent Questions".

### Pi

> [!WARNING]
> Pi support is experimental. It is tested with Pi 1.5.1 and an Anthropic login only, replies depend on reading Pi's input box off the screen, and the extension, the `pi-model` setting and the store format may still change.

rexq's Pi extension runs the same three hooks for a Pi session in a Rex pane: when a session starts there, when a prompt you typed reaches the model (not for `/` commands, `!` commands, or input another extension handles), and when Pi settles after a run. Install it with the same script, from the checkout:

```sh
python3 scripts/install-hooks.py --pi
```

It writes `~/.pi/agent/extensions/rexq.ts` (under `PI_CODING_AGENT_DIR` when that is set), with the `rexq` binary's path written into it, found as for Claude. Both can be given: `install-hooks.py --pi [extension file [rexq binary]]`. Run `/reload` in Pi sessions that are already open. It won't overwrite a `rexq.ts` that isn't rexq's.

Extraction runs `pi -p` with no session, extensions, skills, context files or tools, on your Pi login. **Each extraction is a billed call** (see [Cost](#cost)), so the first time rexq runs in Pi it asks which model to use before any is made: the model picker opens by itself once Pi has started. It asks once per rexq store, not per session, even when several Pi sessions start together. Pressing Esc keeps Automatic and says which model that is. Until you answer, Pi turns wait and nothing is extracted; a picker left open (Pi quit or crashed) stops holding them after 10 minutes and isn't shown again. It doesn't open when a model was already set with `rexq config set pi-model` or the installer's `--model`, outside interactive Pi, or when `rexq` is missing.

To change the model later, run `/rexq-model` in Pi: it lists "Automatic" with the model that means for the session you're in, then every model your Pi logins can use, the current choice marked, and confirms the choice with the provider that bills it. The choice is rexq's, kept in its store, so every extraction uses it from then on, retries and backfill included. Outside Pi:

```sh
rexq config get pi-model --json                        # the choice, what Automatic is, and whether you were asked
rexq config set pi-model anthropic/claude-haiku-5-5    # or auto
python3 scripts/install-hooks.py --pi --model anthropic/claude-haiku-5-5   # the same, while installing
```

The model is the first of:

1. `REXQ_PI_MODEL`, as `provider/model`, in the environment Pi runs in: an override for scripts;
2. the choice made with `/rexq-model` or `rexq config set pi-model`;
3. automatic: a cheap model of the session's provider (Anthropic, OpenAI, Google, OpenRouter, GitHub Copilot), such as `anthropic/claude-haiku-5-5`, the model `claude -p` calls "haiku";
4. the session's own model.

If `pi -p` rejects the chosen model, say one a provider has retired, rexq tries once more with the session's model. A failed Pi turn is retried by later Pi stops only, with the `REXQ_PI_MODEL` and `REXQ_PI` it ended with; Claude stops retry only Claude turns. A failure that persists shows in `hook.log` and in the panel's header, as a failing `claude -p` does. Like the turn itself, `pi` must be on the `PATH` Pi runs with, or `REXQ_PI` names it.

The extension never holds Pi up: hooks run in the background, and a missing or failing `rexq` is ignored. It does nothing in `pi -p`, JSON or RPC runs.

A resumed Pi tab is labelled `pi` and the last 8 characters of the session id (the panel shows the whole id): Pi's ids start with a timestamp that sessions started in the same minute share.

From the panel, a Pi question is answered as a Claude one is: typed into its pane when Pi is idle, done or showed an error, and its input box is empty, with no autocomplete list or selector open over it; or, without a live pane, by `pi --session <id>` in a new tab, in the session's directory, and never while another Pi runs in that directory.

### Earlier sessions

New Claude sessions are tracked from then on. To pick up the last turn of sessions from the past week, once:

```sh
rexq backfill --dry-run   # lists the sessions it would extract, calls nothing
rexq backfill             # one claude -p call per session
```

`rexq backfill --pi` does the same for Pi's sessions (`~/.pi/agent/sessions/`, or `PI_CODING_AGENT_SESSION_DIR`), one `pi -p` call each. Pi's session files don't say whether a session was interactive or a scripted `pi -p` run, so it extracts scripted runs too; check `--dry-run` first.

## How it works

- **Hooks.** `SessionStart` ties the Claude session to the Rex pane it runs in (for Pi, the extension's start hook does). `UserPromptSubmit` notes what you said, so questions you've answered since are set aside. `Stop` runs async, so Claude never waits: it filters the final message, asks Haiku for the items in it, and stores them. Headless runs (`claude -p`, the SDK) are skipped. A failed extraction is queued and retried on later stops.
- **Store.** One SQLite file, shared by the hooks and the panel. `rexq list -all` prints what's open.
- **Panel.** A Bubble Tea program in a floating Rex block, one per Rex session. It shows that session's questions, or all of them.
- **Replies.** rexq types into a live pane only when the item's agent runs there, is idle (not working, not showing a dialog), and its input box is empty, and it checks the box holds exactly the reply before pressing Enter. Replies are one line; ones starting with `/` or `!` are refused, since Claude would run them as commands. If you've written to that agent since it asked, rexq refuses: jump there instead, or mark it done. Without a live pane it runs `claude --resume` in a new tab, in the directory the conversation started in, and never while another Claude runs in that directory.

## Keys

| Key | Does |
| --- | --- |
| `j` `k` | move |
| `1` `2` `3` | send that answer |
| `r` | write a reply, `enter` sends it |
| `z` | snooze: 1 hour, this evening, tomorrow 9:00, Monday 9:00, or until the agent next stops |
| `u` | wake a snoozed question; right after a change, undo it (also `U`) |
| `d` | done: you handled it |
| `x` | not a question: dismiss it |
| `space` | show the context: the agent's words, your prompt, screenshots |
| `enter` | jump to the agent's pane, or resume the conversation in a new tab |
| `a` | this Rex session or all of them |
| `q` `esc` | hide the panel (quit when it runs outside the Rex layer) |
| `ctrl+c` | quit |

## Where things live

- `~/.local/state/rex-questions/questions.db`: the store (SQLite, WAL). Set `REXQ_DB` to use another file; the log and locks move with it.
- `locks/` beside the store: 64 shard files the `Stop` hooks lock, so one session's turns are extracted one at a time.
- `hook.log` beside the store: hook errors and notes. At 1 MiB it moves to `hook.log.1` and starts over.

The directory is created `0700` and the files in it `0600`.

## Troubleshooting

- Nothing shows up: look at `hook.log`. A logged-out `claude`, a `claude` missing from the hooks' `PATH`, or a usage limit shows there, and the panel's header warns ("extraction failing") while it lasts.
- Failed extractions wait in the `pending` table and are retried on later stops for about 2h42m: `sqlite3 ~/.local/state/rex-questions/questions.db 'select id, claude_session, attempts, datetime(next_at, "unixepoch", "localtime") from pending'`.
- `rexq list -all` prints every open question, outside the panel.
- A session started before the hooks were installed takes its pane at its next stop, if no other conversation has claimed that pane. Until then the panel can only resume it, and refuses while it runs.

## Upgrading

This version adds Pi and changes the store's format (version 2). The first `rexq` of the new version to open the store migrates it, which keeps every question. An older `rexq` then refuses the store with "store was written by a newer rexq": old hooks log that to `hook.log` and do nothing, and an old `rexq list` or panel won't start. A panel from the old binary that is still open when the store moves on stops loading, with "couldn't read the store: … no such table: prompts" in its footer; it can't mistake a Pi question for a Claude one. So, to upgrade:

1. Hide the panel and quit it: open it and press `ctrl+c`.
2. Install the new binary (`go install`) and run `python3 scripts/install-hooks.py` again (and `--pi` for Pi).
3. Reload the Rex config (`rex config reload`); the panel's key starts the new one.

## Uninstall

```sh
python3 scripts/install-hooks.py --uninstall   # removes only rexq's hooks, with a backup
```

It leaves an empty `"hooks": {}` behind if rexq's were the only hooks. For Pi, `python3 scripts/install-hooks.py --pi --uninstall` deletes the extension (only if it is rexq's); `/reload` running Pi sessions. The installer's backups sit beside the settings file as `settings.json.bak-*`; delete them when you no longer need them.

Then remove the `require("rexq")` line from `init.lua`, delete `rexq.lua` beside it and the `rexq` binary (`command -v rexq`), and if you want the data gone too, `~/.local/state/rex-questions/`.

## License

MIT
