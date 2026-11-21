import test from 'node:test';
import assert from 'node:assert/strict';
import { formatCompactStatus, DEFAULT_ENDPOINT } from '../src/format.ts';
import type { PluginConfig } from '../src/config.ts';

const baseConfig: PluginConfig = {
  auto_upload: false,
};

test('parent actual Codex fixture preview cost_usd null unknown 3 replaces masquerading zero', () => {
  // Codex 0.154.0 fixture produces 3 request observations with unknown cost and unknown provider
  // Aggregate sets day.cost_usd = null with known_subtotals.cost_usd = 0 and unknown.cost_usd = 3
  const codexPreview = {
    harness: 'codex',
    root: '/tmp/test/codex',
    device_id: 'device-codex-1',
    cutoff: '2026-10-01T00:00:00.000Z',
    retained_contributions: 3,
    live: 3,
    history: 0,
    excluded: 0,
    days: [
      {
        day: '2026-10-15',
        harness: 'codex',
        provider: 'unknown',
        model: 'fixture-model',
        is_subagent: null,
        input_tokens: 230,
        output_tokens: 0,
        cache_read_tokens: 0,
        cache_write_tokens: 0,
        call_count: 3,
        cost_usd: null,
        known_subtotals: {
          input_tokens: 230,
          output_tokens: 0,
          cache_read_tokens: 0,
          cache_write_tokens: 0,
          call_count: 3,
          cost_usd: 0,
        },
        qualities: ['partial'],
        cost_bases: ['unknown'],
      },
    ],
    unknown: {
      input_tokens: 0,
      output_tokens: 0,
      cache_read_tokens: 0,
      cache_write_tokens: 0,
      call_count: 0,
      cost_usd: 3,
      provider: 3,
      timestamp: 0,
      is_subagent: 3,
    },
    qualities: { partial: 3 },
  };

  const status = formatCompactStatus(codexPreview, baseConfig);

  // Unknown cost must not masquerade as 0
  assert.equal(status.totals.cost_usd, null, 'Codex totals.cost_usd must be null, not 0');
  // Genuine known accounting fields must be preserved
  assert.equal(status.totals.input_tokens, 230);
  assert.equal(status.totals.output_tokens, 0);
  assert.equal(status.totals.cache_read_tokens, 0);
  assert.equal(status.totals.cache_write_tokens, 0);
  assert.equal(status.totals.call_count, 3);
  assert.equal(status.unknown.cost_usd, 3);
});

test('unverified Claude streaming output stays null while preserving known tokens', () => {
  // Claude streaming fixture produces assistant response with unverified placeholder output
  // Aggregate sets day.output_tokens = null and day.cost_usd = null
  const claudePreview = {
    harness: 'claude-code',
    root: '/tmp/test/claude',
    device_id: 'device-claude-1',
    cutoff: '2026-10-01T00:00:00.000Z',
    retained_contributions: 1,
    live: 1,
    history: 0,
    excluded: 0,
    days: [
      {
        day: '2026-10-20',
        harness: 'claude-code',
        provider: 'unknown',
        model: 'claude-3-5-sonnet',
        is_subagent: null,
        input_tokens: 100,
        output_tokens: null,
        cache_read_tokens: 40,
        cache_write_tokens: 60,
        call_count: 1,
        cost_usd: null,
        known_subtotals: {
          input_tokens: 100,
          output_tokens: 0,
          cache_read_tokens: 40,
          cache_write_tokens: 60,
          call_count: 1,
          cost_usd: 0,
        },
        qualities: ['partial'],
        cost_bases: ['unknown'],
      },
    ],
    unknown: {
      input_tokens: 0,
      output_tokens: 1,
      cache_read_tokens: 0,
      cache_write_tokens: 0,
      call_count: 0,
      cost_usd: 1,
      provider: 1,
      timestamp: 0,
      is_subagent: 1,
    },
    qualities: { partial: 1 },
  };

  const status = formatCompactStatus(claudePreview, baseConfig);

  assert.equal(status.totals.output_tokens, null, 'unverified output must stay null, not 0');
  assert.equal(status.totals.cost_usd, null, 'unreported cost must stay null');
  assert.equal(status.totals.input_tokens, 100);
  assert.equal(status.totals.cache_read_tokens, 40);
  assert.equal(status.totals.cache_write_tokens, 60);
  assert.equal(status.totals.call_count, 1);
});

test('mixed known and unknown cost across days stays null without losing complete token totals', () => {
  // Day 1: fully known cost $0.05, 100 input tokens
  // Day 2: unknown cost (null), 50 input tokens
  const mixedPreview = {
    harness: 'claude-code',
    root: '/tmp/test/claude',
    device_id: 'device-claude-mixed',
    cutoff: '2026-10-01T00:00:00.000Z',
    retained_contributions: 2,
    live: 2,
    history: 0,
    excluded: 0,
    days: [
      {
        day: '2026-10-10',
        harness: 'claude-code',
        provider: 'anthropic',
        model: 'claude-3-5-sonnet',
        is_subagent: false,
        input_tokens: 100,
        output_tokens: 20,
        cache_read_tokens: 10,
        cache_write_tokens: 5,
        call_count: 1,
        cost_usd: 0.05,
        known_subtotals: {
          input_tokens: 100,
          output_tokens: 20,
          cache_read_tokens: 10,
          cache_write_tokens: 5,
          call_count: 1,
          cost_usd: 0.05,
        },
        qualities: ['observed'],
        cost_bases: ['reported'],
      },
      {
        day: '2026-10-11',
        harness: 'claude-code',
        provider: 'anthropic',
        model: 'claude-3-5-sonnet',
        is_subagent: false,
        input_tokens: 50,
        output_tokens: 10,
        cache_read_tokens: 0,
        cache_write_tokens: 0,
        call_count: 1,
        cost_usd: null,
        known_subtotals: {
          input_tokens: 50,
          output_tokens: 10,
          cache_read_tokens: 0,
          cache_write_tokens: 0,
          call_count: 1,
          cost_usd: 0,
        },
        qualities: ['partial'],
        cost_bases: ['unknown'],
      },
    ],
    unknown: {
      cost_usd: 1,
    },
  };

  const status = formatCompactStatus(mixedPreview, baseConfig);

  // Mixed known + unknown cost must be null, not $0.05 subtotal
  assert.equal(status.totals.cost_usd, null, 'mixed cost must stay null across days');
  // Tokens have complete coverage across both days and must sum accurately
  assert.equal(status.totals.input_tokens, 150);
  assert.equal(status.totals.output_tokens, 30);
  assert.equal(status.totals.cache_read_tokens, 10);
  assert.equal(status.totals.cache_write_tokens, 5);
  assert.equal(status.totals.call_count, 2);
});

