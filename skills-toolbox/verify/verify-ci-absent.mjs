// verify-ci-absent.mjs — what verify does with a head that no CI reported on, from the base tip's
// `verifyDefaults.ci`. `whenAbsent: "wait"` waits on `ci-not-green-on-head` forever. `"park"` (the
// default) waits out `absentGraceMinutes` from the head's push, then judges the rest and parks
// `ci-absent`, but only when the base requires no checks. Absent CI is never green: no approval
// lifts `ci-absent`, so verify never merges such a head. A person merges it by hand, or the head
// gains CI and is judged as usual.
//
// Pure apart from `readCiAbsence`, which reads the push time through the injected reader.

import { judgeCiOnHead } from '../epic/merge-eligibility.mjs'
import { CHECK_REASONS, epochMs } from '../pr-check-state.mjs'

export const CI_ABSENT = 'ci-absent'
export const CI_ABSENT_DEFAULTS = Object.freeze({ whenAbsent: 'park', absentGraceMinutes: 15 })

/** The `verifyDefaults.ci` settings from a raw `verifyDefaults` block, defaults filled in. */
export function ciAbsentPolicy(block) {
  const ci = block?.ci ?? {}
  const minutes = ci.absentGraceMinutes
  return {
    whenAbsent: ci.whenAbsent === 'wait' ? 'wait' : CI_ABSENT_DEFAULTS.whenAbsent,
    graceMs:
      (Number.isFinite(minutes) && minutes >= 0 ? minutes : CI_ABSENT_DEFAULTS.absentGraceMinutes) *
      60_000,
  }
}

/**
 * A `judgeCiOnHead` result for a head with no check run and no status besides `boss/*`. A context
 * an earlier head reported makes the empty head `absent-gate` rather than `no-checks`, and the
 * candidate scan reads the rollup without earlier heads, so both count: the head's own evidence
 * decides. A queued or running head run is `pending`, never absent.
 */
export const noCiOnHead = (ci) =>
  (ci?.verdict?.reason === CHECK_REASONS.NO_CHECKS ||
    ci?.verdict?.reason === CHECK_REASONS.ABSENT_GATE) &&
  ci.verdict.total === 0

/**
 * `present` when anything besides `boss/*` reported on the head. Otherwise `wait` (the policy
 * waits, or the base requires checks), `grace` (inside the window, or the push time is unknown) or
 * `park`.
 * @returns {'present' | 'wait' | 'grace' | 'park'}
 */
export function ciAbsence({ ci, block, required = [], pushedAt = null, now }) {
  if (!noCiOnHead(ci)) return 'present'
  const policy = ciAbsentPolicy(block)
  if (policy.whenAbsent === 'wait' || required.length > 0) return 'wait'
  if (!Number.isFinite(pushedAt) || now - pushedAt < policy.graceMs) return 'grace'
  return 'park'
}

export const HEAD_PUSH_QUERY = `query($owner: String!, $name: String!, $number: Int!, $oid: GitObjectID!) {
  repository(owner: $owner, name: $name) {
    object(oid: $oid) { ... on Commit { committedDate } }
    pullRequest(number: $number) {
      createdAt
      timelineItems(last: 50, itemTypes: [HEAD_REF_FORCE_PUSHED_EVENT, READY_FOR_REVIEW_EVENT]) {
        nodes {
          __typename
          ... on HeadRefForcePushedEvent { createdAt afterCommit { oid } }
          ... on ReadyForReviewEvent { createdAt }
        }
      }
    }
  }
}`

/**
 * When CI could first have started on `headSha`, in epoch ms: the latest of the head commit's
 * committer date, the PR's opening, its last ready-for-review, and a force push onto this head.
 * GitHub no longer reports a plain push's time, so the committer date stands in for it. null when
 * the payload carries neither the commit nor the PR.
 */
export function headPushedAt(payload, headSha) {
  const repo = payload?.data?.repository
  const commit = epochMs(repo?.object?.committedDate)
  const opened = epochMs(repo?.pullRequest?.createdAt)
  if (commit === null || opened === null) return null
  const head = String(headSha ?? '').toLowerCase()
  const events = (repo.pullRequest.timelineItems?.nodes ?? [])
    .filter(
      (node) =>
        node?.__typename === 'ReadyForReviewEvent' ||
        (node?.__typename === 'HeadRefForcePushedEvent' &&
          String(node.afterCommit?.oid ?? '').toLowerCase() === head),
    )
    .map((node) => epochMs(node.createdAt))
    .filter((at) => at !== null)
  return Math.max(commit, opened, ...events)
}

/**
 * `ciAbsence` for a judged head, reading the base facts (`base()`: `{block, required}` or null when
 * the base policy is unreadable) and the push time only when no CI reported. An unreadable base or
 * push time is `grace`, so verify keeps waiting rather than parking on a guess.
 */
export function readCiAbsence(read, { repo, pr, headSha, ci, base, now }) {
  if (!noCiOnHead(ci)) return 'present'
  const facts = base()
  if (!facts) return 'grace'
  if (ciAbsentPolicy(facts.block).whenAbsent === 'wait' || facts.required.length > 0) return 'wait'
  const [owner, name] = typeof repo === 'string' && repo.includes('/') ? repo.split('/') : []
  const r = read(
    'gh',
    [
      'api',
      'graphql',
      '-f',
      `query=${HEAD_PUSH_QUERY}`,
      ...(owner && name
        ? ['-f', `owner=${owner}`, '-f', `name=${name}`]
        : ['-F', 'owner={owner}', '-F', 'name={repo}']),
      '-F',
      `number=${pr}`,
      '-f',
      `oid=${headSha}`,
    ],
    { json: true },
  )
  const pushedAt = r.ok ? headPushedAt(r.payload, headSha) : null
  return ciAbsence({ ci, block: facts.block, required: facts.required, pushedAt, now })
}

/** The `judgeCiOnHead` result of a PR view's rollup alone, for the candidate scan. */
export const rollupCi = (view, headSha) =>
  judgeCiOnHead({
    headSHA: headSha,
    observedSHA: view?.headRefOid ?? '',
    rollup: view?.statusCheckRollup,
  })
