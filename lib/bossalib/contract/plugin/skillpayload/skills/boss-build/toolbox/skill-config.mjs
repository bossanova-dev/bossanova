// Declarative skill-config convention for the boss-* skills.
// A single surface (.boss-skills.json at the repo root) that holds the
// path-glob -> lens map, build/test commands, test-manifest path, headless
// env-detection signals, and adapter selection the skills used to hard-code.
// Node builtins plus sibling toolbox modules (selection.mjs) only: this module is vendored into
// the reusable toolbox and runs in dependency-free cron worktrees.

import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs'
import { join, dirname, isAbsolute, parse as parsePath } from 'node:path'
import { reviewPolicyFromBlock } from './verify/verify-review-policy.mjs'
import { GITHUB_DEFAULT_STATES } from './tracker/github-config.mjs'
import { effectiveSelection, stageLabelRoles, validateSelectionBlock } from './selection.mjs'
import {
  schema as CONFIG_SCHEMA,
  validate as validateConfigSchema,
} from './skill-config-validate.generated.mjs'

export const CONFIG_FILENAME = '.boss-skills.json'

/**
 * Convert a path glob into an anchored RegExp.
 * Supported: '**' (any chars incl. '/'), '**' + '/' (zero-or-more segments),
 * '*' (any chars except '/'), '?' (one non-'/' char). All other chars literal.
 */
export function globToRegExp(glob) {
  let re = '^'
  for (let i = 0; i < glob.length; i++) {
    const c = glob[i]
    if (c === '*') {
      if (glob[i + 1] === '*') {
        i++ // consume the second '*'
        if (glob[i + 1] === '/') {
          i++ // consume the '/'
          re += '(?:.*/)?' // '**/' -> zero-or-more path segments
        } else {
          re += '.*' // '**' -> any chars including '/'
        }
      } else {
        re += '[^/]*' // '*' -> any chars except '/'
      }
    } else if (c === '?') {
      re += '[^/]'
    } else if ('.+^${}()|[]\\'.includes(c)) {
      re += '\\' + c // escape regex metacharacters (note: '/' is not one)
    } else {
      re += c
    }
  }
  return new RegExp(re + '$')
}

/**
 * Stock Linear workflow names; each tracker supplies its own defaults.
 */
export const DEFAULT_TRACKER_STATES = Object.freeze({
  unplanned: 'Backlog',
  planned: 'Todo',
  inProgress: 'In Progress',
  inReview: 'In Review',
  done: 'Done',
})

/** Pipeline label names a tracker block gets when it does not name its own. */
export const DEFAULT_PIPELINE_LABELS = Object.freeze({
  agentBuild: 'agent-build',
  needsHuman: 'needs-human',
  agentPlan: 'agent-plan',
  agentQuestion: 'agent-question',
  epic: 'epic',
})

/**
 * Fill each declared tracker block's defaults: the MCP server name (the adapter's own name — the
 * preflight resolves the session's actual spelling), the stock state names and the standard
 * pipeline labels, each overridable per key. A repo therefore declares only whatever differs.
 *
 * Zero-config (D11): when the SELECTED tracker is `linear` and the config declares no block for
 * it, a default block is synthesized — the stock server name, states and labels, and NO `team`.
 * The team is resolved per run (resolveTrackerTeam), so the synthesized block alone never makes a
 * repo configured. Synthesis is Linear-only; other trackers require an explicit block.
 */
export function withTrackerDefaults(config) {
  const declared = config?.trackerConfig
  const blocks = declared === undefined ? {} : declared
  if (!blocks || typeof blocks !== 'object' || Array.isArray(blocks)) return config
  const selected = config?.adapters?.tracker
  const synthesize = selected === 'linear' && !Object.hasOwn(blocks, selected)
  if (!synthesize && declared === undefined) return config
  const filled = {}
  for (const [adapter, tc] of Object.entries(synthesize ? { ...blocks, [selected]: {} } : blocks)) {
    if (!tc || typeof tc !== 'object' || Array.isArray(tc)) {
      filled[adapter] = tc
      continue
    }
    const object = (value) =>
      value && typeof value === 'object' && !Array.isArray(value) ? value : {}
    filled[adapter] = {
      ...tc,
      ...(adapter === 'github' ? {} : { mcpServer: tc.mcpServer || adapter }),
      states: {
        ...(adapter === 'github' ? GITHUB_DEFAULT_STATES : DEFAULT_TRACKER_STATES),
        ...object(tc.states),
      },
      labels: { ...DEFAULT_PIPELINE_LABELS, ...object(tc.labels) },
    }
  }
  return { ...config, trackerConfig: filled }
}

export const DEFAULT_CONFIG = Object.freeze({
  // The review boss-build runs. `skill` (absent by default) names a repo skill — or a chain of
  // skills, run in order — that REPLACES boss-review; it must emit the same verdict JSON
  // (review-verdict.mjs). `secondModel` turns boss-review's cross-agent second model on or off.
  review: { secondModel: true },
  // Review dispatch bounds. Review time is never predicted: `hangGuardMinutes` bounds one dispatch
  // so a hung worker is not waited on forever (env: BOSS_REVIEW_HANG_GUARD_MINUTES), and
  // `softBudgetMinutes` bounds only the optional second model (env:
  // BOSS_REVIEW_SOFT_BUDGET_MINUTES).
  reviewDefaults: {
    softBudgetMinutes: 60,
    hangGuardMinutes: 30,
  },
  // Epic scheduling budgets. Only the per-child wall clock lives here today: the epic driver reads
  // it once and hands the expired/not-expired bit to `classifyChildLiveness`, which short-circuits
  // to `wall-clock-expired/fail-isolate` ahead of every liveness read.
  epicDefaults: {
    // Minutes a single child may run before the epic driver treats its budget as spent. Six hours,
    // not the ninety minutes this once hardcoded: children on a repo of this class routinely
    // measure 2-4 h, so a short clock expires on healthy work and fail-isolates a child that was
    // still making progress. Expiry is a budget fact, not death evidence — a genuinely dead child
    // is caught far earlier by the liveness and frozen-repair-lease classifiers, so the cost of a
    // long clock is only that a hung child holds its DAG slot longer, never a missed death. Must
    // be a POSITIVE integer: zero or negative would expire every child at launch. An over-large
    // clock is the symmetric failure: an operator typo can hold a hung child's DAG slot for weeks,
    // and expiry is the only mechanism that ever reclaims that slot.
    childWallClockMinutes: 360,
  },
  // Cadence of the post-terminal `notes` phase (and, in a core that also runs a pre-PR
  // `knowledge` phase, of that phase's shared per-run sampling roll).
  //
  // `sampleRate` is the probability that a top-level run performs its reporting dispatches at
  // all: 1.0 runs them every time, 0.33 roughly one run in three, 0 never. It DEFAULTS TO 1.0 on
  // purpose — the knob exists to let a busy operator pay less for reporting, and a repo that
  // never set it must keep the behaviour it already had. 0.33 is the recommended production
  // setting (see docs/skills/skill-config.md): a corpus fed by every core in the fleet saturates
  // long before every single run is sampled, and each unsampled run gives its whole post-terminal
  // dispatch budget back.
  //
  // ONE roll governs BOTH phases of a run. Rolling per phase would let a run pay for knowledge
  // and skip notes (or the reverse), which is neither the old behaviour nor the configured rate.
  notesDefaults: {
    sampleRate: 1,
  },
  env: {
    headlessSignals: [
      { var: 'BOSS_UNATTENDED', equals: 'true' },
      { var: 'BOSS_CRON', equals: 'true' },
      { var: 'BS_HEADLESS', equals: '1' },
      { var: 'OPENCLAW_SESSION', present: true },
    ],
    headlessWhenNoTty: true,
  },
  adapters: { tracker: 'linear', publish: 'proof', sessionRunner: 'bossd' },
  // Repo-local boss-* extensions may be authored for either supported agent root. Discovery uses
  // this ordered candidate list and dedupes mirrored extensions by name, so a repo can carry both
  // generated mirrors without dispatching every extension twice.
  extensionRoots: ['.claude/skills', '.codex/skills'],
  // Concrete tracker / publish identity, keyed by the selected adapter. These blocks
  // deliberately DEFAULT TO EMPTY: repo-private values (team, project key, publish bucket, public
  // base URL) live ONLY in a checkout's own .boss-skills.json, never as a literal in this vendored
  // module — that is what keeps the published cores project-agnostic. For the Linear tracker,
  // loadSkillConfig() synthesizes a team-less default block (withTrackerDefaults), and each run
  // resolves the team itself: an explicit `team` here, else a --team argument, else the single
  // team the tracker MCP can see (resolveTrackerTeam). No MCP server, no team, or several teams
  // with nothing picking one leaves the repo unconfigured, so a core self-disables cleanly instead
  // of demanding an MCP server or a team that does not exist there.
  trackerConfig: {},
  publishConfig: {},
  // Plan storage is deliberately separate from publishConfig: proof artifacts
  // continue using the publish adapter even when implementation plans live on
  // the configured tracker.
  planStorage: { kind: 'tracker-attachment' },
  // boss-plan dependency-scan tuning a repo declares ONCE instead of rebuilding per run:
  // `moduleRoots` (top-level module names area extraction resolves against), `repoWideTokens`
  // (append-only registries and generated mirrors too shared to mean a conflict) and
  // `areaAliases` (a path -> its generated mirrors). Empty by default so the published cores stay
  // project-agnostic; the step-5 scan unions these with the per-run payload.
  planDependencies: { moduleRoots: [], repoWideTokens: [], areaAliases: {} },
  // The `##`-section contract for the tracker DESCRIPTION boss-plan writes. The plan itself lives in
  // the plan attachment; the description is a short summary plus the reporter's original notes, so
  // only those two are required. The other headings are recognised (older plans carry them, and a
  // short `## Key changes` list feeds the dependency scan) but never required. `version` is stamped
  // in-band as `- Contract: v<N>` under `## Planning`.
  planContract: {
    version: 1,
    sections: [
      { heading: '## Summary', required: 'always' },
      { heading: '## Approach', required: 'optional' },
      { heading: '## Key changes', required: 'optional' },
      { heading: '## Testing', required: 'optional' },
      { heading: '## Risks / unknowns', required: 'optional' },
      { heading: '## Premises', required: 'optional' },
      { heading: '## Acceptance criteria', required: 'optional' },
      { heading: '## Required proof', required: 'optional' },
      { heading: '## Proof harness analysis', required: 'optional' },
      { heading: '## Why this needs a human', required: 'needs-human' },
      { heading: '## Open Questions', required: 'open-questions' },
      { heading: '## Planning', required: 'optional' },
      { heading: '## Original notes', required: 'always' },
    ],
  },
})

// --- Loader: discovery, merge, validation ---------------------------------

export function findConfigFile(startDir) {
  let dir = startDir
  const { root } = parsePath(startDir)
  for (;;) {
    const candidate = join(dir, CONFIG_FILENAME)
    if (existsSync(candidate)) return candidate
    if (dir === root) return null
    const parent = dirname(dir)
    if (parent === dir) return null // fixed point (e.g. a relative startDir): stop, don't spin
    dir = parent
  }
}

export function mergeConfig(base, override) {
  const out = { ...base }
  for (const [k, v] of Object.entries(override || {})) {
    const b = base[k]
    if (Array.isArray(v)) {
      out[k] = v // arrays replace wholesale (a predictable override)
    } else if (v && typeof v === 'object' && b && typeof b === 'object' && !Array.isArray(b)) {
      out[k] = { ...b, ...v } // objects shallow-merge per key
    } else {
      out[k] = v
    }
  }
  return out
}

export const ADAPTER_KINDS = new Set(['tracker', 'publish', 'sessionRunner'])

// Top-level and reviewDefaults keys of the retired review machinery, tolerated with a warning.
const RETIRED_REVIEW_KEYS = Object.freeze(['lensMap', 'reviewLedger'])
const RETIRED_REVIEW_DEFAULT_KEYS = Object.freeze([
  'rounds',
  'deltaFileThreshold',
  'maxDispatchedRounds',
  'forceFull',
])

export const REVIEW_DEFAULT_SOFT_BUDGET_MINUTES = DEFAULT_CONFIG.reviewDefaults.softBudgetMinutes
export const REVIEW_DEFAULT_HANG_GUARD_MINUTES = DEFAULT_CONFIG.reviewDefaults.hangGuardMinutes
// Slack a caller holding a review on its heartbeat adds to the hang guard, so a dispatch that runs
// its full guard is not read as an abandoned review.
export const REVIEW_HEARTBEAT_MARGIN_MS = 15 * 60_000

export const EPIC_DEFAULT_CHILD_WALL_CLOCK_MINUTES =
  DEFAULT_CONFIG.epicDefaults.childWallClockMinutes
// Unlike maxDispatchedRounds, whose default and ceiling are both 6, the child wall-clock default
// and ceiling differ, so callers need a distinct constant for the upper bound.
export const EPIC_MAX_CHILD_WALL_CLOCK_MINUTES = 1440
export const NOTES_DEFAULT_SAMPLE_RATE = DEFAULT_CONFIG.notesDefaults.sampleRate

// The `required` classifications a planContract section may carry. `optional` means RECOGNISED but
// never required: the section may appear without tripping unknown-heading detection, and
// requiredPlanSections() still excludes it. It exists so a template the skill itself prescribes can
// be registered without newly requiring it of the plans already stamped against this contract.
/**
 * The CLOSED vocabulary of description-normalization transform ids a repo may declare as tolerated.
 *
 * A tracker that normalizes markdown on write reshapes a description without changing what it
 * means. The post-save read-back has to tell that apart from a transcription slip, and the only
 * honest way to do it is to name the reshapings this repo has OBSERVED — measured, one id each —
 * and treat everything else as drift. The list is closed on purpose: the failure mode of a
 * free-text tolerance list is a typo that silently disables the gate, so an unknown id is a config
 * error rather than a silently widened tolerance.
 *
 * Every id is generic. No tracker vendor, product, or hostname appears here or may be added: this
 * file ships inside globally installed cores, and vendor-specific knowledge belongs in the
 * consuming repo's own `.boss-skills.json` entry.
 *
 * - `unordered-list-marker-substitution` — a leading `-`/`+` list marker rewritten to `*` (or any
 *   other single canonical marker). Byte-count neutral, so a size comparison cannot detect it.
 * - `emphasis-delimiter-whitespace-migration` — whitespace moving across an emphasis delimiter,
 *   e.g. `**1.** ` stored as `**1. **`.
 * - `emphasis-span-restructuring` — one emphasis span containing an inline code span rewritten as
 *   emphasis + code + emphasis (medial), as code + emphasis (leading) or emphasis + code
 *   (trailing), including a split that wraps across one line break. Renders identically; adds
 *   bytes. Emphasis deleted or demoted, and a split across a blank line, stay drift.
 * - `trailing-whitespace-trimming` — trailing spaces/tabs removed from a line.
 * - `terminal-newline-trimming` — trailing newlines removed from the end of the document.
 * - `table-delimiter-row-normalization` — the dash run inside each cell of a table delimiter row
 *   rewritten to a different length, or the row re-padded, e.g. `| --- | --- |` stored as
 *   `| -- | -- |` and `|---|---|---|` stored as `| -- | -- | -- |`. Alignment
 *   colons are NOT part of this transform: a cell's colons carry alignment where its dash count
 *   carries nothing, so a row that lost one is a semantic change and stays drift.
 * - `block-boundary-blank-line-normalization` — a blank line inserted or removed at a LIST block
 *   boundary: between a list and the heading that follows it, between two adjacent list items, or
 *   between a paragraph and a following list item that can interrupt it (a bullet item, or an
 *   ordered item numbered `1.` / `1)`; a `3.` item cannot, so that blank line stays drift).
 *   Unlike the transforms above this one is not purely cosmetic — a blank line inside a list makes
 *   the list loose, which changes the rendered markup, including when the "paragraph" line is
 *   really a lazy continuation inside a list — so it is a deliberate widening, bounded to those
 *   three boundaries and never dropping a line that carries text. That bound is what keeps a
 *   dropped list item, and two lists merged across the paragraph that separated them, drift.
 * - `backslash-escape-insertion` — a backslash escape inserted before `_`, `*`, `[`, `]`, `~`, `#` or backticks (measured
 *   in tracker writes, including a Next.js dynamic-route path and a bracketed label; other
 *   characters were not measured), e.g. `stg_<source>__` stored as `stg\_<source>\_\_`.
 *   Escapes before the new characters stay at content start after indentation, quote and list
 *   prefixes; ATX headings keep escaped hashes. Each line reaches a code-span fixpoint. Escaped
 *   inline backticks and strikethrough tildes compare equal to their bare forms as accepted residuals.
 *   An escaped backslash (`\\_`)
 *   is a different character and stays drift, and a backslash inside an inline code span, a
 *   fenced block or an indented code block is literal and is never touched. Accepted residual of
 *   a side-agnostic rule: a literal `\*a\*` and an italic `*a*` compare equal. Bracket escapes stay
 *   on lines containing `](` or `][` (including escaped brackets), or an opening `[label]:`
 *   reference definition with up to three leading spaces. A GFM bare-URL autolink containing
 *   escaped brackets is not recognised as a link.
 * - `link-destination-angle-bracket-wrapping` — inline link and image destinations wrapped in
 *   angle brackets compare equal to bare destinations only when nonempty, without whitespace or
 *   angle brackets and with balanced parentheses. Code, autolinks and reference definitions stay.
 */
export const DESCRIPTION_NORMALIZATION_TRANSFORMS = Object.freeze([
  'unordered-list-marker-substitution',
  'emphasis-delimiter-whitespace-migration',
  'emphasis-span-restructuring',
  'trailing-whitespace-trimming',
  'terminal-newline-trimming',
  'table-delimiter-row-normalization',
  'block-boundary-blank-line-normalization',
  'backslash-escape-insertion',
  'link-destination-angle-bracket-wrapping',
])

const DESCRIPTION_NORMALIZATION_TRANSFORM_SET = new Set(DESCRIPTION_NORMALIZATION_TRANSFORMS)

/**
 * How a repo wants the post-save read-back to treat a difference it cannot attribute to a declared
 * transform. `warn` reports and continues; `block` fails the run the way every drift cause did
 * before the severity split. The three content-loss causes are NOT governed by this — they block
 * unconditionally.
 */
export const UNATTRIBUTED_DRIFT_SEVERITIES = Object.freeze(['warn', 'block'])

/** The default severity. `warn`, because the write has already landed by the time it is known. */
export const DEFAULT_UNATTRIBUTED_DRIFT_SEVERITY = 'warn'

export const PLAN_SECTION_REQUIRED_KINDS = new Set([
  'always',
  'needs-human',
  'open-questions',
  'optional',
])

// --- Schema validation -------------------------------------------------------
//
// The hard (throwing) constraints live in the source repo's skill-config-schema/schema.mjs and
// arrive here compiled into skill-config-validate.generated.mjs. validateConfig keeps only what a
// schema cannot say: warn-and-default coercion, retired-key migration, deferred stage blocks, the
// description-normalization vocabulary, and the selection block (selection.mjs owns its rules).

// Blocks whose faults a load may defer (see `deferStageBlocks`), keyed by top-level key.
const SCHEMA_STAGE_BLOCKS = new Set(['retro', 'verifyDefaults', 'planDependencies'])
const TOP_LEVEL_ORDER = Object.keys(CONFIG_SCHEMA.properties)

// How far each schema-path step moves: [segments consumed in the schema path, instance segments].
const SCHEMA_STEPS = {
  properties: [2, 1],
  patternProperties: [2, 1],
  additionalProperties: [1, 1],
  items: [1, 1],
  propertyNames: [1, 0],
  allOf: [2, 0],
  anyOf: [2, 0],
  oneOf: [2, 0],
  not: [1, 0],
}

