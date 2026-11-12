/**
 * The typed facts a proof-describe result may carry beside its items: blocks an area's section
 * renders, banner signals, and risks located in the diff. Validation is strict: an unknown block
 * kind or key rejects the whole result with an error naming its path. These facts travel in the
 * page sidecar, never in the boss-proof/1 record, so the record digest does not depend on them.
 */
import { MAX_TEXT_CHARS } from './verify-proof.mjs'

export const MAX_BLOCKS = 16
export const MAX_SIGNALS = 8
export const MAX_RISKS = 8
export const MAX_SCHEMA_TYPES = 24
export const MAX_SCHEMA_FIELDS = 60
export const MAX_TABLE_COLUMNS = 80
export const MAX_CODE_LINES = 40
export const MAX_CALL_SITE_LINES = 12
export const MAX_STEPS = 10
export const MAX_OPERATION_CHARS = 4000
const NAME_CHARS = 160
const TYPE_CHARS = 120
const LABEL_CHARS = 40

export const CHANGES = Object.freeze(['added', 'changed', 'removed'])
export const SCHEMA_KEYWORDS = Object.freeze([
  'type',
  'input',
  'enum',
  'interface',
  'union',
  'scalar',
])
export const SCHEMA_CLASSES = Object.freeze(['breaking', 'dangerous', 'safe'])
export const ROUTE_METHODS = Object.freeze([
  'GET',
  'HEAD',
  'POST',
  'PUT',
  'PATCH',
  'DELETE',
  'OPTIONS',
  'ANY',
])
export const RELATIONS = Object.freeze([
  'table',
  'view',
  'materialized-view',
  'dbt-model',
  'entity',
])
export const MATERIALIZATIONS = Object.freeze([
  'table',
  'view',
  'incremental',
  'ephemeral',
  'materialized-view',
  'snapshot',
])
export const SIGNAL_STATES = Object.freeze([
  'none',
  'safe',
  'additive',
  'proven',
  'review',
  'manual',
  'unproven',
  'risky',
  'breaking',
])
export const RISK_LEVELS = Object.freeze(['high', 'medium', 'low'])

/**
 * @typedef {{name: string, type: string, default?: string}} SchemaArg
 * @typedef {{name: string, type?: string, args?: SchemaArg[], directives?: string[],
 *   change?: string, before?: string}} SchemaField
 * @typedef {{keyword: string, name: string, change: string, class?: string, mobile?: string,
 *   fields: SchemaField[]}} SchemaType
 * @typedef {{name: string, type: string, constraints?: string, change?: string, pii?: boolean,
 *   note?: string}} Column
 * @typedef {{kind: 'schema', types: SchemaType[]}
 *   | {kind: 'client-call', name: string, operation?: string, before?: string[], after?: string[], path?: string, note?: string}
 *   | {kind: 'route', method: string, path: string, auth: string, change: string, note?: string}
 *   | {kind: 'table', name: string, relation: string, materialization?: string, change: string, columns: Column[]}
 *   | {kind: 'code', path: string, start: number, lines: string[], title?: string}
 *   | {kind: 'steps', steps: string[]}} Block
 * @typedef {{key: string, label: string, state: string, summary: string}} Signal
 * @typedef {{level: string, title: string, impact: string, guard?: string,
 *   at: {path: string, line: number}}} Risk
 * @typedef {{blocks?: Block[], signals?: Signal[], risks?: Risk[]}} Described
 * @typedef {(value: any, at: string, errors: string[]) => void} Rule
 */

/** @param {unknown} value @returns {value is Record<string, unknown>} */
const isRecord = (value) =>
  value !== null &&
  typeof value === 'object' &&
  !Array.isArray(value) &&
  [Object.prototype, null].includes(Object.getPrototypeOf(value))
const BREAK = /[\r\n\u2028\u2029]/
/** A quoted value for an error, never long enough to flood a log. @param {unknown} value */
const shown = (value) => JSON.stringify(value)?.slice(0, 40) ?? String(value)

