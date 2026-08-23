#!/usr/bin/env node

// Fixture-driven. Nothing here reads the live skill tree: a test that asserted against
// real prose would go red every time an unrelated skill was edited, and would stop
// proving the gate's MECHANISM. Each fixture repo is a temp dir carrying only the two
// or three files the assertion needs.

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import {
  assertTaxonomySplit,
  buildExportIndex,
  buildVerbIndex,
  checkRoleCitations,
  checkSkillSymbols,
  checkVerbCitations,
  discoverToolboxOwners,
  extractEnumeratedRun,
  extractExportCitations,
  extractRoleCitations,
  extractVerbCitations,
  findSkillFiles,
  parseDispatchVerbs,
  parseExports,
  resolveRoleKeys,
  TOOLBOX_OWNING_SKILLS,
} from './check-skill-symbols.mjs'

// A deliberately NON-`linear` adapter: the gate must reach the role tables through
// adapterFor/trackerConfigFor, never through a hard-coded `linear` branch.
const FIXTURE_CONFIG = {
  adapters: { tracker: 'fixture-tracker' },
  trackerConfig: {
    'fixture-tracker': {
      mcpServer: 'fixture',
      team: 'Fixture',
      labels: { agentFriendly: 'agent-friendly', epic: 'epic', bug: 'bug' },
      states: { planned: 'Todo', inProgress: 'In Progress' },
      githubLabels: { proofInvalid: 'proof-invalid' },
    },
  },
}

const roleKeys = () => resolveRoleKeys(FIXTURE_CONFIG)

const tempRoots = []

function makeRepo(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'check-skill-symbols-'))
  tempRoots.push(root)
  for (const [relative, contents] of Object.entries(files)) {
    const full = path.join(root, relative)
    fs.mkdirSync(path.dirname(full), { recursive: true })
    fs.writeFileSync(full, contents)
  }
  return root
}

process.on('exit', () => {
  for (const root of tempRoots) fs.rmSync(root, { recursive: true, force: true })
})

// ---------------------------------------------------------------------------
// Check 1 — tracker role citations
// ---------------------------------------------------------------------------

test('form A: a configured role passes and an unconfigured one is named with its line', () => {
  const contents = [
    "ok: `labelName(config, 'epic')`",
    '',
    "bad: `stateName(config, 'shipped')`",
  ].join('\n')
  const citations = extractRoleCitations(contents)
  assert.deepEqual(citations, [
    { line: 1, fn: 'labelName', role: 'epic', form: 'A' },
    { line: 3, fn: 'stateName', role: 'shipped', form: 'A' },
  ])

  const findings = checkRoleCitations(citations, roleKeys())
  assert.equal(findings.length, 1)
  assert.equal(findings[0].line, 3)
  assert.equal(findings[0].kind, 'role-unconfigured')
  assert.match(
    findings[0].detail,
    /'shipped' is\s+not\s+a\s+configured\s+trackerConfig\.states\s+key/,
  )
})

test('optionalLabelName allows taxonomy and absent labels but rejects pipeline roles', () => {
  const contents = [
    "taxonomy: `optionalLabelName(config, 'docs')`",
    "mapped taxonomy: `optionalLabelName(config, 'bug')`",
    "absent: `optionalLabelName(config, 'improvement')`",
    "pipeline: `optionalLabelName(config, 'agentFriendly')`",
  ].join('\n')
  const citations = extractRoleCitations(contents)
  assert.deepEqual(citations, [
    { line: 1, fn: 'optionalLabelName', role: 'docs', form: 'A' },
    { line: 2, fn: 'optionalLabelName', role: 'bug', form: 'A' },
    { line: 3, fn: 'optionalLabelName', role: 'improvement', form: 'A' },
    { line: 4, fn: 'optionalLabelName', role: 'agentFriendly', form: 'A' },
  ])

  const findings = checkRoleCitations(citations, roleKeys())
  assert.equal(findings.length, 1)
  assert.equal(findings[0].line, 4)
  assert.equal(findings[0].kind, 'role-optional-pipeline')
  assert.match(findings[0].detail, /pipeline\s+roles\s+must\s+resolve\s+through\s+labelName\(\)/)
})

