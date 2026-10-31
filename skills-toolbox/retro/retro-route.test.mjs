import test from 'node:test'
import assert from 'node:assert/strict'
import {
  upstreamConfig,
  classifyRemedyOwner,
  route,
  forward,
  forwardDeleteIds,
} from './retro-route.mjs'
import { idempotencyKey } from '../bs-record-notes.mjs'
const probes = {
  trackedFiles: ['app/main.js', '.claude/skills/boss-build/SKILL.md'],
  installedCores: ['/skills/boss-build'],
  pathExists: (p) =>
    [
      '/skills/boss-build/toolbox/tracker/linear.mjs',
      '/skills/boss-build/toolbox/retro/retro-notes.mjs',
      '/skills/boss-build/SKILL.md',
    ].includes(p),
}
const cluster = (target, notes = [{ id: 'real', body: 'verbatim\nRun: run:a' }]) => ({
  key: target,
  ladder: { target },
  notes,
})
const selection = (c) => ({
  selected: [{ cluster: c, reason: 'selected' }],
  deferred: [],
  dropped: [],
  expired: [],
})
test('path evidence resolves installed suffixes, aliases, vendored and mixed ownership', () => {
  for (const target of [
    'tracker/linear.mjs',
    'skills-toolbox/retro/retro-notes.mjs',
    '.claude/skills/boss-build/SKILL.md',
  ])
    assert.equal(classifyRemedyOwner(cluster(target), probes).owner, 'core')
  for (const target of [
    'app/main.js',
    'nothing',
    'absent/thing.js',
    '/absolute/tracker/linear.mjs',
    '~/tracker/linear.mjs',
    'https://example.com/tracker/linear.mjs',
  ])
    assert.equal(classifyRemedyOwner(cluster(target), probes).owner, 'consumer')
  assert.equal(
    classifyRemedyOwner(cluster('old/main.js'), { ...probes, pathAliases: { 'old/': 'app/' } })
      .owner,
    'consumer',
  )
  assert.equal(
    classifyRemedyOwner(cluster('app/main.js tracker/linear.mjs'), probes).owner,
    'mixed',
  )
})
test('route consumes only selected slots, flags local core and mixed, self is unchanged', () => {
  const input = selection(cluster('tracker/linear.mjs'))
  input.deferred = [{ cluster: cluster('app/main.js'), reason: 'over-cap' }]
  const routed = route(input, { upstream: { notesRepo: 'up' }, probes })
  assert.equal(routed.selection.selected.length, 0)
  assert.deepEqual(routed.selection.deferred, input.deferred)
  assert.equal(routed.forward.length, 1)
  assert.equal(route(input, { probes }).selection.selected[0].cluster.remedyOwner.owner, 'core')
  assert.equal(
    route(selection(cluster('app/main.js tracker/linear.mjs')), {
      upstream: { notesRepo: 'up' },
      probes,
    }).flagged[0].owner,
    'mixed',
  )
  assert.deepEqual(route(input, { upstream: { self: true }, probes }).selection, input)
  assert.equal(input.selected[0].cluster.remedyOwner, undefined)
})
test('upstream settings normalize and reject invalid closed shapes', () => {
  assert.deepEqual(
    upstreamConfig(
      { retro: { upstream: { notesRepo: 'up' } } },
      { BOSS_RETRO_UPSTREAM_REPO: 'other' },
    ),
    { notesRepo: 'other' },
  )
  for (const upstream of [
    { self: true, notesRepo: 'up' },
    { notesRepo: ' ' },
    { self: false },
    { other: true },
  ])
    assert.throws(() => upstreamConfig({ retro: { upstream } }))
})
test('forward requires byte-exact read-back for the entire theme before deletion', () => {
  const notes = [
    { id: 'real', body: 'first' },
    { id: 'synthetic:one', body: 'second' },
  ]
  const plan = route(selection(cluster('tracker/linear.mjs', notes)), {
    upstream: { notesRepo: 'up' },
    probes,
  })
  const calls = [],
    stored = new Map()
  const boss = (args) => {
    calls.push(args)
    if (args[1] === 'add') {
      const body = args.at(-1),
        id = `note-${stored.size}`
      assert.equal(args[args.indexOf('--idempotency-key') + 1], idempotencyKey(body))
      stored.set(id, { id, repo_id: 'up', body, tags: ['improvement'] })
      return { status: 0, stdout: JSON.stringify({ id }) }
    }
    return { status: 0, stdout: JSON.stringify(stored.get(args[2])) }
  }
  const result = forward(plan.forward, { repo: 'up', boss })
  assert.deepEqual(result.provenKeys, ['tracker/linear.mjs'])
  assert.deepEqual(forwardDeleteIds(plan, result), ['real'])
  for (const broken of ['body', 'repo_id', 'tags', 'exit', 'json']) {
    const bad = (args) => {
      const response = boss(args)
      if (args[1] !== 'show') return response
      if (broken === 'exit') return { status: 1, stdout: '' }
      if (broken === 'json') return { status: 0, stdout: '{' }
      const note = JSON.parse(response.stdout)
      note[broken] = broken === 'tags' ? [] : 'wrong'
      return { status: 0, stdout: JSON.stringify(note) }
    }
    const failed = forward(plan.forward, { repo: 'up', boss: bad })
    assert.equal(failed.failed.length, 2)
    assert.deepEqual(forwardDeleteIds(plan, failed), [])
  }
  let count = 0
  const partial = forward(plan.forward, {
    repo: 'up',
    boss: (args) => (args[1] === 'show' && count++ === 1 ? { status: 1 } : boss(args)),
  })
  assert.deepEqual(partial.provenKeys, [])
  assert.deepEqual(forwardDeleteIds(plan, partial), [])
  assert.ok(calls.every((args) => args[1] !== 'add' || args[args.length - 2] === '--'))
})
