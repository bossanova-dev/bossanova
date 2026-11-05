import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { appendSectionBullets, sectionListMarker } from './plan-section-append.mjs'

for (const marker of ['*', '-', '+']) {
  test(`append continues a ${marker} list and skips repeated items`, () => {
    const text = `## Planning\n\n${marker} Contract: v1\n${marker} Scope: bounded\n\n## Original notes\n- untouched`
    const options = { heading: 'Planning', items: ['Dependencies: blocks X-1', 'A note'] }
    const result = appendSectionBullets(text, options)
    assert.deepEqual(result, {
      text: text.replace(
        `${marker} Scope: bounded`,
        `${marker} Scope: bounded\n${marker} Dependencies: blocks X-1\n${marker} A note`,
      ),
      appended: options.items,
      skipped: [],
      marker,
      created: false,
    })
    assert.deepEqual(appendSectionBullets(result.text, options), {
      ...result,
      appended: [],
      skipped: options.items,
    })
  })
}

test('marker falls back outside fences and Original notes, then to dash', () => {
  const text =
    '```md\n## Planning\n- fake\n```\n* elsewhere\n## Planning\n\n## Original notes\n- original'
  assert.equal(sectionListMarker(text, 'Planning'), '*')
  assert.equal(sectionListMarker('## Original notes\n+ original', 'Planning'), '-')
  assert.equal(sectionListMarker('no bullets', 'Planning'), '-')
  assert.equal(sectionListMarker('  * nested\n+ top', 'Planning'), '+')
  const result = appendSectionBullets(text, { heading: 'Planning', items: ['added'] })
  assert.equal(
    result.text,
    text.replace('## Planning\n\n## Original', '## Planning\n\n* added\n## Original'),
  )
})

test('missing sections follow template order and preserve protected bytes', () => {
  const tail = '## Original notes\n\n## Planning\n- untouched\n```\n* code\n```'
  const text = `* preamble\n\n## Planning\n\n* plan\n\n${tail}`
  const questions = appendSectionBullets(text, { heading: 'Open Questions', items: ['Question?'] })
  assert.equal(
    questions.text,
    text.replace('## Planning', '## Open Questions\n\n* Question?\n\n## Planning'),
  )
  assert.equal(questions.created, true)
  const planning = appendSectionBullets(`* preamble\n\n${tail}`, {
    heading: 'Planning',
    items: ['plan'],
  })
  assert.equal(planning.text, `* preamble\n\n## Planning\n\n* plan\n\n${tail}`)
  assert.equal(
    appendSectionBullets('preamble', { heading: 'Planning', items: ['plan'] }).text,
    'preamble\n\n## Planning\n\n- plan',
  )
})

test('preserves fenced bytes, EOL style and terminal newline state', () => {
  for (const eol of ['\n', '\r\n'])
    for (const terminal of ['', eol]) {
      const code = ['~~~md', '## Planning', '- fake', '~~~'].join(eol)
      const text =
        ['## Planning', '', '* real', code, '', '## Original notes', '+ original'].join(eol) +
        terminal
      const result = appendSectionBullets(text, { heading: 'Planning', items: ['added'] })
      assert.equal(result.text, text.replace('* real', `* real${eol}* added`))
      const end = `## Planning${eol}${eol}* real${terminal}`
      assert.equal(
        appendSectionBullets(end, { heading: 'Planning', items: ['added'] }).text,
        `## Planning${eol}${eol}* real${eol}* added${terminal}`,
      )
    }
})

test('refuses Original notes and multiline items', () => {
  assert.throws(() => appendSectionBullets('', { heading: 'Original notes', items: ['no'] }))
  for (const item of ['one\ntwo', 'one\rtwo'])
    assert.throws(() => appendSectionBullets('', { heading: 'Planning', items: [item] }))
})

test('appends after the complete last item including wrapped, nested and fenced content', () => {
  for (const continuation of [
    '  wrapped continuation',
    'lazy wrapped continuation',
    '  * nested item\n    nested continuation',
    '\n  second paragraph\n\n  - nested item',
    '  ```md\n  ## Heading inside the item\n  * fenced bullet\n  ```\n  continued after code',
  ]) {
    for (const eol of ['\n', '\r\n']) {
      const item = `* existing\n${continuation}`.replaceAll('\n', eol)
      const text = `## Planning${eol}${eol}${item}${eol}${eol}## Original notes${eol}- untouched`
      const result = appendSectionBullets(text, { heading: 'Planning', items: ['added'] })
      assert.equal(result.text, text.replace(item, `${item}${eol}* added`))
      assert.equal(
        appendSectionBullets(result.text, { heading: 'Planning', items: ['added'] }).text,
        result.text,
      )
    }
  }
})

test('CLI is strict, atomic on refusal and leaves no-op inode unchanged', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'plan-section-append-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const file = join(dir, 'description.md')
  const text = '## Planning\n\n* existing'
  writeFileSync(file, text)
  const run = (...args) =>
    spawnSync(
      process.execPath,
      ['skills-toolbox/plan/plan-section-append.mjs', '--file', file, ...args],
      { encoding: 'utf8' },
    )
  assert.equal(run('--unknown').status, 64)
  assert.equal(readFileSync(file, 'utf8'), text)
  assert.equal(run('--heading', 'Original notes', '--item', 'no').status, 1)
  assert.equal(readFileSync(file, 'utf8'), text)
  const success = run('--heading', 'Planning', '--item', 'added')
  assert.equal(success.status, 0, success.stderr)
  assert.deepEqual(JSON.parse(success.stdout), {
    appended: ['added'],
    skipped: [],
    marker: '*',
    created: false,
  })
  assert.equal(readFileSync(file, 'utf8'), text + '\n* added')
  const before = statSync(file)
  const noop = run('--heading', 'Planning', '--item', 'added')
  assert.equal(noop.status, 0, noop.stderr)
  assert.deepEqual(JSON.parse(noop.stdout).skipped, ['added'])
  assert.equal(statSync(file).ino, before.ino)
  assert.equal(statSync(file).mtimeMs, before.mtimeMs)
})
