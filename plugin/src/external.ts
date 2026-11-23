// Usage from tools that call models outside OMP's own provider layer, such as
// a wrapper that drives another vendor's CLI from an OMP agent. OMP never sees
// those calls, so message_end can't report them. Such a tool appends one JSON
// line per call to <dataDir>/external-usage.jsonl; this module turns valid
// lines into normal usage events and hands them to the reporter's queue, which
// already does delivery, retries, backoff and the 7-day age limit.
//
// Only counters and provider/model ids are read from a line. A line's session
// file path is used locally to derive the same path-free session key the live
// path uses (for pause windows and the subagent flag) and is never uploaded.

import { existsSync, readdirSync, readFileSync, renameSync, statSync, unlinkSync, utimesSync } from "node:fs";
import { join } from "node:path";

import { CLIENT_VERSION, sessionIdFromKey, sessionKeyFromFile, type UsageEvent, uuidV5 } from "./adapter.ts";
import type { PauseLog } from "./pauses.ts";

export const EXTERNAL_FILE = "external-usage.jsonl";

const MAX_AGE_MS = 6.5 * 24 * 60 * 60 * 1000; // ingest rejects > 7 days
const MAX_SKEW_MS = 5 * 60 * 1000; // ingest rejects calls from the future
const STALE_DRAIN_MS = 5 * 60_000;
const SOURCE = /^[a-z0-9][a-z0-9_-]{0,31}$/;

/** One line of external-usage.jsonl, as written by the external tool. */
export interface ExternalUsageLine {
	v: 1;
	/** Short lowercase name of the reporting tool, e.g. "agy". */
	source: string;
	/** The call's unique id from that tool; it keys the event id, so a resend is stored once. */
	id: string;
	/** When the call finished: ms since the epoch, or an ISO 8601 string. */
	ts: number | string;
	provider: string;
	model: string;
	input_tokens?: number;
	output_tokens?: number;
	cache_read_tokens?: number;
	cache_write_tokens?: number;
	cost_usd?: number;
	/** OMP's PI_SESSION_ID for the session that ran the tool, if known. */
	session_id?: string;
	/** OMP's PI_SESSION_FILE for that session, if known. Read locally only. */
	session_file?: string;
}

const count = (v: unknown): number =>
	typeof v === "number" && Number.isFinite(v) && v > 0 ? Math.min(Math.round(v), 1e9) : 0;

const text = (v: unknown, max: number): string | undefined =>
	typeof v === "string" && v.length > 0 && v.length <= max ? v : undefined;

function parseTs(v: unknown): number | undefined {
	const t = typeof v === "number" ? v : typeof v === "string" ? Date.parse(v) : Number.NaN;
	return Number.isFinite(t) && t > 0 ? t : undefined;
}

export type ExternalResult = { event: UsageEvent } | { skipped: "invalid" | "stale" | "paused" | "empty" };

/**
 * Validates one parsed line and builds its usage event. Returns why a line was
 * skipped instead; nothing in a skipped line is sent.
 */
