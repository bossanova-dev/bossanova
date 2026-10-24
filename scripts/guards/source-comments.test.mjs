import assert from 'node:assert/strict'
import test from 'node:test'
import { commentsOf } from './source-comments.mjs'

test('Go extracts comments while skipping interpreted strings, raw strings and runes', () => {
  const source = [
    'package example',
    'var quoted = "escaped \\" // not a comment"',
    'var raw = `// not a comment',
    '/* neither */`',
    "var slash = '/'",
    "var quote = '\\''",
    '// TODO: first',
    '// BOS-12 belongs to the same comment',
    'var x = 1 /* block',
    'continues */',
    '',
    '// separate',
  ].join('\n')
  assert.deepEqual(commentsOf(source, 'go'), [
    { text: '// TODO: first\n// BOS-12 belongs to the same comment', line: 7, endLine: 8 },
    { text: '/* block\ncontinues */', line: 9, endLine: 10 },
    { text: '// separate', line: 12, endLine: 12 },
  ])
})

for (const kind of ['ts', 'mjs']) {
  test(`${kind} ignores literal comment markers and retains actual comments`, () => {
    const source = [
      'const quoted = "// not a comment";',
      'const template = `/* not a comment */',
      '// neither`;',
      'const regexp = /https?:\\/\\/host/;',
      'const slash = /[/*]/;',
      '// TODO: actual',
      '  // BOS-13',
      'const value = 1; /* trailing */',
      'const expression = `text ${ /* expression comment */ value }`;',
      '// final',
    ].join('\n')
    assert.deepEqual(commentsOf(source, kind), [
      { text: '// TODO: actual\n  // BOS-13', line: 6, endLine: 7 },
      { text: '/* trailing */', line: 8, endLine: 8 },
      { text: '/* expression comment */', line: 9, endLine: 9 },
      { text: '// final', line: 10, endLine: 10 },
    ])
  })
}

test('TSX ignores JSX text and attributes but extracts JSX block comments', () => {
  const source = [
    'const view = <div title="// attribute">',
    '  // plain JSX text',
    '  /* also plain JSX text */',
    '  {/* TODO: actual JSX comment */}',
    '  <span>https://example.com</span>',
    '</div>;',
    '// after JSX',
  ].join('\n')
  assert.deepEqual(commentsOf(source, 'tsx'), [
    { text: '/* TODO: actual JSX comment */', line: 4, endLine: 4 },
    { text: '// after JSX', line: 7, endLine: 7 },
  ])
})

test('line comment groups stop at blank lines, block comments and intervening code', () => {
  const source = '// a\r\n  // b\r\n\r\n// c\r\n/* d */\r\n// e\r\nlet x = 0; // f\r\n// g'
  for (const kind of ['go', 'ts']) {
    assert.deepEqual(
      commentsOf(source, kind).map(({ line, endLine }) => [line, endLine]),
      [
        [1, 2],
        [4, 4],
        [5, 5],
        [6, 6],
        [7, 8],
      ],
    )
  }
})
