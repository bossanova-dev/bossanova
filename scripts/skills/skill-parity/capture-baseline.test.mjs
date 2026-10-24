import assert from 'node:assert'
import { mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'

import {
  buildGraph,
  nextToMerge,
  readyTickets,
} from '../../../lib/bossalib/contract/plugin/skillpayload/skills/boss-epic/toolbox/epic/dag-scheduler.mjs'
import {
  BASELINE_DIR,
  computeDagScheduleSignature,
  stableStringify,
  writeAll,
} from './capture-baseline.mjs'

const SNAPSHOT_NAMES = ['dag-schedule.snapshot.json']

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
    assert.deepEqual(readdirSync(dirA).sort(), [...SNAPSHOT_NAMES].sort())
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

  const dagSchedule = JSON.parse(readSnapshot('dag-schedule.snapshot.json'))
  assert.ok(Array.isArray(dagSchedule.readyIds) && dagSchedule.readyIds.length > 0)
  assert.ok(Array.isArray(dagSchedule.mergeOrder) && dagSchedule.mergeOrder.length > 0)
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

test('stableStringify sorts object keys recursively but preserves array order', () => {
  const value = { b: 1, a: [{ z: 1, y: 2 }, 'keep-order'] }
  assert.equal(
    stableStringify(value),
    '{\n  "a": [\n    {\n      "y": 2,\n      "z": 1\n    },\n    "keep-order"\n  ],\n  "b": 1\n}\n',
  )
})
