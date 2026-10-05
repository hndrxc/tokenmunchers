// Per-install sync state, shared by every OMP process on this machine:
//
//   deviceId    random id for this install; history uploads are stored per device
//   createdAt   when 0.2.0+ first ran here (the pause log only covers time after it)
//   liveSince   callTime of the earliest call this install delivered live, or null.
//               History covers calls before it; live + gap fill cover calls after.
//   seeded      false for installs upgraded from 0.1.0 until liveSince has been
//               fetched from the server (0.1.0 didn't record it locally)
//   statsCursor highest stats.db message id the gap fill has looked at

import { randomUUID } from "node:crypto";
import { chmodSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

export interface SyncState {
	deviceId: string;
	createdAt: number;
	liveSince: number | null;
	seeded: boolean;
	statsCursor: number;
}

export function statePath(dataDir: string): string {
	return join(dataDir, "state.json");
}

function read(dataDir: string): SyncState | undefined {
	try {
		const s = JSON.parse(readFileSync(statePath(dataDir), "utf8")) as Partial<SyncState>;
		if (typeof s.deviceId !== "string" || typeof s.createdAt !== "number") return undefined;
		return {
			deviceId: s.deviceId,
			createdAt: s.createdAt,
			liveSince: typeof s.liveSince === "number" ? s.liveSince : null,
			seeded: s.seeded === true,
			statsCursor: typeof s.statsCursor === "number" ? s.statsCursor : 0,
		};
	} catch {
		return undefined;
	}
}

function write(dataDir: string, state: SyncState): void {
	try {
		mkdirSync(dataDir, { recursive: true, mode: 0o700 });
		writeFileSync(statePath(dataDir), `${JSON.stringify(state, null, 2)}\n`, { mode: 0o600 });
		chmodSync(statePath(dataDir), 0o600);
	} catch {
		// Disk trouble must never break the agent.
	}
}

/**
 * Loads the state, creating it on first run. `upgrading` marks an install that
 * already reported under 0.1.0 (a key was configured before this file existed),
 * whose live start must come from the server.
 */
export function loadState(dataDir: string, upgrading: boolean, now = Date.now()): SyncState {
	const existing = read(dataDir);
	if (existing) return existing;
	const fresh: SyncState = { deviceId: randomUUID(), createdAt: now, liveSince: null, seeded: !upgrading, statsCursor: 0 };
	if (!existsSync(statePath(dataDir))) write(dataDir, fresh);
	return read(dataDir) ?? fresh;
}

/** Read-modify-write against the latest file so concurrent processes don't clobber each other. */
export function updateState(dataDir: string, fn: (s: SyncState) => SyncState): SyncState {
	const next = fn(read(dataDir) ?? loadState(dataDir, false));
	write(dataDir, next);
	return next;
}

/** liveSince only ever moves earlier. */
export function noteLive(dataDir: string, callTimeMs: number): SyncState {
	return updateState(dataDir, (s) =>
		s.liveSince !== null && s.liveSince <= callTimeMs ? s : { ...s, liveSince: callTimeMs },
	);
}
