#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import {
  checkDocsAbsoluteSelfLinks,
  discoverDocFiles,
  findAbsoluteSelfLinks,
  scanAbsoluteSelfLinks,
} from './check-docs-absolute-selflinks.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// Capture what the gate prints so a test can assert on the failure text a
// contributor actually sees, not just the boolean.
function captureConsole(run) {
  const out = []
  const err = []
  const originalLog = console.log
  const originalError = console.error
  console.log = (...args) => out.push(args.join(' '))
  console.error = (...args) => err.push(args.join(' '))
  try {
    const result = run()
    return { result, out, err }
  } finally {
    console.log = originalLog
    console.error = originalError
  }
}

function makeTempRepo() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'check-docs-absolute-selflinks-'))
}

function writeDoc(repoRoot, relativePath, contents) {
  const docPath = path.join(repoRoot, 'services', 'docs', 'docs', relativePath)
  fs.mkdirSync(path.dirname(docPath), { recursive: true })
  fs.writeFileSync(docPath, contents)
}

test('findAbsoluteSelfLinks finds an absolute self-link in an inline markdown link', () => {
  const doc = [
    '# MCP',
    '',
    'See [Notes](https://docs.bossanova.dev/guides/notes) for detail.',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [
    { url: 'https://docs.bossanova.dev/guides/notes', line: 3 },
  ])
})

test('findAbsoluteSelfLinks finds a bare absolute self-link URL', () => {
  const doc = ['Read https://docs.bossanova.dev/guides/broadcasts.', ''].join('\n')

  // The sentence-ending period is trimmed so the reported URL is the link, not
  // the prose around it.
  assert.deepEqual(findAbsoluteSelfLinks(doc), [
    { url: 'https://docs.bossanova.dev/guides/broadcasts', line: 1 },
  ])
})

test('findAbsoluteSelfLinks finds an absolute self-link in an MDX href', () => {
  const doc = ['<Card href="https://docs.bossanova.dev/quick-start" title="Start" />'].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [
    { url: 'https://docs.bossanova.dev/quick-start', line: 1 },
  ])
})

test('findAbsoluteSelfLinks ignores an absolute link to a non-docs host', () => {
  const doc = [
    'Source on [GitHub](https://github.com/bossanova/bossanova/blob/main/README.md).',
    'Marketing lives at https://bossanova.dev/pricing.',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [])
})

test('findAbsoluteSelfLinks ignores the bare docs site root, which names no page', () => {
  const doc = [
    'The site is [docs.bossanova.dev](https://docs.bossanova.dev).',
    'Also written https://docs.bossanova.dev/ sometimes.',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [])
})

test('findAbsoluteSelfLinks honours the escape-hatch marker on the preceding line', () => {
  const doc = [
    '<!-- absolute-link: intentional -->',
    'Paste this into a browser: https://docs.bossanova.dev/guides/notes',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [])
})

test('findAbsoluteSelfLinks honours the escape-hatch marker on the offending line', () => {
  const doc = ['Visit https://docs.bossanova.dev/guides/notes <!-- absolute-link: intentional -->']

  assert.deepEqual(findAbsoluteSelfLinks(doc.join('\n')), [])
})

test('findAbsoluteSelfLinks does not let a marker cover a later unmarked line', () => {
  const doc = [
    '<!-- absolute-link: intentional -->',
    'Marked: https://docs.bossanova.dev/guides/notes',
    'Unmarked: https://docs.bossanova.dev/guides/broadcasts',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [
    { url: 'https://docs.bossanova.dev/guides/broadcasts', line: 3 },
  ])
})

test('findAbsoluteSelfLinks ignores absolute self-links inside a fenced code block', () => {
  const doc = [
    'Prose link: https://docs.bossanova.dev/guides/notes',
    '',
    '```bash',
    'curl https://docs.bossanova.dev/guides/broadcasts',
    '```',
    '',
    'Another: https://docs.bossanova.dev/guides/web',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [
    { url: 'https://docs.bossanova.dev/guides/notes', line: 1 },
    { url: 'https://docs.bossanova.dev/guides/web', line: 7 },
  ])
})

test('findAbsoluteSelfLinks records every hit on a line with its line number', () => {
  const doc = [
    'intro',
    '[a](https://docs.bossanova.dev/guides/notes) and [b](https://docs.bossanova.dev/guides/web)',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [
    { url: 'https://docs.bossanova.dev/guides/notes', line: 2 },
    { url: 'https://docs.bossanova.dev/guides/web', line: 2 },
  ])
})

