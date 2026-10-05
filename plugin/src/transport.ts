// Fire-and-forget delivery with a local JSONL retry queue.
//
// Usage events that fail to send are appended to <dataDir>/queue.jsonl and
// retried with backoff. Presence events (start/heartbeat/end) are never queued;
// a stale heartbeat is worthless.

import { appendFileSync, existsSync, mkdirSync, readdirSync, readFileSync, renameSync, statSync, unlinkSync } from "node:fs";
import { join } from "node:path";

import type { OutboundEvent, UsageEvent } from "./adapter.ts";
import type { Config } from "./config.ts";

export type SendResult = "ok" | "retry" | "unauthorized" | "rejected";

const TIMEOUT_MS = 3_000;
const BATCH = 100;
const MAX_AGE_MS = 6.5 * 24 * 60 * 60 * 1000; // ingest rejects > 7 days
const BASE_BACKOFF_MS = 60_000;
const MAX_BACKOFF_MS = 15 * 60_000;
const STALE_FLUSH_MS = 5 * 60_000;

export interface ReporterOptions {
	dataDir: string;
	getConfig: () => Config;
	fetchImpl?: typeof fetch;
	onUnauthorized?: () => void;
	/** Called with the usage events the server accepted. */
	onDelivered?: (events: UsageEvent[]) => void;
	now?: () => number;
}

export class Reporter {
	readonly #dataDir: string;
	readonly #getConfig: () => Config;
	readonly #fetch: typeof fetch;
	readonly #onUnauthorized?: () => void;
	readonly #onDelivered?: (events: UsageEvent[]) => void;
	readonly #now: () => number;
	#flushing = false;
	#failures = 0;
	#nextFlushAt = 0;
	lastError: string | undefined;

	constructor(opts: ReporterOptions) {
		this.#dataDir = opts.dataDir;
		this.#getConfig = opts.getConfig;
		this.#fetch = opts.fetchImpl ?? fetch;
		this.#onUnauthorized = opts.onUnauthorized;
		this.#onDelivered = opts.onDelivered;
		this.#now = opts.now ?? Date.now;
	}

	get queuePath(): string {
		return join(this.#dataDir, "queue.jsonl");
	}

	/** Never throws, never blocks the caller. */
	report(event: OutboundEvent): void {
		void this.#reportAsync(event).catch(() => {});
	}

	async #reportAsync(event: OutboundEvent): Promise<void> {
		if (!this.#getConfig().apiKey) {
			if (event.kind === "usage") this.enqueue([event]);
			return;
		}
		const result = await this.send([event]);
		if (result === "ok") {
			if (this.#failures > 0 || existsSync(this.queuePath)) {
				this.#failures = 0;
				this.#nextFlushAt = 0;
				void this.flush().catch(() => {});
			}
			return;
		}
		if (event.kind === "usage" && (result === "retry" || result === "unauthorized")) {
			this.enqueue([event]);
			this.#backoff();
		}
	}

