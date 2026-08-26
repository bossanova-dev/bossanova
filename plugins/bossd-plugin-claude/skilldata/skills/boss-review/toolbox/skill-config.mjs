// Declarative skill-config convention for the boss-* skills.
// A single surface (.boss-skills.json at the repo root) that holds the
// path-glob -> lens map, build/test commands, test-manifest path, headless
// env-detection signals, and adapter selection the skills used to hard-code.
// Node builtins ONLY: this module is vendored into the reusable toolbox
// and runs in dependency-free cron worktrees.

import { existsSync, readFileSync, statSync } from 'node:fs'
import { join, dirname, isAbsolute, parse as parsePath, posix } from 'node:path'

export const CONFIG_FILENAME = '.boss-skills.json'

/**
 * Convert a path glob into an anchored RegExp.
 * Supported: '**' (any chars incl. '/'), '**' + '/' (zero-or-more segments),
 * '*' (any chars except '/'), '?' (one non-'/' char). All other chars literal.
 */
export function globToRegExp(glob) {
  let re = '^'
  for (let i = 0; i < glob.length; i++) {
    const c = glob[i]
    if (c === '*') {
      if (glob[i + 1] === '*') {
        i++ // consume the second '*'
        if (glob[i + 1] === '/') {
          i++ // consume the '/'
          re += '(?:.*/)?' // '**/' -> zero-or-more path segments
        } else {
          re += '.*' // '**' -> any chars including '/'
        }
      } else {
        re += '[^/]*' // '*' -> any chars except '/'
      }
    } else if (c === '?') {
      re += '[^/]'
    } else if ('.+^${}()|[]\\'.includes(c)) {
      re += '\\' + c // escape regex metacharacters (note: '/' is not one)
    } else {
      re += c
    }
  }
  return new RegExp(re + '$')
}

/** Project-agnostic happy defaults — the fallback when no config file is present. */
export const DEFAULT_CONFIG = Object.freeze({
  // Deliberately NOT a copy of any one checkout's lensMap — the inverse of the pin this
  // block used to carry. The published cores install into every user's GLOBAL skill
  // directory, so these defaults are what a foreign repo inherits: a static, language-shaped
  // catalogue whose every glob starts with `**/` and therefore anchors to no top-level
  // directory. A repo's own path-anchored lenses (and its build commands, test manifest and
  // tracker identity) live ONLY in that checkout's own .boss-skills.json. The catalogue is
  // not gated on marker files because a lens self-selects by matching changed files: an
  // entry that matches nothing is simply inert, whereas gating could only subtract a lens
  // that would otherwise have been correct. Every entry still carries a non-empty
  // fallbackRubric, written for a FOREIGN repo, because bs-review substitutes it into
  // Phase 1 when the named skill can't be loaded (e.g. an operator-global skill like
  // impeccable off the author's machine) — the graceful-degradation path the skill relies on.
  // The `commands` and `test` blocks are absent by design: see detectRepoDefaults().
  lensMap: [
    {
      id: 'go',
      skill: 'golang-pro',
      glob: '**/*.go',
      fallbackRubric:
        'review through an inline Go rubric: idiomatic error handling and wrapping, goroutine/channel safety and leaks, interface and generics design, allocation on hot paths, and table-driven test coverage for new/changed logic',
    },
    {
      id: 'web',
      skill: 'impeccable',
      // Multi-extension matchers MUST use the globs array: globToRegExp escapes `{`/`}`
      // as literals, so `**/*.{ts,tsx}` would match a file literally named that.
      // Plain JS carries the same rubric as TS: a repo that never adopted TypeScript (and
      // dependency-free `.mjs` tooling anywhere) would otherwise match no lens at all.
      globs: ['**/*.ts', '**/*.tsx', '**/*.js', '**/*.jsx', '**/*.mjs', '**/*.cjs', '**/*.css'],
      fallbackRubric:
        'review through an inline React/TypeScript/web-UI rubric: component correctness, hook/effect races, accessibility, type-boundary cleanliness, dead/duplicated code',
    },
    {
      id: 'db',
      skill: 'database-review',
      globs: ['**/migrations/**', '**/*.sql'],
      fallbackRubric:
        'review through an inline schema/migration rubric: audit each changed column and table for naming and shape — snake_case identifiers, boolean columns prefixed is_/has_/should_/can_, counts as {noun}_count rather than num_*, timestamps as {event}_at and dates as {event}_on, foreign keys as {singular_table}_id — then check that each migration is reversible or explicitly documented as one-way, that an added column is nullable or carries a default so the deploy does not block on a table rewrite, that every new query predicate is indexed, and that no destructive drop or rename ships without a backfill and a compatibility window',
    },
    {
      id: 'api',
      skill: 'api-review',
      globs: ['**/*.proto', '**/*.graphql', '**/openapi*.yaml'],
      fallbackRubric:
        'review through an inline API-contract rubric: classify each change as additive, behavioural, or a wire break — an added field or procedure is safe, while a behavioural change (a client built against the old code observing a different response value, default, validation, ordering, error code, or side effect) and a wire break (a removed, renumbered, or retyped field) are compatibility events that must be handled deliberately by whatever versioning discipline this project already uses, and never shipped silently; where the project does version its API, require that the change add a version rather than mutate an existing one, that any compatibility shim target only the affected procedure, convert toward the older client rather than upgrading it, guard every type assertion so it cannot panic, and be a no-op for every other method, proved by a unit test that the shim fires one version back and is a no-op at the newest version, an end-to-end test through the real server, and a client version pin that stays consistent; treat internal-only behaviour no external client can observe, a security fix, and a bug fix that restores documented behaviour as deliberate exemptions rather than demanding a bump; then apply the schema hygiene the format supports: deliberate and specific error codes, edge-case validation, cancellation/deadline propagation, reserved tags or deprecation markers when a field is removed, and consistent snake_case field names with is_/has_/should_/can_ boolean prefixes',
    },
  ],
  // Opportunistic default review rounds — the registry boss-review's Phase D probes.
  //
  // The published core knows only the CAPABILITY id and the dispatch shape; a vendor skill name
  // appears ONLY here, in config, and never in core markdown. That split is deliberate and is the
  // seam the published-core foreign-skill gate scopes itself out of: it scans markdown, because a
  // config default naming a skill is the intended design rather than a leak. Keep it that way — a
  // core that learned the name below would fail that gate, and the remedy is always to reword the
  // core, never to widen the gate.
  //
  // `kind` decides how the capability is probed:
  //   - 'cross-agent' — a shell-queryable binary. The core resolves the opposite agent and probes
  //     it; anything other than a ready classification is a silent skip.
  //   - 'skill' — no shell-queryable fact exists, so the probe IS the dispatch: the worker attempts
  //     to load `skill` and returns a skip envelope rather than findings when it cannot.
  // Every absent capability is a silent, non-fatal skip: a ledger line, never a warning or BLOCKED.
  reviewDefaults: {
    rounds: [
      { capability: 'second-voice', kind: 'cross-agent' },
      { capability: 'code-review', kind: 'skill', skill: 'compound-engineering:ce-code-review' },
    ],
    // Maximum cumulative changed files since the most recent full review before a delta round
    // escalates back to full. Zero is valid and means every non-empty delta escalates.
    deltaFileThreshold: 20,
    // Maximum non-guaranteed round-role dispatches in one review run. Lower-only in config/env;
    // the guaranteed whole-branch pass is admitted independently of this cap.
    maxDispatchedRounds: 6,
    // Emergency switch for hosts that need to suppress delta review entirely without changing
    // round sequencing state. Only boolean true enables it; every other value resolves false.
    forceFull: false,
  },
  env: {
    headlessSignals: [
      { var: 'BOSS_CRON', equals: 'true' },
      { var: 'BS_HEADLESS', equals: '1' },
      { var: 'OPENCLAW_SESSION', present: true },
    ],
    headlessWhenNoTty: true,
  },
  adapters: { tracker: 'linear', publish: 'proof', sessionRunner: 'bossd' },
  // Repo-local boss-* extensions may be authored for either supported agent root. Discovery uses
  // this ordered candidate list and dedupes mirrored extensions by name, so a repo can carry both
  // generated mirrors without dispatching every extension twice.
  extensionRoots: ['.claude/skills', '.codex/skills'],
  // Concrete tracker / publish identity, keyed by the selected adapter. These blocks
  // deliberately DEFAULT TO EMPTY: the real values (MCP server name, team, project key, workflow
  // state names, publish bucket, public base URL) are repo-private data that lives ONLY in a
  // checkout's own .boss-skills.json, never as a literal in this vendored module — that is what
  // keeps the published cores project-agnostic. An unconfigured repo (no .boss-skills.json, or one
  // without a trackerConfig block for its selected tracker) therefore resolves to {} here and
  // isConfiguredForRepo() returns false, so a core invoked in such a repo self-disables cleanly
  // instead of demanding an MCP server that does not exist there.
  trackerConfig: {},
  publishConfig: {},
  // Plan storage is deliberately separate from publishConfig: proof artifacts
  // continue using the publish adapter even when implementation plans live on
  // the configured tracker.
  planStorage: { kind: 'tracker-attachment' },
  // Durable boss-review dispatch ledger location. Repo-relative so published
  // cores keep storage in the checkout that produced the review.
  reviewLedger: { dir: '.git/boss-review-ledgers' },
  // Versioned wire contract for the `##`-section plan description that boss-plan emits and
  // boss-build / bs-sweep-plan consume. `version` is the integer contract
  // version stamped in-band as `- Contract: v<N>` under `## Planning`; `sections` is the
  // ordered heading set as emitted, each classed `always` (every plan carries it),
  // conditional (`needs-human` / `open-questions`), or `optional` (recognised, never
  // required — see PLAN_SECTION_REQUIRED_KINDS). v1 == today's exact section set —
  // introducing the stamp IS the versioning; no sections were added, removed, or renamed.
  planContract: {
    version: 1,
    sections: [
      { heading: '## Summary', required: 'always' },
      { heading: '## Approach', required: 'always' },
      { heading: '## Key changes', required: 'always' },
      { heading: '## Testing', required: 'always' },
      { heading: '## Risks / unknowns', required: 'always' },
      { heading: '## Premises', required: 'optional' },
      { heading: '## Acceptance criteria', required: 'always' },
      { heading: '## Required proof', required: 'always' },
      { heading: '## Proof harness analysis', required: 'optional' },
      { heading: '## Why this needs a human', required: 'needs-human' },
      { heading: '## Open Questions', required: 'open-questions' },
      { heading: '## Planning', required: 'always' },
      { heading: '## Original notes', required: 'always' },
    ],
  },
})

// --- Loader: discovery, merge, validation ---------------------------------

export function findConfigFile(startDir) {
  let dir = startDir
  const { root } = parsePath(startDir)
  for (;;) {
    const candidate = join(dir, CONFIG_FILENAME)
    if (existsSync(candidate)) return candidate
    if (dir === root) return null
    const parent = dirname(dir)
    if (parent === dir) return null // fixed point (e.g. a relative startDir): stop, don't spin
    dir = parent
  }
}

