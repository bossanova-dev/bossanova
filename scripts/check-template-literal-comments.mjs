#!/usr/bin/env node

// check-template-literal-comments — a backtick inside a comment that lives inside a JS template
// literal is a syntax-level trap that `node --check` cannot see, because the damaged file still
// parses. This gate sees it.
//
// Why this exists (BOS-787). A comment added INSIDE the `webStageScript` template literal in
// scripts/proof-playwright-runner.mjs read "renders the `-` fallback". Inside template TEXT there
// are no comments — that `//` is just characters — so the first backtick terminated the literal,
// `-` became a subtraction operator, and the next backtick opened a fresh literal that ran to the
// intended closing one. The file still parsed, so `scripts/lint-scripts.mjs` (a `node --check`
// sweep over exactly this file set) stayed green. Prettier then reformatted the wreckage into a
// string concatenation, so the diff read as a plausible reformat rather than as a broken fixture.
// A reviewer scanning that diff sees formatting, not damage. This gate is the only thing in the
// repo that can red on it.
//
// THE RULE, stated once: an unescaped backtick may not appear in a comment that is nested inside a
// template literal. Two positions count as "a comment nested inside a template literal":
//
//   (1) An apparent comment in template TEXT — a line inside the literal whose first non-whitespace
//       characters are `//` or `/*`. These are comments of the EMBEDDED script the literal carries,
//       not of the host file, and a backtick in one is the recorded defect: it ends the literal.
//   (2) A real host-language comment inside a `${…}` interpolation within a literal. Measured: that
//       one is VALID JavaScript — `` `a ${ /* ` */ 1 } b` `` parses and runs. It is still reported,
//       as hygiene rather than as a syntax hazard: the two positions are visually indistinguishable
//       at the point of edit, moving such a comment one level out (the common edit) silently
//       becomes case (1), and nothing in this repo needs a backtick there.
//
// ONE-DIRECTIONAL, WITH ONE STATED EXCEPTION — AND CASE (2) REPORTED ON PURPOSE. The gate must never
// red a correct file by ACCIDENT — that is the constraint the rule is built around, so every
// ambiguity below resolves toward reporting nothing, and where one ambiguity could not be resolved
// that way it is named rather than left to be found. Two things are still reported on a file that
// parses: case (2) above, which is valid JavaScript flagged deliberately as hygiene, and the one
// unavoidable exception below. Neither is an accident, and the count is two, not zero — a reader who
// takes "one-directional" as "never reds a correct file" will be surprised by the astro-shaped
// case (2) hit that turns up in real code. What the banner claims is a claim about behaviour, not a
// proof, and it is pinned as behaviour: the correct shapes that a naive version of
// this rule DID red are regression tests in check-template-literal-comments.test.mjs, alongside the
// whole-tree test that fails if the file set ever comes back empty or carries a skip.
//
// ONE EXCEPTION, AND IT IS THE PRICE OF THE RULE. Case (1) reports exactly one thing: a backtick
// whose LINE ends with a template literal still open. That is the defect's signature — the second
// backtick of `` `-` `` opens a literal that runs off the line and swallows what follows — and it is
// also, unavoidably, the signature of a CORRECT file whose terminator is followed on the same line
// by another literal that does not close there: `` // done` + ` `` continued below, or a second
// multi-line element in `` [`…// done`, ` ``. Those report. The two are one lexical fact, not two,
// so the only choice is which way to be wrong, and reporting wins because the shape it buys — a
// citation ENDING its comment line, `` // renders the `-` `` — is how authors actually write these
// comments, while the shape it costs needs a multi-line literal opened on the terminator's own line.
// That shape is also not prettier-stable: measured, prettier moves the second backtick onto its own
// line in every variant, and the formatted file is silent. But that mitigation is a CONVENTION, not
// a gate, and the difference is worth stating precisely because it is the load-bearing half of the
// argument. `make format` does route .mjs/.cjs through prettier (format-affected.mjs lists both
// extensions). The pre-commit hook does NOT: scripts/format-staged.sh dispatches on `*.js`, and
// `*.js` does not glob-match `foo.mjs` or `foo.cjs`, so the hook contributes nothing here. Nor does
// anything CHECK it — `lint:docs` is the only `prettier --check` in the tree and it is markdown
// only. So the honest claim is narrower than "out of reach in committed code": the class is out of
// reach in any file whose author ran `make format`, and reachable in one committed without it.
// It is pinned by a test so it stays a known boundary, and when it does bite the remedy
// is the one the defect wants anyway: escape the backtick (which works in template TEXT — see the
// kind-aware note in main(), because in a `${…}` comment it does not).
//
// The accepted false-NEGATIVES, all deliberate:
//
//   (a) A DESYNCED FILE IS DROPPED WHOLE. The scanner is a hand-rolled lexer, and the classic trap
//       is `/`: division and a regex literal are the same character, and a regex may itself contain
//       a backtick (`/a`b/`). Reading such a regex as division would open a phantom template
//       literal and every later comment would be reported — a false POSITIVE, the one outcome this
//       gate may not produce. So the lexer's self-consistency is checked instead: if a file does not
//       end in code state with an empty template stack, or carries an unterminated string or block
//       comment, the lexer is known to have lost the thread and the file's findings are discarded
//       and counted as skipped. `node --check` in lint-scripts.mjs covers a genuinely unterminated
//       construct, but NOT a valid file this lexer merely misread — case (b) below is exactly that,
//       and nothing else watches it. Which is why the skip COUNT is printed on every run, on the
//       success line as well as the failure one: "found nothing" and "looked at nothing" must not
//       print the same line. Be exact about what that buys, because the shorter wording has already
//       been misread by an outside reviewer: a skip is VISIBLE, not FATAL. `ok` is
//       `misses.length === 0` and deliberately does NOT also require `skipped.length === 0`, so a
//       desync does not red the gate — and it must not. A desync is this lexer losing the thread on
//       a file that is perfectly VALID (case (b) below is the shape), so failing the run on one
//       would red a correct file: the single outcome the hard constraint forbids, and the entire
//       reason the discard path exists at all. The tripwire is aimed at a READER of the output,
//       whose job it is to notice a non-zero skip count and go look; it is not an exit code, and
//       turning it into one would trade this gate's defining property for the appearance of rigour.
//
//   (b) `}` AND `)` BEFORE `/` ARE READ AS DIVISION. `if (x) /re/.test(y)` and a regex opening the
//       statement after a block both lex as division here. Neither shape exists in this file set —
//       which is checked rather than asserted: the whole-tree test requires `skipped` to be empty,
//       so the day one appears it either lexes harmlessly or desyncs into (a) and reds.
//
//   (c) AN APPARENT COMMENT MUST BEGIN ITS LINE (case (1) only). `//` or `/*` mid-line inside
//       template text is NOT treated as a comment. This is what keeps a correct one-liner such as
//       `` `https://example.com/x` `` or `` `prefix // suffix` `` from being reported when the
//       literal's own closing backtick arrives on that same line — the `//` there is content, and
//       the backtick is the literal's terminator, not a defect. The cost is that a backtick after a
//       mid-line `//` inside a literal is missed. The recorded defect, and every comment authors
//       actually write inside these embedded scripts, begins its line.
//
//   (d) NARROW SCOPE, ON PURPOSE. This is not a template-literal linter. It reports one shape. A
//       template literal broken by a backtick somewhere OTHER than a comment is out of scope; so is
//       any judgement about what the embedded script means.
//
//   (e) A DEFECT WHOSE LINE STILL BALANCES IS MISSED (case (1) only). In template TEXT the
//       literal's own closing backtick and a defect's backtick are the same character in the same
//       position: `` // done` `` (correct — the author closed the literal at the end of a comment
//       line) and `` // renders the `-` fallback `` (the recorded defect) are indistinguishable
//       where they stand. They differ in what the line leaves behind. The defect's SECOND backtick
//       opens a literal that runs off the end of the line and swallows what follows — the wreckage
//       prettier then reformats into plausible-looking code — while a terminator leaves the line
//       closed however much host code trails it. So case (1) reports only when the template stack
//       is deeper at end of line than it was when the backtick popped it, which is measured by the
//       lexer rather than guessed at, and which correctly stays silent on `` // done` + `tail` ``,
//       `` // done`, b = `z` ``, `` // done` + 'x`y' `` and on an inner literal whose terminator
//       line also closes its encloser. The cost: a defect whose line happens to balance anyway (an
//       even number of backticks after the terminator, e.g. two separate citations plus a stray)
//       goes unreported. The recorded defect is not that shape and neither is any citation an
//       author would write. Case (2) — a real comment inside `${…}` — is unaffected, because that
//       comment is properly delimited and every backtick in it is unambiguous.
//
// The file set is not re-derived here: it is `resolveScriptFiles` from scripts/lint-scripts.mjs, so
// this gate and the `node --check` sweep cannot drift into disagreeing about what "the repo's
// scripts" are. Unlike that sweep this gate is unstamped (it re-reads every file on every run, like
// check-skill-shell.mjs); it is pure string scanning over ~300 files (296 today) and costs roughly
// 150ms — about half of a ~350ms run, the rest being Node's own startup.
//
// Exercised by scripts/check-template-literal-comments.test.mjs and runnable via
// `node scripts/check-template-literal-comments.mjs`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { resolveScriptFiles } from './lint-scripts.mjs'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