/** @param {number} max @returns {Rule} */
const line = (max) => (value, at, errors) => {
  if (typeof value !== 'string' || !value.trim() || value.length > max || BREAK.test(value))
    errors.push(`${at}: expected single-line text of at most ${max} characters`)
}
const text = line(MAX_TEXT_CHARS)
/** A source line: may be blank, never holds a line break. @type {Rule} */
const sourceLine = (value, at, errors) => {
  if (typeof value !== 'string' || value.length > MAX_TEXT_CHARS || BREAK.test(value))
    errors.push(`${at}: expected one source line of at most ${MAX_TEXT_CHARS} characters`)
}
/** @type {Rule} */
const operation = (value, at, errors) => {
  if (typeof value !== 'string' || !value.trim() || value.length > MAX_OPERATION_CHARS)
    errors.push(`${at}: expected operation text of at most ${MAX_OPERATION_CHARS} characters`)
}
/** @param {readonly string[]} values @returns {Rule} */
const oneOf = (values) => (value, at, errors) => {
  if (!values.includes(value)) errors.push(`${at}: expected one of ${values.join(', ')}`)
}
/** @type {Rule} */
const flag = (value, at, errors) => {
  if (typeof value !== 'boolean') errors.push(`${at}: expected boolean`)
}
/** @type {Rule} */
const lineNumber = (value, at, errors) => {
  if (!Number.isSafeInteger(value) || value < 1) errors.push(`${at}: expected a line number`)
}
/** @type {Rule} */
const slug = (value, at, errors) => {
  if (typeof value !== 'string' || !/^[a-z0-9][a-z0-9-]{0,31}$/.test(value))
    errors.push(`${at}: expected a lower-case key`)
}
/** @param {Rule} rule @param {number} max @param {number} [min] @returns {Rule} */
const list =
  (rule, max, min = 0) =>
  (value, at, errors) => {
    if (!Array.isArray(value) || value.length < min || value.length > max)
      return void errors.push(
        `${at}: expected ${min ? `${min} to ${max}` : `at most ${max}`} entries`,
      )
    value.forEach((item, i) => rule(item, `${at}.${i}`, errors))
  }
/**
 * An object with exactly these keys: every `required` one and any `optional` one.
 * @param {Record<string, Rule>} required @param {Record<string, Rule>} [optional]
 * @returns {Rule}
 */
const shape =
  (required, optional = {}) =>
  (value, at, errors) => {
    if (!isRecord(value)) return void errors.push(`${at}: expected object`)
    for (const key of Object.keys(value))
      if (!Object.hasOwn(required, key) && !Object.hasOwn(optional, key))
        errors.push(`${at}.${key}: unknown key`)
    for (const [key, rule] of Object.entries(required))
      if (value[key] === undefined) errors.push(`${at}.${key}: required`)
      else rule(value[key], `${at}.${key}`, errors)
    for (const [key, rule] of Object.entries(optional))
      if (value[key] !== undefined) rule(value[key], `${at}.${key}`, errors)
  }

const schemaType = shape(
  {
    keyword: oneOf(SCHEMA_KEYWORDS),
    name: line(TYPE_CHARS),
    change: oneOf(CHANGES),
    fields: list(
      shape(
        { name: line(TYPE_CHARS) },
        {
          type: line(TYPE_CHARS),
          args: list(
            shape({ name: line(TYPE_CHARS), type: line(TYPE_CHARS) }, { default: line(80) }),
            8,
          ),
          directives: list(line(TYPE_CHARS), 4),
          change: oneOf(CHANGES),
          before: line(TYPE_CHARS),
        },
      ),
      MAX_SCHEMA_FIELDS,
    ),
  },
  { class: oneOf(SCHEMA_CLASSES), mobile: text },
)
const clientCallShape = shape(
  { name: line(NAME_CHARS) },
  {
    operation,
    before: list(sourceLine, MAX_CALL_SITE_LINES),
    after: list(sourceLine, MAX_CALL_SITE_LINES),
    path: text,
    note: text,
  },
)

/** Each block kind's fields besides `kind`. @type {Readonly<Record<string, Rule>>} */
const BLOCKS = Object.freeze({
  schema: shape({ types: list(schemaType, MAX_SCHEMA_TYPES, 1) }),
  'client-call': (value, at, errors) => {
    clientCallShape(value, at, errors)
    const site = [value?.before, value?.after].some((lines) => Array.isArray(lines) && lines.length)
    if (isRecord(value) && !value.operation && !site)
      errors.push(`${at}: a client call needs an operation or a call site`)
  },
  route: shape(
    {
      method: oneOf(ROUTE_METHODS),
      path: line(NAME_CHARS),
      auth: line(LABEL_CHARS),
      change: oneOf(CHANGES),
    },
    { note: text },
  ),
  table: shape(
    {
      name: line(NAME_CHARS),
      relation: oneOf(RELATIONS),
      change: oneOf(CHANGES),
      columns: list(
        shape(
          { name: line(TYPE_CHARS), type: line(TYPE_CHARS) },
          { constraints: text, change: oneOf(CHANGES), pii: flag, note: text },
        ),
        MAX_TABLE_COLUMNS,
      ),
    },
    { materialization: oneOf(MATERIALIZATIONS) },
  ),
  code: shape(
    { path: text, start: lineNumber, lines: list(sourceLine, MAX_CODE_LINES, 1) },
    { title: text },
  ),
  steps: shape({ steps: list(text, MAX_STEPS, 1) }),
})

