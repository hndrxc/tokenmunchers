import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { afterEach, beforeEach, describe, it } from "node:test";

import { callTime, sessionKeyFromFile, toUsageEvent } from "../src/adapter.ts";
import { PauseLog } from "../src/pauses.ts";
import { loadState, noteLive } from "../src/state.ts";
import { callsAfter, callsBefore, checkSchema, type SqlDb, type StatsCall, StatsSchemaError } from "../src/stats.ts";
import { buildHistory, gapFill, gapFillEvents, uploadHistory } from "../src/sync.ts";
import { Syncer } from "../src/syncer.ts";
import { Reporter } from "../src/transport.ts";

const KEY = `tm_${"a".repeat(43)}`;
const DAY = 86_400_000;
const NOW = Date.parse("2026-10-05T12:00:00Z");

const respond = (status: number, body: unknown = {}) =>
	new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

// The parts of omp-stats' messages table we read, plus columns we must never touch.
function statsDb(): DatabaseSync & SqlDb {
	const db = new DatabaseSync(":memory:");
	db.exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT, session_file TEXT NOT NULL, entry_id TEXT NOT NULL, folder TEXT NOT NULL,
		model TEXT NOT NULL, provider TEXT NOT NULL, api TEXT NOT NULL, timestamp INTEGER NOT NULL, duration INTEGER,
		ttft INTEGER, stop_reason TEXT NOT NULL, error_message TEXT, input_tokens INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL, cache_read_tokens INTEGER NOT NULL, cache_write_tokens INTEGER NOT NULL,
		total_tokens INTEGER NOT NULL, cost_total REAL NOT NULL, agent_type TEXT NOT NULL DEFAULT 'main')`);
	return db as DatabaseSync & SqlDb;
}

interface Row {
	file: string;
	folder?: string;
	ts: number;
	duration?: number | null;
	model?: string;
	agent?: string;
	tokens?: [number, number, number, number];
	cost?: number;
}

function insert(db: DatabaseSync, rows: Row[]): void {
	const stmt = db.prepare(`INSERT INTO messages (session_file, entry_id, folder, model, provider, api, timestamp, duration,
		stop_reason, error_message, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, total_tokens, cost_total, agent_type)
		VALUES (?, 'e', ?, ?, 'anthropic', 'messages', ?, ?, 'stop', 'SECRET ERROR', ?, ?, ?, ?, ?, ?, ?)`);
	for (const r of rows) {
		const [i, o, cr, cw] = r.tokens ?? [100, 50, 1000, 10];
		stmt.run(r.file, r.folder ?? "--home-u-proj--", r.model ?? "claude-opus-5-5", r.ts, r.duration === undefined ? 2000 : r.duration,
			i, o, cr, cw, i + o + cr + cw, r.cost ?? 0.5, r.agent ?? "main");
	}
}

const call = (over: Partial<StatsCall>): StatsCall => ({
	id: 1, sessionFile: "/s/--p--/2026_s1.jsonl", folder: "--home-u-proj--", timestamp: NOW - DAY, duration: 1000,
	provider: "anthropic", model: "claude-opus-5-5", input: 100, output: 50, cacheRead: 0, cacheWrite: 0, cost: 0.1,
	isSubagent: false, ...over,
});

const noPause = () => false;
const keyOf = (file: string) => sessionKeyFromFile(file, () => false);

describe("live and stats.db agree", () => {
	it("a stats.db row maps to the same event id and ts as the live message", () => {
		const db = statsDb();
		const file = "/home/u/.omp/agent/sessions/--home-u-proj--/2026-10-04T10-00-00-000Z_abc.jsonl";
		insert(db, [{ file, ts: NOW - 60_000, duration: 4000 }]);
		const [row] = callsAfter(db, 0, 10);
		const key = keyOf(file);

		const live = toUsageEvent(
			{ role: "assistant", provider: "anthropic", model: "claude-opus-5-5", timestamp: NOW - 60_000, duration: 4000,
				usage: { input: 100, output: 50, cacheRead: 1000, cacheWrite: 10, cost: { total: 0 } } },
			{ id: "abc", key, isSubagent: false },
			new Date(NOW),
		)!;
		const [filled] = gapFillEvents([row!], { liveSince: NOW - DAY, createdAt: NOW - DAY }, [], noPause, NOW, keyOf);
		assert.equal(filled!.event_id, live.event_id);
		assert.equal(filled!.ts, live.ts);
		assert.equal(live.ts, new Date(NOW - 56_000).toISOString(), "ts is start + duration");
		assert.equal(filled!.session_id, "abc");
	});

	it("callTime clamps to now and falls back to now without a timestamp", () => {
		assert.equal(callTime(NOW + 10_000, 0, NOW), NOW);
		assert.equal(callTime(undefined, 5, NOW), NOW);
		assert.equal(callTime(NOW - 10, null, NOW), NOW - 10);
	});
});

describe("stats reader", () => {
	it("reads only the allowlisted columns, paging by id", () => {
		const db = statsDb();
		insert(db, [1, 2, 3].map((n) => ({ file: `/s/${n}.jsonl`, ts: NOW - n * DAY })));
		const rows = callsAfter(db, 1, 10);
		assert.deepEqual(rows.map((r) => r.id), [2, 3]);
		assert.ok(!JSON.stringify(rows).includes("SECRET"));
		assert.deepEqual([...callsBefore(db, NOW - 1.5 * DAY, 1)].map((r) => r.id), [2, 3]);
	});

	it("maps agent_type to the subagent flag", () => {
		const db = statsDb();
		insert(db, [{ file: "/a.jsonl", ts: NOW, agent: "main" }, { file: "/b.jsonl", ts: NOW, agent: "subagent" }]);
		assert.deepEqual(callsAfter(db, 0, 10).map((r) => r.isSubagent), [false, true]);
	});

	it("refuses a schema it doesn't know", () => {
		const db = new DatabaseSync(":memory:") as DatabaseSync & SqlDb;
		db.exec("CREATE TABLE messages (id INTEGER PRIMARY KEY, timestamp INTEGER)");
		assert.throws(() => checkSchema(db), StatsSchemaError);
	});
});

describe("gap fill", () => {
	const state = { liveSince: NOW - 3 * DAY, createdAt: NOW - 2 * DAY };

	it("only covers calls after going live, after the pause log existed, and within ingest's window", () => {
		const events = gapFillEvents(
			[
				call({ id: 1, timestamp: NOW - 4 * DAY }), // before liveSince
				call({ id: 2, timestamp: NOW - 2.5 * DAY }), // before createdAt (0.1.0 era)
				call({ id: 3, timestamp: NOW - DAY }),
				call({ id: 4, timestamp: NOW - 1000, duration: null }),
			],
			state, [], noPause, NOW, keyOf,
		);
		assert.equal(events.length, 2);

		const old = gapFillEvents([call({ timestamp: NOW - 6.6 * DAY })], { liveSince: 0, createdAt: 0 }, [], noPause, NOW, keyOf);
		assert.equal(old.length, 0, "older than ingest accepts");
		assert.equal(gapFillEvents([call({})], { liveSince: null, createdAt: 0 }, [], noPause, NOW, keyOf).length, 0);
	});

	it("skips paused calls, including subagents of a paused session", () => {
		const log = new PauseLog(mkdtempSync(join(tmpdir(), "tm-gap-")));
		log.open("2026_s1", NOW - 2 * DAY);
		log.close(["2026_s1"], NOW - DAY + 5000);
		const exists = (p: string) => p === "/s/--p--/2026_s1.jsonl";
		const events = gapFillEvents(
			[
				call({ id: 1, timestamp: NOW - DAY }),
				call({ id: 2, timestamp: NOW - DAY, sessionFile: "/s/--p--/2026_s1/0-Explore.jsonl", isSubagent: true }),
				call({ id: 3, timestamp: NOW - DAY + 10_000 }),
				call({ id: 4, timestamp: NOW - DAY, sessionFile: "/s/--p--/2026_s2.jsonl" }),
			],
			{ liveSince: 0, createdAt: 0 },
			log.read(),
			(k, ts, w) => log.isPaused(k, ts, w),
			NOW,
			(f) => sessionKeyFromFile(f, exists),
		);
		assert.equal(events.length, 2, "only the post-resume call and the other session");
	});

	it("sends in batches, advances the cursor, and queues failures", async () => {
		const dir = mkdtempSync(join(tmpdir(), "tm-gap-"));
		const db = statsDb();
		insert(db, Array.from({ length: 150 }, (_, n) => ({ file: `/s/${n}.jsonl`, ts: Date.now() - DAY + n })));
		let online = true;
		const batches: number[] = [];
		const reporter = new Reporter({
			dataDir: dir,
			getConfig: () => ({ ingestUrl: "https://x.test/ingest", historyUrl: "https://x.test/history", apiKey: KEY }),
			fetchImpl: (async (_u: string, init: RequestInit) => {
				const body = JSON.parse(init.body as string);
				batches.push(body.events?.length ?? 1);
				if (!online) throw new Error("offline");
				return respond(200);
			}) as typeof fetch,
		});
		let cursor = 0;
		const st = { deviceId: "d", createdAt: 0, liveSince: 0, seeded: true, statsCursor: 0 };
		const res = await gapFill({ db, state: st, pauses: new PauseLog(dir), reporter, saveCursor: (c) => (cursor = c) });
		assert.deepEqual(batches, [100, 50]);
		assert.equal(res.sent, 150);
		assert.equal(cursor, 150);
		assert.ok(res.caughtUp);

		online = false;
		insert(db, [{ file: "/s/x.jsonl", ts: Date.now() - 1000 }]);
		const res2 = await gapFill({ db, state: { ...st, statsCursor: cursor }, pauses: new PauseLog(dir), reporter, saveCursor: (c) => (cursor = c) });
		assert.equal(res2.result, "retry");
		assert.equal(cursor, 151);
		assert.equal(reporter.queueSize(), 1, "failed events go to the retry queue");

		// stats.db rebuilt from scratch: ids restart, so the cursor resets.
		const fresh = statsDb();
		insert(fresh, [{ file: "/s/y.jsonl", ts: Date.now() - 1000 }]);
		online = true;
		const res3 = await gapFill({ db: fresh, state: { ...st, statsCursor: 151 }, pauses: new PauseLog(dir), reporter, saveCursor: (c) => (cursor = c) });
		assert.equal(res3.scanned, 1);
		rmSync(dir, { recursive: true, force: true });
	});
});

describe("history", () => {
	const cutoff = Date.parse("2026-10-03T15:00:00Z");

	it("aggregates calls before the cutoff into UTC daily rows per model and subagent flag", () => {
		const h = buildHistory(
			[
				call({ id: 1, timestamp: Date.parse("2026-10-01T23:59:59Z"), duration: 2000 }), // ends on the 2nd
				call({ id: 2, timestamp: Date.parse("2026-10-02T10:00:00Z"), cost: 0.25 }),
				call({ id: 3, timestamp: Date.parse("2026-10-02T11:00:00Z"), isSubagent: true }),
				call({ id: 4, timestamp: Date.parse("2026-10-02T12:00:00Z"), model: "claude-sonnet-5-5" }),
				call({ id: 5, timestamp: cutoff - 500, duration: 1000 }), // ends after cutoff: live's
				call({ id: 6, timestamp: Date.parse("2026-09-30T08:00:00Z"), input: 0, output: 0 }), // no tokens
			],
			cutoff, [], noPause, [], keyOf,
		);
		assert.equal(h.calls, 4);
		assert.deepEqual(h.rows.map((r) => [r.day, r.model, r.is_subagent, r.call_count]), [
			["2026-10-02", "claude-opus-5-5", false, 2],
			["2026-10-02", "claude-opus-5-5", true, 1],
			["2026-10-02", "claude-sonnet-5-5", false, 1],
		]);
		assert.equal(h.rows[0]!.cost_usd, 0.35);
		assert.equal(h.firstDay, "2026-10-02");
		assert.equal(h.days, 1);
	});

	it("excludes projects locally and never puts folders in the rows", () => {
		const h = buildHistory(
			[
				call({ id: 1, folder: "--home-u-client-acme--", timestamp: cutoff - DAY }),
				call({ id: 2, folder: "--home-u-hobby--", timestamp: cutoff - DAY }),
			],
			cutoff, [], noPause, ["ACME"], keyOf,
		);
		assert.equal(h.calls, 1);
		assert.equal(h.skippedExcluded, 1);
		assert.deepEqual(h.projects.map((p) => [p.folder, p.excluded]), [["--home-u-client-acme--", true], ["--home-u-hobby--", false]]);
		assert.ok(!JSON.stringify(h.rows).includes("--home"));
		assert.deepEqual(Object.keys(h.rows[0]!).sort(), [
			"cache_read_tokens", "cache_write_tokens", "call_count", "cost_usd", "day", "input_tokens", "is_subagent",
			"model", "output_tokens", "provider",
		]);
	});

	it("uploads in chunks, resetting only on the first, and an empty upload still clears", async () => {
		const bodies: { reset: boolean; rows: unknown[]; device_id: string; action: string }[] = [];
		const reporter = new Reporter({
			dataDir: mkdtempSync(join(tmpdir(), "tm-hist-")),
			getConfig: () => ({ ingestUrl: "https://x.test/ingest", historyUrl: "https://x.test/history", apiKey: KEY }),
			fetchImpl: (async (url: string, init: RequestInit) => {
				assert.equal(url, "https://x.test/history");
				const body = JSON.parse(init.body as string);
				bodies.push(body);
				return respond(200, { written: body.rows.length });
			}) as typeof fetch,
		});
		const row = { day: "2026-10-01", provider: "p", model: "m", is_subagent: false, call_count: 1, input_tokens: 1,
			output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, cost_usd: 0 };
		const res = await uploadHistory(reporter, "dev", Array.from({ length: 2500 }, () => row));
		assert.deepEqual(res, { ok: true, written: 2500 });
		assert.deepEqual(bodies.map((b) => [b.action, b.reset, b.rows.length]), [["upload", true, 1000], ["upload", false, 1000], ["upload", false, 500]]);

		bodies.length = 0;
		await uploadHistory(reporter, "dev", []);
		assert.deepEqual(bodies.map((b) => [b.reset, b.rows.length]), [[true, 0]]);
	});
});

describe("sync state", () => {
	let dir: string;
	beforeEach(() => {
		dir = mkdtempSync(join(tmpdir(), "tm-state-"));
	});
	afterEach(() => rmSync(dir, { recursive: true, force: true }));

	it("creates a device once; upgraded installs start unseeded", () => {
		const a = loadState(dir, true, 1000);
		const b = loadState(dir, false, 2000);
		assert.equal(a.deviceId, b.deviceId);
		assert.equal(b.seeded, false, "the first run's verdict sticks");
		assert.equal(b.createdAt, 1000);
		assert.equal(loadState(mkdtempSync(join(tmpdir(), "tm-state-")), false).seeded, true);
	});

	it("liveSince only moves earlier", () => {
		loadState(dir, false);
		assert.equal(noteLive(dir, 5000).liveSince, 5000);
		assert.equal(noteLive(dir, 9000).liveSince, 5000);
		assert.equal(noteLive(dir, 3000).liveSince, 3000);
	});

	it("records liveSince from delivered events and seeds upgrades from the server", async () => {
		writeFileSync(join(dir, "config.json"), "{}");
		const calls: string[] = [];
		let syncer: Syncer;
		const reporter = new Reporter({
			dataDir: dir,
			getConfig: () => ({ ingestUrl: "https://x.test/ingest", historyUrl: "https://x.test/history", apiKey: KEY }),
			onDelivered: (events) => syncer.onDelivered(events),
			fetchImpl: (async (url: string) => {
				calls.push(url);
				return url.endsWith("/history")
					? respond(200, { first_event_at: "2026-10-04T08:00:00Z", device: null })
					: respond(200, { accepted: 1 });
			}) as typeof fetch,
		});
		syncer = new Syncer({ dataDir: dir, agentDir: undefined, reporter, pauses: new PauseLog(dir), upgrading: true });
		assert.equal(syncer.state.seeded, false);

		const ev = toUsageEvent(
			{ role: "assistant", provider: "p", model: "m", timestamp: NOW, usage: { input: 1 } },
			{ id: "s", isSubagent: false },
			new Date(NOW),
		)!;
		assert.equal(await reporter.send([ev, { kind: "heartbeat", session_id: "s", client_version: "x" }]), "ok");
		assert.equal(syncer.state.liveSince, NOW);

		assert.ok(await syncer.ensureSeeded());
		assert.equal(syncer.state.seeded, true);
		assert.equal(syncer.state.liveSince, Date.parse("2026-10-04T08:00:00Z"), "server's earlier first event wins");
		assert.ok(calls.includes("https://x.test/history"));
	});
});

describe("sessionKeyFromFile on disk", () => {
	it("detects subagent directories by their sibling transcript", () => {
		const root = mkdtempSync(join(tmpdir(), "tm-sess-"));
		const cwdDir = join(root, "--home-u-proj--");
		mkdirSync(join(cwdDir, "2026_abc"), { recursive: true });
		writeFileSync(join(cwdDir, "2026_abc.jsonl"), "");
		assert.equal(sessionKeyFromFile(join(cwdDir, "2026_abc", "0-Task.jsonl")), "2026_abc/0-Task");
		assert.equal(sessionKeyFromFile(join(cwdDir, "2026_abc.jsonl")), "2026_abc");
		rmSync(root, { recursive: true, force: true });
	});
});
