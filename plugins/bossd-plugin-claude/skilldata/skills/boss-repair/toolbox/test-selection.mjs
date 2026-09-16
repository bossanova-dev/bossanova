// test-selection.mjs — turn a changed-file set into a test-file set, or refuse.
//
// A PURE decision function: it returns a decision object and never executes anything.
// No process spawn, no filesystem walk, no version-control call — the caller already
// knows its own diff and hands it in. That purity is what makes this module publishable
// into every user's global skill directory: it consumes only the resolved `testSelection`
// config block plus the caller's inputs, and carries no repo layout, module name, runner
// or build tool of its own.
//
// The governing posture is FAIL-SAFE, not fail-fast: under-selection is the only
// unacceptable error, so anything this module cannot classify with confidence resolves to
// a full run. There are exactly two outcomes — a narrow set it can defend, or `full`.
// "The mapping produced nothing" is a full run, never a pass — with one stated limit:
// that guarantee is complete only when the caller supplies a `testFiles` universe. See
// the NOTE on stop 5.
//
// The ladder, evaluated in order, first stop wins:
//
//   1. no `testSelection` declared        -> full · no-selection-config
//   2. no changed files supplied          -> full · no-changed-files
//   3. a changed file matches sharedFiles -> full · shared-file (names the path)
//   4. a changed file matched by no rule  -> full · unclassified (names the path)
//   5. the computed selection is empty    -> full · empty-selection  [see note]
//
// NOTE on stop 5: it can only fire when the caller supplies `testFiles`, the universe of
// test files it knows about. Without one this module cannot expand a rule's `tests`
// patterns, so it returns them VERBATIM for the caller's runner to expand, and a pattern
// that expands to nothing is not knowable here. In that pass-through mode stop 5 is
// unreachable and enforcing R2 ("no empty pass") is the CALLER's obligation. The decision
// says which mode produced it in `resolved`, and the report line says so in words.
//   6. otherwise                          -> narrow · the union of matching rules' tests
//
// Stop 3 MUST precede stop 4 and the rule matching underneath it. A lockfile, a shared
// fixture or a test helper will often also match a rule; were rule matching to run first,
// this module would hand back a confident narrow selection for exactly the change that
// invalidates narrow selection. The order is pinned by a test that gives one path both a
// shared-file pattern and a matching rule.

import { globToRegExp, testSelection } from './skill-config.mjs'

/** Machine-readable reason codes. Structured, so a fail-safe branch is pinned by a test
 *  asserting a code rather than by pattern-matching English out of the report line. */
export const SELECTION_REASONS = Object.freeze({
  NO_SELECTION_CONFIG: 'no-selection-config',
  NO_CHANGED_FILES: 'no-changed-files',
  SHARED_FILE: 'shared-file',
  UNCLASSIFIED: 'unclassified',
  EMPTY_SELECTION: 'empty-selection',
})

export const FULL = 'full'
export const NARROW = 'narrow'

// A path this module cannot place is the unclassified case, and an absolute path is
// exactly that: relativizing one would require knowing where the repo root is, which is a
// filesystem fact this module deliberately does not have. Covers POSIX roots and the
// drive-letter form, so a caller on either platform gets the fail-safe rather than a
// pattern that silently never matches.
const ABSOLUTE_PATH = /^(?:[/\\]|[A-Za-z]:[/\\])/

// Control characters are stripped from any path echoed into the report, so a path
// carrying a newline or an escape byte cannot break the single-line, escape-free contract
// the report owes its readers.
const CONTROL_CHARS = /[\x00-\x1f\x7f]/g

function sanitize(value) {
  return String(value).replace(CONTROL_CHARS, ' ').replace(/\s+/g, ' ').trim()
}

/**
 * Compile one pattern to a predicate, memoized per call so a large changed-file list does
 * not recompile the same pattern once per path.
 *
 * Two forms, which is what "glob/prefix" means here:
 *  - a pattern ending in '/' is a directory PREFIX (`src/` matches `src/deep/a.js`);
 *  - anything else is an anchored glob, via the one glob compiler in the toolbox rather
 *    than a second matcher grown here. Its semantics fit: anchored whole-path matching,
 *    '*' stopping at '/', '**' crossing it, and '**' + '/' meaning zero-or-more segments.
 *    Its documented limitation carries too — brace alternation is literal, so a config
 *    author spells multiple extensions as multiple patterns.
 */
