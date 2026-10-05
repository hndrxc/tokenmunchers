/**
 * tokenmunchers: reports per-call token usage from Oh My Pi to a shared leaderboard.
 *
 * Sent: provider, model, token counts, cost, timestamps, session id, subagent flag.
 * Never sent: prompts, responses, code, file paths, repo names, tool calls/output, cwd.
 */

import { randomUUID } from "node:crypto";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

import type { ExtensionAPI, ExtensionCommandContext, ExtensionContext } from "@oh-my-pi/pi-coding-agent";

import { presence, sessionEnd, sessionKeyFromFile, toUsageEvent } from "./src/adapter.ts";
import { KEY_PATTERN, loadConfig, loadFile, saveConfig } from "./src/config.ts";
import { PauseLog } from "./src/pauses.ts";
import { statePath } from "./src/state.ts";
import { Syncer } from "./src/syncer.ts";
import { Reporter } from "./src/transport.ts";

type Timer = ReturnType<ExtensionContext["setInterval"]>;

const HEARTBEAT_MS = 30_000;
const FLUSH_MS = 60_000;
const SHUTDOWN_SEND_MS = 1_500;

function resolveAgentDir(pi: ExtensionAPI): string | undefined {
	try {
		return (pi.pi as { getAgentDir?: () => string }).getAgentDir?.() || undefined;
	} catch {
		return undefined;
	}
}

