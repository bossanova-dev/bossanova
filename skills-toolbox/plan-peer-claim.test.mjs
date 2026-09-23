import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, readFileSync, utimesSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawnSync } from 'node:child_process'
import {
  PEER_CLAIM_LIVENESS_ENV,
  PEER_CLAIM_LIVENESS_MS,
  peerClaimLivenessMs,
  peerClaimVerdict,
  scanPeerClaims,
} from './plan-peer-claim.mjs'
import { PLAN_SCRATCH_ROOT, RUN_SCRATCH_DIR_PREFIX } from './plan-scratch-paths.mjs'
import { STALE_PLAN_SCRATCH_TTL_MS } from './plan-scratch-reap.mjs'

const NOW = 1_700_000_000_000
const WINDOW = 10 * 60 * 1000

const peerDir = (name) => `${PLAN_SCRATCH_ROOT}/${RUN_SCRATCH_DIR_PREFIX}${name}`

const verdict = (overrides = {}) =>
  peerClaimVerdict({ issueId: 'BOS-1283', now: NOW, livenessMs: WINDOW, ...overrides })

test('a live peer run-scratch directory holding the id defers', () => {
  const result = verdict({
    selfRunDir: peerDir('mine'),
    entries: [
      {
        dir: peerDir('theirs'),
        names: ['BOS-1283.precheck.json', 'BOS-1283-some-slug.md'],
        mtimeMs: NOW - 60_000,
      },
    ],
  })

  assert.equal(result.action, 'defer')
  assert.deepEqual(result.peers, [
    { dir: peerDir('theirs'), entry: 'BOS-1283-some-slug.md', ageMs: 60_000 },
  ])
  assert.match(result.reasons.join('\n'), /peer claim: .*holds BOS-1283-some-slug\.md/)
})

test('the identical directory aged past the liveness window proceeds', () => {
  // Presence is not a claim: plan-scratch-reap.mjs keeps a finished run's scratch for
  // 24h, so without this the detector would defer against every completed peer for a day.
  const entries = [
    {
      dir: peerDir('theirs'),
      names: ['BOS-1283-some-slug.md'],
      mtimeMs: NOW - WINDOW - 1,
    },
  ]

  const result = verdict({ selfRunDir: peerDir('mine'), entries })

  assert.equal(result.action, 'proceed')
  assert.deepEqual(result.peers, [])
  assert.match(result.reasons.join('\n'), /past the 600000ms liveness window/)
})

test('the default liveness window sits well inside the scratch reap TTL', () => {
  // If the window ever reached the reap TTL, presence would be the signal again.
  assert.ok(PEER_CLAIM_LIVENESS_MS > 0)
  assert.ok(PEER_CLAIM_LIVENESS_MS < STALE_PLAN_SCRATCH_TTL_MS / 4)
})

test("the caller's own directory never claims against itself, however fresh", () => {
  // A detector that can defer to itself deadlocks the queue permanently — strictly
  // worse than having no detector at all — so identity is checked before recency.
  const result = verdict({
    selfRunDir: peerDir('mine'),
    entries: [{ dir: peerDir('mine'), names: ['BOS-1283-some-slug.md'], mtimeMs: NOW }],
  })

  assert.equal(result.action, 'proceed')
  assert.deepEqual(result.peers, [])
  assert.match(result.reasons.join('\n'), /own scratch directory/)
})

test('self-exclusion matches a bare run-dir name against a full path, and vice versa', () => {
  const bare = `${RUN_SCRATCH_DIR_PREFIX}mine`
  const result = verdict({
    selfRunDir: bare,
    entries: [{ dir: peerDir('mine'), names: ['BOS-1283-some-slug.md'], mtimeMs: NOW }],
  })

  assert.equal(result.action, 'proceed')
})

test('an id carried one level down inside a run directory defers', () => {
  // The ids live inside `run-<RUN-ID>/`, not at the scratch root, which is exactly
  // what the recorded top-level recipe missed.
  const result = verdict({
    selfRunDir: peerDir('mine'),
    entries: [{ dir: peerDir('theirs'), names: ['BOS-1283.description.md'], mtimeMs: NOW }],
  })

  assert.equal(result.action, 'defer')
  assert.equal(result.peers[0].entry, 'BOS-1283.description.md')
})

