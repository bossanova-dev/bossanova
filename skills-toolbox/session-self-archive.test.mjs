import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import path from 'node:path'
import { describe, it } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  DEFAULT_DELAY_MS,
  EXIT_USAGE,
  FINISHED_OUTCOMES,
  UsageError,
  decideSelfArchive,
  isUnattended,
  parseArgs,
  runSelfArchive,
} from './session-self-archive.mjs'

const SELF = 'chat-self'
const WORKTREE = '/work/tree'
const BOSS = '/opt/boss/bin/boss'

const baseEnv = (over = {}) => ({
  BOSS_SESSION_ID: 'sess-1',
  BOSS_AGENT_SESSION_ID: SELF,
  BOSS_WORKTREE: WORKTREE,
  BOSS_UNATTENDED: 'true',
  BOSS_CRON: 'true',
  ...over,
})

// An input that reaches `archive` when nothing is overridden.
const eligible = (over = {}) => ({
  env: baseEnv(),
  outcome: 'planned',
  runScratchExists: false,
  session: { id: 'sess-1', archived_at: '', pr_number: null },
  chats: [{ agent_session_id: SELF, status: 'WORKING' }],
  worktreeClean: true,
  confirmed: false,
  suppressed: false,
  ...over,
})

const reasonOf = (input) => {
  const d = decideSelfArchive(input)
  return `${d.verdict}:${d.reason}`
}

describe('decideSelfArchive — one test per table row', () => {
  it('the eligible baseline archives', () => {
    assert.deepEqual(decideSelfArchive(eligible()), { verdict: 'archive', reason: '' })
  })

  it('BOSS_SESSION_ID unset ⇒ skip not-managed', () => {
    assert.equal(reasonOf(eligible({ env: baseEnv({ BOSS_SESSION_ID: '' }) })), 'skip:not-managed')
  })

  it('BOSS_SELF_ARCHIVE_SUPPRESSED=1 ⇒ skip caller-owns-session', () => {
    assert.equal(
      reasonOf(eligible({ env: baseEnv({ BOSS_SELF_ARCHIVE_SUPPRESSED: '1' }) })),
      'skip:caller-owns-session',
    )
  })

  it('--suppressed ⇒ skip caller-owns-session', () => {
    assert.equal(reasonOf(eligible({ suppressed: true })), 'skip:caller-owns-session')
  })

  it('BOSS_CRON_JOB_ID set ⇒ skip cron-finalize-owns-cleanup', () => {
    assert.equal(
      reasonOf(eligible({ env: baseEnv({ BOSS_CRON_JOB_ID: 'job-1' }) })),
      'skip:cron-finalize-owns-cleanup',
    )
  })

  it('every finished outcome is eligible; any other outcome skips as outcome-<value>', () => {
    for (const outcome of FINISHED_OUTCOMES)
      assert.equal(reasonOf(eligible({ outcome })), 'archive:')
    for (const outcome of ['blocked', 'failed', 'drift']) {
      assert.equal(reasonOf(eligible({ outcome })), `skip:outcome-${outcome}`)
    }
  })

  it('a retained run scratch ⇒ skip scratch-retained', () => {
    assert.equal(reasonOf(eligible({ runScratchExists: true })), 'skip:scratch-retained')
  })

  it('an unreadable show or chats read ⇒ skip session-unreadable', () => {
    assert.equal(reasonOf(eligible({ session: null })), 'skip:session-unreadable')
    assert.equal(reasonOf(eligible({ chats: null })), 'skip:session-unreadable')
  })

  it('archived_at non-empty ⇒ skip already-archived', () => {
    assert.equal(
      reasonOf(eligible({ session: { archived_at: '2026-10-05T16:00:00Z', pr_number: null } })),
      'skip:already-archived',
    )
  })

  it('pr_number non-null ⇒ skip session-has-pr', () => {
    assert.equal(
      reasonOf(eligible({ session: { archived_at: '', pr_number: 42 } })),
      'skip:session-has-pr',
    )
  })

  it('this chat missing from the session chats ⇒ skip self-chat-unknown', () => {
    assert.equal(
      reasonOf(eligible({ chats: [{ agent_session_id: 'someone-else', status: 'STOPPED' }] })),
      'skip:self-chat-unknown',
    )
    assert.equal(
      reasonOf(eligible({ env: baseEnv({ BOSS_AGENT_SESSION_ID: '' }) })),
      'skip:self-chat-unknown',
    )
  })

  it('another chat that is not STOPPED ⇒ skip other-live-chats', () => {
    for (const status of ['WORKING', 'IDLE', 'QUESTION', 'WAITING', 'LIMITED']) {
      const chats = [
        { agent_session_id: SELF, status: 'WORKING' },
        { agent_session_id: 'other', status },
      ]
      assert.equal(reasonOf(eligible({ chats })), 'skip:other-live-chats', status)
    }
  })

  it('an UNSPECIFIED or unknown other-chat status counts as live', () => {
    for (const status of ['UNSPECIFIED', 'SOMETHING_NEW', '', undefined]) {
      const chats = [
        { agent_session_id: SELF, status: 'WORKING' },
        { agent_session_id: 'other', status },
      ]
      assert.equal(reasonOf(eligible({ chats })), 'skip:other-live-chats', String(status))
    }
  })

  it('other chats that are all STOPPED do not block (enum-prefixed spelling too)', () => {
    const chats = [
      { agent_session_id: SELF, status: 'WORKING' },
      { agent_session_id: 'a', status: 'STOPPED' },
      { agent_session_id: 'b', status: 'CHAT_STATUS_STOPPED' },
    ]
    assert.equal(reasonOf(eligible({ chats })), 'archive:')
  })

  it('a dirty worktree ⇒ skip worktree-dirty; a git failure ⇒ skip worktree-unreadable', () => {
    assert.equal(reasonOf(eligible({ worktreeClean: false })), 'skip:worktree-dirty')
    assert.equal(reasonOf(eligible({ worktreeClean: null })), 'skip:worktree-unreadable')
  })

  it('an attended chat without --confirmed ⇒ ask attended-confirm', () => {
    const env = baseEnv({ BOSS_UNATTENDED: undefined, BOSS_CRON: undefined })
    assert.deepEqual(decideSelfArchive(eligible({ env })), {
      verdict: 'ask',
      reason: 'attended-confirm',
    })
  })

  it('ask becomes archive with --confirmed', () => {
    const env = baseEnv({ BOSS_UNATTENDED: undefined, BOSS_CRON: undefined })
    assert.equal(reasonOf(eligible({ env, confirmed: true })), 'archive:')
  })
})

