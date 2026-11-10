// rexq Pi extension: installed by scripts/install-hooks.py --pi.
//
// It runs rexq's hooks for a Pi session the way Claude Code runs them for
// a Claude one: "start" when a session starts in this pane, "prompt" when
// a prompt you typed reaches the model, and "stop" when Pi settles after a
// run, with the final message and the prompt that started it. rexq extracts
// the questions in the final message into its store, and its Rex panel
// lists them. Each extraction is a pi -p call, billed to the provider of
// the model it uses; /rexq-model chooses that model.
//
// It must never break or slow Pi. Every hook runs in a child process that
// Pi doesn't wait for; any failure (no rexq binary, a crash, a slow rexq)
// is dropped silently. rexq logs its own errors to hook.log beside its
// store. It does nothing outside Pi's interactive mode, so pi -p, JSON and
// RPC runs leave no questions.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";

// The installer writes this line.
const REXQ_BIN = "rexq";

/** How long a start or prompt hook may run before it is killed. */
const SYNC_HOOK_TIMEOUT_MS = 5000;

export interface Options {
	/** The rexq binary. */
	bin: string;
	/** How long start and prompt hooks may run. */
	timeoutMs?: number;
}

/** Runs `rexq hook <kind>` with input on stdin, without waiting for it. */
function runHook(opts: Options, kind: "start" | "prompt" | "stop", input: Record<string, unknown>): void {
	try {
		// stop may run for minutes (a model call): detached, it outlives
		// Pi and the pane, so a closing pane doesn't cut its extraction.
		const child = spawn(opts.bin, ["hook", kind], {
			stdio: ["pipe", "ignore", "ignore"],
			detached: kind === "stop",
			env: process.env,
		});
		child.on("error", () => {});
		child.stdin?.on("error", () => {});
		if (kind === "stop") {
			child.unref();
		} else {
			const timer = setTimeout(() => {
				try {
					child.kill("SIGKILL");
				} catch {}
			}, opts.timeoutMs ?? SYNC_HOOK_TIMEOUT_MS);
			timer.unref();
			child.on("exit", () => clearTimeout(timer));
			child.unref();
		}
		child.stdin?.end(JSON.stringify({ agent: "pi", ...input }));
	} catch {
		// rexq is missing or can't start: Pi goes on without it.
	}
}

interface Block {
	type?: string;
	text?: string;
}

/** The text blocks of a message's content, joined as rexq reads them. */
function textOf(content: unknown): string {
	if (typeof content === "string") return content;
	if (!Array.isArray(content)) return "";
	return (content as Block[])
		.filter((b) => b?.type === "text" && typeof b.text === "string" && b.text !== "")
		.map((b) => b.text as string)
		.join("\n\n");
}

/**
 * The finished turn at the end of the session's branch: the final
 * assistant message, which must have ended normally with some text, and
 * the user message that started its turn. undefined when the run ended in
 * a tool call, an error or an interruption.
 */
export function lastTurn(branch: any[]): { promptId: string; prompt: string; final: string; provider?: string; model?: string } | undefined {
	let final: any;
	for (let i = branch.length - 1; i >= 0; i--) {
		const e = branch[i];
		if (e?.type !== "message") continue;
		const role = e.message?.role;
		if (!final) {
			if (role === "user") return undefined; // the user had the last word
			if (role !== "assistant") continue;
			const stop = e.message.stopReason;
			if (stop !== "stop" && stop !== "length") return undefined;
			if (textOf(e.message.content) === "") return undefined;
			final = e;
			continue;
		}
		if (role === "user") {
			return {
				promptId: e.id,
				prompt: textOf(e.message.content),
				final: textOf(final.message.content),
				provider: final.message.provider,
				model: final.message.model,
			};
		}
	}
	return undefined;
}

/** What came in through Pi's input event, waiting to reach the model. */
interface Input {
	text: string;
	fromExtension: boolean;
}

/** How many inputs are kept waiting; ones another extension handled never arrive. */
const MAX_WAITING = 20;

/** How long a rexq config call may take. */
const CONFIG_TIMEOUT_MS = 5000;

/** What `rexq config get pi-model --json` prints. */
interface ModelConfig {
	"pi-model": string;
	automatic: string;
	override: string;
	/** never, open or done: whether the user was asked for a model. */
	asked: string;
	/** From `config ask`: this caller should show the first-run picker. */
	ask?: boolean;
}

const firstLine = (s: string) => (s ?? "").trim().split("\n")[0] ?? "";

