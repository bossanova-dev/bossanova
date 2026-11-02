// ci/pr-state-envelope.mjs — the one mapping from a `boss pr state --json` envelope to the raw
// inputs pr-check-state's classifier reads, plus the CLI payload reader both share.
//
// The envelope carries the daemon's own `check_verdict`, which follows the daemon's checks_passed
// meaning (an all-skipped head is green there, in-flight runs too). It is never read here: the
// verdict is recomputed from the raw `checks`, `workflow_runs` and `head_sha`, so the agent-side
// rules (no-gate-ran is unknown, an unfinished head run is pending, a missing required check before
// any run is pending, the prior-head absent gate) hold on the daemon path exactly as on the gh one.
//
// The daemon reads status, checks and runs separately, so the runs are trusted only when every one
// carries the envelope's head SHA. The checks carry no SHA at all, so a cached check set can belong
// to an older head than the status; ci-wait therefore ends a daemon-path wait only on a `--fresh`
// reading. Runs that were never read are `null`, never an empty list: an
// empty list says the head has no run, which is evidence; `null` is no evidence.
//
// Accepts the bare envelope or the `pr-state.mjs read` output (`{ source, state }`). Pure apart
// from `readPayload`, which reads a CLI payload flag; Node builtins only.

import { readFileSync } from 'node:fs'

export const RUNS_NOT_READ = 'daemon read carried no workflow runs'

const upper = (value) => (typeof value === 'string' ? value.trim().toUpperCase() : '')
const text = (value) => (typeof value === 'string' ? value.trim() : '')
const isObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

// unwrapPrState returns the envelope inside either accepted shape, or null.
export function unwrapPrState(input) {
  if (!isObject(input)) return null
  return isObject(input.state) ? input.state : input
}

// failureOf names a `{"error":{code,connect_code,message}}` failure envelope, or returns null.
export function failureOf(input) {
  const error = isObject(input) ? input.error : null
  if (!isObject(error)) return null
  const code = text(error.code) || upper(error.connect_code) || 'UNKNOWN'
  return `${code}: ${text(error.message) || 'no message'}`
}

// rollupFromEnvelope maps the envelope's checks to `statusCheckRollup` nodes. A check the daemon
// could not classify keeps no conclusion, so it stays unclassified rather than reading as a pass.
export function rollupFromEnvelope(envelope) {
  const checks = Array.isArray(envelope?.checks) ? envelope.checks : []
  return checks
    .filter((check) => isObject(check) && text(check.name) !== '')
    .map((check) => ({
      name: text(check.name),
      status: upper(check.status),
      conclusion: check.is_unclassified === true ? '' : upper(check.conclusion),
      description: typeof check.description === 'string' ? check.description : '',
    }))
}

// Only `is_workflow_runs_read` proves the list is the head's every run. `has_settled_workflow_runs`
// does not: an older daemon sets it on its own green while leaving `workflow_runs` empty, and that
// empty list would read as "no head run" while runs are still in flight.
function runsWereRead(envelope) {
  return envelope?.is_workflow_runs_read === true
}

// headShaMismatch reports runs read for a head other than the envelope's: status and runs came from
// different reads, so the envelope does not describe one head.
export function headShaMismatch(envelope) {
  const head = text(envelope?.head_sha)
  const runs = Array.isArray(envelope?.workflow_runs) ? envelope.workflow_runs : []
  return runs.some((run) => text(run?.head_sha) !== '' && text(run?.head_sha) !== head)
}

// workflowRunsFromEnvelope returns `gh run list --json name,workflowName,status,conclusion,headSha,
// event` rows, or `{ runs: null, error }` when the runs were not read or belong to another head.
export function workflowRunsFromEnvelope(envelope) {
  if (!runsWereRead(envelope) || !Array.isArray(envelope?.workflow_runs)) {
    return { runs: null, error: RUNS_NOT_READ }
  }
  if (headShaMismatch(envelope)) {
    return {
      runs: null,
      error: `workflow runs read for a head other than ${text(envelope.head_sha)}`,
    }
  }
  const runs = envelope.workflow_runs.filter(isObject).map((run) => ({
    name: text(run.name),
    workflowName: text(run.name),
    status: upper(run.status),
    conclusion: upper(run.conclusion),
    headSha: text(run.head_sha),
    event: text(run.event),
  }))
  return { runs, error: null }
}

// classifyInputsFromPrState maps one envelope to `classifyChecks` inputs. Both SHAs are the
// envelope head; a failure envelope, a missing head SHA or runs read for another head is a read
// error, so the verdict is unreadable.
export function classifyInputsFromPrState(input) {
  const failure = failureOf(input)
  const envelope = failure === null ? unwrapPrState(input) : null
  const headSHA = text(envelope?.head_sha)
  let readError = failure
  if (readError === null && envelope === null) readError = 'no PR state envelope'
  if (readError === null && headSHA === '') readError = 'PR state carried no head SHA'
  if (readError === null && headShaMismatch(envelope)) {
    readError = `workflow runs read for a head other than ${headSHA}`
  }
  return {
    headSHA,
    observedSHA: headSHA,
    rollup: envelope === null ? null : rollupFromEnvelope(envelope),
    workflowRuns: envelope === null ? null : workflowRunsFromEnvelope(envelope).runs,
    readError,
  }
}

// The payload flags are documented as a PATH or `-` for stdin. Handing one inline JSON instead is
// the natural mistake, and it used to produce two different wrong answers from the same wrong call:
// a short payload was swallowed as ENOENT → null → `no-checks`/exit 0 (an agent reads that as "the
// gates are not ready yet"), while a long one threw a raw ENAMETOOLONG straight past the ENOENT
// guard. Neither named the mistake. Inline JSON is now simply accepted, and every OTHER unreadable
// value raises naming the path-or-`-` contract.
const PAYLOAD_SHAPE = '<path>|- (stdin)|inline JSON'

/**
 * @param {Record<string, string | boolean | undefined>} flags
 * @param {string} name
 * @param {(name: string) => void} [onUnreadable]
 */
export function readPayload(flags, name, onUnreadable = () => {}) {
  const value = flags[name]
  if (value === undefined || value === true || value === '') return null
  // Inline arm: a value whose first non-space character opens a JSON array or object was never a
  // path. Decided by the leading character rather than by a failed stat, so it holds for a payload
  // far past the filesystem's name-length limit too.
  const trimmed = typeof value === 'string' ? value.trim() : ''
  if (trimmed.startsWith('[') || trimmed.startsWith('{')) {
    try {
      return JSON.parse(trimmed)
    } catch (err) {
      throw new Error(
        `pr-check-state: --${name} (${PAYLOAD_SHAPE}) — inline JSON payload is malformed: ${err?.message ?? err}`,
      )
    }
  }
  let raw
  try {
    raw = value === '-' ? readFileSync(0, 'utf8') : readFileSync(String(value), 'utf8')
  } catch (err) {
    // Optional --prior/--checks inputs may be absent. Required fetched payloads report a
    // read error through the classifier, preserving its single JSON verdict on stdout.
    if (err?.code === 'ENOENT') {
      process.stderr.write(`pr-check-state: --${name} payload not found: ${value}\n`)
      onUnreadable(name)
      return null
    }
    throw new Error(
      `pr-check-state: --${name} (${PAYLOAD_SHAPE}) — cannot read payload ${JSON.stringify(value)}: ${err?.code ?? err?.message ?? err}`,
    )
  }
  if (raw.trim() === '') {
    onUnreadable(name)
    return null
  }
  return JSON.parse(raw)
}
