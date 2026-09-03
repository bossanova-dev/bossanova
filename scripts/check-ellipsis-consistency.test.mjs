#!/usr/bin/env node

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import {
  EXEMPTIONS,
  IGNORED_DIRECTORIES,
  IGNORED_EXTENSIONS,
  OPT_OUT_MARKER,
  SCAN_TREES,
  checkEllipsisConsistency,
  discoverScanFiles,
  scanFile,
} from './check-ellipsis-consistency.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const GATE = path.join(REPO_ROOT, 'scripts', 'check-ellipsis-consistency.mjs')

// The run under test, assembled rather than typed — for the same reason the gate assembles it. A
// fixture written with the literal would put the pattern into this file, where a repo-wide audit
// for remaining uses would find it and have to decide whether it counted.
const DOTS = String.fromCharCode(46).repeat(3)

// Fixtures are built from single-quoted lines and interpolated, never written in the syntax under
// test. The two places this file does spell the run out are deliberate and cannot be assembled:
// the Go variadic fixtures below, where the syntax IS the thing being proved harmless, and the rest
// parameters in captureConsole, which is the house idiom copied from its sibling gates' tests.
const fixture = (lines) => lines.join('\n') + '\n'

// Capture what the gate prints, so an assertion can read the text a contributor actually sees
// rather than only the boolean.
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

function write(root, relative, contents) {
  const full = path.join(root, relative)
  fs.mkdirSync(path.dirname(full), { recursive: true })
  fs.writeFileSync(full, contents)
  return full
}

// A synthetic repository holding every declared scan tree, each with one clean file — so a test
// adding one more file is testing its own fixture rather than the coverage tripwires.
//
// DERIVED FROM SCAN_TREES, never a hand-listed copy of it. A hardcoded list turns every coverage
// tripwire in this file red the moment the gate's scan surface changes, which reads as "the gate
// broke" when the truth is "the fixture went stale" — and the person widening the surface then has
// to tell the two apart across twenty failures. Deriving it means adding a tree needs no edit here,
// and the baseline count below cannot drift out of step with the trees it counts.
function cleanFileFor(tree) {
  const extension = tree.extensions[0]
  const segment = tree.path.split('/').filter(Boolean).pop()
  const identifier = segment.replace(/[^A-Za-z0-9_]/g, '_')
  const relative = `${tree.path}/clean${extension}`
  if (extension === '.go') {
    return [relative, fixture([`package ${identifier}`, '', 'const clean = 1'])]
  }
  return [relative, fixture([`export const clean_${identifier} = 1`])]
}

function makeRepo() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'check-ellipsis-consistency-'))
  for (const tree of SCAN_TREES) {
    const [relative, contents] = cleanFileFor(tree)
    write(root, relative, contents)
  }
  return root
}

const BASELINE_FILES = SCAN_TREES.length

function run(root, exemptions = []) {
  return captureConsole(() => checkEllipsisConsistency({ root, exemptions }))
}

// ---------------------------------------------------------------------------
// The rule fires
// ---------------------------------------------------------------------------

test('the rule fires on a rendered Go string', () => {
  const root = makeRepo()
  write(
    root,
    'services/boss/internal/views/newsession.go',
    fixture(['package views', '', `const status = "Cloning repository${DOTS}"`]),
  )

  const { result, err } = run(root)

  assert.equal(result, false)
  const joined = err.join('\n')
  assert.match(joined, /services\/boss\/internal\/views\/newsession\.go:3/)
  assert.match(joined, /Cloning\s+repository/)
})

test('the rule fires on a Go raw string literal, across the line it actually sits on', () => {
  const root = makeRepo()
  write(
    root,
    'services/boss/internal/views/help.go',
    fixture(['package views', '', 'const help = `first line', `second line${DOTS}`, '`']),
  )

  const { result, err } = run(root)

  assert.equal(result, false)
  assert.match(err.join('\n'), /services\/boss\/internal\/views\/help\.go:4/)
})

