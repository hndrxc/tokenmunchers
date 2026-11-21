import type { Harness } from '../../shared/src/observations.ts';
import type { PluginConfig } from './config.ts';

export const DEFAULT_ENDPOINT = 'https://fywgkzqgtitpwijweojk.supabase.co/functions/v1/contributions';

export interface CompactTotals {
  input_tokens: number | null;
  output_tokens: number | null;
  cache_read_tokens: number | null;
  cache_write_tokens: number | null;
  call_count: number | null;
  cost_usd: number | null;
}

export interface CompactStatus {
  harness: Harness;
  root: string;
  device_id: string;
  cutoff: string;
  paused: boolean;
  recurring_upload: boolean;
  endpoint: string;
  range_utc: { first: string | null; last: string | null };
  retained_contributions: number;
  live: number;
  history: number;
  excluded: number;
  totals: CompactTotals;
  qualities: Record<string, number>;
  unknown: Record<string, number>;
  weak_identity_count: number;
  exclusions: string[];
  diagnostics: Record<string, number>;
  progress: {
    scans: number;
    observations: number;
    at: string | null;
  };
  pending_snapshot: string | null;
  pending_acknowledged: { requests: number; chunk_count: number } | null;
  delivery?: Record<string, unknown>;
  scan?: {
    duplicates: number;
    excluded: number;
    rejected: number;
  };
}

