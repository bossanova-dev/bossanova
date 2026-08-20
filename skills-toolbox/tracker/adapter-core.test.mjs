import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  TRACKER_CAPABILITIES,
  OPTIONAL_TRACKER_CAPABILITIES,
  OPTIONAL_TRACKER_OPERATIONS,
  TRACKER_STATE_ROLES,
  REQUIRED_TRACKER_OPERATIONS,
  resolveTrackerAdapter,
  assertConforms,
} from './adapter-core.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))

// The reason this module exists, asserted rather than described: a consumer must be
// able to vendor the tracker CONTRACT without also taking the Linear implementation
// and its transitive helpers. Walk the transitive relative-import graph and require
// the closure to be exactly this one file. Any new `import ... from './x.mjs'` here —
// even a "harmless" one — fails this and is caught before it reaches a downstream
// repo that cannot afford the substrate.
test('adapter-core.mjs has a closure of exactly one file (no concrete-tracker coupling)', () => {
  const seen = new Set()
  const walk = (file) => {
    if (seen.has(file)) return
    seen.add(file)
    const src = readFileSync(file, 'utf8')
    // Match the `from '<relative>'` clause on its own rather than anchoring to a
    // line-leading `import`/`export`: a braced specifier list spans lines, and a
    // newline-forbidding pattern would sail straight past the most common import
    // shape in this tree and report a closure of 1 for a file that has imports.
    // Over-inclusion here is safe — this gate must fail closed.
    const specs = [
      ...src.matchAll(/\bfrom\s*['"](\.[^'"]+)['"]/g), // static import/export ... from
      ...src.matchAll(/(?:^|\n)\s*import\s*['"](\.[^'"]+)['"]/g), // side-effect import './x'
      // A lazy `await import('./x')` still drags the substrate at call time, so it
      // counts against the closure exactly like a static import.
      ...src.matchAll(/\bimport\(\s*['"](\.[^'"]+)['"]\s*\)/g),
    ]
    for (const [, spec] of specs) walk(resolve(dirname(file), spec))
  }
  walk(resolve(HERE, 'adapter-core.mjs'))

  assert.deepEqual(
    [...seen].map((f) => relative(HERE, f)).sort(),
    ['adapter-core.mjs'],
    'adapter-core.mjs must import nothing — it is the half a foreign tracker vendors alone',
  )
})

test('TRACKER_CAPABILITIES lists the full capability surface', () => {
  assert.deepEqual(TRACKER_CAPABILITIES, [
    'hasWork',
    'hasUnblockedWork',
    'readDependencies',
    'isUnblocked',
    'formatClaimComment',
    'resolveClaim',
    'normalizeTicket',
    'operationMap',
  ])
})

test('REQUIRED_TRACKER_OPERATIONS lists the full required op surface, including updateComment', () => {
  assert.deepEqual(REQUIRED_TRACKER_OPERATIONS, [
    'selectPlanned',
    'getIssue',
    'moveState',
    'readComments',
    'writeComment',
    'updateComment',
    'readLabels',
    'setPriorityEstimate',
    'appendDependency',
  ])
  // extractImages and createLabel deliberately do NOT appear here: no skill's control
  // flow depends on either (the one extractImages call site is documented best-effort,
  // createLabel has no caller), so requiring them at LOAD time rejected otherwise-usable
  // adapters over capabilities nothing depends on at CALL time.
  for (const moved of ['extractImages', 'createLabel']) {
    assert.ok(
      !REQUIRED_TRACKER_OPERATIONS.includes(moved),
      `${moved} must stay optional — requiring it fails adapters that legitimately omit it`,
    )
  }
})

test('OPTIONAL_TRACKER_CAPABILITIES lists states, and TRACKER_CAPABILITIES does NOT (BOS-524)', () => {
  assert.deepEqual(OPTIONAL_TRACKER_CAPABILITIES, ['states'])
  // The separation is the whole point: assertConforms REQUIRES every
  // TRACKER_CAPABILITIES entry, so promoting `states` there would fail every
  // conforming adapter that legitimately omits it.
  assert.ok(
    !TRACKER_CAPABILITIES.includes('states'),
    'states must stay optional — a required states would break conforming adapters without one',
  )
})