export function mergeConfig(base, override) {
  const out = { ...base }
  for (const [k, v] of Object.entries(override || {})) {
    const b = base[k]
    if (Array.isArray(v)) {
      out[k] = v // arrays replace wholesale (predictable lensMap override)
    } else if (v && typeof v === 'object' && b && typeof b === 'object' && !Array.isArray(b)) {
      out[k] = { ...b, ...v } // objects shallow-merge per key
    } else {
      out[k] = v
    }
  }
  return out
}

export const ADAPTER_KINDS = new Set(['tracker', 'publish', 'sessionRunner'])

// How a default review round's capability is probed. 'cross-agent' is shell-queryable (resolve the
// opposite agent's binary and classify the probe); 'skill' is not, so its probe is the dispatch.
export const REVIEW_DEFAULT_ROUND_KINDS = new Set(['cross-agent', 'skill'])

export const REVIEW_DEFAULT_DELTA_FILE_THRESHOLD = DEFAULT_CONFIG.reviewDefaults.deltaFileThreshold
export const REVIEW_DEFAULT_MAX_DISPATCHED_ROUNDS =
  DEFAULT_CONFIG.reviewDefaults.maxDispatchedRounds

// The `required` classifications a planContract section may carry. `optional` means RECOGNISED but
// never required: the section may appear without tripping unknown-heading detection, and
// requiredPlanSections() still excludes it. It exists so a template the skill itself prescribes can
// be registered without newly requiring it of the plans already stamped against this contract.
export const PLAN_SECTION_REQUIRED_KINDS = new Set([
  'always',
  'needs-human',
  'open-questions',
  'optional',
])

export function validateConfig(config, source) {
  const fail = (msg) => {
    throw new Error(`skill-config: invalid config from ${source}: ${msg}`)
  }
  if (!Array.isArray(config.lensMap)) fail('lensMap must be an array')
  for (const rule of config.lensMap) {
    if (!rule || typeof rule !== 'object') fail('lensMap entries must be objects')
    if (typeof rule.id !== 'string' || rule.id.length === 0) {
      fail(`lensMap entry ${JSON.stringify(rule)} needs a non-empty string id`)
    }
    if (typeof rule.skill !== 'string' || rule.skill.length === 0) {
      // An empty skill makes skillForLens() return "" and dispatch nothing;
      // reject it here rather than accept an unusable lens.
      fail(`lensMap entry "${rule.id}" needs a non-empty string skill`)
    }
    const hasGlob = typeof rule.glob === 'string'
    const hasGlobs = Array.isArray(rule.globs)
    if (!hasGlob && !hasGlobs) {
      fail(`lensMap entry "${rule.id}" needs a glob or globs matcher`)
    }
    if (hasGlob && rule.glob.length === 0) {
      fail(`lensMap entry "${rule.id}" glob must be a non-empty string`)
    }
    if (
      hasGlobs &&
      (rule.globs.length === 0 || !rule.globs.every((g) => typeof g === 'string' && g.length > 0))
    ) {
      fail(`lensMap entry "${rule.id}" globs must be a non-empty array of non-empty strings`)
    }
    // Every lens must carry a real inline fallback rubric: bs-review substitutes it
    // into Phase 1 when the named skill can't be loaded (e.g. an operator-global
    // skill like impeccable off the author's machine), so a lens without one would
    // silently drop its specialist pass. Enforce it here rather than emit a
    // fallback-less lens from a consuming repo's config or the defaults.
    if (typeof rule.fallbackRubric !== 'string' || rule.fallbackRubric.length === 0) {
      fail(`lensMap entry "${rule.id}" needs a non-empty string fallbackRubric`)
    }
  }
  // reviewDefaults: the opportunistic default-round registry. Optional as a whole (a hand-built
  // config predating the block, or a repo that merged it away, resolves to no default rounds and
  // the accessor returns []), but a PRESENT block must be usable: reviewDefaultRounds() hands each
  // entry straight to a core's Phase D, which dereferences `capability`, switches on `kind`, and —
  // for kind:'skill' — dispatches `skill`. A malformed entry would otherwise surface as a default
  // round that silently probes nothing, which is indistinguishable from a capability being absent.
  if (config.reviewDefaults !== undefined) {
    if (
      !config.reviewDefaults ||
      typeof config.reviewDefaults !== 'object' ||
      Array.isArray(config.reviewDefaults)
    ) {
      fail('reviewDefaults must be an object when present')
    }
    if (config.reviewDefaults.rounds !== undefined) {
      if (!Array.isArray(config.reviewDefaults.rounds)) {
        fail('reviewDefaults.rounds must be an array')
      }
      // Phase D's duplicate suppression and its ledger lines are both keyed on `capability`, so
      // two entries sharing one id are not two rounds — they are one id whose ledger line cannot
      // say which entry produced it, and whose "covered by extension" drop silently applies to
      // both. Reject the collision at config time rather than let it read as a working registry.
      const seenCapabilities = new Set()
      for (const round of config.reviewDefaults.rounds) {
        if (!round || typeof round !== 'object' || Array.isArray(round)) {
          fail('reviewDefaults.rounds entries must be objects')
        }
        if (typeof round.capability !== 'string' || round.capability.length === 0) {
          fail(
            `reviewDefaults.rounds entry ${JSON.stringify(round)} needs a non-empty string capability`,
          )
        }
        if (!REVIEW_DEFAULT_ROUND_KINDS.has(round.kind)) {
          fail(
            `reviewDefaults.rounds entry "${round.capability}" kind must be one of ${[...REVIEW_DEFAULT_ROUND_KINDS].join(', ')}`,
          )
        }
        // A kind:'skill' entry with no skill names nothing to dispatch, so its round would probe
        // an undefined skill and skip every time — a capability quietly retired, not configured.
        if (
          round.kind === 'skill' &&
          (typeof round.skill !== 'string' || round.skill.length === 0)
        ) {
          fail(`reviewDefaults.rounds entry "${round.capability}" needs a non-empty string skill`)
        }
        if (seenCapabilities.has(round.capability)) {
          fail(`reviewDefaults.rounds entry "${round.capability}" duplicates an earlier capability`)
        }
        seenCapabilities.add(round.capability)
      }
    }
    if (
      config.reviewDefaults.deltaFileThreshold === undefined ||
      !Number.isInteger(config.reviewDefaults.deltaFileThreshold) ||
      config.reviewDefaults.deltaFileThreshold < 0
    ) {
      console.warn(
        `skill-config: ${source}: reviewDefaults.deltaFileThreshold must be a non-negative integer; using ${REVIEW_DEFAULT_DELTA_FILE_THRESHOLD}`,
      )
      config.reviewDefaults.deltaFileThreshold = REVIEW_DEFAULT_DELTA_FILE_THRESHOLD
    }
    if (
      config.reviewDefaults.maxDispatchedRounds === undefined ||
      !Number.isInteger(config.reviewDefaults.maxDispatchedRounds) ||
      config.reviewDefaults.maxDispatchedRounds < 1 ||
      config.reviewDefaults.maxDispatchedRounds > REVIEW_DEFAULT_MAX_DISPATCHED_ROUNDS
    ) {
      console.warn(
        `skill-config: ${source}: reviewDefaults.maxDispatchedRounds must be an integer in [1, ${REVIEW_DEFAULT_MAX_DISPATCHED_ROUNDS}]; using ${REVIEW_DEFAULT_MAX_DISPATCHED_ROUNDS}`,
      )
      config.reviewDefaults.maxDispatchedRounds = REVIEW_DEFAULT_MAX_DISPATCHED_ROUNDS
    }
    if (config.reviewDefaults.forceFull !== true) {
      config.reviewDefaults.forceFull = false
    }
  }
  // commands / test: optional. DEFAULT_CONFIG ships neither — a repo whose marker files
  // declare no recognised target legitimately resolves with no `commands` key at all (absent,
  // not {}), and a test-command manifest is a project artifact rather than a portable concept.
  // Only a present-but-malformed block fails here; the accessors return null on absence.
  if (config.commands !== undefined) {
    if (!config.commands || typeof config.commands !== 'object' || Array.isArray(config.commands)) {
      fail('commands must be an object when present')
    }
    for (const [key, val] of Object.entries(config.commands)) {
      // Accessors like moduleTestCommand() call .replace() on these; a non-string
      // override must fail here with a skill-config: error rather than throwing a
      // raw TypeError deep in a headless skill run.
      if (typeof val !== 'string' || val.length === 0) {
        fail(`commands.${key} must be a non-empty string`)
      }
    }
  }
  if (config.test !== undefined) {
    if (!config.test || typeof config.test !== 'object' || Array.isArray(config.test)) {
      fail('test must be an object when present')
    }
    // Non-empty, exactly like commands.*: manifestPath() treats "" as absent and returns null, so
    // an empty string is a config that silently does nothing rather than a configured manifest.
    if (
      config.test.manifestPath !== undefined &&
      (typeof config.test.manifestPath !== 'string' || config.test.manifestPath.length === 0)
    ) {
      fail('test.manifestPath must be a non-empty string when present')
    }
  }
  if (!Array.isArray(config.env?.headlessSignals)) fail('env.headlessSignals must be an array')
  for (const sig of config.env.headlessSignals) {
    // isHeadless() dereferences sig.var/sig.present/sig.equals; a non-object or
    // a signal without a string var throws a raw TypeError at run time instead
    // of the promised skill-config: error, so validate each entry's shape here.
    if (!sig || typeof sig !== 'object' || Array.isArray(sig)) {
      fail('env.headlessSignals entries must be objects')
    }
    if (typeof sig.var !== 'string' || sig.var.length === 0) {
      fail(`env.headlessSignals entry ${JSON.stringify(sig)} needs a non-empty string var`)
    }
    if (sig.present !== true && typeof sig.equals !== 'string') {
      fail(`env.headlessSignals entry "${sig.var}" needs present:true or a string equals`)
    }
  }
  if (typeof config.env.headlessWhenNoTty !== 'boolean') {
    // Documented as a boolean and gates headless control flow; a string like
    // "false" would coerce truthy in isHeadless(), so reject non-booleans here.
    fail('env.headlessWhenNoTty must be a boolean')
  }
  if (!config.adapters || typeof config.adapters !== 'object' || Array.isArray(config.adapters)) {
    fail('adapters must be an object')
  }
  for (const kind of ADAPTER_KINDS) {
    // adapterFor() promises a usable adapter name; an array or a null/non-string
    // selection yields undefined downstream, so require a non-empty string here.
    if (typeof config.adapters[kind] !== 'string' || config.adapters[kind].length === 0) {
      fail(`adapters.${kind} must be a non-empty string`)
    }
  }
  if (
    !Array.isArray(config.extensionRoots) ||
    config.extensionRoots.length === 0 ||
    !config.extensionRoots.every((root) => typeof root === 'string' && root.length > 0)
  ) {
    fail('extensionRoots must be a non-empty array of non-empty strings')
  }
  // trackerConfig / publishConfig: optional per-adapter identity blocks. Absent or
  // empty is the norm (DEFAULT_CONFIG ships them as {}), so only a present-but-malformed override
  // fails here — a raw TypeError deep in isConfiguredForRepo()/trackerConfigFor() would otherwise
  // surface as an opaque crash mid-run instead of the promised skill-config: error.
  if (
    config.trackerConfig == null ||
    typeof config.trackerConfig !== 'object' ||
    Array.isArray(config.trackerConfig)
  ) {
    fail('trackerConfig must be an object')
  }
  for (const [adapter, tc] of Object.entries(config.trackerConfig)) {
    if (!tc || typeof tc !== 'object' || Array.isArray(tc)) {
      fail(`trackerConfig.${adapter} must be an object`)
    }
    // mcpServer + team are the load-bearing identity a core needs to reach a real tracker;
    // isConfiguredForRepo() keys on them, so an entry missing either is not a usable config.
    for (const field of ['mcpServer', 'team']) {
      if (typeof tc[field] !== 'string' || tc[field].length === 0) {
        fail(`trackerConfig.${adapter}.${field} must be a non-empty string`)
      }
    }
    for (const field of ['teamKey', 'workspace']) {
      if (field in tc && (typeof tc[field] !== 'string' || tc[field].length === 0)) {
        fail(`trackerConfig.${adapter}.${field} must be a non-empty string when present`)
      }
    }
    for (const field of ['states', 'labels', 'githubLabels']) {
      if (!(field in tc)) continue
      if (!tc[field] || typeof tc[field] !== 'object' || Array.isArray(tc[field])) {
        fail(`trackerConfig.${adapter}.${field} must be an object when present`)
      }
      for (const [role, name] of Object.entries(tc[field])) {
        if (typeof name !== 'string' || name.length === 0) {
          fail(`trackerConfig.${adapter}.${field}.${role} must be a non-empty string`)
        }
      }
    }
    // followUpLabels: the verbatim label set boss-review's follow-up-issue prompt applies. A list
    // (not a role map) so the choice of which labels lives in config, not the published-core renderer.
    if ('followUpLabels' in tc) {
      if (
        !Array.isArray(tc.followUpLabels) ||
        tc.followUpLabels.length === 0 ||
        !tc.followUpLabels.every((l) => typeof l === 'string' && l.length > 0)
      ) {
        fail(
          `trackerConfig.${adapter}.followUpLabels must be a non-empty array of non-empty strings`,
        )
      }
    }
  }
  if (
    config.publishConfig == null ||
    typeof config.publishConfig !== 'object' ||
    Array.isArray(config.publishConfig)
  ) {
    fail('publishConfig must be an object')
  }
  for (const [adapter, pc] of Object.entries(config.publishConfig)) {
    if (!pc || typeof pc !== 'object' || Array.isArray(pc)) {
      fail(`publishConfig.${adapter} must be an object`)
    }
    for (const field of ['bucket', 'baseUrl']) {
      if (typeof pc[field] !== 'string' || pc[field].length === 0) {
        fail(`publishConfig.${adapter}.${field} must be a non-empty string`)
      }
    }
  }
  if (
    !config.planStorage ||
    typeof config.planStorage !== 'object' ||
    Array.isArray(config.planStorage)
  ) {
    fail('planStorage must be an object')
  }
  if (config.planStorage.kind === 'r2') {
    console.warn(
      `skill-config: ${source}: planStorage.kind="r2" is deprecated and ignored; using "tracker-attachment"`,
    )
    config.planStorage = { kind: 'tracker-attachment' }
  } else if (config.planStorage.kind !== 'tracker-attachment') {
    fail('planStorage.kind must be "tracker-attachment"')
  }
  if (
    !config.reviewLedger ||
    typeof config.reviewLedger !== 'object' ||
    Array.isArray(config.reviewLedger)
  ) {
    fail('reviewLedger must be an object')
  }
  if (typeof config.reviewLedger.dir !== 'string' || config.reviewLedger.dir.length === 0) {
    fail('reviewLedger.dir must be a non-empty string')
  }
  if (config.reviewLedger.dir.includes('\\')) {
    fail('reviewLedger.dir must use POSIX separators')
  }
  if (isAbsolute(config.reviewLedger.dir)) {
    fail('reviewLedger.dir must be repo-relative')
  }
  const normalizedReviewLedgerDir = posix.normalize(config.reviewLedger.dir)
  if (normalizedReviewLedgerDir === '..' || normalizedReviewLedgerDir.startsWith('../')) {
    fail('reviewLedger.dir must stay within the repository')
  }
  // planContract is the extension point this feature exists for (a consuming repo overrides
  // the section set), so a malformed override must fail here with a skill-config: error rather
  // than a raw TypeError deep in planSections()/requiredPlanSections()/validatePlanDescription().
  if (
    !config.planContract ||
    typeof config.planContract !== 'object' ||
    Array.isArray(config.planContract)
  ) {
    fail('planContract must be an object')
  }
  if (!Number.isInteger(config.planContract.version) || config.planContract.version < 1) {
    fail('planContract.version must be a positive integer')
  }
  if (!Array.isArray(config.planContract.sections) || config.planContract.sections.length === 0) {
    fail('planContract.sections must be a non-empty array')
  }
  for (const section of config.planContract.sections) {
    if (!section || typeof section !== 'object' || Array.isArray(section)) {
      fail('planContract.sections entries must be objects')
    }
    if (typeof section.heading !== 'string' || section.heading.length === 0) {
      fail(
        `planContract.sections entry ${JSON.stringify(section)} needs a non-empty string heading`,
      )
    }
    // requiredPlanSections() filters on `required === 'always'`; a typo like 'alway' would
    // silently drop the section from the required set, quietly weakening validation. Reject
    // any classification outside the known set here.
    if (!PLAN_SECTION_REQUIRED_KINDS.has(section.required)) {
      fail(
        `planContract.sections entry "${section.heading}" required must be one of ${[...PLAN_SECTION_REQUIRED_KINDS].join(', ')}`,
      )
    }
  }
}

