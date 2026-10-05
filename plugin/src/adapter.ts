// The only place that knows OMP's message/usage shapes. If an OMP release
// changes them, this file is what needs updating.

import { randomUUID } from "node:crypto";

export const CLIENT_VERSION = "0.1.0";

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

/**
 * Builds a metadata-only usage event from a finished assistant message, or
 * returns null for anything else (user/tool messages, calls with no usage).
 * Only numeric counters and the provider/model ids are read; content is never touched.
 */
export function toUsageEvent(
	message: unknown,
	sessionId: string,
	isSubagent: boolean,
	now: Date = new Date(),
): UsageEvent | null {
	if (!message || typeof message !== "object") return null;
	const m = message as Record<string, unknown>;
	if (m.role !== "assistant" || !m.usage || typeof m.usage !== "object") return null;

	const usage = m.usage as Record<string, unknown>;
	const costs = (usage.cost && typeof usage.cost === "object" ? usage.cost : {}) as Record<string, unknown>;

	const event: UsageEvent = {
		kind: "usage",
		event_id: randomUUID(),
		session_id: sessionId.slice(0, 128),
		ts: now.toISOString(),
		provider: text(m.provider, 64) ?? "unknown",
		model: text(m.model, 128) ?? "unknown",
		input_tokens: count(usage.input),
		output_tokens: count(usage.output),
		cache_read_tokens: count(usage.cacheRead),
		cache_write_tokens: count(usage.cacheWrite),
		cost_usd: typeof costs.total === "number" && Number.isFinite(costs.total) && costs.total > 0
			? Math.min(Math.round(costs.total * 1e6) / 1e6, 10_000)
			: 0,
		is_subagent: isSubagent,
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