/** Runs `rexq config ...` and returns its JSON, or why it couldn't. */
async function readConfig(pi: ExtensionAPI, opts: Options, ctx: any, verb: "get" | "ask"): Promise<ModelConfig | string> {
	const missing = `rexq isn't installed at ${opts.bin}: install it and run scripts/install-hooks.py --pi again`;
	if (opts.bin.startsWith("/") && !existsSync(opts.bin)) return missing;
	const args = ["config", verb, "pi-model", "--provider", ctx.model?.provider ?? "", "--session-model", ctx.model?.id ?? ""];
	if (verb === "get") args.splice(3, 0, "--json");
	const got = await pi.exec(opts.bin, args, { timeout: CONFIG_TIMEOUT_MS });
	if (got.code !== 0) {
		return /ENOENT/.test(got.stderr) ? missing : `rexq couldn't read the model setting: ${firstLine(got.stderr || got.stdout)}`;
	}
	try {
		return JSON.parse(got.stdout) as ModelConfig;
	} catch {
		return `rexq printed something unexpected: ${firstLine(got.stdout)}`;
	}
}

/**
 * Shows Pi's selector of extraction models and saves the choice through
 * rexq: "chosen" (Automatic included), "dismissed" (Esc), or "failed".
 */
async function chooseModel(pi: ExtensionAPI, opts: Options, ctx: any, cfg: ModelConfig, title: string): Promise<"chosen" | "dismissed" | "failed"> {
	const current = cfg["pi-model"] ?? "";
	// Marked only once someone chose: the first-run picker marks nothing.
	const chose = cfg.asked !== "never" && cfg.asked !== "open";
	const mark = (on: boolean) => (on && chose ? "  ✓" : "");
	const values: string[] = [""];
	const labels: string[] = [`Automatic: ${cfg.automatic || "the session's model"} for this session${mark(current === "")}`];
	const seen = new Set<string>();
	const add = (id: string) => {
		if (seen.has(id)) return;
		seen.add(id);
		values.push(id);
		labels.push(id + mark(id === current));
	};
	if (current !== "") add(current); // even when no login offers it now
	for (const m of ctx.modelRegistry.getAvailable() as { provider: string; id: string }[]) add(`${m.provider}/${m.id}`);
	const choice = await ctx.ui.select(title, labels);
	if (choice === undefined) return "dismissed";
	const value = values[labels.indexOf(choice)];
	if (value === undefined) return "dismissed";
	const set = await pi.exec(opts.bin, ["config", "set", "pi-model", value === "" ? "auto" : value], { timeout: CONFIG_TIMEOUT_MS });
	if (set.code !== 0) {
		ctx.ui.notify(`rexq couldn't save the model: ${firstLine(set.stderr || set.stdout)}`, "error");
		return "failed";
	}
	const model = value === "" ? cfg.automatic : value;
	const modelProvider = model.split("/")[0] ?? "";
	let billedTo = modelProvider;
	try {
		billedTo = ctx.modelRegistry.getProviderDisplayName(modelProvider) || modelProvider;
	} catch {}
	const how = value === "" ? `automatic, now ${model || "the session's model"}` : model;
	ctx.ui.notify(`rexq extracts questions with ${how}. Each extraction is a pi -p call billed to ${billedTo || "the session's provider"}.`, "info");
	if (cfg.override) {
		ctx.ui.notify(`REXQ_PI_MODEL=${cfg.override} is set where Pi runs, and overrides this choice.`, "warning");
	}
	return "chosen";
}

/** /rexq-model: choose the model rexq's extraction runs with, any time. */
async function pickModel(pi: ExtensionAPI, opts: Options, ctx: any): Promise<void> {
	if (!ctx.hasUI) return;
	const cfg = await readConfig(pi, opts, ctx, "get");
	if (typeof cfg === "string") {
		ctx.ui.notify(cfg, "error");
		return;
	}
	await chooseModel(pi, opts, ctx, cfg, "Model for rexq's question extraction (each run is a billed pi -p call)");
}

const FIRST_RUN_TITLE =
	"rexq collects the questions Pi leaves you. Each one costs a pi -p call billed to your provider. " +
	"Pick the model to extract with (change it later with /rexq-model)";

/**
 * The first time rexq runs in Pi (per rexq store), the model picker opens
 * by itself, so nobody pays for extractions without having seen the
 * choice. `rexq config ask` marks it asked as it is shown, so two sessions
 * starting together ask once, and a crash mid-picker doesn't ask again.
 * Esc keeps Automatic and records the answer. When rexq is missing or
 * fails, nothing at all happens.
 */
