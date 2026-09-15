#!/usr/bin/env node

// check-prose-pins — a test may assert what a HELPER DOES; it may not assert what a DOCUMENT SAYS.
// This gate refuses a NEW assertion of the second kind in a skill test file.
//
// THE DEFECT (BOS-1210, child of epic BOS-1206). Once a regex in a skill test matches a clause of
// skill markdown, that clause is load-bearing to a test: deleting it reds a test usually named
// after the incident that added it, so the cheapest correct action is always retention — and
// retention compounds. The skill bodies grow because their tests will not let them shrink. The
// discriminating rule is the one at the top of this file, and it is structural rather than
// stylistic: an assertion over a helper's OUTPUT survives a rewrite of the prose around it, an
// assertion over the prose does not.
//
// WHAT THIS GATE DOES NOT DO. It does not convert the existing population — 1947 pins over 2080
// assertion sites, the per-file split banked in PROSE_PIN_BASELINE below and the verdicts recorded
// in docs/skills/prose-pins.md. (PINS, not SITES: 2080 is every `assert.match` /
// `assert.doesNotMatch` in scope, and 133 of those are not pins.) Converting
// a pin is bulk work that belongs with the prose it pins, in the rewrite children of the epic.
// This gate stops the INFLOW, which is the mechanism change.
//
// ---------------------------------------------------------------------------------------------
// THE RULE, MECHANICALLY
//
// A PROSE PIN is an `assert.match(` / `assert.doesNotMatch(` call whose FIRST ARGUMENT mentions a
// MARKDOWN-BOUND identifier.
//
// An identifier is MARKDOWN-BOUND when it is bound in that file to something derived from markdown.
// Computed parser-free, as a transitive closure over `const`/`let` bindings, `for (const X of …)`
// bindings, and `function X(…) {…}` declarations:
//
//   seed:    the binding's initialiser text mentions a `.md` path literal — `'SKILL.md'`,
//            `` `${CORE}/SKILL.md` ``, `'references/finalize-and-stop.md'`. That is what a skill
//            test does to get a document: it reads one off disk by name.
//   closure: the binding's initialiser mentions an identifier that is ALREADY markdown-bound.
//
// The closure is what makes the rule usable rather than decorative. Measured over this repo's
// scope, the single commonest assertion subject is not the document variable but a REGION of it —
// `const step5 = region(skill, '## Step 5', '## Step 6')`, `const section = sectionRegion(reviewStack,
// heading)`, `const flat = skill.replace(/\s+/g, ' ')`. A rule keyed on the initialiser's LEADING
// identifier would bind `skill` and miss every one of those, because the leading identifier there is
// the region helper, not the document. So the closure asks whether the initialiser MENTIONS a bound
// name, in any position. A helper defined in the file is caught by the same closure without being
// special-cased: `const finalizeAndStop = (dir) => fs.readFileSync(path.join(rootDir, dir, REF))`
// mentions `REF`, which the seed bound off `'references/finalize-and-stop.md'`.
//
// THE EXECUTABLE ASSERTION IS THE POINT, AND IS NOT FLAGGED. An assertion that spawns a helper and
// reads its output tests behaviour, and behaviour is what a pin should become. Any binding whose
// initialiser mentions a subprocess callee (SPAWNER_CALLEES below) is therefore NOT markdown-bound,
// however the document reached it — the skill path handed to the child process is markdown, but the
// stdout that comes back is a verdict. This exclusion is deliberately generous: it is keyed on a
// MENTION anywhere in the initialiser rather than on the callee position, because a false flag on
// an executable assertion would push authors away from the exact shape this gate exists to reward.
// The cost is recorded in RESIDUAL.
//
// An assertion over a non-markdown artifact — a `.mjs` source, a `.go` file, JSON — is not flagged
// either, and needs no exclusion: those bindings are never markdown-bound in the first place.
//
// ---------------------------------------------------------------------------------------------
// "NEW", NOT "ANY" — the per-file exact-count ratchet
//
// The gate compares each file's prose-pin count against PROSE_PIN_BASELINE for EQUALITY, following
// `assertExactSize` in scripts/size-ratchet-lib.mjs rather than a one-sided bound. Equality is not
// pedantry: a one-sided ceiling drifts upward invisibly, because a genuine deletion widens the gap
// instead of clearing it, and the next author spends the slack without anyone deciding to. With
// equality, a deletion must be banked in the constant, which is what makes the number trustworthy.
//
// A skill test file ABSENT from the baseline has an implied baseline of 0, so a brand-new skill
// test file may add no prose pins at all. That is the intended asymmetry: new tests are written
// against helpers.
//
// THERE IS NO INLINE OPT-OUT MARKER, and that is a decision rather than an omission. Every sibling
// gate in this directory ships one (`size-ratchet-ok:`, `prose-pin: literal-space ok`) because
// their rules have known false-positive shapes. This rule's "false positive" IS the defect: a
// comment marker would let a new pin land inside the same commit that adds it, which is precisely
// the inflow being stopped. The escape hatch is the visible one — bump the file's number in
// PROSE_PIN_BASELINE, in the same commit, where a reviewer sees a diff line that says a pin was
// added and can ask why a helper would not do.
//
// See docs/skills/prose-pins.md for the policy, the per-site inventory, and the accepted trade:
// some skill prose WILL drift out of date, and that is the deal this gate signs on purpose.
//
// Exercised by scripts/check-prose-pins.test.mjs and runnable via
// `node scripts/check-prose-pins.mjs`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

// The repo root relative to this file, so the gate works from any cwd — from the repo root via
// `node scripts/check-prose-pins.mjs`, and from `scripts/` via the scripts Makefile.
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// The policy this gate enforces. Named once, printed on every failure, so a red run teaches the
// rule instead of only refusing the commit.
export const POLICY_DOC = 'docs/skills/prose-pins.md'