const decodePointer = (pointer) =>
  pointer
    .replace(/^#/, '')
    .split('/')
    .slice(1)
    .map((segment) => segment.replaceAll('~1', '/').replaceAll('~0', '~'))

// The schema nodes along `segments`, root first, each with the instance depth it validates, plus
// the keyword left over at the end (the one that failed).
function schemaTrail(segments) {
  const trail = [{ node: CONFIG_SCHEMA, depth: 0, end: 0 }]
  let index = 0
  while (index < segments.length && Object.hasOwn(SCHEMA_STEPS, segments[index])) {
    const [width, consumes] = SCHEMA_STEPS[segments[index]]
    const last = trail.at(-1)
    const node =
      width === 2 ? last.node[segments[index]]?.[segments[index + 1]] : last.node[segments[index]]
    if (!node || typeof node !== 'object') break
    index += width
    trail.push({ node, depth: last.depth + consumes, end: index })
  }
  return { trail, keyword: segments[index] }
}

function fillMessage(template, { instance, data, extra }) {
  return template.replace(/\{(\d+|json|extra|\.\w+)\}/g, (_, token) => {
    if (token === 'json') return String(JSON.stringify(data))
    if (token === 'extra') return String(extra)
    if (token.startsWith('.')) return String(data?.[token.slice(1)])
    return instance[Number(token)] ?? ''
  })
}

function classifySchemaError(error, config) {
  let segments = decodePointer(error.schemaPath)
  let instance = decodePointer(error.instancePath)
  let { trail, keyword } = schemaTrail(segments)
  // A missing required property reads as a `type` failure of that property's own schema.
  const missing = error.params?.missingProperty
  if (keyword === 'required' && trail.at(-1).node.properties?.[missing]) {
    segments = [...segments.slice(0, trail.at(-1).end), 'properties', missing, 'type']
    instance = [...instance, missing]
    ;({ trail, keyword } = schemaTrail(segments))
  }
  const extra = error.params?.additionalProperty ?? error.params?.propertyName ?? error.propertyName
  const own = trail.at(-1).node
  if (keyword === 'additionalProperties' && own['x-unknown'] === 'warn') {
    return { warning: `${[...instance, extra].join('.')} is not a known key; ignored` }
  }
  let message = null
  for (let i = trail.length - 1; i >= 0 && message === null; i -= 1) {
    const { node, depth } = trail[i]
    const msg = node['x-msg']
    const template =
      typeof msg === 'string'
        ? msg
        : i === trail.length - 1 && msg && typeof msg[keyword] === 'string'
          ? msg[keyword]
          : null
    if (template === null) continue
    const data = instance.slice(0, depth).reduce((value, key) => value?.[key], config)
    message = fillMessage(template, { instance, data, extra })
  }
  message ??= `${instance.join('.') || '(config)'} ${error.message}`
  return { block: SCHEMA_STAGE_BLOCKS.has(instance[0]) ? instance[0] : null, message, instance }
}

/**
 * Run the compiled schema over `config`: unknown keys of a warn-closed object as `warnings`, and
 * every other failure as `errors` ({block, message}) in top-level key order, `block` naming the
 * deferrable stage block it belongs to, or null.
 */
function schemaFaults(config) {
  const warnings = []
  const errors = []
  if (!validateConfigSchema(config)) {
    for (const error of validateConfigSchema.errors) {
      const result = classifySchemaError(error, config)
      if (result.warning) warnings.push(result.warning)
      else errors.push(result)
    }
  }
  const rank = (entry) => {
    const index = TOP_LEVEL_ORDER.indexOf(entry.instance[0])
    return index === -1 ? TOP_LEVEL_ORDER.length : index
  }
  errors.sort((a, b) => rank(a) - rank(b))
  return { warnings: [...new Set(warnings)], errors }
}

const STAGE_BLOCK_FAULTS = Symbol('stageBlockFaults')

function assertStageBlockReadable(config, block) {
  const message = config?.[STAGE_BLOCK_FAULTS]?.get(block)
  if (message) throw new Error(message)
}

export function validateConfig(config, source, { deferStageBlocks = false } = {}) {
  const fail = (msg) => {
    throw new Error(`skill-config: invalid config from ${source}: ${msg}`)
  }
  const faults = new Map()
  if (deferStageBlocks)
    Object.defineProperty(config, STAGE_BLOCK_FAULTS, { value: faults, configurable: true })
  const defer = (block, message) => {
    if (faults.has(block)) return
    faults.set(block, message)
    console.warn(`skill-config: deferred ${block}: ${message}; run boss skills install`)
  }
  const stageBlock = (block, validate) => {
    if (!deferStageBlocks) return validate()
    try {
      validate()
    } catch (error) {
      defer(block, error.message)
    }
  }
  const { warnings, errors } = schemaFaults(config)
  for (const warning of warnings) console.warn(`skill-config: ${source}: ${warning}`)
  for (const { block, message } of errors) {
    if (block) stageBlock(block, () => fail(message))
    else fail(message)
  }
  // Retired review keys (the lens map, default review rounds, delta-depth tuning and the dispatch
  // ledger) are tolerated so an older .boss-skills.json keeps loading; each is ignored with a warning.
  for (const key of RETIRED_REVIEW_KEYS) {
    if (config[key] !== undefined) {
      console.warn(`skill-config: ${source}: ${key} is retired and ignored`)
      delete config[key]
    }
  }
  if (config.reviewDefaults !== undefined) {
    for (const key of RETIRED_REVIEW_DEFAULT_KEYS) {
      if (config.reviewDefaults[key] !== undefined) {
        console.warn(`skill-config: ${source}: reviewDefaults.${key} is retired and ignored`)
        delete config.reviewDefaults[key]
      }
    }
    for (const [key, fallback] of [
      ['softBudgetMinutes', REVIEW_DEFAULT_SOFT_BUDGET_MINUTES],
      ['hangGuardMinutes', REVIEW_DEFAULT_HANG_GUARD_MINUTES],
    ]) {
      const value = config.reviewDefaults[key]
      if (value === undefined) {
        config.reviewDefaults[key] = fallback
      } else if (!Number.isInteger(value) || value < 1) {
        console.warn(
          `skill-config: ${source}: reviewDefaults.${key} must be a positive integer of minutes; using ${fallback}`,
        )
        config.reviewDefaults[key] = fallback
      }
    }
  }
  // review: which review boss-build runs. A malformed value warns and falls back to the default
  // review (boss-review with its second model on) rather than refusing to load.
  if (config.review !== undefined) {
    if (!config.review || typeof config.review !== 'object' || Array.isArray(config.review)) {
      console.warn(`skill-config: ${source}: review must be an object; using the default review`)
      config.review = { ...DEFAULT_CONFIG.review }
    }
    if (config.review.secondModel !== undefined && typeof config.review.secondModel !== 'boolean') {
      console.warn(`skill-config: ${source}: review.secondModel must be true or false; using true`)
      config.review.secondModel = true
    }
    if (config.review.skill !== undefined && reviewSkillChain(config.review.skill) === null) {
      console.warn(
        `skill-config: ${source}: review.skill must be a skill name or a non-empty list of them; using boss-review`,
      )
      delete config.review.skill
    }
  }
  // epicDefaults: the epic scheduling budgets. A PRESENT block must be an object (the schema
  // rejects `epicDefaults: 360`, a repo trying to set the budget and silently setting nothing), but
  // a malformed *key* warns and coerces: a bad wall clock must never leave the driver comparing
  // against NaN, where every child would read as never-expired. Zero is not valid either — a
  // zero-minute clock expires every child the instant it launches.
  if (config.epicDefaults !== undefined) {
    const minutes = config.epicDefaults.childWallClockMinutes
    if (!Number.isInteger(minutes) || minutes < 1 || minutes > EPIC_MAX_CHILD_WALL_CLOCK_MINUTES) {
      console.warn(
        `skill-config: ${source}: epicDefaults.childWallClockMinutes must be an integer in [1, ${EPIC_MAX_CHILD_WALL_CLOCK_MINUTES}]; using ${EPIC_DEFAULT_CHILD_WALL_CLOCK_MINUTES}`,
      )
      config.epicDefaults.childWallClockMinutes = EPIC_DEFAULT_CHILD_WALL_CLOCK_MINUTES
    }
  }
  // notesDefaults: a malformed BLOCK is rejected by the schema, while a malformed SCALAR warns and
  // falls back to 1.0 — the fail-safe direction: a typo costs a few extra dispatches, where a
  // coerced 0 would silently switch a repo's reporting off.
  if (config.notesDefaults !== undefined) {
    const rate = config.notesDefaults.sampleRate
    if (typeof rate !== 'number' || !Number.isFinite(rate) || rate < 0 || rate > 1) {
      console.warn(
        `skill-config: ${source}: notesDefaults.sampleRate must be a number in [0, 1]; using ${NOTES_DEFAULT_SAMPLE_RATE}`,
      )
      config.notesDefaults.sampleRate = NOTES_DEFAULT_SAMPLE_RATE
    }
  }
  if (config.verifyDefaults !== undefined && !faults.has('verifyDefaults')) {
    // Verify refuses this carry at runtime; the warning says why an approval never carries.
    const { generatedPaths, generatedChecks } = config.verifyDefaults
    if (generatedPaths?.length > 0 && !(generatedChecks?.length > 0))
      console.warn(
        `skill-config: ${source}: verifyDefaults.generatedPaths has no generatedChecks to prove it regenerated, so no approval carries across a rebase`,
      )
  }
  for (const [adapter, tc] of Object.entries(config.trackerConfig)) {
    // descriptionNormalization: absent means the FULL vocabulary; an explicitly EMPTY `tolerated`
    // array opts in to byte-exact strictness. The schema rejects a STRUCTURAL fault; an unfamiliar
    // but well-formed id warns and is dropped here, because this file ships in every installed
    // core and a hard failure would turn an additive vocabulary widening into a crash for every
    // consumer of the config. Dropping is the safe direction — a smaller tolerated set makes the
    // comparison stricter, so the worst case is a spurious drift, never a false pass.
    const dn = tc.descriptionNormalization
    if (dn && Array.isArray(dn.tolerated)) {
      const unrecognised = dn.tolerated.filter(
        (id) => !DESCRIPTION_NORMALIZATION_TRANSFORM_SET.has(id),
      )
      if (unrecognised.length > 0) {
        console.warn(
          `skill-config: ${source}: trackerConfig.${adapter}.descriptionNormalization.tolerated ` +
            `names ${unrecognised.map((id) => JSON.stringify(id)).join(', ')}, which this copy of the ` +
            `transform vocabulary does not recognise; dropping it, which makes the comparison STRICTER. ` +
            `Known ids: ${DESCRIPTION_NORMALIZATION_TRANSFORMS.join(', ')}`,
        )
        dn.tolerated = dn.tolerated.filter((id) => DESCRIPTION_NORMALIZATION_TRANSFORM_SET.has(id))
      }
    }
    // Same role split for the severity, but the fallback is the DEFAULT (`warn`): an unreadable
    // severity must not silently start failing completed runs.
    if (dn && 'onUnattributedDrift' in dn) {
      const severity = dn.onUnattributedDrift
      if (!UNATTRIBUTED_DRIFT_SEVERITIES.includes(severity)) {
        console.warn(
          `skill-config: ${source}: trackerConfig.${adapter}.descriptionNormalization.onUnattributedDrift ` +
            `is ${JSON.stringify(severity)}, which this copy does not recognise; falling back to ` +
            `${JSON.stringify(DEFAULT_UNATTRIBUTED_DRIFT_SEVERITY)}. Known values: ${UNATTRIBUTED_DRIFT_SEVERITIES.join(', ')}`,
        )
        dn.onUnattributedDrift = DEFAULT_UNATTRIBUTED_DRIFT_SEVERITY
      }
    }
    // selection: narrows WHICH CANDIDATES each pipeline stage considers (rules: selection.mjs
    // `validateSelectionBlock`, and docs/skills/skill-config.md). Every fault THROWS, unknown and
    // legacy keys included: an ignored filter key silently WIDENS the scan. Loads may defer the
    // fault, but every selection reader must still reject it before deriving a query.
    if ('selection' in tc)
      stageBlock(`trackerConfig.${adapter}.selection`, () => {
        for (const error of validateSelectionBlock(
          tc.selection,
          `trackerConfig.${adapter}.selection`,
        )) {
          fail(error)
        }
      })
  }
  if (config.planStorage.kind === 'r2') {
    console.warn(
      `skill-config: ${source}: planStorage.kind="r2" is deprecated and ignored; using "tracker-attachment"`,
    )
    config.planStorage = { kind: 'tracker-attachment' }
  }
}

// --- Detected happy defaults ----------------------------------------------
//
// DEFAULT_CONFIG deliberately ships no `commands` block (see above), so a repo without a
// .boss-skills.json would otherwise hand every core a null build/lint/test command. This layer
// recovers commands the repo ITSELF declares in Makefile targets, package.json scripts or Cargo/Go.
// Two rules keep it honest:
//   1. Read DECLARED TARGETS, not marker presence. A Makefile with no `check:` or `lint:` target
//      yields no lint command; guessing `make lint` there would hand a core a command that fails.
//   2. First writer wins, in the fixed order Makefile -> package.json -> Cargo.toml -> go.mod.
//      Makefile is the repo's front door; the toolchains fill only keys it left empty.
//      Makefile aliases prefer check > lint and format > fmt > fix; test-affected fills testAffected.
// `commands.testModule` is NEVER detected: per-module test targets are a repo-shaped convention,
// not a language fact, so its absence stays the honest answer outside a repo that configures it.

/** Command keys this layer can detect. `testModule` is deliberately absent. */
const DETECTED_COMMAND_KEYS = ['build', 'lint', 'format', 'test', 'testAffected']

// A Makefile rule head: everything before `:` / `::`, excluding `:=` / `::=` assignments. One head
// can declare SEVERAL targets (`build lint:`), so the names are captured as a group and split.
const MAKE_RULE_RE = /^([A-Za-z0-9][^:=]*?)\s*:{1,2}(?![=:])(.*)$/
/** A single target name, once the head is split on whitespace. */
const MAKE_TARGET_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9_./-]*$/
// `build: CFLAGS=-O2` scopes a variable to a target; on its own it declares no rule and no recipe,
// so `make build` there fails. Only a head NOT followed by an assignment counts as a declaration.
const MAKE_TARGET_VAR_RE = /^\s*[A-Za-z_][A-Za-z0-9_.]*\s*[:+?]?=/

// `define NAME` ... `endef` brackets a multi-line variable body. Its lines are TEXT, not rules —
// a `build:` inside one is a template make only sees after an `$(eval)` that may never happen, so
// scanning it would emit `make build` for a target the repo does not declare.
const MAKE_DEFINE_RE = /^\s*define\s/
const MAKE_ENDEF_RE = /^\s*endef\b/

