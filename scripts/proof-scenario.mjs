/**
 * Scenario loader + shape-only validator + downstream scene derivation (BOS-218).
 *
 * `validateScenario` is a hand-rolled recursive-descent validator (decision D2 —
 * no ajv). It mirrors `proof/scenarios/schema.json`; the schema-agreement suite
 * pins the two together. Validation is SHAPE-ONLY (D5/D7): it never enumerates
 * legal terminal keys, daemon actions, or fixture preset/seed/env content — those
 * are opaque here and validated downstream (BOS-217/BOS-219). Unknown keys or
 * actions fail loudly later, at replay.
 *
 * Expectation checking is delegated to `normalizeExpectation` from the shared
 * matcher so the validator and the matcher can never disagree.
 */
import fs from 'node:fs'
import { normalizeExpectation, displayText } from './proof-evidence-matcher.mjs'

export const STEP_OPS = ['key', 'type', 'waitFor', 'waitMs', 'daemon', 'expect']
export const SCENARIO_MAX_SCENES = 4

// Bounds for the optional top-level `terminal` size (BOS-571). A sane floor and
// ceiling so a typo cannot allocate an absurd pty. Exported so the
// schema-agreement suite pins proof/scenarios/schema.json to these rather than
// to magic numbers.
export const TERMINAL_COLS_MIN = 40
export const TERMINAL_COLS_MAX = 300
export const TERMINAL_ROWS_MIN = 10
export const TERMINAL_ROWS_MAX = 100

const TOP_LEVEL_KEYS = new Set(['$schema', 'version', 'title', 'fixture', 'terminal', 'scenes'])
const FIXTURE_KEYS = new Set(['preset', 'seed', 'env'])
const TERMINAL_KEYS = new Set(['cols', 'rows'])
const SCENE_KEYS = new Set(['id', 'title', 'steps'])
const SCENE_ID_RE = /^[a-z0-9][a-z0-9-]*$/

// Allowed keys per step op (additionalProperties:false at the step level).
const STEP_OP_KEYS = {
  key: ['key', 'caption'],
  type: ['type', 'caption'],
  waitFor: ['waitFor', 'timeoutMs', 'caption'],
  waitMs: ['waitMs', 'caption'],
  daemon: ['daemon', 'caption'],
  expect: ['expect', 'caption'],
}

function isPlainObject(v) {
  return v !== null && typeof v === 'object' && !Array.isArray(v)
}

/**
 * Validate a parsed scenario object. Collects ALL shape violations in one pass
 * (never stops at the first) so an authoring agent can fix a file in one edit.
 * @param {unknown} obj
 * @returns {{ok: true, scenario: object} | {ok: false, errors: string[]}}
 */
export function validateScenario(obj) {
  const errors = []
  const push = (pointer, msg) => errors.push(`${pointer}: ${msg}`)

  if (!isPlainObject(obj)) {
    return { ok: false, errors: ['scenario: must be a JSON object'] }
  }

  for (const k of Object.keys(obj)) {
    if (!TOP_LEVEL_KEYS.has(k)) push('scenario', `unknown top-level field "${k}"`)
  }

  if (obj.version !== 1) push('version', 'must be 1')
  if (typeof obj.title !== 'string' || !obj.title.trim()) {
    push('title', 'must be a non-empty string')
  }

  if (obj.fixture !== undefined) validateFixture(obj.fixture, push)
  if (obj.terminal !== undefined) validateTerminal(obj.terminal, push)

  validateScenes(obj.scenes, push)

  if (errors.length) return { ok: false, errors }
  // `fixture` gets a preset:'demo' default; `terminal` deliberately does NOT —
  // an absent terminal must stay absent so the bridge keeps its own default size
  // and the spawned argv stays byte-identical to today's.
  const scenario = {
    ...obj,
    fixture: { preset: 'demo', ...(isPlainObject(obj.fixture) ? obj.fixture : {}) },
  }
  return { ok: true, scenario }
}

