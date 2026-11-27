// verify-policy-ref.mjs — an operator's trial verify policy. A verify-only session created with
// `boss new --pr <n> --verify-only --policy-ref <ref>` (or MCP create_session `policy_ref`) stores
// the ref on its session row; nothing in a PR can set it. Verify running in that session reads its
// policy, `.boss-skills.json` and verify extensions from origin/<ref> instead of the PR base tip,
// pins the ref to one commit for the whole run, and parks every verdict it would otherwise post as
// `trial-policy`, so a trial can never merge or count as a pass.

import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { liveSessionsFor } from './verify-rank.mjs'
import { resolveRunDir } from './verify-run-dir.mjs'

export const TRIAL_POLICY_CODE = 'trial-policy'
// Mirrors policyRefPattern in services/bossd/api/daemon/session_create_rpc.go: plain branch path
// segments, so a ref is never an option, a revision expression or a park-code separator.
export const POLICY_REF_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]*(\/[A-Za-z0-9][A-Za-z0-9._-]*)*$/
export const POLICY_PIN_FILE = 'trial-policy.json'
// The verdicts a trial run would post as a decision; it parks each of them instead.
const DECIDING = new Set(['pass', 'human', 'blocked', 'defect'])
const isSha = (sha) => typeof sha === 'string' && /^[0-9a-f]{40}$/i.test(sha)

/** @param {unknown} ref */
export function validPolicyRef(ref) {
  return (
    typeof ref === 'string' &&
    ref.length <= 200 &&
    POLICY_REF_PATTERN.test(ref) &&
    !ref.includes('..') &&
    !ref.endsWith('.lock') &&
    !ref.endsWith('.')
  )
}

/**
 * The policy ref of boss session `id` (the CLI's `BOSS_SESSION_ID`): `{state: 'none'}`,
 * `{state: 'set', ref}`, or `{state: 'unreadable', why}`. A verify-only session whose row does not
 * name `policy_ref` at all comes from a CLI too old to say, so it is unknown, never "no trial".
 * @param {Function} read @param {string | undefined} id
 */
export function sessionPolicyRef(read, id) {
  if (typeof id !== 'string' || id.trim() === '') return { state: 'none' }
  const show = read('boss', ['show', id, '--json'], { json: true })
  const session = show?.ok ? show.payload?.session : null
  if (!session || typeof session !== 'object') return { state: 'unreadable', why: 'session' }
  const ref = session.policy_ref
  if (ref === undefined || ref === null)
    return session.is_verify_only === true
      ? { state: 'unreadable', why: 'policy-ref-absent' }
      : { state: 'none' }
  if (typeof ref !== 'string') return { state: 'unreadable', why: 'policy-ref-malformed' }
  return ref === '' ? { state: 'none' } : { state: 'set', ref }
}

/**
 * origin's commit for branch `ref`: `{ok: true, sha}`, or `{ok: false, why}` where `not-on-origin`
 * means origin answered and has no such branch.
 * @param {Function} read @param {string} ref
 */
export function resolveOriginRef(read, ref) {
  if (!validPolicyRef(ref)) return { ok: false, why: 'invalid-ref' }
  const r = read('git', ['ls-remote', '--exit-code', 'origin', `refs/heads/${ref}`])
  // `--exit-code` exits 2 when origin answers and lists no matching ref.
  if (!r?.ok) return { ok: false, why: r?.exit === 2 ? 'not-on-origin' : 'origin-unreadable' }
  const line = String(r.output ?? '')
    .split('\n')
    .map((row) => row.trim().split(/\s+/))
    .find(([sha, name]) => name === `refs/heads/${ref}` && isSha(sha))
  return line ? { ok: true, sha: line[0].toLowerCase() } : { ok: false, why: 'not-on-origin' }
}

/** This run's directory, or null outside a boss-verify run. @param {string} cwd @param {object} env */
export function defaultRunDir(cwd, env) {
  try {
    const top = spawnSync('git', ['rev-parse', '--absolute-git-dir'], { cwd, encoding: 'utf8' })
    return top.status === 0 ? resolveRunDir({ gitDir: top.stdout.trim(), env }) : null
  } catch {
    return null
  }
}

function readPin(runDir, ref) {
  if (!runDir) return null
  try {
    const pin = JSON.parse(readFileSync(join(runDir, POLICY_PIN_FILE), 'utf8'))
    return pin?.ref === ref && isSha(pin?.sha) ? pin.sha : null
  } catch {
    return null
  }
}

