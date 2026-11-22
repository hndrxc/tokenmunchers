import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  ACTIVITY_TAG_LABEL_LIMIT,
  ACTIVITY_TAG_SOURCES,
  DEFAULT_ACTIVITY_TAGS,
  parseActivityTags,
  sanitizeActivityTagLabel,
} from '../lib/activity-tags.ts';

test('default tags hide ingest under a neutral label and show collector names', () => {
  assert.deepEqual(ACTIVITY_TAG_SOURCES, ['ingest', 'claude-code', 'codex']);
  assert.deepEqual(parseActivityTags(null), {
    ingest: { label: 'Standard', visible: false },
    'claude-code': { label: 'Claude Code', visible: true },
    codex: { label: 'Codex', visible: true },
  });
});

test('malformed JSON and invalid top-level values use fresh defaults', () => {
  for (const raw of ['{', 'null', '[]', 'true', '"tags"']) {
    assert.deepEqual(parseActivityTags(raw), DEFAULT_ACTIVITY_TAGS);
  }
  const preferences = parseActivityTags(null);
  preferences.ingest.label = 'Customized';
  preferences.ingest.visible = true;
  assert.deepEqual(DEFAULT_ACTIVITY_TAGS.ingest, { label: 'Standard', visible: false });
});

test('stored labels and visibility round-trip for all sources', () => {
  const preferences = {
    ingest: { label: 'API calls', visible: true },
    'claude-code': { label: 'Claude', visible: false },
    codex: { label: 'Code', visible: false },
  };
  assert.deepEqual(parseActivityTags(JSON.stringify(preferences)), preferences);
});

test('decoding ignores unknown sources and falls back independently for invalid fields', () => {
  const raw = '{"ingest":{"label":"  ","visible":true},"claude-code":{"label":" Claude ","visible":"false"},"codex":{"label":7,"visible":false},"extra":{"label":"Unknown","visible":true},"__proto__":{}}';
  assert.deepEqual(parseActivityTags(raw), {
    ingest: { label: 'Standard', visible: true },
    'claude-code': { label: 'Claude', visible: true },
    codex: { label: 'Codex', visible: false },
  });
  assert.deepEqual(parseActivityTags('{"codex":null,"claude-code":[]}'), DEFAULT_ACTIVITY_TAGS);
});

test('labels are trimmed, limited to 32 characters, and default when blank', () => {
  assert.equal(ACTIVITY_TAG_LABEL_LIMIT, 32);
  const label = 'x'.repeat(ACTIVITY_TAG_LABEL_LIMIT);
  assert.equal(sanitizeActivityTagLabel('codex', `  ${label}extra  `), label);
  assert.equal(sanitizeActivityTagLabel('claude-code', ' \n\t '), 'Claude Code');
  assert.equal(sanitizeActivityTagLabel('ingest', null), 'Standard');
});
