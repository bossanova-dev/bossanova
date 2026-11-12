import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const guard = path.join(root, 'scripts/guards/check-web-iframe-sandbox.mjs')
const file = 'services/web/src/Seed.tsx'

function run(t, source, name = file) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'web-iframe-sandbox-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  fs.mkdirSync(path.dirname(path.join(dir, name)), { recursive: true })
  fs.writeFileSync(path.join(dir, name), source)
  execFileSync('git', ['init', '-q', dir])
  execFileSync('git', ['-C', dir, 'add', '.'])
  return spawnSync(process.execPath, [guard, '--root', dir], { encoding: 'utf8' })
}

const frame = (attributes, allow = 'allow=""') =>
  `export const F = () => (\n  <iframe src="https://x" ${allow} ${attributes} />\n)\n`
const jsx = (element) => `export const F = () => (\n  ${element}\n)\n`

test('an iframe with allow-same-origin fails naming the file and line', (t) => {
  const result = run(t, frame('sandbox="allow-scripts allow-same-origin"'))
  assert.equal(result.status, 1)
  assert.match(
    result.stderr,
    /services\/web\/src\/Seed\.tsx:2: <iframe> sandbox grants allow-same-origin/,
  )
})

const granted = 'allow-scripts allow-popups allow-popups-to-escape-sandbox'

for (const token of [
  'allow-same-origin',
  'allow-top-navigation',
  'allow-top-navigation-by-user-activation',
  'allow-top-navigation-to-custom-protocols',
  'allow-forms',
  'allow-modals',
  'allow-downloads',
  'allow-presentation',
  'allow-pointer-lock',
  'allow-orientation-lock',
  'allow-storage-access-by-user-activation',
]) {
  test(`rejects ${token} beside every allowed token, naming it`, (t) => {
    const result = run(t, frame(`sandbox="${granted} ${token}"`))
    assert.equal(result.status, 1)
    assert.match(result.stderr, new RegExp(`Seed\\.tsx:2: <iframe> sandbox grants ${token}$`, 'm'))
  })
}

for (const [label, attributes] of [
  ['no sandbox', 'title="t"'],
  ['upper-case ALLOW-FORMS', 'sandbox="ALLOW-FORMS"'],
  ['a computed sandbox', 'sandbox={tokens}'],
  ['mixed-case Allow-Same-Origin', 'sandbox="allow-scripts Allow-Same-Origin"'],
  ['spread props', '{...props} sandbox="allow-scripts"'],
]) {
  test(`rejects ${label}`, (t) => {
    assert.equal(run(t, frame(attributes)).status, 1)
  })
}

for (const escape of ['allow-popups-to-escape-sandbox', 'Allow-Popups-To-Escape-Sandbox']) {
  test(`rejects ${escape} without allow-popups`, (t) => {
    const result = run(t, frame(`sandbox="allow-scripts ${escape}"`))
    assert.equal(result.status, 1)
    assert.match(result.stderr, /<iframe> sandbox grants allow-popups-to-escape-sandbox without/)
  })
}

test('mixed-case Allow-Same-Origin fails naming the token', (t) => {
  const result = run(t, frame('sandbox="allow-scripts Allow-Same-Origin"'))
  assert.equal(result.status, 1)
  assert.match(result.stderr, /Seed\.tsx:2: <iframe> sandbox grants Allow-Same-Origin/)
})

for (const [label, allow] of [
  ['no allow attribute', ''],
  ['a feature policy in allow', 'allow="camera"'],
  ['a computed allow', 'allow={features}'],
]) {
  test(`rejects ${label}`, (t) => {
    assert.equal(run(t, frame('sandbox="allow-scripts"', allow)).status, 1)
  })
}

for (const tag of ['object', 'embed', 'frame']) {
  test(`rejects a JSX <${tag}>`, (t) => {
    const result = run(t, jsx(`<${tag} data="https://x" />`))
    assert.equal(result.status, 1)
    assert.match(result.stderr, new RegExp(`Seed\\.tsx:2: <${tag}>`))
  })
}

for (const [label, html] of [
  ['an upper-case <IFRAME string', "'<IFRAME src=x></IFRAME>'"],
  ['an <object> template', '`<object data="x"></object>`'],
  ['an <embed> template with a substitution', '`<embed src="${src}">`'],
  ['a <frame> string', '"<frame src=x>"'],
  ['a non-literal value', 'html'],
]) {
  test(`rejects dangerouslySetInnerHTML with ${label}`, (t) => {
    const result = run(t, jsx(`<div dangerouslySetInnerHTML={{ __html: ${html} }} />`))
    assert.equal(result.status, 1)
    assert.match(result.stderr, /Seed\.tsx:2: <div> dangerouslySetInnerHTML/)
  })
}

test('accepts dangerouslySetInnerHTML with literal markup that frames nothing', (t) => {
  assert.equal(run(t, jsx(`<div dangerouslySetInnerHTML={{ __html: '<p>hi</p>' }} />`)).status, 0)
})

test('rejects an iframe created outside JSX', (t) => {
  assert.equal(run(t, "document.createElement('iframe')\n", 'services/web/src/seed.ts').status, 1)
})

for (const sandbox of [
  'sandbox=""',
  'sandbox="allow-scripts"',
  'sandbox={"allow-scripts"}',
  'sandbox="  Allow-Scripts\tallow-scripts "',
  'sandbox="allow-popups"',
  `sandbox="${granted}"`,
  'sandbox={"ALLOW-POPUPS Allow-Popups-To-Escape-Sandbox allow-scripts"}',
]) {
  test(`accepts ${sandbox}`, (t) => {
    assert.equal(run(t, frame(sandbox)).status, 0)
  })
}

test('accepts allow={""}', (t) => {
  assert.equal(run(t, frame('sandbox="allow-scripts"', 'allow={""}')).status, 0)
})

test('rejects an <object> created outside JSX', (t) => {
  assert.equal(run(t, "document.createElement('OBJECT')\n", 'services/web/src/seed.ts').status, 1)
})

test('ignores iframe markup inside strings', (t) => {
  assert.equal(run(t, "export const html = '<iframe src=x></iframe>'\n").status, 0)
})

test('real tree passes', () => {
  const result = spawnSync(process.execPath, [guard], { cwd: root, encoding: 'utf8' })
  assert.equal(result.status, 0, result.stderr)
})
