import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";

import { sessionKeyFromFile, toUsageEvent, usageEventId } from "../src/adapter.ts";
import { PauseLog } from "../src/pauses.ts";
import { Reporter } from "../src/transport.ts";

const KEY = `tm_${"a".repeat(43)}`;

const assistantMessage = {
	role: "assistant",
	provider: "anthropic",
	model: "claude-opus-5-5",
	content: [{ type: "text", text: "SECRET RESPONSE TEXT" }],
	usage: {
		input: 1200,
		output: 800,
		cacheRead: 30000,
		cacheWrite: 500,
		totalTokens: 32500,
		cost: { input: 0.01, output: 0.02, cacheRead: 0.03, cacheWrite: 0.04, total: 0.1234567891 },
	},
	stopReason: "stop",
	timestamp: 1_791_115_200_000,
};

const main = (id = "sess-1") => ({ id, key: `2026-10-04T12-00-00-000Z_${id}`, isSubagent: false });
const sub = { id: "sub-1", key: "2026-10-04T12-00-00-000Z_sess-1/0-Explore", isSubagent: true };

describe("toUsageEvent", () => {
	it("maps OMP usage to the event payload", () => {
		const e = toUsageEvent(assistantMessage, main(), new Date("2026-10-04T12:00:00Z"));
		assert.ok(e);
		assert.equal(e.provider, "anthropic");
		assert.equal(e.model, "claude-opus-5-5");
		assert.equal(e.input_tokens, 1200);
		assert.equal(e.output_tokens, 800);
		assert.equal(e.cache_read_tokens, 30000);
		assert.equal(e.cache_write_tokens, 500);
		assert.equal(e.cost_usd, 0.123457);
		assert.equal(e.is_subagent, false);
		assert.equal(e.ts, "2026-10-04T12:00:00.000Z");
		assert.match(e.event_id, /^[0-9a-f-]{36}$/);
	});

	it("never includes message content", () => {
		const e = toUsageEvent(assistantMessage, sub);
		assert.ok(!JSON.stringify(e).includes("SECRET"));
		assert.deepEqual(Object.keys(e!).sort(), [
			"cache_read_tokens", "cache_write_tokens", "client_version", "cost_usd", "event_id", "input_tokens",
			"is_subagent", "kind", "model", "output_tokens", "provider", "session_id", "ts",
		]);
	});

	it("ignores non-assistant messages and zero-usage calls", () => {
		assert.equal(toUsageEvent({ role: "user", content: "hi" }, { id: "s", isSubagent: false }), null);
		assert.equal(toUsageEvent({ role: "toolResult" }, { id: "s", isSubagent: false }), null);
		assert.equal(toUsageEvent({ ...assistantMessage, usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }, { id: "s", isSubagent: false }), null);
		assert.equal(toUsageEvent(null, { id: "s", isSubagent: false }), null);
	});

	it("tolerates missing or junk usage fields", () => {
		const e = toUsageEvent({ role: "assistant", usage: { input: 10, output: Number.NaN, cacheRead: -5 } }, { id: "s", isSubagent: false });
		assert.ok(e);
		assert.equal(e.provider, "unknown");
		assert.equal(e.output_tokens, 0);
		assert.equal(e.cache_read_tokens, 0);
		assert.equal(e.cost_usd, 0);
	});
});

