import fs from 'node:fs'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

const DEFAULT_ORDER = 100

// The SINGLE role table. Discovery (`KNOWN_EXTENSION_ROLES`) and validation (`ROLE_SCHEMAS`) are
// both derived from it, so a role cannot exist for one and not the other — the two registries were
// separate literals and had already drifted apart more than once, which is how a role
// discovery happily accepted came back from `validateResult` as `unknown role`.
//
// Each entry declares the shape of the result that role returns:
//   kind: 'items'  — the standard `{ ok, extension, role, items[] }` envelope; `keys` are the keys
//                    every element of `items[]` must carry.
//   kind: 'fields' — a role that ships BEHAVIOUR rather than a findings list, so its result carries
//                    named top-level fields instead of `items[]`; `keys` are those fields.
//   nonEmpty       — the declared keys must be non-empty strings, not merely present. Set for the
//                    roles whose keys are a CLAIM OF PERSISTENCE (an empty `noteId`/`path`/
//                    `planPath` satisfies a presence check while proving nothing was written).
//   header         — whether the result carries the standard `ok`/`extension`/`role` envelope
//                    header. False for the two roles whose documented result is a bare record:
//                    `methodology` returns the core's fixed short task contract, and `agent-driver`
//                    returns a `SurfaceRun` (see the agent-driver contract doc).
export const EXTENSION_ROLES = {
  lens: { kind: 'items', keys: ['severity', 'file', 'line', 'title', 'detail'] },
  round: { kind: 'items', keys: ['severity', 'file', 'line', 'title', 'detail'] },
  surface: { kind: 'items', keys: ['path', 'caption', 'evidenceTokens'] },
  'plan-reviewer': { kind: 'items', keys: ['severity', 'section', 'title', 'detail'] },
  notes: { kind: 'items', keys: ['tag', 'body', 'noteId'], nonEmpty: true },
  // A knowledge artifact is a file in the tree, so `path` is its proof of persistence — the same
  // role `noteId` plays for `notes`. Both are enforced as non-empty.
  knowledge: { kind: 'items', keys: ['path', 'title', 'kind'], nonEmpty: true },
  // A draft extension writes the plan to `context.planPath` and reports it back; the plan file
  // itself is the deliverable, so the envelope's job is to name where it landed.
  draft: { kind: 'fields', keys: ['planPath'], nonEmpty: true },
  // The fixed short task contract a methodology extension returns to its core.
  methodology: {
    kind: 'fields',
    header: false,
    keys: [
      'taskId',
      'filesTouched',
      'testsAddedOrPassing',
      'interfaceSignatures',
      'residualRisks',
      'decisionsRecorded',
      'commitsMade',
    ],
  },
  // The nine required `SurfaceRun` keys — see the agent-driver contract doc. A proof host may
  // additionally check that
  // `surface` matches the driver it came from (a check only the caller can make).
  'agent-driver': {
    kind: 'fields',
    header: false,
    keys: [
      'surface',
      'captureShapes',
      'brief',
      'agentResult',
      'hasFailure',
      'noSurface',
      'scanTexts',
      'elapsedMs',
      'reasonCode',
    ],
  },
}

const KNOWN_EXTENSION_ROLES = new Set(Object.keys(EXTENSION_ROLES))