	/** Sends events (max 100) in one request. Drops single invalid events and retries the rest. */
	async send(events: OutboundEvent[], timeoutMs = TIMEOUT_MS): Promise<SendResult> {
		const { apiKey, ingestUrl } = this.#getConfig();
		if (!apiKey) return "unauthorized";
		let pending = events;
		while (pending.length > 0) {
			let res: Response;
			try {
				res = await this.#fetch(ingestUrl, {
					method: "POST",
					headers: { "Content-Type": "application/json", Authorization: `Bearer ${apiKey}` },
					body: JSON.stringify(pending.length === 1 ? pending[0] : { events: pending }),
					signal: AbortSignal.timeout(timeoutMs),
				});
			} catch (err) {
				this.lastError = err instanceof Error ? err.message : String(err);
				return "retry";
			}
			if (res.ok) {
				this.lastError = undefined;
				const usage = pending.filter((e): e is UsageEvent => e.kind === "usage");
				if (usage.length > 0) {
					try {
						this.#onDelivered?.(usage);
					} catch {
						// bookkeeping must never fail a send
					}
				}
				return "ok";
			}
			const body = (await res.json().catch(() => ({}))) as { error?: string; index?: number; detail?: string };
			this.lastError = `${res.status} ${body.error ?? ""} ${body.detail ?? ""}`.trim();
			if (res.status === 401) {
				this.#onUnauthorized?.();
				return "unauthorized";
			}
			if (res.status === 400 && typeof body.index === "number" && pending.length > 1) {
				pending = pending.filter((_, i) => i !== body.index);
				continue;
			}
			if (res.status === 400 || res.status === 413) return "rejected";
			return "retry";
		}
		return "ok";
	}

	/**
	 * POSTs to the history function (same key, same host as ingest). Returns the
	 * parsed body on success, or a SendResult describing the failure.
	 */
	async history<T>(body: unknown, timeoutMs = 30_000): Promise<{ ok: true; body: T } | { ok: false; result: SendResult }> {
		const { apiKey, historyUrl } = this.#getConfig();
		if (!apiKey) return { ok: false, result: "unauthorized" };
		let res: Response;
		try {
			res = await this.#fetch(historyUrl, {
				method: "POST",
				headers: { "Content-Type": "application/json", Authorization: `Bearer ${apiKey}` },
				body: JSON.stringify(body),
				signal: AbortSignal.timeout(timeoutMs),
			});
		} catch (err) {
			this.lastError = err instanceof Error ? err.message : String(err);
			return { ok: false, result: "retry" };
		}
		const parsed = (await res.json().catch(() => ({}))) as T & { error?: string; detail?: string };
		if (res.ok) return { ok: true, body: parsed };
		this.lastError = `${res.status} ${parsed.error ?? ""} ${parsed.detail ?? ""}`.trim();
		if (res.status === 401) {
			this.#onUnauthorized?.();
			return { ok: false, result: "unauthorized" };
		}
		return { ok: false, result: res.status === 400 || res.status === 413 ? "rejected" : "retry" };
	}

	enqueue(events: OutboundEvent[]): void {
		if (events.length === 0) return;
		try {
			mkdirSync(this.#dataDir, { recursive: true, mode: 0o700 });
			appendFileSync(this.queuePath, events.map((e) => `${JSON.stringify(e)}\n`).join(""), { mode: 0o600 });
		} catch {
			// Disk trouble must never break the agent.
		}
	}

	queueSize(): number {
		try {
			return readFileSync(this.queuePath, "utf8").split("\n").filter(Boolean).length;
		} catch {
			return 0;
		}
	}

	/** Called on a timer; respects backoff unless forced. */
	async flush(force = false): Promise<void> {
		if (this.#flushing || !this.#getConfig().apiKey) return;
		if (!force && this.#now() < this.#nextFlushAt) return;
		this.#flushing = true;
		try {
			for (const file of this.#claimQueueFiles()) {
				const ok = await this.#flushFile(file);
				if (!ok) break;
			}
		} finally {
			this.#flushing = false;
		}
	}

	// Atomically moves the live queue aside (rename) so concurrent OMP processes
	// can keep appending, and adopts files abandoned by a crashed flush.
	#claimQueueFiles(): string[] {
		const claimed: string[] = [];
		try {
			if (existsSync(this.queuePath)) {
				const target = join(this.#dataDir, `queue.${process.pid}.${this.#now()}.flushing`);
				renameSync(this.queuePath, target);
				claimed.push(target);
			}
			for (const name of readdirSync(this.#dataDir)) {
				if (!name.endsWith(".flushing")) continue;
				const full = join(this.#dataDir, name);
				if (claimed.includes(full)) continue;
				if (this.#now() - statSync(full).mtimeMs > STALE_FLUSH_MS) claimed.push(full);
			}
		} catch {
			// ignore
		}
		return claimed;
	}

	async #flushFile(file: string): Promise<boolean> {
		let events: OutboundEvent[];
		try {
			events = readFileSync(file, "utf8")
				.split("\n")
				.filter(Boolean)
				.flatMap((line) => {
					try {
						return [JSON.parse(line) as OutboundEvent];
					} catch {
						return [];
					}
				})
				.filter((e) => e.kind === "usage" && this.#now() - Date.parse(e.ts) < MAX_AGE_MS);
		} catch {
			return true;
		}

		for (let i = 0; i < events.length; i += BATCH) {
			const result = await this.send(events.slice(i, i + BATCH), 10_000);
			if (result === "retry" || result === "unauthorized") {
				this.enqueue(events.slice(i));
				this.#safeUnlink(file);
				this.#backoff();
				return false;
			}
		}
		this.#safeUnlink(file);
		this.#failures = 0;
		this.#nextFlushAt = 0;
		return true;
	}

	#backoff(): void {
		this.#failures++;
		const delay = Math.min(BASE_BACKOFF_MS * 2 ** (this.#failures - 1), MAX_BACKOFF_MS);
		this.#nextFlushAt = this.#now() + delay;
	}

	#safeUnlink(file: string): void {
		try {
			unlinkSync(file);
		} catch {
			// ignore
		}
	}
}