describe("event ids", () => {
	it("are deterministic per call, so a backfill dedupes against live events", () => {
		const a = toUsageEvent(assistantMessage, main())!;
		const b = toUsageEvent(assistantMessage, main(), new Date(0))!;
		assert.equal(a.event_id, b.event_id);
		assert.match(a.event_id, /^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
		assert.equal(a.event_id, usageEventId(main().key, assistantMessage.timestamp, "anthropic", "claude-opus-5-5"));
	});

	it("differ across sessions, subagents, timestamps and models", () => {
		const ids = new Set([
			toUsageEvent(assistantMessage, main())!.event_id,
			toUsageEvent(assistantMessage, main("sess-2"))!.event_id,
			toUsageEvent(assistantMessage, sub)!.event_id,
			toUsageEvent({ ...assistantMessage, timestamp: assistantMessage.timestamp + 1 }, main())!.event_id,
			toUsageEvent({ ...assistantMessage, model: "claude-sonnet-5-5" }, main())!.event_id,
		]);
		assert.equal(ids.size, 5);
	});

	it("fall back to random ids without a session key or timestamp", () => {
		const noKey = { id: "s", isSubagent: false };
		assert.notEqual(toUsageEvent(assistantMessage, noKey)!.event_id, toUsageEvent(assistantMessage, noKey)!.event_id);
		const noTs = { ...assistantMessage, timestamp: undefined };
		assert.notEqual(toUsageEvent(noTs, main())!.event_id, toUsageEvent(noTs, main())!.event_id);
	});
});

describe("sessionKeyFromFile", () => {
	const sessions = "/home/u/.omp/agent/sessions/--home-u-secret-repo--";
	const files = new Set([`${sessions}/2026_abc.jsonl`, `${sessions}/2026_abc/0-Explore.jsonl`]);
	const exists = (p: string) => files.has(p);

	it("uses the file stem for main sessions, never the cwd directory", () => {
		const key = sessionKeyFromFile(`${sessions}/2026_abc.jsonl`, exists);
		assert.equal(key, "2026_abc");
	});

	it("prefixes subagents (and their children) with the parent session", () => {
		assert.equal(sessionKeyFromFile(`${sessions}/2026_abc/0-Explore.jsonl`, exists), "2026_abc/0-Explore");
		assert.equal(sessionKeyFromFile(`${sessions}/2026_abc/0-Explore/1-Task.jsonl`, exists), "2026_abc/0-Explore/1-Task");
	});
});

describe("PauseLog", () => {
	let dir: string;
	beforeEach(() => {
		dir = mkdtempSync(join(tmpdir(), "tm-pause-"));
	});
	afterEach(() => rmSync(dir, { recursive: true, force: true }));

	it("records windows that cover the session and its subagents", () => {
		const log = new PauseLog(dir);
		log.open("s1", 1000);
		log.open("s1", 1500); // already open: no-op
		assert.equal(log.read().length, 1);
		assert.ok(log.isPaused("s1", 2000), "open window covers later calls");
		assert.ok(log.isPaused("s1/0-Explore", 2000));
		assert.ok(!log.isPaused("s10", 2000), "prefix must end at a path boundary");

		log.close(["s1"], 3000);
		assert.ok(log.isPaused("s1", 3000));
		assert.ok(!log.isPaused("s1", 999));
		assert.ok(!log.isPaused("s1", 3001));
		assert.equal(statSync(join(dir, "pauses.json")).mode & 0o777, 0o600);
	});

	it("only closes the given sessions, leaving other processes' pauses open", () => {
		const log = new PauseLog(dir);
		log.open("mine", 1000);
		log.open("theirs", 1000);
		log.close(["mine"], 2000);
		assert.deepEqual(log.read().map((w) => [w.session, w.to]), [["mine", 2000], ["theirs", null]]);
	});
});

describe("Reporter", () => {
	let dir: string;
	beforeEach(() => {
		dir = mkdtempSync(join(tmpdir(), "tm-test-"));
	});
	afterEach(() => rmSync(dir, { recursive: true, force: true }));

	const usage = () => toUsageEvent(assistantMessage, { id: "s", isSubagent: false })!;
	const respond = (status: number, body: unknown = {}) =>
		new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
	const tick = () => new Promise((r) => setTimeout(r, 20));

	it("queues usage events when the network fails, then flushes them in one batch", async () => {
		const calls: unknown[] = [];
		let online = false;
		const reporter = new Reporter({
			dataDir: dir,
			getConfig: () => ({ ingestUrl: "https://x.test/ingest", historyUrl: "https://x.test/history", apiKey: KEY }),
			fetchImpl: (async (_url: string, init: RequestInit) => {
				calls.push(JSON.parse(init.body as string));
				if (!online) throw new Error("offline");
				return respond(200, { accepted: 1 });
			}) as typeof fetch,
		});

		reporter.report(usage());
		reporter.report(usage());
		reporter.report({ kind: "heartbeat", session_id: "s", client_version: "0.2.0" });
		await tick();
		assert.equal(reporter.queueSize(), 2, "usage queued, heartbeat dropped");

		online = true;
		await reporter.flush(true);
		assert.equal(reporter.queueSize(), 0);
		const last = calls.at(-1) as { events: unknown[] };
		assert.equal(last.events.length, 2);
	});

	it("drops a single invalid event from a batch and retries the rest", async () => {
		const sent: number[] = [];
		const reporter = new Reporter({
			dataDir: dir,
			getConfig: () => ({ ingestUrl: "https://x.test/ingest", historyUrl: "https://x.test/history", apiKey: KEY }),
			fetchImpl: (async (_url: string, init: RequestInit) => {
				const body = JSON.parse(init.body as string);
				const n = body.events ? body.events.length : 1;
				sent.push(n);
				return n === 3 ? respond(400, { error: "invalid_event", index: 1 }) : respond(200);
			}) as typeof fetch,
		});
		assert.equal(await reporter.send([usage(), usage(), usage()]), "ok");
		assert.deepEqual(sent, [3, 2]);
	});

	it("queues without a key and reports unauthorized once", async () => {
		let unauthorized = 0;
		let key: string | undefined;
		const reporter = new Reporter({
			dataDir: dir,
			getConfig: () => ({ ingestUrl: "https://x.test/ingest", historyUrl: "https://x.test/history", apiKey: key }),
			fetchImpl: (async () => respond(401, { error: "invalid_key" })) as typeof fetch,
			onUnauthorized: () => unauthorized++,
		});
		reporter.report(usage());
		await tick();
		assert.equal(reporter.queueSize(), 1);

		key = KEY;
		reporter.report(usage());
		await tick();
		assert.equal(unauthorized, 1);
		assert.equal(reporter.queueSize(), 2, "rejected-key events are kept for after re-login");
		assert.ok(readFileSync(reporter.queuePath, "utf8").includes('"kind":"usage"'));
	});
});
