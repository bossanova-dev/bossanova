#!/usr/bin/env node

// skill-drift-verdict.mjs — the single consumer-side verdict on `boss skills check --gate`.
//
// The gate classifies every drifted path by KIND and DIRECTION and prints one
// `  - <path> (<kind>, <direction>)` row per path. The published cores that consume it used to
// throw that classification away: each collapsed every non-zero exit into one line asserting the
// drift was "bookkeeping only, work state unaffected". That claim is true of a stale-but-present
// file and false of a missing one. An absent helper is an ABSENT CAPABILITY — the run does not
// fail at the preflight that reported it, it fails later at the step that invokes the file by
// path, with a message about the step rather than about the install.
//
// So the rule this module implements is the recording-versus-capability split: a record of which
// payload a run read warns, and a capability that is not there blocks. It exists as one module,
// rather than as a `case` block per consumer, because three prose copies of a severity rule are
// three chances to disagree — and because the disagreement is invisible until the run that needed
// the missing file is already past its preflight.
//
// Scope limit (load-bearing, do not "fix"): the gate's stdout and its exit status are the ENTIRE
// input. This module runs no command, reads no file and reaches for no repository. It ships
// vendored inside globally installed cores, where there is no checkout to consult and where the
// gate has already performed the only comparison that can be performed. Everything it knows, it
// parses; anything it cannot parse, it refuses to call clean.
//
// Fail-closed in both directions. An unrecognised kind or direction token is `undecided`, and so
// is a non-zero gate whose output carries no parseable drift row at all — the shape that a
// failed comparison or an unevaluable tree produces. A verdict allowlist whose default arm is the
// benign one is not a default, it is a hole.
//
// Scope: the verdict is about the tree THIS RUN executes, not the whole installed tree. Rows under
// the running core, and under the sibling cores it loads in-run, decide; rows under any other core
// are still reported, but decide nothing. The running core is read from where this module sits —
// a vendored copy lives at `<skills>/<core>/toolbox/` — or given by `--core`. That location and the
// flag are the only inputs besides the gate's own output; no checkout is read. An unrecognised or
// unlocatable core falls back to whole-tree scope, which is the fail-closed direction.

