import assert from 'node:assert/strict'
import { test } from 'node:test'
import { writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { discoverExtensions } from '../skill-extensions.mjs'
import {
  describeTerraform,
  terraformBlocks,
  terraformDiff,
  terraformRoots,
  terraformStep,
} from './verify-describe-terraform.mjs'
import { envelopeFor, gitFixture, runDescriberScript } from './verify-git-fixture.mjs'
import { MAX_STEPS } from './verify-describe-blocks.mjs'
import { refineDescription } from './verify-describe.mjs'
import { MAX_ITEMS, withDigest } from './verify-proof.mjs'
import { renderProofHtml } from './verify-proof-page.mjs'

const PAYLOAD = new URL(
  '../../lib/bossalib/contract/plugin/skillpayload/skills/boss-verify/extensions/',
  import.meta.url,
)
const resource = (name, value = 'safe') =>
  `resource "test_item" "${name}" {\n  value = "${value}"\n}\n`
const variable = (name) => `variable "${name}" {\n  type = string\n}\n`

function change(t, before, after) {
  const repo = gitFixture(t)
  repo.write({ README: 'base', ...before })
  const base = repo.commit('base')
  repo.write(after)
  const head = repo.commit('head')
  const envelope = envelopeFor(repo.dir, base, head, 'infra', 'boss-verify-terraform')
  return { repo, envelope, result: describeTerraform(envelope, { cwd: repo.dir }) }
}

test('parses every block kind, movements, removed blocks and a heredoc containing a closing brace', () => {
  const parsed = terraformBlocks(
    `${resource('one')}data "test_lookup" "one" {\n}\nmodule "one" {\n source = "./child"\n}\n${variable('token')}output "one" {\n value = "secret"\n}\nmoved {\n from = test_item.old\n to = test_item.one\n}\nremoved {\n from = test_item.gone\n}\nresource "test_item" "policy" {\n policy = <<EOF\n{\n}\nEOF\n value = "after"\n}\n`,
  )
  assert.deepEqual(
    [...parsed.blocks.keys()],
    [
      'test_item.one',
      'data.test_lookup.one',
      'module.one',
      'var.token',
      'output.one',
      'test_item.policy',
    ],
  )
  assert.match(parsed.blocks.get('test_item.policy'), /value = "after"/)
  assert.deepEqual(parsed.moved, [{ from: 'test_item.old', to: 'test_item.one' }])
  assert.deepEqual(parsed.removed, new Map([['test_item.gone', true]]))
  assert.deepEqual(
    terraformDiff(resource('one'), resource('one').replace('  value', '  # note\n\n  value')),
    {
      added: [],
      changed: [],
      removed: [],
      removedByBlock: new Map(),
      removedBlocks: [],
      renamed: [],
      requiredVariables: new Set(),
      unmovedRenames: [],
    },
  )
})

test('nested template quotes retain URL edits and keep literal braces inside their block', (t) => {
  const before =
    'resource "test_item" "one" {\n value = "${format("https://old.example/%s", var.host)}"\n}\n'
  const after = before.replace('old.example', 'new.example')
  const { result } = change(t, { 'tf/main.tf': before }, { 'tf/main.tf': after })
  assert.match(result.items.join('\n'), /test_item.one/)
  assert.match(result.blocks[0].steps.join('\n'), /terraform apply/)
  assert.doesNotMatch(JSON.stringify(result), /old.example|new.example/)
  const template =
    'resource "test_item" "template" {\n value = "${replace(var.text, "{", "")}"\n}\n'
  const diff = terraformDiff(template + resource('next'), template + resource('next', 'changed'))
  assert.deepEqual(diff.changed, ['test_item.next'])
  assert.deepEqual(
    [...terraformBlocks(template + resource('next')).blocks.keys()],
    ['test_item.template', 'test_item.next'],
  )
})

test('roots walk local callers, share roots, and bound cycles and long chains', () => {
  assert.deepEqual(terraformRoots('root', new Map()), ['root'])
  assert.deepEqual(terraformRoots('child', new Map([['child', ['root']]])), ['root'])
  assert.deepEqual(
    terraformRoots(
      'grandchild',
      new Map([
        ['grandchild', ['child']],
        ['child', ['staging', 'prod']],
      ]),
    ),
    ['prod', 'staging'],
  )
  assert.deepEqual(
    terraformRoots(
      'a',
      new Map([
        ['a', ['b']],
        ['b', ['a']],
      ]),
    ),
    [],
  )
  const chain = new Map(Array.from({ length: 10 }, (_, n) => [`${n}`, [`${n + 1}`]]))
  assert.deepEqual(terraformRoots('0', chain), [])
})

test('no Terraform changes, same-directory file moves and comments need no facts or apply', (t) => {
  const empty = change(t, {}, { README: 'changed' })
  assert.deepEqual(empty.result, { items: [], breaking: false })
  const moved = change(
    t,
    { 'tf/a.tf': resource('one') },
    { 'tf/a.tf': '', 'tf/b.tf': resource('one') },
  )
  assert.deepEqual(moved.result, { items: [], breaking: false })
  const comments = change(
    t,
    { 'tf/a.tf': resource('one') },
    { 'tf/a.tf': resource('one') + '# note\n' },
  )
  assert.deepEqual(comments.result, { items: [], breaking: false })
})

test('moving and reordering three locals bodies within a directory needs no facts or apply', (t) => {
  const locals = (name) => `locals {\n ${name} = 1\n}\n`
  const before = {
    'tf/a.tf': locals('a'),
    'tf/b.tf': locals('b'),
    'tf/c.tf': locals('c'),
  }
  for (const after of [
    { 'tf/b.tf': null, 'tf/d.tf': locals('b') },
    { 'tf/a.tf': locals('c'), 'tf/c.tf': locals('a') },
  ]) {
    assert.deepEqual(change(t, before, after).result, { items: [], breaking: false })
  }
})

test('moved blocks preserve resource and module moves including destinations outside the directory', (t) => {
  const { result } = change(
    t,
    {
      'tf/main.tf': resource('old') + 'module "old" {\n source = "./child"\n}\n',
    },
    {
      'tf/main.tf':
        resource('new') +
        'moved {\n from = test_item.old\n to = test_item.new\n}\nmoved {\n from = module.old\n to = module.parent.module.old\n}\n',
    },
  )
  assert.equal(result.breaking, false)
  assert.match(result.items.join('\n'), /renamed with a moved block/)
  assert.match(result.items.join('\n'), /module.parent.module.old/)
  assert.match(result.blocks[0].steps[0], /2 moved/)
  assert.doesNotMatch(result.items.join('\n'), /destroys/)
})

test('identical-body renames without moves warn about destroy and recreate', (t) => {
  const { result } = change(t, { 'tf/main.tf': resource('old') }, { 'tf/main.tf': resource('new') })
  assert.match(result.items[0], /destroys and recreates it/)
  assert.match(result.blocks[0].steps[0], /1 recreated/)
})

test('a state move retains simultaneous resource configuration edits', (t) => {
  const result = change(
    t,
    { 'tf/main.tf': resource('old', 'BEFORE_SECRET') },
    {
      'tf/main.tf':
        resource('new', 'AFTER_SECRET') +
        'moved {\n from = test_item.old\n to = test_item.new\n}\n',
    },
  ).result
  assert.match(result.items.join('\n'), /configuration changed \(test_item.new\)/)
  assert.match(result.items.join('\n'), /renamed with a moved block/)
  assert.match(result.blocks[0].steps[0], /1 changed, 1 moved/)
  assert.doesNotMatch(JSON.stringify(result), /BEFORE_SECRET|AFTER_SECRET/)
})

test('directory migrations compare the same configuration across its old and new locations', (t) => {
  for (const edited of [false, true]) {
    const repo = gitFixture(t)
    const source =
      'terraform {\n backend "remote" {\n key = "UNCHANGED_STATE_KEY"\n }\n}\n' +
      resource('one', 'before')
    repo.write({ 'infra/prod/main.tf': source })
    const base = repo.commit('base')
    repo.move('infra/prod/main.tf', 'terraform/prod/main.tf')
    if (edited) repo.write({ 'terraform/prod/main.tf': source.replace('"before"', '"after"') })
    const head = repo.commit('head')
    const result = describeTerraform(envelopeFor(repo.dir, base, head, 'infra'), { cwd: repo.dir })
    assert.match(
      result.items.join('\n'),
      /configuration directory moved.*infra\/prod.*terraform\/prod/,
    )
    assert.doesNotMatch(
      JSON.stringify(result),
      /destroys|destroyed|Decommission|resource added|UNCHANGED_STATE_KEY/,
    )
    assert.match(result.blocks[0].steps.join('\n'), /state\/backend continuity/)
    assert.match(result.blocks[0].steps.join('\n'), /terraform apply in `terraform\/prod`/)
    if (edited) assert.match(result.items.join('\n'), /configuration changed/)
  }
})

test('ambiguous directory splits stay review-only even in the overflow step', (t) => {
  const repo = gitFixture(t)
  const before = Object.fromEntries(
    Array.from({ length: 4 }, (_, n) => [
      [`tf/old-${n}/a.tf`, resource(`one_${n}`)],
      [`tf/old-${n}/b.tf`, resource(`two_${n}`)],
    ]).flat(),
  )
  repo.write(before)
  const base = repo.commit('base')
  for (let n = 0; n < 4; n++) {
    repo.move(`tf/old-${n}/a.tf`, `tf/new-${n}-a/main.tf`)
    repo.move(`tf/old-${n}/b.tf`, `tf/new-${n}-b/main.tf`)
  }
  const head = repo.commit('head')
  const result = describeTerraform(envelopeFor(repo.dir, base, head, 'infra'), { cwd: repo.dir })
  assert.doesNotMatch(
    JSON.stringify(result),
    /so the plan destroys|Decommission|Run terraform apply/,
  )
  assert.match(result.items.join('\n'), /if state addresses continue, the plan destroys/)
  assert.equal(result.blocks[0].steps.length, MAX_STEPS)
  assert.match(result.blocks[0].steps.at(-1), /review.*state\/backend continuity/i)
  assert.match(result.blocks[0].steps.at(-1), /\d+ potentially destroyed/)
  assert.doesNotMatch(result.blocks[0].steps.join('\n'), /\b\d+ (?:destroyed|recreated)\b/)
})

test('moving a child configuration still applies through its calling root', (t) => {
  const repo = gitFixture(t)
  repo.write({
    'root/main.tf': 'module "child" { source = "../old" }\n',
    'old/main.tf': resource('one'),
  })
  const base = repo.commit('base')
  repo.move('old/main.tf', 'new/main.tf')
  repo.write({ 'root/main.tf': 'module "child" { source = "../new" }\n' })
  const head = repo.commit('head')
  const result = describeTerraform(envelopeFor(repo.dir, base, head, 'infra'), { cwd: repo.dir })
  const steps = result.blocks[0].steps.join('\n')
  assert.match(steps, /configuration moved from `old` to `new`.*terraform apply in `root`/)
  assert.doesNotMatch(steps, /terraform apply in `new`|Decommission/)
})

test('resource body edits describe configuration without promising an in-place provider action', (t) => {
  const { result } = change(
    t,
    { 'tf/main.tf': resource('server', 'old-region') },
    { 'tf/main.tf': resource('server', 'new-region') },
  )
  assert.match(result.items.join('\n'), /resource configuration changed/)
  assert.doesNotMatch(result.items.join('\n'), /in place|old-region|new-region/)
})

test('nested and indexed state moves do not imply destruction of their containing module', (t) => {
  const moved =
    'moved {\n from = module.old.test_item.one[0]\n to = module.new.test_item.one[0]\n}\n'
  const split = change(
    t,
    { 'tf/main.tf': 'module "old" { source = "./old" }\n' },
    { 'tf/main.tf': 'module "new" { source = "./new" }\n' + moved },
  ).result
  assert.match(split.items.join('\n'), /module.old.test_item.one\[0\]/)
  assert.doesNotMatch(JSON.stringify(split), /destroys|destroyed|recreates/)
  assert.match(split.blocks[0].steps[0], /1 moved/)
  const onlyMove = change(t, { 'tf/main.tf': resource('one') }, { 'tf/moved.tf': moved }).result
  assert.match(onlyMove.items.join('\n'), /moved block/)
  assert.match(onlyMove.blocks[0].steps[0], /1 moved/)
})

test('moved string-keyed instances retain spaces and escaped quotes in addresses', () => {
  for (const key of ['prod key', 'prod \\"key']) {
    const from = `test_item.old["${key}"]`
    const to = `test_item.new["${key}"]`
    const source = `moved {\n from = ${from}\n to = ${to}\n}\n`
    assert.deepEqual(terraformBlocks(source).moved, [{ from, to }])
    assert.deepEqual(terraformDiff('', source).renamed, [{ from, to }])
  }
})

test('explicit removed blocks keep identical-body additions separate from renames', (t) => {
  const { result } = change(
    t,
    { 'tf/main.tf': resource('old') },
    {
      'tf/main.tf':
        resource('new') +
        'removed {\n from = test_item.old\n lifecycle {\n destroy = false\n }\n}\n',
    },
  )
  assert.match(result.items.join('\n'), /resource added \(test_item.new\)/)
  assert.match(result.items.join('\n'), /dropped through a removed block \(test_item.old\)/)
  assert.match(result.blocks[0].steps[0], /1 added/)
  assert.doesNotMatch(JSON.stringify(result), /recreate|destroy/)
})

test('plain deletion destroys resources; removed blocks and data deletions say what they know', (t) => {
  const { result } = change(
    t,
    {
      'tf/main.tf': resource('one') + resource('two', 'other') + 'data "test_lookup" "one" {\n}\n',
    },
    {
      'tf/main.tf': 'removed {\n from = test_item.two\n}\n',
    },
  )
  assert.match(result.items.join('\n'), /so the plan destroys it \(test_item.one\)/)
  assert.match(result.items.join('\n'), /dropped through a removed block \(test_item.two\)/)
  assert.match(result.items.join('\n'), /data source removed/)
  assert.doesNotMatch(
    result.items.find((x) => x.includes('data source')),
    /destroy/,
  )
  assert.match(result.blocks[0].steps[0], /2 destroyed/)
})

test('removed block destruction defaults to true and respects explicit lifecycle settings', () => {
  for (const destroy of [undefined, true, false]) {
    const lifecycle = destroy === undefined ? '' : `lifecycle { destroy = ${destroy} }`
    for (const [address, source] of [
      ['test_item.old', resource('old')],
      ['module.old', 'module "old" { source = "./child" }\n'],
    ]) {
      const diff = terraformDiff(source, `removed {\n from = ${address}\n ${lifecycle}\n}\n`)
      const step = terraformStep('tf', diff)
      if (destroy === false) assert.doesNotMatch(step, /destroyed/)
      else assert.match(step, /1 destroyed/)
    }
  }
})

test('standalone removed blocks report state changes and apply through calling roots', (t) => {
  /** @param {boolean | undefined} destroy */
  const removal = (destroy) =>
    `removed {\n from = module.inner.test_item.old\n${destroy === undefined ? '' : ` lifecycle { destroy = ${destroy} }\n`}}\n`
  /** @type {[string, string, string, boolean | undefined][]} */
  const cases = [
    ['', removal(undefined), 'added', true],
    ['', removal(false), 'added', false],
    [removal(false), removal(true), 'changed', true],
    [removal(true), removal(false), 'changed', false],
    [removal(false), '', 'removed', undefined],
  ]
  for (const [before, after, operation, destroy] of cases) {
    const { result } = change(
      t,
      {
        'root/main.tf': 'module "child" {\n source = "../child"\n}\n',
        'child/state.tf': before,
      },
      { 'child/state.tf': after },
    )
    const facts = result.items.join('\n')
    assert.match(facts, /module\.inner\.test_item\.old/)
    assert.match(facts, new RegExp(`removed block ${operation}`))
    if (destroy !== undefined) assert.match(facts, new RegExp(`destroy=${destroy}`))
    const steps = result.blocks?.flatMap((block) => block.steps).join('\n') ?? ''
    assert.match(steps, /review.*plan.*prior state/i)
    assert.match(steps, /terraform apply in `root` for module `child`/)
    if (destroy) assert.match(steps, /potentially destroyed/)
    else assert.doesNotMatch(steps, /destroyed/)
  }
  assert.deepEqual(terraformDiff(removal(undefined), removal(true)).removedBlocks, [])
})

test('required root variables are passed, child variables belong to callers, and values never print', (t) => {
  const { result } = change(
    t,
    {
      'envs/prod/main.tf': 'module "child" {\n source = "../../modules/x"\n}\n',
      'modules/x/main.tf': resource('one'),
    },
    {
      'envs/prod/vars.tf': variable('root_token'),
      'modules/x/main.tf': resource('one', 'DO_NOT_PRINT_SECRET') + variable('child_token'),
    },
  )
  assert.match(result.items.join('\n'), /child_token \(no default\)/)
  assert.match(result.blocks[0].steps.join('\n'), /passing `root_token`/)
  assert.match(
    result.blocks[0].steps.join('\n'),
    /in `envs\/prod` for module `modules\/x`.*callers must set `child_token`/,
  )
  assert.doesNotMatch(JSON.stringify(result), /DO_NOT_PRINT_SECRET/)
  if (process.env.BOS_CHILD_PROOF)
    writeFileSync(process.env.BOS_CHILD_PROOF, JSON.stringify(result, null, 2) + '\n')
})

test('required variables ignore default text in heredocs and nested type objects', (t) => {
  for (const body of [
    ' description = <<EOT\ndefault = supplied by the caller\nEOT\n type = string\n',
    ' type = object({\n default = string\n })\n',
    ' description = "default = supplied by the caller"\n type = string\n',
  ]) {
    const source = `variable "token" {\n${body}}\n`
    const { result } = change(t, {}, { 'root/main.tf': source })
    assert.match(result.items.join('\n'), /token \(no default\)/)
    assert.match(result.blocks[0].steps[0], /passing `token`/)
    const withDefault = source.replace('\n}', '\n default = null\n}')
    assert.equal(terraformDiff('', withDefault).requiredVariables.size, 0)
    assert.ok(terraformDiff(withDefault, source).requiredVariables.has('var.token'))
  }
})

test('local module sources with trailing slashes apply in their calling roots', (t) => {
  for (const source of ['../child/', '../child//', '../child/./']) {
    const { result } = change(
      t,
      {
        'root/main.tf': `module "child" {\n source = "${source}"\n}\n`,
        'child/main.tf': resource('one'),
      },
      { 'child/main.tf': resource('one', 'changed') },
    )
    assert.match(result.blocks[0].steps.join('\n'), /in `root` for module `child`/)
    assert.doesNotMatch(result.blocks[0].steps.join('\n'), /apply in `child`/)
  }
})

test('existing variable and output edits name their facts and newly required arguments', (t) => {
  const { result } = change(
    t,
    {
      'root/main.tf':
        'variable "token" {\n default = "OLD_SECRET"\n}\noutput "url" {\n value = "OLD_URL"\n}\n',
    },
    { 'root/main.tf': variable('token') + 'output "url" {\n value = "NEW_SECRET_URL"\n}\n' },
  )
  assert.match(result.items.join('\n'), /variables changed: token \(no default\)/)
  assert.match(result.items.join('\n'), /outputs changed: url/)
  assert.match(result.blocks[0].steps[0], /passing `token`/)
  assert.doesNotMatch(JSON.stringify(result), /OLD_SECRET|OLD_URL|NEW_SECRET_URL/)
})

test('locals-only changes produce a named fact and an apply step without values', (t) => {
  const source =
    'locals {\n hostname = "BEFORE_SECRET"\n}\nresource "test_item" "one" {\n value = local.hostname\n}\n'
  const { result } = change(
    t,
    { 'tf/main.tf': source },
    { 'tf/main.tf': source.replace('BEFORE_SECRET', 'AFTER_SECRET') },
  )
  assert.match(result.items.join('\n'), /local values changed/)
  assert.doesNotMatch(result.items.join('\n'), /resource/)
  assert.match(result.blocks[0].steps[0], /terraform apply in `tf`.*1 changed/)
  assert.doesNotMatch(JSON.stringify(result), /BEFORE_SECRET|AFTER_SECRET/)
  for (const [before, after] of [
    [resource('one'), source],
    [source, resource('one')],
  ]) {
    const facts = change(t, { 'tf/main.tf': before }, { 'tf/main.tf': after }).result
    assert.doesNotMatch(JSON.stringify(facts), /resource added|resource removed|destroys|destroyed/)
  }
})

test('a removed child uses its base callers; registry sources are not guessed as callers', (t) => {
  const deleted = change(
    t,
    {
      'root/main.tf': 'module "child" {\n source = "../child"\n}\n',
      'child/main.tf': resource('one'),
    },
    { 'root/main.tf': '', 'child/main.tf': null },
  )
  assert.match(deleted.result.blocks[0].steps.join('\n'), /in `root` for module `child`/)
  const registry = change(
    t,
    {
      'root/main.tf': 'module "child" {\n source = "registry.invalid/child"\n}\n',
      'child/main.tf': resource('one'),
    },
    { 'child/main.tf': resource('two') },
  )
  assert.match(registry.result.blocks[0].steps[0], /in `child`/)
})

test('deleted roots require prior configuration and state instead of apply in missing directories', (t) => {
  const deleted = change(t, { 'root/main.tf': resource('one') }, { 'root/main.tf': null }).result
  assert.match(deleted.blocks[0].steps[0], /Decommission deleted Terraform root `root`/)
  assert.match(deleted.blocks[0].steps[0], /previous configuration and state/)
  assert.doesNotMatch(deleted.blocks[0].steps[0], /Run terraform apply/)
  const tree = change(
    t,
    {
      'root/main.tf': 'module "child" { source = "../child" }\n',
      'child/main.tf': resource('one'),
    },
    { 'root/main.tf': null, 'child/main.tf': null },
  ).result
  assert.doesNotMatch(tree.blocks[0].steps.join('\n'), /Run terraform apply/)
  const before = Object.fromEntries(
    Array.from({ length: 12 }, (_, n) => [`root-${n}/main.tf`, resource('one')]),
  )
  const overflow = change(
    t,
    before,
    Object.fromEntries(Object.keys(before).map((file) => [file, null])),
  ).result
  assert.equal(overflow.blocks[0].steps.length, MAX_STEPS)
  assert.match(overflow.blocks[0].steps.at(-1), /decommission/i)
  assert.doesNotMatch(overflow.blocks[0].steps.join('\n'), /Run terraform apply/)
})

test('overflow keeps one bounded steps block and names every changed directory', (t) => {
  const files = Object.fromEntries(
    Array.from({ length: 17 }, (_, n) => [
      `dir-${String(n).padStart(2, '0')}/main.tf`,
      resource('one') + variable('token'),
    ]),
  )
  const { result } = change(t, {}, files)
  assert.equal(result.items.length, MAX_ITEMS)
  assert.match(result.items.at(-1), /not shown/)
  assert.equal(result.blocks.length, 1)
  assert.equal(result.blocks[0].steps.length, MAX_STEPS)
  assert.match(result.blocks[0].steps.at(-1), /8 more directories/)
  assert.deepEqual(
    refineDescription({
      ...result,
      ok: true,
      extension: 'boss-verify-terraform',
      role: 'proof-describe',
    }),
    [],
  )
  for (const file of Object.keys(files))
    assert.ok(JSON.stringify(result).includes(file.split('/')[0]))
})

test('overflow applies changed child modules through all resolved calling roots', (t) => {
  const before = Object.fromEntries(
    Array.from({ length: 11 }, (_, n) => [
      `modules/child-${String(n).padStart(2, '0')}/main.tf`,
      resource('one'),
    ]),
  )
  const calls = Object.keys(before)
    .map(
      (file, n) => `module "child${n}" {\n source = "../../${file.replace('/main.tf', '')}"\n}\n`,
    )
    .join('')
  const { result } = change(
    t,
    { ...before, 'envs/prod/main.tf': calls, 'envs/stage/main.tf': calls },
    Object.fromEntries(Object.keys(before).map((file) => [file, resource('one', 'updated')])),
  )
  const overflow = result.blocks[0].steps.at(-1)
  assert.equal(result.blocks[0].steps.length, MAX_STEPS)
  assert.match(overflow, /2 more directories/)
  assert.match(overflow, /`envs\/prod`/)
  assert.match(overflow, /`envs\/stage`/)
  assert.doesNotMatch(overflow, /modules\/child/)
})

test('apply steps render as unchecked After merge items, outside infra facts', (t) => {
  const { result } = change(t, {}, { 'tf/main.tf': resource('one') })
  const html = renderProofHtml({
    proof: withDigest({
      schema: 'boss-proof/1',
      repo: 'example/app',
      pr: 1,
      head: '1'.repeat(40),
      base: '2'.repeat(40),
      confidence: {
        level: 'moderate',
        asks: ['infra'],
        loweredBy: [{ code: 'gap', text: 'Infrastructure requires review' }],
      },
      whatItDoes: { source: 'pr-body', bullets: ['Infra'] },
      areas: [
        {
          name: 'infra',
          label: 'Infrastructure',
          ask: true,
          items: result.items,
          media: [],
          proven: false,
        },
      ],
      review: { models: [], blockers: [], notes: [] },
      health: [],
      page: null,
    }),
    described: { infra: { blocks: result.blocks } },
    timeline: [],
  })
  assert.match(html, /After merge/)
  assert.match(html, /type="checkbox"[^>]*>.*Run terraform apply/s)
  assert.equal((html.match(/Run terraform apply/g) ?? []).length, 1)
})

test('payload discovery and vendored entry point produce the canonical bounded facts', (t) => {
  const { repo, envelope, result } = change(
    t,
    { 'tf/main.tf': resource('old') },
    { 'tf/main.tf': resource('new') },
  )
  const { extensions } = discoverExtensions({
    core: 'boss-verify',
    role: 'proof-describe',
    builtinDir: fileURLToPath(PAYLOAD),
    root: repo.dir,
  })
  const extension = extensions.find((x) => x.name === 'boss-verify-terraform')
  assert.equal(extension.builtin, true)
  assert.deepEqual(extension.describeAreas, ['infra', 'no-sign-off'])
  const actual = runDescriberScript(
    new URL('boss-verify-terraform/describe.mjs', PAYLOAD),
    envelope,
    repo.dir,
  )
  assert.deepEqual(actual.items, result.items)
  assert.deepEqual(actual.blocks, result.blocks)
  assert.equal(actual.breaking, false)
})

test('boilerplate renames between unrelated roots retain decommission actions', (t) => {
  const versions = 'terraform { required_version = ">= 1.0" }\n'
  const { repo, envelope, result } = change(
    t,
    {
      'a/versions.tf': versions,
      'a/main.tf': resource('old', 'old-value'),
    },
    {
      'a/versions.tf': null,
      'a/main.tf': null,
      'b/versions.tf': versions,
      'b/main.tf': resource('new', 'new-value'),
    },
  )
  assert.match(
    repo.git('diff', '--name-status', '-M', envelope.baseSha, envelope.headSha),
    /R100\s+a\/versions.tf\s+b\/versions.tf/,
  )
  assert.match(result.blocks[0].steps.join('\n'), /Decommission deleted Terraform root `a`/)
  assert.match(result.blocks[0].steps.join('\n'), /terraform apply in `b`/)
  assert.doesNotMatch(result.items.join('\n'), /directory moved/)
  const actual = runDescriberScript(
    new URL('boss-verify-terraform/describe.mjs', PAYLOAD),
    envelope,
    repo.dir,
  )
  assert.deepEqual(actual.blocks, result.blocks)
})

test('directory migrations retain conditional destruction and recreation warnings', (t) => {
  for (const child of [false, true]) {
    for (const rename of [false, true]) {
      const repo = gitFixture(t)
      repo.write({
        'old/anchor.tf': resource('anchor'),
        'old/main.tf': resource('gone'),
        ...(child ? { 'root/main.tf': 'module "child" { source = "../old" }\n' } : {}),
      })
      const base = repo.commit('base')
      repo.move('old/anchor.tf', 'new/anchor.tf')
      repo.write({
        'old/main.tf': null,
        ...(rename ? { 'new/main.tf': resource('renamed') } : {}),
        ...(child ? { 'root/main.tf': 'module "child" { source = "../new" }\n' } : {}),
      })
      const head = repo.commit('head')
      const envelope = envelopeFor(repo.dir, base, head, 'infra')
      const result = describeTerraform(envelope, { cwd: repo.dir })
      assert.match(
        result.items.join('\n'),
        rename
          ? /if state addresses continue.*destroys and recreates/
          : /if state addresses continue.*destroys/,
      )
      const steps = result.blocks[0].steps.join('\n')
      assert.match(steps, rename ? /1 potentially recreated/ : /1 potentially destroyed/)
      assert.match(steps, child ? /terraform apply in `root`/ : /terraform apply in `new`/)
      const actual = runDescriberScript(
        new URL('boss-verify-terraform/describe.mjs', PAYLOAD),
        envelope,
        repo.dir,
      )
      assert.deepEqual(actual.items, result.items)
      assert.deepEqual(actual.blocks, result.blocks)
    }
  }
})

test('provider, Terraform and import edits produce facts and steps without values', (t) => {
  for (const block of ['provider "aws"', 'terraform', 'import']) {
    const before = `${block} {\n value = "PRIVATE_BEFORE"\n}\n`
    const { result } = change(
      t,
      { 'tf/main.tf': before },
      {
        'tf/main.tf': before.replace('PRIVATE_BEFORE', 'PRIVATE_AFTER'),
      },
    )
    assert.match(result.items.join('\n'), /configuration changed/)
    assert.match(result.blocks[0].steps[0], /terraform apply in `tf`/)
    assert.doesNotMatch(JSON.stringify(result), /PRIVATE/)
    assert.deepEqual(terraformDiff(before, before + '# comment\n').changed, [])
  }
})

test('long root lists keep change counts and required-variable instructions ahead of truncation', () => {
  const diff = terraformDiff(resource('gone'), resource('new', 'changed') + variable('token'))
  const roots = Array.from({ length: 20 }, (_, n) => `environments/long-root-${n}`)
  const step = terraformStep('child', diff, roots)
  assert.match(step, /1 added/)
  assert.match(step, /1 destroyed/)
  assert.match(step, /set 1 required variables/)
  assert.ok(step.length <= 300)
  assert.match(step, /terraform apply/)
})

test('long output-only steps begin with their apply action', () => {
  const diff = terraformDiff('', 'output "url" { value = "private" }\n')
  const roots = Array.from({ length: 20 }, (_, n) => `environments/long-root-${n}`)
  const step = terraformStep('child', diff, roots)
  assert.match(step, /^Run terraform apply in /)
  assert.ok(step.length <= 300)
})

test('migration destruction counts combine declaration and state-only removals', () => {
  const diff = terraformDiff(resource('gone'), 'removed {\n from = test_item.state_only\n}\n')
  const step = terraformStep('new', diff, ['new'], [], 'old')
  assert.match(step, /2 potentially destroyed/)
  assert.equal((step.match(/potentially destroyed/g) ?? []).length, 1)
})

test('variable and output counts follow the same convention for added and changed blocks', () => {
  const before = variable('token') + 'output "url" { value = "old" }\n'
  const after = before.replace('type = string', 'type = number').replace('"old"', '"new"')
  for (const diff of [terraformDiff('', before), terraformDiff(before, after)]) {
    const step = terraformStep('root', diff)
    assert.doesNotMatch(step, /\d+ added|\d+ changed/)
  }
})

test('overflow retains destruction counts and required variables', (t) => {
  const before = Object.fromEntries(
    Array.from({ length: 12 }, (_, n) => [`dir-${n}/main.tf`, resource('gone')]),
  )
  const after = Object.fromEntries(
    Object.keys(before).map((file) => [file, resource('new', 'changed') + variable('token')]),
  )
  const { result } = change(t, before, after)
  const overflow = result.blocks[0].steps.at(-1)
  assert.match(overflow, /destroyed/)
  assert.match(overflow, /set 1 required variables/)
  assert.match(overflow, /apply/)
  assert.ok(overflow.length <= 300)
})

test('moved instance labels preserve dotted keys and module paths', (t) => {
  for (const [from, to, label] of [
    ['test_item.one["before.key"]', 'test_item.one["after.key"]', 'one["after.key"]'],
    [
      'module.child["before.key"].test_item.one',
      'module.child["after.key"].test_item.two',
      'module.child["after.key"].test_item.two',
    ],
  ]) {
    const { result } = change(
      t,
      { 'tf/main.tf': resource('one') },
      {
        'tf/main.tf': resource('one') + `moved {\n from = ${from}\n to = ${to}\n}\n`,
      },
    )
    assert.ok(result.items.some((text) => text.includes(`${from} -> ${label}`)))
  }
})
