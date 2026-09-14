#!/usr/bin/env node

// Guardrail: a test that asserts an upper bound on measured wall-clock time against a bare numeric
// literal goes red when the host is loaded, not when the code is wrong. Eleven recorded incidents
// (BOS-1252) share that shape: parallel Bazel, `-race`, or a shared CI runner stretches a correct
// implementation past a hardcoded limit and burns an autonomous run on a false negative.
//
// The rule has two mechanically decidable discriminators:
//
//   Direction   — only a fail-when-too-slow bound is load-fragile. Load only ever increases elapsed
//                 time, so it can flip such a bound red on its own. A fail-when-too-fast bound
//                 ("did not return early") is pushed further into passing by load.
//   Derivation  — only a bare literal is a defect. A bound written as a multiple of the budget the
//                 test itself handed the code under test (`elapsed > 2*modalBudget`) moves with that
//                 budget and states its own headroom. What the gate decides mechanically is the
//                 weaker "the limit names an identifier"; that the identifier names a real budget
//                 rather than a renamed literal is a reviewer's call, as with the annotation reason.
//
// The check is textual because the rule is a property of the written assertion, not of runtime
// behaviour: a runtime check would have to reproduce the load that causes the flake.
//
// Fail-closed (BOS-1252 Requirement 2): an input the gate cannot decide is a violation, never a
// silent skip. A walked file whose extension has no analyzer, a comparison operand it cannot
// classify as either literal or derived, and a `timing-bound:` annotation with an empty reason each
// report with file, line and reason.
//
// The escape hatch is a named excuse, never a class exemption: `// timing-bound: <reason>` on the
// asserting line or the line above it, where the reason names the external resource whose latency
// the bound limits. The gate enforces non-emptiness; reviewers enforce the rest.
//
// Exercised by scripts/check-timing-bounds.test.mjs and runnable via
// `node scripts/check-timing-bounds.mjs [root...]`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const scriptDir = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(scriptDir, '..')

const DEFAULT_ROOTS = [repoRoot]

const SKIP_DIRS = new Set(['.git', 'node_modules', '.claude', 'dist', 'build', 'bin'])

// The corpus BOS-1252 scopes the gate to: every `**/*_test.go` and every `**/*.test.mjs`.
export function defaultIsTestSource(basename) {
  return basename.endsWith('_test.go') || basename.endsWith('.test.mjs')
}

const DURATION_UNITS = new Set([
  'Nanosecond',
  'Microsecond',
  'Millisecond',
  'Second',
  'Minute',
  'Hour',
])

const ANNOTATION_RE = /\/\/\s*timing-bound:(?<reason>.*)$/

function relativePath(file) {
  return path.relative(repoRoot, file).split(path.sep).join('/')
}

// Fail-closed (BOS-1252 Requirement 2): a directory the gate cannot list is an input it cannot
// decide, not an empty one. Swallowing the error let `node check-timing-bounds.mjs <typo>` print
// `OK: no bare-literal...` and exit 0 having looked at nothing at all. The roots are either the
// repo root (always present) or paths a caller named explicitly, so nothing in the tree passes an
// optional path here and this cannot become a permanent false red; SKIP_DIRS already excludes the
// generated trees, and it is consulted before the recursion so a skipped directory is never read.
function walk(dir, isTestSource, files, problems) {
  let entries
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true })
  } catch (error) {
    problems.push({ dir, error })
    return
  }
  for (const entry of entries) {
    const full = path.join(dir, entry.name)
    if (entry.isDirectory()) {
      if (SKIP_DIRS.has(entry.name)) continue
      walk(full, isTestSource, files, problems)
    } else if (entry.isFile() && isTestSource(entry.name)) {
      files.push(full)
    }
  }
}

// Blank out line comments, block comments and string/rune literals so that `elapsed >` occurring
// only in prose or in a failure message is not mistaken for an assertion. Column positions are
// preserved so reported line numbers stay exact.
export function stripCommentsAndStrings(contents) {
  const out = new Array(contents.length)
  let index = 0
  let quote = null
  let escaped = false
  let comment = null
  while (index < contents.length) {
    const ch = contents[index]
    const next = contents[index + 1]
    const keep = ch === '\n' ? '\n' : ' '
    if (comment === 'line') {
      out[index] = keep
      if (ch === '\n') comment = null
      index += 1
      continue
    }
    if (comment === 'block') {
      out[index] = keep
      if (ch === '*' && next === '/') {
        out[index + 1] = ' '
        index += 2
        comment = null
        continue
      }
      index += 1
      continue
    }
    if (quote) {
      out[index] = keep
      if (quote !== '`' && escaped) {
        escaped = false
      } else if (quote !== '`' && ch === '\\') {
        escaped = true
      } else if (ch === quote) {
        quote = null
      }
      index += 1
      continue
    }
    if (ch === '/' && next === '/') {
      comment = 'line'
      out[index] = ' '
      index += 1
      continue
    }
    if (ch === '/' && next === '*') {
      comment = 'block'
      out[index] = ' '
      index += 1
      continue
    }
    if (ch === '"' || ch === "'" || ch === '`') {
      quote = ch
      out[index] = ' '
      index += 1
      continue
    }
    out[index] = ch
    index += 1
  }
  return out.join('')
}

