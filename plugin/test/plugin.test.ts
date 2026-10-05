import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";

import { toUsageEvent } from "../src/adapter.ts";
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
};

describe("toUsageEvent", () => {
	it("maps OMP usage to the event payload", () => {
		const e = toUsageEvent(assistantMessage, "sess-1", false, new Date("2026-10-04T12:00:00Z"));
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
		const e = toUsageEvent(assistantMessage, "sess-1", true);
		assert.ok(!JSON.stringify(e).includes("SECRET"));
		assert.deepEqual(Object.keys(e!).sort(), [
			"cache_read_tokens", "cache_write_tokens", "client_version", "cost_usd", "event_id", "input_tokens",
			"is_subagent", "kind", "model", "output_tokens", "provider", "session_id", "ts",
		]);
	});

	it("ignores non-assistant messages and zero-usage calls", () => {
		assert.equal(toUsageEvent({ role: "user", content: "hi" }, "s", false), null);
		assert.equal(toUsageEvent({ role: "toolResult" }, "s", false), null);
		assert.equal(toUsageEvent({ ...assistantMessage, usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }, "s", false), null);
		assert.equal(toUsageEvent(null, "s", false), null);
	});

	it("tolerates missing or junk usage fields", () => {
		const e = toUsageEvent({ role: "assistant", usage: { input: 10, output: Number.NaN, cacheRead: -5 } }, "s", false);
		assert.ok(e);
		assert.equal(e.provider, "unknown");
		assert.equal(e.output_tokens, 0);
		assert.equal(e.cache_read_tokens, 0);
		assert.equal(e.cost_usd, 0);
	});
});

describe("Reporter", () => {
	let dir: string;
	beforeEach(() => {
		dir = mkdtempSync(join(tmpdir(), "tm-test-"));
	});
	afterEach(() => rmSync(dir, { recursive: true, force: true }));

	const usage = () => toUsageEvent(assistantMessage, "s", false)!;
	const respond = (status: number, body: unknown = {}) =>
		new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
	const tick = () => new Promise((r) => setTimeout(r, 20));

	it("queues usage events when the network fails, then flushes them in one batch", async () => {
		const calls: unknown[] = [];
		let online = false;
		const reporter = new Reporter({
			dataDir: dir,
			getConfig: () => ({ ingestUrl: "https://x.test/ingest", apiKey: KEY }),
			fetchImpl: (async (_url: string, init: RequestInit) => {
				calls.push(JSON.parse(init.body as string));
				if (!online) throw new Error("offline");
				return respond(200, { accepted: 1 });
			}) as typeof fetch,
		});

		reporter.report(usage());
		reporter.report(usage());
		reporter.report({ kind: "heartbeat", session_id: "s", client_version: "0.1.0" });
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
			getConfig: () => ({ ingestUrl: "https://x.test/ingest", apiKey: KEY }),
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
			getConfig: () => ({ ingestUrl: "https://x.test/ingest", apiKey: key }),
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
