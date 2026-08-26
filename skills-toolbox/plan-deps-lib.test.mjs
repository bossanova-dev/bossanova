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
  DEFAULT_CANCELED_STATE_TYPES,
  DEFAULT_CLEARED_STATE_TYPES,
  DEFAULT_PRIORITY_ORDER,
  DEPENDENCY_REASONS,
  areasOverlap,
  classifyDependencyEdge,
  extractKeyChangeAreas,
  planDependencyEdges,
} from './plan-deps-lib.mjs'

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
    subjectAreas: ['app/api'],
    candidateAreas: ['app/api'],
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

test('the inlined DEFAULT_PRIORITY_ORDER still matches the scheduler ranking it copies', () => {
  // dag-scheduler keeps its PRIORITY_ORDER private, so the agreement is asserted
  // BEHAVIOURALLY: identical createdAt isolates priority as the only sort input.
  const stamp = '2026-01-01T00:00:00.000Z'
  const nodes = [4, 0, 2, 1, 3].map((priority) => ({
    id: `n${priority}`,
    priority,
    createdAt: stamp,
    blockedBy: [],
  }))
  const ordered = readyTickets(buildGraph(nodes), {
    merged: new Set(),
    failed: new Set(),
    inFlight: new Set(),
    externallyCleared: new Set(),
  }).map((node) => node.priority)
  assert.deepEqual(
    ordered,
    [...DEFAULT_PRIORITY_ORDER],
    'the copy in plan-deps-lib must rank exactly as the scheduler does, or the same two tickets get opposite orderings depending on which module decided',
  )
  assert.notDeepEqual(
    [...DEFAULT_PRIORITY_ORDER],
    [0, 1, 2, 3, 4],
    'the order must NOT be plain ascending — that is the naive a.priority - b.priority ordering the whole rung exists to avoid',
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
    'blockedBy',
    'the same pair WITH an overlap must still produce an edge',
  )
  assert.equal(overlapping.basis, 'overlap')
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
  })
  assert.equal(review.edge, 'relatedTo')
  assert.equal(review.reason, 'downgraded-candidate-started')

  const planned = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({ priority: 3 }),
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
  assert.equal(open.edge, 'blocks', 'the same fixture while still open must produce an edge')
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

test('rung 4 precedes rung 5 — a cleared candidate never reaches orientation', () => {
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
    'equal priority and equal createdAt would be ambiguous-orientation if the ladder ran orientation first',
  )
  assert.equal(cleared.question, null)

  const open = classify({
    subject: subject({ priority: 2, createdAt: stamp }),
    candidate: candidate({ priority: 2, createdAt: stamp }),
  })
  assert.equal(
    open.reason,
    'ambiguous-orientation',
    'non-vacuity: the identical fixture WITHOUT the cleared state must genuinely reach orientation and be ambiguous',
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
  assert.equal(result.edge, 'blocks')
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
  assert.equal(result.notes.length, 2)
  assert.ok(
    result.notes.every(
      (entry) =>
        entry.destination === 'planning' &&
        entry.reason === 'same-epic-member' &&
        entry.text.includes('epic'),
    ),
    'internal epic coordination must surface as planning context rather than a dependency write',
  )
})