// --- Detected happy defaults ----------------------------------------------
//
// DEFAULT_CONFIG deliberately ships no `commands` block (see above), so a repo without a
// .boss-skills.json would otherwise hand every core a null build/lint/test command. This layer
// recovers the obvious ones from what the repo ITSELF declares — a Makefile target, a package.json
// script, a Cargo/Go module — rather than from what any one checkout happens to use.
//
// Two rules keep it honest:
//   1. Read DECLARED TARGETS, not marker presence. A Makefile with no `lint:` target yields no
//      lint command; guessing `make lint` there would hand a core a command that fails.
//   2. First writer wins, in the fixed order Makefile -> package.json -> Cargo.toml -> go.mod.
//      A Makefile is the repo's own front door, so where one exists it outranks the toolchain
//      underneath it; the rest fill only the keys it left empty.
// `commands.testModule` is NEVER detected: per-module test targets are a repo-shaped convention,
// not a language fact, so its absence stays the honest answer outside a repo that configures it.

/** Command keys this layer can detect. `testModule` is deliberately absent. */
const DETECTED_COMMAND_KEYS = ['build', 'lint', 'format', 'test']

// A Makefile rule head: everything before `:` / `::`, excluding `:=` / `::=` assignments. One head
// can declare SEVERAL targets (`build lint:`), so the names are captured as a group and split.
const MAKE_RULE_RE = /^([A-Za-z0-9][^:=]*?)\s*:{1,2}(?![=:])(.*)$/
/** A single target name, once the head is split on whitespace. */
const MAKE_TARGET_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9_./-]*$/
// `build: CFLAGS=-O2` scopes a variable to a target; on its own it declares no rule and no recipe,
// so `make build` there fails. Only a head NOT followed by an assignment counts as a declaration.
const MAKE_TARGET_VAR_RE = /^\s*[A-Za-z_][A-Za-z0-9_.]*\s*[:+?]?=/

// `define NAME` ... `endef` brackets a multi-line variable body. Its lines are TEXT, not rules —
// a `build:` inside one is a template make only sees after an `$(eval)` that may never happen, so
// scanning it would emit `make build` for a target the repo does not declare.
const MAKE_DEFINE_RE = /^\s*define\s/
const MAKE_ENDEF_RE = /^\s*endef\b/