test('the rule fires on a TSX attribute string and on a JSX text node', () => {
  const root = makeRepo()
  write(
    root,
    'services/web/src/pages/Search.tsx',
    fixture([
      'export function Search() {',
      `  return <input placeholder="Search sessions${DOTS}" />`,
      '}',
      'export function Waiting() {',
      `  return <p>Loading${DOTS}</p>`,
      '}',
    ]),
  )

  const { result, err } = run(root)

  assert.equal(result, false)
  const joined = err.join('\n')
  assert.match(joined, /services\/web\/src\/pages\/Search\.tsx:2/)
  assert.match(joined, /services\/web\/src\/pages\/Search\.tsx:5/)
})

test('the rule fires inside a template literal and inside its interpolated expressions', () => {
  const root = makeRepo()
  write(
    root,
    'services/web/src/status.ts',
    fixture([
      'export const outer = (n: number) => `Fetching ${n} sessions' + DOTS + '`',
      'export const inner = (n: number) => `${n === 0 ? ' + `'Idle${DOTS}'` + " : 'Busy'}`",
    ]),
  )

  const { result, err } = run(root)

  assert.equal(result, false)
  const joined = err.join('\n')
  assert.match(joined, /services\/web\/src\/status\.ts:1/)
  assert.match(joined, /services\/web\/src\/status\.ts:2/)
})

// ---------------------------------------------------------------------------
// Language syntax that shares the spelling
// ---------------------------------------------------------------------------

test('JSX and JavaScript spread do not fire, though the same run in a string right beside them does', () => {
  const root = makeRepo()
  write(
    root,
    'services/web/src/components/Row.tsx',
    fixture([
      'export function Row(props: Props) {',
      `  const merged = { ${DOTS}defaults, ${DOTS}props }`,
      `  const list = [${DOTS}items]`,
      `  return <div {${DOTS}merged}>{list}</div>`,
      '}',
    ]),
  )

  assert.equal(run(root).result, true)

  // Same file, one string added: the fixture is only proof of a skip if it would otherwise be a
  // finding, so the scanner is shown to be looking at this file at all.
  write(
    root,
    'services/web/src/components/Row.tsx',
    fixture([
      'export function Row(props: Props) {',
      `  const merged = { ${DOTS}defaults, ${DOTS}props }`,
      `  return <div {${DOTS}merged}>{'Loading${DOTS}'}</div>`,
      '}',
    ]),
  )

  const { result, err } = run(root)
  assert.equal(result, false)
  assert.match(err.join('\n'), /services\/web\/src\/components\/Row\.tsx:3/)
})

test('Go variadic parameters and call spreads do not fire, though a string on the same line does', () => {
  const root = makeRepo()
  write(
    root,
    'services/boss/internal/auth/args.go',
    fixture([
      'package auth',
      '',
      'func join(sep string, args ...string) string {',
      `\treturn strings.Join(args, sep) + fmt.Sprint(more${DOTS})`,
      '}',
    ]),
  )

  assert.equal(run(root).result, true)

  write(
    root,
    'services/boss/internal/auth/args.go',
    fixture([
      'package auth',
      '',
      'func join(sep string, args ...string) string {',
      `\treturn fmt.Sprintf("Signing in${DOTS}", args${DOTS})`,
      '}',
    ]),
  )

  const { result, err } = run(root)
  assert.equal(result, false)
  assert.match(err.join('\n'), /services\/boss\/internal\/auth\/args\.go:4/)
})

// ---------------------------------------------------------------------------
// The enumerated exclusions, for grammars that live inside string literals
// ---------------------------------------------------------------------------

test('a git revision range in a string is excused by an EXEMPTIONS entry, not by a pattern', () => {
  const root = makeRepo()
  write(
    root,
    'services/boss/internal/views/compare.go',
    fixture([
      'package views',
      '',
      `\trange := baseRef + "${DOTS}HEAD"`,
      `\tstatus := "Comparing${DOTS}"`,
    ]),
  )

  // Unexcused, BOTH lines are findings — which is what makes the excused run below meaningful.
  const before = run(root)
  assert.equal(before.result, false)
  assert.match(before.err.join('\n'), /compare\.go:3/)
  assert.match(before.err.join('\n'), /compare\.go:4/)

  const exemptions = [
    {
      path: 'services/boss/internal/views/compare.go',
      text: 'baseRef',
      reason: 'a git revision-range operator, not copy',
    },
  ]
  const after = run(root, exemptions)
  assert.equal(after.result, false, 'the exemption is line-scoped and must not excuse line 4')
  assert.doesNotMatch(after.err.join('\n'), /compare\.go:3/)
  assert.match(after.err.join('\n'), /compare\.go:4/)
})

