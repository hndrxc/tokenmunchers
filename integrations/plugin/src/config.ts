import { chmod, mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import type { Harness } from '../../shared/src/observations.ts';

export interface PluginConfig {
  auto_upload: boolean;
  endpoint?: string;
  updated_at?: string;
  last_delivered_revisions?: Record<string, string>;
}

export function pluginConfigPath(stateDir: string, harness: Harness): string {
  return join(stateDir, `${harness}-plugin.json`);
}

export function validateEndpoint(endpoint: unknown): string {
  if (typeof endpoint !== 'string' || endpoint.trim().length === 0) {
    throw new Error('Endpoint must be a non-empty string URL');
  }
  let url: URL;
  try {
    url = new URL(endpoint);
  } catch {
    throw new Error(`Invalid endpoint URL: ${endpoint}`);
  }
  if (url.username || url.password) {
    throw new Error('Endpoint URL must not contain credentials (username or password)');
  }
  const isLoopback = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname);
  if (url.protocol !== 'https:' && !(url.protocol === 'http:' && isLoopback)) {
    throw new Error('Endpoint requires HTTPS (HTTP allowed for localhost smoke tests only)');
  }
  return url.toString();
}

export function getLastDeliveredRevision(config: PluginConfig, endpoint: string): string | undefined {
  return config.last_delivered_revisions?.[endpoint];
}

export function updateDeliveredRevision(config: PluginConfig, endpoint: string, revision: string): PluginConfig {
  return {
    ...config,
    last_delivered_revisions: {
      ...(config.last_delivered_revisions ?? {}),
      [endpoint]: revision,
    },
    updated_at: new Date().toISOString(),
  };
}

function parseConfig(value: unknown): PluginConfig {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    return { auto_upload: false };
  }
  const record = value as Record<string, unknown>;
  const autoUpload = record.auto_upload === true;
  let endpoint: string | undefined;
  if (typeof record.endpoint === 'string' && record.endpoint.length > 0) {
    validateEndpoint(record.endpoint);
    endpoint = record.endpoint;
  }
  const updatedAt = typeof record.updated_at === 'string' ? record.updated_at : undefined;

  let lastDeliveredRevisions: Record<string, string> | undefined;
  if (record.last_delivered_revisions !== null && typeof record.last_delivered_revisions === 'object' && !Array.isArray(record.last_delivered_revisions)) {
    lastDeliveredRevisions = {};
    for (const [k, v] of Object.entries(record.last_delivered_revisions as Record<string, unknown>)) {
      if (typeof v === 'string' && v.length > 0) {
        lastDeliveredRevisions[k] = v;
      }
    }
    if (Object.keys(lastDeliveredRevisions).length === 0) {
      lastDeliveredRevisions = undefined;
    }
  }

  return {
    auto_upload: autoUpload,
    ...(endpoint ? { endpoint } : {}),
    ...(updatedAt ? { updated_at: updatedAt } : {}),
    ...(lastDeliveredRevisions ? { last_delivered_revisions: lastDeliveredRevisions } : {}),
  };
}


export async function readPluginConfig(stateDir: string, harness: Harness): Promise<PluginConfig> {
  const path = pluginConfigPath(stateDir, harness);
  let raw: string;
  try {
    raw = await readFile(path, 'utf8');
  } catch (error: unknown) {
    if (error instanceof Error && 'code' in error && error.code === 'ENOENT') {
      return { auto_upload: false };
    }
    throw error;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new Error(`Invalid plugin configuration file at ${path}; remove or repair JSON configuration.`);
  }
  try {
    return parseConfig(parsed);
  } catch {
    throw new Error(`Invalid plugin configuration file at ${path}; remove or repair JSON configuration.`);
  }
}

export async function writePluginConfig(stateDir: string, harness: Harness, config: PluginConfig): Promise<void> {
  if (config.endpoint !== undefined) {
    validateEndpoint(config.endpoint);
  }
  await mkdir(stateDir, { recursive: true, mode: 0o700 });
  await chmod(stateDir, 0o700).catch(() => {});
  const targetPath = pluginConfigPath(stateDir, harness);
  const tempPath = join(stateDir, `.${harness}-plugin.json.tmp.${randomUUID()}`);
  const payload: PluginConfig = {
    auto_upload: config.auto_upload === true,
    ...(config.endpoint ? { endpoint: config.endpoint } : {}),
    updated_at: config.updated_at ?? new Date().toISOString(),
    ...(config.last_delivered_revisions ? { last_delivered_revisions: config.last_delivered_revisions } : {}),
  };
  await writeFile(tempPath, JSON.stringify(payload, null, 2) + '\n', { mode: 0o600 });
  await chmod(tempPath, 0o600).catch(() => {});
  await rename(tempPath, targetPath);
}