// `ifeq`/`ifneq`/`ifdef`/`ifndef` ... `endif` brackets a conditional body. Whether that body is
// live depends on variables this static reader does not evaluate, so a target declared only there
// is not a target the repo unconditionally declares — reporting it would emit e.g. `make test` for
// a repo where the branch is false. Skipped wholesale, exactly like a `define` body.
// An `else` / `else ifeq (...)` line opens no new conditional: the same `endif` closes it.
// The lookahead, not `\b`, is what make requires: `\b` also matches before a `-`, so the legal
// target `ifeq-check:` would open a conditional that swallows the rest of the file (and
// `endif-foo:` would close one early, reopening scanning inside a conditional body).
const MAKE_COND_RE = /^\s*(?:ifeq|ifneq|ifdef|ifndef)(?=[\s(]|$)/
const MAKE_ENDIF_RE = /^\s*endif(?=[\s(]|$)/

function readTextFile(path) {
  try {
    return readFileSync(path, 'utf8')
  } catch {
    return null // unreadable is indistinguishable from absent for detection purposes
  }
}

/**
 * The declared Makefile targets in `dir`, as a Set (empty when there is no makefile).
 *
 * This is a static reader, not a make implementation, so it resolves every gap the same way — by
 * under-detecting rather than reporting a target the repo may not actually declare:
 *   - **Conditionals.** A target reachable only inside an `ifeq`/`ifneq`/`ifdef`/`ifndef` body
 *     (including its `else` branches) is NOT reported: whether that branch is live means evaluating
 *     variables, and emitting `make test` for a repo where the condition is false hands a core a
 *     command that fails. A target declared after the closing `endif` is reported as usual.
 *   - **`include`d makefiles.** Only the top-level file is read, so a target defined in an included
 *     fragment is missed. That under-detects too.
 */
function makefileTargets(dir) {
  const out = new Set()
  // GNU make's own lookup order, not alphabetical: GNUmakefile, then makefile, then Makefile.
  // Scanning the wrong one in a repo carrying two would report targets make never reads.
  for (const name of ['GNUmakefile', 'makefile', 'Makefile']) {
    const text = readTextFile(join(dir, name))
    if (text === null) continue
    let inDefine = false
    let condDepth = 0 // nesting depth of open ifeq/ifneq/ifdef/ifndef blocks
    // make joins a line ending in `\` with the next before parsing, so a rule head split across
    // two lines is still one declaration. Recipe lines stay indented and so still never match.
    for (const line of text.replace(/\\\n/g, ' ').split('\n')) {
      // An unterminated `define` or `ifeq` swallows the rest of the file, which under-detects
      // rather than over-detects — the safe direction, and the same file make itself would reject.
      if (inDefine) {
        if (MAKE_ENDEF_RE.test(line)) inDefine = false
        continue
      }
      if (condDepth > 0) {
        // Nested conditionals must not let the first `endif` reopen scanning, and an `else` (or
        // `else ifeq ...`) branch is still inside the same conditional, so it changes no depth.
        if (MAKE_COND_RE.test(line)) condDepth++
        else if (MAKE_ENDIF_RE.test(line)) condDepth--
        continue
      }
      if (MAKE_DEFINE_RE.test(line)) {
        inDefine = true
        continue
      }
      if (MAKE_COND_RE.test(line)) {
        condDepth = 1
        continue
      }
      const m = MAKE_RULE_RE.exec(line)
      if (!m || MAKE_TARGET_VAR_RE.test(m[2])) continue
      for (const target of m[1].split(/\s+/)) {
        if (MAKE_TARGET_NAME_RE.test(target)) out.add(target)
      }
    }
    break // one makefile per directory; the one make itself would read wins
  }
  return out
}

/** The declared package.json scripts in `dir`, as an object (empty when absent/malformed). */
function packageScripts(dir) {
  const text = readTextFile(join(dir, 'package.json'))
  if (text === null) return {}
  let parsed
  try {
    parsed = JSON.parse(text)
  } catch {
    return {} // a broken package.json detects nothing rather than throwing at load time
  }
  const scripts = parsed && typeof parsed === 'object' ? parsed.scripts : null
  return scripts && typeof scripts === 'object' && !Array.isArray(scripts) ? scripts : {}
}

/**
 * The package manager a JS repo declares via its lockfile (npm is the safe default).
 *
 * `bun.lockb` is bun's binary lockfile and `bun.lock` its newer text one; either names bun. The
 * probe order is fixed rather than meaningful — a repo carrying two lockfiles is already ambiguous,
 * and a stable answer beats a clever one.
 */
function packageManager(dir) {
  if (existsSync(join(dir, 'pnpm-lock.yaml'))) return 'pnpm'
  if (existsSync(join(dir, 'yarn.lock'))) return 'yarn'
  if (existsSync(join(dir, 'bun.lockb')) || existsSync(join(dir, 'bun.lock'))) return 'bun'
  return 'npm'
}

/**
 * Detect a repo's build/lint/format/test commands from the files it declares.
 *
 * Pure and side-effect-free apart from reading the marker files in `cwd` itself: it never walks
 * the tree, never executes anything, and never mutates DEFAULT_CONFIG. Returns a PARTIAL config
 * — `{}` when nothing is declared, otherwise `{commands: {...}}` — so it composes through the
 * ordinary mergeConfig() layering with no special-casing at the call site.
 *
 * @param {{cwd?: string, keys?: string[]}} [opts] `cwd` is the directory to inspect (the repo root,
 *   not a nested dir); `keys` narrows detection to the command keys still wanted, and an empty
 *   intersection with the detectable set returns `{}` without reading a single marker file.
 * @returns {{commands?: Record<string,string>}}
 */
export function detectRepoDefaults({ cwd = process.cwd(), keys = DETECTED_COMMAND_KEYS } = {}) {
  const wanted = new Set(keys.filter((key) => DETECTED_COMMAND_KEYS.includes(key)))
  if (wanted.size === 0) return {} // nothing to learn: do no marker-file I/O at all
  const commands = {}
  const set = (key, value) => {
    if (!wanted.has(key)) return
    if (!(key in commands) && typeof value === 'string' && value.length > 0) commands[key] = value
  }

  // 1. Makefile — the repo's own front door.
  const targets = makefileTargets(cwd)
  if (targets.size > 0) {
    for (const key of DETECTED_COMMAND_KEYS) {
      if (targets.has(key)) set(key, `make ${key}`)
    }
    if (targets.has('fmt')) set('format', 'make fmt')
  }

  // 2. package.json scripts, run through whichever manager the lockfile names.
  const scripts = packageScripts(cwd)
  const scriptNames = Object.keys(scripts)
  if (scriptNames.length > 0) {
    const pm = packageManager(cwd)
    for (const key of DETECTED_COMMAND_KEYS) {
      if (typeof scripts[key] === 'string' && scripts[key].length > 0) set(key, `${pm} run ${key}`)
    }
    if (typeof scripts.fmt === 'string' && scripts.fmt.length > 0) set('format', `${pm} run fmt`)
  }

  // 3. Cargo — the subcommands below are part of the toolchain a Cargo.toml declares.
  if (existsSync(join(cwd, 'Cargo.toml'))) {
    set('build', 'cargo build')
    set('lint', 'cargo clippy')
    set('format', 'cargo fmt')
    set('test', 'cargo test')
  }

  // 4. Go — same reasoning; no lint, because no linter ships with the toolchain.
  if (existsSync(join(cwd, 'go.mod'))) {
    set('build', 'go build ./...')
    set('format', 'go fmt ./...')
    set('test', 'go test ./...')
  }

  return Object.keys(commands).length > 0 ? { commands } : {}
}

export function loadSkillConfig({ cwd = process.cwd() } = {}) {
  const file = findConfigFile(cwd)
  let user = {}
  if (file) {
    let raw
    try {
      raw = readFileSync(file, 'utf8')
    } catch (e) {
      throw new Error(`skill-config: cannot read ${file}: ${e.message}`)
    }
    try {
      user = JSON.parse(raw)
    } catch (e) {
      throw new Error(`skill-config: ${file} is not valid JSON: ${e.message}`)
    }
    if (!user || typeof user !== 'object' || Array.isArray(user)) {
      // Valid JSON that is null, an array, or a primitive would merge as an
      // empty override and silently fall back to defaults — reject it loudly so
      // a broken consuming-repo config never masquerades as Bossanova defaults.
      throw new Error(`skill-config: ${file} must contain a JSON object`)
    }
  }
  // Detection fills only what nothing else declares, PER KEY: a config that declares just
  // `commands.testModule` still gets the detected build/lint/format/test, matching the documented
  // shallow-merge semantics rather than losing all four to an all-or-nothing short-circuit. When
  // the config already declares every detectable key, no key is wanted and detectRepoDefaults()
  // returns without reading a marker file — a fully-configured repo still does no I/O at all.
  // Composition order is DEFAULT_CONFIG < detected < user: repo config beats detection, and
  // detection beats nothing. Detection is anchored at the config file's directory (the repo root)
  // when there is one, and at `cwd` otherwise — never at a nested dir under a discovered config.
  const declared =
    user.commands && typeof user.commands === 'object' && !Array.isArray(user.commands)
      ? user.commands
      : {}
  const detected = detectRepoDefaults({
    cwd: file ? dirname(file) : cwd,
    keys: DETECTED_COMMAND_KEYS.filter((key) => !(key in declared)),
  })
  const merged = mergeConfig(mergeConfig(DEFAULT_CONFIG, detected), user)
  validateConfig(merged, file || '(defaults)')
  return merged
}

// --- Accessors: lenses, commands, manifest, headless, adapters ------------

function ruleGlobs(rule) {
  return Array.isArray(rule.globs) ? rule.globs : [rule.glob]
}

export function lensesForFile(config, filePath) {
  return config.lensMap
    .filter((rule) => ruleGlobs(rule).some((g) => globToRegExp(g).test(filePath)))
    .map((rule) => rule.id)
}

export function detectChangeTypes(files, lensMap = DEFAULT_CONFIG.lensMap) {
  const list = Array.isArray(files) ? files : []
  const cfg = { lensMap }
  const out = {}
  for (const rule of lensMap) out[rule.id] = false
  for (const f of list) {
    for (const id of lensesForFile(cfg, f)) out[id] = true
  }
  return out
}

export function skillForLens(config, id) {
  const rule = config.lensMap.find((r) => r.id === id)
  return rule ? rule.skill : null
}

/**
 * The opportunistic default review rounds this repo resolves — `[]` when the config carries none.
 *
 * `[]` is the honest "this repo default-runs no extra round", and it is what a core's Phase D
 * treats as an empty (silent, non-fatal) phase. Returning the array rather than throwing is what
 * lets a config predating the block, or one that deliberately merged it away, degrade to no
 * default rounds instead of failing a review run.
 * @returns {{capability: string, kind: 'cross-agent'|'skill', skill?: string}[]}
 */
export function reviewDefaultRounds(config) {
  const rounds = config?.reviewDefaults?.rounds
  return Array.isArray(rounds) ? rounds : []
}

export function reviewDeltaDefaults(config) {
  const defaults = config?.reviewDefaults || {}
  const threshold =
    Number.isInteger(defaults.deltaFileThreshold) && defaults.deltaFileThreshold >= 0
      ? defaults.deltaFileThreshold
      : REVIEW_DEFAULT_DELTA_FILE_THRESHOLD
  return {
    deltaFileThreshold: threshold,
    forceFull: defaults.forceFull === true,
  }
}

export function reviewMaxDispatchedRoundDefault(config) {
  const value = config?.reviewDefaults?.maxDispatchedRounds
  return Number.isInteger(value) && value >= 1 && value <= REVIEW_DEFAULT_MAX_DISPATCHED_ROUNDS
    ? value
    : REVIEW_DEFAULT_MAX_DISPATCHED_ROUNDS
}

export function reviewLedgerConfig(config) {
  return config?.reviewLedger || DEFAULT_CONFIG.reviewLedger
}

/**
 * A configured/detected command string, or null when this repo declares none. `null` is the
 * honest "no command is known here" — a core that needs one falls back to its own discovery
 * prose (project instructions, CI config, command files) rather than running a wrong target.
 * @returns {string|null}
 */
export function command(config, key) {
  const value = config.commands?.[key]
  return typeof value === 'string' && value.length > 0 ? value : null
}

/**
 * The per-module test command with `{module}` substituted, or null when the repo declares no
 * `commands.testModule`. `testModule` is a repo-shaped convention rather than a language fact,
 * so it is never detected — absence is the norm outside a repo that configures it.
 * @returns {string|null}
 */
export function moduleTestCommand(config, module) {
  const template = config.commands?.testModule
  return typeof template === 'string' && template.length > 0
    ? template.replace('{module}', module)
    : null
}

/**
 * The configured test-command manifest path, or null when this repo has none (the default —
 * a manifest is a project artifact, not a portable concept).
 * @returns {string|null}
 */
export function manifestPath(config) {
  const value = config.test?.manifestPath
  return typeof value === 'string' && value.length > 0 ? value : null
}

export function isHeadless(config, env = process.env, { isTTY = process.stdin.isTTY } = {}) {
  const signalHit = config.env.headlessSignals.some((s) => {
    if (s.present) return env[s.var] != null && env[s.var] !== ''
    return env[s.var] === s.equals
  })
  if (signalHit) return true
  return Boolean(config.env.headlessWhenNoTty) && !isTTY
}

export function adapterFor(config, kind) {
  if (!ADAPTER_KINDS.has(kind)) {
    throw new Error(`skill-config: unknown adapter kind "${kind}"`)
  }
  return config.adapters[kind]
}

/** Ordered repo-local skill roots to scan for boss-* extensions. */
export function extensionRootsFor(config) {
  return Array.isArray(config?.extensionRoots)
    ? config.extensionRoots
    : DEFAULT_CONFIG.extensionRoots
}

// --- Per-adapter identity resolution + configured-repo probe ---------------

/**
 * The concrete tracker identity block for the selected (or named) tracker adapter, or null when
 * the repo carries none. Defaults the adapter to the tracker selection in `adapters.tracker` so a
 * core can call `trackerConfigFor(config)` without re-deriving it.
 * @returns {{ mcpServer: string, team: string, teamKey?: string, workspace?: string, states?: Record<string,string> } | null}
 */
export function trackerConfigFor(config, adapter = adapterFor(config, 'tracker')) {
  const tc = config.trackerConfig?.[adapter]
  return tc && typeof tc === 'object' ? tc : null
}

function trackerRoleName(config, field, role, required = true) {
  const adapter = adapterFor(config, 'tracker')
  const roleMap = trackerConfigFor(config, adapter)?.[field]
  const hasRole =
    roleMap && typeof roleMap === 'object' && Object.prototype.hasOwnProperty.call(roleMap, role)
  if (!hasRole && !required) return null
  const value = hasRole ? roleMap[role] : undefined
  if (typeof value !== 'string' || value.length === 0) {
    throw new Error(
      `skill-config: trackerConfig.${adapter}.${field}.${role} must be configured as a non-empty string`,
    )
  }
  return value
}

/** Resolve a configured Linear workflow-state display name by its stable role. */
export function stateName(config, role) {
  return trackerRoleName(config, 'states', role)
}

/**
 * Resolve the configured workflow-state display names into the state-name -> role map
 * consumed by dependency planning.
 */
export function stateRolesFor(config) {
  const roles = ['unplanned', 'planned', 'inProgress', 'inReview']
  const out = {}
  for (const role of roles) out[stateName(config, role)] = role
  return out
}

/** Resolve a configured Linear issue-label display name by its stable role. */
export function labelName(config, role) {
  return trackerRoleName(config, 'labels', role)
}

/**
 * Resolve an optional Linear issue-label display name by its stable role.
 * Content-taxonomy labels are legitimately unmapped in some repos, so absence returns null.
 * @returns {string|null}
 */
export function optionalLabelName(config, role) {
  return trackerRoleName(config, 'labels', role, false)
}

/** Resolve a configured GitHub PR-label display name by its stable role. */
export function githubLabelName(config, role) {
  return trackerRoleName(config, 'githubLabels', role)
}

// --- Skill prose tracker-config claim scanner ------------------------------

const CLAIM_IGNORE_MARKER = '<!-- skill-config-claim: ignore -->'
const ROLE_TOKEN = '[A-Za-z][A-Za-z0-9_-]*'
const ROLE_NAME = '(?:`|\\*\\*)?(?<role>' + ROLE_TOKEN + ')(?:`|\\*\\*)?'
const ROLE_PROSE_PATTERNS = [
  {
    pattern: new RegExp(
      `\\b(?:the\\s+)?${ROLE_NAME}\\s+role\\s+(?:(?:is|was)\\s+)?(?:deliberately\\s+unmapped|unmapped|unavailable|not\\s+mapped)\\b`,
      'gi',
    ),
    field: null,
  },
  {
    pattern: new RegExp(
      `\\b(?:the\\s+)?${ROLE_NAME}\\s+label\\s+role\\s+(?:(?:is|was)\\s+)?(?:deliberately\\s+unmapped|unmapped|unavailable|not\\s+mapped)\\b`,
      'gi',
    ),
    field: 'labels',
  },
  {
    pattern: new RegExp(
      `\\b(?:the\\s+)?${ROLE_NAME}\\s+state\\s+role\\s+(?:(?:is|was)\\s+)?(?:deliberately\\s+unmapped|unmapped|unavailable|not\\s+mapped)\\b`,
      'gi',
    ),
    field: 'states',
  },
  {
    pattern: new RegExp(
      `\\blabelName\\(\\s*config\\s*,\\s*['"]${ROLE_NAME}['"]\\s*\\)` + '`?\\s+throws\\b',
      'gi',
    ),
    field: 'labels',
  },
  {
    pattern: new RegExp(
      `\\bstateName\\(\\s*config\\s*,\\s*['"]${ROLE_NAME}['"]\\s*\\)` + '`?\\s+throws\\b',
      'gi',
    ),
    field: 'states',
  },
]

function lineNumberAt(text, index) {
  let line = 1
  for (let i = 0; i < index; i += 1) {
    if (text.charCodeAt(i) === 10) line += 1
  }
  return line
}

function claimQuote(text, start, end) {
  const scope = claimScope(text, start, end)
  return scope.replace(/\s+/g, ' ').trim()
}

function claimScope(text, start, end) {
  const lineStart = text.lastIndexOf('\n', start - 1) + 1
  const lineEndIndex = text.indexOf('\n', start)
  const lineEnd = lineEndIndex === -1 ? text.length : lineEndIndex
  const line = text.slice(lineStart, lineEnd)
  if (/^\s*(?:[-*+]|\d+[.)])\s+/.test(line)) {
    let to = lineEnd
    while (to < text.length) {
      const nextLineStart = to + 1
      const nextLineEndIndex = text.indexOf('\n', nextLineStart)
      const nextLineEnd = nextLineEndIndex === -1 ? text.length : nextLineEndIndex
      const nextLine = text.slice(nextLineStart, nextLineEnd)
      if (/^\s*(?:[-*+]|\d+[.)])\s+/.test(nextLine)) break
      to = nextLineEnd
    }
    return text.slice(lineStart, to)
  }
  const before = text.lastIndexOf('\n\n', start)
  const after = text.indexOf('\n\n', end)
  const from = before === -1 ? 0 : before + 2
  const to = after === -1 ? text.length : after
  return text.slice(from, to)
}

