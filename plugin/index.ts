/**
 * tokenmunchers: reports per-call token usage from Oh My Pi to a shared leaderboard.
 *
 * Sent: provider, model, token counts, cost, timestamps, session id, subagent flag.
 * Never sent: prompts, responses, code, file paths, repo names, tool calls/output, cwd.
 */

import { randomUUID } from "node:crypto";
import { homedir } from "node:os";
import { join } from "node:path";

import type { ExtensionAPI, ExtensionContext } from "@oh-my-pi/pi-coding-agent";

import { presence, sessionEnd, toUsageEvent } from "./src/adapter.ts";
import { KEY_PATTERN, loadConfig, saveConfig } from "./src/config.ts";
import { Reporter } from "./src/transport.ts";

type Timer = ReturnType<ExtensionContext["setInterval"]>;

const HEARTBEAT_MS = 30_000;
const FLUSH_MS = 60_000;
const SHUTDOWN_SEND_MS = 1_500;

function resolveDataDir(pi: ExtensionAPI): string {
	try {
		const agentDir = (pi.pi as { getAgentDir?: () => string }).getAgentDir?.();
		if (agentDir) return join(agentDir, "tokenmunchers");
	} catch {
		// fall through
	}
	return join(homedir(), ".omp", "agent", "tokenmunchers");
}

