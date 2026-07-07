#!/usr/bin/env node

// Pre-generalization skill-behavior baseline capture (BOS-249). Records the
// CURRENT (pre-generalization) deterministic behavior of four Bossanova skill
// functions into committed JSON snapshots under scripts/skill-parity/baseline/.
// A later ticket (BOS-248) re-runs the post-refactor skill functions over the
// SAME inputs and asserts the outputs still match these snapshots — so this
// module intentionally imports TODAY'S real source paths (never the
// generalized/extracted successors): the snapshots are what must survive the
// refactor, not this capture script. Node builtins only (no new deps).
//
// INPUT MODEL — committed fixtures vs. live-read reference sources. Each
// signature has two inputs: the *scenario* input (frozen) and, for three of the
// four, a *reference source* read live from the working tree (NOT frozen):
//
//   signature       scenario input (committed)          reference source (read live)
//   ------------     ------------------------------      ---------------------------------
//   review-lens      review-lens.input.json (files)      .boss-skills.json lensMap
//   proof-surface    proof-surface.input.json (files)    proof/recipes/default.json
//   dag-schedule     dag-schedule.input.json (nodes)     — (fully self-contained)
//   plan-sections    — (the template IS the signature)   headless-drafting-brief.md Step 7
//
// This is deliberate: the signature is "what TODAY'S real skill (real registry/
// catalog/template included) produces," so the reference sources are read live
// on purpose. The consequence a consumer must know: because proof/** and
// .claude/skills/** are in test-scripts.yml's path filter, an UNRELATED future
// edit to proof/recipes/default.json (a new recipe matching a fixture file) or
// to the Step-7 headings drifts the recomputed snapshot and FAILS the
// determinism test in scripts CI — that edit is then expected to re-run the
// capture and re-commit the baseline as a DELIBERATE rebaseline. (The lensMap
// input is the exception: .boss-skills.json is NOT in that path filter, so a
// lensMap-only edit drifts review-lens.snapshot.json silently until some other
// scripts/skills change re-triggers the gate.) A fully-hermetic alternative —
// freezing copies of the lensMap/catalog/template alongside the scenario inputs
// so the parity comparison isolates code drift from config drift — is a BOS-248
// contract decision left to a human, out of scope for this capture ticket.
//
// Snapshot schema (consumed by BOS-248's parity-gate.mjs without
// reverse-engineering):
//
//   review-lens.snapshot.json — boss-review lens selection (bs-review-detect.mjs
//     matchLenses() against the repo's loadSkillConfig().lensMap):
//       {
//         matchedLensIds: string[],   // lens ids that fired, sorted
//         lenses: Array<{
//           lens: string,             // lens id
//           skill: string,            // skill dispatched for this lens
//           fallbackRubric: string,   // inline rubric substituted when the skill can't load
//           files: string[],          // matched changed-file paths, sorted
//         }>,                          // sorted by `lens`
//       }
//
//   proof-surface.snapshot.json — boss-proof surface classification
//     (proof-surfaces.mjs classifySurfaces() against proof/recipes/default.json):
//       {
//         tui: boolean,
//         web: boolean,
//         recipes: Array<{ id: string, surface: string }>, // browser (marketing/docs)
//           // recipes only, sorted by `id` — NOT the full recipe object (keeps the
//           // snapshot stable against unrelated recipe-field churn).
//       }
//
//   dag-schedule.snapshot.json — bs-epic DAG schedule (dag-scheduler.mjs
//     buildGraph/readyTickets/nextToMerge over the committed node-set fixture):
//       {
//         readyIds: string[],   // readyTickets() initial ready-node ids, IN ORDER
//                                // (order is the signature — do not sort)
//         mergeOrder: string[], // ids in the order nextToMerge() would serialize
//                                // them, IN ORDER (order is the signature — do not sort).
//                                // Captured over a FULLY-GREEN pool: every not-yet-
//                                // merged node is fed to nextToMerge() as a candidate.
//                                // This snapshots nextToMerge()'s dependency + priority/
//                                // oldest-tiebreak ORDERING only; it deliberately does
//                                // NOT model the real driver's merge-ELIGIBILITY filter
//                                // (failed/cascade-skipped exclusion, external merge
//                                // gating), which is a separate concern out of scope here.
//       }
//
//   plan-sections.snapshot.json — boss-plan Step 7 section contract (the
//     byte-identical ```markdown template under "## Step 7" in
//     services/boss/internal/skillinstall/skills/boss-plan/references/headless-drafting-brief.md):
//       {
//         sections: string[], // ordered "## " heading lines, IN TEMPLATE ORDER
//                              // (order IS the contract — do not sort)
//       }
//
// Determinism rules: no timestamps/Date.now/Math.random/absolute paths/env
// values in any snapshot; every set-like collection is sorted with a fixed
// comparator; order is preserved only where order IS the signature (dag ready/
// merge order, plan sections); every object's keys are recursively sorted
// before stringifying so re-running yields byte-identical files.

import { readFileSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { matchLenses } from '../../services/boss/internal/skillinstall/skills/boss-review/toolbox/bs-review-detect.mjs'
import { loadSkillConfig } from '../../services/boss/internal/skillinstall/skills/boss-review/toolbox/skill-config.mjs'
import { classifySurfaces } from '../proof-surfaces.mjs'
import {
  buildGraph,
  nextToMerge,
  readyTickets,
} from '../../services/boss/internal/skillinstall/skills/boss-epic/toolbox/dag-scheduler.mjs'

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url))
export const REPO_ROOT = join(SCRIPT_DIR, '..', '..')
export const BASELINE_DIR = join(SCRIPT_DIR, 'baseline')

const PROOF_CATALOG_PATH = join(REPO_ROOT, 'proof', 'recipes', 'default.json')
const PLAN_BRIEF_PATH = join(
  REPO_ROOT,
  'services',
  'boss',
  'internal',
  'skillinstall',
  'skills',
  'boss-plan',
  'references',
  'headless-drafting-brief.md',
)

function byString(a, b) {
  if (a < b) return -1
  if (a > b) return 1
  return 0
}

/** Recursively sorts object keys (arrays keep their order — order can be the signature). */
function sortKeysDeep(value) {
  if (Array.isArray(value)) return value.map(sortKeysDeep)
  if (value && typeof value === 'object') {
    const sorted = {}
    for (const key of Object.keys(value).sort()) sorted[key] = sortKeysDeep(value[key])
    return sorted
  }
  return value
}

/** Stable, deterministic JSON serialization: sorted keys, 2-space pretty, trailing newline. */
export function stableStringify(value) {
  return JSON.stringify(sortKeysDeep(value), null, 2) + '\n'
}

// --- 1. boss-review lens selection -----------------------------------------

/**
 * @param {string[]} changedFiles
 * @returns {{ matchedLensIds: string[], lenses: Array<{lens:string,skill:string,fallbackRubric:string,files:string[]}> }}
 */
export function computeReviewLensSignature(changedFiles) {
  const lensMap = loadSkillConfig({ cwd: REPO_ROOT }).lensMap
  const matched = matchLenses(changedFiles, lensMap)
  const lenses = matched
    .map((m) => ({
      lens: m.lens,
      skill: m.skill,
      fallbackRubric: m.fallbackRubric,
      files: [...m.files].sort(byString),
    }))
    .sort((a, b) => byString(a.lens, b.lens))
  return { matchedLensIds: lenses.map((l) => l.lens), lenses }
}

// --- 2. proof surface classification ----------------------------------------

/**
 * @param {string[]} changedFiles
 * @param {object} catalog parsed proof/recipes/default.json
 * @returns {{ tui: boolean, web: boolean, recipes: Array<{id:string,surface:string}> }}
 */
export function computeProofSurfaceSignature(changedFiles, catalog) {
  const { tui, web, recipes } = classifySurfaces({ changedFiles, catalog })
  const sortedRecipes = recipes
    .map((r) => ({ id: r.id, surface: r.surface }))
    .sort((a, b) => byString(a.id, b.id))
  return { tui, web, recipes: sortedRecipes }
}