function validateFixture(fixture, push) {
  if (!isPlainObject(fixture)) {
    push('fixture', 'must be an object')
    return
  }
  for (const k of Object.keys(fixture)) {
    if (!FIXTURE_KEYS.has(k)) push('fixture', `unknown field "${k}"`)
  }
  if (
    fixture.preset !== undefined &&
    (typeof fixture.preset !== 'string' || !fixture.preset.trim())
  ) {
    push('fixture.preset', 'must be a non-empty string')
  }
  if (fixture.seed !== undefined && !isPlainObject(fixture.seed)) {
    push('fixture.seed', 'must be an object')
  }
  if (fixture.env !== undefined) {
    if (!isPlainObject(fixture.env)) {
      push('fixture.env', 'must be an object')
    } else if (!Object.values(fixture.env).every((v) => typeof v === 'string')) {
      push('fixture.env', 'env values must be strings')
    }
  }
}

/**
 * Validate the optional top-level `terminal` size (BOS-571). Both members are
 * optional; an omitted member keeps the Go bridge's own default (cols 140 /
 * rows 36).
 */
function validateTerminal(terminal, push) {
  if (!isPlainObject(terminal)) {
    push('terminal', 'must be an object')
    return
  }
  for (const k of Object.keys(terminal)) {
    if (!TERMINAL_KEYS.has(k)) push('terminal', `unknown field "${k}"`)
  }
  if (
    terminal.cols !== undefined &&
    !isIntInRange(terminal.cols, TERMINAL_COLS_MIN, TERMINAL_COLS_MAX)
  ) {
    push('terminal.cols', `must be an integer ${TERMINAL_COLS_MIN}..${TERMINAL_COLS_MAX}`)
  }
  if (
    terminal.rows !== undefined &&
    !isIntInRange(terminal.rows, TERMINAL_ROWS_MIN, TERMINAL_ROWS_MAX)
  ) {
    push('terminal.rows', `must be an integer ${TERMINAL_ROWS_MIN}..${TERMINAL_ROWS_MAX}`)
  }
}

function validateScenes(scenes, push) {
  if (!Array.isArray(scenes)) {
    push('scenes', 'must be an array')
    return
  }
  if (scenes.length === 0) {
    push('scenes', 'must have at least one scene')
  }
  if (scenes.length > SCENARIO_MAX_SCENES) {
    push('scenes', `at most ${SCENARIO_MAX_SCENES} scenes (got ${scenes.length})`)
  }
  scenes.forEach((scene, i) => validateScene(scene, `scenes[${i}]`, push))
}

function validateScene(scene, p, push) {
  if (!isPlainObject(scene)) {
    push(p, 'must be an object')
    return
  }
  for (const k of Object.keys(scene)) {
    if (!SCENE_KEYS.has(k)) push(p, `unknown field "${k}"`)
  }
  if (scene.id !== undefined && (typeof scene.id !== 'string' || !SCENE_ID_RE.test(scene.id))) {
    push(p, 'id must match ^[a-z0-9][a-z0-9-]*$')
  }
  if (typeof scene.title !== 'string' || !scene.title.trim()) {
    push(p, 'title must be a non-empty string')
  }
  if (!Array.isArray(scene.steps)) {
    push(`${p}.steps`, 'must be an array')
    return
  }
  if (scene.steps.length === 0) {
    push(`${p}.steps`, 'must have at least one step')
  }
  scene.steps.forEach((step, j) => validateStep(step, `${p}.steps[${j}]`, push))
}

function validateStep(step, p, push) {
  if (!isPlainObject(step)) {
    push(p, 'must be an object')
    return
  }
  const ops = STEP_OPS.filter((k) => k in step)
  if (ops.length === 0) {
    const extras = Object.keys(step).filter((k) => k !== 'caption')
    const named = extras.length ? extras.map((k) => `"${k}"`).join(', ') : '(none)'
    push(p, `unknown step op ${named} (expected one of ${STEP_OPS.join('|')})`)
    return
  }
  if (ops.length > 1) {
    push(p, `step must have exactly one op, found ${ops.join(', ')}`)
    return
  }
  const op = ops[0]
  const allowed = STEP_OP_KEYS[op]
  for (const k of Object.keys(step)) {
    if (!allowed.includes(k)) push(p, `field "${k}" is not allowed on a "${op}" step`)
  }
  if (step.caption !== undefined && (typeof step.caption !== 'string' || !step.caption.trim())) {
    push(p, 'caption must be a non-empty string')
  }
  validateOp(op, step, p, push)
}

