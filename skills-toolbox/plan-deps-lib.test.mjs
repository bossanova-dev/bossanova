// Tests for skills-toolbox/plan-deps-lib.mjs — the dependency-linking decision core.
//
// The table is TWO-DIRECTIONAL by construction: every row that must be flagged is
// paired with a row that must be cleared, because a gate that only ever sees the
// values it rejects cannot tell "narrowing works" from "narrowing rejects
// everything". Each of the six defects in the plan gets at least one row that is
// green against the old prose behaviour and red against this helper.
//
// This file is NEVER vendored into a published skill core, so unlike the module
// under test it may name real repository paths and may import tracker-named
// modules. It uses that freedom for exactly one purpose: importing the ORIGINALS
// of the two constants plan-deps-lib.mjs is forced to inline, and asserting the
// copies still agree with them.

import test from 'node:test'
import assert from 'node:assert/strict'

import { DEFAULT_CONFIG, stateRolesFor } from './skill-config.mjs'
import { BLOCKER_CLEARED_STATE_TYPES } from './linear-deps-lib.mjs'
import { buildGraph, readyTickets } from './dag-scheduler.mjs'
import {
  COULD_NOT_EVALUATE_REASONS,
  DEFAULT_CANCELED_STATE_TYPES,
  DEFAULT_CLEARED_STATE_TYPES,
  DEPENDENCY_REASONS,
  areasOverlap,
  classifyDependencyEdge,
  dependencyScanVerdict,
  extractKeyChangeAreas,
  planDependencyEdges,
  transitiveBlockWarnings,
  validateDependencyScanInput,
  withScanDefaults,
} from './plan-deps-lib.mjs'
import { descriptionAppearsTruncated } from './plan-epic-lib.mjs'

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const CONFIG = DEFAULT_CONFIG

// A caller-resolved state-name -> role map. The module never guesses these: the
// role vocabulary is the tracker adapter's, supplied per run.
const STATE_ROLES = {
  Planned: 'planned',
  'In Progress': 'inProgress',
  'In Review': 'inReview',
  Backlog: 'backlog',
}

const CONFIG_WITH_STATES = {
  ...DEFAULT_CONFIG,
  trackerConfig: {
    linear: {
      mcpServer: 'linear',
      team: 'Example',
      states: {
        unplanned: 'Todo',
        planned: 'Planned',
        inProgress: 'In Progress',
        inReview: 'In Review',
      },
    },
  },
}

function subject(over = {}) {
  return {
    id: 'uuid-subject',
    identifier: 'TCK-1',
    priority: 3,
    createdAt: '2026-01-02T00:00:00.000Z',
    stateName: 'Planned',
    stateType: 'unstarted',
    labels: [],
    ...over,
  }
}

function candidate(over = {}) {
  return {
    id: 'uuid-candidate',
    identifier: 'TCK-2',
    priority: 3,
    createdAt: '2026-01-02T00:00:00.000Z',
    stateName: 'Planned',
    stateType: 'unstarted',
    labels: [],
    ...over,
  }
}

/** One classification with the whole-fixture defaults filled in. */
function classify(over = {}) {
  return classifyDependencyEdge({
    subject: subject(),
    candidate: candidate(),
    // A same FILE on both sides. An overlap never blocks: it is a non-blocking
    // `file-overlap` (or `directory-overlap`) relation. Blocking edges need a
    // `logicalDependency` verdict, which the tests that exercise them pass.
    subjectAreas: ['app/api/x.go'],
    candidateAreas: ['app/api/x.go'],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
    ...over,
  })
}

/** A `## Key changes` section wrapped in enough surrounding plan for the slicer to work on. */
function planBody(keyChangesBody, { before = '', after = '## Testing\n\n- run the suite\n' } = {}) {
  return `## Planning\n\n- Contract: v1\n${before}\n## Key changes\n\n${keyChangesBody}\n${after}`
}

function areas(description, options = {}) {
  return extractKeyChangeAreas(CONFIG, description, options)
}

// ---------------------------------------------------------------------------
// The two inlined constants still agree with their originals
// ---------------------------------------------------------------------------

test('the inlined cleared/canceled split still reassembles into the original cleared set', () => {
  const union = new Set([...DEFAULT_CLEARED_STATE_TYPES, ...DEFAULT_CANCELED_STATE_TYPES])
  assert.deepEqual(
    [...union].sort(),
    [...BLOCKER_CLEARED_STATE_TYPES].sort(),
    'plan-deps-lib may not import linear-deps-lib (it is vendored into a published core), so the split copy is asserted against the original here — a divergence means one of the two files silently changed what "cleared" means',
  )
  // Non-vacuity: the split must be a real split, not both halves holding everything.
  assert.ok(
    !DEFAULT_CLEARED_STATE_TYPES.includes('canceled'),
    'the completed set must genuinely exclude canceled, or rung 4 cannot tell a satisfied prerequisite from a dropped one',
  )
  assert.ok(
    !DEFAULT_CANCELED_STATE_TYPES.includes('completed'),
    'the canceled set must genuinely exclude completed',
  )
})

// ---------------------------------------------------------------------------
// Defect 1 — the `## Key changes` section is the oracle, not fuzzy search
// ---------------------------------------------------------------------------

test('a mid-document Key changes section is parsed and reported as key-changes', () => {
  const result = areas(
    planBody('- `app/api/handlers.ts` — add the route\n- `app/web/page.tsx` — render it\n', {
      before: '\n## Problem\n\nSomething is wrong in `app/legacy/thing.ts`.\n',
    }),
  )
  assert.equal(result.source, 'key-changes')
  assert.deepEqual(result.areas, ['app/api/handlers.ts', 'app/web/page.tsx'])
  assert.ok(
    !result.areas.includes('app/legacy/thing.ts'),
    'a path named in ## Problem must NOT leak into the areas — the section is the oracle, and full-text scanning is exactly the defect',
  )
})

test('the Key changes section stops at the next heading', () => {
  const result = areas(planBody('- `app/api/handlers.ts` — add the route\n'))
  assert.ok(
    !result.areas.some((area) => area.includes('testing')),
    'content after the terminating heading must not be absorbed',
  )
  assert.deepEqual(result.areas, ['app/api/handlers.ts'])
})

test('a ### subheading inside Key changes does not terminate the section', () => {
  const result = areas(
    planBody('- `app/api/handlers.ts` — first\n\n### Follow-up\n\n- `app/web/page.tsx` — second\n'),
  )
  assert.equal(result.source, 'key-changes')
  assert.ok(
    result.areas.includes('app/web/page.tsx'),
    'only a top-level ## heading ends a section; a ### subheading that truncated it would silently halve the areas',
  )
})

test('a fenced code block inside Key changes contributes no areas', () => {
  const fenced = planBody(
    '- `app/api/handlers.ts` — add the route\n\n```bash\ncd app/fake/module && go build ./...\n```\n',
  )
  const result = areas(fenced)
  assert.ok(
    fenced.includes('app/fake/module'),
    'non-vacuity: the fixture must genuinely contain the fenced path, or this case proves nothing',
  )
  assert.ok(
    !result.areas.includes('app/fake/module'),
    'a path inside an illustrative fence is sample output, not a declared area',
  )
  assert.deepEqual(result.areas, ['app/api/handlers.ts'])
})

test('a ## Key changes heading that itself sits inside a fence is not a section', () => {
  const body = [
    '## Planning',
    '',
    '- Contract: v1',
    '',
    '```markdown',
    '## Key changes',
    '',
    '- `app/fenced/only.ts` — illustrative',
    '```',
    '',
    '## Testing',
    '',
    '- `app/real/spec.ts` — run it',
    '',
  ].join('\n')
  const result = areas(body)
  assert.equal(
    result.source,
    'fallback-text',
    'a fenced heading is a demonstration of the template, not a real section — treating it as one would slice the wrong span',
  )
  assert.ok(
    !result.areas.includes('app/fenced/only.ts'),
    'the fenced sample path must stay out of the areas even on the fallback path',
  )
})

test('CRLF line endings parse identically to LF', () => {
  const lf = planBody('- `app/api/handlers.ts` — add the route\n- `app/web/page.tsx` — render it\n')
  const crlf = lf.replace(/\n/g, '\r\n')
  assert.deepEqual(
    areas(crlf),
    areas(lf),
    'a tracker that normalises to CRLF on save must not change which areas a ticket declares',
  )
  assert.ok(areas(crlf).areas.length > 0, 'non-vacuity: the CRLF fixture must yield areas at all')
})

test('a real-corpus wrapped bullet yields only paths, never description words', () => {
  // Copied verbatim from docs/plans/2026-07-10-bos-329-listdaemons-multi-instance.md:47-49 —
  // backticked paths, an em dash, backticked SYMBOL names, and a wrap across three lines.
  const bullet = [
    '- `services/bosso/internal/stream/registry.go` — populate the new `Hostname`/`ConnectedAt` on the',
    '  `DaemonClaim` at the three `ClaimDaemon` call sites (918, 1007, 1013) from the `DaemonState`',
    '  (hostname from register, `connectedAt` from `NewDaemonState`).',
  ].join('\n')
  const result = areas(planBody(`${bullet}\n`))
  assert.deepEqual(
    result.areas,
    ['services/bosso/internal/stream/registry.go'],
    'the wrapped continuation lines must join into ONE entry (a line-by-line scan splits the span and finds nothing), and backticked Go symbol names must not be mistaken for paths',
  )
  assert.ok(
    !result.areas.some((area) => /populate|hostname from register/.test(area)),
    'no description word may survive as an area',
  )
})

test('missing heading, empty body, and a parsed-but-arealess section are three distinct outcomes', () => {
  const missing = areas('## Planning\n\n- Contract: v1\n\n## Testing\n\n- `app/api/x.ts`\n')
  assert.equal(missing.source, 'fallback-text')

  const empty = areas(planBody('\n'))
  assert.equal(empty.source, 'none')
  assert.deepEqual(empty.areas, [])

  const arealess = areas(planBody('- Rewrite the onboarding copy so it reads in plain English.\n'))
  assert.equal(arealess.source, 'key-changes')
  assert.deepEqual(arealess.areas, [])

  assert.notEqual(
    empty.source,
    arealess.source,
    'collapsing "there was nothing to read" into "we read it and found no areas" makes every arealess ticket read as a clean non-conflict',
  )
})

test('moduleRoots admits a bare top-level token, and without them the same token is dropped', () => {
  const body = planBody('- `scripts` — the whole helper directory\n')
  assert.deepEqual(
    areas(body, { moduleRoots: ['scripts'] }).areas,
    ['scripts'],
    'a caller-declared module root is a legitimate area even with no slash',
  )
  assert.deepEqual(
    areas(body).areas,
    [],
    'without the caller declaring it, a slash-free token is a symbol name, not a path — admitting every bare word is how a symbol becomes a phantom overlap',
  )
})

test('area tokens are normalized and deduped, and non-path tokens are rejected', () => {
  const result = areas(
    planBody(
      [
        '- **`./app/api/handlers.ts`** — bold, dot-slash prefixed',
        '- `app/api/handlers.ts:120-140` — the same file with a line anchor',
        '- `App/Api/Handlers.ts` — the same file, different case',
        '- `app/web/` — trailing slash',
        '- `app/web/**` — glob suffix',
        '- `make test-affected` — a command, not a path',
        '- `https://example.test/app/api` — a URL',
        '- `--include=app/api` — a flag',
        '- `resolveTrackerAdapter` — a bare symbol name',
      ].join('\n') + '\n',
    ),
  )
  assert.deepEqual(
    result.areas,
    ['app/api/handlers.ts', 'app/web'],
    'normalization must collapse the five spellings of two real paths and reject the four non-paths',
  )
})

test('brace-collapsed area tokens expand to comparable paths, never brace-bearing tokens', () => {
  const result = areas(
    planBody('- `services/{boss,bossd}/internal/skillinstall/install.go` — update both paths\n'),
  )
  assert.deepEqual(result.areas, [
    'services/boss/internal/skillinstall/install.go',
    'services/bossd/internal/skillinstall/install.go',
  ])
  assert.ok(
    !result.areas.some((area) => area.includes('{') || area.includes('}')),
    'brace syntax is a compact way to name concrete paths, not an area token itself',
  )
  assert.equal(
    areasOverlap(result.areas, ['services/boss/internal/skillinstall/install.go']).overlap,
    true,
    'expanded areas must compare against the concrete path they represent',
  )
})

test('pathological brace expansion falls back to no area instead of emitting braces', () => {
  const tooWide = Array.from({ length: 65 }, (_, index) => `m${index}`).join(',')
  const result = areas(planBody(`- \`services/{${tooWide}}/x.go\` — too many products\n`))
  assert.deepEqual(
    result.areas,
    [],
    'bounded expansion must fail closed to no comparable area rather than create an unbounded product',
  )
})

test('extractKeyChangeAreas rejects a swapped argument order rather than reading a config as prose', () => {
  assert.throws(
    () => extractKeyChangeAreas('## Key changes\n\n- `app/api/x.ts`\n', CONFIG),
    /arguments look swapped/,
    'a swapped call must fail loudly; silently parsing a config object as a description reports every ticket as arealess, which reads as a clean non-conflict',
  )
})

// The duplicated guard splits the same three faults as its `skill-config.mjs` original: a correctly
// ordered contractless config needs a config loaded, not arguments reordered. Every direction is
// pinned — the swapped case above must keep failing, or this relaxation is a vacuous gate.
//
// This suite is what turns the "kept in step BY HAND" note above the copy into a gate for the fault
// CLASSIFICATION (never for the message text, which legitimately differs between the two).
test('extractKeyChangeAreas reports an absent config as a missing config, not swapped arguments', () => {
  const description = '## Key changes\n\n- `app/api/x.ts`\n'
  for (const absent of [undefined, null]) {
    assert.throws(
      () => extractKeyChangeAreas(absent, description),
      (error) => {
        assert.match(error.message, /^plan-deps-lib: extractKeyChangeAreas\(config, description\)/)
        assert.match(error.message, /no config passed/)
        assert.doesNotMatch(
          error.message,
          /arguments look swapped/,
          'an absent config is not an argument-ORDER fault',
        )
        return true
      },
      `extractKeyChangeAreas(${String(absent)}, description)`,
    )
  }
})

test('extractKeyChangeAreas diagnoses a correctly ordered empty config as a missing contract', () => {
  assert.throws(
    () => extractKeyChangeAreas({}, '## Key changes\n\n- `app/api/x.ts`\n'),
    (error) => {
      assert.match(error.message, /^plan-deps-lib: extractKeyChangeAreas\(config, description\)/)
      assert.match(error.message, /no plan contract loaded/)
      assert.match(error.message, /loadSkillConfig\(\)/)
      assert.doesNotMatch(error.message, /arguments look swapped/)
      return true
    },
  )
})

// ---------------------------------------------------------------------------
// BOS-1187 — one row per RECORDED extraction failure.
//
// Each row below quotes a shape that actually reached a tracker write. The two
// directions are not symmetric and the rows are written to hold that asymmetry:
// a fabricated area must vanish, an unresolvable one must be REPORTED, and a
// resolvable one must survive — a gate that only ever rejects is the same defect
// pointing the other way.
// ---------------------------------------------------------------------------

test('BOS-1173: a git ref quoted in prose contributes no area', () => {
  const body = planBody('- Regenerate the fixtures — verified on `origin/main` `6a2b25eaa`\n')
  assert.ok(
    body.includes('origin/main'),
    'non-vacuity: the fixture must genuinely quote the ref, or this row proves nothing',
  )
  const result = areas(body, { moduleRoots: ['services', 'scripts', 'skills-toolbox'] })
  assert.deepEqual(
    result.areas,
    [],
    'untuned, `origin/main` became an area and prefix-matched a ticket whose only other tie was a plan filename, writing a blockedBy edge that stranded a buildable ticket',
  )
  assert.deepEqual(
    result.unresolved,
    ['origin/main'],
    'it is path-shaped, so it is reported rather than dropped in silence — the whole point is that a human can see what the scan refused to guess at',
  )
})

test('BOS-1145: a bare directory named parenthetically yields no area while the real change site survives', () => {
  const result = areas(
    planBody('- `app/api/router.ts` — add the route (the `vendor/legacy` tree is untouched)\n'),
    { moduleRoots: ['app'] },
  )
  assert.deepEqual(
    result.areas,
    ['app/api/router.ts'],
    'a directory mentioned to say it is NOT touched must not prefix-match everything beneath it, and the narrowing must not take the real change site with it',
  )
  assert.deepEqual(result.unresolved, [])
  assert.deepEqual(
    result.referenced,
    ['vendor/legacy'],
    'GIG-461: description-tail directory is referenced, not unresolved',
  )
})

