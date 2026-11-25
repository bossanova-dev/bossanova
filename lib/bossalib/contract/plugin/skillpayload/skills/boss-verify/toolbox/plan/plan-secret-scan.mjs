// Mechanical secret floor for a planning run's persisted artifacts.
//
// A secret gate that greps a plan file through the agent's shell can be satisfied by fabricated
// absence: a command-rewriting shell hook, alias or function can turn the grep into a wrapper that
// prints nothing and exits as if nothing matched. This helper reads each named file with
// `fs.readFileSync` — never a shell grep — and matches a fixed set of HIGH-PRECISION credential
// shapes plus, when enabled (the CLI default), a bounded private-suffix internal-hostname
// detector. It is a floor UNDER the gate's judgement read, never a replacement: PII, unusual
// credentials and hostnames outside the suffix set remain the reader's job.
//
// Precision over recall, on purpose. False positives in a mandatory gate train agents to ignore it,
// so the bare words "token", "secret" and "password" never match, placeholder passwords in
// connection URLs (`${…}`, `<…>`, REPLACE_ME, pass, password, ***) are excluded, a PEM block only
// counts with a real base64 body, and the documented redaction forms (`[REDACTED…]`,
// `token=REDACTED`) pass. Every quantifier is bounded so a long line cannot backtrack
// pathologically.
//
// The overlapping shapes are re-expressed rather than imported from a runtime log scrubber: a
// log scrubber redacts output, this gates a document, and a vendored toolbox cannot import it.
//
// Output: one `file:line:kind` per hit with the value masked (never echoed), then a fixed rotation
// line for credential hits and a redaction note for internal-hostname hits. Exit codes: 0 clean,
// 1 hit, 2 a file unreadable or no files given (the gate fails closed).
//
// Node built-ins plus ./main-module.mjs only.

import { readFileSync } from 'node:fs'

import { isMainModule } from '../main-module.mjs'

export const VERDICTS = Object.freeze({ 0: 'clean', 1: 'redact', 2: 'fail-closed' })

export const ROTATION_LINE =
  'plan-secret-scan: a real credential that reached a draft may already be exposed — report it for ' +
  'rotation, not only redaction.'

export const HOSTNAME_LINE =
  'plan-secret-scan: an internal hostname is network detail, not a credential — replace it ' +
  'with a role name or [REDACTED: internal hostname].'

const PLACEHOLDER_HOST_LABELS = new Set([
  'example',
  'examples',
  'host',
  'hostname',
  'myhost',
  'your-host',
  'server',
  'machine',
  'foo',
  'bar',
  'baz',
  'placeholder',
  'redacted',
])

const WELL_KNOWN_INTERNAL_HOSTS = new Set([
  'host.docker.internal',
  'gateway.docker.internal',
  'metadata.google.internal',
])

// A connection-URL password that is a placeholder or an interpolation, never a literal secret.
const PLACEHOLDER_PASSWORD =
  /^(?:(?:\$\{[^}]{0,256}\})+|<[^>]{0,256}>|REPLACE_ME|pass|password|\*+|x+|\[?REDACTED\b.*)$/i
