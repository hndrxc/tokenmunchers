#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// Enforce Node.js >= 26
const [nodeMajorStr] = process.versions.node.split('.');
const nodeMajor = parseInt(nodeMajorStr, 10);
if (isNaN(nodeMajor) || nodeMajor < 26) {
  console.error(`Error: Node.js >= 26 is required (detected ${process.version}). Please upgrade Node.js.`);
  process.exit(1);
}

export interface PackagePluginOptions {
  outputDir: string;
  sourceDir?: string;
  silent?: boolean;
}

export interface PackagePluginResult {
  targetDir: string;
  copiedFiles: string[];
  copiedDirectories: string[];
}

export interface AllowlistEntry {
  relPath: string;
  type: 'file' | 'dir';
  required: boolean;
}

export const ALLOWLIST_ENTRIES: readonly AllowlistEntry[] = [
  // Native manifests
  { relPath: '.claude-plugin', type: 'dir', required: true },
  { relPath: '.codex-plugin', type: 'dir', required: true },
  { relPath: '.agents/plugins', type: 'dir', required: true },
  // Platform plugin directories (hooks, skills, platform READMEs)
  { relPath: 'plugins/claude-code', type: 'dir', required: true },
  { relPath: 'plugins/codex', type: 'dir', required: true },
  // Integration runtime & shared packages
  { relPath: 'integrations/package.json', type: 'file', required: true },
  { relPath: 'integrations/shared/src', type: 'dir', required: true },
  { relPath: 'integrations/collector/src', type: 'dir', required: true },
  { relPath: 'integrations/claude-code/src', type: 'dir', required: true },
  { relPath: 'integrations/codex/src', type: 'dir', required: true },
  { relPath: 'integrations/plugin/src', type: 'dir', required: true },
  // Root metadata & documentation
  { relPath: 'LICENSE', type: 'file', required: false },
  { relPath: 'README.md', type: 'file', required: false },
];

/**
 * Returns true if the file or directory name matches excluded patterns
 * (credentials, git, node_modules, local plans, fixtures, build artifacts).
 */
export function isIgnoredName(name: string): boolean {
  if (name === '.git' || name === 'node_modules') return true;
  if (name.startsWith('.git')) return true;
  if (name.startsWith('.env')) return true;
  if (name.includes('PLAN')) return true;
  if (name.includes('fixture')) return true;
  if (name === 'dist' || name === 'build' || name === 'out' || name === '.turbo' || name === '.next') return true;
  if (name === '.DS_Store' || name.endsWith('~') || name.endsWith('.swp') || name.endsWith('.tmp') || name.endsWith('.log')) return true;
  return false;
}

/**
 * Parses CLI arguments. Fails on unknown flags or arguments.
 */
export function parseArgs(args: string[]): { outputDir: string } {
  let outputDir: string | undefined;

  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (arg === '--output') {
      if (i + 1 >= args.length) {
        throw new Error('Flag --output requires a directory path argument.');
      }
      i++;
      outputDir = args[i];
    } else if (arg.startsWith('--output=')) {
      outputDir = arg.slice('--output='.length);
      if (!outputDir) {
        throw new Error('Flag --output requires a non-empty directory path argument.');
      }
    } else {
      throw new Error(`Unknown argument or flag: ${arg}`);
    }
  }

  if (!outputDir) {
    throw new Error('Missing required argument: --output <dir>');
  }

  return { outputDir };
}

/**
 * Packages a safe, self-contained native plugin marketplace distribution into outputDir.
 */
