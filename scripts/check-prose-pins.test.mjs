#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import {
  GATE_FILE_GLOB,
  POLICY_DOC,
  PROSE_PIN_BASELINE,
  SPAWNER_CALLEES,
  checkProsePins,
  discoverGateFiles,
  findMarkdownBoundNames,
  findProsePins,
  measureProsePins,
} from './check-prose-pins.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

const fixture = (...lines) => lines.join('\n') + '\n'

// Capture what the gate prints, so a test can assert on the text a contributor actually sees
// rather than only on the boolean. A failure message that does not name the site is a failure
// message that sends the reader looking.
function captureConsole(run) {
  const out = []
  const err = []
  const originalLog = console.log
  const originalError = console.error
  console.log = (...args) => out.push(args.join(' '))
  console.error = (...args) => err.push(args.join(' '))
  try {
    return { result: run(), out, err }
  } finally {
    console.log = originalLog
    console.error = originalError
  }
}

function makeTempRepo() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'check-prose-pins-'))
  fs.mkdirSync(path.join(root, 'scripts'))
  return root
}

// A file name that GATE_FILE_GLOB matches, so a temp-repo fixture is really in scope.
const IN_SCOPE = 'demo-skill.test.mjs'

function writeScript(repoRoot, name, contents) {
  fs.writeFileSync(path.join(repoRoot, 'scripts', name), contents)
}

// The shape a skill test uses to get a document: read a `.md` off disk, then assert over a slice.
const READS_MARKDOWN = "const skill = fs.readFileSync(path.join(root, 'SKILL.md'), 'utf8')"

// ---------------------------------------------------------------------------
// The rule: what is markdown-bound
// ---------------------------------------------------------------------------

test('a binding seeded by a .md path literal is markdown-bound', () => {
  const bound = findMarkdownBoundNames(fixture(READS_MARKDOWN))

  assert.ok(bound.has('skill'))
})

test('the closure reaches a region sliced out of a markdown binding', () => {
  const bound = findMarkdownBoundNames(
    fixture(
      READS_MARKDOWN,
      "const step5 = region(skill, '## Step 5', '## Step 6')",
      "const flat = skill.replace(/\\s+/g, ' ')",
      'const deeper = sectionRegion(step5, heading)',
    ),
  )

  // `region(skill, …)` is the commonest subject shape in the scanned population; a rule keyed on
  // the initialiser's LEADING identifier would bind `skill` alone and miss all three.
  assert.deepEqual([...bound].sort(), ['deeper', 'flat', 'skill', 'step5'])
})

test('a helper defined in the file is markdown-bound through the constant it reads', () => {
  const bound = findMarkdownBoundNames(
    fixture(
      "const FINALIZE_REF = 'references/finalize-and-stop.md'",
      'const finalizeAndStop = (dir) => fs.readFileSync(path.join(root, dir, FINALIZE_REF), "utf8")',
    ),
  )

  assert.ok(bound.has('finalizeAndStop'))
})

// BOS-1210 review: the closure was blind to the two commonest DECLARATION shapes in the scanned
// corpus, so a pin written either way was uncounted and unrefused. Both regressions are pinned as
// PROPERTIES rather than as numbers — reformatting must not change the count, and the
// source/codex-mirror loop must produce one.

test('wrapping a binding across a line break does not change what it binds', () => {
  const oneLine = fixture(
    "const FINALIZE_REF = 'references/finalize-and-stop.md'",
    'const finalizeAndStop = (dir = CORE) => fs.readFileSync(path.join(root, dir, FINALIZE_REF), "utf8")',
    'assert.match(finalizeAndStop(), /proof\\.mjs plan/)',
  )
  // Byte-for-byte the same binding, wrapped the way prettier wraps it at this repo's print width.
  const wrapped = fixture(
    "const FINALIZE_REF = 'references/finalize-and-stop.md'",
    'const finalizeAndStop = (dir = CORE) =>',
    '  fs.readFileSync(path.join(root, dir, FINALIZE_REF), "utf8")',
    'assert.match(finalizeAndStop(), /proof\\.mjs plan/)',
  )

  assert.ok(findMarkdownBoundNames(oneLine).has('finalizeAndStop'))
  assert.ok(findMarkdownBoundNames(wrapped).has('finalizeAndStop'))
  assert.equal(findProsePins(wrapped).length, findProsePins(oneLine).length)
})

