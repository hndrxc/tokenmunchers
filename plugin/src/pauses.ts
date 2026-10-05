// Local record of when reporting was paused, per session. The live path just
// stops sending; this log exists so a later backfill from omp-stats (which sees
// every call in the transcripts) can skip what the user chose not to share.
// It never leaves the machine.

import { chmodSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

export interface PauseWindow {
	/** Session key from sessionKeyFromFile; subagent keys extend it as `<key>/…`. */
	session: string;
	from: number;
	/** null while the pause is still open. */
	to: number | null;
}

export class PauseLog {
	readonly #path: string;
	readonly #dataDir: string;

	constructor(dataDir: string) {
		this.#dataDir = dataDir;
		this.#path = join(dataDir, "pauses.json");
	}

	read(): PauseWindow[] {
		try {
			const parsed = JSON.parse(readFileSync(this.#path, "utf8")) as unknown;
			return Array.isArray(parsed) ? (parsed as PauseWindow[]) : [];
		} catch {
			return [];
		}
	}

	/** Opens a window for a session unless one is already open. */
	open(session: string, now = Date.now()): void {
		const windows = this.read();
		if (windows.some((w) => w.session === session && w.to === null)) return;
		windows.push({ session, from: now, to: null });
		this.#write(windows);
	}

	/**
	 * Closes the open windows for these sessions (resume, or the process going
	 * away). Other OMP processes share the file, so only our own are touched.
	 */
	close(sessions: Iterable<string>, now = Date.now()): void {
		const keys = new Set(sessions);
		const windows = this.read();
		if (!windows.some((w) => w.to === null && keys.has(w.session))) return;
		this.#write(windows.map((w) => (w.to === null && keys.has(w.session) ? { ...w, to: now } : w)));
	}

	/** True if a call at `ts` in `sessionKey` (or a subagent under it) fell in a pause. */
	isPaused(sessionKey: string, ts: number, windows: PauseWindow[] = this.read()): boolean {
		return windows.some(
			(w) =>
				(sessionKey === w.session || sessionKey.startsWith(`${w.session}/`)) &&
				ts >= w.from &&
				(w.to === null || ts <= w.to),
		);
	}

	#write(windows: PauseWindow[]): void {
		try {
			mkdirSync(this.#dataDir, { recursive: true, mode: 0o700 });
			writeFileSync(this.#path, `${JSON.stringify(windows)}\n`, { mode: 0o600 });
			chmodSync(this.#path, 0o600);
		} catch {
			// Disk trouble must never break the agent.
		}
	}
}
