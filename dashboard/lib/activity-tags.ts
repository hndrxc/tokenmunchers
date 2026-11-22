import type { ActivityCall } from "./activity";

export type ActivitySource = ActivityCall["source"];
export type ActivityTagPreferences = Record<ActivitySource, { label: string; visible: boolean }>;

export const ACTIVITY_TAG_SOURCES = ["ingest", "claude-code", "codex"] as const;
export const ACTIVITY_TAG_LABEL_LIMIT = 32;
export const ACTIVITY_TAG_STORAGE_KEY = "tokenmunchers.activity-tags.v1";

export const DEFAULT_ACTIVITY_TAGS: ActivityTagPreferences = {
  ingest: { label: "Standard", visible: false },
  "claude-code": { label: "Claude Code", visible: true },
  codex: { label: "Codex", visible: true },
};

export function sanitizeActivityTagLabel(source: ActivitySource, value: unknown): string {
  const label = typeof value === "string" ? value.trim().slice(0, ACTIVITY_TAG_LABEL_LIMIT).trimEnd() : "";
  return label || DEFAULT_ACTIVITY_TAGS[source].label;
}

export function parseActivityTags(raw: string | null): ActivityTagPreferences {
  let parsed: unknown;
  if (raw !== null) {
    try {
      parsed = JSON.parse(raw);
    } catch {
      // Invalid browser data is equivalent to missing preferences.
    }
  }

  const stored = parsed !== null && typeof parsed === "object" && !Array.isArray(parsed)
    ? parsed as Partial<Record<ActivitySource, unknown>>
    : undefined;
  const preferences = { ...DEFAULT_ACTIVITY_TAGS };
  for (const source of ACTIVITY_TAG_SOURCES) {
    const entry = stored?.[source];
    const tag = entry !== null && typeof entry === "object" ? entry : undefined;
    const label = tag && "label" in tag ? tag.label : undefined;
    const visible = tag && "visible" in tag ? tag.visible : undefined;
    preferences[source] = {
      label: sanitizeActivityTagLabel(source, label),
      visible: typeof visible === "boolean" ? visible : DEFAULT_ACTIVITY_TAGS[source].visible,
    };
  }
  return preferences;
}
