import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  existsSync,
  mkdtempSync,
  writeFileSync,
  chmodSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  globToRegExp,
  DEFAULT_CONFIG,
  CONFIG_FILENAME,
  findConfigFile,
  mergeConfig,
  validateConfig,
  loadSkillConfig,
  detectRepoDefaults,
  lensesForFile,
  detectChangeTypes,
  skillForLens,
  reviewDefaultRounds,
  reviewDeltaDefaults,
  reviewMaxDispatchedRoundDefault,
  REVIEW_DEFAULT_MAX_DISPATCHED_ROUNDS,
  reviewLedgerConfig,
  command,
  moduleTestCommand,
  manifestPath,
  isHeadless,
  adapterFor,
  trackerConfigFor,
  publishConfigFor,
  planStorageFor,
  stateName,
  labelName,
  optionalLabelName,
  githubLabelName,
  isConfiguredForRepo,
  isConfiguredForPlanning,
  scanUnmappedRoleClaims,
  planContractVersion,
  planSections,
  planDescriptionSections,
  requiredPlanSections,
  validatePlanDescription,
  parseAcceptanceCriteria,
  parsePremises,
  validateVerifyOnlyEvidence,
  classifyCheckCommand,
  tokenizeSimpleShell,
  VERIFY_ONLY_MARKER,
  VERIFY_ONLY_CHECK,
  VERIFY_ONLY_CHECKED,
  VERIFY_ONLY_RESULT,
} from './skill-config.mjs'

// --- Task 2: glob matcher + default config --------------------------------

test('globToRegExp: **/*.go matches nested and top-level .go files', () => {
  const re = globToRegExp('**/*.go')
  assert.equal(re.test('services/bossd/internal/tmux/tmux.go'), true)
  assert.equal(re.test('main.go'), true)
  assert.equal(re.test('README.md'), false)
  assert.equal(re.test('services/web/src/App.tsx'), false)
})

test('globToRegExp: services/boss/** matches anything under the dir', () => {
  const re = globToRegExp('services/boss/**')
  assert.equal(re.test('services/boss/internal/views/attach.go'), true)
  assert.equal(re.test('services/boss/main.go'), true)
  assert.equal(re.test('services/bossd/main.go'), false)
})

test('globToRegExp: * does not cross a path separator', () => {
  const re = globToRegExp('services/*/main.go')
  assert.equal(re.test('services/boss/main.go'), true)
  assert.equal(re.test('services/boss/internal/main.go'), false)
})

test('CONFIG_FILENAME is the repo-root dotfile', () => {
  assert.equal(CONFIG_FILENAME, '.boss-skills.json')
})

test('DEFAULT_CONFIG carries the four language lenses', () => {
  // Order-independent: adding a lens should be a one-line edit here rather than a
  // positional merge conflict. THIS repo's own (path-anchored) lens set is pinned
  // separately, by the .boss-skills.json reproduction test below.
  const ids = DEFAULT_CONFIG.lensMap.map((r) => r.id)
  assert.deepEqual([...ids].sort(), ['api', 'db', 'go', 'web'])
  const byId = Object.fromEntries(DEFAULT_CONFIG.lensMap.map((r) => [r.id, r]))
  assert.equal(byId.go.skill, 'golang-pro')
  assert.equal(byId.web.skill, 'impeccable')
  assert.equal(byId.db.skill, 'database-review')
  assert.equal(byId.api.skill, 'api-review')
  // `tui` is deliberately NOT a default: its only honest matcher is a repo path, and
  // a directory-naming guess would dispatch a Bubbletea rubric at a repo using something else.
  assert.equal(
    DEFAULT_CONFIG.lensMap.some((r) => r.id === 'tui'),
    false,
  )
})

test('BOS-850: DEFAULT_CONFIG carries no project-specific path literal', () => {
  // Zero-tolerance agnosticism gate, modelled on the skills_manifest identity gate but scoped
  // to this object rather than every payload file (14 payload files legitimately say `services/`).
  // The published cores install into every user's GLOBAL skill directory, so a path literal
  // from any one checkout leaking back in here would ship to thousands of unrelated repos.
  const serialized = JSON.stringify(DEFAULT_CONFIG)
  for (const literal of ['services/', 'lib/bossalib', 'proto/', 'docs/testing/']) {
    assert.equal(
      serialized.includes(literal),
      false,
      `DEFAULT_CONFIG must not contain the project path literal "${literal}"`,
    )
  }
})

test('BOS-850: every DEFAULT_CONFIG lens glob is language-shaped (starts with **/)', () => {
  // The structural companion to the literal gate: requiring the `**/` prefix mechanically
  // forbids anchoring a default lens to any top-level directory, including a directory name
  // the four banned literals above would not catch.
  for (const rule of DEFAULT_CONFIG.lensMap) {
    const globs = Array.isArray(rule.globs) ? rule.globs : [rule.glob]
    for (const glob of globs) {
      assert.ok(
        glob.startsWith('**/'),
        `default lens "${rule.id}" glob "${glob}" must start with "**/" (no path anchoring)`,
      )
    }
  }
})

test('BOS-850: DEFAULT_CONFIG ships no commands block and no test manifest', () => {
  assert.equal('commands' in DEFAULT_CONFIG, false)
  assert.equal('test' in DEFAULT_CONFIG, false)
})

test('DEFAULT_CONFIG lenses each carry a non-empty inline fallbackRubric', () => {
  // The defaults are the fallback when no .boss-skills.json is present, so a
  // checkout without one still gets a real inline rubric per lens — otherwise a
  // non-vendored lens skill (e.g. impeccable) could dispatch with nothing to
  // substitute into Phase 1.
  for (const rule of DEFAULT_CONFIG.lensMap) {
    assert.ok(
      typeof rule.fallbackRubric === 'string' && rule.fallbackRubric.trim().length > 0,
      `default lens "${rule.id}" needs a non-empty fallbackRubric`,
    )
  }
})

// --- Task 3: loader — discovery, merge, validation ------------------------

function scratchRepo(configJson) {
  const root = mkdtempSync(join(tmpdir(), 'skill-config-'))
  if (configJson !== undefined) writeFileSync(join(root, '.boss-skills.json'), configJson)
  const nested = join(root, 'a', 'b')
  mkdirSync(nested, { recursive: true })
  return { root, nested, cleanup: () => rmSync(root, { recursive: true, force: true }) }
}

test('findConfigFile walks up to the repo root', () => {
  const { root, nested, cleanup } = scratchRepo('{}')
  try {
    assert.equal(findConfigFile(nested), join(root, '.boss-skills.json'))
  } finally {
    cleanup()
  }
})

test('findConfigFile returns null when absent', () => {
  const { nested, cleanup } = scratchRepo(undefined)
  try {
    assert.equal(findConfigFile(nested), null)
  } finally {
    cleanup()
  }
})

test('loadSkillConfig returns defaults when no file present', () => {
  const { nested, cleanup } = scratchRepo(undefined)
  try {
    const cfg = loadSkillConfig({ cwd: nested })
    assert.equal(cfg.adapters.tracker, 'linear')
    assert.deepEqual(cfg.lensMap.map((r) => r.id).sort(), ['api', 'db', 'go', 'web'])
    // A bare scratch dir declares no build system, so detection adds nothing and the
    // accessors report absence rather than throwing. The key must be ABSENT, not an
    // empty object: `commands` in cfg is how a consumer distinguishes "nothing declared"
    // from "declared empty".
    assert.equal('commands' in cfg, false)
    assert.equal(manifestPath(cfg), null)
    assert.equal(command(cfg, 'build'), null)
    assert.equal(moduleTestCommand(cfg, 'boss'), null)
  } finally {
    cleanup()
  }
})

test('planStorageFor always resolves tracker attachments', () => {
  assert.deepEqual(planStorageFor(DEFAULT_CONFIG), { kind: 'tracker-attachment' })
  assert.deepEqual(planStorageFor({ planStorage: { kind: 'r2' } }), { kind: 'tracker-attachment' })
})

test('validateConfig warns and coerces legacy R2 plan storage', () => {
  const config = mergeConfig(DEFAULT_CONFIG, { planStorage: { kind: 'r2' } })
  const originalWarn = console.warn
  const warnings = []
  console.warn = (message) => warnings.push(String(message))
  try {
    validateConfig(config, 'test')
  } finally {
    console.warn = originalWarn
  }
  assert.deepEqual(config.planStorage, { kind: 'tracker-attachment' })
  assert.match(warnings.join('\n'), /planStorage\.kind="r2" is deprecated and ignored/)
})

test('validateConfig rejects an unknown plan storage kind', () => {
  assert.throws(
    () => validateConfig(mergeConfig(DEFAULT_CONFIG, { planStorage: { kind: 'unknown' } }), 'test'),
    /skill-config:.*planStorage\.kind must be "tracker-attachment"/,
  )
})

test('reviewLedgerConfig reads the durable review ledger directory', () => {
  assert.deepEqual(reviewLedgerConfig(DEFAULT_CONFIG), { dir: '.git/boss-review-ledgers' })
  assert.deepEqual(reviewLedgerConfig({ reviewLedger: { dir: 'custom/ledgers' } }), {
    dir: 'custom/ledgers',
  })
  validateConfig(DEFAULT_CONFIG, 'test')
})

test('validateConfig rejects malformed reviewLedger configuration', () => {
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, reviewLedger: [] }, 'test'),
    /skill-config:.*reviewLedger must be an object/,
  )
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, reviewLedger: { dir: '' } }, 'test'),
    /skill-config:.*reviewLedger\.dir must be a non-empty string/,
  )
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, reviewLedger: { dir: '/tmp/ledgers' } }, 'test'),
    /skill-config:.*reviewLedger\.dir must be repo-relative/,
  )
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, reviewLedger: { dir: '..\\shared' } }, 'test'),
    /skill-config:.*reviewLedger\.dir must use POSIX separators/,
  )
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, reviewLedger: { dir: '../shared' } }, 'test'),
    /skill-config:.*reviewLedger\.dir must stay within the repository/,
  )
  assert.throws(
    () =>
      validateConfig({ ...DEFAULT_CONFIG, reviewLedger: { dir: 'custom/../../shared' } }, 'test'),
    /skill-config:.*reviewLedger\.dir must stay within the repository/,
  )
})

test('mergeConfig replaces arrays and shallow-merges objects', () => {
  const merged = mergeConfig(
    { lensMap: [{ id: 'go' }], adapters: { tracker: 'linear', publish: 'proof' } },
    { lensMap: [{ id: 'rb' }], adapters: { tracker: 'jira' } },
  )
  assert.deepEqual(merged.lensMap, [{ id: 'rb' }]) // array replaced wholesale
  assert.deepEqual(merged.adapters, { tracker: 'jira', publish: 'proof' }) // object merged
})

test('loadSkillConfig overrides a single adapter, keeps the rest', () => {
  const { nested, cleanup } = scratchRepo('{"adapters":{"tracker":"jira"}}')
  try {
    const cfg = loadSkillConfig({ cwd: nested })
    assert.equal(cfg.adapters.tracker, 'jira')
    assert.equal(cfg.adapters.publish, 'proof') // default preserved
  } finally {
    cleanup()
  }
})

test('loadSkillConfig throws a clear error on malformed JSON', () => {
  const { nested, cleanup } = scratchRepo('{ not json')
  try {
    assert.throws(() => loadSkillConfig({ cwd: nested }), /skill-config:.*not valid JSON/)
  } finally {
    cleanup()
  }
})

test('loadSkillConfig rejects a non-object config (null, array, primitive)', () => {
  // Valid JSON that is not an object would merge as an empty override and
  // silently fall back to defaults — it must fail with a skill-config: error.
  for (const body of ['null', '[]', '"nope"', '42']) {
    const { nested, cleanup } = scratchRepo(body)
    try {
      assert.throws(
        () => loadSkillConfig({ cwd: nested }),
        /skill-config:.*must contain a JSON object/,
      )
    } finally {
      cleanup()
    }
  }
})

test('validateConfig rejects a non-string / empty command override', () => {
  // A malformed command value must fail here rather than throwing a raw
  // TypeError later when an accessor calls .replace() on it.
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, commands: { testModule: null } }, 'test'),
    /skill-config:.*commands\.testModule must be a non-empty string/,
  )
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, commands: { build: '' } }, 'test'),
    /skill-config:.*commands\.build must be a non-empty string/,
  )
})

test('BOS-850: validateConfig accepts a config with no commands and no test block', () => {
  // The defaults themselves are the primary fixture: absent (not {}) must validate.
  validateConfig(DEFAULT_CONFIG, 'test')
  validateConfig({ ...DEFAULT_CONFIG, commands: { build: 'make' } }, 'test')
  validateConfig({ ...DEFAULT_CONFIG, test: {} }, 'test')
  validateConfig({ ...DEFAULT_CONFIG, test: { manifestPath: 'docs/t.md' } }, 'test')
})

test('BOS-850: validateConfig still rejects a present-but-malformed commands / test block', () => {
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, commands: [] }, 'test'),
    /skill-config:.*commands must be an object when present/,
  )
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, commands: null }, 'test'),
    /skill-config:.*commands must be an object when present/,
  )
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, test: [] }, 'test'),
    /skill-config:.*test must be an object when present/,
  )
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, test: { manifestPath: 7 } }, 'test'),
    /skill-config:.*test\.manifestPath must be a non-empty string when present/,
  )
})

