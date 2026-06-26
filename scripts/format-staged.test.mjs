#!/usr/bin/env node

/**
 * Tests for scripts/format-staged.sh
 *
 * Each test is hermetic: it creates a throwaway temp git repo, injects stub
 * formatters via PATH or node_modules/.bin/, runs the helper, and asserts on
 * exit code + staged blob state + working-tree content.
 *
 * No real gofmt / prettier are required — the stubs deterministically append
 * a marker line so assertions are exact.
 */

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { after, test } from 'node:test';

const SCRIPT = path.join(path.dirname(fileURLToPath(import.meta.url)), 'format-staged.sh');

// PATH that has git but NO gofmt (excludes Homebrew /opt/homebrew/bin)
const MINIMAL_PATH = '/usr/bin:/bin:/usr/local/bin';

const tempDirs = [];

function mkTemp(prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  tempDirs.push(dir);
  return dir;
}

/** Create a fresh repo with an empty initial commit so HEAD exists. */
function initRepo() {
  const dir = mkTemp('fmtstaged-repo-');
  execFileSync('git', ['init', '-q', dir]);
  execFileSync('git', ['-C', dir, 'config', 'user.email', 'test@example.com']);
  execFileSync('git', ['-C', dir, 'config', 'user.name', 'Test']);
  execFileSync('git', ['-C', dir, 'commit', '--allow-empty', '-q', '-m', 'init']);
  return dir;
}

/** Run format-staged.sh in cwd=repoDir with the given PATH. Returns {code, stdout, stderr}. */
function runScript(repoDir, pathOverride = process.env.PATH) {
  try {
    const stdout = execFileSync('sh', [SCRIPT], {
      cwd: repoDir,
      encoding: 'utf8',
      env: {
        HOME: process.env.HOME ?? '/tmp',
        PATH: pathOverride,
      },
    });
    return { code: 0, stdout, stderr: '' };
  } catch (err) {
    return {
      code: err.status ?? 1,
      stdout: err.stdout?.toString() ?? '',
      stderr: err.stderr?.toString() ?? '',
    };
  }
}

/** Return staged blob content of a path in a repo. */
function stagedContent(repoDir, relPath) {
  return execFileSync('git', ['-C', repoDir, 'show', `:${relPath}`], { encoding: 'utf8' });
}

/**
 * Write a stub formatter executable.
 * When called with (-w | --write) <file...> it appends a marker line to each file.
 */
function writeStub(dir, name, marker) {
  const p = path.join(dir, name);
  fs.writeFileSync(
    p,
    `#!/bin/sh\n# stub ${name}: skip flags, append marker to each file arg\nwhile [ "$1" = "-w" ] || [ "$1" = "--write" ]; do shift; done\nfor f in "$@"; do\n  printf '\\n# ${marker}\\n' >> "$f"\ndone\n`,
  );
  fs.chmodSync(p, 0o755);
  return p;
}

after(() => {
  for (const dir of tempDirs) fs.rmSync(dir, { recursive: true, force: true });
});

// ---------------------------------------------------------------------------
// a. Staged Go file is formatted by stub gofmt and re-staged
// ---------------------------------------------------------------------------
test('format-staged: staged Go file is formatted by stub gofmt and re-staged (exit 0)', () => {
  const repo = initRepo();
  const binDir = mkTemp('fmtstaged-bin-');
  writeStub(binDir, 'gofmt', 'stub-gofmt');

  // Create and stage a Go file
  const goFile = path.join(repo, 'main.go');
  fs.writeFileSync(goFile, 'package main\nfunc main() {}\n');
  execFileSync('git', ['-C', repo, 'add', 'main.go']);

  // Capture staged blob before running the script
  const before = stagedContent(repo, 'main.go');

  const result = runScript(repo, `${binDir}:${MINIMAL_PATH}`);

  assert.equal(result.code, 0, `exit code must be 0; stderr: ${result.stderr}`);

  const after = stagedContent(repo, 'main.go');
  assert.notEqual(before, after, 'staged blob must change after formatting');
  assert.ok(after.includes('stub-gofmt'), 'staged blob must include the stub marker');
});