test('scanAbsoluteSelfLinks separates findings from marked exceptions in one pass', () => {
  const doc = [
    'Unmarked: https://docs.bossanova.dev/guides/notes',
    '<!-- absolute-link: intentional -->',
    'Marked: https://docs.bossanova.dev/guides/web and https://docs.bossanova.dev/guides/logging',
    '```bash',
    'curl https://docs.bossanova.dev/guides/fenced',
    '```',
  ].join('\n')

  // The fenced hit is neither a finding nor an exception — it is not a link.
  assert.deepEqual(scanAbsoluteSelfLinks(doc), {
    unmarked: [{ url: 'https://docs.bossanova.dev/guides/notes', line: 1 }],
    marked: 2,
    unterminatedFence: null,
  })
})

test('a marker on the offending line excuses every URL on that line', () => {
  // Deliberately over-scoped: the marker is per-line, so a line carrying two
  // absolute links is excused twice. Pinned because the alternative reading
  // (first URL only) would be a silent partial pass.
  const doc =
    'Both https://docs.bossanova.dev/guides/notes and https://docs.bossanova.dev/guides/web ' +
    '<!-- absolute-link: intentional -->'

  assert.deepEqual(scanAbsoluteSelfLinks(doc), {
    unmarked: [],
    marked: 2,
    unterminatedFence: null,
  })
})

test('an inner ``` inside a ````-delimited fence does not blind the scanner', () => {
  // The regression the naive `^\s*``` ` toggle had: the inner fence flipped the
  // state, so everything after it read as prose-in-code and was never scanned.
  const doc = [
    '````markdown',
    '```bash',
    'curl https://docs.bossanova.dev/guides/fenced',
    '```',
    '````',
    '',
    'Prose after: https://docs.bossanova.dev/guides/notes',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [
    { url: 'https://docs.bossanova.dev/guides/notes', line: 7 },
  ])
})

test('tilde fences are tracked, and a backtick run does not close them', () => {
  const doc = [
    '~~~bash',
    'curl https://docs.bossanova.dev/guides/fenced',
    '```',
    'curl https://docs.bossanova.dev/guides/still-fenced',
    '~~~',
    '',
    'Prose: https://docs.bossanova.dev/guides/notes',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [
    { url: 'https://docs.bossanova.dev/guides/notes', line: 7 },
  ])
})

test('a closing fence run may be longer than the opener', () => {
  const doc = [
    '```',
    'curl https://docs.bossanova.dev/guides/fenced',
    '`````',
    'Prose: https://docs.bossanova.dev/guides/notes',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [
    { url: 'https://docs.bossanova.dev/guides/notes', line: 4 },
  ])
})

test('a closing fence run shorter than the opener does not close it', () => {
  // The half of the rule the longer-closer case above cannot pin: under the old
  // toggle the short run closed the fence and the URL below it was scanned.
  const doc = [
    '````',
    'curl https://docs.bossanova.dev/guides/fenced',
    '```',
    'still fenced: https://docs.bossanova.dev/guides/notes',
    '````',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [])
})

test('a CRLF document does not read a closed fence as unterminated', () => {
  // Regression: splitting on '\n' alone leaves a trailing \r on every line, so
  // the closing fence never matches and a valid page hard-fails `make lint`
  // with "fence opened here and never closed" — while its real findings go
  // unreported, because the scanner believes it is still inside code.
  const doc =
    '# Title\r\n\r\n```bash\r\ncurl https://docs.bossanova.dev/guides/fenced\r\n```\r\n' +
    '\r\nProse: https://docs.bossanova.dev/guides/notes\r\n'

  assert.deepEqual(scanAbsoluteSelfLinks(doc), {
    unmarked: [{ url: 'https://docs.bossanova.dev/guides/notes', line: 7 }],
    marked: 0,
    unterminatedFence: null,
  })
})

test('scanAbsoluteSelfLinks reports an unterminated fence rather than going quiet', () => {
  const doc = [
    '```bash',
    'curl https://docs.bossanova.dev/guides/fenced',
    '',
    'Prose the scanner never reaches: https://docs.bossanova.dev/guides/notes',
  ].join('\n')

  assert.deepEqual(scanAbsoluteSelfLinks(doc), {
    unmarked: [],
    marked: 0,
    unterminatedFence: 1,
  })
})

test('checkDocsAbsoluteSelfLinks fails loudly on an unterminated fence', () => {
  const repoRoot = makeTempRepo()
  writeDoc(repoRoot, 'guides/mcp.md', ['# MCP', '', '```bash', 'echo hi', ''].join('\n'))

  const { result, err } = captureConsole(() => checkDocsAbsoluteSelfLinks(repoRoot))

  assert.equal(result, false)
  assert.ok(
    err.includes('services/docs/docs/guides/mcp.md:3: fence opened here and never closed'),
    `expected the unterminated-fence miss, got:\n${err.join('\n')}`,
  )
})

