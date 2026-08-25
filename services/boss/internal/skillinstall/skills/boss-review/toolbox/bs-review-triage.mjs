// Deterministic triage helper: dedupes reviewer findings without losing the
// reviewer set, and promotes a group to must-fix on either a Critical/Warning
// severity or cross-reviewer convergence (>= CONVERGENCE_THRESHOLD distinct
// `lens` ids reporting the same (file, line, title)).
//
// This is the deterministic half of the must-fix-earns-its-status contract:
// a lone reviewer's Suggestion never promotes on its own, but two *distinct*
// reviewers independently reporting the same finding do — even if each is
// individually a Suggestion. Occurrence count is NOT reviewer count:
// the same lens reporting the same finding twice must never look like
// convergence (see the falsification test in bs-review-triage.test.mjs).
//
// Node built-ins only — cron worktrees are dependency-free.

import { existsSync, readdirSync, readFileSync, writeFileSync } from 'node:fs'
import { isAbsolute, join, resolve, relative } from 'node:path'
import { isMainModule } from './main-module.mjs'
import { validateResult } from './skill-extensions.mjs'

// The default cross-reviewer convergence threshold: two distinct reviewers
// reporting the same (file, line, title) is enough to promote it, even below
// Critical/Warning severity. Overridable per call via `convergenceThreshold`.
export const CONVERGENCE_THRESHOLD = 2
export const MONOCLASS_MIN_FINDINGS = 3
export const MONOCLASS_MAJORITY_THRESHOLD = 0.75

// Severity merges upward: Critical > Warning > Suggestion.
const SEVERITY_RANK = { Suggestion: 1, Warning: 2, Critical: 3 }
const PROSE_CLASS_EXTENSIONS = /\.(adoc|markdown|md|mdx|rst|txt)$/i

function isRepoRelativePath(file) {
  return !isAbsolute(file) && !relative('', file).startsWith('..')
}

function isInside(root, file) {
  const rel = relative(root, file)
  return rel === '' || (!rel.startsWith('..') && !isAbsolute(rel))
}

function countOccurrences(text, needle) {
  if (needle.length === 0) return 0
  let count = 0
  let index = 0
  while ((index = text.indexOf(needle, index)) !== -1) {
    count += 1
    index += 1
  }
  return count
}

function validatePatch(item, { repoRoot = process.cwd() } = {}) {
  const proseClass = PROSE_CLASS_EXTENSIONS.test(item.file)
  if (!Object.hasOwn(item, 'patch')) {
    return proseClass
      ? { reason: 'prose-class finding requires patch or patch:null with patchReason' }
      : { meta: { kind: 'narrative' } }
  }

  if (item.patch === null) {
    if (typeof item.patchReason !== 'string' || item.patchReason.trim() === '') {
      return { reason: 'patch:null requires non-empty patchReason' }
    }
    return { meta: { kind: 'null-with-reason', reason: item.patchReason } }
  }

  if (typeof item.patch !== 'object' || Array.isArray(item.patch)) {
    return { reason: 'patch must be an object or null' }
  }

  const { file, old_string: oldString, new_string: newString } = item.patch
  if (typeof file !== 'string' || file.trim() === '')
    return { reason: 'patch.file missing or blank' }
  if (!isRepoRelativePath(file)) return { reason: 'patch.file must be repo-root relative' }
  if (typeof oldString !== 'string' || oldString.length === 0) {
    return { reason: 'patch.old_string missing or empty' }
  }
  if (typeof newString !== 'string') return { reason: 'patch.new_string missing or non-string' }

  const root = resolve(repoRoot)
  const path = resolve(root, file)
  if (!isInside(root, path)) return { reason: 'patch.file escapes repo root' }
  if (!existsSync(path)) return { reason: `patch.file not found: ${file}` }

  const bytes = readFileSync(path, 'utf8')
  const matches = countOccurrences(bytes, oldString)
  if (matches !== 1) return { reason: `patch.old_string matched ${matches} times in ${file}` }
  const start = bytes.indexOf(oldString)
  return {
    meta: {
      kind: 'patch',
      patch: { file, old_string: oldString, new_string: newString },
      span: { file, start, end: start + oldString.length },
    },
  }
}

