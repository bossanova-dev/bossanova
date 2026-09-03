#!/usr/bin/env node

// check-ellipsis-consistency — user-facing copy elides with the ellipsis character U+2026 (…),
// never with a run of three full stops. This gate is the ratchet that keeps BOS-1087 converged.
//
// THE RULE, stated once: inside a STRING LITERAL in a rendered-copy tree, a run of three
// consecutive full stops is a defect. Write the single ellipsis character instead.
//
// WHY. The repo shipped both spellings side by side — a truncation marker rendered one way in the
// TUI and the other on the web, a progress label written one way in a Go view and the other in the
// React page that mirrors it. The two are indistinguishable at a glance and identical in meaning,
// so nothing drove them back together; a sweep converged them once, and without a gate the next
// hand-typed label reopens the split. The character also measures as one cell in the TUI's width
// arithmetic where the three-stop spelling measures as three, so the two spellings do not even
// truncate to the same place.
//
// WHY THE LITERAL IS ASSEMBLED FROM PARTS BELOW. This file names the thing it forbids. Spelling it
// out here would make the gate its own first offender under any repo-wide search for the pattern —
// the search would answer itself with this file, and a reader auditing the remaining uses could not
// tell the gate's own prose from a real site. So the run is built with String.fromCharCode and
// repeat(), and this header describes it in words. For the same reason there is no rest/spread
// syntax anywhere in this file: the spread operator is spelled with the very run under test.
//
// COMMENTS ARE NOT SCANNED. Go `//` and `/* */`, TypeScript `//` and `/* */`, all skipped, and this
// is a decision rather than an oversight. What survives in this repo's comments is elision inside
// quoted prose and shape description — a path abbreviated in the middle, a call written in Go
// notation with its arguments left out, a type sketched with its tail omitted. None of it is copy a
// user ever reads, none of it should carry a typographic ellipsis, and scanning it would grow the
// exception list past the point where anyone audits it. A comment is not a rendering surface.
//
// WHAT COUNTS AS A STRING LITERAL:
//   * Go — interpreted literals delimited by double quotes, and raw literals delimited by
//     backticks. Rune literals are lexed (so a quote inside one cannot desync the walk) but never
//     scanned: a rune holds one code point and cannot hold a run of three.
//   * TypeScript / TSX — single-quoted, double-quoted, and template-literal text. A template's
//     `${…}` expressions are lexed back as code, so a nested literal inside one is scanned on its
//     own terms rather than as template text.
//   * TSX additionally — JSX text nodes, the bare copy between an opening tag and the next `<` or
//     `{`. Attribute strings are read in the tag walk, so they need no special case.
//
// WHAT IS DELIBERATELY NOT FLAGGED, each one a decision:
//
//   * Language syntax that shares the spelling: JavaScript spread and rest, and Go variadic
//     parameters and call spreads. All of it is code rather than string contents, so a
//     literal-only scanner excludes it structurally rather than by pattern. There is no regex here
//     trying to tell it apart from copy, which is why it cannot be told apart wrongly. The
//     spellings are not written out here for the reason given above; the fixtures in
//     scripts/check-ellipsis-consistency.test.mjs show them, where they are the thing under test.
//   * A git revision range inside a string — the operator between two refs. It is an argument to
//     git, not copy, and the character would break it.
//   * Cobra variadic-argument notation inside a `Use:` string. Same: a usage grammar the CLI
//     framework parses, not a sentence.
//
//     Those last two are NOT structural, and the distinction matters: unlike everything else in
//     this list they live INSIDE a string literal, so the scanner does see them and this gate does
//     flag them. They are excused per site, through one of the two mechanisms below — a marker on
//     the line, or an EXEMPTIONS entry — exactly so that each one is a decision somebody recorded
//     rather than a pattern quietly matching whatever resembles it. Adding a new one to a scanned
//     tree therefore means adding its opt-out too; the gate will say so.
//   * The inside of a JSX brace group. Element children are read as text only up to the next `{`,
//     and what follows is lexed back as code — so a spread passed as a child cannot be read as
//     copy, and a string inside an interpolation is scanned as the string it is.
//   * A regular-expression literal's own source. It is skipped with the comments, so a pattern
//     that matches the run is not itself a use of it.
//
// THE TWO EXCLUSION MECHANISMS, both enumerated rather than pattern-matched:
//
//   1. An inline marker, `ellipsis: literal-dots ok`, on the offending line or the line
//      immediately above it. Matched whitespace-tolerantly, so the audit command must be too:
//
//          grep -rEn 'ellipsis:[[:space:]]+literal-dots[[:space:]]+ok' services
//
//      Prefer this for a single line: it sits at the site and says why in the same comment.
//   2. The EXEMPTIONS list below, for a file no marker can reach — generated output, or a file
//      whose every occurrence shares one reason. It RATCHETS: an entry matching nothing is itself
//      a failure, so a survivor that is finally converted cannot leave its licence behind.
//
// SCOPE, and what a green run does not prove. The scanned trees are the ones where rendered copy
// actually lives: the Boss TUI's views, its account-registration flow, its auth prompts, and the
// web app's source. Copy composed elsewhere and rendered through these trees is not covered, and
// neither is any string this lexer reads as code. A green run means no scanned literal in those
// trees states the run — never that every ellipsis in the product is the right character.
//
// Exercised by scripts/check-ellipsis-consistency.test.mjs and runnable via
// `node scripts/check-ellipsis-consistency.mjs`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

