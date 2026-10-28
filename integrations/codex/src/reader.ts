import { constants, type ReadStream } from 'node:fs';
import { lstat, open, readdir } from 'node:fs/promises';
import { join, relative, resolve, sep } from 'node:path';
import type { Readable } from 'node:stream';
import { constants as zstdConstants, createZstdDecompress } from 'node:zlib';
import type { Diagnostic, ScanOptions } from '../../shared/src/observations.ts';

export interface ReadProgress { files: number; bytesRead: number; deferred: number; sourceBytesRead?: number }
/** `truncated`: the scan byte budget stopped this file before its end (as opposed to a damaged or partial source). */
export interface Cursor { file: string; line: number; bytesRead: number; complete: boolean; truncated: boolean }
export interface Row { value: unknown; line: number }
const diagnostic = (code: string, message: string, file?: string, line?: number): Diagnostic => ({ code, message, file, line });

/** Only the selected root's active/archive rollouts; symlinks are never followed. */
export async function discover(root: string, exclude: string[], diagnostics: Diagnostic[]): Promise<string[]> {
  const base = resolve(root);
  const paths: string[] = [];
  const excluded = (path: string) => exclude.some(prefix => {
    const normalized = prefix.replaceAll('\\', '/').replace(/^\.\//, '').replace(/\/$/, '');
    return path === normalized || path.startsWith(`${normalized}/`);
  });
  async function visit(path: string): Promise<void> {
    const local = relative(base, path).split(sep).join('/');
    if (excluded(local)) { diagnostics.push(diagnostic('EXCLUDED', 'Selected rollout path excluded locally.', local)); return; }
    try {
      const info = await lstat(path);
      if (info.isSymbolicLink()) { diagnostics.push(diagnostic('SYMLINK_SKIPPED', 'Source symlink not followed.', local)); return; }
      if (info.isDirectory()) {
        for (const entry of (await readdir(path)).sort()) await visit(join(path, entry));
      } else if (info.isFile() && /^rollout-.*\.jsonl(?:\.zst)?$/.test(path.split(sep).at(-1)!)) paths.push(path);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') diagnostics.push(diagnostic('DISCOVERY_FAILED', 'Unable to inspect selected source path.', local));
    }
  }
  // Check every ancestor we traverse: selected root itself may not be a symlink.
  try {
    const info = await lstat(base);
    if (!info.isDirectory() || info.isSymbolicLink()) {
      diagnostics.push(diagnostic('INVALID_ROOT', 'Selected source root must be a regular directory.'));
      return paths;
    }
  } catch { diagnostics.push(diagnostic('INVALID_ROOT', 'Selected source root is unavailable.')); return paths; }
  await visit(join(base, 'sessions'));
  await visit(join(base, 'archived_sessions'));
  return paths;
}

/**
 * Node's zstd decoder silently discards input that follows a completed frame within the same write, which loses
 * concatenated frames and hides truncated or garbage trailers. Splits source chunks so no write spans a frame end.
 */
class ZstdFrames {
  private readonly header = Buffer.alloc(16);
  private have = 0;
  private want = 4;
  private phase: 'magic' | 'skippable' | 'descriptor' | 'frame' | 'block' | 'checksum' = 'magic';
  private body = 0;
  private endsAfterBody = false;
  private checksum = false;
  invalid = false;
  get boundary(): boolean { return this.phase === 'magic' && this.have === 0 && this.body === 0; }

  /** Pieces end at frame ends or at the chunk end; after `invalid`, only bytes through the last complete frame. */
  split(chunk: Buffer): Buffer[] {
    const pieces: Buffer[] = [];
    let start = 0;
    let offset = 0;
    while (offset < chunk.length && !this.invalid) {
      let ended: boolean;
      if (this.body > 0) {
        const take = Math.min(this.body, chunk.length - offset);
        this.body -= take;
        offset += take;
        if (this.body > 0) break;
        ended = this.endsAfterBody;
        this.endsAfterBody = false;
      } else {
        const take = Math.min(this.want - this.have, chunk.length - offset);
        chunk.copy(this.header, this.have, offset, offset + take);
        this.have += take;
        offset += take;
        if (this.have < this.want) break;
        this.have = 0;
        ended = this.parse();
      }
      if (ended) { pieces.push(chunk.subarray(start, offset)); start = offset; }
    }
    if (!this.invalid && start < chunk.length) pieces.push(chunk.subarray(start));
    return pieces;
  }

  /** Consumes a complete header field; returns whether it completed a frame. */
  private parse(): boolean {
    const header = this.header;
    switch (this.phase) {
      case 'magic': {
        const magic = header.readUInt32LE(0);
        if (magic === 0xFD2FB528) { this.phase = 'descriptor'; this.want = 1; }
        else if ((magic & 0xFFFFFFF0) >>> 0 === 0x184D2A50) { this.phase = 'skippable'; this.want = 4; }
        else this.invalid = true;
        return false;
      }
      case 'skippable':
        this.body = header.readUInt32LE(0);
        this.phase = 'magic'; this.want = 4;
        this.endsAfterBody = this.body > 0;
        return this.body === 0;
      case 'descriptor': {
        const descriptor = header[0];
        const single = (descriptor & 0x20) !== 0;
        this.checksum = (descriptor & 0x04) !== 0;
        const rest = (single ? 0 : 1) + [0, 1, 2, 4][descriptor & 3] + [single ? 1 : 0, 2, 4, 8][descriptor >> 6];
        if (rest) { this.phase = 'frame'; this.want = rest; }
        else { this.phase = 'block'; this.want = 3; }
        return false;
      }
      case 'frame':
        this.phase = 'block'; this.want = 3;
        return false;
      case 'block': {
        const value = header[0] | (header[1] << 8) | (header[2] << 16);
        const type = (value >> 1) & 3;
        if (type === 3) { this.invalid = true; return false; }
        this.body = type === 1 ? 1 : value >>> 3;
        const last = (value & 1) === 1;
        if (last && this.checksum) { this.phase = 'checksum'; this.want = 4; }
        else if (last) { this.phase = 'magic'; this.want = 4; }
        const ended = last && !this.checksum;
        if (this.body === 0) return ended;
        this.endsAfterBody = ended;
        return false;
      }
      case 'checksum':
        this.phase = 'magic'; this.want = 4;
        return true;
    }
  }
}

/** Limits count decompressed bytes, so compressed expansion cannot bypass the scan budget. */
export async function* readRows(file: string, local: string, options: ScanOptions, progress: ReadProgress,
  diagnostics: Diagnostic[], cursor: Cursor): AsyncGenerator<Row> {
  const maxBytes = options.maxBytes ?? 256 * 1024 * 1024;
  const maxLineBytes = options.maxLineBytes ?? 8 * 1024 * 1024;
  const compressed = file.endsWith('.zst');
  let source: ReadStream | undefined;
  let stream: Readable | undefined;
  let fragments: Buffer[] = [];
  let length = 0;
  let oversize = false;
  let line = 0;
  let stopped = false;
  let frameError = false;
  progress.files++;
  try {
    // Numeric flags keep the read-only, no-follow open even if the path was swapped for a symlink after discovery.
    const handle = await open(file, constants.O_RDONLY | constants.O_NOFOLLOW);
    source = handle.createReadStream({ highWaterMark: 64 * 1024 });
    const input = source;
    if (compressed) {
      const decoder = createZstdDecompress({ chunkSize: 64 * 1024, params: { [zstdConstants.ZSTD_d_windowLogMax]: 27 } });
      const frames = new ZstdFrames();
      // Compressed input is bounded twice: source bytes here, decompressed bytes in the line loop below.
      input.on('data', (chunk: Buffer) => {
        progress.sourceBytesRead = (progress.sourceBytesRead ?? 0) + chunk.length;
        if (progress.sourceBytesRead > maxBytes) {
          cursor.truncated = true;
          diagnostics.push(diagnostic('SCAN_TRUNCATED', 'Compressed source byte budget reached; retry with a larger budget.', local));
          input.destroy(new Error('SOURCE_BYTE_LIMIT'));
          return;
        }
        let writable = true;
        for (const piece of frames.split(chunk)) writable = decoder.write(piece) && writable;
        if (frames.invalid) { frameError = true; input.destroy(); decoder.end(); }
        else if (!writable) { input.pause(); decoder.once('drain', () => input.resume()); }
      });
      input.on('end', () => { if (!frames.boundary) frameError = true; decoder.end(); });
      input.on('error', () => decoder.destroy(new Error('SOURCE_READ_FAILED')));
      stream = decoder;
    } else stream = input;
    for await (const chunk of stream) {
      const data = chunk as Buffer;
      const available = Math.max(0, maxBytes - progress.bytesRead);
      const size = Math.min(data.length, available);
      progress.bytesRead += size;
      cursor.bytesRead += size;
      let start = 0;
      for (let offset = 0; offset < size; offset++) {
        if (data[offset] !== 10) continue;
        const piece = data.subarray(start, offset);
        if (!oversize && length + piece.length <= maxLineBytes) {
          fragments.push(piece);
          const bytes = Buffer.concat(fragments, length + piece.length);
          line++;
          cursor.line = line;
          try {
            const text = new TextDecoder('utf-8', { fatal: true }).decode(bytes).replace(/\r$/, '');
            if (text.trim()) yield { value: JSON.parse(text), line };
          } catch { diagnostics.push(diagnostic('MALFORMED_JSON', 'Complete UTF-8 JSONL record could not be decoded.', local, line)); }
        } else { line++; cursor.line = line; diagnostics.push(diagnostic('LINE_TOO_LARGE', 'Oversized JSONL record skipped.', local, line)); }
        fragments = []; length = 0; oversize = false; start = offset + 1;
      }
      const remainder = data.subarray(start, size);
      if (!oversize && length + remainder.length <= maxLineBytes) { fragments.push(remainder); length += remainder.length; }
      else { fragments = []; length = 0; oversize = true; }
      if (size < data.length) {
        stopped = true;
        cursor.truncated = true;
        diagnostics.push(diagnostic('SCAN_TRUNCATED', 'Decompressed scan byte budget reached; rescan with a larger budget.', local, line + 1));
        break;
      }
    }
    // Truncated or invalid trailing frames must not pass as a complete archive.
    if (frameError && !stopped) throw new Error('ZSTD_FRAME_INCOMPLETE');
    if (length || oversize || stopped) {
      progress.deferred++;
      diagnostics.push(diagnostic('DEFERRED_PARTIAL_LINE', 'Unterminated or truncated record deferred; no partial record accepted.', local, line + 1));
    } else cursor.complete = true;
  } catch {
    progress.deferred++;
    // A source byte budget stop is already reported as SCAN_TRUNCATED; it is not damage.
    if (!cursor.truncated) diagnostics.push(diagnostic(compressed ? 'DECOMPRESSION_FAILED' : 'READ_FAILED', 'Rollout could not be read completely; retry after source write completes.', local, line + 1));
  } finally { stream?.destroy(); source?.destroy(); }
}
