import fs from 'node:fs'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

const DEFAULT_ORDER = 100
const KNOWN_EXTENSION_ROLES = new Set([
  'lens',
  'round',
  'surface',
  'plan-reviewer',
  'agent-driver',
  'draft',
  'methodology',
  'notes',
  'knowledge',
])

// Minimal YAML-frontmatter reader. Supports the flat scalar keys and the single
// nested `x-boss-extension:` block this contract needs — no external YAML dep
// (matches the no-new-deps constraint of the scripts/ helpers).
//
// `hasFrontmatter` reports whether a delimited block was found at all. It matters because a file
// with a broken fence (an opening `---` with no closing one, say) parses to the SAME empty `data`
// as a file whose frontmatter is perfectly valid and simply declares no marker — and discovery has
// to tell a failed declaration apart from a deliberate non-extension.
export function parseFrontmatter(text) {
  const match = /^---\n([\s\S]*?)\n---\n?([\s\S]*)$/.exec(text)
  if (!match) return { data: {}, body: text, hasFrontmatter: false }
  const data = {}
  let current = null // name of the block we are collecting nested keys into
  for (const raw of match[1].split('\n')) {
    if (!raw.trim() || raw.trim().startsWith('#')) continue
    const nested = /^ {2,}([\w-]+):\s*(.*)$/.exec(raw)
    if (nested && current) {
      data[current][nested[1]] = coerce(nested[2])
      continue
    }
    const top = /^([\w-]+):\s*(.*)$/.exec(raw)
    if (!top) continue
    if (top[2] === '') {
      data[top[1]] = {}
      current = top[1]
    } else {
      data[top[1]] = coerce(top[2])
      current = null
    }
  }
  return { data, body: match[2], hasFrontmatter: true }
}