/**
 * Validate one findings item against the repo's reviewer findings contract.
 * Returns a human-readable reason string when invalid, or `null` when valid.
 * @param {unknown} item
 * @returns {string|null}
 */
function validateFinding(item, opts = {}) {
  if (item === null || typeof item !== 'object' || Array.isArray(item)) {
    return { reason: 'item is not an object' }
  }
  const { severity, file, line, title, detail, lens } = item
  if (typeof file !== 'string' || file.trim() === '') return { reason: 'missing or blank file' }
  if (typeof title !== 'string' || title.trim() === '') return { reason: 'missing or blank title' }
  // Deliberately NOT `.trim() === ''` like `file`/`title` above: a blank detail
  // on one occurrence is legal because a duplicate occurrence may supply the
  // real one, and only the merge in `triageFindings` knows whether any did.
  // The all-blank verdict is made there, per group, once every occurrence is in.
  if (typeof detail !== 'string') return { reason: 'missing or non-string detail' }
  if (!Object.hasOwn(SEVERITY_RANK, severity))
    return { reason: `unknown severity: ${String(severity)}` }
  if (line !== null && !Number.isInteger(line)) return { reason: 'line must be an integer or null' }
  if (typeof lens !== 'string' || lens.trim() === '') return { reason: 'missing or blank lens' }
  return validatePatch(item, opts)
}

function invalidReason(item) {
  return validateFinding(item).reason ?? null
}

function findingCategory(item) {
  return typeof item?.category === 'string' ? item.category.trim() : ''
}

/**
 * Classify the shape of one round's findings. A monoclass round needs a
 * machine-readable optional `category` on every finding, enough findings to be
 * a pattern, and either unanimity or a conservative supermajority.
 *
 * @param {unknown} findings
 * @param {{minFindings?: number, majorityThreshold?: number}} [opts]
 * @returns {{monoclass: boolean, category: string|null, findings: object[], reason: string, total: number, count: number}}
 */
export function classifyMonoclassRound(
  findings,
  { minFindings = MONOCLASS_MIN_FINDINGS, majorityThreshold = MONOCLASS_MAJORITY_THRESHOLD } = {},
) {
  if (!Array.isArray(findings)) {
    return {
      monoclass: false,
      category: null,
      findings: [],
      reason: 'findings is not an array',
      total: 0,
      count: 0,
    }
  }

  const total = findings.length
  if (total < minFindings) {
    return {
      monoclass: false,
      category: null,
      findings: [],
      reason: `below minimum finding floor (${total}/${minFindings})`,
      total,
      count: 0,
    }
  }

  const missing = findings.some((item) => findingCategory(item) === '')
  if (missing) {
    return {
      monoclass: false,
      category: null,
      findings: [],
      reason: 'one or more findings are missing category',
      total,
      count: 0,
    }
  }

  const byCategory = new Map()
  for (const item of findings) {
    const category = findingCategory(item)
    if (!byCategory.has(category)) byCategory.set(category, [])
    byCategory.get(category).push(item)
  }

  const [category, members] = [...byCategory.entries()].sort((a, b) => b[1].length - a[1].length)[0]
  const count = members.length
  if (count === total || (count >= minFindings && count / total >= majorityThreshold)) {
    return {
      monoclass: true,
      category,
      findings: members,
      reason: count === total ? 'unanimous' : 'majority-threshold',
      total,
      count,
    }
  }

  return {
    monoclass: false,
    category: null,
    findings: [],
    reason: `mixed categories (${count}/${total} below threshold ${majorityThreshold})`,
    total,
    count,
  }
}

/**
 * Append one derived within-run observation without mutating earlier entries.
 * The caller owns synthesis; this helper owns the run-scoped list shape.
 */