test('an initialiser that begins on the next line is not read as empty', () => {
  const bound = findMarkdownBoundNames(
    fixture('const body =', "  fs.readFileSync(path.join(root, 'SKILL.md'), 'utf8')"),
  )

  assert.ok(bound.has('body'))
})

test('a destructured for-of subject is markdown-bound and its assertions count', () => {
  // The source/codex-mirror idiom. A bare-identifier declaration regex bound NOTHING here, so a
  // file written this way reported zero pins however many regexes it ran over the markdown.
  const source = fixture(
    "const SKILL = read('../.claude/skills/demo/SKILL.md')",
    "const CODEX = read('../.codex/skills/demo/SKILL.md')",
    "for (const [label, skill] of [['source', SKILL], ['codex mirror', CODEX]]) {",
    '  assert.match(skill, /must dispatch the reviewer/)',
    '}',
  )

  assert.ok(findMarkdownBoundNames(source).has('skill'))
  assert.deepEqual(findProsePins(source), [{ line: 4, subject: 'skill' }])
})

test('an object pattern binds its targets, not its property names', () => {
  const bound = findMarkdownBoundNames(
    fixture(READS_MARKDOWN, 'const { body: text, missing = fallback } = parse(skill)'),
  )

  assert.ok(bound.has('text'))
  // `body` is the property NAME being read, and `fallback` is a default VALUE; neither is bound.
  assert.ok(!bound.has('body'))
  assert.ok(!bound.has('fallback'))
})

test('a multi-line template initialiser does not swallow the statement after it', () => {
  // The continuation rule reads significance from the SOURCE, so a blanked template body ends the
  // run. Reading it as whitespace would walk back to the `=` and keep going forever.
  const bound = findMarkdownBoundNames(
    fixture(
      'const banner = `first line',
      'second line`',
      "const helper = fs.readFileSync(path.join(root, 'SKILL.md'), 'utf8')",
    ),
  )

  assert.ok(bound.has('helper'))
  assert.ok(!bound.has('banner'))
})

test('a binding read from a non-markdown artifact is not markdown-bound', () => {
  const bound = findMarkdownBoundNames(
    fixture(
      "const helper = fs.readFileSync(path.join(root, 'toolbox/run.mjs'), 'utf8')",
      "const source = fs.readFileSync(path.join(root, 'main.go'), 'utf8')",
    ),
  )

  assert.deepEqual([...bound], [])
})

for (const callee of SPAWNER_CALLEES) {
  test(`a binding whose initialiser calls ${callee} is not markdown-bound`, () => {
    const bound = findMarkdownBoundNames(
      fixture(READS_MARKDOWN, `const out = ${callee}('node', [gate, skill], { encoding: 'utf8' })`),
    )

    assert.ok(bound.has('skill'))
    assert.ok(!bound.has('out'))
  })
}

test('the executable exclusion propagates through a helper that spawns', () => {
  const bound = findMarkdownBoundNames(
    fixture(
      READS_MARKDOWN,
      "const block = region(skill, '```sh', '```')",
      'function runBlock(input) { return spawnSync("bash", ["-c", input]) }',
      'const relayed = runBlock(block)',
    ),
  )

  // `relayed` mentions `block`, which IS markdown — but it reads a child process's result, and
  // punishing that shape would teach the opposite of the rule.
  assert.ok(bound.has('block'))
  assert.ok(!bound.has('relayed'))
})

// ---------------------------------------------------------------------------
// The rule: what counts as a prose pin
// ---------------------------------------------------------------------------

test('flags an assertion whose subject is markdown, at the right line', () => {
  const pins = findProsePins(
    fixture(
      READS_MARKDOWN,
      "const step5 = region(skill, '## Step 5', '## Step 6')",
      'test("x", () => {',
      '  assert.match(step5, /must dispatch the reviewer/)',
      '})',
    ),
  )

  assert.deepEqual(pins, [{ line: 4, subject: 'step5' }])
})

