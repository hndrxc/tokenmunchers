"use client";

import { createContext, useContext, useEffect, useId, useRef, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import {
  ACTIVITY_TAG_LABEL_LIMIT,
  ACTIVITY_TAG_SOURCES,
  ACTIVITY_TAG_STORAGE_KEY,
  DEFAULT_ACTIVITY_TAGS,
  parseActivityTags,
  sanitizeActivityTagLabel,
} from "@/lib/activity-tags";
import type { ActivitySource, ActivityTagPreferences } from "@/lib/activity-tags";

const SOURCE_NAMES: Record<ActivitySource, string> = {
  ingest: "Standard calls",
  "claude-code": "Claude Code",
  codex: "Codex",
};

type TagContext = {
  tags: ActivityTagPreferences;
  editorVersion: number;
  toggle: (source: ActivitySource) => void;
  saveLabel: (source: ActivitySource, label: string) => string;
  reset: () => void;
};

const ActivityTagContext = createContext<TagContext | null>(null);

function useActivityTags() {
  const context = useContext(ActivityTagContext);
  if (!context) throw new Error("Activity tags require an ActivityTagPreferencesProvider");
  return context;
}

export function ActivityTagPreferencesProvider({ children }: { children: ReactNode }) {
  const [tags, setTags] = useState(DEFAULT_ACTIVITY_TAGS);
  const [editorVersion, setEditorVersion] = useState(0);
  const current = useRef(tags);

  useEffect(() => {
    function readStorage(event?: StorageEvent) {
      try {
        const storage = window.localStorage;
        if (event && (event.storageArea !== storage || (event.key !== null && event.key !== ACTIVITY_TAG_STORAGE_KEY))) {
          return;
        }
        const next = parseActivityTags(storage.getItem(ACTIVITY_TAG_STORAGE_KEY));
        current.current = next;
        setTags(next);
        setEditorVersion((version) => version + 1);
      } catch {
        // Storage can be unavailable; the current preferences still work in memory.
      }
    }

    window.addEventListener("storage", readStorage);
    readStorage();
    return () => window.removeEventListener("storage", readStorage);
  }, []);

  function save(next: ActivityTagPreferences) {
    current.current = next;
    setTags(next);
    try {
      window.localStorage.setItem(ACTIVITY_TAG_STORAGE_KEY, JSON.stringify(next));
    } catch {
      // Keep the new preferences for this page session even if they cannot be saved.
    }
  }

  function toggle(source: ActivitySource) {
    const latest = current.current;
    const tag = latest[source];
    save({ ...latest, [source]: { ...tag, visible: !tag.visible } });
  }

  function saveLabel(source: ActivitySource, draft: string) {
    const latest = current.current;
    const label = sanitizeActivityTagLabel(source, draft);
    save({ ...latest, [source]: { ...latest[source], label } });
    return label;
  }

  function reset() {
    save(DEFAULT_ACTIVITY_TAGS);
    setEditorVersion((version) => version + 1);
  }

  return (
    <ActivityTagContext.Provider value={{ tags, editorVersion, toggle, saveLabel, reset }}>
      {children}
    </ActivityTagContext.Provider>
  );
}

function ActivityTagEditor({ source }: { source: ActivitySource }) {
  const { tags, editorVersion, toggle, saveLabel } = useActivityTags();
  const { label, visible } = tags[source];
  const name = SOURCE_NAMES[source];
  const inputId = useId();
  const input = useRef<HTMLInputElement>(null);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (input.current) input.current.value = saveLabel(source, input.current.value);
  }

  return (
    <fieldset className="activity-tags__source">
      <legend>{name}</legend>
      <button type="button" aria-label={`Show ${name} tag`} aria-pressed={visible} onClick={() => toggle(source)}>
        Show tag: {visible ? "On" : "Off"}
      </button>
      <form className="activity-tags__label" onSubmit={submit}>
        <label htmlFor={inputId}>
          <span className="visually-hidden">{name} tag </span>label
        </label>
        <input key={editorVersion} ref={input} id={inputId} type="text" maxLength={ACTIVITY_TAG_LABEL_LIMIT} defaultValue={label} />
        <button type="submit">Save</button>
      </form>
    </fieldset>
  );
}

export function ActivityTagControls() {
  const { reset } = useActivityTags();

  return (
    <details className="activity-tags">
      <summary>Customize tags</summary>
      <div className="activity-tags__settings">
        {ACTIVITY_TAG_SOURCES.map((source) => <ActivityTagEditor key={source} source={source} />)}
        <button type="button" onClick={reset}>Reset tags</button>
      </div>
    </details>
  );
}

export function ActivitySourceBadge({ source }: { source: ActivitySource }) {
  const { tags } = useActivityTags();
  const { label, visible } = tags[source];
  return visible ? <>{" "}<span className="badge activity-source-badge">{label}</span></> : null;
}