// The single source of truth for what this gate reads — a glob, never a hand-list, so a newly
// added skill test file is covered without editing this file. Shares its spelling with
// scripts/check-prose-pin-whitespace.mjs deliberately: the two gates police the same population
// from different angles, and a divergence between their scopes would be invisible in both.
export const GATE_FILE_GLOB = 'scripts/*skill*.test.mjs'

// A binding whose initialiser mentions any of these is NOT markdown-bound: what comes back is a
// child process's output, which is behaviour. `spawn`/`exec` are listed for completeness even
// though an async spawn is not an assertion subject in this scope today.
export const SPAWNER_CALLEES = [
  'execFileSync',
  'execSync',
  'spawnSync',
  'execFile',
  'exec',
  'spawn',
]

// A path literal ending `.md`. Anchored on the extension rather than on a directory, so a
// reference doc, a mirror under .codex, and a plan doc all seed alike.
const MARKDOWN_LITERAL = /\.md\b/

const IDENTIFIER = /[A-Za-z_$][A-Za-z0-9_$]*/g

// Words after which a `/` OPENS a regex rather than dividing. Without them `return /…/` reads as
// division and the regex body is never masked, so its brackets can unbalance the initialiser walk
// and the identifiers inside it become false references. Kept BYTE-IDENTICAL to the set in
// scripts/check-prose-pin-whitespace.mjs: the two gates run the same lexer over the same file set,
// and the BOS-1210 review found this exact entry present there and absent here — a divergence that
// was invisible in both because nothing compares them.
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

// RESIDUAL — what a green run here does NOT establish, stated in the gate rather than left for a
// reader to discover:
//
//   1. It does not prove the EXISTING pins are sound. They are counted, not judged; the per-site
//      verdicts live in docs/skills/prose-pins.md and the rewrite children act on them.
//   2. It does not see a pin whose subject reaches the assertion through a FUNCTION PARAMETER —
//      `const check = (body) => assert.match(body, /…/)` called with markdown. The closure walks
//      bindings, not call graphs. A pin written that way is uncounted and unrefused.
//   3. The SPAWNER_CALLEES exclusion is keyed on a mention anywhere in the initialiser, so a
//      binding that both spawns a helper AND slices markdown is treated as executable. That is the
//      safe direction for this rule — a wrongly flagged executable assertion teaches the opposite
//      of the lesson — but it is a hole a determined author can walk through.
//   4. It counts SITES, not sentences. One `assert.match` can pin a paragraph and another can pin a
//      single word; the ratchet cannot tell them apart, so a swap of one for the other is silent.
//   5. It is scoped to GATE_FILE_GLOB. A prose pin written in a test file whose name does not
//      contain "skill" is not scanned at all.
//   6. The SPAWNER_CALLEES exclusion is keyed on a NAME across the whole file, not on a scope. One
//      binding named `block` that mentions a spawner excludes EVERY `block` in the file, including
//      a `block` that is a slice of markdown in an unrelated test. Measured in the BOS-1210 review:
//      correcting the scanner made ten spawning helpers visible in boss-build-skill.test.mjs and
//      that alone moved 27 sites out of the count. An author who wants a pin unrefused can reuse a
//      spawning helper's variable name on purpose.
//   7. The binding walk is REGEX-SHAPED, so a declaration form it does not spell is invisible
//      rather than refused. It reads `const`/`let`/`var` declarators (identifier and destructuring
//      pattern alike, including wrapped initialisers), `for (… of …)` bindings and `function`
//      declarations. It does NOT read a declare-then-assign (`let skill` on one line and
//      `skill = fs.readFileSync('SKILL.md')` on another), a `for (… in …)` binding, a class field,
//      or an assignment to a property. That is the same class of hole as (2): unseen, not allowed.
export const RESIDUAL =
  'a green run means no skill test file gained a prose pin beyond its banked baseline — not ' +
  'that the existing pins are sound, not that a pin reaching an assertion through a function ' +
  'parameter or a declare-then-assign was seen, not that a binding which both spawns a helper and ' +
  'slices markdown was counted, not that a same-named spawning binding elsewhere in the file did ' +
  'not exclude the subject, not that the pinned sentences did not change size, and not that a ' +
  'prose pin outside ' +
  GATE_FILE_GLOB +
  ' exists at all'

