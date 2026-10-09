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
  verifyAlwaysHumanPaths,
  DEFAULT_TRACKER_STATES,
  DEFAULT_PIPELINE_LABELS,
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
  epicChildWallClockMinutes,
  EPIC_DEFAULT_CHILD_WALL_CLOCK_MINUTES,
  EPIC_MAX_CHILD_WALL_CLOCK_MINUTES,
  notesSampleRate,
  NOTES_DEFAULT_SAMPLE_RATE,
  reviewLedgerConfig,
  planDependencyDefaults,
  command,
  fixRoundGates,
  moduleTestCommand,
  manifestPath,
  markdownH2Heading,
  isHeadless,
  adapterFor,
  trackerConfigFor,
  selectionBlockFor,
  stageSelectionQuery,
  toleratedDescriptionTransforms,
  DESCRIPTION_NORMALIZATION_TRANSFORMS,
  unattributedDriftSeverity,
  publishConfigFor,
  planStorageFor,
  stateName,
  labelName,
  optionalLabelName,
  CONTENT_LABEL_ROLES,
  contentLabelNames,
  githubLabelName,
  isConfiguredForRepo,
  isConfiguredForPlanning,
  scanUnmappedRoleClaims,
  planContractVersion,
  planSections,
  planSectionsForDescriptionMode,
  planDescriptionSections,
  keyChangesHeading,
  keyChangesSection,
  descriptionAppearsTruncated,
  requiredPlanSections,
  requiredSectionsForDescriptionMode,
  validatePlanDescription,
  mergedListItems,
  parseAcceptanceCriteria,
  parsePremises,
  validateVerifyOnlyEvidence,
  validateLocalVerification,
  classifyCheckCommand,
  COMMAND_BLOCKING_CODES,
  hasCountAssertion,
  commandFindingRemedy,
  tokenizeSimpleShell,
  VERIFY_ONLY_MARKER,
  VERIFY_ONLY_CHECK,
  VERIFY_ONLY_CHECKED,
  VERIFY_ONLY_RESULT,
  withTrackerDefaults,
  resolveTrackerTeam,
} from './skill-config.mjs'
import { sections } from './merge-eligibility.mjs'

test('keyChangesSection preserves wrapped paths and ignores fenced or original-note headings', () => {
  const body = '\n- `app/api/file.mjs`: keep\n  wrapped wording.\n\n'
  assert.equal(
    keyChangesSection(DEFAULT_CONFIG, '## Key changes\n' + body + '## Testing\nnext'),
    body.slice(0, -1),
  )
  assert.equal(
    keyChangesSection(
      DEFAULT_CONFIG,
      '```md\n## Key changes\nwrong\n```\n## Original notes\n## Key changes\nwrong',
    ),
    null,
  )
  assert.equal(keyChangesSection(DEFAULT_CONFIG, '## Summary\nnone'), null)
  assert.equal(keyChangesHeading(DEFAULT_CONFIG, '## Files'), '## Files')
  const custom = mergeConfig(DEFAULT_CONFIG, {
    planContract: { sections: [{ heading: '## KEY CHANGES' }, { heading: '## Original notes' }] },
  })
  assert.equal(keyChangesHeading(custom), '## KEY CHANGES')
  assert.equal(keyChangesSection(custom, '## KEY CHANGES\nexact\n## Original notes\nrest'), 'exact')
  assert.equal(descriptionAppearsTruncated('(truncated, use get_issue for full description)'), true)
  assert.equal(descriptionAppearsTruncated('complete'), false)
})

test('keyChanges helpers normalize case and reject config-first argument swaps', () => {
  assert.equal(keyChangesSection(DEFAULT_CONFIG, '## Key Changes\nbody\n## Testing\nrest'), 'body')
  const custom = mergeConfig(DEFAULT_CONFIG, {
    planContract: { sections: [{ heading: '## KEY CHANGES' }, { heading: '## Original notes' }] },
  })
  assert.equal(keyChangesSection(custom, '## Key changes\nbody'), 'body')
  assert.throws(() => keyChangesHeading('description', DEFAULT_CONFIG), /keyChangesHeading/)
  assert.throws(() => keyChangesSection('description', DEFAULT_CONFIG), /keyChangesSection/)
})

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

test('BOS-1337: DEFAULT_CONFIG ships an empty planDependencies block', () => {
  assert.deepEqual(DEFAULT_CONFIG.planDependencies, {
    moduleRoots: [],
    repoWideTokens: [],
    areaAliases: {},
  })
  assert.doesNotThrow(() => validateConfig({ ...DEFAULT_CONFIG }, 'test'))
})

test('BOS-1337: planDependencyDefaults fills defaults and returns copies', () => {
  const empty = { moduleRoots: [], repoWideTokens: [], areaAliases: {} }
  assert.deepEqual(planDependencyDefaults(DEFAULT_CONFIG), empty)
  assert.deepEqual(planDependencyDefaults({}), empty, 'an unconfigured repo resolves empty')
  assert.deepEqual(planDependencyDefaults(undefined), empty)
  const config = {
    planDependencies: { repoWideTokens: ['docs/index.md'], areaAliases: { 'a/x.go': ['b/x.go'] } },
  }
  const resolved = planDependencyDefaults(config)
  assert.deepEqual(resolved, {
    moduleRoots: [],
    repoWideTokens: ['docs/index.md'],
    areaAliases: { 'a/x.go': ['b/x.go'] },
  })
  resolved.repoWideTokens.push('mutated')
  resolved.areaAliases['a/x.go'].push('mutated')
  assert.deepEqual(config.planDependencies.repoWideTokens, ['docs/index.md'])
  assert.deepEqual(config.planDependencies.areaAliases['a/x.go'], ['b/x.go'])
})

test('BOS-1337: a repo planDependencies block merges over the defaults on load', () => {
  const { nested, cleanup } = scratchRepo(
    JSON.stringify({ planDependencies: { repoWideTokens: ['docs/index.md'] } }),
  )
  try {
    assert.deepEqual(planDependencyDefaults(loadSkillConfig({ cwd: nested })), {
      moduleRoots: [],
      repoWideTokens: ['docs/index.md'],
      areaAliases: {},
    })
  } finally {
    cleanup()
  }
})