test('optional operations — attachment AND adapter-discretion — validate only when declared', () => {
  assert.deepEqual(OPTIONAL_TRACKER_OPERATIONS, [
    'preparePlanAttachment',
    'finalizePlanAttachment',
    'readPlanAttachment',
    'deletePlanAttachment',
    'extractImages',
    'createLabel',
    'appendRelatedTo',
  ])
  assert.doesNotThrow(() => assertConforms(stubAdapterWithOperationMap(validOperationMap())))

  const map = validOperationMap()
  map.preparePlanAttachment = { tool: '', summary: 'prepare upload' }
  assert.throws(
    () => assertConforms(stubAdapterWithOperationMap(map)),
    /tracker adapter operation preparePlanAttachment missing tool/,
  )
})

test('an adapter declaring ONLY the 9 required ops conforms — absence of either moved op is fine', () => {
  const map = nineRequiredOperationMap()
  // Non-vacuity: pin that the fixture genuinely lacks both keys, so this case cannot
  // pass against a map that quietly still declares them. Asserted BEFORE the conformance
  // call so a reverted list move fails on assertConforms itself, naming the missing op.
  assert.ok(!('extractImages' in map), 'fixture must genuinely omit extractImages')
  assert.ok(!('createLabel' in map), 'fixture must genuinely omit createLabel')
  assert.doesNotThrow(() => assertConforms(stubAdapterWithOperationMap(map)))
})

test('a DECLARED extractImages or createLabel is validated as strictly as a required op', () => {
  // Moving an op to the optional list changes only the meaning of ABSENCE. An adapter
  // that does declare one still owes a usable tool + summary, or the call site would
  // dispatch to a tool name that names nothing.
  for (const key of ['extractImages', 'createLabel']) {
    for (const blank of ['', ' ', '\t  ']) {
      const toolMap = validOperationMap()
      toolMap[key] = { tool: blank, summary: `summary for ${key}` }
      assert.throws(
        () => assertConforms(stubAdapterWithOperationMap(toolMap)),
        new RegExp(`tracker adapter operation ${key} missing tool`),
        `expected a throw for ${key} tool = ${JSON.stringify(blank)}`,
      )

      const summaryMap = validOperationMap()
      summaryMap[key] = { tool: `mcp__stub__${key}`, summary: blank }
      assert.throws(
        () => assertConforms(stubAdapterWithOperationMap(summaryMap)),
        new RegExp(`tracker adapter operation ${key} missing summary`),
        `expected a throw for ${key} summary = ${JSON.stringify(blank)}`,
      )
    }
  }
})

test('TRACKER_STATE_ROLES pins the roles the skills consume by name (BOS-524)', () => {
  assert.deepEqual(TRACKER_STATE_ROLES, ['planned', 'inProgress', 'inReview'])
})

test('assertConforms accepts an adapter with NO states capability (BOS-524)', () => {
  // The acceptance case for the MUST-populate rule: such an adapter conforms, and
  // trackerConfig.<tracker>.states becomes the only source for repos using it.
  const adapter = stubAdapterWithOperationMap(validOperationMap())
  assert.equal(adapter.states, undefined)
  assert.doesNotThrow(() => assertConforms(adapter))
})

test('assertConforms accepts a states FUNCTION and rejects a non-callable one (BOS-524)', () => {
  const withStates = (states) => ({ ...stubAdapterWithOperationMap(validOperationMap()), states })
  assert.doesNotThrow(() => assertConforms(withStates(() => ({ planned: 'Ready' }))))
  // A present-but-not-callable states passes the presence loop and would then blow up
  // at the call site as a raw TypeError, defeating the caller's fallback.
  for (const bad of [{ planned: 'Ready' }, 'Ready', 42, [], true]) {
    assert.throws(
      () => assertConforms(withStates(bad)),
      /tracker adapter optional capability states must be a function/,
      `expected a throw for states = ${JSON.stringify(bad)}`,
    )
  }
})

