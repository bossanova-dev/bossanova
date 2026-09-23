// plan-scratch-paths.mjs
//
// The canonical declaration of the plan-scratch contract: where a planning run's
// scratch lives, and what every artifact in it is called.
//
// Two properties make this file the single source of truth rather than a fifth
// hand-maintained copy of a filename list:
//
//   1. **Every artifact is addressable.** A step that tells an agent to write
//      scratch to a path it has to invent produces a name no cleanup can match.
//      Each family below is a declared name, including the four that used to be
//      written through a placeholder: the guard safe source, the composed
//      description, the dependency-scan input, and the epic parent overview.
//   2. **Scratch is addressed by run, not by issue.** Two runs can plan the same
//      ticket at the same time, so an issue-id prefix does not separate this
//      run's scratch from a concurrent peer's — only the run id does. Everything
//      a run writes lives under `runScratchDir(runId)`, which makes cleanup one
//      recursive removal of a directory no peer can be inside.
//
// The published skill markdown cites these names as templates (`<ISSUE-ID>`,
// `<slug>`, …). `planScratchToken` reads a token in either form — templated as
// written in the payload, or concrete as written on disk — which is what lets a
// ratchet test assert that every `.linear-plans/…` token in the payload resolves
// to a declared family.

import { join } from 'node:path'
import { isMainModule } from './main-module.mjs'

/** The gitignored scratch root, relative to the repository root. */
export const PLAN_SCRATCH_ROOT = '.linear-plans'

/** Every run scratch directory is this prefix followed by the run id. */
export const RUN_SCRATCH_DIR_PREFIX = 'run-'

/**
 * @typedef {object} PlanScratchFamily
 * @property {string} family    stable key callers pass to `planScratchPath`
 * @property {string} template  the basename as the published payload cites it
 * @property {string} purpose   what the artifact is, for the reader of a diff
 * @property {(parts: Record<string, string>) => string} basename concrete builder
 * @property {RegExp} pattern   matches a normalized templated or concrete basename
 */

// A single path segment: either a normalized `<placeholder>` or a literal run of
// name characters. It deliberately excludes `.` so that a suffix such as
// `.image-guard-orig.md` can never be swallowed into the segment before it.
const SEG = '(?:§|[A-Za-z0-9][A-Za-z0-9_-]*)'

const seg = (s) => s.replace(/SEG/g, SEG)

/** Collapse every `<placeholder>` to one sentinel character. */
function normalizeToken(token) {
  let out = String(token)
  for (let i = 0; i < 8; i += 1) {
    const next = out.replace(/<[^<>]*>/g, '§')
    if (next === out) break
    out = next
  }
  return out
}

function fam(family, template, purpose, basename, pattern) {
  return { family, template, purpose, basename, pattern: new RegExp(seg(pattern)) }
}

/**
 * Every scratch artifact a planning run may write, most specific first —
 * `planScratchToken` returns the first family whose pattern matches, so a child
 * variant must be declared before the parent form it shares a suffix with.
 * @type {PlanScratchFamily[]}
 */