describe('decideSelfArchive — rule order', () => {
  it('a cron job id beats an otherwise eligible session', () => {
    assert.equal(
      reasonOf(eligible({ env: baseEnv({ BOSS_CRON_JOB_ID: 'job-1' }) })),
      'skip:cron-finalize-owns-cleanup',
    )
  })

  it('suppression beats every rule after not-managed', () => {
    const everythingWrong = eligible({
      env: baseEnv({ BOSS_SELF_ARCHIVE_SUPPRESSED: '1', BOSS_CRON_JOB_ID: 'job-1' }),
      outcome: 'failed',
      runScratchExists: true,
      session: null,
      chats: null,
      worktreeClean: false,
    })
    assert.equal(reasonOf(everythingWrong), 'skip:caller-owns-session')
    assert.equal(
      reasonOf({ ...everythingWrong, env: { ...everythingWrong.env, BOSS_SESSION_ID: '' } }),
      'skip:not-managed',
    )
  })

  it('each later rule only fires once every earlier one passes', () => {
    // Walk the table bottom-up: each step makes one more earlier rule fail and expects it to win.
    const cases = [
      [
        {
          worktreeClean: false,
          env: baseEnv({ BOSS_CRON: undefined, BOSS_UNATTENDED: undefined }),
        },
        'skip:worktree-dirty',
      ],
      [
        {
          worktreeClean: false,
          chats: [{ agent_session_id: SELF }, { agent_session_id: 'o', status: 'IDLE' }],
        },
        'skip:other-live-chats',
      ],
      [{ chats: [{ agent_session_id: 'o', status: 'IDLE' }] }, 'skip:self-chat-unknown'],
      [{ chats: [], session: { pr_number: 7 } }, 'skip:session-has-pr'],
      [{ session: { pr_number: 7, archived_at: 'x' } }, 'skip:already-archived'],
      [{ session: null, runScratchExists: false }, 'skip:session-unreadable'],
      [{ session: null, runScratchExists: true }, 'skip:scratch-retained'],
      [{ session: null, runScratchExists: true, outcome: 'blocked' }, 'skip:outcome-blocked'],
    ]
    for (const [over, want] of cases)
      assert.equal(reasonOf(eligible(over)), want, JSON.stringify(over))
  })
})

