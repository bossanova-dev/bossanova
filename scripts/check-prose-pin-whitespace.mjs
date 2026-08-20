#!/usr/bin/env node

// check-prose-pin-whitespace — a multi-word phrase pinned with LITERAL spaces is a defect in a
// skill-content gate, whichever direction it is read from. This gate sees it.
//
// THE RULE, stated once: in a regex literal inside a skill-content gate file, the gap between two
// words is written `\s+`, never a literal space. `/revert\s+to\s+Opus/`, not `/revert to Opus/`.
//
// WHY (BOS-797, from BOS-641 and PR #1831). The line breaks in the prose these patterns pin are not
// stable. Prettier is not the one moving them: the root `.prettierrc` sets no `proseWrap`, so it is
// at the `preserve` default and markdown paragraphs keep the breaks their author gave them.
// Prettier does reformat the tables and code constructs around them at `printWidth: 100`, and a
// human or an agent rewraps a paragraph while editing a neighbouring sentence — either way a pinned
// phrase moves across a line break with no word of it changing. The rule has two consumers and it
// fails differently for each:
//
//   (1) A GATE PIN fails LOUDLY. `/revert to Opus/` goes red the moment the phrase wraps, and the
//       red names no real defect: the prose is intact and correct, only its line breaks moved.
//       That is a debugging detour costing an author a round trip to discover the assertion was
//       about formatting.
//
//   (2) A FALSIFICATION / MUTATION pattern fails SILENTLY, and that is the dangerous half. A
//       literal-space `perl -pi -e` substitution aimed at a wrapped sentence matches NOTHING, exits
//       0, changes no bytes, and leaves the suite green — which is byte-identical to the genuine
//       "this pin is vacuous" finding the probe exists to produce. Two of four substitutions in one
//       recorded round failed exactly this way and inverted the conclusion. This gate can only see
//       consumer (1), because a mutation pattern lives in a shell command in a transcript rather
//       than in a file this repo lints. Consumer (2) is covered by docs/skills/README.md, which
//       states the rule for both; this header exists so the reader who lands here from a red run
//       learns that the same rule governs the probe they are about to write.
//
// WHAT IS FLAGGED: a run of one or more literal spaces or tabs inside a regex literal, where the
// character immediately before the run and the character immediately after it are both word
// characters (`[A-Za-z0-9_]`). A RUN, not a single space: with `/alpha  beta/` neither space has a
// word character on both sides, so a per-character rule reads the defect as clean.
//
// WHAT IS DELIBERATELY NOT FLAGGED, each one a decision rather than an oversight:
//
//   * A space inside a character class — `/alpha[ ]beta/`. That is the documented spelling for a
//     pin that must NOT tolerate a line break (see "`\s+` widens the match" below); rewriting it
//     would reverse the author's explicit choice, and rewriting the space inside the class would
//     break the class outright.
//   * A backslash-escaped space — `/alpha\ beta/`. Already an explicit spelling.
//   * A space whose left neighbour is the TAIL OF AN ESCAPE — `/\n  heartbeat/`. The character
//     sitting at `start - 1` there is the `n` of `\n`, which is a word character to a raw
//     character test, but the token it belongs to is a newline, not a word. Flagging it once cost a
//     real assertion: a pin on a fenced code block was rewritten `\n\s+`, and because `\s` matches
//     a newline the pin stopped proving the statements were on consecutive lines. Nothing here is
//     prose, so the run is skipped. That leaves `/\d 5/` and friends unflagged too — a missed
//     offender, which is this gate's safe direction; a forced wrong rewrite is not.
//   * A space not flanked by word characters on both sides — `/gh pr create --base "$B"/` has its
//     `create --base` gap flanked by `e` and `-`, so only the `gh pr create` half is reported. This
//     leaves command-line patterns partially converted, which reads oddly but is the rule as
//     scoped: widening the flanking set to every non-space character would pull in regex syntax
//     (`)`, `|`, `]`) where a `\s+` rewrite is far less obviously safe.
//   * A quantified space — `/alpha +beta/`. Same reflow hazard (a `+` on a literal space still does
//     not match a newline), but rewriting a quantifier is not the mechanical substitution this gate
//     asks for. Known gap, recorded here rather than left for the next reader to rediscover.
//   * A pattern built by `new RegExp('alpha beta')` or from a template literal. Only regex LITERALS
//     are scanned. A string-built pattern has the same hazard and no gate; write it `\\s+`.
//
// `\s+` WIDENS THE MATCH. `/a\s+b/` matches `a\n\nb` across a paragraph break where `/a b/` would
// not. For phrase pins that is the point. For a pin that must prove two words share one line, use
// `/a[ ]b/`. No opt-out marker is needed with it — a space inside a character class is not what
// this gate flags.
//
// NOT EVERY PIN IS A PROSE PIN. When the subject is a command invocation inside a fenced block, or
// a message a script builds at runtime, the reflow hazard cannot reach it: prettier does not
// reformat fenced content and a runtime string is not markdown. `\s+` there is a pure loosening —
// `/git\s+push/` accepts `git  push` and `git\npush` where only `git push` runs — so those pins are
// written `/git[ ]push/`. A pin that reads the same command both in a fence and in a sentence is a
// prose pin and keeps `\s+`, because the sentence can rewrap. And a NEGATIVE assertion
// (`assert.doesNotMatch`) keeps `\s+` whatever its subject: there the widening runs the other way
// -- a prohibition written `\s+` forbids a superset of the spellings `[ ]` forbids, so it is the
// strictly stronger one.
//
// THE OPT-OUT: `prose-pin: literal-space ok` on the offending line or the line immediately above
// it. One fixed string, so every use is auditable in one command:
//
//     grep -rEn 'prose-pin:[[:space:]]+literal-space[[:space:]]+ok' scripts
//
// The marker is matched whitespace-tolerantly (`/prose-pin:\\s+literal-space\\s+ok/`), so the
// audit command must be too — a fixed-string grep for the single-space spelling would miss a
// marker the gate honours and report a complete audit that is not one.
//
// It is for a pattern deliberately matching a code construct rather than prose — a fenced command
// line whose spacing is the thing under test, say — not for a pin the author would rather not
// convert.
//
// SCOPE: the file set is derived from the glob `scripts/*skill*.test.mjs`, never a hand-list, so a
// newly added skill-content gate is covered without editing this file. That glob is a superset of
// `scripts/*-skill.test.mjs`: it also catches `skill-model-tier.test.mjs`, `skill-frontmatter.test.mjs`
// and friends, whose names put "skill" first, and the `check-skill-*` / `sync-codex-skills` gates.
// Scanning a file that is not strictly a prose-pin gate costs nothing — the rule holds there too —
// while missing one of the named candidates would have left the ticket's own examples uncovered.
//
// NECESSARY BUT NOT SUFFICIENT. A green run proves no gate file pins prose with literal spaces. It
// does not prove any pin still MATCHES its target — a converted pin that stopped matching is the
// silent vacuity this ticket exists to prevent, and only the gate files' own test runs prove that.
//
// Exercised by scripts/check-prose-pin-whitespace.test.mjs and runnable via
// `node scripts/check-prose-pin-whitespace.mjs`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