// The repo root relative to this file, so the gate works from any cwd — it runs from the repo root
// via `node scripts/check-ellipsis-consistency.mjs` and from `scripts/` via the scripts Makefile.
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// The forbidden run, assembled rather than written. See "WHY THE LITERAL IS ASSEMBLED FROM PARTS"
// in the header: a gate that spells out its own pattern answers every audit search with itself.
const FULL_STOP = String.fromCharCode(46)
export const FORBIDDEN_RUN = FULL_STOP.repeat(3)

// The character the rule converges on, named so an error message can show it without this file
// depending on the reader's editor rendering it.
export const ELLIPSIS = String.fromCharCode(0x2026)

// The inline opt-out, matched anywhere on the line so it works from a Go `//`, a TypeScript `//`,
// a block comment, or a trailing comment after the literal itself.
export const OPT_OUT_MARKER = /ellipsis:\s+literal-dots\s+ok/

/**
 * The trees this gate reads, with the extensions it scans in each.
 *
 * Hand-maintained and ratcheted: a tree that is missing, or that holds none of its declared
 * extensions, is reported as a coverage hole rather than walked past. A renamed or mistyped root
 * would otherwise cover zero files and keep this gate green over copy nobody reads.
 *
 * These are the RENDERING surfaces: a string here reaches a human as product copy. The Boss TUI's
 * views, the two CLI flows that print progress lines of their own (accountflow, auth), the CLI
 * commands themselves, and the web app.
 *
 * The boundary is deliberate and was set by measurement, not by which files BOS-1087 happened to
 * edit. Running this scanner over the other trees that ticket touched — services/boss/internal/agent,
 * services/bossd/internal/server, and both plugins/bossd-plugin-* — yields 18 findings and zero
 * rendered-copy defects: every one is `truncate(...) = %q`-style call notation inside a test failure
 * message, an agent prompt-template fixture, or a data-URI placeholder. Those trees PRODUCE strings
 * a human eventually sees (a chat title, a truncated plan summary) but they are not where copy is
 * WRITTEN, and admitting them would buy an 18-entry exception ledger for no detection. Re-run that
 * measurement before widening; do not widen because a sweep touched a file.
 */
export const SCAN_TREES = [
  { path: 'services/boss/internal/views', extensions: ['.go'] },
  { path: 'services/boss/internal/accountflow', extensions: ['.go'] },
  { path: 'services/boss/internal/auth', extensions: ['.go'] },
  { path: 'services/boss/cmd', extensions: ['.go'] },
  { path: 'services/web/src', extensions: ['.ts', '.tsx'] },
]

/**
 * Extensions present in the scanned trees that hold no scannable source.
 *
 * This list exists so that an extension in NEITHER set fails the gate by name. A new file type
 * appearing in a rendered-copy tree is either a surface this gate must read or one it must be told
 * to ignore, and the one answer it must never give is silence.
 */
export const IGNORED_EXTENSIONS = [
  '.bazel',
  '.build',
  '.css',
  '.json',
  '.md',
  '.png',
  '.snap',
  '.svg',
]

/** Directory names skipped wherever they appear inside a scanned tree. */
export const IGNORED_DIRECTORIES = ['build', 'coverage', 'dist', 'node_modules', 'testdata']

/**
 * Files, or single lines within them, excused by name.
 *
 * `text` is a substring of the offending line; `null` excuses every occurrence in the file. Each
 * entry ratchets — see the header. Prefer the inline marker for a single line; this list is for
 * what a marker cannot reach.
 */
