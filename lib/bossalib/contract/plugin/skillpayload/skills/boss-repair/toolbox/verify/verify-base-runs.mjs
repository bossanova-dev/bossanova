// verify-base-runs.mjs — which check runs on a base tip are the base branch's own CI, so a
// release PR headed on the base cannot lend the base its PR-event runs.

import { epochMs } from '../pr-check-state.mjs'

const timeOf = (value) => epochMs(value) ?? -Infinity
const idOf = (run) => (Number.isFinite(Number(run?.id)) ? Number(run.id) : -Infinity)

// Workflow-run events that test another PR's merge or queue ref rather than the base itself. A
// release PR headed on the base attaches these runs to the base tip's SHA, so they are not the
// base's own CI. Every other event (`push`, `schedule`, `workflow_dispatch`, …) is.
export const FOREIGN_BASE_EVENTS = Object.freeze([
  'pull_request',
  'pull_request_target',
  'pull_request_review',
  'pull_request_review_comment',
  'merge_group',
])

// Flattens a `--paginate --slurp` payload (or a single page) into its `key` entries; `null` when a
// page is malformed, `truncated` when any page's `total_count` exceeds what was read.
function pageEntries(payload, key) {
  const pages = Array.isArray(payload) ? payload : [payload]
  if (pages.some((page) => !page || typeof page !== 'object' || !Array.isArray(page[key])))
    return null
  const entries = pages.flatMap((page) => page[key])
  return { entries, truncated: pages.some((page) => Number(page.total_count) > entries.length) }
}

// Is `a` newer than `b`? Completed runs compare by `completed_at`, others by `started_at`; the
// larger id breaks a tie.
function newer(a, b, field) {
  const ta = timeOf(a[field])
  const tb = timeOf(b[field])
  return ta !== tb ? ta > tb : idOf(a) > idOf(b)
}

// The base tip's own check runs: GitHub Actions runs whose suite belongs to a foreign (PR-family)
// event are dropped and counted, an Actions run that no workflow run on `sha` attributes is
// ambiguous, and duplicates within one workflow (or app) and check name are decided by the newest
// completed run. Runs from other apps carry no event and are kept.
/** @param {{checkRuns?: unknown, workflowRuns?: unknown, sha?: string}} [input] */
export function selectBaseOwnCheckRuns({ checkRuns, workflowRuns, sha } = {}) {
  const runs = pageEntries(workflowRuns, 'workflow_runs')
  if (!runs) return { ok: false, reason: 'base-runs-unreadable' }
  if (runs.truncated) return { ok: false, reason: 'base-runs-truncated' }
  const checks = pageEntries(checkRuns, 'check_runs')
  if (!checks) return { ok: false, reason: 'base-check-runs-unreadable' }
  if (checks.truncated) return { ok: false, reason: 'base-check-runs-truncated' }
  const suites = new Map()
  for (const run of runs.entries) {
    if (run?.head_sha && run.head_sha !== sha) continue
    if (run?.check_suite_id === undefined || run.check_suite_id === null) continue
    suites.set(String(run.check_suite_id), {
      event: String(run.event ?? ''),
      // The stable workflow id, not the display name: two workflow files may share a name.
      workflow: String(run.workflow_id ?? run.name ?? ''),
    })
  }
  let foreign = 0
  const groups = new Map()
  for (const check of checks.entries) {
    const app = String(check?.app?.slug ?? '')
    let owner = app
    if (app === 'github-actions') {
      const suiteId = check?.check_suite?.id
      const suite =
        suiteId === undefined || suiteId === null ? undefined : suites.get(String(suiteId))
      if (!suite) return { ok: false, reason: 'base-suite-unattributed' }
      if (FOREIGN_BASE_EVENTS.includes(suite.event)) {
        foreign += 1
        continue
      }
      owner = `workflow:${suite.workflow}`
    }
    const key = `${owner}\u0000${check?.name ?? ''}`
    const group = groups.get(key)
    if (!group) groups.set(key, [check])
    else group.push(check)
  }
  const selected = []
  for (const group of groups.values()) {
    const completed = group.filter((run) => run?.status === 'completed')
    const pool = completed.length > 0 ? completed : group
    const field = completed.length > 0 ? 'completed_at' : 'started_at'
    selected.push(pool.reduce((best, run) => (newer(run, best, field) ? run : best)))
  }
  return { ok: true, checkRuns: selected, foreign }
}
