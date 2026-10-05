import { chmodSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

export const DEFAULT_INGEST_URL = "https://fywgkzqgtitpwijweojk.supabase.co/functions/v1/ingest";
export const KEY_PATTERN = /^tm_[A-Za-z0-9_-]{43}$/;

export interface Config {
	ingestUrl: string;
	/** Derived from ingestUrl: the history function lives next to ingest. */
	historyUrl: string;
	apiKey?: string;
}

export function historyUrlFor(ingestUrl: string): string {
	return ingestUrl.replace(/\/ingest\/?$/, "/history");
}

export function configPath(dataDir: string): string {
	return join(dataDir, "config.json");
}

/** Env vars win over the file so a key can be supplied without writing it to disk. */
export function loadConfig(dataDir: string, env: NodeJS.ProcessEnv = process.env): Config {
	const file = loadFile(dataDir);
	const ingestUrl = env.TOKENMUNCHERS_URL || file.ingestUrl || DEFAULT_INGEST_URL;
	return {
		ingestUrl,
		historyUrl: historyUrlFor(ingestUrl),
		apiKey: env.TOKENMUNCHERS_KEY || file.apiKey,
	};
}

/** What's on disk, ignoring env overrides. */
export function loadFile(dataDir: string): Partial<Pick<Config, "ingestUrl" | "apiKey">> {
	let file: Partial<Config> = {};
	const path = configPath(dataDir);
	if (existsSync(path)) {
		try {
			file = JSON.parse(readFileSync(path, "utf8")) as Partial<Config>;
		} catch {
			file = {};
		}
	}
	return file;
}

export function saveConfig(dataDir: string, config: Partial<Pick<Config, "ingestUrl" | "apiKey">>): void {
	const path = configPath(dataDir);
	mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
	writeFileSync(path, `${JSON.stringify(config, null, 2)}\n`, { mode: 0o600 });
	chmodSync(path, 0o600);
}
