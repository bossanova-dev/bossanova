import {
  refineVerify,
  VERIFY_HUMAN_REASON,
  VERIFY_PASS_WITHOUT_EVIDENCE,
} from './verify-contract.mjs'
export {
  VERIFY_PASS_WITHOUT_EVIDENCE,
  VERIFY_VERDICTS,
  VERIFY_HUMAN_REASON,
  VERIFY_WAIT_REASON,
  refineVerify,
  VERIFY_OUTCOME_REASONS,
  VERIFY_UNTRUSTED_TEXT_FIELDS,
  validateVerifyContext,
} from './verify-contract.mjs'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const nonEmptyString = (value) => typeof value === 'string' && value.trim() !== ''
const plainObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

const DEFAULT_ORDER = 100
export const DEFAULT_EXTENSION_ROOTS = ['.claude/skills', '.codex/skills']

// Extensions a published core ships with itself, beside its vendored toolbox:
// `<core>/extensions/<core>-<suffix>/EXTENSION.md`. They are scanned after the repo roots, so a
// repo-local extension with the same name replaces one, and `"builtinExtensions": false` in
// `.boss-skills.json` turns them all off. The file is EXTENSION.md, not SKILL.md, so no harness
// registers it as a standalone skill. From the canonical skills-toolbox/ copy this directory does
// not exist, which makes it a no-op there.
export const BUILTIN_EXTENSIONS_DIR = fileURLToPath(new URL('../extensions/', import.meta.url))
const BUILTIN_EXTENSION_FILE = 'EXTENSION.md'

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
function refineRelease(result) {
  const errors = []
  if (!['released', 'skipped', 'needs-human'].includes(result.action))
    errors.push('action must be released, skipped, or needs-human')
  if (!nonEmptyString(result.reason)) errors.push('reason must be a non-empty string')
  if (!(
    typeof result.ref === 'string' &&
    (/^[0-9a-f]{40}$/i.test(result.ref) || (result.action !== 'released' && result.ref === ''))
  ))
    errors.push('ref must be a 40-hex SHA (or empty when not released)')
  return errors
}

// A maintenance extension's outcomes. `filed` and `tracked` claim a tracker issue exists, so they
// must name at least one; the rest legitimately report none.
export const MAINTENANCE_OUTCOMES = Object.freeze([
  'clean',
  'tracked',
  'filed',
  'previewed',
  'skipped',
])
const MAINTENANCE_NEEDS_ISSUE = new Set(['filed', 'tracked'])

function refineMaintenance(result) {
  const errors = []
  if (!MAINTENANCE_OUTCOMES.includes(result.outcome))
    errors.push(`outcome must be one of ${MAINTENANCE_OUTCOMES.join(', ')}`)
  if (!nonEmptyString(result.summary)) errors.push('summary must be a non-empty string')
  if (!Array.isArray(result.issues)) {
    if ('issues' in result) errors.push('issues must be an array')
    return errors
  }
  result.issues.forEach((issue, idx) => {
    if (!nonEmptyString(issue)) errors.push(`issue ${idx} must be a non-empty string`)
  })
  if (MAINTENANCE_NEEDS_ISSUE.has(result.outcome) && !result.issues.some(nonEmptyString))
    errors.push(`${result.outcome} requires at least one issue identifier`)
  return errors
}

