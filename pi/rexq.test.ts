// Tests for the Pi extension, run with node --test (go test runs them too,
// see ext_test.go). They load the extension with a stand-in for Pi's API.

import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, chmodSync } from "node:fs";
import { execFile } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { makeExtension, lastTurn } from "./rexq.ts";

type Handler = (event: any, ctx: any) => unknown;

function load(bin: string, timeoutMs?: number) {
	const handlers = new Map<string, Handler>();
	const pi = { on: (name: string, h: Handler) => handlers.set(name, h), registerCommand: () => {} };
	makeExtension({ bin, timeoutMs })(pi as any);
	return handlers;
}

const user = (id: string, text: string) => ({ type: "message", id, message: { role: "user", content: [{ type: "text", text }] } });
const assistant = (id: string, text: string, stopReason = "stop") => ({
	type: "message", id,
	message: { role: "assistant", content: [{ type: "thinking", thinking: "" }, { type: "text", text }], stopReason, provider: "anthropic", model: "claude-opus-5" },
});

function ctx(branch: any[], mode = "tui") {
	return {
		mode,
		model: { provider: "anthropic", id: "claude-opus-5" },
		sessionManager: {
			getSessionId: () => "s-1",
			getCwd: () => "/code/app",
			getSessionFile: () => "/sessions/s-1.jsonl",
			getBranch: () => branch,
		},
	};
}

/** A stand-in rexq that appends "<kind> <stdin>" lines to a log. */
function recordingRexq(): { bin: string; log: string } {
	const dir = mkdtempSync(join(tmpdir(), "rexq-ext-"));
	const bin = join(dir, "rexq");
	const log = join(dir, "log");
	writeFileSync(bin, `#!/bin/sh\nin=$(cat)\nprintf '%s %s\\n' "$2" "$in" >> "${log}"\n`);
	chmodSync(bin, 0o755);
	return { bin, log };
}

async function waitFor(path: string, lines: number): Promise<string[]> {
	for (let i = 0; i < 200; i++) {
		if (existsSync(path)) {
			const got = readFileSync(path, "utf8").trim().split("\n");
			if (got.length >= lines) return got;
		}
		await new Promise((r) => setTimeout(r, 10));
	}
	throw new Error(`no ${lines} lines in ${path}`);
}

const turn = [user("u1", "first"), assistant("a1", "Q1?"), user("u2", "which file?"), assistant("a2", "Which file should I open?")];

const userMsg = (text: string) => ({ message: { role: "user", content: [{ type: "text", text }] } });

test("sends start, prompt and stop as rexq's hooks expect", async () => {
	const { bin, log } = recordingRexq();
	const h = load(bin);
	h.get("session_start")!({ reason: "startup" }, ctx([]));
	await waitFor(log, 1);
	h.get("input")!({ text: "which file?", source: "interactive" }, ctx([]));
	h.get("message_end")!(userMsg("which file?"), ctx([]));
	await waitFor(log, 2);
	h.get("agent_settled")!({ aborted: false }, ctx(turn));
	const lines = await waitFor(log, 3);
	assert.deepEqual(lines.map((l) => [l.split(" ")[0], JSON.parse(l.slice(l.indexOf(" ") + 1))]), [
		["start", { agent: "pi", session_id: "s-1" }],
		["prompt", { agent: "pi", session_id: "s-1", prompt: "which file?" }],
		["stop", {
			agent: "pi", session_id: "s-1", prompt_id: "u2", cwd: "/code/app", transcript_path: "/sessions/s-1.jsonl",
			last_assistant_message: "Which file should I open?", prompt: "which file?",
			provider: "anthropic", model: "claude-opus-5",
		}],
	]);
});

test("a missing rexq is ignored, and nothing waits for it", () => {
	const h = load("/nonexistent/rexq");
	const t0 = Date.now();
	for (const [name, ev] of [["session_start", {}], ["input", { text: "x", source: "interactive" }], ["message_end", userMsg("x")], ["agent_settled", { aborted: false }]] as const) {
		assert.equal(h.get(name)!(ev, ctx(turn)), undefined);
	}
	assert.ok(Date.now() - t0 < 200, "handlers took too long");
});