test('a directory that is not run-scoped carries no run identity and is ignored', () => {
  const result = verdict({
    selfRunDir: peerDir('mine'),
    entries: [
      { dir: `${PLAN_SCRATCH_ROOT}/children`, names: ['BOS-1283-some-slug.md'], mtimeMs: NOW },
    ],
  })

  assert.equal(result.action, 'proceed')
  assert.match(result.reasons.join('\n'), /not a .*run scratch directory/)
})

test('an id that is a strict substring of a different ticket id does not defer', () => {
  const result = verdict({
    issueId: 'BOS-128',
    selfRunDir: peerDir('mine'),
    entries: [
      {
        dir: peerDir('theirs'),
        names: ['BOS-1283-some-slug.md', 'BOS-1283.precheck.json'],
        mtimeMs: NOW,
      },
    ],
  })

  assert.equal(result.action, 'proceed')
  assert.deepEqual(result.peers, [])
})

test('the same id as a whole token still defers around its separators', () => {
  for (const name of ['BOS-128-a-slug.md', 'BOS-128.precheck.json', 'BOS-128']) {
    const result = verdict({
      issueId: 'BOS-128',
      selfRunDir: peerDir('mine'),
      entries: [{ dir: peerDir('theirs'), names: [name], mtimeMs: NOW }],
    })
    assert.equal(result.action, 'defer', `${name} should read as a claim`)
  }
})

test('an unreadable or absent listing proceeds with a recorded reason', () => {
  const result = verdict({ selfRunDir: peerDir('mine'), entries: undefined })

  assert.equal(result.action, 'proceed')
  assert.match(result.reasons.join('\n'), /failing open/)
})

test('a missing issue id proceeds rather than matching everything', () => {
  const result = verdict({
    issueId: '',
    entries: [{ dir: peerDir('theirs'), names: ['BOS-1283-some-slug.md'], mtimeMs: NOW }],
  })

  assert.equal(result.action, 'proceed')
  assert.match(result.reasons.join('\n'), /no issue id/)
})

test('an entry with no readable mtime is ignored rather than treated as live', () => {
  const result = verdict({
    selfRunDir: peerDir('mine'),
    entries: [{ dir: peerDir('theirs'), names: ['BOS-1283-some-slug.md'], mtimeMs: undefined }],
  })

  assert.equal(result.action, 'proceed')
  assert.match(result.reasons.join('\n'), /no readable mtime/)
})

test('a future mtime reads as just-touched rather than as a negative age', () => {
  const result = verdict({
    selfRunDir: peerDir('mine'),
    entries: [{ dir: peerDir('theirs'), names: ['BOS-1283-some-slug.md'], mtimeMs: NOW + 5_000 }],
  })

  assert.equal(result.action, 'defer')
  assert.equal(result.peers[0].ageMs, 0)
})

test('the liveness window is overridable by env and by argument', () => {
  assert.equal(peerClaimLivenessMs(undefined, {}), PEER_CLAIM_LIVENESS_MS)
  assert.equal(peerClaimLivenessMs(1234, {}), 1234)
  assert.equal(peerClaimLivenessMs(undefined, { [PEER_CLAIM_LIVENESS_ENV]: '4321' }), 4321)
  // A typo must not silently widen or close the window.
  assert.equal(
    peerClaimLivenessMs(undefined, { [PEER_CLAIM_LIVENESS_ENV]: 'soon' }),
    PEER_CLAIM_LIVENESS_MS,
  )
  assert.equal(peerClaimLivenessMs(-1, {}), PEER_CLAIM_LIVENESS_MS)
})

test('the scratch shape is imported, never hardcoded', () => {
  // R4: a relocation of the scratch contract must move this detector with it rather
  // than silently blind it, which is how the recorded top-level recipe rotted.
  const source = readFileSync(new URL('./plan-peer-claim.mjs', import.meta.url), 'utf8')
  assert.match(
    source,
    /import\s*\{[^}]*PLAN_SCRATCH_ROOT[^}]*RUN_SCRATCH_DIR_PREFIX[^}]*\}\s*from\s*'\.\/plan-scratch-paths\.mjs'/,
  )
  assert.ok(
    !source.includes(PLAN_SCRATCH_ROOT),
    'the scratch root must never appear as a bare literal in the module',
  )
  assert.ok(
    !/['"`]run-['"`]/.test(source),
    'the run-dir prefix must never appear as a bare literal in the module',
  )
})

// ── IO level: the same rules against real directories ───────────────────────────

