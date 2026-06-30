#!/usr/bin/env node

import assert from 'node:assert/strict'
import test from 'node:test'

import {
  buildReportUrl,
  formatReportTimestamp,
  proofReportDestPath,
  publishProofReport,
  pushWithRebaseRetry,
  reportTarget,
} from './proof-publish-report.mjs'

// ── formatReportTimestamp ────────────────────────────────────────────────────

test('formatReportTimestamp converts ISO string to YYYYMMDD-HHMMSS', () => {
  assert.equal(formatReportTimestamp('2026-06-23T08:44:48.123Z'), '20260623-084448')
})

test('formatReportTimestamp handles midnight and zero fields', () => {
  assert.equal(formatReportTimestamp('2024-01-05T00:00:00.000Z'), '20240105-000000')
})

test('formatReportTimestamp throws on malformed input', () => {
  assert.throws(() => formatReportTimestamp('not-a-date'), /malformed/i)
  assert.throws(() => formatReportTimestamp(''), /malformed/i)
  assert.throws(() => formatReportTimestamp(null), /malformed/i)
})

// ── reportTarget ─────────────────────────────────────────────────────────────

test('reportTarget defaults to recurser/bs-proof when no env set', () => {
  const result = reportTarget({})
  assert.equal(result.enabled, true)
  assert.equal(result.repo, 'recurser/bs-proof')
})

test('reportTarget returns disabled when BOSS_PROOF_REPORT=0', () => {
  const result = reportTarget({ BOSS_PROOF_REPORT: '0' })
  assert.equal(result.enabled, false)
  assert.equal(result.reason, 'disabled')
})

test('reportTarget returns disabled when BOSS_PROOF_REPORT_REPO is empty string', () => {
  const result = reportTarget({ BOSS_PROOF_REPORT_REPO: '' })
  assert.equal(result.enabled, false)
  assert.equal(result.reason, 'disabled')
})

test('reportTarget uses custom BOSS_PROOF_REPORT_REPO when set', () => {
  const result = reportTarget({ BOSS_PROOF_REPORT_REPO: 'myorg/my-proof' })
  assert.equal(result.enabled, true)
  assert.equal(result.repo, 'myorg/my-proof')
})

// ── proofReportDestPath ───────────────────────────────────────────────────────

test('proofReportDestPath builds pr-N path for numeric prNumber', () => {
  const result = proofReportDestPath({
    owner: 'recurser',
    sourceRepo: 'bossanova',
    prNumber: '788',
    branch: 'main',
    timestamp: '20260623-084448',
    runId: 'abc123',
  })
  assert.equal(result, 'recurser-bossanova/pr-788/20260623-084448-abc123')
})

test('proofReportDestPath uses branch-slug fallback for prNumber local', () => {
  const result = proofReportDestPath({
    owner: 'recurser',
    sourceRepo: 'bossanova',
    prNumber: 'local',
    branch: 'dave/BOS-56-my-branch',
    timestamp: '20260623-084448',
    runId: 'abc123',
  })
  assert.equal(result, 'recurser-bossanova/branch-dave-bos-56-my-branch/20260623-084448-abc123')
})

test('proofReportDestPath uses branch-slug fallback for absent prNumber', () => {
  const result = proofReportDestPath({
    owner: 'recurser',
    sourceRepo: 'bossanova',
    prNumber: undefined,
    branch: 'feature/thing',
    timestamp: '20260623-084448',
    runId: 'run-1',
  })
  assert.equal(result, 'recurser-bossanova/branch-feature-thing/20260623-084448-run-1')
})

test('proofReportDestPath builds owner-repo seg from owner and sourceRepo', () => {
  const result = proofReportDestPath({
    owner: 'myorg',
    sourceRepo: 'myrepo',
    prNumber: '42',
    branch: 'main',
    timestamp: '20260101-120000',
    runId: 'r1',
  })
  assert.ok(result.startsWith('myorg-myrepo/'))
})

test('proofReportDestPath uses unknown fallback for empty branch', () => {
  const result = proofReportDestPath({
    owner: 'a',
    sourceRepo: 'b',
    prNumber: 'local',
    branch: '',
    timestamp: '20260101-120000',
    runId: 'r1',
  })
  assert.equal(result, 'a-b/branch-unknown/20260101-120000-r1')
})