test('Cobra variadic-argument notation in a Use string is excused by the inline marker', () => {
  const root = makeRepo()
  write(
    root,
    'services/boss/internal/views/commands.go',
    fixture([
      'package views',
      '',
      `\tcmd := &cobra.Command{Use: "tail [source${DOTS}]"}`,
      `\trename := &cobra.Command{Use: "rename <session-id> <new-title${DOTS}>"}`,
    ]),
  )

  const before = run(root)
  assert.equal(before.result, false)

  write(
    root,
    'services/boss/internal/views/commands.go',
    fixture([
      'package views',
      '',
      '\t// A usage grammar cobra parses, not a sentence: ellipsis: literal-dots ok',
      `\tcmd := &cobra.Command{Use: "tail [source${DOTS}]"}`,
      `\trename := &cobra.Command{Use: "rename <session-id> <new-title${DOTS}>"} // ellipsis: literal-dots ok`,
    ]),
  )

  const { result, out } = run(root)
  assert.equal(result, true)
  assert.match(out.join('\n'), /2 marked exception\(s\)/)
})

test('the inline marker is honoured on the offending line and the line above, but no further', () => {
  const root = makeRepo()
  write(
    root,
    'services/boss/internal/views/marks.go',
    fixture([
      'package views',
      '',
      `const own = "one${DOTS}" // ellipsis: literal-dots ok`,
      '// ellipsis: literal-dots ok',
      `const above = "two${DOTS}"`,
      '// ellipsis: literal-dots ok',
      '',
      `const twoAbove = "three${DOTS}"`,
    ]),
  )

  const { result, err, out } = run(root)

  assert.equal(result, false)
  assert.match(err.join('\n'), /marks\.go:8/)
  assert.doesNotMatch(err.join('\n'), /marks\.go:3/)
  assert.doesNotMatch(err.join('\n'), /marks\.go:5/)
  assert.deepEqual(out, [])
})

test('the inline marker is matched whitespace-tolerantly, so an audit grep must be too', () => {
  assert.ok(OPT_OUT_MARKER.test('// ellipsis:   literal-dots\tok'))
  assert.ok(!OPT_OUT_MARKER.test('// ellipsis:literal-dots ok'))
})

// A gate is read through its file:line and its opt-out. Both used to be derived from a
// `split(/\r?\n/)` whose running offset advanced by one character per line, so on a CRLF file every
// line start after the first drifted — the finding was reported one line late with empty text, and
// the marker lookup read the wrong line and stopped suppressing anything. That last part is the
// costly half: the contributor writes the documented opt-out and the gate goes on rejecting.
test('CRLF line endings do not shift the reported line, its text, or the marker lookup', () => {
  // Enough filler that the accumulated drift is at least one whole line by the offender.
  const filler = Array.from({ length: 10 }, (_, i) => `const pad${i} = ${i}`)
  const body = ['package views', '', ...filler, `\tvar msg = "Loading${DOTS}"`]
  const offenderLine = body.length

  const lf = scanFile(body.join('\n') + '\n', 'go')
  const crlf = scanFile(body.join('\r\n') + '\r\n', 'go')

  // The premise: without enough lines the drift would not cross a line boundary and this test
  // would pass against the bug.
  assert.ok(offenderLine > 3, 'fixture needs enough lines for the drift to be observable')
  assert.deepEqual(lf.findings, [{ line: offenderLine, text: `var msg = "Loading${DOTS}"` }])
  assert.deepEqual(
    crlf.findings,
    lf.findings,
    'a CRLF file must report the same line and the same text as the LF file',
  )
})