const fmt = (n: number) =>
	n >= 1e9 ? `${(n / 1e9).toFixed(2)}B` : n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${(n / 1e3).toFixed(1)}K` : `${n}`;

export default function tokenmunchers(pi: ExtensionAPI) {
	const agentDir = resolveAgentDir(pi);
	const dataDir = join(agentDir ?? join(homedir(), ".omp", "agent"), "tokenmunchers");
	const fallbackSessionId = randomUUID();
	let config = loadConfig(dataDir);
	let paused = false;
	let warnedUnauthorized = false;
	let lastCtx: ExtensionContext | undefined;
	let sessionId: string | undefined;
	// Sessions this process opened pause windows for, closed on resume/shutdown.
	const pausedKeys = new Set<string>();
	let startedAt = new Date();
	let heartbeatTimer: Timer | undefined;
	let flushTimer: Timer | undefined;

	const notify = (ctx: ExtensionContext | undefined, msg: string, level: "info" | "warning" | "error" = "info") => {
		if (ctx?.hasUI) ctx.ui.notify(`tokenmunchers: ${msg}`, level);
		else pi.logger.info(`tokenmunchers: ${msg}`);
	};

	const pauses = new PauseLog(dataDir);

	const reporter = new Reporter({
		dataDir,
		getConfig: () => config,
		onUnauthorized: () => {
			if (warnedUnauthorized) return;
			warnedUnauthorized = true;
			notify(lastCtx, "API key was rejected. Usage is queued locally; run /munch login with a new key.", "warning");
		},
		onDelivered: (events) => syncer.onDelivered(events),
	});

	// A key configured before state.json existed means this install reported under 0.1.0.
	const syncer = new Syncer({
		dataDir,
		agentDir,
		reporter,
		pauses,
		upgrading: !existsSync(statePath(dataDir)) && !!config.apiKey,
	});

	// Runs on session start and every FLUSH_MS: catch the live data up from stats.db.
	const backgroundSync = async () => {
		if (paused || !config.apiKey) return;
		if (!(await syncer.ensureSeeded())) return;
		await syncer.gapFillTick();
	};

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

	const sessionKeyOf = (ctx: ExtensionContext): string | undefined => {
		try {
			const file = ctx.sessionManager.getSessionFile();
			if (typeof file === "string" && file) return sessionKeyFromFile(file);
		} catch {
			// fall through
		}
		return undefined;
	};

	// Pause windows are keyed like event ids; an unpersisted session falls back to its id.
	const markPaused = (ctx: ExtensionContext) => {
		const key = sessionKeyOf(ctx) ?? sessionIdOf(ctx);
		pauses.open(key);
		pausedKeys.add(key);
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
		if (paused) markPaused(ctx);
		if (!paused) reporter.report(presence("session_start", sessionId, ctx.model, startedAt));
		if (!flushTimer) {
			flushTimer = ctx.setInterval(() => {
				void reporter.flush().then(backgroundSync).catch(() => {});
			}, FLUSH_MS);
			void reporter.flush(true).then(backgroundSync).catch(() => {});
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
		const usage = toUsageEvent(event.message, {
			id: sessionIdOf(ctx),
			key: sessionKeyOf(ctx),
			isSubagent: isSubagent(ctx),
		});
		if (usage) reporter.report(usage);
	});

	pi.on("session_shutdown", async (_event, ctx) => {
		stopHeartbeat(ctx);
		if (flushTimer) ctx.clearTimer(flushTimer);
		flushTimer = undefined;
		if (pausedKeys.size > 0) pauses.close(pausedKeys);
		if (sessionId && !isSubagent(ctx) && !paused && config.apiKey) {
			await reporter.send([sessionEnd(sessionId)], SHUTDOWN_SEND_MS).catch(() => {});
		}
	});

	// ---- /munch ---------------------------------------------------------------

	const login = async (arg: string, ctx: ExtensionCommandContext) => {
		let key = arg;
		if (!key && ctx.hasUI) key = (await ctx.ui.input("tokenmunchers API key", "tm_…"))?.trim() ?? "";
		if (!key) return notify(ctx, "usage: /munch login tm_…", "warning");
		if (!KEY_PATTERN.test(key)) return notify(ctx, "that doesn't look like a tokenmunchers key (tm_…)", "error");

		saveConfig(dataDir, { ingestUrl: loadFile(dataDir).ingestUrl, apiKey: key });
		config = loadConfig(dataDir);
		warnedUnauthorized = false;

		const result = await reporter.send([
			presence("session_start", sessionId ?? sessionIdOf(ctx), ctx.model, startedAt),
		]);
		if (result === "ok") {
			notify(ctx, "logged in. Usage from this machine now shows on the dashboard. Run /munch backfill to add your history from before today.");
			void reporter.flush(true).then(backgroundSync).catch(() => {});
		} else if (result === "unauthorized") {
			notify(ctx, "key saved, but the server rejected it. Is it revoked?", "error");
		} else {
			notify(ctx, `key saved, but the server couldn't be reached (${reporter.lastError ?? result}). Usage will queue and retry.`, "warning");
		}
	};

	const pause = (ctx: ExtensionCommandContext) => {
		if (paused) return notify(ctx, "already paused");
		if (sessionId && config.apiKey) reporter.report(sessionEnd(sessionId));
		stopHeartbeat(ctx);
		paused = true;
		markPaused(ctx);
		setStatus(ctx);
		notify(ctx, "paused. Nothing is reported until /munch resume or a new OMP process.");
	};

	const resume = (ctx: ExtensionCommandContext) => {
		if (!paused) return notify(ctx, "not paused");
		paused = false;
		pauses.close(pausedKeys);
		pausedKeys.clear();
		setStatus(ctx);
		if (sessionId) reporter.report(presence("session_start", sessionId, ctx.model, startedAt));
		notify(ctx, "resumed.");
	};

	const status = (ctx: ExtensionCommandContext) => {
		const key = config.apiKey ? `${config.apiKey.slice(0, 10)}…` : "not set (run /munch login)";
		const { liveSince, seeded, statsCursor, deviceId } = syncer.state;
		const gap = syncer.lastGapFill;
		const lines = [
			`key: ${key}`,
			`state: ${paused ? "paused" : "reporting"}`,
			`queued events: ${reporter.queueSize()}`,
			`last error: ${reporter.lastError ?? syncer.lastError ?? "none"}`,
			`live since: ${liveSince ? new Date(liveSince).toISOString() : seeded ? "no calls reported yet" : "unknown (waiting for server)"}`,
			`gap fill: stats.db row ${statsCursor}${gap ? `, last pass resent ${gap.sent} of ${gap.scanned}` : ""}`,
			`device: ${deviceId.slice(0, 8)}`,
			`endpoint: ${config.ingestUrl}`,
		];
		notify(ctx, lines.join("\n"));
	};

	const sync = async (ctx: ExtensionCommandContext) => {
		if (paused) return notify(ctx, "paused. /munch resume first.", "warning");
		if (!config.apiKey) return notify(ctx, "no key. Run /munch login first.", "warning");
		notify(ctx, "refreshing OMP's stats database (omp stats)…");
		try {
			await syncer.refreshStats();
		} catch (err) {
			notify(ctx, `couldn't refresh stats (${err instanceof Error ? err.message : err}); using what's there.`, "warning");
		}
		if (!(await syncer.ensureSeeded())) {
			return notify(ctx, `couldn't reach the server (${reporter.lastError ?? "unknown error"}).`, "error");
		}
		const result = await syncer.gapFillTick();
		if (!result) {
			return syncer.skipReason
				? notify(ctx, `nothing to do: ${syncer.skipReason}.`)
				: notify(ctx, syncer.lastError ?? "sync failed.", "error");
		}
		notify(
			ctx,
			`checked ${result.scanned} recent calls, resent ${result.sent} (the server ignores ones it already has).` +
				(result.caughtUp ? "" : " More will follow in the background."),
		);
	};

	const backfill = async (args: string[], ctx: ExtensionCommandContext) => {
		if (paused) return notify(ctx, "paused. /munch resume first.", "warning");
		if (!config.apiKey) return notify(ctx, "no key. Run /munch login first.", "warning");

		let yes = false;
		const exclude: string[] = [];
		for (let i = 0; i < args.length; i++) {
			const arg = args[i]!;
			if (arg === "--yes" || arg === "-y") yes = true;
			else if (arg === "--exclude") exclude.push(...(args[++i] ?? "").split(","));
			else if (arg.startsWith("--exclude=")) exclude.push(...arg.slice("--exclude=".length).split(","));
			else return notify(ctx, `unknown option "${arg}". Use: /munch backfill [--exclude a,b] [--yes]`, "warning");
		}

		if (!(await syncer.ensureSeeded())) {
			return notify(ctx, `couldn't reach the server (${reporter.lastError ?? "unknown error"}).`, "error");
		}
		// Queued events are live calls not yet delivered; they must land first so
		// they count as live rather than being folded into history as well.
		await reporter.flush(true);
		if (reporter.queueSize() > 0) {
			return notify(ctx, "some usage is still queued for delivery. Try again once /munch status shows 0 queued.", "warning");
		}

		notify(ctx, "refreshing OMP's stats database (omp stats)…");
		try {
			await syncer.refreshStats();
		} catch (err) {
			notify(ctx, `couldn't refresh stats (${err instanceof Error ? err.message : err}); using what's there.`, "warning");
		}

		let cutoff: number;
		let preview: Awaited<ReturnType<Syncer["historyPreview"]>>["preview"];
		try {
			({ cutoff, preview } = await syncer.historyPreview(exclude));
		} catch (err) {
			return notify(ctx, err instanceof Error ? err.message : String(err), "error");
		}
		const existing = await syncer.serverStatus();
		const previous = existing.ok && existing.body.device?.rows ? existing.body.device : undefined;

		const projects = preview.projects
			.slice(0, 8)
			.map((p) => `  ${p.excluded ? "✗" : "•"} ${p.folder || "(unknown)"}: ${fmt(p.tokens)}`);
		const summary = [
			preview.calls > 0
				? `${preview.calls.toLocaleString()} calls · ${fmt(preview.tokens)} tokens · ${preview.firstDay} → ${preview.lastDay} (${preview.days} days)`
				: "No usage found before this machine went live.",
			`Covers calls before ${new Date(cutoff).toISOString()}; later ones are already live.`,
			...(preview.skippedExcluded ? [`Excluded ${preview.skippedExcluded.toLocaleString()} calls by project.`] : []),
			...(preview.skippedPaused ? [`Skipped ${preview.skippedPaused.toLocaleString()} paused calls.`] : []),
			...(previous ? [`Replaces this machine's previous upload (${fmt(previous.total_tokens)} tokens).`] : []),
			...(projects.length ? ["Projects (shown here only, never uploaded):", ...projects] : []),
			"Only daily totals per model are sent. Exclude projects with /munch backfill --exclude <text>,<text>.",
		].join("\n");

		if (preview.calls === 0 && !previous) return notify(ctx, summary);
		if (!yes) {
			if (!ctx.hasUI) return notify(ctx, `${summary}\nRe-run with --yes to upload.`);
			if (!(await ctx.ui.confirm("Upload usage history?", summary))) return notify(ctx, "backfill cancelled.");
		}

		const result = await syncer.uploadHistory(preview);
		if (result.ok) {
			notify(ctx, `history uploaded: ${preview.rows.length} daily rows, ${fmt(preview.tokens)} tokens. It shows in all-time totals now.`);
		} else {
			notify(ctx, `upload failed (${reporter.lastError ?? result.result}). Nothing changed if the first chunk failed; re-run to finish.`, "error");
		}
	};

	const subcommands = [
		{ value: "status", description: "Key prefix, paused state, queued events and last error" },
		{ value: "sync", description: "Resend recent calls the live reporter missed, from OMP's stats" },
		{ value: "backfill", description: "Upload daily totals from before this machine went live" },
		{ value: "login", description: "Save your API key from the dashboard's settings page" },
		{ value: "pause", description: "Stop reporting (e.g. private or client work)" },
		{ value: "resume", description: "Resume reporting after a pause" },
	];

	pi.registerCommand("munch", {
		description: "tokenmunchers: status | login [key] | pause | resume | sync | backfill",
		getArgumentCompletions: (prefix) => {
			const p = prefix.trimStart();
			if (p.includes(" ")) return null;
			const items = subcommands
				.filter((c) => c.value.startsWith(p))
				.map((c) => ({ value: c.value, label: c.value, description: c.description }));
			return items.length > 0 ? items : null;
		},
		handler: async (args, ctx) => {
			const [sub = "status", ...rest] = args.trim().split(/\s+/).filter(Boolean);
			switch (sub) {
				case "status":
					return status(ctx);
				case "login":
					return login(rest.join(" ").trim(), ctx);
				case "pause":
					return pause(ctx);
				case "resume":
					return resume(ctx);
				case "sync":
					return sync(ctx);
				case "backfill":
					return backfill(rest, ctx);
				default:
					return notify(ctx, `unknown option "${sub}". Use: /munch status | login [key] | pause | resume | sync | backfill`, "warning");
			}
		},
	});
}