export function appendCarriedObservation(observations = [], observation = {}) {
  const prior = Array.isArray(observations) ? observations : []
  const round = Number(observation.round)
  const category = typeof observation.category === 'string' ? observation.category.trim() : ''
  const paragraph = typeof observation.paragraph === 'string' ? observation.paragraph.trim() : ''
  if (!Number.isInteger(round) || round < 1) {
    throw new Error('carried observation needs a positive integer round')
  }
  if (!category) throw new Error('carried observation needs a category')
  if (!paragraph) throw new Error('carried observation needs a paragraph')
  return [...prior, { round, category, paragraph }]
}

function composeCluster(bytes, records) {
  const min = Math.min(...records.map((r) => r.patchMeta.span.start))
  const max = Math.max(...records.map((r) => r.patchMeta.span.end))
  const chars = [...bytes.slice(min, max)]

  for (const record of records) {
    const { patch, span } = record.patchMeta
    if (patch.old_string.length !== patch.new_string.length) {
      return { conflict: 'overlapping patches with changed lengths cannot be composed safely' }
    }
    const offset = span.start - min
    for (let i = 0; i < patch.new_string.length; i += 1) {
      const next = patch.new_string[i]
      const current = chars[offset + i]
      if (current !== patch.old_string[i] && current !== next) {
        return { conflict: 'overlapping patches write conflicting replacement bytes' }
      }
      chars[offset + i] = next
    }
  }

  return {
    patch: {
      file: records[0].patchMeta.patch.file,
      old_string: bytes.slice(min, max),
      new_string: chars.join(''),
    },
  }
}

function composePatchPlan(records, { repoRoot = process.cwd() } = {}) {
  const patchRecords = records.filter((record) => record.patchMeta?.kind === 'patch')
  const byFile = new Map()
  for (const record of patchRecords) {
    const file = record.patchMeta.patch.file
    if (!byFile.has(file)) byFile.set(file, [])
    byFile.get(file).push(record)
  }

  const items = []
  const conflicts = []
  for (const [file, fileRecords] of byFile) {
    const sorted = [...fileRecords].sort((a, b) => a.patchMeta.span.start - b.patchMeta.span.start)
    const bytes = readFileSync(resolve(repoRoot, file), 'utf8')
    let cluster = []
    let clusterEnd = -1
    const flush = () => {
      if (!cluster.length) return
      if (cluster.length === 1) {
        items.push(cluster[0].patchMeta.patch)
      } else {
        const composed = composeCluster(bytes, cluster)
        if (composed.conflict) {
          conflicts.push({
            item: cluster.map((record) => record.record),
            reason: `${file}: ${composed.conflict}`,
          })
        } else {
          items.push(composed.patch)
        }
      }
      cluster = []
      clusterEnd = -1
    }

    for (const record of sorted) {
      const { start, end } = record.patchMeta.span
      if (!cluster.length || start < clusterEnd) {
        cluster.push(record)
        clusterEnd = Math.max(clusterEnd, end)
      } else {
        flush()
        cluster.push(record)
        clusterEnd = end
      }
    }
    flush()
  }

  return { items, conflicts }
}

/** The dedupe key: (file, line, title). A null line never collapses into a numbered one. */
function keyFor(item) {
  return JSON.stringify([item.file, item.line, item.title])
}

/**
 * Return the reviewer identity selected by the dispatch that owns a findings
 * file. Bare-array reviewers are model output, so their per-item `lens` is
 * untrusted: one reviewer must not turn a repeated finding into convergence
 * merely by spelling that field differently on each occurrence.
 *
 * @returns {{lens: string}|{reason: string}}
 */
