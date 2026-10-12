#!/usr/bin/env node

// Pre-generalization skill-behavior baseline capture (BOS-249). Records the
// CURRENT (pre-generalization) deterministic behavior of two Bossanova skill
// functions into committed JSON snapshots under scripts/skill-parity/baseline/.
// A later ticket (BOS-248) re-runs the post-refactor skill functions over the
// SAME inputs and asserts the outputs still match these snapshots — so this
// module intentionally imports TODAY'S real source paths (never the
// generalized/extracted successors): the snapshots are what must survive the
// refactor, not this capture script. Node builtins only (no new deps).
//
// INPUT MODEL — committed fixtures vs. live-read reference sources. Each
// signature has a *scenario* input (frozen) and, for one of the two, a *reference
// source* read live from the working tree (NOT frozen):
//
//   signature       scenario input (committed)          reference source (read live)
//   ------------     ------------------------------      ---------------------------------
//   review-lens      review-lens.input.json (files)      .boss-skills.json lensMap
//   dag-schedule     dag-schedule.input.json (nodes)     — (fully self-contained)
//
// This is deliberate: the signature is "what TODAY'S real skill (real registry
// included) produces," so the reference source is read live on purpose. The
// consequence a consumer must know: .boss-skills.json is NOT in test-scripts.yml's
// path filter, so a lensMap-only edit drifts review-lens.snapshot.json silently
// until some other scripts/skills change re-triggers the gate; that edit is then
// expected to re-run the capture and re-commit the baseline as a DELIBERATE
// rebaseline. A fully-hermetic alternative — freezing a copy of the lensMap
// alongside the scenario inputs so the parity comparison isolates code drift from
// config drift — is a BOS-248 contract decision left to a human, out of scope for
// this capture ticket.
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
//
// Determinism rules: no timestamps/Date.now/Math.random/absolute paths/env
// values in any snapshot; every set-like collection is sorted with a fixed
// comparator; order is preserved only where order IS the signature (dag ready/
// merge order); every object's keys are recursively sorted
// before stringifying so re-running yields byte-identical files.

import { readFileSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { matchLenses } from '../../services/boss/internal/skillinstall/skills/boss-review/toolbox/bs-review-detect.mjs'
import { loadSkillConfig } from '../../services/boss/internal/skillinstall/skills/boss-review/toolbox/skill-config.mjs'
import {
  buildGraph,
  nextToMerge,
  readyTickets,
} from '../../services/boss/internal/skillinstall/skills/boss-epic/toolbox/dag-scheduler.mjs'

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url))
export const REPO_ROOT = join(SCRIPT_DIR, '..', '..')
export const BASELINE_DIR = join(SCRIPT_DIR, 'baseline')

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

// --- 2. bs-epic DAG schedule -------------------------------------------------

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

// --- Fixture I/O + orchestration --------------------------------------------

function readJsonFixture(name) {
  return JSON.parse(readFileSync(join(BASELINE_DIR, name), 'utf8'))
}

/** Computes every signature from its committed real inputs. Pure aside from the reads. */
export function computeAll() {
  const reviewLensFixture = readJsonFixture('review-lens.input.json')
  const dagScheduleFixture = readJsonFixture('dag-schedule.input.json')

  return {
    'review-lens.snapshot.json': computeReviewLensSignature(reviewLensFixture),
    'dag-schedule.snapshot.json': computeDagScheduleSignature(dagScheduleFixture),
  }
}

/** Computes and writes every snapshot (2-space pretty, sorted keys) into `outDir`. */
export function writeAll(outDir = BASELINE_DIR) {
  const snapshots = computeAll()
  for (const [filename, value] of Object.entries(snapshots)) {
    writeFileSync(join(outDir, filename), stableStringify(value))
  }
  return snapshots
}

import { isMainModule } from '../../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  const snapshots = writeAll()
  console.log(`wrote ${Object.keys(snapshots).length} snapshots to ${BASELINE_DIR}`)
}