test('flags a negative assertion over markdown too', () => {
  const pins = findProsePins(
    fixture(READS_MARKDOWN, 'assert.doesNotMatch(skill, /never ask the user/)'),
  )

  assert.deepEqual(pins, [{ line: 2, subject: 'skill' }])
})

test('does not flag an assertion over a spawned helper output', () => {
  const pins = findProsePins(
    fixture(
      READS_MARKDOWN,
      "const out = execFileSync('node', [gate, skill], { encoding: 'utf8' })",
      "const run = spawnSync('node', [gate], { input: skill, encoding: 'utf8' })",
      'assert.match(out, /VERDICT: ok/)',
      'assert.match(run.stderr, /absent/)',
    ),
  )

  assert.deepEqual(pins, [])
})

test('does not flag an assertion over a non-markdown artifact', () => {
  const pins = findProsePins(
    fixture(
      READS_MARKDOWN,
      "const helper = fs.readFileSync(path.join(root, 'toolbox/run.mjs'), 'utf8')",
      'assert.match(helper, /export function classify/)',
    ),
  )

  assert.deepEqual(pins, [])
})

test('does not flag a non-match assertion over markdown', () => {
  // `assert.equal` / `assert.ok` are outside this rule by construction: the gate refuses the
  // REGEX-over-markdown shape, which is the one that pins a clause rather than a value.
  const pins = findProsePins(
    fixture(
      READS_MARKDOWN,
      'assert.equal(skill.length > 0, true)',
      'assert.ok(skill.includes(fm))',
    ),
  )

  assert.deepEqual(pins, [])
})

test('an assertion inside a comment or a string is not a pin', () => {
  const pins = findProsePins(
    fixture(
      READS_MARKDOWN,
      '// assert.match(skill, /commented out/)',
      'const advice = "assert.match(skill, /quoted/)"',
    ),
  )

  assert.deepEqual(pins, [])
})

test('a regex literal containing an unbalanced bracket does not desync the scan', () => {
  const pins = findProsePins(
    fixture(
      READS_MARKDOWN,
      'assert.match(skill, /\\)/)',
      'assert.match(skill, /the second pin still counts/)',
    ),
  )

  assert.deepEqual(
    pins.map((pin) => pin.line),
    [2, 3],
  )
})

test('a regex opened after a keyword is masked, not read as division', () => {
  // BOS-1210 review: scripts/check-prose-pin-whitespace.mjs runs the same lexer over the same file
  // set and knows the keywords after which `/` opens a regex; this gate did not. Read as division,
  // the regex body is never masked, so its unbalanced bracket runs the initialiser walk on into the
  // NEXT binding and borrows that binding's `.md` seed — binding a name that touches no markdown.
  // Falsified against the pre-fix lexer, which reports a SECOND, false pin on `noise` at line 3.
  const source = fixture(
    'const noise = (t) => { return /\\(/.test(t) }',
    READS_MARKDOWN,
    'assert.match(noise, /not a document, so not a pin/)',
    'assert.match(skill, /a real pin/)',
  )

  assert.deepEqual(findProsePins(source), [{ line: 4, subject: 'skill' }])
})

// ---------------------------------------------------------------------------
// The ratchet: NEW pins, not any pins
// ---------------------------------------------------------------------------

test('a file at its banked baseline passes', () => {
  const root = makeTempRepo()
  writeScript(root, IN_SCOPE, fixture(READS_MARKDOWN, 'assert.match(skill, /pinned/)'))

  const { result } = captureConsole(() => checkProsePins(root, { [`scripts/${IN_SCOPE}`]: 1 }))

  assert.equal(result, true)
})