import { readFileSync, realpathSync } from 'node:fs'
import { basename, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

// The direct-invocation predicate has exactly one definition repo-wide (enforced by
// main-module.test.mjs), so this module shares it rather than inlining a second copy — every
// skill that vendors this file already vendors main-module.mjs alongside it.
import { isMainModule } from './main-module.mjs'

// The five verdicts. `clean` and `advisory`/`advisory-withheld` continue; `undecided` and
// `blocking` stop.
export const SKILL_DRIFT_VERDICTS = Object.freeze({
  CLEAN: 'clean',
  ADVISORY: 'advisory',
  ADVISORY_WITHHELD: 'advisory-withheld',
  UNDECIDED: 'undecided',
  BLOCKING: 'blocking',
})

// The gate's own kind vocabulary (`DriftKind` in the skill-install library). Exactly five.
export const SKILL_DRIFT_KINDS = Object.freeze([
  'absent',
  'content',
  'mode',
  'unexpected',
  'broken-symlink',
])

// The gate's own direction vocabulary (`skillDriftDirection` in the boss CLI). Exactly four.
export const SKILL_DRIFT_DIRECTIONS = Object.freeze([
  'lossless',
  'behind',
  'unrecoverable',
  'unknown',
])

// Kinds on the absent-capability side: the file a later step invokes by path is not there
// (`absent`), is there but cannot be invoked directly (`mode`), or does not resolve
// (`broken-symlink`). Direction does not soften any of the three — a capability that is absent is
// absent however cheaply it could be restored.
const CAPABILITY_KINDS = new Set(['absent', 'mode', 'broken-symlink'])

// Directions for which the gate refuses to offer a reinstall, because it would overwrite installed
// bytes the checkout cannot restore. The tree still runs, so this stays on the advisory side: only
// the REMEDY is unsafe, and withholding the command while still warning is the decision the gate
// itself already made.
const WITHHELD_DIRECTIONS = new Set(['unrecoverable', 'unknown'])

// Ascending severity; the aggregate verdict of a mixed report is its most severe row. `blocking`
// outranks `undecided` so a report carrying both names the stop it can actually prove rather than
// the one row it could not read. Both are terminal, so the ordering changes the label and never
// the behaviour.
const SEVERITY = Object.freeze({
  clean: 0,
  advisory: 1,
  'advisory-withheld': 2,
  undecided: 3,
  blocking: 4,
})

// The advisory sentence is a contract, not a phrasing. Consumers and gates grep for it to prove
// the recording side is still reported as work-state-neutral, so it lives in one constant and is
// reachable from the advisory renders alone.
export const SKILL_DRIFT_ADVISORY_SENTENCE = 'bookkeeping only, work state unaffected'

// The cores each consumer loads during its own run, itself included. A core missing from this table
// — or no core at all — is judged on the whole installed tree, so a forgotten entry fails closed.
// A consumer that starts loading another sibling core must add it here.
export const SKILL_DRIFT_CORE_CLOSURE = Object.freeze({
  'boss-plan': Object.freeze(['boss-plan']),
  'boss-repair': Object.freeze(['boss-repair']),
  // boss-build invokes boss-review for its review stack and boss-finalize to ship.
  'boss-build': Object.freeze(['boss-build', 'boss-review', 'boss-finalize']),
})

// A published core directory name. A first path segment that is not one — the gate keys a missing
// install root on the namespace directory itself — belongs to no single core, so it stays in scope.
const CORE_NAME = /^boss(-[a-z0-9-]+)?$/

// The running core, read from this module's own location: a `toolbox/` directory whose parent is a
// published core. Symlinks are resolved first, because installed cores are reached through a
// top-level link into the namespaced payload. Anything else — the canonical source copy, a test
// fixture, an unreadable path — infers no core.
export function inferRunningCore(moduleUrl = import.meta.url) {
  let file
  try {
    file = fileURLToPath(moduleUrl)
  } catch {
    return null
  }
  try {
    file = realpathSync(file)
  } catch {
    // Keep the unresolved path; inference then reads the link's own location.
  }
  const toolbox = dirname(file)
  if (basename(toolbox) !== 'toolbox') return null
  const core = basename(dirname(toolbox))
  return core.startsWith('boss-') && CORE_NAME.test(core) ? core : null
}

// The set of cores in scope for the given core names, or null for whole-tree scope. Any name the
// closure table does not know widens to the whole tree rather than narrowing to nothing.
export function scopeForCores(cores = []) {
  const names = [...cores].filter((name) => name !== null && name !== undefined)
  if (names.length === 0) return null
  const scope = new Set()
  for (const name of names) {
    if (!Object.hasOwn(SKILL_DRIFT_CORE_CLOSURE, name)) return null
    for (const core of SKILL_DRIFT_CORE_CLOSURE[name]) scope.add(core)
  }
  return scope
}

// Whether a gate path belongs to the tree this run executes. The gate prints paths relative to the
// namespace, so the first segment is the core.
export function rowInScope(path, scope) {
  if (scope === null || scope === undefined) return true
  const head = String(path).split('/')[0]
  if (!CORE_NAME.test(head)) return true
  return scope.has(head)
}

// `  - <path> (<kind>, <direction>)`. The two-space indent is what separates a drift row from the
// gate's own headers (`boss skills gate: …`, which are flush left) and from the indented entries
// inside a withheld-remedy block (four spaces, and no `- `).
const DRIFT_ROW = /^ {2}- (.+?) \(([^(),]+), ([^(),]+)\)\s*$/

// `  run `<command>`` — optionally followed by the gate's unverified-direction note.
const RUN_LINE = /^ {2}run `(.+?)`/

// Either withheld lead: the destructive-overwrite refusal, and the undecided-comparison refusal.
const WITHHELD_LEAD = /^ {2}reinstall withheld\b/

// A usage error from a `boss` too old to know `--gate` is not a drift report. Real drift output
// provably never contains the literal flag (pinned on the producer side), so this stays a sound
// narrow probe. It is classified `undecided` rather than given a verdict of its own: the consumer
// arm that handles an old binary runs before this module is reached, so arriving here with this
// shape means the probe upstream did not fire, which is not something to call clean.
export function isUnsupportedFlagOutput(output = '') {
  return String(output).includes('--gate')
}

// The verdict for one classified path. Total over the gate's vocabulary, and closed against
// anything outside it.
export function verdictForDriftRow(kind, direction) {
  if (!SKILL_DRIFT_KINDS.includes(kind) || !SKILL_DRIFT_DIRECTIONS.includes(direction)) {
    return SKILL_DRIFT_VERDICTS.UNDECIDED
  }
  if (CAPABILITY_KINDS.has(kind)) return SKILL_DRIFT_VERDICTS.BLOCKING
  return WITHHELD_DIRECTIONS.has(direction)
    ? SKILL_DRIFT_VERDICTS.ADVISORY_WITHHELD
    : SKILL_DRIFT_VERDICTS.ADVISORY
}

// Split the gate's stdout into the drift rows and the remedy it offered. Lines the gate emits that
// are NOT drift rows — the per-agent `skill drift detected` headers, the `self-edited drift:` line,
// the `clean — compared N file(s)` coverage line, and the indented entries restated inside a
// withheld block — are all rejected by the row shape rather than by naming each of them, so a new
// header line the producer adds cannot be mistaken for a path.
export function parseSkillGateOutput(output = '') {
  const rows = []
  let command = null
  let withheld = false
  for (const line of String(output).split('\n')) {
    const row = DRIFT_ROW.exec(line)
    if (row) {
      rows.push({ path: row[1], kind: row[2], direction: row[3] })
      continue
    }
    if (line.startsWith('  - ')) {
      // A drift row in a shape this parser does not know. Kept as a row with unreadable tokens
      // rather than dropped: dropping it would let an unreadable report fall through to the
      // no-row arm, where it reads as a gate that enumerated nothing.
      rows.push({ path: line.slice(4).trim(), kind: null, direction: null })
      continue
    }
    if (WITHHELD_LEAD.test(line)) {
      withheld = true
      continue
    }
    const run = RUN_LINE.exec(line)
    if (run && command === null) command = run[1]
  }
  // A withheld lead anywhere suppresses the command everywhere. The gate reports per agent, and a
  // consumer that echoes one agent's safe command beside another agent's refusal invites running
  // it for both.
  if (withheld) command = null
  return { rows, remedy: { command, withheld } }
}

// An out-of-scope row can raise the aggregate no higher than this: it is reported, never decisive.
const OUT_OF_SCOPE_CEILING = SKILL_DRIFT_VERDICTS.ADVISORY_WITHHELD

// The verdict on one gate observation: its exit status and its captured output, plus the cores
// whose rows decide (`cores`; empty or absent means the whole tree).
export function classifySkillDrift({ exitStatus = 0, output = '', cores = [] } = {}) {
  const parsed = parseSkillGateOutput(output)
  const scope = scopeForCores(cores)
  const rows = parsed.rows.map((row) => {
    const verdict = verdictForDriftRow(row.kind, row.direction)
    return {
      ...row,
      verdict,
      // An unreadable row — unparseable, or parsed with a kind or direction outside the gate's
      // vocabulary — stays in scope whatever its path: what it names cannot be trusted, and the
      // out-of-scope ceiling would otherwise demote its `undecided` to advisory.
      inScope: verdict === SKILL_DRIFT_VERDICTS.UNDECIDED || rowInScope(row.path, scope),
    }
  })
  const base = {
    rows,
    scope: scope === null ? null : [...scope].sort(),
    outOfScope: rows.filter((row) => !row.inScope),
    kinds: [...new Set(rows.map((row) => row.kind).filter(Boolean))].sort(),
    directions: [...new Set(rows.map((row) => row.direction).filter(Boolean))].sort(),
    remedy: parsed.remedy,
  }
  const decided = (verdict, reason, evidence = []) => ({
    ...base,
    verdict,
    reason,
    evidence,
    blocking: SEVERITY[verdict] >= SEVERITY[SKILL_DRIFT_VERDICTS.UNDECIDED],
  })

  if (Number(exitStatus) === 0) return decided(SKILL_DRIFT_VERDICTS.CLEAN, 'gate-clean')
  if (isUnsupportedFlagOutput(output)) {
    return decided(SKILL_DRIFT_VERDICTS.UNDECIDED, 'unsupported-flag')
  }
  if (rows.length === 0) return decided(SKILL_DRIFT_VERDICTS.UNDECIDED, 'no-drift-row')

  const effective = (row) => {
    if (row.inScope) return row.verdict
    if (SEVERITY[row.verdict] <= SEVERITY[OUT_OF_SCOPE_CEILING]) return row.verdict
    // A capability row under a core this run never loads is reported, not a stop.
    return SKILL_DRIFT_VERDICTS.ADVISORY
  }
  let verdict = rows.reduce(
    (worst, row) => (SEVERITY[effective(row)] > SEVERITY[worst] ? effective(row) : worst),
    SKILL_DRIFT_VERDICTS.CLEAN,
  )
  // The gate can withhold its remedy on a report whose own rows all read as recoverable. Take it
  // at its word: it refused for a reason this module cannot see, so the command is suppressed and
  // the verdict says so.
  if (parsed.remedy.withheld && verdict === SKILL_DRIFT_VERDICTS.ADVISORY) {
    verdict = SKILL_DRIFT_VERDICTS.ADVISORY_WITHHELD
  }
  const matching = rows.filter((row) => row.inScope && row.verdict === verdict)
  const evidence = matching.length > 0 ? matching : rows
  const reason = {
    blocking: 'absent-capability',
    undecided: 'unreadable-drift-row',
    'advisory-withheld': 'remedy-withheld',
    advisory: 'stale-record',
  }[verdict]
  return decided(verdict, reason, evidence)
}

// The lines a consumer prints. The advisory renders carry the contract sentence; the blocking and
// undecided renders must not, because that sentence is the claim this whole module exists to stop
// making about an absent capability.
// Each drifted path once, however many agent trees reported it.
const uniqueLabels = (rows) => [
  ...new Set(rows.map((row) => `${row.path} (${row.kind}, ${row.direction})`)),
]

// Kinds whose stale copy is one the run reads or executes: the claim that the work state is
// unaffected is exactly what cannot be made about them when they are in scope.
const EXECUTED_STALE_KINDS = new Set(['content', 'unexpected'])

// The warning for capability rows under cores this run does not load. Reported, never dropped:
// they are real drift, just not this run's.
function outOfScopeCapabilityLines(result) {
  const rows = (result.outOfScope ?? []).filter((row) => CAPABILITY_KINDS.has(row.kind))
  if (rows.length === 0) return []
  const labels = uniqueLabels(rows)
  return [
    `warning: ${labels.length} installed path(s) are missing from cores this run does not load ` +
      `(${[...new Set(rows.map((row) => row.path.split('/')[0]))].sort().join(', ')}); they decide nothing ` +
      `here: ${labels.join(', ')}`,
  ]
}

export function renderSkillDriftVerdict(result) {
  const { verdict, remedy, evidence = [], reason, rows = [] } = result
  if (verdict === SKILL_DRIFT_VERDICTS.CLEAN) return []
  const executedStale = rows.filter((row) => row.inScope && EXECUTED_STALE_KINDS.has(row.kind))
  if (
    (verdict === SKILL_DRIFT_VERDICTS.ADVISORY ||
      verdict === SKILL_DRIFT_VERDICTS.ADVISORY_WITHHELD) &&
    executedStale.length > 0
  ) {
    // Not "work state unaffected": these are copies this run executes, and they lag the source
    // whose behaviour the run is expected to have.
    const labels = uniqueLabels(executedStale)
    const where =
      verdict === SKILL_DRIFT_VERDICTS.ADVISORY_WITHHELD
        ? 'the gate withheld its reinstall remedy, so there is no command to offer — read the gate output above for the next action'
        : remedy.command
          ? `run: ${remedy.command}`
          : 'see gate output above'
    return [
      `warning: installed boss skills drift from checkout source; this run executes ${labels.length} ` +
        `installed path(s) that lag checkout source, so its behaviour can differ from the checkout's: ` +
        `${labels.join(', ')} — ${where}`,
      ...outOfScopeCapabilityLines(result),
    ]
  }
  if (verdict === SKILL_DRIFT_VERDICTS.ADVISORY) {
    const where = remedy.command ? `run: ${remedy.command}` : 'see gate output above'
    return [
      `warning: installed boss skills drift from checkout source; ${where} — ${SKILL_DRIFT_ADVISORY_SENTENCE}`,
      ...outOfScopeCapabilityLines(result),
    ]
  }
  if (verdict === SKILL_DRIFT_VERDICTS.ADVISORY_WITHHELD) {
    // No command, by construction: the gate withheld it because running it would overwrite bytes
    // this checkout cannot restore, or because it could not decide whether it would.
    return [
      'warning: installed boss skills drift from checkout source; the gate withheld its reinstall ' +
        'remedy, so there is no command to offer — read the gate output above for the next action ' +
        `— ${SKILL_DRIFT_ADVISORY_SENTENCE}`,
      ...outOfScopeCapabilityLines(result),
    ]
  }
  if (verdict === SKILL_DRIFT_VERDICTS.UNDECIDED) {
    return [
      `BLOCKED: the installed-skill drift gate exited non-zero with no classifiable drift row (${reason}) —` +
        ' it did not evaluate the installed tree, so drift is UNDECIDED, not clean. Re-run the gate' +
        ' and read its own output before continuing.',
    ]
  }
  // The gate reports per agent tree, so the same path arrives once per installed agent. Name
  // each one once and carry both counts: a list that repeats a path reads as more distinct
  // missing files than there are, and a count that drops the duplicates understates the report.
  const labels = uniqueLabels(evidence)
  const lines = [
    `BLOCKED: installed boss skills are missing ${labels.length} capability file(s) a later step invokes by path` +
      `${evidence.length === labels.length ? '' : ` (${evidence.length} drift rows across the installed agent trees)`}: ${labels.join(', ')}`,
    '  this is an absent capability, not a stale record: the run does not fail here, it fails at' +
      ' the step that invokes one of these and reports it as that step failing.',
    remedy.command
      ? `  repair: ${remedy.command}`
      : '  repair: the gate withheld its reinstall command — move or delete the paths it named as' +
        ' unrecoverable first, then re-run it.',
  ]
  // Only when the gate's own command lacks the build step: a command that already rebuilds the
  // plugins needs no second instruction to do so.
  if (!remedy.command || !remedy.command.includes('build plugins')) {
    lines.push(
      '  the reinstall alone does not hold for a whole run: an agent plugin binary that still embeds' +
        ' the old payload restores it at daemon start, so rebuild the plugin binaries from the same' +
        ' source first (in a make-driven checkout: make build plugins).',
    )
  }
  return [...lines, ...outOfScopeCapabilityLines(result)]
}

function readStdin() {
  try {
    return readFileSync(0, 'utf8')
  } catch {
    // No stdin is not a clean tree. Returning empty here lands on the no-row arm, which is
    // `undecided`.
    return ''
  }
}

// The cores named by every `--core <name>`, or null when none was given. A `--core` with no value
// widens to the whole tree (an unknown name) rather than falling back to inference, which could
// only narrow it.
function coreFlags(argv) {
  const cores = []
  let given = false
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i] !== '--core') continue
    given = true
    const value = argv[i + 1]
    cores.push(value === undefined || value.startsWith('--') ? '' : value)
  }
  return given ? cores : null
}

export function main(argv, { stdin, stdout = process.stdout, moduleUrl = import.meta.url } = {}) {
  const command = argv[0]
  if (command !== 'classify') {
    stdout.write(
      `skill-drift-verdict: unknown command ${command ?? '(none)'}; expected: classify\n`,
    )
    return 2
  }
  // The CLI is called from a consumer's non-zero arm, so a missing `--status` means non-zero
  // rather than clean: defaulting the other way would turn a dropped flag into a silent pass.
  const flag = argv.indexOf('--status')
  const status = flag !== -1 && argv[flag + 1] !== undefined ? Number(argv[flag + 1]) : 1
  const result = classifySkillDrift({
    exitStatus: status,
    output: stdin !== undefined ? stdin : readStdin(),
    cores: coreFlags(argv) ?? [inferRunningCore(moduleUrl)],
  })
  for (const line of renderSkillDriftVerdict(result)) stdout.write(`${line}\n`)
  return result.blocking ? 1 : 0
}

if (isMainModule(import.meta.url)) {
  process.exit(main(process.argv.slice(2)))
}