export const EXEMPTIONS = [
  // Five occurrences across three generator fixtures, all the same two lines of stdout from a
  // user's own setup script. Five near-identical inline comments in one test file would read as
  // noise, and the reason is a property of the fixture rather than of any one line — so it is
  // stated once here. Note the `text` values carry no full stops of their own: they are matched as
  // substrings, which keeps this file from spelling out the run it forbids.
  {
    path: 'services/web/src/pages/NewSession.test.tsx',
    text: 'Setting up workspace',
    reason: 'replayed stdout from a repository setup script, not copy this project writes',
  },
  {
    path: 'services/web/src/pages/NewSession.test.tsx',
    text: 'Installing dependencies',
    reason: 'replayed stdout from a repository setup script, not copy this project writes',
  },
  // The Sentry scrub marker. It is machine-facing, not copy, and it is replicated across three
  // surfaces that cannot import from one another (lib/bossalib/errortrack/scrub.go,
  // services/web/src/sentry-init.ts, services/docs/src/sentry-init.ts) and must stay byte-identical
  // between them, so it keeps the plain spelling. Carried here rather than as an inline marker
  // precisely BECAUSE of that replication: only the web copy is inside this gate's scan surface, so
  // an inline comment would make one of the three siblings differ from the other two, and "these
  // three files are byte-identical" is the property a reader diffs them to check.
  {
    path: 'services/web/src/sentry-init.ts',
    text: '[truncated]',
    reason: 'machine-facing Sentry scrub marker, replicated byte-identically across three surfaces',
  },
]

// ---------------------------------------------------------------------------
// The rule, per language
// ---------------------------------------------------------------------------

// Absolute offsets of every forbidden run wholly inside `[start, end)`.
function collectRuns(source, start, end, hits) {
  let at = source.indexOf(FORBIDDEN_RUN, start)
  while (at !== -1 && at + FORBIDDEN_RUN.length <= end) {
    hits.push(at)
    at = source.indexOf(FORBIDDEN_RUN, at + FORBIDDEN_RUN.length)
  }
}

/**
 * Scan one Go source for forbidden runs inside string literals.
 *
 * Returns `{ hits, desync }` — absolute offsets, and null on a clean lex or a short reason string.
 * A DESYNCED LEX REPORTS NOTHING: if the walk loses the thread the offsets after that point are
 * suspect, so they are discarded and the caller fails the file rather than printing a wrong
 * `file:line` at an author with nothing wrong.
 */
export function scanGo(source) {
  const hits = []
  let index = 0
  let desync = null

  while (index < source.length) {
    const char = source[index]

    if (char === '/' && source[index + 1] === '/') {
      while (index < source.length && source[index] !== '\n') index += 1
      continue
    }
    if (char === '/' && source[index + 1] === '*') {
      const end = source.indexOf('*/', index + 2)
      if (end === -1) {
        desync = 'block comment never closed'
        break
      }
      index = end + 2
      continue
    }
    if (char === '"') {
      const bodyStart = index + 1
      index += 1
      let closed = false
      while (index < source.length) {
        if (source[index] === '\\') {
          index += 2
          continue
        }
        if (source[index] === '\n') break
        if (source[index] === '"') {
          closed = true
          break
        }
        index += 1
      }
      if (!closed) {
        desync = 'interpreted string literal never closed on its line'
        break
      }
      collectRuns(source, bodyStart, index, hits)
      index += 1
      continue
    }
    if (char === '`') {
      // A raw literal spans lines by design, so it ends at the next backtick and nowhere else.
      const end = source.indexOf('`', index + 1)
      if (end === -1) {
        desync = 'raw string literal never closed'
        break
      }
      collectRuns(source, index + 1, end, hits)
      index = end + 1
      continue
    }
    if (char === "'") {
      // A rune literal holds one code point, so it can never hold the run. It is lexed only so a
      // quote character inside one cannot be mistaken for the start of a string.
      index += 1
      let closed = false
      while (index < source.length) {
        if (source[index] === '\\') {
          index += 2
          continue
        }
        if (source[index] === '\n') break
        if (source[index] === "'") {
          closed = true
          break
        }
        index += 1
      }
      if (!closed) {
        desync = 'rune literal never closed on its line'
        break
      }
      index += 1
      continue
    }
    index += 1
  }

  return desync === null ? { hits, desync: null } : { hits: [], desync }
}

