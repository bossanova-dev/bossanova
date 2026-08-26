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

import {
  extractImageRefs,
  findDroppedImages,
  parseImageGuardArgs,
  uploadIdentity,
} from './plan-image-guard.mjs'

const GUARD = fileURLToPath(new URL('./plan-image-guard.mjs', import.meta.url))
const UPLOAD = 'https://uploads.linear.app/abc-123/screenshot.png'
const UPLOAD_B = 'https://uploads.linear.app/def-456/second.png'
const EXTERNAL_IMAGE =
  'https://cdn.example.test/screenshot.png?X-Amz-Credential=demo-credential&X-Amz-Signature=demo-signature&width=640'
const REDACTED_EXTERNAL_IMAGE =
  'https://cdn.example.test/screenshot.png?X-Amz-Credential=[REDACTED]&X-Amz-Signature=[REDACTED]&width=640'
const REDACTED_EXTERNAL_IMAGE_SIMPLE =
  'https://cdn.example.test/screenshot.png?X-Amz-Credential=REDACTED&X-Amz-Signature=REDACTED&width=640'
const REDACTED_EXTERNAL_IMAGE_REFERENCE =
  'https://cdn.example.test/screenshot.png?X-Amz-Credential=[REDACTED:%20value%20lives%20in%20vault]&X-Amz-Signature=[REDACTED:%20value%20lives%20in%20vault]&width=640'

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
    name: 'angle-bracket destination with title ![](<url> "title")',
    markdown: `![](<${EXTERNAL_IMAGE}> "the reporter screenshot")`,
    expected: [EXTERNAL_IMAGE],
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
    name: 'HTML <img src=url> unquoted with numeric entities',
    markdown: '<img alt=report src=https&#58;//uploads.linear.app/abc-123/screenshot.png>',
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
    name: 'query strings stay raw for extraction (comparison canonicalizes separately)',
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

test('extractImageRefs: preserves balanced parentheses in an inline image destination', () => {
  const url = 'https://uploads.linear.app/foo(bar)?signature=LIVESECRET'
  assert.deepEqual([...extractImageRefs(`![a](${url})`)], [url])
})

test('extractImageRefs: preserves an escaped closing parenthesis in an inline image destination', () => {
  const url = 'https://uploads.linear.app/foo)?signature=LIVESECRET'
  assert.deepEqual(
    [...extractImageRefs(`![a](https://uploads.linear.app/foo\\)?signature=LIVESECRET)`)],
    [url],
  )
})

test('extractImageRefs: parses an escaped closing bracket in an inline image label', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  assert.deepEqual([...extractImageRefs(`![build \\] screenshot](${url})`)], [url])
})

test('extractImageRefs: preserves an escaped closing angle bracket in an angle destination', () => {
  const url = 'https://cdn.example.test/x>?token=LIVESECRET'
  assert.deepEqual(
    [...extractImageRefs(`![a](<https://cdn.example.test/x\\>?token=LIVESECRET>)`)],
    [url],
  )
})

test('extractImageRefs: resolves a reference-style Markdown image destination', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  const markdown = `![build][reporter-shot]\n\n[reporter-shot]: <${url}> "reporter screenshot"`
  assert.deepEqual([...extractImageRefs(markdown)], [url])
})

test('extractImageRefs: resolves a reference definition with an escaped closing bracket', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  const markdown = `![build][asset\\]id]\n\n[asset\\]id]: <${url}>`
  assert.deepEqual([...extractImageRefs(markdown)], [url])
})

test('extractImageRefs: resolves an entity-encoded reference destination on the next line', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  const markdown =
    '![build][asset]\n\n[asset]:\n  https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET'
  assert.deepEqual([...extractImageRefs(markdown)], [url])
})

test('extractImageRefs: resolves a reference definition inside a block quote', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  const markdown = `> ![build][asset]\n> [asset]: https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET`
  assert.deepEqual([...extractImageRefs(markdown)], [url])
})

test('extractImageRefs: resolves a reference definition in an indented list continuation', () => {
  assert.deepEqual(
    [
      ...extractImageRefs(
        `-    ![build][asset]\n     [asset]:\n     https://uploads.linear.app/abc-123/screenshot.png`,
      ),
    ],
    [UPLOAD],
  )
})

test('extractImageRefs: ignores a fenced reference definition before the real definition', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  const markdown = `![build][asset]\n\n\`\`\`markdown\n[asset]: https://example.test/example.png\n\`\`\`\n\n[asset]: <${url}>`
  assert.deepEqual([...extractImageRefs(markdown)], [url])
})

test('extractImageRefs: ignores a reference definition inside a raw HTML block', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  const markdown = `![build][asset]\n\n<div>\n[asset]: https://safe.example.test/build.png\n</div>\n\n[asset]: <${url}>`
  assert.deepEqual([...extractImageRefs(markdown)], [url])
})

