// bs-mutation-obligations.mjs — the shape a fix has decides which mutants its
// non-vacuity proof owes, and this module owns that mapping.
//
// THE DEFECT THIS EXISTS FOR. The repository's non-vacuity contract told an agent
// to run ONE mutation and require ONE red. For a `replacement` — one condition
// swapped for another — a single red is sufficient, and that is the case the
// contract was written against. For every other shape a single red is ambiguous
// between "the new guard is load-bearing" and something strictly weaker:
//
//   a WIDENED condition reds in the half that was already covered;
//   a COMPOUND guard reds when only one of its clauses matters;
//   a NEW GATE reds without proving the old gate missed anything;
//   an ASSERTION OVER TEXT reds against the old defect while over-rejecting
//     something new.
//
// The contract never named which extra mutants each shape owes, so an agent
// reading the unqualified instruction ran the first one and stopped.
//
// GREEN-EXPECTED MUTANTS ARE THE POINT. The mutant an author never writes
// unprompted is the one whose required verdict is GREEN: the pre-existing sibling
// test that must stay green under a widening revert, the PRE-fix gate that must
// stay green on the violation a new gate claims to catch, the legitimately-skipped
// input that must take a different branch. Those separate "this guard fires" from
// "this guard fires for the reason claimed". They are ordinary obligations here,
// adjudicated exactly as a red one is.
//
// WHAT IS IN SCOPE. This module maps a shape to its obliged mutant set and
// adjudicates a RECORDED proof against that set. It does not run mutants, does not
// choose a gate, and does not judge whether an observed verdict was honestly
// recorded. Producing one mutant's observed verdict is a runner's job; this is the
// adjudicator, and it consumes records regardless of which runner produced them.
//
// FAIL CLOSED. A missing mutant, a relabelled required verdict, an observed verdict
// that contradicts the required one, and an unreadable or absent record are each a
// non-discharge. An operator error is never a verdict.
//
// Node built-ins only — cron worktrees are dependency-free.

import { readFileSync } from 'node:fs'
import { isMainModule } from './main-module.mjs'

/** The closed verdict vocabulary a single mutant can be required to produce, or observed at. */
export const MUTANT_VERDICTS = Object.freeze(['red', 'green'])

/**
 * The closed set of ways a mutant's verdict may be reached. `test-first-red` is an
 * EQUAL discharge, not a lesser one: writing the test before the production change
 * and showing it red for a named reason reaches the same guarantee as a sandboxed
 * probe, whenever the pass controls edit ordering. The contract states both as
 * alternatives rather than mandating the sandbox.
 */
export const DISCHARGE_MECHANISMS = Object.freeze(['probe', 'test-first-red'])

/**
 * The closed adjudication vocabulary, and the action each obliges at every call site:
 *
 *   satisfied     the record ran the set its shape obliges and every mutant produced
 *                 the verdict that shape assigns it — the proof discharges non-vacuity
 *   unclassified  the record used the 'other' escape hatch: it obliged no mutant set, so
 *                 NOTHING was mutated and nothing was proven. The justification is well
 *                 formed and echoed back, but this is a recorded admission, not a proof.
 *                 It is deliberately NOT spelled as a prefix of `satisfied`, because a
 *                 caller grepping `^satisfied` must not read it as one, and the CLI exits
 *                 non-zero on it so a caller proceeding on exit status alone cannot either.
 *                 Clearing a gate on it takes an explicit, recorded human override.
 *   insufficient  a mutant is missing, a verdict contradicts its obligation, or a skip
 *                 is unjustified — the proof does NOT discharge, and no caller may
 *                 proceed on it
 */
export const ADJUDICATIONS = Object.freeze(['satisfied', 'unclassified', 'insufficient'])

/**
 * The floor a written `shapeJustification` must clear. `other` is the module's one
 * fail-open surface: before this, any non-empty string discharged it, so `"x"` and
 * `"n/a"` bought the same exit 0 as a fully discharged three-mutant proof. A length
 * floor plus a named classified shape does not make the hatch honest — nothing can —
 * but it costs more than a keystroke and forces the author to engage the enumeration.
 */