// `ifeq`/`ifneq`/`ifdef`/`ifndef` ... `endif` brackets a conditional body. Whether that body is
// live depends on variables this static reader does not evaluate, so a target declared only there
// is not a target the repo unconditionally declares — reporting it would emit e.g. `make test` for
// a repo where the branch is false. Skipped wholesale, exactly like a `define` body.
// An `else` / `else ifeq (...)` line opens no new conditional: the same `endif` closes it.
// The lookahead, not `\b`, is what make requires: `\b` also matches before a `-`, so the legal
// target `ifeq-check:` would open a conditional that swallows the rest of the file (and
// `endif-foo:` would close one early, reopening scanning inside a conditional body).
const MAKE_COND_RE = /^\s*(?:ifeq|ifneq|ifdef|ifndef)(?=[\s(]|$)/
const MAKE_ENDIF_RE = /^\s*endif(?=[\s(]|$)/

function readTextFile(path) {
  try {
    return readFileSync(path, 'utf8')
  } catch {
    return null // unreadable is indistinguishable from absent for detection purposes
  }
}

/**
 * The declared Makefile targets in `dir`, as a Set (empty when there is no makefile).
 *
 * This is a static reader, not a make implementation, so it resolves every gap the same way — by
 * under-detecting rather than reporting a target the repo may not actually declare:
 *   - **Conditionals.** A target reachable only inside an `ifeq`/`ifneq`/`ifdef`/`ifndef` body
 *     (including its `else` branches) is NOT reported: whether that branch is live means evaluating
 *     variables, and emitting `make test` for a repo where the condition is false hands a core a
 *     command that fails. A target declared after the closing `endif` is reported as usual.
 *   - **`include`d makefiles.** Only the top-level file is read, so a target defined in an included
 *     fragment is missed. That under-detects too.
 */
function makefileTargets(dir) {
  const out = new Set()
  // GNU make's own lookup order, not alphabetical: GNUmakefile, then makefile, then Makefile.
  // Scanning the wrong one in a repo carrying two would report targets make never reads.
  for (const name of ['GNUmakefile', 'makefile', 'Makefile']) {
    const text = readTextFile(join(dir, name))
    if (text === null) continue
    let inDefine = false
    let condDepth = 0 // nesting depth of open ifeq/ifneq/ifdef/ifndef blocks
    // make joins a line ending in `\` with the next before parsing, so a rule head split across
    // two lines is still one declaration. Recipe lines stay indented and so still never match.
    for (const line of text.replace(/\\\n/g, ' ').split('\n')) {
      // An unterminated `define` or `ifeq` swallows the rest of the file, which under-detects
      // rather than over-detects — the safe direction, and the same file make itself would reject.
      if (inDefine) {
        if (MAKE_ENDEF_RE.test(line)) inDefine = false
        continue
      }
      if (condDepth > 0) {
        // Nested conditionals must not let the first `endif` reopen scanning, and an `else` (or
        // `else ifeq ...`) branch is still inside the same conditional, so it changes no depth.
        if (MAKE_COND_RE.test(line)) condDepth++
        else if (MAKE_ENDIF_RE.test(line)) condDepth--
        continue
      }
      if (MAKE_DEFINE_RE.test(line)) {
        inDefine = true
        continue
      }
      if (MAKE_COND_RE.test(line)) {
        condDepth = 1
        continue
      }
      const m = MAKE_RULE_RE.exec(line)
      if (!m || MAKE_TARGET_VAR_RE.test(m[2])) continue
      for (const target of m[1].split(/\s+/)) {
        if (MAKE_TARGET_NAME_RE.test(target)) out.add(target)
      }
    }
    break // one makefile per directory; the one make itself would read wins
  }
  return out
}

/** The declared package.json scripts in `dir`, as an object (empty when absent/malformed). */
function packageScripts(dir) {
  const text = readTextFile(join(dir, 'package.json'))
  if (text === null) return {}
  let parsed
  try {
    parsed = JSON.parse(text)
  } catch {
    return {} // a broken package.json detects nothing rather than throwing at load time
  }
  const scripts = parsed && typeof parsed === 'object' ? parsed.scripts : null
  return scripts && typeof scripts === 'object' && !Array.isArray(scripts) ? scripts : {}
}

/**
 * The package manager a JS repo declares via its lockfile (npm is the safe default).
 *
 * `bun.lockb` is bun's binary lockfile and `bun.lock` its newer text one; either names bun. The
 * probe order is fixed rather than meaningful — a repo carrying two lockfiles is already ambiguous,
 * and a stable answer beats a clever one.
 */
function packageManager(dir) {
  if (existsSync(join(dir, 'pnpm-lock.yaml'))) return 'pnpm'
  if (existsSync(join(dir, 'yarn.lock'))) return 'yarn'
  if (existsSync(join(dir, 'bun.lockb')) || existsSync(join(dir, 'bun.lock'))) return 'bun'
  return 'npm'
}

/**
 * Detect a repo's commands from the files it declares, including Makefile affected tests.
 *
 * Pure and side-effect-free apart from reading the marker files in `cwd` itself: it never walks
 * the tree, never executes anything, and never mutates DEFAULT_CONFIG. Returns a PARTIAL config
 * — `{}` when nothing is declared, otherwise `{commands: {...}}` — so it composes through the
 * ordinary mergeConfig() layering with no special-casing at the call site.
 *
 * @param {{cwd?: string, keys?: string[]}} [opts] `cwd` is the directory to inspect (the repo root,
 *   not a nested dir); `keys` narrows detection to the command keys still wanted, and an empty
 *   intersection with the detectable set returns `{}` without reading a single marker file.
 * @returns {{commands?: Record<string,string>}}
 */
export function detectRepoDefaults({ cwd = process.cwd(), keys = DETECTED_COMMAND_KEYS } = {}) {
  const wanted = new Set(keys.filter((key) => DETECTED_COMMAND_KEYS.includes(key)))
  if (wanted.size === 0) return {} // nothing to learn: do no marker-file I/O at all
  const commands = {}
  const set = (key, value) => {
    if (!wanted.has(key)) return
    if (!(key in commands) && typeof value === 'string' && value.length > 0) commands[key] = value
  }

  // 1. Makefile — the repo's own front door.
  const targets = makefileTargets(cwd)
  if (targets.size > 0) {
    if (targets.has('check')) set('lint', 'make check')
    for (const key of DETECTED_COMMAND_KEYS) {
      if (key === 'testAffected') continue
      if (targets.has(key)) set(key, `make ${key}`)
    }
    if (targets.has('test-affected')) set('testAffected', 'make test-affected')
    if (targets.has('fmt')) set('format', 'make fmt')
    if (targets.has('fix')) set('format', 'make fix')
  }

  // 2. package.json scripts, run through whichever manager the lockfile names.
  const scripts = packageScripts(cwd)
  const scriptNames = Object.keys(scripts)
  if (scriptNames.length > 0) {
    const pm = packageManager(cwd)
    for (const key of DETECTED_COMMAND_KEYS) {
      if (key === 'testAffected') continue
      if (typeof scripts[key] === 'string' && scripts[key].length > 0) set(key, `${pm} run ${key}`)
    }
    if (typeof scripts.fmt === 'string' && scripts.fmt.length > 0) set('format', `${pm} run fmt`)
  }

  // 3. Cargo — the subcommands below are part of the toolchain a Cargo.toml declares.
  if (existsSync(join(cwd, 'Cargo.toml'))) {
    set('build', 'cargo build')
    set('lint', 'cargo clippy')
    set('format', 'cargo fmt')
    set('test', 'cargo test')
  }

  // 4. Go — same reasoning; no lint, because no linter ships with the toolchain.
  if (existsSync(join(cwd, 'go.mod'))) {
    set('build', 'go build ./...')
    set('format', 'go fmt ./...')
    set('test', 'go test ./...')
  }

  return Object.keys(commands).length > 0 ? { commands } : {}
}

export function loadSkillConfig({ cwd = process.cwd() } = {}) {
  const file = findConfigFile(cwd)
  let user = {}
  if (file) {
    let raw
    try {
      raw = readFileSync(file, 'utf8')
    } catch (e) {
      throw new Error(`skill-config: cannot read ${file}: ${e.message}`)
    }
    try {
      user = JSON.parse(raw)
    } catch (e) {
      throw new Error(`skill-config: ${file} is not valid JSON: ${e.message}`)
    }
    if (!user || typeof user !== 'object' || Array.isArray(user)) {
      // Valid JSON that is null, an array, or a primitive would merge as an
      // empty override and silently fall back to defaults — reject it loudly so
      // a broken consuming-repo config never masquerades as Bossanova defaults.
      throw new Error(`skill-config: ${file} must contain a JSON object`)
    }
  }
  // Detection fills only what nothing else declares, PER KEY: a config that declares just
  // `commands.testModule` still gets the detected commands, matching the documented
  // shallow-merge semantics rather than losing them to an all-or-nothing short-circuit. When
  // the config already declares every detectable key, no key is wanted and detectRepoDefaults()
  // returns without reading a marker file — a fully-configured repo still does no I/O at all.
  // Composition order is DEFAULT_CONFIG < detected < user: repo config beats detection, and
  // detection beats nothing. Detection is anchored at the config file's directory (the repo root)
  // when there is one, and at `cwd` otherwise — never at a nested dir under a discovered config.
  const declared =
    user.commands && typeof user.commands === 'object' && !Array.isArray(user.commands)
      ? user.commands
      : {}
  const detected = detectRepoDefaults({
    cwd: file ? dirname(file) : cwd,
    keys: DETECTED_COMMAND_KEYS.filter((key) => !(key in declared)),
  })
  const merged = withTrackerDefaults(mergeConfig(mergeConfig(DEFAULT_CONFIG, detected), user))
  validateConfig(merged, file || '(defaults)', { deferStageBlocks: true })
  return merged
}

// --- Accessors: review, commands, manifest, headless, adapters ------------

/**
 * A `review.skill` value as an ordered skill chain, or null when it names nothing usable. A string
 * is a one-skill chain; an array must be a non-empty list of skill names. Each name is one token
 * (`boss-review`, `plugin:skill`), since it is written one per line and dropped into a prompt.
 * @returns {string[]|null}
 */
export function reviewSkillChain(value) {
  const name = (v) => typeof v === 'string' && /^[A-Za-z0-9][A-Za-z0-9._:/-]*$/.test(v.trim())
  if (name(value)) return [value.trim()]
  if (Array.isArray(value) && value.length > 0 && value.every(name))
    return value.map((v) => v.trim())
  return null
}

/**
 * The review boss-build dispatches: the repo's replacement skill chain from `review.skill`, or
 * `['boss-review']` when none is configured. A replacement receives the same inputs and must emit
 * the same verdict JSON (review-verdict.mjs); boss-build consumes nothing else.
 * @returns {{skills: string[], override: boolean}}
 */
export function reviewSkillFor(config) {
  const chain = reviewSkillChain(config?.review?.skill)
  return chain ? { skills: chain, override: true } : { skills: ['boss-review'], override: false }
}

/** Whether boss-review runs its cross-agent second model. On unless `review.secondModel` is false. */
export function reviewSecondModel(config) {
  return config?.review?.secondModel !== false
}

/**
 * The review run's time settings, in the units callers compare against: `softBudgetSeconds`
 * gates optional review work after a round finishes, and `hangGuardMs` bounds one dispatch as a
 * hang guard. A positive-integer env value (`BOSS_REVIEW_SOFT_BUDGET_MINUTES`,
 * `BOSS_REVIEW_HANG_GUARD_MINUTES`) overrides the config, which overrides the default. Either way
 * may RAISE the value: these are not caps on review quality, so they are not lower-only.
 * Self-defending like the other accessors: anything unreadable resolves to the default.
 *
 * `dispatchLegMs` is the leg a review's cross-model helper runs inside: the configured hang guard
 * unless an explicit `BOSS_SKILL_EXTENSION_TIMEOUT_MS` (plain positive decimal digits) names
 * another; the helper's timeout is fitted strictly below it (`cross-review-lib.mjs
 * fitTimeoutToLeg`). `hangGuardMs` is the ONE bound the orchestrator waits each dispatch on, so it
 * is never below that leg: a longer explicit leg raises it, and a helper running its whole leg is
 * never recorded as timed out mid-run. A shorter leg bounds only the helper; every other dispatch
 * keeps the configured guard.
 * `heartbeatStaleMs` is DERIVED, never configured: the hang guard plus
 * REVIEW_HEARTBEAT_MARGIN_MS — the staleness window a caller holding the review on its heartbeat
 * (`bs-dispatch-await.mjs wait --stale`) must use so a dispatch that runs its full bound is not
 * read as an abandoned review.
 * @returns {{softBudgetSeconds: number, hangGuardMs: number, dispatchLegMs: number, heartbeatStaleMs: number}}
 */
export function reviewTimeDefaults(config, env = process.env) {
  const minutes = (envKey, configValue, fallback) => {
    const raw = env?.[envKey]
    if (typeof raw === 'string' && /^[0-9]+$/.test(raw.trim())) {
      const n = Number.parseInt(raw.trim(), 10)
      if (n >= 1) return n
    }
    return Number.isInteger(configValue) && configValue >= 1 ? configValue : fallback
  }
  const defaults = config?.reviewDefaults || {}
  const configuredGuardMs =
    minutes(
      'BOSS_REVIEW_HANG_GUARD_MINUTES',
      defaults.hangGuardMinutes,
      REVIEW_DEFAULT_HANG_GUARD_MINUTES,
    ) * 60_000
  const legRaw = env?.BOSS_SKILL_EXTENSION_TIMEOUT_MS
  const leg = legRaw != null && /^[0-9]+$/.test(String(legRaw)) ? Number(legRaw) : 0
  const dispatchLegMs = Number.isSafeInteger(leg) && leg > 0 ? leg : configuredGuardMs
  // The one bound the orchestrator waits a dispatch on: never below the leg a helper runs inside.
  const hangGuardMs = Math.max(configuredGuardMs, dispatchLegMs)
  return {
    softBudgetSeconds:
      minutes(
        'BOSS_REVIEW_SOFT_BUDGET_MINUTES',
        defaults.softBudgetMinutes,
        REVIEW_DEFAULT_SOFT_BUDGET_MINUTES,
      ) * 60,
    hangGuardMs,
    dispatchLegMs,
    heartbeatStaleMs: hangGuardMs + REVIEW_HEARTBEAT_MARGIN_MS,
  }
}

/**
 * Minutes a single epic child may run before its budget is spent — the default when this repo
 * configures none. Returning the default rather than throwing is what lets a config predating the
 * block resolve a usable clock; a caller must never have to reach into the raw block, because a
 * missing key there reads as `undefined` and every `elapsed > undefined` comparison is false, i.e.
 * a child that can never expire.
 * @returns {number} an integer in [1, EPIC_MAX_CHILD_WALL_CLOCK_MINUTES]
 */
export function epicChildWallClockMinutes(config) {
  const value = config?.epicDefaults?.childWallClockMinutes
  return Number.isInteger(value) && value >= 1 && value <= EPIC_MAX_CHILD_WALL_CLOCK_MINUTES
    ? value
    : EPIC_DEFAULT_CHILD_WALL_CLOCK_MINUTES
}

// notesSampleRate resolves the per-run reporting cadence for a core's post-terminal `notes`
// phase and, where a core also runs one, its pre-PR `knowledge` phase.
//
// Self-defending, like reviewMaxDispatchedRoundDefault: callers include hand-built configs and
// merged objects that never passed through validateConfig, so anything that is not a finite
// number in [0, 1] resolves to the documented default rather than reaching an `awk` roll as
// `undefined` — which would compare false and silently disable reporting forever.
export function notesSampleRate(config) {
  const value = config?.notesDefaults?.sampleRate
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 1
    ? value
    : NOTES_DEFAULT_SAMPLE_RATE
}

/**
 * The repo's declared dependency-scan tuning, as fresh COPIES with every field defaulted (an
 * unconfigured repo resolves to empty lists and `{}`), so a caller mutating the result never
 * reaches into the loaded config or the frozen defaults.
 * @returns {{moduleRoots: string[], repoWideTokens: string[], areaAliases: Record<string, string|string[]>}}
 */
export function planDependencyDefaults(config) {
  assertStageBlockReadable(config, 'planDependencies')
  const block = config?.planDependencies
  const list = (value) => (Array.isArray(value) ? [...value] : [])
  const aliases = block?.areaAliases
  const areaAliases = {}
  if (aliases && typeof aliases === 'object' && !Array.isArray(aliases)) {
    for (const [area, value] of Object.entries(aliases)) {
      areaAliases[area] = Array.isArray(value) ? [...value] : value
    }
  }
  return {
    moduleRoots: list(block?.moduleRoots),
    repoWideTokens: list(block?.repoWideTokens),
    areaAliases,
  }
}

/**
 * A configured/detected command string, or null when this repo declares none. `null` is the
 * honest "no command is known here" — a core that needs one falls back to its own discovery
 * prose (project instructions, CI config, command files) rather than running a wrong target.
 * @returns {string|null}
 */
export function command(config, key) {
  const value = config.commands?.[key]
  return typeof value === 'string' && value.length > 0 ? value : null
}

/** The fix-round gates in execution order, with unknown commands left for discovery. */
export function fixRoundGates(config) {
  const gates = []
  const missing = []
  for (const key of ['lint', 'testAffected']) {
    const resolved = command(config, key)
    if (resolved === null) missing.push(key)
    else gates.push({ key, command: resolved })
  }
  return { gates, missing }
}

/**
 * The per-module test command with `{module}` substituted, or null when the repo declares no
 * `commands.testModule`. `testModule` is a repo-shaped convention rather than a language fact,
 * so it is never detected — absence is the norm outside a repo that configures it.
 * @returns {string|null}
 */
export function moduleTestCommand(config, module) {
  const template = config.commands?.testModule
  return typeof template === 'string' && template.length > 0
    ? template.replace('{module}', module)
    : null
}

/**
 * The configured test-command manifest path, or null when this repo has none (the default —
 * a manifest is a project artifact, not a portable concept).
 * @returns {string|null}
 */
export function manifestPath(config) {
  const value = config.test?.manifestPath
  return typeof value === 'string' && value.length > 0 ? value : null
}

export function isHeadless(config, env = process.env, { isTTY = process.stdin.isTTY } = {}) {
  const signalHit = config.env.headlessSignals.some((s) => {
    if (s.present) return env[s.var] != null && env[s.var] !== ''
    return env[s.var] === s.equals
  })
  if (signalHit) return true
  return Boolean(config.env.headlessWhenNoTty) && !isTTY
}

export function adapterFor(config, kind) {
  if (!ADAPTER_KINDS.has(kind)) {
    throw new Error(`skill-config: unknown adapter kind "${kind}"`)
  }
  return config.adapters[kind]
}

/** Ordered repo-local skill roots to scan for boss-* extensions. */
export function extensionRootsFor(config) {
  return Array.isArray(config?.extensionRoots)
    ? config.extensionRoots
    : DEFAULT_CONFIG.extensionRoots
}

// --- Per-adapter identity resolution + configured-repo probe ---------------

/**
 * The concrete tracker identity block for the selected (or named) tracker adapter, or null when
 * the repo carries none. Defaults the adapter to the tracker selection in `adapters.tracker` so a
 * core can call `trackerConfigFor(config)` without re-deriving it.
 * @returns {{ mcpServer?: string, team?: string, repository?: string, teamKey?: string, workspace?: string, states?: Record<string,string> } | null}
 */
export function trackerConfigFor(config, adapter = adapterFor(config, 'tracker')) {
  const tc = config.trackerConfig?.[adapter]
  return tc && typeof tc === 'object' ? tc : null
}

/**
 * The description-normalization transform ids this repo tolerates its tracker performing on write.
 *
 * Returns a `Set`. An ABSENT `descriptionNormalization` block yields the FULL vocabulary; an
 * explicitly EMPTY `tolerated` array yields the empty set. The two are deliberately DIFFERENT:
 * absence is "no opinion, use the sane default", explicit-empty is "I want byte-exact".
 *
 * Absence used to yield the empty set too, which made the strictest possible comparison the
 * default for every repo that had never heard of this knob. That is the direction that looks safe
 * and measures badly — see the inverted-default note on the validator above. Tolerating a
 * meaning-preserving reshape is not the same as tolerating content loss, and the read-back detects
 * loss through conjuncts this set does not touch.
 *
 * Every member is in the closed vocabulary BY CONSTRUCTION: validation warns and drops an
 * unrecognised id, and the filter is repeated here so the guarantee holds even for a config that
 * never went through `validateConfig`.
 *
 * @returns {Set<string>}
 */
export function toleratedDescriptionTransforms(config, adapter = adapterFor(config, 'tracker')) {
  const block = trackerConfigFor(config, adapter)?.descriptionNormalization
  // Distinguish "no block / no key" from "declared, possibly empty". Only the former defaults.
  const declared = block && Array.isArray(block.tolerated) ? block.tolerated : null
  if (declared === null) return new Set(DESCRIPTION_NORMALIZATION_TRANSFORMS)
  return new Set(declared.filter((id) => DESCRIPTION_NORMALIZATION_TRANSFORM_SET.has(id)))
}

/**
 * How this repo wants an UNATTRIBUTED difference — one outside the tolerated transform set —
 * treated by the post-save read-back. Returns a member of `UNATTRIBUTED_DRIFT_SEVERITIES`.
 *
 * Defaults to `warn`, and falls back to `warn` for any unrecognised value, so a config this copy
 * cannot parse never starts failing completed runs. It governs ONLY that one conjunct: a lost
 * section, a dropped upload identity, or a changed verbatim block blocks regardless.
 *
 * @returns {'warn'|'block'}
 */
export function unattributedDriftSeverity(config, adapter = adapterFor(config, 'tracker')) {
  const declared = trackerConfigFor(config, adapter)?.descriptionNormalization?.onUnattributedDrift
  return UNATTRIBUTED_DRIFT_SEVERITIES.includes(declared)
    ? declared
    : DEFAULT_UNATTRIBUTED_DRIFT_SEVERITY
}

/**
 * The raw `trackerConfig.<adapter>.selection` block this repo declares, or `null` when it declares
 * none. A deferred validation fault throws; hand-built configs that never went through
 * `loadSkillConfig` remain tolerant. The adapter default is read
 * straight off `config` (not through `adapterFor`, which indexes `config.adapters` unguarded) for
 * the same reason. Callers build queries through `stageSelectionQuery`, which validates.
 * @returns {object|null}
 */
export function selectionBlockFor(config, adapter = config?.adapters?.tracker) {
  assertStageBlockReadable(config, `trackerConfig.${adapter}.selection`)
  // Checked before `trackerConfigFor` is reached: an explicitly `undefined` adapter re-triggers
  // that function's own `adapterFor` default and its TypeError.
  const tc =
    config && typeof config === 'object' && typeof adapter === 'string' && adapter.length > 0
      ? trackerConfigFor(config, adapter)
      : null
  const block = tc?.selection
  return block && typeof block === 'object' && !Array.isArray(block) ? block : null
}

/**
 * THE one derivation of a stage's candidate query, read by every gate and worker so a gate and its
 * worker cannot narrow differently: `{state, selection, requireLabels, excludeLabels}`.
 *
 *   - `state`: the `unplanned` state for `plan`, the `planned` state for `build`/`epic`, and `null`
 *     for the PR-oriented stages, which do not scan by state.
 *   - `selection`: `effectiveSelection(block, stage, flags)` — per-slot precedence flag > stage >
 *     shared. Unresolved (names, emails, `me`); the adapter resolves it.
 *   - `requireLabels`: the stage's core label roles (`agent-plan` / the build label), ANDed in.
 *   - `excludeLabels`: `needs-human`, always excluded by the core. A user's `labels.exclude`
 *     replaces only their own list and never re-admits it.
 *
 * Throws on an invalid block (fail closed: a malformed filter must not become no filter) and,
 * through `stateName` / `labelName`, on an unconfigured state or stage label.
 * @param {object} config
 * @param {string} stage one of SELECTION_STAGES
 * @param {object} [flags] parsed selection flags (`parseSelectionFlags(...).flags`)
 * @returns {{state: string|null, selection: object, requireLabels: string[], excludeLabels: string[]}}
 */
export function stageSelectionQuery(config, stage, flags = {}) {
  const roles = stageLabelRoles(stage)
  const block = selectionBlockFor(config)
  if (block !== null) {
    const errors = validateSelectionBlock(block, 'trackerConfig.selection')
    if (errors.length > 0) throw new Error(`skill-config: ${errors.join('; ')}`)
  }
  const stateRole =
    stage === 'plan' ? 'unplanned' : stage === 'build' || stage === 'epic' ? 'planned' : null
  const needsHuman = optionalLabelName(config, 'needsHuman')
  return {
    state: stateRole === null ? null : stateName(config, stateRole),
    selection: effectiveSelection(block, stage, flags),
    requireLabels: roles.map((role) => labelName(config, role)),
    excludeLabels: needsHuman ? [needsHuman] : [],
  }
}

function trackerRoleName(config, field, role, required = true) {
  const adapter = adapterFor(config, 'tracker')
  const roleMap = trackerConfigFor(config, adapter)?.[field]
  const hasRole =
    roleMap && typeof roleMap === 'object' && Object.prototype.hasOwnProperty.call(roleMap, role)
  if (!hasRole && !required) return null
  const value = hasRole ? roleMap[role] : undefined
  if (typeof value !== 'string' || value.length === 0) {
    throw new Error(
      `skill-config: trackerConfig.${adapter}.${field}.${role} must be configured as a non-empty string`,
    )
  }
  return value
}

/** Resolve a configured Linear workflow-state display name by its stable role. */
export function stateName(config, role) {
  return trackerRoleName(config, 'states', role)
}

/**
 * Resolve the configured workflow-state display names into the state-name -> role map
 * consumed by dependency planning.
 */
export function stateRolesFor(config) {
  const roles = ['unplanned', 'planned', 'inProgress', 'inReview']
  const out = {}
  for (const role of roles) out[stateName(config, role)] = role
  return out
}

/** Resolve a configured Linear issue-label display name by its stable role. */
export function labelName(config, role) {
  return trackerRoleName(config, 'labels', role)
}

/**
 * Resolve an optional Linear issue-label display name by its stable role.
 * Content-taxonomy labels are legitimately unmapped in some repos, so absence returns null.
 * @returns {string|null}
 */
export function optionalLabelName(config, role) {
  return trackerRoleName(config, 'labels', role, false)
}

// The content taxonomy a drafting dispatch may return in its bounded metadata `labels`. Pipeline
// labels (agent-build, needs-human, agent-question, …) are orchestrator-owned and never
// returned. The repo's skill-symbol lint keeps a CONTENT_LABELS denylist that must stay a subset of
// this runtime list (it excludes `bug`, a configured role, by design).
export const CONTENT_LABEL_ROLES = Object.freeze(['bug', 'feature', 'improvement', 'docs'])

/**
 * Resolve the content taxonomy to display names: each role through `optionalLabelName`, the role
 * literal when the repo leaves it unmapped.
 * @returns {string[]}
 */
export function contentLabelNames(config) {
  return CONTENT_LABEL_ROLES.map((role) => optionalLabelName(config, role) ?? role)
}

const labelKey = (value) =>
  String(value)
    .toLowerCase()
    .replace(/[\s_-]+/g, '')

/**
 * Map a label a drafter returned onto the configured content-taxonomy display name, matching the
 * role key or the display name case-, space-, `-`- and `_`-insensitively (`feature`, `Feature`,
 * `FEATURE` all resolve). Returns null for a label outside the taxonomy.
 * @returns {string|null}
 */
export function canonicalContentLabel(config, label) {
  if (typeof label !== 'string' || label.trim() === '') return null
  const key = labelKey(label)
  for (const role of CONTENT_LABEL_ROLES) {
    const name = optionalLabelName(config, role) ?? role
    if (labelKey(role) === key || labelKey(name) === key) return name
  }
  return null
}

/** Resolve a configured GitHub PR-label display name by its stable role. */
export function githubLabelName(config, role) {
  return trackerRoleName(config, 'githubLabels', role)
}

// --- Skill prose tracker-config claim scanner ------------------------------

const CLAIM_IGNORE_MARKER = '<!-- skill-config-claim: ignore -->'
const ROLE_TOKEN = '[A-Za-z][A-Za-z0-9_-]*'
const ROLE_NAME = '(?:`|\\*\\*)?(?<role>' + ROLE_TOKEN + ')(?:`|\\*\\*)?'
const ROLE_PROSE_PATTERNS = [
  {
    pattern: new RegExp(
      `\\b(?:the\\s+)?${ROLE_NAME}\\s+role\\s+(?:(?:is|was)\\s+)?(?:deliberately\\s+unmapped|unmapped|unavailable|not\\s+mapped)\\b`,
      'gi',
    ),
    field: null,
  },
  {
    pattern: new RegExp(
      `\\b(?:the\\s+)?${ROLE_NAME}\\s+label\\s+role\\s+(?:(?:is|was)\\s+)?(?:deliberately\\s+unmapped|unmapped|unavailable|not\\s+mapped)\\b`,
      'gi',
    ),
    field: 'labels',
  },
  {
    pattern: new RegExp(
      `\\b(?:the\\s+)?${ROLE_NAME}\\s+state\\s+role\\s+(?:(?:is|was)\\s+)?(?:deliberately\\s+unmapped|unmapped|unavailable|not\\s+mapped)\\b`,
      'gi',
    ),
    field: 'states',
  },
  {
    pattern: new RegExp(
      `\\blabelName\\(\\s*config\\s*,\\s*['"]${ROLE_NAME}['"]\\s*\\)` + '`?\\s+throws\\b',
      'gi',
    ),
    field: 'labels',
  },
  {
    pattern: new RegExp(
      `\\bstateName\\(\\s*config\\s*,\\s*['"]${ROLE_NAME}['"]\\s*\\)` + '`?\\s+throws\\b',
      'gi',
    ),
    field: 'states',
  },
]

function lineNumberAt(text, index) {
  let line = 1
  for (let i = 0; i < index; i += 1) {
    if (text.charCodeAt(i) === 10) line += 1
  }
  return line
}

function claimQuote(text, start, end) {
  const scope = claimScope(text, start, end)
  return scope.replace(/\s+/g, ' ').trim()
}

function claimScope(text, start, end) {
  const lineStart = text.lastIndexOf('\n', start - 1) + 1
  const lineEndIndex = text.indexOf('\n', start)
  const lineEnd = lineEndIndex === -1 ? text.length : lineEndIndex
  const line = text.slice(lineStart, lineEnd)
  if (/^\s*(?:[-*+]|\d+[.)])\s+/.test(line)) {
    let to = lineEnd
    while (to < text.length) {
      const nextLineStart = to + 1
      const nextLineEndIndex = text.indexOf('\n', nextLineStart)
      const nextLineEnd = nextLineEndIndex === -1 ? text.length : nextLineEndIndex
      const nextLine = text.slice(nextLineStart, nextLineEnd)
      if (/^\s*(?:[-*+]|\d+[.)])\s+/.test(nextLine)) break
      to = nextLineEnd
    }
    return text.slice(lineStart, to)
  }
  const before = text.lastIndexOf('\n\n', start)
  const after = text.indexOf('\n\n', end)
  const from = before === -1 ? 0 : before + 2
  const to = after === -1 ? text.length : after
  return text.slice(from, to)
}