/**
 * Find bounded prose claims that a tracker-config role is unmapped/unavailable/throwing.
 *
 * This is intentionally not full NLU. It catches the phrasing families that have caused real
 * drift, including line-wrapped claims, and returns enough location data for a tree-wide test to
 * compare each named role against the committed tracker config. A rare legitimate claim can opt out
 * by placing `<!-- skill-config-claim: ignore -->` in the same paragraph or list item.
 * @returns {{ role: string, line: number, quote: string, field?: string }[]}
 */
export function scanUnmappedRoleClaims(body) {
  const text = String(body ?? '')
  const claims = []
  for (const { pattern, field } of ROLE_PROSE_PATTERNS) {
    pattern.lastIndex = 0
    for (const match of text.matchAll(pattern)) {
      if (field === null && /^(?:label|state)$/i.test(match.groups.role)) continue
      const matchEnd = match.index + match[0].length
      if (claimScope(text, match.index, matchEnd).includes(CLAIM_IGNORE_MARKER)) continue
      claims.push({
        role: match.groups.role,
        line: lineNumberAt(text, match.index),
        quote: claimQuote(text, match.index, matchEnd),
        field,
        index: match.index,
      })
    }
  }
  return claims
    .sort((a, b) => a.index - b.index)
    .map(({ role, line, quote, field }) =>
      field === null ? { role, line, quote } : { role, line, quote, field },
    )
}

/**
 * The concrete publish-store identity block for the selected (or named) publish adapter, or null.
 * @returns {{ bucket: string, baseUrl: string } | null}
 */
export function publishConfigFor(config, adapter = adapterFor(config, 'publish')) {
  const pc = config.publishConfig?.[adapter]
  return pc && typeof pc === 'object' ? pc : null
}

/** Resolve the implementation-plan store without changing proof publishing. */
export function planStorageFor(config) {
  return { kind: 'tracker-attachment' }
}

/**
 * Is this checkout wired to a concrete tracker? True iff the selected tracker adapter has an
 * identity block carrying the load-bearing fields (a non-empty mcpServer + team). This is the
 * preflight self-disable probe: a repo with no .boss-skills.json (or one without a trackerConfig
 * block for its tracker) resolves false, and a core invoked there stops with one clear line and
 * makes no tracker write instead of demanding an MCP server that does not exist in that repo.
 * validateConfig() already rejects a present-but-partial block, so a truthy resolve is a usable one.
 */
export function isConfiguredForRepo(config) {
  const tc = trackerConfigFor(config)
  return Boolean(
    tc &&
    typeof tc.mcpServer === 'string' &&
    tc.mcpServer.length > 0 &&
    typeof tc.team === 'string' &&
    tc.team.length > 0,
  )
}

/**
 * Stricter planning-readiness probe for boss-plan's preflight. isConfiguredForRepo() only proves the
 * tracker identity (mcpServer + team); boss-plan additionally resolves state by role — the unplanned
 * and planned states for selection and write-back, plus inProgress/inReview for the active-backlog
 * reads — from trackerConfigFor(config).states. validateConfig() leaves the states map optional (other
 * cores need only the identity), so a repo configured for those cores but missing boss-plan's state
 * roles would pass isConfiguredForRepo() and then read/write with undefined state names. Require the
 * full role map here so boss-plan self-disables cleanly in such a repo instead of failing mid-run
 * after drafting work.
 */
export function isConfiguredForPlanning(config) {
  if (!isConfiguredForRepo(config)) return false
  const states = (trackerConfigFor(config) || {}).states || {}
  return ['unplanned', 'planned', 'inProgress', 'inReview'].every(
    (role) => typeof states[role] === 'string' && states[role].length > 0,
  )
}

// --- Plan-description contract ---------------------------------------------

/** The integer version of the plan-description section contract (default 1). */
export function planContractVersion(config) {
  return config.planContract.version
}

/** The full ordered `{ heading, required }` section list, as emitted. */
export function planSections(config) {
  return config.planContract.sections
}

/** Headings whose `required === 'always'`, in order — the sections every plan MUST carry. */
export function requiredPlanSections(config) {
  return config.planContract.sections.filter((s) => s.required === 'always').map((s) => s.heading)
}

const EPIC_PARENT_PLAN_SECTIONS = Object.freeze([
  Object.freeze({ heading: '## Summary', required: 'always' }),
  Object.freeze({ heading: '## Child tickets', required: 'always' }),
  Object.freeze({ heading: '## Planning', required: 'always' }),
  Object.freeze({ heading: '## Original notes', required: 'always' }),
])

