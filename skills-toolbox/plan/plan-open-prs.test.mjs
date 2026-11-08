import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, writeFileSync, chmodSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { GhError } from '../tracker/github-gh.mjs'
import {
  fetchOpenPullRequests,
  parseTicketIds,
  openPullRequestOverlaps,
  validateOpenPrsSnapshot,
} from './plan-open-prs.mjs'

const CLI = fileURLToPath(new URL('./plan-open-prs.mjs', import.meta.url))
const pr = (over = {}) => ({
  number: 12,
  url: 'https://example.invalid/pull/12',
  title: '[ABC-2] Handler',
  headRefName: 'build-2',
  isDraft: true,
  files: ['app/api/x.go'],
  filesTruncated: false,
  ticketIds: ['ABC-2'],
  ...over,
})
const snapshot = (...pullRequests) => ({ listTruncated: false, pullRequests })
const classify = (value, extra = {}) =>
  openPullRequestOverlaps({
    subjectIdentifier: 'ABC-1',
    subjectAreas: ['app/api/x.go'],
    snapshot: value,
    candidateIdentifiers: ['ABC-2'],
    ...extra,
  })

test('fetch extracts unique title and branch ticket tokens and labels its single gh read', () => {
  assert.deepEqual(parseTicketIds({ title: '[abc-2] ABC-2 and Z9-3', headRefName: 'fix/ABC-4' }), [
    'ABC-2',
    'Z9-3',
    'ABC-4',
  ])
  let observed
  const result = fetchOpenPullRequests({
    env: { KEEP: 'yes' },
    gh: (args, options) => {
      observed = { args, options }
      return [
        {
          ...pr(),
          title: '[abc-2] Handler',
          headRefName: 'fix/ABC-3',
          files: [{ path: 'app/api/x.go' }],
        },
      ]
    },
  })
  assert.deepEqual(observed.args, [
    'pr',
    'list',
    '--state',
    'open',
    '--limit',
    '100',
    '--json',
    'number,title,url,headRefName,isDraft,files',
  ])
  assert.equal(observed.options.env.BOSS_GH_USAGE_LABEL, 'fallback:plan-open-prs')
  assert.equal(observed.options.env.KEEP, 'yes')
  assert.deepEqual(result.pullRequests[0].ticketIds, ['ABC-2', 'ABC-3'])
  assert.deepEqual(result.pullRequests[0].files, ['app/api/x.go'])
  assert.equal(result.listTruncated, false)
})

test('fetch reports list and per-PR file caps and propagates gh failure', () => {
  const result = fetchOpenPullRequests({
    limit: 1,
    gh: () => [
      { ...pr(), files: Array.from({ length: 100 }, (_, i) => ({ path: `app/${i}.go` })) },
    ],
  })
  assert.equal(result.listTruncated, true)
  assert.equal(result.pullRequests[0].filesTruncated, true)
  assert.throws(
    () =>
      fetchOpenPullRequests({
        gh: () => {
          throw new GhError('rate-limited', 'quota exhausted')
        },
      }),
    /quota exhausted/,
  )
})