/**
 * Find bounded prose claims that a tracker-config role is unmapped/unavailable/throwing.
 *
 * This is intentionally not full NLU. It catches the phrasing families that have caused real
 * drift, including line-wrapped claims, and returns enough location data for a tree-wide test to
 * compare each named role against the committed tracker config. A rare legitimate claim can opt out
 * by placing `<!-- skill-config-claim: ignore -->` in the same paragraph or list item.
 * @returns {{ role: string, line: number, quote: string, field?: string }[]}
 */
export function scanUnmappedRoleClaims(body) {
  const text = String(body ?? '')
  const claims = []
  for (const { pattern, field } of ROLE_PROSE_PATTERNS) {
    pattern.lastIndex = 0
    for (const match of text.matchAll(pattern)) {
      if (field === null && /^(?:label|state)$/i.test(match.groups.role)) continue
      const matchEnd = match.index + match[0].length
      if (claimScope(text, match.index, matchEnd).includes(CLAIM_IGNORE_MARKER)) continue
      claims.push({
        role: match.groups.role,
        line: lineNumberAt(text, match.index),
        quote: claimQuote(text, match.index, matchEnd),
        field,
        index: match.index,
      })
    }
  }
  return claims
    .sort((a, b) => a.index - b.index)
    .map(({ role, line, quote, field }) =>
      field === null ? { role, line, quote } : { role, line, quote, field },
    )
}

/**
 * The concrete publish-store identity block for the selected (or named) publish adapter, or null.
 * @returns {{ bucket: string, baseUrl: string } | null}
 */
export function publishConfigFor(config, adapter = adapterFor(config, 'publish')) {
  const pc = config.publishConfig?.[adapter]
  return pc && typeof pc === 'object' ? pc : null
}

/** Resolve the implementation-plan store without changing proof publishing. */
export function planStorageFor(config) {
  return { kind: 'tracker-attachment' }
}

function trackerReadinessFacts(config) {
  const tc = trackerConfigFor(config)
  const nonEmpty = (value) => typeof value === 'string' && value.length > 0
  return {
    hasServer: nonEmpty(
      adapterFor(config, 'tracker') === 'github' ? tc?.repository : tc?.mcpServer,
    ),
    hasTeam: nonEmpty(adapterFor(config, 'tracker') === 'github' ? tc?.repository : tc?.team),
    hasStates: ['unplanned', 'planned', 'inProgress', 'inReview'].every((role) =>
      nonEmpty(tc?.states?.[role]),
    ),
  }
}

/** Classify tracker readiness before resolving this run's team. */
export function trackerReadiness(config, { planning = false } = {}) {
  const { hasServer, hasTeam, hasStates } = trackerReadinessFacts(config)
  if (!hasServer || (planning && !hasStates)) return 'unconfigured'
  return hasTeam ? 'ready' : 'needs-team'
}

/**
 * Is this checkout wired to a concrete tracker? True iff the selected tracker adapter has an
 * identity block with a non-empty mcpServer AND a team: the configured `team`, or else the
 * non-empty `team` the caller resolved for this run (resolveTrackerTeam — --team or the single
 * visible team). This is the preflight self-disable probe. Called with no second argument it keeps
 * its strict meaning — a configured team is required — so a caller that has not resolved a team
 * stays fail-closed; a zero-config Linear repo carries a synthesized team-less block and is
 * configured only once a run passes the team it resolved. Unconfigured means the core stops with
 * one clear line and makes no tracker write.
 */
export function isConfiguredForRepo(config, options = {}) {
  const { hasServer, hasTeam } = trackerReadinessFacts(config)
  const team = options?.team
  return hasServer && (hasTeam || (typeof team === 'string' && team.trim().length > 0))
}

/**
 * Stricter planning-readiness probe for boss-plan's preflight. isConfiguredForRepo() only proves the
 * tracker identity (mcpServer + team); boss-plan additionally resolves state by role — the unplanned
 * and planned states for selection and write-back, plus inProgress/inReview for the active-backlog
 * reads — from trackerConfigFor(config).states. validateConfig() leaves the states map optional (other
 * cores need only the identity), so a repo configured for those cores but missing boss-plan's state
 * roles would pass isConfiguredForRepo() and then read/write with undefined state names. Require the
 * full role map here so boss-plan self-disables cleanly in such a repo instead of failing mid-run
 * after drafting work.
 */
export function isConfiguredForPlanning(config, options = {}) {
  const { hasServer, hasTeam, hasStates } = trackerReadinessFacts(config)
  const team = options?.team
  return hasServer && hasStates && (hasTeam || (typeof team === 'string' && team.trim().length > 0))
}

// --- Per-run tracker team resolution (zero-config, D11) ---------------------

// How many team names an ambiguous / unknown-team message lists before eliding the rest.
const TEAM_NAMES_SHOWN = 10

/**
 * Normalize a tracker's team listing into `{teams: [{id, name, key}], hasNextPage}`, or null when
 * it is not a listing at all. Accepts the raw MCP `list_teams` result (`{teams: [...],
 * hasNextPage}`) or a bare array of `{id, name, key?}`. Anything malformed — a non-object, a
 * non-array `teams`, an entry without a non-empty `name` — is null ("not listed"), never a throw.
 */
function normalizeVisibleTeams(visibleTeams) {
  let entries
  let hasNextPage = false
  if (Array.isArray(visibleTeams)) entries = visibleTeams
  else if (visibleTeams && typeof visibleTeams === 'object' && Array.isArray(visibleTeams.teams)) {
    entries = visibleTeams.teams
    hasNextPage = visibleTeams.hasNextPage === true
  } else return null
  const text = (value) => (typeof value === 'string' && value.trim() !== '' ? value.trim() : null)
  const teams = []
  for (const entry of entries) {
    if (!entry || typeof entry !== 'object' || text(entry.name) === null) return null
    teams.push({ id: text(entry.id), name: text(entry.name), key: text(entry.key) })
  }
  return { teams, hasNextPage }
}

function oneLineMessage(value) {
  return String(value ?? '')
    .replace(/\s*\n\s*/g, ' ')
    .trim()
}

/**
 * Resolve this run's tracker team from facts the caller gathered — pure and synchronous, like
 * trackerMcpPreflight. Precedence: a configured `trackerConfig.<tracker>.team` > `teamFlag`
 * (--team) > the single visible team. Feed the result's `team` to
 * `isConfiguredForRepo(config, {team})` / `isConfiguredForPlanning(config, {team})`.
 *
 * @param {object} config a loaded skill config
 * @param {object} [facts]
 * @param {{ok?: boolean, message?: string, mcpServer?: string}} [facts.preflight] a
 *   trackerMcpPreflight result
 * @param {unknown} [facts.visibleTeams] the raw MCP list_teams result or a bare `{id, name, key?}`
 *   array; null/absent/malformed means "not listed"
 * @param {string} [facts.teamFlag] the --team argument; blank counts as absent
 * @returns {{configured: boolean, team: string|null, teamKey: string|null,
 *   source: 'config'|'flag'|'detected'|null, reason: string|null, message: string}}
 *   `reason` is null when configured, else one of `no-tracker-mcp`, `unknown-team`,
 *   `ambiguous`, `no-teams`, `teams-unlisted`. `message` is one line: empty or a warning when
 *   configured, and always non-empty when unconfigured.
 */
export function resolveTrackerTeam(config, { preflight, visibleTeams, teamFlag } = {}) {
  const adapter = typeof config?.adapters?.tracker === 'string' ? config.adapters.tracker : 'linear'
  const label = adapter === 'linear' ? 'Linear' : adapter
  const teamPath = `trackerConfig.${adapter}.team`
  let tc = null
  try {
    tc = trackerConfigFor(config)
  } catch {
    tc = null
  }
  const flag = typeof teamFlag === 'string' && teamFlag.trim() !== '' ? teamFlag.trim() : null
  const configured = (team, teamKey, source, message = '') => ({
    configured: true,
    team,
    teamKey: teamKey ?? null,
    source,
    reason: null,
    message,
  })
  const unconfigured = (reason, message) => ({
    configured: false,
    team: null,
    teamKey: null,
    source: null,
    reason,
    message: oneLineMessage(message),
  })
  const same = (a, b) => typeof b === 'string' && a.toLowerCase() === b.toLowerCase()

  // 1. An explicit configured team always wins — even over a failed preflight, which the caller
  //    still treats as the hard stop it always was.
  if (typeof tc?.team === 'string' && tc.team.trim() !== '') {
    const teamKey = typeof tc.teamKey === 'string' && tc.teamKey !== '' ? tc.teamKey : null
    const warning =
      flag !== null && !same(flag, tc.team) && !same(flag, teamKey)
        ? `--team ${flag} ignored: ${teamPath} is ${tc.team}`
        : ''
    return configured(tc.team, teamKey, 'config', warning)
  }

  // 2. No reachable tracker MCP server and no configured team: nothing can be resolved.
  if (preflight?.ok !== true) {
    const server =
      (typeof preflight?.mcpServer === 'string' && preflight.mcpServer) ||
      (typeof tc?.mcpServer === 'string' && tc.mcpServer) ||
      adapter
    const own = oneLineMessage(preflight?.message)
    return unconfigured(
      'no-tracker-mcp',
      own !== ''
        ? own
        : `no ${label} MCP server "${server}" is reachable in this session — declare it to the harness, or set ${teamPath} in .boss-skills.json`,
    )
  }

  const listing = normalizeVisibleTeams(visibleTeams)
  const names = (teams, more) => {
    const shown = teams.slice(0, TEAM_NAMES_SHOWN).map((team) => team.name)
    return shown.join(', ') + (more || teams.length > TEAM_NAMES_SHOWN ? ', …' : '')
  }

  if (flag !== null) {
    // 3. The operator named a team and nothing can check it: trust the explicit choice.
    if (listing === null) return configured(flag, null, 'flag')
    // 4. Match case-insensitively on name or key, exactly on id; answer with the canonical name.
    const match = listing.teams.find(
      (team) => same(flag, team.name) || same(flag, team.key) || flag === team.id,
    )
    if (match) return configured(match.name, match.key, 'flag')
    // More teams exist than one page shows, so absence proves nothing: trust the explicit choice.
    if (listing.hasNextPage) return configured(flag, null, 'flag')
    // 5. A named team that is not visible is a typo or a missing grant, never a guess.
    return unconfigured(
      'unknown-team',
      `--team ${flag} matches no visible ${label} team (visible: ${
        listing.teams.length > 0 ? names(listing.teams, listing.hasNextPage) : 'none'
      }) — pass a visible team name or set ${teamPath} in .boss-skills.json`,
    )
  }

  if (listing !== null) {
    const count = listing.teams.length
    // 6. More than one team (or more than one page) and nothing picks one.
    if (listing.hasNextPage || count >= 2) {
      return unconfigured(
        'ambiguous',
        `${listing.hasNextPage ? `more than ${count}` : count} ${label} teams are visible (${names(
          listing.teams,
          listing.hasNextPage,
        )}) — pass --team <name> or set ${teamPath} in .boss-skills.json`,
      )
    }
    // 7. Exactly one visible team: the zero-config happy path.
    if (count === 1) return configured(listing.teams[0].name, listing.teams[0].key, 'detected')
    // 8. Nothing visible at all.
    return unconfigured(
      'no-teams',
      `no ${label} teams are visible to the tracker MCP server — check its credentials, or set ${teamPath} in .boss-skills.json`,
    )
  }

  // 9. No listing and no flag.
  return unconfigured(
    'teams-unlisted',
    `the visible ${label} teams could not be listed — pass --team <name> or set ${teamPath} in .boss-skills.json`,
  )
}

// --- Plan-description contract ---------------------------------------------

/** The integer version of the plan-description section contract (default 1). */
export function planContractVersion(config) {
  return config.planContract.version
}

/** The full ordered `{ heading, required }` child-plan section list, as emitted. */
// Mode-blind by design: config authors and existing callers use this as the child-plan contract.
export function planSections(config) {
  return config.planContract.sections
}

/** Headings whose `required === 'always'`, in order — the child-plan sections every plan MUST carry. */
// Mode-blind by design: epic-parent callers use requiredSectionsForDescriptionMode explicitly.
export function requiredPlanSections(config) {
  return config.planContract.sections.filter((s) => s.required === 'always').map((s) => s.heading)
}

const EPIC_PARENT_PLAN_SECTIONS = Object.freeze([
  Object.freeze({ heading: '## Summary', required: 'always' }),
  Object.freeze({ heading: '## Child tickets', required: 'always' }),
  Object.freeze({ heading: '## Planning', required: 'optional' }),
  Object.freeze({ heading: '## Original notes', required: 'always' }),
])

function normalisePlanDescriptionMode(mode) {
  const selected = mode || 'child-plan'
  if (selected === 'child-plan') return selected
  if (selected === 'epic-parent') return selected
  console.warn(
    `skill-config: validatePlanDescription unknown mode "${selected}" — falling back to child-plan`,
  )
  return 'child-plan'
}

export function planSectionsForDescriptionMode(config, mode) {
  return normalisePlanDescriptionMode(mode) === 'epic-parent'
    ? EPIC_PARENT_PLAN_SECTIONS
    : planSections(config)
}

export function requiredSectionsForDescriptionMode(config, mode) {
  return planSectionsForDescriptionMode(config, mode)
    .filter((s) => s.required === 'always')
    .map((s) => s.heading)
}

/**
 * One pass over `text`, returning the lines OUTSIDE fenced code blocks (`{ line, index }`, 0-based)
 * and whether a fence was still open at EOF. Both facts come from the same scan so they can never
 * disagree.
 *
 * Fence tracking is intentionally simple: an opening ``` / ~~~ run of 3+ is closed by a run of the
 * same character that is at least as long. It only has to be good enough to tell markdown STRUCTURE
 * from quoted sample text — a plan legitimately fences command output and markdown examples.
 *
 * ANY leading indent opens a fence, deliberately wider than CommonMark's 3-space limit for a
 * top-level fence. A fence nested in a list item is indented by the marker width, so the strict
 * limit missed exactly the shape a plan uses most — quoted output under a bullet — and let its
 * sample lines read as document structure.
 *
 * The widening is BIDIRECTIONAL, not one-way: the same match drives the close branch, so an
 * indented run can also TERMINATE an enclosing fence early and expose its content. Measured
 * residual, accepted rather than fixed: a 3-backtick fence nested inside a 3-backtick outer fence
 * closes the outer one, so the nested sample lines become visible again. Incidence is 0 of the
 * 1212 tracked markdown files in this repo, and the CommonMark-idiomatic nesting — a LONGER outer
 * fence — is unaffected, because the close branch still requires `run[1].length >= fence.length`.
 * The airtight fix is to record the opening indent on `fence` and require the closing run's indent
 * to be no greater than it plus 3; do that if the shape is ever observed in practice.
 *
 * It lives here, beside the splitter that needs it, because `plan-contract-guard.mjs` imports this
 * module: putting it there instead would invert the dependency, and a second copy would drift.
 */
