// The only place that knows OMP's message/usage shapes. If an OMP release
// changes them, this file is what needs updating.

import { createHash, randomUUID } from "node:crypto";
import { existsSync } from "node:fs";
import { basename, dirname } from "node:path";

export const CLIENT_VERSION = "0.2.0";

export interface UsageEvent {
	kind: "usage";
	event_id: string;
	session_id: string;
	ts: string;
	provider: string;
	model: string;
	input_tokens: number;
	output_tokens: number;
	cache_read_tokens: number;
	cache_write_tokens: number;
	cost_usd: number;
	is_subagent: boolean;
	client_version: string;
}

export interface PresenceEvent {
	kind: "session_start" | "heartbeat";
	session_id: string;
	provider?: string;
	model?: string;
	started_at?: string;
	client_version: string;
}

export interface SessionEndEvent {
	kind: "session_end";
	session_id: string;
	client_version: string;
}

export type OutboundEvent = UsageEvent | PresenceEvent | SessionEndEvent;

const count = (v: unknown): number =>
	typeof v === "number" && Number.isFinite(v) && v > 0 ? Math.min(Math.round(v), 1e9) : 0;

const text = (v: unknown, max: number): string | undefined =>
	typeof v === "string" && v.length > 0 ? v.slice(0, max) : undefined;

// Fixed namespace for tokenmunchers event ids. Changing it re-keys every event
// and breaks dedupe between the live path and a stats.db backfill.
const EVENT_NAMESPACE = Buffer.from("6f1d3c52a0e44b7f9c8e2d5a7b3f1e90", "hex");

/** RFC 4122 v5 (SHA-1, name-based) UUID. */
export function uuidV5(name: string, namespace: Buffer = EVENT_NAMESPACE): string {
	const hash = createHash("sha1").update(namespace).update(name, "utf8").digest();
	hash[6] = (hash[6]! & 0x0f) | 0x50;
	hash[8] = (hash[8]! & 0x3f) | 0x80;
	const hex = hash.subarray(0, 16).toString("hex");
	return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

/**
 * Stable, path-free key for a session transcript, shared by the live path and
 * omp-stats rows (which store the same file path). Main sessions are
 * `<cwd dir>/<ts>_<sessionId>.jsonl`; subagents live under their parent as
 * `<parent stem>/<agentId>.jsonl`, so we walk up while the directory is itself a
 * session (`<dir>.jsonl` exists). The cwd directory is never part of the key.
 */
export function sessionKeyFromFile(file: string, exists: (path: string) => boolean = existsSync): string {
	const parts = [basename(file, ".jsonl")];
	let dir = dirname(file);
	for (let depth = 0; depth < 8 && exists(`${dir}.jsonl`); depth++) {
		parts.unshift(basename(dir));
		dir = dirname(dir);
	}
	return parts.join("/");
}

/**
 * Deterministic event id for one model call, so a call reported live and again
 * by a stats.db backfill lands once (ingest does `on conflict (event_id) do nothing`).
 * Falls back to a random id when the call can't be identified.
 */
export function usageEventId(sessionKey: string | undefined, timestamp: unknown, provider: string, model: string): string {
	if (!sessionKey || typeof timestamp !== "number" || !Number.isFinite(timestamp)) return randomUUID();
	return uuidV5(`${sessionKey}|${Math.trunc(timestamp)}|${provider}|${model}`);
}

export interface SessionInfo {
	id: string;
	/** From {@link sessionKeyFromFile}; without it the event id is random. */
	key?: string;
	isSubagent: boolean;
}

/**
 * Builds a metadata-only usage event from a finished assistant message, or
 * returns null for anything else (user/tool messages, calls with no usage).
 * Only numeric counters and the provider/model ids are read; content is never touched.
 */
export function toUsageEvent(
	message: unknown,
	session: SessionInfo,
	now: Date = new Date(),
): UsageEvent | null {
	if (!message || typeof message !== "object") return null;
	const m = message as Record<string, unknown>;
	if (m.role !== "assistant" || !m.usage || typeof m.usage !== "object") return null;

	const usage = m.usage as Record<string, unknown>;
	const costs = (usage.cost && typeof usage.cost === "object" ? usage.cost : {}) as Record<string, unknown>;

	const provider = text(m.provider, 64) ?? "unknown";
	const model = text(m.model, 128) ?? "unknown";

	const event: UsageEvent = {
		kind: "usage",
		event_id: usageEventId(session.key, m.timestamp, provider, model),
		session_id: session.id.slice(0, 128),
		ts: now.toISOString(),
		provider,
		model,
		input_tokens: count(usage.input),
		output_tokens: count(usage.output),
		cache_read_tokens: count(usage.cacheRead),
		cache_write_tokens: count(usage.cacheWrite),
		cost_usd: typeof costs.total === "number" && Number.isFinite(costs.total) && costs.total > 0
			? Math.min(Math.round(costs.total * 1e6) / 1e6, 10_000)
			: 0,
		is_subagent: session.isSubagent,
		client_version: CLIENT_VERSION,
	};

	const tokens = event.input_tokens + event.output_tokens + event.cache_read_tokens + event.cache_write_tokens;
	return tokens > 0 ? event : null;
}

export function presence(
	kind: PresenceEvent["kind"],
	sessionId: string,
	model: { id?: unknown; provider?: unknown } | undefined,
	startedAt?: Date,
): PresenceEvent {
	return {
		kind,
		session_id: sessionId.slice(0, 128),
		provider: text(model?.provider, 64),
		model: text(model?.id, 128),
		...(startedAt ? { started_at: startedAt.toISOString() } : {}),
		client_version: CLIENT_VERSION,
	};
}

export function sessionEnd(sessionId: string): SessionEndEvent {
	return { kind: "session_end", session_id: sessionId.slice(0, 128), client_version: CLIENT_VERSION };
}