export function packagePlugin(options: PackagePluginOptions): PackagePluginResult {
  // 1. Resolve source repository root without dependence on CWD for inferred root
  const rawInferredRepoRoot = path.resolve(path.dirname(scriptPath), '..');
  const inferredRepoRoot = fs.existsSync(rawInferredRepoRoot)
    ? fs.realpathSync(rawInferredRepoRoot)
    : rawInferredRepoRoot;
  const sourceRepoRoot = options.sourceDir ? path.resolve(options.sourceDir) : inferredRepoRoot;

  // Validate source directory exists and is a real directory (not a symlink)
  if (!fs.existsSync(sourceRepoRoot)) {
    throw new Error(`Source directory does not exist: "${sourceRepoRoot}"`);
  }

  const sourceRootLstat = fs.lstatSync(sourceRepoRoot);
  if (sourceRootLstat.isSymbolicLink()) {
    throw new Error(`Source directory cannot be a symbolic link: "${sourceRepoRoot}"`);
  }
  if (!sourceRootLstat.isDirectory()) {
    throw new Error(`Source path is not a directory: "${sourceRepoRoot}"`);
  }

  const realSourceRepoRoot = fs.realpathSync(sourceRepoRoot);

  // 2. Resolve target output directory
  const targetOutputDir = path.resolve(process.cwd(), options.outputDir);

  // 3. Strict destination symlink rejection:
  // Reject if targetOutputDir itself or any existing ancestor component is a symbolic link.
  let checkDest: string = targetOutputDir;
  while (true) {
    try {
      const destLstat = fs.lstatSync(checkDest);
      if (destLstat.isSymbolicLink()) {
        if (checkDest === targetOutputDir) {
          throw new Error(`Output directory cannot be a symbolic link: "${checkDest}"`);
        } else {
          throw new Error(
            `Output directory ancestor component cannot be a symbolic link: "${checkDest}". ` +
            `If this is a system path alias (such as /tmp or /home on some operating systems), ` +
            `please specify the canonical physical path.`
          );
        }
      }
    } catch (err: unknown) {
      if (err && typeof err === 'object' && 'code' in err && (err.code === 'ENOENT' || err.code === 'ENOTDIR')) {
        // Path component does not exist yet; continue checking parent components
      } else {
        throw err;
      }
    }
    const parent = path.dirname(checkDest);
    if (parent === checkDest) break;
    checkDest = parent;
  }

  // Determine real canonical path of targetOutputDir
  let realTargetOutputDir: string;
  if (fs.existsSync(targetOutputDir)) {
    realTargetOutputDir = fs.realpathSync(targetOutputDir);
  } else {
    let existingAncestor = targetOutputDir;
    while (!fs.existsSync(existingAncestor)) {
      const p = path.dirname(existingAncestor);
      if (p === existingAncestor) break;
      existingAncestor = p;
    }
    const realAncestor = fs.realpathSync(existingAncestor);
    realTargetOutputDir = path.resolve(realAncestor, path.relative(existingAncestor, targetOutputDir));
  }

  // 4. Output directory safety checks against source repository (both lexical and realpath)
  if (targetOutputDir === sourceRepoRoot || realTargetOutputDir === realSourceRepoRoot) {
    throw new Error(`Output directory cannot be the source repository root: ${targetOutputDir}`);
  }

  if (
    sourceRepoRoot.startsWith(targetOutputDir + path.sep) ||
    realSourceRepoRoot.startsWith(realTargetOutputDir + path.sep)
  ) {
    throw new Error(`Output directory cannot be an ancestor of the source repository: ${targetOutputDir}`);
  }

  // Check if output is inside any copied source subtree to prevent recursion
  for (const entry of ALLOWLIST_ENTRIES) {
    const sourceEntryPath = path.resolve(sourceRepoRoot, entry.relPath);
    const realSourceEntryPath = fs.existsSync(sourceEntryPath)
      ? fs.realpathSync(sourceEntryPath)
      : path.resolve(realSourceRepoRoot, entry.relPath);

    if (
      targetOutputDir === sourceEntryPath ||
      targetOutputDir.startsWith(sourceEntryPath + path.sep) ||
      realTargetOutputDir === realSourceEntryPath ||
      realTargetOutputDir.startsWith(realSourceEntryPath + path.sep)
    ) {
      throw new Error(`Output directory cannot be inside copied source path: "${entry.relPath}"`);
    }
    if (
      sourceEntryPath.startsWith(targetOutputDir + path.sep) ||
      realSourceEntryPath.startsWith(realTargetOutputDir + path.sep)
    ) {
      throw new Error(`Output directory cannot contain copied source path: "${entry.relPath}"`);
    }
  }

  // 5. Validate target directory status (empty existing directory or nonexistent)
  let targetDirExisted = false;
  if (fs.existsSync(targetOutputDir)) {
    const stat = fs.statSync(targetOutputDir);
    if (!stat.isDirectory()) {
      throw new Error(`Output path exists and is not a directory: ${targetOutputDir}`);
    }
    const existingEntries = fs.readdirSync(targetOutputDir);
    if (existingEntries.length > 0) {
      throw new Error(`Output directory is not empty: ${targetOutputDir}`);
    }
    targetDirExisted = true;
  }

  // 6. Strict source symlink pre-validation:
  // Every existing ancestor component from source root through allowlist path must not be a symbolic link.
  for (const entry of ALLOWLIST_ENTRIES) {
    const segments = entry.relPath.split(/[/\\]+/).filter(Boolean);
    let currentCheck = sourceRepoRoot;
    for (const segment of segments) {
      currentCheck = path.join(currentCheck, segment);
      try {
        const segLstat = fs.lstatSync(currentCheck);
        if (segLstat.isSymbolicLink()) {
          throw new Error(
            `Symbolic link detected at "${currentCheck}". Symbolic links are strictly forbidden in source tree.`
          );
        }
      } catch (err: unknown) {
        if (err && typeof err === 'object' && 'code' in err && (err.code === 'ENOENT' || err.code === 'ENOTDIR')) {
          break;
        }
        throw err;
      }
    }

    const srcPath = path.resolve(sourceRepoRoot, entry.relPath);
    if (!fs.existsSync(srcPath)) {
      if (entry.required) {
        throw new Error(`Required source path not found: "${entry.relPath}" (expected at ${srcPath})`);
      }
      continue;
    }

    const entryLstat = fs.lstatSync(srcPath);
    if (entryLstat.isSymbolicLink()) {
      throw new Error(`Symbolic link detected at "${srcPath}". Symbolic links are strictly forbidden in source tree.`);
    }

    if (entry.type === 'file' && !entryLstat.isFile()) {
      throw new Error(`Expected file at "${entry.relPath}", but found a non-file.`);
    }
    if (entry.type === 'dir' && !entryLstat.isDirectory()) {
      throw new Error(`Expected directory at "${entry.relPath}", but found a non-directory.`);
    }

    const realSrcPath = fs.realpathSync(srcPath);
    if (realSrcPath !== realSourceRepoRoot && !realSrcPath.startsWith(realSourceRepoRoot + path.sep)) {
      throw new Error(`Source path "${srcPath}" resolves outside repository root to "${realSrcPath}".`);
    }
  }

  let createdOutputDir = false;
  const createdTopLevelEntries = new Set<string>();
  const copiedFiles: string[] = [];
  const copiedDirectories: string[] = [];

  function recordCreated(destPath: string) {
    const relToTarget = path.relative(targetOutputDir, destPath);
    const topSegment = relToTarget.split(path.sep)[0];
    if (topSegment) {
      createdTopLevelEntries.add(topSegment);
    }
  }

  try {
    if (!targetDirExisted) {
      fs.mkdirSync(targetOutputDir, { recursive: true });
      createdOutputDir = true;
    }

    // Copy allowlist entries
    for (const entry of ALLOWLIST_ENTRIES) {
      const srcPath = path.resolve(sourceRepoRoot, entry.relPath);
      const destPath = path.resolve(targetOutputDir, entry.relPath);

      if (!fs.existsSync(srcPath)) {
        if (entry.required) {
          throw new Error(`Required source path not found: "${entry.relPath}" (expected at ${srcPath})`);
        }
        continue;
      }

      const lstat = fs.lstatSync(srcPath);
      if (lstat.isSymbolicLink()) {
        throw new Error(`Symbolic link detected at "${srcPath}". Symbolic links are strictly forbidden in source tree.`);
      }

      if (entry.type === 'file') {
        if (!lstat.isFile()) {
          throw new Error(`Expected file at "${entry.relPath}", but found a non-file.`);
        }
        const destDir = path.dirname(destPath);
        if (!fs.existsSync(destDir)) {
          fs.mkdirSync(destDir, { recursive: true });
          recordCreated(destDir);
        }
        fs.copyFileSync(srcPath, destPath);
        recordCreated(destPath);
        copiedFiles.push(entry.relPath);
      } else if (entry.type === 'dir') {
        if (!lstat.isDirectory()) {
          throw new Error(`Expected directory at "${entry.relPath}", but found a non-directory.`);
        }

        // Recursive directory traversal
        const traverseDirectory = (currentSrc: string, currentDest: string) => {
          const currentLstat = fs.lstatSync(currentSrc);
          if (currentLstat.isSymbolicLink()) {
            throw new Error(`Symbolic link detected at "${currentSrc}". Symbolic links are strictly forbidden in source tree.`);
          }

          const realCurrentSrc = fs.realpathSync(currentSrc);
          if (realCurrentSrc !== realSourceRepoRoot && !realCurrentSrc.startsWith(realSourceRepoRoot + path.sep)) {
            throw new Error(`Path escaped source repository root: "${currentSrc}" resolves to "${realCurrentSrc}"`);
          }

          if (!fs.existsSync(currentDest)) {
            fs.mkdirSync(currentDest, { recursive: true });
            recordCreated(currentDest);
            copiedDirectories.push(path.relative(targetOutputDir, currentDest));
          }

          const dirents = fs.readdirSync(currentSrc, { withFileTypes: true });
          for (const dirent of dirents) {
            const entrySrc = path.join(currentSrc, dirent.name);
            const entryDest = path.join(currentDest, dirent.name);

            if (dirent.isSymbolicLink()) {
              throw new Error(`Symbolic link detected at "${entrySrc}". Symbolic links are strictly forbidden in source tree.`);
            }

            if (isIgnoredName(dirent.name)) {
              continue;
            }

            if (dirent.isDirectory()) {
              traverseDirectory(entrySrc, entryDest);
            } else if (dirent.isFile()) {
              const fileLstat = fs.lstatSync(entrySrc);
              if (fileLstat.isSymbolicLink()) {
                throw new Error(`Symbolic link detected at "${entrySrc}". Symbolic links are strictly forbidden in source tree.`);
              }
              const realFileSrc = fs.realpathSync(entrySrc);
              if (!realFileSrc.startsWith(realSourceRepoRoot + path.sep)) {
                throw new Error(`File escaped source repository root: "${entrySrc}" resolves to "${realFileSrc}"`);
              }

              fs.copyFileSync(entrySrc, entryDest);
              recordCreated(entryDest);
              copiedFiles.push(path.relative(targetOutputDir, entryDest));
            }
          }
        };

        traverseDirectory(srcPath, destPath);
      }
    }

    if (!options.silent) {
      console.log(`Successfully packaged native plugin distribution:`);
      console.log(`  Source: ${sourceRepoRoot}`);
      console.log(`  Target: ${targetOutputDir}`);
      console.log(`  Files copied: ${copiedFiles.length}`);
    }

    return {
      targetDir: targetOutputDir,
      copiedFiles,
      copiedDirectories,
    };
  } catch (err) {
    // Fail-cleanly semantics: do not delete preexisting user files or directories
    try {
      if (createdOutputDir) {
        fs.rmSync(targetOutputDir, { recursive: true, force: true });
      } else {
        for (const entry of createdTopLevelEntries) {
          const entryPath = path.join(targetOutputDir, entry);
          if (fs.existsSync(entryPath)) {
            fs.rmSync(entryPath, { recursive: true, force: true });
          }
        }
      }
    } catch (cleanupErr) {
      console.error('Warning: Error while cleaning up failed packaging output:', cleanupErr);
    }
    throw err;
  }
}

// CLI entrypoint
const scriptPath = fileURLToPath(import.meta.url);
const isMain = process.argv[1] && (
  path.resolve(process.argv[1]) === scriptPath ||
  process.argv[1].endsWith('package-plugin.ts') ||
  process.argv[1].endsWith('package-plugin.js')
);

if (isMain) {
  try {
    const { outputDir } = parseArgs(process.argv.slice(2));
    packagePlugin({ outputDir });
  } catch (err: unknown) {
    console.error(err instanceof Error ? err.message : String(err));
    process.exit(1);
  }
}