const OPEN = { '(': ')', '[': ']', '{': '}' }
const CLOSE = new Set([')', ']', '}'])

// Read one comparison operand starting at `start`, stopping at a top-level stop character or at a
// closer that would unbalance the expression (the `)` of the enclosing `assert.ok(`, for example).
function readOperand(text, start, stops) {
  const stack = []
  let index = start
  for (; index < text.length; index += 1) {
    const ch = text[index]
    if (stack.length === 0 && stops.includes(ch)) {
      // A reversed-operand comparison whose right side gofmt or prettier wrapped onto the next
      // line (`if time.Second <\n\telapsed {`) used to read an EMPTY operand here, and an operand
      // that looks non-elapsed on both sides dropped the comparison outright — a fail-when-too-slow
      // bound with no violation at all. Cross a newline that has yielded nothing but whitespace.
      if (ch !== '\n' || text.slice(start, index).trim() !== '') break
      continue
    }
    if (OPEN[ch]) {
      stack.push(OPEN[ch])
      continue
    }
    if (CLOSE.has(ch)) {
      if (stack.length === 0) break
      stack.pop()
      continue
    }
    if (stack.length > 0) continue
    if ((ch === '&' && text[index + 1] === '&') || (ch === '|' && text[index + 1] === '|')) break
  }
  return { text: text.slice(start, index), end: index }
}