function reviewerForFile(name, lensEntries) {
  const indexedLens = /^findings-lens-(\d+)-/.exec(name)
  if (indexedLens) {
    const entry = lensEntries?.[Number(indexedLens[1])]
    if (typeof entry?.skill !== 'string' || entry.skill.trim() === '') {
      return { reason: `${name}: no configured lens skill for its entry index` }
    }
    return { lens: entry.skill }
  }

  // Tier 2 lens outputs use the same entry-indexed filename shape as Tier 1,
  // so the branch above gives duplicate configured skills distinct files while
  // retaining their configured identity. Tier 2/3 round fallbacks are also one
  // file per selected dispatch. Extension envelopes below retain their
  // validated extension identity instead.
  const dispatch = /^findings-(.+)\.json$/.exec(name)
  return dispatch ? { lens: dispatch[1] } : { reason: `${name}: unknown reviewer output filename` }
}

// This private marker keeps the filename and configured reviewer attached while
// a bare-array item travels to triage. It cannot collide with reviewer JSON.
const FINDING_SOURCE = Symbol('finding source')

function withReviewerLens(items, lens, filename) {
  const source = { filename, reviewer: lens }
  return items.map((item) => ({
    [FINDING_SOURCE]: source,
    item:
      item !== null && typeof item === 'object' && !Array.isArray(item) ? { ...item, lens } : item,
  }))
}

/**
 * Dedupe and triage a flat array of reviewer findings.
 * @param {unknown} items
 * @param {{convergenceThreshold?: number}} [opts]
 * @returns {{mustFix: object[], pool: object[], invalid: {item: unknown, reason: string}[]}}
 */