export const MIN_SHAPE_JUSTIFICATION_CHARS = 80

/**
 * Bare source-language names, which are exactly what an "impossible input" skip
 * justification reaches for when it is wrong.
 *
 * The incident: a skip was justified by the validity rules of the language the file
 * is nominally WRITTEN in, when the layer that actually consumes the input is a text
 * scanner — one that reads the file with `readFileSync` and splits on newlines, and
 * so never compiles that language at all. The source language's validity rules gate
 * nothing there, so the input it calls impossible reaches the guard unimpeded.
 *
 * A skip therefore names the consuming layer, and a consuming layer that is only one
 * of these tokens has named the language instead of the layer.
 */
export const SOURCE_LANGUAGE_TOKENS = Object.freeze([
  'bash',
  'c',
  'c++',
  'cjs',
  'css',
  'go',
  'golang',
  'html',
  'java',
  'javascript',
  'js',
  'json',
  'jsx',
  'kotlin',
  'markdown',
  'md',
  'mjs',
  'php',
  'proto',
  'protobuf',
  'python',
  'ruby',
  'rust',
  'scss',
  'sh',
  'shell',
  'sql',
  'swift',
  'toml',
  'ts',
  'tsx',
  'typescript',
  'yaml',
  'yml',
  'zsh',
])

/**
 * The enumeration. Each shape names its trigger and the mutants it owes; each mutant
 * names the verdict the SHAPE REQUIRES — not the verdict an author expects.
 *
 * `justification: true` on a mutant means the observed verdict alone does not settle
 * it: the record must also carry the argument the mutant exists to force into the
 * open (why a newly-rejected input deserves rejection, what is legitimately in a
 * dropped set, at what count a suggested alternative fails).
 *
 * Every row is traceable to a recorded incident; the incident is the `why`.
 */
