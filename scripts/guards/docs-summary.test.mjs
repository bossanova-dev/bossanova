import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fixture, cli } from './docs-guard-fixture.mjs'

test('docs-summary rejects missing and block scalar summaries with the registered command', (t) => {
  for (const text of [
    '# Doc\n',
    '---\nsummary: |\n  text\n---\n',
    '---\nsummary: ""\n---\n',
    '---\nsummary: first\n  continued\n---\n',
  ]) {
    const root = fixture(t, { 'docs/guides/a.md': text })
    assert.equal(cli('docs-summary', root).status, 1)
    fs.writeFileSync(path.join(root, 'docs/guides/a.md'), '---\nsummary: A doc.\n---\n')
    assert.equal(cli('docs-summary', root).status, 0)
  }
})
test('docs-summary exempts email bodies, solutions and skills but checks email metadata', (t) => {
  const root = fixture(t, {
    'docs/email-course/day-1.md': '',
    'docs/solutions/a.md': '',
    'docs/skills/a.md': '',
    'docs/email-course/VOICE.md': '',
  })
  assert.equal(cli('docs-summary', root).status, 1)
  fs.writeFileSync(path.join(root, 'docs/email-course/VOICE.md'), '---\nsummary: Voice.\n---\n')
  assert.equal(cli('docs-summary', root).status, 0)
})