function normalisePlanDescriptionMode(mode) {
  const selected = mode || 'child-plan'
  if (selected === 'child-plan') return selected
  if (selected === 'epic-parent') return selected
  console.warn(
    `skill-config: validatePlanDescription unknown mode "${selected}" — falling back to child-plan`,
  )
  return 'child-plan'
}

function planSectionsForDescriptionMode(config, mode) {
  return mode === 'epic-parent' ? EPIC_PARENT_PLAN_SECTIONS : planSections(config)
}

function requiredSectionsForDescriptionMode(config, mode) {
  return planSectionsForDescriptionMode(config, mode)
    .filter((s) => s.required === 'always')
    .map((s) => s.heading)
}

/**
 * One pass over `text`, returning the lines OUTSIDE fenced code blocks (`{ line, index }`, 0-based)
 * and whether a fence was still open at EOF. Both facts come from the same scan so they can never
 * disagree.
 *
 * Fence tracking is intentionally simple: an opening ``` / ~~~ run of 3+ is closed by a run of the
 * same character that is at least as long. It only has to be good enough to tell markdown STRUCTURE
 * from quoted sample text — a plan legitimately fences command output and markdown examples.
 *
 * ANY leading indent opens a fence, deliberately wider than CommonMark's 3-space limit for a
 * top-level fence. A fence nested in a list item is indented by the marker width, so the strict
 * limit missed exactly the shape a plan uses most — quoted output under a bullet — and let its
 * sample lines read as document structure.
 *
 * The widening is BIDIRECTIONAL, not one-way: the same match drives the close branch, so an
 * indented run can also TERMINATE an enclosing fence early and expose its content. Measured
 * residual, accepted rather than fixed: a 3-backtick fence nested inside a 3-backtick outer fence
 * closes the outer one, so the nested sample lines become visible again. Incidence is 0 of the
 * 1212 tracked markdown files in this repo, and the CommonMark-idiomatic nesting — a LONGER outer
 * fence — is unaffected, because the close branch still requires `run[1].length >= fence.length`.
 * The airtight fix is to record the opening indent on `fence` and require the closing run's indent
 * to be no greater than it plus 3; do that if the shape is ever observed in practice.
 *
 * It lives here, beside the splitter that needs it, because `plan-contract-guard.mjs` imports this
 * module: putting it there instead would invert the dependency, and a second copy would drift.
 */
export function scanFences(text) {
  const lines = []
  let fence = null
  const source = String(text ?? '').split('\n')
  for (let index = 0; index < source.length; index += 1) {
    const bare = source[index].replace(/\r$/, '')
    const run = /^[ \t]*(`{3,}|~{3,})(.*)$/.exec(bare)
    if (fence === null) {
      if (run && !(run[1][0] === '`' && run[2].includes('`'))) {
        fence = { character: run[1][0], length: run[1].length }
        continue
      }
      lines.push({ line: bare, index })
      continue
    }
    if (run && run[1][0] === fence.character && run[1].length >= fence.length && !run[2].trim()) {
      fence = null
    }
  }
  return { lines, unterminated: fence !== null }
}

/**
 * Split a plan description into its emitted top-level `##` sections, in order.
 *
 * The final contract section (`## Original notes`) echoes the ticket's original description
 * verbatim, which can itself contain `##` headings and stray `- Contract:` lines. To keep that
 * preserved body from masquerading as emitted plan structure, splitting stops at the terminal
 * section's heading: everything after it is that section's body, not a new section.
 *
 * A `##` line inside a FENCED CODE BLOCK is likewise not a section: plans routinely quote command
 * output and markdown examples that begin with `##` (a `make help` listing, a `git status` line),
 * and reading one as emitted structure invents a section the drafter never wrote. That was merely
 * cosmetic while only `missing` was consumed; once a producer-side gate rejects off-contract
 * headings it becomes a deterministic hard abort with no remedy the drafter can act on. Skipping
 * them can only REMOVE spurious sections, so `missing`/`ok` are unchanged except where a required
 * heading appears solely inside a fence — where the stricter reading is the correct one.
 *
 * Exported so producer-side gates (section ordering, placeholder scoping) share this one terminal-
 * section rule instead of re-deriving it — a duplicate splitter would drift from the validator.
 * @returns {{ heading: string, bodyLines: string[] }[]}
 */
function splitPlanDescriptionSections(contractSections, description) {
  const terminalHeading = contractSections[contractSections.length - 1]?.heading
  const outside = new Set(scanFences(description).lines.map(({ index }) => index))
  const sections = []
  let current = null
  let inTerminal = false
  const lines = String(description ?? '').split('\n')
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]
    const trimmed = line.trim()
    if (!inTerminal && outside.has(index) && /^##\s/.test(trimmed)) {
      current = { heading: trimmed, bodyLines: [] }
      sections.push(current)
      if (trimmed === terminalHeading) inTerminal = true
      continue
    }
    if (current) current.bodyLines.push(line)
  }
  return sections
}

export function planDescriptionSections(config, description) {
  return splitPlanDescriptionSections(planSections(config), description)
}

/**
 * Validate a plan description against the contract.
 *
 * The argument order is **config first**: `validatePlanDescription(config, description)`. The
 * natural-reading `(description, config)` call is detected and rejected with a named error rather
 * than allowed to surface as a `Cannot read properties of undefined (reading 'sections')` TypeError
 * from deep inside `planSections()`.
 *
 * Parses the `- Contract: v<N>` stamp from the body of the `## Planning` section only — either
 * bullet marker is accepted, because a tracker may renormalise `-` to `*` on save, which would
 * otherwise make the stamp undetectable on read-back. A missing stamp is treated as back-compat v1
 * (`version: null`); a stamped version newer than this contract is flagged as `unsupportedVersion`.
 *
 * `missing` lists any required heading not emitted as a top-level `##` section. `unknown` lists
 * every emitted top-level `##` heading that is not in `planSections(config)` at all — including
 * sections classed `optional`, which are recognised and therefore never unknown. Headings and
 * stamps echoed inside the verbatim `## Original notes` body do not participate: splitting stops at
 * that terminal section.
 *
 * `ok` is true iff nothing is missing and the version is supported. `unknown` is deliberately
 * **additive** and does NOT flip `ok`: consumers (boss-build) gate on `ok`, and folding `unknown`
 * into it would newly block already-planned tickets. Producer-side strictness belongs in the
 * producer's own guard, which reads `unknown` directly.
 * The optional third argument selects the document shape. Default `child-plan` is the existing
 * implementation-plan contract. `epic-parent` validates the parent overview shape emitted when an
 * oversized ticket becomes an epic parent. Unknown modes warn and fall back to `child-plan`, because
 * installed toolbox copies can lag behind plan prose.
 *
 * @returns {{ ok: boolean, version: number | null, missing: string[], unknown: string[], unsupportedVersion: boolean }}
 */
export function validatePlanDescription(config, description, { mode = 'child-plan' } = {}) {
  // A config is an object carrying a planContract object; a description is a string. That makes the
  // swapped call unambiguously detectable, so diagnose it here instead of failing obscurely below.
  if (typeof config === 'string' || !config || typeof config !== 'object' || !config.planContract) {
    throw new Error(
      'skill-config: validatePlanDescription(config, description) — arguments look swapped; pass the config first',
    )
  }
  const resolvedMode = normalisePlanDescriptionMode(mode)
  const contractSections = planSectionsForDescriptionMode(config, resolvedMode)
  const sections = splitPlanDescriptionSections(contractSections, description)
  const present = new Set(sections.map((s) => s.heading))
  const missing = requiredSectionsForDescriptionMode(config, resolvedMode).filter(
    (heading) => !present.has(heading),
  )
  const recognised = new Set(contractSections.map((s) => s.heading))
  const unknown = [...new Set(sections.map((s) => s.heading))].filter((h) => !recognised.has(h))

  const planning = sections.find((s) => s.heading === '## Planning')
  let version = null
  if (planning) {
    for (const bodyLine of planning.bodyLines) {
      const stamp = /^[-*]\s*Contract:\s*v(\d+)\s*$/.exec(bodyLine.trim())
      if (stamp) {
        version = Number(stamp[1])
        break
      }
    }
  }
  const unsupportedVersion = version !== null && version > planContractVersion(config)
  return {
    ok: missing.length === 0 && !unsupportedVersion,
    version,
    missing,
    unknown,
    unsupportedVersion,
  }
}

// --- Verify-only acceptance criteria ---------------------------------------
//
// A criterion whose correct outcome is "this file needed no change" produces no diff, so every gate
// that reasons over changed paths is blind to it. Marking such a criterion with a LITERAL token
// makes classification exact — a bullet either carries the marker or it does not, so there are no
// heuristics and no false positives — and lets the consumer require named, re-runnable evidence
// instead of a silent tick. The four literals below are exported so producer prose, consumer prose
// and this parser all read one definition; a hand-typed copy in three places is how a contract
// drifts, and a sync test asserts the prose carries these exact bytes.

/** Literal prefix that classifies an acceptance-criterion bullet as verify-only. */
export const VERIFY_ONLY_MARKER = '(verify-only)'

/** Plan-time clause naming the command a reviewer can re-run. */
export const VERIFY_ONLY_CHECK = ' — check: '

/** Discharge-time clause naming the command the run actually executed. */
export const VERIFY_ONLY_CHECKED = ' — checked: '

/** Separator between a discharged command and the result it produced. */
export const VERIFY_ONLY_RESULT = ' → '

/** The `## Acceptance criteria` contract heading these helpers parse. */
const ACCEPTANCE_HEADING = '## Acceptance criteria'

/** The optional `## Premises` contract heading. */
const PREMISES_HEADING = '## Premises'
const CENTRAL_PREMISE_MARKER_RE = /^(?:\*\*|__|\*|_)?\(central\)(?:\*\*|__|\*|_)?\s*/i
const SHELL_BUILTINS = new Set(['[', 'cd', 'echo', 'false', 'printf', 'pwd', 'set', 'test', 'true'])
const SHELL_COMMAND_SEPARATORS = new Set([';', '|', '&&', '||'])

/** A markdown checkbox list item: `- [ ] text` / `- [x] text` (either box case, any list marker). */
const CRITERION_RE = /^[-*+]\s+\[([ xX])\]\s*(.*)$/

/** Any other list item — it ends the previous criterion rather than continuing it. */
const LIST_ITEM_RE = /^[-*+]\s|^\d+[.)]\s/

/**
 * The result separator, matched tolerantly on surrounding whitespace.
 *
 * `VERIFY_ONLY_RESULT` is the canonical written form and stays exactly ` → `, but it is the one
 * clause that can legitimately sit at END OF LINE — precisely when the result is EMPTY, which is
 * the mutation this gate exists to catch. Every markdown formatter strips trailing whitespace, so
 * a strict `indexOf(' → ')` would fail to find the separator in exactly that case, silently
 * reclassifying "empty result" as "no result clause at all". The two happen to route to the same
 * verdict today; relying on that coincidence would make the gate's reason wrong even when its
 * answer is right, and it would break the moment either branch is reported separately.
 */