export function triageFindings(
  items,
  { convergenceThreshold = CONVERGENCE_THRESHOLD, repoRoot = process.cwd() } = {},
) {
  if (!Array.isArray(items)) {
    return {
      mustFix: [],
      pool: [],
      invalid: [{ item: items, reason: 'items is not an array' }],
      patchSummary: { patchable: 0, narrative: 0, nullWithReason: 0 },
      patchPlan: { items: [] },
    }
  }

  const invalid = []
  const groups = new Map() // dedupe key -> accumulator

  for (const candidate of items) {
    const source = candidate?.[FINDING_SOURCE]
    const item = source ? candidate.item : candidate
    const validation = validateFinding(item, { repoRoot })
    if (validation.reason) {
      invalid.push(
        source ? { item, reason: validation.reason, source } : { item, reason: validation.reason },
      )
      continue
    }
    const patchMeta = validation.meta

    const key = keyFor(item)
    let group = groups.get(key)
    if (!group) {
      group = {
        severity: item.severity,
        file: item.file,
        line: item.line,
        title: item.title,
        // Ambiguity resolution (per task brief): the merged `detail` takes the
        // first non-BLANK `detail` seen across the group's items, in
        // encounter order. A later, possibly more detailed, write never
        // overwrites an already-chosen non-blank detail. "Blank" is trimmed,
        // not merely empty: a whitespace-only detail explains nothing, so
        // letting it count as chosen would block the real detail behind it.
        detail: item.detail,
        lenses: [],
        lensSeen: new Set(),
        // Every contributing occurrence, with the output file it came from.
        // `lenses` cannot stand in for this: it holds the CONFIGURED lens
        // identity, and several output files can share one configured lens
        // (see `reviewerForFile`), so it names no file to repair.
        occurrences: [],
      }
      groups.set(key, group)
    }
    group.occurrences.push({ item, source, patchMeta })

    if (SEVERITY_RANK[item.severity] > SEVERITY_RANK[group.severity]) {
      group.severity = item.severity
    }
    if (group.detail.trim() === '' && item.detail.trim() !== '') {
      group.detail = item.detail
    }
    if (!group.lensSeen.has(item.lens)) {
      group.lensSeen.add(item.lens)
      group.lenses.push(item.lens)
    }
  }

  const mustFix = []
  const pool = []
  for (const group of groups.values()) {
    // A finding is a location PLUS an explanation. `invalidReason` cannot judge
    // a blank `detail` on its own item, because the merge above is allowed to
    // rescue one from a duplicate — so the verdict has to wait until every
    // occurrence has been merged. Once it has, an all-blank detail means no
    // occurrence ever explained the defect: promoting that leaves the Phase 6
    // fixer a location and an empty requested change, which it cannot use to
    // adjudicate the premise. Report it as malformed instead, so the owning
    // reviewer is repaired rather than a hollow item being acted on.
    //
    // This applies to the pool as much as to must-fix: an unexplained
    // suggestion is equally unusable as follow-up evidence, and severity is not
    // what makes a finding legible.
    //
    // The verdict is reached per group, but it is REPORTED per occurrence, in
    // the same `{item, reason, source}` shape the item-level rejection above
    // emits. Every occurrence is a reviewer output that owes an explanation, and
    // only its own `source` names the file to repair — the merged group's
    // `lenses` holds configured identities, which several files can share. A
    // single group-shaped entry would leave the invalid-only repair path unable
    // to tell which output to replace, so it could re-run forever and cap.
    if (group.detail.trim() === '') {
      const reason = 'blank detail on every occurrence of this finding'
      for (const { item, source } of group.occurrences) {
        invalid.push(source ? { item, reason, source } : { item, reason })
      }
      continue
    }

    const reviewerCount = group.lenses.length
    let promotedBy = null
    if (group.severity === 'Critical' || group.severity === 'Warning') {
      promotedBy = 'severity'
    } else if (reviewerCount >= convergenceThreshold) {
      promotedBy = 'convergence'
    }
    const patchMeta = group.occurrences.find(
      (occurrence) => occurrence.patchMeta?.kind === 'patch',
    )?.patchMeta
    const nullWithReason = group.occurrences.find(
      (occurrence) => occurrence.patchMeta?.kind === 'null-with-reason',
    )?.patchMeta
    const record = {
      severity: group.severity,
      file: group.file,
      line: group.line,
      title: group.title,
      detail: group.detail,
      lenses: group.lenses,
      reviewerCount,
      promotedBy,
    }
    if (patchMeta) record.patch = patchMeta.patch
    if (nullWithReason) {
      record.patch = null
      record.patchReason = nullWithReason.reason
    }
    const planRecord = { record, patchMeta }
    ;(promotedBy ? mustFix : pool).push(record)
    record.__patchPlanRecord = planRecord
  }

  const planRecords = mustFix.map((record) => record.__patchPlanRecord).filter(Boolean)
  const patchPlan = composePatchPlan(planRecords, { repoRoot })
  invalid.push(...patchPlan.conflicts)
  for (const record of [...mustFix, ...pool]) delete record.__patchPlanRecord
  const patchSummary = {
    patchable: mustFix.filter((record) => record.patch && typeof record.patch === 'object').length,
    narrative: mustFix.filter((record) => !Object.hasOwn(record, 'patch')).length,
    nullWithReason: mustFix.filter((record) => record.patch === null).length,
  }

  return { mustFix, pool, invalid, patchSummary, patchPlan: { items: patchPlan.items } }
}

/**
 * Read and concatenate every `findings-*.json` file directly under `dir`.
 *
 * One unusable file never costs the others. A file whose top level is not an
 * array holds no items we can triage — but it is NOT empty, so dropping it
 * would report "no findings" for a reviewer that actually returned some (an
 * object wrapper like `{"findings": [...]}` parses fine). A file that does not
 * parse at all (truncated output is a routine way for a reviewer to fail) is
 * the same loss, and aborting the directory over it would additionally discard
 * every other reviewer's findings.
 *
 * Both are reported per file into `invalid`, named, so the round can be
 * repaired or re-run rather than mistaking a lost reviewer for a clean one —
 * except for a file the roster supersedes, which is a ledger skip (see
 * `rejectFile`). Only a failure to list the directory itself is fatal.
 *
 * @returns {{items: unknown[], invalid: {item: unknown, reason: string}[]}}
 */