for (const tag of ['script', 'pre', 'style']) {
  test(`extractImageRefs: keeps a type-1 ${tag} block open across a blank line`, () => {
    const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
    const markdown = `![build][asset]\n\n<${tag}>\n\n[asset]: https://safe.example.test/build.png\n</${tag}>\n\n[asset]: <${url}>`
    assert.deepEqual([...extractImageRefs(markdown)], [url])
  })
}

test('extractImageRefs: ignores a reference definition inside an HTML comment', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  const markdown = `![build][asset]\n\n<!--\n[asset]: https://safe.example.test/build.png\n-->\n\n[asset]: <${url}>`
  assert.deepEqual([...extractImageRefs(markdown)], [url])
})

for (const [name, opener, closer] of [
  ['processing instruction', '<?safe', '?>'],
  ['declaration', '<!DOCTYPE safe', '>'],
  ['CDATA section', '<![CDATA[', ']]>'],
]) {
  test(`extractImageRefs: ignores a reference definition inside a raw HTML ${name}`, () => {
    const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
    const markdown = `![build][asset]\n\n${opener}\n[asset]: https://safe.example.test/build.png\n${closer}\n\n[asset]: <${url}>`
    assert.deepEqual([...extractImageRefs(markdown)], [url])
  })
}

test('extractImageRefs: does not open a backtick fence with backticks in its info string', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  const markdown = `![build][asset]\n\n\`\`\`markdown\`oops\n[asset]: <${url}>`
  assert.deepEqual([...extractImageRefs(markdown)], [url])
})

test('extractImageRefs: reads an HTML src after a quoted greater-than sign', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  assert.deepEqual([...extractImageRefs(`<img title="a > b" src="${url}">`)], [url])
})

test('extractImageRefs: includes HTML srcset candidates', () => {
  const first = 'https://cdn.example.test/build.png?token=LIVESECRET'
  const second = 'https://cdn.example.test/build@2x.png?token=SECONDSECRET'
  assert.deepEqual(
    [...extractImageRefs(`<img srcset="${first} 1x, ${second} 2x">`)],
    [first, second],
  )
})

test('extractImageRefs: includes entity-encoded HTML <source srcset> candidates', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  assert.deepEqual(
    [
      ...extractImageRefs(
        `<picture><source srcset="https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET 2x"></picture>`,
      ),
    ],
    [url],
  )
})

test('extractImageRefs: finds srcset after a data-srcset attribute', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  assert.deepEqual(
    [
      ...extractImageRefs(
        `<img data-srcset="https://safe.example.test/build.png" srcset="${url} 2x">`,
      ),
    ],
    [url],
  )
})

test('extractImageRefs: decodes an unquoted HTML srcset candidate', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  assert.deepEqual(
    [
      ...extractImageRefs(
        '<img srcset=https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET>',
      ),
    ],
    [url],
  )
})

test('extractImageRefs: decodes named HTML entities in image URLs', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  assert.deepEqual(
    [
      ...extractImageRefs(
        '<img src="https&colon;//cdn.example.test/build.png?token&equals;LIVESECRET">',
      ),
    ],
    [url],
  )
})

test('extractImageRefs: decodes character references in inline Markdown destinations', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  assert.deepEqual(
    [...extractImageRefs('![build](https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET)')],
    [url],
  )
})

test('extractImageRefs: unescapes Markdown punctuation in inline destinations', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  assert.deepEqual(
    [...extractImageRefs('![build](https\\://cdn.example.test/build.png?token=LIVESECRET)')],
    [url],
  )
})

test('extractImageRefs: decodes character references in reference Markdown destinations', () => {
  const url = 'https://cdn.example.test/build.png?token=LIVESECRET'
  assert.deepEqual(
    [
      ...extractImageRefs(
        '![build][reporter-shot]\n\n[reporter-shot]: https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET',
      ),
    ],
    [url],
  )
})

// ---------------------------------------------------------------------------
// uploadIdentity — stable upload asset identity; extraction remains raw.
// ---------------------------------------------------------------------------

test('uploadIdentity: signed and unsigned forms of one upload asset are equal', () => {
  const signed = `${UPLOAD}?signature=deadbeef#fragment`
  assert.equal(uploadIdentity(signed), UPLOAD)
  assert.equal(uploadIdentity(signed), uploadIdentity(UPLOAD))
})

test('uploadIdentity: different upload asset paths remain distinct', () => {
  assert.notEqual(
    uploadIdentity(`${UPLOAD}?signature=one`),
    uploadIdentity(`${UPLOAD_B}?signature=two`),
  )
})

test('uploadIdentity: non-upload URLs preserve their query string', () => {
  const url = 'https://example.com/image.png?cache=bust#fragment'
  assert.equal(uploadIdentity(url), url)
})

test('uploadIdentity: credential-redacted external URLs retain their safe image identity', () => {
  assert.equal(uploadIdentity(EXTERNAL_IMAGE), uploadIdentity(REDACTED_EXTERNAL_IMAGE))
})

