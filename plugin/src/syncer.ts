// Ties gap fill and history import to the plugin's state, stats.db and timers.

import { spawn } from "node:child_process";
import { closeSync, mkdirSync, openSync, statSync, unlinkSync, utimesSync } from "node:fs";
import { join } from "node:path";

import type { UsageEvent } from "./adapter.ts";
import type { PauseLog } from "./pauses.ts";
import { findStatsDbPath, openStatsDb, type SqlDb } from "./stats.ts";
import { loadState, noteLive, type SyncState, updateState } from "./state.ts";
import {
	gapFill,
	type GapFillResult,
	type HistoryPreview,
	historyStatus,
	readHistory,
	uploadHistory,
} from "./sync.ts";
import type { Reporter } from "./transport.ts";

const REFRESH_TIMEOUT_MS = 10 * 60_000;
const LOCK_STALE_MS = 2 * 60_000;

/**
 * Cross-process lock so only one OMP process on the machine gap-fills at a
 * time (they'd all send the same rows and share one rate limit). A lock older
 * than LOCK_STALE_MS belongs to a crashed process and is taken over.
 */
function tryLock(dir: string, path: string, now = Date.now()): boolean {
	try {
		mkdirSync(dir, { recursive: true, mode: 0o700 });
		closeSync(openSync(path, "wx", 0o600));
		return true;
	} catch {
		try {
			if (now - statSync(path).mtimeMs < LOCK_STALE_MS) return false;
			utimesSync(path, now / 1000, now / 1000);
			return true;
		} catch {
			return false;
		}
	}
}

export class Syncer {
	readonly #dataDir: string;
	readonly #agentDir: string | undefined;
	readonly #reporter: Reporter;
	readonly #pauses: PauseLog;
	#state: SyncState;
	#gapFilling = false;
	#statsPath: string | undefined;
	lastGapFill: GapFillResult | undefined;
	lastError: string | undefined;
	/** Why the last gapFillTick did nothing, when it wasn't an error. */
	skipReason: string | undefined;

	#skip(reason: string): undefined {
		this.skipReason = reason;
		return undefined;
	}

	constructor(opts: { dataDir: string; agentDir: string | undefined; reporter: Reporter; pauses: PauseLog; upgrading: boolean }) {
		this.#dataDir = opts.dataDir;
		this.#agentDir = opts.agentDir;
		this.#reporter = opts.reporter;
		this.#pauses = opts.pauses;
		this.#state = loadState(opts.dataDir, opts.upgrading);
	}

	get state(): SyncState {
		return this.#state;
	}

	/** Reporter hook: the earliest delivered call marks when this machine went live. */
	onDelivered(events: UsageEvent[]): void {
		let min = Number.POSITIVE_INFINITY;
		for (const e of events) {
			const t = Date.parse(e.ts);
			if (Number.isFinite(t) && t < min) min = t;
		}
		if (!Number.isFinite(min)) return;
		if (this.#state.liveSince !== null && this.#state.liveSince <= min) return;
		this.#state = noteLive(this.#dataDir, min);
	}

	/**
	 * 0.1.0 didn't record when this machine went live, so an upgraded install asks
	 * the server for the account's first event. Returns whether we're seeded.
	 */
	async ensureSeeded(): Promise<boolean> {
		if (this.#state.seeded) return true;
		const res = await historyStatus(this.#reporter, this.#state.deviceId);
		if (!res.ok) return false;
		const first = res.body.first_event_at ? Date.parse(res.body.first_event_at) : Number.NaN;
		this.#state = updateState(this.#dataDir, (s) => ({
			...s,
			seeded: true,
			liveSince: Number.isFinite(first) ? Math.min(s.liveSince ?? first, first) : s.liveSince,
		}));
		return true;
	}

	async #withDb<T>(fn: (db: SqlDb) => T | Promise<T>): Promise<T> {
		this.#statsPath ??= await findStatsDbPath(this.#agentDir);
		if (!this.#statsPath) throw new Error("couldn't find OMP's stats database. Run `omp stats` once to create it.");
		const db = await openStatsDb(this.#statsPath);
		try {
			return await fn(db);
		} finally {
			db.close();
		}
	}

	/** One bounded gap-fill pass. Never throws; safe to call from a timer. */
	async gapFillTick(): Promise<GapFillResult | undefined> {
		this.skipReason = undefined;
		if (this.#gapFilling) return this.#skip("a sync is already running");
		const lock = join(this.#dataDir, "gapfill.lock");
		if (!tryLock(this.#dataDir, lock)) return this.#skip("another OMP process is syncing; it'll be covered there");
		this.#gapFilling = true;
		try {
			this.#state = loadState(this.#dataDir, false); // other processes may have moved it
			if (this.#state.liveSince === null) return this.#skip("no calls reported from this machine yet");
			const result = await this.#withDb((db) =>
				gapFill({
					db,
					state: this.#state,
					pauses: this.#pauses,
					reporter: this.#reporter,
					// Last writer wins; a cursor that moves back only causes a deduped rescan.
					saveCursor: (cursor) => {
						this.#state = updateState(this.#dataDir, (s) => ({ ...s, statsCursor: cursor }));
					},
				}),
			);
			this.lastGapFill = result;
			this.lastError = undefined;
			return result;
		} catch (err) {
			this.lastError = err instanceof Error ? err.message : String(err);
			return undefined;
		} finally {
			this.#gapFilling = false;
			try {
				unlinkSync(lock);
			} catch {
				// already gone
			}
		}
	}

	/** Runs `omp stats --summary` in a subprocess so stats.db catches up with the session logs. */
	refreshStats(): Promise<void> {
		return new Promise((resolve, reject) => {
			const child = spawn("omp", ["stats", "--summary"], { stdio: "ignore" });
			const timer = setTimeout(() => {
				child.kill();
				reject(new Error("`omp stats` took too long"));
			}, REFRESH_TIMEOUT_MS);
			child.on("error", (err) => {
				clearTimeout(timer);
				reject(err);
			});
			child.on("exit", (code) => {
				clearTimeout(timer);
				if (code === 0) resolve();
				else reject(new Error(`\`omp stats\` exited with ${code}`));
			});
		});
	}

	/**
	 * Everything before this machine went live. If it never has, now becomes the
	 * split point: calls from here on are live, earlier ones are history.
	 */
	async historyPreview(exclude: string[]): Promise<{ cutoff: number; preview: HistoryPreview }> {
		if (this.#state.liveSince === null) {
			const now = Date.now();
			this.#state = updateState(this.#dataDir, (s) => (s.liveSince === null ? { ...s, liveSince: now } : s));
		}
		const cutoff = this.#state.liveSince!;
		const preview = await this.#withDb((db) => readHistory(db, cutoff, this.#pauses, exclude));
		return { cutoff, preview };
	}

	uploadHistory(preview: HistoryPreview) {
		return uploadHistory(this.#reporter, this.#state.deviceId, preview.rows);
	}

	serverStatus() {
		return historyStatus(this.#reporter, this.#state.deviceId);
	}
}