function predicateFor(cache, pattern) {
  let predicate = cache.get(pattern)
  if (predicate === undefined) {
    if (pattern.endsWith('/')) {
      predicate = (path) => path.startsWith(pattern)
    } else {
      const re = globToRegExp(pattern)
      predicate = (path) => re.test(path)
    }
    cache.set(pattern, predicate)
  }
  return predicate
}

function matchesAny(cache, patterns, path) {
  return patterns.some((pattern) => predicateFor(cache, pattern)(path))
}

/** Repo-relative normalization. Returns null for anything unusable, which the caller
 *  turns into the unclassified fail-safe rather than dropping the path. */
function normalizeRepoPath(raw) {
  if (typeof raw !== 'string') return null
  let value = raw.trim()
  while (value.startsWith('./')) value = value.slice(2)
  if (value.length === 0) return null
  if (ABSOLUTE_PATH.test(value)) return null
  return value
}

function pushUnique(list, value) {
  if (!list.includes(value)) list.push(value)
}

function buildDecision(mode, { tests = [], reasons = [], totalCount = null, resolved = false }) {
  const decision = {
    mode,
    tests,
    // Whether `tests` holds concrete FILES resolved against a caller-supplied universe
    // (true) or unexpanded PATTERNS the caller must still expand (false). A consumer that
    // must enforce R2 reads this: on `false` the empty-selection guard did not run here.
    resolved,
    selectedCount: tests.length,
    totalCount,
    reasons,
  }
  decision.report = renderSelectionReport(decision)
  return decision
}

/**
 * Render a decision to a single log line: no ANSI escapes, no newline, no trailing
 * whitespace, safe to embed in any core's log.
 *
 * A narrow line carries the saving. A full line carries the REASONS instead — a reader
 * seeing a full run needs to know which branch forced it far more than they need a count —
 * and names every one of them, because a run forced by both a shared file and an
 * unclassified path is two separate things for its reader to fix.
 *
 * The total comes from `testRoots`. When it is unknown the line says so rather than
 * printing a denominator the config never declared; an unknown total is a reporting
 * concern and never, on its own, a reason to force a full run.
 */
export function renderSelectionReport(decision) {
  if (decision.mode === NARROW) {
    const selected = decision.selectedCount
    // Pass-through: these are PATTERNS, not files. Saying "test files" here would report a
    // pattern count as a file count and hide that the empty-selection guard never ran.
    if (!decision.resolved) {
      const noun = selected === 1 ? 'test pattern' : 'test patterns'
      return `test-selection: narrow run, selected ${selected} unexpanded ${noun}; no test-file universe supplied, so the empty-selection guard did not run`
    }
    if (decision.totalCount === null) {
      const noun = selected === 1 ? 'test file' : 'test files'
      return `test-selection: narrow run, selected ${selected} ${noun} of an unknown total`
    }
    const noun = decision.totalCount === 1 ? 'test file' : 'test files'
    return `test-selection: narrow run, selected ${selected} of ${decision.totalCount} ${noun}`
  }
  const forced = decision.reasons
    .map((reason) =>
      reason.path === undefined ? reason.code : `${reason.code} (${sanitize(reason.path)})`,
    )
    .join(', ')
  return `test-selection: full run forced by ${forced || 'an unstated reason'}`
}

/**
 * Decide which tests a changed-file set needs.
 *
 * @param {object} input
 * @param {object} input.config       Resolved skill config; the `testSelection` block is
 *                                    read through the accessor, so an absent or unusable
 *                                    block inherits that contract's `null` rather than
 *                                    being re-derived here.
 * @param {string[]} input.changedFiles  Repo-relative changed paths, supplied by the caller.
 * @param {string[]} [input.testFiles]   OPTIONAL universe of test files the caller knows
 *                                    about. When supplied, a rule's `tests` patterns are
 *                                    resolved against it and `testRoots` counts against it
 *                                    for the total. When omitted, the patterns pass
 *                                    through verbatim for the caller's own runner to
 *                                    expand, and the total stays unknown — in that mode
 *                                    the empty-selection stop cannot fire, because a
 *                                    pattern's emptiness is not knowable without a universe.
 * @returns {{mode: string, tests: string[], resolved: boolean, selectedCount: number, totalCount: number|null,
 *            reasons: Array<{code: string, path?: string}>, report: string}}
 */
