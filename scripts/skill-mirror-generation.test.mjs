#!/usr/bin/env node

// BOS-1212: the plugin skill payload is an `rsync -a --delete` of the canonical skillinstall
// tree (`make copy-skills`). The skill suites used to re-assert every clause against BOTH
// directories — a per-clause parity loop that cost two assertions to add and two to remove
// while distinguishing no state a single assertion could not, EXCEPT a broken rsync. This file
// is that exception, asserted once for the whole tree instead of once per clause (see the
// second-owner note below — the byte-comparison half is shared with a pre-existing Go gate):
//
//   1. the premise — `copy-skills` really is an rsync of SRC into DEST (read from the Makefile,
//      so the day someone replaces the recipe with something hand-maintained, this reddens);
//   2. the committed mirror matches its canonical source, every path and every byte;
//   3. running the generation twice produces the same tree (idempotence);
//   4. the comparison is shown able to FIRE — a perturbed fixture must go red.
//
// (4) is not decoration. One tree comparison standing in for ~100 loops is exactly the shape
// that ends up vacuous, so the falsification fixtures are a first-class gate here: they perturb
// a temp copy four ways (changed byte, extra file, missing file, executable-bit flip) and
// assert each is reported.
//
// NOT THE ONLY OWNER. services/boss/internal/skillparity/parity_test.go
// (`TestSkillPayloadParity`, BOS-344) already owns whole-tree path-set plus sha256 content
// parity between the same two directories, and it predates this file — so (2) above is a second
// reading of an invariant Go already gates, not the first. What this file adds that the Go gate
// does NOT have: the `copy-skills` recipe premise pin (1), idempotence (3), `--delete`
// semantics, the four falsification fixtures (4), and sensitivity to the executable bit and to
// symlink-vs-file kind. The exec bit is load-bearing rather than theoretical — the payload ships
// three executable files (boss-finalize/add-pr-numbers.sh, boss-build/toolbox/worktree-lock.sh,
// boss-repair/scripts/review-feedback-probe.js) whose mode the Go manifest does not record.
// The two deliberately disagree on three details, so expect one to red where the other does not:
// parity_test.go skips `.gitkeep` and records neither mode nor symlink kind; `listTree`/
// `hashTree` below skip nothing and record both.
//
// NOT COVERED by either: that the canonical SOURCE is correct. This proves only that the mirror is
// what the generator produces from it — a wrong source produces a faithful wrong mirror. The clause
// gates in the per-skill suites are what pin the source; they now run against the canonical
// directory ONLY, because this file and the Go gate are what make the second run redundant.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import crypto from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const rootDir = fileURLToPath(new URL('..', import.meta.url))

const SKILLS_SRC_DIR = 'services/boss/internal/skillinstall/skills'
const SKILLS_PLUGIN_DIR = 'plugins/bossd-plugin-claude/skilldata/skills'

const abs = (rel) => path.join(rootDir, rel)

/**
 * Every file under `dir`, as a sorted list of `/`-separated paths relative to `dir`.
 * Symlinks are recorded as their own kind so a symlink swapped for a regular file of the
 * same bytes is still a difference.
 */
function listTree(dir) {
  const out = []
  const walk = (current, prefix) => {
    const entries = fs
      .readdirSync(current, { withFileTypes: true })
      .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
    for (const entry of entries) {
      const full = path.join(current, entry.name)
      const rel = prefix ? `${prefix}/${entry.name}` : entry.name
      if (entry.isDirectory()) {
        walk(full, rel)
      } else {
        out.push(rel)
      }
    }
  }
  walk(dir, '')
  return out.sort()
}

/** Map of relative path -> sha256 of the file's bytes (plus a mode/kind marker). */
function hashTree(dir) {
  const hashes = new Map()
  for (const rel of listTree(dir)) {
    const full = path.join(dir, rel)
    const stat = fs.lstatSync(full)
    const kind = stat.isSymbolicLink() ? 'link' : 'file'
    const bytes = stat.isSymbolicLink() ? Buffer.from(fs.readlinkSync(full)) : fs.readFileSync(full)
    // The executable bit ships with the payload (toolbox helpers), so it is part of identity.
    const exec = !stat.isSymbolicLink() && (stat.mode & 0o111) !== 0 ? 'x' : '-'
    hashes.set(rel, `${kind}:${exec}:${crypto.createHash('sha256').update(bytes).digest('hex')}`)
  }
  return hashes
}

/**
 * Compare two trees by content. Returns the three ways they can differ; an empty
 * result in all three is byte-for-byte identity over every path.
 */