test('validateConfig rejects an empty test.manifestPath', () => {
  // Symmetry with commands.*: manifestPath() treats "" as absent and returns null, so accepting
  // it would let a repo believe it configured a manifest while every core reads "none".
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, test: { manifestPath: '' } }, 'test'),
    /skill-config:.*test\.manifestPath must be a non-empty string when present/,
  )
})

// BOS-856: the opportunistic default-round registry. It is the seam that keeps a concrete
// reviewer's name out of the published, project-agnostic cores — a core knows only a capability id.
test('DEFAULT_CONFIG ships the default review rounds and reviewDefaultRounds reads them', () => {
  const rounds = reviewDefaultRounds(DEFAULT_CONFIG)
  assert.deepEqual(
    rounds.map((r) => r.capability),
    ['second-voice', 'code-review'],
  )
  const byId = Object.fromEntries(rounds.map((r) => [r.capability, r]))
  assert.equal(byId['second-voice'].kind, 'cross-agent')
  assert.equal(byId['code-review'].kind, 'skill')
  // A kind:'skill' entry must name what to dispatch; that name lives in config, never in a core.
  assert.equal(typeof byId['code-review'].skill, 'string')
  assert.ok(byId['code-review'].skill.length > 0)
  validateConfig(DEFAULT_CONFIG, 'test')
})

test('DEFAULT_CONFIG documents review delta defaults and reviewDeltaDefaults reads them', () => {
  assert.deepEqual(reviewDeltaDefaults(DEFAULT_CONFIG), {
    deltaFileThreshold: 20,
    forceFull: false,
  })
  assert.equal(REVIEW_DEFAULT_MAX_DISPATCHED_ROUNDS, 6)
  assert.equal(reviewMaxDispatchedRoundDefault(DEFAULT_CONFIG), 6)
  validateConfig(DEFAULT_CONFIG, 'test')
})

test('reviewDefaultRounds returns [] for a config carrying no registry', () => {
  // [] is the honest "this repo default-runs no extra round" — a config predating the block, or one
  // that merged it away, must degrade to an empty phase rather than failing a review run.
  assert.deepEqual(reviewDefaultRounds({}), [])
  assert.deepEqual(reviewDefaultRounds({ reviewDefaults: {} }), [])
  assert.deepEqual(reviewDefaultRounds(undefined), [])
  validateConfig({ ...DEFAULT_CONFIG, reviewDefaults: undefined }, 'test')
})

test('reviewDeltaDefaults returns documented fallbacks for a config carrying no registry', () => {
  assert.deepEqual(reviewDeltaDefaults({}), {
    deltaFileThreshold: 20,
    forceFull: false,
  })
  assert.deepEqual(reviewDeltaDefaults(undefined), {
    deltaFileThreshold: 20,
    forceFull: false,
  })
  assert.equal(reviewMaxDispatchedRoundDefault({}), 6)
  assert.equal(reviewMaxDispatchedRoundDefault(undefined), 6)
})

test('mergeConfig replaces reviewDefaults.rounds wholesale', () => {
  const merged = mergeConfig(DEFAULT_CONFIG, {
    reviewDefaults: { rounds: [{ capability: 'second-voice', kind: 'cross-agent' }] },
  })
  assert.deepEqual(reviewDefaultRounds(merged), [
    { capability: 'second-voice', kind: 'cross-agent' },
  ])
})

test('mergeConfig preserves reviewDefaults delta keys beside a rounds override', () => {
  const merged = mergeConfig(DEFAULT_CONFIG, {
    reviewDefaults: {
      rounds: [{ capability: 'second-voice', kind: 'cross-agent' }],
      deltaFileThreshold: 7,
      maxDispatchedRounds: 4,
      forceFull: true,
    },
  })
  assert.deepEqual(reviewDeltaDefaults(merged), {
    deltaFileThreshold: 7,
    forceFull: true,
  })
  assert.equal(reviewMaxDispatchedRoundDefault(merged), 4)
})

test('validateConfig rejects a non-array reviewDefaults.rounds', () => {
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, reviewDefaults: { rounds: {} } }, 'test'),
    /skill-config:.*reviewDefaults\.rounds must be an array/,
  )
})

test('validateConfig warns and coerces malformed reviewDefaults delta settings', () => {
  for (const deltaFileThreshold of [undefined, null, '7', 1.5, -1]) {
    const cfg = mergeConfig(DEFAULT_CONFIG, { reviewDefaults: { deltaFileThreshold } })
    const originalWarn = console.warn
    const warnings = []
    console.warn = (message) => warnings.push(String(message))
    try {
      validateConfig(cfg, 'test')
    } finally {
      console.warn = originalWarn
    }
    assert.equal(cfg.reviewDefaults.deltaFileThreshold, 20)
    assert.equal(cfg.reviewDefaults.forceFull, false)
    assert.match(warnings.join('\n'), /reviewDefaults\.deltaFileThreshold/)
  }
})

test('validateConfig warns and coerces malformed reviewDefaults maxDispatchedRounds settings', () => {
  for (const maxDispatchedRounds of [undefined, null, '4', 1.5, -1, 0, 7]) {
    const cfg = mergeConfig(DEFAULT_CONFIG, { reviewDefaults: { maxDispatchedRounds } })
    const originalWarn = console.warn
    const warnings = []
    console.warn = (message) => warnings.push(String(message))
    try {
      validateConfig(cfg, 'test')
    } finally {
      console.warn = originalWarn
    }
    assert.equal(cfg.reviewDefaults.maxDispatchedRounds, 6)
    assert.equal(reviewMaxDispatchedRoundDefault(cfg), 6)
    assert.match(warnings.join('\n'), /reviewDefaults\.maxDispatchedRounds/)
    assert.match(warnings.join('\n'), /using 6/)
  }
})

test('validateConfig accepts a lower reviewDefaults maxDispatchedRounds value', () => {
  const cfg = mergeConfig(DEFAULT_CONFIG, { reviewDefaults: { maxDispatchedRounds: 3 } })
  validateConfig(cfg, 'test')
  assert.equal(reviewMaxDispatchedRoundDefault(cfg), 3)
})

test('validateConfig accepts only boolean true for reviewDefaults.forceFull', () => {
  const enabled = mergeConfig(DEFAULT_CONFIG, { reviewDefaults: { forceFull: true } })
  validateConfig(enabled, 'test')
  assert.deepEqual(reviewDeltaDefaults(enabled), { deltaFileThreshold: 20, forceFull: true })

  for (const forceFull of [false, 'true', 1, null, undefined]) {
    const cfg = mergeConfig(DEFAULT_CONFIG, { reviewDefaults: { forceFull } })
    validateConfig(cfg, 'test')
    assert.deepEqual(reviewDeltaDefaults(cfg), { deltaFileThreshold: 20, forceFull: false })
  }
})

test('validateConfig rejects a default round with an empty or non-string capability', () => {
  // reviewDefaultRounds() hands entries straight to a core's default-round phase, which
  // dereferences `capability` for its ledger line and its suppression check.
  assert.throws(
    () =>
      validateConfig(
        {
          ...DEFAULT_CONFIG,
          reviewDefaults: { rounds: [{ capability: '', kind: 'cross-agent' }] },
        },
        'test',
      ),
    /skill-config:.*non-empty string capability/,
  )
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, reviewDefaults: { rounds: [{ capability: 7, kind: 'cross-agent' }] } },
        'test',
      ),
    /skill-config:.*non-empty string capability/,
  )
})

test('validateConfig rejects a default round with an unknown kind', () => {
  // An unrecognised kind has no probe, so the round would silently do nothing on every run —
  // indistinguishable from the capability being unavailable.
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, reviewDefaults: { rounds: [{ capability: 'x', kind: 'telepathy' }] } },
        'test',
      ),
    /skill-config:.*kind must be one of/,
  )
})

test("validateConfig rejects a kind:'skill' default round with no skill", () => {
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, reviewDefaults: { rounds: [{ capability: 'x', kind: 'skill' }] } },
        'test',
      ),
    /skill-config:.*needs a non-empty string skill/,
  )
})

// Phase D keys duplicate suppression and its ledger lines on `capability`, so two entries
// sharing an id are one ambiguous id, not two rounds — and a "covered by extension" drop
// silently applies to both. The collision must fail at config time, not read as a registry.
test('validateConfig rejects two default rounds sharing a capability', () => {
  assert.throws(
    () =>
      validateConfig(
        {
          ...DEFAULT_CONFIG,
          reviewDefaults: {
            rounds: [
              { capability: 'code-review', kind: 'cross-agent' },
              { capability: 'code-review', kind: 'skill', skill: 'some:reviewer' },
            ],
          },
        },
        'test',
      ),
    /skill-config:.*"code-review" duplicates an earlier capability/,
  )
  // Distinct ids are the ordinary case and must stay accepted.
  validateConfig(
    {
      ...DEFAULT_CONFIG,
      reviewDefaults: {
        rounds: [
          { capability: 'second-voice', kind: 'cross-agent' },
          { capability: 'code-review', kind: 'skill', skill: 'some:reviewer' },
        ],
      },
    },
    'test',
  )
})

test('validateConfig rejects a non-object reviewDefaults or rounds entry', () => {
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, reviewDefaults: [] }, 'test'),
    /skill-config:.*reviewDefaults must be an object when present/,
  )
  assert.throws(
    () =>
      validateConfig({ ...DEFAULT_CONFIG, reviewDefaults: { rounds: ['second-voice'] } }, 'test'),
    /skill-config:.*reviewDefaults\.rounds entries must be objects/,
  )
})

test('validateConfig rejects a lensMap rule with no matcher', () => {
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, lensMap: [{ id: 'x', skill: 'y' }] }, 'test'),
    /skill-config:.*lensMap/,
  )
})

test('validateConfig rejects a well-formed lens missing a fallbackRubric', () => {
  // A lens with a valid matcher but no inline fallback would silently drop its
  // specialist pass when the named skill can't be loaded — reject it here.
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, lensMap: [{ id: 'x', skill: 'y', glob: '**/*.go' }] },
        'test',
      ),
    /skill-config:.*non-empty string fallbackRubric/,
  )
  assert.throws(
    () =>
      validateConfig(
        {
          ...DEFAULT_CONFIG,
          lensMap: [{ id: 'x', skill: 'y', glob: '**/*.go', fallbackRubric: '' }],
        },
        'test',
      ),
    /skill-config:.*non-empty string fallbackRubric/,
  )
})

test('validateConfig rejects a globs array with a non-string/empty entry', () => {
  // A malformed globs entry must fail with a skill-config: error, not crash
  // later in globToRegExp with a raw TypeError.
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, lensMap: [{ id: 'x', skill: 'y', globs: [null] }] },
        'test',
      ),
    /skill-config:.*globs must be a non-empty array/,
  )
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, lensMap: [{ id: 'x', skill: 'y', globs: [''] }] },
        'test',
      ),
    /skill-config:.*globs must be a non-empty array/,
  )
})

test('validateConfig rejects an empty-matcher rule (empty glob or empty globs array)', () => {
  // An empty singular glob or an empty globs array is a silent no-op matcher —
  // reject it with a clear skill-config: error rather than validating a rule
  // that can never match anything.
  assert.throws(
    () =>
      validateConfig({ ...DEFAULT_CONFIG, lensMap: [{ id: 'x', skill: 'y', glob: '' }] }, 'test'),
    /skill-config:.*glob must be a non-empty string/,
  )
  assert.throws(
    () =>
      validateConfig({ ...DEFAULT_CONFIG, lensMap: [{ id: 'x', skill: 'y', globs: [] }] }, 'test'),
    /skill-config:.*globs must be a non-empty array/,
  )
})

test('validateConfig rejects an empty lens id or skill', () => {
  // An empty id makes a "" change-type key that dispatch never fires; an empty
  // skill makes skillForLens() return "". Both must fail here, not downstream.
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, lensMap: [{ id: '', skill: 'y', glob: '**/*.go' }] },
        'test',
      ),
    /skill-config:.*non-empty string id/,
  )
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, lensMap: [{ id: 'go', skill: '', glob: '**/*.go' }] },
        'test',
      ),
    /skill-config:.*non-empty string skill/,
  )
})

test('validateConfig rejects a malformed headlessSignals entry', () => {
  // A non-object entry or one without a string var throws a raw TypeError in
  // isHeadless(); reject it here with a skill-config: error instead.
  const withSignals = (headlessSignals) => ({
    ...DEFAULT_CONFIG,
    env: { ...DEFAULT_CONFIG.env, headlessSignals },
  })
  assert.throws(
    () => validateConfig(withSignals([null]), 'test'),
    /skill-config:.*headlessSignals entries must be objects/,
  )
  assert.throws(
    () => validateConfig(withSignals([{ equals: 'true' }]), 'test'),
    /skill-config:.*needs a non-empty string var/,
  )
  assert.throws(
    () => validateConfig(withSignals([{ var: 'FOO' }]), 'test'),
    /skill-config:.*present:true or a string equals/,
  )
})

test('validateConfig rejects a non-boolean headlessWhenNoTty', () => {
  // The JSON string "false" would coerce truthy in isHeadless(), keeping
  // no-TTY runs headless; require an actual boolean.
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, env: { ...DEFAULT_CONFIG.env, headlessWhenNoTty: 'false' } },
        'test',
      ),
    /skill-config:.*headlessWhenNoTty must be a boolean/,
  )
})