const IDENTIFIER_START = /[A-Za-z_$]/
const IDENTIFIER_PART = /[A-Za-z0-9_$]/
const DIGIT = /[0-9]/
const NUMBER_PART = /[0-9A-Za-z_.]/
const REGEX_FLAG = /[a-z]/i
// A `<` opening a JSX element is followed by a tag name, a fragment's `>`, or a closing slash.
const JSX_TAG_START = /[A-Za-z_$>]/

// Words after which a `/` opens a regex rather than dividing, and after which a `<` opens a JSX
// element rather than comparing. Everything else that can end an expression — identifier, number,
// string, `)`, `]`, `}` — makes the `/` division and the `<` a comparison or a type argument, which
// is what keeps `Array<string>` and `useState<Foo>(x)` out of the JSX walk. Same reading, and the
// same safe direction, as scripts/check-prose-pin-whitespace.mjs.
const EXPRESSION_KEYWORDS = new Set([
  'await',
  'case',
  'default',
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

/**
 * True when the `<` at `index` opens a TYPE PARAMETER LIST rather than a JSX element.
 *
 * In a .tsx file a generic arrow function must be written `<T,>() => …` — the trailing comma is
 * there precisely because the parser cannot otherwise tell it from an element either. That comma is
 * what this reads: a JSX opening tag can never have one where an attribute name belongs. `extends`
 * is accepted for the same reason, since `<T extends X>(…)` is the other spelling. Everything else
 * that could confuse the two is already excluded upstream, where a `<` after a value token is a
 * comparison or a type argument (`Promise<T>`, `useState<Foo>(x)`) and never opens an element.
 */
function looksLikeTypeParameterList(source, index) {
  let at = index + 1
  while (at < source.length && IDENTIFIER_PART.test(source[at])) at += 1
  if (at === index + 1) return false
  while (at < source.length && /\s/.test(source[at])) at += 1
  if (source[at] === ',') return true
  return source.startsWith('extends ', at)
}

/**
 * Scan a regex literal starting at `start` (the opening slash). Returns the index just past the
 * flags, or -1 when the candidate is not a regex after all. A regex may not contain a raw line
 * terminator, so hitting one means the slash was division and the caller rewinds.
 */
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
      index += 1
      while (index < source.length && REGEX_FLAG.test(source[index])) index += 1
      return index
    }
    index += 1
  }

  return -1
}

/**
 * Scan one TypeScript or TSX source for forbidden runs inside string literals, template text, JSX
 * attribute strings and JSX text nodes.
 *
 * Returns `{ hits, desync }` with the same fail-closed contract as scanGo.
 *
 * JSX IS LEXED, NOT PATTERN-MATCHED, and it has to be. JSX text is not code: an apostrophe in
 * `Codex accounts can't be added` is a letter, and a code-mode lexer reads it as the start of a
 * string that never closes. Three files in services/web/src did exactly that to an earlier draft of
 * this gate. So the walk carries a small stack of frames — template, `${…}` or `{…}` expression,
 * open tag, element children — and each mode reads only what that mode can contain. The stack must
 * end empty; a walk that ends inside anything is reported as a desync and fails the file, rather
 * than reporting offsets it can no longer place.
 */
