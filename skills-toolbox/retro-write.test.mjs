import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { lock, snapshot, retire, runCli } from './retro-write.mjs'

function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'retro-write-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  return dir
}
const plan = (del = [], retag = []) => ({
  delete: del,
  retag,
  counts: { delete: del.length, retag: retag.length, drain: del.length + retag.length },
})
const note = (id) => ({ id, body: 'evidence', created_at: '2026-10-01', tags: ['improvement'] })
function execution(t, ids = ['a', 'b']) {
  const dir = fixture(t)
  const idsFile = path.join(dir, 'ids')
  fs.writeFileSync(idsFile, ids.join('\n') + '\n')
  const progress = path.join(dir, 'progress')
  const calls = []
  const opts = {
    progress,
    run: (binary, args) => {
      calls.push([binary, args])
      return { status: 0 }
    },
    env: { BOSS_BIN: process.execPath, PATH: '' },
  }
  return { dir, idsFile, progress, calls, opts }
}
test('lock acquires, rejects peer, touches and releases only for owner', (t) => {
  const dir = fixture(t)
  assert.deepEqual(lock('acquire', { dir, token: 'one', now: () => 100_000 }), {
    verdict: 'acquired',
    token: 'one',
  })
  assert.equal(lock('acquire', { dir, token: 'two', now: () => 100_000 }).verdict, 'held')
  assert.equal(lock('touch', { dir, token: 'two' }).verdict, 'not-owner')
  assert.equal(lock('release', { dir, token: 'two' }).verdict, 'not-owner')
  assert.equal(lock('touch', { dir, token: 'one', now: () => 200_000 }).verdict, 'touched')
  assert.equal(
    fs.readFileSync(path.join(dir, 'boss-retro.lock', 'heartbeat'), 'utf8').trim(),
    '200',
  )
  assert.equal(lock('release', { dir, token: 'one' }).verdict, 'released')
  assert.equal(fs.existsSync(path.join(dir, 'boss-retro.lock')), false)
})
test('lock reclaims at configured stale boundary', (t) => {
  const dir = fixture(t)
  lock('acquire', { dir, token: 'one', now: () => 100_000 })
  assert.equal(
    lock('acquire', {
      dir,
      token: 'two',
      now: () => 109_000,
      env: { BOSS_RETRO_LOCK_STALE_SECS: '10' },
    }).verdict,
    'held',
  )
  assert.equal(
    lock('acquire', {
      dir,
      token: 'two',
      now: () => 109_000,
      env: { BOSS_RETRO_LOCK_STALE_SECS: '10' },
    }).verdict,
    'held',
  )
  assert.equal(
    lock('acquire', {
      dir,
      token: 'two',
      now: () => 110_000,
      env: { BOSS_RETRO_LOCK_STALE_SECS: '10' },
    }).verdict,
    'acquired',
  )
  assert.equal(lock('release', { dir, token: 'one' }).verdict, 'not-owner')
})
test('lock with unreadable heartbeat is held rather than reclaimed', (t) => {
  const dir = fixture(t)
  fs.mkdirSync(path.join(dir, 'boss-retro.lock'))
  assert.equal(lock('acquire', { dir }).verdict, 'held')
})
test('snapshot validates all notes before writing unique IDs', (t) => {
  const dir = fixture(t),
    out = path.join(dir, 'ids')
  assert.deepEqual(snapshot([note('a'), note('b')], out), { count: 2 })
  assert.equal(fs.readFileSync(out, 'utf8'), 'a\nb\n')
  for (const notes of [
    null,
    [{}],
    [note('a'), note('a')],
    [{ ...note('a'), tags: [] }],
    [{ ...note('a'), body: null }],
    [{ ...note('a'), created_at: '' }],
  ]) {
    fs.writeFileSync(out, 'unchanged')
    assert.throws(() => snapshot(notes, out))
    assert.equal(fs.readFileSync(out, 'utf8'), 'unchanged')
  }
})
test('retire refuses entire malformed plan or IDs outside snapshot', (t) => {
  const e = execution(t)
  for (const p of [
    null,
    {},
    plan(['a', 'later']),
    { ...plan(), delete: [null] },
    { ...plan(), counts: null },
  ]) {
    assert.throws(() => retire(p, e.idsFile, e.opts))
    assert.equal(e.calls.length, 0)
    assert.equal(fs.existsSync(e.progress), false)
  }
})
test('a note recorded after snapshot is never deleted or retagged', (t) => {
  const e = execution(t, ['before'])
  for (const p of [plan(['before', 'after']), plan(['before'], ['after'])]) {
    assert.throws(() => retire(p, e.idsFile, e.opts), /snapshot/)
    assert.deepEqual(e.calls, [])
  }
})
test('delete wins over retag and progress skips successful operations on resume', (t) => {
  const e = execution(t)
  const p = plan(['a'], ['a', 'b'])
  assert.deepEqual(retire(p, e.idsFile, e.opts), { done: 2, failed: null, remaining: 0 })
  assert.deepEqual(
    e.calls.map(([, args]) => args),
    [
      ['notes', 'rm', 'a'],
      ['notes', 'edit', 'b', '--tag', 'stale'],
    ],
  )
  assert.equal(e.calls[0][0], process.execPath)
  assert.equal(fs.readFileSync(e.progress, 'utf8'), 'delete a\nretag b\n')
  retire(p, e.idsFile, e.opts)
  assert.equal(e.calls.length, 2)
})
test('retire stops on first failure, records only successes, then resumes', (t) => {
  const e = execution(t)
  e.opts.run = (binary, args) => {
    e.calls.push([binary, args])
    return { status: args.includes('b') ? 1 : 0 }
  }
  assert.deepEqual(retire(plan(['a', 'b']), e.idsFile, e.opts), {
    done: 1,
    failed: { op: 'delete', id: 'b' },
    remaining: 1,
  })
  assert.equal(fs.readFileSync(e.progress, 'utf8'), 'delete a\n')
  e.opts.run = (binary, args) => {
    e.calls.push([binary, args])
    return { status: 0 }
  }
  assert.deepEqual(retire(plan(['a', 'b']), e.idsFile, e.opts), {
    done: 1,
    failed: null,
    remaining: 0,
  })
})
test('dry run validates snapshot but runs nothing and writes no progress', (t) => {
  const e = execution(t)
  assert.deepEqual(retire(plan(['a'], ['b']), e.idsFile, { ...e.opts, dryRun: true }), {
    delete: 1,
    retag: 1,
    drain: 2,
  })
  assert.deepEqual(e.calls, [])
  assert.equal(fs.existsSync(e.progress), false)
})
test('CLI returns held exit 3 and failed retire exit 1', (t) => {
  const e = execution(t)
  runCli(['lock', 'acquire', '--dir', e.dir, '--token', 'one'])
  assert.equal(runCli(['lock', 'acquire', '--dir', e.dir, '--token', 'two']).exitCode, 3)
  const p = path.join(e.dir, 'plan.json')
  fs.writeFileSync(p, JSON.stringify(plan(['a'])))
  assert.equal(
    runCli(['retire', p, e.idsFile, '--progress', e.progress], {
      ...e.opts,
      run: () => ({ status: 1 }),
    }).exitCode,
    1,
  )
})