test('an inline marker is honoured on a CRLF file', () => {
  const root = makeRepo()
  const filler = Array.from({ length: 10 }, (_, i) => `const pad${i} = ${i}`)
  const body = [
    'package views',
    '',
    ...filler,
    '// ellipsis: literal-dots ok',
    `\tvar msg = "Loading${DOTS}"`,
  ]

  // Unmarked and CRLF, this is a finding — so the marked run below is not passing vacuously.
  write(
    root,
    'services/boss/internal/views/crlf.go',
    body.filter((l) => !l.includes('ok')).join('\r\n') + '\r\n',
  )
  assert.equal(run(root).result, false)

  write(root, 'services/boss/internal/views/crlf.go', body.join('\r\n') + '\r\n')
  const { result, out } = run(root)

  assert.equal(result, true, 'the marker must suppress the finding under CRLF exactly as under LF')
  assert.match(out.join('\n'), /1 marked exception\(s\)/)
})

test('a stale EXEMPTIONS entry ratchets: an entry that matches nothing fails the gate', () => {
  const root = makeRepo()
  write(
    root,
    'services/boss/internal/views/converted.go',
    fixture(['package views', '', 'const status = "Cloning repository…"']),
  )

  const exemptions = [
    {
      path: 'services/boss/internal/views/converted.go',
      text: 'Cloning',
      reason: 'once a survivor, since converted',
    },
  ]

  const { result, err } = run(root, exemptions)

  assert.equal(result, false)
  assert.match(err.join('\n'), /converted\.go: exemption for "Cloning" no longer matches anything/)
})

test('a whole-file EXEMPTIONS entry excuses every occurrence and still ratchets', () => {
  const root = makeRepo()
  write(
    root,
    'services/web/src/machine.ts',
    fixture([`export const a = 'x${DOTS}y'`, `export const b = 'p${DOTS}q'`]),
  )
  const exemptions = [
    { path: 'services/web/src/machine.ts', text: null, reason: 'machine-facing, replicated' },
  ]

  const excused = run(root, exemptions)
  assert.equal(excused.result, true)
  assert.match(excused.out.join('\n'), /2 marked exception\(s\)/)

  write(root, 'services/web/src/machine.ts', fixture(["export const a = 'x…y'"]))
  const stale = run(root, exemptions)
  assert.equal(stale.result, false)
  assert.match(stale.err.join('\n'), /machine\.ts: exemption for the whole file no longer matches/)
})

// ---------------------------------------------------------------------------
// Excluded paths, each proved with a file that would otherwise be a finding
// ---------------------------------------------------------------------------

test('comments are not scanned, in either language, though code on the next line still is', () => {
  const root = makeRepo()
  write(
    root,
    'services/boss/internal/views/commented.go',
    fixture([
      'package views',
      '',
      `// A path abbreviated: /Applications/Ghostty.app/${DOTS}/ghostty`,
      `/* And a call shape: Padding(0, ${DOTS}) */`,
      'const ok = "converged…"',
    ]),
  )
  write(
    root,
    'services/web/src/commented.ts',
    fixture([
      `// A truncation marker: '${DOTS}[truncated]'`,
      `/* and a type sketch: { a, b, ${DOTS} } */`,
      "export const ok = 'converged…'",
    ]),
  )

  assert.equal(run(root).result, true)

  // Proof the two files are read at all: move one run out of its comment and into a string.
  write(
    root,
    'services/web/src/commented.ts',
    fixture([
      `// A truncation marker: '${DOTS}[truncated]'`,
      `export const ok = 'converged${DOTS}'`,
    ]),
  )
  const { result, err } = run(root)
  assert.equal(result, false)
  assert.match(err.join('\n'), /services\/web\/src\/commented\.ts:2/)
})

test('a regular-expression source is not a use of the run, though a string on the next line is', () => {
  const root = makeRepo()
  write(
    root,
    'services/web/src/patterns.ts',
    fixture(['export const re = /a\\.\\.\\.b/g', `export const label = 'Waiting${DOTS}'`]),
  )

  const { result, err } = run(root)

  assert.equal(result, false)
  assert.match(err.join('\n'), /services\/web\/src\/patterns\.ts:2/)
  assert.doesNotMatch(err.join('\n'), /patterns\.ts:1/)
})

test('every ignored extension is skipped, and the same bytes in a scanned extension are a finding', () => {
  for (const extension of IGNORED_EXTENSIONS) {
    const root = makeRepo()
    const body = fixture([`the label is "Loading${DOTS}" here`])
    write(root, `services/web/src/copy${extension}`, body)
    assert.equal(run(root).result, true, `${extension} must be skipped`)

    write(root, 'services/web/src/copy.ts', fixture([`export const label = 'Loading${DOTS}'`]))
    assert.equal(run(root).result, false, `${extension}: the same bytes in a .ts file must fire`)
  }
})