// ---------------------------------------------------------------------------------------------
// Masking: everything that is not code becomes spaces
//
// The scan below counts brackets and matches identifiers, and both go wrong inside a string, a
// comment, or a regex literal — `/\)/` alone would unbalance the walk. So the source is first
// rewritten to an equal-length "masked" copy in which every string body, template text, comment
// body, and regex body is replaced by spaces, newlines preserved so line numbers still map 1:1.
// Offsets into the mask are offsets into the original, which is what lets the `.md` seed read the
// ORIGINAL slice (the path literal lives inside a string) while every structural test reads the
// mask. A `${…}` interpolation stays code, because the identifiers inside it are real references.
export function maskNonCode(source) {
  const out = source.split('')
  const blank = (from, to) => {
    for (let i = from; i < to && i < out.length; i += 1) {
      if (out[i] !== '\n') out[i] = ' '
    }
  }

  // Template-literal frames: 'template' while inside a literal's text, `{depth}` while inside one
  // of its `${…}` expressions.
  const stack = []
  let index = 0
  // 'value' when the previous significant token can end an expression, so a following `/` divides
  // rather than opening a regex. Same reading — and the same safe-direction false negative — as
  // scripts/check-prose-pin-whitespace.mjs.
  let previous = 'none'

  while (index < source.length) {
    const char = source[index]
    const inTemplateText = stack.length > 0 && stack[stack.length - 1] === 'template'

    if (inTemplateText) {
      if (char === '\\') {
        blank(index, index + 2)
        index += 2
        continue
      }
      if (char === '$' && source[index + 1] === '{') {
        stack.push({ depth: 0 })
        index += 2
        previous = 'none'
        continue
      }
      if (char === '`') {
        stack.pop()
        index += 1
        previous = 'value'
        continue
      }
      blank(index, index + 1)
      index += 1
      continue
    }

    if (char === '/' && source[index + 1] === '/') {
      const end = source.indexOf('\n', index)
      const stop = end === -1 ? source.length : end
      blank(index, stop)
      index = stop
      continue
    }
    if (char === '/' && source[index + 1] === '*') {
      const end = source.indexOf('*/', index + 2)
      const stop = end === -1 ? source.length : end + 2
      blank(index, stop)
      index = stop
      continue
    }
    if (char === '"' || char === "'") {
      const quote = char
      let cursor = index + 1
      while (cursor < source.length) {
        if (source[cursor] === '\\') {
          cursor += 2
          continue
        }
        if (source[cursor] === quote || source[cursor] === '\n') break
        cursor += 1
      }
      blank(index, Math.min(cursor + 1, source.length))
      index = Math.min(cursor + 1, source.length)
      previous = 'value'
      continue
    }
    if (char === '`') {
      stack.push('template')
      blank(index, index + 1)
      index += 1
      continue
    }
    if (char === '{') {
      const top = stack[stack.length - 1]
      if (top && top !== 'template') top.depth += 1
      index += 1
      previous = 'op'
      continue
    }
    if (char === '}') {
      const top = stack[stack.length - 1]
      if (top && top !== 'template') {
        if (top.depth === 0) {
          stack.pop()
          index += 1
          previous = 'value'
          continue
        }
        top.depth -= 1
      }
      index += 1
      previous = 'value'
      continue
    }
    if (char === '/' && previous !== 'value') {
      const end = scanRegexLiteral(source, index)
      if (end !== -1) {
        blank(index + 1, end)
        index = end + 1
        while (index < source.length && /[a-z]/i.test(source[index])) index += 1
        previous = 'value'
        continue
      }
      index += 1
      previous = 'op'
      continue
    }
    if (/[A-Za-z_$]/.test(char)) {
      const start = index
      while (index < source.length && /[A-Za-z0-9_$]/.test(source[index])) index += 1
      previous = REGEX_PRECEDING_KEYWORDS.has(source.slice(start, index)) ? 'keyword' : 'value'
      continue
    }
    if (/[0-9)\]]/.test(char)) {
      previous = 'value'
      index += 1
      continue
    }
    if (/\s/.test(char)) {
      index += 1
      continue
    }
    previous = 'op'
    index += 1
  }

  return out.join('')
}

// The index of a regex literal's closing `/`, or -1 when the `/` at `start` did not open one. A
// regex may not contain a raw line terminator, so hitting one means it was division.
function scanRegexLiteral(source, start) {
  let index = start + 1
  let inClass = false
  while (index < source.length) {
    const char = source[index]
    if (char === '\n') return -1
    if (char === '\\') {
      index += 2
      continue
    }
    if (inClass) {
      if (char === ']') inClass = false
      index += 1
      continue
    }
    if (char === '[') {
      inClass = true
      index += 1
      continue
    }
    if (char === '/') return index
    index += 1
  }
  return -1
}

// A code character that cannot END an expression, so a line break after it continues the
// initialiser. `>` is here for the `=>` of a wrapped arrow; `.` and `?` for a chain broken across
// lines.
const CONTINUES_AFTER = new Set([...'=<>+-*/%&|^.?:('])
// A code character that cannot BEGIN an expression, so a line break before it continues the
// initialiser — a leading `.method()` on the next line, a leading `?` of a ternary.
const CONTINUES_BEFORE = new Set([...'=<>+-*/%&|^.?:'])

// The index just past a balanced run starting at `from`, stopping at a `;`/`,` at bracket depth 0
// or at the first newline at depth 0 that does not continue an expression. Operates on masked text
// for structure, so brackets inside strings and regexes cannot unbalance it.
//
// WHY A NEWLINE IS NOT ALWAYS A TERMINATOR (BOS-1210 review). Reading the first newline as the end
// treats an initialiser that BEGINS on the next line as EMPTY, and prettier wraps at this repo's
// print width constantly:
//
//   const body =
//     fs.readFileSync('SKILL.md', 'utf8')
//   const finalizeAndStop = (dir = CORE) =>
//     fs.readFileSync(path.join(rootDir, dir, FINALIZE_REF), 'utf8')
//
// Both seeded nothing, so `finalizeAndStop` was not markdown-bound and every assertion over its
// output went uncounted and unrefused. The continuation test therefore reads SIGNIFICANCE from the
// ORIGINAL source and OPERATOR IDENTITY from the mask: a position the mask blanked (a string body,
// a template's text, a comment) is significant enough to stop the scan but can never be read as a
// dangling operator, so a multi-line template in an initialiser ends the run instead of swallowing
// the statements after it.
function endOfInitialiser(masked, source, from) {
  let depth = 0
  let index = from
  let seen = false
  while (index < masked.length) {
    const char = masked[index]
    if (char === '(' || char === '[' || char === '{') {
      depth += 1
      seen = true
    } else if (char === ')' || char === ']' || char === '}') {
      if (depth === 0) return index
      depth -= 1
      seen = true
    } else if (depth === 0 && (char === ';' || char === ',')) return index
    else if (char === '\n') {
      if (depth === 0 && seen && !continuesAcross(masked, source, index)) return index
    } else if (!/\s/.test(source[index])) seen = true
    index += 1
  }
  return masked.length
}