export default function tokenmunchers(pi: ExtensionAPI) {
	const dataDir = resolveDataDir(pi);
	const fallbackSessionId = randomUUID();
	let config = loadConfig(dataDir);
	let paused = false;
	let warnedUnauthorized = false;
	let lastCtx: ExtensionContext | undefined;
	let sessionId: string | undefined;
	let startedAt = new Date();
	let heartbeatTimer: Timer | undefined;
	let flushTimer: Timer | undefined;

	const notify = (ctx: ExtensionContext | undefined, msg: string, level: "info" | "warning" | "error" = "info") => {
		if (ctx?.hasUI) ctx.ui.notify(`tokenmunchers: ${msg}`, level);
		else pi.logger.info(`tokenmunchers: ${msg}`);
	};

	const reporter = new Reporter({
		dataDir,
		getConfig: () => config,
		onUnauthorized: () => {
			if (warnedUnauthorized) return;
			warnedUnauthorized = true;
			notify(lastCtx, "API key was rejected. Usage is queued locally; run /usage-login with a new key.", "warning");
		},
	});

	const isSubagent = (ctx: ExtensionContext) => ctx.agent?.kind === "sub";

	const sessionIdOf = (ctx: ExtensionContext): string => {
		try {
			const id = ctx.sessionManager.getSessionId();
			if (typeof id === "string" && id) return id;
		} catch {
			// fall through
		}
		return fallbackSessionId;
	};

	const setStatus = (ctx: ExtensionContext | undefined) => {
		if (ctx?.hasUI) ctx.ui.setStatus("tokenmunchers", paused ? "usage paused" : undefined);
	};

	const stopHeartbeat = (ctx: ExtensionContext) => {
		if (heartbeatTimer) ctx.clearTimer(heartbeatTimer);
		heartbeatTimer = undefined;
	};

	const beginSession = (ctx: ExtensionContext) => {
		lastCtx = ctx;
		sessionId = sessionIdOf(ctx);
		startedAt = new Date();
		if (isSubagent(ctx)) return;
		if (!paused) reporter.report(presence("session_start", sessionId, ctx.model, startedAt));
		if (!flushTimer) {
			flushTimer = ctx.setInterval(() => void reporter.flush(), FLUSH_MS);
			void reporter.flush(true);
		}
		setStatus(ctx);
	};

	const endSession = (ctx: ExtensionContext) => {
		stopHeartbeat(ctx);
		if (sessionId && !isSubagent(ctx) && !paused) reporter.report(sessionEnd(sessionId));
	};

	// ---- lifecycle -----------------------------------------------------------

	pi.on("session_start", async (_event, ctx) => beginSession(ctx));

	pi.on("session_switch", async (_event, ctx) => {
		endSession(ctx);
		beginSession(ctx);
	});

	pi.on("session_branch", async (_event, ctx) => {
		endSession(ctx);
		beginSession(ctx);
	});

	pi.on("agent_start", async (_event, ctx) => {
		lastCtx = ctx;
		if (isSubagent(ctx)) return;
		const beat = () => {
			if (!paused) reporter.report(presence("heartbeat", sessionIdOf(ctx), ctx.model, startedAt));
		};
		beat();
		stopHeartbeat(ctx);
		heartbeatTimer = ctx.setInterval(beat, HEARTBEAT_MS);
	});

	pi.on("agent_end", async (_event, ctx) => stopHeartbeat(ctx));

	// One event per finished model call. The message object is only read for
	// provider/model/usage counters inside toUsageEvent.
	pi.on("message_end", async (event, ctx) => {
		if (paused) return;
		const usage = toUsageEvent(event.message, sessionIdOf(ctx), isSubagent(ctx));
		if (usage) reporter.report(usage);
	});

	pi.on("session_shutdown", async (_event, ctx) => {
		stopHeartbeat(ctx);
		if (flushTimer) ctx.clearTimer(flushTimer);
		flushTimer = undefined;
		if (sessionId && !isSubagent(ctx) && !paused && config.apiKey) {
			await reporter.send([sessionEnd(sessionId)], SHUTDOWN_SEND_MS).catch(() => {});
		}
	});

	// ---- commands ------------------------------------------------------------

	pi.registerCommand("usage-login", {
		description: "Save your tokenmunchers API key (from the dashboard's settings page)",
		handler: async (args, ctx) => {
			let key = args.trim();
			if (!key && ctx.hasUI) key = (await ctx.ui.input("tokenmunchers API key", "tm_…"))?.trim() ?? "";
			if (!key) return notify(ctx, "usage: /usage-login tm_…", "warning");
			if (!KEY_PATTERN.test(key)) return notify(ctx, "that doesn't look like a tokenmunchers key (tm_…)", "error");

			const fileConfig = loadConfig(dataDir, {});
			saveConfig(dataDir, { ingestUrl: fileConfig.ingestUrl, apiKey: key });
			config = loadConfig(dataDir);
			warnedUnauthorized = false;

			const result = await reporter.send([
				presence("session_start", sessionId ?? sessionIdOf(ctx), ctx.model, startedAt),
			]);
			if (result === "ok") {
				notify(ctx, "logged in. Usage from this machine now shows on the dashboard.");
				void reporter.flush(true);
			} else if (result === "unauthorized") {
				notify(ctx, "key saved, but the server rejected it. Is it revoked?", "error");
			} else {
				notify(ctx, `key saved, but the server couldn't be reached (${reporter.lastError ?? result}). Usage will queue and retry.`, "warning");
			}
		},
	});

	pi.registerCommand("usage-pause", {
		description: "Stop reporting usage for this session (e.g. private or client work)",
		handler: async (_args, ctx) => {
			if (paused) return notify(ctx, "already paused");
			if (sessionId && config.apiKey) reporter.report(sessionEnd(sessionId));
			stopHeartbeat(ctx);
			paused = true;
			setStatus(ctx);
			notify(ctx, "paused. Nothing is reported until /usage-resume or a new OMP process.");
		},
	});

	pi.registerCommand("usage-resume", {
		description: "Resume usage reporting after /usage-pause",
		handler: async (_args, ctx) => {
			if (!paused) return notify(ctx, "not paused");
			paused = false;
			setStatus(ctx);
			if (sessionId) reporter.report(presence("session_start", sessionId, ctx.model, startedAt));
			notify(ctx, "resumed.");
		},
	});

	pi.registerCommand("usage-status", {
		description: "Show tokenmunchers reporting status",
		handler: async (_args, ctx) => {
			const key = config.apiKey ? `${config.apiKey.slice(0, 10)}…` : "not set (run /usage-login)";
			const lines = [
				`key: ${key}`,
				`state: ${paused ? "paused" : "reporting"}`,
				`queued events: ${reporter.queueSize()}`,
				`last error: ${reporter.lastError ?? "none"}`,
				`endpoint: ${config.ingestUrl}`,
			];
			notify(ctx, lines.join("\n"));
		},
	});
}
