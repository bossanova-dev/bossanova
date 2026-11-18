// Frozen context validator from ../verify-contract.mjs at 1fff2c31, executed by base-tree fixtures.
// Keep this closed contract independent of the current validator.
const nonEmptyString = (value) => typeof value === 'string' && value.trim() !== ''
const plainObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)
const INTENT_SOURCES = ['acceptance-criteria', 'issue', 'pr-body', 'commits', 'unknown']
const MAX_TEXT_CHARS = 300
const MAX_BULLETS = 5

const VERIFY_CONTEXT_KEYS = Object.freeze(['pr', 'headSha', 'baseSha', 'changedPaths', 'ticket'])
const VERIFY_CONTEXT_OPTIONAL_KEYS = Object.freeze(['range', 'intent'])
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
  if ('intent' in context) {
    const intent = context.intent
    if (!plainObject(intent)) errors.push('intent must be an object')
    else {
      for (const key of Object.keys(intent))
        if (!['source', 'ref', 'bullets'].includes(key))
          errors.push(`unexpected intent key "${key}"`)
      if (!INTENT_SOURCES.includes(intent.source))
        errors.push('intent.source must name an intent source')
      const singleLine = (text) =>
        nonEmptyString(text) && !/[\r\n\u2028\u2029]/.test(text) && text.length <= MAX_TEXT_CHARS
      if (
        !Array.isArray(intent.bullets) ||
        intent.bullets.length > MAX_BULLETS ||
        !intent.bullets.every(singleLine)
      )
        errors.push('intent.bullets must contain at most five short single-line strings')
      else if ((intent.source === 'unknown') !== (intent.bullets.length === 0))
        errors.push('intent.bullets must be empty exactly when source is unknown')
      if ('ref' in intent && (intent.source === 'unknown' || !singleLine(intent.ref)))
        errors.push('intent.ref must be short single-line text on a known source')
    }
  }
  return { ok: errors.length === 0, errors }
}