// The repo root relative to this file, so the gate works from any cwd — it runs from the repo root
// via `node scripts/check-template-literal-comments.mjs` and from `scripts/` via the scripts
// Makefile `lint` target.
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// Words after which a `/` opens a regex rather than dividing. Everything else that can end an
// expression (identifier, number, string, `)`, `]`, `}`) makes it division — see false-negative (b).
const REGEX_PRECEDING_KEYWORDS = new Set([
  'await',
  'case',
  'delete',
  'do',
  'else',
  'in',
  'instanceof',
  'new',
  'of',
  'return',
  'throw',
  'typeof',
  'void',
  'yield',
])

const IDENTIFIER_START = /[A-Za-z_$]/
const IDENTIFIER_PART = /[A-Za-z0-9_$]/
const DIGIT = /[0-9]/
const NUMBER_PART = /[0-9A-Za-z_.]/
const WHITESPACE = /\s/
const REGEX_FLAG = /[a-z]/i

// Scan a regex literal starting at `start` (the opening `/`). Returns the index just past the
// closing `/` and its flags, or -1 when the candidate is not a regex after all. A regex may not
// span a newline, so hitting one means the `/` was division — returning -1 lets the caller rewind
// rather than swallow the rest of the file.
function scanRegexLiteral(source, start) {
  let index = start + 1
  let inCharacterClass = false
  while (index < source.length) {
    const char = source[index]
    if (char === '\n') return -1
    if (char === '\\') {
      // A regex literal may not contain a raw line terminator, so an escaped newline does not
      // continue one — it means this `/` was division after all, and returning -1 rewinds the
      // caller to that reading. Stepping over the newline instead would leave `line` understated
      // for the whole REST of the file, so every later finding would cite the wrong line in a gate
      // whose entire output is `file:line` — silently, since the end-of-scan self-consistency check
      // looks at mode and stack, not at the line counter.
      if (source[index + 1] === '\n' || source[index + 1] === '\r') return -1
      index += 2
      continue
    }
    if (inCharacterClass) {
      if (char === ']') inCharacterClass = false
      index += 1
      continue
    }
    if (char === '[') {
      inCharacterClass = true
      index += 1
      continue
    }
    if (char === '/') {
      index += 1
      while (index < source.length && REGEX_FLAG.test(source[index])) index += 1
      return index
    }
    index += 1
  }
  return -1
}

