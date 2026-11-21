import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm, mkdir, writeFile, symlink } from 'node:fs/promises';
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { packagePlugin, ALLOWLIST_ENTRIES } from '../../package-plugin.ts';

/**
 * Creates a fully compliant synthetic source repository in an isolated temporary root.
 * Populates all required files and directories from ALLOWLIST_ENTRIES without touching real checkout files.
 */
async function createSyntheticSourceRepo(root: string): Promise<void> {
  for (const entry of ALLOWLIST_ENTRIES) {
    const fullPath = join(root, entry.relPath);
    if (entry.type === 'dir') {
      await mkdir(fullPath, { recursive: true });
      await writeFile(join(fullPath, 'entry.ts'), `// synthetic payload for ${entry.relPath}\n`);
    } else {
      await mkdir(dirname(fullPath), { recursive: true });
      await writeFile(
        fullPath,
        entry.relPath.endsWith('.json') ? '{"name": "synthetic-plugin"}\n' : '# Synthetic metadata\n',
      );
    }
  }
}

test('rejects intermediate ancestor symlink in source and never copies external private sentinel', async (t) => {
  const tmp = await mkdtemp(join(tmpdir(), 'tm-pkg-symlink-src-'));
  t.after(() => rm(tmp, { recursive: true, force: true }));

  // 1. Outside directory containing private sentinel
  const outsideDir = join(tmp, 'outside');
  const outsideSrc = join(outsideDir, 'src');
  await mkdir(outsideSrc, { recursive: true });
  const sentinelPath = join(outsideSrc, 'private.txt');
  const sentinelSecret = 'PRIVATE_CREDENTIAL_SENTINEL_NEVER_COPY';
  await writeFile(sentinelPath, sentinelSecret);

  // 2. Synthetic source root where integrations/shared is a symlink pointing to outsideDir
  const sourceDir = join(tmp, 'source');
  await createSyntheticSourceRepo(sourceDir);

  // Replace integrations/shared directory with an intermediate symlink to outsideDir.
  // In the allowlist, 'integrations/shared/src' is required.
  // The leaf 'src' is a real directory inside outsideDir, but intermediate component
  // 'integrations/shared' is a symlink escaping the source repo.
  await rm(join(sourceDir, 'integrations', 'shared'), { recursive: true, force: true });
  await symlink(outsideDir, join(sourceDir, 'integrations', 'shared'), 'dir');

  const outputDir = join(tmp, 'dist');

  // Packaging must reject because intermediate ancestor 'shared' is a symlink
  assert.throws(
    () => packagePlugin({ sourceDir, outputDir, silent: true }),
    (err: unknown) => err instanceof Error,
  );

  // Confidentiality invariant: the sentinel file MUST NOT have been copied to output
  const copiedSentinel = join(outputDir, 'integrations', 'shared', 'src', 'private.txt');
  assert.equal(existsSync(copiedSentinel), false);

  // Ensure sentinel was not written anywhere inside outputDir
  if (existsSync(outputDir)) {
    const hasSentinel = (dir: string): boolean => {
      for (const dirent of readdirSync(dir, { withFileTypes: true })) {
        if (dirent.name === 'private.txt') return true;
        if (dirent.isDirectory() && hasSentinel(join(dir, dirent.name))) return true;
      }
      return false;
    };
    assert.equal(hasSentinel(outputDir), false);
  }

  // Ensure external private sentinel was not modified or removed
  assert.equal(existsSync(sentinelPath), true);
  assert.equal(readFileSync(sentinelPath, 'utf8'), sentinelSecret);
});

test('rejects output directory when it or an ancestor is a symlink before writes', async (t) => {
  const tmp = await mkdtemp(join(tmpdir(), 'tm-pkg-symlink-dest-'));
  t.after(() => rm(tmp, { recursive: true, force: true }));

  const sourceDir = join(tmp, 'source');
  await createSyntheticSourceRepo(sourceDir);

  // Case 1: Direct output symlink pointing to an empty external directory
  const externalEmpty = join(tmp, 'external-empty');
  await mkdir(externalEmpty, { recursive: true });
  const directOutputSymlink = join(tmp, 'direct-output-symlink');
  await symlink(externalEmpty, directOutputSymlink, 'dir');

  assert.throws(
    () => packagePlugin({ sourceDir, outputDir: directOutputSymlink, silent: true }),
    (err: unknown) => err instanceof Error,
  );
  assert.equal(readdirSync(externalEmpty).length, 0);

  // Case 2: Symlink ancestor to external parent with an empty output child
  const externalParent = join(tmp, 'external-parent');
  await mkdir(externalParent, { recursive: true });
  const ancestorSymlink = join(tmp, 'ancestor-symlink');
  await symlink(externalParent, ancestorSymlink, 'dir');

  const emptyChild = join(ancestorSymlink, 'empty-child');
  await mkdir(join(externalParent, 'empty-child'), { recursive: true });

  assert.throws(
    () => packagePlugin({ sourceDir, outputDir: emptyChild, silent: true }),
    (err: unknown) => err instanceof Error,
  );
  assert.equal(readdirSync(join(externalParent, 'empty-child')).length, 0);

  // Case 2b: Symlink ancestor to external parent with an absent/new output child
  const absentChild = join(ancestorSymlink, 'absent-child');
  assert.throws(
    () => packagePlugin({ sourceDir, outputDir: absentChild, silent: true }),
    (err: unknown) => err instanceof Error,
  );
  const externalAbsentChild = join(externalParent, 'absent-child');
  assert.equal(
    !existsSync(externalAbsentChild) || readdirSync(externalAbsentChild).length === 0,
    true,
  );
});

test('rejects nonempty output directory and preserves preexisting user files without deletion', async (t) => {
  const tmp = await mkdtemp(join(tmpdir(), 'tm-pkg-preexisting-'));
  t.after(() => rm(tmp, { recursive: true, force: true }));

  const sourceDir = join(tmp, 'source');
  await createSyntheticSourceRepo(sourceDir);

  const outputDir = join(tmp, 'preexisting-output');
  await mkdir(outputDir, { recursive: true });

  const preexistingUserFile = join(outputDir, 'user-preexisting-database.sqlite');
  const userPayload = 'CRITICAL_USER_DATA_DO_NOT_DELETE_ON_FAILURE';
  await writeFile(preexistingUserFile, userPayload);

  assert.throws(
    () => packagePlugin({ sourceDir, outputDir, silent: true }),
    (err: unknown) => err instanceof Error,
  );

  // Preexisting user file must still exist and content must remain untouched
  assert.equal(existsSync(preexistingUserFile), true);
  assert.equal(readFileSync(preexistingUserFile, 'utf8'), userPayload);
});