export const MUTATION_SHAPES = Object.freeze({
  replacement: Object.freeze({
    trigger: 'one condition or literal swapped for another',
    why: 'The single-red case the old contract was written against; one mutant genuinely settles it.',
    mutants: Object.freeze([
      Object.freeze({
        id: 'revert-pre-fix-text',
        expected: 'red',
        description: 'restore the pre-fix condition or literal and require the new test to fail',
      }),
    ]),
  }),
  widening: Object.freeze({
    trigger: 'a clause added to a disjunction, or a rule loosened',
    why: 'A red can land in the half that was ALREADY covered and prove nothing about the added clause.',
    mutants: Object.freeze([
      Object.freeze({
        id: 'revert-added-clause',
        expected: 'red',
        description: 'revert ONLY the added clause and require the new test to fail',
      }),
      Object.freeze({
        id: 'sibling-under-same-mutation',
        expected: 'green',
        description:
          'run a pre-existing sibling test under that same mutation and require it to stay green, so the red is attributable to the added clause rather than to the whole condition',
      }),
    ]),
  }),
  tightening: Object.freeze({
    trigger: 'a clause added to a conjunction, or a rule narrowed',
    why: 'A tightening carries the OPPOSITE risk to a loosening — over-rejection — and that is the side nobody tests. An arm added to reject a non-positive value also rejected the zero-padded positive values the surrounding helper accepts; the round that wrote it saw green.',
    mutants: Object.freeze([
      Object.freeze({
        id: 'revert-narrowing',
        expected: 'red',
        description: 'revert the narrowing and require the new test to fail',
      }),
      Object.freeze({
        id: 'newly-rejected-input',
        expected: 'red',
        justification: true,
        description:
          'feed an input the narrowed form NEWLY rejects, require the guard to reject it, and record why that input deserves rejection — the boundary that moved, not the case that still passes',
      }),
    ]),
  }),
  'compound-guard': Object.freeze({
    trigger: 'a guard of the form `A && B` or `A || B`',
    why: 'Removing the whole guard reds even when only one clause is load-bearing, so the other clause can be inert or over-firing and the proof still looks complete.',
    mutants: Object.freeze([
      Object.freeze({
        id: 'remove-whole-guard',
        expected: 'red',
        description: 'remove the entire guard and require the test to fail',
      }),
      Object.freeze({
        id: 'drop-clause-a',
        expected: 'red',
        description: 'drop clause A alone and require the test to fail',
      }),
      Object.freeze({
        id: 'drop-clause-b',
        expected: 'red',
        description: 'drop clause B alone and require the test to fail',
      }),
    ]),
  }),
  'new-gate': Object.freeze({
    trigger: 'a gate added, or replacing another, claiming coverage the old one lacked',
    why: 'Mutating a new gate proves it fires. It does not prove it covers anything the old gate missed; only running the PRE-fix gate against the same violation proves that.',
    mutants: Object.freeze([
      Object.freeze({
        id: 'new-gate-on-violation',
        expected: 'red',
        description: 'run the NEW gate on the violation it claims to catch and require it to fail',
      }),
      Object.freeze({
        id: 'pre-fix-gate-on-violation',
        expected: 'green',
        description:
          'run the PRE-fix gate on that same violation and require it to stay green — the coverage claim is the gap between the two, and a pre-fix gate that also reds means the new gate added none',
      }),
    ]),
  }),
  'text-assertion': Object.freeze({
    trigger: 'an assertion over source or prose text',
    why: 'Mutant-testing an assertion against the PRIOR text proves only that it catches the OLD defect. The new pin can simultaneously over-reject, and two extra repair rounds were the cost of finding that out later.',
    mutants: Object.freeze([
      Object.freeze({
        id: 'revert-pre-fix-text',
        expected: 'red',
        description: 'restore the pre-fix text and require the assertion to fail',
      }),
      Object.freeze({
        id: 'perturb-new-pin',
        expected: 'red',
        description:
          'perturb the NEW guard itself — an equally whitespace-tolerant substitution over the new sentence or literal — and require the assertion to fail, so the pin is shown to be about the text it names rather than about anything that happens to be there',
      }),
    ]),
  }),
  'skip-rule': Object.freeze({
    trigger: 'a "drop or skip X when P" rule widened',
    why: 'Widening a skip rule needs a falsification in the OPPOSITE direction: the red proves the rule now fires, and nothing proves it did not start swallowing inputs that should still be processed.',
    mutants: Object.freeze([
      Object.freeze({
        id: 'revert-widened-rule',
        expected: 'red',
        description: 'revert the widening and require the new test to fail',
      }),
      Object.freeze({
        id: 'legitimately-skipped-input',
        expected: 'green',
        justification: true,
        description:
          'run an input that legitimately satisfies P, require it to take the OTHER branch and stay green, and record the enumeration of what is legitimately in the dropped set',
      }),
    ]),
  }),
  'design-departure': Object.freeze({
    trigger: "the fix deliberately departs from a reviewer's literal suggestion",
    why: 'Mutating in one direction proves the behaviour. Mutating toward the suggested alternative is what proves the DESIGN CHOICE, and that direction is not part of any documented routine.',
    mutants: Object.freeze([
      Object.freeze({
        id: 'revert-chosen-implementation',
        expected: 'red',
        description: 'revert the chosen implementation and require the test to fail',
      }),
      Object.freeze({
        id: 'suggested-alternative',
        expected: 'red',
        justification: true,
        description:
          'implement the reviewer\'s suggested alternative, require it to fail TOO, and record at what count or level it fails — "both fail, at different levels" is the evidence for the departure; "both pass" means the departure was unjustified',
      }),
    ]),
  }),
  other: Object.freeze({
    trigger: 'a fix whose shape none of the rows above describes',
    why: 'The enumeration may not cover a real fix. An unclassifiable fix is RECORDED rather than silently waved through: this shape obliges no mutant set, so it NEVER adjudicates to `satisfied` and never exits 0. Its best outcome is `unclassified` — an echoed admission that nothing was mutated — which clears a gate only on an explicit, recorded override.',
    requiresShapeJustification: true,
    mutants: Object.freeze([]),
  }),
})

/** Every shape name, in enumeration order. */
export const SHAPE_NAMES = Object.freeze(Object.keys(MUTATION_SHAPES))

