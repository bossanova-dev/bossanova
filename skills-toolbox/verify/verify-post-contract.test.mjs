import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  decideVerdict,
  parsePostOptions,
  postOptionsFromArgv,
  VERIFY_VERDICTS,
} from './verify-gate.mjs'
import { postArgv } from './verify-post-files.mjs'
import { sweep } from './verify-route.mjs'

const HEAD = 'a'.repeat(40)
const POLICY = '0123456789abcdef'
const PR = 12030
const REPO = 'o/r'
const facts = (extra = {}) => ({
  changedPaths: [],
  judgeAreas: [],
  prState: 'OPEN',
  isDraft: false,
  headSha: HEAD,
  receipt: 'success',
  ci: { settled: true, green: true },
  extensions: [],
  // A repo whose merge rule lets a high head merge.
  mergePolicy: { auto: [{ level: 'high' }] },
  ...extra,
})
const judge = (extra = {}) => ({
  ...decideVerdict(facts(extra)),
  pr: PR,
  policyFingerprint: POLICY,
})

function fixture(t, judged, { failPosts = false } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'verify-post-contract-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const posts = []
  const errors = []
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  const run = (cmd, args) => {
    if (cmd === '/fake/boss' && args[0] === 'ls') return ok({ sessions: [] })
    if (cmd === 'gh') return { ok: false, error: 'offline', exit: 1 }
    assert.equal(cmd, 'node', `unexpected command ${cmd}`)
    switch (args[1]) {
      case 'candidates':
        return ok({
          candidates: [
            { pr: PR, headSha: HEAD, repo: REPO, url: `https://github.com/${REPO}/pull/${PR}` },
          ],
          skipped: [],
          mergeRetries: [],
        })
      case 'judge':
        return ok(judged)
      case 'post': {
        try {
          const options = postOptionsFromArgv(args.slice(2))
          const parsed = parsePostOptions(options)
          posts.push({ args: args.slice(2), options, parsed })
          if (failPosts && options.reason !== 'verify-post-failed')
            return { ok: false, error: 'exit-2', exit: 2, stderr: 'post transport refused' }
          return ok({ posted: true, won: true, token: 'contract-token' })
        } catch (error) {
          errors.push(error.message)
          return { ok: false, error: 'exit-2', exit: 2, stderr: error.message }
        }
      }
      default:
        assert.fail(`unexpected gate verb ${args[1]}`)
    }
  }
  return {
    posts,
    errors,
    tick(dryRun = true) {
      return sweep(
        { dryRun },
        {
          run,
          nodeBin: 'node',
          now: () => 1_000_000,
          resolveBoss: () => ({ ok: true, path: '/fake/boss' }),
          readProjectConfig: () => null,
          env: { PATH: '/usr/bin', REPO_DIR: dir },
          cwd: dir,
          logDir: dir,
          stateFile: join(dir, 'sweep-state.json'),
        },
      )
    },
  }
}

for (const scenario of [
  {
    name: 'human park',
    changes: { areas: [{ area: 'verify-policy', label: 'Verify policy', defaultAsk: 'always' }] },
    verdict: 'human',
  },
  {
    name: 'failed check',
    changes: {
      ci: { settled: true, green: false, state: 'failing' },
      failedChecks: [{ name: 'ci' }],
    },
    verdict: 'blocked',
  },
  {
    name: 'merge conflict',
    changes: { eligibilityReasons: ['merge-state-dirty'] },
    verdict: 'blocked',
  },
  { name: 'verified head', changes: {}, verdict: 'pass' },
  {
    name: 'stale finding',
    changes: {
      reportHead: 'b'.repeat(40),
      reviewFindings: [
        {
          severity: 'P2',
          verified: true,
          introduced: true,
          file: 'app.js',
          line: 1,
          title: 'error',
          evidence: 'failure',
        },
      ],
    },
    verdict: 'resolution-required',
  },
  { name: 'unjudged head', changes: { judgeAreas: undefined }, verdict: 'judge-required' },
  {
    name: '#12030 boss head without a receipt',
    changes: { receipt: 'absent', prClass: 'boss' },
    verdict: 'review-required',
  },
  {
    name: 'installed extensions',
    changes: { extensions: [{ name: 'proof' }] },
    verdict: 'extensions-required',
  },
]) {
  test(`sweep post contract: ${scenario.name}`, (t) => {
    const judged = judge(scenario.changes)
    assert.equal(judged.verdict, scenario.verdict)
    const w = fixture(t, judged)
    const result = w.tick()
    assert.deepEqual(w.errors, [])
    assert.equal(w.posts.length, 1)
    const { args, options, parsed } = w.posts[0]
    const claimed = [
      'review-required',
      'extensions-required',
      'judge-required',
      'resolution-required',
    ].includes(judged.verdict)
    assert.equal(parsed.verdict, claimed ? 'claim' : judged.verdict)
    assert.equal(parsed.pr, PR)
    assert.equal(parsed.headSha, HEAD)
    assert.equal(parsed.dryRun, true)
    assert.equal(options.repo, REPO)
    assert.equal(args.includes('--policy'), judged.verdict === 'human')
    if (judged.verdict === 'human') assert.equal(options.policy, POLICY)
    if (judged.verdict === 'blocked') {
      assert.deepEqual(parsed.blockers.blockers, judged.blockers)
      assert.equal(options.reason, judged.reason)
    }
    if (claimed) {
      assert.equal(result.records[0].outcome, 'dispatched')
      assert.equal(args.includes('--blockers'), false)
    }
    assert.equal(result.records[0].line.includes('post failed'), false)
  })
}