test('form B: every role in the enumerated run is cited, and the prose tail is not', () => {
  // The defect shape from boss-plan/SKILL.md: a `<role>` placeholder whose real roles
  // follow as a comma-separated inline-code run, trailed by PROSE that also carries
  // inline-code tokens. Swallowing the tail would report the display names as roles.
  const contents =
    "roles resolve through `labelName(config, '<role>')`, whose keys are camelCase: " +
    '`agentFriendly`, `epic`, `shipped` — the display names they resolve to are ' +
    '`agent-friendly`, `epic` and so on.'
  const citations = extractRoleCitations(contents)
  assert.deepEqual(
    citations.map((c) => c.role),
    ['agentFriendly', 'epic', 'shipped'],
  )
  assert.ok(citations.every((c) => c.form === 'B' && c.line === 1))

  const findings = checkRoleCitations(citations, roleKeys())
  assert.equal(findings.length, 1)
  assert.match(
    findings[0].detail,
    /'shipped' is\s+not\s+a\s+configured\s+trackerConfig\.labels\s+key/,
  )
  assert.match(findings[0].detail, /form\s+B/)
})

test('form B: a placeholder with no enumerated run yields no citations', () => {
  assert.deepEqual(extractRoleCitations("call `labelName(config, '<role>')` to resolve it."), [])
  assert.deepEqual(extractEnumeratedRun(' with no colon at all'), [])
  // The run must start on the SAME line as the placeholder.
  assert.deepEqual(extractEnumeratedRun(':\n`agentFriendly`'), [])
})

test('direction 2: a content-taxonomy label is rejected even when the config defines the key', () => {
  // `bug` IS a configured label role in the fixture (as it is in the live repo), yet
  // routing it through labelName() is still wrong: it is applied literally.
  const citations = extractRoleCitations("`labelName(config, 'bug')`")
  const findings = checkRoleCitations(citations, {
    ...roleKeys(),
    labels: new Set(['bug']),
  })
  assert.equal(findings.length, 0, 'a configured key is not a taxonomy finding')

  const improvement = checkRoleCitations(
    extractRoleCitations("`labelName(config, 'improvement')`"),
    { ...roleKeys(), labels: new Set(['improvement']) },
  )
  assert.equal(improvement.length, 1)
  assert.equal(improvement[0].kind, 'role-taxonomy')
  assert.match(improvement[0].detail, /content-taxonomy\s+label\s+applied\s+literally/)
})

test('the taxonomy split self-check fires when a content label becomes a configured role', () => {
  assert.doesNotThrow(() => assertTaxonomySplit(roleKeys()))
  assert.throws(
    () =>
      assertTaxonomySplit({
        labels: new Set(['docs']),
        states: new Set(),
        githubLabels: new Set(),
      }),
    /CONTENT_LABELS\s+overlaps\s+configured\s+trackerConfig\s+label\s+roles: docs/,
  )
})

// ---------------------------------------------------------------------------
// Check 2 — toolbox export citations
// ---------------------------------------------------------------------------

test('parseExports handles every ESM form the repo uses', () => {
  const names = parseExports(
    [
      'export function alpha() {}',
      'export async function beta() {}',
      'export const gamma = 1',
      'export class Delta {}',
      'const hidden = 2',
      'export { hidden as epsilon, zeta }',
    ].join('\n'),
  )
  assert.deepEqual([...names].sort(), ['Delta', 'alpha', 'beta', 'epsilon', 'gamma', 'zeta'])
})

test('parseExports refuses to silently miss an unclassifiable export form', () => {
  assert.throws(
    () => parseExports('export default helper', 'skills-toolbox/x.mjs'),
    /unclassifiable\s+export\s+at\s+skills-toolbox\/x\.mjs:1/,
  )
})

test('the export index unions skills-toolbox + scripts and seeds tracker operations', () => {
  const root = makeRepo({
    'skills-toolbox/alpha.mjs': 'export function fromToolbox() {}\n',
    'skills-toolbox/alpha.test.mjs': 'export function fromATestFile() {}\n',
    'scripts/beta.mjs': 'export const fromScripts = 1\n',
  })
  const { names, modules } = buildExportIndex(root)
  assert.equal(modules, 2, 'a *.test.mjs sibling is not a module')
  assert.ok(names.has('fromToolbox'))
  assert.ok(names.has('fromScripts'))
  assert.ok(!names.has('fromATestFile'))
  assert.ok(names.has('preparePlanAttachment'), 'tracker operations are seeded into the index')
  assert.ok(names.has('readComments'), 'tracker capabilities are seeded into the index')
})

test('only inline CALL citations count, and conventional-commit prefixes are ignored', () => {
  const contents = [
    'call `validateDecomposition(plan)` first.',
    'bare `validateDecomposition` is not a citation.',
    'commit `feat(scope): subject`, `chore(x): y`, `test(a): b`, `init(a): b`, `type(a): b`.',
  ].join('\n')
  assert.deepEqual(extractExportCitations(contents), [{ line: 1, name: 'validateDecomposition' }])
})