// --- 3. bs-epic DAG schedule -------------------------------------------------

/**
 * @param {{nodes:Array<{id:string,blockedBy:string[],priority:number,createdAt:string}>,
 *   merged:string[], failed:string[], inFlight:string[], externallyCleared:string[]}} fixture
 * @returns {{ readyIds: string[], mergeOrder: string[] }}
 */
export function computeDagScheduleSignature(fixture) {
  const graph = buildGraph(fixture.nodes)
  const state = {
    merged: new Set(fixture.merged ?? []),
    failed: new Set(fixture.failed ?? []),
    inFlight: new Set(fixture.inFlight ?? []),
    externallyCleared: new Set(fixture.externallyCleared ?? []),
  }
  const readyIds = readyTickets(graph, state).map((n) => n.id)

  const merged = new Set(state.merged)
  const mergeOrder = []
  for (let i = 0; i < fixture.nodes.length; i++) {
    const greens = fixture.nodes.filter((n) => !merged.has(n.id))
    const nextId = nextToMerge(greens, graph, merged)
    if (nextId === null) break
    mergeOrder.push(nextId)
    merged.add(nextId)
  }
  return { readyIds, mergeOrder }
}

// --- 4. boss-plan section contract ------------------------------------------

/**
 * @param {string} markdown the full headless-drafting-brief.md content
 * @returns {{ sections: string[] }}
 */
export function computePlanSectionsSignature(markdown) {
  const stepIdx = markdown.indexOf('## Step 7')
  if (stepIdx === -1) {
    throw new Error('capture-baseline: "## Step 7" heading not found in headless-drafting-brief.md')
  }
  const afterStep = markdown.slice(stepIdx)
  const fenceMarker = '```markdown'
  const fenceStart = afterStep.indexOf(fenceMarker)
  if (fenceStart === -1) {
    throw new Error('capture-baseline: no ```markdown fence found under "## Step 7"')
  }
  const afterFenceOpen = afterStep.slice(fenceStart + fenceMarker.length)
  const fenceEnd = afterFenceOpen.indexOf('```')
  if (fenceEnd === -1) {
    throw new Error('capture-baseline: unterminated ```markdown fence under "## Step 7"')
  }
  const fenceBody = afterFenceOpen.slice(0, fenceEnd)
  const sections = fenceBody.split('\n').filter((line) => /^## /.test(line))
  return { sections }
}

// --- Fixture I/O + orchestration --------------------------------------------

function readJsonFixture(name) {
  return JSON.parse(readFileSync(join(BASELINE_DIR, name), 'utf8'))
}

/** Computes all four signatures from their committed real inputs. Pure aside from the reads. */
export function computeAll() {
  const reviewLensFixture = readJsonFixture('review-lens.input.json')
  const proofSurfaceFixture = readJsonFixture('proof-surface.input.json')
  const dagScheduleFixture = readJsonFixture('dag-schedule.input.json')
  const proofCatalog = JSON.parse(readFileSync(PROOF_CATALOG_PATH, 'utf8'))
  const planBrief = readFileSync(PLAN_BRIEF_PATH, 'utf8')

  return {
    'review-lens.snapshot.json': computeReviewLensSignature(reviewLensFixture),
    'proof-surface.snapshot.json': computeProofSurfaceSignature(proofSurfaceFixture, proofCatalog),
    'dag-schedule.snapshot.json': computeDagScheduleSignature(dagScheduleFixture),
    'plan-sections.snapshot.json': computePlanSectionsSignature(planBrief),
  }
}

/** Computes and writes all four snapshots (2-space pretty, sorted keys) into `outDir`. */
export function writeAll(outDir = BASELINE_DIR) {
  const snapshots = computeAll()
  for (const [filename, value] of Object.entries(snapshots)) {
    writeFileSync(join(outDir, filename), stableStringify(value))
  }
  return snapshots
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const snapshots = writeAll()
  console.log(`wrote ${Object.keys(snapshots).length} snapshots to ${BASELINE_DIR}`)
}