function readFindingsDir(dir, { lensEntries = null, expectedOutputs = null } = {}) {
  const files = readdirSync(dir)
    .filter((name) => /^findings-.*\.json$/.test(name))
    .sort()
  const items = []
  const invalid = []
  const present = new Set(files)
  for (const output of expectedOutputs ?? []) {
    if (!present.has(output)) {
      invalid.push({ item: null, reason: `${output}: expected reviewer output is missing` })
    }
  }

  // The roster is the authority on which dispatches this round actually
  // selected. A file absent from it belongs to a dispatch a later fallback
  // replaced, so neither its failure nor its findings are this round's evidence.
  //
  // With no roster at all (`expectedOutputs === null`) nothing is superseded.
  const isSuperseded = (name) => expectedOutputs !== null && !expectedOutputs.includes(name)

  // Every file-level rejection goes through here so the non-rostered skip
  // cannot be forgotten at one of them.
  //
  // Tier 1 extension failures are ledger skips, not unread reviewer evidence.
  // Only the Tier 2/3 fallback selected after every Tier 1 descriptor has
  // settled is registered in `expectedOutputs`; once it succeeds, the
  // superseded Tier 1 attempt must not prevent a clean verdict -- and HOW that
  // attempt failed does not change whose evidence it is. A truncated write, a
  // non-envelope top level and an unrecognised filename are all the same
  // superseded file, so gating only the envelope-validation failure would let
  // the other shapes cap a run that already holds valid fallback output.
  const rejectFile = (name, item, reason) => {
    if (isSuperseded(name)) return
    invalid.push({ item, reason })
  }

  for (const name of files) {
    let parsed
    try {
      parsed = JSON.parse(readFileSync(join(dir, name), 'utf8'))
    } catch (err) {
      // JSON.parse messages carry no filename, so name it here.
      rejectFile(name, null, `${name}: ${err.message}`)
      continue
    }
    if (Array.isArray(parsed)) {
      // A superseded file's FINDINGS are skipped too, not just its failures.
      // A Tier 1 extension is contractually required to emit an envelope, so a
      // bare array under an indexed lens filename means that extension broke
      // its contract: Phase 1 records it skipped and runs a fallback. But the
      // filename still resolves to a configured lens, so without this the
      // rejected extension's items would be consumed alongside the fallback's
      // and its Critical/Warning entries would drive fixes the ledger says were
      // never collected. Every legitimate bare-array producer (the selected
      // Tier 2/3 fallbacks) is rostered, and a successful Tier 1 extension is
      // an ENVELOPE, handled below — so nothing legitimate is dropped here.
      if (isSuperseded(name)) continue
      const reviewer = reviewerForFile(name, lensEntries)
      if ('reason' in reviewer) {
        rejectFile(name, parsed, reviewer.reason)
        continue
      }
      items.push(...withReviewerLens(parsed, reviewer.lens, name))
    } else if (
      parsed !== null &&
      typeof parsed === 'object' &&
      ('ok' in parsed || 'extension' in parsed || 'role' in parsed || 'items' in parsed)
    ) {
      // Extension envelopes are accepted only after contract validation. The
      // filename selects the expected role; trusting the envelope's own role
      // would let a round result masquerade as a lens result (or vice versa).
      const expectedRole = name.startsWith('findings-round-')
        ? 'round'
        : name.startsWith('findings-lens-')
          ? 'lens'
          : null
      if (expectedRole === null) {
        rejectFile(
          name,
          parsed,
          `${name}: extension envelope filename does not identify a reviewer role`,
        )
        continue
      }
      const validation = validateResult(parsed, expectedRole)
      if (!validation.ok) {
        rejectFile(
          name,
          parsed,
          `${name}: invalid ${expectedRole} extension envelope: ${validation.errors.join('; ')}`,
        )
        continue
      }
      // Phase R and lens extensions omit per-item `lens`, so identity comes
      // from the file — and from the FILENAME, never from the envelope's own
      // `extension` field. The orchestrator writes one file per dispatch
      // (`findings-round-<extension-name>.json`), so the filename is unique by
      // construction; `extension` is content the extension writes about itself,
      // and `validateResult` only checks it is non-empty. Two round extensions
      // declaring the same name — one copying another's envelope example — would
      // otherwise collapse into a single reviewer, and their agreeing
      // Suggestions would never reach the convergence threshold.
      //
      // `reviewerForFile` resolves both roles: an indexed lens file maps to its
      // configured LENSES_JSON skill (several descriptors can legitimately serve
      // one configured lens, so their extension names would falsely INFLATE
      // convergence), and any other dispatch file maps to its own filename slug.
      const reviewer = reviewerForFile(name, lensEntries)
      if ('reason' in reviewer) {
        rejectFile(name, parsed, reviewer.reason)
        continue
      }
      items.push(...withReviewerLens(parsed.items, reviewer.lens, name))
    } else {
      rejectFile(name, parsed, `${name}: top level is not a list of findings`)
    }
  }
  return { items, invalid }
}

