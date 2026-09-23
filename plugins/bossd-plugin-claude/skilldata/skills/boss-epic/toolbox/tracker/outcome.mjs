// skills-toolbox/tracker/outcome.mjs — how a tracker attempt TURNED OUT, as one vocabulary.
//
// `tracker/` models what a tracker operation IS — an operationMap naming an MCP tool and its
// argument shape — and modelled nothing about the outcome of an attempt. A tracker attempt has four
// outcomes and only two were representable: success, and thrown-failure. The two that were missing
// are the expensive ones:
//
//   indeterminate — the request MAY have applied, so retrying blindly risks a duplicate write while
//                   abandoning it risks a lost one. Neither is safe without a read-back.
//   false-empty   — the read returned nothing, but nothing is not evidence of nothing. A gate that
//                   cannot tell this from a genuine zero row reports "no work" and skips a cycle.
//
// A site that cannot NAME those two collapses them into one of the other two, and both collapses
// are silent. That is the whole defect this module removes.
//
// Contract-level and pure, beside `adapter-core.mjs` and `preflight.mjs`: it IMPORTS NOTHING, so a
// vendored copy resolves with no co-located siblings. Classification is not something a tracker
// *performs* — it never enters an `operationMap`, or every vendored adapter becomes non-conforming.
//
// House style is `plan-writeback-verify.mjs`: a frozen verdict set, a SEPARATE closed reason
// vocabulary, and a verdict that is MEASURED from what was observed rather than configured.

/**
 * The five verdicts. Exactly one is returned per classification. Frozen because a sixth verdict
 * invented at a call site is the rival vocabulary this module exists to prevent.
 * @typedef {'ok'|'retryable'|'permanent'|'indeterminate'|'false-empty'} TrackerVerdict
 */
export const TRACKER_VERDICTS = Object.freeze({
  /** The attempt produced a readable result. */
  OK: 'ok',
  /** A transient transport-layer failure. Another attempt is warranted, bounded. */
  RETRYABLE: 'retryable',
  /** A credential, permission or query defect. Another attempt changes nothing. */
  PERMANENT: 'permanent',
  /** The request may have applied. VERIFY, then decide — never blind-retry, never abandon. */
  INDETERMINATE: 'indeterminate',
  /** The read produced no usable evidence. "No rows" and "could not read" are NOT the same. */
  FALSE_EMPTY: 'false-empty',
})

/** Every verdict, as a frozen set, so a caller can assert membership without re-listing them. */
export const TRACKER_VERDICT_SET = Object.freeze(new Set(Object.values(TRACKER_VERDICTS)))

/**
 * Why a verdict was reached, as a closed slug vocabulary kept SEPARATE from the verdict. The
 * verdict alone cannot answer "is the retry earning its keep": a run that recorded only `retryable`
 * cannot tell a rate-limit from a DNS blip, which are the same verdict and very different
 * operational facts.
 */
export const TRACKER_OUTCOME_REASONS = Object.freeze({
  SUCCESS: 'success',
  ROWS_PRESENT: 'rows-present',
  NO_ROWS: 'no-rows',
  UNREADABLE_PAYLOAD: 'unreadable-payload',
  TRANSPORT_FAILED: 'transport-failed',
  REQUEST_TIMEOUT: 'request-timeout',
  SERVER_ERROR: 'server-error',
  RATE_LIMITED: 'rate-limited',
  MISSING_CREDENTIAL: 'missing-credential',
  UNAUTHORIZED: 'unauthorized',
  FORBIDDEN: 'forbidden',
  VALIDATION_ERROR: 'validation-error',
  WRITE_MAY_HAVE_APPLIED: 'write-may-have-applied',
  UNRECOGNIZED: 'unrecognized',
})

/** Every reason slug, frozen, for the same membership reason as the verdict set. */
export const TRACKER_OUTCOME_REASON_SET = Object.freeze(
  new Set(Object.values(TRACKER_OUTCOME_REASONS)),
)

/**
 * The caps. Small and explicit ON PURPOSE: a retry that is generous enough to ride out a real
 * outage converts a two-second failure into a long one, which is a worse unattended failure than
 * the blip it was meant to survive. Three attempts across a ≤2s ceiling costs at most a few
 * seconds against a cron cadence measured in hours.
 */
