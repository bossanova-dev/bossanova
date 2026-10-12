// scripts/verify-security-surface.mjs
//
// The boss-verify-security judge's deterministic half. One file is the surface scan, the
// applicability check, the verdict finalizer, and the park-comment writer, so any edit to the
// pattern table touches the declared `applies` script and therefore the verify policy surface.
//
//   node scripts/verify-security-surface.mjs            applies mode: verify envelope on stdin;
//                                                        exit 10 = not applicable (clean scan,
//                                                        zero signals), 0 = applies, 2 = error
//   ... scan --envelope <file> [--json]                 one JSON line: signals, injection, judge
//   ... verdict --envelope <file> [--judgments <file>]  one JSON line: the full verify result,
//                                                        only ever `pass` or `human`; exits 0
//   ... comment --pr <n> --head <sha> --result <file>   upserts one marked PR comment for a
//                                                        `human` result; never fails the run
//
// Diff content is untrusted data: nothing here executes or follows it. Only node builtins and the
// repo's entry guard are imported.

import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

export const EXTENSION_NAME = 'boss-verify-security'
export const HUMAN_REASON = 'human-review:security'
export const NOT_APPLICABLE_EXIT = 10
export const ERROR_EXIT = 2
export const MAX_SIGNALS = 40
export const MAX_DIFF_BYTES = 5 * 1024 * 1024
export const MAX_HUNK_LINES = 80
export const MAX_WHY = 300
export const MAX_NOTE = 200
export const COMMENT_MARKER = '<!-- boss-verify-security -->'
export const PROBLEM_KINDS = Object.freeze([
  'security-concern',
  'unjudged',
  'malformed-judgment',
  'injection-suspect',
  'over-budget',
  'judge-error',
])

const FULL_SHA = /^[0-9a-f]{40}$/i

// ---------------------------------------------------------------------------
// Path exclusions: docs, tests, generated mirrors and lockfiles never signal.

const EXCLUDED_PATHS = [
  /^docs\//,
  /\.md$/i,
  /_test\.go$/,
  /\.test\.[^/]+$/,
  /\.spec\.[^/]+$/,
  /(?:^|\/)testdata\//,
  /(?:^|\/)__tests__\//,
  /(?:^|\/)fixtures\//,
  /^\.codex\/skills\//,
  /\.pb\.go$/,
  /(?:^|\/)(?:go\.sum|go\.work\.sum|pnpm-lock\.yaml)$/,
]

export function isExcludedPath(file) {
  return typeof file !== 'string' || EXCLUDED_PATHS.some((re) => re.test(file))
}

// ---------------------------------------------------------------------------
// The pattern table. Each row is `{category, pathFilter?, pattern}` and is biased toward
// signalling: a false positive costs one short judge session, a miss costs a security review.

const GO = /\.go$/
const JS = /\.(?:[cm]?js|jsx|[cm]?ts|tsx)$/
const SHELL = /(?:\.(?:sh|bash|zsh|fish|ya?ml|mk)$|(?:^|\/)(?:Makefile|Dockerfile)[^/]*$)/

export const CATEGORIES = Object.freeze(['code-execution', 'auth', 'secrets', 'outbound-request'])