test('uploadIdentity: documented credential-redaction forms retain their safe image identity', () => {
  for (const redacted of [REDACTED_EXTERNAL_IMAGE_SIMPLE, REDACTED_EXTERNAL_IMAGE_REFERENCE]) {
    assert.equal(uploadIdentity(EXTERNAL_IMAGE), uploadIdentity(redacted))
  }
})

test('uploadIdentity: credential-redacted external URLs retain non-credential query values', () => {
  assert.notEqual(
    uploadIdentity(EXTERNAL_IMAGE),
    uploadIdentity(REDACTED_EXTERNAL_IMAGE.replace('width=640', 'width=320')),
  )
})

test('uploadIdentity: malformed URL stays unchanged', () => {
  const malformed = 'https://uploads.linear.app:bad/path?signature=deadbeef'
  assert.equal(uploadIdentity(malformed), malformed)
})

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

test('findDroppedImages: signed original and unsigned rewrite of one upload asset -> []', () => {
  assert.deepEqual(findDroppedImages(`![a](${UPLOAD}?signature=old)`, `![a](${UPLOAD})`), [])
})

test('findDroppedImages: unsigned original and signed rewrite of one upload asset -> []', () => {
  assert.deepEqual(findDroppedImages(`![a](${UPLOAD})`, `![a](${UPLOAD}?signature=new)`), [])
})

test('findDroppedImages: credential-redacted external image URL is preserved', () => {
  assert.deepEqual(
    findDroppedImages(`![a](${EXTERNAL_IMAGE})`, `![a](${REDACTED_EXTERNAL_IMAGE})`),
    [],
  )
})

test('findDroppedImages: reports the original raw URL when a signed upload is dropped', () => {
  const signed = `${UPLOAD}?signature=old`
  assert.deepEqual(findDroppedImages(`![a](${signed})`, '[screenshot: removed]'), [signed])
})

