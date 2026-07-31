import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const scriptPath = fileURLToPath(new URL('./check-public-mirror-workflows.mjs', import.meta.url))

const requiredPublicWorkflows = [
  '.github/workflows/ci.yml',
  '.github/workflows/test-proto.yml',
  '.github/workflows/test-scripts.yml',
  '.github/workflows/test-go.yml',
]

function withMirrorWorkflow(content, callback) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mirror-workflow-'))
  fs.mkdirSync(path.join(dir, '.github/workflows'), { recursive: true })
  fs.writeFileSync(path.join(dir, '.github/workflows/mirror-public.yml'), content)

  try {
    callback(dir)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
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
  ].join('\n')
}

// The merge sequence the guard requires: wait for the mirror PR's checks, merge
// synchronously, then verify that public main received the marker. Each option
// below breaks exactly one leg of it.
function mergeSequence({ wait = 'before', auto = false, marker = 'after' } = {}) {
  const waitLine =
    'gh pr checks "$MIRROR_PR_NUMBER" --repo bossanova-dev/bossanova --json name,state'
  const mergeLines = [
    'gh pr merge "$MIRROR_PR_NUMBER" \\',
    '  --repo bossanova-dev/bossanova \\',
    ...(auto ? ['  --auto \\'] : []),
    '  --rebase \\',
    '  --delete-branch',
  ]
  const markerLine =
    'ACTUAL_MIRROR_SHA=$(git show public/main:.last-mirror-sha 2>/dev/null || true)'

  return [
    ...(wait === 'before' ? [waitLine] : []),
    ...(marker === 'before' ? [markerLine] : []),
    ...mergeLines,
    ...(wait === 'after' ? [waitLine] : []),
    ...(marker === 'after' ? [markerLine] : []),
  ]
}

function assertMirrorWorkflowRejected(lines, expected) {
  withMirrorWorkflow(mirrorWorkflowWith(lines), (dir) => {
    assert.throws(
      () => execFileSync('node', [scriptPath], { cwd: dir, encoding: 'utf8' }),
      (error) => {
        assert.equal(error.status, 1)
        assert.match(error.stderr, expected)
        return true
      },
    )
  })
}

test('requires .env.example as a distinct filename token', () => {
  withMirrorWorkflow(mirrorWorkflowWith([]), (dir) => {
    assert.throws(
      () => execFileSync('node', [scriptPath], { cwd: dir, encoding: 'utf8' }),
      (error) => {
        assert.equal(error.status, 1)
        assert.match(error.stderr, /\.env\.example/)
        return true
      },
    )
  })
})

test('accepts mirror workflow when private and public env example filenames are both wired', () => {
  withMirrorWorkflow(
    mirrorWorkflowWith(['.env.example', '--state open', ...mergeSequence()]),
    (dir) => {
      const output = execFileSync('node', [scriptPath], { cwd: dir, encoding: 'utf8' })

      assert.match(output, /Public mirror workflows OK/)
    },
  )
})

test('rejects merging the mirror pull request without waiting for its checks', () => {
  assertMirrorWorkflowRejected(
    ['.env.example', '--state open', ...mergeSequence({ wait: 'none' })],
    /no `gh pr checks` wait found/,
  )
})

test('rejects a checks wait that runs after the merge', () => {
  assertMirrorWorkflowRejected(
    ['.env.example', '--state open', ...mergeSequence({ wait: 'after' })],
    /`gh pr checks` wait runs after `gh pr merge`/,
  )
})

test('rejects an auto-merge, which has not landed when the marker check runs', () => {
  assertMirrorWorkflowRejected(
    ['.env.example', '--state open', ...mergeSequence({ auto: true })],
    /carries `--auto`/,
  )
})

test('rejects marker verification that runs before the merge', () => {
  assertMirrorWorkflowRejected(
    ['.env.example', '--state open', ...mergeSequence({ marker: 'before' })],
    /verification does not run after `gh pr merge`/,
  )
})

test('requires lookup of an open mirror pull request', () => {
  withMirrorWorkflow(mirrorWorkflowWith(['.env.example']), (dir) => {
    assert.throws(
      () => execFileSync('node', [scriptPath], { cwd: dir, encoding: 'utf8' }),
      (error) => {
        assert.equal(error.status, 1)
        assert.match(error.stderr, /--state open/)
        return true
      },
    )
  })
})

test('requires verification that public main received the mirror marker', () => {
  withMirrorWorkflow(mirrorWorkflowWith(['.env.example', '--state open']), (dir) => {
    assert.throws(
      () => execFileSync('node', [scriptPath], { cwd: dir, encoding: 'utf8' }),
      (error) => {
        assert.equal(error.status, 1)
        assert.match(error.stderr, /public\/main:\.last-mirror-sha/)
        return true
      },
    )
  })
})
