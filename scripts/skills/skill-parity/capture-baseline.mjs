#!/usr/bin/env node

// Pre-generalization skill-behavior baseline capture (BOS-249). Records the
// CURRENT (pre-generalization) deterministic behavior of a Bossanova skill
// function into a committed JSON snapshot under scripts/skills/skill-parity/baseline/.
// A later ticket (BOS-248) re-runs the post-refactor skill functions over the
// SAME inputs and asserts the outputs still match these snapshots — so this
// module intentionally imports TODAY'S real source paths (never the
// generalized/extracted successors): the snapshots are what must survive the
// refactor, not this capture script. Node builtins only (no new deps).
//
// INPUT MODEL — the signature's scenario input is a committed fixture
// (dag-schedule.input.json) and the signature is fully self-contained: nothing is
// read live from the working tree.
//
// Snapshot schema (consumed by BOS-248's parity-gate.mjs without
// reverse-engineering):
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

import {
  buildGraph,
  nextToMerge,
  readyTickets,
} from '../../../lib/bossalib/contract/plugin/skillpayload/skills/boss-epic/toolbox/epic/dag-scheduler.mjs'

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url))
export const REPO_ROOT = join(SCRIPT_DIR, '../../..')
export const BASELINE_DIR = join(SCRIPT_DIR, 'baseline')

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

// --- bs-epic DAG schedule -------------------------------------------------

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
  const dagScheduleFixture = readJsonFixture('dag-schedule.input.json')

  return {
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

import { isMainModule } from '../../../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  const snapshots = writeAll()
  console.log(`wrote ${Object.keys(snapshots).length} snapshots to ${BASELINE_DIR}`)
}
