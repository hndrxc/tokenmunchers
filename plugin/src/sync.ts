// Fills in what the live path missed, from OMP's local stats.db.
//
// Gap fill: calls from the last ~6.5 days made after this machine went live
// (liveSince), resent as normal usage events. Event ids match the live path's,
// so calls the server already has are ignored.
//
// History: daily totals of every call before liveSince, uploaded once per
// device with replace semantics (/munch backfill).
//
// Both skip calls inside a /munch pause window.

import { sessionKeyFromFile, statsCallToEvent, type UsageEvent, callTime } from "./adapter.ts";
import type { PauseLog, PauseWindow } from "./pauses.ts";
import { callsAfter, callsBefore, maxCallId, type SqlDb, type StatsCall } from "./stats.ts";
import type { SyncState } from "./state.ts";
import type { Reporter, SendResult } from "./transport.ts";

const MAX_AGE_MS = 6.5 * 24 * 60 * 60 * 1000; // ingest rejects > 7 days
// Ingest allows 600 events/min per key, shared with live traffic.
export const GAP_FILL_PER_RUN = 400;
const SEND_BATCH = 100;
const HISTORY_CHUNK = 1000;

// ---- gap fill -----------------------------------------------------------------

export interface GapFillResult {
	scanned: number;
	sent: number;
	cursor: number;
	/** True when the scan reached the newest stats.db row. */
	caughtUp: boolean;
	result: SendResult | "skipped";
}

/** Keys are cached per run: sessionKeyFromFile stats the filesystem. */
function keyCache(): (file: string) => string {
	const cache = new Map<string, string>();
	return (file) => {
		let key = cache.get(file);
		if (key === undefined) {
			key = sessionKeyFromFile(file);
			cache.set(file, key);
		}
		return key;
	};
}

/** Picks the gap-fill events out of a page of stats.db calls. Pure; exported for tests. */
export function gapFillEvents(
	calls: StatsCall[],
	state: Pick<SyncState, "liveSince" | "createdAt">,
	pauses: PauseWindow[],
	isPaused: (key: string, ts: number, windows: PauseWindow[]) => boolean,
	now: number,
	keyOf: (file: string) => string = keyCache(),
): UsageEvent[] {
	if (state.liveSince === null) return [];
	// Before createdAt there was no pause log (0.1.0), so we can't tell what was paused.
	const from = Math.max(state.liveSince, state.createdAt, now - MAX_AGE_MS);
	const events: UsageEvent[] = [];
	for (const call of calls) {
		const at = callTime(call.timestamp, call.duration, now);
		if (at < from) continue;
		const key = keyOf(call.sessionFile);
		if (isPaused(key, call.timestamp, pauses)) continue;
		const event = statsCallToEvent(call, key, now);
		if (event) events.push(event);
	}
	return events;
}

/**
 * Scans up to GAP_FILL_PER_RUN stats.db rows past the cursor and sends the ones
 * the server may be missing. The cursor advances over everything scanned;
 * events that fail to send go to the reporter's retry queue.
 */
export async function gapFill(opts: {
	db: SqlDb;
	state: SyncState;
	pauses: PauseLog;
	reporter: Reporter;
	saveCursor: (cursor: number) => void;
	now?: number;
}): Promise<GapFillResult> {
	const { db, state, pauses, reporter } = opts;
	const now = opts.now ?? Date.now();
	let cursor = state.statsCursor;

	// omp-stats can rebuild its database from scratch, restarting ids. Rescanning
	// is safe because event ids dedupe.
	const maxId = maxCallId(db);
	if (maxId < cursor) cursor = 0;
	if (state.liveSince === null) return { scanned: 0, sent: 0, cursor, caughtUp: true, result: "skipped" };

	const calls = callsAfter(db, cursor, GAP_FILL_PER_RUN);
	if (calls.length === 0) {
		if (cursor !== state.statsCursor) opts.saveCursor(cursor);
		return { scanned: 0, sent: 0, cursor, caughtUp: true, result: "ok" };
	}
	const events = gapFillEvents(calls, state, pauses.read(), (k, ts, w) => pauses.isPaused(k, ts, w), now);
	cursor = calls[calls.length - 1]!.id;

	let result: SendResult = "ok";
	let sent = 0;
	for (let i = 0; i < events.length; i += SEND_BATCH) {
		const batch = events.slice(i, i + SEND_BATCH);
		result = await reporter.send(batch, 10_000);
		if (result === "retry" || result === "unauthorized") {
			reporter.enqueue(events.slice(i));
			break;
		}
		if (result === "ok") sent += batch.length;
	}
	opts.saveCursor(cursor);
	return { scanned: calls.length, sent, cursor, caughtUp: cursor >= maxId, result };
}

// ---- history ------------------------------------------------------------------

export interface HistoryRow {
	day: string;
	provider: string;
	model: string;
	is_subagent: boolean;
	call_count: number;
	input_tokens: number;
	output_tokens: number;
	cache_read_tokens: number;
	cache_write_tokens: number;
	cost_usd: number;
}