// The repo root relative to this file, so the gate works from any cwd — it runs from the repo root
// via `node scripts/check-prose-pin-whitespace.mjs` and from `scripts/` via the scripts Makefile.
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// The single source of truth for what this gate reads. Exported so a test can assert the file set
// really is glob-derived rather than re-listing files the gate happens to know about today.
export const GATE_FILE_GLOB = 'scripts/*skill*.test.mjs'

// The opt-out, matched anywhere on the line so it works from a `//` comment, a `/* */` comment, or
// a trailing comment after the pattern itself. Its own inter-word gap is `\s+`, because a gate that
// breaks its own rule in its own marker teaches the wrong thing to whoever copies it.
const OPT_OUT_MARKER = /prose-pin:\s+literal-space\s+ok/

const IDENTIFIER_START = /[A-Za-z_$]/
const IDENTIFIER_PART = /[A-Za-z0-9_$]/
const DIGIT = /[0-9]/
const NUMBER_PART = /[0-9A-Za-z_.]/
const REGEX_FLAG = /[a-z]/i
const WORD_CHARACTER = /[A-Za-z0-9_]/

// Words after which a `/` opens a regex rather than dividing. Everything else that can end an
// expression (identifier, number, string, `)`, `]`, `}`) makes it division — the same reading, and
// the same known false negative, as scripts/check-template-literal-comments.mjs. A missed regex is
// an unreported offender, never a false one, so the two gates fail in the same safe direction.
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