export function scanTypeScript(source, options = {}) {
  const jsx = options.jsx === true
  const hits = []
  let desync = null
  let index = 0
  // 'value' when the previous significant token can end an expression; anything else means a `/`
  // may open a regex and a `<` may open a JSX element.
  let previous = 'none'
  const stack = []
  let mode = 'code'

  const top = () => (stack.length > 0 ? stack[stack.length - 1] : null)
  const modeForTop = () => {
    const frame = top()
    if (frame === null) return 'code'
    if (frame.type === 'template') return 'template'
    if (frame.type === 'jsxTag') return 'jsxTag'
    if (frame.type === 'jsxChildren') return 'jsxChildren'
    return 'code'
  }

  // Leave the frame the walk is currently inside and resume in whatever encloses it.
  //
  // Written once because these three statements are an INVARIANT rather than a coincidence: every
  // site that departs a frame must pop it, restore `mode` from the new top, AND mark `previous`
  // value-like, since what the walk just finished reading — a closed template, a self-closed or
  // closed JSX element — is a completed expression. Four sites depart a frame, and repeating the
  // triple at each is how one of them ends up missing a line. That omission does not fail loudly:
  // it mislexes the rest of the file, and the desync check below only notices the subset that also
  // leaves the stack non-empty, so the rest surfaces as findings reported against the wrong
  // offsets — or as no findings at all.
  //
  // NOT for the `>` case that ends an opening JSX tag. That site REPLACES its frame (pop, then
  // push `jsxChildren`) rather than departing one, and sets `mode` from the frame it pushes; it is
  // deliberately not a caller here, and folding it in would drop the pushed frame.
  const leaveFrame = () => {
    stack.pop()
    mode = modeForTop()
    previous = 'value'
  }

  while (index < source.length && desync === null) {
    // ---- template literal text -------------------------------------------------
    if (mode === 'template') {
      const textStart = index
      let closed = false
      while (index < source.length) {
        if (source[index] === '\\') {
          index += 2
          continue
        }
        if (source[index] === '`') {
          closed = true
          break
        }
        if (source[index] === '$' && source[index + 1] === '{') break
        index += 1
      }
      collectRuns(source, textStart, Math.min(index, source.length), hits)
      if (closed) {
        index += 1
        leaveFrame()
        continue
      }
      if (index >= source.length) {
        desync = 'template literal never closed'
        break
      }
      stack.push({ type: 'expr', braceDepth: 0 })
      mode = 'code'
      previous = 'none'
      index += 2
      continue
    }

    // ---- inside an opening tag: `<Foo bar="x" baz={y}` -------------------------
    if (mode === 'jsxTag') {
      const char = source[index]
      if (char === '"' || char === "'") {
        const quote = char
        const bodyStart = index + 1
        index += 1
        let closed = false
        while (index < source.length) {
          if (source[index] === quote) {
            closed = true
            break
          }
          index += 1
        }
        if (!closed) {
          desync = 'JSX attribute string never closed'
          break
        }
        collectRuns(source, bodyStart, index, hits)
        index += 1
        continue
      }
      if (char === '{') {
        stack.push({ type: 'expr', braceDepth: 0 })
        mode = 'code'
        previous = 'none'
        index += 1
        continue
      }
      if (char === '/' && source[index + 1] === '>') {
        leaveFrame()
        index += 2
        continue
      }
      if (char === '>') {
        stack.pop()
        stack.push({ type: 'jsxChildren' })
        mode = 'jsxChildren'
        index += 1
        continue
      }
      index += 1
      continue
    }

    // ---- between an opening and a closing tag: the copy itself -----------------
    if (mode === 'jsxChildren') {
      const textStart = index
      while (index < source.length && source[index] !== '<' && source[index] !== '{') index += 1
      collectRuns(source, textStart, Math.min(index, source.length), hits)
      if (index >= source.length) {
        desync = 'JSX element never closed'
        break
      }
      if (source[index] === '{') {
        stack.push({ type: 'expr', braceDepth: 0 })
        mode = 'code'
        previous = 'none'
        index += 1
        continue
      }
      if (source[index + 1] === '/') {
        const end = source.indexOf('>', index + 2)
        if (end === -1) {
          desync = 'JSX closing tag never terminated'
          break
        }
        leaveFrame()
        index = end + 1
        continue
      }
      stack.push({ type: 'jsxTag' })
      mode = 'jsxTag'
      index += 1
      continue
    }

    // ---- code ------------------------------------------------------------------
    const char = source[index]

    if (char === '\n' || char === ' ' || char === '\t' || char === '\r') {
      index += 1
      continue
    }
    if (char === '/' && source[index + 1] === '/') {
      while (index < source.length && source[index] !== '\n') index += 1
      continue
    }
    if (char === '/' && source[index + 1] === '*') {
      const end = source.indexOf('*/', index + 2)
      if (end === -1) {
        desync = 'block comment never closed'
        break
      }
      index = end + 2
      continue
    }
    if (char === '"' || char === "'") {
      const quote = char
      const bodyStart = index + 1
      index += 1
      let closed = false
      while (index < source.length) {
        if (source[index] === '\\') {
          index += 2
          continue
        }
        if (source[index] === '\n') break
        if (source[index] === quote) {
          closed = true
          break
        }
        index += 1
      }
      if (!closed) {
        desync = 'string literal never closed on its line'
        break
      }
      collectRuns(source, bodyStart, index, hits)
      index += 1
      previous = 'value'
      continue
    }
    if (char === '`') {
      stack.push({ type: 'template' })
      mode = 'template'
      index += 1
      continue
    }
    if (char === '{') {
      const frame = top()
      if (frame !== null && frame.type === 'expr') frame.braceDepth += 1
      previous = 'op'
      index += 1
      continue
    }
    if (char === '}') {
      const frame = top()
      if (frame !== null && frame.type === 'expr') {
        if (frame.braceDepth === 0) {
          leaveFrame()
          index += 1
          continue
        }
        frame.braceDepth -= 1
      }
      previous = 'value'
      index += 1
      continue
    }
    if (IDENTIFIER_START.test(char)) {
      const start = index
      while (index < source.length && IDENTIFIER_PART.test(source[index])) index += 1
      previous = EXPRESSION_KEYWORDS.has(source.slice(start, index)) ? 'keyword' : 'value'
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
      const end = scanRegexLiteral(source, index)
      if (end === -1) {
        index += 1
        previous = 'op'
        continue
      }
      // A regex source is a pattern, not copy: skipped without scanning, so a pattern that MATCHES
      // the run is not itself a use of it.
      index = end
      previous = 'value'
      continue
    }
    if (
      jsx &&
      char === '<' &&
      previous !== 'value' &&
      JSX_TAG_START.test(source[index + 1] ?? '') &&
      !looksLikeTypeParameterList(source, index)
    ) {
      stack.push({ type: 'jsxTag' })
      mode = 'jsxTag'
      index += 1
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

  if (desync === null && stack.length > 0) {
    desync = `walk ended inside an unclosed ${top().type} construct`
  }
  if (desync !== null) return { hits: [], desync }

  const unique = Array.from(new Set(hits))
  unique.sort((a, b) => a - b)
  return { hits: unique, desync: null }
}

/** Dispatch on file kind: 'go', 'ts', or 'tsx'. */
export function scanSource(source, kind) {
  if (kind === 'go') return scanGo(source)
  return scanTypeScript(source, { jsx: kind === 'tsx' })
}

/**
 * The offending LINES of one source, as `{ line, text }` with a 1-based line and the trimmed source
 * line. Two runs on one line are one finding: the marker and the exemption both address a line, so
 * reporting a line twice would ask an author to excuse it twice.
 */
/**
 * Split `source` into lines AND the absolute offset each one starts at.
 *
 * Both halves have to come from the same walk of the real bytes. Deriving the offsets from a
 * `split(/\r?\n/)` instead loses exactly the character the split consumed: it drops `\r\n` (two
 * characters) and then advances the running offset by one, so on a CRLF file every line start
 * after the first drifts by one more character than the last. Hits are absolute offsets into the
 * unmodified source, so the drift lands on the two things the gate is read through — the reported
 * `file:line` and, because the caller resolves the inline marker with `lines[finding.line - 1]`,
 * whether an opt-out is seen at all. Measured on a 14-line CRLF fixture: the offender reported at
 * line 15 with empty text, and a marker on the line above it stopped suppressing anything, which
 * is the worst shape a gate can fail in — a contributor writes the documented opt-out and the gate
 * keeps rejecting, with no way to tell why.
 *
 * The trailing `\r` is stripped from the text so a finding reads identically under either line
 * ending, while the offsets stay true to the bytes.
 */
function splitLines(source) {
  const lines = []
  const lineStarts = []
  let cursor = 0
  for (;;) {
    const newline = source.indexOf('\n', cursor)
    const end = newline === -1 ? source.length : newline
    lineStarts.push(cursor)
    lines.push(source.slice(cursor, end).replace(/\r$/, ''))
    if (newline === -1) break
    cursor = newline + 1
  }
  return { lines, lineStarts }
}

export function scanFile(source, kind) {
  const { hits, desync } = scanSource(source, kind)
  if (desync !== null) return { findings: [], desync }

  const { lines, lineStarts } = splitLines(source)

  const seen = new Set()
  const findings = []
  for (const hit of hits) {
    let low = 0
    let high = lineStarts.length - 1
    while (low < high) {
      const mid = Math.ceil((low + high) / 2)
      if (lineStarts[mid] <= hit) low = mid
      else high = mid - 1
    }
    const line = low + 1
    if (seen.has(line)) continue
    seen.add(line)
    findings.push({ line, text: (lines[low] ?? '').trim() })
  }

  return { findings, desync: null }
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

/**
 * Every scannable file under the declared trees, plus the coverage problems found on the way.
 *
 * Returns `{ files, problems }`. `files` are `{ relative, absolute, kind }` in sorted order.
 * `problems` names a missing tree, a tree covering no scannable file, or a file whose extension is
 * in neither the scan set nor the ignore set.
 */
export function discoverScanFiles(root = REPO_ROOT) {
  const files = []
  const problems = []
  const ignoredExtensions = new Set(IGNORED_EXTENSIONS)
  const ignoredDirectories = new Set(IGNORED_DIRECTORIES)

  for (const tree of SCAN_TREES) {
    const treeRoot = path.join(root, tree.path)
    if (!fs.existsSync(treeRoot)) {
      problems.push(
        `${tree.path}: declared scan tree is missing, so this gate reads none of it; restore it or remove it from SCAN_TREES`,
      )
      continue
    }
    const scanned = new Set(tree.extensions)
    const found = []
    const stack = [treeRoot]
    while (stack.length > 0) {
      const directory = stack.pop()
      let entries
      try {
        entries = fs.readdirSync(directory, { withFileTypes: true })
      } catch (error) {
        problems.push(
          `${path.relative(root, directory)}: directory unreadable (${error && error.code ? error.code : 'unknown error'})`,
        )
        continue
      }
      for (const entry of entries) {
        const absolute = path.join(directory, entry.name)
        if (entry.isDirectory()) {
          if (ignoredDirectories.has(entry.name)) continue
          stack.push(absolute)
          continue
        }
        // Everything that is not a directory is a candidate, symlinks included. Dropping a
        // non-regular entry here would let a dangling symlink named like a scanned file vanish
        // silently; letting it reach readFileSync makes it an unreadable file, which fails.
        const relative = path.relative(root, absolute).split(path.sep).join('/')
        const extension = path.extname(entry.name)
        if (scanned.has(extension)) {
          found.push({ relative, absolute, kind: extension === '.go' ? 'go' : extension.slice(1) })
          continue
        }
        if (ignoredExtensions.has(extension)) continue
        // Neither scanned nor ignored. Silence here is the one answer this gate must not give: a
        // new rendered-copy file type would be walked past forever and the green line would still
        // claim the tree was covered.
        problems.push(
          `${relative}: extension ${extension || '(none)'} is in neither the scan set nor the ignore set, so this gate does not know whether it holds copy; add it to SCAN_TREES or to IGNORED_EXTENSIONS`,
        )
      }
    }
    if (found.length === 0) {
      problems.push(
        `${tree.path}: scan tree holds no ${tree.extensions.join('/')} file, so the entry covers nothing; restore them or remove it from SCAN_TREES`,
      )
      continue
    }
    for (const file of found) files.push(file)
  }

  files.sort((a, b) => (a.relative < b.relative ? -1 : a.relative > b.relative ? 1 : 0))
  return { files, problems }
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

export function checkEllipsisConsistency(options = {}) {
  const root = options.root === undefined ? REPO_ROOT : options.root
  const exemptions = options.exemptions === undefined ? EXEMPTIONS : options.exemptions

  const { files, problems } = discoverScanFiles(root)

  // Narrowing tripwire. This gate's success state is "found nothing", which is byte-identical to a
  // run that LOOKED at nothing: a moved directory or a mistyped root leaves the file set empty and
  // prints a green line forever. Reported before the per-tree problems so the headline says what
  // actually happened.
  if (files.length === 0) {
    console.error('This gate scanned no files at all, so its clean result means nothing:')
    for (const problem of problems) console.error(problem)
    console.error(
      'Fix SCAN_TREES in scripts/check-ellipsis-consistency.mjs so it stops passing without ' +
        'reading anything.',
    )
    return false
  }

  if (problems.length > 0) {
    console.error(
      'This gate could not account for everything in its scan trees, so it cannot pass:',
    )
    for (const problem of problems) console.error(problem)
    return false
  }

  const misses = []
  const skipped = []
  const usedExemptions = new Set()
  let marked = 0
  let scanned = 0

  for (const file of files) {
    let source
    try {
      source = fs.readFileSync(file.absolute, 'utf8')
    } catch (error) {
      skipped.push(
        `${file.relative}: unreadable (${error && error.code ? error.code : 'unknown error'})`,
      )
      continue
    }

    const { findings, desync } = scanFile(source, file.kind)
    if (desync !== null) {
      skipped.push(`${file.relative}: ${desync}`)
      continue
    }

    scanned += 1
    // The SAME splitter scanFile numbered these findings with. Resolving the marker against a
    // second, differently-derived line list is how an opt-out ends up read off the wrong line.
    const { lines } = splitLines(source)
    for (const finding of findings) {
      const own = lines[finding.line - 1] ?? ''
      const above = finding.line > 1 ? (lines[finding.line - 2] ?? '') : ''
      if (OPT_OUT_MARKER.test(own) || OPT_OUT_MARKER.test(above)) {
        marked += 1
        continue
      }
      const exemption = exemptions.find(
        (entry) =>
          entry.path === file.relative && (entry.text === null || own.includes(entry.text)),
      )
      if (exemption) {
        usedExemptions.add(`${exemption.path}:${exemption.text === null ? '*' : exemption.text}`)
        marked += 1
        continue
      }
      misses.push(`${file.relative}:${finding.line}: ${finding.text}`)
    }
  }

  // A file the gate could not read or could not lex is a file it cannot vouch for, and the skip is
  // silent in the one direction that matters: an offender inside it is never reported, so exiting 0
  // here would be the exact false green this gate exists to remove. CI reads the exit code, not the
  // summary line, so saying so on stdout does not discharge it.
  if (skipped.length > 0) {
    console.error('This gate could not check every file in its set, so it cannot pass:')
    for (const entry of skipped) console.error(entry)
    console.error(
      'A skipped file is unchecked, not clean. Extend the lexer in ' +
        'scripts/check-ellipsis-consistency.mjs, or rephrase the construct it could not follow.',
    )
    if (misses.length > 0) {
      console.error(
        'String literals elide with three full stops instead of the ellipsis character:',
      )
      for (const miss of misses) console.error(miss)
    }
    return false
  }

  // Defensive, and — as the code stands — unreachable: the empty-set guard above already returned
  // when nothing was discovered, and every path out of the loop either scans or skips. Kept, and
  // said plainly to be dead, so a future loop path that neither scans nor skips fails CLOSED rather
  // than printing a green line over nothing.
  if (scanned === 0) {
    console.error(
      `${files.length} file(s) were discovered but none were scanned, so this gate checked nothing.`,
    )
    return false
  }

  const stale = []
  for (const entry of exemptions) {
    const key = `${entry.path}:${entry.text === null ? '*' : entry.text}`
    if (!usedExemptions.has(key)) {
      stale.push(
        `${entry.path}: exemption for ${entry.text === null ? 'the whole file' : JSON.stringify(entry.text)} no longer matches anything; delete it`,
      )
    }
  }

  if (misses.length > 0 || stale.length > 0) {
    if (misses.length > 0) {
      console.error(
        'String literals elide with three full stops instead of the ellipsis character:',
      )
      for (const miss of misses) console.error(miss)
      console.error(
        `Use the single character ${ELLIPSIS} (U+2026). It is one cell wide where the three-stop ` +
          'spelling is three, so the two do not even truncate to the same place.',
      )
      console.error(
        'If the string is a grammar rather than copy — a git revision range, a variadic usage ' +
          'line, output from someone else’s program — mark it `ellipsis: literal-dots ok` on ' +
          'that line or the line above it, or add it to EXEMPTIONS with a reason.',
      )
    }
    for (const entry of stale) console.error(entry)
    console.error('See the header of scripts/check-ellipsis-consistency.mjs for the rule.')
    return false
  }

  console.log(`Ellipsis consistency OK (${scanned} file(s) scanned, ${marked} marked exception(s))`)
  return true
}

if (isMainModule(import.meta.url)) {
  // `--root <dir>` exists so this gate's own test can run this file as a real subprocess against a
  // synthetic tree and assert the EXIT CODE, not just the returned boolean. Asserting the return
  // value alone would still pass if this block forgot to exit non-zero — which is precisely the
  // always-green gate the ticket exists to prevent.
  //
  // `--no-exemptions` drops the reviewed list for those synthetic trees, which contain none of the
  // real survivors and would otherwise trip the stale-exemption ratchet. It only ever makes the
  // gate STRICTER, so it cannot hide a defect from the tests.
  const rootFlag = process.argv.indexOf('--root')
  const root = rootFlag === -1 ? REPO_ROOT : process.argv[rootFlag + 1]
  // A missing value and a following FLAG are the same mistake: `--root --no-exemptions` would
  // otherwise scan a directory named `--no-exemptions`, find no trees, and report the coverage
  // failures as if the repository were broken. Say what is actually wrong instead.
  if (rootFlag !== -1 && (!root || root.startsWith('--'))) {
    process.stderr.write('--root requires a directory argument\n')
    process.exit(2)
  }
  const options = {}
  if (rootFlag !== -1) options.root = root
  if (process.argv.includes('--no-exemptions')) options.exemptions = []
  if (!checkEllipsisConsistency(options)) process.exit(1)
}
