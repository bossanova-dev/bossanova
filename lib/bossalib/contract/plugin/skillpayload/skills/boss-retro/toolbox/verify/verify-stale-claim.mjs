// verify-stale-claim.mjs — a `boss/verify` claim past CLAIM_STALE_AFTER_MS never stands. The sweep
// takes an expired claim over before it posts a verdict or a wait on that head, a quiet tick wakes
// when a live claim expires, and a machine that does not own a PR's session replaces a claim left
// standing twice that long with `waiting: claim-expired` (never a dispatch, never a merge).

import { postArgv } from './verify-post-files.mjs'
import { CLAIM_EXPIRED_REASON, CLAIM_STALE_AFTER_MS, claimState } from './verify-vocabulary.mjs'

export { CLAIM_EXPIRED_REASON }

// How long a machine that does not own a PR's session leaves an expired claim to its owner.
export const REMOTE_ORPHAN_AFTER_MS = 2 * CLAIM_STALE_AFTER_MS

const text = (value) => typeof value === 'string' && value.trim() !== ''
const isSha = (value) => typeof value === 'string' && /^[0-9a-f]{40}$/.test(value)

/**
 * Whether a verdict or wait post under `token` is abandoned as claim-lost: the latest status is a
 * live claim of another run, or an expired one. A dry-run takeover writes no claim, so the expired
 * claim it would replace is still the latest status when the post under its token reads back;
 * under dry-run that token counts as held, so a dry run reports what a real run would post.
 *
 * @param {{description?: string, updatedAt?: string} | null} status
 * @param {string | undefined} token
 * @param {number} now
 * @param {boolean} dryRun
 */
export function verdictClaimLost(status, token, now, dryRun) {
  const kind = claimState(status, { token, now }).kind
  if (kind === 'foreign') return true
  return kind === 'stale' && !(dryRun === true && text(token))
}

/**
 * `post --verdict claim` through `gate`, which takes over a stale claim and notes it. Returns
 * `{won: true, token, replaced?}`, `replaced` naming the expired claim's token, or `{won: false}`.
 * With `expired` (the default) the caller saw an expired claim at discovery: the claim passes
 * `--takeover`, so the gate writes nothing unless the status is still that expired claim, and the
 * takeover counts only when the gate names the claim it replaced. A second sweep that saw the same
 * expired claim therefore never claims over the first sweep's verdict.
 *
 * @param {(args: string[]) => {ok: boolean, payload?: any}} gate
 * @param {{pr: number, head: string, repoArgs?: string[], dry?: string[], expired?: boolean}} target
 * @returns {{won: boolean, token?: string, replaced?: string}}
 */
export function takeOverExpiredClaim(gate, { pr, head, repoArgs = [], dry = [], expired = true }) {
  const r = gate([
    'post',
    '--pr',
    String(pr),
    '--head',
    head,
    '--verdict',
    'claim',
    ...(expired ? ['--takeover'] : []),
    ...repoArgs,
    ...dry,
  ])
  if (!r?.ok || r.payload?.won !== true || !text(r.payload?.token)) return { won: false }
  if (expired && !text(r.payload.replaced)) return { won: false }
  return {
    won: true,
    token: r.payload.token,
    ...(text(r.payload.replaced) ? { replaced: r.payload.replaced } : {}),
  }
}

/**
 * Withdraws this run's claim on `head` once its dispatch failed: the gate re-posts the park the
 * claim covered, so no `verifying…` strands over it. True only when the park stands again.
 *
 * @param {(args: string[]) => {ok: boolean, payload?: any}} gate
 * @param {{pr: number, head: string, token: string, repoArgs?: string[], dry?: string[]}} target
 */
export function releaseClaim(gate, { pr, head, token, repoArgs = [], dry = [] }) {
  const r = gate([
    ...['post', '--pr', String(pr), '--head', head, '--verdict', 'claim', '--release'],
    ...['--token', token, ...repoArgs, ...dry],
  ])
  return r?.ok === true && r.payload?.released === true
}

/**
 * The claim a verdict on candidate `c` posts under: `{}` for a head with no expired claim, the won
 * takeover for a stale one, or null when another run took the head first.
 *
 * @param {{claim?: string}} c
 * @param {Parameters<typeof takeOverExpiredClaim>[0]} gate
 * @param {Parameters<typeof takeOverExpiredClaim>[1]} target
 * @returns {{won?: boolean, token?: string, replaced?: string} | null}
 */
export function claimForVerdict(c, gate, target) {
  if (c?.claim !== 'stale') return {}
  const took = takeOverExpiredClaim(gate, target)
  return took.won ? took : null
}

/**
 * Take the expired claim over, then post `--verdict wait --reason <reason>` under the new token.
 * Returns the takeover plus `posted` and the wait's argv; `{won: false}` when the takeover lost.
 *
 * @param {Parameters<typeof takeOverExpiredClaim>[0]} gate
 * @param {Parameters<typeof takeOverExpiredClaim>[1] & {reason: string}} target
 * @returns {{won: boolean, token?: string, replaced?: string, posted?: boolean, argv?: string[]}}
 */
export function waitOverExpiredClaim(gate, { reason, ...target }) {
  const took = takeOverExpiredClaim(gate, target)
  if (!took.won) return took
  const argv = postArgv({
    pr: target.pr,
    head: target.head,
    judged: {},
    verdict: 'wait',
    reason,
    repoArgs: target.repoArgs,
    dry: target.dry,
    token: took.token,
  })
  const posted = gate(argv)
  return { ...took, posted: posted?.ok === true && posted.payload?.posted === true, argv }
}