test('BOS-1056: a path in a sibling-enumeration table row is not promoted to a change site', () => {
  // The rule is the shape gate, not table awareness: this module cannot read a
  // row's verdict column, and a rule that could would have to parse markdown
  // tables to decide dependency edges. What it can do is refuse a slashed token
  // whose leading segment the caller never declared.
  const table = [
    '- `app/api/router.ts` — the fix',
    '',
    '| Site | Verdict |',
    '| --- | --- |',
    '| `vendor/legacy` | not a defect |',
  ].join('\n')
  const result = areas(planBody(`${table}\n`), { moduleRoots: ['app'] })
  assert.deepEqual(
    result.areas,
    ['app/api/router.ts'],
    'a row whose own verdict says "not a defect" must not inject its directory as a change site prefix-matching everything beneath it',
  )
  assert.deepEqual(result.unresolved, ['vendor/legacy'])
  // The boundary, stated so it is argued with rather than discovered: a row
  // naming a concrete FILE is still extracted, because a token carrying an
  // extension is how a real path under an undeclared root stays visible. The
  // recorded BOS-1056 edge came through a shared `docs/plans/…md` row, and that
  // is closed one rung later by the suppression defaults, not here.
  const withFile = areas(planBody('- `vendor/legacy/router.ts` — enumerated sibling\n'), {
    moduleRoots: ['app'],
  })
  assert.deepEqual(
    withFile.areas,
    ['vendor/legacy/router.ts'],
    'the extension escape hatch is deliberate: without it every real path under a root the caller forgot to declare would vanish',
  )
})

test('BOS-1187 review: an unmarked module-root WORD in prose is not a change site', () => {
  // The widened whitespace split feeds ordinary English to the shape gate, and a
  // slash-free word is indistinguishable from a module root by shape alone. Before
  // the provenance gate this bullet contributed `web` and `services` as areas —
  // which `areasOverlap` then containment-matched against every file beneath them,
  // fabricating exactly the blocking edge this scan exists to refuse.
  const bullet = '- Rework the telemetry handler so the web and services teams share one shape'
  assert.ok(
    !bullet.includes('`'),
    'non-vacuity: the bullet must carry NO backtick span, or the provenance gate is not what is under test',
  )
  const result = areas(planBody(`${bullet}\n`), { moduleRoots: ['web', 'services'] })
  assert.deepEqual(result.areas, [], 'an unmarked English word is prose, never a change site')
  assert.deepEqual(
    result.unresolved,
    [],
    'and it is not path-shaped either, so it raises no warning',
  )

  // The same words MARKED as code still resolve: the gate reads provenance, not a denylist.
  assert.deepEqual(
    areas(planBody('- Rework the handler so `web` and `services` share one shape\n'), {
      moduleRoots: ['web', 'services'],
    }).areas,
    ['web', 'services'],
    'a backticked module root is a deliberate change site and must survive',
  )
})

test('BOS-1187 review: a bare directory named parenthetically yields only the change site', () => {
  // The AC names a BARE directory; the sibling BOS-1145 case pins the slash-bearing
  // form (`vendor/legacy`), which travels a different branch of the classifier.
  const result = areas(planBody('- Update services/boss/main.go (the docs tree is untouched)\n'), {
    moduleRoots: ['services', 'docs'],
  })
  assert.deepEqual(
    result.areas,
    ['services/boss/main.go'],
    'a directory named in prose to say it is NOT touched must not become a change site, and narrowing must not take the real one with it',
  )
})

test('BOS-1187 review: dotted prose is not reported as an unresolved path', () => {
  // `subject-unresolved-areas` tells a planner to rewrite the named tokens as
  // repo-relative paths before writing any edge. That instruction is unsatisfiable
  // for a version number or an abbreviation, so admitting them halts a scan that
  // resolved every real path — and a warning raised on nearly every plan is how a
  // real one stops being read.
  const result = areas(
    planBody('- Bump to v1.2 (2.10 in CI), e.g. for the release job, and touch README.md\n'),
    { moduleRoots: ['services'] },
  )
  assert.deepEqual(result.areas, [], 'none of these is a resolvable change site')
  assert.deepEqual(
    result.unresolved,
    ['readme.md'],
    'only the genuine bare basename is reported; `v1.2`, `2.10` and `e.g` are prose',
  )
})

test('BOS-946: a path written mid-sentence, with no backtick span in the bullet, is extracted', () => {
  const bullet =
    '- Replace the two maps in services/bosso/internal/telemetry_actions.go with one table'
  assert.ok(
    !bullet.includes('`'),
    'non-vacuity: the bullet must carry NO backtick span, or the widened scan is not what is under test',
  )
  const result = areas(planBody(`${bullet}\n`), { moduleRoots: ['services'] })
  assert.deepEqual(
    result.areas,
    ['services/bosso/internal/telemetry_actions.go'],
    'taking only the leading fragment of an unbackticked bullet is how two tickets editing the same file both scored no-overlap',
  )
})

test('BOS-1116: a bare basename with no directory is reported unresolved, never silently dropped', () => {
  const result = areas(planBody('- boss-build SKILL.md + references\n'), {
    moduleRoots: ['services', 'scripts'],
  })
  assert.deepEqual(
    result.areas,
    [],
    'a basename matches dozens of real files; resolving it here would trade one recorded missed edge for an unbounded source of fabricated ones',
  )
  assert.deepEqual(
    result.unresolved,
    ['skill.md'],
    'but it IS path-shaped, so the caller must be told the scan saw it and could not place it',
  )
  assert.equal(
    result.source,
    'key-changes',
    'the section was read; "we looked and could not resolve it" is a third outcome, not the arealess one',
  )
})

test('BOS-1176: an unresolved subject token raises its own warning beside the arealess one', () => {
  const result = planDependencyEdges({
    subject: { ...subject(), areas: [], unresolvedAreas: ['skill.md'] },
    candidates: [{ ...candidate(), areas: ['app/api'] }],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  const reasons = result.notes.map((entry) => entry.reason)
  assert.ok(
    reasons.includes('subject-unresolved-areas'),
    'a non-zero candidate count over unresolvable tokens printed `compared: 16, edges: []` — byte-identical to a real clean scan',
  )
  assert.ok(
    reasons.includes('no-subject-areas'),
    'BOTH fire: "nothing was comparable" and "here are the tokens we could not place" are different things to tell the caller, and folding them collapses the more specific one',
  )
  const warning = result.notes.find((entry) => entry.reason === 'subject-unresolved-areas')
  assert.equal(warning.severity, 'warning')
  assert.equal(warning.destination, 'risks')
  assert.match(warning.text, /skill\.md/, 'the note must NAME the token so it can be resolved')
  assert.match(warning.text, /leading directory in `moduleRoots`/)
  assert.match(warning.text, /root file itself in `moduleRoots`/)
  assert.match(warning.text, /present it as code-marked or as the lead of a split bullet/)
})

test('BOS-1164: repoWideTokens EXTENDS the shipped defaults instead of replacing them', () => {
  // The recorded run had to restate eleven defaults just to add one token; the
  // next caller that forgets one silently re-enables the edges it suppressed.
  const result = areasOverlap(['node_modules', 'proof/recipes/default.json'], ['node_modules'], {
    repoWideTokens: ['proof/recipes/default.json'],
  })
  assert.equal(
    result.overlap,
    false,
    'adding one repo-specific token must not drop `node_modules` — the shipped default that was the only thing suppressing this pair',
  )
  assert.deepEqual(result.shared, [])
})

test('BOS-1164: the named replace-mode opt-out still replaces the shipped defaults', () => {
  const result = areasOverlap(['node_modules'], ['node_modules'], {
    repoWideTokens: ['proof/recipes/default.json'],
    replaceRepoWideTokens: true,
  })
  assert.equal(
    result.overlap,
    true,
    'a caller that genuinely wants the shipped list gone must be able to say so — but by name, never as the silent side effect of passing one token',
  )
  assert.deepEqual(result.shared, ['node_modules'])
})

test('BOS-1163: a slash-bearing suppression token suppresses areas beneath it, a single-segment one does not', () => {
  const mirror = 'plugins/bossd-plugin-claude/skilldata'
  const declared = [`${mirror}/skills/boss-plan`]
  const beneath = [`${mirror}/skills/boss-plan/toolbox/x.mjs`]
  assert.equal(
    areasOverlap(declared, beneath).overlap,
    true,
    'non-vacuity: these two genuinely contain one another, so the suppression below is what makes the difference — not a pair that never overlapped',
  )
  const nested = areasOverlap(declared, beneath, { repoWideTokens: [mirror] })
  assert.equal(
    nested.overlap,
    false,
    'a generated mirror directory declared noisy must suppress everything it contains; an exact-match-only token never matches the deep paths that are the whole problem',
  )
  const concrete = areasOverlap(['docs/api.md'], ['docs/api.md'], { repoWideTokens: ['docs'] })
  assert.equal(
    concrete.overlap,
    true,
    'the narrowing must not reject everything: a single-segment token keeps exact-match semantics, so a concrete file two tickets really do share still overlaps',
  )
})

test('BOS-1119: the boilerplate plan-copy directory alone produces no overlap and no edge', () => {
  // "copy the plan to docs/plans/<id>.md" ships in every plan by construction,
  // so the plans directory is the one area every candidate shares. A tracker-only
  // ticket with no source change was scoring a blocking edge on exactly that.
  const subjectAreas = areas(
    planBody(
      '- `app/api/router.ts` — the change\n- copy the plan to `docs/plans/bos-1.md` and commit it\n',
    ),
    { moduleRoots: ['app'] },
  ).areas
  assert.deepEqual(
    subjectAreas,
    ['app/api/router.ts', 'docs/plans/bos-1.md'],
    'non-vacuity: the boilerplate line must genuinely still yield a docs/plans area, or the suppression below is passing for free',
  )
  const trackerOnly = ['docs/plans']
  assert.equal(
    areasOverlap(subjectAreas, ['app/api/router.ts']).overlap,
    true,
    'non-vacuity: the same subject still overlaps a ticket that really does touch its source file',
  )
  assert.equal(
    areasOverlap(subjectAreas, trackerOnly).overlap,
    false,
    'co-appearing under the plans directory is an artifact of the template, never an ordering constraint',
  )
  const result = planDependencyEdges({
    subject: { ...subject(), areas: subjectAreas },
    candidates: [{ ...candidate(), areas: trackerOnly }],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.deepEqual(result.edges, [], 'and no edge reaches the tracker write')
})

test('an unresolved-token warning stands down when every token resolved', () => {
  const clean = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api'], unresolvedAreas: [] },
    candidates: [{ ...candidate(), areas: ['app/api'] }],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.ok(
    !clean.notes.some((entry) => entry.reason === 'subject-unresolved-areas'),
    'non-vacuity: a warning that fires on every run carries no information at all',
  )
})

// ---------------------------------------------------------------------------
// BOS-1256 — one row per RECORDED mis-parse of a NON-PATH token.
//
// Same two-directional construction as the block above: every row pairs the
// defective input with a CONTROL the rule must not move, because a classifier
// change that suppressed every dotted token, every colon and every bracket would
// pass a one-sided assertion while destroying the extraction it was meant to fix.
// ---------------------------------------------------------------------------

test('BOS-1187: a line locator is stripped so one changed file is one area', () => {
  const result = areas(
    planBody(
      [
        '- `services/bosso/cmd/main.go` — the handler',
        '- `services/bosso/cmd/main.go:1218+` — the append site',
        '- `services/bosso/cmd/main.go:339,344,345` — the three call sites',
      ].join('\n') + '\n',
    ),
    { moduleRoots: ['services'] },
  )
  assert.deepEqual(
    result.areas,
    ['services/bosso/cmd/main.go'],
    'a `+` suffix and a comma list are line LOCATORS, not path segments; retained, one changed file arrived as three areas that scored no-overlap against each other',
  )
  assert.equal(
    areasOverlap(result.areas, ['services/bosso/cmd/main.go']).overlap,
    true,
    'and the surviving area must compare true against a bare citation of the same file',
  )

  // The rule genuinely RUNS: with no bare spelling in the body there is nothing
  // for a dedupe to collapse onto, so the stripped form can only come from the strip.
  assert.deepEqual(
    areas(planBody('- `services/bosso/cmd/main.go:339,344,345` — only the suffixed form\n'), {
      moduleRoots: ['services'],
    }).areas,
    ['services/bosso/cmd/main.go'],
  )

  // Control: the two forms that already worked must not move.
  assert.deepEqual(
    areas(planBody('- `app/api/handlers.ts:120-140` and `app/api/router.ts:12`\n'), {
      moduleRoots: ['app'],
    }).areas,
    ['app/api/handlers.ts', 'app/api/router.ts'],
  )
})

test('BOS-1196: an angle-bracketed placeholder is prose, and its bullet-mate still resolves', () => {
  const result = areas(
    planBody(
      '- the verdict line is at `<run-dir>/log`, written by `services/boss/internal/run.go`\n',
    ),
    { moduleRoots: ['services'] },
  )
  assert.deepEqual(
    result.areas,
    ['services/boss/internal/run.go'],
    'control: the drop must be per TOKEN — rejecting the whole entry would take the real change site with it',
  )
  assert.deepEqual(
    result.unresolved,
    [],
    'a runtime placeholder names no file that can ever exist, so no rewrite can satisfy the remedy the warning prescribes',
  )
  assert.ok(
    ![...result.areas, ...result.unresolved].some(
      (token) => token.includes('<') || token.includes('>'),
    ),
    'the leading-character strip ate the `<` and shipped the mangled `run-dir>/log`; no returned token may carry a bracket',
  )
})

test('BOS-1226/BOS-1229: a dotted SELECTOR is prose while a dotted FILENAME is still reported', () => {
  const result = areas(
    planBody(
      [
        '- thread `cfg.Hostname` through `daemonstate.Metadata`, `settings.DaemonName` and `daemonstate.Write`',
        '- and refresh `CLAUDE.md`',
      ].join('\n') + '\n',
    ),
    { moduleRoots: ['services'] },
  )
  assert.deepEqual(result.areas, [], 'a selector expression is not a change site')
  assert.deepEqual(
    result.unresolved,
    ['claude.md'],
    'control: the extension case is the whole discriminator — suppressing every dotted token would take the genuine basename with it, and the remedy for the selectors would have minted `cfg` and `daemonstate` as module roots',
  )
  assert.deepEqual(
    result.arealessEntries,
    [
      'thread `cfg.Hostname` through `daemonstate.Metadata`, `settings.DaemonName` and `daemonstate.Write`',
    ],
    'dropping the selectors must not make the loss invisible: the bullet that resolved to nothing is still reported',
  )
})

test('BOS-1225: a dot-prefixed root resolves undeclared, while a parent escape does not', () => {
  const result = areas(
    planBody('- `.codex/skills/bs-sweep-update/` regenerated from `../escape/x.go`\n'),
    { moduleRoots: ['services'] },
  )
  assert.deepEqual(
    result.areas,
    ['.codex/skills/bs-sweep-update'],
    'nothing but a source root is written `.name/`, so it needs no declaration — the recorded remedy worked but required a root every ad-hoc list forgets',
  )
  assert.deepEqual(
    result.unresolved,
    ['../escape/x.go'],
    'control: the rule needs a name character after the dot, or `..` reads as a root and a path pointing OUT of the repository becomes an area on the strength of its extension',
  )
})

test('BOS-1197: a bare basename already covered by a resolved area is suppressed', () => {
  const covered = areas(
    planBody(
      [
        '- the `bs-review-caps.mjs` cap is recomputed',
        '- in `skills-toolbox/bs-review-caps.mjs`',
      ].join('\n') + '\n',
    ),
    { moduleRoots: ['skills-toolbox'] },
  )
  assert.deepEqual(
    covered.areas,
    ['skills-toolbox/bs-review-caps.mjs'],
    'the bare mention and the qualified path are ONE file named twice',
  )
  assert.deepEqual(
    covered.unresolved,
    [],
    'and the suppression is order-INDEPENDENT: the qualifying path is in a later bullet than the bare mention',
  )

  // Control: a basename with no matching area is still reported, or the
  // suppression has swallowed the BOS-1116 outcome it must leave standing.
  assert.deepEqual(
    areas(planBody('- the `bs-review-caps.mjs` cap is recomputed\n'), {
      moduleRoots: ['skills-toolbox'],
    }).unresolved,
    ['bs-review-caps.mjs'],
  )
})

test('BOS-1191: a wildcard segment collapses to the directory it names', () => {
  const result = areas(
    planBody('- `services/bosso/migrations_postgres/*.sql` — the new migration\n'),
    { moduleRoots: ['services'] },
  )
  assert.deepEqual(
    result.areas,
    ['services/bosso/migrations_postgres'],
    'no real file is ever named `*.sql`, so the retained glob was an area that could never match the files it was written to name',
  )
  assert.equal(
    areasOverlap(result.areas, ['services/bosso/migrations_postgres/0042_add_org.sql']).overlap,
    true,
  )
  assert.ok(
    !result.areas.some((area) => area.includes('*')),
    'a wildcard is a way of naming concrete paths, never an area token itself',
  )

  // Control: the two glob forms that already collapsed must not move.
  assert.deepEqual(
    areas(planBody('- `app/web/**` and `app/api/*`\n'), { moduleRoots: ['app'] }).areas,
    ['app/web', 'app/api'],
  )
})

test('BOS-1256: a doublestar path segment is not bold, and never mints an empty-segment area', () => {
  const globbed = areas(planBody('- `services/**/testdata` regenerated\n'), {
    moduleRoots: ['services'],
  })
  assert.deepEqual(
    globbed.areas,
    [],
    'stripping `**` as bold rewrote the glob into `services//testdata` — an area no changed-file path can ever equal, emitted as a confident area because the wildcard guard never saw a wildcard',
  )
  assert.ok(
    !globbed.areas.some((area) => area.includes('//')),
    'an empty path segment names no site at all',
  )
  assert.deepEqual(
    globbed.arealessEntries,
    ['`services/**/testdata` regenerated'],
    'the mid-path glob is recorded as a loss rather than guessed at',
  )

  // Control: REAL bold delimiters must still be stripped, or the narrowed rule
  // has traded a malformed area for a mangled one.
  assert.deepEqual(
    areas(planBody('- **`services/boss/main.go`** — the real change\n'), {
      moduleRoots: ['services'],
    }).areas,
    ['services/boss/main.go'],
  )
})

test('BOS-1256: an uppercase extension still names a file, while a selector member does not', () => {
  const shouted = areas(planBody('- `services/boss/main.go` and `README.MD` both change\n'), {
    moduleRoots: ['services'],
  })
  assert.deepEqual(
    shouted.unresolved,
    ['readme.md'],
    'reading the extension in its original case to reject Go selectors also dropped a real file written in caps out of `unresolved` — and beside a resolved area the loss had no accounting at all',
  )
  assert.deepEqual(shouted.areas, ['services/boss/main.go'])

  // Control: the selector rule the case read was added for must still hold, or
  // the uppercase repair has walked `cfg.Hostname` back in as a filename.
  const selectors = areas(planBody('- thread `cfg.Hostname` through `daemonstate.Write`\n'), {
    moduleRoots: ['services'],
  })
  assert.deepEqual(selectors.unresolved, [], 'a selector expression is not a change site')
  assert.deepEqual(selectors.areas, [])
})

test('BOS-1256: a one-level glob under an undeclared root is reported, not dropped to prose', () => {
  assert.deepEqual(
    areas(planBody('- `skills-toolbox/*.mjs` are regenerated\n'), { moduleRoots: ['services'] })
      .unresolved,
    ['skills-toolbox'],
    'the collapse consumed the token`s only slash, so the surviving word fell out of the path branch and produced NOTHING — while the explicit spelling of the same change produced an area',
  )

  // Control: the same shape under a DECLARED root stays an area, or the new
  // route has promoted every resolvable directory glob into a warning.
  const declared = areas(planBody('- `docs/plans/*.md` are added\n'), { moduleRoots: ['docs'] })
  assert.deepEqual(declared.areas, ['docs/plans'])
  assert.deepEqual(declared.unresolved, [])
})

test('BOS-1191: a bullet that resolved to nothing is reported, and a resolved body reports none', () => {
  const lossy = areas(
    planBody('- Tidy up the wording throughout\n- `services/boss/main.go` — the real change\n'),
    { moduleRoots: ['services'] },
  )
  assert.deepEqual(
    lossy.arealessEntries,
    ['Tidy up the wording throughout'],
    'a non-empty `areas` beside `unresolved: []` understated the change surface with nothing saying so',
  )
  assert.deepEqual(
    lossy.unresolved,
    [],
    'and a prose bullet is NOT promoted into the actionable list — a warning raised on nearly every plan is how a real one stops being read',
  )
  assert.deepEqual(lossy.areas, ['services/boss/main.go'])

  // Control: the field must be capable of being EMPTY, or its populated case
  // proves only that every bullet lands in it.
  assert.deepEqual(
    areas(planBody('- `services/boss/main.go` — the only change\n'), { moduleRoots: ['services'] })
      .arealessEntries,
    [],
  )
})

// ---------------------------------------------------------------------------
// Defect 2 — overlap is a precondition, and the granularity is stated
// ---------------------------------------------------------------------------

test('sibling module names do not overlap, but ancestor containment does', () => {
  assert.equal(
    areasOverlap(['services/boss'], ['services/bossd']).overlap,
    false,
    'services/boss and services/bossd are different Go modules; a startsWith test would call them the same area',
  )
  const nested = areasOverlap(['services/boss'], ['services/boss/internal/skillinstall'])
  assert.equal(nested.overlap, true, 'a descendant of a declared area is the same area')
  assert.deepEqual(
    nested.shared,
    ['services/boss/internal/skillinstall'],
    'the shared region is the DEEPER path — the part both tickets actually touch',
  )
})

test('two different files under the same module do NOT overlap — the stated granularity', () => {
  assert.equal(
    areasOverlap(['app/api/handlers.ts'], ['app/api/router.ts']).overlap,
    false,
    'truncating areas to their module root makes every file in a module "the same area" and over-links a monorepo exactly as badly as skipping overlap under-links it',
  )
  assert.equal(
    areasOverlap(['app/api/handlers.ts'], ['app/api/handlers.ts']).overlap,
    true,
    'the same file is still the same area — the narrowing must not reject everything',
  )
})

test('a repo-wide token is excluded from shared, not merely from the boolean', () => {
  const result = areasOverlap(['docs', 'app/api'], ['docs', 'app/web'], {
    repoWideTokens: ['docs'],
  })
  assert.equal(result.overlap, false, 'sharing only a repo-wide directory is not a conflict')
  assert.ok(
    !result.shared.includes('docs'),
    'a caller reading `shared` must never see a token the boolean already decided to ignore',
  )
  const kept = areasOverlap(['docs', 'app/api'], ['docs', 'app/api'], { repoWideTokens: ['docs'] })
  assert.deepEqual(kept.shared, ['app/api'], 'the real shared area survives the denylist')
})

test('areaAliases closes a known false negative without the module knowing the repo', () => {
  const withoutAlias = areasOverlap(['app/api/handlers.ts'], ['generated/api/handlers.ts'])
  assert.equal(
    withoutAlias.overlap,
    false,
    'non-vacuity: without the alias these two genuinely do not overlap, so the alias case below is not passing for free',
  )
  const withAlias = areasOverlap(['app/api/handlers.ts'], ['generated/api/handlers.ts'], {
    areaAliases: { 'generated/api/handlers.ts': ['app/api/handlers.ts'] },
  })
  assert.equal(
    withAlias.overlap,
    true,
    'a caller-supplied alias makes a generated mirror overlap its source',
  )
})

test('shared is sorted deterministically', () => {
  const forward = areasOverlap(['app/web', 'app/api'], ['app/api', 'app/web'])
  const reversed = areasOverlap(['app/api', 'app/web'], ['app/web', 'app/api'])
  assert.deepEqual(forward.shared, ['app/api', 'app/web'])
  assert.deepEqual(
    forward.shared,
    reversed.shared,
    'the dependency line goes into the tracker description; an input-order-dependent `shared` makes every re-plan produce a spurious diff',
  )
})

test('a file-disjoint pair produces no edge even when the candidate is far more urgent', () => {
  const disjoint = classify({
    subject: subject({ priority: 4 }),
    candidate: candidate({ priority: 1 }),
    subjectAreas: ['app/api/handlers.ts'],
    candidateAreas: ['app/web/page.tsx'],
  })
  assert.equal(disjoint.edge, 'none')
  assert.equal(disjoint.basis, null)
  assert.equal(disjoint.reason, 'no-overlap')
  assert.equal(
    disjoint.write,
    null,
    'orientation must be UNREACHABLE without a basis — a priority-ranked edge here is the defect that serializes file-disjoint tickets',
  )

  // Paired must-flag row: identical priorities, overlapping areas.
  const overlapping = classify({
    subject: subject({ priority: 4 }),
    candidate: candidate({ priority: 1 }),
    subjectAreas: ['app/api/handlers.ts'],
    candidateAreas: ['app/api/handlers.ts'],
  })
  assert.equal(
    overlapping.edge,
    'relatedTo',
    'the same pair WITH an overlap is recorded as a non-blocking relation',
  )
  assert.equal(overlapping.basis, 'overlap')
  assert.equal(overlapping.reason, 'file-overlap')
  assert.equal(overlapping.write, null, 'an overlap never writes a blocking edge')
})

test('an arealess side reports no-areas, distinct from no-overlap', () => {
  const arealess = classify({ subjectAreas: [], candidateAreas: ['app/api'] })
  assert.equal(arealess.reason, 'no-areas')
  const disjoint = classify({ subjectAreas: ['app/api'], candidateAreas: ['app/web'] })
  assert.equal(disjoint.reason, 'no-overlap')
  assert.notEqual(
    arealess.reason,
    disjoint.reason,
    '"we could not tell" and "we checked and they are disjoint" carry different confidence; one code for both hides the parse failure',
  )
})

// ---------------------------------------------------------------------------
// Defect 3 — no blocking edge onto a started ticket, symmetrically
// ---------------------------------------------------------------------------

test('an outbound edge onto an inProgress candidate downgrades; the inbound edge survives', () => {
  const outbound = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({ priority: 3, stateName: 'In Progress', stateType: 'started' }),
    logicalDependency: { direction: 'blocks' },
  })
  assert.equal(outbound.edge, 'relatedTo')
  assert.equal(outbound.reason, 'downgraded-candidate-started')
  assert.equal(outbound.write, null, 'a downgraded edge must carry no write intent at all')
  assert.ok(
    outbound.note && outbound.note.text.length > 0,
    'the downgrade must be explained, not silent',
  )

  const inbound = classify({
    subject: subject({ priority: 3 }),
    candidate: candidate({ priority: 1, stateName: 'In Progress', stateType: 'started' }),
    logicalDependency: true,
  })
  assert.equal(
    inbound.edge,
    'blockedBy',
    'the SAME started candidate on an inbound edge keeps its blocking edge — the write lands on the planned subject, which strands nobody',
  )
  assert.deepEqual(inbound.write, { id: 'uuid-subject', blockedBy: ['uuid-candidate'] })
})

test('an inReview candidate downgrades exactly as an inProgress one does', () => {
  const review = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({ priority: 3, stateName: 'In Review', stateType: 'started' }),
    logicalDependency: { direction: 'blocks' },
  })
  assert.equal(review.edge, 'relatedTo')
  assert.equal(review.reason, 'downgraded-candidate-started')

  const planned = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({ priority: 3 }),
    logicalDependency: { direction: 'blocks' },
  })
  assert.equal(
    planned.edge,
    'blocks',
    'a planned candidate in the same position still takes the blocking edge',
  )
  assert.deepEqual(planned.write, { id: 'uuid-candidate', blockedBy: ['uuid-subject'] })
})

