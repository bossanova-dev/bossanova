import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, mkdirSync, rmSync } from 'node:fs'
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
  lensesForFile,
  detectChangeTypes,
  skillForLens,
  command,
  moduleTestCommand,
  manifestPath,
  isHeadless,
  adapterFor,
  planContractVersion,
  planSections,
  requiredPlanSections,
  validatePlanDescription,
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

test('DEFAULT_CONFIG carries the three current lenses', () => {
  const ids = DEFAULT_CONFIG.lensMap.map((r) => r.id)
  assert.deepEqual(ids, ['go', 'tui', 'web'])
  const byId = Object.fromEntries(DEFAULT_CONFIG.lensMap.map((r) => [r.id, r]))
  assert.equal(byId.go.skill, 'golang-pro')
  assert.equal(byId.tui.skill, 'tui-design')
  assert.equal(byId.web.skill, 'impeccable')
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
    assert.equal(cfg.test.manifestPath, 'docs/testing/test-command-manifest.md')
    assert.equal(cfg.adapters.tracker, 'linear')
  } finally {
    cleanup()
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
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, commands: { ...DEFAULT_CONFIG.commands, testModule: null } },
        'test',
      ),
    /skill-config:.*commands\.testModule must be a non-empty string/,
  )
  assert.throws(
    () =>
      validateConfig(
        { ...DEFAULT_CONFIG, commands: { ...DEFAULT_CONFIG.commands, build: '' } },
        'test',
      ),
    /skill-config:.*commands\.build must be a non-empty string/,
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

// --- Task 4: accessors ----------------------------------------------------

test('lensesForFile matches by glob', () => {
  assert.deepEqual(lensesForFile(DEFAULT_CONFIG, 'services/boss/internal/x.go'), ['go', 'tui'])
  assert.deepEqual(lensesForFile(DEFAULT_CONFIG, 'services/web/src/App.tsx'), ['web'])
  assert.deepEqual(lensesForFile(DEFAULT_CONFIG, 'docs/foo.md'), [])
})

test('detectChangeTypes preserves the legacy {go,tui,web} shape (one-arg)', () => {
  assert.deepEqual(detectChangeTypes(['services/boss/internal/views/attach.go']), {
    go: true,
    tui: true,
    web: false,
  })
  assert.deepEqual(detectChangeTypes(['docs/foo.md', 'CONCEPTS.md']), {
    go: false,
    tui: false,
    web: false,
  })
})

test('skillForLens maps ids to review skills', () => {
  assert.equal(skillForLens(DEFAULT_CONFIG, 'go'), 'golang-pro')
  assert.equal(skillForLens(DEFAULT_CONFIG, 'web'), 'impeccable')
  assert.equal(skillForLens(DEFAULT_CONFIG, 'nope'), null)
})

test('command and moduleTestCommand return the current targets', () => {
  assert.equal(command(DEFAULT_CONFIG, 'testSmoke'), 'make test-smoke')
  assert.equal(command(DEFAULT_CONFIG, 'lint'), 'make lint')
  assert.equal(moduleTestCommand(DEFAULT_CONFIG, 'bossd'), 'make test-bossd')
})

test('manifestPath returns the current manifest', () => {
  assert.equal(manifestPath(DEFAULT_CONFIG), 'docs/testing/test-command-manifest.md')
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
      '## Acceptance criteria',
      '## Required proof',
      '## Why this needs a human',
      '## Open Questions',
      '## Planning',
      '## Original notes',
    ],
  )
})

test('requiredPlanSections excludes the two conditional sections', () => {
  const req = requiredPlanSections(DEFAULT_CONFIG)
  assert.ok(!req.includes('## Why this needs a human'))
  assert.ok(!req.includes('## Open Questions'))
  assert.ok(req.includes('## Summary') && req.includes('## Original notes'))
})

// Build a plan description whose ## Planning body carries `planningLine`, laid out in the real
// emitted order (## Planning before the terminal ## Original notes).
const planDesc = (planningLine) =>
  requiredPlanSections(DEFAULT_CONFIG)
    .join('\n\nx\n\n')
    .replace('## Planning\n\nx', `## Planning\n\n${planningLine}`)

test('validatePlanDescription accepts a well-formed v1 description', () => {
  const r = validatePlanDescription(DEFAULT_CONFIG, planDesc('- Contract: v1'))
  assert.deepEqual(r, { ok: true, version: 1, missing: [], unsupportedVersion: false })
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

// --- Task 5: committed .boss-skills.json parity ---------------------------

// The repo root is one level up from skills-toolbox/skill-config.test.mjs.
const REPO_ROOT = fileURLToPath(new URL('..', import.meta.url))

test('the committed .boss-skills.json reproduces the current hard-coded values', () => {
  const cfg = loadSkillConfig({ cwd: REPO_ROOT })
  // lens map parity with skills-toolbox/bs-review-detect.mjs + boss-review SKILL
  assert.deepEqual(
    cfg.lensMap.map((r) => [r.id, r.skill]),
    [
      ['go', 'golang-pro'],
      ['tui', 'tui-design'],
      ['web', 'impeccable'],
    ],
  )
  // manifest + commands parity with docs/testing/test-command-manifest.md
  assert.equal(cfg.test.manifestPath, 'docs/testing/test-command-manifest.md')
  assert.equal(cfg.commands.testSmoke, 'make test-smoke')
  assert.equal(cfg.commands.testAffected, 'make test-affected')
  assert.equal(moduleTestCommand(cfg, 'boss'), 'make test-boss')
  // env parity with boss-implement + boss-plan headless detection
  assert.equal(isHeadless(cfg, { BOSS_CRON: 'true' }, { isTTY: true }), true)
  assert.equal(isHeadless(cfg, { BS_HEADLESS: '1' }, { isTTY: true }), true)
  // adapter selection parity
  assert.equal(adapterFor(cfg, 'tracker'), 'linear')
  assert.equal(adapterFor(cfg, 'publish'), 'proof')
  assert.equal(adapterFor(cfg, 'sessionRunner'), 'bossd')
})
