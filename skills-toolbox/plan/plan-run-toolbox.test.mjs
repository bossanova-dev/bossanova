import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, realpathSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { pathToFileURL } from 'node:url'
import { formatHelperProvenance, runToolboxProvenance } from './plan-run-toolbox.mjs'

function fixture(t) {
  const dir = realpathSync(mkdtempSync(join(tmpdir(), 'plan-run-toolbox-')))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const root = join(dir, 'real/boss-plan/toolbox')
  const linked = join(dir, 'skills/boss-plan')
  const helper = join(root, 'plan/h.mjs')
  const foreign = join(dir, 'other/boss-plan/toolbox')
  mkdirSync(join(root, 'plan'), { recursive: true })
  mkdirSync(join(dir, 'skills'))
  mkdirSync(foreign, { recursive: true })
  writeFileSync(helper, '')
  symlinkSync(join(dir, 'real/boss-plan'), linked)
  return { dir, root, helper, linked: join(linked, 'toolbox'), foreign }
}

test('an unset or empty run toolbox preserves helper provenance without a root', (t) => {
  const { helper } = fixture(t)
  for (const env of [{}, { BOSS_PLAN_TOOLBOX: '' }]) {
    assert.deepEqual(runToolboxProvenance(pathToFileURL(helper), env), {
      helper,
      root: null,
      contained: null,
    })
  }
})

test('a symlink spelling reports the canonical toolbox and helper as contained', (t) => {
  const { helper, root, linked } = fixture(t)
  assert.deepEqual(
    runToolboxProvenance(pathToFileURL(join(linked, 'plan/h.mjs')), { BOSS_PLAN_TOOLBOX: linked }),
    { helper, root, contained: true },
  )
})

test('a foreign toolbox and a missing root report outside without throwing', (t) => {
  const { dir, helper, foreign } = fixture(t)
  for (const root of [foreign, join(dir, 'missing')]) {
    assert.deepEqual(runToolboxProvenance(pathToFileURL(helper), { BOSS_PLAN_TOOLBOX: root }), {
      helper,
      root,
      contained: false,
    })
  }
})

test('a sibling sharing the toolbox prefix is outside', (t) => {
  const { root } = fixture(t)
  const helper = join(`${root}2`, 'h.mjs')
  mkdirSync(`${root}2`)
  writeFileSync(helper, '')
  assert.deepEqual(runToolboxProvenance(pathToFileURL(helper), { BOSS_PLAN_TOOLBOX: root }), {
    helper,
    root,
    contained: false,
  })
})

test('helper provenance formats absent, contained and outside run toolboxes', () => {
  const helper = '/skills/toolbox/plan/h.mjs'
  const prefix = 'plan-writeback-verify:'
  assert.equal(
    formatHelperProvenance(prefix, { helper, root: null, contained: null }),
    `${prefix} helper ${helper}`,
  )
  assert.equal(
    formatHelperProvenance(prefix, { helper, root: '/skills/toolbox', contained: true }),
    `${prefix} helper ${helper} (run toolbox /skills/toolbox)`,
  )
  assert.equal(
    formatHelperProvenance(prefix, { helper, root: '/other/toolbox', contained: false }),
    `${prefix} helper ${helper} (OUTSIDE run toolbox /other/toolbox)`,
  )
})