export const PLAN_SCRATCH_FAMILIES = [
  fam(
    'precheck',
    '<ISSUE-ID>.precheck.json',
    'the selected issue payload the idempotence precheck reads',
    ({ issueId }) => `${issueId}.precheck.json`,
    '^SEG\\.precheck\\.json$',
  ),
  fam(
    'draft-metadata',
    '<ISSUE-ID>.draft-metadata.json',
    'the bounded metadata a drafting dispatch returns',
    ({ issueId }) => `${issueId}.draft-metadata.json`,
    '^SEG\\.draft-metadata\\.json$',
  ),
  fam(
    // The LIVENESS artifact of an awaited dispatch: the worker (or a gate wrapper around one long
    // command it is blocked inside) touches this while it is still working, and the await side
    // reads it to tell a live dispatch from a dead one. It is declared here rather than invented at
    // the dispatch site for the same reason every other family is — a name outside this registry is
    // one no cleanup and no TTL reap can match. It is deliberately NOT the run-file sentinel: the
    // sentinel records a TERMINAL decision exactly once, so its mtime is the seed's and never moves
    // while the dispatch works.
    'dispatch-heartbeat',
    '<ISSUE-ID>.dispatch-heartbeat.json',
    'the liveness artifact an awaited dispatch touches while it is still working',
    ({ issueId }) => `${issueId}.dispatch-heartbeat.json`,
    '^SEG\\.dispatch-heartbeat\\.json$',
  ),
  fam(
    'premises',
    '<ISSUE-ID>.premises.json',
    'the premise list the plan depends on',
    ({ issueId }) => `${issueId}.premises.json`,
    '^SEG\\.premises\\.json$',
  ),
  fam(
    'premise-states',
    '<ISSUE-ID>.premise-states.json',
    'the live states those premises were re-read at',
    ({ issueId }) => `${issueId}.premise-states.json`,
    '^SEG\\.premise-states\\.json$',
  ),
  fam(
    'deps-input',
    '<ISSUE-ID>.deps-in.json',
    'the dependency-scan input (candidates + subject areas)',
    ({ issueId }) => `${issueId}.deps-in.json`,
    '^SEG\\.deps-in\\.json$',
  ),
  fam(
    'candidates',
    '<ISSUE-ID>.candidates.json',
    'the fetched dependency candidates, each with the full description the list op truncates',
    ({ issueId }) => `${issueId}.candidates.json`,
    '^SEG\\.candidates\\.json$',
  ),
  fam(
    'write-description-descriptor',
    '<ISSUE-ID>.write-description.json',
    'the descriptor the tracker write-description verb emits, read for its explicit outcome',
    ({ issueId }) => `${issueId}.write-description.json`,
    '^SEG\\.write-description\\.json$',
  ),
  fam(
    'epic-spec',
    '<ISSUE-ID>.epic-spec.json',
    'the serialized epic decomposition spec',
    ({ issueId }) => `${issueId}.epic-spec.json`,
    '^SEG\\.epic-spec\\.json$',
  ),
  fam(
    'attachment-headers',
    '<ISSUE-ID>.attachment-headers-<n>.json',
    'signed upload headers for one plan attachment PUT',
    ({ issueId, n }) => `${issueId}.attachment-headers-${n}.json`,
    '^SEG\\.attachment-headers-SEG\\.json$',
  ),
  fam(
    'child-image-guard-orig',
    '<ISSUE-ID>.child-<CHILD-ID>.image-guard-orig.md',
    "a child's raw-description snapshot for the image-parity guard",
    ({ issueId, childId }) => `${issueId}.child-${childId}.image-guard-orig.md`,
    '^SEG\\.child-SEG\\.image-guard-orig\\.md$',
  ),
  fam(
    'child-image-guard-new',
    '<ISSUE-ID>.child-<CHILD-ID>.image-guard-new.md',
    "a child's composed description, as the image-parity guard reads it",
    ({ issueId, childId }) => `${issueId}.child-${childId}.image-guard-new.md`,
    '^SEG\\.child-SEG\\.image-guard-new\\.md$',
  ),
  fam(
    'child-attachment-guard-orig',
    '<ISSUE-ID>.child-<CHILD-ID>.attachment-guard-orig.md',
    "a child's redacted safe source for the verbatim check",
    ({ issueId, childId }) => `${issueId}.child-${childId}.attachment-guard-orig.md`,
    '^SEG\\.child-SEG\\.attachment-guard-orig\\.md$',
  ),
  fam(
    'image-guard-orig',
    '<ISSUE-ID>.image-guard-orig.md',
    'the single raw-description snapshot taken before any rewrite',
    ({ issueId }) => `${issueId}.image-guard-orig.md`,
    '^SEG\\.image-guard-orig\\.md$',
  ),
  fam(
    'image-guard-new',
    '<ISSUE-ID>.image-guard-new.md',
    'the composed description, as the orchestrator hands it to the guard',
    ({ issueId }) => `${issueId}.image-guard-new.md`,
    '^SEG\\.image-guard-new\\.md$',
  ),
  fam(
    'image-guard-final',
    '<ISSUE-ID>.image-guard-final.md',
    "the exact bytes of the run's final description save, the write-back check's intended input",
    ({ issueId }) => `${issueId}.image-guard-final.md`,
    '^SEG\\.image-guard-final\\.md$',
  ),
  fam(
    'image-guard-stored',
    '<ISSUE-ID>.image-guard-stored.md',
    "the description read back from the tracker, the write-back check's stored input",
    ({ issueId }) => `${issueId}.image-guard-stored.md`,
    '^SEG\\.image-guard-stored\\.md$',
  ),
  fam(
    'attachment-guard-orig',
    '<ISSUE-ID>.attachment-guard-orig.md',
    'the redacted safe source the verbatim check is anchored against',
    ({ issueId }) => `${issueId}.attachment-guard-orig.md`,
    '^SEG\\.attachment-guard-orig\\.md$',
  ),
  fam(
    'description',
    '<ISSUE-ID>.description.md',
    'the description body a drafting dispatch composes, assembled as bytes',
    ({ issueId }) => `${issueId}.description.md`,
    '^SEG\\.description\\.md$',
  ),
  fam(
    'epic-overview',
    '<ISSUE-ID>.epic-overview.md',
    'the epic parent overview composed before the parent is repurposed',
    ({ issueId }) => `${issueId}.epic-overview.md`,
    '^SEG\\.epic-overview\\.md$',
  ),
  fam(
    'child-plan-rejected',
    '<PARENT>-child-<key>-<slug>.md.rejected',
    'a rejected child plan retained as a structure artifact',
    ({ parentId, key, slug }) => `${parentId}-child-${key}-${slug}.md.rejected`,
    '^SEG-child-SEG-SEG\\.md\\.rejected$',
  ),
  fam(
    'child-plan',
    '<PARENT>-child-<key>-<slug>.md',
    "one epic child's full plan text; the epic sentinel matches this basename exactly",
    ({ parentId, key, slug }) => `${parentId}-child-${key}-${slug}.md`,
    '^SEG-child-SEG-SEG\\.md$',
  ),
  fam(
    'plan-rejected',
    '<ISSUE-ID>-<slug>.md.rejected',
    'a rejected single-ticket plan retained as a structure artifact',
    ({ issueId, slug }) => `${issueId}-${slug}.md.rejected`,
    '^SEG-SEG\\.md\\.rejected$',
  ),
  fam(
    'plan',
    '<ISSUE-ID>-<slug>.md',
    'the single-ticket plan text',
    ({ issueId, slug }) => `${issueId}-${slug}.md`,
    '^SEG-SEG\\.md$',
  ),
]

