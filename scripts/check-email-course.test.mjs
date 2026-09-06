// Unit coverage for scripts/check-email-course.mjs.
//
// Every rule group the checker enforces gets a fixture that FAILS it, because a
// checker that silently passes everything is worse than no checker: it reads as
// evidence in CI while guarding nothing. The baseline fixture below passes all
// nine groups, and each test mutates exactly one thing.
//
// The final test runs the real checker over the real docs/email-course/, so
// `make test-scripts` guards the shipped course as well as the code that checks it.

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  bannedPhraseRegex,
  buildRoutesFromFiles,
  checkCourse,
  countFencedBlocks,
  countWords,
  findRepoRoot,
  normaliseRoute,
  headingSlugs,
  parseGoStringConstant,
  parseBannedPhrases,
  parsePublishedSkills,
  proseSentences,
  proseOf,
  run,
  sectionBody,
  slugifyHeading,
  ALLOWED_LINK_HOSTS,
  NON_EVENT_SNAKE_TOKENS,
  TRIAL_ENROLLMENT_GO,
  TRIAL_EVENT_SYMBOL,
} from './check-email-course.mjs'

// ---------------------------------------------------------------------------
// Fixtures

const OFFSETS = [0, 1, 2, 3, 4, 5, 6, 12]
const FILLER = ['alpha', 'bravo', 'charlie', 'delta', 'echo', 'foxtrot']

const filler = (n) => Array.from({ length: n }, (_, i) => FILLER[i % FILLER.length]).join(' ')

const bodyName = (offset) => `day-${offset}-fixture.md`
const subjectFor = (offset) => `Fixture subject for day ${offset}`

function body(offset, { words = 400, fenced = true, extra = '' } = {}) {
  const lines = [`# Day ${offset}`, '', `**Subject:** ${subjectFor(offset)}`, '', filler(words)]
  if (extra) lines.push('', extra)
  if (fenced) lines.push('', '```bash', 'boss ls --state fixing_checks', '```')
  return lines.join('\n') + '\n'
}

// The event the fixture's Go constant is pretending to hold. The checker compares
// the guide against an INJECTED value, so this string is the whole of the Go side
// here -- the real constant is exercised by the shipped-course test at the bottom.
const EVENT = 'trial_started'

function readme(
  offsets = OFFSETS,
  { trialClaim = true, loopsSetup = true, triggeringEvent = EVENT } = {},
) {
  const lines = ['# Trial onboarding email course', '']
  if (trialClaim) lines.push('The product trial is 14 days.', '')
  for (const offset of offsets) {
    lines.push(
      `### Day ${offset}`,
      '',
      `Subject: ${subjectFor(offset)}`,
      '',
      `Send offset: ${offset} ${offset === 1 ? 'day' : 'days'} after the event`,
      '',
    )
  }
  if (loopsSetup) {
    lines.push('## Loops setup', '')
    if (triggeringEvent !== null) lines.push(`- Triggering event: \`${triggeringEvent}\``, '')
  }
  return lines.join('\n')
}

function baselineFiles(overrides = {}) {
  const files = new Map()
  for (const offset of OFFSETS) {
    files.set(bodyName(offset), body(offset, offset === 12 ? { words: 200, fenced: false } : {}))
  }
  files.set('README.md', readme())
  files.set('VOICE.md', '# Voice guide\n\nThe rubric lives here.\n')
  for (const [name, content] of Object.entries(overrides)) {
    if (content === null) files.delete(name)
    else files.set(name, content)
  }
  return files
}

const ROUTES = new Map([
  ['guides/pr-lifecycle', new Set(['steps', 'where-to-look-when-something-stalls'])],
  ['skills', new Set(['the-skill-suite'])],
  ['quick-start', new Set()],
])

const PUBLISHED = ['boss', 'boss-build', 'boss-epic', 'boss-plan', 'boss-review']

const check = (overrides = {}) =>
  checkCourse({
    files: baselineFiles(overrides),
    routes: ROUTES,
    publishedSkills: PUBLISHED,
    trialEnrolmentEvent: EVENT,
  })