test("a slow rexq doesn't hold Pi up, and start is killed at its timeout", async () => {
	const dir = mkdtempSync(join(tmpdir(), "rexq-ext-"));
	const bin = join(dir, "rexq");
	const mark = join(dir, "done");
	writeFileSync(bin, `#!/bin/sh\nsleep 3\ntouch "${mark}"\n`);
	chmodSync(bin, 0o755);
	const h = load(bin, 200);
	const t0 = Date.now();
	h.get("session_start")!({}, ctx([]));
	assert.ok(Date.now() - t0 < 200);
	await new Promise((r) => setTimeout(r, 3500));
	assert.equal(existsSync(mark), false, "start ran past its timeout");
});

test("a failing rexq is ignored", async () => {
	const dir = mkdtempSync(join(tmpdir(), "rexq-ext-"));
	const bin = join(dir, "rexq");
	writeFileSync(bin, "#!/bin/sh\necho boom >&2\nexit 3\n");
	chmodSync(bin, 0o755);
	const notExec = join(dir, "not-executable");
	writeFileSync(notExec, "x");
	for (const b of [bin, notExec, dir, ""]) {
		const h = load(b);
		h.get("session_start")!({}, ctx([]));
		h.get("agent_settled")!({ aborted: false }, ctx(turn));
	}
	await new Promise((r) => setTimeout(r, 300));
});

test("errors in Pi's own objects are swallowed", () => {
	const h = load("/nonexistent/rexq");
	const broken = { mode: "tui", get sessionManager(): any { throw new Error("gone"); } };
	for (const name of ["session_start", "input", "message_end", "agent_settled"]) {
		assert.doesNotThrow(() => h.get(name)!({ text: "x", source: "interactive", ...userMsg("x") }, broken));
	}
	assert.doesNotThrow(() => h.get("agent_settled")!(undefined, undefined));
});

test("headless modes, the extractor, aborted runs and unfinished turns send nothing", async () => {
	const { bin, log } = recordingRexq();
	for (const mode of ["print", "json", "rpc"]) {
		const h = load(bin);
		h.get("session_start")!({}, ctx(turn, mode));
		h.get("input")!({ text: "x", source: "interactive" }, ctx(turn, mode));
		h.get("message_end")!(userMsg("x"), ctx(turn, mode));
		h.get("agent_settled")!({ aborted: false }, ctx(turn, mode));
	}
	const h = load(bin);
	h.get("input")!({ text: "from an extension", source: "extension" }, ctx(turn));
	h.get("message_end")!(userMsg("from an extension"), ctx(turn));
	h.get("agent_settled")!({ aborted: true }, ctx(turn));
	h.get("agent_settled")!({ aborted: false }, ctx([...turn, user("u3", "again")]));
	h.get("agent_settled")!({ aborted: false }, ctx([user("u1", "x"), assistant("a1", "Let me look.", "toolUse")]));
	process.env.REXQ_EXTRACTOR = "1";
	try {
		h.get("session_start")!({}, ctx(turn));
	} finally {
		delete process.env.REXQ_EXTRACTOR;
	}
	await new Promise((r) => setTimeout(r, 300));
	assert.equal(existsSync(log), false, existsSync(log) ? readFileSync(log, "utf8") : "");
});

test("lastTurn reads the branch the way rexq's Go reader does", () => {
	assert.deepEqual(lastTurn(turn), { promptId: "u2", prompt: "which file?", final: "Which file should I open?", provider: "anthropic", model: "claude-opus-5" });
	const multi = { type: "message", id: "a3", message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: "one" }, { type: "toolCall" }, { type: "text", text: "two" }] } };
	assert.equal(lastTurn([user("u", "p"), multi])?.final, "one\n\ntwo");
	assert.equal(lastTurn([user("u", "p"), assistant("a", "x", "error")]), undefined);
	assert.equal(lastTurn([user("u", "p"), assistant("a", "x", "aborted")]), undefined);
	assert.equal(lastTurn([user("u", "p"), assistant("a", "", "stop")]), undefined);
	assert.equal(lastTurn([assistant("a", "x")]), undefined);
	const custom = { type: "custom_message", id: "c" };
	assert.equal(lastTurn([user("u", "p"), assistant("a", "Q?"), custom])?.final, "Q?");
});