// ---------------------------------------------------------------------------
// Check 3 — helper CLI verbs
// ---------------------------------------------------------------------------

test('dispatch verbs are read from both === and !== comparison idioms', () => {
  assert.deepEqual([...parseDispatchVerbs("if (cmd === 'make-ctx') {}")], ['make-ctx'])
  assert.deepEqual([...parseDispatchVerbs("if (command !== 'put') fail()")], ['put'])
  assert.deepEqual([...parseDispatchVerbs("if (args.sub === 'run') {}")], ['run'])
  assert.deepEqual([...parseDispatchVerbs("if (other === 'run') {}")], [])
})

test('verb citations require the literal node token', () => {
  assert.deepEqual(extractVerbCitations('run `node toolbox/bs-run-sentinel.mjs make-ctx` now'), [
    { line: 1, script: 'bs-run-sentinel.mjs', verb: 'make-ctx' },
  ])
  assert.deepEqual(extractVerbCitations('`node "$TOOLBOX/tracker/cli.mjs" claim-token`'), [
    { line: 1, script: 'cli.mjs', verb: 'claim-token' },
  ])
  assert.deepEqual(extractVerbCitations('`bs-run-sentinel.mjs` missing (needs BOS-144)'), [])
})

test('the verb index unions same-basename modules and tolerates a zero-dispatch shim', () => {
  const root = makeRepo({
    'skills-toolbox/tracker/cli.mjs': "if (cmd === 'claim-token') {}\n",
    'skills-toolbox/finalize/cli.mjs': "if (cmd === 'inject-pr-tag') {}\n",
    'skills-toolbox/skill-extensions.mjs': "if (cmd === 'discover') {}\n",
    'scripts/skill-extensions.mjs':
      "export { discover } from '../skills-toolbox/skill-extensions.mjs'\n",
  })
  const index = buildVerbIndex(root)
  assert.deepEqual([...index.get('cli.mjs')].sort(), ['claim-token', 'inject-pr-tag'])
  assert.deepEqual(
    [...index.get('skill-extensions.mjs')],
    ['discover'],
    'the shim does not empty the union',
  )

  assert.deepEqual(
    checkVerbCitations(
      [
        { line: 1, script: 'cli.mjs', verb: 'inject-pr-tag' },
        { line: 2, script: 'skill-extensions.mjs', verb: 'discover' },
        { line: 3, script: 'not-indexed.mjs', verb: 'anything' },
      ],
      index,
    ),
    [],
  )

  const bad = checkVerbCitations([{ line: 9, script: 'cli.mjs', verb: 'clam-token' }], index)
  assert.equal(bad.length, 1)
  assert.equal(bad[0].kind, 'verb-unknown')
  assert.match(bad[0].detail, /known: claim-token, inject-pr-tag/)
})

test('a cited helper whose dispatch idiom changed is reported, not silently passed', () => {
  const root = makeRepo({ 'skills-toolbox/drift.mjs': 'export function main() {}\n' })
  const findings = checkVerbCitations(
    [{ line: 4, script: 'drift.mjs', verb: 'check' }],
    buildVerbIndex(root),
  )
  assert.equal(findings.length, 1)
  assert.equal(findings[0].kind, 'verb-index-empty')
  assert.match(findings[0].detail, /the\s+dispatch\s+idiom\s+changed/)
})

// ---------------------------------------------------------------------------
// Walker + orchestration
// ---------------------------------------------------------------------------

test('the walker reads references/ but skips vendored toolbox and generated mirrors', () => {
  const root = makeRepo({
    '.claude/skills/boss-build/SKILL.md': '# skill\n',
    '.claude/skills/boss-build/references/review-stack.md': '# ref\n',
    '.claude/skills/boss-build/toolbox/README.md': '# vendored\n',
    '.claude/skills/boss-build/notes.txt': 'not markdown\n',
  })
  assert.deepEqual(
    findSkillFiles(path.join(root, '.claude/skills')).map((f) => path.relative(root, f)),
    [
      path.join('.claude/skills/boss-build/SKILL.md'),
      path.join('.claude/skills/boss-build/references/review-stack.md'),
    ],
  )

  const mirrors = makeRepo({
    '.codex/skills/boss-build/SKILL.md': '# generated mirror\n',
    'plugins/bossd-plugin-claude/skilldata/skills/boss-build/SKILL.md': '# generated mirror\n',
    'keep/SKILL.md': '# kept\n',
  })
  assert.deepEqual(
    findSkillFiles(mirrors).map((f) => path.relative(mirrors, f)),
    [path.join('keep/SKILL.md')],
  )
})

