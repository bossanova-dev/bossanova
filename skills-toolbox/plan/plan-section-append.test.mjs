import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { appendSectionBullets, planningBullet, sectionListMarker } from './plan-section-append.mjs'
import { runCli } from '../tracker/cli.mjs'
import { applyDescriptionPatch } from '../tracker/description-patch.mjs'
import { buildLinearOperationMap } from '../tracker/linear.mjs'
import { region } from '../../scripts/test-infra/gate-region-lib.mjs'

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

test('Planning and Open Questions appends reach Linear as safe description deltas', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'planning-description-patch-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const baseFile = join(dir, 'base.md')
  const bodyFile = join(dir, 'body.md')
  const lastBullet = '* Plan attachment: Implementation plan (TEST-1)'
  const summary = 'Preserve the stored issue while recording planning decisions.'
  const original = [
    '```md',
    '## Planning',
    '* A heading inside source code is not the active section.',
    '```',
    '![Original screenshot](https://uploads.linear.app/example/screenshot.png)',
    ...Array.from(
      { length: 80 },
      (_, i) => `Original observation ${i}: preserve these gated bytes.`,
    ),
  ].join('\n')
  const planning = {
    heading: 'Planning',
    items: ['Dependencies: TEST-2 is merged', 'Keep the chosen scope'],
  }
  const questions = { heading: 'Open Questions', items: ['Use the planned default? Chosen: yes.'] }
  const compose = (text) =>
    appendSectionBullets(appendSectionBullets(text, planning).text, questions).text
  const describe = (base, body) => {
    writeFileSync(baseFile, base)
    writeFileSync(bodyFile, body)
    let out = '',
      err = ''
    const code = runCli(
      ['write-description', '--id', 'TEST-1', '--base-file', baseFile, '--body-file', bodyFile],
      {
        write: (s) => {
          out += s
        },
        errWrite: (s) => {
          err += s
        },
        resolveAdapter: () => ({ operationMap: buildLinearOperationMap('linear') }),
      },
    )
    assert.equal(code, 0, err)
    return JSON.parse(out)
  }
  for (const duplicateAnchor of [false, true]) {
    const base = [
      '## Summary',
      '',
      summary,
      ...(duplicateAnchor ? [lastBullet] : []),
      '',
      '## Key changes',
      '',
      '* Add planning decisions mechanically.',
      '',
      '## Planning',
      '',
      '* Contract: v1',
      '* Scope: bounded',
      lastBullet,
      '',
      '## Original notes',
      '',
      original,
      '',
    ].join('\n')
    assert.equal(base.split(lastBullet).length - 1, duplicateAnchor ? 2 : 1)
    const body = compose(base)
    assert.equal(compose(body), body)
    assert.equal(region(body, '## Original notes'), region(base, '## Original notes'))
    const descriptor = describe(base, body)
    assert.equal(descriptor.outcome, 'descriptor-emitted')
    if (duplicateAnchor && descriptor.mode === 'replace') {
      assert.equal(descriptor.args.description, body)
      assert.equal(descriptor.args.patch, undefined)
    } else {
      assert.equal(descriptor.mode, 'patch')
      assert.equal('description' in descriptor.args, false)
      const ops = descriptor.args.patch
      assert.ok(ops.length > 0)
      let reconstructed = base
      for (const op of ops) {
        const anchor = op.op === 'replace' ? op.old_string : op.anchor
        assert.equal(reconstructed.split(anchor).length - 1, 1)
        reconstructed = applyDescriptionPatch(reconstructed, [op])
      }
      assert.equal(reconstructed, body)
      const inserted = ops.map((op) => op.text ?? op.new_string).join('\n')
      for (const item of [...planning.items, ...questions.items]) assert.ok(inserted.includes(item))
      // Headings and fence delimiters are shared syntax; protect the existing prose.
      const protectedLines = [summary, ...original.split('\n')].filter(
        (line) => line && !line.startsWith('## ') && !line.startsWith('```'),
      )
      for (const line of protectedLines) assert.ok(!inserted.split('\n').includes(line))
      assert.equal(descriptor.patchBytes, Buffer.byteLength(JSON.stringify(ops)))
      assert.equal(descriptor.bytes, Buffer.byteLength(body))
      assert.ok(descriptor.patchBytes < descriptor.bytes)
    }
    const unchanged = describe(body, compose(body))
    assert.equal(unchanged.args.patch, undefined)
    assert.equal(unchanged.args.description, body)
  }
})

for (const marker of ['-', '*', '+']) {
  test(`planningBullet reads ${marker} items with continuation lines`, () => {
    assert.equal(
      planningBullet(
        `## Planning\n${marker} Atomic-5: one\n  indivisible\n  change\n${marker} Other: next`,
        'Atomic-5',
      ),
      'Atomic-5: one indivisible change',
    )
  })
}

test('planningBullet ignores fences, other sections, empty labels and Original notes', () => {
  for (const text of [
    '## Other\n- Atomic-5: other',
    '```md\n## Planning\n- Atomic-5: example\n```',
    '## Original notes\n## Planning\n- Atomic-5: stale',
    '## Planning\n- Atomic-5:   ',
    '## Planning\n```md\n- Atomic-5: example\n```',
  ])
    assert.equal(planningBullet(text, 'Atomic-5'), null)
  assert.equal(
    planningBullet(
      '## Planning\n- Atomic-5: actual\n```md\n  ignored\n```\n## Original notes\n  stale',
      'Atomic-5',
    ),
    'Atomic-5: actual',
  )
})