// Scan one JavaScript source string for backticks in comments nested inside template literals.
//
// Returns `{ findings, desync }`. Each finding is `{ line, kind }` with a 1-based line number;
// `kind` names the position so a reader can tell the recorded defect (`template-text-*`) from the
// hygiene rule (`interpolation-*`). `desync` is null on a clean lex, or a short reason string — and
// when it is set, `findings` is EMPTY: a lexer that lost the thread reports nothing rather than
// risk a false positive (false-negative (a) in the header).
export function scanTemplateLiteralComments(source) {
  const findings = []
  let desync = null

  let index = 0
  let line = 1
  // Whether any non-whitespace character has been seen on the current physical line. Shared across
  // code and template modes because they share physical lines; it is what implements the
  // "an apparent comment must begin its line" rule (false-negative (c)).
  let lineHasContent = false
  // Frames: { kind: 'template', embedded } for a literal's text, { kind: 'subst', braceDepth } for
  // a `${…}` expression. A 'subst' frame only ever sits above a 'template' frame, so in code mode a
  // non-empty stack means "inside a template literal".
  const stack = []
  let mode = 'code'
  // 'value' when the previous significant token can end an expression (so `/` divides); anything
  // else means `/` opens a regex.
  let previous = 'none'

  // Case (1) findings are held until the end of their physical line. A backtick on an
  // apparent-comment line inside template TEXT ends the literal either way — which is exactly what
  // a CORRECT file's own closing backtick does — so the character and its position cannot tell the
  // two apart. What can is what the line LEAVES BEHIND. The recorded defect opens a literal that
  // spills past the line and swallows what follows (the wreckage prettier then disguises as a
  // reformat), whereas a genuine terminator leaves the line closed no matter how much host code
  // trails it: `` // done` ``, `` // done`) ``, `` // done` + `tail` ``, `` // done`, b = `z` ``.
  // So the judgement is deferred to the lexer itself rather than guessed at: report only when the
  // template stack is DEEPER at end of line than it was immediately after this backtick popped it.
  // Invariant: resolvePending() runs at EVERY physical line boundary — including the ones crossed
  // inside a code-mode block comment or a backslash-continued string, which do not pass through the
  // main newline branch. Settling a candidate one line late would let a literal opened on THAT line
  // confirm it, which is the false positive this whole mechanism exists to avoid.
  let pending = []
  const resolvePending = () => {
    for (const candidate of pending) {
      if (stack.length > candidate.depth) {
        findings.push({ line: candidate.line, kind: candidate.kind })
      }
    }
    pending = []
  }

  if (source.startsWith('#!')) {
    const newline = source.indexOf('\n')
    if (newline === -1) return { findings, desync }
    index = newline + 1
    line = 2
  }

  while (index < source.length) {
    const char = source[index]

    if (char === '\n') {
      resolvePending()
      line += 1
      lineHasContent = false
      // Clearing `embedded` is deliberately narrower than clearing `lineHasContent`, and the
      // asymmetry is the point rather than an oversight. `line` and `lineHasContent` track PHYSICAL
      // lines of this file; `embedded` tracks a line comment in the EMBEDDED script, which only ends
      // where that script sees a newline. Two constructs cross a physical line without putting one
      // there: a backslash continuation in template text (the branch below, which resets
      // `lineHasContent` but leaves `embedded` set for exactly this reason) and a `${...}` spanning
      // host lines (which this reset skips, being guarded on `mode === 'template'` — inside a subst
      // the top frame is a `subst`, which has no `embedded` to clear anyway). In both, the embedded
      // comment genuinely continues, so `embedded` genuinely should survive. Consequence, measured:
      // a correct file that continues an embedded comment across a physical line and ends that line
      // with a template still open reports — i.e. it lands in the ONE EXCEPTION class the header
      // already discloses, and no wider. Do not "fix" this in either direction without re-reading
      // that paragraph: clearing `embedded` here would blind the gate to the multi-physical-line
      // form of the defect, and keeping `lineHasContent` set would break the `!lineHasContent`
      // comment-start test on the continued line.
      if (mode === 'template') {
        const frame = stack[stack.length - 1]
        if (frame.embedded === 'line') frame.embedded = null
      }
      index += 1
      continue
    }

    if (mode === 'template') {
      const frame = stack[stack.length - 1]

      if (char === '\\') {
        // An escaped backtick is not a terminator and not a defect — this is the branch that keeps
        // the gate off the legitimate `\`cutoff\`` comments in proof-playwright-runner.mjs.
        // When the escaped character is a NEWLINE this is a line continuation: it resets
        // `lineHasContent` (a new physical line) but deliberately leaves `embedded` alone (the
        // embedded script sees no newline, so its line comment continues). See the long note on
        // the main newline branch above before changing either half. BOTH line endings are matched
        // here, and the CRLF half is not decoration: under CRLF the escaped character is `\r`, the
        // branch would not fire, and the pair would fall through to the main newline branch — which
        // clears `embedded`, the one thing this branch exists to prevent. Measured before the fix,
        // a CRLF file reported nothing where its byte-identical LF twin reported a finding.
        lineHasContent = true
        const escapedCrlf = source[index + 1] === '\r' && source[index + 2] === '\n'
        if (source[index + 1] === '\n' || escapedCrlf) {
          resolvePending()
          line += 1
          lineHasContent = false
        }
        index += escapedCrlf ? 3 : 2
        continue
      }

      if (char === '`') {
        // Whether this backtick is the author's own terminator or the recorded defect cannot be
        // decided here — the two are the same character in the same position. Record a candidate
        // and let resolvePending() settle it at end of line; see false-negative (e).
        if (frame.embedded !== null) {
          pending.push({
            line,
            kind:
              frame.embedded === 'line'
                ? 'template-text-line-comment'
                : 'template-text-block-comment',
            depth: stack.length - 1,
          })
        }
        // Reported or not, the grammar says the literal ends here. Closing it keeps the lexer in
        // step with what Node actually parsed, which is the whole reason the damaged file is
        // syntactically valid in the first place.
        stack.pop()
        mode = 'code'
        previous = 'value'
        lineHasContent = true
        index += 1
        continue
      }

      if (char === '$' && source[index + 1] === '{') {
        stack.push({ kind: 'subst', braceDepth: 0 })
        mode = 'code'
        previous = 'none'
        lineHasContent = true
        index += 2
        continue
      }

      if (frame.embedded === null && char === '/' && !lineHasContent) {
        if (source[index + 1] === '/') {
          frame.embedded = 'line'
          lineHasContent = true
          index += 2
          continue
        }
        if (source[index + 1] === '*') {
          frame.embedded = 'block'
          lineHasContent = true
          index += 2
          continue
        }
      }

      if (frame.embedded === 'block' && char === '*' && source[index + 1] === '/') {
        frame.embedded = null
        lineHasContent = true
        index += 2
        continue
      }

      if (!WHITESPACE.test(char)) lineHasContent = true
      index += 1
      continue
    }

    // Code mode.
    if (char === '/' && source[index + 1] === '/') {
      const insideTemplate = stack.length > 0
      let cursor = index + 2
      let reported = false
      while (cursor < source.length && source[cursor] !== '\n') {
        if (insideTemplate && !reported && source[cursor] === '`') {
          findings.push({ line, kind: 'interpolation-line-comment' })
          reported = true
        }
        cursor += 1
      }
      index = cursor
      continue
    }

    if (char === '/' && source[index + 1] === '*') {
      const insideTemplate = stack.length > 0
      let cursor = index + 2
      let reported = false
      let closed = false
      while (cursor < source.length) {
        const commentChar = source[cursor]
        if (commentChar === '*' && source[cursor + 1] === '/') {
          cursor += 2
          closed = true
          break
        }
        if (insideTemplate && !reported && commentChar === '`') {
          findings.push({ line, kind: 'interpolation-block-comment' })
          reported = true
        }
        if (commentChar === '\n') {
          resolvePending()
          line += 1
          lineHasContent = false
        }
        cursor += 1
      }
      if (!closed) {
        desync = 'unterminated block comment'
        break
      }
      index = cursor
      lineHasContent = true
      continue
    }

    if (char === '/') {
      if (previous !== 'value') {
        const end = scanRegexLiteral(source, index)
        if (end !== -1) {
          index = end
          previous = 'value'
          lineHasContent = true
          continue
        }
      }
      previous = 'operator'
      lineHasContent = true
      index += 1
      continue
    }

    if (char === "'" || char === '"') {
      let cursor = index + 1
      let closed = false
      while (cursor < source.length) {
        const stringChar = source[cursor]
        if (stringChar === '\\') {
          if (source[cursor + 1] === '\n') {
            resolvePending()
            line += 1
            lineHasContent = false
          }
          cursor += 2
          continue
        }
        if (stringChar === '\n') break
        if (stringChar === char) {
          cursor += 1
          closed = true
          break
        }
        cursor += 1
      }
      if (!closed) {
        desync = 'unterminated string literal'
        break
      }
      index = cursor
      previous = 'value'
      lineHasContent = true
      continue
    }

    if (char === '`') {
      stack.push({ kind: 'template', embedded: null })
      mode = 'template'
      lineHasContent = true
      index += 1
      continue
    }

    if (char === '{') {
      const top = stack[stack.length - 1]
      if (top && top.kind === 'subst') top.braceDepth += 1
      previous = 'operator'
      lineHasContent = true
      index += 1
      continue
    }

    if (char === '}') {
      const top = stack[stack.length - 1]
      if (top && top.kind === 'subst' && top.braceDepth === 0) {
        stack.pop()
        mode = 'template'
        lineHasContent = true
        index += 1
        continue
      }
      if (top && top.kind === 'subst') top.braceDepth -= 1
      previous = 'value'
      lineHasContent = true
      index += 1
      continue
    }

    if (char === ')' || char === ']') {
      previous = 'value'
      lineHasContent = true
      index += 1
      continue
    }

    if (IDENTIFIER_START.test(char)) {
      let cursor = index
      while (cursor < source.length && IDENTIFIER_PART.test(source[cursor])) cursor += 1
      previous = REGEX_PRECEDING_KEYWORDS.has(source.slice(index, cursor)) ? 'keyword' : 'value'
      index = cursor
      lineHasContent = true
      continue
    }

    if (DIGIT.test(char)) {
      let cursor = index
      while (cursor < source.length && NUMBER_PART.test(source[cursor])) cursor += 1
      previous = 'value'
      index = cursor
      lineHasContent = true
      continue
    }

    if (WHITESPACE.test(char)) {
      index += 1
      continue
    }

    previous = 'operator'
    lineHasContent = true
    index += 1
  }

  // A file whose last line has no trailing newline still gets that line resolved.
  resolvePending()

  if (desync === null && (mode !== 'code' || stack.length > 0)) {
    desync = 'unterminated template literal or interpolation'
  }
  if (desync !== null) return { findings: [], desync }
  return { findings, desync: null }
}

