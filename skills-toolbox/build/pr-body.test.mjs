import assert from 'node:assert/strict'
import { test } from 'node:test'
import { main, mergePrBody, parsePrBody } from './pr-body.mjs'

const LEGACY = "`go test ./internal/views -run '^TestLegacyBaseline$' -count=1 -v` → PASS"
const PTY =
  "`go test ./internal/tuitest -run '^TestTUI_(Timeline|Scroll)$' -count=1 -timeout 120s` → PASS"

const CUBIC = [
  '<!-- This is an auto-generated description by cubic. -->',
  '---',
  '## Summary by cubic',
  'Reworks the details view.',
  '<!-- End of auto-generated description by cubic. -->',
].join('\n')

const PUBLISHED = `Linear issue: https://tracker.example/ISSUE-1

Session details now show a timeline.

## Acceptance criteria
- [x] Details show the timeline.

## Local verification
- \`go test ./internal/views -run TestLegacyBaseline -count=1\` → PASS
- \`go test ./internal/tuitest -run TestPTY -count=1\` → PASS

## Autonomous decisions
- Used the transcript for the full question.

## Human follow-up
- none

## Open questions
- none

## Host notes
- local lint: installed buf differed from the pinned one.

## Unapplied review findings
- [ ] **P2** \`views/session_timeline.go:28\` — refresh the question.

## Cross-model review
skipped: OAuth session expired

## Review coverage
full

${CUBIC}
`

const UPDATE = `## Local verification
- legacy regression: ${LEGACY}
- PTY: ${PTY}
- odd tokens: \`printf '%s' "$&" "$\`" "$1" "$$"\` → literal

## Host notes
- local lint: installed buf differed from the pinned one.
- cross-model review: OAuth session expired.

## Review coverage
full
`

const GITHUB_ROUND_TRIPS = {
  verbatim: (body) => body,
  crlf: (body) => body.replace(/\n/g, '\r\n'),
  'trailing whitespace': (body) => {
    const bot = body.indexOf(CUBIC)
    return `${body.slice(0, bot).replace(/^(## .*)$/gm, '$1  ')}${body.slice(bot)}\n\n  \n`
  },
  'no trailing newline': (body) => body.trimEnd(),
}

const headingCounts = (body) => {
  const counts = {}
  for (const { key } of parsePrBody(body).sections) counts[key] = (counts[key] ?? 0) + 1
  return counts
}

const sectionText = (body, key) => {
  const section = parsePrBody(body).sections.find((s) => s.key === key)
  return section ? section.lines.join('\n').trim() : undefined
}

const mergeTwice = (body, update, options) => {
  const once = mergePrBody(body, update, options)
  assert.equal(
    mergePrBody(once.body, update, options).body,
    once.body,
    'second merge changed the body',
  )
  return once
}

const OPEN = '<!-- This is an auto-generated description by cubic. -->'
const CLOSE = '<!-- End of auto-generated description by cubic. -->'

test('merging its own output with the same update is a fixed point', () => {
  const once = mergePrBody(PUBLISHED, UPDATE).body
  let body = once
  for (let pass = 2; pass <= 6; pass += 1) {
    body = mergePrBody(body, UPDATE).body
    assert.equal(body, once, `pass ${pass} changed the body`)
  }
  for (const [name, roundTrip] of Object.entries(GITHUB_ROUND_TRIPS)) {
    assert.equal(
      mergePrBody(roundTrip(once), UPDATE).body,
      once,
      `${name} round trip changed the body`,
    )
  }
  for (const count of Object.values(headingCounts(once))) assert.equal(count, 1)
})

test('replacement text carrying $-patterns is written byte for byte', () => {
  const body = mergePrBody(PUBLISHED, UPDATE).body
  assert.equal(
    sectionText(body, 'local verification'),
    parsePrBody(UPDATE).sections[0].lines.join('\n').trim(),
  )
  assert.ok(body.includes(LEGACY))
  assert.ok(body.includes(PTY))
  assert.ok(body.includes('"$&" "$`" "$1" "$$"'))
})

test('a body spliced by String#replace heals once the update renders the sections that differ', () => {
  let corrupted = PUBLISHED
  corrupted = corrupted.replace(
    'go test ./internal/views -run TestLegacyBaseline -count=1',
    "go test ./internal/views -run '^TestLegacyBaseline$' -count=1 -v",
  )
  corrupted = corrupted.replace(
    'go test ./internal/tuitest -run TestPTY -count=1',
    "go test ./internal/tuitest -run '^TestTUI_(Timeline|Scroll)$' -count=1 -timeout 120s",
  )
  assert.equal(
    headingCounts(corrupted)['host notes'],
    4,
    'the fixture reproduces the duplicated sections',
  )

  const healed = mergePrBody(corrupted, UPDATE).body
  for (const [key, count] of Object.entries(headingCounts(healed))) assert.equal(count, 1, key)
  assert.equal(sectionText(healed, 'review coverage'), 'full')
  assert.equal(healed.split('<!-- This is an auto-generated description by cubic. -->').length, 2)
  assert.equal(mergePrBody(healed, UPDATE).body, healed)
})

