#!/usr/bin/env node

// plan-writeback-verify — the post-save read-back that MEASURES round-trip fidelity of a tracker
// description write instead of assuming it.
//
// Every other gate in the planning skill is pre-write prevention: they read local files and abort
// before the save. Nothing observed what actually landed, so a transcription slip at the save step
// survived the whole gate stack — and the tracker exposes no description history to an agent, so the
// written text is the only surviving copy. This helper closes that half.
//
// Two recorded field beliefs contradict each other. One says the tracker normalizes markdown on
// write, so a description never round-trips byte-identically and a post-save byte diff can only
// false-positive. The other says the normalization belongs to the write TRANSPORT, and a raw write
// does round-trip byte-identically. This helper encodes neither: round-trip fidelity is a property
// of the transport, so it is measured in the run that used it and the strongest check the
// measurement supports is applied.
//
//   tier 1  byte-exact              stored == intended, byte for byte.
//   tier 2  normalized-equivalent   every difference is attributable to a DECLARED transform, AND
//                                   the semantic contract validates against the stored text, AND
//                                   the verbatim block matches under the same normalization with
//                                   its upload identities unchanged. All three, or it is not tier 2.
//   tier 3  drift                   anything else. Exit non-zero, name the differing line and
//                                   column, retain the scratch for triage.
//
// The tier-3 branch is deliberately NON-DESTRUCTIVE. By the time this runs the description is
// already stored, so the "no write, abort" branch the pre-write gates take is unavailable — and an
// automatic corrective rewrite would be an unattended agent overwriting a description it has just
// proven it cannot reproduce faithfully. This helper never writes anything.
//
// Node builtins only — this runs in dependency-light cron worktrees.

import { readFileSync } from 'node:fs'

import { isMainModule } from './main-module.mjs'
import { findDroppedImages, originalNotesBodies } from './plan-image-guard.mjs'
import {
  DESCRIPTION_NORMALIZATION_TRANSFORMS,
  loadSkillConfig,
  toleratedDescriptionTransforms,
  validatePlanDescription,
} from './skill-config.mjs'

/** The three verdicts. Exactly one is emitted per run, or none at all on a refusal. */
export const WRITEBACK_VERDICTS = Object.freeze({
  BYTE_EXACT: 'byte-exact',
  NORMALIZED_EQUIVALENT: 'normalized-equivalent',
  DRIFT: 'drift',
})