test('classifier drops own PRs, folds candidate PR files, and records their source', () => {
  const result = classify(snapshot(pr({ ticketIds: ['ABC-1', 'ABC-2'] }), pr()))
  assert.deepEqual(result.foldIn, { 'ABC-2': ['app/api/x.go'] })
  assert.equal(result.notes.length, 1)
  assert.equal(result.notes[0].severity, 'info')
  assert.equal(result.notes[0].destination, 'planning')
  assert.match(result.notes[0].text, /#12.*app\/api\/x.go/)
  assert.deepEqual(result.skipped, [])
})

test('classifier routes unfetched ids and unmapped or foreign-prefix PRs without a write', () => {
  const missing = classify(snapshot(pr({ ticketIds: ['ABC-8'] })))
  assert.deepEqual(missing.skipped, [{ id: 'ABC-8', reason: 'open-pr-ticket-unfetched', pr: 12 }])
  assert.deepEqual(missing.foldIn, {})
  for (const ids of [[], ['FOREIGN-1']]) {
    const result = classify(snapshot(pr({ ticketIds: ids })))
    assert.deepEqual(result.foldIn, {})
    assert.deepEqual(result.skipped, [])
    assert.equal(result.notes[0].destination, 'risks')
    assert.equal(result.notes[0].severity, 'warning')
    assert.equal(result.notes[0].level, 'file')
    assert.deepEqual(result.notes[0].shared, ['app/api/x.go'])
    assert.equal(result.notes[0].write, undefined)
  }
  const directory = classify(snapshot(pr({ ticketIds: [], files: ['app/api'] })))
  assert.equal(directory.notes[0].level, 'directory')
})

test('classifier obeys overlap options and warns on unseen truncated work', () => {
  const result = classify({
    listTruncated: true,
    pullRequests: [pr({ files: ['elsewhere/x.go'], filesTruncated: true })],
  })
  assert.equal(result.notes[0].reason, 'open-prs-truncated')
  const aliased = classify(snapshot(pr({ files: ['generated/x.go'] })), {
    overlapOptions: { areaAliases: { 'generated/x.go': ['app/api/x.go'] } },
  })
  assert.deepEqual(aliased.foldIn, { 'ABC-2': ['generated/x.go'] })
  assert.deepEqual(
    classify(snapshot(pr()), { overlapOptions: { repoWideTokens: ['app/api'] } }).foldIn,
    {},
  )
})

test('snapshot validation refuses malformed rows and accepts the fetch shape', () => {
  assert.equal(validateOpenPrsSnapshot(snapshot(pr())), null)
  for (const bad of [
    null,
    [],
    {},
    { pullRequests: [] },
    snapshot({}),
    snapshot(pr({ files: [3] })),
    snapshot(pr({ ticketIds: 'ABC-2' })),
  ]) {
    assert.equal(typeof validateOpenPrsSnapshot(bad), 'string')
  }
})

test('overlap CLI lists overlapping PRs and candidate tickets before a subject description exists', () => {
  const dir = tempDir('plan-open-prs-')
  const file = join(dir, 'prs.json'),
    candidates = join(dir, 'candidates.json')
  writeFileSync(file, JSON.stringify(snapshot(pr())))
  writeFileSync(
    candidates,
    JSON.stringify([
      {
        identifier: 'ABC-3',
        title: 'Sibling',
        stateName: 'Todo',
        description: '## Key changes\n\n- `app/api/x.go`: change handler',
      },
      { identifier: 'ABC-1', description: '## Key changes\n\n- `app/api/x.go`' },
    ]),
  )
  const args = [CLI, 'overlap', '--open-prs', file, '--subject', 'ABC-1']
  const missing = spawnSync(process.execPath, args, { encoding: 'utf8' })
  assert.equal(missing.status, 2)
  const result = spawnSync(
    process.execPath,
    [...args, '--paths', 'app/api/x.go', '--candidates', candidates],
    { encoding: 'utf8' },
  )
  assert.equal(result.status, 0, result.stderr)
  const output = JSON.parse(result.stdout)
  assert.deepEqual(
    output.pullRequests.map((row) => row.number),
    [12],
  )
  assert.deepEqual(
    output.tickets.map((row) => row.identifier),
    ['ABC-3'],
  )
  assert.deepEqual(output.tickets[0].shared, ['app/api/x.go'])
})

test('fetch CLI writes a receipt only on success and leaves no snapshot on gh failure', () => {
  const dir = tempDir('plan-open-prs-fetch-'),
    out = join(dir, 'out.json')
  const gh = join(dir, 'gh')
  writeFileSync(gh, '#!/bin/sh\necho "HTTP 401: not logged in" >&2\nexit 1\n')
  chmodSync(gh, 0o755)
  const env = { ...process.env, PATH: `${dir}:${process.env.PATH}` }
  const failed = spawnSync(process.execPath, [CLI, 'fetch', '--out-file', out], {
    encoding: 'utf8',
    env,
  })
  assert.equal(failed.status, 1)
  assert.match(failed.stderr, /unauthenticated/)
  assert.equal(existsSync(out), false)
  writeFileSync(gh, '#!/bin/sh\nprintf "[]"\n')
  const success = spawnSync(process.execPath, [CLI, 'fetch', '--out-file', out, '--limit', '2'], {
    encoding: 'utf8',
    env,
  })
  assert.equal(success.status, 0, success.stderr)
  assert.deepEqual(JSON.parse(success.stdout), {
    count: 0,
    listTruncated: false,
    filesTruncatedCount: 0,
  })
  assert.equal(existsSync(out), true)
  assert.equal(
    spawnSync(process.execPath, [CLI, 'fetch', '--out-file', out, '--limit', '0']).status,
    2,
  )
})
