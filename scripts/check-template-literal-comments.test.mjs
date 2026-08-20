import assert from 'node:assert/strict'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import {
  checkTemplateLiteralComments,
  scanTemplateLiteralComments,
} from './check-template-literal-comments.mjs'

// Fixtures are built from single-quoted lines rather than written as template literals, for the
// obvious reason: a fixture for "a backtick inside a template literal" written INSIDE a template
// literal is the defect this file tests for. Single quotes keep every backtick inert, and this test
// file is itself in the gate's file set.
const fixture = (...lines) => lines.join('\n') + '\n'

test('reports a backtick in a line comment inside a template literal', () => {
  const { findings, desync } = scanTemplateLiteralComments(
    fixture('const script = `', '  // renders the `-` fallback', '  doThing();', '`'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [{ line: 2, kind: 'template-text-line-comment' }])
})

test('reports a backtick in a block comment inside a template literal', () => {
  const { findings, desync } = scanTemplateLiteralComments(
    fixture('const script = `', '  /* renders the `-` fallback */', '  doThing();', '`'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [{ line: 2, kind: 'template-text-block-comment' }])
})

test('ignores a backtick-free comment inside a template literal', () => {
  const { findings, desync } = scanTemplateLiteralComments(
    fixture('const script = `', '  // renders the dash fallback', '  doThing();', '`'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [])
})

test('ignores a backtick in a comment outside any template literal', () => {
  const { findings, desync } = scanTemplateLiteralComments(
    fixture('// renders the `-` fallback', '/* and a `-` block one */', 'const value = 1'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [])
})

test('ignores an escaped backtick inside a template literal', () => {
  const { findings, desync } = scanTemplateLiteralComments(
    fixture('const script = `', '  // renders the \\`-\\` fallback', '  doThing();', '`'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [])
})

test('reports a backtick in a comment inside a ${...} expression', () => {
  // Measured: this shape is VALID JavaScript — it parses and runs. It is reported as hygiene, per
  // the header of check-template-literal-comments.mjs.
  const { findings, desync } = scanTemplateLiteralComments(
    fixture('const script = `a ${ /* ` */ 1 } b`'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [{ line: 1, kind: 'interpolation-block-comment' }])
})

test('reports a backtick in a line comment inside a ${...} expression', () => {
  const { findings, desync } = scanTemplateLiteralComments(
    fixture('const script = `a ${', '  // a ` backtick', '  1', '} b`'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [{ line: 2, kind: 'interpolation-line-comment' }])
})

test('ignores comment-shaped text that does not begin its line inside a literal', () => {
  // Both of these are correct files. A URL carries `//`, and a one-line literal's closing backtick
  // arrives on the same line as its content — reporting either would be the false positive this
  // gate may never produce.
  const url = scanTemplateLiteralComments(
    fixture('const script = `', '  fetch("https://example.com/x");', '`'),
  )
  const inline = scanTemplateLiteralComments(fixture('const s = `prefix // suffix`'))

  assert.equal(url.desync, null)
  assert.deepEqual(url.findings, [])
  assert.equal(inline.desync, null)
  assert.deepEqual(inline.findings, [])
})

test('handles a template literal nested inside a ${...} expression', () => {
  const clean = scanTemplateLiteralComments(fixture('const outer = `a ${ inner(`b`) } c`'))
  const nested = scanTemplateLiteralComments(
    fixture('const outer = `a ${ inner(`', '  // has a `-` here', '  ok', '`) } c`'),
  )

  assert.equal(clean.desync, null)
  assert.deepEqual(clean.findings, [])
  assert.equal(nested.desync, null)
  assert.deepEqual(nested.findings, [{ line: 2, kind: 'template-text-line-comment' }])
})

test('discards findings when the lexer desyncs', () => {
  // One-directional by construction: a lexer that lost the thread reports nothing rather than risk
  // a false positive. `node --check` in lint-scripts.mjs is what covers a genuinely broken file.
  const { findings, desync } = scanTemplateLiteralComments(
    fixture('const script = `', '  // renders the `-` fallback'),
  )

  assert.equal(desync, 'unterminated template literal or interpolation')
  assert.deepEqual(findings, [])
})

test('desyncs on an unterminated string literal or block comment, discarding findings', () => {
  // The other two ways `desync` is set. These discard paths ARE the one-directional guarantee — they
  // are what throws away findings from a file the lexer misread — so a regression that dropped the
  // `closed` check in either scanner would let a misread file emit findings, the single forbidden
  // false-positive outcome, with the rest of the suite still green. Only the template-stack reason
  // was pinned before; these two were reachable and untested.
  const unterminatedString = scanTemplateLiteralComments(
    fixture("const a = 'oops", 'const s = `x`'),
  )
  const unterminatedBlock = scanTemplateLiteralComments(fixture('/* never closed', 'const s = `x`'))

  assert.equal(unterminatedString.desync, 'unterminated string literal')
  assert.deepEqual(unterminatedString.findings, [])
  assert.equal(unterminatedBlock.desync, 'unterminated block comment')
  assert.deepEqual(unterminatedBlock.findings, [])
})

test('an escaped newline inside a would-be regex does not drift the line counter', () => {
  // A regex may not contain a raw line terminator, so `/a\<newline>b/` is division, not a regex. The
  // escape-skip used to step over that newline without counting it, understating `line` for the rest
  // of the file — the finding below was reported at line 3 instead of 4, silently, because the
  // end-of-scan consistency check inspects mode and stack and never the line counter. Both files
  // differ only in that escape, so the two reported lines must agree with the physical ones.
  const withEscape = scanTemplateLiteralComments(
    fixture('const r = /a\\', 'b/', 'const s = `', '  // note` + `', 'tail`'),
  )
  const withoutEscape = scanTemplateLiteralComments(
    fixture('const r = /ab/', '', 'const s = `', '  // note` + `', 'tail`'),
  )

  assert.deepEqual(withEscape.findings, [{ line: 4, kind: 'template-text-line-comment' }])
  assert.deepEqual(withoutEscape.findings, [{ line: 4, kind: 'template-text-line-comment' }])
})

test('a CRLF file reports what its byte-identical LF twin reports', () => {
  // The line-continuation branch matched only `\n`, so under CRLF the escaped character was `\r`,
  // the branch did not fire, and the pair fell through to the main newline reset — which clears
  // `embedded`, exactly what that branch exists to prevent. The measured symptom was a lost
  // finding: this fixture reported nothing as CRLF and a line-3 finding as LF. Line NUMBERS are
  // asserted equal too, since a mis-split line ending would show up there first.
  const lf = fixture('const s = `', '  // note \\', '  more` + `', 'tail`')
  const crlf = lf.replace(/\n/g, '\r\n')

  const lfResult = scanTemplateLiteralComments(lf)
  const crlfResult = scanTemplateLiteralComments(crlf)

  assert.deepEqual(lfResult.findings, [{ line: 3, kind: 'template-text-line-comment' }])
  assert.deepEqual(crlfResult.findings, lfResult.findings)
  assert.equal(crlfResult.desync, lfResult.desync)
})

test('does not report a literal whose terminator lands on an apparent comment line', () => {
  // The one-directional constraint, tested from the side that matters: these three are CORRECT
  // files. A closing backtick at the end of a `//` line is the literal's terminator, not a
  // citation, and reporting it would red a file that is fine. See false-negative (e).
  const trailing = scanTemplateLiteralComments(
    fixture('const script = `', '  doThing();', '  // done`'),
  )
  const wrapped = scanTemplateLiteralComments(fixture('run(`', '  // done`)'))
  const url = scanTemplateLiteralComments(fixture('const script = `', '//cdn.example.com/a.js`'))

  for (const result of [trailing, wrapped, url]) {
    assert.equal(result.desync, null)
    assert.deepEqual(result.findings, [])
  }
})

test('does not report a terminator after an unterminated apparent block comment', () => {
  // Same class as above through the block-comment branch: `/* hey` inside template TEXT never
  // closes, so every later line still looks like comment interior — including the line carrying
  // the literal's own terminator.
  const { findings, desync } = scanTemplateLiteralComments(
    fixture('const script = `', '/* hey', '`'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [])
})

test('does not report a terminator followed by ordinary code on the same line', () => {
  // Past the terminator the grammar is back in CODE, where a backtick is entirely legitimate. A
  // rule that judged the line by counting backticks reddened every one of these — all correct,
  // all realistic. The first is the shape that matters most here: an embedded init script whose
  // last line is a comment, passed alongside a second template argument.
  const twoTemplateArgs = scanTemplateLiteralComments(
    fixture(
      'await page.addInitScript(`',
      '  window.__bossReady = true',
      '  // the harness polls this flag`, `#root`)',
    ),
  )
  const concatLiteral = scanTemplateLiteralComments(
    fixture('const a = `', '  doThing()', '  // done` + `tail`'),
  )
  const secondBinding = scanTemplateLiteralComments(fixture('const a = `', '  // done`, b = `z`'))
  const taggedTemplate = scanTemplateLiteralComments(fixture('const a = `', '  // done` + tag`hi`'))
  const tickInsideString = scanTemplateLiteralComments(fixture('const a = `', "  // done` + 'x`y'"))

  for (const result of [
    twoTemplateArgs,
    concatLiteral,
    secondBinding,
    taggedTemplate,
    tickInsideString,
  ]) {
    assert.equal(result.desync, null)
    assert.deepEqual(result.findings, [])
  }
})

test('does not report an inner literal whose terminator line also closes its encloser', () => {
  const { findings, desync } = scanTemplateLiteralComments(
    fixture('const outer = `a ${ inner(`', '  // ok`) } c`'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [])
})

test('does not report a terminator whose line ends inside a multi-line construct', () => {
  // The candidate must be settled at the end of ITS OWN physical line, which means every line
  // boundary counts — including the ones a code-mode block comment or a backslash-continued string
  // crosses without passing through the main newline branch. Both of these are correct files;
  // settling the line-2 candidate at the end of line 3 instead would let the template opened there
  // confirm it and red them.
  const acrossBlockComment = scanTemplateLiteralComments(
    fixture('const a = `', '  // done` /* hey', '  */ + `tail', '`'),
  )
  const acrossContinuedString = scanTemplateLiteralComments(
    fixture('const a = `', "  // done` + 'one \\", "  two' + `tail", '`'),
  )

  for (const result of [acrossBlockComment, acrossContinuedString]) {
    assert.equal(result.desync, null)
    assert.deepEqual(result.findings, [])
  }
})

test('still reports a citation whose literal spills past the line', () => {
  // The other side of the same rule: what makes the recorded defect a defect is that its second
  // backtick opens a literal that outlives the line. Every kind must keep firing.
  const lineComment = scanTemplateLiteralComments(
    fixture('const s = `', '  // renders the `-` fallback', '  more', '`'),
  )
  const blockComment = scanTemplateLiteralComments(
    fixture('const s = `', '  /* renders the `-` fallback */', '  more', '`'),
  )
  const nestedInner = scanTemplateLiteralComments(
    fixture('const o = `a${ inner(`', '  // has a `-` here', '  ok', '`) } c`'),
  )

  assert.deepEqual(lineComment.findings, [{ line: 2, kind: 'template-text-line-comment' }])
  assert.deepEqual(blockComment.findings, [{ line: 2, kind: 'template-text-block-comment' }])
  assert.deepEqual(nestedInner.findings, [{ line: 2, kind: 'template-text-line-comment' }])
})

test('the documented exception: a terminator that opens a multi-line literal reports', () => {
  // Pinned deliberately, and it is the one place this gate reds a CORRECT file. "The line ends with
  // a template still open" is the defect's only signature, and it is also the signature of a
  // terminator followed on the same line by a literal that does not close there. See the ONE
  // EXCEPTION paragraph in the module header for why reporting is the right side to be wrong on.
  const concatenated = scanTemplateLiteralComments(
    fixture('const script = `', '  doThing()', '  // done` + `', '  more()', '`'),
  )
  const arrayElement = scanTemplateLiteralComments(
    fixture('const a = [`', '  // done`, `', '  // done2`]'),
  )

  assert.deepEqual(concatenated.findings, [{ line: 3, kind: 'template-text-line-comment' }])
  assert.deepEqual(arrayElement.findings, [{ line: 2, kind: 'template-text-line-comment' }])

  // The mitigation, which is what keeps the exception out of any file whose author ran
  // `make format`: prettier does not leave either shape as written — it moves the trailing
  // backtick onto its own line. Both fixtures below are the verbatim output of THIS REPO'S
  // prettier config (`.prettierrc` sets `semi: false`, hence no trailing semicolons — a
  // default-config prettier would add them), and both are silent.
  const formattedConcat = scanTemplateLiteralComments(
    fixture('const script =', '  `', '  doThing()', '  // done` +', '  `', '  more()', '`'),
  )
  const formattedArray = scanTemplateLiteralComments(
    fixture('const a = [', '  `', '  // done`,', '  `', '  // done2`,', ']'),
  )

  for (const result of [formattedConcat, formattedArray]) {
    assert.equal(result.desync, null)
    assert.deepEqual(result.findings, [])
  }
})

test('an embedded line comment survives a construct that crosses a physical line', () => {
  // Pins the asymmetry documented on the main newline branch: `lineHasContent` tracks PHYSICAL
  // lines, `embedded` tracks the EMBEDDED script's lines, and a backslash continuation or a
  // multi-line `${...}` crosses one without producing the other. Both files below are valid
  // JavaScript; the comment genuinely does continue, so the backtick that ends it is genuinely the
  // one that closes the literal, and the line still ends with a template open — i.e. this lands in
  // the disclosed ONE EXCEPTION class and no wider. Recorded so the next editor sees it as a choice.
  const continued = scanTemplateLiteralComments(
    fixture('const s = `', '  // note \\', '  more` + `', 'tail`'),
  )
  const notContinued = scanTemplateLiteralComments(
    fixture('const s = `', '  // note', '  more` + `', 'tail`'),
  )

  assert.deepEqual(continued.findings, [{ line: 3, kind: 'template-text-line-comment' }])
  assert.deepEqual(notContinued.findings, [])
})

test('escaping clears a template-text finding but not one inside ${...}', () => {
  // The two kinds have different remedies, which is why main() prints advice per kind rather than
  // one line for both. Template text is lexed by this scanner, which honours the escape. A host
  // comment inside an interpolation has no escape syntax at all, so a backslash there is inert and
  // the finding stands — an author who followed a single "escape it" instruction would re-run into
  // an identical red. Measured, not assumed.
  const escapedInText = scanTemplateLiteralComments(
    fixture('const s = `', '  // renders the \\`-\\` fallback', '  more', '`'),
  )
  const escapedInInterpolation = scanTemplateLiteralComments(
    fixture('const s = `a ${ /* \\` */ 1 } b`'),
  )

  assert.deepEqual(escapedInText.findings, [])
  assert.deepEqual(escapedInInterpolation.findings, [
    { line: 1, kind: 'interpolation-block-comment' },
  ])
})

test('an empty file set is a failure, not a green run', () => {
  // The tripwire that stops the gate degenerating into "looked at nothing, printed green". If a
  // change to resolveScriptFiles ever returns nothing, this must red rather than pass.
  const result = checkTemplateLiteralComments(
    path.join(os.tmpdir(), 'bos787-no-such-root-' + process.pid),
  )

  assert.equal(result.ok, false)
  assert.equal(result.scanned, 0)
  assert.ok(
    result.misses.some((miss) => miss.includes('no files')),
    `expected an explanatory miss, got ${JSON.stringify(result.misses)}`,
  )
})

test('the repo script tree is clean and non-empty', () => {
  const result = checkTemplateLiteralComments()

  assert.deepEqual(result.misses, [])
  assert.deepEqual(
    result.skipped,
    [],
    'A skip means this lexer lost the thread on a file that is almost certainly VALID (see ' +
      'false-negative (a)/(b) in the module header), so that file is now unchecked by the gate. ' +
      'This assertion is the only fail-closed consumer of a skip — the gate itself keeps ' +
      '`ok = misses.length === 0` on purpose, because failing a run on a desync would red a ' +
      'correct file. The remedy is to teach the lexer that shape (or to accept it deliberately ' +
      'and narrow this assertion), NOT to edit the file that tripped it.',
  )
  assert.equal(result.ok, true)
  assert.ok(
    result.scanned > 0,
    'resolveScriptFiles() must resolve files for this gate to mean anything',
  )
})
