#!/usr/bin/env node

// Unit + CLI coverage for scripts/plan-image-guard.mjs (BOS-378).
//
// The guard is the mechanical, LLM-untrusted half of the reporter-screenshot fix:
// boss-plan Phase 4 runs it on (original description) vs (rewritten descriptionSummary)
// before the Linear write-back and aborts when any image the reporter embedded is dropped.
// These tests pin the extractor's coverage of Linear's image shapes, the direction-aware
// drop detector, and the fail-loud CLI exit codes. Node builtins only — cron worktrees are
// dependency-free.

import assert from 'node:assert/strict'
import test from 'node:test'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { extractImageRefs, findDroppedImages, parseImageGuardArgs } from './plan-image-guard.mjs'

const GUARD = fileURLToPath(new URL('./plan-image-guard.mjs', import.meta.url))
const UPLOAD = 'https://uploads.linear.app/abc-123/screenshot.png'
const UPLOAD_B = 'https://uploads.linear.app/def-456/second.png'

// ---------------------------------------------------------------------------
// extractImageRefs — every Linear image shape, normalized + de-duplicated.
// ---------------------------------------------------------------------------

const extractCases = [
  {
    name: 'inline markdown ![alt](url)',
    markdown: `Here is a shot: ![session list](${UPLOAD})`,
    expected: [UPLOAD],
  },
  {
    name: 'inline markdown with title ![a](url "t")',
    markdown: `![a](${UPLOAD} "the reporter screenshot")`,
    expected: [UPLOAD],
  },
  {
    name: 'angle-bracket destination ![](<url>)',
    markdown: `![](<${UPLOAD}>)`,
    expected: [UPLOAD],
  },
  {
    name: 'HTML <img src="url"> double-quoted',
    markdown: `<img alt="x" src="${UPLOAD}" width="400">`,
    expected: [UPLOAD],
  },
  {
    name: "HTML <img src='url'> single-quoted",
    markdown: `<img src='${UPLOAD}'>`,
    expected: [UPLOAD],
  },
  {
    name: 'bare uploads.linear.app URL in prose',
    markdown: `The reporter attached ${UPLOAD} to the ticket.`,
    expected: [UPLOAD],
  },
  {
    name: 'bare URL with trailing prose punctuation is trimmed',
    markdown: `See ${UPLOAD}.`,
    expected: [UPLOAD],
  },
  {
    name: 'query strings are preserved (not stripped)',
    markdown: `![a](${UPLOAD}?signature=deadbeef)`,
    expected: [`${UPLOAD}?signature=deadbeef`],
  },
  {
    name: 'multiple images de-duplicated',
    markdown: `![a](${UPLOAD}) again ${UPLOAD} and <img src="${UPLOAD}">`,
    expected: [UPLOAD],
  },
  {
    name: 'two distinct images both collected',
    markdown: `![a](${UPLOAD})\n![b](${UPLOAD_B})`,
    expected: [UPLOAD, UPLOAD_B],
  },
  {
    name: 'markdown with no images -> empty set',
    markdown: 'Just prose, a [link](https://example.com/page), no pictures.',
    expected: [],
  },
]

for (const { name, markdown, expected } of extractCases) {
  test(`extractImageRefs: ${name}`, () => {
    const refs = extractImageRefs(markdown)
    assert.ok(refs instanceof Set, 'extractImageRefs must return a Set')
    assert.deepEqual([...refs].sort(), [...expected].sort())
  })
}

// ---------------------------------------------------------------------------
// findDroppedImages — original -> rewritten drops only, order-stable, deduped.
// ---------------------------------------------------------------------------

test('findDroppedImages: identical original/rewritten -> []', () => {
  const md = `![a](${UPLOAD})`
  assert.deepEqual(findDroppedImages(md, md), [])
})

test('findDroppedImages: inline image paraphrased to [screenshot: …] -> returns the dropped URL', () => {
  // The exact BOS-364 failure shape: the reporter's inline image is replaced by a text placeholder.
  const original = `## Original notes\n\n![session list](${UPLOAD})`
  const rewritten = `## Original notes\n\n[screenshot: session-list narrow agent selector]`
  assert.deepEqual(findDroppedImages(original, rewritten), [UPLOAD])
})