// ---------------------------------------------------------------------------
// b. Unstaged file is left completely untouched
// ---------------------------------------------------------------------------
test('format-staged: unstaged-only file is not modified', () => {
  const repo = initRepo();
  const binDir = mkTemp('fmtstaged-bin-');
  writeStub(binDir, 'gofmt', 'stub-gofmt');

  // Create an initial committed Go file
  const goFile = path.join(repo, 'existing.go');
  fs.writeFileSync(goFile, 'package main\n');
  execFileSync('git', ['-C', repo, 'add', 'existing.go']);
  execFileSync('git', ['-C', repo, 'commit', '-q', '-m', 'add existing.go']);

  // Modify it in the working tree WITHOUT staging
  const unstaged = 'package main\n// unstaged change\n';
  fs.writeFileSync(goFile, unstaged);

  // Also stage a different file so the script has something to process
  const otherFile = path.join(repo, 'other.go');
  fs.writeFileSync(otherFile, 'package main\n');
  execFileSync('git', ['-C', repo, 'add', 'other.go']);

  const result = runScript(repo, `${binDir}:${MINIMAL_PATH}`);

  assert.equal(result.code, 0, `exit code must be 0; stderr: ${result.stderr}`);

  // The unstaged working-tree file must be unchanged (no stub marker added to it)
  const content = fs.readFileSync(goFile, 'utf8');
  assert.equal(content, unstaged, 'unstaged file must not be modified');
});

// ---------------------------------------------------------------------------
// c. Dep-free scenario: no gofmt on PATH + no node_modules → silent no-op
// ---------------------------------------------------------------------------
test('format-staged: dep-free worktree (no gofmt, no node_modules) exits 0 silently', () => {
  const repo = initRepo();

  // Stage a Go file so the script would have something to do if tools were present
  const goFile = path.join(repo, 'main.go');
  fs.writeFileSync(goFile, 'package main\n');
  execFileSync('git', ['-C', repo, 'add', 'main.go']);

  const before = stagedContent(repo, 'main.go');

  // Run with a minimal PATH that has NO gofmt and no node_modules in repo
  const result = runScript(repo, MINIMAL_PATH);

  assert.equal(result.code, 0, `dep-free must exit 0; stderr: ${result.stderr}`);

  // Nothing staged should change
  const after = stagedContent(repo, 'main.go');
  assert.equal(before, after, 'staged blob must be unchanged in dep-free mode');
  assert.equal(result.stdout.trim(), '', 'no-op must produce no stdout');
});

// ---------------------------------------------------------------------------
// d. Missing-tool resilience: staged .go with no gofmt on PATH → skipped, exit 0
// ---------------------------------------------------------------------------
test('format-staged: staged Go file with no gofmt on PATH is skipped, exit 0', () => {
  const repo = initRepo();

  // Create node_modules so the dep-free short-circuit does NOT trigger
  // (only triggers when BOTH gofmt absent AND node_modules absent)
  fs.mkdirSync(path.join(repo, 'node_modules'), { recursive: true });

  const goFile = path.join(repo, 'main.go');
  fs.writeFileSync(goFile, 'package main\n');
  execFileSync('git', ['-C', repo, 'add', 'main.go']);

  const before = stagedContent(repo, 'main.go');

  // PATH has no gofmt
  const result = runScript(repo, MINIMAL_PATH);

  assert.equal(result.code, 0, `missing gofmt must still exit 0; stderr: ${result.stderr}`);

  const after = stagedContent(repo, 'main.go');
  assert.equal(before, after, 'staged Go blob must be unchanged when gofmt is absent');
});

// ---------------------------------------------------------------------------
// e. Staged web file (TS) is formatted by stub prettier and re-staged
// ---------------------------------------------------------------------------
test('format-staged: staged .ts file is formatted by stub prettier and re-staged (exit 0)', () => {
  const repo = initRepo();

  // Create node_modules/.bin/prettier stub
  const binPath = path.join(repo, 'node_modules', '.bin');
  fs.mkdirSync(binPath, { recursive: true });
  writeStub(binPath, 'prettier', 'stub-prettier');

  // Stage a TypeScript file
  const tsFile = path.join(repo, 'app.ts');
  fs.writeFileSync(tsFile, 'const x = 1\n');
  execFileSync('git', ['-C', repo, 'add', 'app.ts']);

  const before = stagedContent(repo, 'app.ts');

  // PATH has no gofmt (no Go files staged, so it doesn't matter, but let's use minimal)
  const result = runScript(repo, MINIMAL_PATH);

  assert.equal(result.code, 0, `exit code must be 0; stderr: ${result.stderr}`);

  const after = stagedContent(repo, 'app.ts');
  assert.notEqual(before, after, 'staged blob must change after prettier formatting');
  assert.ok(after.includes('stub-prettier'), 'staged blob must include stub marker');
});