async function firstRun(pi: ExtensionAPI, opts: Options, ctx: any): Promise<void> {
	const cfg = await readConfig(pi, opts, ctx, "ask");
	if (typeof cfg === "string" || !cfg.ask) return;
	let how: "chosen" | "dismissed" | "failed" = "failed";
	try {
		how = await chooseModel(pi, opts, ctx, cfg, FIRST_RUN_TITLE);
	} finally {
		if (how !== "chosen") {
			await pi.exec(opts.bin, ["config", "dismiss", "pi-model"], { timeout: CONFIG_TIMEOUT_MS });
		}
	}
	if (how === "dismissed") {
		ctx.ui.notify(`rexq extracts questions with automatic, now ${cfg.automatic || "the session's model"} for this session. Change it with /rexq-model.`, "info");
	}
}

export function makeExtension(opts: Options) {
	return function (pi: ExtensionAPI) {
		pi.registerCommand("rexq-model", {
			description: "Choose the model rexq extracts questions with (billed pi -p calls)",
			handler: async (_args, ctx) => {
				try {
					await pickModel(pi, opts, ctx);
				} catch (e) {
					try {
						ctx.ui.notify(`rexq-model failed: ${firstLine(String((e as Error)?.message ?? e))}`, "error");
					} catch {}
				}
			},
		});

		// Inputs, oldest first, until their user message reaches the model.
		// Pi's input event also fires for input another extension then
		// handles, which never reaches the model and is no prompt (Pi
		// 1.5.1; built-in and extension /commands and ! commands don't fire
		// it). A user message does: message_end with role "user", for a
		// steering or follow-up message too.
		let waiting: Input[] = [];
		// What you typed for the last user message of the run, before
		// skills and templates expanded it; the session stores the
		// expansion.
		let typed: string | undefined;
		// While the first-run picker is open, stops wait here, so no turn is
		// extracted with a model the user hasn't seen.
		let held: Record<string, unknown>[] | undefined;
		const stop = (input: Record<string, unknown>) => (held ? held.push(input) : runHook(opts, "stop", input));

		const active = (ctx: any): boolean => {
			try {
				return ctx?.mode === "tui" && !process.env.REXQ_EXTRACTOR;
			} catch {
				return false;
			}
		};

		pi.on("session_start", (_event, ctx) => {
			try {
				if (!active(ctx)) return;
				typed = undefined;
				waiting = [];
				runHook(opts, "start", { session_id: ctx.sessionManager.getSessionId() });
				if (ctx.hasUI && !held) {
					held = [];
					// Not awaited: Pi finishes starting, then the picker shows.
					firstRun(pi, opts, ctx)
						.catch(() => {})
						.finally(() => {
							const go = held ?? [];
							held = undefined;
							for (const input of go) runHook(opts, "stop", input);
						});
				}
			} catch {}
		});

		pi.on("input", (event, ctx) => {
			try {
				if (!active(ctx)) return;
				waiting.push({ text: event.text, fromExtension: event.source === "extension" });
				if (waiting.length > MAX_WAITING) waiting.shift();
			} catch {}
		});

		pi.on("message_end", (event, ctx) => {
			try {
				if (!active(ctx) || (event.message as any)?.role !== "user") return;
				const text = textOf((event.message as any).content);
				// Its input: the one with the same text, or a /skill or
				// /template command that expanded into it.
				let i = waiting.findIndex((w) => w.text === text);
				if (i < 0 && waiting.length > 0 && waiting[waiting.length - 1].text.startsWith("/")) {
					i = waiting.length - 1;
				}
				const input = i >= 0 ? waiting[i] : undefined;
				waiting = waiting.slice(i + 1);
				if (input?.fromExtension) return; // another extension's message
				typed = input?.text ?? text;
				runHook(opts, "prompt", { session_id: ctx.sessionManager.getSessionId(), prompt: typed });
			} catch {}
		});

		pi.on("agent_settled", (event, ctx) => {
			try {
				const prompt = typed;
				typed = undefined;
				waiting = []; // what never reached the model was handled
				if (!active(ctx) || event.aborted) return;
				const sm = ctx.sessionManager;
				const turn = lastTurn(sm.getBranch());
				if (!turn) return;
				stop({
					session_id: sm.getSessionId(),
					prompt_id: turn.promptId,
					cwd: sm.getCwd(),
					transcript_path: sm.getSessionFile() ?? "",
					last_assistant_message: turn.final,
					prompt: prompt ?? turn.prompt,
					provider: turn.provider ?? ctx.model?.provider ?? "",
					model: turn.model ?? ctx.model?.id ?? "",
				});
			} catch {}
		});
	};
}

export default makeExtension({ bin: REXQ_BIN });