// Run the scan over every file `scripts/lint-scripts.mjs` resolves. Returns
// `{ ok, scanned, misses, skipped }`; `misses` are `file:line: message` strings and `skipped` names
// the files whose lex desynced (false-negative (a)) so a run can say so out loud.
export function checkTemplateLiteralComments(repoRoot = REPO_ROOT) {
  const files = resolveScriptFiles(repoRoot)

  // Narrowing tripwire, same shape as check-docs-absolute-selflinks.mjs: this gate's success state
  // is "found nothing", which is byte-identical to a run that looked at nothing. If the shared file
  // resolution ever returns empty, say so rather than printing a green line.
  if (files.length === 0) {
    return {
      ok: false,
      scanned: 0,
      skipped: [],
      misses: [
        'resolveScriptFiles() from scripts/lint-scripts.mjs returned no files — this gate would ' +
          'pass without checking anything. Fix the file-set resolution there.',
      ],
    }
  }

  const misses = []
  const skipped = []
  for (const relative of files) {
    let source
    try {
      source = fs.readFileSync(path.join(repoRoot, relative), 'utf8')
    } catch (error) {
      // This catches ANY read failure, not only the vanished-between-resolution-and-read case it
      // used to name: EACCES, a directory, a symlink loop. The file was not read, so it must not
      // disappear silently. `scanned` counts files CONSIDERED and `skipped` is the subset that was
      // not actually checked — a desync already uses that channel, and an unreadable file belongs
      // in it for the same reason. Dropping it on the floor instead would break this module's own
      // rule, stated in main(): never claim more than was checked.
      skipped.push(
        `${relative}: unreadable (${error && error.code ? error.code : 'unknown error'})`,
      )
      continue
    }
    const { findings, desync } = scanTemplateLiteralComments(source)
    if (desync !== null) {
      skipped.push(`${relative}: ${desync}`)
      continue
    }
    for (const finding of findings) {
      misses.push(
        `${relative}:${finding.line}: backtick in a comment inside a template literal [${finding.kind}]`,
      )
    }
  }

  return { ok: misses.length === 0, scanned: files.length, misses, skipped }
}