// Whether the expression spanning the newline at `index` runs on: the last significant character
// before it cannot end an expression, or the first significant character after it cannot begin
// one. Whitespace is judged on `source` so a blanked string body is never skipped over.
function continuesAcross(masked, source, index) {
  let before = index - 1
  while (before >= 0 && /[ \t\r]/.test(source[before])) before -= 1
  if (before >= 0 && CONTINUES_AFTER.has(masked[before])) return true
  let after = index + 1
  while (after < source.length && /\s/.test(source[after])) after += 1
  return after < source.length && CONTINUES_BEFORE.has(masked[after])
}

// The index just past a `{ … }` block whose opening brace is at or after `from`, or -1.
function endOfBlock(masked, from) {
  const open = masked.indexOf('{', from)
  if (open === -1) return -1
  let depth = 0
  for (let index = open; index < masked.length; index += 1) {
    if (masked[index] === '{') depth += 1
    else if (masked[index] === '}') {
      depth -= 1
      if (depth === 0) return index + 1
    }
  }
  return -1
}

// Every identifier referenced in `masked.slice(from, to)`, skipping property accesses (`a.skill`
// is not a reference to `skill`).
function referencedNames(masked, from, to) {
  const text = masked.slice(from, to)
  const names = new Set()
  IDENTIFIER.lastIndex = 0
  for (const match of text.matchAll(IDENTIFIER)) {
    if (match.index > 0 && text[match.index - 1] === '.') continue
    names.add(match[0])
  }
  return names
}

// The index just past the bracket opened at `open`, or -1 when it never closes.
function endOfBracket(masked, open) {
  const close = { '{': '}', '[': ']', '(': ')' }[masked[open]]
  let depth = 0
  for (let index = open; index < masked.length; index += 1) {
    if (masked[index] === masked[open]) depth += 1
    else if (masked[index] === close) {
      depth -= 1
      if (depth === 0) return index
    }
  }
  return -1
}

// The offsets of top-level `,` separators inside `text`, ignoring nested brackets.
function topLevelSplit(text) {
  const parts = []
  let depth = 0
  let start = 0
  for (let index = 0; index < text.length; index += 1) {
    const char = text[index]
    if (char === '(' || char === '[' || char === '{') depth += 1
    else if (char === ')' || char === ']' || char === '}') depth -= 1
    else if (char === ',' && depth === 0) {
      parts.push(text.slice(start, index))
      start = index + 1
    }
  }
  parts.push(text.slice(start))
  return parts
}

// The first top-level occurrence of `char` in `text`, or -1.
function topLevelIndexOf(text, char) {
  let depth = 0
  for (let index = 0; index < text.length; index += 1) {
    const current = text[index]
    if (current === '(' || current === '[' || current === '{') depth += 1
    else if (current === ')' || current === ']' || current === '}') depth -= 1
    else if (current === char && depth === 0) return index
  }
  return -1
}

/**
 * The identifiers a destructuring pattern BINDS, given the masked text including its brackets.
 *
 * `{ a, b: c, ...rest }` binds `a`, `c` and `rest` — `b` is a property NAME, not a binding — and
 * `[first, , second]` binds both elements. A `= default` inside an element is a REFERENCE rather
 * than a binding, so it is dropped. Nested patterns recurse.
 *
 * @param {string} pattern Masked pattern text, brackets included.
 * @param {Set<string>} names Accumulator.
 * @returns {Set<string>} Bound identifier names.
 */
function patternBindings(pattern, names = new Set()) {
  for (const element of topLevelSplit(pattern.slice(1, -1))) {
    let text = element.trim()
    if (text.startsWith('...')) text = text.slice(3).trim()
    const colon = topLevelIndexOf(text, ':')
    if (colon !== -1) text = text.slice(colon + 1).trim()
    const equals = text.search(/(?<![=!<>])=(?![=>])/)
    if (equals !== -1) text = text.slice(0, equals).trim()
    if (text.startsWith('{') || text.startsWith('[')) {
      patternBindings(text, names)
      continue
    }
    if (/^[A-Za-z_$][A-Za-z0-9_$]*$/.test(text)) names.add(text)
  }
  return names
}

const DECLARATOR_NAME = /[A-Za-z_$][A-Za-z0-9_$]*/y
const DECLARATOR_INITIALISER = /\s*(?:=(?![=>])|of\b)/y