test('validateConfig rejects malformed adapter selections', () => {
  // An array or a null/non-string selection yields undefined from adapterFor();
  // require a non-empty string per supported adapter kind.
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, adapters: [] }, 'test'),
    /skill-config:.*adapters must be an object/,
  )
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, adapters: { ...DEFAULT_CONFIG.adapters, tracker: null } },
        'test',
      ),
    /skill-config:.*adapters\.tracker must be a non-empty string/,
  )
})

test('findConfigFile terminates on a relative startDir (fixed-point guard, no infinite loop)', () => {
  // Regression guard: a relative startDir has an empty parse().root, so the loop
  // must stop at the dirname fixed point ('.') instead of spinning forever. The
  // return value depends on cwd; all that matters is that the call terminates.
  const result = findConfigFile('a/b/c')
  assert.ok(result === null || typeof result === 'string')
})

test('lensesForFile honours a multi-glob (globs:[...]) rule', () => {
  const cfg = { lensMap: [{ id: 'multi', skill: 's', globs: ['docs/**', '**/*.md'] }] }
  assert.deepEqual(lensesForFile(cfg, 'docs/x.txt'), ['multi'])
  assert.deepEqual(lensesForFile(cfg, 'README.md'), ['multi'])
  assert.deepEqual(lensesForFile(cfg, 'main.go'), [])
})

// --- BOS-850: detected happy defaults -------------------------------------

/** A scratch dir seeded with {relativePath: contents}. Returns {dir, cleanup}. */
function markerRepo(files) {
  const dir = mkdtempSync(join(tmpdir(), 'skillcfg-detect-'))
  for (const [name, contents] of Object.entries(files)) writeFileSync(join(dir, name), contents)
  return { dir, cleanup: () => rmSync(dir, { recursive: true, force: true }) }
}

function withMarkers(files, fn) {
  const { dir, cleanup } = markerRepo(files)
  try {
    return fn(dir)
  } finally {
    cleanup()
  }
}

test('detectRepoDefaults reads declared Makefile targets, not the Makefile itself', () => {
  withMarkers(
    {
      Makefile: [
        'GO := go', // a variable assignment is not a target
        '.PHONY: build lint',
        '',
        'build: deps',
        '\t$(GO) build ./...',
        '',
        'lint::',
        '\tgolangci-lint run',
        '',
        'deps:',
        '\techo deps',
      ].join('\n'),
    },
    (dir) => {
      // No `test:` and no `format:` target were declared, so no test/format command is
      // invented — the whole point of reading targets instead of marker presence.
      assert.deepEqual(detectRepoDefaults({ cwd: dir }), {
        commands: { build: 'make build', lint: 'make lint' },
      })
    },
  )
})

test('detectRepoDefaults returns {} for a Makefile declaring no recognised target', () => {
  withMarkers({ Makefile: 'deploy:\n\techo deploy\n' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {})
  })
})

test('detectRepoDefaults reads every target a multi-target or continued rule head declares', () => {
  // `build lint:` declares BOTH targets, and make joins a head split over a `\` continuation
  // before parsing it — reading only the first name would miss a real declaration.
  withMarkers({ Makefile: 'build lint:\n\techo both\n' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {
      commands: { build: 'make build', lint: 'make lint' },
    })
  })
  withMarkers({ Makefile: 'format \\\n  test:\n\techo joined\n' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {
      commands: { format: 'make format', test: 'make test' },
    })
  })
})

test('detectRepoDefaults ignores a target-specific variable assignment', () => {
  // `build: CFLAGS=-O2` scopes a variable to `build`; on its own it declares no recipe, so
  // `make build` fails. The `test:` rule below it is the only real declaration here.
  withMarkers({ Makefile: 'build: CFLAGS=-O2\n\ntest:\n\techo real\n' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), { commands: { test: 'make test' } })
  })
})

test('detectRepoDefaults reads the makefile GNU make itself would read', () => {
  // GNU make's lookup order is GNUmakefile, makefile, Makefile. A repo carrying two would
  // otherwise be reported from the file make never opens — `make test` for a rule that,
  // as `make -n test` shows, does not exist.
  withMarkers(
    { GNUmakefile: 'build:\n\techo real\n', Makefile: 'test:\n\techo shadowed\n' },
    (dir) => {
      assert.deepEqual(detectRepoDefaults({ cwd: dir }), { commands: { build: 'make build' } })
    },
  )
  // `makefile` vs `Makefile` is deliberately NOT exercised: they are the same path on a
  // case-insensitive filesystem, so the pair cannot be seeded portably.
})

test('detectRepoDefaults ignores rule heads inside a define ... endef body', () => {
  withMarkers(
    {
      Makefile: [
        'define MODULE_RULES', // a template, expanded only by an $(eval) that may never happen
        'build:',
        '\techo not-a-real-target',
        'format:',
        '\techo also-not-real',
        'endef',
        '',
        'test:', // the only rule this Makefile actually declares
        '\tgo test ./...',
      ].join('\n'),
    },
    (dir) => {
      assert.deepEqual(detectRepoDefaults({ cwd: dir }), { commands: { test: 'make test' } })
    },
  )
})

test('detectRepoDefaults stops scanning at an unterminated define (under-detects, never over-)', () => {
  withMarkers({ Makefile: 'define BODY\nbuild:\n\techo x\n' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {})
  })
})

test('detectRepoDefaults ignores rule heads inside an ifeq ... endif body', () => {
  // Whether the branch is live means evaluating variables this static reader never evaluates, so
  // a target only reachable there is not a target the repo declares: emitting `make test` for a
  // repo whose condition is false hands a core a command that fails.
  withMarkers(
    {
      Makefile: [
        'ifeq ($(CI),1)',
        'test:',
        '\techo ci-only',
        'else',
        'lint:', // an else branch is still inside the same conditional
        '\techo not-ci',
        'endif',
        '',
        'build:', // the only unconditional rule this Makefile declares
        '\techo real',
      ].join('\n'),
    },
    (dir) => {
      assert.deepEqual(detectRepoDefaults({ cwd: dir }), { commands: { build: 'make build' } })
    },
  )
})

test('detectRepoDefaults counts nested conditionals so the first endif does not leak', () => {
  withMarkers(
    {
      Makefile: [
        'ifdef RELEASE',
        'ifneq ($(OS),linux)',
        'lint:',
        '\techo inner',
        'endif', // closes only the inner conditional
        'test:',
        '\techo outer',
        'endif',
        'format:',
        '\techo real',
      ].join('\n'),
    },
    (dir) => {
      assert.deepEqual(detectRepoDefaults({ cwd: dir }), { commands: { format: 'make format' } })
    },
  )
})

test('detectRepoDefaults stops scanning at an unterminated ifeq (under-detects, never over-)', () => {
  // Same call as an unterminated `define`: the rest of the file is swallowed, which under-detects
  // rather than over-detects — and it is the same file make itself would reject.
  withMarkers({ Makefile: 'ifeq ($(CI),1)\nbuild:\n\techo x\n\ntest:\n\techo y\n' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {})
  })
})

test('detectRepoDefaults treats a hyphenated ifeq-like target as a target, not a conditional', () => {
  // `ifeq-check:` is a legal target name. make requires the directive to be a standalone word, so
  // this opens no conditional — matching it as one would swallow every rule after it.
  withMarkers(
    {
      Makefile: ['ifeq-check:', '\techo x', '', 'build:', '\techo b', '', 'test:', '\techo t'].join(
        '\n',
      ),
    },
    (dir) => {
      assert.deepEqual(detectRepoDefaults({ cwd: dir }), {
        commands: { build: 'make build', test: 'make test' },
      })
    },
  )
})

test('detectRepoDefaults does not close a conditional on a hyphenated endif-like rule head', () => {
  // `endif-foo:` is a rule head, not the `endif` directive: closing on it would reopen scanning
  // inside the conditional and over-detect the targets declared there.
  withMarkers(
    {
      Makefile: [
        'ifeq ($(CI),1)',
        'endif-foo:',
        '\techo x',
        'test:', // still inside the conditional
        '\techo ci-only',
        'endif',
        'build:',
        '\techo real',
      ].join('\n'),
    },
    (dir) => {
      assert.deepEqual(detectRepoDefaults({ cwd: dir }), { commands: { build: 'make build' } })
    },
  )
})

test('detectRepoDefaults maps a Makefile fmt target onto the format key', () => {
  withMarkers({ Makefile: 'fmt:\n\tgofmt -w .\n' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), { commands: { format: 'make fmt' } })
  })
})

test('detectRepoDefaults runs package.json scripts through the lockfile package manager', () => {
  const pkg = JSON.stringify({ scripts: { build: 'vite build', test: 'vitest', deploy: 'x' } })
  withMarkers({ 'package.json': pkg, 'pnpm-lock.yaml': '' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {
      commands: { build: 'pnpm run build', test: 'pnpm run test' },
    })
  })
  withMarkers({ 'package.json': pkg, 'yarn.lock': '' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {
      commands: { build: 'yarn run build', test: 'yarn run test' },
    })
  })
  // bun ships two lockfile names — the binary `bun.lockb` and the newer text `bun.lock`. Either
  // one names bun; falling through to `npm run build` would hand a core the wrong runner.
  for (const lock of ['bun.lockb', 'bun.lock']) {
    withMarkers({ 'package.json': pkg, [lock]: '' }, (dir) => {
      assert.deepEqual(
        detectRepoDefaults({ cwd: dir }),
        { commands: { build: 'bun run build', test: 'bun run test' } },
        `${lock} must select bun`,
      )
    })
  }
  // No lockfile: npm is the safe default rather than no command at all.
  withMarkers({ 'package.json': pkg }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {
      commands: { build: 'npm run build', test: 'npm run test' },
    })
  })
})

test('detectRepoDefaults detects nothing from a malformed or script-less package.json', () => {
  withMarkers({ 'package.json': '{ not json' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {})
  })
  withMarkers({ 'package.json': '{"name":"x"}' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {})
  })
})

test('detectRepoDefaults maps Cargo and Go toolchains onto their standard subcommands', () => {
  withMarkers({ 'Cargo.toml': '[package]\nname = "x"\n' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {
      commands: {
        build: 'cargo build',
        lint: 'cargo clippy',
        format: 'cargo fmt',
        test: 'cargo test',
      },
    })
  })
  withMarkers({ 'go.mod': 'module example.com/x\n' }, (dir) => {
    // No lint: no linter ships with the Go toolchain, so inventing one would fail.
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {
      commands: { build: 'go build ./...', format: 'go fmt ./...', test: 'go test ./...' },
    })
  })
})

test('detectRepoDefaults resolves precedence first-writer-wins (Makefile outranks go.mod)', () => {
  withMarkers({ Makefile: 'test:\n\tmake test\n', 'go.mod': 'module example.com/x\n' }, (dir) => {
    const { commands } = detectRepoDefaults({ cwd: dir })
    assert.equal(commands.test, 'make test') // the Makefile's declared target wins
    assert.equal(commands.build, 'go build ./...') // go.mod fills only what it left empty
    assert.equal(commands.format, 'go fmt ./...')
    assert.equal(commands.lint, undefined)
  })
})

test('detectRepoDefaults returns {} for a directory declaring no build system', () => {
  withMarkers({ 'README.md': '# hi\n' }, (dir) => {
    assert.deepEqual(detectRepoDefaults({ cwd: dir }), {})
  })
})

test('detectRepoDefaults never invents commands.testModule', () => {
  // A per-module test target is a repo-shaped convention, not a language fact.
  const all = {
    Makefile: 'build:\n\ttrue\nlint:\n\ttrue\nformat:\n\ttrue\ntest:\n\ttrue\n',
    'package.json': JSON.stringify({ scripts: { build: 'x', lint: 'x', test: 'x' } }),
    'Cargo.toml': '[package]\n',
    'go.mod': 'module x\n',
  }
  withMarkers(all, (dir) => {
    const { commands } = detectRepoDefaults({ cwd: dir })
    assert.equal('testModule' in commands, false)
    assert.deepEqual(Object.keys(commands).sort(), ['build', 'format', 'lint', 'test'])
    assert.equal(moduleTestCommand({ commands }, 'boss'), null)
  })
})

test('detectRepoDefaults writes nothing — the marker dir is byte-identical afterwards', () => {
  const all = { Makefile: 'build:\n\ttrue\n', 'package.json': '{"scripts":{"test":"x"}}' }
  withMarkers(all, (dir) => {
    const before = readdirSync(dir).sort()
    detectRepoDefaults({ cwd: dir })
    loadSkillConfig({ cwd: dir })
    assert.deepEqual(readdirSync(dir).sort(), before)
  })
})

test('loadSkillConfig layers detection between the defaults and the repo config', () => {
  withMarkers({ Makefile: 'build:\n\ttrue\nlint:\n\ttrue\n' }, (dir) => {
    const cfg = loadSkillConfig({ cwd: dir })
    assert.equal(command(cfg, 'build'), 'make build')
    assert.equal(command(cfg, 'lint'), 'make lint')
    assert.equal(command(cfg, 'test'), null) // undeclared stays null, never guessed
    assert.equal(manifestPath(cfg), null) // never detected
    assert.deepEqual(
      cfg.lensMap.map((r) => r.id).sort(),
      ['api', 'db', 'go', 'web'], // the default catalogue is untouched by detection
    )
  })
})

