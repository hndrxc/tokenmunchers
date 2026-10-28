import { readdir, realpath } from 'node:fs/promises';
import { basename, join, relative, resolve, sep } from 'node:path';
import type { Diagnostic } from '../../shared/src/observations.ts';

export interface Transcript {
  path: string;
  relativePath: string;
  project: string;
  sessionId: string;
  parentSessionId: string | null;
  isSubagent: boolean;
}

/** Exclusions are selected-root-relative file/directory prefixes, not project names or globs. */
export function excluded(path: string, prefixes: string[]): boolean {
  const value = path.split(sep).join('/');
  return prefixes.some((prefix) => {
    const normalized = prefix.replaceAll('\\', '/').replace(/^\.\//, '').replace(/\/+$/, '');
    return normalized === '.' || normalized === '' || value === normalized || value.startsWith(`${normalized}/`);
  });
}

export async function discover(root: string, exclude: string[], diagnostics: Diagnostic[]): Promise<Transcript[]> {
  const selected = await realpath(resolve(root));
  const projects = join(selected, 'projects');
  const files: Transcript[] = [];
  async function entries(path: string) {
    try { return (await readdir(path, { withFileTypes: true })).sort((a, b) => a.name.localeCompare(b.name, 'en')); }
    catch (error) {
      diagnostics.push({ code: 'DISCOVERY_UNAVAILABLE', message: `Selected transcript directory is unavailable (${(error as NodeJS.ErrnoException).code ?? 'IO'}).`, file: path });
      return [];
    }
  }
  function allowed(path: string) { return !excluded(relative(selected, path), exclude); }
  // Never recurse through links: a selected config directory does not authorize another home.
  const rootEntries = await entries(selected);
  if (!rootEntries.some((entry) => entry.name === 'projects' && entry.isDirectory()) || !allowed(projects)) return files;
  for (const project of await entries(projects)) {
    const directory = join(projects, project.name);
    if (!project.isDirectory() || !allowed(directory)) continue;
    const children = await entries(directory);
    for (const entry of children) {
      const path = join(directory, entry.name);
      if (!allowed(path)) continue;
      if (entry.isFile() && entry.name.endsWith('.jsonl')) {
        const id = basename(entry.name, '.jsonl');
        files.push({ path, relativePath: relative(selected, path), project: project.name, sessionId: id, parentSessionId: null, isSubagent: id.startsWith('agent-') });
      } else if (entry.isDirectory()) {
        const subagents = join(path, 'subagents');
        if (!allowed(subagents)) continue;
        // Missing subagents is normal, not a broken scan.
        const nested = await entries(path);
        if (!nested.some((child) => child.name === 'subagents' && child.isDirectory())) continue;
        for (const child of await entries(subagents)) {
          const childPath = join(subagents, child.name);
          if (!child.isFile() || !child.name.startsWith('agent-') || !child.name.endsWith('.jsonl') || !allowed(childPath)) continue;
          files.push({ path: childPath, relativePath: relative(selected, childPath), project: project.name, sessionId: `${entry.name}/${basename(child.name, '.jsonl')}`, parentSessionId: entry.name, isSubagent: true });
        }
      }
    }
  }
  return files.sort((a, b) => a.relativePath.localeCompare(b.relativePath, 'en'));
}