test('BOS-1337: validateConfig rejects each malformed planDependencies shape', () => {
  const reject = (planDependencies, pattern) =>
    assert.throws(
      () => validateConfig({ ...DEFAULT_CONFIG, planDependencies }, 'test'),
      pattern,
      JSON.stringify(planDependencies),
    )
  reject(null, /planDependencies must be an object/)
  reject([], /planDependencies must be an object/)
  reject('docs', /planDependencies must be an object/)
  reject({ repoWideToken: [] }, /planDependencies\.repoWideToken is not a known key/)
  reject({ moduleRoots: 'services' }, /planDependencies\.moduleRoots must be an array/)
  reject({ repoWideTokens: [3] }, /planDependencies\.repoWideTokens entries must be non-empty/)
  reject({ repoWideTokens: [''] }, /planDependencies\.repoWideTokens entries must be non-empty/)
  reject({ areaAliases: [] }, /planDependencies\.areaAliases must be an object/)
  reject({ areaAliases: { 'a/x.go': 3 } }, /areaAliases\.a\/x\.go must map/)
  reject({ areaAliases: { 'a/x.go': ['b/x.go', ''] } }, /areaAliases\.a\/x\.go must map/)
  reject({ areaAliases: { 'a/x.go': '' } }, /areaAliases\.a\/x\.go must map/)
  // Control: every well-formed shape, including a partial block and both alias forms, passes.
  for (const planDependencies of [
    undefined,
    {},
    { moduleRoots: ['services'] },
    { areaAliases: { 'a/x.go': 'b/x.go', 'a/y.go': ['b/y.go', 'c/y.go'] } },
  ]) {
    assert.doesNotThrow(() => validateConfig({ ...DEFAULT_CONFIG, planDependencies }, 'test'))
  }
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

test('notesSampleRate defaults to 1.0 so an unconfigured repo keeps taking notes', () => {
  // The knob exists to let an operator PAY LESS for post-terminal reporting; it must never
  // silently take reporting away from a repo that never opted down. A config predating the
  // block, one that merged it away, and a bare `{}` all resolve to the full rate.
  assert.equal(NOTES_DEFAULT_SAMPLE_RATE, 1)
  assert.equal(notesSampleRate(DEFAULT_CONFIG), 1)
  assert.equal(notesSampleRate({}), 1)
  assert.equal(notesSampleRate({ notesDefaults: {} }), 1)
  assert.equal(notesSampleRate(undefined), 1)
  validateConfig({ ...DEFAULT_CONFIG, notesDefaults: undefined }, 'test')
})

test('notesSampleRate accepts every in-range rate including both endpoints', () => {
  for (const sampleRate of [0, 0.33, 0.5, 1]) {
    const cfg = mergeConfig(DEFAULT_CONFIG, { notesDefaults: { sampleRate } })
    validateConfig(cfg, 'test')
    assert.equal(notesSampleRate(cfg), sampleRate)
  }
})

test('validateConfig warns and coerces an out-of-range or wrong-type notesDefaults.sampleRate', () => {
  // Same degradation style as the reviewDefaults scalars: a malformed tuning knob warns and
  // falls back to the documented default rather than failing the run. Falling back to 1.0 is
  // the fail-SAFE direction — a typo costs extra dispatches, never lost notes.
  for (const sampleRate of [-0.1, 1.1, 2, '0.33', null, true, NaN, Infinity]) {
    const cfg = mergeConfig(DEFAULT_CONFIG, { notesDefaults: { sampleRate } })
    const originalWarn = console.warn
    const warnings = []
    console.warn = (message) => warnings.push(String(message))
    try {
      validateConfig(cfg, 'test')
    } finally {
      console.warn = originalWarn
    }
    assert.equal(cfg.notesDefaults.sampleRate, 1)
    assert.equal(notesSampleRate(cfg), 1)
    assert.match(warnings.join('\n'), /notesDefaults\.sampleRate/)
    assert.match(warnings.join('\n'), /using 1/)
  }
})

test('validateConfig rejects a non-object notesDefaults', () => {
  // A present-but-unusable BLOCK is a config error, not a tuning typo: every accessor below it
  // would dereference a non-object. This is the reviewDefaults split — reject the block, coerce
  // the scalar.
  for (const notesDefaults of [[], 'sampled', 7, null]) {
    assert.throws(
      () => validateConfig({ ...DEFAULT_CONFIG, notesDefaults }, 'test'),
      /skill-config:.*notesDefaults must be an object when present/,
    )
  }
})

test('notesSampleRate ignores a malformed rate that never went through validateConfig', () => {
  // Accessors are called on configs the core loaded, but also on hand-built ones in tests and
  // in a caller that merged its own object. The accessor must be self-defending, exactly like
  // reviewMaxDispatchedRoundDefault.
  for (const sampleRate of ['0.5', -1, 2, null, undefined, NaN]) {
    assert.equal(notesSampleRate({ notesDefaults: { sampleRate } }), 1)
  }
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

test('DEFAULT_CONFIG documents a 360-minute child wall clock and the reader returns it', () => {
  // 360, not 90: children on a repo of this class measure 2-4 h, so a short clock expires on
  // healthy work. Pin the number here so a silent reduction has to change a test.
  assert.equal(EPIC_DEFAULT_CHILD_WALL_CLOCK_MINUTES, 360)
  assert.equal(EPIC_MAX_CHILD_WALL_CLOCK_MINUTES, 1440)
  assert.equal(DEFAULT_CONFIG.epicDefaults.childWallClockMinutes, 360)
  assert.equal(epicChildWallClockMinutes(DEFAULT_CONFIG), 360)
  validateConfig(DEFAULT_CONFIG, 'test')
})

test('epicChildWallClockMinutes falls back to the default for a config carrying no block', () => {
  // A config predating the block, or one that merged it away, must still resolve a usable clock —
  // `undefined` would make every `elapsed > clock` comparison false, i.e. a child that never expires.
  assert.equal(epicChildWallClockMinutes({}), 360)
  assert.equal(epicChildWallClockMinutes({ epicDefaults: {} }), 360)
  assert.equal(epicChildWallClockMinutes(undefined), 360)
  validateConfig({ ...DEFAULT_CONFIG, epicDefaults: undefined }, 'test')
})

test('mergeConfig honors an epicDefaults.childWallClockMinutes override', () => {
  const merged = mergeConfig(DEFAULT_CONFIG, { epicDefaults: { childWallClockMinutes: 480 } })
  validateConfig(merged, 'test')
  assert.equal(epicChildWallClockMinutes(merged), 480)

  const shorter = mergeConfig(DEFAULT_CONFIG, { epicDefaults: { childWallClockMinutes: 45 } })
  validateConfig(shorter, 'test')
  assert.equal(epicChildWallClockMinutes(shorter), 45)

  const maximum = mergeConfig(DEFAULT_CONFIG, { epicDefaults: { childWallClockMinutes: 1440 } })
  validateConfig(maximum, 'test')
  assert.equal(epicChildWallClockMinutes(maximum), 1440)
})

test('validateConfig rejects a non-object epicDefaults block', () => {
  for (const epicDefaults of [360, 'long', []]) {
    assert.throws(
      () => validateConfig({ ...DEFAULT_CONFIG, epicDefaults }, 'test'),
      /skill-config:.*epicDefaults must be an object when present/,
    )
  }
})

test('validateConfig warns and coerces a malformed epicDefaults.childWallClockMinutes', () => {
  // Note 0 and -1 are both invalid here, unlike reviewDefaults.deltaFileThreshold where zero is a
  // meaningful setting: a zero-minute clock would expire every child at launch.
  for (const childWallClockMinutes of [undefined, null, '360', 1.5, 0, -1, NaN, {}, 1441, 36000]) {
    const cfg = mergeConfig(DEFAULT_CONFIG, { epicDefaults: { childWallClockMinutes } })
    const originalWarn = console.warn
    const warnings = []
    console.warn = (message) => warnings.push(String(message))
    try {
      validateConfig(cfg, 'test')
    } finally {
      console.warn = originalWarn
    }
    assert.equal(cfg.epicDefaults.childWallClockMinutes, 360)
    assert.equal(epicChildWallClockMinutes(cfg), 360)
    assert.match(warnings.join('\n'), /epicDefaults\.childWallClockMinutes/)
    assert.match(warnings.join('\n'), /must be an integer in \[1, 1440\]/)
    assert.match(warnings.join('\n'), /using 360/)
  }
})

test('epicChildWallClockMinutes rejects an above-ceiling value without validation', () => {
  assert.equal(epicChildWallClockMinutes({ epicDefaults: { childWallClockMinutes: 1 } }), 1)
  assert.equal(epicChildWallClockMinutes({ epicDefaults: { childWallClockMinutes: 1440 } }), 1440)
  assert.equal(epicChildWallClockMinutes({ epicDefaults: { childWallClockMinutes: 36000 } }), 360)
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

test('fixRoundGates returns configured lint and affected tests in gate order', () => {
  assert.deepEqual(
    fixRoundGates({
      commands: { test: 'make test-all', testAffected: 'make test-affected', lint: 'make lint' },
    }),
    {
      gates: [
        { key: 'lint', command: 'make lint' },
        { key: 'testAffected', command: 'make test-affected' },
      ],
      missing: [],
    },
  )
})

test('fixRoundGates uses declared Makefile lint and affected test targets', () => {
  withMarkers({ Makefile: 'lint:\n\ttrue\ntest-affected:\n\ttrue\ntest:\n\ttrue\n' }, (dir) => {
    assert.deepEqual(fixRoundGates(loadSkillConfig({ cwd: dir })), {
      gates: [
        { key: 'lint', command: 'make lint' },
        { key: 'testAffected', command: 'make test-affected' },
      ],
      missing: [],
    })
  })
})

test('fixRoundGates names absent gates and never falls back to the full test command', () => {
  assert.deepEqual(fixRoundGates({}), { gates: [], missing: ['lint', 'testAffected'] })
  assert.deepEqual(fixRoundGates({ commands: { test: 'make test-all' } }), {
    gates: [],
    missing: ['lint', 'testAffected'],
  })
  assert.deepEqual(fixRoundGates({ commands: { lint: 'make lint', testAffected: '' } }), {
    gates: [{ key: 'lint', command: 'make lint' }],
    missing: ['testAffected'],
  })
  assert.deepEqual(fixRoundGates({ commands: { testAffected: 'make test-affected' } }), {
    gates: [{ key: 'testAffected', command: 'make test-affected' }],
    missing: ['lint'],
  })
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
  assert.equal(isHeadless(DEFAULT_CONFIG, { BOSS_UNATTENDED: 'true' }, { isTTY: true }), true)
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

// The full description shape boss-plan used to require. Only Summary and Original notes are required
// now; fixtures that exercise the other sections build from this list.
const FULL_PLAN_SECTIONS = [
  '## Summary',
  '## Approach',
  '## Key changes',
  '## Testing',
  '## Risks / unknowns',
  '## Acceptance criteria',
  '## Required proof',
  '## Planning',
  '## Original notes',
]

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

test('mode-aware section accessors discriminate child-plan and epic-parent contracts', () => {
  assert.deepEqual(
    planSectionsForDescriptionMode(DEFAULT_CONFIG, 'child-plan').map((s) => s.heading),
    planSections(DEFAULT_CONFIG).map((s) => s.heading),
  )
  assert.deepEqual(
    planSectionsForDescriptionMode(DEFAULT_CONFIG, 'epic-parent').map((s) => s.heading),
    ['## Summary', '## Child tickets', '## Planning', '## Original notes'],
  )
  assert.deepEqual(requiredSectionsForDescriptionMode(DEFAULT_CONFIG, 'epic-parent'), [
    '## Summary',
    '## Child tickets',
    '## Original notes',
  ])
  assert.deepEqual(requiredPlanSections(DEFAULT_CONFIG), ['## Summary', '## Original notes'])
})

// Build a plan description whose ## Planning body carries `planningLine`, laid out in the real
// emitted order (## Planning before the terminal ## Original notes).
const planDesc = (planningLine) =>
  FULL_PLAN_SECTIONS.join('\n\nx\n\n').replace('## Planning\n\nx', `## Planning\n\n${planningLine}`)

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

test('planDescriptionSections accepts an explicit epic-parent overview mode', () => {
  assert.deepEqual(
    planDescriptionSections(DEFAULT_CONFIG, epicParentDesc(), { mode: 'epic-parent' }).map(
      (s) => s.heading,
    ),
    ['## Summary', '## Child tickets', '## Planning', '## Original notes'],
  )
  assert.deepEqual(
    planDescriptionSections(DEFAULT_CONFIG, epicParentDesc()).map((s) => s.heading),
    ['## Summary', '## Child tickets', '## Planning', '## Original notes'],
  )
})

test('validatePlanDescription keeps the default child-plan contract unchanged', () => {
  // A child-plan description needs only Summary and Original notes; the epic-only heading is
  // reported as unknown without failing.
  const r = validatePlanDescription(DEFAULT_CONFIG, epicParentDesc())
  assert.equal(r.ok, true)
  assert.deepEqual(r.unknown, ['## Child tickets'])
})

test('validatePlanDescription warns and falls back for an unknown mode', () => {
  const originalWarn = console.warn
  const warnings = []
  console.warn = (message) => warnings.push(String(message))
  try {
    const r = validatePlanDescription(DEFAULT_CONFIG, epicParentDesc(), { mode: 'future-parent' })
    assert.deepEqual(r.unknown, ['## Child tickets'])
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

// The config-first guard reports THREE faults that need different fixes, so no two of them may share
// one message. `{}` passed FIRST is correctly ordered — the remedy is to load a real config — but the
// old single predicate reported it as "arguments look swapped", sending the fix toward argument
// order. Every direction is pinned at all four sites, because a relaxation with only the new
// message asserted would still pass if the swapped-argument branch were deleted outright.
test('a correctly ordered contractless config reports the missing contract, not argument order', () => {
  const description = planDesc('- Contract: v1')
  const sites = [
    ['validatePlanDescription', () => validatePlanDescription({}, description)],
    ['parseAcceptanceCriteria', () => parseAcceptanceCriteria({}, description)],
    ['parsePremises', () => parsePremises({}, description)],
    ['validateVerifyOnlyEvidence', () => validateVerifyOnlyEvidence({}, description)],
  ]
  for (const [name, call] of sites) {
    assert.throws(
      call,
      (error) => {
        assert.match(error.message, /^skill-config: /, `${name} must stay module-prefixed`)
        assert.match(error.message, new RegExp(`${name}\\(config, description\\)`))
        assert.match(error.message, /no plan contract loaded/)
        assert.match(
          error.message,
          /loadSkillConfig\(\)/,
          'must name where a real config comes from',
        )
        assert.doesNotMatch(
          error.message,
          /arguments look swapped/,
          `${name}: a correctly ordered empty config must not be diagnosed as an argument-order fault`,
        )
        return true
      },
      `${name} must diagnose an empty config distinctly`,
    )
  }
  // The BOUNDARY of the split, pinned deliberately. A non-object that could be a description keeps
  // the argument-order error it has always had; an ABSENT config now gets its own message (below).
  assert.throws(() => validatePlanDescription([], description), /arguments look swapped/)
})

// The third fault class (BOS-1244 row 8). An absent config was measured being reported as "arguments
// look swapped", which names a bug the caller does not have: the recorded call was the
// `loadConfig`-for-`loadSkillConfig` typo, where the import resolves to `undefined` and the argument
// ORDER is perfectly correct. The guard's own doc comment claimed an absent value reached the
// missing-contract arm; it could not, because `undefined` fails `isConfigShaped` first.
test('an absent config reports a missing config, never swapped arguments', () => {
  const description = planDesc('- Contract: v1')
  const sites = [
    ['validatePlanDescription', (config) => validatePlanDescription(config, description)],
    ['parseAcceptanceCriteria', (config) => parseAcceptanceCriteria(config, description)],
    ['parsePremises', (config) => parsePremises(config, description)],
    ['validateVerifyOnlyEvidence', (config) => validateVerifyOnlyEvidence(config, description)],
  ]
  for (const [name, call] of sites) {
    for (const absent of [undefined, null]) {
      assert.throws(
        () => call(absent),
        (error) => {
          assert.match(error.message, /^skill-config: /, `${name} must stay module-prefixed`)
          assert.match(error.message, new RegExp(`${name}\\(config, description\\)`))
          assert.match(error.message, /no config passed/)
          assert.match(
            error.message,
            /loadSkillConfig\(\)/,
            'must name where a real config comes from',
          )
          assert.doesNotMatch(
            error.message,
            /arguments look swapped/,
            `${name}: an absent config is not an argument-ORDER fault`,
          )
          return true
        },
        `${name}(${String(absent)}, description) must diagnose the absent config`,
      )
    }
  }
  // All three faults stay DISTINCT messages, or the split bought nothing: a test asserting only the
  // new one would still pass with either of the other two arms deleted outright.
  const messageOf = (call) => {
    try {
      call()
      return ''
    } catch (error) {
      return error.message
    }
  }
  const absent = messageOf(() => validatePlanDescription(undefined, description))
  const swapped = messageOf(() => validatePlanDescription(description, DEFAULT_CONFIG))
  const contractless = messageOf(() => validatePlanDescription({}, description))
  assert.equal(new Set([absent, swapped, contractless]).size, 3, 'three faults, three messages')
})

test('a genuinely swapped call still throws the module-prefixed argument-order error', () => {
  const description = planDesc('- Contract: v1')
  const sites = [
    ['validatePlanDescription', () => validatePlanDescription(description, DEFAULT_CONFIG)],
    ['parseAcceptanceCriteria', () => parseAcceptanceCriteria(description, DEFAULT_CONFIG)],
    ['parsePremises', () => parsePremises(description, DEFAULT_CONFIG)],
    ['validateVerifyOnlyEvidence', () => validateVerifyOnlyEvidence(description, DEFAULT_CONFIG)],
  ]
  for (const [name, call] of sites) {
    assert.throws(
      call,
      (error) => {
        assert.match(error.message, new RegExp(`^skill-config: ${name}\\(config, description\\)`))
        assert.match(error.message, /arguments look swapped; pass the config first/)
        assert.doesNotMatch(error.message, /no plan contract loaded/)
        assert.doesNotMatch(error.message, /Cannot read properties/)
        return true
      },
      `${name} must still catch the natural-reading swapped call`,
    )
  }
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

test('planDescriptionSections follows Markdown H2 indentation rules', () => {
  assert.equal(markdownH2Heading('## Summary'), '## Summary')
  assert.equal(markdownH2Heading('   ## Summary'), '## Summary')
  assert.equal(markdownH2Heading('    ## Summary'), null)

  const desc = planDesc('- Contract: v1').replace(
    '## Key changes\n\nx',
    '## Key changes\n\n    ## quoted code, not a section',
  )
  const headings = planDescriptionSections(DEFAULT_CONFIG, desc).map((s) => s.heading)
  assert.ok(!headings.includes('## quoted code, not a section'))
  const r = validatePlanDescription(DEFAULT_CONFIG, desc)
  assert.deepEqual(r.unknown, [])
  assert.deepEqual(r.missing, [])
  assert.equal(r.ok, true)
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
  assert.deepEqual(r.missing, ['## Original notes'])
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
  const emitted = FULL_PLAN_SECTIONS.filter((h) => h !== '## Summary')
    .join('\n\nx\n\n')
    .replace('## Planning\n\nx', '## Planning\n\n- Contract: v1')
  const desc = `${emitted}\n\n## Summary\n\n(echoed from the ticket)\n\n- Contract: v99\n`
  const r = validatePlanDescription(DEFAULT_CONFIG, desc)
  assert.deepEqual(r.missing, ['## Summary'])
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
          agentBuild: 'friendly',
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
  assert.equal(labelName(cfg, 'agentBuild'), 'friendly')
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

test('contentLabelNames resolves mapped roles and falls back to the literal for unmapped ones', () => {
  assert.deepEqual(CONTENT_LABEL_ROLES, ['bug', 'feature', 'improvement', 'docs'])
  // DEFAULT_CONFIG maps no content role, so every role falls back to its literal.
  assert.deepEqual(contentLabelNames(DEFAULT_CONFIG), ['bug', 'feature', 'improvement', 'docs'])
  // The fixture maps `bug` to `defect`; the rest stay literal.
  assert.deepEqual(contentLabelNames(configuredFixture()), [
    'defect',
    'feature',
    'improvement',
    'docs',
  ])
  const mapped = mergeConfig(configuredFixture(), {
    trackerConfig: { demo: { labels: { bug: 'defect', improvement: 'Enhancement' } } },
  })
  assert.deepEqual(contentLabelNames(mapped), ['defect', 'feature', 'Enhancement', 'docs'])
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
      "Calling labelName(config, 'agentBuild') throws here.",
      'The release role is unavailable for this tracker.',
      'The epic role was deliberately unmapped before BOS-792.',
      "Calling `stateName(config, 'planned')` throws in this example.",
    ].join('\n'),
  )
  assert.deepEqual(
    claims.map((claim) => [claim.role, claim.line]),
    [
      ['bug', 1],
      ['agentBuild', 2],
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
  for (const role of ['agentPlan', 'agentBuild', 'needsHuman', 'agentQuestion', 'epic', 'bug']) {
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

test('validateConfig rejects a trackerConfig entry missing mcpServer or carrying a blank team', () => {
  // Checked on the RAW (un-defaulted) config: mcpServer stays required there.
  assert.throws(
    () =>
      validateConfig(mergeConfig(DEFAULT_CONFIG, { trackerConfig: { demo: { team: 'T' } } }), 't'),
    /skill-config:.*trackerConfig\.demo\.mcpServer must be a non-empty string/,
  )
  // team is optional (zero-config resolves it per run), but a present one must be usable.
  validateConfig(mergeConfig(DEFAULT_CONFIG, { trackerConfig: { demo: { mcpServer: 'x' } } }), 't')
  for (const team of ['', 7]) {
    assert.throws(
      () =>
        validateConfig(
          mergeConfig(DEFAULT_CONFIG, { trackerConfig: { demo: { mcpServer: 'x', team } } }),
          't',
        ),
      /skill-config:.*trackerConfig\.demo\.team must be a non-empty string when present/,
    )
  }
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
  assert.equal(cfg.commands.test, 'make test-full')
  assert.equal(cfg.commands.testAffected, 'make test-affected')
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
    'agentBuild',
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
  FULL_PLAN_SECTIONS.map((heading) => {
    if (heading === '## Acceptance criteria') return `${heading}\n\n${criteria}`
    if (heading === '## Planning') return `${heading}\n\n- Contract: v1`
    return `${heading}\n\nbody`
  }).join('\n\n')

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
    ["grep -c '<th' f", 'substring-count-overmatch'],
    ["sed '1{/^$/d}' f", 'gnu-only-sed-address'],
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

test('classifyCheckCommand rejects a concrete Go package run filter that matches no test', () => {
  const tmp = mkdtempSync(join(tmpdir(), 'boss-skill-config-go-run-'))
  try {
    mkdirSync(join(tmp, 'pkg'))
    writeFileSync(
      join(tmp, 'pkg', 'pkg_test.go'),
      'package pkg\nfunc TestPresent(t *testing.T) {}\n',
    )
    const options = { cwd: tmp, env: process.env }
    const unmatched = classifyCheckCommand('go test -run TestMissing ./pkg', options)
    assert.equal(unmatched.blocking.at(-1).code, 'selection-matches-no-test')
    assert.equal(
      classifyCheckCommand('go test -timeout 1s -run TestMissing ./pkg', options).blocking.at(-1)
        .code,
      'selection-matches-no-test',
    )
    assert.ok(unmatched.advisory.some((finding) => finding.code === 'zero-selection-filter'))
    assert.deepEqual(classifyCheckCommand('go test -run TestPresent ./pkg', options).blocking, [])
    assert.deepEqual(classifyCheckCommand('go test -run "[" ./pkg', options).blocking, [])
    assert.deepEqual(classifyCheckCommand('go test -run TestMissing ./...', options).blocking, [])
    assert.deepEqual(
      classifyCheckCommand('go test -run TestMissing ./unreadable', options).blocking,
      [],
    )
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

test('classifyCheckCommand distinguishes criterion negative searches from premise searches', () => {
  const criterion = classifyCheckCommand(
    '! grep -q setupLines services/boss/internal/views/setupoutput.go',
    {
      kind: 'criterion',
    },
  )
  assert.ok(criterion.blocking.some((finding) => finding.code === 'unanchored-negative-search'))
  assert.deepEqual(
    classifyCheckCommand('! grep -q setupLines services/boss/internal/views/setupoutput.go', {
      kind: 'premise',
    }).blocking,
    [],
  )
  for (const command of [
    '! grep -qw setupLines file',
    '! grep -q -e "\\bsetupLines\\b" file',
    '! grep -q --regexp="\\bsetupLines\\b" file',
    "! grep -q '^setupLines' file",
    '! grep -q setupLines\\. file',
  ]) {
    assert.equal(
      classifyCheckCommand(command, { kind: 'criterion' }).blocking.some(
        (finding) => finding.code === 'unanchored-negative-search',
      ),
      false,
      command,
    )
  }
  assert.ok(
    classifyCheckCommand('rg needle', { kind: 'premise' }).advisory.some(
      (finding) => finding.code === 'unscoped-premise-search',
    ),
  )
  assert.equal(
    classifyCheckCommand('rg needle', { kind: 'criterion' }).advisory.some(
      (finding) => finding.code === 'unscoped-premise-search',
    ),
    false,
  )
})

test('classifyCheckCommand exports every blocking reason and its remedy', () => {
  assert.ok(Object.isFrozen(COMMAND_BLOCKING_CODES))
  for (const code of COMMAND_BLOCKING_CODES) {
    assert.notEqual(commandFindingRemedy(code), '')
  }
  for (const command of [
    'this-command-does-not-exist-bos1247',
    'go test -run TestMissing ./skills-toolbox',
    '! grep -q setupLines skills-toolbox/skill-config.mjs',
  ]) {
    for (const finding of classifyCheckCommand(command, { kind: 'criterion' }).blocking) {
      assert.ok(COMMAND_BLOCKING_CODES.includes(finding.code), finding.code)
    }
  }
})

// BOS-1289 — the three findings whose command can exit 0 while asserting nothing are blocking for a
// criterion (whose check IS its discharge evidence) and advisory for a premise (which observes the
// pre-change tree, where the zero result is frequently the fact being recorded). Both directions are
// pinned per code, because a one-directional pin would pass on a global promotion that rejects
// correct premises.
const VACUOUS_GREEN_CASES = [
  ['zero-selection-filter', 'go test ./... -run TestNoMatch'],
  ['pipe-without-pipefail', 'make test | tee out.log'],
  ['git-grep-word-boundary', 'git grep -E "\\bneedle\\b"'],
]

test('classifyCheckCommand promotes vacuous-green findings for criteria only', () => {
  for (const [code, command] of VACUOUS_GREEN_CASES) {
    const criterion = classifyCheckCommand(command, { kind: 'criterion' })
    assert.ok(
      criterion.blocking.some((finding) => finding.code === code),
      `${command} must block as a criterion with ${code}`,
    )
    assert.equal(
      criterion.advisory.some((finding) => finding.code === code),
      false,
      `${command} must not also stay advisory as a criterion`,
    )

    const premise = classifyCheckCommand(command, { kind: 'premise' })
    assert.ok(
      premise.advisory.some((finding) => finding.code === code),
      `${command} must stay advisory as a premise with ${code}`,
    )
    assert.equal(
      premise.blocking.some((finding) => finding.code === code),
      false,
      `${command} must not block as a premise`,
    )

    // An unknown kind is not a claim that the command is discharge evidence, so it keeps the
    // advisory tier — this is the direction every pre-BOS-1289 caller relies on.
    const unkeyed = classifyCheckCommand(command)
    assert.ok(
      unkeyed.advisory.some((finding) => finding.code === code),
      `${command} must stay advisory with no kind`,
    )
    assert.equal(unkeyed.blocking.length, 0, `${command} must not block with no kind`)

    assert.ok(COMMAND_BLOCKING_CODES.includes(code), `${code} must be a frozen blocking code`)
    assert.notEqual(commandFindingRemedy(code), '')
    assert.notEqual(
      commandFindingRemedy(code),
      commandFindingRemedy('command-unresolvable'),
      `${code} must carry its own remedy, not the fallback`,
    )
  }
})

test('hasCountAssertion recognises every wc measurement, not only -l', () => {
  // One shared definition of "this command re-measures something": classifyCheckCommand's
  // zero-selection rule and the plan-contract guard's unmeasured-count-claim both read it, so the
  // two cannot disagree about what counts as a measurement.
  for (const command of [
    'wc -l f',
    'wc -c f',
    'wc -w f',
    'wc -m f',
    'rg -q x f',
    'go test -count=1',
  ]) {
    assert.equal(hasCountAssertion(command), true, command)
  }
  for (const command of ['rg -n needle file', 'make test', 'go test ./...']) {
    assert.equal(hasCountAssertion(command), false, command)
  }
  // The byte-size case is why `-c` was added: a descending-budget premise is measured with `wc -c`,
  // so a criterion checking one must not also raise zero-selection-filter.
  assert.deepEqual(classifyCheckCommand('wc -c skills-toolbox/skill-config.mjs').advisory, [])
})

test('the promotion leaves every other advisory finding advisory for a criterion', () => {
  // The discriminator is "can exit 0 asserting nothing", not "is a command-shape risk". A global
  // promotion would pass the test above and red here.
  for (const [command, code] of [
    ['git diff -- skills-toolbox/skill-config.mjs', 'working-tree-scoped-git-check'],
    // `# pass 2` supplies the count assertion, so this isolates the glob finding from the
    // zero-selection finding `--include` would otherwise raise alongside it.
    ['node --include=*.md # pass 2', 'unquoted-option-glob'],
    ['bazel test //services/boss/...', 'cached-bazel-test'],
    ["grep -c '<th' skills-toolbox/skill-config.mjs", 'substring-count-overmatch'],
    ["sed '1{/^$/d}' skills-toolbox/skill-config.mjs", 'gnu-only-sed-address'],
  ]) {
    const result = classifyCheckCommand(command, { kind: 'criterion' })
    assert.ok(
      result.advisory.some((finding) => finding.code === code),
      `${command} should still report ${code} as advisory`,
    )
    assert.equal(result.blocking.length, 0, `${command} must not block as a criterion`)
  }
})

test('verify-only discharge accepts explanatory prose before the first backticked command', () => {
  const body = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} inv${VERIFY_ONLY_CHECKED}after checking the fixture, \`make test-scripts\`${VERIFY_ONLY_RESULT}pass`,
  )
  const parsed = parseAcceptanceCriteria(DEFAULT_CONFIG, body)[0]
  assert.equal(parsed.check, 'make test-scripts')
  assert.equal(parsed.result, 'pass')
  assert.equal(validateVerifyOnlyEvidence(DEFAULT_CONFIG, body).ok, true)
  const missing = planBody(
    `- [x] ${VERIFY_ONLY_MARKER} inv${VERIFY_ONLY_CHECKED}after checking the fixture`,
  )
  assert.equal(
    validateVerifyOnlyEvidence(DEFAULT_CONFIG, missing).missingEvidence[0].reason,
    'undelimited-command',
  )
})

test('classifyCheckCommand resolves make goals and path operands only when absence is decidable', () => {
  const tmp = mkdtempSync(join(tmpdir(), 'boss-skill-config-operands-'))
  try {
    mkdirSync(join(tmp, 'scripts'))
    writeFileSync(
      join(tmp, 'Makefile'),
      [
        '.PHONY: phony \\',
        '  continued',
        'defined:',
        'lint test:',
        'continued-a \\',
        ' continued-b:',
        '',
      ].join('\n'),
    )
    const options = { cwd: tmp, env: process.env }

    assert.deepEqual(classifyCheckCommand('make defined', options).blocking, [])
    assert.deepEqual(classifyCheckCommand('make phony continued', options).blocking, [])
    assert.deepEqual(classifyCheckCommand('make lint test', options).blocking, [])
    assert.deepEqual(classifyCheckCommand('make continued-a continued-b', options).blocking, [])
    assert.deepEqual(classifyCheckCommand('make -j 8 defined', options).blocking, [])
    assert.deepEqual(classifyCheckCommand('make --jobs 8 defined', options).blocking, [])
    assert.equal(
      classifyCheckCommand('make absent', options).blocking[0].code,
      'make-goal-undefined',
    )
    assert.equal(
      classifyCheckCommand('make -C scripts absent', options).advisory[0].code,
      'make-goal-unresolved',
    )
    assert.equal(
      classifyCheckCommand('make -f absent.mk absent', options).advisory[0].code,
      'make-goal-unresolved',
    )

    writeFileSync(join(tmp, 'Open.mk'), 'generated-%:\n\t@true\n')
    assert.equal(
      classifyCheckCommand('make -f Open.mk absent', options).advisory[0].code,
      'make-goal-unresolved',
    )
    writeFileSync(join(tmp, 'Attached.mk'), 'defined:\n')
    assert.deepEqual(classifyCheckCommand('make -fAttached.mk defined', options).blocking, [])
    writeFileSync(join(tmp, 'GNUmakefile'), 'gnu-defined:\n')
    assert.deepEqual(classifyCheckCommand('make gnu-defined', options).blocking, [])
    assert.equal(
      classifyCheckCommand('make defined', options).blocking[0].code,
      'make-goal-undefined',
    )
    writeFileSync(join(tmp, 'first.mk'), 'from-first:\n')
    writeFileSync(join(tmp, 'second.mk'), 'from-second:\n')
    const multipleMakefiles = classifyCheckCommand(
      'make -f first.mk -f second.mk from-first',
      options,
    )
    assert.deepEqual(multipleMakefiles.blocking, [])
    assert.equal(multipleMakefiles.advisory[0].code, 'make-goal-unresolved')
    assert.equal(
      classifyCheckCommand('node --test missing-dir/new.test.mjs', options).blocking[0].code,
      'path-operand-missing',
    )
    assert.equal(
      classifyCheckCommand('node --test scripts/new.test.mjs', options).advisory[0].code,
      'path-operand-absent',
    )
    assert.deepEqual(classifyCheckCommand('node --test scripts/*.test.mjs', options).blocking, [])
    assert.deepEqual(
      classifyCheckCommand(
        'node --test --test-name-pattern missing-dir/pattern scripts/new.test.mjs',
        options,
      ).blocking,
      [],
    )
    for (const option of [
      '--test-concurrency',
      '--test-coverage-exclude',
      '--experimental-test-isolation',
      '--test-global-setup',
      '--test-isolation',
      '--test-rerun-failures',
      '--test-skip-pattern',
      '--test-timeout',
    ]) {
      assert.deepEqual(
        classifyCheckCommand(`node --test ${option} missing-dir/value`, options).blocking,
        [],
        option,
      )
    }
    assert.deepEqual(classifyCheckCommand('node -e "true"', options).blocking, [])
    assert.equal(
      classifyCheckCommand('bash missing-dir/script.sh', options).blocking[0].code,
      'path-operand-missing',
    )
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }

  assert.deepEqual(classifyCheckCommand('make target-that-does-not-exist-bos1239').blocking, [])
  assert.ok(
    classifyCheckCommand('make target-that-does-not-exist-bos1239').advisory.some(
      (finding) => finding.code === 'make-goal-unresolved',
    ),
  )
})

// A negated check command is legitimate: `! grep -q needle file` is the natural way to assert a
// pattern is ABSENT. `!` is the POSIX negation keyword and must be its own word, so it landed as
// the segment head and resolved to no executable, making the whole shape unrecordable.
//
// This is a RELAXATION, so both directions are pinned. Pinning only the positive case would make
// the gate vacuous: a later change that dropped head resolution entirely would still pass.
test('classifyCheckCommand accepts a leading `!` negation without excusing the head', () => {
  // Positive: the negation is stripped and the real head classifies normally.
  // Heads here must be PATH-guaranteed on a bare CI runner: `classifyCheckCommand` resolves the
  // head against PATH, so an example like `rg` (not installed on GitHub's ubuntu image) reports
  // `command-unresolvable` and reds this test for a reason that has nothing to do with negation.
  // `grep` and `make` are both present everywhere this suite runs.
  assert.deepEqual(classifyCheckCommand('! grep -q needle file').blocking, [])
  assert.deepEqual(classifyCheckCommand('! make test').blocking, [])
  // ...in any segment, not only the first.
  assert.deepEqual(classifyCheckCommand('make test && ! grep -q needle file').blocking, [])
  assert.deepEqual(classifyCheckCommand('true; ! grep -q needle file').blocking, [])

  // Negative 1 — a segment that is ONLY `!` has no head at all and must stay unresolvable.
  assert.equal(classifyCheckCommand('!').blocking[0].code, 'command-unresolvable')
  assert.equal(classifyCheckCommand('make test && !').blocking[0].code, 'command-unresolvable')

  // Negative 2 — negating an unknown head does not make it resolve. The relaxation removes `!`
  // from the head position; it does not excuse the head from resolving.
  assert.equal(
    classifyCheckCommand('! this-command-does-not-exist-bos1189').blocking[0].code,
    'command-unresolvable',
  )
  assert.equal(
    classifyCheckCommand('make test && ! this-command-does-not-exist-bos1189').blocking[0].code,
    'command-unresolvable',
  )

  // Only ONE leading `!` is stripped, so a doubled negation still leaves `!` as the head.
  assert.equal(
    classifyCheckCommand('! ! grep -q needle file').blocking[0].code,
    'command-unresolvable',
  )
})

// BOS-1186: the two measured false-drift shapes. Both are advisory by design — a blocking finding
// here would make the verification instrument more brittle than the premise it re-checks, which is
// the failure this ticket exists to avoid.
test('classifyCheckCommand flags the two measured false-drift shapes as advisory only', () => {
  const overmatch = classifyCheckCommand("grep -c '<th' f")
  assert.deepEqual(
    overmatch.advisory.map((finding) => finding.code),
    ['substring-count-overmatch'],
  )
  assert.match(overmatch.advisory[0].message, /superstring/)
  assert.deepEqual(overmatch.blocking, [])

  // Anchoring by flag suppresses it — this is the correction the BOS-1159 note recorded.
  assert.deepEqual(classifyCheckCommand("grep -cw '<th' f").advisory, [])
  assert.deepEqual(classifyCheckCommand("grep -cw '<th' f").blocking, [])
  // Anchoring inside the pattern suppresses it too.
  assert.deepEqual(classifyCheckCommand("grep -c '^th' f").advisory, [])
  // A count with no anchor is the shape, whichever tool spells it.
  assert.deepEqual(
    classifyCheckCommand('rg --count needle f').advisory.map((finding) => finding.code),
    ['substring-count-overmatch'],
  )
  // `-C` is context, not count, and must not be read as one.
  assert.deepEqual(classifyCheckCommand("grep -C 3 '<th' f").advisory, [])
  // A grep with no count flag is not this shape at all.
  assert.deepEqual(classifyCheckCommand("grep -n '<th' f").advisory, [])

  const sedAddress = classifyCheckCommand("sed '1{/^$/d}' f")
  assert.deepEqual(
    sedAddress.advisory.map((finding) => finding.code),
    ['gnu-only-sed-address'],
  )
  assert.match(sedAddress.advisory[0].message, /BSD sed/)
  assert.deepEqual(sedAddress.blocking, [])

  assert.deepEqual(classifyCheckCommand("sed -n '1,5p' f").advisory, [])
  assert.deepEqual(classifyCheckCommand("sed -n '1,5p' f").blocking, [])
  // The `-e` spelling carries the same script and the same risk.
  assert.deepEqual(
    classifyCheckCommand("sed -e '1{/^$/d}' f").advisory.map((finding) => finding.code),
    ['gnu-only-sed-address'],
  )
  // ...and so does the ATTACHED spelling, where the script rides on the option token itself. The
  // detector's non-option test never sees these, so before BOS-1186 they evaded it silently.
  for (const command of ["sed -e'1{/^$/d}' f", "sed --expression='1{/^$/d}' f"]) {
    assert.deepEqual(
      classifyCheckCommand(command).advisory.map((finding) => finding.code),
      ['gnu-only-sed-address'],
      command,
    )
    assert.deepEqual(classifyCheckCommand(command).blocking, [], command)
  }
  // An attached option that carries no brace block is still not this shape.
  assert.deepEqual(classifyCheckCommand("sed -e'1,5p' f").advisory, [])
  // Neither finding may reach the blocking tier through the plan-contract vacuity gate.
  for (const command of ["grep -c '<th' f", "sed '1{/^$/d}' f"]) {
    assert.deepEqual(classifyCheckCommand(command).blocking, [], command)
  }
})

// BOS-1358 — three check-command shapes that report a false red or a mis-scoped result. All three
// are advisory: none of them is a vacuous green, so none joins the blocking tier.
const BOS1358_CODES = [
  'pipefail-early-exit-reader',
  'unscoped-negative-directory-search',
  'go-build-writes-output',
]
const advisoryCodes = (command, options) =>
  classifyCheckCommand(command, options).advisory.map((finding) => finding.code)

test('BOS-1358 pipefail-early-exit-reader flags a reader that exits on its first hit', () => {
  for (const command of [
    'set -o pipefail; go test -v ./x | rg -q PASS',
    'set -o pipefail; go test -v ./x | grep -q PASS',
    'set -o pipefail; go test -v ./x | grep -qE "PASS|ok"',
    'set -o pipefail; go test -v ./x | grep --quiet PASS',
    'set -o pipefail; go test -v ./x | grep -m 1 PASS',
    'set -o pipefail; go test -v ./x | rg --max-count=1 PASS',
    'set -o pipefail; go test -v ./x | head -1',
  ]) {
    assert.ok(advisoryCodes(command).includes('pipefail-early-exit-reader'), command)
  }
  for (const command of [
    // Counting drains the whole stream, so the writer never takes SIGPIPE.
    'set -o pipefail; go test -v ./x | grep -c PASS',
    'set -o pipefail; go test -v ./x | rg -c PASS',
    // Without pipefail this is the existing vacuous-green rule's shape, not this one.
    'go test -v ./x | grep -q PASS',
    // A quiet reader that is not downstream of a pipe closes nothing.
    'set -o pipefail; grep -q PASS out.log',
  ]) {
    assert.equal(advisoryCodes(command).includes('pipefail-early-exit-reader'), false, command)
  }
  const [finding] = classifyCheckCommand('set -o pipefail; go test -v ./x | grep -q PASS', {
    kind: 'criterion',
  }).advisory.filter((entry) => entry.code === 'pipefail-early-exit-reader')
  assert.match(finding.message, /SIGPIPE/)
  assert.match(finding.message, /141/)
  assert.match(finding.message, /grep -c/)
})

test('BOS-1358 unscoped-negative-directory-search flags a negated search over a whole directory', () => {
  const tmp = mkdtempSync(join(tmpdir(), 'boss-skill-config-negdir-'))
  try {
    mkdirSync(join(tmp, 'migrations'))
    writeFileSync(join(tmp, 'migrations', '001.sql'), 'create table t (id int);\n')
    writeFileSync(join(tmp, 'migrations', 'guard_test.go'), 'package m // names jsonb\n')
    const options = { cwd: tmp }
    for (const command of [
      '! grep -rqi jsonb migrations',
      '! grep -r -q jsonb migrations',
      '! grep -Rq jsonb migrations',
      '! rg -q jsonb migrations',
    ]) {
      assert.ok(
        advisoryCodes(command, options).includes('unscoped-negative-directory-search'),
        command,
      )
    }
    for (const command of [
      "! grep -rqi --include='*.sql' jsonb migrations",
      "! grep -rqi --exclude='*_test.go' jsonb migrations",
      "! rg -q -g '*.sql' jsonb migrations",
      '! rg -q --type sql jsonb migrations',
      // Not negated: a positive search over a directory can only be falsified by a real match.
      'grep -rqi jsonb migrations',
      // Not recursive: plain grep over a directory reads nothing from it.
      '! grep -qi jsonb migrations/001.sql',
      // The path operand is a file, not a directory.
      '! rg -q jsonb migrations/001.sql',
      // The path does not exist, so the directory branch is not the shape here.
      '! grep -rqi jsonb missing-dir',
      // `-m` takes a value: the pattern is `migrations`, and the only path operand is a file.
      '! rg -m 1 migrations migrations/001.sql',
    ]) {
      assert.equal(
        advisoryCodes(command, options).includes('unscoped-negative-directory-search'),
        false,
        command,
      )
    }
    const [finding] = classifyCheckCommand('! grep -rqi jsonb migrations', options).advisory.filter(
      (entry) => entry.code === 'unscoped-negative-directory-search',
    )
    assert.match(finding.message, /tests and guards/)
    assert.match(finding.message, /--include='\*\.sql'/)
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

test('BOS-1358 go-build-writes-output flags a single-package go build without -o', () => {
  for (const command of [
    'go build ./cmd/',
    'cd services/boss && go build ./cmd/',
    'go build -tags integration ./cmd/boss',
  ]) {
    assert.ok(advisoryCodes(command).includes('go-build-writes-output'), command)
  }
  for (const command of [
    'go build -o /dev/null ./cmd/',
    'go build -o=/dev/null ./cmd/',
    'go vet ./cmd/',
    'go build ./...',
    'go build ./services/boss/...',
  ]) {
    assert.equal(advisoryCodes(command).includes('go-build-writes-output'), false, command)
  }
  const [finding] = classifyCheckCommand('go build ./cmd/').advisory.filter(
    (entry) => entry.code === 'go-build-writes-output',
  )
  assert.match(finding.message, /same-named directory/)
  assert.match(finding.message, /go build -o \/dev\/null/)
})

test('BOS-1358 codes stay advisory: none is blocking, for either kind', () => {
  for (const code of BOS1358_CODES) {
    assert.equal(COMMAND_BLOCKING_CODES.includes(code), false, code)
    assert.notEqual(
      commandFindingRemedy(code),
      commandFindingRemedy('command-unresolvable'),
      `${code} needs its own remedy`,
    )
  }
  const tmp = mkdtempSync(join(tmpdir(), 'boss-skill-config-bos1358-'))
  try {
    mkdirSync(join(tmp, 'migrations'))
    mkdirSync(join(tmp, 'cmd'))
    // Every head here is PATH-guaranteed on a bare CI runner (`rg` is not), so an empty blocking
    // tier is about the new findings and not about head resolution.
    const carriers = [
      ['pipefail-early-exit-reader', 'set -o pipefail; go test -v ./x | grep -q PASS'],
      // `-w` keeps a criterion clear of the separate `unanchored-negative-search` blocking rule.
      ['unscoped-negative-directory-search', '! grep -rqw jsonb migrations'],
      ['go-build-writes-output', 'go build ./cmd/'],
    ]
    for (const [code, command] of carriers) {
      for (const kind of ['criterion', 'premise', null]) {
        const result = classifyCheckCommand(command, { cwd: tmp, kind })
        assert.ok(
          result.advisory.some((finding) => finding.code === code),
          `${command} (${kind})`,
        )
        assert.deepEqual(result.blocking, [], `${command} (${kind})`)
      }
    }
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
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

// ---------------------------------------------------------------------------
// BOS-1199 U3 — the tolerated description-normalization transform seam.
//
// A tracker that normalizes markdown on write reshapes a description without changing what it
// means. The post-save read-back must tell that apart from a transcription slip, so a repo may
// declare which transforms it tolerates, from a closed vocabulary.
//
// The DEFAULT was inverted: absence now yields the FULL vocabulary, and an explicitly empty
// `tolerated` array is the opt-in to byte-exact strictness. The empty default made the strictest
// possible comparison apply to every repo that had never heard of the knob, and measured over 60
// runs that fired on 55% of verifications with no content loss in any inspected case. Absence
// yielding "tolerate all" is safe precisely because tolerance governs only the COSMETIC conjunct:
// a lost section, a dropped upload identity, or a changed verbatim block is detected by conjuncts
// this set does not touch, and each still blocks.
// ---------------------------------------------------------------------------

const withNormalization = (tolerated) =>
  mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        ...(tolerated === undefined ? {} : { descriptionNormalization: { tolerated } }),
      },
    },
  })

test('U3: a config declaring a valid subset loads and the accessor returns exactly that set', () => {
  const config = withNormalization([
    'unordered-list-marker-substitution',
    'terminal-newline-trimming',
  ])
  validateConfig(config, 'test')
  assert.deepEqual(
    toleratedDescriptionTransforms(config),
    new Set(['unordered-list-marker-substitution', 'terminal-newline-trimming']),
  )
})

test('U3: an absent normalization block yields the FULL vocabulary (default lenient)', () => {
  const config = withNormalization(undefined)
  validateConfig(config, 'test')
  assert.deepEqual(
    toleratedDescriptionTransforms(config),
    new Set(DESCRIPTION_NORMALIZATION_TRANSFORMS),
  )
})

test('U3: an explicitly empty tolerated array is the opt-in to byte-exact strictness', () => {
  const config = withNormalization([])
  validateConfig(config, 'test')
  assert.deepEqual(toleratedDescriptionTransforms(config), new Set())
})

test('U3: absence and an explicitly empty array are NOT equivalent', () => {
  // The inverted default turns on this distinction: "no opinion" and "I want byte-exact" used to
  // be the same input. If these ever collapse again, one of the two intents has been lost.
  assert.notDeepEqual(
    toleratedDescriptionTransforms(withNormalization(undefined)),
    toleratedDescriptionTransforms(withNormalization([])),
  )
})

test('U3: onUnattributedDrift defaults to warn, honours block, and falls back on an unknown value', () => {
  const withSeverity = (onUnattributedDrift) =>
    mergeConfig(DEFAULT_CONFIG, {
      adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
      trackerConfig: {
        demo: {
          mcpServer: 'demo-tracker',
          team: 'Demo',
          descriptionNormalization: { onUnattributedDrift },
        },
      },
    })
  assert.equal(unattributedDriftSeverity(withNormalization(undefined)), 'warn')
  assert.equal(unattributedDriftSeverity(withSeverity('warn')), 'warn')
  assert.equal(unattributedDriftSeverity(withSeverity('block')), 'block')

  // An unrecognised value warns and falls back to the DEFAULT, not to the stricter reading: a
  // severity this copy cannot parse must never start failing completed runs on its own.
  const config = withSeverity('explode')
  const warned = warningsFrom(() => validateConfig(config, 'test'))
  assert.match(warned, /onUnattributedDrift is "explode"/)
  assert.match(warned, /falling back to "warn"/)
  assert.equal(unattributedDriftSeverity(config), 'warn')

  // A structural fault still throws — a repo that meant to configure something and did not.
  assert.throws(() => validateConfig(withSeverity(42), 'test'), /onUnattributedDrift/)
})

/** Capture whatever validation warns about, without letting it reach the suite's output. */
function warningsFrom(run) {
  const originalWarn = console.warn
  const warnings = []
  console.warn = (message) => warnings.push(String(message))
  try {
    run()
  } finally {
    console.warn = originalWarn
  }
  return warnings.join('\n')
}

test('U3: an unfamiliar but well-formed id validates, is DROPPED from the resolved set, and warns', () => {
  // Retargeted from a throw (BOS-1223 U3). This file is copy-distributed and then extracted into
  // every user's global skill directory, and the whole config is validated before any of it is
  // returned — so a hard failure on an id a stale copy has not learned yet turns an additive
  // widening of the vocabulary into a crash for review, build and repair, not just planning.
  // Dropping is the safe direction: a smaller tolerated set makes the comparison STRICTER.
  const config = withNormalization(['unordered-list-marker-substitution', 'tabs-to-spaces'])
  const warned = warningsFrom(() => validateConfig(config, 'test'))
  assert.match(warned, /descriptionNormalization\.tolerated names "tabs-to-spaces"/)
  assert.match(warned, /STRICTER/)
  // A tolerated list mixing a known and an unfamiliar id keeps the known one and drops the other —
  // and this is exactly why the authoring guard below had to move off the resolved set: a mistyped
  // id can no longer appear in it, so a membership loop over it could never fail.
  assert.deepEqual(
    toleratedDescriptionTransforms(config),
    new Set(['unordered-list-marker-substitution']),
  )
})

test('U3: a structurally bad tolerated ENTRY still throws — only unfamiliar ids degrade', () => {
  // The strictness split is by role, not a blanket loosening. A non-string or empty-string entry is
  // a repo that meant to configure something and did not; there is nothing forward-compatible about
  // it, so it stays a hard failure.
  for (const bad of [42, null, { id: 'x' }, '']) {
    assert.throws(
      () => validateConfig(withNormalization(['terminal-newline-trimming', bad]), 'test'),
      /skill-config:.*descriptionNormalization\.tolerated entries must be non-empty strings/,
      `${JSON.stringify(bad)} is structurally wrong, not merely unfamiliar`,
    )
  }
})

test('U3: a non-array tolerated fails validation, naming the expected type', () => {
  for (const bad of ['unordered-list-marker-substitution', { id: 'x' }]) {
    assert.throws(
      () => validateConfig(withNormalization(bad), 'test'),
      /skill-config:.*descriptionNormalization\.tolerated must be an array of transform ids/,
    )
  }
  assert.throws(
    () =>
      validateConfig(
        mergeConfig(DEFAULT_CONFIG, {
          adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
          trackerConfig: {
            demo: { mcpServer: 'demo-tracker', team: 'Demo', descriptionNormalization: [] },
          },
        }),
        'test',
      ),
    /skill-config:.*descriptionNormalization must be an object when present/,
  )
})

test('U3: an explicitly empty array means byte-exact, and an absent block does not', () => {
  // Inverted from its original form, which asserted the two were equivalent. They are now the two
  // distinct intents this seam exists to express: "I want byte-exact" and "no opinion".
  const explicit = withNormalization([])
  const absent = withNormalization(undefined)
  validateConfig(explicit, 'test')
  validateConfig(absent, 'test')
  assert.equal(toleratedDescriptionTransforms(explicit).size, 0)
  assert.equal(
    toleratedDescriptionTransforms(absent).size,
    DESCRIPTION_NORMALIZATION_TRANSFORMS.length,
  )
})

/** Every tolerated id as WRITTEN in a raw config object, across every tracker adapter it configures. */
const rawToleratedIds = (raw) =>
  Object.values(raw.trackerConfig ?? {}).flatMap(
    (tc) => tc?.descriptionNormalization?.tolerated ?? [],
  )

const unrecognisedIn = (ids) =>
  ids.filter((id) => !DESCRIPTION_NORMALIZATION_TRANSFORMS.includes(id))

test("U3: the repo's own .boss-skills.json parses and validates under the new rules", () => {
  // Read the ids as COMMITTED, before validation filters them. The guard used to assert membership
  // over the RESOLVED set, and once validation drops an unfamiliar id that loop is unfalsifiable —
  // a typo would vanish with a warning instead of reddening, which would satisfy the
  // forward-compatible consuming path while silently voiding the closed authoring vocabulary.
  // Strict on the authoring path, forgiving on the consuming one, is the whole shape of U3.
  const config = loadSkillConfig({ cwd: REPO_ROOT })
  validateConfig(config, 'repo')

  // Whatever this repo commits — a list, or nothing at all — must be in the closed vocabulary.
  // No longer requires it to commit one: the default is now the full vocabulary, and this repo
  // deliberately relies on it rather than restating five of six ids that then rot out of step.
  const committed = rawToleratedIds(
    JSON.parse(readFileSync(join(REPO_ROOT, CONFIG_FILENAME), 'utf8')),
  )
  assert.deepEqual(
    unrecognisedIn(committed),
    [],
    'every id this repo COMMITS must be in the closed vocabulary',
  )

  // R10: the reshaping this repo's tracker was MEASURED performing on every table it stores.
  // Without it the write-back gate reports drift on any plan containing a table, and a verdict that
  // fires on most runs trains its reader to discount it. The invariant is that the RESOLVED set
  // carries it, which is what the gate actually reads — declared or defaulted is immaterial.
  assert.ok(
    toleratedDescriptionTransforms(config).has('table-delimiter-row-normalization'),
    "the measured table-delimiter reshaping resolves, so this repo's own gate can reach tier 2",
  )
})

test('U3: that authoring guard REDS on a mistyped id — proven, not merely passing today', () => {
  // A guard that has only ever passed proves nothing. Splice an id outside the vocabulary into a
  // config and assert the guard's own predicate rejects it.
  //
  // Built on a SYNTHETIC config rather than a mutated copy of the repo's own. The probe used to
  // require this repo to commit a tolerated list purely so there was something to mistype, which
  // made a non-vacuity proof depend on an unrelated config choice — and it duly broke the moment
  // this repo dropped its list in favour of the default. What is under test is the predicate, not
  // the repo.
  const raw = {
    trackerConfig: {
      demo: {
        descriptionNormalization: {
          tolerated: ['terminal-newline-trimming', 'terminal-newline-trimmingg'],
        },
      },
    },
  }
  assert.ok(
    rawToleratedIds(raw).includes('terminal-newline-trimmingg'),
    'the probe mutation must actually land, or a green below would be vacuous',
  )
  assert.deepEqual(unrecognisedIn(rawToleratedIds(raw)), ['terminal-newline-trimmingg'])
  // And the valid neighbour is NOT flagged, so the predicate discriminates rather than rejecting.
  assert.ok(unrecognisedIn(['terminal-newline-trimming']).length === 0)
})

// --- trackerConfig.<adapter>.selection: candidate narrowing (BOS-1378) ------------------------
//
// The schema's own rule table lives in selection.test.mjs. These pin the config seam: validateConfig
// turns every selection error into a throw (unknown and legacy keys included), `selectionBlockFor`
// never throws, and `stageSelectionQuery` is the one derivation every gate and worker reads.

/** A config whose sole tracker adapter carries the given `selection` value (or none). */
const withSelection = (selection) =>
  mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        states: { planned: 'Planned', unplanned: 'Unplanned' },
        labels: { agentBuild: 'agent-build', agentPlan: 'agent-plan', needsHuman: 'needs-human' },
        ...(selection === undefined ? {} : { selection }),
      },
    },
  })

const TICKET_EXAMPLE = {
  labels: { include: [], exclude: [] },
  assignees: { include: [], exclude: [] },
  creators: { include: [], exclude: [] },
  projects: { include: [], exclude: [] },
  stages: { build: { labels: { exclude: ['infra'] } } },
}

test('selection: the ticket example validates and the block reads back unchanged', () => {
  const config = withSelection(TICKET_EXAMPLE)
  validateConfig(config, 'test')
  assert.deepEqual(selectionBlockFor(config), TICKET_EXAMPLE)
})

test('selection: unknown, legacy and malformed keys THROW with a message naming the replacement', () => {
  const cases = [
    ['a non-object selection', [], /trackerConfig\.demo\.selection must be an object when present/],
    ['a null selection', null, /trackerConfig\.demo\.selection must be an object when present/],
    [
      'the legacy assigneeOrCreator',
      { assigneeOrCreator: 'me' },
      /trackerConfig\.demo\.selection\.assigneeOrCreator was removed; use trackerConfig\.demo\.selection\.assignees\.include and\/or trackerConfig\.demo\.selection\.creators\.include/,
    ],
    [
      'the legacy array-valued labels',
      { labels: ['label-a'] },
      /trackerConfig\.demo\.selection\.labels as an array was removed; use trackerConfig\.demo\.selection\.labels: \{include: \[\.\.\.\]\}/,
    ],
    [
      'an unknown key',
      { label: { include: ['a'] } },
      /selection has unknown key "label"; allowed keys: labels, assignees, creators, projects, stages/,
    ],
    [
      'an unknown stage',
      { stages: { deploy: {} } },
      /selection\.stages has unknown stage "deploy"/,
    ],
    [
      'a display-name user',
      { creators: { exclude: ['Some Bot'] } },
      /"Some Bot" is not a user selector; use me, a user id \(UUID\), or an email/,
    ],
  ]
  for (const [label, selection, pattern] of cases) {
    assert.throws(() => validateConfig(withSelection(selection), 'test'), pattern, label)
    assert.throws(() => validateConfig(withSelection(selection), 'test'), /^Error: skill-config:/)
  }
})

test('selectionBlockFor: returns null for an absent block and never throws on garbage', () => {
  assert.equal(selectionBlockFor(withSelection(undefined)), null)
  for (const raw of [
    { adapters: { tracker: 'demo' }, trackerConfig: { demo: { selection: [] } } },
    { adapters: { tracker: 'demo' }, trackerConfig: { demo: { selection: 'me' } } },
    { adapters: { tracker: 'demo' }, trackerConfig: { demo: { selection: null } } },
    { adapters: { tracker: 'demo' }, trackerConfig: {} },
    { trackerConfig: { demo: { selection: { labels: { include: ['a'] } } } } },
    {},
    null,
    undefined,
  ]) {
    assert.equal(selectionBlockFor(raw), null, JSON.stringify(raw))
  }
})

test('stageSelectionQuery: the zero-config build query is planned AND agent-build AND NOT needs-human', () => {
  const query = stageSelectionQuery(withSelection(undefined), 'build')
  assert.deepEqual(query, {
    state: 'Planned',
    selection: {
      labels: { include: [], exclude: [] },
      assignees: { include: [], exclude: [] },
      creators: { include: [], exclude: [] },
      projects: { include: [], exclude: [] },
    },
    requireLabels: ['agent-build'],
    excludeLabels: ['needs-human'],
  })
  assert.equal(stageSelectionQuery(withSelection(undefined), 'plan').state, 'Unplanned')
})

test('stageSelectionQuery: precedence is flag > stage > shared, per slot', () => {
  const config = withSelection({
    ...TICKET_EXAMPLE,
    labels: { include: ['backend'], exclude: ['shared'] },
  })
  assert.deepEqual(stageSelectionQuery(config, 'build').selection.labels, {
    include: ['backend'],
    exclude: ['infra'],
  })
  assert.deepEqual(stageSelectionQuery(config, 'plan').selection.labels, {
    include: ['backend'],
    exclude: ['shared'],
  })
  assert.deepEqual(
    stageSelectionQuery(config, 'build', { labels: { exclude: ['flag'] } }).selection.labels,
    { include: ['backend'], exclude: ['flag'] },
  )
})

test('stageSelectionQuery: an unconfigured state or an invalid hand-built block throws rather than widening', () => {
  const config = withSelection(undefined)
  delete config.trackerConfig.demo.states.planned
  assert.throws(() => stageSelectionQuery(config, 'build'), /trackerConfig\.demo\.states\.planned/)
  const handBuilt = withSelection(undefined)
  handBuilt.trackerConfig.demo.selection = { assigneeOrCreator: 'me' }
  assert.throws(() => stageSelectionQuery(handBuilt, 'build'), /assigneeOrCreator was removed/)
  assert.throws(
    () => stageSelectionQuery(withSelection(undefined), 'deploy'),
    /unknown stage "deploy"/,
  )
})

test('selection: this repo ships no selection block, so every stage scan is unnarrowed', () => {
  assert.equal(selectionBlockFor(loadSkillConfig()), null)
})

// BOS-1328: an orchestrator-inserted Planning bullet merged into its neighbour passed every gate.
const planningDescription = (planning, { tail = '' } = {}) =>
  [
    ...FULL_PLAN_SECTIONS.filter((h) => h !== '## Planning' && h !== '## Original notes').map(
      (h) => `${h}\n\nBody.`,
    ),
    `## Planning\n\n${planning}`,
    `## Original notes\n\nReporter text.${tail}`,
  ].join('\n\n')

test('mergedListItems finds a Planning bullet joined mid-line to its neighbour', () => {
  const description = planningDescription(
    '- Contract: v1\n- Atomic-5: epic DAG.- Agent-friendly: needs-human',
  )
  const hits = mergedListItems(DEFAULT_CONFIG, description)
  assert.equal(hits.length, 1)
  assert.equal(hits[0].text, '- Agent-friendly:')
  assert.equal(description.split('\n')[hits[0].line - 1].startsWith('- Atomic-5:'), true)
})

test('mergedListItems finds a Planning bullet joined to its neighbour by a space', () => {
  const description = planningDescription('- Contract: v1 - Agent-friendly: needs-human')
  const hits = mergedListItems(DEFAULT_CONFIG, description)
  assert.deepEqual(
    hits.map(({ text }) => text),
    ['- Agent-friendly:'],
  )
  assert.equal(description.split('\n')[hits[0].line - 1].startsWith('- Contract:'), true)
  // An indented nested bullet with a known key is its own list item, not a merge.
  assert.deepEqual(
    mergedListItems(DEFAULT_CONFIG, planningDescription('- Contract: v1\n  - Dependencies: x')),
    [],
  )
})

test('BOS-1358 mergedListItems knows the Oversized-child Planning bullet', () => {
  // Space-joined merges are flagged for KNOWN keys only, so an unregistered key would slip through.
  assert.deepEqual(
    mergedListItems(
      DEFAULT_CONFIG,
      planningDescription(
        '- Contract: v1 - Oversized-child: twelve parts; split into three siblings',
      ),
    ).map(({ text }) => text),
    ['- Oversized-child:'],
  )
  assert.deepEqual(
    mergedListItems(
      DEFAULT_CONFIG,
      planningDescription(
        '- Contract: v1\n- Oversized-child: twelve parts; split into three siblings',
      ),
    ),
    [],
  )
})

test('mergedListItems reads a blockquote prefix as part of the first bullet', () => {
  assert.deepEqual(mergedListItems(DEFAULT_CONFIG, planningDescription('> - Contract: v1')), [])
  assert.deepEqual(mergedListItems(DEFAULT_CONFIG, planningDescription('> > - Contract: v1')), [])
  const hits = mergedListItems(
    DEFAULT_CONFIG,
    planningDescription('> - Contract: v1 - Agent-friendly: needs-human'),
  )
  assert.deepEqual(
    hits.map(({ text }) => text),
    ['- Agent-friendly:'],
  )
})

test('mergedListItems passes separate bullets, code spans and hyphenated prose', () => {
  for (const planning of [
    '- Contract: v1\n- Atomic-5: epic DAG.\n- Agent-friendly: needs-human',
    '- Contract: v1 with `- X: inside a span`',
    '* Contract: v1\n* Triage: single-ticket SUBSTANTIAL fallback — parent BOS-1326',
    '- Contract: v1\n\n```\n- A: b.- C: d\n```',
    '- Estimate: 3 — split: backend - Frontend: later',
  ]) {
    assert.deepEqual(mergedListItems(DEFAULT_CONFIG, planningDescription(planning)), [], planning)
  }
})

test('mergedListItems is scoped to ## Planning and stops at the verbatim section', () => {
  const merged = 'epic DAG.- Agent-friendly: needs-human'
  const inSummary = planningDescription('- Contract: v1').replace(
    '## Summary\n\nBody.',
    `## Summary\n\n- Atomic-5: ${merged}`,
  )
  assert.deepEqual(mergedListItems(DEFAULT_CONFIG, inSummary), [])
  const inNotes = planningDescription('- Contract: v1', {
    tail: `\n\n## Planning\n\n- Atomic-5: ${merged}`,
  })
  assert.deepEqual(mergedListItems(DEFAULT_CONFIG, inNotes), [])
})

test('mergedListItems is config-first like its siblings', () => {
  assert.throws(() => mergedListItems('## Planning', DEFAULT_CONFIG), /arguments look swapped/)
})

// BOS-1328: `zero-selection-filter` is judged per segment. A plain enumeration grep that selects
// zero files exits 1, so it cannot go green on nothing when its status is the command's.
test('zero-selection-filter spares a non-negated grep whose exit status is the command', () => {
  const blocks = (command) =>
    classifyCheckCommand(command, { kind: 'criterion' }).blocking.some(
      (finding) => finding.code === 'zero-selection-filter',
    )
  assert.equal(blocks("grep -rn scanFences skills-toolbox --include='*.mjs'"), false)
  assert.equal(blocks("grep -rn scanFences skills-toolbox --include='*.mjs' && echo ok"), false)
  // A negated search inverts exactly that status: zero files reads as success.
  assert.equal(blocks("! grep -rq needle . --include='*.md'"), true)
  // Test selectors keep today's behaviour.
  assert.equal(blocks('node --test --test-name-pattern foo x.test.mjs'), true)
  // Anything that hands the verdict to another command keeps the finding.
  assert.equal(blocks("grep -rn scanFences skills-toolbox --include='*.mjs' || true"), true)
  assert.equal(blocks("grep -rn scanFences skills-toolbox --include='*.mjs'; echo done"), true)
  const piped = classifyCheckCommand(
    "grep -rn scanFences skills-toolbox --include='*.mjs' | wc -l",
    {
      kind: 'criterion',
    },
  ).blocking.map((finding) => finding.code)
  // `wc -l` is a count assertion over the whole command, so only the pipeline finding remains —
  // and it still blocks: the tail's status is not the grep's.
  assert.ok(piped.includes('pipe-without-pipefail'), piped.join(','))
  const pipedPlain = classifyCheckCommand(
    "grep -rn scanFences skills-toolbox --include='*.mjs' | sort",
    { kind: 'criterion' },
  ).blocking.map((finding) => finding.code)
  assert.ok(pipedPlain.includes('zero-selection-filter'), pipedPlain.join(','))
  assert.ok(pipedPlain.includes('pipe-without-pipefail'), pipedPlain.join(','))
})

test('path-operand-absent carries the operand path for a caller with more context', () => {
  const tmp = mkdtempSync(join(tmpdir(), 'boss-skill-config-operand-'))
  try {
    mkdirSync(join(tmp, 'scripts'))
    const finding = classifyCheckCommand('node --test scripts/new.test.mjs', {
      cwd: tmp,
      env: process.env,
    }).advisory.find((f) => f.code === 'path-operand-absent')
    assert.equal(finding.path, 'scripts/new.test.mjs')
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
})

test('a team-only tracker block gets the default server, states and pipeline labels', () => {
  const dir = mkdtempSync(join(tmpdir(), 'skill-config-minimal-'))
  try {
    writeFileSync(
      join(dir, '.boss-skills.json'),
      JSON.stringify({
        trackerConfig: { linear: { team: 'Tech', states: { unplanned: 'Unplanned' } } },
      }),
    )
    const config = loadSkillConfig({ cwd: dir })
    const tc = trackerConfigFor(config)
    assert.equal(tc.mcpServer, 'linear')
    assert.deepEqual(tc.states, {
      unplanned: 'Unplanned',
      planned: 'Todo',
      inProgress: 'In Progress',
      inReview: 'In Review',
      done: 'Done',
    })
    assert.equal(labelName(config, 'agentBuild'), 'agent-build')
    assert.equal(labelName(config, 'epic'), 'epic')
    assert.equal(optionalLabelName(config, 'bug'), null, 'content labels stay literal')
    assert.equal(isConfiguredForPlanning(config), true)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('tracker defaults synthesize a team-less linear block that alone never configures a repo', () => {
  const dir = mkdtempSync(join(tmpdir(), 'skill-config-none-'))
  try {
    writeFileSync(join(dir, '.boss-skills.json'), '{}')
    const config = loadSkillConfig({ cwd: dir })
    const tc = trackerConfigFor(config)
    assert.equal(tc.mcpServer, 'linear')
    assert.equal('team' in tc, false, 'the synthesized block names no team')
    assert.deepEqual(tc.states, { ...DEFAULT_TRACKER_STATES })
    assert.deepEqual(tc.labels, { ...DEFAULT_PIPELINE_LABELS })
    // No second argument keeps the strict meaning: a configured team is required.
    assert.equal(isConfiguredForRepo(config), false)
    assert.equal(isConfiguredForPlanning(config), false)
    // A team the run resolved satisfies the requirement.
    assert.equal(isConfiguredForRepo(config, { team: 'T' }), true)
    assert.equal(isConfiguredForPlanning(config, { team: 'T' }), true)
    assert.equal(isConfiguredForPlanning(config, { team: '  ' }), false)
    assert.equal(
      DEFAULT_CONFIG.trackerConfig && Object.keys(DEFAULT_CONFIG.trackerConfig).length,
      0,
    )
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('withTrackerDefaults synthesizes only for a selected linear tracker that declares no block', () => {
  // Absent or empty trackerConfig both synthesize when linear is selected.
  for (const config of [
    { adapters: { tracker: 'linear' } },
    { adapters: { tracker: 'linear' }, trackerConfig: {} },
  ]) {
    const tc = withTrackerDefaults(config).trackerConfig.linear
    assert.equal(tc.mcpServer, 'linear')
    assert.equal(tc.team, undefined)
    assert.equal(tc.states.planned, 'Todo')
    assert.equal(tc.labels.agentBuild, 'agent-build')
  }
  // A non-Linear selected tracker gets nothing — it keeps the explicit-config requirement.
  const jira = withTrackerDefaults({ adapters: { tracker: 'jira' }, trackerConfig: {} })
  assert.deepEqual(jira.trackerConfig, {})
  assert.equal(
    trackerConfigFor(mergeConfig(DEFAULT_CONFIG, { adapters: { tracker: 'jira' } })),
    null,
  )
  // A declared linear block is filled, never replaced, and other blocks are untouched.
  const declared = withTrackerDefaults({
    adapters: { tracker: 'linear' },
    trackerConfig: { linear: { team: 'T', mcpServer: 'acme' }, other: { mcpServer: 'o' } },
  }).trackerConfig
  assert.deepEqual(Object.keys(declared).sort(), ['linear', 'other'])
  assert.equal(declared.linear.team, 'T')
  assert.equal(declared.linear.mcpServer, 'acme')
  // A malformed trackerConfig is left for validateConfig to reject.
  const malformed = { adapters: { tracker: 'linear' }, trackerConfig: [] }
  assert.equal(withTrackerDefaults(malformed), malformed)
})

// --- resolveTrackerTeam (BOS-1393) -------------------------------------------
// Pure classifier over facts the agent gathered: its preflight, the raw MCP list_teams result,
// and any --team. Precedence: configured team > --team > the single visible team.

const ZERO_CONFIG = withTrackerDefaults(mergeConfig(DEFAULT_CONFIG, {}))
const PINNED_CONFIG = withTrackerDefaults(
  mergeConfig(DEFAULT_CONFIG, { trackerConfig: { linear: { team: 'Pinned', teamKey: 'PIN' } } }),
)
const OK = { ok: true, status: 'ok', mcpServer: 'linear', resolvedServer: 'linear', message: '' }
const listed = (teams, hasNextPage = false) => ({ teams, hasNextPage })
const ALPHA = { id: 'id-alpha', name: 'Alpha', icon: 'Rocket', visibility: 'public' }
const BETA = { id: 'id-beta', name: 'Beta' }

test('resolveTrackerTeam: a failed preflight is no-tracker-mcp, passing its message through', () => {
  for (const status of ['absent', 'unreachable']) {
    const message = `tracker MCP server "linear" is ${status} for claude`
    const r = resolveTrackerTeam(ZERO_CONFIG, {
      preflight: { ok: false, status, mcpServer: 'linear', message },
      visibleTeams: listed([ALPHA]),
    })
    assert.equal(r.configured, false)
    assert.equal(r.reason, 'no-tracker-mcp')
    assert.equal(r.message, message)
    assert.equal(r.team, null)
  }
  // No preflight at all, or one with no message, still yields a one-line message naming the server.
  for (const preflight of [undefined, { ok: false, message: '' }]) {
    const r = resolveTrackerTeam(ZERO_CONFIG, { preflight })
    assert.equal(r.reason, 'no-tracker-mcp')
    assert.match(r.message, /"linear"/)
    assert.equal(r.message.includes('\n'), false)
  }
})

test('resolveTrackerTeam: one visible team is detected, with that team', () => {
  const r = resolveTrackerTeam(ZERO_CONFIG, { preflight: OK, visibleTeams: listed([ALPHA]) })
  assert.deepEqual(r, {
    configured: true,
    team: 'Alpha',
    teamKey: null,
    source: 'detected',
    reason: null,
    message: '',
  })
  assert.equal(isConfiguredForPlanning(ZERO_CONFIG, { team: r.team }), true)
  // A key is kept when the source supplies one (the Go GraphQL path does; MCP does not).
  const keyed = resolveTrackerTeam(ZERO_CONFIG, {
    preflight: OK,
    visibleTeams: [{ id: 'x', name: 'Alpha', key: 'ALP' }],
  })
  assert.equal(keyed.teamKey, 'ALP')
})

test('resolveTrackerTeam: several teams are ambiguous with the actionable --team message', () => {
  const r = resolveTrackerTeam(ZERO_CONFIG, { preflight: OK, visibleTeams: listed([ALPHA, BETA]) })
  assert.equal(r.configured, false)
  assert.equal(r.reason, 'ambiguous')
  assert.equal(
    r.message,
    '2 Linear teams are visible (Alpha, Beta) — pass --team <name> or set trackerConfig.linear.team in .boss-skills.json',
  )
  assert.ok(r.message.includes('--team') && r.message.includes('trackerConfig.linear.team'))
  assert.equal(isConfiguredForPlanning(ZERO_CONFIG, { team: r.team }), false)
})

test('resolveTrackerTeam: hasNextPage with one listed team is ambiguous, never a single team', () => {
  const r = resolveTrackerTeam(ZERO_CONFIG, { preflight: OK, visibleTeams: listed([ALPHA], true) })
  assert.equal(r.configured, false)
  assert.equal(r.reason, 'ambiguous')
  assert.match(r.message, /^more than 1 Linear teams are visible \(Alpha, …\)/)
})

test('resolveTrackerTeam: zero visible teams is no-teams', () => {
  const r = resolveTrackerTeam(ZERO_CONFIG, { preflight: OK, visibleTeams: listed([]) })
  assert.equal(r.configured, false)
  assert.equal(r.reason, 'no-teams')
  assert.ok(r.message.length > 0)
})

test('resolveTrackerTeam: an explicit configured team always wins', () => {
  const cases = [
    { preflight: OK, visibleTeams: listed([ALPHA]) },
    { preflight: OK, visibleTeams: listed([ALPHA, BETA], true) },
    { preflight: { ok: false, status: 'absent', message: 'absent' } },
    { preflight: OK, visibleTeams: null },
  ]
  for (const facts of cases) {
    const r = resolveTrackerTeam(PINNED_CONFIG, facts)
    assert.equal(r.configured, true)
    assert.equal(r.team, 'Pinned')
    assert.equal(r.teamKey, 'PIN')
    assert.equal(r.source, 'config')
    assert.equal(r.message, '')
  }
  const flagged = resolveTrackerTeam(PINNED_CONFIG, {
    preflight: OK,
    visibleTeams: listed([ALPHA]),
    teamFlag: 'Alpha',
  })
  assert.equal(flagged.team, 'Pinned')
  assert.equal(flagged.source, 'config')
  assert.equal(flagged.message, '--team Alpha ignored: trackerConfig.linear.team is Pinned')
  // A flag naming the configured team (any case, or its key) is not a conflict.
  for (const teamFlag of ['pinned', 'PIN']) {
    assert.equal(resolveTrackerTeam(PINNED_CONFIG, { preflight: OK, teamFlag }).message, '')
  }
})

test('resolveTrackerTeam: a flag matching a listed name (any case) or id resolves to the canonical name', () => {
  for (const teamFlag of ['beta', 'BETA', ' Beta ', 'id-beta']) {
    const r = resolveTrackerTeam(ZERO_CONFIG, {
      preflight: OK,
      visibleTeams: listed([ALPHA, BETA]),
      teamFlag,
    })
    assert.equal(r.configured, true, teamFlag)
    assert.equal(r.team, 'Beta')
    assert.equal(r.source, 'flag')
  }
  const byKey = resolveTrackerTeam(ZERO_CONFIG, {
    preflight: OK,
    visibleTeams: [{ id: '1', name: 'Gamma', key: 'GAM' }],
    teamFlag: 'gam',
  })
  assert.equal(byKey.team, 'Gamma')
  assert.equal(byKey.teamKey, 'GAM')
  // The id match is exact, never case-folded.
  assert.equal(
    resolveTrackerTeam(ZERO_CONFIG, { preflight: OK, visibleTeams: [BETA], teamFlag: 'ID-BETA' })
      .reason,
    'unknown-team',
  )
})

test('resolveTrackerTeam: a flag absent from a listing with more pages is trusted, not unknown-team', () => {
  const r = resolveTrackerTeam(ZERO_CONFIG, {
    preflight: OK,
    visibleTeams: listed([ALPHA, BETA], true),
    teamFlag: 'Zeta',
  })
  assert.equal(r.configured, true)
  assert.equal(r.team, 'Zeta')
  assert.equal(r.source, 'flag')
})

test('resolveTrackerTeam: a flag matching no listed team is unknown-team, naming the visible teams', () => {
  const r = resolveTrackerTeam(ZERO_CONFIG, {
    preflight: OK,
    visibleTeams: listed([ALPHA, BETA]),
    teamFlag: 'Delta',
  })
  assert.equal(r.configured, false)
  assert.equal(r.reason, 'unknown-team')
  assert.match(r.message, /Delta/)
  assert.match(r.message, /Alpha, Beta/)
})

test('resolveTrackerTeam: a flag with no listing is trusted as given; no flag is teams-unlisted', () => {
  const flagged = resolveTrackerTeam(ZERO_CONFIG, {
    preflight: OK,
    visibleTeams: null,
    teamFlag: 'Example',
  })
  assert.deepEqual(
    [flagged.configured, flagged.team, flagged.source, flagged.teamKey],
    [true, 'Example', 'flag', null],
  )
  for (const teamFlag of [undefined, '', '   ']) {
    const r = resolveTrackerTeam(ZERO_CONFIG, { preflight: OK, visibleTeams: null, teamFlag })
    assert.equal(r.configured, false)
    assert.equal(r.reason, 'teams-unlisted')
    assert.ok(r.message.includes('--team') && r.message.includes('trackerConfig.linear.team'))
  }
})

test('resolveTrackerTeam: the raw {teams, hasNextPage} shape and a bare array give the same verdicts', () => {
  for (const [teams, teamFlag] of [
    [[ALPHA], undefined],
    [[ALPHA, BETA], undefined],
    [[], undefined],
    [[ALPHA, BETA], 'alpha'],
    [[ALPHA], 'zeta'],
  ]) {
    assert.deepEqual(
      resolveTrackerTeam(ZERO_CONFIG, { preflight: OK, visibleTeams: listed(teams), teamFlag }),
      resolveTrackerTeam(ZERO_CONFIG, { preflight: OK, visibleTeams: teams, teamFlag }),
    )
  }
})

test('resolveTrackerTeam: malformed listings are "not listed" and never throw', () => {
  for (const visibleTeams of [
    'Alpha',
    { teams: 'x' },
    [{ id: '1' }],
    [ALPHA, { id: '2', name: '' }],
    [null],
    42,
    { nodes: [ALPHA] },
  ]) {
    assert.equal(
      resolveTrackerTeam(ZERO_CONFIG, { preflight: OK, visibleTeams }).reason,
      'teams-unlisted',
      JSON.stringify(visibleTeams),
    )
    assert.equal(
      resolveTrackerTeam(ZERO_CONFIG, { preflight: OK, visibleTeams, teamFlag: 'X' }).source,
      'flag',
    )
  }
  assert.doesNotThrow(() => resolveTrackerTeam(undefined))
  assert.doesNotThrow(() => resolveTrackerTeam(ZERO_CONFIG))
  // Every unconfigured verdict carries a non-empty, one-line message.
  const r = resolveTrackerTeam(undefined, {})
  assert.equal(r.configured, false)
  assert.ok(r.message.length > 0 && !r.message.includes('\n'))
})

test('explicit tracker names override the defaults per key', () => {
  const config = withTrackerDefaults({
    trackerConfig: {
      linear: { team: 'T', mcpServer: 'acme-linear', labels: { agentBuild: 'Agent Friendly' } },
    },
  })
  const tc = config.trackerConfig.linear
  assert.equal(tc.mcpServer, 'acme-linear')
  assert.equal(tc.labels.agentBuild, 'Agent Friendly')
  assert.equal(tc.labels.needsHuman, 'needs-human')
})

test('completionMergeAllowed is retired; a stray completionDefaults key still validates and loads', async () => {
  const module = await import('./skill-config.mjs')
  assert.equal('completionMergeAllowed' in module, false)
  const { nested, cleanup } = scratchRepo('{"completionDefaults":{"allowMerge":true}}')
  try {
    const cfg = loadSkillConfig({ cwd: nested })
    assert.equal(cfg.adapters.tracker, 'linear')
    assert.doesNotThrow(() => validateConfig(cfg, '.boss-skills.json'))
  } finally {
    cleanup()
  }
})

test('the done tracker state defaults to Done and stays overridable', () => {
  assert.equal(DEFAULT_TRACKER_STATES.done, 'Done')
  assert.equal(
    withTrackerDefaults({ trackerConfig: { linear: {} } }).trackerConfig.linear.states.done,
    'Done',
  )
  assert.equal(
    withTrackerDefaults({ trackerConfig: { linear: { states: { done: 'Closed' } } } }).trackerConfig
      .linear.states.done,
    'Closed',
  )
})

test('DEFAULT_PIPELINE_LABELS carries exactly the five pipeline roles, build intake as agentBuild (no alias)', () => {
  // BOS-1379 renamed the build-intake role with no alias: the key set is pinned whole so a
  // second spelling of that role cannot creep back in beside it.
  assert.deepEqual(Object.keys(DEFAULT_PIPELINE_LABELS).sort(), [
    'agentBuild',
    'agentPlan',
    'agentQuestion',
    'epic',
    'needsHuman',
  ])
  assert.equal(DEFAULT_PIPELINE_LABELS.agentBuild, 'agent-build')
  // The default reaches a tracker block that names no labels of its own.
  assert.equal(
    withTrackerDefaults({ trackerConfig: { linear: { team: 'T' } } }).trackerConfig.linear.labels
      .agentBuild,
    'agent-build',
  )
})

test('verifyAlwaysHumanPaths defaults to an empty list and returns configured globs', () => {
  assert.deepEqual(verifyAlwaysHumanPaths(DEFAULT_CONFIG), [])
  assert.deepEqual(verifyAlwaysHumanPaths(undefined), [])
  assert.deepEqual(verifyAlwaysHumanPaths({ verifyDefaults: {} }), [])
  assert.deepEqual(verifyAlwaysHumanPaths({ verifyDefaults: { alwaysHumanPaths: 'x' } }), [])
  const config = { verifyDefaults: { alwaysHumanPaths: ['migrations/**', 'billing/*.go'] } }
  const paths = verifyAlwaysHumanPaths(config)
  assert.deepEqual(paths, ['migrations/**', 'billing/*.go'])
  paths.push('mutated')
  assert.equal(config.verifyDefaults.alwaysHumanPaths.length, 2)
})

test('validateConfig rejects a malformed verifyDefaults block', () => {
  assert.doesNotThrow(() => validateConfig({ ...DEFAULT_CONFIG }, 'test'))
  assert.doesNotThrow(() =>
    validateConfig({ ...DEFAULT_CONFIG, verifyDefaults: { alwaysHumanPaths: [] } }, 'test'),
  )
  assert.doesNotThrow(() =>
    validateConfig({ ...DEFAULT_CONFIG, verifyDefaults: { alwaysHumanPaths: ['a/**'] } }, 'test'),
  )
  for (const verifyDefaults of [
    [],
    null,
    'x',
    { alwaysHumanPaths: 'a/**' },
    { alwaysHumanPaths: { glob: 'a' } },
    { alwaysHumanPaths: ['a/**', 3] },
    { alwaysHumanPaths: [''] },
  ]) {
    assert.throws(
      () => validateConfig({ ...DEFAULT_CONFIG, verifyDefaults }, 'test'),
      /verifyDefaults/,
      JSON.stringify(verifyDefaults),
    )
  }
})

test('retro config validates numbers, aliases and audit options', async () => {
  const { retroConfig, validateConfig, DEFAULT_CONFIG, mergeConfig } =
    await import('./skill-config.mjs')
  const valid = {
    maxIssues: 0,
    staleDays: 0,
    minRuns: 2,
    gateThreshold: 5,
    pathAliases: { 'old/': 'new/' },
    guidanceAudit: { maxLines: 99 },
  }
  const config = mergeConfig(DEFAULT_CONFIG, { retro: valid })
  assert.doesNotThrow(() => validateConfig(config, 'test'))
  const copy = retroConfig(config)
  copy.pathAliases['old/'] = 'other'
  copy.guidanceAudit.maxLines = 1
  assert.equal(config.retro.pathAliases['old/'], 'new/')
  assert.equal(config.retro.guidanceAudit.maxLines, 99)
  for (const retro of [
    null,
    [],
    { wat: 1 },
    { minRuns: 0 },
    { staleDays: -1 },
    { maxIssues: 1.5 },
    { gateThreshold: '5' },
    { pathAliases: { a: '' } },
    { guidanceAudit: [] },
  ])
    assert.throws(() => validateConfig({ ...DEFAULT_CONFIG, retro }, 'test'), /retro/)
  assert.deepEqual(
    retroConfig({ retro: { maxIssues: 'bad', guidanceAudit: [], pathAliases: null } }),
    {
      maxIssues: null,
      staleDays: null,
      minRuns: null,
      gateThreshold: null,
      pathAliases: {},
      guidanceAudit: null,
    },
  )
})

test('validateLocalVerification accepts command summaries, phases, arrows and gaps', () => {
  const body =
    '## local verification   \n- `node --test unit.test.mjs` → pass 1, fail 0\n- after rebase: `make test-affected` -> ok 214\n- gap: selector matched no tests; ran closest covering unit test\n## Other\n- ignored'
  const result = validateLocalVerification(DEFAULT_CONFIG, body)
  assert.equal(result.ok, true)
  assert.deepEqual(result.entries, [
    { phase: null, command: 'node --test unit.test.mjs', result: 'pass 1, fail 0' },
    { phase: 'after rebase', command: 'make test-affected', result: 'ok 214' },
  ])
  assert.deepEqual(result.gaps, ['selector matched no tests; ran closest covering unit test'])
  assert.equal(
    validateLocalVerification(DEFAULT_CONFIG, '## Local verification\n- gap: no reliable mapping')
      .ok,
    true,
  )
})

test('validateLocalVerification reports missing and empty sections outside fences', () => {
  for (const body of ['', '```md\n## Local verification\n- `test` → pass\n```']) {
    assert.equal(
      validateLocalVerification(DEFAULT_CONFIG, body).missingEvidence[0].reason,
      'missing-section',
    )
  }
  assert.equal(
    validateLocalVerification(DEFAULT_CONFIG, '## Local verification\nNo commands yet')
      .missingEvidence[0].reason,
    'empty-section',
  )
})

test('validateLocalVerification identifies malformed bullets without losing valid evidence', () => {
  for (const [bullet, reason] of [
    ['make test → passed', 'undelimited-command'],
    ['`` → passed', 'empty-command'],
    ['`make test` → ', 'empty-result'],
    ['gap: ', 'empty-gap'],
  ]) {
    const result = validateLocalVerification(
      DEFAULT_CONFIG,
      '## Local verification\n- `unit-test` → pass 1\n- ' + bullet,
    )
    assert.equal(result.ok, false)
    assert.equal(result.entries.length, 1)
    assert.equal(result.missingEvidence.length, 1)
    assert.equal(result.missingEvidence[0].reason, reason)
    assert.ok(result.missingEvidence[0].remedy)
  }
})

test('validateLocalVerification enforces config-first arguments', () => {
  assert.throws(
    () => validateLocalVerification('body', DEFAULT_CONFIG),
    /skill-config.*validateLocalVerification\(config, description\)/,
  )
})

test('validateLocalVerification rejects the publish template pasted unfilled', () => {
  // Exact copy of the `## Local verification` block in boss-build references/publish.md.
  const template = [
    '## Local verification',
    '- `<local command>` → <the command’s final summary line>',
    '- after rebase: `<covering command>` → <final summary line>',
    '- gap: <what could not be selected reliably and why>',
  ].join('\n')
  const result = validateLocalVerification(DEFAULT_CONFIG, template)
  assert.equal(result.ok, false)
  assert.deepEqual(result.entries, [])
  assert.deepEqual(result.gaps, [])
  assert.deepEqual(
    result.missingEvidence.map((f) => f.reason),
    ['placeholder', 'placeholder', 'placeholder'],
  )
  assert.ok(result.missingEvidence.every((f) => f.remedy))
  // Each field is checked on its own: one filled field does not excuse a placeholder beside it.
  for (const bullet of ['`make test` → <final summary line>', '`<covering command>` → ok 3']) {
    const one = validateLocalVerification(DEFAULT_CONFIG, '## Local verification\n- ' + bullet)
    assert.equal(one.missingEvidence[0]?.reason, 'placeholder')
  }
  // Angle brackets inside real output are not a placeholder.
  assert.equal(
    validateLocalVerification(
      DEFAULT_CONFIG,
      '## Local verification\n- `make test` → <3 skipped> ok 5',
    ).ok,
    true,
  )
})

test('validateLocalVerification ends the section at a parent # heading', () => {
  const result = validateLocalVerification(
    DEFAULT_CONFIG,
    '## Local verification\n- `make test` → ok 5\n# Appendix\n- not a verification entry\n- gap: nor this',
  )
  assert.equal(result.ok, true)
  assert.deepEqual(result.entries, [{ phase: null, command: 'make test', result: 'ok 5' }])
  assert.deepEqual(result.gaps, [])
  assert.deepEqual(result.missingEvidence, [])
})

test('validateLocalVerification matches the PR-body section parser on duplicate and colon headings', () => {
  const duplicate = validateLocalVerification(
    DEFAULT_CONFIG,
    '## Local verification\n- `make test` → ok 5\n## Notes\n## Local Verification\n- `make lint` → 0 issues',
  )
  assert.equal(duplicate.ok, false)
  assert.deepEqual(
    duplicate.missingEvidence.map((f) => f.reason),
    ['duplicate-section'],
  )
  assert.ok(duplicate.missingEvidence[0].remedy)
  // `sections()` keys `## Local verification:` as a different section, so it is not this one.
  assert.equal(
    validateLocalVerification(DEFAULT_CONFIG, '## Local verification:\n- `make test` → ok 5')
      .missingEvidence[0].reason,
    'missing-section',
  )
  assert.equal(sections('## Local verification:\n- x').has('local verification'), false)
})

test('validateLocalVerification attaches indented and wrapped lines to the entry above', () => {
  const result = validateLocalVerification(
    DEFAULT_CONFIG,
    [
      '## Local verification',
      '- `make test` → ok 5',
      '  - selected by the affected-test selector',
      '- `make lint` →',
      '  0 issues',
      '- gap: no selector for docs',
      '  so the closest covering test ran',
      '',
      'Trailing prose after a blank line is not an entry.',
    ].join('\n'),
  )
  assert.deepEqual(result.missingEvidence, [])
  assert.equal(result.ok, true)
  assert.deepEqual(result.entries, [
    { phase: null, command: 'make test', result: 'ok 5 selected by the affected-test selector' },
    { phase: null, command: 'make lint', result: '0 issues' },
  ])
  assert.deepEqual(result.gaps, ['no selector for docs so the closest covering test ran'])
  // A blank line ends the entry, so a result separated from its bullet is still missing.
  assert.equal(
    validateLocalVerification(
      DEFAULT_CONFIG,
      '## Local verification\n- `make lint` →\n\n  0 issues',
    ).missingEvidence[0].reason,
    'empty-result',
  )
})

test('validateLocalVerification keeps same-indent siblings as separate entries', () => {
  // A uniformly indented list is still a FLAT list. Treating any indent as nesting folded every
  // sibling into the first entry, so a later sibling's empty result or unfilled placeholder was
  // swallowed into the first entry's result and the section passed.
  for (const pad of [' ', '  ', '   ', '\t']) {
    const emptyResult = validateLocalVerification(
      DEFAULT_CONFIG,
      `## Local verification\n${pad}- \`make lint\` → ok\n${pad}- \`make test\` → `,
    )
    assert.equal(emptyResult.ok, false, `indent ${JSON.stringify(pad)}`)
    assert.deepEqual(emptyResult.entries, [{ phase: null, command: 'make lint', result: 'ok' }])
    assert.deepEqual(
      emptyResult.missingEvidence.map((finding) => [finding.reason, finding.text]),
      [['empty-result', '`make test` →']],
    )
    const placeholder = validateLocalVerification(
      DEFAULT_CONFIG,
      `## Local verification\n${pad}- \`make test\` → ok 5\n${pad}- \`make lint\` →\n${pad}- gap: <…>`,
    )
    assert.equal(placeholder.ok, false, `indent ${JSON.stringify(pad)}`)
    assert.deepEqual(
      placeholder.missingEvidence.map((finding) => finding.reason),
      ['empty-result', 'placeholder'],
    )
  }
  // A sub-bullet indented DEEPER than its opener still continues it, even when the opener itself
  // is indented; a shallower item after it is a new entry again.
  const nested = validateLocalVerification(
    DEFAULT_CONFIG,
    '## Local verification\n  - `make test` →\n    - ok 5\n  - `make lint` → 0 issues',
  )
  assert.deepEqual(nested.missingEvidence, [])
  assert.deepEqual(nested.entries, [
    { phase: null, command: 'make test', result: 'ok 5' },
    { phase: null, command: 'make lint', result: '0 issues' },
  ])
})

test('parseAcceptanceCriteria keeps same-indent checkbox siblings as separate criteria', () => {
  // The shared grouping rule: an equally indented checkbox is a sibling, so an OPEN criterion can
  // no longer hide inside a ticked one's text.
  for (const pad of [' ', '  ', '   ']) {
    const criteria = parseAcceptanceCriteria(
      DEFAULT_CONFIG,
      planBody(`${pad}- [x] first\n${pad}- [ ] second`),
    )
    assert.deepEqual(
      criteria.map((c) => [c.text, c.checked]),
      [
        ['first', true],
        ['second', false],
      ],
      `indent ${JSON.stringify(pad)}`,
    )
  }
  const nested = parseAcceptanceCriteria(
    DEFAULT_CONFIG,
    planBody(`  - [x] first\n    - [x] detail\n  - [ ] second`),
  )
  assert.deepEqual(
    nested.map((c) => c.text),
    ['first [x] detail', 'second'],
  )
})
