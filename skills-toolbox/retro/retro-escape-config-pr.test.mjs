import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { escapeRecord } from '../verify/verify-escapes.mjs'
import { escapeRows } from './retro-outcomes.mjs'
import { applyProposal, proposeAlwaysReview } from './retro-escape-config.mjs'
import { reviewPolicyFromBlock } from '../verify/verify-review-policy.mjs'
import { collect, main } from './retro-escape-config-pr.mjs'

const ok = (value) => ({ status: 0, stdout: JSON.stringify(value) })
const failure = (message) => ({ status: 1, stderr: message })
function fixture(t, options = {}) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'retro-config-test-'))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true, maxRetries: 3 }))
  const start = '2026-10-01T00:00:00Z',
    end = '2026-11-01T00:00:00Z'
  const records = Array.from({ length: options.count ?? 1 }, (_, i) => {
    const original = { number: i + 1, title: 'Original', mergedAt: start, files: [`app${i}/x.go`] }
    const fix = {
      number: i + 20,
      title: 'Fix',
      mergedAt: '2026-10-02T00:00:00Z',
      files: original.files,
    }
    return escapeRecord({
      repo: 'owner/repo',
      fix,
      link: { original, basis: 'explicit-reference', evidence: `Fixes #${i + 1}` },
      tracedAt: end,
    })
  })
  const section = options.section ?? escapeRows(records, { start, end })
  const outcomes = path.join(directory, 'outcomes.json')
  fs.writeFileSync(outcomes, JSON.stringify({ sections: { escapeRows: section } }))
  const calls = [],
    payloads = [],
    tempFiles = []
  let creates = 0,
    putCount = 0,
    lists = 0
  const branchText = options.resume
    ? applyProposal('{}', proposeAlwaysReview(section.rows, reviewPolicyFromBlock({})).proposals[0])
    : '{}'
  const file = (text) => ({
    type: 'file',
    encoding: 'base64',
    content: Buffer.from(text).toString('base64'),
    sha: 'blob-sha',
  })
  const run = (command, args) => {
    calls.push([command, ...args])
    assert.equal(command, 'gh')
    if (args[0] === 'repo') return ok({ nameWithOwner: 'owner/repo' })
    const endpoint = args[1]
    const methodIndex = args.indexOf('--method')
    const method = methodIndex < 0 ? 'GET' : args[methodIndex + 1]
    if (method !== 'GET') {
      const input = args[args.indexOf('--input') + 1]
      tempFiles.push(input)
      const payload = JSON.parse(fs.readFileSync(input, 'utf8'))
      payloads.push({ endpoint, method, payload })
      if (endpoint.endsWith('/git/refs'))
        return options.resume ? failure('Reference already exists (HTTP 422)') : ok({})
      if (endpoint.includes('/contents/')) {
        putCount++
        return options.failFirstPut && putCount === 1 ? failure('HTTP 500') : ok({})
      }
      if (endpoint.endsWith('/pulls')) {
        creates++
        return options.prRace
          ? failure('A pull request already exists (HTTP 422)')
          : ok({ number: 100 + creates, html_url: `https://example.test/pr/${creates}` })
      }
      throw new Error(`unexpected write ${endpoint}`)
    }
    if (endpoint === 'repos/owner/repo') return ok({ default_branch: 'trunk' })
    if (endpoint.includes('/git/ref/')) return ok({ object: { sha: 'a'.repeat(40) } })
    if (endpoint.includes('/contents/')) {
      if (options.badConfig) return failure('API rate limit exceeded')
      return options.absent
        ? failure('Not Found (HTTP 404)')
        : ok(file(endpoint.endsWith('a'.repeat(40)) ? '{}' : branchText))
    }
    if (endpoint.includes('/pulls?')) {
      lists++
      return ok(
        (options.closed && endpoint.includes('escape-1-20')) || (options.prRace && lists > 1)
          ? [{ number: 50, state: 'closed', html_url: 'https://example.test/pr/50' }]
          : [],
      )
    }
    throw new Error(`unexpected read ${endpoint}`)
  }
  return { outcomes, calls, payloads, tempFiles, deps: { run, stdout: () => {} } }
}