export function decideTestSelection({ config, changedFiles, testFiles } = {}) {
  const block = testSelection(config)
  const cache = new Map()
  // Hoisted above stop 1 so every decision — including the early returns — can state which
  // mode produced it.
  const hasUniverse = Array.isArray(testFiles)

  // Stop 1 — no declaration at all. The opt-in path: a repo that configures nothing keeps
  // exactly the behaviour it has today, and says why.
  if (!block) {
    return buildDecision(FULL, {
      reasons: [{ code: SELECTION_REASONS.NO_SELECTION_CONFIG }],
      resolved: hasUniverse,
    })
  }

  const universe = []
  if (Array.isArray(testFiles)) {
    for (const raw of testFiles) {
      const path = normalizeRepoPath(raw)
      if (path !== null) pushUnique(universe, path)
    }
  }
  const totalCount =
    hasUniverse && block.testRoots.length > 0
      ? universe.filter((path) => matchesAny(cache, block.testRoots, path)).length
      : null

  // Stop 2 — a caller that could not determine what changed knows nothing, and knowing
  // nothing is the fail-safe's core case. An empty list is NOT a narrow run of nothing.
  if (!Array.isArray(changedFiles) || changedFiles.length === 0) {
    return buildDecision(FULL, {
      reasons: [{ code: SELECTION_REASONS.NO_CHANGED_FILES }],
      totalCount,
      resolved: hasUniverse,
    })
  }

  const shared = []
  const unclassified = []
  const matchedRules = []

  for (const raw of changedFiles) {
    const label = typeof raw === 'string' ? raw : String(raw)
    const path = normalizeRepoPath(raw)
    if (path === null) {
      pushUnique(unclassified, label)
      continue
    }
    // Stop 3 before stop 4 and before rule matching — see the ladder note at the top.
    // A shared hit also CLASSIFIES the path: it is recognized as invalidating, not unknown,
    // so it contributes one reason rather than two.
    if (matchesAny(cache, block.sharedFiles, path)) {
      pushUnique(shared, label)
      continue
    }
    const hits = block.rules.filter((rule) => predicateFor(cache, rule.changed)(path))
    if (hits.length === 0) {
      pushUnique(unclassified, label)
      continue
    }
    for (const rule of hits) matchedRules.push(rule)
  }

  // Stops 3 and 4 share one verdict but report every path that forced it. Shared-file
  // reasons lead, mirroring the ladder order.
  const reasons = [
    ...shared.map((path) => ({ code: SELECTION_REASONS.SHARED_FILE, path })),
    ...unclassified.map((path) => ({ code: SELECTION_REASONS.UNCLASSIFIED, path })),
  ]
  if (reasons.length > 0) return buildDecision(FULL, { reasons, totalCount, resolved: hasUniverse })

  // Union semantics, fixed by the config contract: EVERY matching rule contributes its
  // tests. Deduplicated and stably ordered (first-seen across rules, or universe order
  // when a universe was supplied) so a caller's command line is reproducible run to run.
  const patterns = []
  for (const rule of matchedRules) {
    for (const pattern of rule.tests) pushUnique(patterns, pattern)
  }
  const tests = hasUniverse
    ? universe.filter((path) => matchesAny(cache, patterns, path))
    : patterns

  // Stop 5 — R2. A selection of zero is a full run with a stated reason, never a narrow
  // run that trivially succeeds by testing nothing.
  if (tests.length === 0) {
    return buildDecision(FULL, {
      reasons: [{ code: SELECTION_REASONS.EMPTY_SELECTION }],
      totalCount,
      resolved: hasUniverse,
    })
  }

  return buildDecision(NARROW, { tests, totalCount, resolved: hasUniverse })
}
