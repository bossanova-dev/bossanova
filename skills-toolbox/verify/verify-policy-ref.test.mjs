import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  resolveOriginRef,
  trialMergeRefusal,
  trialPark,
  trialPolicyFor,
  trialPost,
  trialProofActions,
  trialHandBack,
  validPolicyRef,
} from './verify-policy-ref.mjs'
import { proofPageInput } from './verify-proof-publish.mjs'
import { parkStillRaised } from './verify-held-park.mjs'

const SHA_1 = '1'.repeat(40)
const SHA_2 = '2'.repeat(40)
const TRIAL = { ref: 'config/trial', sha: SHA_1 }

/** A reader whose session names `ref` and whose origin points the branch at `tips[n]` on call n. */
function world({ ref = 'config/trial', tips = [SHA_1], session } = {}) {
  const calls = []
  const read = (command, args) => {
    calls.push([command, ...args].join(' '))
    if (command === 'boss')
      return session === null
        ? { ok: false, exit: 1 }
        : { ok: true, payload: { session: session ?? { is_verify_only: true, policy_ref: ref } } }
    const tip = tips[Math.min(calls.filter((c) => c.startsWith('git')).length - 1, tips.length - 1)]
    return tip
      ? { ok: true, exit: 0, output: `${tip}\trefs/heads/${ref}\n` }
      : { ok: false, exit: 2, output: '' }
  }
  return { read, calls }
}

test('only plain branch names are policy refs', () => {
  for (const ref of ['dev', 'config/verify-trial', 'v1.2_x'])
    assert.equal(validPolicyRef(ref), true, ref)
  for (const ref of ['', '-x', 'a..b', 'a,b', 'a b', 'dev@{1}', 'a/', '/a', 'x.lock', 'HEAD~1'])
    assert.equal(validPolicyRef(ref), false, ref)
})

test('origin resolution reads the exact branch and tells a missing branch from an unreadable origin', () => {
  const read = () => ({
    ok: true,
    exit: 0,
    output: `${SHA_2}\trefs/heads/config/trial-old\n${SHA_1}\trefs/heads/config/trial\n`,
  })
  assert.deepEqual(resolveOriginRef(read, 'config/trial'), { ok: true, sha: SHA_1 })
  assert.deepEqual(
    resolveOriginRef(() => ({ ok: false, exit: 2 }), 'dev'),
    {
      ok: false,
      why: 'not-on-origin',
    },
  )
  assert.deepEqual(
    resolveOriginRef(() => ({ ok: false, exit: 128 }), 'dev'),
    {
      ok: false,
      why: 'origin-unreadable',
    },
  )
})

test('outside a session, or in one without a policy ref, there is no trial and no read', () => {
  const plain = world()
  assert.deepEqual(trialPolicyFor(plain.read, {}), { trial: null })
  assert.deepEqual(plain.calls, [])
  const normal = world({ session: { is_verify_only: false } })
  assert.deepEqual(trialPolicyFor(normal.read, { sessionId: 's', runDir: null }), { trial: null })
})

test('a run resolves its ref once: later verbs of the same run keep the pinned commit', (t) => {
  const runDir = mkdtempSync(join(tmpdir(), 'trial-pin-'))
  t.after(() => rmSync(runDir, { recursive: true, force: true, maxRetries: 3 }))
  const moving = world({ tips: [SHA_1, SHA_2] })
  const deps = { sessionId: 's', runDir }
  assert.deepEqual(trialPolicyFor(moving.read, deps), { trial: TRIAL })
  assert.deepEqual(trialPolicyFor(moving.read, deps), { trial: TRIAL }, 'the pin outlives a push')
  assert.equal(moving.calls.filter((c) => c.startsWith('git ls-remote')).length, 1)
  const nextRun = mkdtempSync(join(tmpdir(), 'trial-pin-'))
  t.after(() => rmSync(nextRun, { recursive: true, force: true, maxRetries: 3 }))
  assert.deepEqual(trialPolicyFor(moving.read, { sessionId: 's', runDir: nextRun }), {
    trial: { ref: 'config/trial', sha: SHA_2 },
  })
})

test('a ref origin lacks is refused; an unreadable origin or session is unknown', () => {
  assert.match(
    trialPolicyFor(world({ tips: [] }).read, { sessionId: 's', runDir: null }).refused,
    /not a branch on origin/,
  )
  assert.deepEqual(trialPolicyFor(world({ session: null }).read, { sessionId: 's' }), {
    unreadable: 'session',
  })
})