test('gate still rejects a policy fingerprint on a blocked post', (t) => {
  const w = fixture(t, judge({ eligibilityReasons: ['merge-state-dirty'] }))
  w.tick()
  assert.equal(w.posts.length, 1)
  const args = [...w.posts[0].args, '--policy', POLICY]
  assert.throws(() => parsePostOptions(postOptionsFromArgv(args)), /with --verdict human only/)
})

test('failed posts escalate through argv accepted by the strict gate', (t) => {
  const judged = judge({ eligibilityReasons: ['merge-state-dirty'] })
  const w = fixture(t, judged, { failPosts: true })
  w.tick(false)
  w.tick(false)
  w.tick(false)
  assert.deepEqual(w.errors, [])
  assert.deepEqual(
    w.posts.map(({ parsed }) => parsed.verdict),
    ['blocked', 'blocked', 'blocked', 'human'],
  )
  const escalation = w.posts.at(-1)
  assert.equal(escalation.options.reason, 'verify-post-failed')
  assert.equal(escalation.options.policy, POLICY)
  assert.deepEqual(escalation.parsed.humanCodes, ['verify-post-failed'])
  const notes = JSON.parse(readFileSync(escalation.options.notes, 'utf8'))
  assert.equal(notes.length, 1)
  assert.match(JSON.stringify(notes), /post transport refused/)
})

test('the shared argv builder preserves explicit escalation overrides', () => {
  const args = postArgv({
    pr: PR,
    head: HEAD,
    judged: judge(),
    verdict: 'human',
    reason: 'verify-post-failed',
    repoArgs: ['--repo', REPO],
    dry: ['--dry-run'],
  })
  assert.equal(args[0], 'post')
  const options = postOptionsFromArgv(args.slice(1))
  assert.equal(options.reason, 'verify-post-failed')
  assert.equal(options.policy, POLICY)
  assert.equal(parsePostOptions(options).verdict, 'human')
})

test('every gate verdict has an explicit sweep post or deliberate nonpost route', (t) => {
  const routes = {
    human: 'human',
    blocked: 'blocked',
    pass: 'pass',
    'review-required': 'claim',
    'extensions-required': 'claim',
    'judge-required': 'claim',
    'resolution-required': 'claim',
    wait: null,
    defect: null,
    reverify: null,
  }
  assert.deepEqual([...VERIFY_VERDICTS].sort(), Object.keys(routes).sort())
  for (const verdict of VERIFY_VERDICTS.filter((value) => routes[value] === null)) {
    // wait comes from the judge; defect and reverify belong to other gate operations.
    const judged = verdict === 'wait' ? judge({ prState: 'CLOSED' }) : { ...judge(), verdict }
    assert.equal(judged.verdict, verdict)
    const w = fixture(t, judged)
    const result = w.tick()
    assert.deepEqual(w.errors, [])
    assert.deepEqual(w.posts, [])
    assert.equal(result.records[0].outcome, 'skipped')
    assert.match(result.records[0].line, verdict === 'wait' ? /wait / : /unexpected verdict/)
  }
})