export const TRACKER_RETRY_CAPS = Object.freeze({
  /** Total attempts, INCLUDING the first. 3 = the original call plus two retries. */
  maxAttempts: 3,
  /** First backoff, doubled per attempt and then clamped by `maxDelayMs`. */
  baseDelayMs: 250,
  /** No requested sleep ever exceeds this, whatever the attempt number. */
  maxDelayMs: 2000,
})

/** Operations a classification can be made about. A write is the only one that can be applied. */
export const TRACKER_OPERATION_KINDS = Object.freeze(['read', 'write'])

const V = TRACKER_VERDICTS
const R = TRACKER_OUTCOME_REASONS

/** @returns {{verdict: TrackerVerdict, reason: string, retry: boolean}} */
function outcome(verdict, reason, retry) {
  return Object.freeze({ verdict, reason, retry })
}

/**
 * The conservative fallback. An error text this module does not recognise is NOT `ok` and is NOT
 * retried: today every tracker failure throws on the first attempt, so falling here preserves that
 * behaviour exactly and the retry is added only where a transient signature is positively
 * recognised. Widening the recognised set is a cheap, reviewable edit; guessing that an unknown
 * failure is transient is a budget spend nobody asked for.
 */
const UNRECOGNIZED = outcome(V.PERMANENT, R.UNRECOGNIZED, false)

// Signature tables. Each is a POSITIVE recognition — a text this module has seen a tracker emit —
// and an unmatched text falls through to UNRECOGNIZED rather than to a default verdict.
const MISSING_CREDENTIAL =
  /api[_ -]?key(?:\s+is)?\s+(?:not set|missing|empty)|missing api[_ -]?key|no api[_ -]?key/i
const TIMEOUT = /timed?\s*out|timeout|ETIMEDOUT|ESOCKETTIMEDOUT|UND_ERR_(?:CONNECT_)?TIMEOUT/i
const TRANSPORT =
  /fetch failed|ECONNRESET|ECONNREFUSED|ENOTFOUND|EAI_AGAIN|EPIPE|socket hang up|network (?:error|failure)|terminated/i
const VALIDATION =
  /graphql (?:error|validation)|validation (?:error|failed)|cannot query field|unknown argument|invalid value|bad request|argument .* is required/i

/** Pull an HTTP status out of an explicit field or out of the error text (`... HTTP 429`). */
function readStatus(status, text) {
  if (Number.isInteger(status)) return status
  const m = /\bHTTP[ /]?(\d{3})\b/i.exec(text) ?? /\bstatus(?:\s+code)?[ :=]+(\d{3})\b/i.exec(text)
  return m ? Number(m[1]) : null
}

/** Normalize the many shapes a caller may hold an observation in into `{text, status, result}`. */
function normalize(observed) {
  if (observed === null || observed === undefined) return { text: '', status: null, has: false }
  if (typeof observed === 'string') return { text: observed, status: null, has: observed !== '' }
  if (observed instanceof Error) {
    return {
      text: String(observed.message ?? ''),
      status: readStatus(observed.status ?? observed.statusCode, String(observed.message ?? '')),
      kind: typeof observed.kind === 'string' ? observed.kind : null,
      has: true,
    }
  }
  if (typeof observed !== 'object') return { text: String(observed), status: null, has: true }
  // An object observation is a record: `{error?, status?, result?}`. `result` is only consulted
  // when no error is present, because an attempt that threw did not produce a result.
  if ('error' in observed && observed.error !== null && observed.error !== undefined) {
    const inner = normalize(observed.error)
    return {
      text: inner.text,
      status: readStatus(observed.status ?? inner.status, inner.text),
      kind: typeof observed.kind === 'string' ? observed.kind : (inner.kind ?? null),
      has: true,
    }
  }
  if ('result' in observed) return { result: observed.result, isResult: true, has: true }
  if (Number.isInteger(observed.status)) return { text: '', status: observed.status, has: true }
  // A bare object with no error and no status IS the result.
  return { result: observed, isResult: true, has: true }
}

