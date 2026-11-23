#!/usr/bin/env python3
"""Append one metadata-only Tokenmunchers usage event from AGY JSON on stdin."""
import json
import os
import pathlib
import sys
import time
import uuid


def nonnegative_int(value):
    return value if type(value) is int and value >= 0 else 0


def make_event(data, model, environ=None, now_ms=None):
    environ = os.environ if environ is None else environ
    if not isinstance(data, dict) or data.get("status") != "SUCCESS":
        return None
    usage = data.get("usage")
    if not isinstance(usage, dict):
        return None
    input_tokens = nonnegative_int(usage.get("input_tokens"))
    output_tokens = nonnegative_int(usage.get("output_tokens"))
    cache_read_tokens = nonnegative_int(usage.get("cache_read_tokens"))
    if not (input_tokens or output_tokens or cache_read_tokens):
        return None
    # Unique *invocation* ID: a conversation_id may be reused across multiple calls.
    event = {
        "v": 1,
        "source": "agy",
        "id": str(uuid.uuid4()),
        "ts": int(time.time() * 1000) if now_ms is None else now_ms,
        "provider": "google-antigravity",
        "model": model,
        "input_tokens": input_tokens,
        "output_tokens": output_tokens,
        "cache_read_tokens": cache_read_tokens,
    }
    if environ.get("PI_SESSION_ID"):
        event["session_id"] = environ["PI_SESSION_ID"]
    if environ.get("PI_SESSION_FILE"):
        event["session_file"] = environ["PI_SESSION_FILE"]
    return event


def main():
    if len(sys.argv) != 2 or not sys.argv[1]:
        print("usage: record-usage.py MODEL (JSON on stdin)", file=sys.stderr)
        return 2
    try:
        data = json.load(sys.stdin)
        event = make_event(data, sys.argv[1])
        if event is None:
            print("warning: no successful AGY usage to record", file=sys.stderr)
            return 1
        agent_dir = pathlib.Path(
            os.environ.get("TOKENMUNCHERS_AGENT_DIR")
            or os.environ.get("PI_CODING_AGENT_DIR")
            or pathlib.Path.home() / ".omp" / "agent"
        ).expanduser()
        destination = agent_dir / "tokenmunchers" / "external-usage.jsonl"
        destination.parent.mkdir(parents=True, exist_ok=True)
        line = (json.dumps(event, separators=(",", ":"), ensure_ascii=True) + "\n").encode("utf-8")
        # O_APPEND + one write per event avoids rewriting the drain's claimed file.
        fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
        try:
            if os.write(fd, line) != len(line):
                raise OSError("short write")
        finally:
            os.close(fd)
        return 0
    except (OSError, ValueError, TypeError, json.JSONDecodeError) as exc:
        print(f"warning: could not record AGY usage: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
