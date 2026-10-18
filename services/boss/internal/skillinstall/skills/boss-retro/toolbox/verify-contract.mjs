// The verify role's one rule the outcome classifier must recognise without matching prose.
export const VERIFY_PASS_WITHOUT_EVIDENCE = 'pass requires at least one evidence item'
export const VERIFY_VERDICTS = Object.freeze(['pass', 'fail', 'abstain', 'human', 'wait'])
// The shape of a `human` verdict's `reason`. Membership in the verify gate's closed park-code list is
// checked by classifyVerifyOutcome against the list its caller injects (importing that list here
// would be a cycle); an unknown reason is a malfunction.
export const VERIFY_HUMAN_REASON = /^human-review:[a-z][a-z-]*$/
// The shape of a `wait` verdict's `reason`: a short kebab-case code the verify stage posts as
// `waiting: <reason>` (for example `stack-busy`).
export const VERIFY_WAIT_REASON = /^[a-z][a-z-]*$/
const nonEmptyString = (value) => typeof value === 'string' && value.trim() !== ''
const plainObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

export function refineVerify(envelope) {
  const errors = []
  if (!VERIFY_VERDICTS.includes(envelope.verdict))
    errors.push(`verdict must be one of ${VERIFY_VERDICTS.join(', ')}`)
  if (!Array.isArray(envelope.evidence)) errors.push('evidence must be an array')
  else
    envelope.evidence.forEach((item, idx) => {
      if (!plainObject(item) || !nonEmptyString(item.kind) || !nonEmptyString(item.ref))
        errors.push(`evidence ${idx} needs non-empty string "kind" and "ref"`)
    })
  if (!Array.isArray(envelope.findings)) errors.push('findings must be an array')
  else
    envelope.findings.forEach((item, idx) => {
      if (!plainObject(item) || !nonEmptyString(item.title))
        errors.push(`finding ${idx} needs a non-empty string "title"`)
    })
  if (
    envelope.verdict === 'pass' &&
    Array.isArray(envelope.evidence) &&
    envelope.evidence.length === 0
  )
    errors.push(VERIFY_PASS_WITHOUT_EVIDENCE)
  if (
    envelope.verdict === 'fail' &&
    Array.isArray(envelope.findings) &&
    envelope.findings.length === 0
  )
    errors.push('fail requires at least one finding')
  if (envelope.verdict === 'human') {
    if (typeof envelope.reason !== 'string' || !VERIFY_HUMAN_REASON.test(envelope.reason))
      errors.push('human requires a reason of the form human-review:<category>')
    if (Array.isArray(envelope.evidence) && envelope.evidence.length === 0)
      errors.push('human requires at least one evidence item')
  }
  if (envelope.verdict === 'wait') {
    if (typeof envelope.reason !== 'string' || !VERIFY_WAIT_REASON.test(envelope.reason))
      errors.push('wait requires a reason of the form <kebab-case-code>')
    if (Array.isArray(envelope.findings) && envelope.findings.length > 0)
      errors.push('wait must not carry findings')
  }
  return errors
}

// Every reason classifyVerifyOutcome can return. The five malfunction reasons stay distinct from a
// valid `fail` so the verify stage can route a broken extension to a human rather than to repair.
export const VERIFY_OUTCOME_REASONS = Object.freeze([
  'timed-out',
  'crashed',
  'reported-failure',
  'pass-without-evidence',
  'malformed-result',
  'abstain',
  'pass',
  'fail',
  'human',
  'wait',
])

// The fields of a verify envelope's context that carry PR or ticket author text. A dispatcher
// passes them to an extension as quoted data, never as instructions.
export const VERIFY_UNTRUSTED_TEXT_FIELDS = Object.freeze([
  'pr.title',
  'pr.body',
  'ticket.title',
  'ticket.description',
])
const VERIFY_CONTEXT_KEYS = Object.freeze(['pr', 'headSha', 'baseSha', 'changedPaths', 'ticket'])
const VERIFY_CONTEXT_OPTIONAL_KEYS = Object.freeze(['range'])
const VERIFY_RANGE_KEYS = Object.freeze(['kind', 'anchorSha'])
const VERIFY_RANGE_KINDS = Object.freeze(['full', 'delta'])
const FULL_SHA = /^[0-9a-f]{40}$/i

// Validates the verify role's envelope context `{pr, headSha, baseSha, changedPaths, ticket}` plus
// the optional `range: {kind: 'full'|'delta', anchorSha}`, which says whether `changedPaths` is the
// whole PR or its delta since an approval anchor (`anchorSha` may be null only on `full`). Any other
// top-level key is an error, so a caller cannot smuggle instructions in beside the data.
// Returns `{ok, errors}` and never throws.
export function validateVerifyContext(context) {
  const errors = []
  if (!plainObject(context)) return { ok: false, errors: ['context is not an object'] }
  for (const key of Object.keys(context))
    if (!VERIFY_CONTEXT_KEYS.includes(key) && !VERIFY_CONTEXT_OPTIONAL_KEYS.includes(key))
      errors.push(`unexpected context key "${key}"`)
  for (const key of VERIFY_CONTEXT_KEYS)
    if (!(key in context)) errors.push(`missing context key "${key}"`)
  const optionalStrings = (value, label, keys) => {
    for (const key of keys)
      if (key in value && typeof value[key] !== 'string')
        errors.push(`${label}.${key} must be a string`)
  }
  if ('pr' in context) {
    if (!plainObject(context.pr)) errors.push('pr must be an object')
    else {
      if (!Number.isInteger(context.pr.number) || context.pr.number < 1)
        errors.push('pr.number must be a positive integer')
      optionalStrings(context.pr, 'pr', ['url', 'title', 'body'])
    }
  }
  for (const key of ['headSha', 'baseSha'])
    if (key in context && !(typeof context[key] === 'string' && FULL_SHA.test(context[key])))
      errors.push(`${key} must be a 40-hex SHA`)
  if ('changedPaths' in context) {
    if (!Array.isArray(context.changedPaths)) errors.push('changedPaths must be an array')
    else
      context.changedPaths.forEach((entry, idx) => {
        if (!nonEmptyString(entry)) errors.push(`changedPaths ${idx} must be a non-empty string`)
      })
  }
  if ('ticket' in context && context.ticket !== null) {
    if (!plainObject(context.ticket)) errors.push('ticket must be null or an object')
    else optionalStrings(context.ticket, 'ticket', ['id', 'url', 'title', 'description'])
  }
  if ('range' in context) {
    const range = context.range
    if (!plainObject(range)) errors.push('range must be an object')
    else {
      for (const key of Object.keys(range))
        if (!VERIFY_RANGE_KEYS.includes(key)) errors.push(`unexpected range key "${key}"`)
      for (const key of VERIFY_RANGE_KEYS)
        if (!(key in range)) errors.push(`missing range key "${key}"`)
      if ('kind' in range && !VERIFY_RANGE_KINDS.includes(range.kind))
        errors.push('range.kind must be "full" or "delta"')
      const nullOnFull = range.anchorSha === null && range.kind === 'full'
      if (
        'anchorSha' in range &&
        !nullOnFull &&
        !(typeof range.anchorSha === 'string' && FULL_SHA.test(range.anchorSha))
      )
        errors.push('range.anchorSha must be a 40-hex SHA')
    }
  }
  return { ok: errors.length === 0, errors }
}