test('checkSkillSymbols reports each finding with its file, line and kind', () => {
  const root = makeRepo({
    // A role citation is also a CALL citation, so the fixture exports the resolvers
    // exactly as skills-toolbox/skill-config.mjs does — otherwise check 2 would
    // double-report every check-1 finding.
    'skills-toolbox/cli.mjs':
      "export function known() {}\nexport function stateName() {}\nexport function labelName() {}\nif (cmd === 'run') {}\n",
    // Check 2 is scoped to skills that vendor a toolbox, and that is discovered from
    // the tree, so the fixture has to actually vendor one.
    'services/boss/internal/skillinstall/skills/boss-build/toolbox/cli.mjs': 'export {}\n',
    'services/boss/internal/skillinstall/skills/boss-build/SKILL.md': [
      "state: `stateName(config, 'shipped')`",
      'call `missingExport(x)`',
      'run `node toolbox/cli.mjs walk`',
    ].join('\n'),
  })
  const { findings, scanned, modules } = checkSkillSymbols(root, {
    config: FIXTURE_CONFIG,
    requireRoleForms: false,
    requireToolboxOwnerPin: false,
  })
  assert.equal(scanned, 1)
  assert.equal(modules, 1)
  assert.deepEqual(
    findings.map(({ line, kind }) => ({ line, kind })),
    [
      { line: 1, kind: 'role-unconfigured' },
      { line: 2, kind: 'export-unknown' },
      { line: 3, kind: 'verb-unknown' },
    ],
  )
  assert.ok(
    findings.every(
      (f) => f.file === 'services/boss/internal/skillinstall/skills/boss-build/SKILL.md',
    ),
    'findings carry the repo-relative posix path',
  )
})

// Ownership is a property of the tree, not of a name on a list: the two fixtures below
// differ ONLY by the presence of a vendored toolbox, and both use skill names absent
// from TOOLBOX_OWNING_SKILLS so a name-matching implementation could not pass them.
test('export citations are scoped to skills that own a vendored toolbox', () => {
  const files = {
    'skills-toolbox/cli.mjs': 'export function known() {}\n',
  }
  const owning = makeRepo({
    ...files,
    '.claude/skills/brand-new-skill/toolbox/cli.mjs': 'export {}\n',
    '.claude/skills/brand-new-skill/SKILL.md': 'call `missingExport(x)`\n',
  })
  assert.equal(
    checkSkillSymbols(owning, {
      config: FIXTURE_CONFIG,
      requireRoleForms: false,
      requireToolboxOwnerPin: false,
    }).findings.length,
    1,
    'a skill vendoring a toolbox is in scope the moment it lands, with no list to update',
  )

  const foreign = makeRepo({
    ...files,
    '.claude/skills/some-other-skill/SKILL.md': 'call `missingExport(x)`\n',
  })
  assert.deepEqual(
    checkSkillSymbols(foreign, {
      config: FIXTURE_CONFIG,
      requireRoleForms: false,
      requireToolboxOwnerPin: false,
    }).findings,
    [],
  )
})

test('discoverToolboxOwners reads both skill roots and ignores non-owners', () => {
  const root = makeRepo({
    '.claude/skills/bs-sweep-debt/toolbox/cli.mjs': 'export {}\n',
    '.claude/skills/plain-skill/SKILL.md': '# no toolbox\n',
    'services/boss/internal/skillinstall/skills/boss-plan/toolbox/tracker/cli.mjs': 'export {}\n',
    'services/boss/internal/skillinstall/skills/boss-plan/SKILL.md': '# owner\n',
  })
  assert.deepEqual(
    [...discoverToolboxOwners(root)].sort(),
    ['boss-plan', 'bs-sweep-debt'],
    'both roots contribute, and a skill without a toolbox is not an owner',
  )

  // A missing root is not an error — .codex/skills is generated, so a checkout that has
  // not run the mirror step still discovers the roots it does have.
  assert.deepEqual([...discoverToolboxOwners(makeRepo({ 'keep.md': '#\n' }))], [])
})