test('untouched sections, preamble and the bot block keep their place; new sections append', () => {
  const update =
    '## Review coverage\nnone: review did not run (disabled)\n\n## Review follow-ups\n- [ ] file a ticket\n'
  const body = mergePrBody(PUBLISHED, update).body
  assert.deepEqual(
    parsePrBody(body).sections.map((s) => s.heading),
    [
      '## Acceptance criteria',
      '## Local verification',
      '## Autonomous decisions',
      '## Human follow-up',
      '## Open questions',
      '## Host notes',
      '## Unapplied review findings',
      '## Cross-model review',
      '## Review coverage',
      '## Review follow-ups',
    ],
  )
  assert.ok(
    body.startsWith(
      'Linear issue: https://tracker.example/ISSUE-1\n\nSession details now show a timeline.\n\n',
    ),
  )
  assert.ok(body.trimEnd().endsWith(CUBIC))
  assert.equal(
    sectionText(body, 'local verification'),
    sectionText(PUBLISHED, 'local verification'),
  )
})

test('a non-blank update preamble replaces the published one', () => {
  const body = mergePrBody(
    PUBLISHED,
    'Linear issue: https://tracker.example/ISSUE-2\n\n## Open questions\n- none\n',
  ).body
  assert.ok(
    body.startsWith('Linear issue: https://tracker.example/ISSUE-2\n\n## Acceptance criteria'),
  )
})

test('an unterminated fence never swallows a later section, on any pass', () => {
  const inUpdate = mergeTwice('## B\nold\n\n## C\nkeep me\n', '## B\n```\nx\n')
  assert.equal(sectionText(inUpdate.body, 'c'), 'keep me')
  assert.equal(sectionText(inUpdate.body, 'b'), '```\nx\n```')
  assert.equal(inUpdate.warnings.length, 1)

  const inPreamble = mergeTwice('## B\nold\n', 'Linear issue: x\n```\n')
  assert.equal(sectionText(inPreamble.body, 'b'), 'old')

  const inBody = mergeTwice('## A\n```\nfoo\n## B\nold\n', '## B\nnew\n')
  assert.equal(headingCounts(inBody.body).b, 1)
  assert.equal(sectionText(inBody.body, 'b'), 'new')
})

test('a bot opener without its own closer is ordinary text', () => {
  const body = `## A\na\n${OPEN}\n## B\nb\n\n${OPEN}\nbot\n${CLOSE}\n`
  const { body: merged } = mergeTwice(body, `## A\nnew a\n\n${OPEN}\nbot2\n${CLOSE}\n`)
  assert.equal(sectionText(merged, 'b'), 'b')
  assert.equal(merged.split(CLOSE).length, 2)
  assert.ok(merged.includes(`${OPEN}\nbot2\n${CLOSE}`))
})

test('bot markers quoted inside a fence stay in the fence', () => {
  const body = `## Notes\n\`\`\`\n${OPEN}\nexample\n${CLOSE}\n\`\`\`\nafter\n`
  const { body: merged } = mergeTwice(body, '## Host notes\n- x\n')
  assert.equal(sectionText(merged, 'notes'), `\`\`\`\n${OPEN}\nexample\n${CLOSE}\n\`\`\`\nafter`)
})

test('a heading with closing hashes is the same section', () => {
  const { body } = mergeTwice('## Host notes ##\nold\n', '## Host notes\nnew\n')
  assert.deepEqual(headingCounts(body), { 'host notes': 1 })
})

test('heading match ignores case, spacing and a trailing colon', () => {
  const body = mergePrBody(PUBLISHED, '##   host NOTES:\n- replaced\n').body
  assert.equal(headingCounts(body)['host notes'], 1)
  assert.equal(sectionText(body, 'host notes'), '- replaced')
})

test('a ## line inside a fenced block is content, not a section', () => {
  const published = '## Local verification\n```\n## Host notes\n```\n- `make test` → ok\n'
  const body = mergePrBody(published, '## Host notes\n- real\n').body
  assert.deepEqual(
    parsePrBody(body).sections.map((s) => s.heading),
    ['## Local verification', '## Host notes'],
  )
  assert.ok(body.includes('```\n## Host notes\n```'))
})

test('a bot block in the update replaces the published one instead of stacking', () => {
  const fresh = CUBIC.replace('Reworks the details view.', 'Reworks it again.')
  const body = mergePrBody(PUBLISHED, `## Open questions\n- none\n\n${fresh}\n`).body
  assert.equal(body.split('<!-- This is an auto-generated description by cubic. -->').length, 2)
  assert.ok(body.includes('Reworks it again.'))
  assert.equal(mergePrBody(body, `## Open questions\n- none\n\n${fresh}\n`).body, body)
})

test('an update that repeats a heading warns and uses the first copy', () => {
  const { body, warnings } = mergePrBody(
    PUBLISHED,
    '## Host notes\n- first\n\n## Host notes\n- second\n',
  )
  assert.equal(sectionText(body, 'host notes'), '- first')
  assert.equal(warnings.length, 1)
})