test('loadSkillConfig skips detection PER KEY, not for the whole commands block', () => {
  // The declared key wins outright; the keys the config leaves absent still get detected, which
  // is the documented shallow-merge (DEFAULT_CONFIG < detected < file), not all-or-nothing.
  withMarkers(
    {
      Makefile: 'build:\n\ttrue\nlint:\n\ttrue\ntest:\n\ttrue\n',
      '.boss-skills.json': JSON.stringify({ commands: { build: 'bazel build //...' } }),
    },
    (dir) => {
      const cfg = loadSkillConfig({ cwd: dir })
      assert.equal(command(cfg, 'build'), 'bazel build //...')
      assert.equal(command(cfg, 'lint'), 'make lint')
      assert.equal(command(cfg, 'test'), 'make test')
      assert.equal(command(cfg, 'format'), null) // undeclared and undetected stays null
    },
  )
})

test('loadSkillConfig still detects for a config declaring only an undetectable key', () => {
  // The footgun the per-key rule removes: `commands.testModule` is never detected, so an
  // all-or-nothing short-circuit made declaring it silently forfeit all four detected commands.
  withMarkers(
    {
      Makefile: 'build:\n\ttrue\nlint:\n\ttrue\nformat:\n\ttrue\ntest:\n\ttrue\n',
      '.boss-skills.json': JSON.stringify({ commands: { testModule: 'make test-{module}' } }),
    },
    (dir) => {
      const cfg = loadSkillConfig({ cwd: dir })
      assert.equal(moduleTestCommand(cfg, 'boss'), 'make test-boss')
      assert.equal(command(cfg, 'build'), 'make build')
      assert.equal(command(cfg, 'lint'), 'make lint')
      assert.equal(command(cfg, 'format'), 'make format')
      assert.equal(command(cfg, 'test'), 'make test')
    },
  )
})

test('detectRepoDefaults does no marker-file I/O when no detectable key is wanted', () => {
  // The property the old whole-block short-circuit bought, kept: a config declaring all four
  // detectable keys leaves `keys` empty, and detection returns before reading anything. Asserted
  // through the return value because the marker dir is full of files it would otherwise read.
  withMarkers(
    { Makefile: 'build:\n\ttrue\nlint:\n\ttrue\nformat:\n\ttrue\ntest:\n\ttrue\n' },
    (dir) => {
      assert.deepEqual(detectRepoDefaults({ cwd: dir, keys: [] }), {})
      // `testModule` is not detectable, so wanting only it is also a no-I/O call.
      assert.deepEqual(detectRepoDefaults({ cwd: dir, keys: ['testModule'] }), {})
      // ...and the skip happens before a single path is even constructed: a cwd that would throw
      // the moment it were joined returns cleanly, which no amount of "detects nothing" would.
      assert.deepEqual(detectRepoDefaults({ cwd: null, keys: [] }), {})
      // ...and a single wanted key detects only that key.
      assert.deepEqual(detectRepoDefaults({ cwd: dir, keys: ['lint'] }), {
        commands: { lint: 'make lint' },
      })
    },
  )
})

test('loadSkillConfig detects for a config file that declares no commands block', () => {
  withMarkers(
    { Makefile: 'test:\n\ttrue\n', '.boss-skills.json': JSON.stringify({ adapters: {} }) },
    (dir) => {
      assert.equal(command(loadSkillConfig({ cwd: dir }), 'test'), 'make test')
    },
  )
})

test('loadSkillConfig anchors detection at the config file dir, not a nested cwd', () => {
  const { dir, cleanup } = markerRepo({
    Makefile: 'test:\n\ttrue\n',
    '.boss-skills.json': '{}',
  })
  try {
    const nested = join(dir, 'a', 'b')
    mkdirSync(nested, { recursive: true })
    assert.equal(command(loadSkillConfig({ cwd: nested }), 'test'), 'make test')
  } finally {
    cleanup()
  }
})

// --- Task 4: accessors ----------------------------------------------------

test('lensesForFile matches by glob', () => {
  assert.deepEqual(lensesForFile(DEFAULT_CONFIG, 'services/web/src/App.tsx'), ['web'])
  assert.deepEqual(lensesForFile(DEFAULT_CONFIG, 'docs/foo.md'), [])
})

test('BOS-850: default lenses match by language anywhere in the tree', () => {
  // The point of the `**/`-only catalogue: the same file type resolves identically
  // whatever directory a foreign repo puts it in, and no lens is path-anchored.
  assert.deepEqual(lensesForFile(DEFAULT_CONFIG, 'x/y.go'), ['go'])
  assert.deepEqual(lensesForFile(DEFAULT_CONFIG, 'main.go'), ['go'])
  assert.deepEqual(lensesForFile(DEFAULT_CONFIG, 'db/migrations/001_init.sql'), ['db'])
  assert.deepEqual(lensesForFile(DEFAULT_CONFIG, 'src/api/schema.graphql'), ['api'])
  assert.deepEqual(lensesForFile(DEFAULT_CONFIG, 'app/styles/main.css'), ['web'])
  // A path under this checkout's own layout gets exactly the language lens and nothing
  // repo-shaped: the `tui` lens no longer fires on services/boss/**.
  assert.deepEqual(lensesForFile(DEFAULT_CONFIG, 'services/boss/internal/x.go'), ['go'])
})

test('BOS-850: the web lens covers plain JS as well as TypeScript', () => {
  // A repo that never adopted TypeScript must still select a lens under DEFAULT_CONFIG: without
  // these globs a JS-only change matched nothing at all.
  for (const path of [
    'src/index.js',
    'scripts/tool.mjs',
    'config/thing.cjs',
    'src/App.jsx',
    'src/App.tsx',
    'src/lib.ts',
  ]) {
    assert.deepEqual(
      lensesForFile(DEFAULT_CONFIG, path),
      ['web'],
      `${path} must select the web lens`,
    )
  }
})

test('detectChangeTypes reports every default lens (one-arg)', () => {
  assert.deepEqual(detectChangeTypes(['services/boss/internal/views/attach.go']), {
    go: true,
    web: false,
    db: false,
    api: false,
  })
  assert.deepEqual(detectChangeTypes(['docs/foo.md', 'CONCEPTS.md']), {
    go: false,
    web: false,
    db: false,
    api: false,
  })
})

test('skillForLens maps ids to review skills', () => {
  assert.equal(skillForLens(DEFAULT_CONFIG, 'go'), 'golang-pro')
  assert.equal(skillForLens(DEFAULT_CONFIG, 'web'), 'impeccable')
  assert.equal(skillForLens(DEFAULT_CONFIG, 'nope'), null)
})

test('command and moduleTestCommand read a configured commands block', () => {
  const cfg = mergeConfig(DEFAULT_CONFIG, {
    commands: { testSmoke: 'make test-smoke', lint: 'make lint', testModule: 'make test-{module}' },
  })
  assert.equal(command(cfg, 'testSmoke'), 'make test-smoke')
  assert.equal(command(cfg, 'lint'), 'make lint')
  assert.equal(moduleTestCommand(cfg, 'bossd'), 'make test-bossd')
})

test('BOS-850: the accessors return null (never throw) when the block is absent', () => {
  // Regression: moduleTestCommand() used to call .replace() on undefined and throw a
  // raw TypeError. `null` is the documented "not configured — go discover it" signal.
  assert.equal(command(DEFAULT_CONFIG, 'lint'), null)
  assert.equal(command(DEFAULT_CONFIG, 'nope'), null)
  assert.equal(moduleTestCommand(DEFAULT_CONFIG, 'bossd'), null)
  assert.equal(manifestPath(DEFAULT_CONFIG), null)
  // Empty-string entries are treated as absent too, not returned verbatim.
  assert.equal(command({ commands: { lint: '' } }, 'lint'), null)
  assert.equal(moduleTestCommand({ commands: { testModule: '' } }, 'x'), null)
  assert.equal(manifestPath({ test: { manifestPath: '' } }), null)
  assert.equal(manifestPath({}), null)
})

test('manifestPath returns a configured manifest', () => {
  const cfg = mergeConfig(DEFAULT_CONFIG, {
    test: { manifestPath: 'docs/testing/test-command-manifest.md' },
  })
  assert.equal(manifestPath(cfg), 'docs/testing/test-command-manifest.md')
})

test('isHeadless honours each configured signal', () => {
  assert.equal(isHeadless(DEFAULT_CONFIG, { BOSS_CRON: 'true' }, { isTTY: true }), true)
  assert.equal(isHeadless(DEFAULT_CONFIG, { BS_HEADLESS: '1' }, { isTTY: true }), true)
  assert.equal(isHeadless(DEFAULT_CONFIG, { OPENCLAW_SESSION: 'x' }, { isTTY: true }), true)
  assert.equal(isHeadless(DEFAULT_CONFIG, {}, { isTTY: false }), true) // no TTY
  assert.equal(isHeadless(DEFAULT_CONFIG, {}, { isTTY: true }), false)
  assert.equal(isHeadless(DEFAULT_CONFIG, { BOSS_CRON: 'false' }, { isTTY: true }), false)
})

test('adapterFor returns selections and rejects unknown kinds', () => {
  assert.equal(adapterFor(DEFAULT_CONFIG, 'tracker'), 'linear')
  assert.equal(adapterFor(DEFAULT_CONFIG, 'publish'), 'proof')
  assert.equal(adapterFor(DEFAULT_CONFIG, 'sessionRunner'), 'bossd')
  assert.throws(() => adapterFor(DEFAULT_CONFIG, 'bogus'), /skill-config:.*unknown adapter kind/)
})

// --- BOS-204: versioned plan-description contract -------------------------

test('planContract default is version 1 with today’s ordered section set', () => {
  assert.equal(planContractVersion(DEFAULT_CONFIG), 1)
  assert.deepEqual(
    planSections(DEFAULT_CONFIG).map((s) => s.heading),
    [
      '## Summary',
      '## Approach',
      '## Key changes',
      '## Testing',
      '## Risks / unknowns',
      '## Premises',
      '## Acceptance criteria',
      '## Required proof',
      '## Proof harness analysis',
      '## Why this needs a human',
      '## Open Questions',
      '## Planning',
      '## Original notes',
    ],
  )
})

test('requiredPlanSections excludes the conditional and optional sections', () => {
  const req = requiredPlanSections(DEFAULT_CONFIG)
  assert.ok(!req.includes('## Why this needs a human'))
  assert.ok(!req.includes('## Open Questions'))
  // `optional` is RECOGNISED but never required: registering the drafting template's
  // `## Proof harness analysis` must not newly require it of the plans already stamped v1.
  assert.ok(!req.includes('## Proof harness analysis'))
  assert.ok(!req.includes('## Premises'))
  assert.ok(req.includes('## Summary') && req.includes('## Original notes'))
})

// Build a plan description whose ## Planning body carries `planningLine`, laid out in the real
// emitted order (## Planning before the terminal ## Original notes).
const planDesc = (planningLine) =>
  requiredPlanSections(DEFAULT_CONFIG)
    .join('\n\nx\n\n')
    .replace('## Planning\n\nx', `## Planning\n\n${planningLine}`)

const epicParentDesc = () =>
  [
    '## Summary\n\nEpic parent overview.',
    '## Child tickets\n\n- [ ] BOS-1 — child plan',
    '## Planning\n\n- Contract: v1',
    '## Original notes\n\nReporter text.',
  ].join('\n\n')

test('validatePlanDescription accepts a well-formed v1 description', () => {
  const r = validatePlanDescription(DEFAULT_CONFIG, planDesc('- Contract: v1'))
  assert.deepEqual(r, {
    ok: true,
    version: 1,
    missing: [],
    unknown: [],
    unsupportedVersion: false,
  })
})

test('validatePlanDescription accepts an explicit epic-parent overview mode', () => {
  const r = validatePlanDescription(DEFAULT_CONFIG, epicParentDesc(), { mode: 'epic-parent' })
  assert.deepEqual(r, {
    ok: true,
    version: 1,
    missing: [],
    unknown: [],
    unsupportedVersion: false,
  })
})

test('validatePlanDescription keeps the default child-plan contract unchanged', () => {
  const r = validatePlanDescription(DEFAULT_CONFIG, epicParentDesc())
  assert.equal(r.ok, false)
  assert.ok(r.missing.includes('## Approach'))
  assert.ok(r.missing.includes('## Acceptance criteria'))
})

test('validatePlanDescription warns and falls back for an unknown mode', () => {
  const originalWarn = console.warn
  const warnings = []
  console.warn = (message) => warnings.push(String(message))
  try {
    const r = validatePlanDescription(DEFAULT_CONFIG, epicParentDesc(), { mode: 'future-parent' })
    assert.equal(r.ok, false)
    assert.ok(r.missing.includes('## Approach'))
  } finally {
    console.warn = originalWarn
  }
  assert.equal(warnings.length, 1)
  assert.match(warnings[0], /skill-config: validatePlanDescription unknown mode "future-parent"/)
  assert.match(warnings[0], /falling back to child-plan/)
})

test('validatePlanDescription throws a named argument-order error when arguments are swapped', () => {
  // The natural-reading (description, config) call used to surface as
  // `Cannot read properties of undefined (reading 'sections')` from deep inside planSections().
  assert.throws(
    () => validatePlanDescription(planDesc('- Contract: v1'), DEFAULT_CONFIG),
    (error) => {
      assert.ok(error instanceof Error)
      assert.match(error.message, /validatePlanDescription\(config, description\)/)
      assert.match(error.message, /arguments look swapped/)
      assert.doesNotMatch(error.message, /Cannot read properties/)
      return true
    },
  )
})

test('validatePlanDescription detects an asterisk-bullet Contract stamp', () => {
  // A tracker may renormalise `-` to `*` on save, which made the stamp undetectable on read-back.
  const r = validatePlanDescription(DEFAULT_CONFIG, planDesc('* Contract: v1'))
  assert.equal(r.version, 1)
  assert.equal(r.unsupportedVersion, false)
  assert.equal(r.ok, true)
})