test('a started SUBJECT downgrades an inbound edge — the rung is symmetric', () => {
  const started = classify({
    subject: subject({ priority: 3, stateName: 'In Progress', stateType: 'started' }),
    candidate: candidate({ priority: 1 }),
    logicalDependency: true,
  })
  assert.equal(
    started.edge,
    'relatedTo',
    'an inbound blockedBy onto a subject an agent is already working strands that agent — the same harm as the outbound case',
  )
  assert.equal(started.reason, 'downgraded-subject-started')
  assert.equal(started.write, null)

  const plannedSubject = classify({
    subject: subject({ priority: 3 }),
    candidate: candidate({ priority: 1 }),
    logicalDependency: true,
  })
  assert.equal(
    plannedSubject.edge,
    'blockedBy',
    'a planned subject still receives the blocking edge',
  )
})

test('a logical downgrade is a louder note, routed differently, from an overlap downgrade', () => {
  const started = { priority: 3, stateName: 'In Progress', stateType: 'started' }
  const fromOverlap = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate(started),
  })
  // The logical pair is oriented by its VERDICT, not by priority, so the started
  // side that receives the write here is the subject — the same rung, reached from
  // the other direction.
  const fromLogical = classify({
    subject: subject({ priority: 1, stateName: 'In Progress', stateType: 'started' }),
    candidate: candidate({ priority: 3 }),
    subjectAreas: ['app/api'],
    candidateAreas: ['app/web'],
    logicalDependency: true,
  })
  assert.equal(fromOverlap.basis, 'overlap')
  assert.equal(fromLogical.basis, 'logical')
  assert.equal(fromOverlap.note.severity, 'info')
  assert.equal(fromOverlap.note.destination, 'planning')
  assert.equal(
    fromLogical.note.severity,
    'warning',
    'degrading an overlap costs rebase churn; degrading a logical prerequisite drops a real ordering constraint, and the note must say so',
  )
  assert.equal(fromLogical.note.destination, 'risks')
  assert.notEqual(fromOverlap.note.text, fromLogical.note.text)
})

test('an unrecognized state name degrades rather than betting a blocking edge on it', () => {
  const unknown = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({ priority: 3, stateName: 'Shipping Soon' }),
    logicalDependency: { direction: 'blocks' },
  })
  assert.ok(
    !('Shipping Soon' in STATE_ROLES),
    'non-vacuity: the fixture state must genuinely be absent from the role map',
  )
  assert.equal(unknown.edge, 'relatedTo')
  assert.equal(unknown.reason, 'downgraded-unknown-state')
  assert.equal(unknown.write, null)
})

test('status fields classify the same as stateName and stateType on both sides', () => {
  const fromStateFields = classify({
    subject: subject({ priority: 3, stateName: 'Planned', stateType: 'unstarted' }),
    candidate: candidate({ priority: 1, stateName: 'Planned', stateType: 'unstarted' }),
  })
  const fromStatusFields = classify({
    subject: subject({
      priority: 3,
      stateName: undefined,
      stateType: undefined,
      status: 'Planned',
      statusType: 'unstarted',
    }),
    candidate: candidate({
      priority: 1,
      stateName: undefined,
      stateType: undefined,
      status: 'Planned',
      statusType: 'unstarted',
    }),
  })
  assert.equal(fromStatusFields.edge, fromStateFields.edge)
  assert.equal(fromStatusFields.reason, fromStateFields.reason)
  assert.deepEqual(fromStatusFields.write, fromStateFields.write)
})

test('a bare state string resolves its role identically to a named state object', () => {
  const fromObject = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({
      priority: 3,
      stateName: undefined,
      stateType: undefined,
      state: { name: 'In Progress', type: 'started' },
    }),
  })
  const fromString = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({
      priority: 3,
      stateName: undefined,
      stateType: undefined,
      state: 'In Progress',
    }),
  })
  assert.equal(fromString.edge, fromObject.edge)
  assert.equal(fromString.reason, fromObject.reason)
  assert.equal(fromString.note.destination, fromObject.note.destination)
})

test('an issue carrying no state field at all downgrades on unknown state', () => {
  const noState = classify({
    subject: {
      id: 'uuid-subject',
      identifier: 'TCK-1',
      priority: 3,
      createdAt: '2026-01-02T00:00:00.000Z',
      labels: [],
    },
    candidate: {
      id: 'uuid-candidate',
      identifier: 'TCK-2',
      priority: 1,
      createdAt: '2026-01-02T00:00:00.000Z',
      labels: [],
    },
    logicalDependency: true,
  })
  assert.equal(noState.edge, 'relatedTo')
  assert.equal(noState.reason, 'downgraded-unknown-state')
  assert.match(noState.note.text, /no state name/)
})

// ---------------------------------------------------------------------------
// Defect 4 — a cleared logical prerequisite survives; canceled is not satisfied
// ---------------------------------------------------------------------------

test('a cleared candidate with an overlap basis is dropped quietly', () => {
  const cleared = classify({
    candidate: candidate({ stateName: 'Done', stateType: 'completed' }),
    subject: subject({ priority: 1 }),
  })
  assert.equal(cleared.edge, 'none')
  assert.equal(cleared.reason, 'candidate-cleared')
  assert.equal(cleared.basis, 'overlap')
  assert.equal(
    cleared.note,
    null,
    'a merged file-overlap is simply gone; a note here is pure noise',
  )

  const open = classify({ subject: subject({ priority: 1 }) })
  assert.equal(open.edge, 'relatedTo', 'the same fixture while still open is a relation')
  assert.equal(open.reason, 'file-overlap')
})

test('a COMPLETED candidate with a logical basis reports the prerequisite as satisfied', () => {
  const done = classify({
    candidate: candidate({ stateName: 'Done', stateType: 'completed' }),
    subjectAreas: ['app/api'],
    candidateAreas: ['app/web'],
    logicalDependency: true,
  })
  assert.equal(done.edge, 'none')
  assert.equal(done.basis, 'logical')
  assert.equal(done.reason, 'prerequisite-satisfied')
  assert.ok(
    done.note && done.note.text.includes('TCK-2'),
    'the note must NAME the prerequisite — a real dependency that merged is information the plan needs, and dropping it silently is the defect',
  )
  assert.equal(done.note.severity, 'info')
})

test('a CANCELED logical prerequisite is a warning, never a satisfaction', () => {
  const dropped = classify({
    candidate: candidate({ stateName: 'Canceled', stateType: 'canceled' }),
    subjectAreas: ['app/api'],
    candidateAreas: ['app/web'],
    logicalDependency: true,
  })
  assert.equal(dropped.reason, 'prerequisite-canceled')
  assert.equal(
    dropped.note.severity,
    'warning',
    'recording a canceled prerequisite as satisfied sends an implementer to build against a feature nobody is building',
  )
  assert.equal(dropped.note.destination, 'risks')

  const satisfied = classify({
    candidate: candidate({ stateName: 'Done', stateType: 'completed' }),
    subjectAreas: ['app/api'],
    candidateAreas: ['app/web'],
    logicalDependency: true,
  })
  assert.notEqual(
    dropped.note.text,
    satisfied.note.text,
    'the two outcomes must not share a note; they ask the reader to do different things',
  )
})