/**
 * The trial policy for this run: `{trial: null}` outside a trial session, `{trial: {ref, sha}}`
 * inside one, `{unreadable: why}` when the session row cannot say, or `{refused: message}` for a
 * ref origin does not have. The first verb of a run resolves the ref and pins the commit in the run
 * directory, so every later judge, post and proof of the same run reads the same tree. Only an
 * explicit `sessionId` names the session: the gate's CLI passes `BOSS_SESSION_ID`, and an
 * in-process caller that passes none is never a trial.
 * @param {Function} read
 * @param {{sessionId?: string, cwd?: string, runDir?: string | null}} [deps]
 */
export function trialPolicyFor(read, { sessionId, cwd = process.cwd(), runDir } = {}) {
  const session = sessionPolicyRef(read, sessionId)
  if (session.state === 'none') return { trial: null }
  if (session.state === 'unreadable') return { unreadable: session.why }
  const { ref } = session
  const dir = runDir === undefined ? defaultRunDir(cwd, process.env) : runDir
  const pinned = readPin(dir, ref)
  if (pinned) return { trial: { ref, sha: pinned } }
  const resolved = resolveOriginRef(read, ref)
  if (!resolved.ok)
    return resolved.why === 'origin-unreadable'
      ? { unreadable: 'origin-unreadable' }
      : {
          refused: `verify-gate: trial policy ref ${JSON.stringify(ref)} is not a branch on origin`,
        }
  if (dir)
    try {
      writeFileSync(join(dir, POLICY_PIN_FILE), `${JSON.stringify({ ref, sha: resolved.sha })}\n`)
    } catch {
      /* Unpinned, a later verb resolves the ref again. */
    }
  return { trial: { ref, sha: resolved.sha } }
}

// A proof for a session that cannot say whether it is a trial (or names a ref origin lacks) is
// withheld: a page published then could hide that trial policy judged the head.
const PROOF_WITHHELD_CODE = 'policy-ref-unreadable'

/**
 * The trial a proof for this session names: `{trial}` (null outside a trial session), or
 * `{withheld}` when the session cannot say.
 * @param {Function} read @param {object} deps
 */
export function trialProofPolicy(read, deps) {
  const policy = trialPolicyFor(read, deps)
  return 'trial' in policy ? { trial: policy.trial ?? null } : { withheld: PROOF_WITHHELD_CODE }
}

/**
 * The policy fingerprint a trial park records: derived from the trial commit, so it never matches
 * the base policy's and the ordinary verify sweep re-judges the head under base policy.
 * @param {{ref: string, sha: string}} trial
 */
const trialFingerprint = (trial) =>
  createHash('sha256')
    .update(`${TRIAL_POLICY_CODE}\0${trial.ref}\0${trial.sha}`)
    .digest('hex')
    .slice(0, 16)

/**
 * Why `approve` refuses a park with these codes, or null: a trial park approves nothing, since base
 * policy never judged its head.
 * @param {string[]} codes
 */
export const trialParkRefusal = (codes) => (codes.includes(TRIAL_POLICY_CODE) ? 'trial-park' : null)

/**
 * Whether the candidates sweep leaves a standing trial park alone. Its fingerprint never matches
 * base policy, but re-judging while a trial session is live would route back into that session
 * and park again, every tick. So it holds while a live session on the PR is a trial, or cannot say
 * (a `boss ls` row without `policy_ref`, or an unreadable list); once none is, the head is
 * re-judged under base policy.
 * @param {string[] | null} parked @param {() => unknown} sessions @param {string} prUrl
 */
export function trialParkHolds(parked, sessions, prUrl) {
  if (!parked?.includes(TRIAL_POLICY_CODE)) return false
  const rows = sessions()
  if (!Array.isArray(rows)) return true
  return liveSessionsFor(rows, prUrl).some((row) => row.policy_ref !== '')
}

/**
 * A `post` in a trial session: a claim carries its trial; otherwise `{options, trial}` with a human park's codes joined by
 * `trial-policy`, its reusable records dropped and its policy record the trial's own (never the
 * base's, which would mark the head judged under base policy), or `{refused}` for a pass, defect
 * or blocked verdict, which a trial session never posts. A session that cannot say whether it is a trial
 * still parks, with its proof `proofWithheld`, but refuses everything else.
 * @param {Record<string, any>} options @param {Function} read @param {object} deps
 */