const RESULT_SEPARATOR_RE = new RegExp(`\\s*${VERIFY_ONLY_RESULT.trim()}\\s*`)

/**
 * Both new exports are **config-first**, exactly like `validatePlanDescription`. Diagnose the
 * natural-reading swapped call by name instead of letting it surface as a TypeError from deep
 * inside `planSections()`.
 */
function assertConfigFirst(config, fn) {
  if (typeof config === 'string' || !config || typeof config !== 'object' || !config.planContract) {
    throw new Error(
      `skill-config: ${fn}(config, description) — arguments look swapped; pass the config first`,
    )
  }
}

/** Strip one wrapping pair of backticks, if present, then trim. */
function unwrapCode(value) {
  const trimmed = value.trim()
  const fenced = /^`+(.*?)`+$/s.exec(trimmed)
  return (fenced ? fenced[1] : trimmed).trim()
}

/** A string that carries actual content — "present" must never be confused with "non-empty". */
function nonEmpty(value) {
  return typeof value === 'string' && value.trim().length > 0
}

function pushShellToken(tokens, value, quoted) {
  if (value) tokens.push({ value, quoted })
}

function tokenizeSimpleShellDetailed(command) {
  const source = String(command ?? '')
  const tokens = []
  let current = ''
  let quoted = false
  let quote = null
  for (let index = 0; index < source.length; index += 1) {
    const char = source[index]
    if (quote) {
      if (char === quote) quote = null
      else current += char
      continue
    }
    if (char === '"' || char === "'" || char === '`') {
      quote = char
      quoted = true
    } else if (
      (char === '&' && source[index + 1] === '&') ||
      (char === '|' && source[index + 1] === '|')
    ) {
      pushShellToken(tokens, current, quoted)
      current = ''
      quoted = false
      tokens.push({ value: char + source[index + 1], quoted: false })
      index += 1
    } else if (char === '|' || char === ';') {
      pushShellToken(tokens, current, quoted)
      current = ''
      quoted = false
      tokens.push({ value: char, quoted: false })
    } else if (/\s/.test(char)) {
      pushShellToken(tokens, current, quoted)
      current = ''
      quoted = false
    } else {
      current += char
    }
  }
  pushShellToken(tokens, current, quoted)
  return tokens
}

export function tokenizeSimpleShell(command) {
  return tokenizeSimpleShellDetailed(command).map((token) => token.value)
}

function isExecutableFile(path) {
  try {
    const stat = statSync(path)
    return stat.isFile() && (stat.mode & 0o111) !== 0
  } catch {
    return false
  }
}

function resolvePathBinary(head, envPath) {
  if (!head || head.includes('/')) return false
  for (const dir of String(envPath ?? '').split(':')) {
    if (!dir) continue
    if (isExecutableFile(join(dir, head))) return true
  }
  return false
}

function commandHeadResolvable(tokens, cwd, env) {
  const head = tokens[0]
  if (!head) return false
  if (SHELL_BUILTINS.has(head)) return true
  if (head.startsWith('./') || head.startsWith('../') || head.includes('/')) {
    return isExecutableFile(isAbsolute(head) ? head : join(cwd, head))
  }
  if (resolvePathBinary(head, env?.PATH)) return true
  return false
}

function hasCommittedAnchor(tokens) {
  return tokens.some(
    (token) =>
      token === '--cached' ||
      token === '--staged' ||
      token === 'HEAD' ||
      token === 'FETCH_HEAD' ||
      token.includes('..') ||
      /^[0-9a-f]{7,40}$/i.test(token),
  )
}