test('validatePlanDescription reports an off-contract heading in `unknown` without flipping `ok`', () => {
  // CONSUMER COMPATIBILITY PIN: boss-build gates on `ok`. Folding `unknown` into `ok` here would
  // newly BLOCK every already-planned ticket carrying an extra heading. Producer-side strictness
  // belongs in the producer's guard, which reads `unknown` directly.
  const desc = planDesc('- Contract: v1').replace('## Planning', '## Notes\n\nx\n\n## Planning')
  const r = validatePlanDescription(DEFAULT_CONFIG, desc)
  assert.deepEqual(r.unknown, ['## Notes'])
  assert.deepEqual(r.missing, [])
  assert.equal(r.ok, true)
})

test('planDescriptionSections ignores a `##` line inside a fenced code block', () => {
  // A plan that quotes `make help` output or a markdown example carries `##` lines that are sample
  // TEXT, not emitted structure. Reading one as a section invented an off-contract heading, which
  // the producer-side gate then rejects with no remedy the drafter can act on.
  const desc = planDesc('- Contract: v1').replace(
    '## Key changes\n\nx',
    '## Key changes\n\n```\n## test: Run tests with race detector\n```',
  )
  const headings = planDescriptionSections(DEFAULT_CONFIG, desc).map((s) => s.heading)
  assert.ok(!headings.includes('## test: Run tests with race detector'))
  const r = validatePlanDescription(DEFAULT_CONFIG, desc)
  assert.deepEqual(r.unknown, [])
  assert.deepEqual(r.missing, [])
  assert.equal(r.ok, true)
  // A real heading outside the fence is still a section — skipping can only REMOVE spurious ones.
  assert.ok(headings.includes('## Key changes'))
})

test('validatePlanDescription treats a registered `optional` section as recognised, not unknown', () => {
  const desc = planDesc('- Contract: v1').replace(
    '## Planning',
    '## Proof harness analysis\n\nx\n\n## Planning',
  )
  const r = validatePlanDescription(DEFAULT_CONFIG, desc)
  assert.deepEqual(r.unknown, [])
  assert.equal(r.ok, true)
})

test('validatePlanDescription reports a missing required section', () => {
  const desc = '## Summary\n\nx\n\n## Planning\n\n- Contract: v1\n'
  const r = validatePlanDescription(DEFAULT_CONFIG, desc)
  assert.equal(r.ok, false)
  assert.ok(r.missing.includes('## Approach'))
})

test('validatePlanDescription flags an unsupported future version', () => {
  const r = validatePlanDescription(DEFAULT_CONFIG, planDesc('- Contract: v99'))
  assert.equal(r.unsupportedVersion, true)
  assert.equal(r.ok, false)
  assert.equal(r.version, 99)
})

test('validatePlanDescription treats a missing stamp as back-compat v1', () => {
  const r = validatePlanDescription(DEFAULT_CONFIG, planDesc('- Complexity: 3'))
  assert.equal(r.version, null)
  assert.equal(r.unsupportedVersion, false)
  assert.equal(r.ok, true)
})

test('validatePlanDescription ignores headings and stamps echoed in ## Original notes', () => {
  // A plan that omits its own ## Testing section, whose verbatim ## Original notes body happens to
  // echo a `## Testing` heading and a stray `- Contract: v99` from the ticket. Neither may satisfy
  // the contract: the section is still missing and the authoritative version is the ## Planning v1.
  const emitted = requiredPlanSections(DEFAULT_CONFIG)
    .filter((h) => h !== '## Testing')
    .join('\n\nx\n\n')
    .replace('## Planning\n\nx', '## Planning\n\n- Contract: v1')
  const desc = `${emitted}\n\n## Testing\n\n(echoed from the ticket)\n\n- Contract: v99\n`
  const r = validatePlanDescription(DEFAULT_CONFIG, desc)
  assert.deepEqual(r.missing, ['## Testing'])
  assert.equal(r.version, 1)
  assert.equal(r.unsupportedVersion, false)
  assert.equal(r.ok, false)
})

test('the committed .boss-skills.json planContract matches the default section set', () => {
  const cfg = loadSkillConfig({ cwd: REPO_ROOT })
  assert.equal(planContractVersion(cfg), planContractVersion(DEFAULT_CONFIG))
  assert.deepEqual(planSections(cfg), planSections(DEFAULT_CONFIG))
})

test('validateConfig rejects a malformed planContract override', () => {
  // planContract is the consumer-overridable extension point; a bad shape must surface as a
  // skill-config: error, not a raw TypeError deep in planSections()/validatePlanDescription().
  assert.throws(
    () =>
      validateConfig({ ...DEFAULT_CONFIG, planContract: { version: 1, sections: 'oops' } }, 't'),
    /skill-config:.*planContract\.sections must be a non-empty array/,
  )
  assert.throws(
    () => validateConfig({ ...DEFAULT_CONFIG, planContract: { sections: [] } }, 't'),
    /skill-config:.*planContract\.version must be a positive integer/,
  )
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, planContract: { version: 1, sections: [{ heading: '## X' }] } },
        't',
      ),
    /skill-config:.*required must be one of/,
  )
  // A typo in `required` must not silently drop the section from requiredPlanSections().
  assert.throws(
    () =>
      validateConfig(
        {
          ...DEFAULT_CONFIG,
          planContract: { version: 1, sections: [{ heading: '## X', required: 'alway' }] },
        },
        't',
      ),
    /skill-config:.*required must be one of/,
  )
})

// --- BOS-448: tracker/publish config resolution + configured-repo probe ---

// A synthetic populated config — deliberately NOT the Bossanova identity, so this file never
// duplicates the literals BOS-448 centralizes into .boss-skills.json.
function configuredFixture() {
  return mergeConfig(DEFAULT_CONFIG, {
    adapters: { tracker: 'demo', publish: 'store', sessionRunner: 'bossd' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'DemoTeam',
        teamKey: 'DEMO',
        workspace: 'demo-workspace',
        states: {
          unplanned: 'Backlog',
          planned: 'Ready',
          inProgress: 'Doing',
          inReview: 'Reviewing',
        },
        labels: {
          agentPlan: 'planning',
          agentFriendly: 'friendly',
          needsHuman: 'human-review',
          agentQuestion: 'question',
          bug: 'defect',
        },
        githubLabels: { proofInvalid: 'invalid-proof' },
      },
    },
    publishConfig: { store: { bucket: 'demo-bucket', baseUrl: 'https://demo.example.com' } },
  })
}

test('DEFAULT_CONFIG ships empty trackerConfig / publishConfig (no baked-in identity)', () => {
  assert.deepEqual(DEFAULT_CONFIG.trackerConfig, {})
  assert.deepEqual(DEFAULT_CONFIG.publishConfig, {})
})

test('isConfiguredForRepo: false for the bare defaults (unconfigured repo)', () => {
  assert.equal(isConfiguredForRepo(DEFAULT_CONFIG), false)
  assert.equal(trackerConfigFor(DEFAULT_CONFIG), null)
  assert.equal(publishConfigFor(DEFAULT_CONFIG), null)
})

test('isConfiguredForRepo: true when the selected tracker has an identity block', () => {
  const cfg = configuredFixture()
  assert.equal(isConfiguredForRepo(cfg), true)
})

test('isConfiguredForRepo: false when a trackerConfig block exists but not for the selected tracker', () => {
  // A config that names tracker "other" but only carries a block for "demo" is not configured.
  const cfg = mergeConfig(configuredFixture(), { adapters: { tracker: 'other' } })
  assert.equal(isConfiguredForRepo(cfg), false)
  assert.equal(trackerConfigFor(cfg), null)
})

test('isConfiguredForPlanning: true when the tracker carries the full state role map', () => {
  assert.equal(isConfiguredForPlanning(configuredFixture()), true)
})

test('isConfiguredForPlanning: false for the bare defaults (unconfigured repo)', () => {
  assert.equal(isConfiguredForPlanning(DEFAULT_CONFIG), false)
})

test('isConfiguredForPlanning: false when tracker identity is present but the states map is absent', () => {
  // A repo configured for a stateless core (identity only) passes isConfiguredForRepo but is not
  // planning-ready — boss-plan resolves states by role, so it must self-disable, not run with
  // undefined state names.
  const identityOnly = mergeConfig(DEFAULT_CONFIG, {
    adapters: { tracker: 'demo' },
    trackerConfig: { demo: { mcpServer: 'demo-tracker', team: 'DemoTeam' } },
  })
  assert.equal(isConfiguredForRepo(identityOnly), true)
  assert.equal(isConfiguredForPlanning(identityOnly), false)
})

test('isConfiguredForPlanning: false when a required state role is missing', () => {
  const missingInReview = mergeConfig(DEFAULT_CONFIG, {
    adapters: { tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'DemoTeam',
        states: { unplanned: 'Backlog', planned: 'Ready', inProgress: 'Doing' },
      },
    },
  })
  assert.equal(isConfiguredForRepo(missingInReview), true)
  assert.equal(isConfiguredForPlanning(missingInReview), false)
})

test('trackerConfigFor / publishConfigFor resolve the selected adapter, and accept an explicit one', () => {
  const cfg = configuredFixture()
  const tc = trackerConfigFor(cfg)
  assert.equal(tc.mcpServer, 'demo-tracker')
  assert.equal(tc.team, 'DemoTeam')
  assert.equal(tc.states.planned, 'Ready')
  assert.equal(publishConfigFor(cfg).baseUrl, 'https://demo.example.com')
  // explicit adapter override
  assert.equal(trackerConfigFor(cfg, 'demo').teamKey, 'DEMO')
  assert.equal(trackerConfigFor(cfg, 'missing'), null)
})

test('stateName, labelName, and githubLabelName resolve tracker roles', () => {
  const cfg = configuredFixture()
  assert.equal(stateName(cfg, 'planned'), 'Ready')
  assert.equal(labelName(cfg, 'agentFriendly'), 'friendly')
  assert.equal(githubLabelName(cfg, 'proofInvalid'), 'invalid-proof')
})

test('optionalLabelName resolves configured roles and returns null for absent label roles', () => {
  const cfg = configuredFixture()
  for (const [role, name] of Object.entries(trackerConfigFor(cfg).labels)) {
    assert.equal(optionalLabelName(cfg, role), name)
  }
  for (const role of ['docs', 'feature', 'improvement', 'bugfix', 'epic']) {
    assert.equal(optionalLabelName(cfg, role), null)
  }
})

test('stateName, labelName, and githubLabelName fail closed for missing roles', () => {
  const cfg = configuredFixture()
  assert.throws(() => stateName(cfg, 'done'), /skill-config:.*states\.done must be configured/)
  assert.throws(() => labelName(cfg, 'docs'), /skill-config:.*labels\.docs must be configured/)
  assert.throws(() => labelName(cfg, 'bugfix'), /skill-config:.*labels\.bugfix must be configured/)
  assert.throws(
    () => githubLabelName(cfg, 'release'),
    /skill-config:.*githubLabels\.release must be configured/,
  )
})

test('optionalLabelName still fails closed for malformed configured label roles', () => {
  const cfg = configuredFixture()
  const withEmpty = mergeConfig(cfg, { trackerConfig: { demo: { labels: { agentPlan: '' } } } })
  const withNonString = mergeConfig(cfg, { trackerConfig: { demo: { labels: { agentPlan: 7 } } } })
  assert.throws(
    () => optionalLabelName(withEmpty, 'agentPlan'),
    /skill-config:.*labels\.agentPlan must be configured as a non-empty string/,
  )
  assert.throws(
    () => optionalLabelName(withNonString, 'agentPlan'),
    /skill-config:.*labels\.agentPlan must be configured as a non-empty string/,
  )
})

test('scanUnmappedRoleClaims detects bounded tracker-role claim families', () => {
  const claims = scanUnmappedRoleClaims(
    [
      'The bug role is deliberately unmapped in this repo.',
      "Calling labelName(config, 'agentFriendly') throws here.",
      'The release role is unavailable for this tracker.',
      'The epic role was deliberately unmapped before BOS-792.',
      "Calling `stateName(config, 'planned')` throws in this example.",
    ].join('\n'),
  )
  assert.deepEqual(
    claims.map((claim) => [claim.role, claim.line]),
    [
      ['bug', 1],
      ['agentFriendly', 2],
      ['release', 3],
      ['epic', 4],
      ['planned', 5],
    ],
  )
  assert.match(claims[0].quote, /bug role is deliberately unmapped/)
})

test('scanUnmappedRoleClaims ignores ordinary role prose and explicit opt-outs', () => {
  assert.deepEqual(
    scanUnmappedRoleClaims(
      [
        'The bug role maps to the bug label in this repo.',
        'The foreign role is deliberately unmapped elsewhere.',
        '<!-- skill-config-claim: ignore -->',
      ].join('\n'),
    ),
    [],
  )
})

test('scanUnmappedRoleClaims detects line-wrapped multi-word claims', () => {
  const claims = scanUnmappedRoleClaims('The bug role is deliberately\nunmapped in this repo.')
  assert.equal(claims.length, 1)
  assert.equal(claims[0].role, 'bug')
  assert.equal(claims[0].line, 1)
  assert.match(claims[0].quote, /deliberately unmapped/)
})