// One canonicalizer per transform id in skill-config's closed vocabulary. Each maps BOTH spellings
// of its transform onto one canonical form, so it can be applied to the intended and the stored side
// alike without knowing which one the tracker reshaped. Each is idempotent.
export const DESCRIPTION_TRANSFORM_NORMALIZERS = Object.freeze({
  // `- item` / `+ item` / `* item` -> one canonical marker. Byte-count neutral, which is why a size
  // comparison cannot stand in for the byte comparison.
  'unordered-list-marker-substitution': (text) => text.replace(/^([ \t]*)[-+*]([ \t])/gm, '$1*$2'),
  // `**1.** ` stored as `**1. **`: whitespace migrating across a delimiter run. Canonical form puts
  // the whitespace outside. The delimiter must be FOLLOWED by whitespace or end-of-line, which is
  // what keeps an ordinary opening delimiter (`a *b* c`) out of the rule.
  'emphasis-delimiter-whitespace-migration': (text) =>
    text.replace(/[ \t]+(\*{1,3}|_{1,3})(?=[ \t]|$)/gm, '$1'),
  // `**a `b` c**` stored as `**a **`b`** c**`: one emphasis span containing an inline code span
  // rewritten as emphasis + code + emphasis. Canonical form MERGES the split back into the single
  // span; it does NOT strip delimiter runs that merely sit against a code-span boundary.
  //
  // Stripping was lossy in the one direction that matters. `The **`foo`** helper.` and
  // `The `foo` helper.` — a stored description that genuinely LOST the emphasis around an inline
  // code span — both reduced to `The `foo` helper.`, so the fidelity gate certified real content
  // loss as normalized-equivalent and exited zero. A canonicalizer for a RESTRUCTURING must
  // preserve the presence or absence of the emphasis and normalize only its position.
  //
  // The merge is anchored on the OUTER delimiter pair: the pattern is D…D `code` D…D, four runs of
  // the SAME delimiter, and only the two interior ones are dropped. A run that IS the emphasis
  // (`**`x`**`, two runs) has no outer pair to anchor on and survives untouched, so it still
  // differs from the un-emphasised `` `x` ``. Deliberately conservative: a shape this pattern does
  // not recognise stays a byte difference and is reported as drift, which is the safe direction.
  'emphasis-span-restructuring': (text) => {
    // A and B may not contain a backtick or a newline, which bounds the match to one emphasis span
    // on one line — the shape the transform was measured on. Every one of the four delimiter runs
    // is fenced by `(?<![*_])` / `(?![*_])` so it can only match a MAXIMAL run: without that fence
    // the engine backtracks `**` down to `*` and rewrites `**`x`**` as `*`x`*`, which loses a
    // delimiter and collides bold with italic — the same class of loss this rewrite exists to end.
    const RUN = String.raw`(?<![*_])\1(?![*_])`
    const split = new RegExp(
      String.raw`(?<![*_])(\*{1,3}|_{1,3})(?![*_])([^\`\n]*?)${RUN}(\`[^\`\n]*\`)${RUN}([^\`\n]*?)${RUN}`,
      'g',
    )
    let out = String(text)
    // Run to a fixpoint (bounded) so the result is idempotent and order-independent.
    for (let pass = 0; pass < 10; pass += 1) {
      const next = out.replace(split, '$1$2$3$4$1')
      if (next === out) break
      out = next
    }
    return out
  },
  'trailing-whitespace-trimming': (text) => text.replace(/[ \t]+$/gm, ''),
  'terminal-newline-trimming': (text) => text.replace(/\n+$/, ''),
})

/**
 * The vocabulary and the normalizer table are two hand-written restatements of ONE list, in two
 * files. Cross-check them at load, bidirectionally, so they cannot drift silently:
 *
 *   - a vocabulary id with no normalizer would validate in `.boss-skills.json`, be declarable as
 *     tolerated, and then be INERT at normalization time — a config knob that quietly does nothing;
 *   - a normalizer with no vocabulary id can never be reached, because validation rejects the id.
 *
 * This throws rather than warning. An unverifiable fidelity gate must fail loudly: the whole point
 * of the helper is that nothing else observes what landed on the tracker.
 */
const missingNormalizers = DESCRIPTION_NORMALIZATION_TRANSFORMS.filter(
  (id) => typeof DESCRIPTION_TRANSFORM_NORMALIZERS[id] !== 'function',
)
const orphanNormalizers = Object.keys(DESCRIPTION_TRANSFORM_NORMALIZERS).filter(
  (id) => !DESCRIPTION_NORMALIZATION_TRANSFORMS.includes(id),
)
if (missingNormalizers.length > 0 || orphanNormalizers.length > 0) {
  throw new Error(
    'plan-writeback-verify: DESCRIPTION_TRANSFORM_NORMALIZERS is out of step with skill-config ' +
      `DESCRIPTION_NORMALIZATION_TRANSFORMS — declarable but inert: [${missingNormalizers.join(', ')}]; ` +
      `unreachable normalizers: [${orphanNormalizers.join(', ')}]`,
  )
}

/**
 * Apply ONLY the declared transforms, in a fixed order so both sides canonicalize identically.
 * An empty tolerated set is the identity function — the strictest possible comparison.
 */
export function normalizeDescription(text, tolerated) {
  let out = String(text ?? '')
  for (const [id, normalize] of Object.entries(DESCRIPTION_TRANSFORM_NORMALIZERS)) {
    if (tolerated.has(id)) out = normalize(out)
  }
  return out
}

/**
 * The description-contract modes this helper can verify against, mirroring the sibling pre-write
 * lint. `child-plan` is the implementation-plan contract; `epic-parent` is the parent-overview
 * shape an epic decomposition stores. The seam exists because the two contracts require DIFFERENT
 * sections: verifying a correctly stored parent overview against the child contract reports `drift`
 * at the worst possible moment — already stored, non-zero exit, scratch retained, and no corrective
 * rewrite permitted. The default keeps every existing caller on `child-plan`.
 */
