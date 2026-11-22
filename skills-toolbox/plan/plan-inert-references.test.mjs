import assert from 'node:assert/strict'
import { test } from 'node:test'
import { codeRef, inertReferences } from './plan-inert-references.mjs'

const examples = [
  ['Dependencies: blocks ABC-12', 'Dependencies: blocks `ABC-12`'],
  ['Atomic-5: follows abc-12', 'Atomic-5: follows `abc-12`'],
  ['Open-PR overlap: #4175', 'Open-PR overlap: `#4175`'],
  ['See [#4175], owner/repo#4175.', 'See `[#4175]`, `owner/repo#4175`.'],
  [
    'See http://example.invalid/a and https://example.invalid/b',
    'See `http://example.invalid/a` and `https://example.invalid/b`',
  ],
  ['See (https://example.invalid/a).', 'See (`https://example.invalid/a`).'],
  ['See ((https://example.invalid/a_(b))).', 'See ((`https://example.invalid/a_(b)`)).'],
  ['ABC-12, abc-13; X9-4: Z-5! A-6?', '`ABC-12`, `abc-13`; `X9-4`: `Z-5`! `A-6`?'],
  ['Blocked by ABC-12: #4175', 'Blocked by `ABC-12`: `#4175`'],
  ['Blocked by #4175: ABC-12', 'Blocked by `#4175`: `ABC-12`'],
  ['utf-8 and sha-256', '`utf-8` and `sha-256`'],
  [
    'ABC-0 abc-01 ABC-12tail 1ABC-12 ABCDEFGHIJK-12',
    'ABC-0 abc-01 ABC-12tail 1ABC-12 ABCDEFGHIJK-12',
  ],
  ['thing#12 foo-0 plain/file.mjs e.g.', 'thing#12 foo-0 plain/file.mjs e.g.'],
  [
    '`ABC-12 #4175 https://example.invalid` ABC-13',
    '`ABC-12 #4175 https://example.invalid` `ABC-13`',
  ],
  ['``ABC-12 ` #4175`` ABC-13', '``ABC-12 ` #4175`` `ABC-13`'],
  ['`a: ABC-12` ABC-13', '`a: ABC-12` `ABC-13`'],
  ['See ``a: ABC-12`` #13', 'See ``a: ABC-12`` `#13`'],
  ['[see: ABC-12](https://example.invalid) #13', '[see: ABC-12](https://example.invalid) `#13`'],
  [
    'See [a: ABC-12](https://example.invalid) #13',
    'See [a: ABC-12](https://example.invalid) `#13`',
  ],
  ['<span title="a: ABC-12">ABC-13</span> #14', '<span title="a: ABC-12">ABC-13</span> `#14`'],
  [
    '[ABC-12](https://example.invalid/pull/12) #13',
    '[ABC-12](https://example.invalid/pull/12) `#13`',
  ],
  ['[ABC-12](https://example.invalid/a_(b)) #13', '[ABC-12](https://example.invalid/a_(b)) `#13`'],
  ['<https://example.invalid/ABC-12> ABC-13', '<https://example.invalid/ABC-12> `ABC-13`'],
  [
    '<pull-request id="12" label="ABC-12 #12" /> #13',
    '<pull-request id="12" label="ABC-12 #12" /> `#13`',
  ],
  [
    '<span title="ABC-12 > #12">ABC-13</span> ABC-14',
    '<span title="ABC-12 > #12">ABC-13</span> `ABC-14`',
  ],
  [
    'Open PR #4175 (https://github.com/owner/repo/pull/4175) for ABC-12 overlaps at file level (shared: app/api/x.go).',
    'Open PR `#4175` (`https://github.com/owner/repo/pull/4175`) for `ABC-12` overlaps at file level (shared: app/api/x.go).',
  ],
]

for (const [input, expected] of examples) {
  test(`renders inert references in ${input}`, () => {
    assert.equal(inertReferences(input), expected)
    assert.equal(inertReferences(expected), expected)
  })
}

test('codeRef uses a fence wider than embedded backticks with edge padding', () => {
  assert.equal(codeRef('ABC-12'), '`ABC-12`')
  assert.equal(codeRef('a`b``c'), '```a`b``c```')
  assert.equal(codeRef('`ABC-12'), '`` `ABC-12 ``')
  assert.equal(codeRef('ABC-12`'), '`` ABC-12` ``')
})

test('backtick runs close code only at exactly the opening width', () => {
  assert.equal(inertReferences('`` #12 ` #13 `` #14'), '`` #12 ` #13 `` `#14`')
  assert.equal(inertReferences('``` #12 `` #13 ``` #14'), '``` #12 `` #13 ``` `#14`')
  for (const input of ['unclosed ` #12', '#12 unclosed `', 'unclosed `` #12']) {
    const rendered = inertReferences(input)
    assert.equal(inertReferences(rendered), rendered)
    assert.ok(rendered.includes('#12'))
  }
})