test('scanUnmappedRoleClaims detects Markdown-wrapped role identifiers', () => {
  const claims = scanUnmappedRoleClaims(
    [
      'The `bug` role is deliberately unmapped here.',
      'The **epic** label role is unavailable.',
    ].join('\n'),
  )
  assert.deepEqual(
    claims.map((claim) => [claim.role, claim.field ?? null]),
    [
      ['bug', null],
      ['epic', 'labels'],
    ],
  )
})

test('scanUnmappedRoleClaims scopes ignore markers to the containing list item', () => {
  const claims = scanUnmappedRoleClaims(
    [
      '- The foreign role is deliberately unmapped. <!-- skill-config-claim: ignore -->',
      '- The bug role is deliberately unmapped.',
    ].join('\n'),
  )
  assert.equal(claims.length, 1)
  assert.equal(claims[0].role, 'bug')
  assert.match(claims[0].quote, /^- The bug role is deliberately unmapped\./)
})

test('the committed tracker config supplies every operational state and label role', () => {
  const cfg = loadSkillConfig({ cwd: REPO_ROOT })
  for (const role of [
    'backlog',
    'unplanned',
    'planned',
    'inProgress',
    'inReview',
    'done',
    'canceled',
    'duplicate',
  ]) {
    assert.ok(stateName(cfg, role).length > 0, `missing state role ${role}`)
  }
  // The five pipeline roles plus the one content-taxonomy role this repo maps. `labelName` has no
  // allowlist — it resolves whatever `trackerConfig.<tracker>.labels` supplies — so a taxonomy role
  // is resolvable exactly when a repo configures it, and unconfigured roles throw (see the
  // `bugfix` fail-closed case above). Nothing here is universal to the published core.
  for (const role of ['agentPlan', 'agentFriendly', 'needsHuman', 'agentQuestion', 'epic', 'bug']) {
    assert.ok(labelName(cfg, role).length > 0, `missing label role ${role}`)
  }
  assert.ok(githubLabelName(cfg, 'proofInvalid').length > 0)
})

test('BOS-458: adapters.tracker selects the config with TRACKER env unset (no baked-in linear)', () => {
  // Regression guard for the BOS-458 preflight bug: a repo that declares a non-linear tracker
  // ONLY in .boss-skills.json (adapters.tracker) — without exporting TRACKER — must resolve THAT
  // adapter's config, never fall back to a hard-coded `linear` key. trackerConfigFor and the
  // isConfiguredFor* probes key on adapters.tracker and never read process.env.TRACKER, so
  // config-selected resolution must hold even when the env var is absent.
  const savedTracker = process.env.TRACKER
  delete process.env.TRACKER
  try {
    assert.equal(process.env.TRACKER, undefined) // intent: env-unset → config-selected default

    // Positive: adapters.tracker "jira" + a full trackerConfig.jira block resolves the jira config.
    const jira = mergeConfig(DEFAULT_CONFIG, {
      adapters: { tracker: 'jira' },
      trackerConfig: {
        jira: {
          mcpServer: 'jira-tracker',
          team: 'JiraTeam',
          teamKey: 'JIRA',
          states: {
            unplanned: 'Backlog',
            planned: 'Ready',
            inProgress: 'In Progress',
            inReview: 'In Review',
          },
        },
      },
    })
    const tc = trackerConfigFor(jira)
    assert.equal(tc.mcpServer, 'jira-tracker')
    assert.equal(tc.states.planned, 'Ready')
    assert.equal(isConfiguredForRepo(jira), true)
    assert.equal(isConfiguredForPlanning(jira), true)

    // Negative twin: adapters.tracker pointed at an adapter with NO trackerConfig block resolves
    // null / false — proving resolution follows adapters.tracker, not a baked-in `linear` default.
    const unbacked = mergeConfig(jira, { adapters: { tracker: 'notconfigured' } })
    assert.equal(trackerConfigFor(unbacked), null)
    assert.equal(isConfiguredForRepo(unbacked), false)
  } finally {
    if (savedTracker === undefined) delete process.env.TRACKER
    else process.env.TRACKER = savedTracker
  }
})

test('validateConfig rejects a trackerConfig entry missing mcpServer or team', () => {
  assert.throws(
    () =>
      validateConfig(mergeConfig(DEFAULT_CONFIG, { trackerConfig: { demo: { team: 'T' } } }), 't'),
    /skill-config:.*trackerConfig\.demo\.mcpServer must be a non-empty string/,
  )
  assert.throws(
    () =>
      validateConfig(
        mergeConfig(DEFAULT_CONFIG, { trackerConfig: { demo: { mcpServer: 'x' } } }),
        't',
      ),
    /skill-config:.*trackerConfig\.demo\.team must be a non-empty string/,
  )
})

test('validateConfig rejects malformed states / publishConfig', () => {
  assert.throws(
    () =>
      validateConfig(
        mergeConfig(DEFAULT_CONFIG, {
          trackerConfig: { demo: { mcpServer: 'x', team: 'T', states: { planned: '' } } },
        }),
        't',
      ),
    /skill-config:.*trackerConfig\.demo\.states\.planned must be a non-empty string/,
  )
  assert.throws(
    () =>
      validateConfig(
        mergeConfig(DEFAULT_CONFIG, { publishConfig: { store: { bucket: 'b' } } }),
        't',
      ),
    /skill-config:.*publishConfig\.store\.baseUrl must be a non-empty string/,
  )
  assert.throws(
    () => validateConfig(mergeConfig(DEFAULT_CONFIG, { trackerConfig: [] }), 't'),
    /skill-config:.*trackerConfig must be an object/,
  )
  assert.throws(
    () => validateConfig(mergeConfig(DEFAULT_CONFIG, { publishConfig: { store: 7 } }), 't'),
    /skill-config:.*publishConfig\.store must be an object/,
  )
})

test('validateConfig rejects malformed tracker label maps', () => {
  const tracker = { mcpServer: 'x', team: 'T' }
  assert.throws(
    () =>
      validateConfig(
        mergeConfig(DEFAULT_CONFIG, { trackerConfig: { demo: { ...tracker, labels: [] } } }),
        't',
      ),
    /skill-config:.*trackerConfig\.demo\.labels must be an object when present/,
  )
  assert.throws(
    () =>
      validateConfig(
        mergeConfig(DEFAULT_CONFIG, {
          trackerConfig: { demo: { ...tracker, labels: { agentPlan: '' } } },
        }),
        't',
      ),
    /skill-config:.*trackerConfig\.demo\.labels\.agentPlan must be a non-empty string/,
  )
  assert.throws(
    () =>
      validateConfig(
        mergeConfig(DEFAULT_CONFIG, {
          trackerConfig: { demo: { ...tracker, githubLabels: { proofInvalid: 7 } } },
        }),
        't',
      ),
    /skill-config:.*trackerConfig\.demo\.githubLabels\.proofInvalid must be a non-empty string/,
  )
})

test('validateConfig accepts a minimal tracker block (mcpServer + team only)', () => {
  // teamKey / workspace / states are optional; a block with just the two load-bearing
  // fields must validate (and read as configured).
  const cfg = mergeConfig(DEFAULT_CONFIG, {
    adapters: { tracker: 'demo' },
    trackerConfig: { demo: { mcpServer: 'x', team: 'T' } },
  })
  validateConfig(cfg, 't') // does not throw
  assert.equal(isConfiguredForRepo(cfg), true)
})