export const WRITEBACK_DESCRIPTION_MODES = Object.freeze(['child-plan', 'epic-parent'])

const WRITEBACK_DESCRIPTION_MODE_SET = new Set(WRITEBACK_DESCRIPTION_MODES)

/** Reject an unknown mode loudly rather than silently falling back to the child contract. */
function assertWritebackMode(mode) {
  if (!WRITEBACK_DESCRIPTION_MODE_SET.has(mode)) {
    throw new Error(
      `--mode must be one of ${WRITEBACK_DESCRIPTION_MODES.join(', ')}; got ${JSON.stringify(mode)}`,
    )
  }
  return mode
}

/** The 1-based line and column of the first byte at which `a` and `b` differ, or null if equal. */
function firstDifference(a, b) {
  if (a === b) return null
  const limit = Math.max(a.length, b.length)
  let index = 0
  for (; index < limit; index += 1) {
    if (a.charAt(index) !== b.charAt(index)) break
  }
  const before = a.slice(0, index)
  const line = before.split('\n').length
  const column = index - (before.lastIndexOf('\n') + 1) + 1
  return { line, column }
}

function locate(intendedText, storedText) {
  return firstDifference(intendedText, storedText) ?? { line: 1, column: 1 }
}

/**
 * Compute the read-back verdict.
 *
 * Returns `{ verdict, exitCode, reason, line, column, tolerated }`. A `verdict` of `null` is a
 * REFUSAL, not a tier: the comparison could not be performed at all, which is neither a pass nor
 * drift, and callers must not report it as either.
 */
export function verifyWriteback({
  config,
  intendedText,
  storedText,
  tolerated,
  mode = 'child-plan',
} = {}) {
  const declared = tolerated ?? toleratedDescriptionTransforms(config)
  const resolvedMode = assertWritebackMode(mode)

  // Refuse to CERTIFY a comparison we could not meaningfully perform. An empty stored description is
  // the exact input a broken read-back produces, and it is also the input a vacuous gate is most
  // confident about — byte-equal against an empty intended text would otherwise read as tier 1.
  if (String(storedText ?? '').trim() === '') {
    return {
      verdict: null,
      exitCode: 1,
      reason: 'stored description is empty — nothing was verified',
      line: null,
      column: null,
      tolerated: declared,
    }
  }

  if (intendedText === storedText) {
    return {
      verdict: WRITEBACK_VERDICTS.BYTE_EXACT,
      exitCode: 0,
      reason:
        'stored description is byte-identical to the intended bytes; the transport round-trips',
      line: null,
      column: null,
      tolerated: declared,
    }
  }

  const { line, column } = locate(intendedText, storedText)
  const at = `line ${line}, column ${column}`
  const drift = (reason) => ({
    verdict: WRITEBACK_VERDICTS.DRIFT,
    exitCode: 1,
    reason: `${reason} (first difference at ${at})`,
    line,
    column,
    tolerated: declared,
  })

  // KTD3 — tier 2 is a CONJUNCTION, not a fuzzy match: EVERY byte difference is attributable to a
  // declared transform, AND the semantic contract validates against the stored text, AND the
  // verbatim block matches under the same normalization with its upload identities unchanged. Any
  // one failing drops the verdict to tier 3, because the semantic validator alone cannot see a
  // dropped image and the image check alone cannot see a dropped section.
  //
  // All four are evaluated, and the REASON names the most specific failure rather than whichever
  // one the flowchart draws first. The verdict is identical either way — a failing conjunct is
  // tier 3 however it is discovered — but "the stored text is missing ## Required proof" sends
  // triage somewhere, while "differs outside the declared transform set" on the same input does
  // not. A conjunct failure is also the strictly more alarming reading of the same bytes.
  const contract = validatePlanDescription(config, storedText, { mode: resolvedMode })
  if (!contract.ok) {
    const detail = contract.unsupportedVersion
      ? `stamped Contract: v${contract.version}, newer than this contract`
      : `missing ${contract.missing.join(', ')}`
    return drift(`stored description fails the semantic description contract: ${detail}`)
  }

  const droppedUploads = findDroppedImages(intendedText, storedText)
  if (droppedUploads.length > 0) {
    return drift(
      `stored description lost ${droppedUploads.length} upload identit${
        droppedUploads.length === 1 ? 'y' : 'ies'
      }: ${droppedUploads.join(', ')}`,
    )
  }

  const intendedNotes = originalNotesBodies(intendedText).map((body) =>
    normalizeDescription(body, declared),
  )
  const storedNotes = originalNotesBodies(storedText).map((body) =>
    normalizeDescription(body, declared),
  )
  if (
    intendedNotes.length !== storedNotes.length ||
    intendedNotes.some((body, index) => body !== storedNotes[index])
  ) {
    return drift(
      'stored `## Original notes` block differs from the intended block under the same normalization',
    )
  }

  const normalizedIntended = normalizeDescription(intendedText, declared)
  const normalizedStored = normalizeDescription(storedText, declared)
  if (normalizedIntended !== normalizedStored) {
    const declaredList = declared.size === 0 ? 'none declared' : [...declared].sort().join(', ')
    return drift(
      `stored description differs from the intended bytes outside the declared transform set (${declaredList})`,
    )
  }

  return {
    verdict: WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT,
    exitCode: 0,
    reason:
      `stored description differs only by declared transforms (${[...declared].sort().join(', ')}); ` +
      'the semantic contract, the verbatim block and every upload identity survived the write',
    line,
    column,
    tolerated: declared,
  }
}