// Pi 1.5.1 fires input for text another extension then handles, which never
// reaches the model (checked by hand: an input handler returning "handled",
// idle and as a steer). Only a user message that reaches the model is a
// prompt, so the panel doesn't take handled input for writing to the agent.
test("only input that reaches the model is recorded as a prompt", async () => {
	const { bin, log } = recordingRexq();
	const h = load(bin);
	const c = ctx(turn);
	// Handled by another extension: input, but no user message.
	h.get("input")!({ text: "ping", source: "interactive" }, c);
	// A run: typed, then its message.
	h.get("input")!({ text: "which file?", source: "interactive" }, c);
	h.get("message_end")!(userMsg("which file?"), c);
	// A steer that another extension handles, and one that arrives.
	h.get("input")!({ text: "ping", source: "interactive", streamingBehavior: "steer" }, c);
	h.get("input")!({ text: "also deltas", source: "interactive", streamingBehavior: "steer" }, c);
	h.get("message_end")!(userMsg("also deltas"), c);
	// Assistant and custom messages are not prompts.
	h.get("message_end")!({ message: { role: "assistant", content: [{ type: "text", text: "x" }] } }, c);
	h.get("message_end")!({ message: { role: "custom", content: "x" } }, c);
	h.get("agent_settled")!({ aborted: false }, c);
	// A skill command, recorded as typed, not as its expansion.
	h.get("input")!({ text: "/skill:x do it", source: "interactive" }, c);
	h.get("message_end")!(userMsg("<skill name=\"x\">...</skill> do it"), c);
	// After a settle, a handled input left over doesn't pose as the next.
	h.get("agent_settled")!({ aborted: false }, c);
	h.get("input")!({ text: "ping", source: "interactive" }, c);
	h.get("agent_settled")!({ aborted: true }, c);
	h.get("message_end")!(userMsg("Reply"), c); // e.g. pi --session id -- Reply
	const lines = await waitFor(log, 6);
	await new Promise((r) => setTimeout(r, 200));
	const all = readFileSync(log, "utf8").trim().split("\n");
	const prompts = all.filter((l) => l.startsWith("prompt ")).map((l) => JSON.parse(l.slice(7)).prompt);
	// Each hook is its own process, so the log's order is theirs: compare sorted.
	assert.deepEqual(prompts.sort(), ["/skill:x do it", "Reply", "also deltas", "which file?"]);
	const stops = all.filter((l) => l.startsWith("stop ")).map((l) => JSON.parse(l.slice(5)).prompt);
	assert.deepEqual(stops.sort(), ["/skill:x do it", "also deltas"]);
	assert.ok(lines.length >= 6);
});