/**
 * Classify one tracker attempt.
 *
 * @param {unknown} observed An Error, its text, or a record `{error?, status?, result?}`. A bare
 *   object with neither `error` nor `status` is read AS the result.
 * @param {{operation?: 'read'|'write'}} [options] `operation` is what makes a timeout mean two
 *   different things: a READ that timed out changed nothing and can simply be retried, while a
 *   WRITE that timed out may already have applied — that is the `indeterminate` case, and retry is
 *   DECLINED there so the caller is forced through a read-back instead of a blind second write.
 * @returns {{verdict: TrackerVerdict, reason: string, retry: boolean}} Frozen. Never throws:
 *   `null`, `undefined`, an empty string and a non-object all classify.
 */
export function classifyTrackerOutcome(observed, { operation = 'read' } = {}) {
  const isWrite = operation === 'write'
  const n = normalize(observed)

  if (n.isResult) return classifyResult(n.result)
  // Nothing at all was observed. Not `ok` — an attempt that reported nothing is not an attempt
  // that succeeded, and calling it `ok` is exactly the silent collapse this module removes.
  if (!n.has) return UNRECOGNIZED

  const text = n.text ?? ''
  const status = n.status

  if (MISSING_CREDENTIAL.test(text)) return outcome(V.PERMANENT, R.MISSING_CREDENTIAL, false)

  // Provenance beats prose. A caller that TAGGED the failure has told us where it came from, and a
  // `graphql` failure is a server-side rejection however the server worded it — so it is answered
  // from the field, ahead of every text table. Read from prose alone, a GraphQL message merely
  // CONTAINING "terminated" or "network error" matched TRANSPORT first and was retried, and under
  // `operation: 'write'` it became `indeterminate` and sent the caller to read back a write the
  // server had already deterministically refused.
  if (n.kind === 'graphql') return outcome(V.PERMANENT, R.VALIDATION_ERROR, false)

  if (status !== null) {
    if (status === 401) return outcome(V.PERMANENT, R.UNAUTHORIZED, false)
    if (status === 403) return outcome(V.PERMANENT, R.FORBIDDEN, false)
    if (status === 429) return outcome(V.RETRYABLE, R.RATE_LIMITED, true)
    if (status >= 500 && status <= 599) return outcome(V.RETRYABLE, R.SERVER_ERROR, true)
    if (status === 400 || status === 422) return outcome(V.PERMANENT, R.VALIDATION_ERROR, false)
    // A 408 "Request Timeout" is a timeout first and a status second; fall through to the
    // operation-sensitive timeout rule rather than answering it as a generic 4xx.
    if (status !== 408 && status >= 400 && status <= 499) return UNRECOGNIZED
  }

  if (TIMEOUT.test(text) || status === 408) {
    return isWrite
      ? outcome(V.INDETERMINATE, R.WRITE_MAY_HAVE_APPLIED, false)
      : outcome(V.RETRYABLE, R.REQUEST_TIMEOUT, true)
  }
  if (TRANSPORT.test(text)) {
    // A write whose CONNECTION failed is also unsafe to repeat blind: the request may have reached
    // the server before the socket died. Reads have nothing to lose and are retried.
    return isWrite
      ? outcome(V.INDETERMINATE, R.WRITE_MAY_HAVE_APPLIED, false)
      : outcome(V.RETRYABLE, R.TRANSPORT_FAILED, true)
  }
  if (VALIDATION.test(text)) return outcome(V.PERMANENT, R.VALIDATION_ERROR, false)

  return UNRECOGNIZED
}

/**
 * A result, rather than a failure. `null`/`undefined` and primitives are NOT `ok`: a read that
 * handed back nothing readable is the false-empty case, and answering `ok` there is how a zero-row
 * report gets manufactured out of an unanswerable one.
 */
function classifyResult(result) {
  if (result === null || result === undefined) {
    return outcome(V.FALSE_EMPTY, R.UNREADABLE_PAYLOAD, false)
  }
  if (typeof result !== 'object') return outcome(V.FALSE_EMPTY, R.UNREADABLE_PAYLOAD, false)
  return outcome(V.OK, R.SUCCESS, false)
}

/**
 * The read's THIRD answer. `rows` is whatever the caller extracted from its payload; when the
 * extraction could not be made at all the caller passes the non-array it got, and gets
 * `false-empty` rather than a zero it would have reported as "no work".
 *
 * @param {unknown} rows An array of rows, or anything else when the payload could not be read.
 * @returns {{verdict: TrackerVerdict, reason: string, rows: number|null}} `rows` is the count, or
 *   `null` when the payload was unreadable — deliberately NOT `0`, which is the collapse itself.
 */