export function parseWritebackVerifyArgs(argv) {
  const args = { intended: null, stored: null, mode: 'child-plan' }
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    if (flag === '--intended') {
      args.intended = argv[(i += 1)]
    } else if (flag === '--stored') {
      args.stored = argv[(i += 1)]
    } else if (flag === '--mode') {
      args.mode = assertWritebackMode(argv[(i += 1)])
    } else {
      throw new Error(`unknown argument: ${flag}`)
    }
  }
  if (!args.intended) throw new Error('--intended <path> is required')
  if (!args.stored) throw new Error('--stored <path> is required')
  return args
}

function main() {
  const { intended, stored, mode } = parseWritebackVerifyArgs(process.argv.slice(2))

  // A read failure is NOT drift and is never a pass. Keep the two distinguishable in the message and
  // in the output shape: a refusal emits no verdict line at all, so a reader (and a grep) cannot
  // mistake an unverifiable run for a measured one.
  let intendedText
  try {
    intendedText = readFileSync(intended, 'utf8')
  } catch (error) {
    console.error(
      `plan-writeback-verify: cannot read intended description ${intended}: ${error.message}`,
    )
    process.exitCode = 1
    return
  }
  let storedText
  try {
    storedText = readFileSync(stored, 'utf8')
  } catch (error) {
    console.error(
      `plan-writeback-verify: cannot read stored description ${stored}: ${error.message}`,
    )
    process.exitCode = 1
    return
  }

  const config = loadSkillConfig({ cwd: process.cwd() })
  const result = verifyWriteback({ config, intendedText, storedText, mode })
  if (result.verdict === null) {
    console.error(`plan-writeback-verify: ${result.reason}`)
    process.exitCode = result.exitCode
    return
  }

  console.log(`writeback-verdict: ${result.verdict}`)
  console.log(`plan-writeback-verify: ${result.reason}`)
  if (result.exitCode !== 0) {
    console.error(`plan-writeback-verify: ${result.reason}`)
    console.error(
      'plan-writeback-verify: the description is ALREADY stored — do not attempt a corrective rewrite; ' +
        'retain the scratch and triage the diff',
    )
    process.exitCode = result.exitCode
  }
}

// isMainModule resolves both paths through symlinks so this fail-closed CLI gate cannot be skipped.
const invokedDirectly = isMainModule(import.meta.url)
if (invokedDirectly) {
  try {
    main()
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