// ── buildReportUrl ────────────────────────────────────────────────────────────

test('buildReportUrl builds expected GitHub blob URL', () => {
  const url = buildReportUrl({
    repo: 'recurser/bs-proof',
    destPath: 'recurser-bossanova/pr-788/20260623-084448-abc123',
  })
  assert.equal(
    url,
    'https://github.com/recurser/bs-proof/blob/main/recurser-bossanova/pr-788/20260623-084448-abc123/README.md',
  )
})

// ── pushWithRebaseRetry ───────────────────────────────────────────────────────

test('pushWithRebaseRetry succeeds on first try', async () => {
  const calls = []
  const runGit = (args) => {
    calls.push(args)
    return { status: 0 }
  }
  const result = await pushWithRebaseRetry({ runGit })
  assert.equal(result.ok, true)
  assert.equal(result.attempts, 1)
  assert.deepEqual(calls, [['push', 'origin', 'HEAD:main']])
})

test('pushWithRebaseRetry retries with pull --rebase and succeeds on 3rd attempt', async () => {
  const calls = []
  let pushCount = 0
  const runGit = (args) => {
    calls.push(args)
    if (args[0] === 'push') {
      pushCount += 1
      return { status: pushCount < 3 ? 1 : 0 }
    }
    return { status: 0 }
  }
  const result = await pushWithRebaseRetry({ runGit })
  assert.equal(result.ok, true)
  assert.equal(result.attempts, 3)
  // pull --rebase should have been called between retries
  const pullCalls = calls.filter((a) => a[0] === 'pull')
  assert.equal(pullCalls.length, 2)
  assert.deepEqual(pullCalls[0], ['pull', '--rebase', 'origin', 'main'])
})

test('pushWithRebaseRetry returns ok:false after exhausting maxRetries', async () => {
  const calls = []
  const runGit = (args) => {
    calls.push(args)
    if (args[0] === 'push') return { status: 1 }
    return { status: 0 }
  }
  const result = await pushWithRebaseRetry({ runGit, maxRetries: 3 })
  assert.equal(result.ok, false)
  const pushCalls = calls.filter((a) => a[0] === 'push')
  assert.equal(pushCalls.length, 3)
  // pulls happen between attempts only — never after the last failed push
  const pullCalls = calls.filter((a) => a[0] === 'pull')
  assert.equal(pullCalls.length, 2)
})

// ── publishProofReport ────────────────────────────────────────────────────────

const baseManifest = {
  version: 1,
  generatedAt: '2026-06-23T08:44:48.000Z',
  commit: 'abc1234',
  prNumber: '788',
  runId: 'run-42',
  publicBaseUrl: 'https://proof.bossanova.dev/proof/bossanova/pr-788/abc1234/run-42/tok',
  publicLiveCapture: false,
  captures: [],
}

const baseIdentity = {
  owner: 'recurser',
  sourceRepo: 'bossanova',
  prNumber: '788',
  branch: 'main',
  runId: 'run-42',
}

test('publishProofReport returns skipped:true when disabled via env', async () => {
  let cloneCalled = false
  const result = await publishProofReport({
    manifest: baseManifest,
    identity: baseIdentity,
    env: { BOSS_PROOF_REPORT: '0' },
    deps: {
      cloneRepo: () => {
        cloneCalled = true
        return { status: 0 }
      },
    },
  })
  assert.equal(result.ok, false)
  assert.equal(result.skipped, true)
  assert.equal(result.reason, 'disabled')
  assert.equal(cloneCalled, false)
})

test('publishProofReport returns skipped:true when clone fails', async () => {
  let rmrfCalled = false
  const result = await publishProofReport({
    manifest: baseManifest,
    identity: baseIdentity,
    env: {},
    deps: {
      mkdtemp: () => '/tmp/fake-clone-dir',
      cloneRepo: () => ({ status: 1, stderr: 'not found' }),
      rmrf: () => {
        rmrfCalled = true
      },
      log: { warn: () => {} },
      runGit: () => ({ status: 0 }),
      writeFile: () => {},
      mkdirp: () => {},
    },
  })
  assert.equal(result.ok, false)
  assert.equal(result.skipped, true)
  assert.equal(result.reason, 'clone-failed')
  assert.equal(rmrfCalled, true)
})