const FAMILY_BY_KEY = new Map(PLAN_SCRATCH_FAMILIES.map((f) => [f.family, f]))

/** @returns {PlanScratchFamily|null} */
export function planScratchFamily(family) {
  return FAMILY_BY_KEY.get(family) ?? null
}

/**
 * This run's scratch directory. Cleanup removes exactly this directory, which is
 * why no peer run's artifact can be reached by it.
 * @param {string} runId
 * @param {string} [root]
 * @returns {string}
 */
export function runScratchDir(runId, root = PLAN_SCRATCH_ROOT) {
  const id = String(runId ?? '').trim()
  if (!id) throw new Error('plan-scratch-paths: runScratchDir requires a run id')
  if (id.includes('/') || id === '.' || id === '..') {
    throw new Error(`plan-scratch-paths: invalid run id ${JSON.stringify(runId)}`)
  }
  return join(root, `${RUN_SCRATCH_DIR_PREFIX}${id}`)
}

/**
 * Resolve one declared artifact inside this run's scratch directory.
 * @param {string} runId
 * @param {string} family  a `PLAN_SCRATCH_FAMILIES` key
 * @param {Record<string, string>} [parts]  template values (issueId, slug, …)
 * @param {string} [root]
 * @returns {string}
 */
export function planScratchPath(runId, family, parts = {}, root = PLAN_SCRATCH_ROOT) {
  const declared = planScratchFamily(family)
  if (!declared) {
    throw new Error(
      `plan-scratch-paths: undeclared scratch family ${JSON.stringify(family)} — ` +
        `declare it in PLAN_SCRATCH_FAMILIES rather than inventing a name`,
    )
  }
  const basename = declared.basename(parts)
  if (basename.includes('undefined') || basename.includes('/')) {
    throw new Error(
      `plan-scratch-paths: family ${family} produced an unusable basename ${JSON.stringify(basename)}`,
    )
  }
  return join(runScratchDir(runId, root), basename)
}