// The offending runs inside one regex literal's SOURCE (the text between the delimiters), as
// `{ start, end }` offsets into that source. Exported so the rule can be tested directly on small
// inputs: its exclusions are the subtle part of this gate, and asserting them against whole files
// would prove them only where the tree happens to exercise them. No codemod ships here — the
// conversion was a one-off — so the test suite is the only consumer.
export function literalSpaceRuns(body) {
  const runs = []
  let index = 0
  let inCharacterClass = false
  // Where the most recently consumed escape sequence ended. A space run starting exactly there has
  // an escape, not a word, on its left — see "WHAT IS DELIBERATELY NOT FLAGGED" above.
  let escapeEnd = -1

  while (index < body.length) {
    const char = body[index]

    // A backslash consumes the next character whatever it is, so `\ ` never reaches the run test.
    if (char === '\\') {
      index += 2
      escapeEnd = index
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
    if (char === ' ' || char === '\t') {
      const start = index
      while (index < body.length && (body[index] === ' ' || body[index] === '\t')) index += 1
      const before = start > 0 ? body[start - 1] : ''
      const after = index < body.length ? body[index] : ''
      const afterEscape = start === escapeEnd
      if (!afterEscape && WORD_CHARACTER.test(before) && WORD_CHARACTER.test(after)) {
        runs.push({ start, end: index })
      }
      continue
    }
    index += 1
  }

  return runs
}

// Scan a regex literal starting at `start` (the opening `/`). Returns `{ end, bodyEnd }` — the index
// just past the flags, and the index of the closing `/` — or -1 when the candidate is not a regex
// after all. A regex may not contain a raw line terminator, so hitting one means the `/` was
// division; returning -1 lets the caller rewind rather than swallow the rest of the file.
function scanRegexLiteral(source, start) {
  let index = start + 1
  let inCharacterClass = false

  while (index < source.length) {
    const char = source[index]
    if (char === '\n') return -1
    if (char === '\\') {
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
      const bodyEnd = index
      index += 1
      while (index < source.length && REGEX_FLAG.test(source[index])) index += 1
      return { end: index, bodyEnd }
    }
    index += 1
  }

  return -1
}

// Every regex literal in one JavaScript source, as
// `{ line, start, end, bodyStart, bodyEnd, pattern }` with a 1-based line and absolute offsets, plus
// `desync`: null on a clean lex, or a short reason string.
//
// A DESYNCED LEX REPORTS NOTHING. If the walk ends inside a string, template literal, or block
// comment, the lexer lost the thread somewhere and every `/` it read after that point is suspect —
// so the literals are discarded rather than reported, and the caller surfaces the file as skipped.
// Reporting them instead would put a false `file:line` in front of an author with nothing wrong.
// Discarding is not forgiving, though: `checkProsePinWhitespace` FAILS on a skip, because a file
// the lexer could not follow is a file whose offenders would go unreported — silence in the one
// direction this gate exists to remove.
export function findRegexLiterals(source) {
  const literals = []
  // A shebang is not JavaScript, and its `/usr/bin` reads as a regex literal to the scan below. No
  // false finding comes of it — `usr` has no space in it — but a wrong literal is a wrong thing to
  // hand the codemod that shares this function, so the line is skipped outright.
  const shebang = source.startsWith('#!')
  let index = shebang ? source.indexOf('\n') + 1 || source.length : 0
  let line = shebang ? 2 : 1
  // 'value' when the previous significant token can end an expression (so `/` divides); anything
  // else means `/` opens a regex.
  let previous = 'none'
  // Frames: 'template' for a literal's text, or `{ braceDepth }` for a `${…}` expression.
  const stack = []
  let mode = 'code'
  let desync = null

  while (index < source.length && desync === null) {
    const char = source[index]

    if (mode === 'template') {
      if (char === '\\') {
        if (source[index + 1] === '\n') line += 1
        index += 2
        continue
      }
      if (char === '\n') {
        line += 1
        index += 1
        continue
      }
      if (char === '$' && source[index + 1] === '{') {
        stack.push({ braceDepth: 0 })
        mode = 'code'
        previous = 'none'
        index += 2
        continue
      }
      if (char === '`') {
        stack.pop()
        mode = stack.length > 0 && stack[stack.length - 1] === 'template' ? 'template' : 'code'
        previous = 'value'
        index += 1
        continue
      }
      index += 1
      continue
    }

    if (char === '\n') {
      line += 1
      index += 1
      continue
    }
    if (char === ' ' || char === '\t' || char === '\r') {
      index += 1
      continue
    }
    if (char === '/' && source[index + 1] === '/') {
      while (index < source.length && source[index] !== '\n') index += 1
      continue
    }
    if (char === '/' && source[index + 1] === '*') {
      index += 2
      while (index < source.length && !(source[index] === '*' && source[index + 1] === '/')) {
        if (source[index] === '\n') line += 1
        index += 1
      }
      if (index >= source.length) {
        desync = 'block comment never closed'
        break
      }
      index += 2
      continue
    }
    if (char === '"' || char === "'") {
      const quote = char
      index += 1
      let closed = false
      while (index < source.length) {
        if (source[index] === '\\') {
          if (source[index + 1] === '\n') line += 1
          index += 2
          continue
        }
        if (source[index] === '\n') break
        if (source[index] === quote) {
          index += 1
          closed = true
          break
        }
        index += 1
      }
      if (!closed) {
        desync = 'string literal never closed on its line'
        break
      }
      previous = 'value'
      continue
    }
    if (char === '`') {
      stack.push('template')
      mode = 'template'
      index += 1
      continue
    }
    if (char === '{') {
      const top = stack[stack.length - 1]
      if (top && top !== 'template') top.braceDepth += 1
      previous = 'op'
      index += 1
      continue
    }
    if (char === '}') {
      const top = stack[stack.length - 1]
      if (top && top !== 'template') {
        if (top.braceDepth === 0) {
          stack.pop()
          mode = 'template'
          index += 1
          continue
        }
        top.braceDepth -= 1
      }
      previous = 'value'
      index += 1
      continue
    }
    if (IDENTIFIER_START.test(char)) {
      const start = index
      while (index < source.length && IDENTIFIER_PART.test(source[index])) index += 1
      previous = REGEX_PRECEDING_KEYWORDS.has(source.slice(start, index)) ? 'keyword' : 'value'
      continue
    }
    if (DIGIT.test(char)) {
      while (index < source.length && NUMBER_PART.test(source[index])) index += 1
      previous = 'value'
      continue
    }
    if (char === '/') {
      if (previous === 'value') {
        index += 1
        previous = 'op'
        continue
      }
      const scanned = scanRegexLiteral(source, index)
      if (scanned === -1) {
        index += 1
        previous = 'op'
        continue
      }
      literals.push({
        line,
        start: index,
        end: scanned.end,
        bodyStart: index + 1,
        bodyEnd: scanned.bodyEnd,
        pattern: source.slice(index, scanned.end),
      })
      index = scanned.end
      previous = 'value'
      continue
    }
    if (char === ')' || char === ']') {
      previous = 'value'
      index += 1
      continue
    }
    previous = 'op'
    index += 1
  }

  if (desync === null && mode === 'template') desync = 'template literal never closed'
  if (desync === null && stack.length > 0) desync = 'unbalanced template interpolation'

  return desync === null ? { literals, desync: null } : { literals: [], desync }
}

// Scan one JavaScript source for prose pinned with literal spaces.
//
// Returns `{ findings, marked, desync }`. Each finding is `{ line, pattern }` with a 1-based line;
// `marked` counts the opt-out-excused literals so a clean run can say how many exceptions it
// honoured rather than reporting silence. Findings and exceptions are decided in one walk, so the
// two counts cannot drift apart.
export function scanProsePinWhitespace(source) {
  const { literals, desync } = findRegexLiterals(source)
  if (desync !== null) return { findings: [], marked: 0, desync }

  const lines = source.split(/\r?\n/)
  const findings = []
  let marked = 0

  for (const literal of literals) {
    if (literalSpaceRuns(source.slice(literal.bodyStart, literal.bodyEnd)).length === 0) continue

    const own = lines[literal.line - 1] ?? ''
    const above = literal.line > 1 ? (lines[literal.line - 2] ?? '') : ''
    if (OPT_OUT_MARKER.test(own) || OPT_OUT_MARKER.test(above)) {
      marked += 1
      continue
    }

    findings.push({ line: literal.line, pattern: literal.pattern })
  }

  return { findings, marked, desync: null }
}

// Every skill-content gate file, as absolute paths in stable (sorted) order. Derived from
// `GATE_FILE_GLOB` against `<repoRoot>/scripts`, so adding a gate file adds coverage.
export function discoverGateFiles(repoRoot = REPO_ROOT) {
  if (typeof fs.globSync !== 'function') {
    // Loud, not silent: an older Node would otherwise throw a TypeError that reads as a broken gate
    // rather than as a wrong runtime. `.node-version` pins 22.22.2, where globSync exists.
    throw new Error(
      'fs.globSync is unavailable — this gate needs Node >= 22 (see .node-version). ' +
        `Cannot expand ${GATE_FILE_GLOB}.`,
    )
  }
  return fs
    .globSync(GATE_FILE_GLOB, { cwd: repoRoot })
    .map((relative) => path.join(repoRoot, relative))
    .sort()
}

export function checkProsePinWhitespace(repoRoot = REPO_ROOT) {
  const gateFiles = discoverGateFiles(repoRoot)

  // Narrowing tripwire. This gate's success state is "found nothing", which is byte-identical to a
  // run that LOOKED at nothing: a renamed convention, a moved directory, or a typo in the glob
  // leaves the file set empty and prints a green line forever. The gate lives in `scripts/`
  // alongside the files it reads, so an empty expansion always means the glob has gone stale.
  if (gateFiles.length === 0) {
    console.error(`No files matched ${GATE_FILE_GLOB}, so this gate checked nothing.`)
    console.error(
      'Update GATE_FILE_GLOB in scripts/check-prose-pin-whitespace.mjs so it stops passing ' +
        'without scanning anything.',
    )
    return false
  }

  const misses = []
  const skipped = []
  let marked = 0
  let scanned = 0

  for (const gateFile of gateFiles) {
    const relative = path.relative(repoRoot, gateFile)
    let source
    try {
      source = fs.readFileSync(gateFile, 'utf8')
    } catch (error) {
      // The file was not read, so it must not disappear silently — `scanned` counts files actually
      // read, and everything else lands in `skipped`.
      skipped.push(
        `${relative}: unreadable (${error && error.code ? error.code : 'unknown error'})`,
      )
      continue
    }

    const { findings, marked: fileMarked, desync } = scanProsePinWhitespace(source)
    if (desync !== null) {
      skipped.push(`${relative}: ${desync}`)
      continue
    }

    scanned += 1
    marked += fileMarked
    for (const finding of findings) {
      misses.push(`${relative}:${finding.line}: ${finding.pattern}`)
    }
  }

  // A file the gate could not read or could not lex is a file it cannot vouch for, and a skip is
  // silent in the one direction that matters: the offender inside it is never reported, so an
  // exit 0 here would be the exact false green this gate exists to remove. CI reads the exit code,
  // not the summary line, so saying "SKIPPED" on stdout does not discharge it. Fail instead, and
  // let the author either fix the file or teach the lexer.
  if (skipped.length > 0) {
    console.error('This gate could not check every file in its set, so it cannot pass:')
    for (const entry of skipped) console.error(entry)
    console.error(
      'A skipped file is unchecked, not clean — a literal-space pin inside it would go ' +
        'unreported. A lex failure is not proof the file is invalid JavaScript: this lexer reads ' +
        'a `/` after `)`, `]` or `}` as division, so a valid regex literal in one of those ' +
        'positions can desync it. Extend the lexer in scripts/check-prose-pin-whitespace.mjs, or ' +
        'rephrase the construct it could not follow.',
    )
    if (misses.length > 0) {
      console.error('Regex literals pin multi-word prose with literal spaces:')
      for (const miss of misses) console.error(miss)
    }
    return false
  }

  // A defensive invariant, and — as the code stands — an UNREACHABLE one. Reaching here means the
  // empty-glob guard above passed (`gateFiles.length > 0`) and the skip guard above passed
  // (`skipped.length === 0`), and every path out of the loop either increments `scanned` or pushes
  // to `skipped`, so `scanned` equals `gateFiles.length` and cannot be zero. It is kept, and said
  // plainly to be dead, so that a future loop path which neither scans nor skips fails CLOSED
  // instead of printing a green line over nothing. Do not write a test claiming to exercise it:
  // every input that reaches zero scanned files today is caught by the skip guard first.
  if (scanned === 0) {
    console.error(
      `${gateFiles.length} file(s) matched ${GATE_FILE_GLOB} but none were scanned, so this ` +
        'gate checked nothing.',
    )
    return false
  }

  if (misses.length > 0) {
    console.error('Regex literals pin multi-word prose with literal spaces:')
    for (const miss of misses) console.error(miss)
    console.error(
      'Join the words with \\s+ (e.g. /revert\\s+to\\s+Opus/). A literal space goes red the ' +
        'moment the phrase is rewrapped across a line break — a failure about formatting, ' +
        'not about the pinned contract being absent.',
    )
    console.error(
      'If the pattern deliberately matches a code construct whose spacing is the thing under ' +
        'test, mark it `prose-pin: literal-space ok` on that line or the line above it.',
    )
    console.error('See the header of scripts/check-prose-pin-whitespace.mjs for the rule.')
    return false
  }

  // No skip summary is appended here: a non-empty `skipped` already returned false above, so on
  // this path it is always empty.
  console.log(`Prose-pin whitespace OK (${scanned} file(s) scanned, ${marked} marked exception(s))`)
  return true
}

if (isMainModule(import.meta.url)) {
  if (!checkProsePinWhitespace()) process.exit(1)
}