test('publishProofReport happy path writes manifest.json and README.md and returns reportUrl', async () => {
  const written = {}
  const result = await publishProofReport({
    manifest: baseManifest,
    identity: baseIdentity,
    env: {},
    deps: {
      mkdtemp: () => '/tmp/fake-dir',
      cloneRepo: () => ({ status: 0 }),
      mkdirp: () => {},
      writeFile: (filePath, content) => {
        written[filePath] = content
      },
      runGit: () => ({ status: 0 }),
      rmrf: () => {},
      log: { warn: () => {} },
    },
  })
  assert.equal(result.ok, true)
  assert.equal(result.skipped, false)
  assert.equal(
    result.reportUrl,
    'https://github.com/recurser/bs-proof/blob/main/recurser-bossanova/pr-788/20260623-084448-run-42/README.md',
  )
  // manifest.json written
  const manifestPath = Object.keys(written).find((k) => k.endsWith('manifest.json'))
  assert.ok(manifestPath, 'manifest.json should be written')
  const parsed = JSON.parse(written[manifestPath])
  assert.equal(parsed.commit, 'abc1234')
  // README.md written with renderGallery content
  const readmePath = Object.keys(written).find((k) => k.endsWith('README.md'))
  assert.ok(readmePath, 'README.md should be written')
  assert.ok(written[readmePath].includes('PR 788'))
})

test('publishProofReport returns skipped:true when commit fails and cleans up tmp', async () => {
  let rmrfCalled = false
  const result = await publishProofReport({
    manifest: baseManifest,
    identity: baseIdentity,
    env: {},
    deps: {
      mkdtemp: () => '/tmp/fake-commit-dir',
      cloneRepo: () => ({ status: 0 }),
      mkdirp: () => {},
      writeFile: () => {},
      runGit: (args) => {
        if (args.includes('commit')) return { status: 1 }
        return { status: 0 }
      },
      rmrf: () => {
        rmrfCalled = true
      },
      log: { warn: () => {} },
    },
  })
  assert.equal(result.ok, false)
  assert.equal(result.skipped, true)
  assert.equal(result.reason, 'commit-failed')
  assert.equal(rmrfCalled, true)
})

test('publishProofReport commits with an inline author identity so fresh CI envs do not fail', async () => {
  let commitArgs = null
  const result = await publishProofReport({
    manifest: baseManifest,
    identity: baseIdentity,
    env: {},
    deps: {
      mkdtemp: () => '/tmp/fake-author-dir',
      cloneRepo: () => ({ status: 0 }),
      mkdirp: () => {},
      writeFile: () => {},
      runGit: (args) => {
        if (args.includes('commit')) commitArgs = args
        return { status: 0 }
      },
      rmrf: () => {},
      log: { warn: () => {} },
    },
  })
  assert.equal(result.ok, true)
  assert.ok(commitArgs, 'commit should run')
  // -c flags must precede the commit subcommand
  assert.ok(commitArgs.indexOf('-c') < commitArgs.indexOf('commit'))
  assert.ok(commitArgs.includes('user.name=bossanova-proof'))
  assert.ok(commitArgs.includes('user.email=proof@bossanova.dev'))
})

test('publishProofReport returns skipped:true when push is exhausted', async () => {
  const result = await publishProofReport({
    manifest: baseManifest,
    identity: baseIdentity,
    env: {},
    deps: {
      mkdtemp: () => '/tmp/fake-dir',
      cloneRepo: () => ({ status: 0 }),
      mkdirp: () => {},
      writeFile: () => {},
      runGit: (args) => {
        // commit succeeds, push always fails
        if (args[0] === 'push') return { status: 1 }
        return { status: 0 }
      },
      rmrf: () => {},
      log: { warn: () => {} },
    },
  })
  assert.equal(result.ok, false)
  assert.equal(result.skipped, true)
  assert.equal(result.reason, 'push-failed')
})