function hasCountAssertion(command) {
  return /(#\s*(?:pass|tests)\s+\d+|grep\s+-q|rg\s+-q|wc\s+-l|assert|count)/i.test(command)
}

function advisory(code, message) {
  return { code, message }
}

function normalizeCommandTokens(tokens, { stripShellPreamble = true } = {}) {
  tokens = [...tokens]
  while (tokens.length > 0 && /^[A-Za-z_][A-Za-z0-9_]*=/.test(tokens[0])) tokens.shift()
  if (tokens[0] === 'env') {
    tokens.shift()
    while (tokens.length > 0) {
      if (/^[A-Za-z_][A-Za-z0-9_]*=/.test(tokens[0])) {
        tokens.shift()
      } else if (tokens[0] === '--') {
        tokens.shift()
        break
      } else if (tokens[0] === '-u' || tokens[0] === '--unset') {
        tokens.splice(0, 2)
      } else if (tokens[0].startsWith('--unset=')) {
        tokens.shift()
      } else if (tokens[0] === '-C' || tokens[0] === '--chdir') {
        tokens.splice(0, 2)
      } else if (tokens[0].startsWith('--chdir=')) {
        tokens.shift()
      } else if (tokens[0] === '-i' || tokens[0] === '--ignore-environment') {
        tokens.shift()
      } else if (tokens[0].startsWith('-')) {
        tokens.shift()
      } else {
        break
      }
    }
  }
  if (stripShellPreamble && tokens[0] === 'set' && tokens[1] === '-o' && tokens[2] === 'pipefail') {
    tokens.splice(0, tokens[3] === ';' ? 4 : 3)
  }
  return tokens
}

function commandTokens(command) {
  return normalizeCommandTokens(tokenizeSimpleShell(command))
}

function commandSegments(tokens) {
  const segments = []
  let current = []
  for (const token of tokens) {
    if (SHELL_COMMAND_SEPARATORS.has(token.value)) {
      if (current.length > 0) segments.push(current)
      current = []
    } else {
      current.push(token)
    }
  }
  if (current.length > 0) segments.push(current)
  return segments.map((segment) =>
    normalizeCommandTokens(
      segment.map((token) => token.value),
      { stripShellPreamble: false },
    ),
  )
}

function hasUnquotedOptionGlob(tokens) {
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index]
    if (/^--[^=\s]+=.*[*?[]/.test(token.value) && !token.quoted) return true
    if (
      /^--(?:include|exclude|glob)$/.test(token.value) &&
      tokens[index + 1] &&
      /[*?[]/.test(tokens[index + 1].value) &&
      !tokens[index + 1].quoted
    ) {
      return true
    }
  }
  return false
}

/**
 * Classify a recorded check command without executing it.
 *
 * The command is PR-body text, so this function deliberately uses only static checks. Decidable
 * failures land in `blocking`; heuristic proof-quality risks land in `advisory`.
 */
export function classifyCheckCommand(command, { cwd = process.cwd(), env = process.env } = {}) {
  const detailedTokens = tokenizeSimpleShellDetailed(command)
  const tokens = commandTokens(command)
  const blocking = []
  const advisoryFindings = []
  const trimmed = String(command ?? '').trim()
  if (!trimmed) {
    return {
      blocking: [
        {
          code: 'empty-command',
          message: 'the check command is empty',
        },
      ],
      advisory: advisoryFindings,
    }
  }
  if (
    commandSegments(detailedTokens).some((segment) => !commandHeadResolvable(segment, cwd, env))
  ) {
    blocking.push({
      code: 'command-unresolvable',
      message:
        'the command head resolves to no executable PATH binary or executable repo-relative script',
    })
  }

  if (
    tokens[0] === 'git' &&
    (tokens[1] === 'diff' || tokens[1] === 'status') &&
    !hasCommittedAnchor(tokens)
  ) {
    advisoryFindings.push(
      advisory(
        'working-tree-scoped-git-check',
        'working-tree-scoped git evidence can pass after unrelated local cleanup',
      ),
    )
  }
  if (
    tokens.some((token) =>
      /^(-run|--test-name-pattern|--test-only|--test-skip-pattern|--include)(=|$)/.test(token),
    ) &&
    !hasCountAssertion(trimmed)
  ) {
    advisoryFindings.push(
      advisory(
        'zero-selection-filter',
        'a test or file-selection filter can select zero tests/files without a count assertion',
      ),
    )
  }
  if (hasUnquotedOptionGlob(detailedTokens)) {
    advisoryFindings.push(
      advisory(
        'unquoted-option-glob',
        'a glob embedded in an option value can be expanded or rejected by the shell before the command runs',
      ),
    )
  }
  if (
    tokens.includes('|') &&
    !tokens.includes('pipefail') &&
    !trimmed.includes('set -o pipefail')
  ) {
    advisoryFindings.push(
      advisory(
        'pipe-without-pipefail',
        'a pipeline without pipefail reports the tail command status rather than the failing command',
      ),
    )
  }
  if (tokens[0] === 'git' && tokens[1] === 'grep' && tokens.includes('-E') && /\\b/.test(trimmed)) {
    advisoryFindings.push(
      advisory('git-grep-word-boundary', 'git grep -E does not interpret \\b as a word boundary'),
    )
  }
  if (
    tokens[0] === 'bazel' &&
    tokens[1] === 'test' &&
    !tokens.some((token) => token === '--nocache_test_results')
  ) {
    advisoryFindings.push(
      advisory('cached-bazel-test', 'a cached Bazel test can predate newly added inputs'),
    )
  }
  return { blocking, advisory: advisoryFindings }
}

/**
 * Strip a leading run of markdown emphasis so `**(verify-only)**` classifies like `(verify-only)`.
 *
 * The marker is a literal PREFIX by design, and that stays true — this normalises only where the
 * prefix may start, never what counts as the marker, so it adds no heuristic and no false positive.
 * What it removes is a silent FALSE NEGATIVE: the drafting brief bolds the token in its own prose a
 * line above the template it tells planners to copy, so a bolded marker is the likeliest drafting
 * slip, and an unrecognised marker hands the criterion the diff-demonstrated route — the very
 * silence this contract exists to end — with nothing anywhere to detect it.
 */
function stripEmphasis(value) {
  return value.replace(/^[*_]+/, '')
}

/**
 * Parse the `## Acceptance criteria` section of a plan description (or a PR body written from the
 * same template) into structured criteria.
 *
 * Scope is the `## Acceptance criteria` section **only**, obtained from the existing
 * `planDescriptionSections()` splitter rather than a second scan of the whole string. That splitter
 * already stops at the terminal `## Original notes` heading, so a criterion echoed inside the
 * preserved original notes cannot spoof this parse — a property already documented and tested for
 * `validatePlanDescription`, inherited here rather than re-derived. Lines inside fenced code blocks
 * are skipped for the same reason `planDescriptionSections()` skips them: a plan legitimately quotes
 * the criterion TEMPLATE, and reading a sample bullet as a real criterion invents one nobody wrote.
 *
 * A criterion wrapped across several lines is joined into one `text` before matching, because the
 * ` — check: ` / ` — checked: ` clause routinely lands on a continuation line.
 *
 * Returns DATA and never throws for a malformed criterion — a bullet with no clause is simply one
 * whose `check` is `null`, which is the caller's decision to make, not this parser's.
 *
 * @param {object} config skill config (config-first, like `validatePlanDescription`)
 * @param {string} description plan description or PR body
 * @returns {{
 *   text: string,
 *   checked: boolean,
 *   verifyOnly: boolean,
 *   malformedVerifyOnlyMarker: boolean,
 *   check: string | null,
 *   result: string | null,
 *   checkedClause: boolean,
 *   checkedCommandDelimited: boolean,
 *   plannedTenseOnTicked: boolean
 * }[]}
 *   `text` is the whole bullet body after the checkbox, marker and clause included, so a report can
 *   name the criterion verbatim. `checked` is the box state. `verifyOnly` is the literal-marker
 *   classification. `check` is the command named by either clause (backticks stripped) or `null`
 *   when neither clause is present. A `— checked: ` clause MUST delimit its command in backticks:
 *   an undelimited one is undecidable (an arrow inside the command is indistinguishable from the
 *   result separator), so it yields `check: null` and the evidence gate blocks rather than guess.
 *   `result` is the observed result from the discharge clause only, or `null` when the criterion is
 *   not yet discharged. An empty clause yields `''`, never `null` — the difference between "absent"
 *   and "empty" is what the evidence gate turns on.
 */
function parseCheckboxSection(config, description, heading, fn) {
  assertConfigFirst(config, fn)
  const section = planDescriptionSections(config, description).find((s) => s.heading === heading)
  if (!section) return []

  const body = section.bodyLines.join('\n')
  const outside = new Set(scanFences(body).lines.map(({ index }) => index))
  const lines = body.split('\n')
  const raw = []
  let current = null
  for (let index = 0; index < lines.length; index += 1) {
    if (!outside.has(index)) continue // fenced sample, not a criterion
    const trimmed = lines[index].trim()
    const indented = /^[ \t]/.test(lines[index])
    const match = CRITERION_RE.exec(trimmed)
    // A criterion is a TOP-LEVEL bullet. Testing `CRITERION_RE` before the indent test let an
    // indented `- [x]` sub-bullet — ordinary markdown habit when the evidence gets its own line —
    // steal the evidence into a phantom criterion of its own, leaving the real one with no clause.
    if (match && !(current && indented)) {
      current = { box: match[1], parts: [match[2].trim()] }
      raw.push(current)
      continue
    }
    if (!current) continue
    // A blank line, a SIBLING (unindented) list item, or a heading ends the criterion; an INDENTED
    // sub-bullet belongs to it. Ending on ANY list item drops a discharge clause written as a
    // nested bullet — the natural shape when the command is long — so evidence that is genuinely
    // present is reported as missing, and the gate BLOCKS a run that did everything right. A
    // false negative here is as expensive as the false positive above, in the opposite direction.
    if (trimmed === '' || /^#{1,6}\s/.test(trimmed) || (LIST_ITEM_RE.test(trimmed) && !indented)) {
      current = null
      continue
    }
    // Drop a continuation sub-bullet's own marker so the joined text reads as one criterion.
    current.parts.push(trimmed.replace(LIST_ITEM_RE, ''))
  }

  return raw.map(({ box, parts }) => {
    const text = parts.filter(Boolean).join(' ').trim()
    let check = null
    let result = null
    let checkedClause = false
    let checkedCommandDelimited = false
    let plannedTenseOnTicked = false
    const dischargeAt = text.indexOf(VERIFY_ONLY_CHECKED)
    if (dischargeAt !== -1) {
      checkedClause = true
      const tail = text.slice(dischargeAt + VERIFY_ONLY_CHECKED.length)
      // A BACKTICKED command is a DELIMITED span, so close it BEFORE looking for the result
      // separator. Searching for the separator first lets an arrow INSIDE the command act as one:
      // `— checked: \`echo a→b\`` carries no result clause at all, yet splitting at that arrow
      // yields a non-empty command AND a non-empty result, so the criterion PASSES — the exact
      // "present but no result" case this gate exists to reject, and a bypass any command
      // containing an arrow (`rg "a→b"`) triggers by accident. Matching the backtick RUN also
      // retires the greedy `unwrapCode` strip, which turned a command carrying a second code span
      // into a garbled `check` no reviewer could paste while the gate still reported green.
      // The delimiter is REQUIRED on the discharge path, and the requirement is what makes the
      // parse decidable at all. Without it there is no way to tell `rg "a→b" src/` (one command,
      // no result) from `make check → identical` (command, then result): both are one arrow in
      // undelimited text, so ANY split rule gets one of them wrong. Guessing picked the bypass —
      // the first row split into a non-empty command and a non-empty result and PASSED carrying no
      // result at all, and the pasteable command was garbled to `rg "a` either way. So an
      // undelimited discharge is `check = null`: the gate BLOCKS and names the requirement, which
      // is the fail-closed direction and a one-keystroke fix for the builder. Both shipped
      // templates already backtick the command, so this rejects nothing they teach.
      const fenced = /^\s*(`+)([\s\S]*?)\1/.exec(tail)
      if (fenced) {
        checkedCommandDelimited = true
        check = fenced[2].trim()
        const rest = tail.slice(fenced[0].length)
        const separator = RESULT_SEPARATOR_RE.exec(rest)
        if (separator) result = rest.slice(separator.index + separator[0].length).trim()
      }
    } else {
      const plannedAt = text.indexOf(VERIFY_ONLY_CHECK)
      if (plannedAt !== -1) {
        if (box.toLowerCase() === 'x') {
          plannedTenseOnTicked = true
        } else {
          check = unwrapCode(text.slice(plannedAt + VERIFY_ONLY_CHECK.length))
        }
      }
    }
    const markerText = stripEmphasis(text)
    return {
      text,
      checked: box.toLowerCase() === 'x',
      verifyOnly: markerText.startsWith(VERIFY_ONLY_MARKER),
      malformedVerifyOnlyMarker:
        markerText.includes(VERIFY_ONLY_MARKER) && !markerText.startsWith(VERIFY_ONLY_MARKER),
      check,
      result,
      checkedClause,
      checkedCommandDelimited,
      plannedTenseOnTicked,
    }
  })
}

export function parseAcceptanceCriteria(config, description) {
  return parseCheckboxSection(config, description, ACCEPTANCE_HEADING, 'parseAcceptanceCriteria')
}

/**
 * Parse the optional `## Premises` section into structured premise bullets.
 *
 * Premises deliberately reuse the acceptance-criterion bullet grammar and check-clause notation so
 * the consumer can discharge them with the same evidence shape. A premise is central only when its
 * claim starts with the literal `(central)` marker, after optional markdown emphasis; two central
 * premises are reported in-band so callers cannot silently pick the last one.
 *
 * @returns {{ text: string, claim: string, check: string | null, central: boolean, duplicateCentral: boolean }[]}
 */
export function parsePremises(config, description) {
  const premises = parseCheckboxSection(config, description, PREMISES_HEADING, 'parsePremises').map(
    (premise) => {
      const central = CENTRAL_PREMISE_MARKER_RE.test(premise.text)
      const claim = (central ? premise.text.replace(CENTRAL_PREMISE_MARKER_RE, '') : premise.text)
        .replace(VERIFY_ONLY_CHECK + (premise.check === null ? '' : `\`${premise.check}\``), '')
        .trim()
      return {
        text: premise.text,
        claim,
        check: premise.check,
        central,
        duplicateCentral: false,
      }
    },
  )
  const central = premises.filter((premise) => premise.central)
  if (central.length > 1) {
    for (const premise of central) premise.duplicateCentral = true
  }
  return premises
}

/**
 * Validate that every DISCHARGED verify-only criterion carries the evidence its marker promises.
 *
 * The gate checks **structure and non-emptiness**, never truth: it cannot tell whether the recorded
 * command was really run, and does not pretend to. What it buys is that the check becomes named and
 * re-runnable by a human reviewer, so a verify-only criterion can no longer be discharged in silence.
 *
 * Only a criterion that is BOTH `verifyOnly` and `checked` can fail. An unticked verify-only
 * criterion is reported in `verifyOnly` but is never a `missingEvidence` failure — an open criterion
 * is already caught by the consumer's existing required-deferred rule, and double-reporting it would
 * make this gate cry wolf on a condition another gate already owns.
 *
 * A description carrying no marker at all yields `verifyOnly: []` and `ok: true`, so every plan
 * written before this contract keeps validating unchanged.
 *
 * @param {object} config skill config (config-first)
 * @param {string} body plan description or PR body carrying an `## Acceptance criteria` section
 * @returns {{ ok: boolean, verifyOnly: object[], missingEvidence: object[], malformedMarker: object[], advisory: object[] }}
 *   criteria as returned by `parseAcceptanceCriteria`. `ok` is true iff `missingEvidence` is empty.
 *   `malformedMarker` surfaces criteria that contain a misplaced `(verify-only)` marker without
 *   reclassifying them. `advisory` carries non-blocking command-shape risks.
 */
export function validateVerifyOnlyEvidence(config, body) {
  assertConfigFirst(config, 'validateVerifyOnlyEvidence')
  const criteria = parseAcceptanceCriteria(config, body)
  const verifyOnly = criteria.filter((c) => c.verifyOnly)
  const malformedMarker = criteria.filter((c) => c.malformedVerifyOnlyMarker)
  const missingEvidence = []
  const advisory = []
  for (const criterion of verifyOnly) {
    if (!criterion.checked) continue
    let reason = null
    let remedy = null
    if (criterion.plannedTenseOnTicked) {
      reason = 'planned-tense-on-ticked'
      remedy = `Use "${VERIFY_ONLY_CHECKED.trim()}" on checked verify-only criteria.`
    } else if (!criterion.checkedClause) {
      reason = 'no-clause'
      remedy = `Add "${VERIFY_ONLY_CHECKED.trim()} \`<command>\`${VERIFY_ONLY_RESULT}<result>".`
    } else if (!criterion.checkedCommandDelimited) {
      reason = 'undelimited-command'
      remedy = 'Wrap the discharged command in backticks so the result separator is unambiguous.'
    } else if (!nonEmpty(criterion.check)) {
      reason = 'empty-command'
      remedy = 'Record a non-empty command between the backticks.'
    } else if (!nonEmpty(criterion.result)) {
      reason = 'empty-result'
      remedy = 'Record the non-empty result after the result separator.'
    } else {
      const classified = classifyCheckCommand(criterion.check)
      advisory.push(...classified.advisory.map((finding) => ({ ...finding, criterion })))
      if (classified.blocking.length > 0) {
        reason = 'command-unresolvable'
        remedy =
          'Use a command whose head resolves to an executable PATH binary or executable repo-relative script.'
      }
    }
    if (reason) {
      missingEvidence.push({
        ...criterion,
        reason,
        remedy,
        check: reason === 'planned-tense-on-ticked' ? null : criterion.check,
      })
    }
  }
  return {
    ok: missingEvidence.length === 0,
    verifyOnly,
    missingEvidence,
    malformedMarker,
    advisory,
  }
}