test('findAbsoluteSelfLinks matches http and mixed-case spellings of the same host', () => {
  const doc = [
    'Plain http: http://docs.bossanova.dev/guides/notes',
    'Shouted: https://Docs.Bossanova.DEV/guides/web',
  ].join('\n')

  assert.deepEqual(findAbsoluteSelfLinks(doc), [
    { url: 'http://docs.bossanova.dev/guides/notes', line: 1 },
    { url: 'https://Docs.Bossanova.DEV/guides/web', line: 2 },
  ])
})

test('checkDocsAbsoluteSelfLinks passes a docs tree with only relative self-links', () => {
  const repoRoot = makeTempRepo()
  writeDoc(repoRoot, 'guides/mcp.md', 'See [Notes](./notes.md) and [Web](./web.md).\n')
  writeDoc(repoRoot, 'guides/notes.mdx', '# Notes\n\nExternal: https://github.com/x/y\n')

  const { result, out } = captureConsole(() => checkDocsAbsoluteSelfLinks(repoRoot))

  assert.equal(result, true)
  assert.match(out.join('\n'), /Docs absolute self-links OK \(2 doc file\(s\) scanned/)
})

test('checkDocsAbsoluteSelfLinks names the file, line, and URL of an offender', () => {
  const repoRoot = makeTempRepo()
  writeDoc(
    repoRoot,
    'guides/mcp.md',
    [
      '# MCP',
      '',
      'The note tools have their own guide:',
      '',
      '[Notes](https://docs.bossanova.dev/guides/notes) covers them.',
      '',
    ].join('\n'),
  )

  const { result, err } = captureConsole(() => checkDocsAbsoluteSelfLinks(repoRoot))

  assert.equal(result, false)
  assert.ok(
    err.includes('services/docs/docs/guides/mcp.md:5: https://docs.bossanova.dev/guides/notes'),
    `expected a file:line: URL miss, got:\n${err.join('\n')}`,
  )
})

test('checkDocsAbsoluteSelfLinks counts a marked exception without failing', () => {
  const repoRoot = makeTempRepo()
  writeDoc(
    repoRoot,
    'guides/mcp.md',
    [
      '<!-- absolute-link: intentional -->',
      'Share https://docs.bossanova.dev/guides/notes',
      '',
    ].join('\n'),
  )

  const { result, out } = captureConsole(() => checkDocsAbsoluteSelfLinks(repoRoot))

  assert.equal(result, true)
  assert.match(out.join('\n'), /1 marked exception\(s\)/)
})

test('checkDocsAbsoluteSelfLinks fails when the docs site is present but the docs tree has moved', () => {
  const repoRoot = makeTempRepo()
  // The site is here, but the tree DOCS_DIR names is not — a relocation or a
  // stale constant. Without the tripwire this returns true after scanning
  // nothing, and "no offenders found" is this gate's ordinary success text.
  fs.mkdirSync(path.join(repoRoot, 'services', 'docs', 'content'), { recursive: true })

  const { result, err } = captureConsole(() => checkDocsAbsoluteSelfLinks(repoRoot))

  assert.equal(result, false)
  assert.match(err.join('\n'), /no \.md\/\.mdx under services\/docs\/docs/)
})

test('checkDocsAbsoluteSelfLinks still passes a checkout with no docs site at all', () => {
  const repoRoot = makeTempRepo()

  const { result } = captureConsole(() => checkDocsAbsoluteSelfLinks(repoRoot))

  assert.equal(result, true)
})

test('checkDocsAbsoluteSelfLinks passes the real repository over a non-empty file set', () => {
  const { result, out } = captureConsole(() => checkDocsAbsoluteSelfLinks(REPO_ROOT))

  assert.equal(result, true)
  // Coverage floor, not decoration. This gate's success state is "found
  // nothing", which is byte-identical to a run that scanned nothing — so the
  // boolean alone cannot tell a real pass from a moved docs tree. Pin the
  // scanned-file count above zero instead; the unit cases above are what prove
  // the detector still fires.
  const counts = /Docs absolute self-links OK \((\d+) doc file\(s\) scanned/.exec(out.join('\n'))
  assert.ok(counts, `expected the OK line with a scanned-file count, got:\n${out.join('\n')}`)
  assert.ok(Number(counts[1]) > 0, `expected a non-zero scanned-file count, got ${counts[1]}`)
})

test('discoverDocFiles walks nested docs and returns [] for a missing tree', () => {
  const repoRoot = makeTempRepo()
  writeDoc(repoRoot, 'guides/notes.md', 'a\n')
  writeDoc(repoRoot, 'reference/cli.mdx', 'b\n')
  writeDoc(repoRoot, 'guides/image.png', 'c\n')

  const docsDir = path.join(repoRoot, 'services', 'docs', 'docs')
  assert.deepEqual(
    discoverDocFiles(docsDir).map((docPath) => path.relative(docsDir, docPath)),
    [path.join('guides', 'notes.md'), path.join('reference', 'cli.mdx')],
  )
  assert.deepEqual(discoverDocFiles(path.join(repoRoot, 'nope')), [])
})
