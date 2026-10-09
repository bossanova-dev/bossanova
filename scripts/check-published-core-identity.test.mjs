import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  checkPublishedCoreIdentity,
  publishedSources,
  readIdentityRules,
} from './check-published-core-identity.mjs'
import { region } from './gate-region-lib.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const GO_RULES = fs.readFileSync(
  path.join(REPO_ROOT, 'services/boss/internal/skillinstall/skills_manifest_test.go'),
  'utf8',
)

function fixture(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'published-core-identity-'))
  for (const [file, content] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), content)
  }
  return root
}

test('every forbiddenIdentity rule is read from the Go test, both literal forms included', () => {
  const rules = readIdentityRules(GO_RULES)
  const block = region(
    GO_RULES,
    'var forbiddenIdentity = []identityRule{',
    'var knownIdentityLeaks',
    'skills_manifest_test.go',
  )
  assert.equal(rules.length, [...block.matchAll(/regexp\.MustCompile\(/g)].length)
  for (const sample of ['BOS-1289', 'bossanova-linear', 'key `BOS`', 'Team **Bossanova**']) {
    assert.ok(
      rules.some((rule) => new RegExp(rule.source).test(sample)),
      `no rule matches ${sample}`,
    )
  }
})

test('a missing rule block fails instead of passing vacuously', () => {
  assert.throws(() => readIdentityRules('package skillinstall\n'), /forbiddenIdentity block/)
})

test('a ticket id in a vendored toolbox source fails at its file and line', () => {
  const root = fixture({
    'skills-toolbox/plan-run-guards.mjs': 'const a = 1\n// Fixed in BOS-1289.\n',
  })
  const { failures } = checkPublishedCoreIdentity({
    repoRoot: root,
    rules: readIdentityRules(GO_RULES),
    sources: new Map([['skills-toolbox/plan-run-guards.mjs', ['boss-plan']]]),
  })
  assert.deepEqual(failures, [
    'skills-toolbox/plan-run-guards.mjs:2: "BOS-1289" ships in published boss-plan',
  ])
})

test('the scan covers vendored toolbox sources of published cores only', () => {
  const sources = publishedSources(REPO_ROOT)
  assert.deepEqual(sources.get('skills-toolbox/plan-run-guards.mjs'), ['boss-plan'])
  assert.ok(
    [...sources.keys()].some((file) => file.endsWith('boss-plan/SKILL.md')),
    'published SKILL.md bodies are scanned',
  )
  assert.ok(
    [...sources.values()].every((cores) => cores.every((core) => core.startsWith('boss'))),
    'repo-local bs-* skills are not scanned',
  )
})