test('an abandoned stale reclaim guard does not permanently block lock recovery', (t) => {
  const dir = fixture(t)
  lock('acquire', { dir, token: 'old', now: () => 100_000 })
  const guard = path.join(dir, 'boss-retro.lock.reclaim')
  fs.mkdirSync(guard)
  fs.utimesSync(guard, 100, 100)
  assert.equal(lock('acquire', { dir, token: 'new', now: () => 4_000_000 }).verdict, 'acquired')
  assert.equal(fs.existsSync(guard), false)
})

for (const action of ['release', 'touch']) {
  test(`${action} serializes owner validation with stale takeover`, (t) => {
    const dir = fixture(t)
    lock('acquire', { dir, token: 'old', now: () => 100_000 })
    let peer
    const io = {
      ...fs,
      readFileSync(file, ...args) {
        const result = fs.readFileSync(file, ...args)
        if (String(file).endsWith('/owner'))
          peer = lock('acquire', { dir, token: 'new', now: () => 4_000_000 })
        return result
      },
    }
    assert.equal(
      lock(action, { dir, token: 'old', fs: io, now: () => 4_000_000 }).verdict,
      action === 'release' ? 'released' : 'touched',
    )
    assert.equal(
      peer.verdict,
      'held',
      'takeover cannot occur between owner validation and mutation',
    )
    if (action === 'release')
      assert.equal(lock('acquire', { dir, token: 'new', now: () => 4_000_000 }).verdict, 'acquired')
    else {
      assert.equal(lock('acquire', { dir, token: 'new', now: () => 4_000_000 }).verdict, 'held')
      assert.equal(
        fs.readFileSync(path.join(dir, 'boss-retro.lock', 'owner'), 'utf8').trim(),
        'old',
      )
    }
    lock('acquire', { dir, token: 'new', now: () => 8_000_000 })
    assert.equal(lock(action, { dir, token: 'old', now: () => 8_000_000 }).verdict, 'not-owner')
    assert.equal(fs.readFileSync(path.join(dir, 'boss-retro.lock', 'owner'), 'utf8').trim(), 'new')
    assert.equal(
      fs.readFileSync(path.join(dir, 'boss-retro.lock', 'heartbeat'), 'utf8').trim(),
      '8000',
    )
  })
}