test('CLI merges files and writes --out', () => {
  const files = { '/cur.md': PUBLISHED, '/upd.md': UPDATE }
  const fs = {
    readFileSync: (path) => {
      if (!(path in files)) throw new Error(`ENOENT ${path}`)
      return files[path]
    },
    writeFileSync: (path, text) => {
      files[path] = text
    },
  }
  const err = []
  const code = main(
    ['merge', '--body-file', '/cur.md', '--update-file', '/upd.md', '--out', '/out.md'],
    {
      fs,
      stdout: () => {},
      stderr: (text) => err.push(text),
    },
  )
  assert.equal(code, 0)
  assert.equal(files['/out.md'], mergePrBody(PUBLISHED, UPDATE).body)
  assert.deepEqual(err, [])
  assert.equal(
    main(['merge', '--body-file', '/cur.md'], { fs, stdout: () => {}, stderr: () => {} }),
    2,
  )
  assert.equal(
    main(['merge', '--body-file', '/missing.md', '--update-file', '/upd.md'], {
      fs,
      stdout: () => {},
      stderr: () => {},
    }),
    1,
  )
})

test('a nested bot block stays whole inside its outer block, on every pass', () => {
  const nested = [OPEN, 'outer summary line', OPEN, 'inner', CLOSE, 'outer tail', CLOSE].join('\n')
  const once = mergeTwice(`## A\na\n${nested}\n## B\nb\n`, '').body
  assert.equal(once, `## A\na\n\n## B\nb\n\n${nested}\n`)
  assert.equal(mergeTwice(once, '## A\nnew a\n').body, `## A\nnew a\n\n## B\nb\n\n${nested}\n`)
})

test('drop removes every copy of a section the update no longer renders', () => {
  const corrupted = PUBLISHED.replace(
    '## Cross-model review',
    '## Unapplied review findings\n- [ ] **P2** stale copy\n\n## Cross-model review',
  )
  const { body, warnings } = mergeTwice(corrupted, UPDATE, {
    drop: ['## Unapplied review findings'],
  })
  assert.equal(headingCounts(body)['unapplied review findings'], undefined)
  assert.ok(!body.includes('- [ ]'))
  assert.equal(sectionText(body, 'host notes'), sectionText(UPDATE, 'host notes'))
  assert.deepEqual(warnings, [])

  const kept = mergePrBody(PUBLISHED, UPDATE, { drop: ['host notes'] })
  assert.equal(sectionText(kept.body, 'host notes'), sectionText(UPDATE, 'host notes'))
  assert.equal(kept.warnings.length, 1)
  assert.match(kept.warnings[0], /Host notes/)
})

test('unnamed duplicate sections that still differ are named in a warning', () => {
  const body =
    '## Review coverage\nfull\n\n## Host notes\nold\n\n## Review coverage\nfull, then junk\n'
  const { warnings } = mergeTwice(body, '## Host notes\nnew\n')
  assert.equal(warnings.length, 1)
  assert.match(warnings[0], /"## Review coverage" survives in 2 copies/)

  const identical = '## Review coverage\nfull\n\n## Host notes\nold\n\n## Review coverage\nfull\n'
  const healed = mergePrBody(identical, '## Host notes\nnew\n')
  assert.deepEqual(healed.warnings, [])
  assert.equal(headingCounts(healed.body)['review coverage'], 1)
})

test('CLI passes --drop through and reports surviving duplicates on stderr', () => {
  const files = {
    '/cur.md': `${PUBLISHED}\n## Review coverage\nstale\n`,
    '/upd.md': '## Open questions\n- none\n',
  }
  const fs = {
    readFileSync: (path) => files[path],
    writeFileSync: (path, text) => {
      files[path] = text
    },
  }
  const err = []
  const argv = ['merge', '--body-file', '/cur.md', '--update-file', '/upd.md', '--out', '/out.md']
  const code = main([...argv, '--drop', 'Host notes', '--drop', 'Unapplied review findings'], {
    fs,
    stdout: () => {},
    stderr: (text) => err.push(text),
  })
  assert.equal(code, 0)
  assert.equal(headingCounts(files['/out.md'])['host notes'], undefined)
  assert.equal(headingCounts(files['/out.md'])['unapplied review findings'], undefined)
  assert.deepEqual(err, [
    'pr-body: warning: "## Review coverage" survives in 2 copies with different content; render it in the update to replace them\n',
  ])
})

test('review notes replace in place and drop when the renderer no longer emits them', () => {
  const published = '## Review notes\nold note\n\n## Review coverage\nfull\n'
  const update =
    '## Review notes\n1 note — notes never block\n\n<details><summary>Notes</summary>\n\n- **P3** useful note\n\n</details>\n'
  const merged = mergeTwice(published, update).body
  assert.deepEqual(
    parsePrBody(merged).sections.map((s) => s.key),
    ['review notes', 'review coverage'],
  )
  assert.equal(sectionText(merged, 'review notes'), sectionText(update, 'review notes'))
  const dropped = mergeTwice(merged, '## Review coverage\nfull\n', {
    drop: ['## Review notes'],
  }).body
  assert.equal(sectionText(dropped, 'review notes'), undefined)
  assert.equal(sectionText(dropped, 'review coverage'), 'full')
})
