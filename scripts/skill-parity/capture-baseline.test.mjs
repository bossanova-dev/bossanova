import assert from 'node:assert'
import { mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'

import { matchLenses } from '../../services/boss/internal/skillinstall/skills/boss-review/toolbox/bs-review-detect.mjs'
import { loadSkillConfig } from '../../services/boss/internal/skillinstall/skills/boss-review/toolbox/skill-config.mjs'
import { classifySurfaces } from '../proof-surfaces.mjs'
import {
  buildGraph,
  nextToMerge,
  readyTickets,
} from '../../services/boss/internal/skillinstall/skills/boss-epic/toolbox/dag-scheduler.mjs'
import {
  BASELINE_DIR,
  REPO_ROOT,
  computeDagScheduleSignature,
  computePlanSectionsSignature,
  computeProofSurfaceSignature,
  computeReviewLensSignature,
  stableStringify,
  writeAll,
} from './capture-baseline.mjs'

const SNAPSHOT_NAMES = [
  'review-lens.snapshot.json',
  'proof-surface.snapshot.json',
  'dag-schedule.snapshot.json',
  'plan-sections.snapshot.json',
]

function readFixture(name) {
  return JSON.parse(readFileSync(join(BASELINE_DIR, name), 'utf8'))
}

function readSnapshot(name) {
  return readFileSync(join(BASELINE_DIR, name), 'utf8')
}

test('determinism: writeAll produces byte-identical output on repeated runs, matching the committed snapshots', () => {
  const dirA = mkdtempSync(join(tmpdir(), 'skill-parity-a-'))
  const dirB = mkdtempSync(join(tmpdir(), 'skill-parity-b-'))
  try {
    writeAll(dirA)
    writeAll(dirB)
    for (const name of SNAPSHOT_NAMES) {
      const bytesA = readFileSync(join(dirA, name), 'utf8')
      const bytesB = readFileSync(join(dirB, name), 'utf8')
      assert.equal(bytesA, bytesB, `${name}: two capture runs produced different bytes`)
      assert.equal(
        bytesA,
        readSnapshot(name),
        `${name}: capture output no longer matches the committed snapshot`,
      )
    }
  } finally {
    rmSync(dirA, { recursive: true, force: true })
    rmSync(dirB, { recursive: true, force: true })
  }
})

test('well-formedness: every committed snapshot is present, valid JSON, and non-empty', () => {
  const files = readdirSync(BASELINE_DIR)
  for (const name of SNAPSHOT_NAMES) {
    assert.ok(files.includes(name), `missing snapshot file ${name}`)
  }

  const reviewLens = JSON.parse(readSnapshot('review-lens.snapshot.json'))
  assert.ok(Array.isArray(reviewLens.matchedLensIds) && reviewLens.matchedLensIds.length > 0)
  assert.ok(Array.isArray(reviewLens.lenses) && reviewLens.lenses.length > 0)
  for (const entry of reviewLens.lenses) {
    assert.equal(typeof entry.lens, 'string')
    assert.equal(typeof entry.skill, 'string')
    assert.equal(typeof entry.fallbackRubric, 'string')
    assert.ok(Array.isArray(entry.files) && entry.files.length > 0)
  }

  const proofSurface = JSON.parse(readSnapshot('proof-surface.snapshot.json'))
  assert.equal(typeof proofSurface.tui, 'boolean')
  assert.equal(typeof proofSurface.web, 'boolean')
  assert.ok(Array.isArray(proofSurface.recipes) && proofSurface.recipes.length > 0)
  for (const recipe of proofSurface.recipes) {
    assert.equal(typeof recipe.id, 'string')
    assert.equal(typeof recipe.surface, 'string')
  }

  const dagSchedule = JSON.parse(readSnapshot('dag-schedule.snapshot.json'))
  assert.ok(Array.isArray(dagSchedule.readyIds) && dagSchedule.readyIds.length > 0)
  assert.ok(Array.isArray(dagSchedule.mergeOrder) && dagSchedule.mergeOrder.length > 0)

  const planSections = JSON.parse(readSnapshot('plan-sections.snapshot.json'))
  assert.ok(Array.isArray(planSections.sections) && planSections.sections.length > 0)
})

test('signature: boss-review matchLenses over the committed fixture matches the committed snapshot', () => {
  const changedFiles = readFixture('review-lens.input.json')
  const lensMap = loadSkillConfig({ cwd: REPO_ROOT }).lensMap
  const rawMatched = matchLenses(changedFiles, lensMap)
  const expectedIds = [...new Set(rawMatched.map((m) => m.lens))].sort()

  const result = computeReviewLensSignature(changedFiles)
  assert.deepEqual(result.matchedLensIds, expectedIds)
  assert.deepEqual(result, JSON.parse(readSnapshot('review-lens.snapshot.json')))
  // Today's real fixture is designed to exercise all three lenses.
  assert.deepEqual(result.matchedLensIds, ['go', 'tui', 'web'])
})

test('signature: proof-surfaces classifySurfaces over the committed fixture matches the committed snapshot', () => {
  const changedFiles = readFixture('proof-surface.input.json')
  const catalog = JSON.parse(
    readFileSync(join(REPO_ROOT, 'proof', 'recipes', 'default.json'), 'utf8'),
  )
  const raw = classifySurfaces({ changedFiles, catalog })

  const result = computeProofSurfaceSignature(changedFiles, catalog)
  assert.equal(result.tui, raw.tui)
  assert.equal(result.web, raw.web)
  assert.equal(result.recipes.length, raw.recipes.length)
  assert.deepEqual(result, JSON.parse(readSnapshot('proof-surface.snapshot.json')))
  // Today's real fixture is designed to trip the TUI prefix, the web prefix,
  // and both the marketing + docs pathRules.
  assert.equal(result.tui, true)
  assert.equal(result.web, true)
})

test('signature: dag-scheduler ready/merge order over the committed fixture matches the committed snapshot', () => {
  const fixture = readFixture('dag-schedule.input.json')
  const graph = buildGraph(fixture.nodes)
  const rawReady = readyTickets(graph, {
    merged: new Set(fixture.merged),
    failed: new Set(fixture.failed),
    inFlight: new Set(fixture.inFlight),
    externallyCleared: new Set(fixture.externallyCleared),
  }).map((n) => n.id)

  const result = computeDagScheduleSignature(fixture)
  assert.deepEqual(result.readyIds, rawReady)
  assert.deepEqual(result, JSON.parse(readSnapshot('dag-schedule.snapshot.json')))

  // Independently re-derive the merge order from the real nextToMerge() to prove
  // the snapshot reflects real scheduler behavior, not just the compute helper.
  const merged = new Set(fixture.merged)
  const mergeOrder = []
  for (let i = 0; i < fixture.nodes.length; i++) {
    const greens = fixture.nodes.filter((n) => !merged.has(n.id))
    const nextId = nextToMerge(greens, graph, merged)
    if (nextId === null) break
    mergeOrder.push(nextId)
    merged.add(nextId)
  }
  assert.deepEqual(result.mergeOrder, mergeOrder)
})

test("signature: boss-plan Step 7 section contract matches the committed snapshot and today's known headings", () => {
  const markdown = readFileSync(
    join(
      REPO_ROOT,
      'services',
      'boss',
      'internal',
      'skillinstall',
      'skills',
      'boss-plan',
      'references',
      'headless-drafting-brief.md',
    ),
    'utf8',
  )
  const result = computePlanSectionsSignature(markdown)
  assert.deepEqual(result, JSON.parse(readSnapshot('plan-sections.snapshot.json')))
  assert.deepEqual(result.sections, [
    '## Summary',
    '## Approach',
    '## Key changes',
    '## Testing',
    '## Risks / unknowns',
    '## Acceptance criteria',
    '## Required proof',
    '## Proof harness analysis',
    '## Why this needs a human',
    '## Open Questions',
    '## Planning',
    '## Original notes',
  ])
})

test('stableStringify sorts object keys recursively but preserves array order', () => {
  const value = { b: 1, a: [{ z: 1, y: 2 }, 'keep-order'] }
  assert.equal(
    stableStringify(value),
    '{\n  "a": [\n    {\n      "y": 2,\n      "z": 1\n    },\n    "keep-order"\n  ],\n  "b": 1\n}\n',
  )
})