test('a newly added pin trips the gate and is named by file and line', () => {
  const root = makeTempRepo()
  writeScript(
    root,
    IN_SCOPE,
    fixture(
      READS_MARKDOWN,
      'assert.match(skill, /the pin that was already banked/)',
      'assert.match(skill, /the pin this commit adds/)',
    ),
  )

  const { result, err } = captureConsole(() => checkProsePins(root, { [`scripts/${IN_SCOPE}`]: 1 }))

  assert.equal(result, false)
  // Line 3 is the added one, and the gate must say so precisely enough to open the file at it.
  assert.ok(err.some((line) => line.includes(`scripts/${IN_SCOPE}:3`)))
  assert.ok(err.some((line) => line.includes('2 prose pin(s), baseline 1')))
  assert.ok(err.some((line) => line.includes(POLICY_DOC)))
})

test('a brand-new skill test file may add no pins at all', () => {
  const root = makeTempRepo()
  writeScript(root, IN_SCOPE, fixture(READS_MARKDOWN, 'assert.match(skill, /a first pin/)'))

  const { result, err } = captureConsole(() => checkProsePins(root, {}))

  assert.equal(result, false)
  assert.ok(err.some((line) => line.includes('1 prose pin(s), baseline 0')))
})

test('a deletion reds until the saving is banked', () => {
  const root = makeTempRepo()
  writeScript(root, IN_SCOPE, fixture(READS_MARKDOWN, 'assert.match(skill, /the surviving pin/)'))

  const { result, err } = captureConsole(() => checkProsePins(root, { [`scripts/${IN_SCOPE}`]: 3 }))

  assert.equal(result, false)
  assert.ok(err.some((line) => line.includes('lower PROSE_PIN_BASELINE to 1')))
})

test('a baseline entry for a file that no longer exists reds', () => {
  const root = makeTempRepo()
  writeScript(root, IN_SCOPE, fixture(READS_MARKDOWN))

  const { result, err } = captureConsole(() =>
    checkProsePins(root, { 'scripts/deleted-skill.test.mjs': 4 }),
  )

  assert.equal(result, false)
  assert.ok(err.some((line) => line.includes('scripts/deleted-skill.test.mjs')))
})

test('an executable assertion added to a file at baseline does not trip the gate', () => {
  // The counter-form of the falsification above, and the reason the gate is worth having: the
  // shape a pin should be converted INTO must be free to land.
  const root = makeTempRepo()
  writeScript(
    root,
    IN_SCOPE,
    fixture(
      READS_MARKDOWN,
      'assert.match(skill, /the pin that was already banked/)',
      "const out = execFileSync('node', [gate, skill], { encoding: 'utf8' })",
      'assert.match(out, /VERDICT: ok/)',
    ),
  )

  const { result } = captureConsole(() => checkProsePins(root, { [`scripts/${IN_SCOPE}`]: 1 }))

  assert.equal(result, true)
})

test('an empty file set fails rather than passing over nothing', () => {
  const root = makeTempRepo()

  const { result, err } = captureConsole(() => checkProsePins(root, {}))

  assert.equal(result, false)
  assert.ok(err.some((line) => line.includes(GATE_FILE_GLOB)))
})

// ---------------------------------------------------------------------------
// Scope and the real tree
// ---------------------------------------------------------------------------

test('the file set is glob-derived, not a hand-list', () => {
  const files = discoverGateFiles(REPO_ROOT).map((file) =>
    path.relative(REPO_ROOT, file).split(path.sep).join('/'),
  )

  assert.ok(files.length > 0)
  assert.ok(files.every((file) => file.startsWith('scripts/') && file.endsWith('.test.mjs')))
  // Every banked file must still be in scope, so the baseline cannot describe a tree the gate
  // stopped reading.
  for (const banked of Object.keys(PROSE_PIN_BASELINE)) assert.ok(files.includes(banked))
})

test('the real tree sits exactly at its banked baseline', () => {
  const { growth, shrink, stale } = measureProsePins(REPO_ROOT)

  assert.deepEqual({ growth, shrink, stale }, { growth: [], shrink: [], stale: [] })
})

test('the policy the failure message points at exists', () => {
  assert.ok(fs.existsSync(path.join(REPO_ROOT, POLICY_DOC)))
})