export function scanFences(text) {
  const lines = []
  let fence = null
  const source = String(text ?? '').split('\n')
  for (let index = 0; index < source.length; index += 1) {
    const bare = source[index].replace(/\r$/, '')
    const run = /^[ \t]*(`{3,}|~{3,})(.*)$/.exec(bare)
    if (fence === null) {
      if (run && !(run[1][0] === '`' && run[2].includes('`'))) {
        fence = { character: run[1][0], length: run[1].length }
        continue
      }
      lines.push({ line: bare, index })
      continue
    }
    if (run && run[1][0] === fence.character && run[1].length >= fence.length && !run[2].trim()) {
      fence = null
    }
  }
  return { lines, unterminated: fence !== null }
}

export function markdownH2Heading(line) {
  const match = /^ {0,3}(##\s.*)$/.exec(String(line ?? '').replace(/\r$/, ''))
  return match ? match[1].trimEnd() : null
}

/**
 * Split a plan description into its emitted top-level `##` sections, in order.
 *
 * The final contract section (`## Original notes`) echoes the ticket's original description
 * verbatim, which can itself contain `##` headings and stray `- Contract:` lines. To keep that
 * preserved body from masquerading as emitted plan structure, splitting stops at the terminal
 * section's heading: everything after it is that section's body, not a new section.
 *
 * A `##` line inside a FENCED CODE BLOCK is likewise not a section: plans routinely quote command
 * output and markdown examples that begin with `##` (a `make help` listing, a `git status` line),
 * and reading one as emitted structure invents a section the drafter never wrote. That was merely
 * cosmetic while only `missing` was consumed; once a producer-side gate rejects off-contract
 * headings it becomes a deterministic hard abort with no remedy the drafter can act on. Skipping
 * them can only REMOVE spurious sections, so `missing`/`ok` are unchanged except where a required
 * heading appears solely inside a fence — where the stricter reading is the correct one.
 *
 * Exported so producer-side gates (section ordering, placeholder scoping) share this one terminal-
 * section rule instead of re-deriving it — a duplicate splitter would drift from the validator.
 * @returns {{ heading: string, bodyLines: string[] }[]}
 */
const headingKey = (heading) =>
  String(heading ?? '')
    .replace(/[\s:]+$/, '')
    .replace(/\s+/g, ' ')
    .toLowerCase()

function splitPlanDescriptionSections(contractSections, description) {
  const terminalHeading = contractSections[contractSections.length - 1]?.heading
  // A heading that differs from a contract heading only in case, spacing or a trailing colon IS that
  // section (`## Acceptance Criteria` is `## Acceptance criteria`); it is reported under the
  // contract spelling so every lookup by heading finds it.
  const canonical = new Map(contractSections.map((s) => [headingKey(s.heading), s.heading]))
  const outside = new Set(scanFences(description).lines.map(({ index }) => index))
  const sections = []
  let current = null
  let inTerminal = false
  const lines = String(description ?? '').split('\n')
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]
    const raw = markdownH2Heading(line)
    const heading = raw ? (canonical.get(headingKey(raw)) ?? raw) : raw
    if (!inTerminal && outside.has(index) && heading) {
      current = { heading, bodyLines: [] }
      sections.push(current)
      if (heading === terminalHeading) inTerminal = true
      continue
    }
    if (current) current.bodyLines.push(line)
  }
  return sections
}

export function planDescriptionSections(config, description, { mode = 'child-plan' } = {}) {
  const resolvedMode = normalisePlanDescriptionMode(mode)
  return splitPlanDescriptionSections(
    planSectionsForDescriptionMode(config, resolvedMode),
    description,
  )
}

function normaliseSectionHeading(heading) {
  return String(heading ?? '')
    .replace(/^#+\s*/, '')
    .trim()
    .toLowerCase()
}

export function keyChangesHeading(config, override) {
  assertConfigFirst(config, 'keyChangesHeading')
  const explicit = typeof override === 'string' ? override.trim() : ''
  if (explicit !== '') return explicit
  return (
    planSections(config).find(
      (section) => normaliseSectionHeading(section?.heading) === 'key changes',
    )?.heading ?? null
  )
}

export function keyChangesSection(config, description) {
  assertConfigFirst(config, 'keyChangesSection')
  const heading = keyChangesHeading(config)
  const section = planDescriptionSections(config, description).find(
    (entry) =>
      heading !== null &&
      normaliseSectionHeading(entry.heading) === normaliseSectionHeading(heading),
  )
  return section ? section.bodyLines.join('\n') : null
}

export function descriptionAppearsTruncated(description) {
  return (
    typeof description === 'string' &&
    description.includes('(truncated, use get_issue for full description)')
  )
}

/**
 * Validate a plan description against the contract.
 *
 * The argument order is **config first**: `validatePlanDescription(config, description)`. The
 * natural-reading `(description, config)` call is detected and rejected with a named error rather
 * than allowed to surface as a `Cannot read properties of undefined (reading 'sections')` TypeError
 * from deep inside `planSections()`.
 *
 * Parses the `- Contract: v<N>` stamp from the body of the `## Planning` section only — either
 * bullet marker is accepted, because a tracker may renormalise `-` to `*` on save, which would
 * otherwise make the stamp undetectable on read-back. A missing stamp is treated as back-compat v1
 * (`version: null`); a stamped version newer than this contract is flagged as `unsupportedVersion`.
 *
 * `missing` lists any required heading not emitted as a top-level `##` section. `unknown` lists
 * every emitted top-level `##` heading that is not in `planSections(config)` at all — including
 * sections classed `optional`, which are recognised and therefore never unknown. Headings and
 * stamps echoed inside the verbatim `## Original notes` body do not participate: splitting stops at
 * that terminal section.
 *
 * `ok` is true iff nothing is missing and the version is supported. `unknown` is deliberately
 * **additive** and does NOT flip `ok`: consumers (boss-build) gate on `ok`, and folding `unknown`
 * into it would newly block already-planned tickets. Producer-side strictness belongs in the
 * producer's own guard, which reads `unknown` directly.
 * The optional third argument selects the document shape. Default `child-plan` is the existing
 * implementation-plan contract. `epic-parent` validates the parent overview shape emitted when an
 * oversized ticket becomes an epic parent. Unknown modes warn and fall back to `child-plan`, because
 * installed toolbox copies can lag behind plan prose.
 *
 * @returns {{ ok: boolean, version: number | null, missing: string[], unknown: string[], unsupportedVersion: boolean }}
 */
export function validatePlanDescription(config, description, { mode = 'child-plan' } = {}) {
  // A config is an object carrying a planContract object; a description is a string. That makes the
  // swapped call unambiguously detectable, so diagnose it here instead of failing obscurely below.
  // DELEGATED, not duplicated: this used to be an inline copy of `assertConfigFirst`, which meant
  // it did not inherit fixes to the shared helper — the empty-config split among them.
  assertConfigFirst(config, 'validatePlanDescription')
  const resolvedMode = normalisePlanDescriptionMode(mode)
  const contractSections = planSectionsForDescriptionMode(config, resolvedMode)
  const sections = splitPlanDescriptionSections(contractSections, description)
  const present = new Set(sections.map((s) => s.heading))
  const missing = requiredSectionsForDescriptionMode(config, resolvedMode).filter(
    (heading) => !present.has(heading),
  )
  const recognised = new Set(contractSections.map((s) => s.heading))
  const unknown = [...new Set(sections.map((s) => s.heading))].filter((h) => !recognised.has(h))

  const planning = sections.find((s) => s.heading === '## Planning')
  let version = null
  if (planning) {
    for (const bodyLine of planning.bodyLines) {
      const stamp = /^[-*]\s*Contract:\s*v(\d+)\s*$/.exec(bodyLine.trim())
      if (stamp) {
        version = Number(stamp[1])
        break
      }
    }
  }
  const unsupportedVersion = version !== null && version > planContractVersion(config)
  return {
    ok: missing.length === 0 && !unsupportedVersion,
    version,
    missing,
    unknown,
    unsupportedVersion,
  }
}

// A `- Key:` / `* Key:` list item GLUED to the preceding text: a marker directly after
// non-whitespace, one space, and a capitalised key of up to four words ending in a colon — the
// shape of every `## Planning` bullet (`Contract:`, `Agent-friendly:`, `Premise drift:`,
// `Plan attachment:`). A hyphen inside prose (`self-falsified`, `step-4`) is followed by no space,
// a spaced dash in prose (`backend - Frontend: later`) is preceded by whitespace, and an em dash
// is not a list marker, so none of them matches.
const PLANNING_LIST_ITEM = /(?<=\S)[-*] ([A-Z][A-Za-z0-9]*(?:[- ][A-Za-z0-9]+){0,3}):(?= |$)/g
// The keys boss-plan writes under `## Planning` — the drafted template's bullets plus the ones the
// orchestrator inserts after the contract gate. A merge that joined two bullets with a SPACE
// (`- Contract: v1 - Agent-friendly: needs-human`) is told apart from spaced prose
// (`backend - Frontend: later`) only by the key, so the space-joined form is flagged for these
// keys alone.
const PLANNING_BULLET_KEYS = Object.freeze([
  'Contract',
  'Agent-friendly',
  'Atomic-5',
  'Oversized-child',
  'Plan attachment',
  'On implementation',
  'Dependencies',
  'Transitive-block warning',
  'Premise drift',
  'Triage',
])
const SPACED_PLANNING_LIST_ITEM = new RegExp(
  `(?<=\\s)[-*] (${PLANNING_BULLET_KEYS.join('|')}):(?= |$)`,
  'g',
)
const PLANNING_HEADING = '## Planning'

/**
 * Every `## Planning` line on which a SECOND `- Key:` / `* Key:` item begins glued to a preceding
 * non-whitespace character, or after a space when the key is a known Planning bullet key — the shape an orchestrator-inserted bullet takes when it is joined to
 * its neighbour without a newline (`- Atomic-5: epic DAG.- Agent-friendly: needs-human`). The
 * contract gate and the writeback verifier both call this, so the two cannot drift: the contract
 * gate sees the drafted description, the verifier sees the intended bytes after the run's final
 * save — the only point that sees the Planning bullets the orchestrator inserts after the contract
 * gate ran.
 *
 * Scoped to `## Planning` (the only section anything inserts into), outside fenced code, with
 * inline code spans masked, and stopping at the terminal verbatim section. A leading blockquote
 * prefix (`> `, `> > `) is part of the line's first item, not a separator before a second one.
 *
 * @returns {{ line: number, text: string }[]} 1-based description line and the merged item's key.
 */
export function mergedListItems(config, description, { mode = 'child-plan' } = {}) {
  assertConfigFirst(config, 'mergedListItems')
  const contractSections = planSectionsForDescriptionMode(
    config,
    normalisePlanDescriptionMode(mode),
  )
  const terminalHeading = contractSections[contractSections.length - 1]?.heading
  const lines = String(description ?? '').split('\n')
  const outside = new Set(scanFences(description).lines.map(({ index }) => index))
  const hits = []
  let inPlanning = false
  for (let index = 0; index < lines.length; index += 1) {
    if (!outside.has(index)) continue
    const line = lines[index].replace(/\r$/, '')
    const heading = markdownH2Heading(line)
    if (heading) {
      if (heading === terminalHeading) break
      inPlanning = heading === PLANNING_HEADING
      continue
    }
    if (!inPlanning) continue
    const masked = line.replace(/`+[^`\n]*`+/g, (span) => '!'.repeat(span.length))
    const first = masked.match(/^[\s>]*/)[0].length
    const matches = [
      ...masked.matchAll(PLANNING_LIST_ITEM),
      ...[...masked.matchAll(SPACED_PLANNING_LIST_ITEM)].filter((match) => match.index > first),
    ].sort((a, b) => a.index - b.index)
    for (const match of matches) {
      hits.push({ line: index + 1, text: match[0] })
    }
  }
  return hits
}

// --- Verify-only acceptance criteria ---------------------------------------
//
// A criterion whose correct outcome is "this file needed no change" produces no diff, so every gate
// that reasons over changed paths is blind to it. Marking such a criterion with a LITERAL token
// makes classification exact — a bullet either carries the marker or it does not, so there are no
// heuristics and no false positives — and lets the consumer require named, re-runnable evidence
// instead of a silent tick. The four literals below are exported so producer prose, consumer prose
// and this parser all read one definition; a hand-typed copy in three places is how a contract
// drifts, and a sync test asserts the prose carries these exact bytes.

/** Literal prefix that classifies an acceptance-criterion bullet as verify-only. */
export const VERIFY_ONLY_MARKER = '(verify-only)'

/** Plan-time clause naming the command a reviewer can re-run. */
export const VERIFY_ONLY_CHECK = ' — check: '

/** Discharge-time clause naming the command the run actually executed. */
export const VERIFY_ONLY_CHECKED = ' — checked: '

/** Separator between a discharged command and the result it produced. */
export const VERIFY_ONLY_RESULT = ' → '

/** The `## Acceptance criteria` contract heading these helpers parse. */
const ACCEPTANCE_HEADING = '## Acceptance criteria'

/** The optional `## Premises` contract heading. */
const PREMISES_HEADING = '## Premises'
const CENTRAL_PREMISE_MARKER_RE = /^(?:\*\*|__|\*|_)?\(central\)(?:\*\*|__|\*|_)?\s*/i
const SHELL_BUILTINS = new Set(['[', 'cd', 'echo', 'false', 'printf', 'pwd', 'set', 'test', 'true'])
const SHELL_COMMAND_SEPARATORS = new Set([';', '|', '&&', '||'])
const NODE_TEST_OPTIONS_WITH_VALUE = new Set([
  '--test-concurrency',
  '--test-coverage-branches',
  '--test-coverage-exclude',
  '--test-coverage-functions',
  '--test-coverage-include',
  '--test-coverage-lines',
  '--test-global-setup',
  '--experimental-test-isolation',
  '--test-isolation',
  '--test-name-pattern',
  '--test-reporter',
  '--test-reporter-destination',
  '--test-rerun-failures',
  '--test-shard',
  '--test-skip-pattern',
  '--test-timeout',
])

/** A markdown checkbox list item: `- [ ] text` / `- [x] text` (either box case, any list marker). */
const CRITERION_RE = /^[-*+]\s+\[([ xX])\]\s*(.*)$/

/** Any other list item — it ends the previous criterion rather than continuing it. */
const LIST_ITEM_RE = /^[-*+]\s|^\d+[.)]\s/

/**
 * The result separator, matched tolerantly on surrounding whitespace.
 *
 * `VERIFY_ONLY_RESULT` is the canonical written form and stays exactly ` → `, but it is the one
 * clause that can legitimately sit at END OF LINE — precisely when the result is EMPTY, which is
 * the mutation this gate exists to catch. Every markdown formatter strips trailing whitespace, so
 * a strict `indexOf(' → ')` would fail to find the separator in exactly that case, silently
 * reclassifying "empty result" as "no result clause at all". The two happen to route to the same
 * verdict today; relying on that coincidence would make the gate's reason wrong even when its
 * answer is right, and it would break the moment either branch is reported separately.
 */
const RESULT_SEPARATOR_RE = new RegExp(`\\s*${VERIFY_ONLY_RESULT.trim()}\\s*`)

/**
 * A config OBJECT — the shape `loadSkillConfig()` returns — as opposed to a description.
 *
 * This is the discriminator that separates the two config-first faults. It is shape-only on
 * purpose: it asks whether the value could be a config at all, never whether it is a complete one.
 */
function isConfigShaped(value) {
  return Boolean(value) && typeof value === 'object' && !Array.isArray(value)
}

/**
 * Every config-first export is **config-first**, exactly like `validatePlanDescription`. Diagnose
 * the natural-reading swapped call, and the correctly ordered contractless config, by name instead
 * of letting either surface as a TypeError from deep inside `planSections()`.
 *
 * **Every config-first export in this module shares this guard, and it reports THREE distinct
 * faults** — conflating any two of them sends the caller to the wrong fix.
 *
 *   1. An ABSENT config (`undefined`/`null`) is a config that was never loaded — overwhelmingly the
 *      `loadConfig`-for-`loadSkillConfig` typo, where the import resolves to `undefined` and the
 *      call site looks correct. The remedy is to load one, and NOTHING about argument order is
 *      wrong. This used to fall into fault 2 and be reported as "arguments look swapped", naming a
 *      bug the caller did not have; the doc comment claiming an absent value reached fault 3 was
 *      simply false, because `undefined` fails `isConfigShaped` first.
 *   2. A value that cannot be a config but COULD be a description (a string, above all) is a
 *      genuinely swapped call, and the remedy is to reorder the arguments.
 *   3. A value that IS config-shaped but carries no `planContract` is correctly ordered — the remedy
 *      is to load a real config, not to reorder anything. Reporting this as "arguments look swapped"
 *      was measured sending a fix toward argument order when the caller had passed `{}` in exactly
 *      the right position.
 *
 * All three messages stay module-prefixed and all three name `fn(config, description)`, so the call
 * site is identifiable from the message alone. `plan-deps-lib.mjs` carries its own copy of this
 * guard rather than importing it, and the two are kept in step BY HAND: a fourth fault class added
 * here must be added there too, and nothing fails if it is not.
 */
function assertConfigFirst(config, fn) {
  if (config === undefined || config === null) {
    throw new Error(
      `skill-config: ${fn}(config, description) — no config passed; the first argument is ${config === null ? 'null' : 'undefined'}, so nothing about the argument ORDER is wrong. Pass a config from loadSkillConfig() (note the name: there is no loadConfig export).`,
    )
  }
  if (!isConfigShaped(config)) {
    throw new Error(
      `skill-config: ${fn}(config, description) — arguments look swapped; pass the config first`,
    )
  }
  if (!config.planContract) {
    throw new Error(
      `skill-config: ${fn}(config, description) — no plan contract loaded; the first argument is config-shaped but carries no planContract. Pass a config from loadSkillConfig() (which merges ${CONFIG_FILENAME} over the built-in defaults), not an empty object.`,
    )
  }
}

/** Strip one wrapping pair of backticks, if present, then trim. */
function unwrapCode(value) {
  const trimmed = value.trim()
  const fenced = /^`+(.*?)`+$/s.exec(trimmed)
  return (fenced ? fenced[1] : trimmed).trim()
}

/** A string that carries actual content — "present" must never be confused with "non-empty". */
function nonEmpty(value) {
  return typeof value === 'string' && value.trim().length > 0
}

function pushShellToken(tokens, value, quoted) {
  if (value) tokens.push({ value, quoted })
}

function tokenizeSimpleShellDetailed(command) {
  const source = String(command ?? '')
  const tokens = []
  let current = ''
  let quoted = false
  let quote = null
  for (let index = 0; index < source.length; index += 1) {
    const char = source[index]
    if (quote) {
      if (char === quote) quote = null
      else current += char
      continue
    }
    if (char === '"' || char === "'" || char === '`') {
      quote = char
      quoted = true
    } else if (
      (char === '&' && source[index + 1] === '&') ||
      (char === '|' && source[index + 1] === '|')
    ) {
      pushShellToken(tokens, current, quoted)
      current = ''
      quoted = false
      tokens.push({ value: char + source[index + 1], quoted: false })
      index += 1
    } else if (char === '|' || char === ';') {
      pushShellToken(tokens, current, quoted)
      current = ''
      quoted = false
      tokens.push({ value: char, quoted: false })
    } else if (/\s/.test(char)) {
      pushShellToken(tokens, current, quoted)
      current = ''
      quoted = false
    } else {
      current += char
    }
  }
  pushShellToken(tokens, current, quoted)
  return tokens
}

export function tokenizeSimpleShell(command) {
  return tokenizeSimpleShellDetailed(command).map((token) => token.value)
}

function isExecutableFile(path) {
  try {
    const stat = statSync(path)
    return stat.isFile() && (stat.mode & 0o111) !== 0
  } catch {
    return false
  }
}

function resolvePathBinary(head, envPath) {
  if (!head || head.includes('/')) return false
  for (const dir of String(envPath ?? '').split(':')) {
    if (!dir) continue
    if (isExecutableFile(join(dir, head))) return true
  }
  return false
}

function commandHeadResolvable(tokens, cwd, env) {
  const head = tokens[0]
  if (!head) return false
  if (SHELL_BUILTINS.has(head)) return true
  if (head.startsWith('./') || head.startsWith('../') || head.includes('/')) {
    return isExecutableFile(isAbsolute(head) ? head : join(cwd, head))
  }
  if (resolvePathBinary(head, env?.PATH)) return true
  return false
}

function hasCommittedAnchor(tokens) {
  return tokens.some(
    (token) =>
      token === '--cached' ||
      token === '--staged' ||
      token === 'HEAD' ||
      token === 'FETCH_HEAD' ||
      token.includes('..') ||
      /^[0-9a-f]{7,40}$/i.test(token),
  )
}

/**
 * Does this command re-measure rather than merely run?
 *
 * Exported because `plan-contract-guard.mjs`'s `unmeasured-count-claim` needs exactly this
 * predicate, and a second definition of "asserts a count" is the copied-forward claim that whole
 * check exists to retire — the two must agree by construction, not by review.
 */
export function hasCountAssertion(command) {
  // `wc -c` / `-w` / `-m` count as re-measurement alongside `-l`: a byte size is the single most
  // common measured quantity in a plan premise (every descending-budget claim is one), and `wc -c`
  // measures it exactly as `wc -l` measures a line count.
  //
  // `grep -c` / `rg -c` are the MOST idiomatic counting commands of all, and omitting them is not a
  // neutral gap: this predicate gates two blocking codes (`unmeasured-count-claim` and, for a
  // criterion, `zero-selection-filter`), so a plan whose claim is correctly re-measured by `rg -c`
  // was rejected outright. The alternation admits bundled short flags (`grep -rc`) and separated
  // ones (`grep -r -c`) because both spell the same measurement.
  return /(#\s*(?:pass|tests)\s+\d+|grep\s+-q|rg\s+-q|\b(?:grep|rg)\s+(?:-\S+\s+)*-[A-Za-z]*c\b|wc\s+-[lcwm]|assert|count)/i.test(
    command,
  )
}

function advisory(code, message) {
  return { code, message }
}

function normalizeCommandTokens(tokens, { stripShellPreamble = true } = {}) {
  tokens = [...tokens]
  // `!` is the POSIX negation KEYWORD, not a command. `! rg -q needle file` is the natural way to
  // assert a pattern is ABSENT, and since `!` must be its own word to negate, it lands as the
  // segment head and resolves to no executable — so a legitimate negated check was unrecordable.
  // Strip exactly ONE leading `!` and no more: the real head is then classified normally, while a
  // segment that is only `!` keeps an EMPTY token list, which `commandHeadResolvable` still
  // rejects. That is what stops the relaxation becoming a blanket pass — the negation is removed
  // from the head position, it does not excuse the head from resolving.
  if (tokens[0] === '!') tokens.shift()
  while (tokens.length > 0 && /^[A-Za-z_][A-Za-z0-9_]*=/.test(tokens[0])) tokens.shift()
  if (tokens[0] === 'env') {
    tokens.shift()
    while (tokens.length > 0) {
      if (/^[A-Za-z_][A-Za-z0-9_]*=/.test(tokens[0])) {
        tokens.shift()
      } else if (tokens[0] === '--') {
        tokens.shift()
        break
      } else if (tokens[0] === '-u' || tokens[0] === '--unset') {
        tokens.splice(0, 2)
      } else if (tokens[0].startsWith('--unset=')) {
        tokens.shift()
      } else if (tokens[0] === '-C' || tokens[0] === '--chdir') {
        tokens.splice(0, 2)
      } else if (tokens[0].startsWith('--chdir=')) {
        tokens.shift()
      } else if (tokens[0] === '-i' || tokens[0] === '--ignore-environment') {
        tokens.shift()
      } else if (tokens[0].startsWith('-')) {
        tokens.shift()
      } else {
        break
      }
    }
  }
  if (stripShellPreamble && tokens[0] === 'set' && tokens[1] === '-o' && tokens[2] === 'pipefail') {
    tokens.splice(0, tokens[3] === ';' ? 4 : 3)
  }
  return tokens
}

function commandTokens(command) {
  return normalizeCommandTokens(tokenizeSimpleShell(command))
}

function commandSegments(tokens) {
  const segments = []
  let current = []
  for (const token of tokens) {
    if (SHELL_COMMAND_SEPARATORS.has(token.value)) {
      if (current.length > 0) segments.push(current)
      current = []
    } else {
      current.push(token)
    }
  }
  if (current.length > 0) segments.push(current)
  return segments.map((segment) =>
    normalizeCommandTokens(
      segment.map((token) => token.value),
      { stripShellPreamble: false },
    ),
  )
}

function resolveMakeInvocation(segment, cwd) {
  let directory = cwd
  const makefiles = []
  let makefileUnresolved = false
  const goals = []
  for (let index = 1; index < segment.length; index += 1) {
    const token = segment[index]
    if (token === '-C' || token === '--directory') {
      const value = segment[++index]
      if (value) directory = isAbsolute(value) ? value : join(directory, value)
    } else if (token.startsWith('-C') && token.length > 2) {
      const value = token.slice(2)
      directory = isAbsolute(value) ? value : join(directory, value)
    } else if (token.startsWith('--directory=')) {
      const value = token.slice('--directory='.length)
      if (value) directory = isAbsolute(value) ? value : join(directory, value)
    } else if (token === '-f' || token === '--file' || token === '--makefile') {
      const value = segment[++index]
      if (value) makefiles.push(value)
      else makefileUnresolved = true
    } else if (token.startsWith('-f') && token.length > 2) {
      makefiles.push(token.slice(2))
    } else if (token.startsWith('--file=')) {
      const value = token.slice('--file='.length)
      if (value) makefiles.push(value)
      else makefileUnresolved = true
    } else if (token.startsWith('--makefile=')) {
      const value = token.slice('--makefile='.length)
      if (value) makefiles.push(value)
      else makefileUnresolved = true
    } else if (
      token === '-I' ||
      token === '-o' ||
      token === '-W' ||
      token === '-E' ||
      token === '--include-dir' ||
      token === '--old-file' ||
      token === '--assume-old' ||
      token === '--what-if' ||
      token === '--new-file' ||
      token === '--assume-new' ||
      token === '--eval'
    ) {
      index += 1
    } else if ((token === '-j' || token === '--jobs') && /^\d+$/.test(segment[index + 1] ?? '')) {
      index += 1
    } else if (
      (token === '-l' || token === '--load-average' || token === '--max-load') &&
      /^(?:\d+(?:\.\d*)?|\.\d+)$/.test(segment[index + 1] ?? '')
    ) {
      index += 1
    } else if (!token.startsWith('-') && !/^[A-Za-z_][A-Za-z0-9_]*=/.test(token)) {
      goals.push(token)
    }
  }
  const makefile =
    makefileUnresolved || makefiles.length > 1
      ? null
      : makefiles.length === 1
        ? makefiles[0]
        : defaultMakefile(directory)
  return {
    makefile: makefile && (isAbsolute(makefile) ? makefile : join(directory, makefile)),
    goals,
  }
}

function defaultMakefile(directory) {
  for (const name of ['GNUmakefile', 'makefile', 'Makefile']) {
    const candidate = join(directory, name)
    if (existsSync(candidate)) return candidate
  }
  return join(directory, 'Makefile')
}

function readMakefileTargets(makefile) {
  if (!makefile) return { ok: false, closed: false, targets: new Set() }
  let source
  try {
    source = readFileSync(makefile, 'utf8')
  } catch {
    return { ok: false, closed: false, targets: new Set() }
  }
  const targets = new Set()
  const lines = source.replace(/\\\n/g, ' ').split('\n')
  let closed = true
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]
    if (/^\s*-?include\b/.test(line) || /\$\(eval\b/.test(line)) closed = false
    const phony = /^\s*\.PHONY:\s*(.*)$/.exec(line)
    if (phony) {
      let names = phony[1]
      while (names.endsWith('\\')) {
        names = names.slice(0, -1)
        names += ` ${lines[++index] ?? ''}`
      }
      for (const name of names.split(/\s+/)) if (name) targets.add(name)
      continue
    }
    const rule = /^\s*([^:#][^:]*)\s*:(?!=)/.exec(line)
    if (!rule) continue
    const head = rule[1].trim()
    if (head.includes('%') || head.includes('$(')) closed = false
    if (head === '.SECONDEXPANSION' || head === '.DEFAULT') closed = false
    for (const target of head.split(/\s+/)) {
      if (/^[A-Za-z][A-Za-z0-9_.-]*$/.test(target)) targets.add(target)
    }
  }
  return { ok: true, closed, targets }
}

function plainMakeGoal(goal) {
  return !/[/%$~*?[]/.test(goal)
}

function pathOperandsForSegment(segment) {
  const [head, ...args] = segment
  if (!['node', 'bash', 'sh', 'zsh'].includes(head)) return []
  if (
    args.some(
      (arg) =>
        ['-e', '--eval', '-p', '--print', '-c', '--input-type'].includes(arg) ||
        arg.startsWith('--input-type='),
    )
  ) {
    return []
  }
  const paths = []
  const testIndex = head === 'node' ? args.indexOf('--test') : -1
  if (testIndex >= 0) {
    const testArgs = args.slice(testIndex + 1)
    for (let index = 0; index < testArgs.length; index += 1) {
      const arg = testArgs[index]
      if (arg === '--') break
      if (NODE_TEST_OPTIONS_WITH_VALUE.has(arg)) {
        index += 1
        continue
      }
      if (!arg.startsWith('-')) paths.push(arg)
    }
  } else {
    for (const arg of args) {
      if (arg === '--' || arg === '#') break
      if (!arg.startsWith('-')) {
        paths.push(arg)
        break
      }
    }
  }
  return paths.filter((path) => !/[*?[$~]/.test(path))
}

function operandPathFinding(path, cwd) {
  const resolved = isAbsolute(path) ? path : join(cwd, path)
  if (existsSync(resolved)) return null
  if (!existsSync(dirname(resolved))) {
    return {
      blocking: true,
      code: 'path-operand-missing',
      message: `the command path operand's parent directory does not exist: ${path}`,
    }
  }
  return {
    blocking: false,
    code: 'path-operand-absent',
    // The operand rides along so a caller holding more context (the contract guard can see the
    // plan's own `## Key changes`) can drop the advisory for a file the plan itself creates. The
    // classifier stays context-free.
    path,
    message: `the command path operand is absent but its parent directory exists: ${path}`,
  }
}

// Measured false-drift shape #1: `grep -c '<th' Daemons.tsx` returned 6 against a correct
// five-column premise, because a bare-substring count also counts `<thead>`. The count flag is
// matched as `-c`, a short cluster containing `c`, or `--count`; `--color` and `-C` (context) are
// not counts and do not match.
const COUNT_FLAG_RE = /^(?:-[A-Za-z]*c[A-Za-z]*|--count)$/
// Word/line anchoring supplied as a FLAG rather than inside the pattern.
const ANCHOR_FLAG_RE = /^(?:-[A-Za-z]*[wx][A-Za-z]*|--word-regexp|--line-regexp)$/
// Word/line anchoring supplied inside the pattern itself.
const REGEX_ANCHOR_RE = /[\^$]|\\b|\\</

/**
 * A count whose pattern is anchored neither by flag nor by the pattern text.
 *
 * Deliberately NOT decided: WHICH token is the pattern. Identifying it means modelling every
 * value-taking option of two different tools, and a wrong guess fires on a correct command — the
 * exact brittleness this finding exists to warn about. Instead any argument carrying an anchor
 * suppresses the finding, so the only errors this can make are false NEGATIVES: `grep -cF '^x' f`
 * (where `-F` makes `^` literal) and a filename that happens to contain `^` or `$` both stay
 * silent. Advisory-only, so a silent correct run costs nothing and a false alarm blocks nothing.
 */
function hasSubstringCountOvermatch(segment) {
  const head = segment[0]
  if (head !== 'grep' && head !== 'rg') return false
  if (!segment.some((token) => COUNT_FLAG_RE.test(token))) return false
  if (segment.some((token) => ANCHOR_FLAG_RE.test(token))) return false
  return !segment.slice(1).some((token) => REGEX_ANCHOR_RE.test(token))
}

// Measured false-drift shape #2: BSD `sed` rejects `sed '1{/^$/d}'` with `extra characters
// at the end of d command`, producing a silent empty result that reads as a refuted premise.
const SED_INLINE_BRACE_RE = /\{[^{}\n]*\}/
// `-e<script>` and `--expression=<script>` carry the script ATTACHED to the option, so the
// non-option test below never sees them. Both spellings are unambiguously the script — no filename
// reaches this shape — so reading them adds true positives without admitting a false one.
const SED_ATTACHED_SCRIPT_RE = /^(?:-e|--expression=)(.+)$/s

/**
 * A `sed` script carrying a brace block that opens and closes on one line.
 *
 * Deliberately NOT decided: which argument is the script, whether the block is GNU-only in the
 * strict sense (BSD accepts some one-line brace forms), or whether the host running the check is
 * BSD at all. Any non-option argument containing a same-line `{...}` is treated as the script,
 * because a FILENAME containing a brace pair is not a shape this corpus produces. The finding says
 * "verify on the host that will run it", which is true of every one-line brace block regardless of
 * which side of the portability line it falls on.
 *
 * Two residual classes, both measured, neither blocking (this finding is advisory-only):
 * - False POSITIVES on braces that are not a block at all — an ERE interval quantifier
 *   (`sed -E 's/a{2,3}/b/' f`) and a literal brace pair in a substitution (`sed 's/{foo}/bar/' f`,
 *   `sed -n 's/{a}/b/p' f`) each fire, and the emitted "verify on the host" message is then
 *   factually wrong for them. Distinguishing a quantifier or a literal from an address block means
 *   parsing the sed script language, which is the brittleness this finding exists to warn about.
 * - False NEGATIVES for a script this cannot recognise as one — anything that is neither a bare
 *   non-option argument nor an `-e`/`--expression=` attachment (a script read from `-f file`, say).
 */
function hasGnuOnlySedAddress(segment) {
  if (segment[0] !== 'sed') return false
  return segment.slice(1).some((token) => {
    const attached = SED_ATTACHED_SCRIPT_RE.exec(token)
    if (attached) return SED_INLINE_BRACE_RE.test(attached[1])
    return !token.startsWith('-') && SED_INLINE_BRACE_RE.test(token)
  })
}

function hasUnquotedOptionGlob(tokens) {
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index]
    if (/^--[^=\s]+=.*[*?[]/.test(token.value) && !token.quoted) return true
    if (
      /^--(?:include|exclude|glob)$/.test(token.value) &&
      tokens[index + 1] &&
      /[*?[]/.test(tokens[index + 1].value) &&
      !tokens[index + 1].quoted
    ) {
      return true
    }
  }
  return false
}

/** Blocking findings emitted by classifyCheckCommand. Keep this closed so prose consumers cannot rot. */
export const COMMAND_BLOCKING_CODES = Object.freeze([
  'empty-command',
  'command-unresolvable',
  'make-goal-undefined',
  'path-operand-missing',
  'selection-matches-no-test',
  'unanchored-negative-search',
  // Promoted from the advisory tier for criteria only (see VACUOUS_GREEN_CODES).
  'zero-selection-filter',
  'pipe-without-pipefail',
  'git-grep-word-boundary',
])

/**
 * Findings whose command can **exit 0 while asserting nothing** — vacuous evidence.
 *
 * The discriminator is deliberately narrow, and it is what keeps the rest of the advisory tier
 * advisory. `cached-bazel-test` asserted something once (stale, not absent); `unquoted-option-glob`
 * aborts loudly under zsh; `substring-count-overmatch` over-counts, which fails loud;
 * `make-goal-unresolved` and `gnu-only-sed-address` are genuinely undecidable statically. Only
 * these three produce a green that demonstrates nothing: a selection filter that matched no test,
 * a pipeline reporting the tail's unconditional status, and a `git grep -E` whose `\b` the matcher
 * never interprets so the search finds nothing.
 *
 * The promotion is scoped to `kind === 'criterion'` — a criterion's check IS the evidence the
 * criterion is discharged by. A premise observes the PRE-change tree, where a zero-selection result
 * is frequently the true and intended answer, so the same finding stays advisory there. An unknown
 * kind keeps the advisory tier too: a caller that did not say what it is classifying has not
 * claimed the command is discharge evidence.
 */
const VACUOUS_GREEN_CODES = Object.freeze([
  'zero-selection-filter',
  'pipe-without-pipefail',
  'git-grep-word-boundary',
])

function rawCommandSegments(tokens) {
  const segments = []
  let current = []
  for (const token of tokens) {
    if (SHELL_COMMAND_SEPARATORS.has(token.value)) {
      if (current.length > 0) segments.push(current)
      current = []
    } else {
      current.push(token.value)
    }
  }
  if (current.length > 0) segments.push(current)
  return segments
}

/** The separator following each segment (`null` for the last), aligned with the segment lists. */
function segmentSeparators(tokens) {
  const separators = []
  let current = 0
  for (const token of tokens) {
    if (SHELL_COMMAND_SEPARATORS.has(token.value)) {
      if (current > 0) separators.push(token.value)
      current = 0
    } else {
      current += 1
    }
  }
  if (current > 0) separators.push(null)
  return separators
}

const SELECTION_FILTER =
  /^(-run|--test-name-pattern|--test-only|--test-skip-pattern|--include)(=|$)/

/**
 * Whether ONE segment carries a selection filter that can select zero and still exit 0.
 *
 * Judged per segment, not over the whole command: a plain `grep … --include='*.mjs'`
 * that selects zero files exits 1, so it cannot go green on nothing — as long as its exit status
 * IS the command's. That holds for the final segment and for one joined onward only by `&&`. A
 * segment followed by `||` (`grep … || true`), `;` or `|` hands the verdict to something else and
 * keeps the finding. A NEGATED search inverts exactly that exit status, so zero files reads as
 * success and keeps the finding too. Test selectors are unchanged: a runner that selects zero
 * tests routinely exits 0.
 */
function zeroSelectionRisk(segment, rawSegment, separatorAfter) {
  if (!segment.some((token) => SELECTION_FILTER.test(token))) return false
  const search = searchPatternAndPaths(segment)
  const negated = rawSegment?.[0] === '!'
  const ownsExitStatus = separatorAfter === null || separatorAfter === '&&'
  return !(search && !negated && ownsExitStatus)
}

const SEARCH_VALUE_OPTIONS = new Set([
  '-A',
  '-B',
  '-C',
  '-m',
  '-g',
  '-t',
  '--context',
  '--max-count',
  '--glob',
  '--iglob',
  '--type',
  '--type-not',
  '--include',
  '--exclude',
  '--exclude-dir',
])

function searchPatternAndPaths(segment) {
  if (!['rg', 'grep'].includes(segment[0])) return null
  const paths = []
  let pattern = null
  for (let index = 1; index < segment.length; index += 1) {
    const token = segment[index]
    if (token === '--') {
      paths.push(...segment.slice(index + 1))
      break
    }
    if (token.startsWith('-')) {
      if (/^(?:-e|--regexp)$/.test(token)) pattern = segment[++index] ?? null
      else if (token.startsWith('--regexp=')) pattern = token.slice('--regexp='.length)
      else if (SEARCH_VALUE_OPTIONS.has(token) || (segment[0] === 'rg' && token === '-T'))
        index += 1
      continue
    }
    if (pattern === null) pattern = token
    else paths.push(token)
  }
  return { pattern, paths }
}

function goTestRunPattern(segment) {
  if (segment[0] !== 'go' || segment[1] !== 'test') return null
  let pattern = null
  let packagePath = null
  const valueFlags = new Set([
    '-bench',
    '-benchtime',
    '-blockprofile',
    '-blockprofilerate',
    '-count',
    '-coverpkg',
    '-coverprofile',
    '-cpu',
    '-cpuprofile',
    '-failfast',
    '-fullpath',
    '-fuzz',
    '-fuzzcachedir',
    '-fuzzminimizetime',
    '-fuzztime',
    '-json',
    '-list',
    '-memprofile',
    '-memprofilerate',
    '-mutexprofile',
    '-mutexprofilefraction',
    '-outputdir',
    '-parallel',
    '-shuffle',
    '-timeout',
    '-trace',
    '-v',
  ])
  for (let index = 2; index < segment.length; index += 1) {
    const token = segment[index]
    if (token === '-run') {
      pattern = segment[++index] ?? null
    } else if (token.startsWith('-run=')) {
      pattern = token.slice('-run='.length)
    } else if (valueFlags.has(token)) {
      index += 1
    } else if (!token.startsWith('-') && packagePath === null) {
      packagePath = token
    }
  }
  if (!pattern || !packagePath || packagePath.includes('...')) return null
  return { pattern, packagePath }
}

function goTestRunFinding(segment, cwd) {
  const run = goTestRunPattern(segment)
  if (!run) return null
  const directory = isAbsolute(run.packagePath) ? run.packagePath : join(cwd, run.packagePath)
  let names
  try {
    if (!statSync(directory).isDirectory()) return null
    names = readdirSync(directory).filter((name) => name.endsWith('_test.go'))
  } catch {
    return null
  }
  let pattern
  try {
    pattern = new RegExp(run.pattern.split('/')[0])
  } catch {
    return null
  }
  try {
    for (const name of names) {
      const source = readFileSync(join(directory, name), 'utf8')
      const tests = source.matchAll(/^func\s+(?:\([^)]*\)\s+)?(Test[A-Za-z0-9_]*)\s*\(/gm)
      for (const test of tests) if (pattern.test(test[1])) return null
    }
  } catch {
    return null
  }
  return {
    code: 'selection-matches-no-test',
    message: `the go test -run pattern matches no test function in the concrete package: ${run.packagePath}`,
  }
}

/**
 * Remedies for the advisory command findings that carry one. The same text is appended to the
 * finding's message (so a `warning:` line is actionable on its own) and returned by
 * `commandFindingRemedy`, so the two cannot drift.
 */
const ADVISORY_COMMAND_REMEDIES = Object.freeze({
  'pipefail-early-exit-reader':
    'Write the output to a file first (cmd > out; grep -q X out), or count the whole stream (grep -c) instead of exiting on the first hit.',
  'unscoped-negative-directory-search':
    "Scope the search to the file type the claim is about (--include='*.sql', rg -g '*.sql'), or exclude test files (*_test.*).",
  'go-build-writes-output':
    'Use go build -o /dev/null <pkg>, go vet <pkg>, or go build ./... so nothing is written into the working directory.',
})

function remediedAdvisory(code, message) {
  return advisory(code, `${message} — ${ADVISORY_COMMAND_REMEDIES[code]}`)
}

/**
 * A reader that exits on its first hit: `grep`/`rg` with `-q`/`--quiet`/`--silent` (bundled short
 * flags such as `-qE` included) or a `-m N`/`--max-count` cap, and `head`. Downstream of a pipe it
 * closes the pipe early, the writer takes SIGPIPE, and under pipefail a PASSING check exits 141.
 * A counting reader (`grep -c`) drains the stream and is not one.
 */
function isEarlyExitReader(segment) {
  const [head, ...args] = segment
  if (head === 'head') return true
  if (head !== 'grep' && head !== 'rg') return false
  return args.some(
    (token) =>
      token === '--quiet' ||
      token === '--silent' ||
      token === '--max-count' ||
      token.startsWith('--max-count=') ||
      /^-[A-Za-z]*q[A-Za-z]*$/.test(token) ||
      /^-[A-Za-z]*m\d*$/.test(token),
  )
}

function hasPipefailEarlyExitReader(command, segments, separatorsAfter) {
  if (!/\bpipefail\b/.test(command)) return false
  return segments.some(
    (segment, index) =>
      index > 0 && separatorsAfter[index - 1] === '|' && isEarlyExitReader(segment),
  )
}

const RECURSIVE_GREP_FLAG_RE = /^(?:-[A-Za-z]*[rR][A-Za-z]*|--recursive|--dereference-recursive)$/
const FILE_TYPE_SCOPE_RE =
  /^(?:--(?:include|exclude|exclude-dir|glob|type|type-not|iglob)(?:=|$)|-[gtT])/

/**
 * A NEGATED recursive search over an existing directory with no file-type scoping. The negative
 * claim is about one kind of file (migration SQL, a config), but a directory-wide search also
 * matches the tests and guards that name the forbidden token, so the check goes red while the
 * intended claim holds. Distinct from `unscoped-premise-search`, which covers a search with NO path
 * operand. The directory is not walked for test files: fixtures and guards that name a token are not
 * reliably `_test` files, and an advisory does not justify a filesystem walk.
 */
function isUnscopedNegativeDirectorySearch(segment, rawSegment, cwd) {
  if (rawSegment?.[0] !== '!') return false
  const [head, ...args] = segment
  if (head === 'grep') {
    if (!args.some((token) => RECURSIVE_GREP_FLAG_RE.test(token))) return false
  } else if (head !== 'rg') {
    return false
  }
  if (args.some((token) => FILE_TYPE_SCOPE_RE.test(token))) return false
  const search = searchPatternAndPaths(segment)
  if (!search) return false
  return search.paths.some((path) => {
    if (/[*?[$~]/.test(path)) return false
    try {
      return statSync(isAbsolute(path) ? path : join(cwd, path)).isDirectory()
    } catch {
      return false
    }
  })
}

const GO_BUILD_VALUE_FLAGS = new Set([
  '-asmflags',
  '-buildmode',
  '-buildvcs',
  '-C',
  '-compiler',
  '-coverpkg',
  '-covermode',
  '-gccgoflags',
  '-gcflags',
  '-installsuffix',
  '-ldflags',
  '-mod',
  '-modfile',
  '-overlay',
  '-p',
  '-pgo',
  '-pkgdir',
  '-tags',
  '-toolexec',
])

/**
 * `go build <one package>` with no `-o`: building a single main package writes a binary named after
 * the directory into the working directory, which fails when a same-named directory exists
 * (`go build ./cmd/` next to `cmd/`). The classifier does not track `cd`, so it flags the risky
 * shape rather than a proven clash — advisory only.
 */
function isGoBuildWritingOutput(segment) {
  if (segment[0] !== 'go' || segment[1] !== 'build') return false
  const operands = []
  for (let index = 2; index < segment.length; index += 1) {
    const token = segment[index]
    if (token === '-o' || token.startsWith('-o=') || token === '--o' || token.startsWith('--o=')) {
      return false
    }
    if (GO_BUILD_VALUE_FLAGS.has(token.replace(/^--/, '-'))) {
      index += 1
      continue
    }
    if (token.startsWith('-')) continue
    operands.push(token)
  }
  return operands.length === 1 && !operands[0].includes('...')
}

/**
 * Classify a recorded check command without executing it.
 *
 * The command is PR-body text, so this function deliberately uses only static checks. Decidable
 * failures land in `blocking`; heuristic proof-quality risks land in `advisory`.
 */
export function classifyCheckCommand(
  command,
  { cwd = process.cwd(), env = process.env, kind = null } = {},
) {
  const detailedTokens = tokenizeSimpleShellDetailed(command)
  const segments = commandSegments(detailedTokens)
  const rawSegments = rawCommandSegments(detailedTokens)
  const separatorsAfter = segmentSeparators(detailedTokens)
  const tokens = commandTokens(command)
  const blocking = []
  const advisoryFindings = []
  // One promotion decision for the whole vacuous-green family, keyed on `kind`. Every member routes
  // through here rather than pushing its own tier, so no call site repeats the tier choice.
  //
  // Adding a fourth member is NOT a one-line edit, and saying so here would be false: it needs a row
  // in `VACUOUS_GREEN_CODES`, a row in `COMMAND_BLOCKING_CODES`, a `commandFindingRemedy` branch, and
  // its call site converted to `recordVacuityRisk`. Nothing gates that those agree.
  const recordVacuityRisk = (code, message) => {
    if (kind === 'criterion' && VACUOUS_GREEN_CODES.includes(code)) blocking.push({ code, message })
    else advisoryFindings.push(advisory(code, message))
  }
  const trimmed = String(command ?? '').trim()
  if (!trimmed) {
    return {
      blocking: [
        {
          code: 'empty-command',
          message: 'the check command is empty',
        },
      ],
      advisory: advisoryFindings,
    }
  }
  if (segments.some((segment) => !commandHeadResolvable(segment, cwd, env))) {
    blocking.push({
      code: 'command-unresolvable',
      message:
        'the command head resolves to no executable PATH binary or executable repo-relative script',
    })
  }
  for (const segment of segments) {
    const goRunFinding = goTestRunFinding(segment, cwd)
    if (goRunFinding) blocking.push(goRunFinding)
    if (segment[0] === 'make') {
      const invocation = resolveMakeInvocation(segment, cwd)
      const makefile = readMakefileTargets(invocation.makefile)
      for (const goal of invocation.goals) {
        if (!plainMakeGoal(goal) || makefile.targets.has(goal)) continue
        const finding =
          makefile.ok && makefile.closed
            ? {
                code: 'make-goal-undefined',
                message: `the make goal is not defined by the statically closed Makefile: ${goal}`,
              }
            : advisory(
                'make-goal-unresolved',
                `the make goal cannot be resolved from a missing, unreadable, or open Makefile: ${goal}`,
              )
        if (makefile.ok && makefile.closed) blocking.push(finding)
        else advisoryFindings.push(finding)
      }
    }
    for (const path of pathOperandsForSegment(segment)) {
      const finding = operandPathFinding(path, cwd)
      if (!finding) continue
      if (finding.blocking) blocking.push({ code: finding.code, message: finding.message })
      else advisoryFindings.push({ ...advisory(finding.code, finding.message), path: finding.path })
    }
  }

  for (let index = 0; index < segments.length; index += 1) {
    const search = searchPatternAndPaths(segments[index])
    if (!search) continue
    if (kind === 'premise' && search.paths.length === 0) {
      advisoryFindings.push(
        advisory(
          'unscoped-premise-search',
          'a premise search without a path operand can inspect the wrong tree',
        ),
      )
    }
    if (
      kind === 'criterion' &&
      rawSegments[index]?.[0] === '!' &&
      search.pattern &&
      !/(?:\\b|\\<|\\>|\^|\$)/.test(search.pattern) &&
      !segments[index].some(
        (token) => /^-[A-Za-z]*w[A-Za-z]*$/.test(token) || token === '--word-regexp',
      ) &&
      /[A-Za-z0-9_]$/.test(search.pattern)
    ) {
      blocking.push({
        code: 'unanchored-negative-search',
        message:
          'a negated bare-substring search can reject a correctly named helper that extends the pattern',
      })
    }
  }

  if (
    tokens[0] === 'git' &&
    (tokens[1] === 'diff' || tokens[1] === 'status') &&
    !hasCommittedAnchor(tokens)
  ) {
    advisoryFindings.push(
      advisory(
        'working-tree-scoped-git-check',
        'working-tree-scoped git evidence can pass after unrelated local cleanup',
      ),
    )
  }
  if (
    segments.some((segment, index) =>
      zeroSelectionRisk(segment, rawSegments[index], separatorsAfter[index]),
    ) &&
    !hasCountAssertion(trimmed)
  ) {
    recordVacuityRisk(
      'zero-selection-filter',
      'a test or file-selection filter can select zero tests/files without a count assertion',
    )
  }
  if (hasUnquotedOptionGlob(detailedTokens)) {
    advisoryFindings.push(
      advisory(
        'unquoted-option-glob',
        'a glob embedded in an option value can be expanded or rejected by the shell before the command runs',
      ),
    )
  }
  if (
    tokens.includes('|') &&
    !tokens.includes('pipefail') &&
    !trimmed.includes('set -o pipefail')
  ) {
    recordVacuityRisk(
      'pipe-without-pipefail',
      'a pipeline without pipefail reports the tail command status rather than the failing command',
    )
  }
  // The pipe rule above passes any pipeline that carries pipefail, which is exactly the shape that
  // turns an early-exit reader's SIGPIPE into a false red (141 on a passing check).
  if (hasPipefailEarlyExitReader(trimmed, segments, separatorsAfter)) {
    advisoryFindings.push(
      remediedAdvisory(
        'pipefail-early-exit-reader',
        'a reader that exits on its first hit closes the pipe early, the writer takes SIGPIPE, and pipefail reports 141 for a passing check',
      ),
    )
  }
  if (
    segments.some((segment, index) =>
      isUnscopedNegativeDirectorySearch(segment, rawSegments[index], cwd),
    )
  ) {
    advisoryFindings.push(
      remediedAdvisory(
        'unscoped-negative-directory-search',
        'a directory-wide negative search also matches the tests and guards that name the forbidden token',
      ),
    )
  }
  if (segments.some(isGoBuildWritingOutput)) {
    advisoryFindings.push(
      remediedAdvisory(
        'go-build-writes-output',
        'building one main package without -o writes a binary named after the directory into the working directory, which fails when a same-named directory exists',
      ),
    )
  }
  if (tokens[0] === 'git' && tokens[1] === 'grep' && tokens.includes('-E') && /\\b/.test(trimmed)) {
    recordVacuityRisk(
      'git-grep-word-boundary',
      'git grep -E does not interpret \\b as a word boundary',
    )
  }
  if (
    tokens[0] === 'bazel' &&
    tokens[1] === 'test' &&
    !tokens.some((token) => token === '--nocache_test_results')
  ) {
    advisoryFindings.push(
      advisory('cached-bazel-test', 'a cached Bazel test can predate newly added inputs'),
    )
  }
  if (segments.some(hasSubstringCountOvermatch)) {
    advisoryFindings.push(
      advisory(
        'substring-count-overmatch',
        'a bare-substring count also counts superstring matches, so the number can exceed the real one',
      ),
    )
  }
  if (segments.some(hasGnuOnlySedAddress)) {
    advisoryFindings.push(
      advisory(
        'gnu-only-sed-address',
        'BSD sed rejects some brace forms GNU sed accepts and can fail into an empty result, so verify it on the host that will run it',
      ),
    )
  }
  return { blocking, advisory: advisoryFindings }
}

/**
 * Strip a leading run of markdown emphasis so `**(verify-only)**` classifies like `(verify-only)`.
 *
 * The marker is a literal PREFIX by design, and that stays true — this normalises only where the
 * prefix may start, never what counts as the marker, so it adds no heuristic and no false positive.
 * What it removes is a silent FALSE NEGATIVE: the drafting brief bolds the token in its own prose a
 * line above the template it tells planners to copy, so a bolded marker is the likeliest drafting
 * slip, and an unrecognised marker hands the criterion the diff-demonstrated route — the very
 * silence this contract exists to end — with nothing anywhere to detect it.
 */
function stripEmphasis(value) {
  return value.replace(/^[*_]+/, '')
}

/**
 * Parse the `## Acceptance criteria` section of a plan description (or a PR body written from the
 * same template) into structured criteria.
 *
 * Scope is the `## Acceptance criteria` section **only**, obtained from the existing
 * `planDescriptionSections()` splitter rather than a second scan of the whole string. That splitter
 * already stops at the terminal `## Original notes` heading, so a criterion echoed inside the
 * preserved original notes cannot spoof this parse — a property already documented and tested for
 * `validatePlanDescription`, inherited here rather than re-derived. Lines inside fenced code blocks
 * are skipped for the same reason `planDescriptionSections()` skips them: a plan legitimately quotes
 * the criterion TEMPLATE, and reading a sample bullet as a real criterion invents one nobody wrote.
 *
 * A criterion wrapped across several lines is joined into one `text` before matching, because the
 * ` — check: ` / ` — checked: ` clause routinely lands on a continuation line.
 *
 * Returns DATA and never throws for a malformed criterion — a bullet with no clause is simply one
 * whose `check` is `null`, which is the caller's decision to make, not this parser's.
 *
 * @param {object} config skill config (config-first, like `validatePlanDescription`)
 * @param {string} description plan description or PR body
 * @returns {{
 *   text: string,
 *   checked: boolean,
 *   verifyOnly: boolean,
 *   malformedVerifyOnlyMarker: boolean,
 *   check: string | null,
 *   result: string | null,
 *   checkedClause: boolean,
 *   checkedCommandDelimited: boolean,
 *   plannedTenseOnTicked: boolean
 * }[]}
 *   `text` is the whole bullet body after the checkbox, marker and clause included, so a report can
 *   name the criterion verbatim. `checked` is the box state. `verifyOnly` is the literal-marker
 *   classification. `check` is the command named by either clause (backticks stripped) or `null`
 *   when neither clause is present. A `— checked: ` clause MUST delimit its command in backticks:
 *   an undelimited one is undecidable (an arrow inside the command is indistinguishable from the
 *   result separator), so it yields `check: null` and the evidence gate blocks rather than guess.
 *   `result` is the observed result from the discharge clause only, or `null` when the criterion is
 *   not yet discharged. An empty clause yields `''`, never `null` — the difference between "absent"
 *   and "empty" is what the evidence gate turns on.
 */
function parseCheckboxSection(config, description, heading, fn) {
  assertConfigFirst(config, fn)
  const section = planDescriptionSections(config, description).find((s) => s.heading === heading)
  if (!section) return []
  return parseCheckboxBody(section.bodyLines.join('\n'))
}

/** Leading-whitespace width in columns, with markdown's 4-column tab stops. */
function indentWidth(line) {
  let width = 0
  for (const char of /^[ \t]*/.exec(line)[0]) {
    width = char === '\t' ? width + 4 - (width % 4) : width + 1
  }
  return width
}

/**
 * Group list entries with the one continuation rule every bullet parser here shares.
 *
 * `lines` are the body's lines OUTSIDE fences, in order (a fenced line is a sample, not an entry,
 * and neither opens nor ends one). `opens(trimmed)` returns a match when a line opens an entry.
 *
 * A list item NESTED deeper than the item that opened the current entry continues it — ordinary
 * markdown habit when evidence or a long command gets its own line — and so does a non-list
 * (wrapped or lazy) line. Ending on ANY list item drops a clause written as a nested bullet, so
 * evidence that is present reads as missing; letting ANY indented item continue folds a uniformly
 * indented flat list (` - a\n - b`) into its first entry, so a sibling's defect is never seen. A
 * blank line, a heading, or a list item at the opener's indent or shallower ends the entry, and the
 * latter opens the next one when `opens` matches it.
 *
 * @returns {{ match: RegExpExecArray, parts: string[] }[]} `parts` are the continuation texts,
 *   trimmed, with a continuation sub-bullet's own marker dropped so the joined text reads as one.
 */
function groupListEntries(lines, opens) {
  const entries = []
  let current = null
  for (const line of lines) {
    const trimmed = line.trim()
    const listItem = LIST_ITEM_RE.test(trimmed)
    const nested = current !== null && listItem && indentWidth(line) > current.indent
    const match = nested ? null : opens(trimmed)
    if (match) {
      current = { match, parts: [], indent: indentWidth(line) }
      entries.push(current)
      continue
    }
    if (!current) continue
    if (trimmed === '' || /^#{1,6}\s/.test(trimmed) || (listItem && !nested)) {
      current = null
      continue
    }
    current.parts.push(trimmed.replace(LIST_ITEM_RE, ''))
  }
  return entries.map(({ match, parts }) => ({ match, parts }))
}

/** The checkbox bullets of one section BODY (no headings), with the same grammar for every caller. */
function parseCheckboxBody(body) {
  const outside = scanFences(body).lines.map(({ line }) => line)
  const raw = groupListEntries(outside, (trimmed) => CRITERION_RE.exec(trimmed)).map(
    ({ match, parts }) => ({ box: match[1], parts: [match[2].trim(), ...parts] }),
  )

  return raw.map(({ box, parts }) => {
    const text = parts.filter(Boolean).join(' ').trim()
    let check = null
    let result = null
    let checkedClause = false
    let checkedCommandDelimited = false
    let plannedTenseOnTicked = false
    const dischargeAt = text.indexOf(VERIFY_ONLY_CHECKED)
    if (dischargeAt !== -1) {
      checkedClause = true
      const tail = text.slice(dischargeAt + VERIFY_ONLY_CHECKED.length)
      // A BACKTICKED command is a DELIMITED span, so close it BEFORE looking for the result
      // separator. Searching for the separator first lets an arrow INSIDE the command act as one:
      // `— checked: \`echo a→b\`` carries no result clause at all, yet splitting at that arrow
      // yields a non-empty command AND a non-empty result, so the criterion PASSES — the exact
      // "present but no result" case this gate exists to reject, and a bypass any command
      // containing an arrow (`rg "a→b"`) triggers by accident. Matching the backtick RUN also
      // retires the greedy `unwrapCode` strip, which turned a command carrying a second code span
      // into a garbled `check` no reviewer could paste while the gate still reported green.
      // The delimiter is REQUIRED on the discharge path, and the requirement is what makes the
      // parse decidable at all. Without it there is no way to tell `rg "a→b" src/` (one command,
      // no result) from `make check → identical` (command, then result): both are one arrow in
      // undelimited text, so ANY split rule gets one of them wrong. Guessing picked the bypass —
      // the first row split into a non-empty command and a non-empty result and PASSED carrying no
      // result at all, and the pasteable command was garbled to `rg "a` either way. So an
      // undelimited discharge is `check = null`: the gate BLOCKS and names the requirement, which
      // is the fail-closed direction and a one-keystroke fix for the builder. Both shipped
      // templates already backtick the command, so this rejects nothing they teach.
      const fenced = /(`+)([\s\S]*?)\1/.exec(tail)
      if (fenced) {
        checkedCommandDelimited = true
        check = fenced[2].trim()
        const rest = tail.slice(fenced[0].length)
        const separator = RESULT_SEPARATOR_RE.exec(rest)
        if (separator) result = rest.slice(separator.index + separator[0].length).trim()
      }
    } else {
      const plannedAt = text.indexOf(VERIFY_ONLY_CHECK)
      if (plannedAt !== -1) {
        if (box.toLowerCase() === 'x') {
          plannedTenseOnTicked = true
        } else {
          check = unwrapCode(text.slice(plannedAt + VERIFY_ONLY_CHECK.length))
        }
      }
    }
    const markerText = stripEmphasis(text)
    return {
      text,
      checked: box.toLowerCase() === 'x',
      verifyOnly: markerText.startsWith(VERIFY_ONLY_MARKER),
      malformedVerifyOnlyMarker:
        markerText.includes(VERIFY_ONLY_MARKER) && !markerText.startsWith(VERIFY_ONLY_MARKER),
      check,
      result,
      checkedClause,
      checkedCommandDelimited,
      plannedTenseOnTicked,
    }
  })
}

export function parseAcceptanceCriteria(config, description) {
  return parseCheckboxSection(config, description, ACCEPTANCE_HEADING, 'parseAcceptanceCriteria')
}

/**
 * Parse the optional `## Premises` section into structured premise bullets.
 *
 * Premises deliberately reuse the acceptance-criterion bullet grammar and check-clause notation so
 * the consumer can discharge them with the same evidence shape. A premise is central only when its
 * claim starts with the literal `(central)` marker, after optional markdown emphasis; two central
 * premises are reported in-band so callers cannot silently pick the last one.
 *
 * @returns {{ text: string, claim: string, check: string | null, central: boolean, duplicateCentral: boolean }[]}
 */
export function parsePremises(config, description) {
  return toPremises(parseCheckboxSection(config, description, PREMISES_HEADING, 'parsePremises'))
}

function toPremises(items) {
  const premises = items.map((premise) => {
    const central = CENTRAL_PREMISE_MARKER_RE.test(premise.text)
    const claim = (central ? premise.text.replace(CENTRAL_PREMISE_MARKER_RE, '') : premise.text)
      .replace(VERIFY_ONLY_CHECK + (premise.check === null ? '' : `\`${premise.check}\``), '')
      .trim()
    return {
      text: premise.text,
      claim,
      check: premise.check,
      central,
      duplicateCentral: false,
    }
  })
  const central = premises.filter((premise) => premise.central)
  if (central.length > 1) {
    for (const premise of central) premise.duplicateCentral = true
  }
  return premises
}

const ATX_HEADING_RE = /^ {0,3}(#{1,6})[ \t]+(.*?)[ \t#]*$/
const PREMISE_HEADING_RE = /premise/i

/**
 * Parse a free-form plan FILE into its criterion and premise checkbox bullets.
 *
 * Since the attachment became the plan and the description only points at it, acceptance criteria
 * and premises live in the plan file under whatever headings the drafter chose (`## Acceptance
 * criteria`, `### Done means`, …). The heading-scoped description parsers never see them, so this
 * one is heading-AGNOSTIC: the plan is split at every ATX heading outside fenced code, parsing stops
 * at the terminal contract heading (`## Original notes`, reporter text), and each checkbox bullet is
 * a PREMISE when its nearest preceding heading contains "premise" and a CRITERION otherwise. A
 * premise misread as a criterion is only linted under the stricter criterion tier, which the
 * contract guard reports as warnings anyway.
 *
 * The bullet grammar is `parseCheckboxBody`, shared with `parseAcceptanceCriteria` / `parsePremises`,
 * so item shapes match theirs exactly. `text` is the pre-terminal plan text the items came from, for
 * callers that compare an item against the rest of the plan.
 *
 * @returns {{ criteria: object[], premises: object[], text: string }}
 */
export function parsePlanCheckItems(config, plan) {
  assertConfigFirst(config, 'parsePlanCheckItems')
  const source = String(plan ?? '')
  const contract = planSectionsForDescriptionMode(config, 'child-plan')
  const terminalKey = headingKey(contract[contract.length - 1]?.heading ?? '')
  const outside = new Set(scanFences(source).lines.map(({ index }) => index))
  const lines = source.split('\n')
  const sections = [{ heading: '', bodyLines: [] }]
  const kept = []
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index].replace(/\r$/, '')
    const atx = outside.has(index) ? ATX_HEADING_RE.exec(line) : null
    if (atx) {
      const heading = `${atx[1]} ${atx[2]}`
      if (terminalKey && headingKey(heading) === terminalKey) break
      sections.push({ heading, bodyLines: [] })
    } else {
      sections[sections.length - 1].bodyLines.push(line)
    }
    kept.push(line)
  }
  const criteria = []
  const premiseItems = []
  for (const section of sections) {
    const items = parseCheckboxBody(section.bodyLines.join('\n'))
    if (PREMISE_HEADING_RE.test(section.heading)) premiseItems.push(...items)
    else criteria.push(...items)
  }
  return { criteria, premises: toPremises(premiseItems), text: kept.join('\n') }
}

/**
 * Validate that every DISCHARGED verify-only criterion carries the evidence its marker promises.
 *
 * The gate checks **structure and non-emptiness**, never truth: it cannot tell whether the recorded
 * command was really run, and does not pretend to. What it buys is that the check becomes named and
 * re-runnable by a human reviewer, so a verify-only criterion can no longer be discharged in silence.
 *
 * Only a criterion that is BOTH `verifyOnly` and `checked` can fail. An unticked verify-only
 * criterion is reported in `verifyOnly` but is never a `missingEvidence` failure — an open criterion
 * is already caught by the consumer's existing required-deferred rule, and double-reporting it would
 * make this gate cry wolf on a condition another gate already owns.
 *
 * A description carrying no marker at all yields `verifyOnly: []` and `ok: true`, so every plan
 * written before this contract keeps validating unchanged.
 *
 * @param {object} config skill config (config-first)
 * @param {string} body plan description or PR body carrying an `## Acceptance criteria` section
 * @returns {{ ok: boolean, verifyOnly: object[], missingEvidence: object[], malformedMarker: object[], advisory: object[] }}
 *   criteria as returned by `parseAcceptanceCriteria`. `ok` is true iff `missingEvidence` is empty.
 *   `malformedMarker` surfaces criteria that contain a misplaced `(verify-only)` marker without
 *   reclassifying them. `advisory` carries non-blocking command-shape risks.
 */
export function validateVerifyOnlyEvidence(config, body) {
  assertConfigFirst(config, 'validateVerifyOnlyEvidence')
  const criteria = parseAcceptanceCriteria(config, body)
  const verifyOnly = criteria.filter((c) => c.verifyOnly)
  const malformedMarker = criteria.filter((c) => c.malformedVerifyOnlyMarker)
  const missingEvidence = []
  const advisory = []
  for (const criterion of verifyOnly) {
    if (!criterion.checked) continue
    let reason = null
    let remedy = null
    if (criterion.plannedTenseOnTicked) {
      reason = 'planned-tense-on-ticked'
      remedy = `Use "${VERIFY_ONLY_CHECKED.trim()}" on checked verify-only criteria.`
    } else if (!criterion.checkedClause) {
      reason = 'no-clause'
      remedy = `Add "${VERIFY_ONLY_CHECKED.trim()} \`<command>\`${VERIFY_ONLY_RESULT}<result>".`
    } else if (!criterion.checkedCommandDelimited) {
      reason = 'undelimited-command'
      remedy = 'Wrap the discharged command in backticks so the result separator is unambiguous.'
    } else if (!nonEmpty(criterion.check)) {
      reason = 'empty-command'
      remedy = 'Record a non-empty command between the backticks.'
    } else if (!nonEmpty(criterion.result)) {
      reason = 'empty-result'
      remedy = 'Record the non-empty result after the result separator.'
    } else {
      const classified = classifyCheckCommand(criterion.check, { kind: 'criterion' })
      advisory.push(...classified.advisory.map((finding) => ({ ...finding, criterion })))
      if (classified.blocking.length > 0) {
        reason = classified.blocking[0].code
        remedy = commandFindingRemedy(reason)
      }
    }
    if (reason) {
      missingEvidence.push({
        ...criterion,
        reason,
        remedy,
        check: reason === 'planned-tense-on-ticked' ? null : criterion.check,
      })
    }
  }
  return {
    ok: missingEvidence.length === 0,
    verifyOnly,
    missingEvidence,
    malformedMarker,
    advisory,
  }
}

function parseVerificationBullet(text) {
  let quoted = false
  let qualifierStart = -1
  for (let i = 0; i < text.length; i += 1) {
    if (text[i] === '`') quoted = !quoted
    if (quoted) continue
    if (text[i] === '(') qualifierStart = i
    const arrowLength = text[i] === '→' ? 1 : text.startsWith('->', i) ? 2 : 0
    if (!arrowLength) continue
    let prefix = text.slice(0, i).trimEnd()
    let qualifier = null
    if (prefix.endsWith(')')) {
      if (qualifierStart < 0) return null
      qualifier = prefix.slice(qualifierStart + 1, -1).trim()
      if (!qualifier || /[()]/.test(qualifier)) return null
      prefix = prefix.slice(0, qualifierStart).trimEnd()
    }
    if (!prefix.endsWith('`')) return null
    const commandStart = prefix.lastIndexOf('`', prefix.length - 2)
    if (commandStart < 0) return null
    const label = prefix.slice(0, commandStart).trimEnd()
    if (label && !label.endsWith(':')) return null
    return {
      phase: label.slice(0, -1).trim() || null,
      command: prefix.slice(commandStart + 1, -1).trim(),
      qualifier,
      result: text.slice(i + arrowLength).trim(),
    }
  }
  return null
}

/**
 * Validate the structural local-test record in a PR body; this never proves execution.
 * Findings carry remedies for body edits and do not determine a shipping route.
 * @returns {{ ok: boolean, entries: object[], gaps: string[], missingEvidence: object[] }}
 */
export function validateLocalVerification(config, body) {
  assertConfigFirst(config, 'validateLocalVerification')
  const entries = []
  const gaps = []
  const missingEvidence = []
  let found = false
  let inSection = false
  let bullets = 0
  const remedies = {
    'missing-section': 'Add a ## Local verification section.',
    'empty-section': 'Record a command and its result, or a gap: explanation.',
    'duplicate-section': 'Merge the ## Local verification sections into one.',
    'undelimited-command':
      'Use an optional label: (backticks allowed), one backticked command, an optional (qualifier), then → or -> and the result; put any other prose after the result.',
    'empty-command': 'Record a non-empty command between the backticks.',
    'empty-result': 'Record the command final summary after → or ->.',
    'empty-gap': 'Describe what could not be selected reliably and why after gap:.',
    placeholder: 'Replace the template <…> placeholder with the real command, result or gap.',
  }
  // A field that is one whole `<…>` token is the publish template pasted unfilled.
  const placeholder = (field) => /^<[^<>]*>$/.test(field.trim())
  const finding = (reason, text) =>
    missingEvidence.push({
      reason,
      remedy: remedies[reason],
      ...(text === undefined ? {} : { text }),
    })
  const sectionLines = []
  for (const { line } of scanFences(body).lines) {
    const heading = markdownH2Heading(line)
    if (heading) {
      sectionLines.push('') // a heading ends any open entry
      inSection = normaliseSectionHeading(heading) === 'local verification'
      // Duplicates are undecidable, as in merge-eligibility.mjs `sections()`; a trailing colon
      // names a different section there too, so it is not tolerated here either.
      if (inSection && found) finding('duplicate-section', heading)
      found ||= inSection
      continue
    }
    // A parent heading ends the section as well.
    if (/^ {0,3}#\s/.test(line)) inSection = false
    if (inSection) sectionLines.push(line)
  }
  // The continuation rule is `parseCheckboxBody`'s, so a wrapped result or a nested detail line is
  // part of its entry, while a same-indent sibling is an entry of its own.
  const items = groupListEntries(sectionLines, (trimmed) => /^[-*+]\s+(.*)$/.exec(trimmed)).map(
    ({ match, parts }) => [match[1], ...parts],
  )
  for (const parts of items) {
    bullets += 1
    const text = parts.filter(Boolean).join(' ').trim()
    const gap = /^gap:\s*(.*)$/i.exec(text)
    if (gap) {
      if (!gap[1]) finding('empty-gap', text)
      else if (placeholder(gap[1])) finding('placeholder', text)
      else gaps.push(gap[1])
      continue
    }
    const entry = parseVerificationBullet(text)
    if (!entry) finding('undelimited-command', text)
    else if (!entry.command) finding('empty-command', text)
    else if (!entry.result) finding('empty-result', text)
    else if (placeholder(entry.command) || placeholder(entry.result)) finding('placeholder', text)
    else entries.push(entry)
  }
  if (!found) finding('missing-section')
  else if (!bullets) finding('empty-section')
  return {
    ok: missingEvidence.length === 0 && entries.length + gaps.length > 0,
    entries,
    gaps,
    missingEvidence,
  }
}

export function commandFindingRemedy(code) {
  if (code === 'make-goal-undefined') {
    return 'Use a goal defined by the named Makefile, or record the underlying check command.'
  }
  if (code === 'path-operand-missing') {
    return 'Use a path whose parent directory exists in the checkout, or record the correct check command.'
  }
  if (code === 'selection-matches-no-test') {
    return 'Use a -run pattern that matches a test in the named concrete package, or run the package without a filter.'
  }
  if (code === 'unanchored-negative-search') {
    return 'Anchor the negative search with a word or line boundary so helper-name extensions do not trip it.'
  }
  if (code === 'zero-selection-filter') {
    return 'Add a count assertion the selection must satisfy, or drop the filter so the whole target runs.'
  }
  if (code === 'pipe-without-pipefail') {
    return 'Prefix the pipeline with "set -o pipefail; ", or record the head command without the pipe.'
  }
  if (code === 'git-grep-word-boundary') {
    return 'Use git grep -P for \\b, or anchor with a character class git grep -E does interpret.'
  }
  if (Object.hasOwn(ADVISORY_COMMAND_REMEDIES, code)) return ADVISORY_COMMAND_REMEDIES[code]
  return 'Use a command whose head resolves to an executable PATH binary or executable repo-relative script.'
}

/**
 * The globs whose change always routes a verify run to a human, as a fresh copy. Absent or
 * malformed (a hand-built config that skipped validateConfig) resolves to `[]`, which costs the
 * verify stage nothing: it reads the PR diff only when this list is non-empty.
 * @returns {string[]}
 */
export function verifyAlwaysHumanPaths(config) {
  assertStageBlockReadable(config, 'verifyDefaults')
  return globList(config?.verifyDefaults?.alwaysHumanPaths)
}

/** The closed set of named human-review categories under `verifyDefaults.humanReview`. */
export const VERIFY_HUMAN_REVIEW_CATEGORIES = Object.freeze(['database', 'security', 'ui'])

/** The longest operator-written label a `{glob, label}` policy entry may carry. */
export const VERIFY_PATH_LABEL_MAX = 60

// A policy entry is a glob string or `{glob, label?}`; `label` names the paths for the person a park
// pages. These readers drop malformed entries (a config that skipped validation); the compiled
// schema rejects them.
const entryGlob = (entry) =>
  typeof entry === 'string'
    ? entry
    : entry && typeof entry === 'object' && !Array.isArray(entry)
      ? entry.glob
      : undefined
const globList = (paths) =>
  Array.isArray(paths)
    ? paths.map(entryGlob).filter((g) => typeof g === 'string' && g.length > 0)
    : []
const isPathLabel = (label) =>
  typeof label === 'string' &&
  label.trim().length > 0 &&
  !/[\r\n]/.test(label) &&
  Array.from(label).length <= VERIFY_PATH_LABEL_MAX
const isGlobEntry = (entry) => {
  if (typeof entry === 'string') return entry.length > 0
  if (!entry || typeof entry !== 'object' || Array.isArray(entry)) return false
  if (Object.keys(entry).some((key) => key !== 'glob' && key !== 'label')) return false
  if (typeof entry.glob !== 'string' || entry.glob.length === 0) return false
  return entry.label === undefined || isPathLabel(entry.label)
}

/**
 * The named human-review globs, always with all three categories (absent ones as `[]`), as fresh
 * copies. A malformed block that skipped validateConfig resolves to empty lists.
 * @returns {{database: string[], security: string[], ui: string[]}}
 */
export function verifyHumanReviewPaths(config) {
  assertStageBlockReadable(config, 'verifyDefaults')
  const review = config?.verifyDefaults?.humanReview
  return Object.fromEntries(
    VERIFY_HUMAN_REVIEW_CATEGORIES.map((category) => [
      category,
      review && typeof review === 'object' ? globList(review[category]) : [],
    ]),
  )
}

/**
 * The labelled policy globs, in config order (`alwaysHumanPaths`, then each `humanReview` category
 * in VERIFY_HUMAN_REVIEW_CATEGORIES order), as `{list, glob, label}` where `list` is
 * `alwaysHumanPaths` or `humanReview.<category>`. Unlabelled and malformed entries are skipped.
 * @returns {{list: string, glob: string, label: string}[]}
 */
export function verifyPathLabels(config) {
  assertStageBlockReadable(config, 'verifyDefaults')
  const block = config?.verifyDefaults
  const review =
    block?.humanReview && typeof block.humanReview === 'object' ? block.humanReview : {}
  const lists = [
    ['alwaysHumanPaths', block?.alwaysHumanPaths],
    ...VERIFY_HUMAN_REVIEW_CATEGORIES.map((c) => [`humanReview.${c}`, review[c]]),
  ]
  const out = []
  for (const [list, entries] of lists)
    for (const entry of Array.isArray(entries) ? entries : [])
      if (isGlobEntry(entry) && typeof entry === 'object' && entry.label !== undefined)
        out.push({ list, glob: entry.glob, label: entry.label.trim() })
  return out
}

/** The PR labels the admission policy reads when `verifyDefaults.admission.labels` names none. */
export const VERIFY_ADMISSION_LABELS = Object.freeze({
  optIn: 'boss-verify',
  optOut: 'boss-no-verify',
  mergeOnPass: 'boss-merge-on-pass',
  repair: 'boss-repair',
})

/**
 * Which ticketless PRs verify admits, by author class, fully defaulted (`admit` false, `merge`
 * `"human"`, the VERIFY_ADMISSION_LABELS labels), or null when the block is absent, which keeps
 * today's admission. `fork` has no `merge`: a fork PR never merges on pass. Malformed values (a
 * config that skipped validation) take their defaults.
 * @returns {null | {human: {admit: boolean, merge: string}, bot: {admit: boolean, merge: string}, fork: {admit: boolean}, labels: {optIn: string, optOut: string, mergeOnPass: string, repair: string}}}
 */
export function verifyAdmissionPolicy(config) {
  assertStageBlockReadable(config, 'verifyDefaults')
  const block = config?.verifyDefaults?.admission
  if (!block || typeof block !== 'object' || Array.isArray(block)) return null
  const entry = (name) => (block[name] && typeof block[name] === 'object' ? block[name] : {})
  const merging = (name) => ({
    admit: entry(name).admit === true,
    merge: entry(name).merge === 'auto' ? 'auto' : 'human',
  })
  const label = (role) => {
    const value = entry('labels')[role]
    return typeof value === 'string' && value.trim() ? value.trim() : VERIFY_ADMISSION_LABELS[role]
  }
  const labels = Object.fromEntries(Object.keys(VERIFY_ADMISSION_LABELS).map((r) => [r, label(r)]))
  return {
    human: merging('human'),
    bot: merging('bot'),
    fork: { admit: entry('fork').admit === true },
    labels,
  }
}

/**
 * The verify policy from a config file's raw text, validating only its `verifyDefaults` block: the
 * verify stage reads policy from the base branch, where the rest of the config is irrelevant.
 * Throws on invalid JSON, a non-object document, or an invalid `verifyDefaults`.
 * `generatedPaths` globs the generated trees an approval may carry across (see verify-gate's
 * approval carry), and `generatedChecks` names the checks that prove them regenerated from source.
 * @returns {{review: import('./verify/verify-review-policy.mjs').ReviewPolicy, requireGreenBase: boolean, alwaysHumanPaths: string[], humanReview: {database: string[], security: string[], ui: string[]}, pathLabels: {list: string, glob: string, label: string}[], generatedPaths: string[], generatedChecks: string[], admission: ReturnType<typeof verifyAdmissionPolicy>}}
 */
export function verifyPolicyFromConfigText(text, source) {
  let parsed
  try {
    parsed = JSON.parse(text)
  } catch (e) {
    throw new Error(`skill-config: ${source} is not valid JSON: ${e.message}`)
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed))
    throw new Error(`skill-config: ${source} must contain a JSON object`)
  // Only verifyDefaults is validated: it is checked inside the default config, which is valid.
  if (parsed.verifyDefaults !== undefined) {
    const fault = schemaFaults({
      ...DEFAULT_CONFIG,
      verifyDefaults: parsed.verifyDefaults,
    }).errors.find((error) => error.block === 'verifyDefaults')
    if (fault) throw new Error(`skill-config: invalid config from ${source}: ${fault.message}`)
  }
  return {
    review: reviewPolicyFromBlock(parsed.verifyDefaults),
    requireGreenBase: parsed.verifyDefaults?.requireGreenBase === true,
    alwaysHumanPaths: verifyAlwaysHumanPaths(parsed),
    humanReview: verifyHumanReviewPaths(parsed),
    pathLabels: verifyPathLabels(parsed),
    generatedPaths: globList(parsed.verifyDefaults?.generatedPaths),
    generatedChecks: [...(parsed.verifyDefaults?.generatedChecks ?? [])],
    reviewers: [...(parsed.verifyDefaults?.reviewers ?? [])],
    admission: verifyAdmissionPolicy(parsed),
  }
}

/**
 * The policy globs that match none of `files`, as `{list, glob}` pairs (`list` is
 * `alwaysHumanPaths` or `humanReview.<category>`). A warning, never an error: a glob may name paths
 * a repo has not created yet.
 */
export function unmatchedPolicyGlobs(policy, files) {
  const lists = [
    ['alwaysHumanPaths', globList(policy?.alwaysHumanPaths)],
    ...VERIFY_HUMAN_REVIEW_CATEGORIES.map((c) => [
      `humanReview.${c}`,
      globList(policy?.humanReview?.[c]),
    ]),
  ]
  lists.push(
    ...Object.entries(policy?.alwaysReview ?? {}).map(([name, globs]) => [
      `alwaysReview.${name}`,
      globList(globs),
    ]),
    ['neverReview', globList(policy?.neverReview)],
  )
  const paths = Array.isArray(files) ? files : []
  const out = []
  for (const [list, globs] of lists)
    for (const glob of globs) {
      const re = globToRegExp(glob)
      if (!paths.some((path) => re.test(path))) out.push({ list, glob })
    }
  return out
}

export function retroConfig(config) {
  assertStageBlockReadable(config, 'retro')
  const block = config?.retro
  const out = {}
  for (const key of ['maxIssues', 'staleDays', 'minRuns', 'gateThreshold'])
    out[key] =
      Number.isSafeInteger(block?.[key]) &&
      block[key] >= (key === 'maxIssues' || key === 'staleDays' ? 0 : 1)
        ? block[key]
        : null
  out.pathAliases =
    block?.pathAliases && typeof block.pathAliases === 'object' && !Array.isArray(block.pathAliases)
      ? Object.fromEntries(
          Object.entries(block.pathAliases).filter(
            ([key, value]) => key.trim() && typeof value === 'string' && value.trim(),
          ),
        )
      : {}
  out.guidanceAudit =
    block?.guidanceAudit &&
    typeof block.guidanceAudit === 'object' &&
    !Array.isArray(block.guidanceAudit)
      ? structuredClone(block.guidanceAudit)
      : null
  return out
}

// Search options whose VALUE is the next token, so that token is never read as the pattern.
const LITERAL_SEARCH_VALUE_OPTIONS = new Set([
  '-A',
  '-B',
  '-C',
  '-g',
  '-m',
  '-t',
  '-T',
  '--context',
  '--glob',
  '--iglob',
  '--include',
  '--exclude',
  '--exclude-dir',
  '--max-count',
  '--type',
  '--type-not',
])
const REGEX_METACHARACTER = /[.*+?^${}()|[\]\\]/

/**
 * The literal needle of a `grep` / `rg` search, or null. A `-F` / `--fixed-strings` pattern is
 * always a literal. With `plainIsLiteral` (a verify-only criterion, which claims pre-existing text
 * is unchanged), a pattern carrying no regex metacharacter is one too: it matches exactly that text,
 * so the plan's own use of the same term cannot be told apart from the intended occurrence.
 */
export function fixedStringNeedle(command, { plainIsLiteral = false } = {}) {
  const tokens = tokenizeSimpleShell(command)
  const commandIndex = tokens.findIndex((token) => token === 'rg' || token === 'grep')
  if (commandIndex < 0) return null
  let fixed = false
  const literal = (pattern) => {
    if (pattern === null || pattern === undefined || pattern === '') return null
    if (fixed) return pattern
    return plainIsLiteral && !REGEX_METACHARACTER.test(pattern) ? pattern : null
  }
  for (let index = commandIndex + 1; index < tokens.length; index += 1) {
    const token = tokens[index]
    if (token === '--') return literal(tokens[index + 1])
    if (token === '-e' || token === '--regexp') {
      index += 1
      continue
    }
    if (token === '-F' || token === '--fixed-strings' || token === '--fixed-regexp') {
      fixed = true
      continue
    }
    if (/^-[A-Za-z]+$/.test(token) && token.includes('F')) {
      fixed = true
      continue
    }
    if (LITERAL_SEARCH_VALUE_OPTIONS.has(token)) {
      index += 1
      continue
    }
    if (token.startsWith('-')) continue
    return literal(token)
  }
  return null
}