export function readEvidence(rows) {
  if (!Array.isArray(rows)) {
    return Object.freeze({ verdict: V.FALSE_EMPTY, reason: R.UNREADABLE_PAYLOAD, rows: null })
  }
  if (rows.length === 0) {
    return Object.freeze({ verdict: V.OK, reason: R.NO_ROWS, rows: 0 })
  }
  return Object.freeze({ verdict: V.OK, reason: R.ROWS_PRESENT, rows: rows.length })
}

/** Default sleep. Injected away in tests so no suite ever waits on wall-clock time. */
const realSleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

/**
 * Run `attempt` under a BOUNDED retry.
 *
 * Bounded is the whole contract: at most `maxAttempts` calls, and no requested sleep above
 * `maxDelayMs`, whatever the attempt number. There is no unbounded loop and no unclamped backoff.
 * A verdict whose `retry` is false — `permanent`, and `indeterminate` — stops on the attempt that
 * produced it, so a bad credential still fails on the first call and a write that may have applied
 * is never repeated blind.
 *
 * @template T
 * @param {(attempt: number) => Promise<T>|T} attempt Called with the 1-based attempt number.
 * @param {object} [options]
 * @param {number} [options.maxAttempts]
 * @param {number} [options.baseDelayMs]
 * @param {number} [options.maxDelayMs]
 * @param {'read'|'write'} [options.operation]
 * @param {(observed: unknown, o: object) => {retry: boolean}} [options.classify]
 * @param {(ms: number) => Promise<void>} [options.sleep] Injected by tests as a RECORDER, which is
 *   what lets the delay cap be proven rather than timed.
 * @param {(info: {attempt: number, verdict: object, error: unknown}) => void} [options.onRetry]
 * @returns {Promise<T>} The first successful result. Otherwise the last error is re-thrown, so a
 *   caller's existing fail-closed handling keeps seeing the error it always saw.
 */
export async function withBoundedRetry(
  attempt,
  {
    maxAttempts = TRACKER_RETRY_CAPS.maxAttempts,
    baseDelayMs = TRACKER_RETRY_CAPS.baseDelayMs,
    maxDelayMs = TRACKER_RETRY_CAPS.maxDelayMs,
    operation = 'read',
    classify = classifyTrackerOutcome,
    sleep = realSleep,
    onRetry,
  } = {},
) {
  if (typeof attempt !== 'function') {
    throw new TypeError('withBoundedRetry: `attempt` must be a function')
  }
  // Clamped rather than validated: a caller that computed 0 attempts from config would otherwise
  // get a helper that silently never calls its own work function.
  const attempts = Math.max(1, Math.floor(Number(maxAttempts) || 1))
  const ceiling = Math.max(0, Number(maxDelayMs) || 0)
  const base = Math.max(0, Number(baseDelayMs) || 0)

  let lastError
  for (let i = 1; i <= attempts; i += 1) {
    try {
      return await attempt(i)
    } catch (err) {
      lastError = err
      const verdict = classify(err, { operation })
      if (!verdict?.retry || i === attempts) throw err
      if (typeof onRetry === 'function') onRetry({ attempt: i, verdict, error: err })
      // Exponential, then CLAMPED. The clamp is what the delay cap actually is — without it the
      // exponent, not the configured ceiling, decides the longest wait.
      await sleep(Math.min(base * 2 ** (i - 1), ceiling))
    }
  }
  /* c8 ignore next 2 — unreachable: the loop either returns or throws on its final attempt. */
  throw lastError
}

/**
 * One machine-readable line, in the write-back verifier's house shape: a stable leading token so a
 * caller can grep for it, then `key=value` pairs in a fixed order. No trailing newline — the caller
 * decides how it is written.
 * @param {{verdict: string, reason: string, retry: boolean}} verdict
 * @param {{operation?: string}} [context]
 */
export function formatOutcomeLine(verdict, { operation = 'read' } = {}) {
  return `tracker-outcome verdict=${verdict.verdict} reason=${verdict.reason} retry=${
    verdict.retry ? 'yes' : 'no'
  } operation=${operation}`
}
