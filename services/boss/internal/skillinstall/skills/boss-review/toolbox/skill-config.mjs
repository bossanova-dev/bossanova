// Declarative skill-config convention for the boss-* skills (BOS-192).
// A single surface (.boss-skills.json at the repo root) that holds the
// path-glob -> lens map, build/test commands, test-manifest path, headless
// env-detection signals, and adapter selection the skills used to hard-code.
// Node builtins ONLY: this module is vendored into the reusable toolbox
// (BOS-191) and runs in dependency-free cron worktrees.

import { existsSync, readFileSync } from 'node:fs'
import { join, dirname, parse as parsePath } from 'node:path'

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

/** Bossanova's current values — the fallback when no config file is present. */
export const DEFAULT_CONFIG = Object.freeze({
  // Byte-identical to the committed .boss-skills.json lensMap: DEFAULT_CONFIG is
  // the fallback when no config file is present, so it must carry the same
  // fallbackRubric on every lens. Without it, `--lenses` in a checkout lacking
  // .boss-skills.json would emit go/tui/web with no inline fallback, and a
  // non-vendored lens skill (e.g. impeccable) could be dispatched with nothing
  // to substitute into Phase 1 — the graceful-degradation path the skill relies on.
  lensMap: [
    {
      id: 'go',
      skill: 'golang-pro',
      glob: '**/*.go',
      fallbackRubric:
        'review through an inline Go rubric: idiomatic error handling and wrapping, goroutine/channel safety and leaks, interface and generics design, allocation on hot paths, and table-driven test coverage for new/changed logic',
    },
    {
      id: 'tui',
      skill: 'tui-design',
      glob: 'services/boss/**',
      fallbackRubric:
        'review through an inline Bubbletea v2 rubric: view/update purity, key-binding and action-bar consistency, layout-constant reuse, confirmation-dialog patterns, and no blocking work in Update',
    },
    {
      id: 'web',
      skill: 'impeccable',
      glob: 'services/web/**',
      fallbackRubric:
        'review through an inline React/TypeScript/web-UI rubric: component correctness, hook/effect races, accessibility, type-boundary cleanliness, dead/duplicated code',
    },
  ],
  commands: {
    build: 'make build',
    lint: 'make lint',
    format: 'make format',
    testSmoke: 'make test-smoke',
    testAffected: 'make test-affected',
    testFull: 'make test-full',
    testModule: 'make test-{module}',
  },
  test: { manifestPath: 'docs/testing/test-command-manifest.md' },
  env: {
    headlessSignals: [
      { var: 'BOSS_CRON', equals: 'true' },
      { var: 'BS_HEADLESS', equals: '1' },
      { var: 'OPENCLAW_SESSION', present: true },
    ],
    headlessWhenNoTty: true,
  },
  adapters: { tracker: 'linear', publish: 'proof', sessionRunner: 'bossd' },
  // Versioned wire contract for the `##`-section plan description that boss-plan emits and
  // boss-implement / bs-sweep-plan consume (BOS-204). `version` is the integer contract
  // version stamped in-band as `- Contract: v<N>` under `## Planning`; `sections` is the
  // ordered heading set as emitted, each classed `always` (every plan carries it) or
  // conditional (`needs-human` / `open-questions`). v1 == today's exact section set —
  // introducing the stamp IS the versioning; no sections were added, removed, or renamed.
  planContract: {
    version: 1,
    sections: [
      { heading: '## Summary', required: 'always' },
      { heading: '## Approach', required: 'always' },
      { heading: '## Key changes', required: 'always' },
      { heading: '## Testing', required: 'always' },
      { heading: '## Risks / unknowns', required: 'always' },
      { heading: '## Acceptance criteria', required: 'always' },
      { heading: '## Required proof', required: 'always' },
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

// The `required` classifications a planContract section may carry (BOS-204).
export const PLAN_SECTION_REQUIRED_KINDS = new Set(['always', 'needs-human', 'open-questions'])

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
  if (!config.commands || typeof config.commands !== 'object' || Array.isArray(config.commands)) {
    fail('commands must be an object')
  }
  for (const [key, val] of Object.entries(config.commands)) {
    // Accessors like moduleTestCommand() call .replace() on these; a non-string
    // override must fail here with a skill-config: error rather than throwing a
    // raw TypeError deep in a headless skill run.
    if (typeof val !== 'string' || val.length === 0) {
      fail(`commands.${key} must be a non-empty string`)
    }
  }
  if (typeof config.test?.manifestPath !== 'string') fail('test.manifestPath must be a string')
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
  const merged = mergeConfig(DEFAULT_CONFIG, user)
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

export function command(config, key) {
  return config.commands[key]
}

export function moduleTestCommand(config, module) {
  return config.commands.testModule.replace('{module}', module)
}

export function manifestPath(config) {
  return config.test.manifestPath
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

// --- Plan-description contract (BOS-204) -----------------------------------

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

/**
 * Split a plan description into its emitted top-level `##` sections, in order.
 *
 * The final contract section (`## Original notes`) echoes the ticket's original description
 * verbatim, which can itself contain `##` headings and stray `- Contract:` lines. To keep that
 * preserved body from masquerading as emitted plan structure, splitting stops at the terminal
 * section's heading: everything after it is that section's body, not a new section.
 * @returns {{ heading: string, bodyLines: string[] }[]}
 */
function planDescriptionSections(config, description) {
  const contractSections = planSections(config)
  const terminalHeading = contractSections[contractSections.length - 1]?.heading
  const sections = []
  let current = null
  let inTerminal = false
  for (const line of description.split('\n')) {
    const trimmed = line.trim()
    if (!inTerminal && /^##\s/.test(trimmed)) {
      current = { heading: trimmed, bodyLines: [] }
      sections.push(current)
      if (trimmed === terminalHeading) inTerminal = true
      continue
    }
    if (current) current.bodyLines.push(line)
  }
  return sections
}

/**
 * Validate a plan description against the contract. Parses the `- Contract: v<N>` stamp from the
 * body of the `## Planning` section only (a missing stamp is treated as back-compat v1,
 * `version: null`), flags a stamped version newer than this contract as `unsupportedVersion`, and
 * lists any required heading not emitted as a top-level `##` section. Headings and stamps echoed
 * inside the verbatim `## Original notes` body do not satisfy the contract. `ok` is true iff
 * nothing is missing and the version is supported.
 * @returns {{ ok: boolean, version: number | null, missing: string[], unsupportedVersion: boolean }}
 */
export function validatePlanDescription(config, description) {
  const sections = planDescriptionSections(config, description)
  const present = new Set(sections.map((s) => s.heading))
  const missing = requiredPlanSections(config).filter((heading) => !present.has(heading))

  const planning = sections.find((s) => s.heading === '## Planning')
  let version = null
  if (planning) {
    for (const bodyLine of planning.bodyLines) {
      const stamp = /^-\s*Contract:\s*v(\d+)\s*$/.exec(bodyLine.trim())
      if (stamp) {
        version = Number(stamp[1])
        break
      }
    }
  }
  const unsupportedVersion = version !== null && version > planContractVersion(config)
  return { ok: missing.length === 0 && !unsupportedVersion, version, missing, unsupportedVersion }
}
