import { constants } from 'node:fs';
import { open } from 'node:fs/promises';
import type { Diagnostic } from '../../shared/src/observations.ts';

export interface ReadState {
  bytesRead: number;
  committedOffset: number;
  line: number;
  deferred: boolean;
  truncated: boolean;
  device: number;
  inode: number;
  startOffset: number;
  discardLine: boolean;
}
export interface JsonRow { value: Record<string, unknown>; line: number }

/** Read complete newline-delimited records only. All offsets and paths remain local. */
export async function* readJsonl(path: string, maxBytes: number, maxLineBytes: number, diagnostics: Diagnostic[], state: ReadState): AsyncGenerator<JsonRow> {
  const handle = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  const parts: Buffer[] = [];
  let lineBytes = 0;
  let oversized = state.discardLine;
  const decoder = new TextDecoder('utf-8', { fatal: true });
  try {
    const stat = await handle.stat();
    const size = stat.size;
    if (state.startOffset && (stat.dev !== state.device || stat.ino !== state.inode || state.startOffset > size)) {
      diagnostics.push({ code: 'CURSOR_RESET', message: 'Transcript was replaced or truncated; rescanning canonical identities.', file: path });
      state.startOffset = 0;
      state.committedOffset = 0;
      state.line = 0;
      state.discardLine = false;
      oversized = false;
    }
    state.device = stat.dev;
    state.inode = stat.ino;
    const buffer = Buffer.allocUnsafe(Math.min(64 * 1024, Math.max(1, maxBytes)));
    while (state.bytesRead < Math.min(maxBytes, size - state.startOffset)) {
      const { bytesRead } = await handle.read(buffer, 0, Math.min(buffer.length, maxBytes - state.bytesRead, size - state.startOffset - state.bytesRead), state.startOffset + state.bytesRead);
      if (!bytesRead) break;
      const base = state.startOffset + state.bytesRead;
      state.bytesRead += bytesRead;
      let start = 0;
      for (let index = 0; index < bytesRead; index++) {
        if (buffer[index] !== 10) continue;
        const slice = buffer.subarray(start, index);
        lineBytes += slice.length;
        state.line++;
        if (oversized || lineBytes > maxLineBytes) {
          diagnostics.push({ code: 'LINE_TOO_LARGE', message: 'Record exceeds the configured line bound; record skipped.', file: path, line: state.line });
        } else {
          parts.push(Buffer.from(slice));
          try {
            const text = decoder.decode(Buffer.concat(parts, lineBytes));
            if (text.trim()) {
              const value: unknown = JSON.parse(text);
              if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('not object');
              yield { value: value as Record<string, unknown>, line: state.line };
            }
          } catch {
            diagnostics.push({ code: 'MALFORMED_JSONL', message: 'Complete record is not a valid UTF-8 JSON object; record skipped.', file: path, line: state.line });
          }
        }
        state.committedOffset = base + index + 1;
        parts.length = 0;
        lineBytes = 0;
        oversized = false;
        start = index + 1;
      }
      if (start < bytesRead) {
        const slice = buffer.subarray(start, bytesRead);
        lineBytes += slice.length;
        if (!oversized && lineBytes <= maxLineBytes) parts.push(Buffer.from(slice));
        else { oversized = true; parts.length = 0; }
      }
    }
    state.truncated = state.startOffset + state.bytesRead < size;
    state.deferred = state.truncated || lineBytes > 0;
    state.discardLine = oversized;
    if (oversized) state.committedOffset = state.startOffset + state.bytesRead;
    if (lineBytes > 0) diagnostics.push({ code: 'PARTIAL_LINE_DEFERRED', message: 'Unterminated trailing record deferred until a subsequent full rescan.', file: path, line: state.line + 1 });
    if (state.truncated) diagnostics.push({ code: 'SCAN_BYTE_LIMIT', message: 'File scan reached the configured byte bound; remaining records deferred.', file: path });
  } finally { await handle.close(); }
}