export function formatCompactStatus(
  preview: Record<string, unknown>,
  config: PluginConfig,
  extra?: { delivery?: Record<string, unknown>; endpoint?: string }
): CompactStatus {
  const days = Array.isArray(preview.days) ? preview.days : [];
  const previewUnknown = (preview.unknown !== null && typeof preview.unknown === 'object' && !Array.isArray(preview.unknown))
    ? preview.unknown as Record<string, unknown>
    : {};

  const fields = ['input_tokens', 'output_tokens', 'cache_read_tokens', 'cache_write_tokens', 'call_count', 'cost_usd'] as const;

  const totals: CompactTotals = {
    input_tokens: 0,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
    call_count: 0,
    cost_usd: 0,
  };

  for (const field of fields) {
    let sum = 0;
    let fieldUnknown = false;

    for (const day of days) {
      if (day !== null && typeof day === 'object' && !Array.isArray(day)) {
        const dayRecord = day as Record<string, unknown>;
        const dayUnknown = (dayRecord.unknown !== null && typeof dayRecord.unknown === 'object' && !Array.isArray(dayRecord.unknown))
          ? dayRecord.unknown as Record<string, unknown>
          : null;
        const subRecord = (dayRecord.known_subtotals !== null && typeof dayRecord.known_subtotals === 'object' && !Array.isArray(dayRecord.known_subtotals))
          ? dayRecord.known_subtotals as Record<string, unknown>
          : null;

        if (dayRecord[field] === null) {
          fieldUnknown = true;
        } else if (dayUnknown && typeof dayUnknown[field] === 'number' && dayUnknown[field] > 0) {
          fieldUnknown = true;
        } else if (!(field in dayRecord) && !dayUnknown) {
          if (typeof previewUnknown[field] === 'number' && previewUnknown[field] > 0) {
            fieldUnknown = true;
          }
        }

        const val = subRecord && typeof subRecord[field] === 'number'
          ? subRecord[field] as number
          : (typeof dayRecord[field] === 'number' ? dayRecord[field] as number : 0);

        if (field === 'cost_usd') {
          sum = Math.round((sum + val) * 1e6) / 1e6;
        } else {
          sum += val;
        }
      }
    }

    if (days.length > 0 && fieldUnknown) {
      totals[field] = null;
    } else {
      totals[field] = sum;
    }
  }

  const range = (preview.range_utc !== null && typeof preview.range_utc === 'object' && !Array.isArray(preview.range_utc))
    ? preview.range_utc as Record<string, unknown>
    : {};

  const rangeUtc = {
    first: typeof range.first === 'string' ? range.first : null,
    last: typeof range.last === 'string' ? range.last : null,
  };

  const qualities = (preview.qualities !== null && typeof preview.qualities === 'object' && !Array.isArray(preview.qualities))
    ? preview.qualities as Record<string, number>
    : {};

  const unknown = (preview.unknown !== null && typeof preview.unknown === 'object' && !Array.isArray(preview.unknown))
    ? preview.unknown as Record<string, number>
    : {};

  const rawProgress = (preview.progress !== null && typeof preview.progress === 'object' && !Array.isArray(preview.progress))
    ? preview.progress as Record<string, unknown>
    : {};

  const diagnosticsMap = (rawProgress.diagnostics !== null && typeof rawProgress.diagnostics === 'object' && !Array.isArray(rawProgress.diagnostics))
    ? rawProgress.diagnostics as Record<string, number>
    : {};

  const exclusions = Array.isArray(preview.exclusions)
    ? preview.exclusions.filter((item): item is string => typeof item === 'string')
    : [];

  const rawAck = preview.pending_acknowledged;
  let acknowledged: { requests: number; chunk_count: number } | null = null;
  if (rawAck !== null && typeof rawAck === 'object' && !Array.isArray(rawAck) && 'requests' in rawAck && 'chunk_count' in rawAck) {
    const rawAckRecord = rawAck as Record<string, unknown>;
    if (typeof rawAckRecord.requests === 'number' && typeof rawAckRecord.chunk_count === 'number') {
      acknowledged = { requests: rawAckRecord.requests, chunk_count: rawAckRecord.chunk_count };
    }
  }

  const status: CompactStatus = {
    harness: (preview.harness as Harness) ?? 'claude-code',
    root: typeof preview.root === 'string' ? preview.root : '',
    device_id: typeof preview.device_id === 'string' ? preview.device_id : '',
    cutoff: typeof preview.cutoff === 'string' ? preview.cutoff : '',
    paused: preview.paused === true,
    recurring_upload: config.auto_upload === true,
    endpoint: extra?.endpoint ?? config.endpoint ?? (typeof preview.endpoint === 'string' ? preview.endpoint : DEFAULT_ENDPOINT),
    range_utc: rangeUtc,
    retained_contributions: typeof preview.retained_contributions === 'number' ? preview.retained_contributions : 0,
    live: typeof preview.live === 'number' ? preview.live : 0,
    history: typeof preview.history === 'number' ? preview.history : 0,
    excluded: typeof preview.excluded === 'number' ? preview.excluded : 0,
    totals,
    qualities,
    unknown,
    weak_identity_count: typeof preview.weak_identity_count === 'number' ? preview.weak_identity_count : 0,
    exclusions,
    diagnostics: diagnosticsMap,
    progress: {
      scans: typeof rawProgress.scans === 'number' ? rawProgress.scans : 0,
      observations: typeof rawProgress.observations === 'number' ? rawProgress.observations : 0,
      at: typeof rawProgress.at === 'string' ? rawProgress.at : null,
    },
    pending_snapshot: typeof preview.pending_snapshot === 'string' ? preview.pending_snapshot : null,
    pending_acknowledged: acknowledged,
  };

  if (extra?.delivery) {
    status.delivery = extra.delivery;
  }

  if (preview.latest_scan !== null && typeof preview.latest_scan === 'object' && !Array.isArray(preview.latest_scan)) {
    const scanRecord = preview.latest_scan as Record<string, unknown>;
    status.scan = {
      duplicates: typeof scanRecord.duplicates === 'number' ? scanRecord.duplicates : 0,
      excluded: typeof scanRecord.excluded === 'number' ? scanRecord.excluded : 0,
      rejected: typeof scanRecord.rejected === 'number' ? scanRecord.rejected : 0,
    };
  }

  return status;
}
