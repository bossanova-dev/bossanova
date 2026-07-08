import assert from 'node:assert/strict'
import test from 'node:test'
import {
  buildPrompt,
  dedupeTicketIds,
  entryFilename,
  linearIdFromBranch,
  parsePrNumbers,
  previousVersionTag,
  releaseDate,
  renderFrontmatter,
  resolveRepo,
  stripHtmlTags,
  summaryFromBody,
} from './lib.mjs'

test('resolveRepo returns GITHUB_REPOSITORY when set', () => {
  assert.equal(resolveRepo({ GITHUB_REPOSITORY: 'recurser/bossanova' }), 'recurser/bossanova')
  assert.equal(resolveRepo({ GITHUB_REPOSITORY: 'owner/other' }), 'owner/other')
})

test('resolveRepo trims surrounding whitespace on the env value', () => {
  assert.equal(resolveRepo({ GITHUB_REPOSITORY: '  recurser/bossanova\n' }), 'recurser/bossanova')
})

test('resolveRepo falls back to the canonical private repo when unset or empty', () => {
  assert.equal(resolveRepo({}), 'recurser/bossanova')
  assert.equal(resolveRepo(), 'recurser/bossanova')
  assert.equal(resolveRepo({ GITHUB_REPOSITORY: '' }), 'recurser/bossanova')
  assert.equal(resolveRepo({ GITHUB_REPOSITORY: '   ' }), 'recurser/bossanova')
})

test('releaseDate prefers published_at, then created_at', () => {
  assert.equal(
    releaseDate({
      published_at: '2024-01-02T03:04:05Z',
      created_at: '2023-12-31T00:00:00Z',
    }).toISOString(),
    '2024-01-02T03:04:05.000Z',
  )
  assert.equal(
    releaseDate({ published_at: null, created_at: '2023-12-31T00:00:00Z' }).toISOString(),
    '2023-12-31T00:00:00.000Z',
  )
  // A malformed published_at must fall through to a valid created_at, not skip
  // straight to `now` (regression guard for the `||`-before-parse pitfall).
  assert.equal(
    releaseDate({ published_at: 'not-a-date', created_at: '2023-12-31T00:00:00Z' }).toISOString(),
    '2023-12-31T00:00:00.000Z',
  )
})

test('releaseDate falls back to now for a missing/absent/malformed release', () => {
  const now = new Date('2025-07-05T12:00:00Z')
  // null → release could not be fetched (non-blocking fallback)
  assert.equal(releaseDate(null, now).toISOString(), now.toISOString())
  // present release object but no timestamps
  assert.equal(releaseDate({}, now).toISOString(), now.toISOString())
  // unparseable timestamp must not yield an Invalid Date (which would throw
  // downstream in renderFrontmatter's toISOString())
  assert.equal(releaseDate({ published_at: 'not-a-date' }, now).toISOString(), now.toISOString())
})

test('linearIdFromBranch extracts BOS id case-insensitively', () => {
  assert.equal(linearIdFromBranch('dave/bos-88-add-a-changelog-page'), 'BOS-88')
  assert.equal(linearIdFromBranch('feature/BOS-12-thing'), 'BOS-12')
  assert.equal(linearIdFromBranch('dependabot/npm_and_yarn/foo'), null)
  assert.equal(linearIdFromBranch(''), null)
})

test('entryFilename strips leading v and appends .md', () => {
  assert.equal(entryFilename('1.56.0'), '1.56.0.md')
  assert.equal(entryFilename('v1.57.1'), '1.57.1.md')
})

test('parsePrNumbers reads this repo [#NN] tags and GitHub (#NN) suffixes', () => {
  // This repo's commit policy: type(scope): [#NN] subject.
  assert.deepEqual(parsePrNumbers(['feat(marketing): [#896] add changelog page']), ['896'])
  // GitHub squash-merge default suffix.
  assert.deepEqual(parsePrNumbers(['Add changelog page (#896)']), ['896'])
  // The scope parens must not be mistaken for a PR ref (no leading #).
  assert.deepEqual(parsePrNumbers(['chore(deps): bump astro from 6 to 7']), [])
  // De-duplicated, order-preserving across many commits.
  assert.deepEqual(
    parsePrNumbers(['fix(boss): [#927] a', 'fix(boss): [#927] b', 'feat: c (#930)']),
    ['927', '930'],
  )
})

test('renderFrontmatter emits required keys with ISO date', () => {
  const fm = renderFrontmatter({
    version: '1.56.0',
    date: new Date('2026-06-20T14:03:00Z'),
    title: 'v1.56.0',
    summary: 'Faster startup.',
  })
  assert.match(fm, /^---\n/)
  assert.match(fm, /\n---\n$/)
  assert.match(fm, /version: '1\.56\.0'/)
  assert.match(fm, /date: 2026-06-20T14:03:00\.000Z/)
  assert.match(fm, /title: 'v1\.56\.0'/)
  assert.match(fm, /summary: 'Faster startup\.'/)
})

test('renderFrontmatter escapes single quotes in summary', () => {
  const fm = renderFrontmatter({
    version: '1.0.0',
    date: new Date('2026-01-01T00:00:00Z'),
    title: 'v1.0.0',
    summary: "it's here",
  })
  assert.match(fm, /summary: 'it''s here'/)
})

