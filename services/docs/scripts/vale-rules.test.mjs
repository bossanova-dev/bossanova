import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { beforeAll, describe, expect, it } from 'vitest'

import { runVale, valeBinary } from './prose-lint.mjs'

const vale = valeBinary()
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const styleDirectory = path.join(root, 'styles', 'BossanovaProse')
const fixtureDirectory = path.join(root, 'testdata', 'vale')
const rules = fs
  .readdirSync(styleDirectory)
  .filter((file) => file.endsWith('.yml'))
  .map((file) => path.basename(file, '.yml'))
  .sort()
const fixtureForRule = (rule) => `${rule.replace(/([a-z0-9])([A-Z])/g, '$1-$2').toLowerCase()}.md`
const fixtures = [...rules.map(fixtureForRule), 'preserved-forms.md']
let alertsByFixture

function alertsFor(fixture) {
  const matching = Object.entries(alertsByFixture).find(([file]) => file.endsWith(`/${fixture}`))
  return matching?.[1] ?? []
}

beforeAll(() => {
  const output = runVale(vale, [
    '--config=.vale.ini',
    '--output=JSON',
    ...fixtures.map((fixture) => `testdata/vale/${fixture}`),
  ])
  alertsByFixture = JSON.parse(output)
})

describe('BossanovaProse Vale rules', () => {
  for (const rule of rules) {
    const fixture = fixtureForRule(rule)
    it(`BossanovaProse.${rule} has a triggering fixture`, () => {
      expect(fs.existsSync(path.join(fixtureDirectory, fixture))).toBe(true)
      expect(alertsFor(fixture).map((alert) => alert.Check)).toContain(`BossanovaProse.${rule}`)
    })
  }

  it('preserves definition labels and bolded negations', () => {
    expect(alertsFor('preserved-forms.md')).toEqual([])
  })
})