// Every reason `discoverExtensions` can put in `skipped`, classified deliberate-vs-broken ONCE,
// here, rather than re-derived in each consuming core's prose from the literal `reason` text.
//
// `deliberate: true` means the skip is the CONTRACT WORKING: the directory is a same-prefix skill
// that is not an extension of this core, so reporting it would cry wolf on every run for as long as
// the helper exists. Everything else is a misconfiguration a core MUST record in its ledger — a
// broken extension that vanishes with no ledger line reads as the intended tier.
export const SKIP_REASONS = {
  'no-skill-md': { deliberate: false },
  'unreadable-frontmatter': { deliberate: false },
  'malformed-frontmatter': { deliberate: false },
  'incomplete-marker': { deliberate: false },
  'missing-marker': { deliberate: true },
  // Split on whether the declared core could EVER have discovered this directory. An extension of
  // `<core>` is a directory named `<core>-<suffix>` (see the contract), and discovery only scans
  // that prefix — so `boss-review-x` declaring `extends: boss-plan` is unreachable from boss-plan
  // too, and suppressing it as "somebody else's extension" would hide a typo'd `extends` at the one
  // site that would have dispatched it, which is the exact silent drop this table exists to end.
  // The deliberate case is real but narrow: core prefixes NEST, so `boss-` also matches
  // `boss-plan-notes`, and a core named `boss` must not warn about every other core's extensions.
  'extends-other-core': { deliberate: true },
  'extends-unrelated-core': { deliberate: false },
  'unknown-requested-role': { deliberate: false },
  'wrong-role': { deliberate: false },
  'invalid-lens-binding': { deliberate: false },
}

