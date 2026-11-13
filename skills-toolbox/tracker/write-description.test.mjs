import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { runCli } from './cli.mjs'
import { applyDescriptionPatch } from './description-patch.mjs'
import { buildLinearOperationMap } from './linear.mjs'
import { createGithubAdapter } from './github.mjs'

const base = '## Planning\n- existing\n\n## Original notes\n' + 'source notes\n'.repeat(100)
const body = base.replace('- existing\n', '- existing\n- dependency ✅\n')
const replacement = { tool: 'mcp__stub__save_issue', summary: '{id, description} -> saved' }
const patch = { tool: 'mcp__stub__save_issue', summary: '{id, patch} -> saved' }

function fixture(run) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'description-write-'))
  try {
    fs.writeFileSync(path.join(dir, 'base.md'), base)
    fs.writeFileSync(path.join(dir, 'body.md'), body)
    run(dir)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
}

function invoke(dir, operations, extra = ['--base-file', path.join(dir, 'base.md')]) {
  let out = '',
    err = ''
  const code = runCli(
    ['write-description', '--id', 'ISSUE-1', '--body-file', path.join(dir, 'body.md'), ...extra],
    {
      write: (s) => {
        out += s
      },
      errWrite: (s) => {
        err += s
      },
      resolveAdapter: () => ({ operationMap: operations }),
    },
  )
  return { code, out, err, descriptor: out ? JSON.parse(out) : null }
}

test('CLI emits a verified patch descriptor and the body file byte count', () =>
  fixture((dir) => {
    const result = invoke(dir, { writeDescription: replacement, patchDescription: patch })
    assert.equal(result.code, 0)
    const d = result.descriptor
    assert.equal(d.mode, 'patch')
    assert.equal(d.tool, patch.tool)
    assert.equal(d.args.id, 'ISSUE-1')
    assert.equal(d.args.description, undefined)
    assert.equal(applyDescriptionPatch(base, d.args.patch), body)
    assert.equal(d.bytes, fs.statSync(path.join(dir, 'body.md')).size)
    assert.equal(d.patchBytes, Buffer.byteLength(JSON.stringify(d.args.patch)))
    assert.equal(d.outcome, 'descriptor-emitted')
  }))

test('missing capability or unhelpful diff emits full replacement with fallback reason', () =>
  fixture((dir) => {
    const result = invoke(dir, { writeDescription: replacement })
    assert.equal(result.code, 0)
    assert.equal(result.descriptor.mode, 'replace')
    assert.equal(result.descriptor.patchFallback, 'no-patchDescription-op')
    assert.deepEqual(result.descriptor.args, { id: 'ISSUE-1', description: body })
    fs.writeFileSync(path.join(dir, 'base.md'), body)
    const identical = invoke(dir, { writeDescription: replacement, patchDescription: patch })
    assert.equal(identical.descriptor.mode, 'replace')
    assert.equal(identical.descriptor.patchFallback, 'identical')
  }))

test('no base preserves the replacement descriptor apart from mode', () =>
  fixture((dir) => {
    const result = invoke(dir, { writeDescription: replacement, patchDescription: patch }, [])
    assert.deepEqual(result.descriptor, {
      tool: replacement.tool,
      args: { id: 'ISSUE-1', description: body },
      bytes: fs.statSync(path.join(dir, 'body.md')).size,
      mode: 'replace',
      outcome: 'descriptor-emitted',
    })
  }))

test('unreadable and invalid UTF-8 bases exit 2 with one diagnostic and no descriptor', () =>
  fixture((dir) => {
    fs.writeFileSync(path.join(dir, 'invalid.md'), Buffer.from([0xff]))
    for (const name of ['missing.md', 'invalid.md']) {
      const result = invoke(dir, { writeDescription: replacement }, [
        '--base-file',
        path.join(dir, name),
      ])
      assert.equal(result.code, 2)
      assert.equal(result.out, '')
      assert.equal(result.err.trim().split('\n').length, 1)
      assert.match(result.err, /--base-file/)
    }
  }))

test('Linear declares the optional patch operation while GitHub omits it', () => {
  assert.match(buildLinearOperationMap('stub').patchDescription.summary, /^\{id, patch\}/)
  assert.equal(buildLinearOperationMap('stub').patchDescription.tool, 'mcp__stub__save_issue')
  assert.equal(
    createGithubAdapter({ config: { trackerConfig: { github: { repository: 'owner/repo' } } } })
      .operationMap.patchDescription,
    undefined,
  )
})

test('link-only anchor neighbourhood emits a byte-exact replacement descriptor', () =>
  fixture((dir) => {
    const linkedBase = '- [A-1](u1)\n- [A-2](u2)\n' + base
    const intended = linkedBase.replace('- [A-2]', '- added\n- [A-2]')
    fs.writeFileSync(path.join(dir, 'base.md'), linkedBase)
    fs.writeFileSync(path.join(dir, 'body.md'), intended)
    const result = invoke(dir, { writeDescription: replacement, patchDescription: patch })
    assert.equal(result.code, 0)
    assert.equal(result.descriptor.mode, 'replace')
    assert.equal(result.descriptor.patchFallback, 'link-anchor')
    assert.deepEqual(result.descriptor.args, { id: 'ISSUE-1', description: intended })
  }))