// Every binding in one source, as `{ name, from, to }` over the initialiser (or function body).
// `const`/`let`/`var` declarations — including DESTRUCTURING patterns and every declarator in a
// comma-separated list — `for (const X of …)` bindings, and `function X(…) {…}`.
//
// The destructuring arm exists because of the BOS-1210 review: the source/codex-mirror idiom
// `for (const [label, skill] of [['source', SKILL], ['codex mirror', CODEX]])` bound NOTHING under
// a bare-identifier regex, and it is how 16 of the 23 scoped files reach their document.
function findBindings(masked, source) {
  const bindings = []

  const declaration = /(?:^|[^\w$])(?:const|let|var)\s+/g
  for (const match of masked.matchAll(declaration)) {
    let cursor = match.index + match[0].length
    // Each declarator in `const a = …, b = …`; the loop ends at the first shape it cannot read.
    while (cursor < masked.length) {
      let names
      const opener = masked[cursor]
      if (opener === '{' || opener === '[') {
        const close = endOfBracket(masked, cursor)
        if (close === -1) break
        names = [...patternBindings(masked.slice(cursor, close + 1))]
        cursor = close + 1
      } else {
        DECLARATOR_NAME.lastIndex = cursor
        const name = DECLARATOR_NAME.exec(masked)
        if (!name) break
        names = [name[0]]
        cursor = DECLARATOR_NAME.lastIndex
      }
      DECLARATOR_INITIALISER.lastIndex = cursor
      const initialiser = DECLARATOR_INITIALISER.exec(masked)
      if (!initialiser) break
      const from = DECLARATOR_INITIALISER.lastIndex
      const to = endOfInitialiser(masked, source, from)
      for (const name of names) bindings.push({ name, from, to })
      if (masked[to] !== ',') break
      cursor = to + 1
      while (cursor < masked.length && /\s/.test(masked[cursor])) cursor += 1
    }
  }

  const declared = /(?:^|[^\w$])function\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(/g
  for (const match of masked.matchAll(declared)) {
    const to = endOfBlock(masked, match.index + match[0].length)
    if (to === -1) continue
    bindings.push({ name: match[1], from: match.index + match[0].length, to })
  }

  return bindings
}

/**
 * The markdown-bound identifiers in one JavaScript source, as a Set of names.
 *
 * Seeded from bindings whose initialiser contains a `.md` path literal, then closed transitively
 * over bindings whose initialiser mentions an already-bound name. A binding whose initialiser
 * mentions a SPAWNER_CALLEES member is excluded at every round, so an executable assertion's
 * subject never enters the set.
 *
 * @param {string} source Whole file text.
 * @returns {Set<string>} Markdown-bound identifier names.
 */
export function findMarkdownBoundNames(source) {
  const masked = maskNonCode(source)
  const bindings = findBindings(masked, source)
  const spawners = new Set(SPAWNER_CALLEES)

  const enriched = bindings.map((binding) => ({
    name: binding.name,
    references: referencedNames(masked, binding.from, binding.to),
    // The `.md` test reads the ORIGINAL slice: the path literal lives inside a string, which the
    // mask has already blanked.
    seed: MARKDOWN_LITERAL.test(source.slice(binding.from, binding.to)),
  }))

  // EXECUTABLE FIRST, and it wins. A value that came out of a child process is a verdict, not a
  // document, however the document reached the process — so the executable closure is computed
  // first and subtracts from the markdown closure rather than competing with it. It propagates
  // for the same reason it exists: `const stale = runPushBlock(block)` reads a spawn result even
  // though `block` is a slice of markdown, and flagging that assertion would punish the exact
  // shape this gate wants authors to write.
  const executable = closeOver(enriched, (binding) =>
    [...binding.references].some((reference) => spawners.has(reference)),
  )

  const markdown = closeOver(
    enriched.filter((binding) => !executable.has(binding.name)),
    (binding) => binding.seed,
  )

  for (const name of executable) markdown.delete(name)
  return markdown
}

// The transitive closure of `bindings` under "the initialiser mentions an already-included name",
// seeded by `isSeed`. Bounded by the binding count, since each round either adds a name or stops.
function closeOver(bindings, isSeed) {
  const included = new Set()
  for (const binding of bindings) {
    if (isSeed(binding)) included.add(binding.name)
  }
  for (let round = 0; round <= bindings.length; round += 1) {
    let grew = false
    for (const binding of bindings) {
      if (included.has(binding.name)) continue
      if ([...binding.references].some((reference) => included.has(reference))) {
        included.add(binding.name)
        grew = true
      }
    }
    if (!grew) break
  }
  return included
}

/**
 * Every prose pin in one JavaScript source, as `{ line, subject }` with a 1-based line, sorted in
 * document order. `subject` is the first argument's text, squeezed to one line and truncated, so a
 * failure names the site rather than only its coordinates.
 *
 * @param {string} source Whole file text.
 * @returns {{line: number, subject: string}[]} Prose pins.
 */
export function findProsePins(source) {
  const masked = maskNonCode(source)
  const bound = findMarkdownBoundNames(source)
  if (bound.size === 0) return []

  const pins = []
  const assertion = /(?:^|[^\w$])assert\s*\.\s*(?:doesNotMatch|match)\s*\(/g
  for (const match of masked.matchAll(assertion)) {
    const from = match.index + match[0].length
    const to = endOfArgument(masked, from)
    const references = referencedNames(masked, from, to)
    if (![...references].some((reference) => bound.has(reference))) continue
    const line = masked.slice(0, match.index + match[0].length).split('\n').length
    const subject = source.slice(from, to).replace(/\s+/g, ' ').trim()
    pins.push({ line, subject: subject.length > 60 ? `${subject.slice(0, 57)}...` : subject })
  }

  return pins.sort((a, b) => a.line - b.line)
}

// The index just past the first argument that starts at `from`: the first `,` or `)` at depth 0.
function endOfArgument(masked, from) {
  let depth = 0
  let index = from
  while (index < masked.length) {
    const char = masked[index]
    if (char === '(' || char === '[' || char === '{') depth += 1
    else if (char === ')' || char === ']' || char === '}') {
      if (depth === 0) return index
      depth -= 1
    } else if (char === ',' && depth === 0) return index
    index += 1
  }
  return masked.length
}

// ---------------------------------------------------------------------------------------------
// THE BASELINE
//
// Measured at implementation time (BOS-1210) with `node scripts/check-prose-pins.mjs`, over the
// files GATE_FILE_GLOB expands to. Compared for EQUALITY: a file that gains a pin reds, and a file
// that LOSES one reds too until the saving is banked here. Banking a reduction is the point — it
// is what converts a deletion into a number nobody can spend again.
//
// A file absent from this map has an implied baseline of 0. Deleting a skill test file means
// deleting its entry in the same commit; a stale entry reds with `no longer matches`.
//
// TO ADD A PIN you must edit this map, in the same commit, and say why a helper would not do. See
// docs/skills/prose-pins.md.
//
// REASON FOR THE CURRENT NUMBERS: the population as it stood when the gate was introduced, banked
// wholesale so the gate could ship without a bulk rewrite, then RE-MEASURED in the BOS-1210 review
// after the scanner was corrected for wrapped initialisers and destructuring patterns (see
// endOfInitialiser and findBindings). Every later movement needs its own reason on the line that
// moves. 1947 pins across 22 files; the four largest are the artifacts the epic exists to shrink.
//
// WHY THE NUMBERS MOVED (BOS-1210 review, one correction, two directions):
//   UP, and this is the point — a binding written across a line break (`const body =\n  read(…)`,
//   `const helper = (dir) =>\n  read(…)`) and a destructured one (`for (const [label, skill] of
//   […])`, the source/codex-mirror idiom) were both read as binding NOTHING, so their assertions
//   were uncounted and unrefused. scripts/bs-sweep-plan-skill.test.mjs is the clearest case: 17
//   regexes over SKILL.md, an implied baseline of 0, and room to grow forever without reddening.
//   It now has an entry.
//   DOWN, in one file — boss-build 866 -> 839. The same correction made ten spawning HELPERS
//   visible for the first time (`const legSeconds = (raw) =>\n  Number(execFileSync(…))`), and the
//   executable exclusion is keyed on a NAME across the whole file, so those names now exclude
//   same-named subjects in unrelated tests. That is the exclusion's documented safe direction
//   widening, not a decision to stop counting those sites; it is disclosed in RESIDUAL.
export const PROSE_PIN_BASELINE = {
  // BOS-1215: 839 -> 830. The dispatch-snapshot PROTOCOL moved out of the resident body into
  // references/resume-assessment.md, which already owned its reading half; the body now states the
  // invariant (work committed before the run advances; unattributable residue stops the run) and
  // one remedy per failure mode. The BOS-519 test was retargeted rather than deleted — every
  // mechanic it protects is still pinned, at the file that now carries it — so the saving is the
  // body's own narration of the same procedure, not lost coverage. Two further sites: the
  // "snapshot-and-check procedure once per **dispatch**" pin, whose behaviour the retargeted test
  // asserts over the same region, and a reference pin whose prose was CORRECTED (it said "recover
  // it the way Step 5 does", which the move made false) and re-pointed at its true location.
  // 830 -> 835, two raises banked together: they landed on opposite sides of a rebase and the
  // file now carries BOTH sets of pins, so the count is the sum rather than either raise alone.
  // +3 with the derived-clean correction: the clean-write bullet now names the DERIVED verb rather
  // than the evidence-free one, its condition must state BOTH blockers (the must-fix half alone is
  // the rule that shipped a false green), and the classify block must route through the
  // disposition helper instead of re-lifting `payload.provisional` into a shell variable it then
  // consulted on one arm out of four. The counterpart negative — that the raw-payload read is gone
  // — is asserted as a doesNotMatch, which this gate does not count.
  // +2 for BOS-1251: the tag-state re-derivation moved onto the injector's own
  // non-empty-work-commit predicate, and the claim-liveness reference stopped declaring the CLI
  // transport unavoidably weaker. Both INVERTED prose the existing pins had made permanent, so
  // those pins were re-aimed at the corrected rule rather than deleted — a net zero. The +2 is the
  // pair with no helper to ask: the reference must name the three per-chat discriminators the
  // daemon computes, and it must name BOTH transports that carry them. No function's return value
  // encodes "this document tells a CLI-transport run to consult the per-chat signal"; the
  // executable half — that the grader agrees with the injector — is asserted over the real module
  // in the push-block gate, which is where the behaviour actually lives.
  'scripts/boss-build-skill.test.mjs': 835,
  // BOS-1243: 13 -> 14. The claim-adjudication pass gives an action per VERDICT but gave none
  // for the CLI's fourth outcome, exit 2 — an operator error where nothing was adjudicated at
  // all and an empty record list reads exactly like "nothing was refuted". The exit CODE itself
  // is asserted behaviourally in skills-toolbox/bs-dispatch-claims.test.mjs; what only the
  // document can carry is the ACTION it obliges, so one pin was added rather than two.
  'scripts/boss-repair-skill.test.mjs': 14,
  // BOS-1213 lowered this from 194: the four regexes quoting §Caller deadline's budget numbers
  // became a numeric comparison against the helper's own constants, plus executable
  // admit-fix-round cases covering every verdict it can return.
  'scripts/boss-review-skill.test.mjs': 193,
  'scripts/boss-skill.test.mjs': 2,
  // 149 -> 150: the wait-recipe pin split in two. It used to assert ONE sentence naming a
  // "session cron" as the fallback wait mechanism — the sentence that was itself teaching
  // drivers to register recurring cron jobs to monitor their children. It is now a pin on the
  // mechanism (`in-session scheduled wake-up`) plus a pin on the prohibition (`never `boss
  // cron``). The prohibition is the point of the change, so it is pinned separately: folding it
  // back into the mechanism pin would let a future rewrite drop the rail while staying green.
  'scripts/bs-epic-skill.test.mjs': 150,
  'scripts/bs-plan-ce-skill.test.mjs': 24,
  // BOS-1214: 308 -> 306. Two pins deleted with the byte mechanics they pinned — the
  // snapshot's trailing-newline shape and `renormalized bullets alone defeat byte-equality`.
  // The bullet-renormalization pin's behaviour is now asserted over the write-back verifier
  // (skills-toolbox/plan-writeback-verify.test.mjs). The trailing-newline pin's is not, and never
  // was: `--require-verbatim` is plan-image-guard, not the write-back verifier, and its
  // trailing-newline exactness is covered where it lives, in
  // skills-toolbox/plan-image-guard.test.mjs.
  // BOS-1214 (brief): 306 -> 305. The `command substitution strips trailing newline bytes`
  // pin went with the warning it pinned; the executable guard that the recipe never assigns
  // `descriptionSummary` through a capture is kept, because it asserts the recipe, not prose.
  // BOS-1254: 305 -> 308. The subject of that change IS the contract prose — the brief required
  // `descriptionSummary` inline while forbidding the drafter to return plan content — so the
  // sentences are the artifact under repair and a pin over them is the only available evidence
  // that the contradiction is gone. What CAN be asserted over a helper was: the by-reference
  // union, its fail-closed resolution, and the contract check over the resolved bytes are all
  // pinned in skills-toolbox/plan-run-guards.test.mjs, not here. The three kept here are the two
  // the guard cannot see — Step 7 instructing the reference and Step 9 documenting the union —
  // plus Phase 4's `cp` of the declared `description` artifact, which is a shell recipe rather
  // than prose: it asserts what the orchestrator RUNS, the way the retained command-substitution
  // guard below it does.
  // BOS-1245 (review): 317 -> 318. Step 5 mandated `verify <signed-url>` while naming no
  // operation that issues one, and described the only mode that does — `readPlanAttachment` in
  // `format="url"` — in terms that steer a reader away from it, so the primary recipe read as
  // unexecutable. The repaired prose IS the artifact under repair here: the operand's provenance
  // lives in a skill reference doc, and no helper can assert what that document says, so a pin is
  // the only available evidence that the source stays named. It pins the rule lead and the mode
  // it names rather than the sentence around them.
  // 318 -> 319, rebased onto the entry above: both raises landed independently on this same
  // count and BOTH stand. One pin added, one retargeted, net +1. The dispatch-failure abort
  // message was
  // asserted twice — once at the verifier that emits it and once at a prose restatement above the
  // fence — so the restatement was cut and its pin retargeted onto the emitter's own `${F}:`
  // template. The +1 is the new pin that the verifier is HANDED `$DISPATCH_FAILURE` as that
  // prefix, which is the half a template-only assertion cannot see: without it the emitter could
  // interpolate anything and still match.
  // 319 -> 321 (BOS-1255), chained onto the entry above. `reconcileEpicChildren` no longer refuses
  // unconditionally on an unmarked live child — the epic-child marker is a MEMBERSHIP verdict, so a
  // sub-issue somebody filed by hand no longer wedges the epic's resume forever — and the resident
  // outcome paragraph plus the drafting brief both described the old rule. Correcting them was
  // mandatory; the +2 is one pin per artifact asserting the discriminator that replaced it. A helper
  // genuinely cannot do this one: the artifact IS published prose an agent reads to decide whether
  // to CREATE children, so there is no behaviour to assert instead, and the already-banked
  // outcome-(3) pin next to each was deliberately LOOSENED (structural lead + rule, no enumeration)
  // in the same commit rather than re-tightened around the new sentence. Held to +2 by folding the
  // membership rule and its fail-closed truncation exception into ONE regex per artifact instead of
  // a pin apiece — they are a single fact, and prose stating either half alone misleads.
  // 321 -> 322 (BOS-1255 review round), chained onto the entry above. The outcome-(3) pin that the
  // entry above deliberately LOOSENED could no longer fail: its `[\s\S]*?` swallows the whole
  // parenthesised refusal set, so the pre-BOS-1255 wording it was loosened to permit still matched
  // it byte-for-byte, and the compensating discriminator pin sits in a different paragraph — a brief
  // stating the corrected rule in one place and the old unconditional refusal in the enumeration
  // satisfied both. The +1 buys a targeted negative that pins the RULE inside the list: capture the
  // refusal set and require an unmarked child named there to carry the `missing` qualifier. It is
  // still a prose pin because the subject is published prose an agent reads to decide whether to
  // CREATE children, so there is no behaviour to assert instead; it is deliberately a rule over a
  // captured region rather than a sentence, so it survives further edits to the list.
  'scripts/bs-plan-skill.test.mjs': 322,
  'scripts/bs-record-notes-skill.test.mjs': 2,
  'scripts/bs-sweep-debt-skill.test.mjs': 32,
  'scripts/bs-sweep-mutation-skill.test.mjs': 32,
  // 82 -> 87 (BOS-1253): the notes sweep's dry run previewed an agent-authored theme partition
  // while the body invited an operator to review one before scheduling the write run — a promise
  // the machinery cannot keep, since the partition is re-authored per run. The fix is prose in a
  // Markdown instruction body with no runtime and therefore no helper to assert against; the five
  // pins are the structural lead, the three clauses the acceptance criterion names, and the
  // absence of the prerequisite framing that was removed.
  'scripts/bs-sweep-notes-skill.test.mjs': 87,
  // 17 -> 27 (BOS-1253): the plan sweep's all-unprioritized branch named no discriminator and its
  // edge-case table restated the same unnamed judgement, and Phase 5 had no epic terminal outcome.
  // All three sites are instruction Markdown an agent loads at execution time — there is no helper
  // whose output could carry the rule instead. The pins key on the rule NAME
  // (`durable-corruption-first`), the outcome TOKEN (`planned epic <PARENT-ID>`) and the roster
  // fields, plus two absence pins that stop the replaced "most impactful" judgement from being
  // supplemented rather than removed.
  'scripts/bs-sweep-plan-skill.test.mjs': 27,
  'scripts/bs-sweep-prettify-skill.test.mjs': 7,
  'scripts/bs-sweep-releases-skill.test.mjs': 66,
  'scripts/bs-sweep-security-skill.test.mjs': 28,
  // 39 -> 45 (BOS-1253): the kill-set gate reference admitted `scripts/` and the docs module on
  // coverage-neutrality alone while naming an exact command only for Go and services/web, so a run
  // reaching either area invented its instrument. The six pins are table ROWS and the metric
  // keyword in references/kill-set-gate.md, asserted against both the source and the generated
  // codex mirror. The rules had to land in the reference rather than the body because
  // .claude/skills/bs-sweep-tests/SKILL.md sits exactly at its banked 26600-byte budget.
  'scripts/bs-sweep-tests-skill.test.mjs': 45,
  'scripts/check-skill-node-fences.test.mjs': 2,
  'scripts/check-skill-shell.test.mjs': 21,
  'scripts/check-skill-symbols.test.mjs': 8,
  'scripts/skill-extensions.test.mjs': 12,
  'scripts/skill-model-tier.test.mjs': 8,
  'scripts/sync-codex-skills.test.mjs': 70,
}

export function discoverGateFiles(repoRoot = REPO_ROOT) {
  if (typeof fs.globSync !== 'function') {
    // Loud, not silent: an older Node would otherwise throw a TypeError that reads as a broken gate
    // rather than as a wrong runtime. `.node-version` pins a Node where globSync exists.
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

/**
 * Compare every scanned file's prose-pin count against `baseline`.
 *
 * @param {string} repoRoot Repository root.
 * @param {Record<string, number>} baseline File (repo-relative, `/`-separated) to expected count.
 * @returns {{counts: Record<string, number>, growth: object[], shrink: object[], stale: string[]}}
 */
export function measureProsePins(repoRoot = REPO_ROOT, baseline = PROSE_PIN_BASELINE) {
  const counts = {}
  const growth = []
  const shrink = []

  for (const file of discoverGateFiles(repoRoot)) {
    const relative = path.relative(repoRoot, file).split(path.sep).join('/')
    const pins = findProsePins(fs.readFileSync(file, 'utf8'))
    counts[relative] = pins.length
    const expected = Object.hasOwn(baseline, relative) ? baseline[relative] : 0
    if (pins.length > expected) {
      growth.push({ file: relative, expected, actual: pins.length, excess: pins.slice(expected) })
    } else if (pins.length < expected) {
      shrink.push({ file: relative, expected, actual: pins.length })
    }
  }

  const stale = Object.keys(baseline).filter((relative) => !Object.hasOwn(counts, relative))
  return { counts, growth, shrink, stale }
}

export function checkProsePins(repoRoot = REPO_ROOT, baseline = PROSE_PIN_BASELINE) {
  const files = discoverGateFiles(repoRoot)

  // Narrowing tripwire. This gate's success state is "nothing over baseline", which is
  // byte-identical to a run that LOOKED at nothing: a renamed convention, a moved directory, or a
  // typo in the glob leaves the file set empty and prints a green line forever.
  if (files.length === 0) {
    console.error(`No files matched ${GATE_FILE_GLOB}, so this gate checked nothing.`)
    console.error(
      'Update GATE_FILE_GLOB in scripts/check-prose-pins.mjs so it stops passing without ' +
        'scanning anything.',
    )
    return false
  }

  const { counts, growth, shrink, stale } = measureProsePins(repoRoot, baseline)
  let failed = false

  if (growth.length > 0) {
    failed = true
    console.error(
      'New prose pins in skill test files. A test may assert what a HELPER DOES; it may not ' +
        'assert what a DOCUMENT SAYS — a regex over skill markdown makes that sentence permanent, ' +
        'because deleting it reds this suite:',
    )
    for (const entry of growth) {
      console.error(`  - ${entry.file}: ${entry.actual} prose pin(s), baseline ${entry.expected}`)
      // The count is EXACT; the named sites are the LAST `actual - expected` pins in document
      // order, which is where a newly added one usually sits but is not a proof that these are the
      // added ones. Said plainly so nobody reads the list as an accusation it cannot support.
      for (const pin of entry.excess) {
        console.error(`      ${entry.file}:${pin.line} [prose-pin] ${pin.subject}`)
      }
    }
    console.error(
      'The count above is exact. The sites named under each file are the last ones in document ' +
        'order, not necessarily the ones just added — read them as a starting point.',
    )
    console.error(
      'Assert what the helper returns instead. If the pin is genuinely the only available ' +
        `evidence, bank the new number in PROSE_PIN_BASELINE in scripts/check-prose-pins.mjs in ` +
        'the same commit, with the reason in the commit message. There is deliberately no inline ' +
        'opt-out marker.',
    )
  }

  if (shrink.length > 0) {
    failed = true
    console.error(
      'Fewer prose pins than the banked baseline. That is good news the gate cannot accept ' +
        'silently — an unbanked saving is slack the next author spends without deciding to:',
    )
    for (const entry of shrink) {
      console.error(
        `  - ${entry.file}: ${entry.actual} prose pin(s), baseline ${entry.expected} — ` +
          `lower PROSE_PIN_BASELINE to ${entry.actual}`,
      )
    }
  }

  if (stale.length > 0) {
    failed = true
    console.error(
      `PROSE_PIN_BASELINE names file(s) that no longer match ${GATE_FILE_GLOB}. Remove the ` +
        'entries so the baseline keeps describing the tree:',
    )
    for (const relative of stale) console.error(`  - ${relative}`)
  }

  if (failed) {
    console.error(`See ${POLICY_DOC} for the rule, the inventory, and the drift it accepts.`)
    return false
  }

  const total = Object.values(counts).reduce((sum, count) => sum + count, 0)
  // Qualified with the scope on purpose: an unqualified "OK" reads as a whole-tree verdict, and
  // this gate looked at a named subset at a banked number.
  console.log(
    `Prose pins at baseline (${files.length} file(s) matching ${GATE_FILE_GLOB}, ` +
      `${total} pin(s) banked). Not covered: ${RESIDUAL}.`,
  )
  return true
}

if (isMainModule(import.meta.url)) {
  if (!checkProsePins()) process.exit(1)
}