function readJsonFile(path, label) {
  try {
    return JSON.parse(readFileSync(path, 'utf8'))
  } catch (err) {
    throw new Error(`failed to read ${label} ${path}: ${err.message}`)
  }
}

function readStringArrayFile(path, label) {
  const values = readJsonFile(path, label)
  if (
    !Array.isArray(values) ||
    values.some((value) => typeof value !== 'string' || value.length === 0)
  ) {
    throw new Error(`${label} must be a JSON array of non-empty strings`)
  }
  return values
}

function addExpectedOutput(path, output) {
  if (!/^findings-[^/]+\.json$/.test(output)) {
    throw new Error('expected output must be a findings-*.json filename')
  }
  const outputs = readStringArrayFile(path, 'expected outputs')
  if (!outputs.includes(output)) outputs.push(output)
  writeFileSync(path, `${JSON.stringify(outputs)}\n`)
}

// Thin CLI (the surface the skill prose invokes):
//   node bs-review-triage.mjs expect <expectedOutputsFile> <findingsFile>
//   node bs-review-triage.mjs categorize <dir> [--lens-entries-file <path>] [--expected-outputs-file <path>]
if (isMainModule(import.meta.url)) {
  const [cmd, ...args] = process.argv.slice(2)
  if (cmd === 'expect' && args.length === 2) {
    try {
      addExpectedOutput(args[0], args[1])
    } catch (err) {
      process.stderr.write(
        `bs-review-triage.mjs: failed to register expected output: ${err.message}\n`,
      )
      process.exit(2)
    }
    process.exit(0)
  }

  const [dir, ...options] = args
  if (cmd === 'categorize' && typeof dir === 'string' && dir.length > 0) {
    let read
    try {
      let lensEntries = null
      let expectedOutputs = null
      while (options.length > 0) {
        const option = options.shift()
        const path = options.shift()
        if (typeof path !== 'string') throw new Error(`missing path after ${option}`)
        if (option === '--lens-entries-file') {
          lensEntries = readJsonFile(path, 'lens entries')
          if (!Array.isArray(lensEntries)) throw new Error('lens entries must be a JSON array')
        } else if (option === '--expected-outputs-file') {
          expectedOutputs = readStringArrayFile(path, 'expected outputs')
        } else {
          throw new Error(`unknown option: ${option}`)
        }
      }
      read = readFindingsDir(dir, { lensEntries, expectedOutputs })
    } catch (err) {
      process.stderr.write(`bs-review-triage.mjs: failed to read ${dir}: ${err.message}\n`)
      process.exit(2)
    }
    const result = triageFindings(read.items)
    // File-level rejections lead: a whole reviewer's output going unread is
    // worse news than one malformed item, and must not be paged past.
    result.invalid.unshift(...read.invalid)
    process.stdout.write(`${JSON.stringify(result)}\n`)
  } else {
    process.stderr.write(
      'usage: bs-review-triage.mjs expect <expectedOutputsFile> <findingsFile>\\n' +
        '   or: bs-review-triage.mjs categorize <dir> [--lens-entries-file <path>] [--expected-outputs-file <path>]\\n',
    )
    process.exit(2)
  }
}