function isElapsedExpression(expr) {
  const trimmed = expr.trim()
  if (/^time\.Since\s*\(/.test(trimmed)) return true
  const tail = trimmed.split('.').pop() ?? ''
  return /elapsed|duration|took/i.test(tail) && /^[A-Za-z_][A-Za-z0-9_]*$/.test(tail)
}

// Classify a comparison limit as `literal`, `derived`, or `unclassifiable`. A limit built only from
// numbers, arithmetic and `time.<Unit>` states nothing about its own headroom and is the defect
// shape; a limit mentioning any other identifier moves with whatever that identifier names.
//
// Scope of the check, stated exactly because the gate must not claim more than it decides: this is
// "the limit NAMES something", not "the limit is derived from the budget the test handed the code
// under test". A bare rename (`const slowBound = 2*time.Second; ... elapsed > slowBound`) passes
// here while remaining exactly as load-fragile as the literal it replaced. Whether the identifier
// names a real budget is a reviewer's judgement, like the annotation's reason — the gate removes
// the mechanically decidable half of the class and says so.
export function classifyLimit(expr, { language }) {
  const text = expr.trim()
  if (text === '') return { kind: 'unclassifiable', detail: 'empty comparison operand' }

  // `time.Duration(<number>)` is a literal wearing a conversion; normalise it before tokenising.
  const normalised = text.replace(/\btime\.Duration\s*\(\s*[0-9][0-9_]*\s*\)/g, '0')

  const tokenRe =
    /\s+|(?<ident>[A-Za-z_][A-Za-z0-9_]*)|(?<number>[0-9][0-9_]*(?:\.[0-9_]+)?)|(?<punct>[-+*/%().])/y
  const identifiers = []
  let index = 0
  while (index < normalised.length) {
    tokenRe.lastIndex = index
    const match = tokenRe.exec(normalised)
    if (!match) {
      return {
        kind: 'unclassifiable',
        detail: `cannot classify limit ${JSON.stringify(text)} (unexpected ${JSON.stringify(normalised[index])})`,
      }
    }
    if (match.groups.ident) identifiers.push({ name: match.groups.ident, at: index })
    index = tokenRe.lastIndex
  }

  // `time.Second` and friends name a unit, not a budget, so a limit built only from them is still a
  // bare literal. Any other identifier names something the limit moves with, which is the target form.
  const meaningful = identifiers.filter((token, position) => {
    if (language !== 'go') return true
    if (token.name === 'time' && DURATION_UNITS.has(identifiers[position + 1]?.name)) return false
    if (DURATION_UNITS.has(token.name) && identifiers[position - 1]?.name === 'time') return false
    return true
  })

  if (meaningful.length === 0) return { kind: 'literal' }
  return { kind: 'derived' }
}

// An assertion helper's argument is the SUCCESS condition (`assert.ok(elapsed < L)` fails when the
// run was too slow); an `if`/`for`/`while` condition is the FAILURE condition (`if elapsed > L {
// t.Fatal(...) }` fails when it was too slow). The two invert each other, so the direction is a
// property of the FORM the comparison sits in, never of the file's language: a JS
// `if (elapsed > 5000) throw` is a failure condition just as a Go one is, and a JS
// `rows.filter((r) => r.duration < 5)` is neither and asserts nothing about elapsed time.
const ASSERTION_CALLEE_RE =
  /(?:^|[^A-Za-z0-9_$.])(?:assert|expect|require|should|must|invariant)[A-Za-z0-9_$]*(?:\.[A-Za-z0-9_$]+)*$/i
const CONDITION_KEYWORD_RE = /(?:^|[^A-Za-z0-9_$.])(?:if|for|while|switch)$/

// The innermost construct the comparison at `opStart` sits inside: `assertion` (the argument of an
// assertion helper), `condition` (an if/for/while head), or `none`.
export function assertionForm(source, opStart) {
  const closers = []
  let index = opStart - 1
  for (; index >= 0; index -= 1) {
    const ch = source[index]
    if (ch === ')' || ch === ']' || ch === '}') {
      closers.push(ch)
      continue
    }
    if (ch === '(' || ch === '[' || ch === '{') {
      if (closers.length > 0) {
        closers.pop()
        continue
      }
      if (ch !== '(') break
      const callee = source.slice(0, index).replace(/\s+$/, '')
      if (CONDITION_KEYWORD_RE.test(callee)) return 'condition'
      if (ASSERTION_CALLEE_RE.test(callee)) return 'assertion'
      // A bare grouping paren `(` carries no callee of its own; keep looking outward.
      if (/[A-Za-z0-9_$)\]]$/.test(callee)) return 'none'
      index = callee.length
      continue
    }
  }
  // No enclosing call: Go's `if cond {` and `for cond {` have no parentheses, and gofmt keeps the
  // head on the comparison's own line.
  const lineStart = source.lastIndexOf('\n', opStart - 1) + 1
  if (/(?:^|[^A-Za-z0-9_$.])(?:if|for|while|switch)\b/.test(source.slice(lineStart, opStart)))
    return 'condition'
  return 'none'
}

function failsWhenSlow({ form, elapsedOnLeft, operator }) {
  if (form === 'none') return false
  const elapsedAboveLimit = elapsedOnLeft
    ? operator === '>' || operator === '>='
    : operator === '<' || operator === '<='
  return form === 'condition' ? elapsedAboveLimit : !elapsedAboveLimit
}

function findComparisons(source, { language }) {
  const found = []
  const operatorRe = /(<=|>=|<|>)/g
  let match
  while ((match = operatorRe.exec(source)) !== null) {
    const operator = match[1]
    const opStart = match.index
    // Skip `<-`, `->`, `<<`, `>>` and `=` comparisons that are not relational.
    if (
      source[opStart + operator.length] === '-' ||
      source[opStart + operator.length] === operator[0]
    )
      continue
    if (source[opStart - 1] === '<' || source[opStart - 1] === '>') continue

    const lineStart = source.lastIndexOf('\n', opStart - 1) + 1
    const before = source.slice(lineStart, opStart)
    // The left side is read as a whole trailing arithmetic expression, not as its last token: a
    // REVERSED bound (`3_000 > elapsed`, `timeoutMs * 3 > elapsed`) puts the LIMIT here, and a
    // single token would report `timeoutMs * 3` as the bare literal `3` it happens to end with.
    const ATOM = String.raw`[A-Za-z_][A-Za-z0-9_.]*|[0-9][0-9_]*(?:\.[0-9_]+)?`
    const leftMatch = before.match(
      new RegExp(
        String.raw`(time\.Since\s*\([^)]*\)|(?:${ATOM})(?:\s*[*/+%-]\s*(?:${ATOM}))*)\s*$`,
      ),
    )
    const right = readOperand(source, opStart + operator.length, [',', ';', '{', '\n'])

    const leftText = leftMatch ? leftMatch[1] : ''
    const leftIsElapsed = isElapsedExpression(leftText)
    const rightIsElapsed = isElapsedExpression(right.text)
    if (leftIsElapsed === rightIsElapsed) continue

    found.push({
      operator,
      elapsedOnLeft: leftIsElapsed,
      limit: leftIsElapsed ? right.text : leftText,
      index: opStart,
      form: assertionForm(source, opStart),
      language,
    })
  }
  return found
}