// ---------------------------------------------------------------------------
// f. File with both staged AND unstaged changes is skipped to preserve unstaged work
// ---------------------------------------------------------------------------
test('format-staged: file with staged+unstaged changes is skipped; unstaged edits preserved', () => {
  const repo = initRepo();
  const binDir = mkTemp('fmtstaged-bin-');
  writeStub(binDir, 'gofmt', 'stub-gofmt');

  // Create and commit an initial version of the file
  const goFile = path.join(repo, 'mixed.go');
  fs.writeFileSync(goFile, 'package main\n');
  execFileSync('git', ['-C', repo, 'add', 'mixed.go']);
  execFileSync('git', ['-C', repo, 'commit', '-q', '-m', 'add mixed.go']);

  // Stage a change to mixed.go
  fs.writeFileSync(goFile, 'package main\nfunc Staged() {}\n');
  execFileSync('git', ['-C', repo, 'add', 'mixed.go']);

  // Add further UNSTAGED change on top of the staged one
  const withUnstaged = 'package main\nfunc Staged() {}\nfunc Unstaged() {}\n';
  fs.writeFileSync(goFile, withUnstaged);

  // Record staged blob before
  const stagedBefore = stagedContent(repo, 'mixed.go');

  const result = runScript(repo, `${binDir}:${MINIMAL_PATH}`);

  assert.equal(result.code, 0, `must exit 0; stderr: ${result.stderr}`);

  // Working-tree file must still contain the unstaged function
  const wt = fs.readFileSync(goFile, 'utf8');
  assert.ok(wt.includes('func Unstaged()'), 'unstaged edits must be preserved in working tree');

  // Staged blob must NOT have been clobbered with unstaged content
  const stagedAfter = stagedContent(repo, 'mixed.go');
  assert.equal(
    stagedBefore,
    stagedAfter,
    'staged blob must be unchanged when file has unstaged edits',
  );
});

// ---------------------------------------------------------------------------
// g. A staged path containing whitespace is handled as ONE path; a colliding
//    untracked file (matching a word-split token) is never staged.
// ---------------------------------------------------------------------------
test('format-staged: whitespace filename is one path; colliding untracked file is not staged', () => {
  const repo = initRepo();
  const binDir = mkTemp('fmtstaged-bin-');
  writeStub(binDir, 'gofmt', 'stub-gofmt');

  // Stage a Go file whose name contains a space.
  fs.writeFileSync(path.join(repo, 'foo bar.go'), 'package main\n');
  execFileSync('git', ['-C', repo, 'add', 'foo bar.go']);

  // An UNTRACKED file named after the second word-split token. If the helper
  // split "foo bar.go" on the space it would format+stage this unrelated file.
  const untracked = 'package main\n// untracked, must stay out of the commit\n';
  fs.writeFileSync(path.join(repo, 'bar.go'), untracked);

  const result = runScript(repo, `${binDir}:${MINIMAL_PATH}`);
  assert.equal(result.code, 0, `must exit 0; stderr: ${result.stderr}`);

  // The spaced file WAS formatted and re-staged (proves newline-only splitting).
  assert.ok(
    stagedContent(repo, 'foo bar.go').includes('stub-gofmt'),
    'the whitespace-named staged file must be formatted',
  );

  // The colliding untracked file must NOT have been staged…
  let barTracked = true;
  try {
    execFileSync('git', ['-C', repo, 'ls-files', '--error-unmatch', 'bar.go'], { stdio: 'pipe' });
  } catch {
    barTracked = false;
  }
  assert.equal(barTracked, false, 'colliding untracked file must not be staged');
  // …nor formatted.
  assert.equal(
    fs.readFileSync(path.join(repo, 'bar.go'), 'utf8'),
    untracked,
    'colliding untracked file must not be modified',
  );
});

// ---------------------------------------------------------------------------
// h. A staged symlink is skipped — the formatter must not write through it to
//    its target (a file outside the staged set).
// ---------------------------------------------------------------------------
test('format-staged: staged symlink is skipped; its target is not modified', () => {
  const repo = initRepo();
  const binDir = mkTemp('fmtstaged-bin-');
  writeStub(binDir, 'gofmt', 'stub-gofmt');

  // An untracked real target, and a staged symlink pointing at it.
  const target = 'package main\n// target outside the staged set\n';
  fs.writeFileSync(path.join(repo, 'target.go'), target);
  fs.symlinkSync('target.go', path.join(repo, 'link.go'));
  execFileSync('git', ['-C', repo, 'add', 'link.go']);

  const result = runScript(repo, `${binDir}:${MINIMAL_PATH}`);
  assert.equal(result.code, 0, `must exit 0; stderr: ${result.stderr}`);

  // The symlink target must be byte-for-byte unchanged (not written through).
  assert.equal(
    fs.readFileSync(path.join(repo, 'target.go'), 'utf8'),
    target,
    'symlink target must not be modified',
  );
});