// A quoted YAML scalar is a string by construction, so the quotes must be honoured
// BEFORE the numeric test rather than stripped ahead of it: `lens: "42"` has to stay
// the string "42" to bind to a config lens id of "42" (validateConfig only requires a
// non-empty string id, so numeric-looking ids are legal), and stripping first turned it
// into Number 42, which extensionMarker's `typeof === 'string'` guard then dropped —
// silently reporting the extension as unbound. Only a bare, unquoted integer coerces.
function coerce(value) {
  const v = value.trim()
  const quoted = /^(["'])([\s\S]*)\1$/.exec(v)
  if (quoted) return quoted[2]
  if (/^-?\d+$/.test(v)) return Number(v)
  return v
}

export function extensionMarker(frontmatter) {
  const block = frontmatter && frontmatter['x-boss-extension']
  if (!block || typeof block !== 'object') return null
  if (typeof block.extends !== 'string' || typeof block.role !== 'string') return null
  const order = typeof block.order === 'number' ? block.order : DEFAULT_ORDER
  const marker = { extends: block.extends, role: block.role, order }
  // Optional binding to a config lens id. It is meaningful only for `role: lens` — a core's
  // lens phase indexes discovered descriptors by it — but it is deliberately NOT validated
  // against the role here: this helper is role-generic, and a stray key on another role is
  // inert rather than a discovery failure. Absent/blank/non-string omits the field entirely,
  // so an unbound descriptor's JSON is byte-identical to what it was before the key existed.
  if (typeof block.lens === 'string' && block.lens !== '') marker.lens = block.lens
  // Optional binding to a review CAPABILITY id — an exact structural mirror of `lens` above.
  // It is meaningful only for `role: round`, where a core's opportunistic default-round phase
  // uses it to tell that a discovered round already covers a capability the core would otherwise
  // default-run, so the repo gets one pass instead of two. Deliberately NOT validated against the
  // role (same reason as `lens`), and absent/blank/non-string omits the field entirely so an
  // undeclared descriptor's JSON is byte-identical to what it was before the key existed.
  if (typeof block.capability === 'string' && block.capability !== '') {
    marker.capability = block.capability
  }
  return marker
}

export function discoverExtensions({ core, root, role }) {
  const skillsDir = path.join(root, '.claude', 'skills')
  const extensions = []
  const skipped = []
  let entries = []
  try {
    entries = fs.readdirSync(skillsDir, { withFileTypes: true })
  } catch {
    return { extensions, skipped } // no skills dir -> no-op
  }
  const prefix = `${core}-`
  for (const entry of entries) {
    if (!entry.isDirectory() || !entry.name.startsWith(prefix)) continue
    const skillPath = path.join(skillsDir, entry.name, 'SKILL.md')
    if (!fs.existsSync(skillPath)) {
      skipped.push({ name: entry.name, reason: 'no SKILL.md' })
      continue
    }
    let marker = null
    let parsed = null
    try {
      parsed = parseFrontmatter(fs.readFileSync(skillPath, 'utf8'))
      marker = extensionMarker(parsed.data)
    } catch (err) {
      skipped.push({ name: entry.name, reason: `unreadable frontmatter: ${err.message}` })
      continue
    }
    if (!marker) {
      // Three very different inputs land here, and cores treat them differently: only a
      // *genuinely* markerless skill is a deliberate non-extension they may quietly ignore.
      // A broken fence and a half-written marker are both failed declarations of a real
      // extension, so each gets its own reason and stays visible in the ledger.
      let reason = 'missing x-boss-extension marker'
      if (!parsed.hasFrontmatter) {
        reason = 'malformed frontmatter: no parseable --- block'
      } else if ('x-boss-extension' in parsed.data) {
        reason = 'incomplete x-boss-extension marker: needs string "extends" and "role"'
      }
      skipped.push({ name: entry.name, reason })
      continue
    }
    if (marker.extends !== core) {
      skipped.push({ name: entry.name, reason: `extends "${marker.extends}", not "${core}"` })
      continue
    }
    if (typeof role === 'string' && role !== '' && !KNOWN_EXTENSION_ROLES.has(role)) {
      skipped.push({ name: entry.name, reason: `unknown requested role "${role}"` })
      continue
    }
    // A same-prefix extension that extends this core but declares another known role is a
    // legitimate cross-role sibling (e.g. boss-review lens vs round) and should not pollute
    // `skipped`. A typo'd/unknown role remains a misconfiguration and is recorded as a skip.
    if (typeof role === 'string' && role !== '' && marker.role !== role) {
      if (!KNOWN_EXTENSION_ROLES.has(marker.role)) {
        skipped.push({ name: entry.name, reason: `role "${marker.role}", not "${role}"` })
      }
      continue
    }
    const descriptor = {
      name: entry.name,
      dir: path.join(skillsDir, entry.name),
      skillPath,
      role: marker.role,
      order: marker.order,
    }
    if (marker.lens !== undefined) descriptor.lens = marker.lens
    if (marker.capability !== undefined) descriptor.capability = marker.capability
    extensions.push(descriptor)
  }
  extensions.sort((a, b) => a.order - b.order || a.name.localeCompare(b.name))
  return { extensions, skipped }
}

export const ROLE_SCHEMAS = {
  lens: ['severity', 'file', 'line', 'title', 'detail'],
  round: ['severity', 'file', 'line', 'title', 'detail'],
  surface: ['path', 'caption', 'evidenceTokens'],
  'plan-reviewer': ['severity', 'section', 'title', 'detail'],
  notes: ['tag', 'body', 'noteId'],
  // A knowledge artifact is a file in the tree, so `path` is its proof of persistence — the same
  // role `noteId` plays for `notes`. Both are enforced as non-empty below.
  knowledge: ['path', 'title', 'kind'],
}

export function validateResult(envelope, role) {
  const errors = []
  if (!envelope || typeof envelope !== 'object') {
    return { ok: false, errors: ['envelope is not an object'] }
  }
  const requiredKeys = ROLE_SCHEMAS[role]
  if (!requiredKeys) return { ok: false, errors: [`unknown role "${role}"`] }
  if (typeof envelope.ok !== 'boolean') {
    errors.push('ok is not a boolean')
  } else if (envelope.ok === false) {
    // A handled failure envelope ({ok:false, ...}) is a *failing-validation*
    // envelope per the contract: the core must skip it ("extension <name>:
    // skipped (<reason>)"), not fold its (empty) items as accepted results.
    // Surface the extension's own error text as the skip reason when present.
    const reason =
      typeof envelope.error === 'string' && envelope.error.trim() !== ''
        ? envelope.error.trim()
        : 'no error detail provided'
    errors.push(`extension reported failure (ok:false): ${reason}`)
  }
  if (typeof envelope.extension !== 'string' || envelope.extension === '') {
    errors.push('extension is not a non-empty string')
  }
  if (envelope.role !== role) {
    errors.push(`envelope role "${envelope.role}" does not match expected "${role}"`)
  }
  if (!Array.isArray(envelope.items)) {
    errors.push('items is not an array')
    return { ok: false, errors }
  }
  envelope.items.forEach((item, idx) => {
    if (!item || typeof item !== 'object') {
      errors.push(`item ${idx} is not an object`)
      return
    }
    for (const key of requiredKeys) {
      if (!(key in item)) errors.push(`item ${idx} missing "${key}"`)
    }
    // Roles whose items are a *claim of persistence* need every declared key to carry real text:
    // an empty `noteId` or `path` satisfies the `in` check above while proving nothing was
    // written. Roles whose items are findings (lens/round/plan-reviewer) legitimately carry
    // blank fields, so the guard stays scoped rather than global.
    if (role === 'notes' || role === 'knowledge') {
      for (const key of requiredKeys) {
        if (key in item && (typeof item[key] !== 'string' || item[key].trim() === '')) {
          errors.push(`item ${idx} "${key}" is not a non-empty string`)
        }
      }
    }
  })
  return { ok: errors.length === 0, errors }
}

function parseArgs(argv) {
  const args = {}
  for (let i = 0; i < argv.length; i += 1) {
    const token = argv[i]
    if (token.startsWith('--')) {
      const key = token.slice(2)
      const next = argv[i + 1]
      if (next === undefined || next.startsWith('--')) {
        args[key] = true
      } else {
        args[key] = next
        i += 1
      }
    }
  }
  return args
}

export function main(argv) {
  const [subcommand, ...rest] = argv
  const args = parseArgs(rest)
  if (subcommand === 'discover') {
    const core = args.core
    if (typeof core !== 'string' || core === '') {
      process.stderr.write('discover: --core <name> is required\n')
      return 2
    }
    const root = typeof args.root === 'string' ? args.root : process.cwd()
    const role = typeof args.role === 'string' ? args.role : undefined
    const result = discoverExtensions({ core, root, role })
    if (args.json) {
      process.stdout.write(`${JSON.stringify(result)}\n`)
    } else {
      for (const ext of result.extensions) {
        process.stdout.write(`${ext.name}\t${ext.role}\t${ext.order}\n`)
      }
    }
    return 0
  }
  if (subcommand === 'validate') {
    const role = args.role
    let source
    try {
      source =
        typeof args.file === 'string'
          ? fs.readFileSync(args.file, 'utf8')
          : fs.readFileSync(0, 'utf8')
    } catch (err) {
      // A missing / unreadable envelope file must degrade to the same clean
      // {ok:false} shape as a malformed one — never an uncaught stack trace
      // (the contract's "never throws" promise; callers skip on a non-zero exit).
      process.stdout.write(
        `${JSON.stringify({ ok: false, errors: [`cannot read input: ${err.message}`] })}\n`,
      )
      return 1
    }
    let envelope
    try {
      envelope = JSON.parse(source)
    } catch (err) {
      process.stdout.write(
        `${JSON.stringify({ ok: false, errors: [`invalid JSON: ${err.message}`] })}\n`,
      )
      return 1
    }
    const result = validateResult(envelope, role)
    process.stdout.write(`${JSON.stringify(result)}\n`)
    return result.ok ? 0 : 1
  }
  process.stderr.write(`unknown subcommand: ${subcommand ?? '(none)'}\n`)
  return 2
}

import { isMainModule } from './main-module.mjs'

if (isMainModule(import.meta.url, { warn: () => {} })) {
  process.exit(main(process.argv.slice(2)))
}