test('stale guard recovery cannot remove a concurrently published successor guard', (t) => {
  const dir = fixture(t)
  lock('acquire', { dir, token: 'old', now: () => 100_000 })
  const guard = path.join(dir, 'boss-retro.lock.reclaim')
  fs.mkdirSync(guard)
  fs.utimesSync(guard, 100, 100)
  let interleaved = false
  const io = {
    ...fs,
    rmdirSync(file, ...args) {
      if (file === guard && !interleaved) {
        interleaved = true
        fs.writeFileSync(path.join(guard, `${process.pid}.successor`), '')
      }
      return fs.rmdirSync(file, ...args)
    },
  }
  assert.equal(lock('acquire', { dir, token: 'new', now: () => 4_000_000, fs: io }).verdict, 'held')
  assert.equal(interleaved, true)
  assert.equal(fs.existsSync(path.join(guard, `${process.pid}.successor`)), true)
  assert.equal(fs.readFileSync(path.join(dir, 'boss-retro.lock', 'owner'), 'utf8').trim(), 'old')
})

test('a stale dead-process guard is recovered, while a live guard blocks every transition', (t) => {
  const dir = fixture(t)
  lock('acquire', { dir, token: 'old', now: () => 100_000 })
  const guard = path.join(dir, 'boss-retro.lock.reclaim')
  fs.mkdirSync(guard)
  fs.writeFileSync(path.join(guard, '2147483647.abandoned'), '')
  fs.utimesSync(guard, 100, 100)
  assert.equal(lock('acquire', { dir, token: 'new', now: () => 4_000_000 }).verdict, 'acquired')
  assert.equal(fs.existsSync(guard), false)
  fs.mkdirSync(guard)
  fs.writeFileSync(path.join(guard, `${process.pid}.active`), '')
  fs.utimesSync(guard, 100, 100)
  for (const action of ['acquire', 'touch', 'release']) {
    assert.equal(
      lock(action, { dir, token: 'new', now: () => 8_000_000 }).verdict,
      action === 'acquire' ? 'held' : 'not-owner',
    )
    assert.equal(fs.readFileSync(path.join(dir, 'boss-retro.lock', 'owner'), 'utf8').trim(), 'new')
    assert.equal(
      fs.readFileSync(path.join(dir, 'boss-retro.lock', 'heartbeat'), 'utf8').trim(),
      '4000',
    )
    assert.equal(fs.existsSync(path.join(guard, `${process.pid}.active`)), true)
  }
})

test('a fresh empty legacy guard is held until its stale threshold', (t) => {
  const dir = fixture(t)
  const guard = path.join(dir, 'boss-retro.lock.reclaim')
  fs.mkdirSync(guard)
  fs.utimesSync(guard, 100, 100)
  assert.equal(lock('acquire', { dir, now: () => 101_000 }).verdict, 'held')
  assert.equal(fs.existsSync(guard), true)
  assert.equal(lock('acquire', { dir, now: () => 4_000_000 }).verdict, 'acquired')
})