export function externalLineToEvent(
	line: unknown,
	pauses: Pick<PauseLog, "isPaused">,
	now: number = Date.now(),
	exists?: (path: string) => boolean,
): ExternalResult {
	if (!line || typeof line !== "object" || Array.isArray(line)) return { skipped: "invalid" };
	const l = line as Record<string, unknown>;
	const source = typeof l.source === "string" && SOURCE.test(l.source) ? l.source : undefined;
	const id = text(l.id, 128);
	const provider = text(l.provider, 64);
	const model = text(l.model, 128);
	const ts = parseTs(l.ts);
	if (l.v !== 1 || !source || !id || !provider || !model || ts === undefined) return { skipped: "invalid" };
	if (ts < now - MAX_AGE_MS || ts > now + MAX_SKEW_MS) return { skipped: "stale" };

	const sessionFile = text(l.session_file, 4096);
	const sessionKey = sessionFile ? sessionKeyFromFile(sessionFile, exists) : undefined;
	if (sessionKey && pauses.isPaused(sessionKey, ts)) return { skipped: "paused" };

	const sessionId = text(l.session_id, 128) ?? (sessionKey ? sessionIdFromKey(sessionKey) : `external-${source}`);
	const cost = typeof l.cost_usd === "number" && Number.isFinite(l.cost_usd) && l.cost_usd > 0
		? Math.min(Math.round(l.cost_usd * 1e6) / 1e6, 10_000)
		: 0;

	const event: UsageEvent = {
		kind: "usage",
		event_id: uuidV5(`external|${source}|${id}`),
		session_id: sessionId.slice(0, 128),
		ts: new Date(ts).toISOString(),
		provider,
		model,
		input_tokens: count(l.input_tokens),
		output_tokens: count(l.output_tokens),
		cache_read_tokens: count(l.cache_read_tokens),
		cache_write_tokens: count(l.cache_write_tokens),
		cost_usd: cost,
		// Subagent session keys are `<parent>/<agent>`; see sessionKeyFromFile.
		is_subagent: sessionKey?.includes("/") ?? false,
		client_version: CLIENT_VERSION,
	};
	const tokens = event.input_tokens + event.output_tokens + event.cache_read_tokens + event.cache_write_tokens;
	return tokens > 0 ? { event } : { skipped: "empty" };
}

export interface DrainResult {
	events: UsageEvent[];
	skipped: number;
}

/**
 * Claims the external usage file (an atomic rename, so writers keep appending
 * to a fresh file and two OMP processes never take the same lines), adopts
 * files left by a crashed drain, and returns their valid events. Removes the
 * claimed files only after `deliver` has taken the events.
 */
export function drainExternalUsage(opts: {
	dataDir: string;
	pauses: Pick<PauseLog, "isPaused">;
	deliver: (events: UsageEvent[]) => void;
	now?: number;
	pid?: number;
}): DrainResult {
	const now = opts.now ?? Date.now();
	const live = join(opts.dataDir, EXTERNAL_FILE);
	const claimed: string[] = [];
	try {
		if (existsSync(live)) {
			const target = join(opts.dataDir, `external-usage.${opts.pid ?? process.pid}.${now}.draining`);
			renameSync(live, target);
			// A rename keeps the old mtime; refresh it so other processes don't
			// mistake a fresh claim for a crashed drain's leftover.
			utimesSync(target, now / 1000, now / 1000);
			claimed.push(target);
		}
		for (const name of readdirSync(opts.dataDir)) {
			if (!name.startsWith("external-usage.") || !name.endsWith(".draining")) continue;
			const full = join(opts.dataDir, name);
			if (claimed.includes(full)) continue;
			if (now - statSync(full).mtimeMs > STALE_DRAIN_MS) claimed.push(full);
		}
	} catch {
		// Another process won the rename, or the directory is missing.
	}

	const events: UsageEvent[] = [];
	const seen = new Set<string>();
	let skipped = 0;
	for (const file of claimed) {
		let lines: string[];
		try {
			lines = readFileSync(file, "utf8").split("\n").filter((l) => l.trim());
		} catch {
			continue;
		}
		for (const raw of lines) {
			let parsed: unknown;
			try {
				parsed = JSON.parse(raw);
			} catch {
				skipped++;
				continue;
			}
			const result = externalLineToEvent(parsed, opts.pauses, now);
			if ("skipped" in result || seen.has(result.event.event_id)) {
				skipped++;
				continue;
			}
			seen.add(result.event.event_id);
			events.push(result.event);
		}
	}

	if (events.length > 0) opts.deliver(events);
	for (const file of claimed) {
		try {
			unlinkSync(file);
		} catch {
			// already gone
		}
	}
	return { events, skipped };
}

/** Lines waiting in the external usage file, for /munch status. */
export function externalPending(dataDir: string): number {
	try {
		return readFileSync(join(dataDir, EXTERNAL_FILE), "utf8").split("\n").filter((l) => l.trim()).length;
	} catch {
		return 0;
	}
}