test('rung 4 precedes the overlap rung — a cleared candidate leaves no relation', () => {
  const stamp = '2026-03-03T00:00:00.000Z'
  const cleared = classify({
    subject: subject({ priority: 2, createdAt: stamp }),
    candidate: candidate({
      priority: 2,
      createdAt: stamp,
      stateName: 'Done',
      stateType: 'completed',
    }),
  })
  assert.equal(
    cleared.reason,
    'candidate-cleared',
    'the cleared rung must run before the overlap rung, or a merged ticket would still be recorded as a relation',
  )
  assert.equal(cleared.question, null)

  const open = classify({
    subject: subject({ priority: 2, createdAt: stamp }),
    candidate: candidate({ priority: 2, createdAt: stamp }),
  })
  assert.equal(
    open.reason,
    'file-overlap',
    'non-vacuity: the identical fixture WITHOUT the cleared state genuinely reaches the overlap rung',
  )
})

// ---------------------------------------------------------------------------
// Defect 6 / rung 2 — epic parents and unschedulable candidates
// ---------------------------------------------------------------------------

test('an epic-labelled candidate is skipped even when its areas overlap strongly', () => {
  const epic = classify({
    candidate: candidate({ labels: [{ name: 'Epic' }] }),
    subjectAreas: ['app/api', 'app/web'],
    candidateAreas: ['app/api', 'app/web'],
  })
  assert.equal(epic.edge, 'none')
  assert.equal(
    epic.reason,
    'epic-parent',
    'rung 2 must precede rung 3: an epic parent description is the union of its children, so it overlaps almost everything — running overlap first reports phantom conflicts',
  )
  assert.equal(epic.basis, null, 'a rejected candidate must not carry a basis it never earned')
  assert.ok(epic.note && epic.note.text.includes('TCK-2'))
})

test('an undefined epicLabel treats NO candidate as epic', () => {
  const labelled = candidate({ labels: [{ name: 'Epic' }] })
  assert.equal(
    classify({ candidate: labelled }).reason,
    'epic-parent',
    'non-vacuity: with the label configured this fixture is genuinely rejected',
  )
  const result = classify({
    subject: subject({ priority: 1 }),
    candidate: { ...labelled, priority: 3 },
    epicLabel: undefined,
  })
  assert.notEqual(
    result.reason,
    'epic-parent',
    'an unconfigured epic label must mean "no candidate is epic", never "every candidate is" — the latter silently produces zero edges for the whole run',
  )
  assert.equal(result.edge, 'relatedTo')
})