// /rexq-model, with a stand-in rexq that keeps the setting in a file, and
// Pi's exec, selector and notifications stood in for.
function picker(opts: { bin?: string; choose?: (labels: string[]) => string | undefined; override?: string } = {}) {
	const dir = mkdtempSync(join(tmpdir(), "rexq-pick-"));
	const state = join(dir, "pi-model");
	const calls = join(dir, "calls");
	const bin = opts.bin ?? join(dir, "rexq");
	if (!opts.bin) {
		writeFileSync(bin, `#!/bin/sh
echo "$*" >> "${calls}"
[ "$1" = config ] || exit 2
case "$2" in
get) v=$(cat "${state}" 2>/dev/null); printf '{"pi-model":"%s","automatic":"anthropic/claude-haiku-5-5","override":"${opts.override ?? ""}","effective":"x"}\\n' "$v" ;;
set) if [ "$4" = auto ]; then rm -f "${state}"; else printf '%s' "$4" > "${state}"; fi ;;
esac
`);
		chmodSync(bin, 0o755);
	}
	const commands = new Map<string, any>();
	const pi = {
		on: () => {},
		registerCommand: (name: string, o: any) => commands.set(name, o),
		exec: (cmd: string, args: string[]) =>
			new Promise((resolve) => {
				execFile(cmd, args, (err: any, stdout: string, stderr: string) =>
					resolve({ stdout, stderr: stderr || (err && !err.code ? "" : err?.code === "ENOENT" ? "spawn ENOENT" : ""), code: err ? (typeof err.code === "number" ? err.code : 1) : 0 }));
			}),
	};
	makeExtension({ bin })(pi as any);
	const notes: [string, string][] = [];
	let shown: string[] | undefined;
	const ctx = {
		hasUI: true,
		model: { provider: "anthropic", id: "claude-opus-5" },
		modelRegistry: {
			getAvailable: () => [{ provider: "anthropic", id: "claude-opus-5" }, { provider: "anthropic", id: "claude-haiku-5-5" }, { provider: "openai", id: "gpt-5-mini" }],
			getProviderDisplayName: (p: string) => ({ anthropic: "Anthropic", openai: "OpenAI" } as any)[p] ?? p,
		},
		ui: {
			select: async (_title: string, labels: string[]) => {
				shown = labels;
				return opts.choose ? opts.choose(labels) : undefined;
			},
			notify: (m: string, kind: string) => notes.push([kind, m]),
		},
	};
	const run = () => commands.get("rexq-model").handler("", ctx);
	const setting = () => (existsSync(state) ? readFileSync(state, "utf8") : "");
	return { dir, run, notes, shown: () => shown, setting, calls: () => (existsSync(calls) ? readFileSync(calls, "utf8") : "") };
}

test("/rexq-model lists Automatic with what it resolves to, then the usable models, the current one marked", async () => {
	const p = picker();
	await p.run();
	assert.deepEqual(p.shown(), [
		"Automatic: anthropic/claude-haiku-5-5 for this session  ✓",
		"anthropic/claude-opus-5",
		"anthropic/claude-haiku-5-5",
		"openai/gpt-5-mini",
	]);
	assert.equal(p.setting(), "", "cancelling set nothing");
	assert.equal(p.notes.length, 0);
	assert.match(p.calls(), /^config get pi-model --json --provider anthropic --session-model claude-opus-5\n$/);
});

test("/rexq-model saves a choice through rexq and says who bills it", async () => {
	const p = picker({ choose: (l) => l.find((x) => x.startsWith("openai/")) });
	await p.run();
	assert.equal(p.setting(), "openai/gpt-5-mini");
	assert.deepEqual(p.notes, [["info", "rexq extracts questions with openai/gpt-5-mini. Each extraction is a pi -p call billed to OpenAI."]]);
	// Shown marked next time; choosing Automatic clears it.
	const q = picker({ choose: (l) => l[0] });
	writeFileSync(join(q.dir, "pi-model"), "openai/gpt-5-mini");
	await q.run();
	assert.ok(q.shown()!.includes("openai/gpt-5-mini  ✓"));
	assert.ok(!q.shown()![0].endsWith("✓"));
	assert.equal(q.setting(), "");
	assert.deepEqual(q.notes, [["info", "rexq extracts questions with automatic, now anthropic/claude-haiku-5-5. Each extraction is a pi -p call billed to Anthropic."]]);
	assert.match(q.calls(), /config set pi-model auto/);
});

test("/rexq-model warns when REXQ_PI_MODEL overrides the choice", async () => {
	const p = picker({ choose: (l) => l[1], override: "google/gemini-2.5-flash" });
	await p.run();
	assert.equal(p.notes[1][0], "warning");
	assert.match(p.notes[1][1], /REXQ_PI_MODEL=google\/gemini-2.5-flash/);
});