const rulesIn = (violations) => new Set(violations.map((v) => v.rule))

// ---------------------------------------------------------------------------
// Baseline

test('baseline fixture passes every rule', () => {
  assert.deepEqual(check(), [])
})

// ---------------------------------------------------------------------------
// structure

test('structure: a missing body fails', () => {
  const violations = check({ [bodyName(3)]: null })
  assert.ok(rulesIn(violations).has('structure'))
  assert.ok(
    violations.some((v) => /expected 8 email bodies, found 7/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('structure: a wrong send offset fails', () => {
  const files = baselineFiles({ [bodyName(6)]: null })
  files.set('day-9-fixture.md', body(9))
  const violations = checkCourse({
    files,
    routes: ROUTES,
    publishedSkills: PUBLISHED,
    trialEnrolmentEvent: EVENT,
  })
  assert.ok(
    violations.some(
      (v) =>
        v.rule === 'structure' && /send offsets are \[0, 1, 2, 3, 4, 5, 9, 12\]/.test(v.message),
    ),
    JSON.stringify(violations),
  )
})

test('structure: a body with no Subject line fails', () => {
  const violations = check({
    [bodyName(2)]: body(2).replace(/^\*\*Subject:\*\*.*$/m, 'Subject-ish line'),
  })
  assert.ok(
    violations.some((v) => v.rule === 'structure' && /no `\*\*Subject:\*\*` line/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('structure: a subject the README does not record fails', () => {
  const violations = check({
    [bodyName(4)]: body(4).replace(subjectFor(4), 'A subject nobody wrote down'),
  })
  assert.ok(
    violations.some((v) => v.rule === 'structure' && /A subject nobody wrote down/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('structure: an offset the README does not record fails', () => {
  const violations = check({
    'README.md': readme().replace('Send offset: 5 days', 'Send offset: 50 days'),
  })
  assert.ok(
    violations.some((v) => v.rule === 'structure' && /Send offset: 5 day\(s\)/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('structure: a missing VOICE.md fails', () => {
  const violations = check({ 'VOICE.md': null })
  assert.ok(
    violations.some((v) => v.rule === 'structure' && v.file === 'VOICE.md'),
    JSON.stringify(violations),
  )
})

// ---------------------------------------------------------------------------
// length

test('length: a teaching send under 350 prose words fails', () => {
  const violations = check({ [bodyName(1)]: body(1, { words: 200 }) })
  assert.ok(
    violations.some(
      (v) => v.rule === 'length' && v.file === bodyName(1) && /200 prose words/.test(v.message),
    ),
    JSON.stringify(violations),
  )
})

test('length: a teaching send over 450 prose words fails', () => {
  const violations = check({ [bodyName(1)]: body(1, { words: 600 }) })
  assert.ok(
    violations.some((v) => v.rule === 'length' && /600 prose words/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('length: the day-12 recap over 250 words fails', () => {
  const violations = check({ [bodyName(12)]: body(12, { words: 400, fenced: false }) })
  assert.ok(
    violations.some(
      (v) => v.rule === 'length' && v.file === bodyName(12) && /capped at 250/.test(v.message),
    ),
    JSON.stringify(violations),
  )
})

test('length: a fenced block does not inflate the word count', () => {
  const big = '```\n' + filler(500) + '\n```'
  assert.deepEqual(check({ [bodyName(1)]: body(1, { extra: big }) }), [])
})

// ---------------------------------------------------------------------------
// concreteness

test('concreteness: a teaching send with no fenced block fails', () => {
  const violations = check({ [bodyName(5)]: body(5, { fenced: false }) })
  assert.ok(
    violations.some((v) => v.rule === 'concreteness' && v.file === bodyName(5)),
    JSON.stringify(violations),
  )
})

test('concreteness: the day-12 recap is exempt', () => {
  assert.deepEqual(
    check({ [bodyName(12)]: body(12, { words: 200, fenced: false }) }).filter(
      (v) => v.rule === 'concreteness',
    ),
    [],
  )
})

// ---------------------------------------------------------------------------
// voice

test('voice: the Try this now stamp fails', () => {
  const violations = check({ [bodyName(0)]: body(0, { extra: 'Try this now: run it.' }) })
  assert.ok(
    violations.some((v) => v.rule === 'voice' && /try this now/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('voice: a marketing verb fails', () => {
  const violations = check({
    [bodyName(0)]: body(0, { extra: 'This will unlock a seamless workflow.' }),
  })
  const messages = violations.filter((v) => v.rule === 'voice').map((v) => v.message)
  assert.ok(
    messages.some((m) => /"unlock"/.test(m)),
    messages.join(' | '),
  )
  assert.ok(
    messages.some((m) => /"seamless"/.test(m)),
    messages.join(' | '),
  )
})

test('voice: a hedge fails', () => {
  const violations = check({
    [bodyName(0)]: body(0, { extra: 'You can do this, and it is optional.' }),
  })
  const messages = violations.filter((v) => v.rule === 'voice').map((v) => v.message)
  assert.ok(
    messages.some((m) => /"can"/.test(m)),
    messages.join(' | '),
  )
  assert.ok(
    messages.some((m) => /"optional"/.test(m)),
    messages.join(' | '),
  )
})

test('voice: a multi-word ban survives a paragraph rewrap', () => {
  const violations = check({ [bodyName(0)]: body(0, { extra: 'only if\nneeded' }) })
  assert.ok(
    violations.some((v) => v.rule === 'voice' && /"if needed"/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('voice: pasted output inside a fenced block is evidence, not voice', () => {
  assert.deepEqual(
    check({ [bodyName(0)]: body(0, { extra: '```\nyou can leverage this\n```' }) }),
    [],
  )
})

test('bannedPhraseRegex treats an apostrophe as part of the word', () => {
  const re = () => bannedPhraseRegex('can')
  assert.equal(re().test('you can go'), true)
  assert.equal(re().test('it cannot go'), false)
  assert.equal(re().test("it can't go"), false)
  assert.equal(re().test('a canvas'), false)
})

test('voice: shared Vale rule supplies the banned register', () => {
  assert.deepEqual(parseBannedPhrases('tokens:\n  - unlock\n  - if needed\n'), [
    'unlock',
    'if needed',
  ])
  assert.throws(() => parseBannedPhrases('tokens: []\n'), /non-empty tokens list/)
  assert.throws(
    () => parseBannedPhrases('tokens:\n  - unlock # comment\n'),
    /unquoted lowercase words/,
  )
  assert.throws(() => parseBannedPhrases('tokens:\n  - "if needed"\n'), /unquoted lowercase words/)
})

// ---------------------------------------------------------------------------
// punctuation

test('punctuation: two em dashes in one sentence fail', () => {
  const violations = check({
    [bodyName(2)]: body(2, { extra: 'One thought — with one aside — keeps going.' }),
  })
  assert.ok(
    violations.some(
      (v) => v.rule === 'punctuation' && /2 em dashes in one sentence/.test(v.message),
    ),
    JSON.stringify(violations),
  )
})

test('punctuation: abbreviation periods do not hide two dashes in one sentence', () => {
  const violations = check({
    [bodyName(2)]: body(2, { extra: 'I tried one — e.g. this — and stopped.' }),
  })
  assert.ok(
    violations.some(
      (v) => v.rule === 'punctuation' && /2 em dashes in one sentence/.test(v.message),
    ),
    JSON.stringify(violations),
  )
})

test('punctuation: spaced initials do not hide two dashes in one sentence', () => {
  const violations = check({
    [bodyName(2)]: body(2, {
      extra: 'I tried one — with J. R. R. Tolkien — and stopped.',
    }),
  })
  assert.ok(
    violations.some(
      (v) => v.rule === 'punctuation' && /2 em dashes in one sentence/.test(v.message),
    ),
    JSON.stringify(violations),
  )
})

test('punctuation: a sentence-final abbreviation keeps the next sentence separate', () => {
  const prose = 'Keep the note — with logs, links, etc. Start the next thought — with one aside.'
  assert.deepEqual(proseSentences(prose), [
    'Keep the note — with logs, links, etc.',
    'Start the next thought — with one aside.',
  ])
})

test('punctuation: separate spoken asides pass', () => {
  assert.deepEqual(
    check({
      [bodyName(2)]: body(2, {
        extra: 'One thought — with one aside. Another thought — with another aside.',
      }),
    }),
    [],
  )
})

test('punctuation: paired structural asides and separate list items pass', () => {
  assert.deepEqual(
    check({
      [bodyName(2)]: body(2, {
        extra: [
          'Run it — `boss repo ls` prints the id — and leave it alone.',
          'Use the lenses — Go, web, database, API — in their own contexts.',
          '- First link — one description',
          '- Second link — another description',
        ].join('\n'),
      }),
    }),
    [],
  )
})

// ---------------------------------------------------------------------------
// skill names

test('skill-names: an unpublished skill fails', () => {
  const violations = check({
    [bodyName(2)]: body(2, { extra: 'Run boss-notreal on the branch.' }),
  })
  assert.ok(
    violations.some((v) => v.rule === 'skill-names' && /`boss-notreal`/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('skill-names: an unpublished skill fails even in the README', () => {
  const violations = check({ 'README.md': readme() + '\nSee boss-notreal.\n' })
  assert.ok(
    violations.some((v) => v.rule === 'skill-names' && v.file === 'README.md'),
    JSON.stringify(violations),
  )
})

test('skill-names: published skills pass', () => {
  assert.deepEqual(
    check({ [bodyName(2)]: body(2, { extra: 'Run boss-plan then boss-build.' }) }),
    [],
  )
})

test("parsePublishedSkills reads the manifest test's own want slice", () => {
  const source = [
    'func TestEmbeddedSkillManifestExcludesBossProof(t *testing.T) {',
    '\twant := []string{',
    '\t\t"boss",',
    '\t\t"boss-build",',
    '\t}',
    '}',
    'func TestSomethingElse(t *testing.T) {',
    '\twant := []string{',
    '\t\t"boss-unpublished",',
    '\t}',
    '}',
  ].join('\n')
  assert.deepEqual(parsePublishedSkills(source), ['boss', 'boss-build'])
})

test('parsePublishedSkills fails closed when the anchor moves', () => {
  assert.throws(
    () => parsePublishedSkills('func Other() {\n\twant := []string{"boss"}\n}'),
    /TestEmbeddedSkillManifestExcludesBossProof not found/,
  )
})

test('parsePublishedSkills agrees with the real manifest test', () => {
  const repoRoot = findRepoRoot(path.dirname(fileURLToPath(import.meta.url)))
  const source = fs.readFileSync(
    path.join(repoRoot, 'services/boss/internal/skillinstall/skills_manifest_test.go'),
    'utf8',
  )
  const skills = parsePublishedSkills(source)
  assert.deepEqual(skills, [
    'boss',
    'boss-build',
    'boss-epic',
    'boss-finalize',
    'boss-plan',
    'boss-repair',
    'boss-review',
    'boss-verify',
  ])
})

// ---------------------------------------------------------------------------
// links

test('links: a URL with no matching page fails', () => {
  const violations = check({
    [bodyName(1)]: body(1, { extra: 'See https://docs.bossanova.dev/guides/nowhere here.' }),
  })
  assert.ok(
    violations.some((v) => v.rule === 'links' && /guides\/nowhere/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('links: a URL with no matching heading anchor fails', () => {
  const violations = check({
    [bodyName(1)]: body(1, {
      extra: 'See https://docs.bossanova.dev/guides/pr-lifecycle#no-such-heading here.',
    }),
  })
  assert.ok(
    violations.some((v) => v.rule === 'links' && /names no heading/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('links: a live page and anchor pass', () => {
  assert.deepEqual(
    check({
      [bodyName(1)]: body(1, {
        extra: 'See https://docs.bossanova.dev/guides/pr-lifecycle#steps here.',
      }),
    }),
    [],
  )
})

test('links: a relative link to a missing course file fails', () => {
  const violations = check({
    'README.md': readme() + '\nBody: [day-9-gone.md](./day-9-gone.md)\n',
  })
  assert.ok(
    violations.some((v) => v.rule === 'links' && /day-9-gone\.md names no file/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('links: a relative link to a present course file passes', () => {
  assert.deepEqual(
    check({
      'README.md': readme() + `\nBody: [${bodyName(2)}](./${bodyName(2)})\n`,
    }),
    [],
  )
})

test('links: a same-page anchor naming no heading fails', () => {
  const violations = check({
    'README.md': readme() + '\nSee [the setup](#no-such-heading).\n',
  })
  assert.ok(
    violations.some((v) => v.rule === 'links' && /names no heading in README\.md/.test(v.message)),
    JSON.stringify(violations),
  )
})

test("links: a same-page anchor for one of the file's own headings passes", () => {
  // The two shapes the shipped README actually uses.
  assert.deepEqual(
    check({
      'README.md': readme() + '\nSee [Loops setup](#loops-setup) and [day 3](#day-3).\n',
    }),
    [],
  )
})

test('links: an anchor resolves through the same slug rules as the heading', () => {
  assert.deepEqual(
    check({
      'README.md': readme() + '\n## `boss build` & the CLI\n\nSee [above](#boss-build--the-cli).\n',
    }),
    [],
  )
})

test('headingSlugs ignores a heading pasted inside a fenced block', () => {
  const slugs = headingSlugs('# Real\n\n```\n# Not a heading\n```\n\n### Also real\n')
  assert.deepEqual([...slugs].sort(), ['also-real', 'real'])
})

test('links: an absolute URL to any other host fails, naming the host', () => {
  const violations = check({
    [bodyName(1)]: body(1, { extra: 'See https://example.com/thing for the rest.' }),
  })
  assert.ok(
    violations.some((v) => v.rule === 'links' && /example\.com/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('links: a reintroduced Linear ticket URL fails', () => {
  // The specific regression. The guide linked trial readers at two linear.app
  // tickets while the event name was unsettled; a reader outside the company
  // cannot open either one.
  const violations = check({
    'README.md': readme() + '\nTracked in [BOS-974](https://linear.app/x/issue/BOS-974).\n',
  })
  assert.ok(
    violations.some((v) => v.rule === 'links' && /linear\.app/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('links: the documentation host itself is the allowlist', () => {
  assert.deepEqual(ALLOWED_LINK_HOSTS, ['docs.bossanova.dev'])
})

test('buildRoutesFromFiles lets a front-matter slug override the file path', () => {
  const routes = buildRoutesFromFiles(
    new Map([
      ['guides/agent-plugins.md', '---\nslug: /guides/agent-runners\n---\n\n# Agent Runners\n'],
      ['guides/logging.md', '---\ntitle: Logging\n---\n\n## Where logs live\n'],
      ['intro.mdx', '---\nslug: /\n---\n\n# Intro\n'],
    ]),
  )
  assert.ok(routes.has('guides/agent-runners'), [...routes.keys()].join(', '))
  assert.equal(routes.has('guides/agent-plugins'), false)
  assert.ok(routes.has('guides/logging'))
  assert.ok(routes.get('guides/logging').has('where-logs-live'))
  assert.ok(routes.has(''), 'a root slug resolves to the empty route')
})

test('slugifyHeading strips markup and honours an explicit id', () => {
  assert.equal(slugifyHeading('Gate command'), 'gate-command')
  assert.equal(slugifyHeading('`boss callback add` — the CLI'), 'boss-callback-add--the-cli')
  assert.equal(slugifyHeading('Anything {#custom-anchor}'), 'custom-anchor')
})

test('normaliseRoute strips surrounding slashes', () => {
  assert.equal(normaliseRoute('/guides/web/'), 'guides/web')
  assert.equal(normaliseRoute('/'), '')
})

// ---------------------------------------------------------------------------
// numbers

test('numbers: a price string fails', () => {
  const violations = check({
    [bodyName(12)]: body(12, { words: 200, fenced: false, extra: 'It is $9 a month.' }),
  })
  assert.ok(
    violations.some((v) => v.rule === 'numbers' && /price string/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('numbers: a non-14 trial length fails', () => {
  const violations = check({
    [bodyName(0)]: body(0, { extra: 'Your 7-day trial starts now.' }),
  })
  assert.ok(
    violations.some((v) => v.rule === 'numbers' && /claims a 7-day trial/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('numbers: a README with no trial-length claim fails', () => {
  const violations = check({ 'README.md': readme(OFFSETS, { trialClaim: false }) })
  assert.ok(
    violations.some((v) => v.rule === 'numbers' && /no trial-length claim/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('numbers: the trial-length rule fires identically on a repeated run', () => {
  // Regression guard (and the reason trialLengthPatterns() builds fresh regexes).
  // The patterns used to be shared module-level /g literals. `RegExp.test` in the
  // README claim-present check advanced their lastIndex, and `String.matchAll`
  // seeds its clone FROM lastIndex — so the second checkCourse() call in a process
  // began scanning each file part-way in and silently missed a violation sitting
  // before that offset. One-shot CLI runs never saw it; the unit suite would have
  // seen it only as an order-dependent flake. The README states its claim in the
  // SAME `<n>-day trial` form as the body's violation, and states it late, which is
  // what made the offset outrun the body.
  const overrides = {
    'README.md': `${readme()}\n${filler(900)}\nThis is a 14-day trial.\n`,
    [bodyName(1)]: body(1, { extra: 'Your 7-day trial starts now.' }),
  }
  const claimed = (violations) => violations.filter((v) => /claims a 7-day trial/.test(v.message))
  const first = claimed(check(overrides))
  const second = claimed(check(overrides))
  assert.equal(first.length, 1, JSON.stringify(first))
  assert.deepEqual(second, first)
})

test('numbers: a send offset in days is not read as a trial length', () => {
  assert.deepEqual(
    check({ 'README.md': readme() + '\nSend offset: 12 days after the trial event.\n' }),
    [],
  )
})

test('numbers: a positional shell argument in a fenced block is not a price', () => {
  // The price scan reads prose, not raw content: the course is a course of runnable
  // commands, and `$1` is ordinary awk/sed/shell vocabulary. The previous fixture here
  // used `$BOSS_REPO_ID`, where a LETTER follows the `$`, so PRICE_RE could never have
  // matched it — the test passed whether or not the rule scanned fenced blocks, which is
  // coverage in name only. This fixture is the shape that actually fires.
  assert.deepEqual(
    check({
      [bodyName(3)]: body(3, { extra: "```bash\ngit log --format=%h | awk '{print $1}'\n```" }),
    }),
    [],
  )
})

test('numbers: an inline-code price still fails', () => {
  // stripFencedBlocks keeps backticked spans, so the prose-only scan is narrower than
  // the whole file but not narrower than the rule: a price the author wrote still reds.
  const violations = check({
    [bodyName(12)]: body(12, { words: 200, fenced: false, extra: 'It is `$9` a month.' }),
  })
  assert.ok(
    violations.some((v) => v.rule === 'numbers' && /price string/.test(v.message)),
    JSON.stringify(violations),
  )
})

// ---------------------------------------------------------------------------
// contract

test('contract: a triggering event that disagrees with the Go constant fails', () => {
  const violations = check({ 'README.md': readme(OFFSETS, { triggeringEvent: 'trial_start' }) })
  const message = violations.find((v) => v.rule === 'contract')?.message ?? ''
  // Both halves have to be named. A message that reports only one of them sends the
  // reader to the wrong file half the time.
  assert.match(message, /`trial_start`/, JSON.stringify(violations))
  assert.match(message, /`trial_started`/, JSON.stringify(violations))
})

test('contract: a README with no Loops setup section fails', () => {
  const violations = check({ 'README.md': readme(OFFSETS, { loopsSetup: false }) })
  assert.ok(
    violations.some(
      (v) => v.rule === 'contract' && /nowhere recording the event name/.test(v.message),
    ),
    JSON.stringify(violations),
  )
})

test('contract: a Loops setup section that names no triggering event fails', () => {
  const violations = check({ 'README.md': readme(OFFSETS, { triggeringEvent: null }) })
  assert.ok(
    violations.some((v) => v.rule === 'contract' && /names no triggering event/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('contract: no supplied event fails loud rather than passing with nothing checked', () => {
  // The failure mode this exists for: the Go side goes missing (renamed constant,
  // moved file, a caller that forgot the input) and the rule quietly compares the
  // guide against undefined. A green run there is worse than a red one.
  // The first case omits the field outright, which is the shape a caller that
  // forgot to thread it produces; the other two are a constant that parsed to
  // nothing.
  for (const supplied of [{}, { trialEnrolmentEvent: '' }, { trialEnrolmentEvent: null }]) {
    const violations = checkCourse({
      files: baselineFiles(),
      routes: ROUTES,
      publishedSkills: PUBLISHED,
      ...supplied,
    })
    assert.ok(
      violations.some(
        (v) =>
          v.rule === 'contract' &&
          v.file === TRIAL_ENROLLMENT_GO &&
          /refusing to pass with nothing checked/.test(v.message),
      ),
      `${JSON.stringify(supplied)}: ${JSON.stringify(violations)}`,
    )
  }
})

test('contract: a surviving BOS-974 forward reference fails', () => {
  const violations = check({ [bodyName(4)]: body(4, { extra: 'Named in BOS-974.' }) })
  assert.ok(
    violations.some(
      (v) =>
        v.rule === 'contract' && v.file === bodyName(4) && /1 forward reference/.test(v.message),
    ),
    JSON.stringify(violations),
  )
})

test('contract: a BOS-974 inside a send-offset line fails', () => {
  // Eight of the thirteen original references sat here, where the structure rule
  // matches with no end anchor and passes either way -- so the offset text alone
  // was never going to force them out.
  const violations = check({
    'README.md': readme().replaceAll('after the event', 'after the BOS-974 event'),
  })
  assert.ok(
    violations.some(
      (v) =>
        v.rule === 'contract' && v.file === 'README.md' && /8 forward reference/.test(v.message),
    ),
    JSON.stringify(violations),
  )
})

test('contract: a stale copy of the event name outside the binding line fails', () => {
  // The defect this closes: the rule used to bind only `- Triggering event:`, so a
  // rename reddened one line, and fixing THAT line turned the run green while every
  // other copy in the guide still documented the old name.
  const violations = check({
    'README.md': `${readme()}\n- Also emitted as \`trial_start\` downstream.\n`,
  })
  const message =
    violations.find((v) => v.rule === 'contract' && /every copy is bound/.test(v.message))
      ?.message ?? ''
  assert.match(message, /`trial_start`/, JSON.stringify(violations))
  assert.match(message, /`trial_started`/, JSON.stringify(violations))
})

test('contract: a documented payload field name is not read as a stale event name', () => {
  // The same rule must not turn every snake_case span in the guide into a
  // violation, or the only available fix is to stop documenting the payload.
  const field = [...NON_EVENT_SNAKE_TOKENS][0]
  const violations = check({
    'README.md': `${readme()}\n- Branch on \`${field}\` if a later message needs it.\n`,
  })
  assert.ok(
    !violations.some((v) => v.rule === 'contract' && /every copy is bound/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('contract: a forward reference to any ticket fails, not just BOS-974', () => {
  // Pinning the pattern to a single ticket id made the rule vacuous the moment the
  // guide deferred to a different one -- which is exactly what shipped: README.md
  // carried an uncaught `(BOS-969)` while this rule reported clean.
  const violations = check({ [bodyName(4)]: body(4, { extra: 'Unresolved (BOS-969).' }) })
  assert.ok(
    violations.some(
      (v) =>
        v.rule === 'contract' &&
        v.file === bodyName(4) &&
        /1 forward reference/.test(v.message) &&
        /BOS-969/.test(v.message),
    ),
    JSON.stringify(violations),
  )
})

test('contract: a ticket id inside a fenced block is a worked example, not a deferral', () => {
  // `boss new --prompt "/boss-plan BOS-1042"` is the course teaching the CLI. Reading
  // it as the guide deferring its own contract would red the gate with a message no
  // author can act on -- the same reason the numbers and voice rules are prose-only.
  const violations = check({
    [bodyName(4)]: body(4, { extra: '```bash\nboss new --prompt "/boss-plan BOS-1042"\n```' }),
  })
  assert.ok(
    !violations.some((v) => v.rule === 'contract' && /forward reference/.test(v.message)),
    JSON.stringify(violations),
  )
})

test('parseGoStringConstant reads the declared value', () => {
  const source = 'package main\n\nconst stripeTrialStartedEvent = "trial_started"\n'
  assert.equal(
    parseGoStringConstant(source, { symbol: 'stripeTrialStartedEvent', file: 'f.go' }),
    'trial_started',
  )
})

test('parseGoStringConstant binds the real constant', () => {
  const repoRoot = findRepoRoot(path.dirname(fileURLToPath(import.meta.url)))
  const source = fs.readFileSync(path.join(repoRoot, TRIAL_ENROLLMENT_GO), 'utf8')
  assert.equal(
    parseGoStringConstant(source, { symbol: TRIAL_EVENT_SYMBOL, file: TRIAL_ENROLLMENT_GO }),
    'trial_started',
  )
})

test('parseGoStringConstant throws when the symbol is renamed away', () => {
  assert.throws(
    () =>
      parseGoStringConstant('const other = "trial_started"\n', { symbol: 'gone', file: 'f.go' }),
    /gone(.|\n)*f\.go/,
  )
})

test('parseGoStringConstant throws when two declarations match', () => {
  const source = 'const e = "a"\nfunc f() {\n\te = "b"\n}\n'
  assert.throws(() => parseGoStringConstant(source, { symbol: 'e', file: 'f.go' }), /[Aa]mbiguous/)
})

test('parseGoStringConstant does not bind a qualified reference', () => {
  assert.throws(
    () => parseGoStringConstant('pkg.e = "a"\n', { symbol: 'e', file: 'f.go' }),
    /f\.go/,
  )
})

test('parseGoStringConstant refuses an empty value', () => {
  // An empty constant would compare equal to a guide that names nothing, so the
  // whole rule would pass while documenting no event at all.
  assert.throws(
    () => parseGoStringConstant('const e = ""\n', { symbol: 'e', file: 'f.go' }),
    /[Ee]mpty/,
  )
})

test('sectionBody returns null for an absent heading and stops at the next one', () => {
  const md = '# T\n\n## A\n\nalpha\n\n### A.1\n\nbravo\n\n## B\n\ncharlie\n'
  assert.equal(sectionBody(md, 'Missing'), null)
  const a = sectionBody(md, 'A')
  assert.match(a, /alpha/)
  assert.match(a, /bravo/, 'a deeper heading stays inside the section')
  assert.doesNotMatch(a, /charlie/)
})

// ---------------------------------------------------------------------------
// helpers and the real course

test('proseOf drops the title, the subject line, and fenced blocks', () => {
  const prose = proseOf('# Title\n\n**Subject:** A subject\n\nreal prose\n\n```\ncode here\n```\n')
  assert.match(prose, /real prose/)
  assert.doesNotMatch(prose, /Title/)
  assert.doesNotMatch(prose, /A subject/)
  assert.doesNotMatch(prose, /code here/)
})

test('countWords counts link text but not link targets', () => {
  assert.equal(countWords('see [the guide](https://example.com/a/b) now'), 4)
})

test('countFencedBlocks counts opening fences only', () => {
  assert.equal(countFencedBlocks('```\na\n```\n\ntext\n\n```bash\nb\n```\n'), 2)
  assert.equal(countFencedBlocks('no fences here'), 0)
})

test('findRepoRoot resolves from the scripts directory', () => {
  const repoRoot = findRepoRoot(path.dirname(fileURLToPath(import.meta.url)))
  assert.ok(fs.existsSync(path.join(repoRoot, 'Makefile')))
  assert.ok(fs.existsSync(path.join(repoRoot, 'docs', 'email-course')))
})

test('the shipped course passes every rule', () => {
  const repoRoot = findRepoRoot(path.dirname(fileURLToPath(import.meta.url)))
  assert.deepEqual(run(repoRoot), [])
})