// `{kind, at}` when the row's latest status is another run's claim (`at` may be NaN), else null.
function otherClaim(row, now) {
  if (!row || row.gap || !row.status) return null
  const { kind } = claimState(row.status, { now })
  if (kind !== 'foreign' && kind !== 'stale') return null
  return { kind, at: Date.parse(row.status.updatedAt ?? '') }
}

/**
 * The earliest `Date.parse(status.updatedAt) + afterMs` still ahead of `now` among batched open-PR
 * rows whose latest status is another run's claim, or null. With the default `afterMs` that is the
 * earliest expiry among the rows `claimState` calls `foreign`.
 *
 * @param {Array<{gap?: unknown, status?: any}>} rows
 * @param {number} now
 * @param {number} [afterMs]
 * @returns {number | null}
 */
export function earliestClaimExpiry(rows, now, afterMs = CLAIM_STALE_AFTER_MS) {
  let earliest = null
  for (const row of Array.isArray(rows) ? rows : []) {
    const due = (otherClaim(row, now)?.at ?? NaN) + afterMs
    if (Number.isFinite(due) && due > now && (earliest === null || due < earliest)) earliest = due
  }
  return earliest
}

/**
 * `candidates`' wake fields: `claimExpiresAt`, when the first in-scope live claim expires, and
 * `remoteOrphanAt`, when the first claim on a remote-owned (excluded) PR reaches
 * REMOTE_ORPHAN_AFTER_MS. Each is omitted when there is none.
 *
 * @param {Array<{pr: any, gap?: unknown, status?: any}>} rows
 * @param {number} now
 * @param {{inScope: (pr: any) => boolean, remote: (pr: any) => boolean}} scope
 */
export function claimWake(rows, now, { inScope, remote }) {
  const all = Array.isArray(rows) ? rows : []
  const expires = earliestClaimExpiry(
    all.filter((row) => inScope(row.pr)),
    now,
  )
  const orphan = earliestClaimExpiry(
    all.filter((row) => remote(row.pr)),
    now,
    REMOTE_ORPHAN_AFTER_MS,
  )
  return {
    ...(expires === null ? {} : { claimExpiresAt: expires }),
    ...(orphan === null ? {} : { remoteOrphanAt: orphan }),
  }
}

/**
 * The time a quiet tick must wake for: the earliest of a candidates payload's claim deadlines, or null.
 *
 * @param {{claimExpiresAt?: unknown, remoteOrphanAt?: unknown} | null | undefined} payload
 * @returns {number | null}
 */
export function wakeAtOf(payload) {
  const due = [payload?.claimExpiresAt, payload?.remoteOrphanAt].filter(Number.isFinite)
  return due.length ? Math.min(.../** @type {number[]} */ (due)) : null
}

/**
 * The claim fields a remote-session skip carries from its batched row: `{claim, claimAt, headSha,
 * repo}` when the head's latest status is another run's claim, else `{}`.
 *
 * @param {{pr?: any, gap?: unknown, status?: any} | undefined} row
 * @param {number} now
 * @param {string} repo
 */
export function remoteClaimFields(row, now, repo) {
  const claim = otherClaim(row, now)
  if (!claim) return {}
  return {
    claim: claim.kind,
    claimAt: row?.status?.updatedAt ?? '',
    headSha: String(row?.pr?.headRefOid ?? '').toLowerCase(),
    repo,
  }
}

/**
 * What a sweep does with remote-session skip `s`. A live claim says nothing; a claim expired less
 * than REMOTE_ORPHAN_AFTER_MS after it was made only says so, as does an older one where `writable`
 * is false (this machine could not read who owns the PR). Past that, the claim is taken over and
 * replaced with `waiting: claim-expired`, which the owner's next tick re-judges like any wait.
 * Returns `{detail, quiet, argv?}`: `quiet` is false once a write was attempted.
 *
 * @param {{pr: number, claim?: string, claimAt?: string, headSha?: string, repo?: string}} s
 * @param {{now: number, gate: Parameters<typeof takeOverExpiredClaim>[0], writable: boolean, dry?: string[]}} ctx
 * `due` marks an orphan old enough to replace that was left only because `writable` was false.
 *
 * @returns {{detail: string, quiet: boolean, due?: boolean, argv?: string[]}}
 */
export function remoteOrphan(s, { now, gate, writable, dry = [] }) {
  if (s?.claim !== 'stale') return { detail: '', quiet: true }
  const at = Date.parse(s.claimAt ?? '')
  const minutes = Math.max(0, Math.floor((now - at - CLAIM_STALE_AFTER_MS) / 60_000))
  const expired = `verify claim expired${Number.isFinite(at) ? ` ${minutes}m ago` : ''}`
  const orphaned = !Number.isFinite(at) || now - at > REMOTE_ORPHAN_AFTER_MS
  if (!orphaned || !isSha(s.headSha)) return { detail: expired, quiet: true }
  if (!writable) return { detail: expired, quiet: true, due: true }
  const repoArgs = text(s.repo) ? ['--repo', s.repo] : []
  const r = waitOverExpiredClaim(gate, {
    pr: s.pr,
    head: /** @type {string} */ (s.headSha),
    repoArgs,
    dry,
    reason: CLAIM_EXPIRED_REASON,
  })
  if (!r.won) return { detail: `${expired}; claim lost`, quiet: false }
  const how = r.posted
    ? `replaced claim${r.replaced ? ` ${r.replaced}` : ''} with waiting: ${CLAIM_EXPIRED_REASON}`
    : 'took the claim, wait post failed'
  return { detail: `${expired}; ${how}`, quiet: false, ...(r.argv ? { argv: r.argv } : {}) }
}