/** The shapes an author can actually classify into — every name but the escape hatch. */
export const CLASSIFIED_SHAPE_NAMES = Object.freeze(
  SHAPE_NAMES.filter((name) => MUTATION_SHAPES[name].requiresShapeJustification !== true),
)

/**
 * Whether an `other` justification says enough to be worth recording: long enough to be
 * a sentence, and naming at least one classified shape the author ruled out. Neither
 * test can make the hatch honest, but `"x"` and `"n/a"` no longer clear it.
 */
export function isSubstantiveShapeJustification(justification) {
  if (!isNonEmptyString(justification)) return false
  if (justification.trim().length < MIN_SHAPE_JUSTIFICATION_CHARS) return false
  const lowered = justification.toLowerCase()
  return CLASSIFIED_SHAPE_NAMES.some((name) => lowered.includes(name))
}

/**
 * The mutants `shape` owes, or `null` when the shape is not in the enumeration.
 * The array is the canonical obligation — a caller may read it, never edit it.
 */
export function obligedMutants(shape) {
  const entry = Object.hasOwn(MUTATION_SHAPES, shape) ? MUTATION_SHAPES[shape] : null
  return entry ? entry.mutants : null
}

/** The whole enumeration as a plain serialisable object (the `shapes --json` payload). */
export function describeShapes() {
  return SHAPE_NAMES.map((name) => {
    const entry = MUTATION_SHAPES[name]
    return {
      shape: name,
      trigger: entry.trigger,
      why: entry.why,
      requiresShapeJustification: entry.requiresShapeJustification === true,
      mutants: entry.mutants.map((mutant) => ({
        id: mutant.id,
        expected: mutant.expected,
        justification: mutant.justification === true,
        description: mutant.description,
      })),
    }
  })
}

function isNonEmptyString(value) {
  return typeof value === 'string' && value.trim().length > 0
}

/**
 * Words that carry no layer information on their own — an article, or a noun that says
 * "the thing that processes a language" without naming WHICH layer. They are stripped
 * so the guard judges what is left.
 *
 * They exist because the guard used to be an EXACT match on the whole normalised
 * string: `Go` was refused, and `Go source`, `the Go compiler`, `Go parser` and
 * `golang toolchain` all passed. One adjacent word defeated the only mechanical check
 * the module applies to a skip — and an accepted skip is the only thing that lets an
 * obliged mutant go unrun at all.
 */
const LAYER_SCAFFOLD_WORDS = Object.freeze([
  'a',
  'an',
  'and',
  'code',
  'compilation',
  'compiler',
  'file',
  'files',
  'grammar',
  'in',
  'it',
  'its',
  'lang',
  'language',
  'of',
  'parse',
  'parser',
  'parsing',
  'runtime',
  'source',
  'spec',
  'standard',
  'syntax',
  'that',
  'the',
  'this',
  'toolchain',
])