// Builds one `skipped` entry. `reason` strings are deliberately unchanged from before the codes
// existed — consuming prose and tests assert on the exact literals — so `code` is the stable key and
// `reason` stays the human sentence. An unclassified code degrades to `deliberate: false` (report
// it) rather than silently to the exempt class; the exhaustiveness ratchet in the test suite is what
// turns that degradation into a red build.
function skipEntry(name, code, reason) {
  const classification = SKIP_REASONS[code]
  return { name, reason, code, deliberate: classification ? classification.deliberate : false }
}

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
      skipped.push(skipEntry(entry.name, 'no-skill-md', 'no SKILL.md'))
      continue
    }
    let marker = null
    let parsed = null
    try {
      parsed = parseFrontmatter(fs.readFileSync(skillPath, 'utf8'))
      marker = extensionMarker(parsed.data)
    } catch (err) {
      skipped.push(
        skipEntry(entry.name, 'unreadable-frontmatter', `unreadable frontmatter: ${err.message}`),
      )
      continue
    }
    if (!marker) {
      // Three very different inputs land here, and cores treat them differently: only a
      // *genuinely* markerless skill is a deliberate non-extension they may quietly ignore.
      // A broken fence and a half-written marker are both failed declarations of a real
      // extension, so each gets its own reason and stays visible in the ledger.
      let code = 'missing-marker'
      let reason = 'missing x-boss-extension marker'
      if (!parsed.hasFrontmatter) {
        code = 'malformed-frontmatter'
        reason = 'malformed frontmatter: no parseable --- block'
      } else if ('x-boss-extension' in parsed.data) {
        code = 'incomplete-marker'
        reason = 'incomplete x-boss-extension marker: needs string "extends" and "role"'
      }
      skipped.push(skipEntry(entry.name, code, reason))
      continue
    }
    if (marker.extends !== core) {
      // Deliberate only when the declared core could actually own THIS DIRECTORY — which is a
      // question about the directory name, not about the two core names. A core owns exactly the
      // directories named `<its-name>-<suffix>`, so the test is whether this entry is one of them.
      // Core prefixes nest (`boss` also enumerates `boss-plan-notes`), and that case still resolves
      // deliberate: `boss-plan-notes` does start with `boss-plan-`, so `boss` stays quiet about an
      // extension `boss-plan` genuinely owns.
      //
      // Comparing the core names instead — `marker.extends.startsWith(\`${core}-\`)` — answers a
      // different question ("is the declared core a sub-core of mine?") and silently suppresses a
      // real typo: directory `boss-review-foo` declaring `extends: boss-review-ce` would pass that
      // test, yet `boss-review-ce` can only ever own `boss-review-ce-*`, so no core anywhere would
      // have reported it. The `reason` is identical for both codes; only the classification differs.
      const code = entry.name.startsWith(`${marker.extends}-`)
        ? 'extends-other-core'
        : 'extends-unrelated-core'
      skipped.push(skipEntry(entry.name, code, `extends "${marker.extends}", not "${core}"`))
      continue
    }
    if (typeof role === 'string' && role !== '' && !KNOWN_EXTENSION_ROLES.has(role)) {
      skipped.push(
        skipEntry(entry.name, 'unknown-requested-role', `unknown requested role "${role}"`),
      )
      continue
    }
    // A same-prefix extension that extends this core but declares another known role is a
    // legitimate cross-role sibling (e.g. boss-review lens vs round) and should not pollute
    // `skipped`. A typo'd/unknown role remains a misconfiguration and is recorded as a skip.
    if (typeof role === 'string' && role !== '' && marker.role !== role) {
      if (!KNOWN_EXTENSION_ROLES.has(marker.role)) {
        skipped.push(skipEntry(entry.name, 'wrong-role', `role "${marker.role}", not "${role}"`))
      }
      continue
    }
    // A `role: lens` descriptor whose `lens` key is PRESENT but unusable is a failed
    // binding, not an absent one. `extensionMarker` drops it silently (its contract is role-generic
    // and unchanged), which left a misconfigured lens extension indistinguishable from one that
    // deliberately declared no binding — both reported `unbound`, so the operator got no signal that
    // the descriptor tried and failed. Recording it here keeps `extensions` ∩ `skipped` = ∅: a
    // rejected extension never reaches `.extensions`.
    //
    // Scoped to `role: lens` on purpose. The `lens` key is role-generic (see the extension
    // contract), so a stray one on another role is carried through and ignored rather than
    // rejected — only a lens descriptor is rendered undispatchable by it.
    //
    // `capability` (the `role: round` key) is the structural mirror of `lens` and deliberately gets
    // NO equivalent skip, because the two are not mirrors in CONSEQUENCE. A lens descriptor binds
    // to a lens id to be dispatched at all, so an unusable `lens` leaves it inert and skipping it
    // costs nothing while buying the operator a signal. A `capability` is only a suppression hint:
    // the round runs identically without it, and the contract already refuses to let a round that
    // failed to load retire the capability it declares. Skipping a round for a malformed
    // `capability` would therefore delete a working whole-branch reviewer to report a typo — strictly
    // worse than running it and ignoring the key. The right treatment there is a warning, which this
    // helper has no channel for; do not "restore the symmetry" by adding one here.
    if (marker.role === 'lens') {
      const declaredLens = parsed.data['x-boss-extension'].lens
      if (declaredLens !== undefined && marker.lens === undefined) {
        skipped.push(
          skipEntry(
            entry.name,
            'invalid-lens-binding',
            'invalid "lens" binding (expected a non-empty string)',
          ),
        )
        continue
      }
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

// Derived view of the single role table above: role -> the keys its result must declare. Kept as a
// named export because it is the long-standing public surface (parity gates and several suites read
// it directly); it is no longer a second hand-maintained literal.
export const ROLE_SCHEMAS = Object.fromEntries(
  Object.entries(EXTENSION_ROLES).map(([role, spec]) => [role, spec.keys]),
)

export function validateResult(envelope, role) {
  const errors = []
  if (!envelope || typeof envelope !== 'object') {
    return { ok: false, errors: ['envelope is not an object'] }
  }
  const spec = EXTENSION_ROLES[role]
  if (!spec) return { ok: false, errors: [`unknown role "${role}"`] }
  const requiredKeys = spec.keys
  if (spec.header !== false) {
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
  }
  // Behaviour-shipping roles (`draft`, `methodology`, `agent-driver`) report named top-level fields
  // instead of an `items[]` findings array — the work they did lives in the tree or in the branch,
  // not in the envelope. They are validated here rather than left as `unknown role`, which is what
  // let a core discover a role it could not then validate.
  if (spec.kind === 'fields') {
    for (const key of requiredKeys) {
      if (!(key in envelope)) {
        errors.push(`missing "${key}"`)
      } else if (
        spec.nonEmpty &&
        (typeof envelope[key] !== 'string' || envelope[key].trim() === '')
      ) {
        errors.push(`"${key}" is not a non-empty string`)
      }
    }
    return { ok: errors.length === 0, errors }
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
    if (spec.nonEmpty) {
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
