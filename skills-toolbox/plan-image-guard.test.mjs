#!/usr/bin/env node

// Unit + CLI coverage for skills-toolbox/plan-image-guard.mjs (BOS-378).
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
import { mkdtempSync, realpathSync, symlinkSync, writeFileSync } from 'node:fs'
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
  // An empty original carries no images, so nothing can be "dropped". This is correct set-difference
  // semantics and is pinned here so it cannot silently flip: the empty-original refusal deliberately
  // lives in the CLI (see the empty-input tests below), NOT in this pure function. Fixing it here
  // instead would make findDroppedImages lie about a legitimate empty-set comparison.
  assert.deepEqual(findDroppedImages('', `![a](${UPLOAD})`), [])
})

// ---------------------------------------------------------------------------
// parseImageGuardArgs — flag parsing.
// ---------------------------------------------------------------------------

test('parseImageGuardArgs reads --original and --rewritten', () => {
  assert.deepEqual(parseImageGuardArgs(['--original', '/tmp/a.md', '--rewritten', '/tmp/b.md']), {
    original: '/tmp/a.md',
    rewritten: '/tmp/b.md',
    allowEmptyOriginal: false,
  })
})

test('parseImageGuardArgs throws when --original is missing', () => {
  assert.throws(() => parseImageGuardArgs(['--rewritten', '/tmp/b.md']), /--original/)
})

test('parseImageGuardArgs throws when --rewritten is missing', () => {
  assert.throws(() => parseImageGuardArgs(['--original', '/tmp/a.md']), /--rewritten/)
})

test('parseImageGuardArgs sets allowEmptyOriginal for --allow-empty-original', () => {
  const args = parseImageGuardArgs([
    '--original',
    '/tmp/a.md',
    '--rewritten',
    '/tmp/b.md',
    '--allow-empty-original',
  ])
  assert.equal(args.allowEmptyOriginal, true)
})

// ---------------------------------------------------------------------------
// CLI — fail loud on a dropped image, exit 0 silent when preserved.
// ---------------------------------------------------------------------------

function runCli(originalMd, rewrittenMd, extraArgs = []) {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-image-guard-'))
  const original = path.join(dir, 'original.md')
  const rewritten = path.join(dir, 'rewritten.md')
  writeFileSync(original, originalMd)
  writeFileSync(rewritten, rewrittenMd)
  return spawnSync(
    process.execPath,
    [GUARD, '--original', original, '--rewritten', rewritten, ...extraArgs],
    { encoding: 'utf8' },
  )
}

// Stderr substrings that must stay distinguishable: a reader has to tell "extraction broke" from
// "images were dropped" at a glance, so every empty-input assertion also denies the other message.
const EMPTY_MSG = 'cannot verify image parity'
const DROPPED_MSG = 'image reference(s) dropped'

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

test('CLI: invoked through a symlinked path -> still fails closed, never a silent exit 0', () => {
  // Regression guard for a fail-OPEN main-module check. Node realpaths the main module before
  // setting import.meta.url but leaves process.argv[1] as typed, so comparing the two literally is
  // false whenever the invoking path crosses a symlink. The CLI body then never runs and the
  // process exits 0 in silence — which boss-plan Phase 4 reads as "no image dropped" and proceeds
  // with the write-back. This is not hypothetical: the skill ships to $BOSS_PLAN_TOOLBOX under a
  // user's global skills dir, which is routinely a symlink, and macOS's /tmp and /var are symlinks
  // to /private/*. The scratch dir is realpath'd so the ONLY symlink under test is the deliberate
  // one, making this meaningful on Linux too.
  const dir = mkdtempSync(path.join(realpathSync(tmpdir()), 'plan-image-guard-link-'))
  const link = path.join(dir, 'guard-via-symlink.mjs')
  symlinkSync(GUARD, link)
  const original = path.join(dir, 'original.md')
  const rewritten = path.join(dir, 'rewritten.md')
  writeFileSync(original, `![a](${UPLOAD})`)
  writeFileSync(rewritten, '[screenshot: session list]')
  const res = spawnSync(
    process.execPath,
    [link, '--original', original, '--rewritten', rewritten],
    {
      encoding: 'utf8',
    },
  )
  assert.notEqual(res.status, 0, 'a symlinked invocation must still fail closed on a dropped image')
  assert.ok(res.stderr.includes(UPLOAD), 'the dropped URL must still be printed to stderr')
})

test('CLI: missing scratch file -> non-zero exit (fail-closed / SAFE branch)', () => {
  // The Phase 4 gate's safety hinges on this: if the orchestrator's Write step never produced a
  // scratch file, the guard must fail closed (non-zero) so the SAFE branch blocks the Linear write —
  // never fail open. readFileSync throws -> top-level catch sets exitCode = 1.
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-image-guard-'))
  const missing = path.join(dir, 'does-not-exist.md')
  const present = path.join(dir, 'present.md')
  writeFileSync(present, `![a](${UPLOAD})`)
  const res = spawnSync(process.execPath, [GUARD, '--original', missing, '--rewritten', present], {
    encoding: 'utf8',
  })
  assert.notEqual(res.status, 0, 'a missing scratch file must fail closed with a non-zero exit')
  assert.equal(res.status, 1, 'the guard has exactly two exit codes — 0 and 1, never 2')
})