export interface HistoryPreview {
	rows: HistoryRow[];
	calls: number;
	tokens: number;
	cost: number;
	firstDay?: string;
	lastDay?: string;
	days: number;
	/** Local-only: tokens per omp-stats project label, to help pick exclusions. Never uploaded. */
	projects: { folder: string; tokens: number; excluded: boolean }[];
	skippedPaused: number;
	skippedExcluded: number;
}

const utcDay = (ms: number) => new Date(ms).toISOString().slice(0, 10);

/**
 * Daily totals of every call before `cutoff`, minus paused calls and projects
 * whose label contains one of `exclude`. Pure over its input; exported for tests.
 */
export function buildHistory(
	calls: Iterable<StatsCall>,
	cutoff: number,
	pauses: PauseWindow[],
	isPaused: (key: string, ts: number, windows: PauseWindow[]) => boolean,
	exclude: string[] = [],
	keyOf: (file: string) => string = keyCache(),
): HistoryPreview {
	const rows = new Map<string, HistoryRow>();
	const projects = new Map<string, { tokens: number; excluded: boolean }>();
	const days = new Set<string>();
	const needles = exclude.map((e) => e.toLowerCase()).filter(Boolean);
	let calls_ = 0;
	let tokens = 0;
	let cost = 0;
	let skippedPaused = 0;
	let skippedExcluded = 0;

	for (const call of calls) {
		const at = callTime(call.timestamp, call.duration, cutoff);
		if (at >= cutoff) continue;
		const callTokens = call.input + call.output + call.cacheRead + call.cacheWrite;
		if (callTokens <= 0) continue;

		const excluded = needles.some((n) => call.folder.toLowerCase().includes(n));
		const project = projects.get(call.folder) ?? { tokens: 0, excluded };
		project.tokens += callTokens;
		projects.set(call.folder, project);
		if (excluded) {
			skippedExcluded++;
			continue;
		}
		if (pauses.length > 0 && isPaused(keyOf(call.sessionFile), call.timestamp, pauses)) {
			skippedPaused++;
			continue;
		}

		// Same normalisation as live events, so history and live agree on ids/limits.
		const event = statsCallToEvent(call, "history", at);
		if (!event) continue;
		const day = utcDay(at);
		const id = `${day}|${event.provider}|${event.model}|${event.is_subagent}`;
		const row = rows.get(id) ?? {
			day,
			provider: event.provider,
			model: event.model,
			is_subagent: event.is_subagent,
			call_count: 0,
			input_tokens: 0,
			output_tokens: 0,
			cache_read_tokens: 0,
			cache_write_tokens: 0,
			cost_usd: 0,
		};
		row.call_count++;
		row.input_tokens += event.input_tokens;
		row.output_tokens += event.output_tokens;
		row.cache_read_tokens += event.cache_read_tokens;
		row.cache_write_tokens += event.cache_write_tokens;
		row.cost_usd += event.cost_usd;
		rows.set(id, row);
		days.add(day);
		calls_++;
		tokens += callTokens;
		cost += event.cost_usd;
	}

	const out = [...rows.values()].map((r) => ({ ...r, cost_usd: Math.round(r.cost_usd * 1e6) / 1e6 }));
	out.sort((a, b) => a.day.localeCompare(b.day));
	const sortedDays = [...days].sort();
	return {
		rows: out,
		calls: calls_,
		tokens,
		cost,
		firstDay: sortedDays[0],
		lastDay: sortedDays.at(-1),
		days: sortedDays.length,
		projects: [...projects.entries()]
			.map(([folder, p]) => ({ folder, ...p }))
			.sort((a, b) => b.tokens - a.tokens),
		skippedPaused,
		skippedExcluded,
	};
}

export function readHistory(
	db: SqlDb,
	cutoff: number,
	pauses: PauseLog,
	exclude: string[],
): HistoryPreview {
	return buildHistory(callsBefore(db, cutoff), cutoff, pauses.read(), (k, ts, w) => pauses.isPaused(k, ts, w), exclude);
}

/** Replaces this device's history on the server. An empty upload clears it. */
export async function uploadHistory(
	reporter: Reporter,
	deviceId: string,
	rows: HistoryRow[],
): Promise<{ ok: true; written: number } | { ok: false; result: SendResult }> {
	let written = 0;
	for (let i = 0; i === 0 || i < rows.length; i += HISTORY_CHUNK) {
		const res = await reporter.history<{ written?: number }>({
			action: "upload",
			device_id: deviceId,
			reset: i === 0,
			rows: rows.slice(i, i + HISTORY_CHUNK),
		});
		if (!res.ok) return res;
		written += res.body.written ?? 0;
	}
	return { ok: true, written };
}

export interface HistoryStatus {
	first_event_at: string | null;
	device: { rows: number; days: number; total_tokens: number; uploaded_at: string | null } | null;
}

export function historyStatus(reporter: Reporter, deviceId: string) {
	return reporter.history<HistoryStatus>({ action: "status", device_id: deviceId }, 10_000);
}