function diffTrees(expectedDir, actualDir) {
  const expected = hashTree(expectedDir)
  const actual = hashTree(actualDir)
  const missing = []
  const extra = []
  const differing = []
  for (const [rel, hash] of expected) {
    if (!actual.has(rel)) missing.push(rel)
    else if (actual.get(rel) !== hash) differing.push(rel)
  }
  for (const rel of actual.keys()) {
    if (!expected.has(rel)) extra.push(rel)
  }
  return { differing: differing.sort(), extra: extra.sort(), missing: missing.sort() }
}

/** Human-readable diff summary, capped so a wholesale drift cannot flood the reporter. */
function describeDiff({ differing, extra, missing }) {
  const cap = (list) =>
    list.length > 10
      ? `${list.slice(0, 10).join(', ')} (+${list.length - 10} more)`
      : list.join(', ')
  const parts = []
  if (missing.length) parts.push(`missing from the mirror: ${cap(missing)}`)
  if (extra.length) parts.push(`present only in the mirror: ${cap(extra)}`)
  if (differing.length) parts.push(`bytes differ: ${cap(differing)}`)
  return parts.join(' | ')
}

function isClean(diff) {
  return diff.differing.length === 0 && diff.extra.length === 0 && diff.missing.length === 0
}

/** Run the same generation `make copy-skills` runs, into an arbitrary destination. */
function generateInto(destDir) {
  fs.mkdirSync(destDir, { recursive: true })
  execFileSync('rsync', ['-a', '--delete', `${abs(SKILLS_SRC_DIR)}/`, `${destDir}/`])
}

const withTempDir = (fn) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'skill-mirror-'))
  try {
    return fn(dir)
  } finally {
    fs.rmSync(dir, { force: true, recursive: true })
  }
}

// ── The premise ────────────────────────────────────────────────────────────────────────────

test('BOS-1212: `make copy-skills` generates the plugin mirror by rsync from the canonical tree', () => {
  const makefile = fs.readFileSync(abs('Makefile'), 'utf8')

  assert.match(
    makefile,
    /^SKILLS_SRC_DIR := services\/boss\/internal\/skillinstall\/skills$/m,
    'SKILLS_SRC_DIR must still name the canonical skillinstall payload',
  )
  assert.match(
    makefile,
    /^SKILLS_PLUGIN_DIR := plugins\/bossd-plugin-claude\/skilldata\/skills$/m,
    'SKILLS_PLUGIN_DIR must still name the bossd-plugin-claude mirror',
  )
  // The whole premise of removing the per-clause parity loops is that the mirror is GENERATED.
  // If the recipe ever stops being a full-tree mirroring copy, the mirror becomes a place a
  // clause can be hand-edited and forgotten, and the deleted loops would need to come back.
  assert.match(
    makefile,
    /^copy-skills:\n\t@rsync -a --delete "\$\(SKILLS_SRC_DIR\)\/" "\$\(SKILLS_PLUGIN_DIR\)\/"$/m,
    'copy-skills must remain `rsync -a --delete SRC/ DEST/` — a full-tree generated mirror. ' +
      'If this recipe changed, the per-clause mirror parity loops removed in BOS-1212 are no ' +
      'longer redundant and this file is no longer sufficient.',
  )
})

// ── Non-vacuity ────────────────────────────────────────────────────────────────────────────

test('BOS-1212: the canonical payload is non-empty and carries the published cores', () => {
  const files = listTree(abs(SKILLS_SRC_DIR))
  assert.ok(
    files.length > 50,
    `the canonical skill payload has ${files.length} files — too few for the comparison below ` +
      'to mean anything. An empty or truncated tree makes every assertion here pass vacuously.',
  )
  for (const core of [
    'boss',
    'boss-build',
    'boss-epic',
    'boss-plan',
    'boss-repair',
    'boss-review',
  ]) {
    assert.ok(
      files.some((rel) => rel === `${core}/SKILL.md`),
      `${core}/SKILL.md must be present in the canonical payload`,
    )
  }
})

// ── The mirror matches its source ──────────────────────────────────────────────────────────

test('BOS-1212: the committed plugin mirror is byte-identical to the canonical tree', () => {
  const diff = diffTrees(abs(SKILLS_SRC_DIR), abs(SKILLS_PLUGIN_DIR))
  assert.ok(
    isClean(diff),
    `${SKILLS_PLUGIN_DIR} has drifted from ${SKILLS_SRC_DIR}: ${describeDiff(diff)}. It is a ` +
      'GENERATED mirror, so this is a sync failure, not a content problem: run `make ' +
      'copy-skills` and stage the result. If the change you wanted belongs in the payload, ' +
      'make it in the canonical tree first — never hand-edit the mirror.',
  )
})