test('assertConforms throws when a capability is missing', () => {
  assert.throws(() => assertConforms({ tracker: 'stub' }), /missing capability: hasWork/)
})

// A stub adapter with every TRACKER_CAPABILITIES member present and a fully
// valid operationMap (every REQUIRED_TRACKER_OPERATIONS key with a non-empty
// tool + summary), so the operationMap-specific tests below isolate exactly
// the mutation under test.
function stubAdapterWithOperationMap(operationMap) {
  return {
    hasWork: () => {},
    hasUnblockedWork: () => {},
    readDependencies: () => {},
    isUnblocked: () => {},
    formatClaimComment: () => {},
    resolveClaim: () => {},
    normalizeTicket: () => {},
    operationMap,
  }
}

function validOperationMap() {
  const map = {}
  for (const key of REQUIRED_TRACKER_OPERATIONS) {
    map[key] = { tool: `mcp__stub__${key}`, summary: `summary for ${key}` }
  }
  return map
}

// The 9 required ops written out literally rather than derived from
// REQUIRED_TRACKER_OPERATIONS. Deriving it would make the omitting-adapter test
// self-fulfilling: restoring extractImages to the required list would silently add it
// back to the fixture too, and the case would keep passing against exactly the state it
// exists to reject. Literal keys make that restoration surface as the contract's own
// "operationMap missing operation" throw.
function nineRequiredOperationMap() {
  const map = {}
  for (const key of [
    'selectPlanned',
    'getIssue',
    'moveState',
    'readComments',
    'writeComment',
    'updateComment',
    'readLabels',
    'setPriorityEstimate',
    'appendDependency',
  ]) {
    map[key] = { tool: `mcp__stub__${key}`, summary: `summary for ${key}` }
  }
  return map
}

test('assertConforms throws the contract error (not a raw TypeError) for a non-object operationMap', () => {
  // `null` is not `undefined`, so it passes the TRACKER_CAPABILITIES presence
  // loop and would otherwise reach `adapter.operationMap[key]` and surface as
  // "Cannot read properties of null" instead of a contract message.
  for (const bad of [null, 'nope', 42, []]) {
    assert.throws(
      () => assertConforms(stubAdapterWithOperationMap(bad)),
      /tracker adapter operationMap must be an object/,
      `expected the contract error for operationMap = ${JSON.stringify(bad)}`,
    )
  }
})

test('assertConforms throws when operationMap is missing a required op', () => {
  const map = validOperationMap()
  delete map.updateComment
  const adapter = stubAdapterWithOperationMap(map)
  assert.throws(
    () => assertConforms(adapter),
    /tracker adapter operationMap missing operation: updateComment/,
  )
})

test('assertConforms throws when a required op has a blank/missing tool', () => {
  const map = validOperationMap()
  map.updateComment = { tool: '', summary: 'valid summary' }
  const adapter = stubAdapterWithOperationMap(map)
  assert.throws(
    () => assertConforms(adapter),
    /tracker adapter operation updateComment missing tool/,
  )
})

test('assertConforms throws when a required op has a blank/missing summary', () => {
  const map = validOperationMap()
  map.updateComment = { tool: 'mcp__stub__updateComment', summary: '' }
  const adapter = stubAdapterWithOperationMap(map)
  assert.throws(
    () => assertConforms(adapter),
    /tracker adapter operation updateComment missing summary/,
  )
})

test('assertConforms throws for a whitespace-only tool or summary, not just an empty one', () => {
  // The contract documents these as non-empty; `" "` or `"\n"` names no MCP tool
  // and describes nothing, so it must not be able to claim conformance.
  for (const blank of [' ', '\n', '\t  ']) {
    const toolMap = validOperationMap()
    toolMap.updateComment = { tool: blank, summary: 'valid summary' }
    assert.throws(
      () => assertConforms(stubAdapterWithOperationMap(toolMap)),
      /tracker adapter operation updateComment missing tool/,
      `expected a throw for tool = ${JSON.stringify(blank)}`,
    )

    const summaryMap = validOperationMap()
    summaryMap.updateComment = { tool: 'mcp__stub__updateComment', summary: blank }
    assert.throws(
      () => assertConforms(stubAdapterWithOperationMap(summaryMap)),
      /tracker adapter operation updateComment missing summary/,
      `expected a throw for summary = ${JSON.stringify(blank)}`,
    )
  }
})