export const PATTERNS = Object.freeze([
  // code-execution
  {
    category: 'code-execution',
    pathFilter: GO,
    pattern: /\b(?:exec\.Command(?:Context)?|syscall\.Exec|os\.StartProcess|plugin\.Open)\s*\(/,
  },
  {
    category: 'code-execution',
    pattern:
      /(?:go\.starlark\.net|gopher-lua|\bgoja\b|robertkrimen\/otto|\byaegi\b|\bv8go\b|\bwazero\b|\bwasmtime\b|expr-lang\/expr|\bcel-go\b|["'`](?:text|html)\/template["'`])/,
  },
  { category: 'code-execution', pathFilter: JS, pattern: /\bchild_process\b/ },
  {
    category: 'code-execution',
    pathFilter: JS,
    pattern: /(?<![\w$])(?:execSync|execFile|execFileSync|spawn|spawnSync|execa)\s*\(/,
  },
  { category: 'code-execution', pathFilter: JS, pattern: /(?<![\w$.])(?:exec|fork)\s*\(/ },
  {
    category: 'code-execution',
    pathFilter: JS,
    pattern: /(?<![\w$.])eval\s*\(|\bnew\s+Function\s*\(/,
  },
  {
    category: 'code-execution',
    pathFilter: JS,
    pattern: /["'`](?:node:)?vm["'`]|\bvm2\b|isolated-vm/,
  },
  { category: 'code-execution', pathFilter: SHELL, pattern: /(?:^|[\s;&|(`])eval\s/ },
  {
    category: 'code-execution',
    pattern: /\b(?:ba|z)?sh\s+-c\b|["'`](?:ba|z)?sh["'`]\s*,\s*["'`]-c["'`]/,
  },

  // auth
  {
    category: 'auth',
    pattern:
      /\b(?:require\w*(?:Access|Owner|Editor|Admin|Member)\w*|ownerFor[A-Z]\w*|owned[A-Z]\w*|[Aa]uthori[sz]e\w*|Authorization)\b/,
  },
  { category: 'auth', pattern: /\b(?:jwt|oauth\w*|csrf|xsrf)\b/i },
  { category: 'auth', pattern: /Access-Control-Allow-|\bInsecureSkipVerify\b/ },
  {
    category: 'auth',
    pattern:
      /\b(?:GRANT|REVOKE)\s+[A-Z]|\bCREATE\s+ROLE\b|\b(?:CREATE|ALTER|DROP)\s+POLICY\b|\bROW\s+LEVEL\s+SECURITY\b/,
  },

  // secrets — a bare `token` is deliberately absent: claim tokens and LLM tokens are everywhere.
  { category: 'secrets', pattern: /\bkeyring\w*/i },
  {
    category: 'secrets',
    pattern: /secret|password|credential|private_?key|api_?key|signing_?key|\bhmac\b/i,
  },
  {
    category: 'secrets',
    pattern:
      /\b(?:Getenv|LookupEnv)\s*\(\s*["'`][^"'`]*(?:TOKEN|SECRET|KEY|PASSWORD)|process\.env(?:\.|\[\s*["'`])[A-Za-z0-9_]*(?:TOKEN|SECRET|KEY|PASSWORD)|\$\{?[A-Z0-9_]*(?:TOKEN|SECRET|KEY|PASSWORD)/,
  },

  // outbound-request — suppressed when the call target is a static literal (see lineSuppressed).
  {
    category: 'outbound-request',
    pathFilter: GO,
    pattern:
      /\b(?:http\.(?:Get|Post|Head|PostForm|NewRequest|NewRequestWithContext)|net\.Dial\w*|websocket\.Dial\w*|grpc\.(?:Dial|DialContext|NewClient))\s*\(/,
  },
  {
    category: 'outbound-request',
    pathFilter: JS,
    pattern: /(?<![\w$.])fetch\s*\(|\baxios\b|\bhttps?\.request\s*\(|\bnew\s+WebSocket\s*\(/,
  },
])

// An outbound call whose destination — the call's own first argument (after a Go context and HTTP
// method) — is a fully static string literal closed by `,` or `)`: an absolute http(s) URL, or a
// same-origin path. A literal carrying `${…}`, a printf verb, or any further expression is
// interpolated and still signals; any `%` in a URL counts as interpolation, and a path opening `//`
// or `/\` is another host, not same-origin. A static literal elsewhere on the line never suppresses.
const STATIC_BODY = String.raw`(?:(?!\1)[^$%\\\n]|\$(?!\{)|%(?![-+# 0-9.]*[svdqx])|\\.)*\1`
const URL_BODY = String.raw`(?:(?!\1)[^$%\\\n]|\$(?!\{)|\\.)*\1`
const CALL_TARGET = String.raw`^(?:\.\w+)?\s*\(?\s*(?:\w*[cC]tx\s*,\s*)?(?:(?:"[A-Z]+"|http\.Method[A-Za-z]+)\s*,\s*)?`
const STATIC_TARGET = new RegExp(
  CALL_TARGET +
    String.raw`(?:(["'\`])https?:\/\/` +
    URL_BODY +
    String.raw`|(["'\`])\/(?![\/\\])` +
    STATIC_BODY.replaceAll('\\1', '\\2') +
    ')\\s*[,)]',
)

function lineSuppressed(category, text, pattern) {
  if (category !== 'outbound-request') return false
  const calls = [...text.matchAll(new RegExp(pattern.source, pattern.flags.replace('g', '') + 'g'))]
  return (
    calls.length > 0 && calls.every((m) => STATIC_TARGET.test(text.slice(m.index + m[0].length)))
  )
}

// A changed file under a path segment named `auth` is an auth signal at its first added line.
const AUTH_SEGMENT = /(?:^|\/)auth\//

// ---------------------------------------------------------------------------
// Injection: added text inside a flagged hunk that addresses a reviewer or asserts a verdict.

const INJECTION_PATTERNS = [
  /\b(?:ignore|disregard|forget|override)\b.{0,40}\b(?:instructions?|previous|above|prior|rules)\b/i,
  /\breviewers?\b/i,
  /\bsecurity\s+judge\b/i,
  /\blanguage\s+models?\b/i,
  /\bLLMs?\b/,
  /\bassistant\b/i,
  // A bare "verdict" is domain vocabulary in verify code; only a verdict asserted toward a pass
  // (or an instruction to call something benign) is reviewer-addressed.
  /\bverdicts?\b.{0,30}\b(?:pass|passes|benign|safe|approve|approved)\b/i,
  /\b(?:mark|judge|rate|classify|return|answer|report)\b.{0,20}\b(?:benign|safe)\b/i,
  /\b(?:safe|benign|harmless|secure|not\s+(?:a\s+)?(?:risk|risky|security))\b.{0,40}\b(?:pass|passes|approve|approved|merge)\b/i,
  /\b(?:pass|approve|merge)\b.{0,40}\b(?:safe|benign|harmless)\b/i,
]

export function isInjectionShaped(text) {
  return typeof text === 'string' && INJECTION_PATTERNS.some((re) => re.test(text))
}

// ---------------------------------------------------------------------------
// Unified-diff parsing.

function unquotePath(raw) {
  // git appends one TAB after a name containing a space; real leading/trailing spaces are kept.
  const value = raw.replace(/\r$/, '').replace(/\t$/, '')
  if (!value.startsWith('"')) return value
  // git C-quotes a path with unusual bytes; undo the common escapes.
  const body = value.slice(1, -1)
  const bytes = []
  for (let i = 0; i < body.length; i++) {
    const ch = body[i]
    if (ch !== '\\') {
      bytes.push(...Buffer.from(ch, 'utf8'))
      continue
    }
    const next = body[++i]
    if (/[0-7]/.test(next)) {
      // gate-region-ok: a fixed three-digit octal escape, not a section window
      bytes.push(Number.parseInt(body.slice(i, i + 3), 8))
      i += 2
    } else
      bytes.push(
        { a: 7, b: 8, t: 9, n: 10, v: 11, f: 12, r: 13, '"': 34, '\\': 92 }[next] ??
          next.charCodeAt(0),
      )
  }
  return Buffer.from(bytes).toString('utf8')
}

function stripPrefix(raw) {
  const value = unquotePath(raw)
  return value.startsWith('b/') ? value.slice(2) : value
}

/**
 * Parses unified diff text into `[{file, hunks: [{header, lines: [{type, text, line}]}]}]`. `line` is
 * the new-file line number for `+` and context lines and null for removed lines. Deleted files
 * (`+++ /dev/null`) and binary files carry no hunks.
 */
export function parseDiff(diffText) {
  const files = []
  let file = null
  let hunk = null
  let newLine = 0
  for (const raw of String(diffText ?? '').split('\n')) {
    if (raw.startsWith('diff --git ')) {
      file = { file: null, hunks: [] }
      files.push(file)
      hunk = null
      continue
    }
    if (!file) continue
    if (hunk === null && raw.startsWith('+++ ')) {
      const target = raw.slice(4)
      file.file = target.replace(/\s+$/, '') === '/dev/null' ? null : stripPrefix(target)
      continue
    }
    const header = /^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(raw)
    if (header) {
      hunk = { header: raw, lines: [] }
      file.hunks.push(hunk)
      newLine = Number(header[1])
      continue
    }
    if (!hunk) continue
    const type = raw[0]
    if (type === '+') hunk.lines.push({ type: '+', text: raw.slice(1), line: newLine++ })
    else if (type === ' ') hunk.lines.push({ type: ' ', text: raw.slice(1), line: newLine++ })
    else if (type === '-') hunk.lines.push({ type: '-', text: raw.slice(1), line: null })
    else if (type === '\\') continue
    else hunk = null
  }
  return files.filter((entry) => entry.file !== null && entry.hunks.length > 0)
}

function hunkExcerpt(hunk) {
  const lines = [hunk.header, ...hunk.lines.map((l) => `${l.type}${l.text}`)]
  if (lines.length <= MAX_HUNK_LINES) return lines.join('\n')
  return [
    ...lines.slice(0, MAX_HUNK_LINES),
    `… (${lines.length - MAX_HUNK_LINES} more lines)`,
  ].join('\n')
}

// ---------------------------------------------------------------------------
// The pure scan.

/**
 * Scans diff text for security surface on added lines of non-excluded paths. `paths`, when given,
 * further limits the scan to those repo-relative paths. Returns
 * `{signals: [{id, category, file, line, excerpt, hunk}], injection: [{file, line}], judge, why?}`;
 * `judge` is false (`why: 'over-budget'`) above MAX_SIGNALS signals or MAX_DIFF_BYTES of diff.
 */
export function scanDiff(diffText, { paths } = {}) {
  const text = String(diffText ?? '')
  if (Buffer.byteLength(text, 'utf8') > MAX_DIFF_BYTES)
    return { signals: [], injection: [], judge: false, why: 'over-budget', diffTooLarge: true }
  const allowed = Array.isArray(paths) ? new Set(paths) : null
  const signals = []
  const injection = []
  const seen = new Set()
  for (const entry of parseDiff(text)) {
    // A parsed path that matches no requested path means the reader and parser disagree about
    // names (prefix config, quoting); skipping it would fail open, so it is an unreadable diff.
    if (allowed && !allowed.has(entry.file) && !isExcludedPath(entry.file))
      throw new Error(`diff path did not round-trip to a requested path: ${entry.file}`)
    if (isExcludedPath(entry.file)) continue
    const flaggedHunks = new Set()
    const add = (category, hunk, l) => {
      const id = `${category}:${entry.file}:${l.line}`
      if (seen.has(id)) return
      seen.add(id)
      flaggedHunks.add(hunk)
      signals.push({
        id,
        category,
        file: entry.file,
        line: l.line,
        // gate-region-ok: a length cap on one diff line, not a section window
        excerpt: l.text.slice(0, 400),
        hunk: hunkExcerpt(hunk),
      })
    }
    let firstAdded = null
    for (const hunk of entry.hunks)
      for (const l of hunk.lines) {
        if (l.type !== '+') continue
        if (firstAdded === null) firstAdded = { hunk, l }
        for (const category of CATEGORIES)
          for (const row of PATTERNS) {
            if (row.category !== category) continue
            if (row.pathFilter && !row.pathFilter.test(entry.file)) continue
            if (row.pattern.test(l.text) && !lineSuppressed(category, l.text, row.pattern)) {
              add(category, hunk, l)
              break
            }
          }
      }
    if (firstAdded && AUTH_SEGMENT.test(entry.file)) add('auth', firstAdded.hunk, firstAdded.l)
    for (const hunk of entry.hunks) {
      if (!flaggedHunks.has(hunk)) continue
      for (const l of hunk.lines)
        if (l.type === '+' && isInjectionShaped(l.text))
          injection.push({ file: entry.file, line: l.line })
    }
  }
  const order = (s) => CATEGORIES.indexOf(s.category)
  const fileOrder = new Map()
  for (const s of signals) if (!fileOrder.has(s.file)) fileOrder.set(s.file, fileOrder.size)
  signals.sort(
    (a, b) =>
      fileOrder.get(a.file) - fileOrder.get(b.file) || a.line - b.line || order(a) - order(b),
  )
  if (signals.length > MAX_SIGNALS) return { signals, injection, judge: false, why: 'over-budget' }
  return { signals, injection, judge: signals.length > 0 }
}

// ---------------------------------------------------------------------------
// Envelope and git read.

export class EnvelopeError extends Error {}

/** Validates the verify envelope fields the scan reads. Throws EnvelopeError on anything else. */
export function parseEnvelope(value) {
  const env = typeof value === 'string' ? JSON.parse(value) : value
  if (env === null || typeof env !== 'object' || Array.isArray(env))
    throw new EnvelopeError('envelope must be a JSON object')
  if (typeof env.headSha !== 'string' || !FULL_SHA.test(env.headSha))
    throw new EnvelopeError('envelope headSha must be a 40-hex SHA')
  if (typeof env.baseSha !== 'string' || !FULL_SHA.test(env.baseSha))
    throw new EnvelopeError('envelope baseSha must be a 40-hex SHA')
  if (
    !Array.isArray(env.changedPaths) ||
    env.changedPaths.some((p) => typeof p !== 'string' || p === '')
  )
    throw new EnvelopeError('envelope changedPaths must be an array of non-empty strings')
  return { headSha: env.headSha, baseSha: env.baseSha, changedPaths: env.changedPaths }
}

/** The default git reader: the three-dot PR diff for `paths`, or a throw. */
export function gitDiffReader({ baseSha, headSha, paths, cwd = process.cwd() }) {
  const args = [
    '-c',
    'core.quotePath=false',
    '-c',
    'diff.noprefix=false',
    '-c',
    'diff.mnemonicPrefix=false',
    'diff',
    '--src-prefix=a/',
    '--dst-prefix=b/',
    '--no-ext-diff',
    '--no-textconv',
    '--no-color',
    '--no-renames',
    '--unified=3',
    `${baseSha}...${headSha}`,
    '--',
    ...paths.map((p) => `:(literal)${p}`),
  ]
  const res = spawnSync('git', args, {
    cwd,
    encoding: 'utf8',
    maxBuffer: MAX_DIFF_BYTES + 1024,
  })
  if (res.error) {
    if (res.error.code === 'ENOBUFS') return { overBudget: true }
    throw new Error(`git diff failed: ${res.error.message}`)
  }
  if (res.status !== 0)
    throw new Error(
      `git diff exited ${res.status}: ${String(res.stderr ?? '')
        .trim()
        .slice(0, 200)}`,
    )
  return res.stdout
}

/**
 * Scans the envelope's PR diff. `reader` is injected for tests. Throws on an unreadable diff.
 * Returns the scanDiff result plus `scannedPaths`.
 */
export function scanEnvelope(envelope, { reader = gitDiffReader, cwd } = {}) {
  const env = parseEnvelope(envelope)
  const paths = [...new Set(env.changedPaths)].filter((p) => !isExcludedPath(p))
  if (paths.length === 0) return { signals: [], injection: [], judge: false, scannedPaths: 0 }
  const diff = reader({ baseSha: env.baseSha, headSha: env.headSha, paths, cwd })
  if (diff && typeof diff === 'object' && diff.overBudget)
    return {
      signals: [],
      injection: [],
      judge: false,
      why: 'over-budget',
      diffTooLarge: true,
      scannedPaths: paths.length,
    }
  if (typeof diff !== 'string') throw new Error('diff reader returned no text')
  return { ...scanDiff(diff, { paths }), scannedPaths: paths.length }
}

/**
 * The applies-mode decision: 10 only after a successful, within-budget scan with zero signals;
 * 0 when there are signals; 2 for every error, including an over-size diff.
 */
export function appliesExit(stdinText, opts = {}) {
  try {
    const scan = scanEnvelope(stdinText, opts)
    if (scan.diffTooLarge) return ERROR_EXIT
    if (scan.signals.length > 0) return 0
    return NOT_APPLICABLE_EXIT
  } catch {
    return ERROR_EXIT
  }
}

// ---------------------------------------------------------------------------
// The verdict finalizer.

const signalRef = (signal) => `${signal.file}:${signal.line}`
const short = (text, max = MAX_NOTE) => {
  const value = String(text ?? '')
    .replace(/[\r\n\t]+/g, ' ')
    .trim()
  return value.length > max ? `${value.slice(0, max - 1)}…` : value
}
const idRef = (id) => {
  const match = typeof id === 'string' ? /^[a-z-]+:(.+)$/.exec(id) : null
  return match ? short(match[1]) : short(JSON.stringify(id) ?? String(id))
}

const result = (verdict, evidence) => ({
  ok: true,
  extension: EXTENSION_NAME,
  role: 'verify',
  verdict,
  ...(verdict === 'human' ? { reason: HUMAN_REASON } : {}),
  evidence,
  findings: [],
})

/** A `human` result with one evidence item per problem. Always at least one item. */
export function parkResult(problems) {
  const evidence = problems.length > 0 ? problems : [{ kind: 'judge-error', ref: 'no result' }]
  return result('human', evidence)
}

const JUDGMENT_KEYS = ['id', 'risk', 'why']
const exactKeys = (obj, keys) =>
  obj !== null &&
  typeof obj === 'object' &&
  !Array.isArray(obj) &&
  Object.keys(obj).length === keys.length &&
  keys.every((key) => Object.hasOwn(obj, key))

/**
 * Validates parsed judgments against the scan. Returns `{problems, benign}`: `problems` is the
 * evidence list for a park, `benign` the judgments that passed (one per signal on success).
 * `judgmentsText` null means no file was supplied.
 */
export function checkJudgments(signals, judgmentsText) {
  const problems = []
  const benign = []
  if (judgmentsText === null) {
    if (signals.length === 0) return { problems, benign }
    for (const s of signals)
      problems.push({ kind: 'unjudged', ref: signalRef(s), category: s.category })
    return { problems, benign }
  }
  let parsed
  try {
    parsed = JSON.parse(judgmentsText)
  } catch (err) {
    problems.push({ kind: 'malformed-judgment', ref: short(`invalid JSON: ${err.message}`) })
    return { problems, benign }
  }
  if (!exactKeys(parsed, ['judgments']) || !Array.isArray(parsed.judgments)) {
    problems.push({ kind: 'malformed-judgment', ref: 'expected exactly {judgments: [...]}' })
    return { problems, benign }
  }
  const byId = new Map(signals.map((s) => [s.id, s]))
  const judged = new Set()
  parsed.judgments.forEach((entry, index) => {
    const at = `judgments[${index}]`
    if (!exactKeys(entry, JUDGMENT_KEYS)) {
      problems.push({ kind: 'malformed-judgment', ref: `${at}: expected exactly {id, risk, why}` })
      return
    }
    const { id, risk, why } = entry
    if (typeof id !== 'string' || !byId.has(id)) {
      problems.push({ kind: 'malformed-judgment', ref: `${at}: unknown id ${idRef(id)}` })
      return
    }
    const signal = byId.get(id)
    if (judged.has(id)) {
      problems.push({ kind: 'malformed-judgment', ref: `${signalRef(signal)}: duplicate id` })
      return
    }
    judged.add(id)
    if (risk !== 'benign' && risk !== 'risky') {
      problems.push({
        kind: 'malformed-judgment',
        ref: `${signalRef(signal)}: risk must be benign or risky`,
      })
      return
    }
    if (typeof why !== 'string' || why.trim() === '' || why.length > MAX_WHY) {
      problems.push({
        kind: 'malformed-judgment',
        ref: `${signalRef(signal)}: why must be 1-${MAX_WHY} characters`,
      })
      return
    }
    if (risk === 'risky')
      problems.push({
        kind: 'security-concern',
        ref: signalRef(signal),
        category: signal.category,
        note: short(why, MAX_WHY),
      })
    else benign.push({ signal, why })
  })
  for (const s of signals)
    if (!judged.has(s.id))
      problems.push({ kind: 'unjudged', ref: signalRef(s), category: s.category })
  return { problems, benign }
}

/**
 * The finalizer: re-runs the scan (never trusting the agent's copy), validates the judgments, and
 * returns the full verify result, only ever `pass` or `human`. Never throws.
 */
export function finalizeVerdict({ envelope, judgmentsText = null, reader, cwd } = {}) {
  try {
    let scan
    try {
      scan = scanEnvelope(envelope, { reader, cwd })
    } catch (err) {
      return parkResult([{ kind: 'judge-error', ref: short(`scan failed: ${err.message}`) }])
    }
    const problems = []
    if (scan.why === 'over-budget')
      problems.push({
        kind: 'over-budget',
        ref: scan.diffTooLarge
          ? `diff over ${MAX_DIFF_BYTES} bytes`
          : `${scan.signals.length} signals (limit ${MAX_SIGNALS})`,
      })
    for (const hit of scan.injection)
      problems.push({ kind: 'injection-suspect', ref: `${hit.file}:${hit.line}` })
    if (problems.length > 0) return parkResult(problems)
    const judged = checkJudgments(scan.signals, judgmentsText)
    if (judged.problems.length > 0) return parkResult(judged.problems)
    if (scan.signals.length === 0)
      return result('pass', [
        {
          kind: 'security-scan',
          ref: `no security surface in ${scan.scannedPaths ?? 0} scanned path(s)`,
        },
      ])
    return result(
      'pass',
      judged.benign.map(({ signal, why }) => ({
        kind: 'security-judgment',
        ref: signalRef(signal),
        category: signal.category,
        note: short(why, MAX_WHY),
      })),
    )
  } catch (err) {
    return parkResult([
      { kind: 'judge-error', ref: short(`internal error: ${err?.message ?? err}`) },
    ])
  }
}

// ---------------------------------------------------------------------------
// The park comment.

// Neutralises untrusted note text for a GitHub comment: one line, truncated, every markdown/HTML
// punctuation backslash-escaped, `@` mentions broken, and autolinks defanged.
export function neutralize(text, max = MAX_NOTE) {
  return short(text, max)
    .replace(/[\\`*_{}[\]()#+!|~<>&"]/g, (ch) => `\\${ch}`)
    .replace(/@/g, '@​')
    .replace(/:\/\//g, ':​//')
    .replace(/\bwww\./gi, (m) => `${m.slice(0, 3)}​.`)
}

const codeRef = (ref) => `\`${short(ref, MAX_NOTE).replace(/`/g, "'")}\``

/** The park comment body for a `human` result at `head`. Carries no diff excerpt. */
export function renderParkComment(res, head) {
  const evidence = Array.isArray(res?.evidence) ? res.evidence : []
  const lines = [
    COMMENT_MARKER,
    '### Security review needed',
    '',
    `\`${EXTENSION_NAME}\` parked head \`${String(head ?? '').slice(0, 40)}\` as \`${HUMAN_REASON}\`. A person should look at each item below before approving the park.`,
    '',
  ]
  for (const item of evidence) {
    const kind = PROBLEM_KINDS.includes(item?.kind) ? item.kind : 'judge-error'
    const note =
      typeof item?.note === 'string' && item.note.trim() ? ` — ${neutralize(item.note)}` : ''
    lines.push(`- **${kind}** ${codeRef(item?.ref ?? '')}${note}`)
  }
  return `${lines.join('\n')}\n`
}

function defaultGh(args, { input } = {}) {
  const res = spawnSync('gh', args, { input, encoding: 'utf8' })
  return { status: res.error ? 1 : res.status, stdout: res.stdout ?? '', stderr: res.stderr ?? '' }
}

/**
 * Upserts the single marked comment on PR `pr`. `gh(args, {input})` returns `{status, stdout}`.
 * Returns `{comment: 'created'|'updated'|'failed'}` and never throws.
 */
export function upsertParkComment({ pr, body, gh = defaultGh }) {
  try {
    const list = gh([
      'api',
      '--paginate',
      `repos/{owner}/{repo}/issues/${pr}/comments`,
      '--jq',
      `.[] | select(.body | contains(${JSON.stringify(COMMENT_MARKER)})) | .id`,
    ])
    if (list.status !== 0) return { comment: 'failed' }
    const id = String(list.stdout ?? '')
      .split('\n')
      .map((line) => line.trim())
      .find((line) => /^\d+$/.test(line))
    const input = JSON.stringify({ body })
    const write = id
      ? gh(['api', '-X', 'PATCH', `repos/{owner}/{repo}/issues/comments/${id}`, '--input', '-'], {
          input,
        })
      : gh(['api', '-X', 'POST', `repos/{owner}/{repo}/issues/${pr}/comments`, '--input', '-'], {
          input,
        })
    if (write.status !== 0) return { comment: 'failed' }
    return { comment: id ? 'updated' : 'created' }
  } catch {
    return { comment: 'failed' }
  }
}

/** The `comment` verb: posts only for a `human` result. Never throws. */
export function commentForResult({ pr, head, resultText, gh }) {
  try {
    if (!/^\d+$/.test(String(pr ?? '')) || !FULL_SHA.test(String(head ?? '')))
      return { comment: 'failed' }
    const res = JSON.parse(resultText)
    if (res?.verdict !== 'human') return { comment: 'skipped' }
    return upsertParkComment({ pr, body: renderParkComment(res, head), gh })
  } catch {
    return { comment: 'failed' }
  }
}

// ---------------------------------------------------------------------------
// CLI.

export class UsageError extends Error {}

const readText = (file) => readFileSync(file, 'utf8')

/** Runs one verb. Returns `{stdout, exit}`; applies mode returns an empty stdout. */
export function runCli(argv, { stdin = () => readFileSync(0, 'utf8'), reader, gh, cwd } = {}) {
  if (argv.length === 0) {
    let text
    try {
      text = stdin()
    } catch {
      return { stdout: '', exit: ERROR_EXIT }
    }
    return { stdout: '', exit: appliesExit(text, { reader, cwd }) }
  }
  const { values, positionals } = parseArgs({
    args: argv,
    allowPositionals: true,
    options: {
      envelope: { type: 'string' },
      judgments: { type: 'string' },
      json: { type: 'boolean' },
      pr: { type: 'string' },
      head: { type: 'string' },
      result: { type: 'string' },
    },
  })
  const [command, ...extra] = positionals
  if (extra.length > 0) throw new UsageError(`unexpected argument: ${extra[0]}`)
  if (command === 'scan') {
    if (!values.envelope) throw new UsageError('scan requires --envelope <file>')
    try {
      const scan = scanEnvelope(readText(values.envelope), { reader, cwd })
      return { stdout: JSON.stringify({ ok: true, ...scan }), exit: 0 }
    } catch (err) {
      return {
        stdout: JSON.stringify({
          ok: false,
          error: short(err.message),
          signals: [],
          injection: [],
          judge: false,
          why: 'scan-error',
        }),
        exit: ERROR_EXIT,
      }
    }
  }
  if (command === 'verdict') {
    let res
    try {
      if (!values.envelope) throw new UsageError('verdict requires --envelope <file>')
      const envelope = readText(values.envelope)
      let judgmentsText = null
      let missing = null
      if (values.judgments !== undefined) {
        try {
          judgmentsText = readText(values.judgments)
        } catch (err) {
          missing = err
        }
      }
      res = missing
        ? parkResult([
            {
              kind: 'malformed-judgment',
              ref: short(`judgments unreadable: ${missing.code ?? missing.message}`),
            },
          ])
        : finalizeVerdict({ envelope, judgmentsText, reader, cwd })
    } catch (err) {
      res = parkResult([{ kind: 'judge-error', ref: short(err?.message ?? String(err)) }])
    }
    return { stdout: JSON.stringify(res), exit: 0 }
  }
  if (command === 'comment') {
    let resultText = ''
    try {
      resultText = readText(values.result ?? '')
    } catch {
      return { stdout: JSON.stringify({ comment: 'failed' }), exit: 0 }
    }
    const out = commentForResult({ pr: values.pr, head: values.head, resultText, gh })
    return { stdout: JSON.stringify(out), exit: 0 }
  }
  throw new UsageError(`unknown verb: ${command ?? '(none)'}`)
}

if (isMainModule(import.meta.url)) {
  const argv = process.argv.slice(2)
  if (argv.length === 0) {
    // Applies mode: every failure is an explicit exit 2, never Node's exit 1.
    const bail = () => process.exit(ERROR_EXIT)
    process.on('uncaughtException', bail)
    process.on('unhandledRejection', bail)
  }
  try {
    const { stdout, exit } = runCli(argv)
    if (stdout) process.stdout.write(`${stdout}\n`)
    process.exitCode = exit
  } catch (err) {
    process.stderr.write(`verify-security-surface: ${err.message}\n`)
    process.exitCode = argv.length === 0 ? ERROR_EXIT : err instanceof UsageError ? 64 : ERROR_EXIT
    if (argv[0] === 'verdict')
      process.stdout.write(
        `${JSON.stringify(parkResult([{ kind: 'judge-error', ref: short(err.message) }]))}\n`,
      )
  }
}
