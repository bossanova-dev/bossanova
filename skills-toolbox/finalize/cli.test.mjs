// skills-toolbox/finalize/cli.test.mjs
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { runCli } from './cli.mjs'
import { finalizeOperationMap, createDefaultRun } from './boss-finalize.mjs'

test('inject-pr-tag routes to the adapter injectPrTag with PR number + BASE_BRANCH', () => {
  const calls = []
  const resolve = () => ({ injectPrTag: (pr, opts) => calls.push({ pr, opts }) })
  const code = runCli(['inject-pr-tag', '1234'], { env: { BASE_BRANCH: 'main' }, resolve })
  assert.equal(code, 0)
  assert.equal(calls.length, 1)
  assert.equal(calls[0].pr, '1234')
  assert.equal(calls[0].opts.baseBranch, 'main')
})

test('inject-pr-tag without a PR number exits 2', () => {
  let err = ''
  const code = runCli(['inject-pr-tag'], { errWrite: (s) => (err += s), resolve: () => ({}) })
  assert.equal(code, 2)
  assert.match(err, /<pr-number> is required/)
})

test('inject-pr-tag returns the adapter failure status and names the failure', () => {
  let err = ''
  const thrown = new Error('helper rejected the rewrite')
  thrown.status = 2
  const code = runCli(['inject-pr-tag', '1234'], {
    errWrite: (s) => (err += s),
    resolve: () => ({
      injectPrTag: () => {
        throw thrown
      },
    }),
  })
  assert.equal(code, 2)
  assert.equal(err, 'inject-pr-tag: helper rejected the rewrite\n')
})

test('inject-pr-tag flattens thrown zero or missing statuses to failure', () => {
  for (const status of [undefined, 0]) {
    let err = ''
    const thrown = new Error(`bad status ${status}`)
    thrown.status = status
    const code = runCli(['inject-pr-tag', '1234'], {
      errWrite: (s) => (err += s),
      resolve: () => ({
        injectPrTag: () => {
          throw thrown
        },
      }),
    })
    assert.equal(code, 1)
    assert.equal(err, `inject-pr-tag: bad status ${status}\n`)
  }
})

test('an unknown finalize capability exits 2', () => {
  let err = ''
  const code = runCli(['bogus'], { errWrite: (s) => (err += s), resolve: () => ({}) })
  assert.equal(code, 2)
  assert.match(err, /unknown finalize capability: bogus/)
})

test('inject-pr-tag defaults resolve to the real finalize adapter factory', () => {
  // With no injected resolve, a fake runImpl proves the real factory is wired without
  // shelling out. resolveFinalizeAdapter(env, {runImpl}) is used by the default path.
  const calls = []
  const code = runCli(['inject-pr-tag', '9'], {
    env: { BASE_BRANCH: 'dev' },
    runImpl: (cmd, args, opts) => calls.push({ cmd, args, opts }),
  })
  assert.equal(code, 0)
  assert.equal(calls.length, 1)
  assert.match(calls[0].cmd, /add-pr-numbers\.sh$/)
  assert.deepEqual(calls[0].args, ['9'])
  assert.equal(calls[0].opts.env.BASE_BRANCH, 'dev')
})

// --- help surface -------------------------------------------------------------

// The capability names are derived from the frozen operation map, never from a literal
// list here: a capability added to the map without reaching help fails this test.
for (const flag of ['--help', '-h', 'help']) {
  test(`${flag} exits 0 and names every finalizeOperationMap key`, () => {
    let out = ''
    let err = ''
    const code = runCli([flag], { write: (s) => (out += s), errWrite: (s) => (err += s) })
    assert.equal(code, 0)
    assert.equal(err, '')
    const keys = Object.keys(finalizeOperationMap)
    assert.ok(keys.length > 0, 'the operation map must not be empty')
    for (const key of keys) {
      assert.ok(out.includes(key), `help output is missing capability ${key}`)
      assert.ok(
        out.includes(finalizeOperationMap[key].summary),
        `help output is missing the summary for ${key}`,
      )
    }
  })
}

test('help names every capability the dispatch chain accepts', () => {
  // Derived from this module's OWN dispatch literals, so a new `cmd === '...'` branch
  // cannot be added without appearing in help.
  const source = readFileSync(new URL('./cli.mjs', import.meta.url), 'utf8')
  const dispatched = [...source.matchAll(/cmd === '([^']+)'/g)]
    .map((m) => m[1])
    .filter((c) => !c.startsWith('-') && c !== 'help')
  assert.ok(dispatched.includes('inject-pr-tag'), 'the dispatch scan found no commands')
  let out = ''
  runCli(['--help'], { write: (s) => (out += s), errWrite: () => {} })
  for (const cmd of dispatched) {
    assert.ok(out.includes(cmd), `help output is missing dispatched command ${cmd}`)
  }
})

test('help still exits 0 when the finalize adapter cannot be resolved', () => {
  let out = ''
  let err = ''
  const code = runCli(['--help'], {
    write: (s) => (out += s),
    errWrite: (s) => (err += s),
    resolve: () => {
      throw new Error('unknown finalize: nope')
    },
  })
  assert.equal(code, 0)
  assert.match(err, /could not resolve the finalize adapter: unknown finalize: nope/)
  assert.match(out, /usage: node finalize\/cli\.mjs/)
})

test('an unknown capability rejection carries the capability list', () => {
  let err = ''
  const code = runCli(['bogus'], { errWrite: (s) => (err += s), write: () => {} })
  assert.equal(code, 2)
  assert.match(err, /unknown finalize capability: bogus/)
  for (const key of Object.keys(finalizeOperationMap)) {
    assert.ok(err.includes(key), `rejection is missing capability ${key}`)
  }
})

// --- the helper's own rejection reason reaches the caller ------------------------

test('a failing helper surfaces its own reason through cli.mjs, not only "Command failed"', () => {
  // End to end through the REAL default runner: a fake helper writes a known token to
  // stderr and exits 3. Under the previous `stdio: 'inherit'` runner the child's reason
  // reached the terminal but `err.stderr` was null, so cli.mjs could print only the
  // wrapper's generic `Command failed: <path> <arg>`.
  let err = ''
  let forwarded = ''
  const run = createDefaultRun({ errWrite: (s) => (forwarded += s) })
  const code = runCli(['inject-pr-tag', '1234'], {
    errWrite: (s) => (err += s),
    write: () => {},
    resolve: () => ({
      injectPrTag: () =>
        run(
          process.execPath,
          [
            '-e',
            "process.stderr.write('HELPER-REASON-TOKEN: base branch is not an ancestor\\n'); process.exit(3)",
          ],
          {},
        ),
    }),
  })
  assert.equal(code, 3, 'the child exit status is preserved')
  assert.match(err, /^inject-pr-tag: Command failed: /)
  assert.match(err, /HELPER-REASON-TOKEN: base branch is not an ancestor/)
  // ...and the live forwarded stream still carries it too.
  assert.match(forwarded, /HELPER-REASON-TOKEN: base branch is not an ancestor/)
})