test('assertConforms throws when a required op has no tool key at all', () => {
  const map = validOperationMap()
  map.updateComment = { summary: 'valid summary' }
  const adapter = stubAdapterWithOperationMap(map)
  assert.throws(
    () => assertConforms(adapter),
    /tracker adapter operation updateComment missing tool/,
  )
})

test('assertConforms throws when a required op has no summary key at all', () => {
  const map = validOperationMap()
  map.updateComment = { tool: 'mcp__stub__updateComment' }
  const adapter = stubAdapterWithOperationMap(map)
  assert.throws(
    () => assertConforms(adapter),
    /tracker adapter operation updateComment missing summary/,
  )
})

// --- the registry-parameterized resolver ------------------------------------

const stubBuilders = {
  linear: () => ({ tracker: 'linear' }),
  jira: ({ env, fetchImpl }) => ({ tracker: 'jira', env, fetchImpl }),
}

test('resolveTrackerAdapter defaults to the linear builder and passes env + fetchImpl through', () => {
  const fetchImpl = () => {}
  const adapter = resolveTrackerAdapter({
    builders: stubBuilders,
    env: { TRACKER: 'jira' },
    fetchImpl,
  })
  assert.equal(adapter.tracker, 'jira')
  assert.deepEqual(adapter.env, { TRACKER: 'jira' })
  assert.equal(adapter.fetchImpl, fetchImpl)

  assert.equal(resolveTrackerAdapter({ builders: stubBuilders, env: {} }).tracker, 'linear')
})

test('resolveTrackerAdapter is SYNCHRONOUS — no call site awaits it', () => {
  // Lazily `await import()`-ing the builder instead of splitting the module would
  // return a Promise here, and every existing caller (cron-gates/boss-build.mjs,
  // scripts/cron-gates/boss-plan.mjs, tracker/cli.mjs) would silently receive one.
  const adapter = resolveTrackerAdapter({ builders: stubBuilders, env: {} })
  assert.ok(!(adapter instanceof Promise), 'resolveTrackerAdapter must not return a Promise')
})

test('resolveTrackerAdapter throws on an unknown tracker', () => {
  assert.throws(
    () => resolveTrackerAdapter({ builders: stubBuilders, env: { TRACKER: 'asana' } }),
    /unknown tracker: asana/,
  )
})

test('an explicitly EMPTY TRACKER fails fast instead of coercing to the default', () => {
  // `||` would silently treat TRACKER='' as "linear", hiding a misconfigured host
  // (an unset-vs-blank env var is exactly the case a deploy script gets wrong).
  assert.throws(
    () => resolveTrackerAdapter({ builders: stubBuilders, env: { TRACKER: '' } }),
    /unknown tracker: $/,
  )
})

test('an inherited Object.prototype member is NOT a tracker', () => {
  // `builders[name]` truthiness would resolve TRACKER=constructor to `Object`, then
  // call `Object({env, fetchImpl})` and hand back a plain object that impersonates an
  // adapter all the way to the first capability call. Object.hasOwn is the guard.
  for (const inherited of ['constructor', 'toString', 'hasOwnProperty', 'valueOf', '__proto__']) {
    assert.throws(
      () => resolveTrackerAdapter({ builders: stubBuilders, env: { TRACKER: inherited } }),
      new RegExp(`unknown tracker: ${inherited.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}`),
      `expected a throw for TRACKER=${inherited}`,
    )
  }
})

test('resolveTrackerAdapter throws rather than crashing when no builders are supplied', () => {
  assert.throws(() => resolveTrackerAdapter({ env: {} }), /unknown tracker: linear/)
  assert.throws(() => resolveTrackerAdapter(), /unknown tracker/)
})