test('a repo with no .boss-skills.json is unconfigured (probe is false)', () => {
  const dir = mkdtempSync(join(tmpdir(), 'skillcfg-unconfigured-'))
  try {
    const cfg = loadSkillConfig({ cwd: dir })
    assert.equal(isConfiguredForRepo(cfg), false)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// --- Task 5: committed .boss-skills.json parity ---------------------------

// The repo root is one level up from skills-toolbox/skill-config.test.mjs.
const REPO_ROOT = fileURLToPath(new URL('..', import.meta.url))
const SKILL_CLAIM_ROOTS = ['.claude/skills', 'services/boss/internal/skillinstall/skills']

function discoverSkillDocs(root) {
  const out = []
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const full = join(dir, entry.name)
      if (entry.isDirectory()) {
        walk(full)
        continue
      }
      if (entry.isFile() && entry.name === 'SKILL.md') out.push(full)
    }
  }
  if (existsSync(root)) walk(root)
  return out.sort()
}

function claimTrackerFields(claim) {
  if (claim.field) return [claim.field]
  if (/\blabelName\(/i.test(claim.quote) || /\blabel\s+role\b/i.test(claim.quote)) {
    return ['labels']
  }
  if (/\bstateName\(/i.test(claim.quote) || /\bstate\s+role\b/i.test(claim.quote)) {
    return ['states']
  }
  return ['states', 'labels', 'githubLabels']
}

function resolvedTrackerRole(config, claim) {
  const tracker = trackerConfigFor(config)
  for (const field of claimTrackerFields(claim)) {
    const value = tracker?.[field]?.[claim.role]
    if (typeof value === 'string' && value.length > 0) return { field, value }
  }
  return null
}

test('the committed .boss-skills.json reproduces the current hard-coded values', () => {
  const cfg = loadSkillConfig({ cwd: REPO_ROOT })
  // lens map parity with skills-toolbox/bs-review-detect.mjs + boss-review SKILL
  assert.deepEqual(
    cfg.lensMap.map((r) => [r.id, r.skill]),
    [
      ['go', 'golang-pro'],
      ['tui', 'tui-design'],
      ['web', 'impeccable'],
      ['db', 'database-review'],
      ['api', 'api-review'],
    ],
  )
  // BOS-850 inverts the old byte-identity pin. The committed lensMap USED to be required to
  // deep-equal DEFAULT_CONFIG.lensMap, which is exactly the coupling that kept this checkout's
  // path-anchored lenses inside the globally published defaults. The invariant is now the
  // opposite: this repo's config is path-anchored and DEFAULT_CONFIG must carry none of it.
  const defaultIds = new Set(DEFAULT_CONFIG.lensMap.map((r) => r.id))
  assert.equal(defaultIds.has('tui'), false, 'the repo-shaped tui lens must stay out of defaults')
  const defaultGlobs = new Set(
    DEFAULT_CONFIG.lensMap.flatMap((r) => (Array.isArray(r.globs) ? r.globs : [r.glob])),
  )
  for (const rule of cfg.lensMap) {
    const globs = Array.isArray(rule.globs) ? rule.globs : [rule.glob]
    for (const glob of globs) {
      assert.ok(typeof glob === 'string' && glob.length > 0, `lens "${rule.id}" needs a matcher`)
      if (!glob.startsWith('**/')) {
        assert.equal(
          defaultGlobs.has(glob),
          false,
          `path-anchored glob "${glob}" from this checkout leaked into DEFAULT_CONFIG`,
        )
      }
    }
    assert.ok(rule.fallbackRubric && rule.fallbackRubric.trim().length > 0)
  }
  // This checkout still anchors lenses to its own layout — the config seam doing its job.
  assert.ok(
    cfg.lensMap.some((r) =>
      (Array.isArray(r.globs) ? r.globs : [r.glob]).some((g) => g.startsWith('services/')),
    ),
    'the committed config is expected to keep its path-anchored lenses',
  )
  // manifest + commands parity with docs/testing/test-command-manifest.md
  assert.equal(cfg.test.manifestPath, 'docs/testing/test-command-manifest.md')
  assert.equal(cfg.commands.testSmoke, 'make test-smoke')
  assert.equal(cfg.commands.testAffected, 'make test-affected')
  assert.equal(moduleTestCommand(cfg, 'boss'), 'make test-boss')
  // env parity with boss-build + boss-plan headless detection
  assert.equal(isHeadless(cfg, { BOSS_CRON: 'true' }, { isTTY: true }), true)
  assert.equal(isHeadless(cfg, { BS_HEADLESS: '1' }, { isTTY: true }), true)
  // adapter selection parity
  assert.equal(adapterFor(cfg, 'tracker'), 'linear')
  assert.equal(adapterFor(cfg, 'publish'), 'proof')
  assert.equal(adapterFor(cfg, 'sessionRunner'), 'bossd')
  // BOS-448: this repo IS configured, and the identity resolves from config (asserted
  // structurally so the test does not re-duplicate the very literals the config centralizes).
  assert.equal(isConfiguredForRepo(cfg), true)
  const tc = trackerConfigFor(cfg)
  assert.ok(tc.mcpServer.length > 0 && tc.team.length > 0 && tc.teamKey.length > 0)
  assert.deepEqual(Object.keys(tc.states).sort(), [
    'backlog',
    'canceled',
    'done',
    'duplicate',
    'inProgress',
    'inReview',
    'planned',
    'unplanned',
  ])
  // Five pipeline roles plus the `bug` taxonomy role this repo maps. A consuming repo whose
  // tracker names that label differently remaps it here (`"bug": "defect"`); the seam is open,
  // so this pin records THIS repo's config, never a contract of the published core.
  assert.deepEqual(Object.keys(tc.labels).sort(), [
    'agentFriendly',
    'agentPlan',
    'agentQuestion',
    'bug',
    'epic',
    'needsHuman',
  ])
  assert.deepEqual(Object.keys(tc.githubLabels), ['proofInvalid'])
  assert.deepEqual(tc.followUpLabels, ['follow-up', 'agent-plan'])
  const pc = publishConfigFor(cfg)
  assert.ok(pc.bucket.length > 0)
  assert.match(pc.baseUrl, /^https:\/\//)
  assert.deepEqual(planStorageFor(cfg), { kind: 'tracker-attachment' })
})

test('skill prose does not claim resolved tracker roles are unmapped', () => {
  // This is a bounded claim-family gate, not full natural-language proof that no false config
  // statement exists. It compares the claims the scanner recognises against this repo's committed
  // tracker role maps and requires any rare legitimate exception to carry the explicit ignore
  // marker near the claim.
  const cfg = loadSkillConfig({ cwd: REPO_ROOT })
  const failures = []
  for (const root of SKILL_CLAIM_ROOTS) {
    for (const file of discoverSkillDocs(join(REPO_ROOT, root))) {
      const body = readFileSync(file, 'utf8')
      for (const claim of scanUnmappedRoleClaims(body)) {
        const resolved = resolvedTrackerRole(cfg, claim)
        if (!resolved) continue
        failures.push(
          `${file}:${claim.line}: role "${claim.role}" is resolved at trackerConfig.${adapterFor(cfg, 'tracker')}.${resolved.field}.${claim.role}="${resolved.value}" but prose claims it is unmapped: ${claim.quote}`,
        )
      }
    }
  }
  assert.deepEqual(failures, [])
})

test('typed helper claims are checked against the matching tracker role namespace', () => {
  const cfg = loadSkillConfig({ cwd: REPO_ROOT })
  const [stateClaim] = scanUnmappedRoleClaims("Calling stateName(config, 'bug') throws here.")
  const [labelClaim] = scanUnmappedRoleClaims("Calling labelName(config, 'bug') throws here.")
  const sameParagraph = scanUnmappedRoleClaims(
    "Calling labelName(config, 'release') throws and stateName(config, 'planned') throws here.",
  )

  assert.equal(resolvedTrackerRole(cfg, stateClaim), null)
  assert.deepEqual(resolvedTrackerRole(cfg, labelClaim), { field: 'labels', value: 'bug' })
  assert.equal(resolvedTrackerRole(cfg, sameParagraph[0]), null)
  assert.deepEqual(resolvedTrackerRole(cfg, sameParagraph[1]), {
    field: 'states',
    value: stateName(cfg, 'planned'),
  })
})

// --- Verify-only acceptance criteria (BOS-861) -----------------------------
//
// A criterion whose correct outcome is "this file needed no change" is invisible to every
// diff-shaped gate, so it either ships on a silent tick or blocks the run as required-deferred.
// The literal `(verify-only)` marker plus a named, re-runnable check replaces both outcomes with
// evidence. Each falsification below isolates ONE branch of the gate: a test that only exercises
// the green path is exactly the vacuous gate this contract exists to prevent.

/** A complete, contract-valid v1 description whose `## Acceptance criteria` body is `criteria`. */
const planBody = (criteria) =>
  requiredPlanSections(DEFAULT_CONFIG)
    .map((heading) => {
      if (heading === '## Acceptance criteria') return `${heading}\n\n${criteria}`
      if (heading === '## Planning') return `${heading}\n\n- Contract: v1`
      return `${heading}\n\nbody`
    })
    .join('\n\n')

const publicCriterion = ({ text, checked, verifyOnly, check, result }) => ({
  text,
  checked,
  verifyOnly,
  check,
  result,
})

test('the marker/clause literals are the exact bytes the plan and PR forms are written in', () => {
  // Producer prose, consumer prose and this parser must read ONE definition — a hand-typed copy in
  // three places is how the contract drifts. `plan-contract.test.mjs` asserts the prose carries
  // these same bytes; this pins the bytes themselves (em dash U+2014, arrow U+2192).
  assert.equal(VERIFY_ONLY_MARKER, '(verify-only)')
  assert.equal(VERIFY_ONLY_CHECK, ' — check: ')
  assert.equal(VERIFY_ONLY_CHECKED, ' — checked: ')
  assert.equal(VERIFY_ONLY_RESULT, ' → ')
  // ` — check: ` must never be a substring of ` — checked: `, or clause detection would be
  // order-dependent and a discharged criterion could parse as a merely-planned one.
  assert.ok(!VERIFY_ONLY_CHECKED.includes(VERIFY_ONLY_CHECK))
})

test('parseAcceptanceCriteria reads box state, marker, clause and wrapped lines', () => {
  const body = planBody(
    [
      '- [x] plain criterion the diff satisfies',
      `- [ ] ${VERIFY_ONLY_MARKER} the mirror still agrees${VERIFY_ONLY_CHECK}\`make vendor-toolbox-check\``,
      // A criterion whose clause lands on a CONTINUATION line — the common plan shape, and the one
      // a line-at-a-time parser silently drops.
      `- [x] ${VERIFY_ONLY_MARKER} no other call site needs the new argument`,
      `      ${VERIFY_ONLY_CHECKED.trimStart()}\`node --test skills-toolbox/skill-config.test.mjs\`${VERIFY_ONLY_RESULT}no matches outside tests`,
      // The marker classifies only as a PREFIX. A criterion that merely mentions the token in
      // prose is an ordinary diff-demonstrated criterion, and classifying it verify-only would
      // hand it the evidence route instead of requiring the change it actually asks for.
      `- [x] document why ${VERIFY_ONLY_MARKER} exists in the drafting brief`,
    ].join('\n'),
  )
  const criteria = parseAcceptanceCriteria(DEFAULT_CONFIG, body)
  assert.equal(criteria.length, 4)
  assert.equal(criteria[3].verifyOnly, false, 'the marker classifies as a prefix, not a substring')

  assert.deepEqual(publicCriterion(criteria[0]), {
    text: 'plain criterion the diff satisfies',
    checked: true,
    verifyOnly: false,
    check: null,
    result: null,
  })

  assert.equal(criteria[1].verifyOnly, true)
  assert.equal(criteria[1].checked, false)
  assert.equal(criteria[1].check, 'make vendor-toolbox-check')
  assert.equal(criteria[1].result, null, 'a planned criterion names no result yet')

  assert.equal(criteria[2].verifyOnly, true)
  assert.equal(criteria[2].checked, true)
  assert.equal(criteria[2].check, 'node --test skills-toolbox/skill-config.test.mjs')
  assert.equal(criteria[2].result, 'no matches outside tests')
})

test('parseAcceptanceCriteria ignores fenced samples and `## Original notes` echoes', () => {
  // The plan template itself is quoted in a fence, and the terminal `## Original notes` section
  // echoes the ticket verbatim. Reading either as a real criterion invents one nobody wrote —
  // the property `planDescriptionSections()` already owns, inherited rather than re-derived.
  const body = planBody(
    [
      '- [x] the one real criterion',
      '',
      '```markdown',
      `- [x] ${VERIFY_ONLY_MARKER} template sample with no evidence at all`,
      '```',
    ].join('\n'),
  ).concat(
    `\n\n## Original notes\n\n## Acceptance criteria\n\n- [x] ${VERIFY_ONLY_MARKER} echoed from the ticket\n`,
  )
  const criteria = parseAcceptanceCriteria(DEFAULT_CONFIG, body)
  assert.deepEqual(
    criteria.map((c) => c.text),
    ['the one real criterion'],
  )
  assert.equal(validateVerifyOnlyEvidence(DEFAULT_CONFIG, body).ok, true)
})

test('falsification 1: a ticked verify-only criterion with NO evidence clause fails', () => {
  const body = planBody(`- [x] ${VERIFY_ONLY_MARKER} the mirror still agrees`)
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
  assert.equal(result.ok, false)
  assert.deepEqual(
    result.missingEvidence.map((c) => c.text),
    [`${VERIFY_ONLY_MARKER} the mirror still agrees`],
  )
  assert.equal(result.verifyOnly.length, 1)
  assert.equal(result.missingEvidence[0].reason, 'no-clause')
  assert.match(result.missingEvidence[0].remedy, /checked/)
})

test('falsification 2a: a present-but-EMPTY command fails (present is not non-empty)', () => {
  // Isolates the command half of the conjunction: the result is non-empty, so deleting the
  // command check from the implementation turns this test green — which is the point.
  const body = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} the mirror still agrees${VERIFY_ONLY_CHECKED}\`\`${VERIFY_ONLY_RESULT}identical`,
  )
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
  assert.equal(result.ok, false)
  assert.equal(result.missingEvidence.length, 1)
  assert.equal(result.missingEvidence[0].check, '')
  assert.equal(result.missingEvidence[0].result, 'identical')
  assert.equal(result.missingEvidence[0].reason, 'empty-command')
})

test('falsification 2b: a present-but-EMPTY result fails', () => {
  // Isolates the result half: the command is non-empty, so deleting the result check turns this
  // test green. 2a and 2b together mean neither conjunct can be dropped unnoticed.
  const body = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} the mirror still agrees${VERIFY_ONLY_CHECKED}\`make vendor-toolbox-check\`${VERIFY_ONLY_RESULT}`,
  )
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
  assert.equal(result.ok, false)
  assert.equal(result.missingEvidence.length, 1)
  assert.equal(result.missingEvidence[0].check, 'make vendor-toolbox-check')
  assert.equal(result.missingEvidence[0].result, '')
  assert.equal(result.missingEvidence[0].reason, 'empty-result')
})

test('falsification 3: an UNTICKED verify-only criterion is reported, never a failure', () => {
  // An open criterion is already a required-deferred item under the consumer's existing rule.
  // Double-reporting it here would make this gate cry wolf on a condition another gate owns.
  const body = planBody(
    `- [ ] ${VERIFY_ONLY_MARKER} the mirror still agrees${VERIFY_ONLY_CHECK}\`make vendor-toolbox-check\``,
  )
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
  assert.equal(result.ok, true)
  assert.deepEqual(result.missingEvidence, [])
  assert.equal(result.verifyOnly.length, 1)
  assert.equal(result.verifyOnly[0].checked, false)
})

test('falsification 4: a pre-existing v1 description with no marker is unaffected', () => {
  // Back-compat for every already-planned ticket: no marker means nothing to gate, and the
  // existing plan-description contract must be untouched by the addition.
  const body = planBody(
    ['- [ ] add the parser', '- [x] wire the consumer', '- [ ] update the mirrors'].join('\n'),
  )
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
  assert.deepEqual(result.verifyOnly, [])
  assert.equal(result.ok, true)
  assert.equal(validatePlanDescription(DEFAULT_CONFIG, body).ok, true)
  assert.equal(planContractVersion(DEFAULT_CONFIG), 1, 'the addition must not bump the contract')
})

test('a fully discharged verify-only criterion passes, and a missing section is not a failure', () => {
  const body = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} no other call site needs the new argument${VERIFY_ONLY_CHECKED}\`node --test skills-toolbox/skill-config.test.mjs\`${VERIFY_ONLY_RESULT}0 hits outside tests`,
  )
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
  assert.equal(result.ok, true)
  assert.deepEqual(result.missingEvidence, [])
  assert.equal(result.verifyOnly[0].check, 'node --test skills-toolbox/skill-config.test.mjs')
  assert.equal(result.verifyOnly[0].result, '0 hits outside tests')
  // A body with no `## Acceptance criteria` section at all returns data, never throws — the
  // "reject rather than degrade" habit applies to the CONFIG, not to a caller's arbitrary body.
  assert.deepEqual(parseAcceptanceCriteria(DEFAULT_CONFIG, '## Summary\n\nnothing here'), [])
  assert.equal(validateVerifyOnlyEvidence(DEFAULT_CONFIG, '').ok, true)
})

test('falsification 5: an arrow INSIDE the command is not the result separator', () => {
  // The bypass this gate could not survive: `— checked: `echo a→b`` names a command and NO result
  // clause, but splitting at the first arrow yields a non-empty command AND a non-empty result, so
  // the criterion passed — the exact "missing result" case falsification 1/2b reject, reintroduced
  // by the separator search. Any real command carrying an arrow (`rg "a→b"`) triggers it by
  // accident. The control below is the SAME shape without an arrow, and it must fail identically.
  const bypass = planBody(`- [x] ${VERIFY_ONLY_MARKER} inv${VERIFY_ONLY_CHECKED}\`echo a→b\``)
  const control = planBody(`- [x] ${VERIFY_ONLY_MARKER} inv${VERIFY_ONLY_CHECKED}\`echo ab\``)
  for (const [label, body] of [
    ['arrow inside the command', bypass],
    ['no arrow at all', control],
  ]) {
    const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
    assert.equal(result.ok, false, `${label}: a missing result clause must fail`)
    assert.equal(result.missingEvidence.length, 1, label)
    assert.equal(result.missingEvidence[0].result, null, `${label}: no result clause was written`)
    assert.equal(result.missingEvidence[0].reason, 'empty-result', label)
  }
  // The command itself survives intact — the gate's whole product is a command a human can paste.
  assert.equal(parseAcceptanceCriteria(DEFAULT_CONFIG, bypass)[0].check, 'echo a→b')
  // And a genuine result after an arrow-bearing command still parses on both sides.
  const discharged = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} inv${VERIFY_ONLY_CHECKED}\`node -e "console.log('a→b')"\`${VERIFY_ONLY_RESULT}0 hits`,
  )
  const parsed = parseAcceptanceCriteria(DEFAULT_CONFIG, discharged)[0]
  assert.equal(parsed.check, `node -e "console.log('a→b')"`)
  assert.equal(parsed.result, '0 hits')
  assert.equal(validateVerifyOnlyEvidence(DEFAULT_CONFIG, discharged).ok, true)
})