test('dry run prints proposals and config diffs using reads only', (t) => {
  const f = fixture(t, { count: 2 })
  const result = collect({ outcomes: f.outcomes, dryRun: true }, f.deps)
  assert.equal(result.proposals.length, 2)
  assert.ok(
    result.proposals.every((proposal) => proposal.diff.some((line) => line.startsWith('+'))),
  )
  assert.equal(f.payloads.length, 0)
  assert.equal(main(['open', '--outcomes', f.outcomes, '--dry-run'], f.deps), 0)
})

test('cap counts opened PRs, existing closed PRs stay closed and defer remaining proposals', (t) => {
  const f = fixture(t, { count: 6, closed: true })
  const result = collect({ outcomes: f.outcomes }, f.deps)
  assert.equal(result.opened.length, 3)
  assert.equal(result.existing[0].state, 'closed')
  assert.equal(result.deferred.length, 2)
  for (const { endpoint, payload } of f.payloads.filter((entry) =>
    entry.endpoint.endsWith('/pulls'),
  )) {
    assert.equal(endpoint, 'repos/owner/repo/pulls')
    assert.equal(payload.draft, false)
    assert.equal(payload.base, 'trunk')
  }
  assert.ok(f.tempFiles.every((file) => !fs.existsSync(file)))
})

test('resume skips committed config write and collapses concurrent PR creation', (t) => {
  const f = fixture(t, { resume: true, prRace: true })
  const result = collect({ outcomes: f.outcomes }, f.deps)
  assert.equal(result.existing[0].number, 50)
  assert.equal(result.opened.length, 0)
  assert.equal(f.payloads.filter((entry) => entry.method === 'PUT').length, 0)
})

test('max can lower the run limit but cannot exceed the three-PR ceiling', (t) => {
  const lower = fixture(t, { count: 5 })
  assert.equal(collect({ outcomes: lower.outcomes, max: 1 }, lower.deps).opened.length, 1)
  const higher = fixture(t, { count: 5 })
  assert.equal(collect({ outcomes: higher.outcomes, max: 100 }, higher.deps).opened.length, 3)
})

test('base failures and unavailable escapes perform no writes', (t) => {
  const unreadable = fixture(t, { badConfig: true })
  assert.equal(collect({ outcomes: unreadable.outcomes }, unreadable.deps).status, 'failed')
  assert.equal(unreadable.payloads.length, 0)
  const skipped = fixture(t, { section: { status: 'failed', reason: 'tracer-missing' } })
  assert.equal(collect({ outcomes: skipped.outcomes }, skipped.deps).reason, 'no-escapes')
  assert.equal(skipped.calls.length, 0)
})

test('one failed write does not stop later proposals and missing config creates file', (t) => {
  const f = fixture(t, { count: 2, failFirstPut: true, absent: true })
  const result = collect({ outcomes: f.outcomes }, f.deps)
  assert.equal(result.failures.length, 1)
  assert.equal(result.opened.length, 1)
  const puts = f.payloads.filter((entry) => entry.method === 'PUT')
  assert.ok(puts.every((entry) => !Object.hasOwn(entry.payload, 'sha')))
})

test('usage errors exit 64 before reads and unreadable outcomes exit nonzero', (t) => {
  const f = fixture(t)
  for (const args of [
    ['open', '--wat'],
    ['open'],
    ['open', '--outcomes', f.outcomes, '--max', '-1'],
  ])
    assert.equal(main(args, { ...f.deps, stderr: () => {} }), 64)
  assert.equal(f.calls.length, 0)
  assert.equal(main(['open', '--outcomes', '/missing'], { ...f.deps, stderr: () => {} }), 1)
})
