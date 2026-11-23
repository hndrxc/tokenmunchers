import assert from "node:assert/strict";
import { appendFileSync, existsSync, mkdtempSync, readdirSync, rmSync, statSync, utimesSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";

import type { UsageEvent } from "../src/adapter.ts";
import { drainExternalUsage, EXTERNAL_FILE, externalLineToEvent, externalPending } from "../src/external.ts";
import { PauseLog } from "../src/pauses.ts";

const NOW = Date.parse("2026-11-20T12:00:00Z");
const noPauses = { isPaused: () => false };
const MAIN_FILE = "/home/u/.omp/agent/sessions/-proj/2026-11-20T11-00-00-000Z_sess-1.jsonl";
const SUB_FILE = "/home/u/.omp/agent/sessions/-proj/2026-11-20T11-00-00-000Z_sess-1/0-Builder.jsonl";
const parentExists = (path: string) => path === SUB_FILE.replace(/\/0-Builder\.jsonl$/, ".jsonl");

const line = (over: Record<string, unknown> = {}) => ({
	v: 1,
	source: "agy",
	id: "6322b946-3d6e-4a56-89e4-ea1206abacbf",
	ts: NOW - 60_000,
	provider: "google-antigravity",
	model: "gemini-4-hydrogen-high",
	input_tokens: 10261,
	output_tokens: 74,
	cache_read_tokens: 2048,
	...over,
});

const eventOf = (r: ReturnType<typeof externalLineToEvent>): UsageEvent => {
	assert.ok("event" in r, `expected an event, got ${JSON.stringify(r)}`);
	return r.event;
};

describe("externalLineToEvent", () => {
	it("maps a line to a metadata-only usage event", () => {
		const e = eventOf(externalLineToEvent(line({ session_id: "sess-1", session_file: MAIN_FILE }), noPauses, NOW));
		assert.equal(e.kind, "usage");
		assert.equal(e.provider, "google-antigravity");
		assert.equal(e.model, "gemini-4-hydrogen-high");
		assert.equal(e.input_tokens, 10261);
		assert.equal(e.output_tokens, 74);
		assert.equal(e.cache_read_tokens, 2048);
		assert.equal(e.cache_write_tokens, 0);
		assert.equal(e.cost_usd, 0);
		assert.equal(e.session_id, "sess-1");
		assert.equal(e.is_subagent, false);
		assert.equal(e.ts, new Date(NOW - 60_000).toISOString());
		assert.match(e.event_id, /^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
		assert.deepEqual(Object.keys(e).sort(), [
			"cache_read_tokens", "cache_write_tokens", "client_version", "cost_usd", "event_id", "input_tokens",
			"is_subagent", "kind", "model", "output_tokens", "provider", "session_id", "ts",
		]);
	});

	it("never uploads the session file path", () => {
		const e = eventOf(externalLineToEvent(line({ session_file: MAIN_FILE }), noPauses, NOW));
		const json = JSON.stringify(e);
		assert.ok(!json.includes("/home/u"));
		assert.ok(!json.includes("-proj"));
	});

	it("gives the same call the same event id, and different calls different ids", () => {
		const a = eventOf(externalLineToEvent(line(), noPauses, NOW));
		const b = eventOf(externalLineToEvent(line({ input_tokens: 1 }), noPauses, NOW));
		const c = eventOf(externalLineToEvent(line({ id: "another-call" }), noPauses, NOW));
		const d = eventOf(externalLineToEvent(line({ source: "other-tool" }), noPauses, NOW));
		assert.equal(a.event_id, b.event_id);
		assert.notEqual(a.event_id, c.event_id);
		assert.notEqual(a.event_id, d.event_id);
	});

	it("marks calls from a subagent session as subagent calls", () => {
		const e = eventOf(externalLineToEvent(line({ session_file: SUB_FILE }), noPauses, NOW, parentExists));
		assert.equal(e.is_subagent, true);
		assert.equal(e.session_id, "2026-11-20T11-00-00-000Z_sess-1/0-Builder");
	});

	it("falls back to a per-tool session id when no session is known", () => {
		assert.equal(eventOf(externalLineToEvent(line(), noPauses, NOW)).session_id, "external-agy");
	});

	it("accepts ISO timestamps", () => {
		const e = eventOf(externalLineToEvent(line({ ts: "2026-11-20T11:59:00Z" }), noPauses, NOW));
		assert.equal(e.ts, "2026-11-20T11:59:00.000Z");
	});

	it("rejects malformed lines", () => {
		for (const bad of [
			null,
			[],
			"text",
			line({ v: 2 }),
			line({ source: "Has Spaces" }),
			line({ source: "x".repeat(40) }),
			line({ id: "" }),
			line({ id: "x".repeat(129) }),
			line({ provider: undefined }),
			line({ model: 42 }),
			line({ ts: "not a date" }),
		]) {
			assert.deepEqual(externalLineToEvent(bad, noPauses, NOW), { skipped: "invalid" }, JSON.stringify(bad));
		}
	});

	it("skips calls the server would reject for age", () => {
		assert.deepEqual(externalLineToEvent(line({ ts: NOW - 7 * 86_400_000 }), noPauses, NOW), { skipped: "stale" });
		assert.deepEqual(externalLineToEvent(line({ ts: NOW + 10 * 60_000 }), noPauses, NOW), { skipped: "stale" });
	});

	it("skips calls with no tokens", () => {
		const empty = line({ input_tokens: 0, output_tokens: 0, cache_read_tokens: 0 });
		assert.deepEqual(externalLineToEvent(empty, noPauses, NOW), { skipped: "empty" });
	});

	it("clamps counters and ignores negative or non-numeric ones", () => {
		const e = eventOf(externalLineToEvent(line({ input_tokens: 5e12, output_tokens: -3, cache_read_tokens: "9" }), noPauses, NOW));
		assert.equal(e.input_tokens, 1e9);
		assert.equal(e.output_tokens, 0);
		assert.equal(e.cache_read_tokens, 0);
	});
});

describe("external usage and pauses", () => {
	let dir: string;
	beforeEach(() => {
		dir = mkdtempSync(join(tmpdir(), "tm-ext-pause-"));
	});
	afterEach(() => rmSync(dir, { recursive: true, force: true }));

	it("skips calls made while their session was paused, including its subagents", () => {
		const pauses = new PauseLog(dir);
		pauses.open("2026-11-20T11-00-00-000Z_sess-1", NOW - 120_000);
		const main = externalLineToEvent(line({ session_file: MAIN_FILE }), pauses, NOW);
		const sub = externalLineToEvent(line({ session_file: SUB_FILE }), pauses, NOW, parentExists);
		assert.deepEqual(main, { skipped: "paused" });
		assert.deepEqual(sub, { skipped: "paused" });
	});

	it("reports calls from other sessions", () => {
		const pauses = new PauseLog(dir);
		pauses.open("2026-11-20T11-00-00-000Z_other", NOW - 120_000);
		assert.ok("event" in externalLineToEvent(line({ session_file: MAIN_FILE }), pauses, NOW));
	});
});

describe("drainExternalUsage", () => {
	let dir: string;
	beforeEach(() => {
		dir = mkdtempSync(join(tmpdir(), "tm-ext-"));
	});
	afterEach(() => rmSync(dir, { recursive: true, force: true }));

	const write = (lines: unknown[], name = EXTERNAL_FILE) =>
		writeFileSync(join(dir, name), lines.map((l) => (typeof l === "string" ? l : JSON.stringify(l))).join("\n") + "\n");

	it("delivers valid lines once, counts the rest as skipped, and removes the file", () => {
		write([line(), line(), "not json", line({ v: 9 }), line({ id: "second" })]);
		assert.equal(externalPending(dir), 5);
		const delivered: UsageEvent[] = [];
		const result = drainExternalUsage({ dataDir: dir, pauses: noPauses, deliver: (e) => delivered.push(...e), now: NOW });
		assert.equal(delivered.length, 2);
		assert.equal(result.skipped, 3);
		assert.equal(externalPending(dir), 0);
		assert.deepEqual(readdirSync(dir), []);
	});

	it("does nothing when there is no file", () => {
		let calls = 0;
		const result = drainExternalUsage({ dataDir: dir, pauses: noPauses, deliver: () => calls++, now: NOW });
		assert.deepEqual(result, { events: [], skipped: 0 });
		assert.equal(calls, 0);
	});

	it("lets writers keep appending while a drain holds the claimed file", () => {
		write([line()]);
		drainExternalUsage({
			dataDir: dir,
			pauses: noPauses,
			now: NOW,
			deliver: () => appendFileSync(join(dir, EXTERNAL_FILE), `${JSON.stringify(line({ id: "during" }))}\n`),
		});
		assert.equal(externalPending(dir), 1, "the line written mid-drain waits for the next drain");
	});

	it("marks a claimed file as fresh even if it was last written long ago", () => {
		write([line({ id: "old-write" })]);
		const old = (NOW - 30 * 60_000) / 1000;
		utimesSync(join(dir, EXTERNAL_FILE), old, old);
		let seenMtime = 0;
		drainExternalUsage({
			dataDir: dir,
			pauses: noPauses,
			now: NOW,
			deliver: () => {
				const claimed = readdirSync(dir).find((n) => n.endsWith(".draining"))!;
				seenMtime = statSync(join(dir, claimed)).mtimeMs;
			},
		});
		assert.ok(Math.abs(seenMtime - NOW) < 2_000, "claimed file's mtime is the claim time");
	});

	it("adopts a crashed drain's file but leaves another process's fresh one alone", () => {
		write([line({ id: "crashed" })], "external-usage.111.1.draining");
		write([line({ id: "in-flight" })], "external-usage.222.2.draining");
		const old = (NOW - 10 * 60_000) / 1000;
		utimesSync(join(dir, "external-usage.111.1.draining"), old, old);
		const fresh = NOW / 1000;
		utimesSync(join(dir, "external-usage.222.2.draining"), fresh, fresh);

		const delivered: UsageEvent[] = [];
		drainExternalUsage({ dataDir: dir, pauses: noPauses, deliver: (e) => delivered.push(...e), now: NOW });
		assert.equal(delivered.length, 1);
		assert.ok(!existsSync(join(dir, "external-usage.111.1.draining")));
		assert.ok(existsSync(join(dir, "external-usage.222.2.draining")));
	});
});