function scratchRoot() {
  const root = mkdtempSync(join(tmpdir(), 'plan-peer-claim-'))
  return {
    root,
    run(name, entryNames, ageMs = 0) {
      const dir = join(root, `${RUN_SCRATCH_DIR_PREFIX}${name}`)
      mkdirSync(dir)
      for (const entry of entryNames) writeFileSync(join(dir, entry), '{}')
      if (ageMs > 0) {
        const when = new Date(Date.now() - ageMs)
        for (const entry of entryNames) utimesSync(join(dir, entry), when, when)
        utimesSync(dir, when, when)
      }
      return dir
    },
  }
}

test('scanPeerClaims defers on a real live peer directory and excludes the caller', () => {
  const scratch = scratchRoot()
  const mine = scratch.run('mine', ['BOS-1283-some-slug.md'])
  const theirs = scratch.run('theirs', ['BOS-1283.precheck.json'])

  const deferred = scanPeerClaims({
    issueId: 'BOS-1283',
    selfRunDir: mine,
    root: scratch.root,
    livenessMs: WINDOW,
  })
  assert.equal(deferred.action, 'defer')
  assert.deepEqual(
    deferred.peers.map((p) => p.dir),
    [theirs],
  )

  // Swap identities: the same tree, read by the other run, defers the other way.
  const swapped = scanPeerClaims({
    issueId: 'BOS-1283',
    selfRunDir: theirs,
    root: scratch.root,
    livenessMs: WINDOW,
  })
  assert.equal(swapped.action, 'defer')
  assert.deepEqual(
    swapped.peers.map((p) => p.dir),
    [mine],
  )
})

test('scanPeerClaims proceeds when the only peer directory is aged out', () => {
  const scratch = scratchRoot()
  const mine = scratch.run('mine', ['BOS-1283.precheck.json'])
  scratch.run('stale', ['BOS-1283-some-slug.md'], WINDOW * 3)

  const result = scanPeerClaims({
    issueId: 'BOS-1283',
    selfRunDir: mine,
    root: scratch.root,
    livenessMs: WINDOW,
  })

  assert.equal(result.action, 'proceed')
  assert.deepEqual(result.peers, [])
})

test('scanPeerClaims matches at run-dir depth, not at the scratch root', () => {
  const scratch = scratchRoot()
  const mine = scratch.run('mine', ['BOS-9999.precheck.json'])
  // A root-level file naming the id belongs to no run, so it is not a claim …
  writeFileSync(join(scratch.root, 'BOS-1283-some-slug.md'), 'stray')
  const rootOnly = scanPeerClaims({
    issueId: 'BOS-1283',
    selfRunDir: mine,
    root: scratch.root,
    livenessMs: WINDOW,
  })
  assert.equal(rootOnly.action, 'proceed')

  // … while the same basename one level down inside a peer's run directory is.
  scratch.run('theirs', ['BOS-1283-some-slug.md'])
  const nested = scanPeerClaims({
    issueId: 'BOS-1283',
    selfRunDir: mine,
    root: scratch.root,
    livenessMs: WINDOW,
  })
  assert.equal(nested.action, 'defer')
})

test('scanPeerClaims treats an absent scratch root as proceed with a reason', () => {
  const missing = join(mkdtempSync(join(tmpdir(), 'plan-peer-claim-missing-')), 'nope')

  const result = scanPeerClaims({ issueId: 'BOS-1283', selfRunDir: 'run-mine', root: missing })

  assert.equal(result.action, 'proceed')
  assert.deepEqual(result.peers, [])
  assert.match(result.reasons.join('\n'), /is unreadable/)
})

// ── CLI level: the contract the consuming SKILL.md actually leans on ────────────
//
// bs-sweep-plan reads `.action` and is told explicitly NOT to read the exit code, so
// the exit code is load-bearing in the opposite direction: a non-zero exit for a real
// verdict would route every deferral into the sweep's BLOCKED path. These spawn the
// CLI rather than the exports because the flag parser, the exit code and the emitted
// JSON shape exist only in that layer — which is exactly where the two defects this
// suite was extended for (a NaN `--now`, a value-less `--self`) both lived.

const CLI = fileURLToPath(new URL('./plan-peer-claim.mjs', import.meta.url))

function runCli(args) {
  const result = spawnSync(process.execPath, [CLI, ...args], { encoding: 'utf8' })
  return { status: result.status, stdout: result.stdout, stderr: result.stderr }
}

