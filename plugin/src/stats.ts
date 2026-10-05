// The only place that knows omp-stats' SQLite schema (~/.omp/stats.db). It is
// internal to OMP, so if an OMP release changes it, this file is what needs
// updating. Only the columns below are ever read: never folder names for
// upload, error messages, user message prose or tool calls.

import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

/** The subset of bun:sqlite / node:sqlite we use. */
export interface SqlDb {
	prepare(sql: string): { all(...params: unknown[]): unknown[] };
	close(): void;
}

/** One model call as omp-stats recorded it. */
export interface StatsCall {
	id: number;
	sessionFile: string;
	/** omp-stats' project label for the session. Used only for local filtering; never uploaded. */
	folder: string;
	timestamp: number;
	duration: number | null;
	provider: string;
	model: string;
	input: number;
	output: number;
	cacheRead: number;
	cacheWrite: number;
	cost: number;
	isSubagent: boolean;
}

const COLUMNS = [
	"id", "session_file", "folder", "timestamp", "duration", "provider", "model",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "cost_total", "agent_type",
] as const;

const SELECT = `SELECT ${COLUMNS.join(", ")} FROM messages`;

export class StatsSchemaError extends Error {}

/** Throws StatsSchemaError if the messages table lacks a column we read. */
export function checkSchema(db: SqlDb): void {
	const have = new Set((db.prepare("PRAGMA table_info(messages)").all() as { name: string }[]).map((c) => c.name));
	const missing = COLUMNS.filter((c) => !have.has(c));
	if (missing.length > 0) {
		throw new StatsSchemaError(`OMP's stats database has an unexpected layout (missing ${missing.join(", ")})`);
	}
}

const num = (v: unknown): number => (typeof v === "number" && Number.isFinite(v) ? v : typeof v === "bigint" ? Number(v) : 0);

function toCall(row: Record<string, unknown>): StatsCall {
	return {
		id: num(row.id),
		sessionFile: String(row.session_file ?? ""),
		folder: String(row.folder ?? ""),
		timestamp: num(row.timestamp),
		duration: row.duration == null ? null : num(row.duration),
		provider: String(row.provider ?? ""),
		model: String(row.model ?? ""),
		input: num(row.input_tokens),
		output: num(row.output_tokens),
		cacheRead: num(row.cache_read_tokens),
		cacheWrite: num(row.cache_write_tokens),
		cost: num(row.cost_total),
		isSubagent: row.agent_type != null && row.agent_type !== "main",
	};
}

/** Calls with id > afterId, oldest id first. */
export function callsAfter(db: SqlDb, afterId: number, limit: number): StatsCall[] {
	return (db.prepare(`${SELECT} WHERE id > ? ORDER BY id LIMIT ?`).all(afterId, limit) as Record<string, unknown>[]).map(toCall);
}

export function maxCallId(db: SqlDb): number {
	const row = db.prepare("SELECT MAX(id) AS id FROM messages").all()[0] as { id?: unknown } | undefined;
	return num(row?.id);
}

/** Every call with timestamp < beforeMs, read in id pages to bound memory. */
export function* callsBefore(db: SqlDb, beforeMs: number, page = 5000): Generator<StatsCall> {
	let after = 0;
	for (;;) {
		const rows = (
			db.prepare(`${SELECT} WHERE id > ? AND timestamp < ? ORDER BY id LIMIT ?`).all(after, beforeMs, page) as Record<string, unknown>[]
		).map(toCall);
		if (rows.length === 0) return;
		yield* rows;
		after = rows[rows.length - 1]!.id;
	}
}

/**
 * Where OMP keeps stats.db. pi-utils knows for sure (profiles, XDG) but isn't
 * always resolvable from a plugin, so fall back to the usual locations.
 */
export async function findStatsDbPath(agentDir: string | undefined): Promise<string | undefined> {
	if (process.env.TOKENMUNCHERS_STATS_DB) return process.env.TOKENMUNCHERS_STATS_DB;
	try {
		const utils = (await import("@oh-my-pi/pi-utils" as string)) as { getStatsDbPath?: () => string };
		const path = utils.getStatsDbPath?.();
		if (path && existsSync(path)) return path;
	} catch {
		// fall through
	}
	const candidates = [
		agentDir ? join(dirname(agentDir), "stats.db") : undefined,
		process.env.XDG_DATA_HOME ? join(process.env.XDG_DATA_HOME, "omp", "stats.db") : undefined,
		join(homedir(), process.env.PI_CONFIG_DIR || ".omp", "stats.db"),
	];
	return candidates.find((p): p is string => !!p && existsSync(p));
}

/** Opens stats.db read-only with Bun's SQLite (OMP runs on Bun). */
export async function openStatsDb(path: string): Promise<SqlDb> {
	const specifier = "bun:sqlite";
	const { Database } = (await import(specifier)) as {
		Database: new (path: string, opts: { readonly: boolean }) => SqlDb;
	};
	const db = new Database(path, { readonly: true });
	try {
		checkSchema(db);
	} catch (err) {
		db.close();
		throw err;
	}
	return db;
}
