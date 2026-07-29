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
    mirrorWorkflowWith(['.env.example', '--state open', 'public/main:.last-mirror-sha']),
    (dir) => {
      const output = execFileSync('node', [scriptPath], { cwd: dir, encoding: 'utf8' })

      assert.match(output, /Public mirror workflows OK/)
    },
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