describe('isUnattended', () => {
  it('accepts BOSS_UNATTENDED=true and the legacy BOSS_CRON=true spelling', () => {
    assert.equal(isUnattended({ BOSS_UNATTENDED: 'true' }), true)
    assert.equal(isUnattended({ BOSS_CRON: 'true' }), true)
    assert.equal(isUnattended({}), false)
    assert.equal(isUnattended({ BOSS_UNATTENDED: '1', BOSS_CRON: 'yes' }), false)
  })

  it('either spelling alone archives without --confirmed', () => {
    assert.equal(reasonOf(eligible({ env: baseEnv({ BOSS_CRON: undefined }) })), 'archive:')
    assert.equal(reasonOf(eligible({ env: baseEnv({ BOSS_UNATTENDED: undefined }) })), 'archive:')
  })
})

describe('parseArgs', () => {
  it('parses every flag and defaults the delay', () => {
    assert.deepEqual(parseArgs(['--outcome', 'planned', '--run-scratch', 'd']), {
      outcome: 'planned',
      runScratch: 'd',
      confirmed: false,
      suppressed: false,
      delayMs: DEFAULT_DELAY_MS,
    })
    const all = parseArgs([
      '--outcome',
      'noop',
      '--run-scratch',
      'd',
      '--confirmed',
      '--suppressed',
      '--delay-ms',
      '0',
    ])
    assert.equal(all.confirmed, true)
    assert.equal(all.suppressed, true)
    assert.equal(all.delayMs, 0)
  })

  it('rejects missing, unknown and malformed arguments as usage errors', () => {
    for (const argv of [
      [],
      ['--outcome', 'planned'],
      ['--run-scratch', 'd'],
      ['--outcome', 'planned', '--run-scratch', 'd', '--bogus'],
      ['--outcome', 'planned', '--run-scratch', 'd', '--delay-ms', '-1'],
      ['--outcome', '--run-scratch', 'd'],
    ]) {
      assert.throws(() => parseArgs(argv), UsageError, JSON.stringify(argv))
    }
  })
})

// ---------------------------------------------------------------------------------------------
// runSelfArchive — injected execFile / spawn / fs, never a real daemon, PATH or repo.
// ---------------------------------------------------------------------------------------------

function harness({
  env = baseEnv({ BOSS_BIN: BOSS }),
  scratchExists = false,
  show = { session: { id: 'sess-1', archived_at: '', pr_number: null } },
  chats = { chats: [{ agent_session_id: SELF, status: 'WORKING' }] },
  showFails = false,
  porcelain = '',
  gitFails = false,
  bossExecutable = true,
  spawnError = null,
} = {}) {
  const calls = { execFile: [], spawn: [], opened: [], closed: [] }
  const fs = {
    existsSync: (p) => scratchExists && p.endsWith('run-abc'),
    statSync: (p) => {
      const bin = env.BOSS_BIN || BOSS
      if ((p === BOSS || p === bin) && bossExecutable) return { isFile: () => true, mode: 0o755 }
      const error = new Error('ENOENT')
      error.code = 'ENOENT'
      throw error
    },
    accessSync: () => {},
    openSync: (p) => {
      calls.opened.push(p)
      return 99
    },
    closeSync: (fd) => calls.closed.push(fd),
  }
  const execFile = (file, args, _options, cb) => {
    calls.execFile.push([file, ...args])
    if (file === 'git') {
      if (gitFails) return cb(new Error('not a git repo'), '')
      return cb(null, porcelain)
    }
    if (showFails) return cb(new Error('exit 1'), '')
    if (args[0] === 'show') return cb(null, JSON.stringify(show))
    if (args[0] === 'chats') return cb(null, JSON.stringify(chats))
    return cb(new Error(`unexpected ${args[0]}`), '')
  }
  const spawn = (file, args, options) => {
    const child = {
      unrefCalled: false,
      unref() {
        this.unrefCalled = true
      },
      // Like a real ChildProcess, the outcome arrives asynchronously after spawn() returns.
      once(event, cb) {
        if (event === (spawnError ? 'error' : 'spawn')) queueMicrotask(() => cb(spawnError))
        return this
      },
    }
    calls.spawn.push({ file, args, options, child })
    return child
  }
  const deps = { env, fs, execFile, spawn, tmpdir: '/tmp/os-tmp', cwd: WORKTREE, now: 1234 }
  return { deps, calls }
}