// A consuming layer that names ONLY a source language — alone or wrapped in scaffold
// words — has named the language the file is written in rather than the layer that
// parses the input. Judged word-wise, not as a whole string: `Go source` and `the
// TypeScript compiler` are refused alongside `go`, while `a text scanner that reads
// the file with readFileSync` keeps words this module does not carry and is accepted.
function namesOnlyASourceLanguage(layer) {
  const words = layer
    .toLowerCase()
    .split(/[^a-z0-9+#]+/)
    .filter((word) => word.length > 0)
  let namedALanguage = false
  for (const word of words) {
    if (SOURCE_LANGUAGE_TOKENS.includes(word)) {
      namedALanguage = true
      continue
    }
    if (!LAYER_SCAFFOLD_WORDS.includes(word)) return false
  }
  return namedALanguage
}

/**
 * Adjudicate a recorded proof against the mutant set its shape obliges.
 *
 * The record:
 *
 *   {
 *     "shape": "widening",
 *     "shapeJustification": "...",         // required for shape "other" only
 *     "mutants": [
 *       { "id": "revert-added-clause", "expected": "red", "observed": "red",
 *         "mechanism": "probe", "justification": "..." }
 *     ],
 *     "skips": [ { "mutantId": "...", "reason": "...", "consumingLayer": "..." } ]
 *   }
 *
 * Returns `{ verdict, shape, missing, failures, extra, echoedJustification }`.
 * `verdict` is one of ADJUDICATIONS and nothing else; `missing` names every obliged
 * mutant id the record neither ran nor validly skipped; `failures` carries one
 * `{ kind, mutantId, detail }` per rule broken.
 */
export function adjudicateProofRecord(record) {
  const failures = []
  const missing = []
  const extra = []
  const fail = (kind, detail, mutantId = null) => failures.push({ kind, mutantId, detail })

  if (record === null || typeof record !== 'object' || Array.isArray(record)) {
    fail('record-not-an-object', 'the proof record must be a JSON object')
    return { verdict: 'insufficient', shape: null, missing, failures, extra }
  }

  const shape = record.shape
  const obliged = typeof shape === 'string' ? obligedMutants(shape) : null
  if (obliged === null) {
    fail(
      'unknown-shape',
      `shape ${JSON.stringify(shape ?? null)} is not one of: ${SHAPE_NAMES.join(', ')} — classify the fix, or record it as 'other' with a written justification`,
    )
    return { verdict: 'insufficient', shape: shape ?? null, missing, failures, extra }
  }

  const entry = MUTATION_SHAPES[shape]
  let echoedJustification = null
  if (entry.requiresShapeJustification === true) {
    if (!isNonEmptyString(record.shapeJustification)) {
      fail(
        'shape-justification-missing',
        `shape '${shape}' obliges no mutant set, so it discharges only on a written justification — set "shapeJustification" to why this fix's shape is not in the enumeration`,
      )
    } else if (!isSubstantiveShapeJustification(record.shapeJustification)) {
      fail(
        'shape-justification-insufficient',
        `shape '${shape}' obliges no mutant set, so its justification carries the whole record: write at least ${MIN_SHAPE_JUSTIFICATION_CHARS} characters and name at least one classified shape (${CLASSIFIED_SHAPE_NAMES.join(', ')}) you considered and why it does not apply`,
      )
    } else {
      echoedJustification = record.shapeJustification
    }
  }

  const mutants = record.mutants
  if (mutants !== undefined && !Array.isArray(mutants)) {
    fail('mutants-not-a-list', '"mutants" must be a JSON array of recorded mutants')
  }
  const recorded = Array.isArray(mutants) ? mutants : []

  const skips = record.skips
  if (skips !== undefined && !Array.isArray(skips)) {
    fail('skips-not-a-list', '"skips" must be a JSON array of recorded skips')
  }
  const recordedSkips = Array.isArray(skips) ? skips : []

  // --- skips first: an ACCEPTED skip is what lets an obliged mutant go unrun. ---
  const obligedIds = new Set(obliged.map((mutant) => mutant.id))
  const acceptedSkips = new Set()
  recordedSkips.forEach((skip, index) => {
    const where = `skips[${index}]`
    if (skip === null || typeof skip !== 'object' || Array.isArray(skip)) {
      fail('skip-not-an-object', `${where} must be an object`)
      return
    }
    const mutantId = skip.mutantId
    if (!isNonEmptyString(mutantId)) {
      fail('skip-target-missing', `${where} must name the mutant it skips in "mutantId"`)
      return
    }
    if (!obligedIds.has(mutantId)) {
      fail(
        'skip-target-unknown',
        `${where} skips '${mutantId}', which shape '${shape}' does not oblige (obliged: ${[...obligedIds].join(', ') || 'none'})`,
        mutantId,
      )
      return
    }
    let accepted = true
    if (!isNonEmptyString(skip.reason)) {
      fail('skip-reason-missing', `${where} must carry a "reason"`, mutantId)
      accepted = false
    }
    if (!isNonEmptyString(skip.consumingLayer)) {
      fail(
        'skip-layer-missing',
        `${where} must name the layer that actually PARSES the input in "consumingLayer" — a skip justified against the language the file is nominally written in is not a discharge`,
        mutantId,
      )
      accepted = false
    } else if (namesOnlyASourceLanguage(skip.consumingLayer)) {
      fail(
        'skip-layer-is-source-language',
        `${where} names '${skip.consumingLayer}', which is the source language rather than the consuming layer — name what parses the input (for example a text scanner that reads the file and splits on newlines never compiles that language, so its validity rules gate nothing)`,
        mutantId,
      )
      accepted = false
    }
    if (accepted) acceptedSkips.add(mutantId)
  })

  // --- then the recorded mutants themselves. ---
  const byId = new Map()
  recorded.forEach((mutant, index) => {
    const where = `mutants[${index}]`
    if (mutant === null || typeof mutant !== 'object' || Array.isArray(mutant)) {
      fail('mutant-not-an-object', `${where} must be an object`)
      return
    }
    if (!isNonEmptyString(mutant.id)) {
      fail('mutant-id-missing', `${where} must carry an "id"`)
      return
    }
    if (!obligedIds.has(mutant.id)) {
      extra.push(mutant.id)
      return
    }
    if (byId.has(mutant.id)) {
      fail('mutant-recorded-twice', `${where} repeats mutant '${mutant.id}'`, mutant.id)
      return
    }
    byId.set(mutant.id, mutant)
  })

  for (const obligation of obliged) {
    const { id, expected } = obligation
    const mutant = byId.get(id)
    if (!mutant) {
      if (acceptedSkips.has(id)) continue
      missing.push(id)
      fail(
        'mutant-missing',
        `shape '${shape}' obliges mutant '${id}' (required verdict: ${expected}) — ${obligation.description}`,
        id,
      )
      continue
    }
    if (acceptedSkips.has(id)) {
      fail(
        'mutant-skipped-and-run',
        `mutant '${id}' is both recorded and skipped — record one or the other`,
        id,
      )
    }
    // The record states the required verdict as well as the observed one. Checking
    // the stated requirement against the canonical obligation is what stops a record
    // relabelling a red obligation as green and then "satisfying" it with a green.
    if (mutant.expected === undefined) {
      fail(
        'mutant-expected-missing',
        `mutant '${id}' must state its required verdict in "expected" (this shape requires ${expected})`,
        id,
      )
    } else if (mutant.expected !== expected) {
      fail(
        'mutant-expected-mismatch',
        `mutant '${id}' records a required verdict of ${JSON.stringify(mutant.expected)}, but shape '${shape}' requires ${expected} — the obligation is not the record's to restate`,
        id,
      )
    }
    if (!MUTANT_VERDICTS.includes(mutant.observed)) {
      fail(
        'mutant-observed-invalid',
        `mutant '${id}' records an observed verdict of ${JSON.stringify(mutant.observed ?? null)}; it must be one of: ${MUTANT_VERDICTS.join(', ')}`,
        id,
      )
    } else if (mutant.observed !== expected) {
      fail(
        'mutant-observed-mismatch',
        `mutant '${id}' was observed ${mutant.observed} where shape '${shape}' requires ${expected} — ${obligation.description}`,
        id,
      )
    }
    const mechanism = mutant.mechanism === undefined ? 'probe' : mutant.mechanism
    if (!DISCHARGE_MECHANISMS.includes(mechanism)) {
      fail(
        'mutant-mechanism-unknown',
        `mutant '${id}' records mechanism ${JSON.stringify(mutant.mechanism)}; it must be one of: ${DISCHARGE_MECHANISMS.join(', ')}`,
        id,
      )
    }
    if (obligation.justification === true && !isNonEmptyString(mutant.justification)) {
      fail(
        'mutant-justification-missing',
        `mutant '${id}' discharges only with a written justification — ${obligation.description}`,
        id,
      )
    }
  }

  // A clean 'other' record is NOT `satisfied`: no mutant ran, so nothing was proven.
  // It gets its own token so a caller reading the verdict — or the exit code, which is
  // non-zero for anything but `satisfied` — has to decide about it rather than sail past.
  const clean = failures.length === 0
  return {
    verdict: clean
      ? entry.requiresShapeJustification === true
        ? 'unclassified'
        : 'satisfied'
      : 'insufficient',
    shape,
    missing,
    failures,
    extra,
    echoedJustification,
  }
}

/** Render an adjudication for a terminal. Callers that want structure read the object. */
export function formatAdjudication(result) {
  const lines = [`${result.verdict}  shape=${result.shape ?? '<none>'}`]
  if (result.missing.length > 0) {
    lines.push(`  missing mutants: ${result.missing.join(', ')}`)
  }
  for (const { kind, detail } of result.failures) {
    lines.push(`  ${kind}: ${detail}`)
  }
  if (result.extra && result.extra.length > 0) {
    lines.push(`  (also recorded, not obliged by this shape: ${result.extra.join(', ')})`)
  }
  if (result.echoedJustification) {
    lines.push(`  recorded justification: ${result.echoedJustification}`)
  }
  return lines.join('\n')
}

/** Render the enumeration for a terminal. */
export function formatShapes(shapes) {
  const lines = []
  for (const entry of shapes) {
    lines.push(`${entry.shape} — ${entry.trigger}`)
    lines.push(`  why: ${entry.why}`)
    if (entry.requiresShapeJustification) {
      lines.push(
        `  obliges NO mutant set: adjudicates to 'unclassified' (exit 1), never 'satisfied', and requires a shapeJustification of at least ${MIN_SHAPE_JUSTIFICATION_CHARS} characters naming a classified shape it is not`,
      )
    }
    for (const mutant of entry.mutants) {
      const marks = mutant.justification ? ' [+ written justification]' : ''
      lines.push(`  - ${mutant.id} → ${mutant.expected}${marks}: ${mutant.description}`)
    }
    lines.push('')
  }
  return lines.join('\n').trimEnd()
}

// Thin CLI (the surface the skill prose invokes):
//   node bs-mutation-obligations.mjs shapes [--json]
//   node bs-mutation-obligations.mjs adjudicate --record <json>
//
// Exit 0 on `satisfied`, 1 on `insufficient`, 2 on an operator error (bad usage,
// unreadable or absent record). An operator error prints NO verdict word, because a
// caller that greps stdout for one must never read a usage failure as a discharge.
//
// Verbs dispatch with `command === '<verb>'` and never a `switch`:
// check-skill-symbols.mjs extracts a helper's verbs through `===`/`!==` only, so a
// `switch` makes every verb here invisible to the gate that checks the skill prose
// citing them.
if (isMainModule(import.meta.url)) {
  const [command, ...args] = process.argv.slice(2)
  const usage =
    'usage: bs-mutation-obligations.mjs shapes [--json] | bs-mutation-obligations.mjs adjudicate --record <json>'
  const fail = (message) => {
    process.stderr.write(`bs-mutation-obligations.mjs: ${message}\n`)
    process.exit(2)
  }

  if (command === 'shapes') {
    let asJson = false
    for (const arg of args) {
      if (arg === '--json') asJson = true
      else fail(`unknown option: ${arg}\n${usage}`)
    }
    const shapes = describeShapes()
    process.stdout.write(asJson ? `${JSON.stringify(shapes)}\n` : `${formatShapes(shapes)}\n`)
    process.exit(0)
  }

  if (command === 'adjudicate') {
    let file = null
    while (args.length > 0) {
      const option = args.shift()
      const value = args.shift()
      if (typeof value !== 'string') fail(`missing value after ${option}\n${usage}`)
      if (option === '--record') file = value
      else fail(`unknown option: ${option}\n${usage}`)
    }
    if (!file) fail(`adjudicate requires --record <json>\n${usage}`)
    let parsed
    try {
      parsed = JSON.parse(readFileSync(file, 'utf8'))
    } catch (err) {
      fail(`failed to read the proof record ${file}: ${err.message}`)
    }
    const result = adjudicateProofRecord(parsed)
    process.stdout.write(`${formatAdjudication(result)}\n`)
    process.exit(result.verdict === 'satisfied' ? 0 : 1)
  }

  fail(usage)
}