/** @type {Rule} */
const block = (value, at, errors) => {
  if (!isRecord(value)) return void errors.push(`${at}: expected object`)
  const { kind, ...fields } = value
  if (typeof kind !== 'string' || !Object.hasOwn(BLOCKS, kind))
    return void errors.push(`${at}.kind: unknown block kind ${shown(kind)}`)
  BLOCKS[kind](fields, at, errors)
}

/** The optional described keys and their rules. @type {Readonly<Record<keyof Described, Rule>>} */
const DESCRIBED = Object.freeze({
  blocks: list(block, MAX_BLOCKS),
  signals: list(
    shape({ key: slug, label: line(LABEL_CHARS), state: oneOf(SIGNAL_STATES), summary: text }),
    MAX_SIGNALS,
  ),
  risks: list(
    shape(
      {
        level: oneOf(RISK_LEVELS),
        title: text,
        impact: text,
        at: shape({ path: text, line: lineNumber }),
      },
      { guard: text },
    ),
    MAX_RISKS,
  ),
})
export const DESCRIBED_KEYS = Object.freeze(
  /** @type {(keyof Described)[]} */ (Object.keys(DESCRIBED)),
)

/**
 * The errors in the described keys `value` carries, each naming its path under `prefix`. Other
 * keys are the caller's to check.
 * @param {Record<string, unknown>} value @param {string} [prefix]
 */
export function refineDescribed(value, prefix = '') {
  /** @type {string[]} */
  const errors = []
  for (const key of DESCRIBED_KEYS)
    if (Object.hasOwn(value, key)) DESCRIBED[key](value[key], `${prefix}${key}`, errors)
  return errors
}

/**
 * One area's described facts from its describers' results, in their order: blocks up to
 * MAX_BLOCKS, the first signal for each key, and each distinct risk. Empty lists are left out, so
 * an area no describer typed stays exactly as it was. `truncated` says a cap cut something.
 * @param {Described[]} descriptions
 * @returns {{described: Described, truncated: boolean}}
 */
export function mergeDescribed(descriptions) {
  const blocks = descriptions.flatMap((d) => d.blocks ?? [])
  const signals = descriptions
    .flatMap((d) => d.signals ?? [])
    .filter((signal, i, all) => all.findIndex((s) => s.key === signal.key) === i)
  const risks = [
    ...new Map(
      descriptions
        .flatMap((d) => d.risks ?? [])
        .map((r) => [JSON.stringify([r.title, r.at.path, r.at.line]), r]),
    ).values(),
  ]
  /** @type {Described} */
  const described = {}
  if (blocks.length) described.blocks = blocks.slice(0, MAX_BLOCKS)
  if (signals.length) described.signals = signals.slice(0, MAX_SIGNALS)
  if (risks.length) described.risks = risks.slice(0, MAX_RISKS)
  return {
    described,
    truncated:
      blocks.length > MAX_BLOCKS || signals.length > MAX_SIGNALS || risks.length > MAX_RISKS,
  }
}

/**
 * The risks whose `at` the diff holds: a changed path, on a line the head has when `hasLine` can
 * tell. Every other risk is dropped with its reason, as the agent spec's risks are.
 * @param {Risk[]} risks
 * @param {{changedPaths: Iterable<string> | null, hasLine?: (path: string, line: number) => boolean | null}} diff
 * @returns {{kept: Risk[], dropped: {at: string, reason: string}[]}}
 */
export function locatedRisks(risks, { changedPaths, hasLine }) {
  const changed = changedPaths ? new Set(changedPaths) : null
  /** @type {Risk[]} */
  const kept = []
  /** @type {{at: string, reason: string}[]} */
  const dropped = []
  risks.forEach((risk, i) => {
    const { path, line: at } = risk.at
    const reason = !changed
      ? 'at.path: the changed files are unknown, so it cannot be checked'
      : !changed.has(path)
        ? `at.path: ${path} is not changed by this PR`
        : hasLine?.(path, at) === false
          ? `at.line: ${path} has no line ${at} at head`
          : null
    if (reason) dropped.push({ at: `risks.${i}`, reason })
    else kept.push(risk)
  })
  return { kept, dropped }
}