const OPTS = {
  outcome: 'planned',
  runScratch: '.linear-plans/run-abc',
  confirmed: false,
  suppressed: false,
  delayMs: 3000,
}

describe('runSelfArchive', () => {
  it('archive spawns a detached, unref-ed /bin/sh outside the worktree with the resolved binary and session id', async () => {
    const { deps, calls } = harness()
    const result = await runSelfArchive(OPTS, deps)
    assert.equal(result.verdict, 'archive')
    assert.equal(result.sessionId, 'sess-1')
    assert.equal(result.log, '/tmp/os-tmp/boss-self-archive-sess-1-1234.log')
    assert.equal(calls.spawn.length, 1)
    const [{ file, args, options, child }] = calls.spawn
    assert.equal(file, '/bin/sh')
    assert.equal(options.detached, true)
    assert.equal(options.cwd, '/tmp/os-tmp')
    assert.ok(!path.resolve(options.cwd).startsWith(WORKTREE), 'cwd must be outside BOSS_WORKTREE')
    assert.ok(args.includes(BOSS), 'spawn must run the path resolveBossBinary returned')
    assert.ok(args.includes('sess-1'), 'spawn must name the session id')
    assert.equal(args[args.indexOf(BOSS) + 1], 'sess-1')
    assert.ok(args.includes('3'), 'delay is passed in seconds')
    assert.ok(child.unrefCalled)
    assert.deepEqual(options.stdio, ['ignore', 99, 99])
    assert.deepEqual(calls.opened, ['/tmp/os-tmp/boss-self-archive-sess-1-1234.log'])
    assert.deepEqual(calls.closed, [99])
  })

  it('reads the daemon through the resolved binary and git through execFile, never a bare boss', async () => {
    const { deps, calls } = harness()
    await runSelfArchive(OPTS, deps)
    assert.deepEqual(
      calls.execFile.map((c) => c.join(' ')).sort(),
      [
        `${BOSS} chats sess-1 --json`,
        `${BOSS} show sess-1 --json`,
        `git -C ${WORKTREE} status --porcelain`,
      ].sort(),
    )
  })

  it('a relative BOSS_BIN is anchored to the caller cwd before running from the temp dir', async () => {
    for (const relative of ['bin/boss', './bin/boss']) {
      const { deps, calls } = harness({ env: baseEnv({ BOSS_BIN: relative }) })
      const result = await runSelfArchive(OPTS, deps)
      const absolute = path.resolve(WORKTREE, relative)
      assert.equal(result.verdict, 'archive', relative)
      const bossCalls = calls.execFile.filter((c) => c[0] !== 'git').map((c) => c[0])
      assert.deepEqual(bossCalls, [absolute, absolute], relative)
      assert.equal(calls.spawn.length, 1, relative)
      assert.ok(calls.spawn[0].args.includes(absolute), `spawn must run ${absolute}`)
      assert.ok(!calls.spawn[0].args.includes(relative), 'spawn must not run the relative path')
    }
  })

  it('a spawn that fails asynchronously is a nonfatal error skip, with the log fd still closed', async () => {
    const spawnError = Object.assign(new Error('spawn /bin/sh EAGAIN'), { code: 'EAGAIN' })
    const { deps, calls } = harness({ spawnError })
    const result = await runSelfArchive(OPTS, deps)
    assert.equal(`${result.verdict}:${result.reason}`, 'skip:error')
    assert.match(result.detail, /EAGAIN/)
    assert.equal(result.sessionId, 'sess-1')
    assert.equal(result.log, undefined, 'a failed launch must not report a log')
    assert.equal(calls.spawn.length, 1)
    assert.equal(calls.spawn[0].child.unrefCalled, false)
    assert.deepEqual(calls.closed, [99])
  })

  it('no spawn for any skip or ask verdict', async () => {
    const cases = [
      [{ env: baseEnv({ BOSS_BIN: BOSS, BOSS_SESSION_ID: '' }) }, OPTS, 'skip:not-managed'],
      [{}, { ...OPTS, suppressed: true }, 'skip:caller-owns-session'],
      [
        { env: baseEnv({ BOSS_BIN: BOSS, BOSS_CRON_JOB_ID: 'j' }) },
        OPTS,
        'skip:cron-finalize-owns-cleanup',
      ],
      [{}, { ...OPTS, outcome: 'blocked' }, 'skip:outcome-blocked'],
      [{ scratchExists: true }, OPTS, 'skip:scratch-retained'],
      [{ showFails: true }, OPTS, 'skip:session-unreadable'],
      [{ bossExecutable: false }, OPTS, 'skip:session-unreadable'],
      [{ show: { session: { archived_at: 'x', pr_number: null } } }, OPTS, 'skip:already-archived'],
      [{ show: { session: { archived_at: '', pr_number: 3 } } }, OPTS, 'skip:session-has-pr'],
      [{ chats: { chats: [] } }, OPTS, 'skip:self-chat-unknown'],
      [
        {
          chats: {
            chats: [{ agent_session_id: SELF }, { agent_session_id: 'o', status: 'UNSPECIFIED' }],
          },
        },
        OPTS,
        'skip:other-live-chats',
      ],
      [{ porcelain: ' M file.go\n' }, OPTS, 'skip:worktree-dirty'],
      [{ gitFails: true }, OPTS, 'skip:worktree-unreadable'],
      [
        { env: baseEnv({ BOSS_BIN: BOSS, BOSS_CRON: undefined, BOSS_UNATTENDED: undefined }) },
        OPTS,
        'ask:attended-confirm',
      ],
    ]
    for (const [over, opts, want] of cases) {
      const { deps, calls } = harness(over)
      const result = await runSelfArchive(opts, deps)
      assert.equal(`${result.verdict}:${result.reason}`, want, JSON.stringify(over))
      assert.equal(calls.spawn.length, 0, `no spawn for ${want}`)
      assert.equal(calls.opened.length, 0, `no log opened for ${want}`)
    }
  })

  it('environment-only skips never read the daemon or git', async () => {
    const { deps, calls } = harness({ env: baseEnv({ BOSS_BIN: BOSS, BOSS_CRON_JOB_ID: 'j' }) })
    await runSelfArchive(OPTS, deps)
    assert.equal(calls.execFile.length, 0)
  })

  it('a non-JSON show reply is session-unreadable', async () => {
    const { deps, calls } = harness()
    const execFile = deps.execFile
    deps.execFile = (file, args, options, cb) =>
      args[0] === 'show'
        ? cb(null, 'boss skills: held refresh\nnot json')
        : execFile(file, args, options, cb)
    const result = await runSelfArchive(OPTS, deps)
    assert.equal(`${result.verdict}:${result.reason}`, 'skip:session-unreadable')
    assert.equal(calls.spawn.length, 0)
  })

  it('an attended ask becomes an archive spawn with --confirmed', async () => {
    const { deps, calls } = harness({
      env: baseEnv({ BOSS_BIN: BOSS, BOSS_CRON: undefined, BOSS_UNATTENDED: undefined }),
    })
    const result = await runSelfArchive({ ...OPTS, confirmed: true }, deps)
    assert.equal(result.verdict, 'archive')
    assert.equal(calls.spawn.length, 1)
  })
})

describe('CLI', () => {
  const script = path.join(path.dirname(fileURLToPath(import.meta.url)), 'session-self-archive.mjs')

  it('prints one JSON skip line and exits 0 outside a managed session', () => {
    const env = { PATH: process.env.PATH }
    const out = execFileSync(
      process.execPath,
      [script, '--outcome', 'planned', '--run-scratch', 'nope'],
      { env },
    )
    assert.deepEqual(JSON.parse(String(out)), {
      verdict: 'skip',
      reason: 'not-managed',
      sessionId: '',
    })
  })

  it('exits 64 on a usage error', () => {
    try {
      execFileSync(process.execPath, [script, '--outcome', 'planned'], { stdio: 'pipe' })
      assert.fail('expected a usage exit')
    } catch (error) {
      assert.equal(error.status, EXIT_USAGE)
    }
  })
})