export const EXTENSION_ROLES = {
  release: {
    kind: 'fields',
    header: false,
    keys: ['action', 'reason', 'ref'],
    refine: refineRelease,
  },
  // A verify extension judges one PR head for the verify stage. `pass` must cite evidence and
  // `fail` must name a finding; `human` parks for a person with a named `human-review:<category>`
  // reason and must cite evidence; `wait` asks the verify stage to retry later with a kebab-case
  // reason and no findings; `abstain` is advisory. Outcome classification (crash, timeout,
  // malformed result, optional extensions) lives in `classifyVerifyOutcome` below.
  verify: { kind: 'fields', keys: ['verdict', 'evidence', 'findings'], refine: refineVerify },
  // A maintenance extension keeps a repo artifact current after a retro and reports what it did —
  // found nothing, saw it already tracked, filed an issue, previewed one (dry-run), or skipped.
  maintenance: {
    kind: 'fields',
    keys: ['outcome', 'summary', 'issues'],
    refine: refineMaintenance,
  },
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

// The run modes a core can discover in. An extension's optional `modes` marker key declares the
// subset it may run on; discovery called with a `mode` skips an extension that does not declare it.
// Eligibility is a declared property of the extension rather than a per-run judgement, because a
// core cannot know whether a third-party extension nests dispatches or asks the user.
export const EXTENSION_MODES = ['interactive', 'headless']

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
  // `modes` is present but not a comma-separated subset of EXTENSION_MODES: a failed declaration,
  // reported rather than read as "every mode".
  'invalid-modes': { deliberate: false },
  'invalid-release-marker': { deliberate: false },
  // The extension declares `modes` and the requested mode is not among them — the declaration
  // working as intended, so a core's ledger must not report it as a recoverable miss.
  'mode-not-declared': { deliberate: true },
  // A verify extension's `applies` is present but is not a repo-relative script path (absolute, a
  // `..` segment, empty or not a string): a failed declaration, never read as "always applies".
  'invalid-applies': { deliberate: false },
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

// Minimal YAML-frontmatter reader. Supports flat scalars, top-level literal/folded block scalars,
// and the single nested `x-boss-extension:` block this contract needs — no external YAML dep
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
  const lines = match[1].split('\n')
  for (let index = 0; index < lines.length; index += 1) {
    const raw = lines[index]
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
      const block = parseBlockHeader(top[2])
      if (block) {
        const parsed = readBlockScalar(lines, index + 1, block)
        data[top[1]] = parsed.value
        index = parsed.end - 1
      } else {
        data[top[1]] = coerce(top[2])
      }
      current = null
    }
  }
  return { data, body: match[2], hasFrontmatter: true }
}

function parseBlockHeader(value) {
  const match = /^([|>])([1-9+-]{0,2})(?:[ \t]+#.*)?$/.exec(value.trim())
  if (!match) return null
  const modifiers = match[2]
  const indentation = /[1-9]/.exec(modifiers)?.[0]
  const chomping = /[+-]/.exec(modifiers)?.[0] ?? ''
  if (modifiers.replace(/[1-9]/, '').replace(/[+-]/, '') !== '') return null
  if ((modifiers.match(/[1-9]/g)?.length ?? 0) > 1) return null
  if ((modifiers.match(/[+-]/g)?.length ?? 0) > 1) return null
  return {
    style: match[1],
    indentation: indentation ? Number(indentation) : null,
    chomping,
  }
}

function readBlockScalar(lines, start, block) {
  let indentation = block.indentation
  let firstContent = start
  while (firstContent < lines.length && lines[firstContent].trim() === '') firstContent += 1
  if (indentation === null && firstContent < lines.length) {
    const leading = /^ */.exec(lines[firstContent])[0].length
    if (leading > 0) indentation = leading
  }

  let end = start
  while (end < lines.length) {
    const raw = lines[end]
    if (raw.trim() === '') {
      end += 1
      continue
    }
    const leading = /^ */.exec(raw)[0].length
    if (indentation === null || leading < indentation) break
    end += 1
  }

  const contentIndentation = indentation ?? 0
  const content = lines
    .slice(start, end)
    .map((raw) =>
      raw.trim() === ''
        ? raw.slice(Math.min(contentIndentation, raw.length))
        : raw.slice(contentIndentation),
    )
  const rendered =
    block.style === '|'
      ? content.length === 0
        ? ''
        : `${content.join('\n')}\n`
      : foldBlockLines(content)
  return { value: chompBlockScalar(rendered, block.chomping), end }
}

function foldBlockLines(lines) {
  if (lines.length === 0) return ''
  let rendered = ''
  let sawNonEmpty = false
  for (let index = 0; index < lines.length - 1; index += 1) {
    const current = lines[index]
    const next = lines[index + 1]
    rendered += current
    if (current === '') {
      if (next === '' || !sawNonEmpty) rendered += '\n'
    } else {
      sawNonEmpty = true
      rendered += next !== '' && !/^\s/.test(current) && !/^\s/.test(next) ? ' ' : '\n'
    }
  }
  return `${rendered}${lines.at(-1)}\n`
}

function chompBlockScalar(value, chomping) {
  if (chomping === '+') return value
  const stripped = value.replace(/\n+$/, '')
  if (chomping === '-' || stripped === '') return stripped
  return `${stripped}\n`
}

// A quoted YAML scalar is a string by construction, so the quotes must be honoured
// BEFORE the numeric test rather than stripped ahead of it: `tagPrefix: "42"` has to stay
// the string "42", and stripping first would turn it into Number 42, which extensionMarker's
// string guards then drop silently. Only a bare, unquoted integer coerces.
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
  // Optional run-mode declaration (see EXTENSION_MODES). Absent means every mode and omits the
  // field, so an undeclared descriptor's JSON is byte-identical to what it was before the key
  // existed; an unusable value is also omitted here and reported by discovery as `invalid-modes`.
  const modes = parseModes(block.modes)
  if (modes) marker.modes = modes
  // Optional `optional: true` declaration, meaningful only for `role: verify`: a malfunction of an
  // optional extension abstains instead of failing (see classifyVerifyOutcome). Only boolean true or
  // the string "true" (any case) declares it; any other value means required, the stricter reading,
  // and omits the field so an undeclared descriptor's JSON is byte-identical to before the key.
  if (parseOptional(block.optional)) marker.optional = true
  // Optional `applies: <repo-relative script>`, meaningful only for `role: verify`: the zero-token
  // applicability check the verify gate runs before dispatching the extension. Absent omits the
  // field (byte-identical descriptor); an unusable value is omitted here and reported by discovery
  // as `invalid-applies`. On any other role the key is inert.
  if (block.role === 'verify') {
    const applies = parseApplies(block.applies)
    if (applies) marker.applies = applies
    // Optional `malfunctionReason: human-review:<category>`: the human-review reason the verify gate
    // parks with, beside `extension-failed`, when this extension malfunctions. Kept only when it has
    // the human-review reason shape; an invalid value is dropped WITHOUT skipping the extension, so a
    // typo still leaves the `extension-failed` park instead of skipping a judge (failing open).
    if (
      typeof block.malfunctionReason === 'string' &&
      VERIFY_HUMAN_REASON.test(block.malfunctionReason)
    )
      marker.malfunctionReason = block.malfunctionReason
  }
  if (block.role === 'release') {
    const environments = parseReleaseEnvironments(block.environments)
    if (environments) marker.environments = environments
    if (['tag', 'branch'].includes(block.state)) marker.state = block.state
    if (nonEmptyString(block.tagPrefix)) marker.tagPrefix = block.tagPrefix
  }
  return marker
}

