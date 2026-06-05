import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const scriptPath = fileURLToPath(new URL('./check-public-mirror-workflows.mjs', import.meta.url));

const requiredPublicWorkflows = [
  '.github/workflows/ci.yml',
  '.github/workflows/test-boss.yml',
  '.github/workflows/test-bossd.yml',
  '.github/workflows/test-scripts.yml',
  '.github/workflows/test-lib-bossalib.yml',
  '.github/workflows/test-proto.yml',
  '.github/workflows/test-plugin-claude.yml',
  '.github/workflows/test-plugin-codex.yml',
  '.github/workflows/test-plugin-dependabot.yml',
  '.github/workflows/test-plugin-linear.yml',
  '.github/workflows/test-plugin-repair.yml',
];

function withMirrorWorkflow(content, callback) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mirror-workflow-'));
  fs.mkdirSync(path.join(dir, '.github/workflows'), { recursive: true });
  fs.writeFileSync(path.join(dir, '.github/workflows/mirror-public.yml'), content);

  try {
    callback(dir);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

function mirrorWorkflowWith(extraLines) {
  return [
    ...requiredPublicWorkflows,
    'AGENTS.md',
    'CLAUDE.md',
    'bossd-plugin-repair',
    '.env.example.public',
    'scripts/check-mirror-leaks.sh',
    '--force-with-lease',
    ...extraLines,
  ].join('\n');
}

test('requires .env.example as a distinct filename token', () => {
  withMirrorWorkflow(mirrorWorkflowWith([]), (dir) => {
    assert.throws(
      () => execFileSync('node', [scriptPath], { cwd: dir, encoding: 'utf8' }),
      (error) => {
        assert.equal(error.status, 1);
        assert.match(error.stderr, /\.env\.example/);
        return true;
      },
    );
  });
});

test('accepts mirror workflow when private and public env example filenames are both wired', () => {
  withMirrorWorkflow(mirrorWorkflowWith(['.env.example']), (dir) => {
    const output = execFileSync('node', [scriptPath], { cwd: dir, encoding: 'utf8' });

    assert.match(output, /Public mirror workflows OK/);
  });
});