test('an unrecognized state on the BLOCKER downgrades too, not only on the blocked side', () => {
  // The subject outranks the candidate, so the subject is the BLOCKER and the
  // candidate is the blocked side. Only the blocker's state is unmappable.
  const classified = classify({
    subject: subject({ priority: 1, stateName: 'Bikeshedding' }),
    candidate: candidate({ priority: 3 }),
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

test('orientation follows the tracker priority order, not a numeric subtraction', () => {
  const strongerSubject = classify({
    subject: subject({ priority: 2 }),
    candidate: candidate({ priority: 0 }),
  })
  assert.equal(
    strongerSubject.edge,
    'blocks',
    'priority 2 outranks priority 0 (none); a plain a.priority - b.priority makes 0 look strongest and inverts this',
  )
  assert.deepEqual(strongerSubject.write, { id: 'uuid-candidate', blockedBy: ['uuid-subject'] })

  const weakerSubject = classify({
    subject: subject({ priority: 0 }),
    candidate: candidate({ priority: 4 }),
  })
  assert.equal(
    weakerSubject.edge,
    'blockedBy',
    'priority 4 (low) still outranks 0 (none) — the second assertion a numeric compare fails',
  )
  assert.deepEqual(weakerSubject.write, { id: 'uuid-subject', blockedBy: ['uuid-candidate'] })
})

test('the {value, name} priority form ranks identically to the bare number', () => {
  const bare = classify({
    subject: subject({ priority: 2 }),
    candidate: candidate({ priority: 0 }),
  })
  const object = classify({
    subject: subject({ priority: { value: 2, name: 'High' } }),
    candidate: candidate({ priority: { value: 0, name: 'No priority' } }),
  })
  assert.equal(object.edge, bare.edge)
  assert.equal(object.reason, 'oriented-by-priority')
  assert.deepEqual(
    object.write,
    bare.write,
    'an adapter that returns the object form must not silently reorder every edge in the plan',
  )
})

test('equal priority is broken by the older createdAt', () => {
  const olderCandidate = classify({
    subject: subject({ priority: 2, createdAt: '2026-05-02T00:00:00.000Z' }),
    candidate: candidate({ priority: 2, createdAt: '2026-05-01T00:00:00.000Z' }),
  })
  assert.equal(olderCandidate.edge, 'blockedBy')
  assert.equal(olderCandidate.reason, 'oriented-by-age')

  const newerCandidate = classify({
    subject: subject({ priority: 2, createdAt: '2026-05-01T00:00:00.000Z' }),
    candidate: candidate({ priority: 2, createdAt: '2026-05-02T00:00:00.000Z' }),
  })
  assert.equal(newerCandidate.edge, 'blocks', 'the tie-break must be directional, not constant')
})

test('a genuinely balanced pair yields ambiguous-orientation deterministically, with a question', () => {
  const balanced = () =>
    classify({
      subject: subject({ priority: 2, createdAt: '2026-05-01T00:00:00.000Z' }),
      candidate: candidate({ priority: 2, createdAt: '2026-05-01T00:00:00.000Z' }),
    })
  const first = balanced()
  assert.equal(first.edge, 'none')
  assert.equal(first.reason, 'ambiguous-orientation')
  assert.equal(first.write, null)
  assert.equal(
    first.note,
    null,
    'an ambiguous orientation is a question, not a note — they route to different sections',
  )
  assert.equal(first.question.destination, 'open-questions')
  assert.ok(first.question.text.includes('TCK-2'))
  for (let attempt = 0; attempt < 3; attempt += 1) {
    assert.deepEqual(
      balanced(),
      first,
      'repeated calls must not flip a coin — epic children share a creation batch',
    )
  }
})

test('an unparseable createdAt is ambiguous, not an arbitrary edge', () => {
  const result = classify({
    subject: subject({ priority: 2, createdAt: 'sometime last week' }),
    candidate: candidate({ priority: 2, createdAt: '2026-05-01T00:00:00.000Z' }),
  })
  assert.equal(result.reason, 'ambiguous-orientation')
  assert.ok(
    result.question,
    'an unusable timestamp must surface as a question rather than silently pick a side',
  )
})

// ---------------------------------------------------------------------------
// Every result is writable as returned
// ---------------------------------------------------------------------------

test('write intent is present exactly on blocking outcomes and names the blocked side', () => {
  const rows = [
    classify({ subject: subject({ priority: 1 }), candidate: candidate({ priority: 3 }) }),
    classify({ subject: subject({ priority: 3 }), candidate: candidate({ priority: 1 }) }),
    classify({
      subject: subject({ priority: 1 }),
      candidate: candidate({ priority: 3, stateName: 'In Progress', stateType: 'started' }),
    }),
    classify({ subjectAreas: ['app/api'], candidateAreas: ['app/web'] }),
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
  for (const [subjectPriority, candidatePriority] of [
    [3, 1],
    [1, 3],
  ]) {
    const row = classify({
      subject: subject({ priority: subjectPriority }),
      candidate: { ...nameless, priority: candidatePriority },
    })
    assert.equal(row.edge, 'none', 'an unnameable side must not produce a blocking edge')
    assert.equal(row.write, null)
    assert.equal(row.reason, 'unidentifiable-issue')
    assert.equal(row.basis, 'overlap', 'the basis that WAS established is still reported')
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
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [
      {
        ...candidate({ id: 'a', identifier: 'TCK-A', priority: 1, stateName: 'Mystery' }),
        areas: ['app/api'],
      },
      {
        ...candidate({ id: 'b', identifier: 'TCK-B', priority: 1, stateName: 'Pending-ish' }),
        areas: ['app/api'],
      },
    ],
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
    subject: { ...subject(), areas: ['app/api'] },
    candidates: [
      { ...candidate({ id: 'a', identifier: 'TCK-A', stateName: 'Mystery' }), areas: ['app/api'] },
      { ...candidate({ id: 'b', identifier: 'TCK-B', priority: 1 }), areas: ['app/api'] },
    ],
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
        subject: subject({ priority: 2, createdAt: '2026-05-02T00:00:00.000Z' }),
        candidate: candidate({ priority: 2, createdAt: '2026-05-01T00:00:00.000Z' }),
      }),
      classify({ subject: subject({ priority: 2 }), candidate: candidate({ priority: 2 }) }),
      classify({
        subject: subject({ priority: 1 }),
        candidate: candidate({ priority: 3, ...started }),
      }),
      classify({
        subject: subject({ priority: 3, ...started }),
        candidate: candidate({ priority: 1 }),
      }),
      classify({
        subject: subject({ priority: 1 }),
        candidate: candidate({ priority: 3, stateName: '?' }),
      }),
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

test('a question surfaces in questions and never in notes', () => {
  const stamp = '2026-06-06T00:00:00.000Z'
  const result = planDependencyEdges({
    subject: { ...subject({ priority: 2, createdAt: stamp }), areas: ['app/api'] },
    candidates: [{ ...candidate({ priority: 2, createdAt: stamp }), areas: ['app/api'] }],
    stateRoles: STATE_ROLES,
    epicLabel: 'Epic',
  })
  assert.equal(result.questions.length, 1)
  assert.equal(result.questions[0].destination, 'open-questions')
  assert.equal(
    result.notes.length,
    0,
    'questions route to the Open Questions section and an agent-question label — a different destination from notes, so mixing them mis-files the output',
  )
  assert.equal(result.skipped[0].reason, 'ambiguous-orientation')
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

  // Non-vacuity: the identical fixture on an OVERLAP basis genuinely orients the
  // other way, so the assertion above tracks the direction and not the fixture.
  const byPriority = classify({
    subject: subject({ priority: 1 }),
    candidate: candidate({ priority: 4 }),
  })
  assert.equal(byPriority.edge, 'blocks')
  assert.equal(byPriority.reason, 'oriented-by-priority')

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
  const kept = areasOverlap(['docs/plans/x.md'], ['docs/plans/x.md'])
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
    subjectAreas: ['app/api'],
    candidateAreas: ['app/api'],
    stateRoles: STATE_ROLES,
    priorityOrder: null,
    clearedStateTypes: undefined,
    canceledStateTypes: 'completed',
  })
  assert.equal(
    decided.reason,
    'oriented-by-priority',
    'the header promises nothing here throws; a malformed tuning list must fall back to the default, not abort the run',
  )
})