test('an epic SUBJECT short-circuits to zero edges', () => {
  const result = classify({ subject: subject({ labels: ['Epic'] }) })
  assert.equal(result.edge, 'none')
  assert.equal(result.reason, 'subject-is-epic')
  assert.equal(result.write, null)

  const set = planDependencyEdges({
    subject: { ...subject({ labels: ['Epic'] }), areas: ['app/api'] },
    candidates: [
      { ...candidate({ priority: 1 }), areas: ['app/api'] },
      { ...candidate({ id: 'uuid-c3', identifier: 'TCK-3', priority: 1 }), areas: ['app/api'] },
    ],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(
    set.edges.length,
    0,
    'an epic parent flipped to planned by an earlier phase must not take a burst of edges onto a container that never merges',
  )
  assert.equal(set.notes.length, 1)
})

test('a candidate in a non-schedulable state is not a valid blocker', () => {
  const backlog = classify({ candidate: candidate({ stateName: 'Backlog' }) })
  assert.equal(backlog.edge, 'none')
  assert.equal(
    backlog.reason,
    'candidate-not-schedulable',
    'a state that never produces a pull request produces a block that never clears',
  )
  const planned = classify({ candidate: candidate({ stateName: 'Planned' }) })
  assert.notEqual(planned.reason, 'candidate-not-schedulable')
})

test('epic children re-enter at rung 1, and the depth cap stops a parent/child cycle', () => {
  const parent = {
    ...candidate({ id: 'uuid-p', identifier: 'TCK-P', labels: ['Epic'] }),
    areas: ['app/api'],
  }
  const child = {
    ...candidate({ id: 'uuid-c', identifier: 'TCK-C', priority: 1 }),
    areas: ['app/api'],
  }
  const expanded = planDependencyEdges({
    subject: { ...subject({ priority: 3 }), areas: ['app/api'] },
    candidates: [parent],
    childrenByParentId: { 'uuid-p': [child] },
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(
    expanded.edges.length,
    1,
    'the expanded child is an ordinary candidate from rung 1 onward',
  )
  assert.equal(expanded.edges[0].identifier, 'TCK-C')
  const parentSkip = expanded.skipped.find((entry) => entry.identifier === 'TCK-P')
  assert.ok(parentSkip, 'the parent itself must still appear in skipped')
  assert.equal(parentSkip.reason, 'epic-parent')
  assert.equal(
    parentSkip.expandChildren,
    false,
    'children were supplied and expanded in place, so the caller must NOT be sent to fetch them again',
  )

  // The same parent with its children WITHHELD is the case the flag exists for.
  const unexpanded = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [parent],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(unexpanded.edges.length, 0, 'an unexpanded parent contributes no edge of its own')
  assert.equal(
    unexpanded.skipped.find((entry) => entry.identifier === 'TCK-P').expandChildren,
    true,
    'with no children supplied the caller is told to fetch them',
  )

  // A parent whose children were supplied as a DELIBERATELY EMPTY list is
  // answered, not re-asked: flagging it would send the caller back for a list it
  // already produced, and step 5(d) would never settle "no active children".
  const childless = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [parent],
    childrenByParentId: { 'uuid-p': [] },
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(
    childless.skipped.find((entry) => entry.identifier === 'TCK-P').expandChildren,
    false,
    'an empty children list is an answer — the caller must not be sent round again',
  )

  // A child that is itself epic-labelled, with the cap set to one level.
  const epicChild = {
    ...candidate({ id: 'uuid-e', identifier: 'TCK-E', labels: ['Epic'] }),
    areas: ['app/api'],
  }
  const capped = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [parent],
    childrenByParentId: { 'uuid-p': [epicChild], 'uuid-e': [parent] },
    maxExpansionDepth: 1,
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  const childSkip = capped.skipped.find((entry) => entry.identifier === 'TCK-E')
  assert.equal(
    childSkip.expandChildren,
    false,
    'at the depth cap the nested parent is reported, not expanded — a malformed parent/child cycle must terminate',
  )
  assert.equal(capped.skipped.filter((entry) => entry.identifier === 'TCK-P').length, 1)
})

test('same-epic siblings and the epic parent are planning notes, not external edges', () => {
  const result = planDependencyEdges({
    subject: { ...subject({ epicParentId: 'uuid-epic' }), areas: ['app/api'] },
    candidates: [
      {
        ...candidate({ id: 'uuid-sibling', identifier: 'TCK-S', priority: 1 }),
        epicParentId: 'uuid-epic',
        areas: ['app/api'],
      },
      {
        ...candidate({ id: 'uuid-epic', identifier: 'TCK-P', priority: 1 }),
        areas: ['app/api'],
      },
    ],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(result.edges.length, 0)
  assert.equal(result.skipped.length, 2)
  assert.deepEqual(
    result.skipped.map((entry) => entry.reason),
    ['same-epic-member', 'same-epic-member'],
  )
  // BOS-1327: the per-candidate records stay, but they share ONE consolidated note
  // naming every member in sorted order — N siblings used to record N near-identical lines.
  assert.equal(result.notes.length, 1)
  const [only] = result.notes
  assert.equal(only.destination, 'planning')
  assert.equal(only.severity, 'info')
  assert.equal(only.reason, 'same-epic-member')
  assert.ok(
    only.text.startsWith('TCK-P, TCK-S ') && only.text.includes('epic'),
    'internal epic coordination must surface as ONE planning note naming every member, not a dependency write',
  )
})

test('an unrecognized state on the BLOCKER downgrades too, not only on the blocked side', () => {
  // The verdict makes the subject the BLOCKER and the candidate the blocked side. Only the blocker's state is unmappable.
  const classified = classify({
    subject: subject({ priority: 1, stateName: 'Bikeshedding' }),
    candidate: candidate({ priority: 3 }),
    logicalDependency: { direction: 'blocks' },
  })
  assert.equal(
    classified.reason,
    'downgraded-unknown-state',
    'a state this run cannot classify must downgrade wherever it sits, not only when it is blocked',
  )
  assert.equal(classified.edge, 'relatedTo')
  assert.equal(classified.write, null, 'a downgraded edge writes no blocking relation')
  assert.ok(
    !('Bikeshedding' in STATE_ROLES),
    'non-vacuity: the fixture state must genuinely be absent from the role map',
  )
  assert.match(
    classified.note.text,
    /TCK-1/,
    'the note must name the side whose state could not be classified',
  )
})

// ---------------------------------------------------------------------------
// Rung 5 — orientation
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Every result is writable as returned
// ---------------------------------------------------------------------------

test('write intent is present exactly on blocking outcomes and names the blocked side', () => {
  const rows = [
    classify({
      subject: subject({ priority: 1 }),
      candidate: candidate({ priority: 3 }),
      logicalDependency: { direction: 'blocks' },
    }),
    classify({
      subject: subject({ priority: 3 }),
      candidate: candidate({ priority: 1 }),
      logicalDependency: true,
    }),
    classify({
      subject: subject({ priority: 1 }),
      candidate: candidate({ priority: 3, stateName: 'In Progress', stateType: 'started' }),
      logicalDependency: { direction: 'blocks' },
    }),
    classify({ subjectAreas: ['app/api'], candidateAreas: ['app/web'] }),
    classify({}),
  ]
  for (const row of rows) {
    if (row.edge === 'blockedBy' || row.edge === 'blocks') {
      assert.ok(row.write, `${row.reason} is a blocking outcome and must carry a write intent`)
      assert.deepEqual(Object.keys(row.write).sort(), ['blockedBy', 'id'])
      assert.equal(row.write.blockedBy.length, 1)
      assert.notEqual(
        row.write.id,
        row.write.blockedBy[0],
        'the write must never name the same issue on both sides',
      )
    } else {
      assert.equal(
        row.write,
        null,
        `${row.reason} is not a blocking outcome and must carry no write`,
      )
    }
  }
  assert.equal(rows[0].write.id, 'uuid-candidate', 'a `blocks` outcome saves onto the CANDIDATE')
  assert.equal(rows[1].write.id, 'uuid-subject', 'a `blockedBy` outcome saves onto the SUBJECT')
})

test('an id-less side stops the edge instead of emitting an unwritable write', () => {
  // `issueKey` returns null when an issue carries neither `id` nor `identifier`. Emitting
  // `{id: null, blockedBy: [null]}` under `edge: 'blocks'` would hand the caller a save it
  // cannot execute while LOOKING like a decided edge — the exact class of failure this module
  // exists to remove. Both orientations must stop, and the stop must be explainable.
  const nameless = {
    priority: 3,
    createdAt: '2026-01-02T00:00:00.000Z',
    stateName: 'Planned',
    stateType: 'unstarted',
    labels: [],
  }
  for (const logicalDependency of [true, { direction: 'blocks' }]) {
    const row = classify({
      subject: subject(),
      candidate: nameless,
      logicalDependency,
    })
    assert.equal(row.edge, 'none', 'an unnameable side must not produce a blocking edge')
    assert.equal(row.write, null)
    assert.equal(row.reason, 'unidentifiable-issue')
    assert.equal(row.basis, 'logical', 'the basis that WAS established is still reported')
    assert.equal(
      row.note.severity,
      'warning',
      'a real dependency that cannot be written is a risk, not a planning aside',
    )
    assert.equal(row.note.destination, 'risks')
  }
  // Non-vacuity: the same pair with both ids present still produces the write.
  const writable = classify({
    subject: subject({ priority: 3 }),
    candidate: candidate({ priority: 1 }),
    logicalDependency: true,
  })
  assert.equal(writable.edge, 'blockedBy')
  assert.deepEqual(writable.write, { id: 'uuid-subject', blockedBy: ['uuid-candidate'] })
})

test('an arealess SUBJECT warns, rather than reporting a clean no-dependencies run', () => {
  // `compared === 0` is not the only way to compare nothing meaningful. When the
  // subject's own key-changes section yields no areas, every candidate stops at
  // `no-areas` — which carries no note — so the caller sees compared > 0, zero
  // edges, zero notes and zero questions: indistinguishable from a genuine clean
  // pass, and the exact silence this module exists to remove.
  const arealess = planDependencyEdges({
    subject: subject(),
    subjectAreas: [],
    candidates: [candidate({ id: 'a', identifier: 'TCK-A', priority: 1 })],
    stateRoles: STATE_ROLES,
  })
  assert.equal(arealess.compared, 1, 'the candidate WAS compared — this is not the empty-set case')
  assert.equal(arealess.edges.length, 0)
  const warning = arealess.notes.find((entry) => entry.reason === 'no-subject-areas')
  assert.ok(warning, 'an arealess subject must not return a note-free "no dependencies"')
  assert.equal(warning.severity, 'warning')
  assert.equal(warning.destination, 'risks')
  assert.match(warning.text, /not because none exist/)
  assert.ok(
    !arealess.notes.some((entry) => entry.reason === 'no-candidates-compared'),
    'the two silences are different outcomes and must not both fire',
  )

  // Stands down when a LOGICAL basis established an edge without areas — the one
  // way a dependency is knowable with nothing to overlap on.
  const logical = planDependencyEdges({
    subject: subject(),
    subjectAreas: [],
    candidates: [candidate({ id: 'a', identifier: 'TCK-A', priority: 1 })],
    logicalDependencies: { 'TCK-A': true },
    stateRoles: STATE_ROLES,
  })
  assert.equal(logical.edges.length, 1)
  assert.ok(
    !logical.notes.some((entry) => entry.reason === 'no-subject-areas'),
    'a logical basis makes the missing areas immaterial',
  )

  // Non-vacuity: the same candidate against a subject WITH areas produces the
  // edge and stays silent, so the warning tracks the missing areas and nothing else.
  const withAreas = planDependencyEdges({
    subject: subject(),
    subjectAreas: ['app/api'],
    candidates: [candidate({ id: 'a', identifier: 'TCK-A', priority: 1, areas: ['app/api'] })],
    stateRoles: STATE_ROLES,
  })
  assert.equal(withAreas.edges.length, 1)
  assert.ok(!withAreas.notes.some((entry) => entry.reason === 'no-subject-areas'))
})

test('a run where every compared pair downgrades on unknown state gets an aggregate warning', () => {
  const allUnknown = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api/x.go'] },
    candidates: [
      {
        ...candidate({ id: 'a', identifier: 'TCK-A', priority: 1, stateName: 'Mystery' }),
        areas: ['app/api/x.go'],
      },
      {
        ...candidate({ id: 'b', identifier: 'TCK-B', priority: 1, stateName: 'Pending-ish' }),
        areas: ['app/api/x.go'],
      },
    ],
    logicalDependencies: { 'TCK-A': true, 'TCK-B': true },
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(allUnknown.compared, 2)
  assert.equal(allUnknown.edges.length, 2)
  assert.ok(
    allUnknown.notes.some(
      (entry) =>
        entry.reason === 'all-pairs-downgraded-unknown-state' && entry.severity === 'warning',
    ),
    'all-unknown downgraded runs must not read like ordinary non-blocking overlap notes',
  )

  const withWrite = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api/x.go'] },
    candidates: [
      {
        ...candidate({ id: 'a', identifier: 'TCK-A', stateName: 'Mystery' }),
        areas: ['app/api/x.go'],
      },
      { ...candidate({ id: 'b', identifier: 'TCK-B', priority: 1 }), areas: ['app/api/x.go'] },
    ],
    logicalDependencies: { 'TCK-A': true, 'TCK-B': true },
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.ok(withWrite.edges.some((entry) => entry.write))
  assert.ok(
    !withWrite.notes.some((entry) => entry.reason === 'all-pairs-downgraded-unknown-state'),
    'one surviving blocking write proves the run did not downgrade every compared pair',
  )
})

test('stateRolesFor inverts all configured workflow state roles', () => {
  assert.deepEqual(stateRolesFor(CONFIG_WITH_STATES), {
    Todo: 'unplanned',
    Planned: 'planned',
    'In Progress': 'inProgress',
    'In Review': 'inReview',
  })
})

test('every reason produced across the whole table is a member of DEPENDENCY_REASONS', () => {
  const started = { stateName: 'In Progress', stateType: 'started' }
  const produced = new Set(
    [
      classify({ subject: subject({ labels: ['Epic'] }) }),
      classify({ candidate: candidate({ id: 'uuid-subject', identifier: 'TCK-1' }) }),
      classify({ excludeIds: ['TCK-2'] }),
      classify({
        subject: subject({ priority: 3 }),
        candidate: {
          priority: 1,
          createdAt: '2026-01-02T00:00:00.000Z',
          stateName: 'Planned',
          stateType: 'unstarted',
          labels: [],
        },
      }),
      classify({ candidate: candidate({ labels: ['Epic'] }) }),
      classify({ candidate: candidate({ stateName: 'Backlog' }) }),
      classify({ subjectAreas: [], candidateAreas: [] }),
      classify({ subjectAreas: ['app/api'], candidateAreas: ['app/web'] }),
      classify({
        subjectAreas: ['app/api'],
        candidateAreas: ['app/web'],
        source: 'declared-related',
      }),
      classify({ candidate: candidate({ stateName: 'Done', stateType: 'completed' }) }),
      classify({
        candidate: candidate({ stateName: 'Done', stateType: 'completed' }),
        subjectAreas: ['app/api'],
        candidateAreas: ['app/web'],
        logicalDependency: true,
      }),
      classify({
        candidate: candidate({ stateName: 'Canceled', stateType: 'canceled' }),
        subjectAreas: ['app/api'],
        candidateAreas: ['app/web'],
        logicalDependency: true,
      }),
      classify({ subject: subject({ priority: 1 }), candidate: candidate({ priority: 3 }) }),
      classify({
        subjectAreas: ['app/api'],
        candidateAreas: ['app/web'],
        logicalDependency: true,
      }),
      classify({
        subject: subject({ priority: 1 }),
        candidate: candidate({ priority: 3, ...started }),
        logicalDependency: { direction: 'blocks' },
      }),
      classify({
        subject: subject({ priority: 3, ...started }),
        candidate: candidate({ priority: 1 }),
        logicalDependency: true,
      }),
      classify({
        subject: subject({ priority: 1 }),
        candidate: candidate({ priority: 3, stateName: '?' }),
        logicalDependency: { direction: 'blocks' },
      }),
      classify({ candidate: candidate({ ...started, landed: { evidence: 'abc123' } }) }),
      classify({ subjectAreas: ['app/api'], candidateAreas: ['app/api/x.go'] }),
    ].map((row) => row.reason),
  )
  for (const reason of produced) {
    assert.ok(
      DEPENDENCY_REASONS.includes(reason),
      `${reason} is returned but is not in the exported enum — a caller branching on the enum would silently fall through`,
    )
  }
  // A floor ("at least N distinct reasons") lets a newly added reason join the enum
  // with nothing producing it, which is the failure this test exists to catch. Assert
  // SET EQUALITY against every classify-level member instead. The three set-level
  // reasons are named here because `classifyDependencyEdge` structurally cannot
  // produce them — they are emitted by `planDependencyEdges` over the whole
  // candidate set — and each is pinned by its own test below.
  const SET_LEVEL_REASONS = [
    'declared-related-unresolved',
    'no-candidates-compared',
    'no-subject-areas',
    'subject-unresolved-areas',
    'no-candidate-areas',
    'all-pairs-downgraded-unknown-state',
    'same-epic-member',
  ]
  const expected = DEPENDENCY_REASONS.filter((reason) => !SET_LEVEL_REASONS.includes(reason))
  assert.deepEqual(
    [...produced].sort(),
    [...expected].sort(),
    'every classify-level reason in the enum must be produced by this table, and nothing else',
  )
})

// ---------------------------------------------------------------------------
// Defect 5 and the set-level contract
// ---------------------------------------------------------------------------

test('a declared relation with no conflict is evaluated and reported, not omitted or re-written', () => {
  const result = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [{ ...candidate(), areas: ['app/web'] }],
    declaredRelatedIds: ['TCK-2'],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(result.edges.length, 0, 'an existing relation must not be re-written')
  assert.equal(
    result.compared,
    1,
    'it must nonetheless have been EVALUATED — a text scan never sees it',
  )
  const entry = result.skipped.find((row) => row.identifier === 'TCK-2')
  assert.equal(entry.reason, 'declared-related-no-conflict')
  assert.equal(entry.source, 'declared-related')
})

test('a declared id absent from the fetched candidate set is reported, never dropped', () => {
  const result = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [{ ...candidate(), areas: ['app/api'] }],
    declaredRelatedIds: ['TCK-2', 'TCK-99'],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  const missing = result.skipped.find((row) => row.reason === 'declared-related-unresolved')
  assert.ok(missing, 'the unfetched declared id must surface as a hole in the comparison set')
  assert.equal(
    missing.id,
    'tck-99',
    'the entry must carry the id so the caller can fetch it directly',
  )
  assert.equal(missing.note.severity, 'warning')
  assert.ok(
    !result.skipped.some(
      (row) => row.identifier === 'TCK-2' && row.reason === 'declared-related-unresolved',
    ),
    'non-vacuity: the declared id that WAS fetched must not also be reported as unresolved',
  )
})

test('a candidate that is both declared and overlapping produces exactly one edge', () => {
  const result = planDependencyEdges({
    subject: { ...subject({ priority: 3 }), areas: ['app/api'] },
    candidates: [
      { ...candidate({ priority: 1 }), areas: ['app/api'] },
      // The same issue arriving again under its uuid rather than its identifier.
      {
        id: 'uuid-candidate',
        priority: 1,
        createdAt: '2026-01-02T00:00:00.000Z',
        stateName: 'Planned',
        areas: ['app/api'],
      },
    ],
    declaredRelatedIds: ['uuid-candidate'],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(
    result.edges.length,
    1,
    'dedupe must normalize uuid-vs-identifier, or a declared-and-overlapping candidate yields two contradictory writes',
  )
  assert.equal(result.edges[0].source, 'declared-related')
})

test('an empty candidate set reports could-not-evaluate, not a clean pass', () => {
  const empty = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(empty.compared, 0)
  assert.equal(empty.edges.length, 0)
  assert.ok(
    empty.notes.some(
      (entry) => entry.reason === 'no-candidates-compared' && entry.severity === 'warning',
    ),
    'zero-checked is not a pass — an empty fetch must be distinguishable from "evaluated, found nothing"',
  )

  const evaluated = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [{ ...candidate(), areas: ['app/web'] }],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(evaluated.compared, 1)
  assert.equal(
    evaluated.notes.filter((entry) => entry.reason === 'no-candidates-compared').length,
    0,
    'the genuinely-evaluated run must NOT carry the could-not-evaluate warning',
  )
})

test('edges are ordered by candidate identifier regardless of input order', () => {
  const build = (order) =>
    planDependencyEdges({
      subject: { ...subject({ priority: 3 }), areas: ['app/api'] },
      candidates: order,
      stateRoles: STATE_ROLES,
      epicLabel: 'Epic',
    }).edges.map((edge) => edge.identifier)
  const a = { ...candidate({ id: 'uuid-a', identifier: 'TCK-A', priority: 1 }), areas: ['app/api'] }
  const b = { ...candidate({ id: 'uuid-b', identifier: 'TCK-B', priority: 1 }), areas: ['app/api'] }
  const c = { ...candidate({ id: 'uuid-c', identifier: 'TCK-C', priority: 1 }), areas: ['app/api'] }
  assert.deepEqual(build([a, b, c]), ['TCK-A', 'TCK-B', 'TCK-C'])
  assert.deepEqual(
    build([c, a, b]),
    ['TCK-A', 'TCK-B', 'TCK-C'],
    'the dependency line lands in the tracker description; an input-order-dependent listing makes every re-plan a spurious diff',
  )
})

// ---------------------------------------------------------------------------
// Direction, repo-wide asymmetry, write-guard order, declared reconciliation
// ---------------------------------------------------------------------------

test('a logical prerequisite keeps its direction whatever the priority order says', () => {
  // The wrong-edge class this module exists to remove, produced by the module
  // itself: the caller declares "the candidate is my prerequisite" and rung 5
  // orients the pair by priority, writing the prerequisite as blocked by the
  // ticket that depends on it whenever the prerequisite is the less urgent one.
  const oriented = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({ priority: 4 }),
    subjectAreas: ['app/api'],
    candidateAreas: ['app/web'],
    logicalDependency: 'the subject cannot start until the candidate ships its parser',
  })
  assert.equal(
    oriented.edge,
    'blockedBy',
    'the declared prerequisite must stay the blocker even though it carries the WEAKER priority',
  )
  assert.equal(oriented.reason, 'oriented-by-logical')
  assert.deepEqual(oriented.write, { id: 'uuid-subject', blockedBy: ['uuid-candidate'] })

  // Non-vacuity: the identical fixture on an OVERLAP basis writes no blocking edge at
  // all, so the assertion above comes from the verdict and not the fixture.
  const byOverlap = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({ priority: 4 }),
  })
  assert.equal(byOverlap.edge, 'relatedTo')
  assert.equal(byOverlap.write, null)

  // The reverse reading is available, and saying it explicitly is the ONLY way to
  // get it — a direction is never inferred from priority or age.
  const reversed = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({ priority: 4 }),
    subjectAreas: ['app/api'],
    candidateAreas: ['app/web'],
    logicalDependency: { direction: 'blocks', note: 'the candidate needs the subject feature' },
  })
  assert.equal(reversed.edge, 'blocks')
  assert.equal(reversed.reason, 'oriented-by-logical')
  assert.deepEqual(reversed.write, { id: 'uuid-candidate', blockedBy: ['uuid-subject'] })
})

test('a cleared candidate the SUBJECT is the prerequisite for is not a satisfied prerequisite', () => {
  const dropped = classify({
    subject: subject(),
    candidate: candidate({ stateName: 'Done', stateType: 'completed' }),
    subjectAreas: ['app/api'],
    candidateAreas: ['app/web'],
    logicalDependency: { direction: 'blocks', note: 'the candidate needs the subject feature' },
  })
  assert.equal(
    dropped.reason,
    'candidate-cleared',
    'the dependent side landed; calling that "prerequisite satisfied" names the wrong side as the prerequisite',
  )
  assert.equal(dropped.edge, 'none')
  const satisfied = classify({
    subject: subject(),
    candidate: candidate({ stateName: 'Done', stateType: 'completed' }),
    subjectAreas: ['app/api'],
    candidateAreas: ['app/web'],
    logicalDependency: true,
  })
  assert.equal(
    satisfied.reason,
    'prerequisite-satisfied',
    'non-vacuity: the DEFAULT direction still reports the landed prerequisite as satisfied',
  )
})

test('a repo-wide token is excluded even when the other side is deeper than it', () => {
  const asymmetric = areasOverlap(['docs'], ['docs/plans/x.md'], { repoWideTokens: ['docs'] })
  assert.equal(
    asymmetric.overlap,
    false,
    'the shared region is the DEEPER path, so a denylist that only tests the region lets a broad token phantom-overlap everything beneath it',
  )
  assert.deepEqual(asymmetric.shared, [])
  const reversed = areasOverlap(['docs/plans/x.md'], ['docs'], { repoWideTokens: ['docs'] })
  assert.equal(reversed.overlap, false, 'the exclusion must not depend on argument order')
  assert.equal(
    areasOverlap(['src'], ['src/app/main.ts']).overlap,
    false,
    'the DEFAULT token list must behave the same way — `src` is on it',
  )
  // `docs/api/x.md`, not `docs/plans/x.md`: `docs/plans` is now a shipped default
  // in its own right and carries a slash, so it suppresses what it contains. The
  // single-segment `docs` above still keeps exact-match semantics, which is what
  // this row exists to prove.
  const kept = areasOverlap(['docs/api/x.md'], ['docs/api/x.md'])
  assert.equal(
    kept.overlap,
    true,
    'non-vacuity: two real paths that happen to live under a repo-wide token still overlap',
  )
})

test('a downgrade whose side carries no id stops instead of returning an unwritable relation', () => {
  const started = { stateName: 'In Progress', stateType: 'started' }
  const idless = classify({
    subject: subject({ priority: 3, ...started }),
    candidate: {
      priority: 1,
      createdAt: '2026-01-02T00:00:00.000Z',
      stateName: 'Planned',
      stateType: 'unstarted',
      labels: [],
    },
    logicalDependency: true,
  })
  assert.equal(
    idless.reason,
    'unidentifiable-issue',
    'a relatedTo the caller is told to save through appendRelatedTo needs both ids as much as a blocking write does',
  )
  assert.equal(idless.edge, 'none')
  assert.equal(idless.write, null)
  const named = classify({
    subject: subject({ priority: 3, ...started }),
    candidate: candidate({ priority: 1 }),
    logicalDependency: true,
  })
  assert.equal(
    named.edge,
    'relatedTo',
    'non-vacuity: the identical fixture with an identifiable candidate genuinely downgrades',
  )
  assert.equal(named.reason, 'downgraded-subject-started')
})

test('a declared id that expansion resolves is not ALSO reported unresolved', () => {
  const parent = candidate({ id: 'uuid-parent', identifier: 'TCK-P', labels: ['Epic'] })
  const child = candidate({
    id: 'uuid-child',
    identifier: 'TCK-C',
    priority: 1,
    areas: ['app/api'],
  })
  const expanded = planDependencyEdges({
    subject: subject(),
    subjectAreas: ['app/api'],
    candidates: [parent],
    declaredRelatedIds: ['TCK-C'],
    childrenByParentId: { 'uuid-parent': [child] },
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(expanded.edges.length, 1, 'the declared child is reachable through the epic parent')
  assert.equal(expanded.edges[0].source, 'declared-related')
  assert.ok(
    !expanded.skipped.some((entry) => entry.reason === 'declared-related-unresolved'),
    'reconciling declared ids before expansion sends the caller back to fetch a ticket this very run already evaluated',
  )
  const unexpanded = planDependencyEdges({
    subject: subject(),
    subjectAreas: ['app/api'],
    candidates: [parent],
    declaredRelatedIds: ['TCK-C'],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.ok(
    unexpanded.skipped.some((entry) => entry.reason === 'declared-related-unresolved'),
    'non-vacuity: with no children supplied the declared id is genuinely unresolved and must still be reported',
  )
})

test('a candidate set holding only rung-1 rejections reports could-not-evaluate', () => {
  const degenerate = planDependencyEdges({
    subject: subject(),
    subjectAreas: ['app/api'],
    candidates: [subject({ areas: ['app/api'] })],
    stateRoles: STATE_ROLES,
  })
  assert.equal(
    degenerate.compared,
    0,
    'the subject matched itself and was never evaluated against anything; counting it makes a degenerate set look like a clean evaluation',
  )
  assert.ok(
    degenerate.notes.some((entry) => entry.reason === 'no-candidates-compared'),
    'the could-not-evaluate warning must fire when nothing was actually compared',
  )
  const excluded = planDependencyEdges({
    subject: subject(),
    subjectAreas: ['app/api'],
    candidates: [candidate({ areas: ['app/api'] })],
    excludeIds: ['TCK-2'],
    stateRoles: STATE_ROLES,
  })
  assert.equal(excluded.compared, 0, 'a caller-excluded id was not evaluated either')
  const real = planDependencyEdges({
    subject: subject(),
    subjectAreas: ['app/api'],
    candidates: [candidate({ areas: ['app/api'] })],
    stateRoles: STATE_ROLES,
  })
  assert.equal(
    real.compared,
    1,
    'non-vacuity: the identical candidate without the exclusion IS compared, so the count tracks evaluation and not the list length',
  )
})

test('an epic parent reports WHY it will not be expanded, not just that it will not be', () => {
  const parent = candidate({ id: 'uuid-parent', identifier: 'TCK-P', labels: ['Epic'] })
  const run = (over) =>
    planDependencyEdges({
      subject: subject(),
      subjectAreas: ['app/api'],
      candidates: [parent],
      stateRoles: STATE_ROLES,
      epicLabel: 'Epic',
      ...over,
    })
  const pending = run({})
  assert.equal(pending.skipped[0].expandChildren, true)
  assert.equal(pending.skipped[0].expansion, 'pending')
  const supplied = run({ childrenByParentId: { 'uuid-parent': [] } })
  assert.equal(supplied.skipped[0].expandChildren, false)
  assert.equal(supplied.skipped[0].expansion, 'supplied')
  const capped = run({ maxExpansionDepth: 0 })
  assert.equal(capped.skipped[0].expandChildren, false)
  assert.equal(
    capped.skipped[0].expansion,
    'depth-capped',
    'a branch the cap left unexamined must not read as a parent this run fully handled',
  )
})

test('caller-supplied tuning that is not a list degrades instead of throwing', () => {
  const decided = classifyDependencyEdge({
    subject: subject({ priority: 1 }),
    candidate: candidate({ priority: 3 }),
    subjectAreas: ['app/api/x.go'],
    candidateAreas: ['app/api/x.go'],
    stateRoles: STATE_ROLES,
    clearedStateTypes: undefined,
    canceledStateTypes: 'completed',
    logicalDependency: { direction: 'blocks' },
  })
  assert.equal(
    decided.reason,
    'oriented-by-logical',
    'the header promises nothing here throws; a malformed tuning list must fall back to the default, not abort the run',
  )
})

// ---------------------------------------------------------------------------
// Shape guard (BOS-1244 row 5)
// ---------------------------------------------------------------------------

test('areasOverlap — a bare area string raises, so the per-candidate .some() misuse fails loudly', () => {
  const subjectAreas = ['app/api/handlers.ts']
  const candidateAreas = ['app/web/page.tsx', 'docs/guide.md']

  // The recorded misuse. `areasOverlap` returns an OBJECT, so this predicate is truthy for every
  // candidate whatever it compared, and over-links the whole set.
  assert.throws(
    () => candidateAreas.some((area) => areasOverlap(subjectAreas, area)),
    (err) => {
      assert.match(err.message, /areasOverlap\(/, 'the message must name the function')
      assert.match(err.message, /candidateAreas\[\]/, 'and the expected array shape')
      return true
    },
  )

  // Either position, and every non-array shape.
  for (const bad of ['app/web/page.tsx', null, undefined, 42, { area: 'app/api' }]) {
    assert.throws(() => areasOverlap(subjectAreas, bad), /areasOverlap\(/, `b=${String(bad)}`)
    assert.throws(() => areasOverlap(bad, candidateAreas), /areasOverlap\(/, `a=${String(bad)}`)
  }

  // The correct call is untouched, in both directions.
  assert.equal(areasOverlap(subjectAreas, candidateAreas).overlap, false)
  assert.equal(areasOverlap(subjectAreas, ['app/api/handlers.ts']).overlap, true)
})

test('classifyDependencyEdge — a non-array area set still answers by NAME, never by throwing', () => {
  // The ladder's never-throws promise is unchanged: it normalizes before calling the guarded helper,
  // because it already has a named rung for an empty subject set. Both are loud; only one of them is
  // `areasOverlap`'s own contract.
  const result = classifyDependencyEdge({
    config: CONFIG,
    stateRoles: STATE_ROLES,
    subject: { id: 'AAA-1', identifier: 'AAA-1', priority: 2, state: { name: 'Planned' } },
    candidate: { id: 'AAA-2', identifier: 'AAA-2', priority: 2, state: { name: 'Planned' } },
    subjectAreas: 'app/api/handlers.ts',
    candidateAreas: 'app/api/handlers.ts',
  })
  assert.equal(result.edge, 'none')
  assert.match(
    result.reason,
    /areas/,
    'the ladder must reach its own named no-areas rung rather than raising',
  )
})

test('DEFAULT_CLEARED_STATE_TYPES / DEFAULT_CANCELED_STATE_TYPES stay frozen ARRAYS (BOS-1244 row 6)', () => {
  // Adjudicated `not a defect`: a `.has()` misuse already raises a TypeError at the call site, which
  // is the behaviour R1 asks for. Converting either to a Set would break the two `Array.isArray`
  // consumers in classifyDependencyEdge, silently restoring the shipped defaults over an override.
  for (const [name, value] of [
    ['DEFAULT_CLEARED_STATE_TYPES', DEFAULT_CLEARED_STATE_TYPES],
    ['DEFAULT_CANCELED_STATE_TYPES', DEFAULT_CANCELED_STATE_TYPES],
  ]) {
    assert.ok(Array.isArray(value), `${name} must remain an array`)
    assert.ok(Object.isFrozen(value), `${name} must remain frozen`)
    assert.equal(typeof value.has, 'undefined', `${name}.has() must keep raising at the call site`)
  }
})

// ---------------------------------------------------------------------------
// BOS-1327 — refuse bad input, report a vacuous result honestly
// ---------------------------------------------------------------------------

// The tracker's list-truncation sentinel, asserted against its single owner so this
// fixture cannot drift from the rule the validator imports.
const TRUNCATED = 'intro (truncated, use get_issue for full description)'

/** A payload the validator must accept: every field the ladder reads, present and consistent. */
function scanPayload(over = {}) {
  return {
    subject: { ...subject(), parentId: null, description: planBody('- `app/api/x.go`') },
    candidates: [{ ...candidate(), parentId: null, description: planBody('- `app/web/y.go`') }],
    declaredRelatedIds: ['TCK-2'],
    epicLabel: 'Epic',
    stateRoles: STATE_ROLES,
    ...over,
  }
}

function defectCodes(payload) {
  return validateDependencyScanInput(payload).defects.map((entry) => entry.code)
}

test('BOS-1327 validator: a clean payload is ok with no defects', () => {
  assert.deepEqual(validateDependencyScanInput(scanPayload()), { ok: true, defects: [] })
})

test('BOS-1327 validator: truncated-description fires on a list-truncated body, subject or candidate', () => {
  assert.equal(descriptionAppearsTruncated(TRUNCATED), true, 'fixture must carry the real sentinel')
  const report = validateDependencyScanInput(
    scanPayload({
      candidates: [{ ...candidate(), parentId: null, description: TRUNCATED }],
    }),
  )
  assert.equal(report.ok, false)
  assert.deepEqual(
    report.defects.map((entry) => [entry.code, entry.id]),
    [['truncated-description', 'TCK-2']],
  )
  assert.match(report.defects[0].remedy, /getIssue/)
  assert.deepEqual(
    defectCodes(scanPayload({ subject: { ...scanPayload().subject, description: TRUNCATED } })),
    ['truncated-description'],
  )
})

test('BOS-1327 validator: declared-related-not-fetched fires before classification, not after', () => {
  const report = validateDependencyScanInput(
    scanPayload({ declaredRelatedIds: ['TCK-2', 'TCK-77'] }),
  )
  assert.deepEqual(
    report.defects.map((entry) => [entry.code, entry.id]),
    [['declared-related-not-fetched', 'tck-77']],
  )
  assert.match(report.defects[0].remedy, /regardless of state/)
  // A declared id held under a supplied epic parent's children resolves in the same call.
  assert.deepEqual(
    defectCodes(
      scanPayload({
        declaredRelatedIds: ['TCK-77'],
        childrenByParentId: { 'uuid-p': [candidate({ id: 'uuid-77', identifier: 'TCK-77' })] },
      }),
    ),
    [],
  )
})

test('BOS-1327 validator: missing-state-type fires when no state-type field resolves', () => {
  const bare = { ...candidate(), parentId: null }
  delete bare.stateType
  assert.deepEqual(defectCodes(scanPayload({ candidates: [bare] })), ['missing-state-type'])
  // The nested detail-read shape resolves just as well as the flat list shape.
  const nested = { ...bare, state: { name: 'Planned', type: 'unstarted' } }
  delete nested.stateName
  assert.deepEqual(defectCodes(scanPayload({ candidates: [nested] })), [])
})

test('BOS-1327 validator: conflicting-state-fields catches a nested-only overlay the flat field hides', () => {
  // The list snapshot said started; a live overlay refreshed only the nested object.
  const stale = {
    ...candidate({ stateName: 'In Progress', stateType: 'started' }),
    parentId: null,
    state: { name: 'Done', type: 'completed' },
  }
  const report = validateDependencyScanInput(scanPayload({ candidates: [stale] }))
  assert.deepEqual(
    report.defects.map((entry) => entry.code),
    ['conflicting-state-fields', 'conflicting-state-fields'],
    'both the type and the name disagree',
  )
  assert.match(report.defects[0].remedy, /flat stateType/)
  const agreeing = { ...stale, stateName: 'Done', stateType: 'completed' }
  assert.deepEqual(defectCodes(scanPayload({ candidates: [agreeing] })), [])
})

test('BOS-1327 validator: missing-parent-field requires the KEY, and accepts an explicit null', () => {
  const noParent = candidate()
  assert.deepEqual(defectCodes(scanPayload({ candidates: [noParent] })), ['missing-parent-field'])
  assert.deepEqual(defectCodes(scanPayload({ candidates: [{ ...noParent, parentId: null }] })), [])
  assert.deepEqual(
    defectCodes(scanPayload({ candidates: [{ ...noParent, epicParentId: 'uuid-epic' }] })),
    [],
  )
})

test('BOS-1327 validator: supplied epic children are validated, because they reach the classifier', () => {
  const parent = candidate({ id: 'uuid-p', identifier: 'TCK-9', labels: ['Epic'] })
  const child = (over) => candidate({ id: 'uuid-c', identifier: 'TCK-30', ...over })
  const bare = child()
  delete bare.stateType
  const expand = (kid) =>
    scanPayload({
      candidates: [{ ...parent, parentId: null }],
      declaredRelatedIds: [],
      childrenByParentId: { 'uuid-p': [kid] },
    })
  // Premise: an expanded child is classified like any candidate — a logical verdict on it
  // writes an edge — so a defect the validator skipped would ship on that edge.
  const run = planDependencyEdges({
    ...expand(child()),
    subjectAreas: ['app/api'],
    logicalDependencies: { 'TCK-30': true },
  })
  assert.deepEqual(
    run.edges.map((entry) => entry.identifier),
    ['TCK-30'],
  )
  assert.deepEqual(defectCodes(expand(bare)), ['missing-state-type'])
  assert.deepEqual(defectCodes(expand(child({ description: TRUNCATED }))), [
    'truncated-description',
  ])
  assert.deepEqual(defectCodes(expand(child({ state: { name: 'Done', type: 'completed' } }))), [
    'conflicting-state-fields',
    'conflicting-state-fields',
  ])
  assert.equal(validateDependencyScanInput(expand(bare)).defects[0].id, 'TCK-30')
  // A child's parent is its childrenByParentId key, so it needs no parent field of its own.
  assert.deepEqual(defectCodes(expand(child())), [])
  // A child that is also a candidate is reported once, under the candidate's stricter checks.
  const twice = scanPayload({
    candidates: [{ ...parent, parentId: null }, bare],
    declaredRelatedIds: [],
    childrenByParentId: { 'uuid-p': [bare] },
  })
  assert.deepEqual(defectCodes(twice), ['missing-state-type', 'missing-parent-field'])
})

test('BOS-1327 validator: missing-epic-label and missing-state-roles are payload-level defects', () => {
  const report = validateDependencyScanInput(scanPayload({ epicLabel: '  ', stateRoles: {} }))
  assert.deepEqual(report.defects, [
    {
      code: 'missing-epic-label',
      id: 'payload',
      remedy: "set epicLabel to labelName(config, 'epic')",
    },
    {
      code: 'missing-state-roles',
      id: 'payload',
      remedy: 'set stateRoles to stateRolesFor(config)',
    },
  ])
  const omitted = scanPayload()
  delete omitted.epicLabel
  delete omitted.stateRoles
  assert.deepEqual(defectCodes(omitted), ['missing-epic-label', 'missing-state-roles'])
})

test('BOS-1327 validator: never throws, and a garbage payload is refused rather than accepted', () => {
  for (const garbage of [undefined, null, 'x', 7, [], { candidates: 'nope' }]) {
    const report = validateDependencyScanInput(garbage)
    assert.equal(report.ok, false, `${JSON.stringify(garbage)} must not validate`)
  }
})

test('BOS-1327 shared: every classifier result carries a shared array', () => {
  const overlapping = classify({
    candidate: candidate({ priority: 1 }),
    subjectAreas: ['app/api/x.go'],
    candidateAreas: ['app/api/x.go'],
  })
  assert.deepEqual(overlapping.shared, ['app/api/x.go'])
  assert.equal(overlapping.edge, 'relatedTo')
  for (const row of [
    classify({ subjectAreas: ['app/api'], candidateAreas: ['app/web'] }),
    classify({ subject: subject({ labels: ['Epic'] }) }),
    classify({ candidate: candidate({ id: 'uuid-subject', identifier: 'TCK-1' }) }),
    classify({ candidate: candidate({ labels: ['Epic'] }) }),
    classify({ candidate: candidate({ stateName: 'Backlog' }) }),
  ]) {
    assert.deepEqual(row.shared, [], `${row.reason} must still carry shared: []`)
  }
  const set = planDependencyEdges({
    subject: { ...subject({ epicParentId: 'uuid-epic' }), areas: ['app/api'] },
    candidates: [
      { ...candidate({ priority: 1 }), areas: ['app/api'] },
      { ...candidate({ id: 'uuid-s', identifier: 'TCK-S' }), epicParentId: 'uuid-epic' },
    ],
    declaredRelatedIds: ['TCK-99'],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  for (const entry of [...set.edges, ...set.skipped]) {
    assert.ok(Array.isArray(entry.shared), `${entry.reason} record must carry a shared array`)
  }
  assert.deepEqual(set.edges[0].shared, ['app/api'])
})

test('BOS-1327 no-candidate-areas: an all-arealess candidate set could not evaluate', () => {
  const result = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [
      { ...candidate(), areas: [] },
      { ...candidate({ id: 'uuid-3', identifier: 'TCK-3' }), areas: [] },
    ],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(result.compared, 2)
  assert.equal(result.candidatesWithoutAreas, 2)
  assert.deepEqual(result.edges, [])
  const warning = result.notes.find((entry) => entry.reason === 'no-candidate-areas')
  assert.ok(warning, 'an all-arealess set must not read as a clean scan')
  assert.equal(warning.severity, 'warning')
  assert.equal(warning.destination, 'risks')
  assert.match(warning.text, /could not evaluate/)
  const verdict = dependencyScanVerdict(result)
  assert.equal(verdict.verdict, 'could-not-evaluate')
  assert.deepEqual(verdict.reasons, ['no-candidate-areas'])
})

test('BOS-1327 no-candidate-areas stands down when one candidate has areas or a logical edge exists', () => {
  const partly = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [
      { ...candidate(), areas: [] },
      { ...candidate({ id: 'uuid-3', identifier: 'TCK-3' }), areas: ['app/web'] },
    ],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(partly.candidatesWithoutAreas, 1)
  assert.equal(
    partly.notes.some((entry) => entry.reason === 'no-candidate-areas'),
    false,
  )
  // AC: compared > 0, zero edges, no could-not-evaluate reason -> an explicit no-dependencies.
  assert.deepEqual(dependencyScanVerdict(partly), {
    verdict: 'no-dependencies',
    compared: 2,
    edges: 0,
    relatedTo: 0,
    reasons: [],
    recordToDescription: false,
  })
  const logical = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [{ ...candidate(), areas: [] }],
    logicalDependencies: { 'TCK-2': true },
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(logical.edges.length, 1)
  assert.equal(
    logical.notes.some((entry) => entry.reason === 'no-candidate-areas'),
    false,
  )
  assert.equal(dependencyScanVerdict(logical).verdict, 'linked')
})

test('BOS-1327 same-epic: three siblings produce exactly one note, and no second save', () => {
  const siblings = ['TCK-C', 'TCK-A', 'TCK-B'].map((identifier) => ({
    ...candidate({ id: `uuid-${identifier}`, identifier }),
    epicParentId: 'uuid-epic',
    areas: ['app/api'],
  }))
  const result = planDependencyEdges({
    subject: { ...subject({ epicParentId: 'uuid-epic' }), areas: ['app/api'] },
    candidates: siblings,
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(result.skipped.filter((entry) => entry.reason === 'same-epic-member').length, 3)
  const sameEpic = result.notes.filter((entry) => entry.reason === 'same-epic-member')
  assert.equal(sameEpic.length, 1)
  assert.equal(result.notes.length, 1)
  assert.ok(
    sameEpic[0].text.startsWith('TCK-A, TCK-B, TCK-C share '),
    'members named in sorted order',
  )
  const verdict = dependencyScanVerdict(result)
  assert.equal(verdict.verdict, 'no-dependencies')
  assert.equal(verdict.recordToDescription, false, 'the consolidated note alone earns no save')
})

test('BOS-1327 landed: a started candidate with merged-PR evidence writes no blocking edge', () => {
  const started = { stateName: 'In Review', stateType: 'started' }
  const overlap = classify({
    subject: subject({ priority: 3 }),
    candidate: candidate({ priority: 1, ...started, landed: { evidence: 'merge 9f3c2e1' } }),
  })
  assert.equal(overlap.reason, 'candidate-landed')
  assert.equal(overlap.write, null)
  assert.equal(overlap.edge, 'none')
  assert.equal(overlap.note.destination, 'planning')
  assert.match(overlap.note.text, /merge 9f3c2e1/)
  const logical = classify({
    subjectAreas: ['app/api'],
    candidateAreas: ['app/web'],
    candidate: candidate({ ...started, landed: { evidence: 'merge 9f3c2e1' } }),
    logicalDependency: true,
  })
  assert.equal(logical.reason, 'prerequisite-satisfied')
  assert.equal(logical.write, null)
  assert.match(logical.note.text, /merge 9f3c2e1/)
  const blocks = classify({
    subjectAreas: ['app/api'],
    candidateAreas: ['app/web'],
    candidate: candidate({ ...started, landed: { evidence: 'merge 9f3c2e1' } }),
    logicalDependency: { direction: 'blocks' },
  })
  assert.equal(blocks.reason, 'candidate-landed')
  // An unschedulable tracker state no longer rejects a candidate whose work merged.
  assert.equal(
    classify({ candidate: candidate({ stateName: 'Backlog', landed: { evidence: 'x' } }) }).reason,
    'candidate-landed',
  )
  // The tracker's own cleared answer still wins over the evidence.
  assert.equal(
    classify({
      candidate: candidate({
        stateName: 'Done',
        stateType: 'completed',
        landed: { evidence: 'x' },
      }),
    }).reason,
    'candidate-cleared',
  )
})

test('BOS-1327 landed: blank or missing evidence changes nothing', () => {
  const plain = classify({
    subject: subject({ priority: 3 }),
    candidate: candidate({ priority: 1 }),
  })
  for (const landed of [{ evidence: '   ' }, { evidence: '' }, {}, { evidence: 42 }, null]) {
    const row = classify({
      subject: subject({ priority: 3 }),
      candidate: candidate({ priority: 1, landed }),
    })
    assert.equal(row.reason, plain.reason, `${JSON.stringify(landed)} must be ignored`)
    assert.deepEqual(row.write, plain.write)
  }
  assert.equal(
    classify({ candidate: candidate({ stateName: 'Backlog', landed: { evidence: ' ' } }) }).reason,
    'candidate-not-schedulable',
  )
})

test('BOS-1327 verdict: each outcome, and could-not-evaluate outranks everything', () => {
  assert.deepEqual(COULD_NOT_EVALUATE_REASONS, [
    'no-candidates-compared',
    'no-subject-areas',
    'subject-unresolved-areas',
    'no-candidate-areas',
  ])
  for (const reason of COULD_NOT_EVALUATE_REASONS) {
    assert.ok(DEPENDENCY_REASONS.includes(reason), `${reason} must be in the reason enum`)
  }
  const empty = planDependencyEdges({ subject: subject(), candidates: [] })
  assert.equal(dependencyScanVerdict(empty).verdict, 'could-not-evaluate')
  assert.deepEqual(dependencyScanVerdict(empty).reasons, ['no-candidates-compared'])
  const arealessSubject = planDependencyEdges({
    subject: { ...subject(), areas: [] },
    candidates: [{ ...candidate(), areas: ['app/api'] }],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.deepEqual(dependencyScanVerdict(arealessSubject).reasons, ['no-subject-areas'])
  const linked = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api/x.go'] },
    candidates: [{ ...candidate({ priority: 1 }), areas: ['app/api/x.go'] }],
    logicalDependencies: { 'TCK-2': true },
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.deepEqual(dependencyScanVerdict(linked), {
    verdict: 'linked',
    compared: 1,
    edges: 1,
    relatedTo: 0,
    reasons: [],
    recordToDescription: true,
  })
  // A plain file overlap is a non-blocking relation, recorded in the description.
  const relatedOnly = planDependencyEdges({
    subject: { ...subject({ priority: 1 }), areas: ['app/api/x.go'] },
    candidates: [{ ...candidate({ priority: 3 }), areas: ['app/api/x.go'] }],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(dependencyScanVerdict(relatedOnly).recordToDescription, true)
  assert.equal(dependencyScanVerdict(relatedOnly).verdict, 'related-only')
  assert.equal(dependencyScanVerdict(relatedOnly).relatedTo, 1)
  // Never throws on a result it cannot read; an unreadable result could not evaluate.
  for (const garbage of [undefined, null, 'x', {}]) {
    assert.equal(dependencyScanVerdict(garbage).verdict, 'could-not-evaluate')
  }
})

test('BOS-1327 transitive warnings: upstream reproduces the blocked-side rule', () => {
  const warnings = transitiveBlockWarnings({
    subjectId: ['uuid-subject', 'TCK-1'],
    writes: [{ id: 'uuid-subject', blockedBy: ['TCK-2'] }],
    relationsById: {
      'tck-2': {
        stateType: 'started',
        blockedBy: [{ identifier: 'TCK-9', stateType: 'unstarted' }, 'TCK-8'],
      },
      'TCK-8': { stateType: 'completed' },
    },
  })
  assert.deepEqual(warnings, [
    {
      direction: 'upstream',
      severity: 'warning',
      blockerId: 'TCK-2',
      blockedId: 'uuid-subject',
      via: ['TCK-9'],
      text: 'blocked by TCK-2, which is itself open and blocked by TCK-9',
    },
  ])
  assert.ok(
    warnings.every((entry) => !('write' in entry)),
    'detection only — never a write',
  )
})

test('BOS-1327 transitive warnings: a cleared or canceled intermediate produces nothing', () => {
  const base = { subjectId: 'TCK-1', writes: [{ id: 'TCK-1', blockedBy: ['TCK-2'] }] }
  assert.deepEqual(
    transitiveBlockWarnings({
      ...base,
      relationsById: {
        'TCK-2': { stateType: 'started', blockedBy: [{ id: 'TCK-9', stateType: 'canceled' }] },
      },
    }),
    [],
  )
  assert.deepEqual(
    transitiveBlockWarnings({
      ...base,
      relationsById: { 'TCK-2': { stateType: 'completed', blockedBy: ['TCK-9'] } },
    }),
    [],
    'a cleared blocker is not itself open',
  )
})

test('BOS-1327 transitive warnings: downstream fires, and escalates for a needs-human subject', () => {
  const input = {
    subjectId: 'TCK-1',
    writes: [{ id: 'TCK-2', blockedBy: ['TCK-1'] }],
    relationsById: {
      'TCK-2': {
        stateType: 'unstarted',
        blocks: ['TCK-7', { id: 'TCK-6', stateType: 'completed' }],
      },
    },
  }
  assert.deepEqual(transitiveBlockWarnings(input), [
    {
      direction: 'downstream',
      severity: 'warning',
      blockerId: 'TCK-1',
      blockedId: 'TCK-2',
      via: ['TCK-7'],
      text: 'blocks TCK-2, which itself blocks open TCK-7',
    },
  ])
  const [escalated] = transitiveBlockWarnings({ ...input, subjectAgentFriendly: false })
  assert.equal(escalated.severity, 'escalated')
  assert.deepEqual(
    transitiveBlockWarnings({ ...input, relationsById: { 'TCK-2': { blocks: [] } } }),
    [],
    'a blocked ticket that blocks nothing open raises nothing',
  )
  for (const garbage of [undefined, null, 'x', { writes: 'nope' }]) {
    assert.deepEqual(transitiveBlockWarnings(garbage), [])
  }
})

test('BOS-1327 integration: validated payload -> edges -> verdict -> transitive warnings', () => {
  const payload = scanPayload({
    subject: { ...scanPayload().subject, areas: undefined },
    candidates: [
      { ...candidate({ priority: 1 }), parentId: null, description: planBody('- `app/api/x.go`') },
    ],
  })
  assert.equal(validateDependencyScanInput(payload).ok, true)
  const withAreas = {
    ...payload,
    subjectAreas: areas(payload.subject.description).areas,
    candidates: payload.candidates.map((entry) => ({
      ...entry,
      areas: areas(entry.description).areas,
    })),
  }
  const result = planDependencyEdges({ ...withAreas, logicalDependencies: { 'TCK-2': true } })
  assert.equal(dependencyScanVerdict(result).verdict, 'linked')
  const writes = result.edges.map((entry) => entry.write).filter(Boolean)
  assert.deepEqual(writes, [{ id: 'uuid-subject', blockedBy: ['uuid-candidate'] }])
  const warnings = transitiveBlockWarnings({
    subjectId: ['uuid-subject', 'TCK-1'],
    writes,
    relationsById: { 'uuid-candidate': { stateType: 'unstarted', blockedBy: ['TCK-40'] } },
  })
  assert.deepEqual(
    warnings.map((entry) => [entry.direction, entry.blockerId, entry.via]),
    [['upstream', 'uuid-candidate', ['TCK-40']]],
  )
})

// ---------------------------------------------------------------------------
// BOS-1337 — directory overlap never blocks; escapes; shared evidence
// ---------------------------------------------------------------------------

test('BOS-1337 escapes: a tracker backslash escape is stripped before classification', () => {
  const escaped = areas(planBody('- services/marketing/public/\\_redirects: add rule\n'), {
    moduleRoots: ['services'],
  })
  assert.deepEqual(escaped.areas, ['services/marketing/public/_redirects'])
  assert.deepEqual(escaped.unresolved, [])
  // An escaped glob star behaves exactly as the unescaped glob.
  for (const moduleRoots of [[], ['services']]) {
    assert.deepEqual(
      areas(planBody('- `dir/\\*.ext` placeholder\n'), { moduleRoots }),
      areas(planBody('- `dir/*.ext` placeholder\n'), { moduleRoots }),
      `moduleRoots=${JSON.stringify(moduleRoots)}`,
    )
  }
})

test('BOS-1337 fileShared: only a same FILE is file-shared; containment is shared only', () => {
  const same = areasOverlap(['app/api/x.go'], ['app/api/x.go'])
  assert.deepEqual(same, { overlap: true, shared: ['app/api/x.go'], fileShared: ['app/api/x.go'] })
  for (const [a, b] of [
    [['app/api'], ['app/api/x.go']],
    [['app/api/x.go'], ['app/api']],
  ]) {
    const contained = areasOverlap(a, b)
    assert.equal(contained.overlap, true, 'containment still reports overlap')
    assert.deepEqual(contained.shared, ['app/api/x.go'])
    assert.deepEqual(contained.fileShared, [], `${a} vs ${b} is directory-level`)
  }
  // The same DIRECTORY on both sides names no file, so it is directory-level too.
  assert.deepEqual(areasOverlap(['app/api'], ['app/api']).fileShared, [])
  // An aliased mirror of one file is the same file.
  const aliased = areasOverlap(['app/api/x.go', 'app/b.ts'], ['gen/api/x.go', 'app/b.ts'], {
    areaAliases: { 'app/api/x.go': 'gen/api/x.go' },
  })
  assert.deepEqual(aliased.fileShared, ['app/b.ts', 'gen/api/x.go'], 'sorted, alias-expanded')
  // Repo-wide exclusion applies to fileShared exactly as to shared.
  const wide = areasOverlap(['docs/x.md'], ['docs/x.md'], { repoWideTokens: ['docs/x.md'] })
  assert.deepEqual(wide, { overlap: false, shared: [], fileShared: [] })
})

test('BOS-1337 directory-overlap: a directory-only pair is relatedTo in every direction', () => {
  const prose = areas(
    '## Planning\n\n- Contract: v1\n\nThe callback dispatcher in services/boss drops the retry when the chat closes.\n',
    { moduleRoots: ['services'] },
  )
  assert.equal(prose.source, 'fallback-text')
  assert.deepEqual(prose.areas, ['services/boss'])
  for (const [label, subjectAreas, candidateAreas, shared] of [
    [
      'subject directory',
      ['services/bosso/internal/server'],
      ['services/bosso/internal/server/billing.go'],
      'services/bosso/internal/server/billing.go',
    ],
    [
      'candidate directory',
      ['services/bosso/internal/server/billing.go'],
      ['services/bosso/internal/server'],
      'services/bosso/internal/server/billing.go',
    ],
    ['prose fallback', ['services/boss/cmd/x.go'], prose.areas, 'services/boss/cmd/x.go'],
    ['extensionless file', ['Makefile'], ['Makefile'], 'makefile'],
  ]) {
    const row = classify({ subjectAreas, candidateAreas })
    assert.equal(row.edge, 'relatedTo', label)
    assert.equal(row.reason, 'directory-overlap', label)
    assert.equal(row.basis, 'overlap', label)
    assert.equal(row.write, null, label)
    assert.equal(row.question, null, `${label}: never reaches orientation`)
    assert.deepEqual(row.shared, [shared], label)
    assert.equal(row.note.severity, 'info', label)
    assert.equal(row.note.destination, 'planning', label)
    assert.equal(row.note.reason, 'directory-overlap', label)
    assert.ok(row.note.text.includes(`(shared: ${shared})`), `${label}: the note names shared`)
    assert.ok(row.note.text.includes('TCK-1') && row.note.text.includes('TCK-2'), label)
  }
  // Non-vacuity: the same file named on both sides is a different reason, still non-blocking.
  const sameFile = classify({
    subjectAreas: ['services/bosso/internal/server/billing.go'],
    candidateAreas: ['services/bosso/internal/server/billing.go'],
  })
  assert.equal(sameFile.reason, 'file-overlap')
  assert.equal(sameFile.edge, 'relatedTo')
  assert.equal(sameFile.write, null)
})

test('BOS-1337 directory-overlap: cleared state and a logical basis both take precedence', () => {
  const cleared = classify({
    candidate: candidate({ stateName: 'Done', stateType: 'completed' }),
    subjectAreas: ['app/api'],
    candidateAreas: ['app/api/x.go'],
  })
  assert.equal(
    cleared.reason,
    'candidate-cleared',
    'rung 4 still drops a cleared candidate quietly',
  )
  assert.equal(cleared.note, null)
  const logical = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({ priority: 4 }),
    subjectAreas: ['app/api'],
    candidateAreas: ['app/api/x.go'],
    logicalDependency: true,
  })
  assert.equal(logical.reason, 'oriented-by-logical', 'a logical basis is untouched')
  assert.deepEqual(logical.write, { id: 'uuid-subject', blockedBy: ['uuid-candidate'] })
  // The relation still needs both ids: an id-less side stops it, as it stops a blocking write.
  const idless = classify({
    candidate: { priority: 3, stateName: 'Planned', stateType: 'unstarted', labels: [] },
    subjectAreas: ['app/api'],
    candidateAreas: ['app/api/x.go'],
  })
  assert.equal(idless.reason, 'unidentifiable-issue')
  assert.equal(idless.edge, 'none')
})

test('BOS-1337 set level: a directory-only pair lands in edges as a related-only verdict', () => {
  const result = planDependencyEdges({
    subject: { ...subject(), areas: ['app/api/x.go'] },
    candidates: [{ ...candidate(), areas: ['app/api'] }],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(result.edges.length, 1)
  assert.equal(result.edges[0].reason, 'directory-overlap')
  assert.equal(result.questions.length, 0)
  assert.equal(dependencyScanVerdict(result).verdict, 'related-only')
})

// ---------------------------------------------------------------------------
// BOS-1337 — the extractor control corpus
// ---------------------------------------------------------------------------
//
// Every fabricating shape the dependency-scan notes have recorded, pinned to its
// EXACT `{areas, unresolved}`. A change that widens or narrows the extractor
// fails a row here, and editing that row's expected value IS the added/removed
// report a widening must carry. Edit a failing row deliberately, with the reason
// in the commit — never delete one to make the table pass.
const EXTRACTOR_CONTROL_CORPUS = [
  {
    name: 'prose fallback: a slashed directory in a sentence is an area (U2 makes it relatedTo)',
    description:
      '## Planning\n\n- Contract: v1\n\nThe callback dispatcher in services/boss drops the retry when the chat closes.\n',
    moduleRoots: ['services'],
    source: 'fallback-text',
    expected: { areas: ['services/boss'], unresolved: [] },
  },
  {
    name: 'a bare directory bullet is an area',
    body: '- `services/bosso/internal/server` — the billing handlers\n',
    moduleRoots: ['services'],
    expected: { areas: ['services/bosso/internal/server'], unresolved: [] },
  },
  {
    name: 'a `dir/*.ext` placeholder is unresolved, never an area (note 44aed9b0)',
    body: '- `dir/*.ext` — one per fixture\n',
    moduleRoots: ['services'],
    expected: { areas: [], unresolved: ['dir'] },
  },
  {
    name: 'a one-level glob under an undeclared root is unresolved',
    body: '- `skills-toolbox/*.mjs` are regenerated\n',
    moduleRoots: ['services'],
    expected: { areas: [], unresolved: ['skills-toolbox'] },
  },
  {
    name: 'the same glob under a declared root collapses to its directory area',
    body: '- `skills-toolbox/*.mjs` are regenerated\n',
    moduleRoots: ['services', 'skills-toolbox'],
    expected: { areas: ['skills-toolbox'], unresolved: [] },
  },
  {
    name: 'a tracker backslash escape resolves to the real path',
    body: '- services/marketing/public/\\_redirects: add rule\n',
    moduleRoots: ['services'],
    expected: { areas: ['services/marketing/public/_redirects'], unresolved: [] },
  },
  {
    name: 'a git ref quoted in prose is unresolved, never an area',
    body: '- Regenerate the fixtures — verified on `origin/main` `6a2b25eaa`\n',
    moduleRoots: ['services'],
    expected: { areas: [], unresolved: ['origin/main'] },
  },
  {
    name: 'an unmarked English module word contributes nothing',
    body: '- so the web and services teams share one shape\n',
    moduleRoots: ['web', 'services'],
    expected: { areas: [], unresolved: [] },
  },
  {
    name: 'a `file:12-20` line locator still names the file',
    body: '- `services/boss/internal/run.go:12-20` — tighten the check\n',
    moduleRoots: ['services'],
    expected: { areas: ['services/boss/internal/run.go'], unresolved: [] },
  },
  {
    name: 'a `<run-dir>/log` runtime placeholder contributes nothing',
    body: '- the verdict line is at `<run-dir>/log`\n',
    moduleRoots: ['services'],
    expected: { areas: [], unresolved: [] },
  },
]

test('extractor control corpus: every fabricating shape extracts exactly as pinned', () => {
  for (const row of EXTRACTOR_CONTROL_CORPUS) {
    const result = areas(row.description ?? planBody(row.body), { moduleRoots: row.moduleRoots })
    assert.equal(result.source, row.source ?? 'key-changes', row.name)
    assert.deepEqual(
      { areas: result.areas, unresolved: result.unresolved },
      row.expected,
      `${row.name} — a changed row is the added/removed report; edit it deliberately`,
    )
  }
})

// ---------------------------------------------------------------------------
// BOS-1337 — withScanDefaults: the repo declares its scan tuning once
// ---------------------------------------------------------------------------

const CONFIG_WITH_PLAN_DEPENDENCIES = {
  ...CONFIG_WITH_STATES,
  trackerConfig: {
    linear: { ...CONFIG_WITH_STATES.trackerConfig.linear, labels: { epic: 'Epic' } },
  },
  planDependencies: {
    moduleRoots: ['app', 'docs'],
    repoWideTokens: ['docs/index.md'],
    areaAliases: { 'app/api/x.go': 'gen/api/x.go', 'app/web/y.ts': 'gen/web/y.ts' },
  },
}

test('BOS-1337 withScanDefaults: unions arrays config-first, payload aliases win per key', () => {
  const input = {
    subject: subject(),
    candidates: [],
    moduleRoots: ['lib', 'app'],
    repoWideTokens: ['CHANGELOG.md'],
    areaAliases: { 'app/web/y.ts': ['mirror/web/y.ts'] },
  }
  const frozen = JSON.stringify(input)
  const out = withScanDefaults(CONFIG_WITH_PLAN_DEPENDENCIES, input)
  assert.deepEqual(out.moduleRoots, ['app', 'docs', 'lib'])
  assert.deepEqual(out.repoWideTokens, ['docs/index.md', 'CHANGELOG.md'])
  assert.deepEqual(out.areaAliases, {
    'app/api/x.go': 'gen/api/x.go',
    'app/web/y.ts': ['mirror/web/y.ts'],
  })
  assert.equal(JSON.stringify(input), frozen, 'pure: the payload is never mutated')
  assert.notEqual(out, input)
  assert.equal(out.subject, input.subject, 'every other field passes through')
})

test('BOS-1337 withScanDefaults: fills epicLabel and stateRoles only when absent', () => {
  const filled = withScanDefaults(CONFIG_WITH_PLAN_DEPENDENCIES, { candidates: [] })
  assert.equal(filled.epicLabel, 'Epic')
  assert.deepEqual(filled.stateRoles, stateRolesFor(CONFIG_WITH_STATES))
  const explicit = withScanDefaults(CONFIG_WITH_PLAN_DEPENDENCIES, {
    epicLabel: 'Initiative',
    stateRoles: { Doing: 'inProgress' },
  })
  assert.equal(explicit.epicLabel, 'Initiative')
  assert.deepEqual(explicit.stateRoles, { Doing: 'inProgress' })
  // An unconfigured repo: empty tuning, and an unresolvable role is left for the
  // validator to name rather than thrown.
  const bare = withScanDefaults(CONFIG, { candidates: [] })
  assert.deepEqual(
    [bare.moduleRoots, bare.repoWideTokens, bare.areaAliases, bare.epicLabel],
    [[], [], {}, undefined],
  )
  const codes = validateDependencyScanInput(bare).defects.map((entry) => entry.code)
  assert.ok(codes.includes('missing-epic-label') && codes.includes('missing-state-roles'))
  // Only the role-resolution error is absorbed: a malformed config (no `adapters`)
  // is a programming fault, and it must surface rather than become a payload defect.
  assert.throws(
    () => withScanDefaults({ ...CONFIG, adapters: undefined }, { candidates: [] }),
    TypeError,
  )
  // Config-first, like every config-reading export; a garbage payload passes through.
  assert.throws(() => withScanDefaults({ candidates: [] }, CONFIG), /withScanDefaults/)
  assert.equal(withScanDefaults(CONFIG, 'x'), 'x')
})

test('BOS-1337 withScanDefaults: config-declared repoWideTokens suppress a shared file end to end', () => {
  const payload = withScanDefaults(CONFIG_WITH_PLAN_DEPENDENCIES, {})
  const shared = (repoWideTokens) =>
    classify({
      subjectAreas: ['docs/index.md'],
      candidateAreas: ['docs/index.md'],
      repoWideTokens,
    }).reason
  assert.equal(shared(payload.repoWideTokens), 'no-overlap')
  assert.equal(shared([]), 'file-overlap', 'non-vacuity: undeclared, the file overlaps')
})

// GIG-461: lead targets and references are deliberately asymmetric.
const REFERENCE_ROWS = [
  ['called path', '- `app/a.ts`: calls `app/b.ts`', ['app/a.ts'], [], ['app/b.ts']],
  ['read path', '- `app/a.ts` — reads `app/b.ts`', ['app/a.ts'], [], ['app/b.ts']],
  ['both edited', '- `app/a.ts`, `app/b.ts` — edit both', ['app/a.ts', 'app/b.ts'], [], []],
  [
    'tail directories',
    '- `app/a.ts` – assert nothing under `app` or `web/lib`',
    ['app/a.ts'],
    [],
    ['app', 'web/lib'],
  ],
  ['lead directory', '- `web/lib` - edit the helpers', ['web/lib'], [], []],
  ['span locator', '- `app/a.ts:12`, `app/b.ts` — edit both', ['app/a.ts', 'app/b.ts'], [], []],
  ['separator in span', '- `app/a.ts`, `a - b` — reads `app/c.ts`', ['app/a.ts'], [], ['app/c.ts']],
  ['path-free lead', '- Note: replace the map in app/api/router.ts', ['app/api/router.ts'], [], []],
  ['unchanged', '- `app/a.ts`: unchanged', [], [], ['app/a.ts']],
  ['no change', '- `app/b.ts` — no change', [], [], ['app/b.ts']],
  ['stays as-is', '- `app/c.ts` stays as-is.', [], [], ['app/c.ts']],
  ['prefix declaration', '- Not touched: `app/d.ts`, `app/e.ts`', [], [], ['app/d.ts', 'app/e.ts']],
  ['later unchanged', '- `app/a.ts`: rename helper; behaviour unchanged', ['app/a.ts'], [], []],
  [
    'qualifier opens edit',
    '- `app/k.mjs`: unchanged behaviour; add doc comment',
    ['app/k.mjs'],
    [],
    [],
  ],
  ['flag qualifier', '- `app/h.sh`: no change to `--x`; adjust list', ['app/h.sh'], [], []],
  ['colon opens edit', '- `app/a.ts`: unchanged: add helper', ['app/a.ts'], [], []],
  [
    'terminal rationale',
    '- `app/a.ts`: read-only — called by `app/b.ts`',
    [],
    [],
    ['app/a.ts', 'app/b.ts'],
  ],
  [
    'cross-entry area',
    '- `app/a.ts`: edit\n- `app/b.ts`: reads `app/a.ts`',
    ['app/a.ts', 'app/b.ts'],
    [],
    [],
  ],
  [
    'cross-entry unresolved',
    '- `SKILL.md`: edit\n- `app/b.ts`: reads `SKILL.md`',
    ['app/b.ts'],
    ['skill.md'],
    [],
  ],
  ['declared root file', '- AGENTS.md — update rules', ['agents.md'], [], []],
  ['marked root file', '- `AGENTS.md` — update rules', ['agents.md'], [], []],
  ['declared root file separator-free prose', '- touch AGENTS.md', [], [], []],
  ['declared root file separator-free marked', '- touch `AGENTS.md`', ['agents.md'], [], []],
  ['undeclared basename', '- touch SKILL.md', [], ['skill.md'], []],
  ['root file prose', '- Note: see README.md for context', [], [], []],
]
for (const [name, body, expectedAreas, unresolved, referenced] of REFERENCE_ROWS) {
  test('GIG-461 referenced: ' + name, () => {
    const result = areas(planBody(body + '\n'), {
      moduleRoots: ['app', 'web', 'AGENTS.md', 'README.md'],
    })
    assert.deepEqual(
      { areas: result.areas, unresolved: result.unresolved, referenced: result.referenced },
      { areas: expectedAreas, unresolved, referenced },
    )
    if (referenced.length) assert.deepEqual(result.arealessEntries, [])
  })
}
test('GIG-461 referenced: every return shape and free-text root-file provenance', () => {
  for (const body of [
    '',
    planBody(''),
    'See README.md for context',
    '- README.md — update rules',
    planBody('- prose only'),
  ]) {
    const result = areas(body, { moduleRoots: ['README.md'] })
    assert.deepEqual(result.referenced, [])
    assert.deepEqual(result.areas, [])
  }
})

// ---------------------------------------------------------------------------
// BOS-1362 — a complete candidate set, honest epic-parent and empty-backlog results,
// and root files that are never silently dropped
// ---------------------------------------------------------------------------

function epicFixture() {
  const parent = {
    ...candidate({ id: 'uuid-p', identifier: 'TCK-P', labels: ['Epic'] }),
    parentId: null,
    areas: ['app/api'],
  }
  const child = {
    ...candidate({ id: 'uuid-c', identifier: 'TCK-C' }),
    parentId: 'uuid-p',
    areas: ['app/web'],
  }
  return { parent, child }
}

function epicRun(candidates, over = {}) {
  return planDependencyEdges({
    subject: { ...subject(), parentId: null, areas: ['app/api'] },
    candidates,
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
    ...over,
  })
}

test('BOS-1362: an epic parent whose child is in a complete set is expanded in-set', () => {
  const { parent, child } = epicFixture()
  const complete = epicRun([parent, child], { candidateSetComplete: true })
  const parentSkip = complete.skipped.find((entry) => entry.identifier === 'TCK-P')
  assert.equal(parentSkip.expansion, 'in-set')
  assert.equal(parentSkip.expandChildren, false)
  assert.deepEqual(parentSkip.children, ['TCK-C'])
  assert.equal(complete.candidateSetComplete, true)
  assert.equal(
    complete.compared,
    2,
    'the child is compared once, as the queued candidate it already was — never re-enqueued',
  )
  // A child keyed by identifier rather than uuid still names its parent.
  const byIdentifier = epicRun([parent, { ...child, parentId: 'TCK-P' }], {
    candidateSetComplete: true,
  })
  assert.deepEqual(byIdentifier.skipped.find((entry) => entry.identifier === 'TCK-P').children, [
    'TCK-C',
  ])

  // Back-compat: without the flag the caller is still sent to fetch the children.
  const incomplete = epicRun([parent, child])
  const pending = incomplete.skipped.find((entry) => entry.identifier === 'TCK-P')
  assert.equal(pending.expansion, 'pending')
  assert.equal(pending.expandChildren, true)
  assert.equal(pending.children, undefined)
  assert.equal(incomplete.candidateSetComplete, false)
})

test('BOS-1362: an epic parent with no active child in a complete set is answered, not re-asked', () => {
  const { parent } = epicFixture()
  const result = epicRun([parent], { candidateSetComplete: true })
  const parentSkip = result.skipped.find((entry) => entry.identifier === 'TCK-P')
  assert.equal(parentSkip.expansion, 'in-set')
  assert.deepEqual(parentSkip.children, [])
  assert.equal(parentSkip.expandChildren, false)
  // Supplied children still win over the in-set read.
  const { child } = epicFixture()
  const supplied = epicRun([parent], {
    candidateSetComplete: true,
    childrenByParentId: { 'uuid-p': [child] },
  })
  assert.equal(supplied.skipped.find((entry) => entry.identifier === 'TCK-P').expansion, 'supplied')
})

test('BOS-1362: a supplied or in-set epic parent adds no epic-parent note and earns no save', () => {
  const { parent, child } = epicFixture()
  const supplied = epicRun([parent], { childrenByParentId: { 'uuid-p': [child] } })
  assert.deepEqual(
    supplied.notes.filter((entry) => entry.reason === 'epic-parent'),
    [],
    'the "compare against its active children instead" note is stale once the children were compared',
  )
  assert.equal(dependencyScanVerdict(supplied).verdict, 'no-dependencies')
  assert.equal(dependencyScanVerdict(supplied).recordToDescription, false)

  const inSet = epicRun([parent, child], { candidateSetComplete: true })
  assert.deepEqual(
    inSet.notes.filter((entry) => entry.reason === 'epic-parent'),
    [],
  )
  assert.equal(dependencyScanVerdict(inSet).recordToDescription, false)

  // Non-vacuity: while the note is still an instruction it ships, and earns a save.
  const pending = epicRun([parent, child])
  assert.equal(pending.notes.filter((entry) => entry.reason === 'epic-parent').length, 1)
  assert.equal(dependencyScanVerdict(pending).recordToDescription, true)
  const capped = epicRun([parent], { maxExpansionDepth: 0 })
  assert.equal(capped.skipped[0].expansion, 'depth-capped')
  assert.equal(capped.notes.filter((entry) => entry.reason === 'epic-parent').length, 1)
})

test('supplied children of an epic parent at the depth cap read depth-capped, keep the note, and go unexamined', () => {
  const { parent } = epicFixture()
  const child = {
    ...candidate({ id: 'uuid-c', identifier: 'TCK-C' }),
    parentId: 'uuid-p',
    areas: ['app/api'],
  }
  const supplied = { childrenByParentId: { 'uuid-p': [child] } }
  const capped = epicRun([parent], { ...supplied, maxExpansionDepth: 0 })
  const parentSkip = capped.skipped.find((entry) => entry.identifier === 'TCK-P')
  assert.equal(
    parentSkip.expansion,
    'depth-capped',
    'the cap wins over supplied children it never queued',
  )
  assert.equal(parentSkip.expandChildren, false)
  assert.notEqual(parentSkip.note, null, 'a capped parent keeps its epic-parent note')
  assert.equal(capped.notes.filter((entry) => entry.reason === 'epic-parent').length, 1)
  assert.equal(
    capped.compared,
    1,
    'only the parent was evaluated; the supplied child past the cap was not',
  )
  assert.equal(
    [...capped.edges, ...capped.skipped].some((entry) => entry.identifier === 'TCK-C'),
    false,
  )
  // Non-vacuity: one rung of headroom queues and examines the same child.
  const within = epicRun([parent], { ...supplied, maxExpansionDepth: 1 })
  assert.equal(within.skipped.find((entry) => entry.identifier === 'TCK-P').expansion, 'supplied')
  assert.equal(within.compared, 2)
})

test('BOS-1362: a complete set holding only the subject is no-candidates, not could-not-evaluate', () => {
  const only = { ...subject(), parentId: null, areas: ['app/api'] }
  const complete = planDependencyEdges({
    subject: only,
    candidates: [only],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
    candidateSetComplete: true,
  })
  assert.equal(complete.compared, 0)
  assert.deepEqual(
    complete.notes.filter((entry) => entry.reason === 'no-candidates-compared'),
    [],
  )
  assert.deepEqual(dependencyScanVerdict(complete), {
    verdict: 'no-candidates',
    compared: 0,
    edges: 0,
    relatedTo: 0,
    reasons: [],
    recordToDescription: false,
  })

  const incomplete = planDependencyEdges({
    subject: only,
    candidates: [only],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(dependencyScanVerdict(incomplete).verdict, 'could-not-evaluate')
  assert.deepEqual(dependencyScanVerdict(incomplete).reasons, ['no-candidates-compared'])
  assert.equal(dependencyScanVerdict(incomplete).recordToDescription, true)

  // A subject whose own areas are broken is still not a clean result.
  const unresolved = planDependencyEdges({
    subject: only,
    candidates: [only],
    subjectUnresolvedAreas: ['skill.md'],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
    candidateSetComplete: true,
  })
  assert.equal(dependencyScanVerdict(unresolved).verdict, 'could-not-evaluate')
  assert.deepEqual(dependencyScanVerdict(unresolved).reasons, ['subject-unresolved-areas'])
  // A truthy non-boolean is not the flag.
  const loose = planDependencyEdges({ subject: only, candidates: [only], candidateSetComplete: 1 })
  assert.equal(dependencyScanVerdict(loose).verdict, 'could-not-evaluate')
})

test('BOS-1362: a known root file is an area when marked or a split lead, unresolved elsewhere', () => {
  const ROOT_FILES = ['Makefile', '.bazelrc', '.boss-skills.json']
  const withRoots = (body) =>
    areas(planBody(`${body}\n`), { moduleRoots: ['scripts'], rootFiles: ROOT_FILES })
  const without = (body) => areas(planBody(`${body}\n`), { moduleRoots: ['scripts'] })
  const pick = (result) => ({
    areas: result.areas,
    unresolved: result.unresolved,
    referenced: result.referenced,
  })

  assert.deepEqual(pick(withRoots('- Makefile: add a target')), {
    areas: ['makefile'],
    unresolved: [],
    referenced: [],
  })
  assert.deepEqual(pick(withRoots('- `.bazelrc`: x')), {
    areas: ['.bazelrc'],
    unresolved: [],
    referenced: [],
  })
  assert.deepEqual(pick(withRoots('- Edit the root Makefile and .bazelrc to add X')), {
    areas: [],
    unresolved: ['makefile', '.bazelrc'],
    referenced: [],
  })
  assert.deepEqual(pick(withRoots('- `scripts/x.mjs`: called from the Makefile')), {
    areas: ['scripts/x.mjs'],
    unresolved: [],
    referenced: ['makefile'],
  })

  // Without rootFiles every one of these returns what it returned before.
  assert.deepEqual(pick(without('- Makefile: add a target')), {
    areas: [],
    unresolved: [],
    referenced: [],
  })
  assert.deepEqual(pick(without('- `.bazelrc`: x')), {
    areas: [],
    unresolved: ['.bazelrc'],
    referenced: [],
  })
  assert.deepEqual(pick(without('- Edit the root Makefile and .bazelrc to add X')), {
    areas: [],
    unresolved: ['.bazelrc'],
    referenced: [],
  })

  // The shape the dependency step used before: the whole root listing unioned into
  // `moduleRoots`. An unmarked root file outside a lead vanished entirely; a known
  // root file is tested first, so the same listing now reports it.
  const legacyRoots = { moduleRoots: ['scripts', ...ROOT_FILES] }
  const prose = planBody('- Edit the root Makefile and .bazelrc to add X\n')
  assert.deepEqual(pick(areas(prose, legacyRoots)), { areas: [], unresolved: [], referenced: [] })
  assert.deepEqual(pick(areas(prose, { ...legacyRoots, rootFiles: ROOT_FILES })), {
    areas: [],
    unresolved: ['makefile', '.bazelrc'],
    referenced: [],
  })
  assert.deepEqual(pick(without('- `scripts/x.mjs`: called from the Makefile')), {
    areas: ['scripts/x.mjs'],
    unresolved: [],
    referenced: [],
  })
})