test('the CLI exits 0 and prints one JSON line for BOTH proceed and defer', () => {
  const scratch = scratchRoot()

  const quiet = runCli(['scan', 'BOS-1283', '--root', scratch.root])
  assert.equal(quiet.status, 0, 'a proceed verdict is not a refusal')
  assert.equal(JSON.parse(quiet.stdout).action, 'proceed')
  assert.equal(quiet.stdout.trimEnd().split('\n').length, 1, 'exactly one JSON line')

  scratch.run('theirs', ['BOS-1283-some-slug.md'])
  const claimed = runCli(['scan', 'BOS-1283', '--root', scratch.root])
  assert.equal(claimed.status, 0, 'a defer verdict is a verdict, not an error')
  const parsed = JSON.parse(claimed.stdout)
  assert.equal(parsed.action, 'defer')
  assert.match(parsed.peers[0].dir, /run-theirs$/)
  assert.ok(Number.isFinite(parsed.peers[0].ageMs), 'peers[].ageMs is the documented number')
})

test('a non-numeric --now falls back to the real clock instead of bypassing recency', () => {
  const scratch = scratchRoot()
  // Stale well past the default window: the ONLY answer that fails open is `proceed`.
  scratch.run('theirs', ['BOS-1283-some-slug.md'], STALE_PLAN_SCRATCH_TTL_MS)

  // A NaN clock made `ageMs` NaN and `NaN >= windowMs` false, so recency was skipped
  // and a day-old directory read as a live claim — the fail-CLOSED direction.
  const result = runCli(['scan', 'BOS-1283', '--root', scratch.root, '--now', 'abc'])
  assert.equal(result.status, 0, 'a present-but-unparseable clock is not a usage error')
  const parsed = JSON.parse(result.stdout)
  assert.equal(parsed.action, 'proceed', '--now abc must not turn stale scratch into a claim')
  assert.deepEqual(parsed.peers, [])

  // An EMPTY value is a different case and takes the usage path below, not this one.
  assert.equal(runCli(['scan', 'BOS-1283', '--root', scratch.root, '--now', '']).status, 2)
})

test('peerClaimVerdict itself coerces a non-finite clock rather than deferring on it', () => {
  // Defence in depth: the exported pure function is the invariant holder, so a caller
  // that is not the CLI cannot reach the recency bypass either.
  for (const now of [Number.NaN, undefined, 'nonsense']) {
    const result = peerClaimVerdict({
      issueId: 'BOS-1283',
      selfRunDir: peerDir('mine'),
      livenessMs: WINDOW,
      now,
      entries: [
        {
          dir: peerDir('theirs'),
          names: ['BOS-1283-some-slug.md'],
          mtimeMs: Date.now() - STALE_PLAN_SCRATCH_TTL_MS,
        },
      ],
    })
    assert.equal(result.action, 'proceed', `now=${String(now)} must read as the real clock`)
  }
})

test('a value-less flag is a usage error, never a silently disabled self-exclusion', () => {
  const scratch = scratchRoot()
  const mine = scratch.run('mine', ['BOS-1283-some-slug.md'])

  // Dropping --self's value would skip the identity exclusion and let the caller defer
  // to ITSELF — a permanent queue deadlock, which the module calls strictly worse than
  // having no detector at all.
  const dangling = runCli(['scan', 'BOS-1283', '--root', scratch.root, '--self'])
  assert.equal(dangling.status, 2, 'a missing flag value is a usage error')
  assert.match(dangling.stderr, /--self requires a value/)
  assert.equal(dangling.stdout, '', 'no verdict is printed for a usage error')

  const flagShaped = runCli(['scan', 'BOS-1283', '--root', scratch.root, '--self', '--liveness'])
  assert.equal(flagShaped.status, 2, 'the next flag is not a value')

  // With a real value the same invocation excludes the caller and proceeds.
  const named = runCli(['scan', 'BOS-1283', '--root', scratch.root, '--self', mine])
  assert.equal(named.status, 0)
  assert.equal(JSON.parse(named.stdout).action, 'proceed')
})

test('an unknown verb is a usage error and prints no verdict', () => {
  const result = runCli(['wat'])
  assert.equal(result.status, 2)
  assert.equal(result.stdout, '')
  assert.match(result.stderr, /usage: plan-peer-claim\.mjs scan/)
})