// The drift tripwire. Derivation widens scope on its own, so the direction that needs a
// pin is the one where it NARROWS: a walk that stops seeing toolboxes degrades check 2
// to asserting nothing while still exiting 0.
test('checkSkillSymbols reports drift between the discovered owners and the pin', () => {
  const withEveryPinnedOwner = (extra = {}) =>
    makeRepo(
      Object.fromEntries([
        ...[...TOOLBOX_OWNING_SKILLS].map((name) => [
          `.claude/skills/${name}/toolbox/cli.mjs`,
          'export {}\n',
        ]),
        ...Object.entries(extra),
      ]),
    )

  assert.deepEqual(
    checkSkillSymbols(withEveryPinnedOwner(), { config: FIXTURE_CONFIG, requireRoleForms: false })
      .findings,
    [],
    'a tree matching the pin exactly is clean',
  )

  const added = checkSkillSymbols(
    withEveryPinnedOwner({ '.claude/skills/brand-new-skill/toolbox/cli.mjs': 'export {}\n' }),
    { config: FIXTURE_CONFIG, requireRoleForms: false },
  ).findings
  assert.deepEqual(
    added.map((f) => f.kind),
    ['toolbox-owner-drift'],
  )
  assert.match(added[0].detail, /newly\s+vendoring\s+a\s+toolbox: brand-new-skill/)

  // The dangerous direction: the tree yields fewer owners than the pin names, so check 2
  // has silently narrowed. Only the pin notices.
  const narrowed = checkSkillSymbols(makeRepo({ 'keep.md': '#\n' }), {
    config: FIXTURE_CONFIG,
    requireRoleForms: false,
  }).findings
  assert.deepEqual(
    narrowed.map((f) => f.kind),
    ['toolbox-owner-drift'],
  )
  assert.match(narrowed[0].detail, /no\s+longer\s+discovered: boss-build, boss-epic/)
})

test('checkSkillSymbols is clean on prose that names only real symbols', () => {
  const root = makeRepo({
    'skills-toolbox/cli.mjs':
      "export function known() {}\nexport function stateName() {}\nexport function labelName() {}\nif (cmd === 'run') {}\n",
    '.claude/skills/boss-build/toolbox/cli.mjs': 'export {}\n',
    '.claude/skills/boss-build/SKILL.md': [
      "state: `stateName(config, 'planned')`",
      "label: `labelName(config, 'agentFriendly')`",
      // Form B, so the vacuity floor below sees both forms populated.
      "roles resolve through `labelName(config, '<role>')`, whose keys are: `epic`, `bug`",
      'call `known(x)` and `readComments(id)`',
      'run `node toolbox/cli.mjs run`',
    ].join('\n'),
  })
  const result = checkSkillSymbols(root, { config: FIXTURE_CONFIG, requireToolboxOwnerPin: false })
  assert.deepEqual(result.findings, [])
  assert.deepEqual(result.roleForms, { A: 2, B: 2 })
})

// The vacuity floor. Both forms are extracted line-scoped, so a prose re-wrap can
// silently reduce a check to zero input while the gate still exits 0 — the failure
// mode that would let the seed defect through unnoticed.
test('checkSkillSymbols reports a vacuous check 1 when a role form goes unpopulated', () => {
  const withOnlyFormA = makeRepo({
    'skills-toolbox/cli.mjs': 'export function stateName() {}\nexport function labelName() {}\n',
    '.claude/skills/boss-build/SKILL.md': "state: `stateName(config, 'planned')`\n",
  })
  const formA = checkSkillSymbols(withOnlyFormA, {
    config: FIXTURE_CONFIG,
    requireToolboxOwnerPin: false,
  })
  assert.deepEqual(
    formA.findings.map((f) => [f.kind, f.detail.includes('form B')]),
    [['role-check-vacuous', true]],
  )

  // The re-wrap itself: the same bullet, broken after "whose keys are". Form B goes to
  // zero even though the prose still cites two roles, and only the floor notices.
  const rewrapped = makeRepo({
    'skills-toolbox/cli.mjs': 'export function stateName() {}\nexport function labelName() {}\n',
    '.claude/skills/boss-build/SKILL.md': [
      "state: `stateName(config, 'planned')`",
      "roles resolve through `labelName(config, '<role>')`, whose keys are",
      'are: `epic`, `bug`',
    ].join('\n'),
  })
  const result = checkSkillSymbols(rewrapped, {
    config: FIXTURE_CONFIG,
    requireToolboxOwnerPin: false,
  })
  assert.equal(result.roleForms.B, 0)
  assert.deepEqual(
    result.findings.map((f) => f.kind),
    ['role-check-vacuous'],
  )
})