test('real measured zeros stay 0 and are never converted to null', () => {
  const completeZeroPreview = {
    harness: 'codex',
    root: '/tmp/test/codex',
    device_id: 'device-zero',
    cutoff: '2026-10-01T00:00:00.000Z',
    retained_contributions: 1,
    live: 1,
    history: 0,
    excluded: 0,
    days: [
      {
        day: '2026-10-15',
        harness: 'codex',
        provider: 'openai',
        model: 'test-model',
        is_subagent: false,
        input_tokens: 0,
        output_tokens: 0,
        cache_read_tokens: 0,
        cache_write_tokens: 0,
        call_count: 1,
        cost_usd: 0,
        known_subtotals: {
          input_tokens: 0,
          output_tokens: 0,
          cache_read_tokens: 0,
          cache_write_tokens: 0,
          call_count: 1,
          cost_usd: 0,
        },
        qualities: ['observed'],
        cost_bases: ['reported'],
      },
    ],
    unknown: {
      input_tokens: 0,
      output_tokens: 0,
      cache_read_tokens: 0,
      cache_write_tokens: 0,
      call_count: 0,
      cost_usd: 0,
    },
  };

  const status = formatCompactStatus(completeZeroPreview, baseConfig);

  assert.equal(status.totals.cost_usd, 0, 'measured zero cost must stay 0');
  assert.equal(status.totals.input_tokens, 0, 'measured zero input must stay 0');
  assert.equal(status.totals.output_tokens, 0, 'measured zero output must stay 0');
  assert.equal(status.totals.cache_read_tokens, 0);
  assert.equal(status.totals.cache_write_tokens, 0);
  assert.equal(status.totals.call_count, 1);
});

test('nonpublished session summary must not spuriously null unrelated true accounting fields', () => {
  // Scenario: 1 published request day with full known cost ($0.12)
  // Plus 1 nonpublished session summary observation with cost_usd: null in preview.unknown
  const previewWithWithheldSummary = {
    harness: 'claude-code',
    root: '/tmp/test/claude',
    device_id: 'device-summary-check',
    cutoff: '2026-10-01T00:00:00.000Z',
    retained_contributions: 2,
    live: 1,
    history: 0,
    excluded: 0,
    days: [
      {
        day: '2026-10-20',
        harness: 'claude-code',
        provider: 'anthropic',
        model: 'claude-3-5-sonnet',
        is_subagent: false,
        input_tokens: 200,
        output_tokens: 50,
        cache_read_tokens: 20,
        cache_write_tokens: 10,
        call_count: 1,
        cost_usd: 0.12,
        known_subtotals: {
          input_tokens: 200,
          output_tokens: 50,
          cache_read_tokens: 20,
          cache_write_tokens: 10,
          call_count: 1,
          cost_usd: 0.12,
        },
        qualities: ['observed'],
        cost_bases: ['reported'],
      },
    ],
    withheld: {
      NONADDITIVE_SUMMARY: 1,
    },
    // The session summary contributed 1 to preview.unknown.cost_usd, but is nonpublished
    unknown: {
      cost_usd: 1,
    },
  };

  const status = formatCompactStatus(previewWithWithheldSummary, baseConfig);

  // The published day has known cost $0.12; nonpublished summary must not spuriously null it
  assert.equal(status.totals.cost_usd, 0.12, 'published known cost must not be nulled by withheld summary');
  assert.equal(status.totals.input_tokens, 200);
  assert.equal(status.totals.output_tokens, 50);
});

test('extra.endpoint overrides stored config and preview default destination without persisting', () => {
  const preview = {
    harness: 'claude-code',
    endpoint: DEFAULT_ENDPOINT,
    days: [],
  };

  const config: PluginConfig = {
    auto_upload: false,
    endpoint: 'https://stored.endpoint.example.com',
  };

  // 1. With extra.endpoint override
  const statusWithOverride = formatCompactStatus(preview, config, {
    endpoint: 'https://override.endpoint.example.com',
  });
  assert.equal(statusWithOverride.endpoint, 'https://override.endpoint.example.com');

  // 2. Without extra.endpoint override: falls back to config.endpoint
  const statusWithoutOverride = formatCompactStatus(preview, config);
  assert.equal(statusWithoutOverride.endpoint, 'https://stored.endpoint.example.com');

  // 3. Without config.endpoint: falls back to preview endpoint / DEFAULT_ENDPOINT
  const statusDefault = formatCompactStatus(preview, { auto_upload: false });
  assert.equal(statusDefault.endpoint, DEFAULT_ENDPOINT);
});
