import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
import {
  GENERATED_HEADER,
  REFERENCE_HEADER,
  spliceInjection,
  exciseSections,
  applyRewrites,
  constructSkill,
  constructReference,
  skillsRootAvailable,
} from './construct-skills.mjs'

const rootDir = fileURLToPath(new URL('..', import.meta.url))

const liveManifest = JSON.parse(
  fs.readFileSync(path.join(rootDir, '.claude/skills/boss-implement/construct.json'), 'utf8'),
)
// Construction reads the component skills from the installed superpowers plugin.
// Where it is absent (a fresh CI runner), skip the integration tests rather than fail.
const sourceSkip =
  !skillsRootAvailable(liveManifest) &&
  `superpowers ${liveManifest.superpowers_version} not installed`

test('spliceInjection replaces between markers and keeps them', () => {
  const tpl = 'A\n<!-- INJECT:x -->\nOLD\n<!-- END INJECT:x -->\nB'
  const out = spliceInjection(tpl, 'x', 'NEW')
  assert.match(out, /<!-- INJECT:x -->\nNEW\n<!-- END INJECT:x -->/)
  assert.doesNotMatch(out, /OLD/)
  assert.match(out, /^A\n/)
  assert.match(out, /\nB$/)
})

test('spliceInjection throws on missing markers', () => {
  assert.throws(() => spliceInjection('no markers', 'x', 'NEW'), /missing or malformed/)
})

test('exciseSections removes matched spans', () => {
  const src = 'keep\n## Required workflow skills\ndrop me\n## Next\nkeep2'
  const out = exciseSections(src, ['^## Required workflow skills[\\s\\S]*?(?=^## )'])
  assert.doesNotMatch(out, /drop me/)
  assert.match(out, /## Next/)
})

test('exciseSections throws when a pattern matches nothing (upstream drift guard)', () => {
  assert.throws(() => exciseSections('abc', ['^## Nonexistent']), /matched nothing/)
})

test('applyRewrites rewrites all occurrences', () => {
  const out = applyRewrites('see ../requesting-code-review/code-reviewer.md', [
    ['\\.\\./requesting-code-review/code-reviewer\\.md', 'the inlined template'],
  ])
  assert.equal(out, 'see the inlined template')
})

test('GENERATED_HEADER warns against editing', () => {
  assert.match(GENERATED_HEADER, /Do not edit/i)
})

test('REFERENCE_HEADER warns against editing', () => {
  assert.match(REFERENCE_HEADER, /Do not edit/i)
})

test(
  'committed generated references match a fresh construction (no drift)',
  { skip: sourceSkip },
  () => {
    const manifest = JSON.parse(
      fs.readFileSync(path.join(rootDir, '.claude/skills/boss-implement/construct.json'), 'utf8'),
    )
    for (const reference of manifest.references ?? []) {
      const fresh = constructReference(manifest, reference, { rootDir })
      const committed = fs.readFileSync(path.join(rootDir, reference.dest), 'utf8')
      assert.equal(
        committed,
        fresh,
        `${reference.dest} is out of date — run "make construct-skills"`,
      )
    }
  },
)

test('committed SKILL.md matches a fresh construction (no drift)', { skip: sourceSkip }, () => {
  const manifest = JSON.parse(
    fs.readFileSync(path.join(rootDir, '.claude/skills/boss-implement/construct.json'), 'utf8'),
  )
  const fresh = constructSkill(manifest, { rootDir })
  const committed = fs.readFileSync(
    path.join(rootDir, '.claude/skills/boss-implement/SKILL.md'),
    'utf8',
  )
  assert.equal(committed, fresh)
})

test('constructed SKILL.md is already Prettier-stable', { skip: sourceSkip }, () => {
  const manifest = JSON.parse(
    fs.readFileSync(path.join(rootDir, '.claude/skills/boss-implement/construct.json'), 'utf8'),
  )
  const fresh = constructSkill(manifest, { rootDir })
  const formatted = execFileSync(
    'pnpm',
    [
      'exec',
      'prettier',
      '--stdin-filepath',
      path.join(rootDir, '.claude/skills/boss-implement/SKILL.md'),
    ],
    { cwd: rootDir, encoding: 'utf8', input: fresh },
  )

  assert.equal(fresh, formatted)
})