test('findDroppedImages: one of two images dropped -> only the missing one', () => {
  const original = `![a](${UPLOAD})\n![b](${UPLOAD_B})`
  const rewritten = `![b](${UPLOAD_B})`
  assert.deepEqual(findDroppedImages(original, rewritten), [UPLOAD])
})

test('findDroppedImages: rewrite adds an extra ## Screenshots image -> [] (extras never fail)', () => {
  const original = `![a](${UPLOAD})`
  const rewritten = `![a](${UPLOAD})\n\n## Screenshots\n\n![b](${UPLOAD_B})`
  assert.deepEqual(findDroppedImages(original, rewritten), [])
})

test('findDroppedImages: dropped attachment URL is returned', () => {
  const original = `The reporter attached ${UPLOAD} for context.`
  const rewritten = 'The reporter attached a screenshot for context.'
  assert.deepEqual(findDroppedImages(original, rewritten), [UPLOAD])
})

test('findDroppedImages: empty original -> [] (documented vacuous pass — the Phase 4 empty-file bypass)', () => {
  // An empty original carries no images, so nothing can be "dropped". The Phase 4 gate warns that
  // an empty ORIG_DESC vacuously passes; this pins that behavior so it cannot silently flip.
  assert.deepEqual(findDroppedImages('', `![a](${UPLOAD})`), [])
})

// ---------------------------------------------------------------------------
// parseImageGuardArgs — flag parsing.
// ---------------------------------------------------------------------------

test('parseImageGuardArgs reads --original and --rewritten', () => {
  assert.deepEqual(parseImageGuardArgs(['--original', '/tmp/a.md', '--rewritten', '/tmp/b.md']), {
    original: '/tmp/a.md',
    rewritten: '/tmp/b.md',
  })
})

test('parseImageGuardArgs throws when --original is missing', () => {
  assert.throws(() => parseImageGuardArgs(['--rewritten', '/tmp/b.md']), /--original/)
})

test('parseImageGuardArgs throws when --rewritten is missing', () => {
  assert.throws(() => parseImageGuardArgs(['--original', '/tmp/a.md']), /--rewritten/)
})

// ---------------------------------------------------------------------------
// CLI — fail loud on a dropped image, exit 0 silent when preserved.
// ---------------------------------------------------------------------------

function runCli(originalMd, rewrittenMd) {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-image-guard-'))
  const original = path.join(dir, 'original.md')
  const rewritten = path.join(dir, 'rewritten.md')
  writeFileSync(original, originalMd)
  writeFileSync(rewritten, rewrittenMd)
  return spawnSync(process.execPath, [GUARD, '--original', original, '--rewritten', rewritten], {
    encoding: 'utf8',
  })
}

test('CLI: dropped image -> non-zero exit and the dropped URL on stderr', () => {
  const res = runCli(`![a](${UPLOAD})`, '[screenshot: session list]')
  assert.notEqual(res.status, 0, 'CLI must exit non-zero when an image is dropped')
  assert.ok(res.stderr.includes(UPLOAD), 'the dropped URL must be printed to stderr')
})

test('CLI: preserved image -> exit 0 and silent', () => {
  const md = `![a](${UPLOAD})`
  const res = runCli(md, md)
  assert.equal(res.status, 0, 'CLI must exit 0 when no image is dropped')
  assert.equal(res.stderr.trim(), '', 'CLI must be silent when nothing is dropped')
})

test('CLI: missing scratch file -> non-zero exit (fail-closed / SAFE branch)', () => {
  // The Phase 4 gate's safety hinges on this: if the orchestrator's Write step never produced a
  // scratch file, the guard must fail closed (non-zero) so the SAFE branch blocks the Linear write —
  // never fail open. readFileSync throws -> top-level catch sets exitCode = 1.
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-image-guard-'))
  const missing = path.join(dir, 'does-not-exist.md')
  const present = path.join(dir, 'present.md')
  writeFileSync(present, `![a](${UPLOAD})`)
  const res = spawnSync(
    process.execPath,
    [GUARD, '--original', missing, '--rewritten', present],
    { encoding: 'utf8' },
  )
  assert.notEqual(res.status, 0, 'a missing scratch file must fail closed with a non-zero exit')
})