test('findDroppedImages: different upload asset path is still reported as dropped', () => {
  assert.deepEqual(
    findDroppedImages(`![a](${UPLOAD}?signature=old)`, `![b](${UPLOAD_B}?signature=new)`),
    [`${UPLOAD}?signature=old`],
  )
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
    expectImages: null,
    requireVerbatim: false,
    requireSafeSource: false,
    requireUnsignedUploads: false,
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

test('parseImageGuardArgs reads --expect-images and guard requirements', () => {
  assert.deepEqual(
    parseImageGuardArgs([
      '--original',
      '/tmp/a.md',
      '--rewritten',
      '/tmp/b.md',
      '--expect-images',
      '2',
      '--require-verbatim',
      '--require-safe-source',
      '--require-unsigned-uploads',
    ]),
    {
      original: '/tmp/a.md',
      rewritten: '/tmp/b.md',
      allowEmptyOriginal: false,
      expectImages: 2,
      requireVerbatim: true,
      requireSafeSource: true,
      requireUnsignedUploads: true,
    },
  )
})

test('parseImageGuardArgs rejects negative and non-integer --expect-images values', () => {
  for (const value of ['-1', '1.5', 'nope']) {
    assert.throws(
      () =>
        parseImageGuardArgs([
          '--original',
          '/tmp/a.md',
          '--rewritten',
          '/tmp/b.md',
          '--expect-images',
          value,
        ]),
      /--expect-images.*non-negative integer/,
    )
  }
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

test('CLI: --expect-images passes when original contains enough distinct identities', () => {
  const original = `![a](${UPLOAD}?signature=one)\n![b](${UPLOAD_B}?signature=two)`
  const rewritten = `![a](${UPLOAD})\n![b](${UPLOAD_B})`
  const res = runCli(original, rewritten, ['--expect-images', '2'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --expect-images fails closed and names observed and expected counts', () => {
  const res = runCli(`![a](${UPLOAD})`, `![a](${UPLOAD})`, ['--expect-images', '2'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /1 distinct image identities/)
  assert.match(res.stderr, /expected at least 2/)
})

test('CLI: --expect-images counts signed and unsigned forms as one upload identity', () => {
  const original = `![a](${UPLOAD}?signature=one)\n![a again](${UPLOAD})`
  const res = runCli(original, original, ['--expect-images', '2'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /1 distinct image identities/)
})

test('CLI: --expect-images excludes non-upload image URLs from the observed count', () => {
  const original = `![external](https://example.com/image.png)\n![upload](${UPLOAD})`
  const res = runCli(original, original, ['--expect-images', '2'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /1 distinct image identities/)
})

test('CLI: invalid --expect-images exits 1 with a clear error', () => {
  const res = runCli(`![a](${UPLOAD})`, `![a](${UPLOAD})`, ['--expect-images', '-1'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /--expect-images.*non-negative integer/)
})

test('CLI: --require-verbatim accepts an exact Original notes section', () => {
  const original = 'A [stable link](https://example.com/a) and text.\n'
  const rewritten = `# Plan\n\n## Tasks\n\n- [ ] do work\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-verbatim preserves H2 headings inside terminal Original notes', () => {
  const original = 'Reporter context.\n\n## Requirements\n\nKeep this heading verbatim.\n'
  const rewritten = `# Plan\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-verbatim ignores an Original notes heading in a fenced example', () => {
  const original = 'Reporter context.\n'
  const rewritten = `# Plan

## Template example

\`\`\`md
## Original notes

Example content, not the plan source.
\`\`\`

## Original notes

${original}`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-verbatim does not close a fenced example on a marker with an info string', () => {
  const original = 'Reporter context.\n'
  const rewritten = `# Plan

## Template example

\`\`\`md
\`\`\`js
## Original notes

Example content, not the plan source.
\`\`\`

## Original notes

${original}`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-verbatim does not open a backtick fence with backticks in its info string', () => {
  const original = 'Reporter context.\n'
  const rewritten = `# Plan

\`\`\`lang\`oops
## Original notes

${original}`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-verbatim selects the wrapper when source notes repeat its heading', () => {
  const original = 'Reporter context.\n\n## Original notes\n\nThis nested heading is source text.\n'
  const rewritten = `# Plan

## Original notes

${original}`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-verbatim preserves leading indentation in Original notes', () => {
  const original = '  indentation is significant\n'
  const rewritten = `## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-verbatim rejects changed leading or trailing whitespace', () => {
  const original = '  indentation is significant  \n'
  const rewritten = '## Original notes\n\nindentation is significant\n'
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes whitespace-only difference at line 1, column 1/)
  assert.match(res.stderr, /original lines: 1, rewritten lines: 1/)
})

test('CLI: --require-verbatim rejects an altered non-image Markdown link', () => {
  const original = 'A [stable link](https://example.com/a) and text.\n'
  const rewritten =
    '# Plan\n\n## Original notes\n\nA [rewritten link](https://example.com/a) and text.\n'
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes content difference at line 1, column 4/)
  assert.ok(!res.stderr.includes('stable link'))
  assert.ok(!res.stderr.includes('rewritten link'))
})

test('CLI: --require-verbatim rejects a missing Original notes section', () => {
  const res = runCli('Original tracker text.\n', '# Plan\n\n## Tasks\n\n- [ ] do work\n', [
    '--require-verbatim',
  ])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes section is missing/)
})

test('CLI: --require-verbatim accepts signed and unsigned forms of one upload URL', () => {
  const original = `![a](${UPLOAD}?signature=old)\n`
  const rewritten = `## Original notes\n\n![a](${UPLOAD})\n`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-verbatim accepts signed and unsigned reference-style upload URLs', () => {
  const original = `![a][reporter-shot]\n\n[reporter-shot]: ${UPLOAD}?signature=old\n`
  const rewritten = `## Original notes\n\n![a][reporter-shot]\n\n[reporter-shot]: ${UPLOAD}\n`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-verbatim preserves the exact safe external credential reference', () => {
  const original = `![a](${REDACTED_EXTERNAL_IMAGE_REFERENCE})\n`
  const rewritten = `## Original notes\n\n![a](${REDACTED_EXTERNAL_IMAGE})\n`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes content difference at line 1,/)
})

test('CLI: --require-verbatim accepts an unchanged safe external credential reference', () => {
  const original = `![a](${REDACTED_EXTERNAL_IMAGE_REFERENCE})\n`
  const rewritten = `## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-verbatim does not echo rejected note content', () => {
  const original = 'deployment token: [REDACTED: vault]\n'
  const rewritten = '## Original notes\n\ndeployment token: live-token-value\n'
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes content difference at line 1,/)
  assert.ok(!res.stderr.includes('live-token-value'))
})

test('CLI: --require-verbatim diagnoses one added trailing newline within original line range', () => {
  const original = 'line one\nline two\nline three'
  const rewritten = `## Original notes\n\n${original}\n`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes trailing newline differs at line 3/)
  assert.match(res.stderr, /original lines: 3, rewritten lines: 3/)
})

test('CLI: --require-verbatim diagnoses one removed trailing newline as trailing newline drift', () => {
  const original = 'line one\nline two\nline three\n'
  const rewritten = '## Original notes\n\nline one\nline two\nline three'
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes trailing newline differs at line 3/)
  assert.match(res.stderr, /original lines: 3, rewritten lines: 3/)
})

test('CLI: --require-verbatim diagnoses indentation-only drift after line one', () => {
  const original = 'line one\nline two\nline three'
  const rewritten = '## Original notes\n\nline one\n  line two\nline three'
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes whitespace-only difference at line 2, column 1/)
  assert.match(res.stderr, /original lines: 3, rewritten lines: 3/)
})

test('CLI: --require-verbatim diagnoses trailing-space-only drift with a column', () => {
  const original = 'line one\nline two\nline three'
  const rewritten = '## Original notes\n\nline one\nline two \nline three'
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes whitespace-only difference at line 2, column 9/)
  assert.match(res.stderr, /original lines: 3, rewritten lines: 3/)
})

test('CLI: --require-verbatim diagnoses truncated Original notes with both line counts', () => {
  const original = 'line one\nline two\nline three'
  const rewritten = '## Original notes\n\nline one\nline two'
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes content difference at line 3, column 1/)
  assert.match(res.stderr, /original lines: 3, rewritten lines: 2/)
})

test('CLI: --require-verbatim never echoes the differing line text', () => {
  const sentinel = 'distinctive-sentinel-source-line'
  const original = `line one\n${sentinel}\nline three`
  const rewritten = '## Original notes\n\nline one\nrewritten secret line\nline three'
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes content difference at line 2,/)
  assert.ok(!res.stderr.includes(sentinel))
  assert.ok(!res.stderr.includes('rewritten secret line'))
})

test('CLI: --require-verbatim rejects an unredacted external credential query value', () => {
  const original = `![a](${EXTERNAL_IMAGE})\n`
  const rewritten = `## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('demo-credential'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject a multiline entity-encoded reference credential', () => {
  const original =
    '![build][asset]\n\n[asset]:\n  https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET\n'
  const rewritten = `## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an entity-encoded blockquote reference credential', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n> ![build][asset]\n> [asset]: https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an indented list continuation reference credential', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n-    supporting context\n     [asset]: https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject a tab-indented list continuation reference credential', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n-\t![shot][asset]\n\t[asset]: https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: --require-safe-source rejects omitted reporter prose', () => {
  const raw = `Reporter context that must survive.\n\n![build](${UPLOAD}?signature=LIVESECRET)\n`
  const safe = `![build](${UPLOAD})\n`
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /safe source drops or alters unredacted original notes/)
})

test('CLI: --require-safe-source canonicalizes upload URLs in srcset candidates', () => {
  const raw = `<img srcset="https&#58;//uploads.linear.app/abc-123/screenshot.png?signature=LIVESECRET 1x">\n`
  const safe = `<img srcset="${UPLOAD} 1x">\n`
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes a bare external credential URL', () => {
  const raw = 'https://cdn.example.test/build.png?token=LIVESECRET\n'
  const safe = 'https://cdn.example.test/build.png?token=REDACTED\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes an entity-encoded bare external credential URL', () => {
  const raw = 'https&colon;&sol;&sol;cdn.example.test/build.png?token&equals;LIVESECRET\n'
  const safe = 'https://cdn.example.test/build.png?token=REDACTED\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes a CSS-escaped external credential URL', () => {
  const raw =
    '<style>img { background-image: url(https\\3a\\2f\\2f cdn.example.test/build.png?token=LIVESECRET) }</style>\n'
  const safe =
    '<style>img { background-image: url(https://cdn.example.test/build.png?token=REDACTED) }</style>\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes a bare URL split by entity-encoded parser controls', () => {
  const raw = 'ht&#9;tps:&#9;/&#9;/cdn.example.test/build.png?token=LIVESECRET\n'
  const safe = 'https://cdn.example.test/build.png?token=REDACTED\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes a scheme-relative bare external credential URL', () => {
  const raw = '//cdn.example.test/build.png?token=LIVESECRET\n'
  const safe = '//cdn.example.test/build.png?token=REDACTED\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes an entity-encoded ordinary Markdown link', () => {
  const raw = '[build log](https&#58;//cdn.example.test/build.log?token&#61;LIVESECRET)\n'
  const safe = '[build log](https://cdn.example.test/build.log?token=REDACTED)\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes documented redacted URL userinfo', () => {
  const raw = '[service](https://apiuser:LIVESECRET@cdn.example.test/build.log)\n'
  const safe = '[service](https://apiuser:REDACTED@cdn.example.test/build.log)\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes fragment credentials', () => {
  const raw = '![build](https://cdn.example.test/build.png#access_token=LIVESECRET)\n'
  const safe = '![build](https://cdn.example.test/build.png#access_token=REDACTED)\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: documented redacted URL userinfo passes safe-source and verbatim gates', () => {
  const raw = '[service](https://apiuser:LIVESECRET@cdn.example.test/build.log)\n'
  const safe = '[service](https://apiuser:REDACTED@cdn.example.test/build.log)\n'
  const safeSource = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(safeSource.status, 0)
  assert.equal(safeSource.stderr.trim(), '')

  const rewritten = `## Original notes\n\n${safe}`
  const verbatim = runCli(safe, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(verbatim.status, 0)
  assert.equal(verbatim.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes input-image source credentials', () => {
  const raw =
    '<input type="image" src="https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET">\n'
  const safe = '<input type="image" src="https://cdn.example.test/build.png?token=REDACTED">\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes video poster credentials', () => {
  const raw = '<video poster="https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET">\n'
  const safe = '<video poster="https://cdn.example.test/build.png?token=REDACTED">\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes HTML link credentials', () => {
  const raw = '<a href="https&#58;//cdn.example.test/build.log?token&#61;LIVESECRET">build</a>\n'
  const safe = '<a href="https://cdn.example.test/build.log?token=REDACTED">build</a>\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes SVG image href credentials', () => {
  const raw =
    '<svg><image href="https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET"></image><image xlink:href="https&#58;//cdn.example.test/legacy.png?token&#61;LIVESECRET"></image></svg>\n'
  const safe =
    '<svg><image href="https://cdn.example.test/build.png?token=REDACTED"></image><image xlink:href="https://cdn.example.test/legacy.png?token=REDACTED"></image></svg>\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source canonicalizes every active reference definition', () => {
  const raw = '[build]: https&#58;//cdn.example.test/build.log?token&#61;LIVESECRET\n'
  const safe = '[build]: https://cdn.example.test/build.log?token=REDACTED\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 0)
  assert.equal(res.stderr.trim(), '')
})

test('CLI: --require-safe-source rejects replacement URL userinfo that is not redacted', () => {
  const raw = '[service](https://apiuser:LIVESECRET@cdn.example.test/build.log)\n'
  const safe = '[service](https://apiuser:OTHERSECRET@cdn.example.test/build.log)\n'
  const res = runCli(raw, safe, ['--require-safe-source'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /safe source drops or alters unredacted original notes/)
})

test('CLI: --require-verbatim rejects an unredacted external credential in an escaped-label image', () => {
  const original = `![build \\] screenshot](${EXTERNAL_IMAGE})\n`
  const rewritten = `## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
})

test('CLI: --require-verbatim rejects an unredacted external credential in a titled angle destination', () => {
  const original = `![a](<${REDACTED_EXTERNAL_IMAGE}> "safe screenshot")\n`
  const rewritten = `# Plan

## Screenshots

![a](<${EXTERNAL_IMAGE}> "live screenshot")

## Original notes

${original}`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(
    res.stderr,
    /rewritten description contains 1 external image URL\(s\) with unredacted credential query values/,
  )
  assert.ok(!res.stderr.includes('demo-credential'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an escaped angle destination with a token credential', () => {
  const safe = 'https://cdn.example.test/x>?token=[REDACTED]'
  const original = `![a](<${safe}>)\n`
  const rewritten = `# Plan\n\n## Screenshots\n\n![a](<https://cdn.example.test/x\\>?token=LIVESECRET>)\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an unredacted reference-style image credential', () => {
  const safe = 'https://cdn.example.test/build.png?token=[REDACTED]'
  const original = `![build][reporter-shot]\n\n[reporter-shot]: <${safe}>\n`
  const rewritten = `# Plan\n\n## Screenshots\n\n![build][reporter-shot]\n\n[reporter-shot]: <https://cdn.example.test/build.png?token=LIVESECRET>\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an unredacted HTML srcset credential', () => {
  const original = '<img srcset="https://cdn.example.test/build.png?token=[REDACTED] 2x">\n'
  const rewritten = `## Original notes\n\n<img srcset="https://cdn.example.test/build.png?token=LIVESECRET 2x">\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an entity-encoded HTML <source srcset> credential', () => {
  const original =
    '<picture><source srcset="https://cdn.example.test/build.png?token=[REDACTED] 2x"></picture>\n'
  const rewritten = `## Original notes\n\n<picture><source srcset="https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET 2x"></picture>\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an entity-encoded HTML image-input credential', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<input name="save" src="https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET" type="image">\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject credentials in an entity-encoded non-image HTML input', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<input type="text" value="https&colon;//cdn.example.test/build.png?token&equals;LIVESECRET">\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an entity-encoded video poster credential', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<video poster="https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET">\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an unquoted entity-encoded HTML srcset credential', () => {
  const original = '<img srcset=https&#58;//cdn.example.test/build.png?token&#61;[REDACTED]>\n'
  const rewritten = `## Original notes\n\n<img srcset=https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET>\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an unquoted HTML srcset credential with literal equals', () => {
  const original = '<img srcset=https://cdn.example.test/build.png?token=[REDACTED]>\n'
  const rewritten = `## Original notes\n\n<img srcset=https://cdn.example.test/build.png?token=LIVESECRET>\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject a srcset credential after data-srcset', () => {
  const original = '<img srcset="https://cdn.example.test/build.png?token=[REDACTED] 2x">\n'
  const rewritten = `## Original notes\n\n<img data-srcset="https://safe.example.test/build.png" srcset="https&colon;//cdn.example.test/build.png?token&equals;LIVESECRET 2x">\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject a reference credential after a raw HTML definition', () => {
  const original = `![build][asset]\n\n[asset]: <https://cdn.example.test/build.png?token=[REDACTED]>\n`
  const rewritten = `## Original notes\n\n<div>\n[asset]: https://safe.example.test/build.png\n</div>\n\n![build][asset]\n\n[asset]: https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

for (const [name, inertDefinition] of [
  ['blockquote HTML block', '> <div>\n> [asset]: https://safe.example.test/build.png\n> </div>\n>'],
  ['blockquote fenced block', '> ```md\n> [asset]: https://safe.example.test/build.png\n> ```\n>'],
  ['list HTML block', '- <div>\n  [asset]: https://safe.example.test/build.png\n  </div>'],
]) {
  test(`CLI: mandatory guards ignore an inert ${name} definition before a live credential`, () => {
    const original = 'Reporter context.\n'
    const rewritten = `## Screenshots\n\n${inertDefinition}\n\n[asset]: https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET\n\n## Original notes\n\n${original}`
    const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
    assert.equal(res.status, 1)
    assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
    assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
  })
}

test('CLI: mandatory guards scan a credential in a duplicate reference definition', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n[asset]: https://safe.example.test/build.png\n[asset]: https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

for (const [name, opener, closer] of [
  ['processing instruction', '<?safe', '?>'],
  ['declaration', '<!DOCTYPE safe', '>'],
  ['CDATA section', '<![CDATA[', ']]>'],
]) {
  test(`CLI: mandatory guards reject a reference credential after a raw HTML ${name}`, () => {
    const original = `![build][asset]\n\n[asset]: <https://cdn.example.test/build.png?token=[REDACTED]>\n`
    const rewritten = `## Original notes\n\n${opener}\n[asset]: https://safe.example.test/build.png\n${closer}\n\n![build][asset]\n\n[asset]: https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET\n\n${original}`
    const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
    assert.equal(res.status, 1)
    assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
    assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
  })
}

test('CLI: mandatory guards ignore a type-7 custom HTML definition before an active credential', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<x-widget>\n[asset]: https://safe.example.test/build.png\n\n[asset]: https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject a named-entity HTML credential URL', () => {
  const original = '<img src="https://cdn.example.test/build.png?token=[REDACTED]">\n'
  const rewritten = `## Original notes\n\n<img src="https&colon;//cdn.example.test/build.png?token&equals;LIVESECRET">\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject a Markdown character-reference credential', () => {
  const original = '![build](https://cdn.example.test/build.png?token=[REDACTED])\n'
  const rewritten = `## Original notes\n\n![build](https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET)\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject a Markdown-escaped scheme credential', () => {
  const original = '![build](https\\://cdn.example.test/build.png?token=[REDACTED])\n'
  const rewritten = `## Original notes\n\n![build](https\\://cdn.example.test/build.png?token=LIVESECRET)\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an encoded ordinary Markdown-link credential', () => {
  const original = 'A [safe link](https&#58;//cdn.example.test/docs?token=[REDACTED])\n'
  const rewritten = `## Original notes\n\n${original}\nA [leaked link](https&#58;//cdn.example.test/docs?token=LIVESECRET)\n`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an encoded reference-style ordinary-link credential', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n[download][asset]\n\n[asset]: https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an entity-encoded HTML anchor credential', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<a href="https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET">download</a>\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an HTML anchor credential after a URL parser control', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<a href="https://cdn.example.test/build.png\n?token=LIVESECRET">download</a>\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject entity-encoded SVG image href credentials', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<svg><image href="https&colon;//cdn.example.test/build.png?token&equals;LIVESECRET"></image><image xlink:href="https&colon;//cdn.example.test/legacy.png?token&equals;LIVESECRET"></image></svg>\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an entity-encoded signed ordinary upload link', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n[download](https&#58;//uploads.linear.app/a.png?signature&#61;LIVESECRET)\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /1 signed upload URL/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an unredacted bare external credential URL', () => {
  const original = `![a](https://cdn.example.test/build.png?token=[REDACTED])\n`
  const rewritten = `## Screenshots\n\nhttps://cdn.example.test/build.png?token=LIVESECRET\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an entity-encoded bare external credential URL', () => {
  const original = `![a](https://cdn.example.test/build.png?token=[REDACTED])\n`
  const rewritten = `## Screenshots\n\nhttps&colon;//cdn.example.test/build.png?token&equals;LIVESECRET\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject a CSS-escaped external credential URL', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<style>img { background-image: url(https\\3a\\2f\\2f cdn.example.test/build.png?token=LIVESECRET) }</style>\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

// The function name is escapable too: `u\72l(…)` is the url() function to a CSS parser. Matching a
// literal `url(` skipped decoding entirely, so the credential below survived the mandatory gate.
test('CLI: mandatory guards reject a CSS credential URL behind an ESCAPED url() identifier', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<style>img { background-image: u\\72 l(https\\3a\\2f\\2f cdn.example.test/build.png?token=LIVESECRET) }</style>\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

// A non-url() function whose name merely ends in `url` must stay untouched, so the widened
// identifier match cannot start normalizing arbitrary CSS functions.
test('CLI: mandatory guards leave a non-url() function whose name ends in "url" alone', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<style>img { background-image: my-url(https\\3a\\2f\\2f cdn.example.test/build.png?token=LIVESECRET) }</style>\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 0)
})

test('CLI: mandatory guards reject a bare credential URL split by entity-encoded parser controls', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<object data="ht&#9;tps:&#9;/&#9;/cdn.example.test/build.png?token=LIVESECRET"></object>\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an unredacted scheme-relative image credential', () => {
  const original = '![a](//cdn.example.test/build.png?token=[REDACTED])\n'
  const rewritten = `## Original notes\n\n![a](//cdn.example.test/build.png?token=LIVESECRET)\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject a scheme-relative credential in an unsupported HTML destination', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n<object data="//cdn.example.test/build.png?token=LIVESECRET"></object>\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject URL userinfo credentials', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n![build](https://apiuser:LIVESECRET@cdn.example.test/build.png)\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject credential fields in URL fragments', () => {
  const original = 'Reporter context.\n'
  const rewritten = `## Screenshots\n\n![build](https://cdn.example.test/build.png#access_token=LIVESECRET)\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject a reference credential after an invalid backtick fence opener', () => {
  const original = `![build][asset]\n\n[asset]: <https://cdn.example.test/build.png?token=[REDACTED]>\n`
  const rewritten = `![build][asset]\n\n\`\`\`markdown\`oops\n[asset]: <https://cdn.example.test/build.png?token=LIVESECRET>\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an HTML credential after a quoted greater-than sign', () => {
  const original = `<img src="https://cdn.example.test/build.png?token=[REDACTED]">\n`
  const rewritten = `## Screenshots\n\n<img title="a > b" src="https://cdn.example.test/build.png?token=LIVESECRET">\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an HTML-entity-encoded credential query key', () => {
  const original = '<img src="https://cdn.example.test/build.png?token=[REDACTED]">\n'
  const rewritten = `## Original notes\n\n<img src="https://cdn.example.test/build.png?to&#107;en=LIVESECRET">\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject a semicolonless numeric HTML-entity credential query key', () => {
  const original = '<img src="https://cdn.example.test/build.png?token=[REDACTED]">\n'
  const rewritten = `## Original notes\n\n<img src="https://cdn.example.test/build.png?to&#107en=LIVESECRET">\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an unquoted HTML credential URL', () => {
  const original = '<img src="https://cdn.example.test/build.png?token=[REDACTED]">\n'
  const rewritten = `## Original notes\n\n<img src=https&#58;//cdn.example.test/build.png?token&#61;LIVESECRET>\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

test('CLI: mandatory guards reject an unredacted escaped-label reference credential', () => {
  const original =
    '![build][asset\\]id]\n\n[asset\\]id]: <https://cdn.example.test/build.png?token=[REDACTED]>\n'
  const rewritten = `## Original notes\n\n![build][asset\\]id]\n\n[asset\\]id]: <https://cdn.example.test/build.png?token=LIVESECRET>\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
  assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
})

for (const key of ['jwt', 'sessionid']) {
  test(`CLI: mandatory guards reject an unredacted ${key} credential query value`, () => {
    const original = `![a](https://cdn.example.test/image.png?${key}=[REDACTED])\n`
    const rewritten = `## Original notes\n\n![a](https://cdn.example.test/image.png?${key}=LIVESECRET)\n`
    const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
    assert.equal(res.status, 1)
    assert.match(res.stderr, /external image URL\(s\) with unredacted credential query values/)
    assert.ok(!res.stderr.includes('LIVESECRET'), 'the guard must not print credential values')
  })
}

test('CLI: --require-unsigned-uploads rejects a signed rewritten upload URL', () => {
  const signed = `![a](${UPLOAD}?signature=still-secret)`
  const res = runCli(signed, signed, ['--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /1 signed upload URL/)
  assert.match(res.stderr, /strip query strings before write/)
})

test('CLI: mandatory guards reject a signed scheme-relative rewritten upload URL', () => {
  const original = `![a](${UPLOAD})\n`
  const rewritten = `## Screenshots\n\n![a](//uploads.linear.app/abc-123/screenshot.png?signature=still-secret)\n\n## Original notes\n\n${original}`
  const res = runCli(original, rewritten, ['--require-verbatim', '--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /1 signed upload URL/)
  assert.match(res.stderr, /strip query strings before write/)
})

test('CLI: --require-unsigned-uploads rejects a signed upload with an escaped closing parenthesis', () => {
  const escaped = '![a](https://uploads.linear.app/foo\\)?signature=still-secret)'
  const res = runCli(escaped, escaped, ['--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /1 signed upload URL/)
})

test('CLI: --require-unsigned-uploads rejects a signed bare upload URL with balanced parentheses', () => {
  const signed = 'https://uploads.linear.app/foo(bar)?signature=still-secret'
  const res = runCli(signed, signed, ['--require-unsigned-uploads'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /1 signed upload URL/)
})

test('CLI: --require-verbatim preserves punctuation after an upload URL', () => {
  const original = `See ${UPLOAD}?signature=old.\n`
  const rewritten = `## Original notes\n\nSee ${UPLOAD}\n`
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes content difference at line 1,/)
})

test('CLI: --require-verbatim preserves ordinary Markdown backslashes', () => {
  const original = 'Windows path: C:\\temp\n'
  const rewritten = '## Original notes\n\nWindows path: C:temp\n'
  const res = runCli(original, rewritten, ['--require-verbatim'])
  assert.equal(res.status, 1)
  assert.match(res.stderr, /Original notes content difference at line 1,/)
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
