#!/usr/bin/env bash
# Illustrative AGY producer for omp-tokenmunchers >= 0.3.0.
# Prompt via stdin only; exactly one positional role.
set -uo pipefail

if [ "$#" -ne 1 ]; then
  printf '%s\n' 'usage: agy-run.sh builder|scout (prompt on stdin; no extra arguments)' >&2
  exit 2
fi
case "$1" in
  builder)
    model="${AGY_BUILDER_MODEL:-gemini-4-hydrogen-high}"
    mode=accept-edits
    timeout=30m
    ;;
  scout)
    model="${AGY_SCOUT_MODEL:-gemini-4-hydrogen-low}"
    mode=plan
    timeout=15m
    ;;
  *)
    printf '%s\n' 'usage: agy-run.sh builder|scout (prompt on stdin)' >&2
    exit 2
    ;;
esac

prompt=$(cat)
if [ -z "${prompt//[[:space:]]/}" ]; then
  printf '%s\n' 'error: empty prompt on stdin' >&2
  exit 2
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp) || exit 1
chmod 600 "$tmp" 2>/dev/null || true
trap 'rm -f -- "$tmp"' EXIT

# This is a minimal example, not a production rate-limit or concurrency guard.
agy -p "$prompt" --mode="$mode" --model "$model" \
  --output-format json --print-timeout "$timeout" >"$tmp"
code=$?

# Preserve the original CLI JSON on stdout for the calling agent.
cat -- "$tmp"
if [ "$code" -eq 0 ]; then
  if ! python3 "$script_dir/record-usage.py" "$model" <"$tmp"; then
    printf '%s\n' 'warning: external usage record not written' >&2
  fi
fi
exit "$code"