test('every ignored directory is skipped, and the same file outside it is a finding', () => {
  for (const directory of IGNORED_DIRECTORIES) {
    const root = makeRepo()
    const body = fixture([`export const label = 'Loading${DOTS}'`])
    write(root, `services/web/src/${directory}/copy.ts`, body)
    assert.equal(run(root).result, true, `${directory}/ must be skipped`)

    write(root, 'services/web/src/copy.ts', body)
    assert.equal(run(root).result, false, `${directory}: the same file outside it must fire`)
  }
})

// ---------------------------------------------------------------------------
// The three fail-closed tripwires
// ---------------------------------------------------------------------------

test('TRIPWIRE: an empty file set fails rather than reporting a clean run', () => {
  // "0 checked, exit 0" is byte-identical to a clean corpus, so a gate has to bound its own corpus.
  // Every tree is present and holds a file — just not one this gate scans.
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'check-ellipsis-consistency-empty-'))
  for (const tree of SCAN_TREES) {
    write(root, `${tree.path}/README.md`, fixture([`a label: Loading${DOTS}`]))
  }

  const { result, err, out } = run(root)

  assert.equal(result, false)
  assert.deepEqual(out, [])
  assert.match(err.join('\n'), /scanned no files at all/)
  for (const tree of SCAN_TREES) {
    assert.match(err.join('\n'), new RegExp(tree.path.replace(/\//g, '\\/')))
  }
})

test('a single scan tree that covers nothing is a reported coverage hole, not a silent skip', () => {
  const root = makeRepo()
  fs.rmSync(path.join(root, 'services/boss/internal/auth/clean.go'))

  const { result, err } = run(root)

  assert.equal(result, false)
  assert.match(err.join('\n'), /services\/boss\/internal\/auth: scan tree holds no \.go file/)
})

test('TRIPWIRE: an unreadable file fails the gate rather than being skipped past', () => {
  const root = makeRepo()
  // A dangling symlink standing where a scanned file should be: listed by readdir, and gone by the
  // time it is opened. Dropping it at discovery would be the silent skip this tripwire forbids.
  fs.symlinkSync(
    path.join(root, 'services/boss/internal/views/absent.go'),
    path.join(root, 'services/boss/internal/views/broken.go'),
  )

  const { result, err, out } = run(root)

  assert.equal(result, false)
  assert.deepEqual(out, [])
  const joined = err.join('\n')
  assert.match(joined, /broken\.go: unreadable/)
  assert.match(joined, /could\s+not\s+check\s+every\s+file/)
})

test('TRIPWIRE: an unparseable file fails the gate, so an offender inside it is never passed over', () => {
  const root = makeRepo()
  // One offending string, then an unterminated template literal that desyncs the walk. A gate that
  // discarded the desync quietly would return true here having scanned nothing in this file.
  write(
    root,
    'services/web/src/broken.ts',
    fixture([`export const label = 'Loading${DOTS}'`, 'export const open = `never closed']),
  )

  const { result, err, out } = run(root)

  assert.equal(result, false)
  assert.deepEqual(out, [])
  const joined = err.join('\n')
  assert.match(joined, /broken\.ts: template\s+literal\s+never\s+closed/)
  assert.match(joined, /could\s+not\s+check\s+every\s+file/)
})

test('TRIPWIRE: an extension in neither the scan set nor the ignore set fails, naming the file', () => {
  const root = makeRepo()
  write(root, 'services/web/src/copy.txt', fixture([`a label: Loading${DOTS}`]))

  const { result, err, out } = run(root)

  assert.equal(result, false)
  assert.deepEqual(out, [])
  const joined = err.join('\n')
  assert.match(joined, /services\/web\/src\/copy\.txt/)
  assert.match(joined, /neither\s+the\s+scan\s+set\s+nor\s+the\s+ignore\s+set/)
})

test('a declared scan tree that is missing is reported by name', () => {
  const root = makeRepo()
  fs.rmSync(path.join(root, 'services/boss/internal/accountflow'), { recursive: true })

  const { result, err } = run(root)

  assert.equal(result, false)
  assert.match(
    err.join('\n'),
    /services\/boss\/internal\/accountflow: declared scan tree is missing/,
  )
})

// ---------------------------------------------------------------------------
// What the gate says when it passes
// ---------------------------------------------------------------------------

test('the success line states its own scope and its residual', () => {
  const root = makeRepo()
  write(
    root,
    'services/web/src/copy.ts',
    fixture([`export const label = 'Loading${DOTS}' // ellipsis: literal-dots ok`]),
  )

  const { result, out, err } = run(root)

  assert.equal(result, true)
  assert.deepEqual(err, [])
  assert.equal(
    out.join('\n'),
    `Ellipsis consistency OK (${BASELINE_FILES + 1} file(s) scanned, 1 marked exception(s))`,
  )
})

test('discoverScanFiles reports the kind of each file it hands the scanner', () => {
  const root = makeRepo()
  write(root, 'services/web/src/page.tsx', fixture(['export const x = 1']))

  const { files, problems } = discoverScanFiles(root)

  assert.deepEqual(problems, [])
  // Derived from SCAN_TREES for the reason makeRepo is: this test is about the KIND each file is
  // handed the scanner under, not about which trees are declared, so a hand-listed expectation
  // would fail on a surface change that this assertion has no opinion about.
  const expected = [
    ...SCAN_TREES.map((tree) => {
      const [relative] = cleanFileFor(tree)
      return `${relative}:${tree.extensions[0].slice(1)}`
    }),
    'services/web/src/page.tsx:tsx',
  ].sort()
  assert.deepEqual(files.map((file) => `${file.relative}:${file.kind}`).sort(), expected)
})

test('scanFile reports one finding per line, whatever the number of runs on it', () => {
  const { findings, desync } = scanFile(
    fixture([`const a = "one${DOTS}two${DOTS}three"`, 'const b = "clean"']),
    'go',
  )

  assert.equal(desync, null)
  assert.deepEqual(
    findings.map((finding) => finding.line),
    [1],
  )
})

// ---------------------------------------------------------------------------
// The CLI contract the Makefile and CI depend on
// ---------------------------------------------------------------------------

test('the main block exits 1 on a finding and 0 on a clean tree', () => {
  const root = makeRepo()
  write(root, 'services/web/src/copy.ts', fixture([`export const label = 'Loading${DOTS}'`]))

  const red = spawnSync(process.execPath, [GATE, '--root', root, '--no-exemptions'], {
    encoding: 'utf8',
  })
  assert.equal(red.status, 1)
  assert.match(red.stderr, /services\/web\/src\/copy\.ts:1/)

  write(root, 'services/web/src/copy.ts', fixture(["export const label = 'Loading…'"]))
  const green = spawnSync(process.execPath, [GATE, '--root', root, '--no-exemptions'], {
    encoding: 'utf8',
  })
  assert.equal(green.status, 0)
  assert.match(green.stdout, /Ellipsis consistency OK/)
})

test('a --root with a missing value or a following flag is a usage error, not a scan of nothing', () => {
  for (const argv of [['--root'], ['--root', '--no-exemptions']]) {
    const result = spawnSync(process.execPath, [GATE].concat(argv), { encoding: 'utf8' })
    assert.equal(result.status, 2, argv.join(' '))
    assert.match(result.stderr, /--root requires a directory argument/)
  }
})

// ---------------------------------------------------------------------------
// The live tree
// ---------------------------------------------------------------------------

test('every declared EXEMPTIONS entry carries a reason', () => {
  for (const entry of EXEMPTIONS) {
    assert.equal(typeof entry.path, 'string')
    assert.ok(entry.text === null || typeof entry.text === 'string')
    assert.equal(typeof entry.reason, 'string')
    assert.notEqual(entry.reason.trim(), '')
  }
})

test('the repository tree is clean under this gate', () => {
  const { result, out } = captureConsole(() => checkEllipsisConsistency())

  assert.equal(result, true)
  assert.match(out.join('\n'), /Ellipsis consistency OK \(\d+ file\(s\) scanned/)
})