test('falsification 5b: an UNDELIMITED discharge command is refused, not guessed at', () => {
  // The other half of falsification 5, and the half a backtick-only fix leaves live. Undelimited,
  // `rg "a→b" src/` (one command, NO result) and `make check → identical` (command THEN result) are
  // the same shape — one arrow in undelimited text — so any split rule gets one of them wrong.
  // Guessing chose the bypass: the first row passed carrying no result, and `check` came out as
  // `rg "a` either way, which is not a command a reviewer can paste. Refusing is fail-closed.
  const bypass = planBody(`- [x] ${VERIFY_ONLY_MARKER} inv${VERIFY_ONLY_CHECKED}rg "a→b" src/`)
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, bypass)
  assert.equal(result.ok, false, 'an undelimited command must never pass as evidence')
  assert.equal(result.missingEvidence.length, 1)
  assert.equal(
    result.missingEvidence[0].check,
    null,
    'an undecidable command is reported absent, never guessed at',
  )
  assert.equal(result.missingEvidence[0].reason, 'undelimited-command')
  // Even with a real result written, the undelimited form is refused rather than half-parsed.
  const undelimited = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} inv${VERIFY_ONLY_CHECKED}make check${VERIFY_ONLY_RESULT}identical`,
  )
  assert.equal(validateVerifyOnlyEvidence(DEFAULT_CONFIG, undelimited).ok, false)
  // ...and the delimited form of that same evidence passes, so the fix is one keystroke.
  const delimited = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} inv${VERIFY_ONLY_CHECKED}\`make check\`${VERIFY_ONLY_RESULT}identical`,
  )
  assert.equal(validateVerifyOnlyEvidence(DEFAULT_CONFIG, delimited).ok, true)
  assert.equal(parseAcceptanceCriteria(DEFAULT_CONFIG, delimited)[0].check, 'make check')
})

test('falsification 6: evidence written as a nested sub-bullet is not dropped', () => {
  // The mirror-image failure: evidence that IS present, reported missing, blocking a run that did
  // everything right. A long command naturally lands on its own indented bullet, and ending the
  // criterion at ANY list item silently discarded it. An UNINDENTED sibling still ends it.
  const nested = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} inv\n  - ${VERIFY_ONLY_CHECKED.trimStart()}\`make vendor-toolbox-check\`${VERIFY_ONLY_RESULT}identical`,
  )
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, nested)
  assert.equal(result.ok, true, 'evidence on a nested bullet is still evidence')
  assert.equal(result.verifyOnly[0].check, 'make vendor-toolbox-check')
  assert.equal(result.verifyOnly[0].result, 'identical')
  // An unindented sibling bullet remains a separate criterion, not a continuation.
  const siblings = parseAcceptanceCriteria(DEFAULT_CONFIG, planBody(`- [x] first\n- [ ] second`))
  assert.deepEqual(
    siblings.map((c) => c.text),
    ['first', 'second'],
  )
  // An INDENTED CHECKBOX sub-bullet is the same continuation, not a criterion of its own. Testing
  // the checkbox pattern before the indent let it steal the evidence into a phantom criterion and
  // leave the real one clauseless — finding 2's false negative wearing a checkbox.
  const nestedBox = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} inv\n  - [x] ${VERIFY_ONLY_CHECKED.trimStart()}\`make x\`${VERIFY_ONLY_RESULT}ok`,
  )
  assert.equal(
    parseAcceptanceCriteria(DEFAULT_CONFIG, nestedBox).length,
    1,
    'an indented checkbox is a continuation, not a new criterion',
  )
  assert.equal(validateVerifyOnlyEvidence(DEFAULT_CONFIG, nestedBox).ok, true)
})

test('falsification 7: an emphasised marker is still a verify-only marker', () => {
  // `**(verify-only)**` is the likeliest drafting slip — the brief bolds the token in its own prose
  // — and an unrecognised marker silently routes the criterion back to the diff-demonstrated rule,
  // which is the silence this whole contract exists to end, with no detector anywhere.
  for (const marker of [
    `**${VERIFY_ONLY_MARKER}**`,
    `*${VERIFY_ONLY_MARKER}*`,
    `__${VERIFY_ONLY_MARKER}__`,
  ]) {
    const body = planBody(`- [x] ${marker} inv`)
    const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
    assert.equal(result.verifyOnly.length, 1, `${marker} must classify as verify-only`)
    assert.equal(result.ok, false, `${marker} is ticked with no evidence, so it must fail`)
  }
  // Still a PREFIX rule: the marker merely mentioned mid-prose is not a verify-only criterion.
  assert.equal(
    parseAcceptanceCriteria(
      DEFAULT_CONFIG,
      planBody(`- [x] document why ${VERIFY_ONLY_MARKER} exists`),
    )[0].verifyOnly,
    false,
  )
})

test('a mis-tensed discharged verify-only criterion reports a named reason and no garbled check', () => {
  const body = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} inv${VERIFY_ONLY_CHECK}\`git diff -- a.md\`${VERIFY_ONLY_RESULT}empty`,
  )
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
  assert.equal(result.ok, false)
  assert.equal(result.missingEvidence.length, 1)
  assert.equal(result.missingEvidence[0].reason, 'planned-tense-on-ticked')
  assert.equal(result.missingEvidence[0].check, null)
  assert.match(result.missingEvidence[0].remedy, /checked/)
})

test('a mid-line verify-only marker is surfaced without reclassifying the criterion', () => {
  const body = planBody(
    `- [x] document why ${VERIFY_ONLY_MARKER} exists${VERIFY_ONLY_CHECKED}\`true\`${VERIFY_ONLY_RESULT}ok`,
  )
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
  assert.equal(result.ok, true)
  assert.deepEqual(result.verifyOnly, [])
  assert.equal(result.malformedMarker.length, 1)
  assert.equal(result.malformedMarker[0].verifyOnly, false)
})

test('an unresolvable verify-only command blocks with command-unresolvable', () => {
  const body = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} inv${VERIFY_ONLY_CHECKED}\`this-command-does-not-exist-bos1015 --nope\`${VERIFY_ONLY_RESULT}exit 0`,
  )
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
  assert.equal(result.ok, false)
  assert.equal(result.missingEvidence.length, 1)
  assert.equal(result.missingEvidence[0].reason, 'command-unresolvable')
})

test('classifyCheckCommand reports blocking and advisory shapes without executing commands', () => {
  assert.deepEqual(classifyCheckCommand('this-command-does-not-exist-bos1015').blocking, [
    {
      code: 'command-unresolvable',
      message:
        'the command head resolves to no executable PATH binary or executable repo-relative script',
    },
  ])
  assert.deepEqual(classifyCheckCommand('make test-scripts').blocking, [])
  assert.equal(
    classifyCheckCommand('make test', { env: { PATH: '' } }).blocking[0].code,
    'command-unresolvable',
  )
  assert.equal(
    classifyCheckCommand('test-scripts', { env: { PATH: '' } }).blocking[0].code,
    'command-unresolvable',
  )
  assert.deepEqual(
    classifyCheckCommand('node --test skills-toolbox/skill-config.test.mjs').blocking,
    [],
  )
  assert.deepEqual(
    classifyCheckCommand('NODE_OPTIONS=--test-reporter=tap make test-scripts').blocking,
    [],
  )
  assert.deepEqual(classifyCheckCommand('env -u BOSS_NO_BAZEL make test').blocking, [])
  assert.deepEqual(classifyCheckCommand('set -o pipefail; make test | tee out.log').blocking, [])
  assert.equal(
    classifyCheckCommand('true; this-command-does-not-exist-bos1015').blocking[0].code,
    'command-unresolvable',
  )
  assert.equal(
    classifyCheckCommand('make test | this-command-does-not-exist-bos1015').blocking[0].code,
    'command-unresolvable',
  )
  assert.equal(
    classifyCheckCommand('true && this-command-does-not-exist-bos1015').blocking[0].code,
    'command-unresolvable',
  )

  const cases = [
    ['git diff -- skills-toolbox/skill-config.mjs', 'working-tree-scoped-git-check'],
    ['go test ./... -run TestNoMatch', 'zero-selection-filter'],
    ['node --include=*.md', 'unquoted-option-glob'],
    ['node --include *.md # pass 2', 'unquoted-option-glob'],
    ['make test | tee out.log', 'pipe-without-pipefail'],
    ['make test|tee out.log', 'pipe-without-pipefail'],
    ['git grep -E "\\bneedle\\b"', 'git-grep-word-boundary'],
    ['bazel test //services/boss/...', 'cached-bazel-test'],
  ]
  for (const [command, code] of cases) {
    const result = classifyCheckCommand(command)
    assert.equal(result.blocking.length, 0, command)
    assert.ok(
      result.advisory.some((finding) => finding.code === code),
      `${command} should report ${code}`,
    )
  }
  assert.deepEqual(
    classifyCheckCommand('git diff HEAD -- skills-toolbox/skill-config.mjs').advisory,
    [],
  )
  assert.deepEqual(classifyCheckCommand("node --include='*.md' # pass 2").advisory, [])
  assert.deepEqual(classifyCheckCommand('node --include "*.md" # pass 2').advisory, [])
})

test('classifyCheckCommand requires executable files for PATH and repo-relative commands', () => {
  const tmp = mkdtempSync(join(tmpdir(), 'boss-skill-config-command-'))
  try {
    const binDir = join(tmp, 'bin')
    mkdirSync(binDir)
    const nonExecutable = join(binDir, 'not-executable')
    writeFileSync(nonExecutable, '#!/bin/sh\n')
    assert.deepEqual(
      classifyCheckCommand('not-executable --version', {
        cwd: tmp,
        env: { PATH: binDir },
      }).blocking,
      [
        {
          code: 'command-unresolvable',
          message:
            'the command head resolves to no executable PATH binary or executable repo-relative script',
        },
      ],
    )

    chmodSync(nonExecutable, 0o755)
    assert.deepEqual(
      classifyCheckCommand('not-executable --version', {
        cwd: tmp,
        env: { PATH: binDir },
      }).blocking,
      [],
    )

    const script = join(tmp, 'script.sh')
    writeFileSync(script, '#!/bin/sh\n')
    assert.equal(
      classifyCheckCommand('./script.sh', { cwd: tmp, env: { PATH: '' } }).blocking[0].code,
      'command-unresolvable',
    )
    chmodSync(script, 0o755)
    assert.deepEqual(
      classifyCheckCommand('./script.sh', { cwd: tmp, env: { PATH: '' } }).blocking,
      [],
    )
    assert.deepEqual(
      classifyCheckCommand('/bin/echo ok', { cwd: tmp, env: { PATH: '' } }).blocking,
      [],
    )
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

test('advisory verify-only command findings do not make evidence fail', () => {
  const body = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} inv${VERIFY_ONLY_CHECKED}\`git diff -- skills-toolbox/skill-config.mjs\`${VERIFY_ONLY_RESULT}empty`,
  )
  const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
  assert.equal(result.ok, true)
  assert.deepEqual(result.missingEvidence, [])
  assert.equal(result.advisory.length, 1)
  assert.equal(result.advisory[0].code, 'working-tree-scoped-git-check')
})

test('classifyCheckCommand is a static classifier, not a process runner', async () => {
  const source = await import('node:fs').then((fs) =>
    fs.readFileSync(new URL('./skill-config.mjs', import.meta.url), 'utf8'),
  )
  assert.doesNotMatch(source, /node:child_process|execSync|spawn\(/)
})

test('both new exports reject a swapped (description, config) call by name', () => {
  assert.throws(
    () => parseAcceptanceCriteria('## Acceptance criteria', DEFAULT_CONFIG),
    /parseAcceptanceCriteria\(config, description\)/,
  )
  assert.throws(
    () => validateVerifyOnlyEvidence('## Acceptance criteria', DEFAULT_CONFIG),
    /validateVerifyOnlyEvidence\(config, description\)/,
  )
})

test('parsePremises reads scoped premise bullets and central markers', () => {
  const body = planBody('- [x] ordinary criterion').replace(
    '## Acceptance criteria',
    [
      '## Premises',
      '',
      '- [ ] (central) the generated plan must still be checked — check: `make test-scripts`',
      '- [ ] supporting premise wraps',
      '  across lines — check: `rg -n parsePremises skills-toolbox`',
      '',
      '```md',
      '- [ ] (central) fenced sample',
      '```',
      '',
      '## Acceptance criteria',
    ].join('\n'),
  )
  const premises = parsePremises(DEFAULT_CONFIG, body)
  assert.equal(premises.length, 2)
  assert.deepEqual(premises[0], {
    text: '(central) the generated plan must still be checked — check: `make test-scripts`',
    claim: 'the generated plan must still be checked',
    check: 'make test-scripts',
    central: true,
    duplicateCentral: false,
  })
  assert.equal(premises[1].claim, 'supporting premise wraps across lines')
  assert.equal(premises[1].check, 'rg -n parsePremises skills-toolbox')
})

test('parsePremises reports two central premises instead of choosing one', () => {
  const body = planBody('- [x] ordinary criterion').replace(
    '## Acceptance criteria',
    [
      '## Premises',
      '',
      '- [ ] (central) first',
      '- [ ] **(central)** second',
      '',
      '## Acceptance criteria',
    ].join('\n'),
  )
  const central = parsePremises(DEFAULT_CONFIG, body).filter((premise) => premise.central)
  assert.equal(central.length, 2)
  assert.deepEqual(
    central.map((premise) => premise.claim),
    ['first', 'second'],
  )
  assert.deepEqual(
    central.map((premise) => premise.duplicateCentral),
    [true, true],
  )
})

test('parsePremises throws the named swapped-argument error', () => {
  assert.throws(
    () => parsePremises('## Premises', DEFAULT_CONFIG),
    /parsePremises\(config, description\)/,
  )
})