// ---------------------------------------------------------------------------
// CLI — an unverifiable (empty) original is refused, not vacuously blessed.
//
// findDroppedImages('', …) correctly returns [] — empty-in/empty-out is the right set-difference
// semantics and the pure functions keep it. The CLI carries the extra obligation the pure functions
// do not: it must refuse to CERTIFY a comparison it could not meaningfully perform. An empty
// original is ambiguous between "the upstream extraction broke" (images now invisible to the gate)
// and "the ticket genuinely has no description", and the guard sees only files, so it cannot tell
// them apart. It therefore fails closed by default and makes the benign case an explicit, auditable
// opt-out instead of a silent default.
// ---------------------------------------------------------------------------

test('CLI: empty original + empty rewritten -> exit 1 with the empty-input message', () => {
  // THE REGRESSION ANCHOR. This exact input produced a false "gate passed" on 2026-07-27: a BSD/GNU
  // sed portability bug left both scratch files at 0 bytes, and the guard exited 0. Whatever breaks
  // the extraction breaks BOTH files at once, so two empty files is precisely the input on which the
  // old guard was most confident. Must fail on the parent commit.
  const res = runCli('', '')
  assert.equal(res.status, 1, 'an empty original must fail closed, never vacuously pass')
  assert.ok(res.stderr.includes(EMPTY_MSG), `stderr must name the empty-input cause: ${res.stderr}`)
  assert.ok(
    !res.stderr.includes(DROPPED_MSG),
    'the empty-input refusal must not masquerade as a dropped-image failure',
  )
})

test('CLI: empty original + non-empty rewritten -> exit 1 with the empty-input message', () => {
  const res = runCli('', `![a](${UPLOAD})`)
  assert.equal(res.status, 1)
  assert.ok(res.stderr.includes(EMPTY_MSG))
  assert.ok(!res.stderr.includes(DROPPED_MSG))
})

test('CLI: whitespace-only original -> exit 1 (semantic emptiness, not length === 0)', () => {
  const res = runCli('\n\n  \n', `![a](${UPLOAD})`)
  assert.equal(res.status, 1, 'a whitespace-only original carries no verifiable content either')
  assert.ok(res.stderr.includes(EMPTY_MSG))
})

test('CLI: empty original + --allow-empty-original -> exit 0 and silent', () => {
  const res = runCli('', 'no images here\n', ['--allow-empty-original'])
  assert.equal(
    res.status,
    0,
    'the explicit opt-out must permit a genuinely description-less ticket',
  )
  assert.equal(res.stderr.trim(), '')
})

test('CLI: whitespace-only original + --allow-empty-original -> exit 0', () => {
  const res = runCli('\n\n  \n', 'no images here\n', ['--allow-empty-original'])
  assert.equal(res.status, 0)
})

test('CLI: --allow-empty-original is inert on a non-empty original (parity -> exit 0)', () => {
  const md = `![a](${UPLOAD})`
  const res = runCli(md, md, ['--allow-empty-original'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --allow-empty-original does NOT suppress a genuine drop', () => {
  // The opt-out must not become a blanket bypass: it waives only the empty-original precondition,
  // never the parity check itself.
  const res = runCli(`![a](${UPLOAD})`, '[screenshot: session list]', ['--allow-empty-original'])
  assert.equal(res.status, 1, 'a dropped image must still fail even with the opt-out passed')
  assert.ok(res.stderr.includes(UPLOAD))
  assert.ok(res.stderr.includes(DROPPED_MSG))
  assert.ok(!res.stderr.includes(EMPTY_MSG))
})

test('CLI: non-empty original + empty rewritten -> exit 1 via the DROPPED-image path', () => {
  // Precedence guard: an empty rewritten already fails correctly (every URL reads as dropped), and
  // that is the more informative message. It must not regress into the new empty-input branch.
  const res = runCli(`![a](${UPLOAD})`, '')
  assert.equal(res.status, 1)
  assert.ok(res.stderr.includes(DROPPED_MSG), 'the dropped-image message is the right one here')
  assert.ok(res.stderr.includes(UPLOAD))
  assert.ok(!res.stderr.includes(EMPTY_MSG), 'must not report this as an empty-input refusal')
})

test('CLI: --allow-empty-original with an unreadable rewritten still fails closed', () => {
  // The opt-out waives the empty-original precondition only — an I/O failure must still fail closed.
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-image-guard-'))
  const original = path.join(dir, 'original.md')
  writeFileSync(original, '')
  const res = spawnSync(
    process.execPath,
    [
      GUARD,
      '--original',
      original,
      '--rewritten',
      path.join(dir, 'does-not-exist.md'),
      '--allow-empty-original',
    ],
    { encoding: 'utf8' },
  )
  assert.equal(res.status, 1)
})