/** Expand one level of `{a,b}` alternation, as a shell would. */
function expandBraces(token) {
  const m = /\{([^{}]*)\}/.exec(token)
  if (!m) return [token]
  return m[1]
    .split(',')
    .flatMap((choice) =>
      expandBraces(token.slice(0, m.index) + choice + token.slice(m.index + m[0].length)),
    )
}

/**
 * Classify one `.linear-plans/…` path token exactly as the published payload
 * writes it. Brace alternations expand, so a token naming several artifacts is
 * accepted only when *every* branch resolves.
 * @param {string} token
 * @returns {{ok: true, kind: 'root'|'run-dir'|'artifact', families: string[]}
 *          |{ok: false, reason: string}}
 */
export function planScratchToken(token) {
  const raw = String(token ?? '').trim()
  if (!raw) return { ok: false, reason: 'empty token' }
  const branches = expandBraces(raw)
  const families = []
  let kind = 'artifact'
  for (const branch of branches) {
    const normalized = normalizeToken(branch).replace(/\/+$/, '')
    if (normalized === PLAN_SCRATCH_ROOT) {
      kind = 'root'
      continue
    }
    if (!normalized.startsWith(`${PLAN_SCRATCH_ROOT}/`)) {
      return { ok: false, reason: `not a plan-scratch path: ${branch}` }
    }
    const rest = normalized.slice(PLAN_SCRATCH_ROOT.length + 1)
    const runDirPattern = new RegExp(seg(`^${RUN_SCRATCH_DIR_PREFIX}SEG$`))
    const slash = rest.indexOf('/')
    const head = slash === -1 ? rest : rest.slice(0, slash)
    if (!runDirPattern.test(head)) {
      return {
        ok: false,
        reason:
          `${branch} is not addressed under ${PLAN_SCRATCH_ROOT}/${RUN_SCRATCH_DIR_PREFIX}<RUN-ID>/ — ` +
          'per-run scratch must be run-scoped so cleanup cannot reach a concurrent run',
      }
    }
    if (slash === -1) {
      kind = 'run-dir'
      continue
    }
    const basename = rest.slice(slash + 1)
    if (basename.includes('/')) {
      return { ok: false, reason: `${branch} nests below the run scratch directory` }
    }
    const declared = PLAN_SCRATCH_FAMILIES.find((f) => f.pattern.test(basename))
    if (!declared) {
      return {
        ok: false,
        reason:
          `${branch} resolves to no declared scratch family — ` +
          'add it to PLAN_SCRATCH_FAMILIES rather than inventing a name',
      }
    }
    families.push(declared.family)
  }
  return { ok: true, kind, families }
}

if (isMainModule(import.meta.url)) {
  const [cmd, ...rest] = process.argv.slice(2)
  if (cmd === 'families' || !cmd) {
    for (const f of PLAN_SCRATCH_FAMILIES) {
      process.stdout.write(`${f.family}\t${f.template}\t${f.purpose}\n`)
    }
  } else if (cmd === 'run-dir') {
    process.stdout.write(`${runScratchDir(rest[0])}\n`)
  } else if (cmd === 'path') {
    const [runId, family, ...pairs] = rest
    const parts = Object.fromEntries(
      pairs.map((p) => {
        const i = p.indexOf('=')
        return i === -1 ? [p, ''] : [p.slice(0, i), p.slice(i + 1)]
      }),
    )
    process.stdout.write(`${planScratchPath(runId, family, parts)}\n`)
  } else if (cmd === 'check') {
    let bad = 0
    for (const token of rest) {
      const r = planScratchToken(token)
      if (!r.ok) {
        process.stderr.write(`${r.reason}\n`)
        bad = 1
      }
    }
    process.exit(bad)
  } else {
    process.stderr.write(
      'usage: plan-scratch-paths.mjs [families | run-dir <runId> | path <runId> <family> [k=v…] | check <token…>]\n',
    )
    process.exit(2)
  }
}