test("/rexq-model without rexq says so and does nothing", async () => {
	for (const bin of ["/nonexistent/rexq", "rexq-not-on-path-anywhere"]) {
		const p = picker({ bin, choose: (l) => l[1] });
		await p.run();
		assert.equal(p.shown(), undefined, "showed a selector");
		assert.equal(p.notes.length, 1);
		assert.equal(p.notes[0][0], "error");
		assert.match(p.notes[0][1], /rexq isn't installed at/);
	}
});

// The first-run picker: a stand-in rexq that keeps the setting and its ask
// state in files, as rexq's store does, and Pi stood in for.
function firstRun(opts: { bin?: string; mode?: string; asked?: boolean; preset?: string; brokenAsk?: boolean; dir?: string } = {}) {
	const dir = opts.dir ?? mkdtempSync(join(tmpdir(), "rexq-first-"));
	const state = join(dir, "pi-model");
	const asked = join(dir, "asked");
	const calls = join(dir, "calls");
	// mkdir claims atomically, as the store's INSERT ... ON CONFLICT does.
	const claim = asked + ".claim";
	if (opts.asked) {
		writeFileSync(asked, "done");
		mkdirSync(claim, { recursive: true });
	}
	if (opts.preset) {
		writeFileSync(state, opts.preset);
		writeFileSync(asked, "done");
		mkdirSync(claim, { recursive: true });
	}
	const bin = opts.bin ?? join(dir, "rexq");
	if (!opts.bin) {
		writeFileSync(bin, `#!/bin/sh
echo "$*" >> "${calls}"
[ "$1" = config ] || { cat > /dev/null; exit 0; }
${opts.brokenAsk ? 'echo "store is broken" >&2; exit 1' : ""}
v=$(cat "${state}" 2>/dev/null)
case "$2" in
ask) if mkdir "${claim}" 2>/dev/null; then a=true; echo open > "${asked}"; else a=false; fi
     printf '{"pi-model":"%s","automatic":"anthropic/claude-haiku-5-5","override":"","effective":"x","asked":"open","ask":%s}\\n' "$v" "$a" ;;
get) printf '{"pi-model":"%s","automatic":"anthropic/claude-haiku-5-5","override":"","effective":"x","asked":"done"}\\n' "$v" ;;
set) if [ "$4" = auto ]; then rm -f "${state}"; else printf '%s' "$4" > "${state}"; fi; echo done > "${asked}" ;;
dismiss) echo done > "${asked}" ;;
esac
`);
		chmodSync(bin, 0o755);
	}
	const handlers = new Map<string, Handler>();
	let selected: () => void = () => {};
	const selectShown = new Promise<void>((r) => (selected = r));
	let release: (v: string | undefined) => void = () => {};
	const pi = {
		on: (name: string, h: Handler) => handlers.set(name, h),
		registerCommand: () => {},
		exec: (cmd: string, args: string[]) =>
			new Promise((resolve) => {
				execFile(cmd, args, (err: any, stdout: string, stderr: string) =>
					resolve({ stdout, stderr: stderr || (err?.code === "ENOENT" ? "spawn ENOENT" : ""), code: err ? (typeof err.code === "number" ? err.code : 1) : 0 }));
			}),
	};
	makeExtension({ bin })(pi as any);
	const notes: [string, string][] = [];
	const titles: string[] = [];
	let shown: string[] | undefined;
	const ctx = {
		mode: opts.mode ?? "tui",
		hasUI: (opts.mode ?? "tui") !== "print" && opts.mode !== "json",
		model: { provider: "anthropic", id: "claude-opus-5" },
		modelRegistry: {
			getAvailable: () => [{ provider: "anthropic", id: "claude-opus-5" }, { provider: "openai", id: "gpt-5-mini" }],
			getProviderDisplayName: (p: string) => ({ anthropic: "Anthropic", openai: "OpenAI" } as any)[p] ?? p,
		},
		sessionManager: {
			getSessionId: () => "s-1",
			getCwd: () => "/code/app",
			getSessionFile: () => "/sessions/s-1.jsonl",
			getBranch: () => turn,
		},
		ui: {
			select: (title: string, labels: string[]) => {
				titles.push(title);
				shown = labels;
				selected();
				return new Promise<string | undefined>((r) => (release = (v) => r(v)));
			},
			notify: (m: string, kind: string) => notes.push([kind, m]),
		},
	};
	const start = () => handlers.get("session_start")!({ reason: "startup" }, ctx);
	const settle = () => handlers.get("agent_settled")!({ aborted: false }, ctx);
	const answer = (pick: (labels: string[]) => string | undefined) => release(pick(shown!));
	const read = (p: string) => (existsSync(p) ? readFileSync(p, "utf8") : "");
	return { dir, start, settle, answer, selectShown, notes, titles, shown: () => shown, calls: () => read(calls), setting: () => read(state), asked: () => read(asked).trim() };
}

const settleTime = () => new Promise((r) => setTimeout(r, 400));
const shown = (f: { selectShown: Promise<void> }) =>
	Promise.race([f.selectShown, new Promise((_, no) => setTimeout(() => no(new Error("no picker within 2s")), 2000).unref())]);

test("first start opens the picker once, with why, and a choice is saved", async () => {
	const f = firstRun();
	f.start();
	await shown(f);
	assert.equal(f.titles.length, 1);
	assert.match(f.titles[0], /rexq collects the questions Pi leaves you/);
	assert.match(f.titles[0], /pi -p call billed to your provider/);
	assert.match(f.titles[0], /\/rexq-model/);
	assert.equal(f.asked(), "open", "marked asked as it was shown");
	assert.equal(f.shown()![0], "Automatic: anthropic/claude-haiku-5-5 for this session");
	f.answer((l) => l.find((x) => x.startsWith("openai/")));
	await settleTime();
	assert.equal(f.setting(), "openai/gpt-5-mini");
	assert.equal(f.asked(), "done");
	assert.equal(f.notes.length, 1);
	assert.match(f.notes[0][1], /openai\/gpt-5-mini.*billed to OpenAI/);
});

test("Esc keeps Automatic, records the answer, and says how to change it", async () => {
	const f = firstRun();
	f.start();
	await shown(f);
	f.answer(() => undefined);
	await settleTime();
	assert.equal(f.setting(), "");
	assert.equal(f.asked(), "done");
	assert.match(f.calls(), /config dismiss pi-model/);
	assert.deepEqual(f.notes, [["info", "rexq extracts questions with automatic, now anthropic/claude-haiku-5-5 for this session. Change it with /rexq-model."]]);
});

test("a run that settles while the picker is open waits for the choice", async () => {
	const f = firstRun();
	f.start();
	await shown(f);
	f.settle();
	await settleTime();
	assert.doesNotMatch(f.calls(), /hook stop/, "stop ran before the choice");
	f.answer((l) => l.find((x) => x.startsWith("openai/")));
	await settleTime();
	const calls = f.calls().trim().split("\n");
	const set = calls.findIndex((c) => c.startsWith("config set pi-model openai/gpt-5-mini"));
	const stop = calls.findIndex((c) => c === "hook stop");
	assert.ok(set >= 0 && stop > set, `stop after the choice: ${calls}`);
});

test("no picker when asked before, chosen from outside, or in another session", async () => {
	for (const f of [firstRun({ asked: true }), firstRun({ preset: "openai/gpt-5-mini" })]) {
		f.start();
		await settleTime();
		assert.equal(f.titles.length, 0);
		assert.equal(f.notes.length, 0);
	}
	// Two sessions on one store starting together: one asks.
	const a = firstRun();
	const b = firstRun({ dir: a.dir });
	a.start();
	b.start();
	await settleTime();
	assert.equal(a.titles.length + b.titles.length, 1);
});

test("no picker, and no message, in headless modes, for the extractor, or when rexq fails", async () => {
	const cases = [
		firstRun({ mode: "print" }),
		firstRun({ mode: "json" }),
		firstRun({ mode: "rpc" }),
		firstRun({ bin: "/nonexistent/rexq" }),
		firstRun({ bin: "rexq-not-on-path-anywhere" }),
		firstRun({ brokenAsk: true }),
	];
	for (const f of cases) f.start();
	process.env.REXQ_EXTRACTOR = "1";
	const x = firstRun();
	try {
		x.start();
	} finally {
		delete process.env.REXQ_EXTRACTOR;
	}
	await settleTime();
	for (const f of [...cases, x]) {
		assert.equal(f.titles.length, 0);
		assert.equal(f.notes.length, 0);
	}
	for (const f of cases.slice(0, 3)) assert.doesNotMatch(f.calls(), /config/);
	assert.equal(x.calls(), "");
});