test('a trial parks every deciding verdict and drops what a later run could reuse', () => {
  for (const verdict of ['pass', 'human', 'blocked', 'defect']) {
    const parked = trialPark(
      {
        verdict,
        reason: 'x',
        extensionRecord: 'r',
        judgeRecord: 'j',
        carryRecord: 'c',
        held: true,
      },
      TRIAL,
    )
    assert.deepEqual(
      [parked.verdict, parked.reason, parked.label],
      ['human', 'trial-policy', 'trial policy config/trial@1111111'],
    )
    assert.deepEqual(parked.trialOf, { verdict, reason: 'x' })
    for (const key of ['extensionRecord', 'judgeRecord', 'carryRecord', 'held'])
      assert.equal(key in parked, false, key)
  }
  for (const verdict of ['wait', 'extensions-required', 'judge-required'])
    assert.equal(trialPark({ verdict }, TRIAL).verdict, verdict)
  const untouched = { verdict: 'pass', reason: 'verified' }
  assert.equal(trialPark(untouched, null), untouched)
})

test('a trial post parks with trial-policy beside its codes and refuses every other decision', () => {
  const deps = { sessionId: 's', runDir: null }
  const human = trialPost(
    { verdict: 'human', reason: 'extension-failed', extensionRecord: 'r', judgeRecord: 'j' },
    world().read,
    deps,
  )
  assert.equal(human.options.reason, 'extension-failed,trial-policy')
  assert.equal(human.options.extensionRecord, undefined)
  assert.equal(human.options.judgeRecord, undefined)
  assert.deepEqual(human.trial, TRIAL)
  assert.equal(trialPost({ verdict: 'pass' }, world().read, deps).refused, 'trial-policy')
  const unknown = world({ session: null }).read
  assert.equal(trialPost({ verdict: 'pass' }, unknown, deps).refused, 'policy-ref-unreadable')
  assert.equal(
    trialPost({ verdict: 'human', reason: 'no-receipt' }, unknown, deps).refused,
    undefined,
  )
  assert.deepEqual(trialMergeRefusal(world().read, deps), {
    verdict: 'human',
    reason: 'trial-policy',
  })
  assert.equal(trialMergeRefusal(world().read, {}), null)
})

test('a standing trial park never holds when an ordinary judge re-judges the head', () => {
  const live = { applicable: ['review'], record: null }
  assert.equal(parkStillRaised(['human-merge'], live), true)
  assert.equal(parkStillRaised(['trial-policy'], live), false)
  assert.equal(parkStillRaised(['human-merge', 'trial-policy'], live), false)
  const recorded = {
    applicable: ['review'],
    record: [{ extension: 'review', outcome: 'trial-policy' }],
  }
  assert.equal(parkStillRaised(['trial-policy'], recorded), false)
})

test('trial proof actions read the trial tree and its merge policy, and a refused sidecar still names the trial', () => {
  const asked = []
  const policies = []
  const actions = trialProofActions(
    {
      trustedTree: (o) => asked.push(o.baseSha),
      readBasePolicy: (_read, _repo, sha) => policies.push(sha),
    },
    TRIAL,
  )
  actions.trustedTree({ baseSha: SHA_2 })
  // The merge policy's config comes from the same trial commit as its tree.
  actions.readBasePolicy(null, 'o/r', SHA_2)
  assert.deepEqual(policies, [SHA_1])
  // Publishers stay on the trusted base tree.
  actions.publisherTree({ baseSha: SHA_2 })
  assert.deepEqual(asked, [SHA_1, SHA_2])
  assert.deepEqual(actions.trialPolicy, TRIAL)

  // Even a sidecar the renderer refuses keeps the trial, so the page still says it.
  const input = proofPageInput({
    proof: { base: SHA_1, head: SHA_2 },
    areas: [],
    judged: { verdict: 'human', trialPolicy: TRIAL },
    git: () => ({ ok: false }),
    prBody: '',
    repo: 'o/r',
    pr: 1,
  })
  assert.deepEqual(input.trialPolicy, TRIAL)
})

test('in a trial, a park nothing approved is judged afresh; every other hand-back is unchanged', () => {
  const deps = { sessionId: 's', runDir: null }
  const none = { approval: 'none' }
  assert.deepEqual(trialHandBack(none, world().read, deps), {
    approval: 'reverify',
    via: 'trial-policy',
  })
  const merged = { approval: 'merged-by-human' }
  assert.equal(trialHandBack(merged, world().read, deps), merged)
  const plain = world({ session: { is_verify_only: true, policy_ref: '' } })
  assert.equal(trialHandBack(none, plain.read, deps), none)
  assert.equal(trialHandBack(none, world({ session: null }).read, deps), none)
})
