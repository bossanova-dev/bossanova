import assert from 'node:assert/strict'
import test from 'node:test'
import { colourLiterals, compareBaseline } from './check-web-css-colours.mjs'

for (const [name, css, count] of [
  ['hex', '.a { color: #fff; }', 1],
  ['long hex in a shorthand', '.a { border: 1px solid #2b3144aa; }', 1],
  [
    'functional colours',
    '.a { box-shadow: 0 2px 6px rgb(0 0 0 / 0.3), 0 0 0 1px hsla(0, 0%, 0%, 0.1); }',
    2,
  ],
  ['a fallback colour', '.a { background: var(--bg-term, #0a0d13); }', 1],
  [
    'a literal mixed with a token',
    '.a { color: color-mix(in oklab, #d97757 75%, var(--text-h)); }',
    1,
  ],
  ['a named colour', '.a { color: color-mix(in srgb, var(--warning) 90%, black); }', 1],
  ['a component-scoped custom property', '.a { --row-tint: #123456; }', 1],
  [
    'tokens only',
    '.a { color: var(--text); background: color-mix(in oklab, var(--success) 38%, transparent); }',
    0,
  ],
  [
    'the token layer on :root',
    ':root { --border: #2b3144; }\n:root[data-theme="light"] { --border: #e6e8ee; }',
    0,
  ],
  [
    'the token layer inside a media query',
    '@media (prefers-color-scheme: light) { :root:not([data-theme]) { --bg: #fafafa; } }',
    0,
  ],
  ['a non-token declaration on :root', ':root { color: #b3bcd0; }', 1],
  ['comments and strings', '/* #fff rgb(0 0 0) */ .a { content: "#fff"; }', 0],
  ['an id selector', '#root .a { color: var(--text); }', 0],
  ['a name that is not a colour property', '.a { grid-area: tan; animation-name: red; }', 0],
  [
    'currentcolor and transparent',
    '.a { border-color: currentColor; background: transparent; }',
    0,
  ],
]) {
  test(`classifier: ${name}`, () => assert.equal(colourLiterals(css).length, count))
}

test('reports the line each literal sits on', () => {
  assert.deepEqual(
    colourLiterals('.a {\n  color: var(--text);\n  background: #000;\n}').map((l) => l.line),
    [3],
  )
})

test('ratchet accepts exact counts', () => {
  assert.deepEqual(compareBaseline({ 'a.css': 2 }, { 'a.css': 2 }), [])
})
for (const [name, counts, baseline, kind] of [
  ['new file', { 'a.css': 1 }, {}, 'new-instance'],
  ['new literal in a baselined file', { 'a.css': 3 }, { 'a.css': 2 }, 'new-instance'],
  ['reduced count', { 'a.css': 1 }, { 'a.css': 2 }, 'stale-entry'],
  ['cleaned file', {}, { 'a.css': 2 }, 'stale-entry'],
  ['unsorted baseline', {}, { 'z.css': 1, 'a.css': 1 }, 'invalid-baseline'],
  ['invalid count', {}, { 'a.css': 0 }, 'invalid-baseline'],
]) {
  test(`ratchet rejects ${name}`, () => {
    assert.ok(compareBaseline(counts, baseline).some((failure) => failure.kind === kind))
  })
}

test('a new hard-coded colour in a web stylesheet fails the ratchet', () => {
  // The workbench's ghost terminal shipped a fallback hex and an rgb() shadow.
  const css =
    '.ghost-term {\n  background: var(--bg-term, #0a0d13);\n  box-shadow: 0 20px 60px rgb(0 0 0 / 0.35);\n}\n'
  const file = 'services/web/src/features/sessions/components/SessionsWorkbench.css'
  const failures = compareBaseline({ [file]: colourLiterals(css).length }, {})
  assert.deepEqual(
    failures.map((failure) => [failure.kind, failure.file]),
    [['new-instance', file]],
  )
})