// A value that is already redacted or a template stand-in (prefix test: the capture stops at `]`).
const REDACTED_VALUE = /^(?:\[?REDACTED\b|%5BREDACTED|\*+$|x+$|<|\$\{|\$[A-Z_])/i

/**
 * Each shape: `kind`, a global regex, and an optional `accept(match)` that returns false to reject a
 * placeholder. Regexes are built once; `lastIndex` is reset per line.
 */
export const SHAPES = [
  {
    kind: 'github-token',
    re: /\b(?:github_pat_[A-Za-z0-9_]{22,255}|gh[pousr]_[A-Za-z0-9]{36,255})\b/g,
  },
  {
    kind: 'jwt',
    re: /\beyJ[A-Za-z0-9_-]{8,4096}\.eyJ[A-Za-z0-9_-]{8,4096}\.[A-Za-z0-9_-]{8,4096}/g,
  },
  { kind: 'stripe-key', re: /\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,255}\b/g },
  {
    kind: 'provider-sk-key',
    // A digit somewhere in the tail keeps a kebab-case slug that happens to start `sk-` out.
    re: /(?<![A-Za-z0-9_-])sk-(?=[A-Za-z_-]{0,254}[0-9])[A-Za-z0-9_-]{20,255}/g,
  },
  { kind: 'slack-token', re: /\bxox[abpr]-[A-Za-z0-9-]{10,255}/g },
  { kind: 'aws-access-key-id', re: /\bAKIA[0-9A-Z]{16}\b/g },
  { kind: 'linear-api-key', re: /\blin_api_[A-Za-z0-9]{20,255}/g },
  {
    kind: 'aws-secret-access-key',
    re: /aws_secret_access_key["']?\s{0,8}[:=]\s{0,8}["']?([A-Za-z0-9/+]{40})(?![A-Za-z0-9/+])/gi,
  },
  {
    kind: 'bearer-token',
    re: /Authorization:\s{0,8}Bearer\s{1,8}([A-Za-z0-9._~+/=-]{20,4096})/gi,
    accept: (m) => !REDACTED_VALUE.test(m[1]),
  },
  {
    kind: 'url-password',
    re: /\b[a-z][a-z0-9+.-]{1,30}:\/\/([^\s:/@]{1,256}):([^\s/@]{1,256})@[^\s/@]{1,256}/gi,
    accept: (m) => !PLACEHOLDER_PASSWORD.test(m[2]),
  },
  {
    kind: 'signed-url-query',
    re: /[?&](signature|sig|token|X-Amz-Signature)=([^&#\s"')\]]{8,4096})/gi,
    accept: (m) => !REDACTED_VALUE.test(m[2]),
  },
]

/**
 * Opt-in private-suffix internal-hostname detector. Kept outside `SHAPES` so library callers that
 * sweep git history for credentials do not inherit hostname findings.
 */
export const INTERNAL_HOSTNAME_SHAPES = [
  {
    kind: 'internal-hostname',
    // Closed lower-case suffix set (.lan, .local, .internal, .localdomain, .home.arpa) with
    // bounded label count (<= 8 for single-label suffixes) and label length (<= 63).
    // Leftmost label >= 2 chars; rejects identifier/dot/$/single-slash left boundaries (while
    // permitting URL `//` and `@`) and trailing label chars, `.label`, or `(`.
    re: /(?<![A-Za-z0-9_.$-])(?<!(?<!\/)\/)([A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?){0,6})\.(?:lan|local|internal|localdomain|home\.arpa)(?![A-Za-z0-9_-]|\.[A-Za-z0-9_-]|\()/g,
    accept: (m) => {
      const full = m[0].toLowerCase()
      if (WELL_KNOWN_INTERNAL_HOSTS.has(full)) return false
      if (full.endsWith('.cluster.local')) return false
      const labels = m[1].toLowerCase().split('.')
      return !labels.some((label) => PLACEHOLDER_HOST_LABELS.has(label))
    },
  },
]

// A PEM private key only counts when at least 64 base64 characters of body precede its END line,
// so a header-only fixture is not a hit. Scanned over the whole text because the body spans lines.
const PEM =
  /-----BEGIN (?:[A-Z0-9]{1,20} ){0,3}PRIVATE KEY-----\r?\n((?:[A-Za-z0-9+/=]{1,128}\r?\n){1,512}?)-----END (?:[A-Z0-9]{1,20} ){0,3}PRIVATE KEY-----/g

/**
 * Scan one text. Returns hits as `{line, kind}` (1-based line), never the matched value.
 * @param {string} text
 * @param {{hostnames?: boolean}} [options]
 */
export function scanText(text, { hostnames = false } = {}) {
  const hits = []
  const shapes = hostnames ? [...SHAPES, ...INTERNAL_HOSTNAME_SHAPES] : SHAPES
  const lines = text.split('\n')
  lines.forEach((line, i) => {
    for (const shape of shapes) {
      shape.re.lastIndex = 0
      let m
      while ((m = shape.re.exec(line)) !== null) {
        if (!shape.accept || shape.accept(m)) {
          hits.push({ line: i + 1, kind: shape.kind })
          break
        }
        if (m[0].length === 0) shape.re.lastIndex += 1
      }
    }
  })
  PEM.lastIndex = 0
  let m
  while ((m = PEM.exec(text)) !== null) {
    if (m[1].replace(/\s/g, '').length >= 64) {
      hits.push({ line: text.slice(0, m.index).split('\n').length, kind: 'private-key' })
    }
  }
  return hits.sort((a, b) => a.line - b.line)
}

/**
 * Scan files. Returns `{code, lines}`: code 0 clean, 1 hit, 2 unreadable/no files.
 * @param {string[]} files
 * @param {{read?: (f: string) => string, hostnames?: boolean}} [deps]
 */
export function scanFiles(files, deps = {}) {
  const read = deps.read || ((f) => readFileSync(f, 'utf8'))
  const hostnames = deps.hostnames ?? true
  if (files.length === 0) {
    return { code: 2, lines: ['plan-secret-scan: no files given — the gate fails closed'] }
  }
  const out = []
  let unreadable = false
  let hasCredentialHit = false
  let hasHostnameHit = false
  for (const file of files) {
    let text
    try {
      text = read(file)
    } catch (err) {
      out.push(`plan-secret-scan: cannot read ${file}: ${err?.code || err?.message || err}`)
      unreadable = true
      continue
    }
    for (const hit of scanText(text, { hostnames })) {
      out.push(`${file}:${hit.line}:${hit.kind} [value masked]`)
      if (hit.kind === 'internal-hostname') hasHostnameHit = true
      else hasCredentialHit = true
    }
  }
  if (unreadable) return { code: 2, lines: out }
  if (out.length > 0) {
    const notes = []
    if (hasCredentialHit) notes.push(ROTATION_LINE)
    if (hasHostnameHit) notes.push(HOSTNAME_LINE)
    return { code: 1, lines: [...out, ...notes] }
  }
  return { code: 0, lines: [] }
}

if (isMainModule(import.meta.url)) {
  const { code, lines } = scanFiles(process.argv.slice(2))
  for (const line of lines) process.stdout.write(`${line}\n`)
  process.stdout.write(`plan-secret-scan: verdict=${VERDICTS[code]}\n`)
  process.exitCode = code
}
