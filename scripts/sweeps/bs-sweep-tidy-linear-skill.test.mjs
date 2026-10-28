// Offline behaviour tests for the tidy-linear worker gate's inverted contract.

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const gatePath = path.join(repoRoot, '.claude/skills/bs-sweep-tidy-linear/gate/gate.mjs')

function runGate(dir, fixture) {
  fs.copyFileSync(path.join(repoRoot, '.boss-skills.json'), path.join(dir, '.boss-skills.json'))
  const env = { PATH: dir, TMPDIR: dir }
  if (fixture) {
    env.BS_TIDY_FIXTURE = path.join(dir, 'fixture.json')
    fs.writeFileSync(env.BS_TIDY_FIXTURE, JSON.stringify(fixture))
  }
  return spawnSync(process.execPath, [gatePath, '--dry-run'], {
    cwd: dir,
    env,
    encoding: 'utf8',
  })
}

function emptyFixture() {
  return {
    prs: [],
    data: {
      workflowStates: { nodes: [{ id: 'done', name: 'Done', type: 'completed' }] },
      issues: { nodes: [], pageInfo: { hasNextPage: false } },
    },
  }
}

test('tidy gate skips when nothing needs tidying or escalation', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-sweep-tidy-empty-'))
  try {
    const result = runGate(dir, emptyFixture())
    assert.equal(result.status, 1, result.stderr)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('tidy gate wakes an agent with a residual ambiguous case', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-sweep-tidy-escalate-'))
  try {
    const fixture = emptyFixture()
    fixture.data.issues.nodes.push({
      id: 'conflict',
      identifier: 'BOS-9',
      state: { id: 'progress', name: 'In Progress', type: 'started' },
      children: { nodes: [] },
    })
    fixture.prs = [
      { number: 90, title: '[BOS-9] merged', state: 'MERGED' },
      { number: 91, title: '[BOS-9] closed', state: 'CLOSED' },
    ]
    const result = runGate(dir, fixture)
    assert.equal(result.status, 0, result.stderr)
    const payload = JSON.parse(
      fs.readFileSync(path.join(dir, 'bs-sweep-tidy-linear-escalations.json'), 'utf8'),
    )
    assert.equal(payload.escalations.length, 1)
    assert.equal(payload.escalations[0].kind, 'close-conflict')
    assert.equal(payload.escalations[0].identifier, 'BOS-9')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('tidy gate reports evaluation failure when Linear credentials are unavailable', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-sweep-tidy-unavailable-'))
  try {
    const result = runGate(dir)
    assert.equal(result.status, 69)
    assert.ok(result.stderr.trim())
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})