function lineNumberAt(contents, index) {
  let line = 1
  for (let i = 0; i < index; i += 1) {
    if (contents.charCodeAt(i) === 10) line += 1
  }
  return line
}

function annotationReasonNear(rawLines, line) {
  for (const candidate of [line, line - 1]) {
    const text = rawLines[candidate - 1]
    if (text === undefined) continue
    const match = text.match(ANNOTATION_RE)
    if (match) return match.groups.reason.trim()
  }
  return null
}

export function analyzeSource({ file, contents, language }) {
  const violations = []
  const rawLines = contents.split('\n')

  for (const [offset, text] of rawLines.entries()) {
    const match = text.match(ANNOTATION_RE)
    if (match && match.groups.reason.trim() === '') {
      violations.push({
        kind: 'empty-annotation',
        file,
        line: offset + 1,
        message: `${file}:${offset + 1} a timing-bound annotation must carry a non-empty reason naming the external resource whose latency the bound limits`,
      })
    }
  }

  const source = stripCommentsAndStrings(contents)
  for (const comparison of findComparisons(source, { language })) {
    if (!failsWhenSlow(comparison)) continue
    const line = lineNumberAt(contents, comparison.index)
    const verdict = classifyLimit(comparison.limit, { language })
    if (verdict.kind === 'derived') continue
    if (annotationReasonNear(rawLines, line)) continue
    if (verdict.kind === 'unclassifiable') {
      violations.push({
        kind: 'unclassifiable-operand',
        file,
        line,
        message: `${file}:${line} ${verdict.detail}; the gate cannot tell a literal from a derived bound here`,
      })
      continue
    }
    violations.push({
      kind: 'bare-literal-bound',
      file,
      line,
      message: `${file}:${line} fail-when-too-slow bound against the bare literal ${comparison.limit.trim()}; load alone can flip it red and its headroom is unstated`,
    })
  }

  return violations
}

export const ANALYZERS = new Map([
  ['.go', (input) => analyzeSource({ ...input, language: 'go' })],
  ['.mjs', (input) => analyzeSource({ ...input, language: 'js' })],
])

export function scanTimingBounds({
  roots = DEFAULT_ROOTS,
  isTestSource = defaultIsTestSource,
  analyzers = ANALYZERS,
} = {}) {
  const files = []
  const problems = []
  for (const root of roots) walk(root, isTestSource, files, problems)

  const violations = problems.map(({ dir, error }) => {
    const rel = relativePath(dir)
    return {
      kind: 'unreadable-directory',
      file: rel,
      line: 0,
      message: `${rel}:0 cannot be listed (${error.code ?? error.message}); the gate fails closed rather than reporting a clean scan of an input it never read`,
    }
  })
  for (const file of files.sort()) {
    const rel = relativePath(file)
    const analyze = analyzers.get(path.extname(file))
    if (!analyze) {
      violations.push({
        kind: 'unrecognised-file',
        file: rel,
        line: 0,
        message: `${rel}:0 walked as a test source but no analyzer is registered for ${path.extname(file) || '(no extension)'}; the gate fails closed rather than skipping it`,
      })
      continue
    }
    violations.push(...analyze({ file: rel, contents: fs.readFileSync(file, 'utf8') }))
  }
  return violations
}

export function checkTimingBounds(options = {}) {
  const violations = scanTimingBounds(options)
  if (violations.length > 0) {
    console.error('Found wall-clock bounds that a loaded host can flip red on its own:')
    for (const violation of violations) console.error(`  - ${violation.message}`)
    console.error(
      'Remedy: derive the bound from a named budget already in scope (`elapsed > 2*modalBudget`) — naming a constant that is itself a bare literal satisfies this gate but not a reviewer — or, where no in-scope budget exists, annotate the site with `// timing-bound: <reason naming the external resource>`.',
    )
    return false
  }
  console.log('OK: no bare-literal fail-when-too-slow wall-clock bounds in test sources')
  return true
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  const roots = process.argv.slice(2)
  if (!checkTimingBounds({ roots: roots.length > 0 ? roots : DEFAULT_ROOTS })) process.exit(1)
}
