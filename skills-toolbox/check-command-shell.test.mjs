import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  DEFAULT_CONFIG,
  classifyCheckCommand,
  parseAcceptanceCriteria,
  parsePremises,
  parsePlanCheckItems,
  tokenizeSimpleShell,
} from './skill-config.mjs'

const clauses = [
  [
    "`git diff --quiet HEAD -- f` (it's untouched)",
    'git diff --quiet HEAD -- f',
    " (it's untouched)",
  ],
  ['`test -f f` (see `g`)', 'test -f f', ' (see `g`)'],
  ["`grep -c 'x' f` → 1", "grep -c 'x' f", ' → 1'],
  ["``grep -c '`' f`` trailing", "grep -c '`' f", ' trailing'],
  ['grep -n foo f', 'grep -n foo f', ''],
  ['`grep -n foo f', '`grep -n foo f', ''],
]
for (const [tail, expected, prose] of clauses) {
  test(`planned clause extracts command: ${tail}`, () => {
    const body = `## Acceptance criteria\n- [ ] Claim — check: ${tail}\n## Premises\n- [ ] Claim — check: ${tail}`
    assert.equal(parseAcceptanceCriteria(DEFAULT_CONFIG, body)[0].check, expected)
    for (const premise of [
      parsePremises(DEFAULT_CONFIG, body)[0],
      parsePlanCheckItems(DEFAULT_CONFIG, body).premises[0],
    ]) {
      assert.equal(premise.check, expected)
      assert.equal(premise.claim, `Claim${prose}`)
    }
  })
}

const accepted = [
  '(cd services && test -d x)',
  'for f in a b; do grep -qw x "$f"; done',
  'grep -qw x f; rc=$?; test "$rc" -eq 0',
  'if grep -qw x f; then true; fi',
  'while false; do true; done',
  '{ grep -qw x f; }',
  'if ! grep -qw x f; then true; fi',
  '( ! grep -qw x f )',
  '! (cd services && test -d x)',
  '! grep -qw foo f',
  'cd dir && make test',
]
for (const command of accepted) {
  for (const kind of ['criterion', 'premise']) {
    test(`resolves ${kind} shell check: ${command}`, () => {
      assert.deepEqual(classifyCheckCommand(command, { kind }).blocking, [])
    })
  }
}
for (const command of [
  'rc=$?',
  '!',
  '( )',
  '( ); true',
  'for f in a; do done',
  '(nonexistent-cmd-zz)',
  'for f in a; do nonexistent-cmd-zz "$f"; done',
  'if nonexistent-cmd-zz; then true; fi',
  '! ! grep -q x f',
  '$(nonexistent-cmd-zz)',
]) {
  test(`rejects unresolved shell check: ${command}`, () => {
    assert.ok(classifyCheckCommand(command).blocking.some((f) => f.code === 'command-unresolvable'))
  })
}
for (const command of [
  '! grep -q foo f',
  'if ! grep -q foo f; then true; fi',
  '( ! grep -q foo f )',
]) {
  test(`keeps negative substring guard: ${command}`, () => {
    assert.ok(
      classifyCheckCommand(command, { kind: 'criterion' }).blocking.some(
        (f) => f.code === 'unanchored-negative-search',
      ),
    )
  })
}
for (const command of ['git ls-files | grep -c x', '(git ls-files | grep -c x)']) {
  test(`keeps pipefail guard: ${command}`, () => {
    assert.ok(
      classifyCheckCommand(command, { kind: 'criterion' }).blocking.some(
        (f) => f.code === 'pipe-without-pipefail',
      ),
    )
  })
}
test('keeps command and process substitutions together', () => {
  assert.deepEqual(tokenizeSimpleShell('echo $(a) b'), ['echo', '$(a)', 'b'])
  assert.deepEqual(tokenizeSimpleShell('echo $((1 + 2)) <(git status) >(true)'), [
    'echo',
    '$((1 + 2))',
    '<(git status)',
    '>(true)',
  ])
})
