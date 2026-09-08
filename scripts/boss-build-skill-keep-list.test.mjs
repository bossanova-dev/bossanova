// Tests for scripts/boss-build-keep-list.mjs — the BOS-1215 keep-list presence gate.
//
// EVERY assertion here is over a SUBPROCESS RESULT, never over skill markdown. That is deliberate
// and structural, not stylistic: this file is named to match `check-prose-pins.mjs`'s own
// GATE_FILE_GLOB (`scripts/*skill*.test.mjs`), so it IS scanned by that gate rather than exempt
// from it, and it must score zero prose pins to pass with no baseline entry. A file that asserted
// `assert.match(skillMarkdown, /…/)` here would red the gate; asserting the helper's verdict does
// not. See docs/skills/prose-pins.md.
//
// The falsification suite is the half that matters. A presence check that cannot be shown to FIRE
// is indistinguishable from one whose regexes never matched anything — the exact vacuous-gate class
// this repository keeps paying for. So for every keep-list item, a fixture payload is built with
// that one item's prose removed, and the helper is required to red AND to name that item.

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import {
  ADJUDICATED,
  DEFAULT_PAYLOAD_ROOT,
  KEEP_LIST,
  RESIDENT_BODY,
  FINALIZE_REFERENCE,
} from './boss-build-keep-list.mjs'

const rootDir = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')
const gate = path.join(rootDir, 'scripts', 'boss-build-keep-list.mjs')
const payloadRoot = path.join(rootDir, ...DEFAULT_PAYLOAD_ROOT.split('/'))

/** Run the gate, returning its exit status and combined output. */
function runGate(args = []) {
  const result = spawnSync(process.execPath, [gate, ...args], { encoding: 'utf8' })
  return { code: result.status, out: `${result.stdout}${result.stderr}` }
}

/**
 * Copy the two payload files the keep list reads into a scratch payload tree, then delete every
 * line of `item.file` that any of `item`'s probes matches. That is "removing one keep-list item":
 * the behaviour's own statements go, and nothing else is touched.
 */
function fixtureWithItemRemoved(item) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-build-keep-list-'))
  fs.mkdirSync(path.join(dir, 'references'), { recursive: true })

  for (const relative of [RESIDENT_BODY, FINALIZE_REFERENCE]) {
    const from = path.join(payloadRoot, ...relative.split('/'))
    const to = path.join(dir, ...relative.split('/'))
    let content = fs.readFileSync(from, 'utf8')
    if (relative === item.file) {
      content = content
        .split('\n')
        .filter((line) => !item.probes.some((probe) => probe.pattern.test(line)))
        .join('\n')
    }
    fs.writeFileSync(to, content)
  }
  return dir
}

test('BOS-1215: the keep-list gate passes against the checked-out payload', () => {
  const { code, out } = runGate()

  assert.equal(code, 0, `keep-list gate failed against the real payload:\n${out}`)
  assert.match(out, /boss-build-keep-list:\s+OK\s+\d+\/\d+\s+keep-list\s+items\s+present\./)
  // The count in the verdict is the whole list, so an item silently dropped from KEEP_LIST shows
  // up here as a smaller denominator rather than as a still-green run.
  assert.match(out, new RegExp(`OK ${KEEP_LIST.length}/${KEEP_LIST.length} `))
})

test('BOS-1215: the gate reports where each item lives, including the non-resident one', () => {
  const { out } = runGate()

  // "Step 12 notes capture" is NOT in the resident body. A gate that looked for it there would red
  // on a correct payload, so the file it is checked in is part of the asserted verdict.
  assert.match(out, /present\s+step-12-notes-capture\s+\(references\/finalize-and-stop\.md/)
  const resident = KEEP_LIST.filter((i) => i.file === RESIDENT_BODY)
  for (const item of resident) {
    assert.match(out, new RegExp(`present ${item.id} \\(SKILL\\.md`))
  }
})

test('BOS-1215: gate-run.mjs is adjudicated out of the payload, not silently dropped', () => {
  const { code, out } = runGate()

  // The epic's keep list names `gate-run.mjs`. It is not in the payload and must not be added: a
  // published core stays project-agnostic. The honest handling is a recorded decision that the
  // verdict PRINTS, so a reader can see the item was considered rather than forgotten.
  assert.equal(code, 0)
  assert.match(out, /adjudicated\s+gate-run-mjs:\s+not-in-payload/)
  assert.match(out, /published\s+core/)
  assert.equal(ADJUDICATED.length, 1)
})

test('BOS-1215: the gate discloses what a green run does not cover', () => {
  const { out } = runGate()

  assert.match(out, /RESIDUAL:\s+a\s+green\s+run\s+means\s+each\s+keep-list\s+probe\s+matched/)
})

// --- Falsification: the gate must be shown able to fire, per item -------------------------------

for (const item of KEEP_LIST) {
  test(`BOS-1215 falsification: removing ${item.id} reds the gate`, () => {
    const dir = fixtureWithItemRemoved(item)
    try {
      const { code, out } = runGate(['--payload', dir])

      assert.equal(code, 1, `gate stayed green after ${item.id} was removed:\n${out}`)
      assert.match(out, new RegExp(`MISSING ${item.id} `))
      // The remedy has to say what was lost, or a red is just a number.
      assert.match(
        out,
        new RegExp(`MISSING ${item.id} \\(${item.file.replace(/[/.]/g, '\\$&')}\\)`),
      )
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })
}

test('BOS-1215 falsification: an absent payload file reds rather than passing vacuously', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-build-keep-list-empty-'))
  try {
    const { code, out } = runGate(['--payload', dir])

    assert.equal(code, 1)
    assert.match(out, /<file\s+absent>/)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('BOS-1215: JSON mode reports the same verdict a caller can branch on', () => {
  const result = spawnSync(process.execPath, [gate, '--json'], { encoding: 'utf8' })
  const parsed = JSON.parse(result.stdout)

  assert.equal(result.status, 0)
  assert.equal(parsed.ok, true)
  assert.equal(parsed.items.length, KEEP_LIST.length)
  assert.deepEqual(
    parsed.items.filter((i) => i.status !== 'present'),
    [],
  )
  assert.equal(parsed.adjudicated[0].id, 'gate-run-mjs')
})