function parseReleaseEnvironments(value) {
  if (typeof value !== 'string') return null
  const scalar = value.trim().replace(/^\[(.*)\]$/, '$1')
  const names = scalar.split(',').map((name) => name.trim().replace(/^(["'])(.*)\1$/, '$2'))
  if (names.some((name) => !/^[a-z0-9][a-z0-9._-]*$/.test(name))) return null
  return [...new Set(names)]
}

// A repo-relative script path with no `..` segment, normalised to forward slashes without a leading
// `./`. Returns null for anything else: not a string, empty, absolute (POSIX, UNC or a drive), or
// any segment that climbs out of the repository.
function parseApplies(value) {
  if (typeof value !== 'string') return null
  const raw = value.trim().replace(/\\/g, '/')
  if (raw === '' || raw.startsWith('/') || /^[A-Za-z]:/.test(raw)) return null
  const segments = raw.split('/').filter((segment) => segment !== '' && segment !== '.')
  if (segments.length === 0 || segments.includes('..')) return null
  return segments.join('/')
}

function parseOptional(value) {
  if (value === true) return true
  return typeof value === 'string' && value.trim().toLowerCase() === 'true'
}

// A `modes` scalar is a comma-separated subset of EXTENSION_MODES (the frontmatter reader supports
// scalars only). Returns the de-duplicated token list, or null when the value is absent or unusable:
// not a string, empty, an empty token, or a token outside EXTENSION_MODES.
function parseModes(value) {
  if (typeof value !== 'string') return null
  const tokens = value.split(',').map((token) => token.trim())
  if (tokens.some((token) => !EXTENSION_MODES.includes(token))) return null
  return [...new Set(tokens)]
}

function extensionRootsForDiscovery(config) {
  const roots = config && config.extensionRoots
  if (roots === undefined) return DEFAULT_EXTENSION_ROOTS
  if (
    !Array.isArray(roots) ||
    roots.length === 0 ||
    roots.some((candidate) => typeof candidate !== 'string' || candidate.trim() === '')
  ) {
    throw new Error('extensionRoots must be a non-empty array of non-empty strings')
  }
  return roots
}

function loadExtensionDiscoveryConfig(root) {
  const configPath = path.join(root, '.boss-skills.json')
  if (!fs.existsSync(configPath)) return {}
  return JSON.parse(fs.readFileSync(configPath, 'utf8'))
}

function assertContainedExtensionRoot(base, candidate, resolved) {
  const relative = path.relative(base, resolved)
  if (relative === '' || (!relative.startsWith('..') && !path.isAbsolute(relative))) return
  throw new Error(`extensionRoots entry escapes repository root: ${candidate}`)
}

export function resolveExtensionRoots(root, config = loadExtensionDiscoveryConfig(root)) {
  const base = path.resolve(root)
  const realBase = fs.realpathSync(base)
  const configuredRoots = extensionRootsForDiscovery(config)
  return configuredRoots
    .map((candidate) => {
      const resolved = path.resolve(base, candidate)
      assertContainedExtensionRoot(base, candidate, resolved)
      return resolved
    })
    .filter((candidate) => {
      let stat
      try {
        stat = fs.statSync(candidate)
      } catch {
        return false
      }
      if (!stat.isDirectory()) return false
      assertContainedExtensionRoot(realBase, candidate, fs.realpathSync(candidate))
      return true
    })
}

function discoverExtensionsInRoot({
  core,
  role,
  mode,
  skillsDir,
  seenNames,
  fileName = 'SKILL.md',
  builtin = false,
}) {
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
    if (seenNames.has(entry.name)) continue
    seenNames.add(entry.name)
    const skillPath = path.join(skillsDir, entry.name, fileName)
    if (!fs.existsSync(skillPath)) {
      skipped.push(skipEntry(entry.name, 'no-skill-md', `no ${fileName}`))
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
    // legitimate cross-role sibling (e.g. boss-plan draft vs plan-reviewer) and should not pollute
    // `skipped`. A typo'd/unknown role remains a misconfiguration and is recorded as a skip.
    if (typeof role === 'string' && role !== '' && marker.role !== role) {
      if (!KNOWN_EXTENSION_ROLES.has(marker.role)) {
        skipped.push(skipEntry(entry.name, 'wrong-role', `role "${marker.role}", not "${role}"`))
      }
      continue
    }
    if (marker.role === 'release') {
      const declared = parsed.data['x-boss-extension']
      if (
        (declared.environments !== undefined && marker.environments === undefined) ||
        (declared.state !== undefined && marker.state === undefined)
      ) {
        skipped.push(
          skipEntry(entry.name, 'invalid-release-marker', 'invalid release environments or state'),
        )
        continue
      }
    }
    // A PRESENT but unusable `modes` is a failed declaration, never silently widened to
    // "every mode".
    const declaredModes = parsed.data['x-boss-extension'].modes
    if (declaredModes !== undefined && marker.modes === undefined) {
      skipped.push(
        skipEntry(
          entry.name,
          'invalid-modes',
          `invalid "modes" (expected a comma-separated subset of ${EXTENSION_MODES.join(', ')})`,
        ),
      )
      continue
    }
    if (marker.role === 'verify') {
      const declaredApplies = parsed.data['x-boss-extension'].applies
      if (declaredApplies !== undefined && marker.applies === undefined) {
        skipped.push(
          skipEntry(
            entry.name,
            'invalid-applies',
            'invalid "applies" (expected a repo-relative script path with no ".." segment)',
          ),
        )
        continue
      }
    }
    if (mode !== undefined && marker.modes !== undefined && !marker.modes.includes(mode)) {
      skipped.push(
        skipEntry(
          entry.name,
          'mode-not-declared',
          `declares modes "${declaredModes}", not "${mode}"`,
        ),
      )
      continue
    }
    const descriptor = {
      name: entry.name,
      dir: path.join(skillsDir, entry.name),
      skillPath,
      role: marker.role,
      order: marker.order,
    }
    if (marker.modes !== undefined) descriptor.modes = marker.modes
    if (marker.optional === true) descriptor.optional = true
    if (marker.applies !== undefined) descriptor.applies = marker.applies
    if (marker.malfunctionReason !== undefined)
      descriptor.malfunctionReason = marker.malfunctionReason
    for (const key of ['environments', 'state', 'tagPrefix'])
      if (marker[key] !== undefined) descriptor[key] = marker[key]
    if (builtin) descriptor.builtin = true
    extensions.push(descriptor)
  }
  return { extensions, skipped }
}

/**
 * @param {{core: string, root?: string, role?: string, mode?: string, roots?: string[],
 *   builtinDir?: string}} options
 */
export function discoverExtensions({
  core,
  root,
  role,
  mode,
  roots,
  builtinDir = BUILTIN_EXTENSIONS_DIR,
}) {
  // An unknown requested mode is a caller bug, not a property of any extension, so it throws rather
  // than silently admitting every undeclared extension. `main` validates `--mode` before this.
  if (mode !== undefined && !EXTENSION_MODES.includes(mode)) {
    throw new Error(
      `unknown mode ${JSON.stringify(mode)}; valid modes are ${EXTENSION_MODES.join(', ')}`,
    )
  }
  const config =
    Array.isArray(roots) || root === undefined ? {} : loadExtensionDiscoveryConfig(root)
  const scanRoots = Array.isArray(roots)
    ? roots.map((candidate) => ({ skillsDir: path.resolve(candidate) }))
    : resolveExtensionRoots(root, config).map((skillsDir) => ({ skillsDir }))
  if (builtinDir && config.builtinExtensions !== false) {
    scanRoots.push({ skillsDir: builtinDir, fileName: BUILTIN_EXTENSION_FILE, builtin: true })
  }
  const extensions = []
  const skipped = []
  const seenNames = new Set()
  for (const scanRoot of scanRoots) {
    const discovered = discoverExtensionsInRoot({ core, role, mode, seenNames, ...scanRoot })
    extensions.push(...discovered.extensions)
    skipped.push(...discovered.skipped)
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

const withWarnings = (result, warnings) => (warnings.length > 0 ? { ...result, warnings } : result)

export function validateResult(envelope, role) {
  const errors = []
  const warnings = []
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
      // The caller already knows which role it dispatched (it names the output file), so a
      // mislabelled envelope is reported, not a reason to discard what the extension found.
      const message = `envelope role "${envelope.role}" does not match expected "${role}"`
      if (spec.nonEmpty || spec.kind === 'fields') errors.push(message)
      else warnings.push(message)
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
    if (typeof spec.refine === 'function') errors.push(...spec.refine(envelope))
    return withWarnings({ ok: errors.length === 0, errors }, warnings)
  }
  if (!Array.isArray(envelope.items)) {
    errors.push('items is not an array')
    return withWarnings({ ok: false, errors }, warnings)
  }
  // Findings roles: a malformed item is reported per item and left for the caller's own per-item
  // triage to normalize or drop. One bad item must not discard every other finding in the envelope.
  const itemProblems = spec.nonEmpty ? errors : warnings
  envelope.items.forEach((item, idx) => {
    if (!item || typeof item !== 'object') {
      itemProblems.push(`item ${idx} is not an object`)
      return
    }
    for (const key of requiredKeys) {
      if (!(key in item)) itemProblems.push(`item ${idx} missing "${key}"`)
    }
    // Roles whose items are a *claim of persistence* need every declared key to carry real text:
    // an empty `noteId` or `path` satisfies the `in` check above while proving nothing was
    // written. Roles whose items are findings (plan-reviewer) legitimately carry
    // blank fields, so the guard stays scoped rather than global.
    if (spec.nonEmpty) {
      for (const key of requiredKeys) {
        if (key in item && (typeof item[key] !== 'string' || item[key].trim() === '')) {
          errors.push(`item ${idx} "${key}" is not a non-empty string`)
        }
      }
    }
  })
  return withWarnings({ ok: errors.length === 0, errors }, warnings)
}

// Classifies one dispatched verify extension: `{result, timedOut, crashed, optional}` →
// `{outcome: ok|failed|abstain, reason, verdict}` (plus `humanReason` for a `human`, `waitReason`
// for a `wait`; a `wait` is a valid verdict even from an optional extension). First match
// wins; `optional: true` turns every malfunction into `abstain` while keeping its `reason`. When
// `humanReasons` is an array, a `human` reason outside it is a `malformed-result` malfunction that
// still carries the `humanReason` it named. Never throws.
export function classifyVerifyOutcome({
  result,
  timedOut = false,
  crashed = false,
  optional = false,
  humanReasons,
} = {}) {
  const malfunction = (reason) => ({
    outcome: optional === true ? 'abstain' : 'failed',
    reason,
    verdict: null,
  })
  if (timedOut === true) return malfunction('timed-out')
  if (crashed === true) return malfunction('crashed')
  if (plainObject(result) && result.ok === false) return malfunction('reported-failure')
  const validation = validateResult(result, 'verify')
  if (!validation.ok) {
    const onlyMissingEvidence =
      validation.errors.length === 1 && validation.errors[0] === VERIFY_PASS_WITHOUT_EVIDENCE
    return malfunction(onlyMissingEvidence ? 'pass-without-evidence' : 'malformed-result')
  }
  if (result.verdict === 'abstain') return { outcome: 'abstain', reason: 'abstain', verdict: null }
  if (result.verdict === 'human') {
    // A reason a person has no approval path for is a malformed result, not a park.
    if (Array.isArray(humanReasons) && !humanReasons.includes(result.reason))
      return { ...malfunction('malformed-result'), humanReason: result.reason }
    return { outcome: 'ok', reason: 'human', verdict: 'human', humanReason: result.reason }
  }
  if (result.verdict === 'wait')
    return { outcome: 'ok', reason: 'wait', verdict: 'wait', waitReason: result.reason }
  return { outcome: 'ok', reason: result.verdict, verdict: result.verdict }
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

// The flag surface, printed by `--help`. It used to exist only as the dispatch chain below, so the
// accepted flags could be learned only by reading this file's tail — and `--help` itself fell through
// to `unknown subcommand: --help` with exit 2 and no usage output at all.
const USAGE = `usage: skill-extensions.mjs <subcommand> [flags]

subcommands:
  discover --core <name> [--role <role>] [--mode <mode>] [--root <dir>] [--json]
      List the extensions of <core> found under the extension roots, in \`order\`.
      --role  restrict to one role: ${Object.keys(EXTENSION_ROLES).join(' | ')}
      --mode  the run mode discovering: ${EXTENSION_MODES.join(' | ')}; an extension whose
              \`modes\` omits it is skipped as mode-not-declared (absent = no mode filter)
      --root  scan below this directory instead of the current one
      --json  print the full {extensions, skipped} envelope instead of one TSV line per extension

  validate [--file <path>] [--role <role>]
      Validate an extension result envelope read from <path>, or from stdin when --file is absent.
      Prints {ok, errors[]} and exits non-zero when it is not ok.
      --role  the role whose result schema to validate against (see the list above)

  --help, -h
      Print this message.
`

export function main(argv) {
  const [subcommand, ...rest] = argv
  const args = parseArgs(rest)
  if (subcommand === '--help' || subcommand === '-h' || subcommand === 'help') {
    process.stdout.write(USAGE)
    return 0
  }
  if (subcommand === 'discover') {
    const core = args.core
    if (typeof core !== 'string' || core === '') {
      process.stderr.write('discover: --core <name> is required\n')
      return 2
    }
    const root = typeof args.root === 'string' ? args.root : process.cwd()
    const role = typeof args.role === 'string' ? args.role : undefined
    // Validated HERE, before scanning. An unrecognised role used to be recorded as a per-extension
    // `unknown-requested-role` skip and exit 0, which is the OPPOSITE diagnosis: a typo'd role reads
    // as "the extensions are misinstalled". The per-extension skip stays in `discoverExtensionsInRoot`
    // for programmatic callers of `discoverExtensions`, which never come through `main`.
    if (role !== undefined && !KNOWN_EXTENSION_ROLES.has(role)) {
      process.stderr.write(
        `discover: unknown --role ${JSON.stringify(role)}; valid roles are ${[...KNOWN_EXTENSION_ROLES].join(', ')}\n`,
      )
      return 2
    }
    const mode = typeof args.mode === 'string' ? args.mode : undefined
    if (args.mode !== undefined && (mode === undefined || !EXTENSION_MODES.includes(mode))) {
      process.stderr.write(
        `discover: unknown --mode ${JSON.stringify(args.mode)}; valid modes are ${EXTENSION_MODES.join(', ')}\n`,
      )
      return 2
    }
    const result = discoverExtensions({ core, root, role, mode })
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
  process.stderr.write(USAGE)
  return 2
}

import { isMainModule } from './main-module.mjs'

if (isMainModule(import.meta.url, { warn: () => {} })) {
  process.exit(main(process.argv.slice(2)))
}