function validateOp(op, step, p, push) {
  switch (op) {
    case 'key':
    case 'type':
      if (typeof step[op] !== 'string' || !step[op].trim()) {
        push(p, `${op} must be a non-empty string`)
      }
      break
    case 'waitFor':
      validateExpectationArg(step.waitFor, `${p}.waitFor`, push)
      if (step.timeoutMs !== undefined && !isIntInRange(step.timeoutMs, 1, 60000)) {
        push(p, 'timeoutMs must be an integer 1..60000')
      }
      break
    case 'waitMs':
      if (!isIntInRange(step.waitMs, 1, 10000)) {
        push(p, 'waitMs must be an integer 1..10000')
      }
      break
    case 'daemon':
      if (
        !isPlainObject(step.daemon) ||
        typeof step.daemon.action !== 'string' ||
        !step.daemon.action.trim()
      ) {
        push(p, 'daemon.action must be a non-empty string')
      }
      break
    case 'expect': {
      const raw = step.expect
      const list = Array.isArray(raw) ? raw : [raw]
      if (Array.isArray(raw) && raw.length === 0) {
        push(`${p}.expect`, 'must be a non-empty array')
        break
      }
      list.forEach((e, i) => {
        const where = Array.isArray(raw) ? `${p}.expect[${i}]` : `${p}.expect`
        validateExpectationArg(e, where, push)
      })
      break
    }
  }
}

function validateExpectationArg(raw, where, push) {
  try {
    normalizeExpectation(raw, where)
  } catch (err) {
    push(where, err.message.replace(new RegExp(`^${escapeRe(where)}:\\s*`), ''))
  }
}

function escapeRe(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

function isIntInRange(v, min, max) {
  return Number.isInteger(v) && v >= min && v <= max
}

/**
 * Read + parse + validate a scenario file. Throws with a filename-prefixed
 * message on an unreadable file, invalid JSON, or validation failure (all
 * collected errors joined).
 * @param {string} filePath
 * @returns {{scenario: object, scenes: Array<{id:string,title:string,stepsHints:string[],expectedEvidence:string[]}>}}
 */
export function loadScenario(filePath) {
  let raw
  try {
    raw = fs.readFileSync(filePath, 'utf8')
  } catch (err) {
    throw new Error(`${filePath}: ${err.message}`)
  }
  let obj
  try {
    obj = JSON.parse(raw)
  } catch (err) {
    throw new Error(`${filePath}: invalid JSON: ${err.message}`)
  }
  const result = validateScenario(obj)
  if (!result.ok) {
    throw new Error(`${filePath}: invalid scenario:\n${result.errors.join('\n')}`)
  }
  return { scenario: result.scenario, scenes: deriveScenes(result.scenario) }
}

/**
 * Derive the downstream, `normalizeScenes`-shaped scene list
 * (`{id, title, stepsHints, expectedEvidence}`, see scripts/proof-brief.mjs:271)
 * that finalize/judge/chapters consume. Only this DERIVED output is
 * brief-compatible; authored steps are a richer shape by design.
 * `expectedEvidence` entries are the `displayText` of each `expect` in scene
 * order (D8), so the judge and gallery show labels, not regex source. Scene ids
 * keep the authored id or synthesize `scene-NN` with the same padding as
 * `normalizeScenes`.
 * @param {object} scenario a validated scenario (validateScenario output)
 */
export function deriveScenes(scenario) {
  const scenes = Array.isArray(scenario?.scenes) ? scenario.scenes : []
  return scenes.map((s, i) => {
    const expectedEvidence = []
    for (const step of Array.isArray(s.steps) ? s.steps : []) {
      if (!('expect' in step)) continue
      const list = Array.isArray(step.expect) ? step.expect : [step.expect]
      for (const e of list) expectedEvidence.push(displayText(normalizeExpectation(e)))
    }
    return {
      id: typeof s.id === 'string' && s.id ? s.id : `scene-${String(i + 1).padStart(2, '0')}`,
      title: String(s.title),
      stepsHints: [],
      expectedEvidence,
    }
  })
}