function main() {
  const result = checkTemplateLiteralComments()

  // Never claim more than was checked: the skip count is part of the result, not a footnote, and it
  // is reported on BOTH paths — a run that found something looked at no more of the tree than a
  // clean one did, and a reader who only ever sees it on success would read silence as coverage.
  const skippedSummary =
    result.skipped.length === 0
      ? ''
      : `${result.skipped.length} file(s) SKIPPED (lexer desynced, findings discarded): ${result.skipped.join('; ')}`

  if (!result.ok) {
    // The headline is kind-NEUTRAL on purpose. It used to assert "the literal ends at that backtick,
    // so the file still parses but means something else", which is true of template-text-* and FALSE
    // of interpolation-*: case (2) in the header is valid JavaScript that means exactly what it says
    // and is reported as hygiene. Printing the syntax-hazard diagnosis above an interpolation-only
    // run told that reader something untrue about their own file, so the diagnosis moved down to sit
    // with the remedy it belongs to. Both halves are kind-specific; only the fact is shared.
    console.error('Backtick found in a comment nested inside a JS template literal:')
    for (const miss of result.misses) console.error(miss)
    // The remedy differs by kind, and one instruction for both sends half the readers back to an
    // identical red. In template TEXT the lexer honours a backslash-escaped backtick, so escaping
    // genuinely fixes it. A host comment inside `${…}` has no escapes at all — a backslash there is
    // just a backslash and the backtick after it still ends nothing and still reports — so that one
    // has to be reworded instead. Measured both ways.
    if (result.misses.some((miss) => miss.includes('[template-text-'))) {
      console.error(
        'template-text-*: the literal ENDS at that backtick, so the file still parses but means ' +
          'something else — and prettier reformats the result into what a reviewer reads as a ' +
          'harmless reformat. Remove the backtick, or escape it as \\` if the comment needs one.',
      )
    }
    if (result.misses.some((miss) => miss.includes('[interpolation-'))) {
      console.error(
        'interpolation-*: this one is valid JavaScript — the backtick is in a HOST comment inside ' +
          '${...}, where it ends nothing. Reported as hygiene because the two positions are ' +
          'indistinguishable at the point of edit and moving the comment one level out makes it ' +
          'the case above. Remove the backtick — escaping does NOT work here. Reword.',
      )
    }
    console.error('See the header of scripts/check-template-literal-comments.mjs for the rule.')
    if (skippedSummary) console.error(skippedSummary)
    return false
  }

  console.log(
    `Template-literal comment backticks OK (${result.scanned} script file(s) scanned)` +
      (skippedSummary ? ` — ${skippedSummary}` : ''),
  )
  return true
}

if (isMainModule(import.meta.url)) {
  if (!main()) process.exit(1)
}