test('dedupeTicketIds is unique, order-preserving, uppercased', () => {
  assert.deepEqual(dedupeTicketIds(['BOS-1', 'bos-1', 'BOS-2']), ['BOS-1', 'BOS-2'])
})

test('buildPrompt includes version, PR titles, and ticket context', () => {
  const prompt = buildPrompt({
    version: '1.56.0',
    prs: [{ number: 1, title: 'feat: faster startup', headRefName: 'dave/bos-1-startup' }],
    tickets: [{ id: 'BOS-1', title: 'Speed up startup', description: 'plan...' }],
    commits: ['fix: typo'],
  })
  assert.match(prompt, /1\.56\.0/)
  assert.match(prompt, /faster startup/)
  assert.match(prompt, /Speed up startup/)
  assert.match(prompt, /product-focused/i)
  assert.match(prompt, /Translate implementation details into outcomes/i)
})

test('buildPrompt targets nontechnical product outcomes in a Conductor-like voice', () => {
  const prompt = buildPrompt({ version: '1.67.0', prs: [], tickets: [], commits: [] })
  assert.match(prompt, /zero technical background/i)
  assert.match(prompt, /superficial\s+knowledge/i)
  assert.match(prompt, /outcomes? a user can\s+see, do, or feel/i)
  assert.match(prompt, /Conductor/i)
  assert.match(prompt, /You can now/i)
  assert.match(prompt, /Fixed an issue where/i)
  assert.match(prompt, /avoid.*(RPC|daemon|endpoint|JSONL|read model|bridge|implementation)/is)
})

test('buildPrompt tells the model to skip trivial copy-only changes', () => {
  const prompt = buildPrompt({ version: '1.67.0', prs: [], tickets: [], commits: [] })
  assert.match(prompt, /trivial copy/i)
  assert.match(prompt, /not news/i)
  assert.match(prompt, /Merging PR/i)
  assert.match(prompt, /esc to return to list/i)
})

test('buildPrompt enforces the friendly, no-Highlights, no-em-dash voice', () => {
  const prompt = buildPrompt({ version: '1.56.0', prs: [], tickets: [], commits: [] })
  assert.match(prompt, /Friendly, clear, and matter-of-fact/)
  assert.match(prompt, /Highlights/)
  // Em dashes are banned in generated copy.
  assert.match(prompt, /em dash/i)
  // No marketing hype.
  assert.match(prompt, /do not sell|Ban hype/i)
})

test('summaryFromBody strips list and bold markdown from the first content line', () => {
  const body = '### Features\n\n- **Model selection.** Choose the model per session.\n'
  assert.equal(summaryFromBody(body, 'fallback'), 'Model selection. Choose the model per session.')
})

test('summaryFromBody skips headings and falls back when empty', () => {
  assert.equal(summaryFromBody('### Features\n\n', 'Release v1.0.0'), 'Release v1.0.0')
  assert.equal(summaryFromBody('', 'Release v1.0.0'), 'Release v1.0.0')
})

test('summaryFromBody caps length at 140 characters', () => {
  const long = `### Features\n\n- ${'x'.repeat(200)}\n`
  assert.equal(summaryFromBody(long, 'fallback').length, 140)
})

test('previousVersionTag finds the next-older stable tag by semver, ignoring list order', () => {
  // Deliberately unsorted, with prereleases interleaved — mirrors what the
  // GitHub list-tags API may return (order is not documented as semver-sorted).
  const tags = ['v1.55.0', 'v1.57.0', 'v1.56.0-staging.1', 'v1.9.0', 'v1.56.0', 'v1.10.0']
  assert.equal(previousVersionTag(tags, 'v1.57.0'), 'v1.56.0')
  // numeric (not lexical) ordering: 1.10.0 > 1.9.0
  assert.equal(previousVersionTag(tags, 'v1.10.0'), 'v1.9.0')
  assert.equal(previousVersionTag(tags, '1.56.0'), 'v1.55.0')
})

test('previousVersionTag returns null for the oldest tag or an unknown tag', () => {
  const tags = ['v1.2.0', 'v1.1.0']
  assert.equal(previousVersionTag(tags, 'v1.1.0'), null)
  assert.equal(previousVersionTag(tags, 'v9.9.9'), null)
})

test('stripHtmlTags removes script/style blocks and HTML tags from model output', () => {
  assert.equal(stripHtmlTags('<script>alert(1)</script>hello'), 'hello')
  assert.equal(stripHtmlTags('<style>body{}</style>x'), 'x')
  assert.equal(stripHtmlTags('a <b>bold</b> word'), 'a bold word')
  assert.equal(stripHtmlTags('<img src=x onerror=alert(1)>caption'), 'caption')
})

test('stripHtmlTags preserves markdown, inequalities, and autolinks', () => {
  assert.equal(stripHtmlTags('- **bold** and _em_'), '- **bold** and _em_')
  assert.equal(stripHtmlTags('if a < b and c > d'), 'if a < b and c > d')
  // Markdown autolink: the URL text survives (it is not an HTML tag).
  assert.equal(stripHtmlTags('see <https://example.com> now'), 'see <https://example.com> now')
})