// ── Idempotence ────────────────────────────────────────────────────────────────────────────

test('BOS-1212: running the generation twice produces the same tree', () => {
  withTempDir((dir) => {
    const dest = path.join(dir, 'mirror')

    generateInto(dest)
    const first = hashTree(dest)

    generateInto(dest)
    const second = hashTree(dest)

    assert.deepEqual(
      [...second.entries()].sort(),
      [...first.entries()].sort(),
      'a second `copy-skills` over an already-generated tree changed it — the generation is ' +
        'not idempotent, so the committed mirror cannot be trusted to equal its source.',
    )

    const diff = diffTrees(abs(SKILLS_SRC_DIR), dest)
    assert.ok(
      isClean(diff),
      `a freshly generated tree does not match its source: ${describeDiff(diff)}`,
    )
  })
})

test('BOS-1212: the generation replaces a dirty destination rather than merging into it', () => {
  withTempDir((dir) => {
    const dest = path.join(dir, 'mirror')

    // `--delete` is what makes the mirror a projection of the source rather than an accumulator.
    // Without it a file deleted from the canonical tree would survive forever in the mirror, and
    // a whole-tree comparison would be the only thing that ever noticed.
    fs.mkdirSync(path.join(dest, 'stale-core'), { recursive: true })
    fs.writeFileSync(path.join(dest, 'stale-core', 'SKILL.md'), '# removed upstream\n')

    generateInto(dest)

    const diff = diffTrees(abs(SKILLS_SRC_DIR), dest)
    assert.ok(isClean(diff), `the generation left the destination dirty: ${describeDiff(diff)}`)
  })
})

// ── Falsification: the comparison must be able to fire ─────────────────────────────────────

test('BOS-1212: the tree comparison reports a changed byte', () => {
  withTempDir((dir) => {
    const dest = path.join(dir, 'mirror')
    generateInto(dest)
    assert.ok(isClean(diffTrees(abs(SKILLS_SRC_DIR), dest)), 'fixture must start clean')

    const victim = path.join(dest, 'boss-build', 'SKILL.md')
    fs.appendFileSync(victim, '\n<!-- perturbed -->\n')

    const diff = diffTrees(abs(SKILLS_SRC_DIR), dest)
    assert.deepEqual(diff.differing, ['boss-build/SKILL.md'])
    assert.deepEqual(diff.missing, [])
    assert.deepEqual(diff.extra, [])
    assert.match(describeDiff(diff), /bytes\s+differ:\s+boss-build\/SKILL\.md/)
  })
})

test('BOS-1212: the tree comparison reports a file present only in the mirror', () => {
  withTempDir((dir) => {
    const dest = path.join(dir, 'mirror')
    generateInto(dest)

    fs.writeFileSync(path.join(dest, 'boss-build', 'SMUGGLED.md'), '# hand-added\n')

    const diff = diffTrees(abs(SKILLS_SRC_DIR), dest)
    assert.deepEqual(diff.extra, ['boss-build/SMUGGLED.md'])
    assert.deepEqual(diff.differing, [])
    assert.deepEqual(diff.missing, [])
  })
})

test('BOS-1212: the tree comparison reports a file missing from the mirror', () => {
  withTempDir((dir) => {
    const dest = path.join(dir, 'mirror')
    generateInto(dest)

    fs.rmSync(path.join(dest, 'boss-build', 'SKILL.md'))

    const diff = diffTrees(abs(SKILLS_SRC_DIR), dest)
    assert.deepEqual(diff.missing, ['boss-build/SKILL.md'])
    assert.deepEqual(diff.differing, [])
    assert.deepEqual(diff.extra, [])
  })
})

test('BOS-1212: the tree comparison reports an executable-bit flip', () => {
  withTempDir((dir) => {
    const dest = path.join(dir, 'mirror')
    generateInto(dest)

    const victim = path.join(dest, 'boss-build', 'SKILL.md')
    fs.chmodSync(victim, fs.statSync(victim).mode | 0o111)

    const diff = diffTrees(abs(SKILLS_SRC_DIR), dest)
    assert.deepEqual(
      diff.differing,
      ['boss-build/SKILL.md'],
      'a mode change with identical bytes must still be reported — the payload ships toolbox ' +
        'helpers whose executable bit is part of their identity',
    )
  })
})