export function trialPost(options, read, deps) {
  if (options.verdict === 'wait') return { options, trial: null }
  const policy = trialPolicyFor(read, deps)
  const trial = policy.trial ?? null
  // A claim carries the trial so `post` can refuse to claim over a base-policy failure; a session
  // that cannot say whether it is a trial never claims, or that guard could not run.
  if (options.verdict === 'claim')
    return 'trial' in policy
      ? { options, trial }
      : { options, trial, refused: 'policy-ref-unreadable' }
  if (options.verdict !== 'human')
    return trial || !('trial' in policy)
      ? { options, trial, refused: trial ? TRIAL_POLICY_CODE : 'policy-ref-unreadable' }
      : { options, trial }
  if (!trial)
    return 'trial' in policy
      ? { options, trial }
      : { options, trial, proofWithheld: PROOF_WITHHELD_CODE }
  const codes = new Set([...String(options.reason ?? '').split(','), TRIAL_POLICY_CODE])
  codes.delete('')
  return {
    options: {
      ...options,
      reason: [...codes].join(','),
      extensionRecord: undefined,
      judgeRecord: undefined,
      policy: trialFingerprint(trial),
    },
    trial,
  }
}

/**
 * The skill's hand-back answer in this session: in a trial, a park that nothing approved
 * (`none`) is judged afresh (`reverify`), since a trial judges with its own policy and no standing
 * park holds it; every other answer, and every answer outside a trial, is unchanged.
 * @param {{approval: string}} handBack @param {Function} read @param {object} deps
 */
export function trialHandBack(handBack, read, deps) {
  if (handBack.approval !== 'none' || !trialPolicyFor(read, deps).trial) return handBack
  return { approval: 'reverify', via: TRIAL_POLICY_CODE }
}

/**
 * Why `merge` must not merge from this session: a trial parks (`human`), an unknown session waits;
 * null outside a trial session.
 * @param {Function} read @param {object} deps
 */
export function trialMergeRefusal(read, deps) {
  const policy = trialPolicyFor(read, deps)
  if (policy.trial) return { verdict: 'human', reason: TRIAL_POLICY_CODE }
  return 'trial' in policy ? null : { verdict: 'wait', reason: 'policy-ref-unreadable' }
}

/** `<ref>@<sha7>`, the trial a page or park names. @param {{ref: string, sha: string}} trial */
export const trialName = (trial) => `${trial.ref}@${trial.sha.slice(0, 7)}`

/** The park label: says which trial judged the head. @param {{ref: string, sha: string}} trial */
export const trialLabel = (trial) => `trial policy ${trialName(trial)}`

/**
 * `verdict` as a trial run reports it: a deciding verdict parks as `trial-policy`, carrying what it
 * would have been in `trialOf`, and drops every record a later run could reuse (an extension or
 * judge record, a carried approval), so nothing a trial judged reaches a merge. Waits and the
 * verdicts that ask for more work pass through. A null trial changes nothing.
 * @param {Record<string, any>} verdict @param {{ref: string, sha: string} | null} trial
 */
export function trialPark(verdict, trial) {
  if (!trial) return verdict
  /** @type {Record<string, any>} */
  const tagged = { ...verdict, trialPolicy: trial }
  if (!DECIDING.has(verdict.verdict)) return tagged
  for (const key of [
    'extensionRecord',
    'judgeRecord',
    'carryRecord',
    'held',
    'blockers',
    'findings',
  ])
    delete tagged[key]
  return {
    ...tagged,
    verdict: 'human',
    reason: TRIAL_POLICY_CODE,
    reasons: [TRIAL_POLICY_CODE],
    labels: [],
    label: trialLabel(trial),
    trialOf: { verdict: verdict.verdict, reason: String(verdict.reason ?? '') },
  }
}

/**
 * Proof actions whose sections, proof config, merge policy and media read the trial tree, the
 * policy the head was judged with. `publisherTree` keeps the base tree: publishers stay on the
 * trusted base.
 * @param {Record<string, any>} actions @param {{ref: string, sha: string} | null} trial
 */
export function trialProofActions(actions, trial) {
  if (!trial) return { ...actions, trialPolicy: null }
  const base = actions.trustedTree
  const policy = actions.readBasePolicy
  return {
    ...actions,
    trialPolicy: trial,
    ...(typeof policy === 'function'
      ? { readBasePolicy: (read, repo) => policy(read, repo, trial.sha) }
      : {}),
    ...(typeof base === 'function'
      ? {
          trustedTree: (options = {}) => base({ ...options, baseSha: trial.sha }),
          publisherTree: base,
        }
      : {}),
  }
}

/** The line a trial page's PR proof note leads with. @param {{ref: string, sha: string}} trial */
export const trialNoteLine = (trial) =>
  `**Trial policy.** Verify judged this page with trial policy from \`${trialName(trial)}\`, not the policy on the PR's base branch. A trial-policy verdict never merges.`

/** `{ref, sha}` from untrusted input, or null. @param {unknown} value */
export function parseTrialPolicy(value) {
  const v = /** @type {any} */ (value)
  return v && typeof v === 'object' && validPolicyRef(v.ref) && isSha(v.sha)
    ? { ref: v.ref, sha: v.sha.toLowerCase() }
    : null
}
