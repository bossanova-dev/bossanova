import { isSendBack } from './verify/verify-vocabulary.mjs'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { describe, it } from 'node:test'
import { fileURLToPath } from 'node:url'

import * as stageChain from './stage-chain.mjs'
import { rearmArgv } from './verify/verify-gate.mjs'

import {
  EXIT_USAGE,
  VERIFY_CHAT_TITLE,
  armDecision,
  hasStageJob,
  invokedSkill,
  isEpicChild,
  main,
  pickVerifyChat,
} from './stage-chain.mjs'

const SCRIPT = fileURLToPath(new URL('./stage-chain.mjs', import.meta.url))
const BOSS = '/opt/boss/bin/boss'

const job = (over = {}) => ({ id: 'job-1', prompt: '/boss-build', enabled: true, ...over })

// ---------------------------------------------------------------------------------------------
// Pure decisions
// ---------------------------------------------------------------------------------------------

describe('invokedSkill — first /name or $name token, exact and prefix-safe', () => {
  it('reads slash and codex dollar tokens', () => {
    assert.equal(invokedSkill('/boss-build'), 'boss-build')
    assert.equal(invokedSkill('$boss-build'), 'boss-build')
    assert.equal(invokedSkill('/boss-build --dry-run'), 'boss-build')
    assert.equal(invokedSkill('please run /boss-verify\n'), 'boss-verify')
  })

  it('never yields the stage skill for a longer name, a URL or prose', () => {
    for (const [prompt, expected] of [
      ['/boss-build-ce', 'boss-build-ce'],
      ['/boss-builder', 'boss-builder'],
      ['$boss-verify-extra', 'boss-verify-extra'],
      ['https://x/boss-build', null],
      ['run boss-build now', null],
      ['/usr/bin/boss-build', null],
      ['/boss-build_x', null],
      ['/boss-buildX', null],
    ])
      assert.equal(invokedSkill(prompt), expected, prompt)
  })

  it('the first token wins', () => {
    assert.equal(invokedSkill('/boss-plan then /boss-build'), 'boss-plan')
    assert.equal(invokedSkill('$boss-verify /boss-build'), 'boss-verify')
  })

  it('non-strings and empty prompts yield null', () => {
    for (const prompt of [undefined, null, 42, '', '   ']) assert.equal(invokedSkill(prompt), null)
  })
})

describe('hasStageJob', () => {
  it('is consent: one or several enabled jobs, never a disabled or prefix-named one', () => {
    const v = (over) => job({ prompt: '/boss-verify', ...over })
    assert.equal(hasStageJob({ stage: 'verify', jobs: [v()] }), true)
    assert.equal(hasStageJob({ stage: 'verify', jobs: [v({ id: 'a' }), v({ id: 'b' })] }), true)
    assert.equal(hasStageJob({ stage: 'verify', jobs: [v({ enabled: false })] }), false)
    assert.equal(hasStageJob({ stage: 'verify', jobs: [v({ prompt: '/boss-verify-x' })] }), false)
    assert.equal(hasStageJob({ stage: 'verify', jobs: null }), false)
    assert.equal(hasStageJob({ stage: 'nope', jobs: [v()] }), false)
  })
})

describe('pickVerifyChat', () => {
  it('picks the newest live chat titled verify', () => {
    const chats = [
      {
        agent_session_id: 'old',
        title: 'verify',
        status: 'IDLE',
        created_at: '2026-01-01T00:00:00Z',
      },
      {
        agent_session_id: 'new',
        title: 'verify',
        status: 'IDLE',
        created_at: '2026-02-01T00:00:00Z',
      },
      {
        agent_session_id: 'main',
        title: 'main',
        status: 'IDLE',
        created_at: '2026-03-01T00:00:00Z',
      },
    ]
    assert.equal(pickVerifyChat(chats).agent_session_id, 'new')
  })

  it('skips STOPPED chats, other titles and id-less rows', () => {
    assert.equal(
      pickVerifyChat([
        { agent_session_id: 's', title: 'verify', status: 'STOPPED' },
        { agent_session_id: 's2', title: 'verify', status: 'CHAT_STATUS_STOPPED' },
        { agent_session_id: '', title: 'verify', status: 'IDLE' },
        { agent_session_id: 'x', title: 'verifier', status: 'IDLE' },
      ]),
      null,
    )
  })

  it('matches the title trimmed and case-insensitively, like the verify router always has', () => {
    assert.equal(
      pickVerifyChat([{ agent_session_id: 'v', title: ' Verify ' }]).agent_session_id,
      'v',
    )
    assert.equal(VERIFY_CHAT_TITLE, 'verify')
  })

  it('finds the verify chat by role after an agent renamed it', () => {
    const chats = [
      {
        agent_session_id: 'legacy',
        title: 'verify',
        status: 'IDLE',
        created_at: '2026-03-01T00:00:00Z',
      },
      {
        agent_session_id: 'renamed',
        title: '再確認する PR #3100 とセッション状態',
        role: 'verify',
        status: 'IDLE',
        created_at: '2026-02-01T00:00:00Z',
      },
    ]
    assert.equal(pickVerifyChat(chats).agent_session_id, 'renamed')
  })

  it('never takes a chat with another role, whatever its title', () => {
    assert.equal(
      pickVerifyChat([
        { agent_session_id: 'op', title: 'verify', role: 'operator', status: 'IDLE' },
        { agent_session_id: 'b', title: 'verify', role: 'build', status: 'IDLE' },
      ]),
      null,
    )
  })

  it('falls back to the title for a chat with no role', () => {
    assert.equal(
      pickVerifyChat([{ agent_session_id: 'v', title: 'verify', role: '', status: 'IDLE' }])
        .agent_session_id,
      'v',
    )
  })

  it('non-arrays yield null', () => {
    assert.equal(pickVerifyChat(null), null)
    assert.equal(pickVerifyChat({ chats: [] }), null)
  })
})

describe('armDecision', () => {
  it('fast-path-off hands off with a state-matched arm', () => {
    assert.deepEqual(armDecision({ action: 'skipped', reason: 'fast-path-off' }), {
      arm: 'state',
      reason: 'fast-path-off',
    })
  })

  it('merged ⇒ none', () => {
    assert.deepEqual(armDecision({ action: 'merged', reason: 'merged-by-verify' }), {
      arm: 'none',
      reason: 'merged',
    })
  })

  it('a posted human verdict ⇒ none', () => {
    assert.equal(armDecision({ action: 'skipped', reason: 'human:policy' }).arm, 'none')
    assert.equal(armDecision({ action: 'skipped', reason: 'human' }).arm, 'none')
  })

  it('epic-child ⇒ none', () => {
    assert.deepEqual(armDecision({ action: 'skipped', reason: 'epic-child' }), {
      arm: 'none',
      reason: 'epic-child',
    })
  })

  it('a posted defect ⇒ transition', () => {
    assert.deepEqual(armDecision({ action: 'skipped', reason: 'defect:findings' }), {
      arm: 'transition',
      reason: 'defect',
    })
  })

  it('everything else ⇒ state', () => {
    for (const record of [
      { action: 'skipped', reason: 'ci-not-settled' },
      { action: 'skipped', reason: 'wait:checks-pending' },
      { action: 'skipped', reason: 'no-session' },
      { action: 'skipped' },
      null,
      undefined,
      [],
      'garbage',
    ])
      assert.equal(armDecision(record).arm, 'state', JSON.stringify(record))
  })

  it('a record from another run is ignored when a run id is given', () => {
    const record = { runId: 'old', action: 'skipped', reason: 'defect:findings' }
    assert.deepEqual(armDecision(record, { runId: 'new' }), {
      arm: 'state',
      reason: 'stale-record',
    })
    assert.equal(armDecision(record, { runId: 'old' }).arm, 'transition')
  })
})

describe('isEpicChild', () => {
  it('unattended without a cron job id is an epic child', () => {
    assert.equal(isEpicChild({ BOSS_UNATTENDED: 'true' }), true)
    assert.equal(isEpicChild({ BOSS_CRON: 'true' }), true)
    assert.equal(isEpicChild({ BOSS_UNATTENDED: 'true', BOSS_CRON_JOB_ID: 'j' }), false)
    assert.equal(isEpicChild({}), false)
  })
})

// ---------------------------------------------------------------------------------------------
// CLI against a fake `boss`
// ---------------------------------------------------------------------------------------------

const managedEnv = (over = {}) => ({
  BOSS_SESSION_ID: 'sess-1',
  BOSS_AGENT_SESSION_ID: 'chat-self',
  BOSS_REPO_ID: 'repo-1',
  BOSS_UNATTENDED: 'true',
  BOSS_CRON_JOB_ID: 'job-build',
  ...over,
})

const json = (value) => ({ stdout: JSON.stringify(value, null, 2) })

/**
 * A fake execFile. `routes` maps the argv joined by spaces (or its first words) to a response:
 * `{stdout, stderr, code, timeout, enoent}`. Every call is recorded; an unrouted call fails the test.
 */
function fakeBoss(routes) {
  const calls = []
  const execFile = (file, args, options, cb) => {
    calls.push({ file, args, timeout: options.timeout })
    const key = args.join(' ')
    const route = Object.entries(routes).find(
      ([prefix]) => key === prefix || key.startsWith(`${prefix} `),
    )
    const respond = route ? route[1] : { code: 99, stderr: `unrouted: ${key}` }
    process.nextTick(() => {
      if (respond.enoent)
        return cb(Object.assign(new Error('spawn ENOENT'), { code: 'ENOENT' }), '', '')
      if (respond.timeout)
        return cb(Object.assign(new Error('killed'), { killed: true, signal: 'SIGKILL' }), '', '')
      if (respond.code)
        return cb(
          Object.assign(new Error('exit'), { code: respond.code }),
          respond.stdout ?? '',
          respond.stderr ?? '',
        )
      cb(null, respond.stdout ?? '', respond.stderr ?? '')
    })
    return { on() {} }
  }
  return { execFile, calls, argvs: () => calls.map((c) => c.args) }
}

async function run(argv, { routes = {}, env = managedEnv(), fs, resolveBoss } = {}) {
  const boss = fakeBoss(routes)
  let warnings = ''
  const { exitCode, line } = await main(argv, {
    env,
    execFile: boss.execFile,
    fs,
    cwd: '/work/tree',
    stderr: { write: (s) => (warnings += s) },
    resolveBoss: resolveBoss ?? (() => ({ ok: true, path: BOSS, reason: '' })),
  })
  return { exitCode, result: line ? JSON.parse(line) : null, boss, warnings, line }
}

const verifyJobs = [{ id: 'job-verify', prompt: '/boss-verify', enabled: true, repo_id: 'repo-1' }]
const fakeFs = (files) => ({
  readFileSync: (p) => {
    if (!(p in files)) throw Object.assign(new Error('ENOENT'), { code: 'ENOENT' })
    return files[p]
  },
})

describe('scheduled stages stay independent', () => {
  it('next/run-next are usage errors', async () => {
    for (const verb of ['next', 'run-next']) {
      const { exitCode, line, boss } = await run([verb, '--stage', 'plan'])
      assert.equal(exitCode, EXIT_USAGE)
      assert.equal(line, '')
      assert.deepEqual(boss.argvs(), [])
      const cli = spawnSync(process.execPath, [SCRIPT, verb, '--stage', 'plan'], {
        encoding: 'utf8',
      })
      assert.equal(cli.status, EXIT_USAGE)
      assert.equal(cli.stdout, '')
    }
  })

  it('removes cron-starting exports and retains callback helpers', () => {
    for (const name of [
      'NEXT_STAGE',
      'resolveNext',
      'runNowArgv',
      'parseRunNow',
      'RUN_NOW_TIMEOUT_MS',
    ])
      assert.equal(Object.hasOwn(stageChain, name), false, name)
    for (const name of ['STAGE_SKILL', 'invokedSkill', 'hasStageJob', 'pickVerifyChat'])
      assert.equal(Object.hasOwn(stageChain, name), true, name)
  })

  it('no verb issues cron run-now', async () => {
    const completion = '/git/completion.json'
    const records = [
      null,
      { action: 'merged' },
      { action: 'skipped', reason: 'human:policy' },
      { action: 'skipped', reason: 'epic-child' },
      { action: 'skipped', reason: 'defect:findings' },
      { action: 'skipped', reason: 'wait:checks-pending' },
      { action: 'skipped', reason: 'unrecognized' },
    ]
    for (const record of records) {
      for (const argv of [
        ['arm-verify', '--pr', '12', '--completion', completion],
        ['phase', 'building'],
      ]) {
        const { exitCode, result, boss } = await run(argv, {
          fs: fakeFs({ [completion]: JSON.stringify(record) }),
          routes: {
            'cron ls': json(
              ['build', 'verify', 'release'].map((stage) =>
                job({ id: stage, prompt: `/boss-${stage}` }),
              ),
            ),
            'chats sess-1': json({ chats: [] }),
            'chat new sess-1': json({ chat: { agent_session_id: 'chat-v' } }),
            'callback add': json({ id: 'cb' }),
            'session phase building': { stdout: 'phase set: building' },
          },
        })
        assert.equal(exitCode, 0)
        assert.notEqual(result.action, 'error')
        assert.ok(boss.calls.length > 0)
        assert.equal(
          boss.argvs().some((args) => args[0] === 'cron' && args[1] === 'run-now'),
          false,
        )
      }
    }
  })
})

describe('arm-verify', () => {
  const completion = '/git/boss-build-completion.json'
  const chatsRoute = (chats) => ({ 'chats sess-1': json({ chats }) })

  it('no verify chat: chat new --title verify, then callback add (state-matched)', async () => {
    const { exitCode, result, boss } = await run(['arm-verify', '--pr', '12'], {
      routes: {
        'cron ls': json(verifyJobs),
        ...chatsRoute([{ agent_session_id: 'chat-self', title: 'build', status: 'WORKING' }]),
        'chat new sess-1': json({
          chat: { agent_session_id: 'chat-v', session_id: 'sess-1', title: 'verify' },
        }),
        'callback add': json({ id: 'cb-1' }),
      },
    })
    assert.equal(exitCode, 0)
    assert.deepEqual(boss.argvs(), [
      ['cron', 'ls', '--repo', 'repo-1', '--json'],
      ['chats', 'sess-1', '--json'],
      ['chat', 'new', 'sess-1', '--title', 'verify', '--role', 'verify', '--json'],
      [
        'callback',
        'add',
        '12',
        'checks_passed_ready',
        '--chat',
        'chat-v',
        '--message',
        '/boss-verify 12',
        '--expires-in',
        '7d',
        '--json',
      ],
    ])
    assert.equal(result.action, 'armed')
    assert.equal(result.mode, 'state')
    assert.equal(result.chatId, 'chat-v')
    assert.equal(result.chatCreated, true)
    assert.equal(result.callbackId, 'cb-1')
  })

  it('a defect record arms the same callback with --on-transition', async () => {
    const { result, boss } = await run(['arm-verify', '--pr', '12', '--completion', completion], {
      fs: fakeFs({
        [completion]: JSON.stringify({ runId: 'r1', action: 'skipped', reason: 'defect:findings' }),
      }),
      routes: {
        'cron ls': json(verifyJobs),
        ...chatsRoute([]),
        'chat new sess-1': json({ chat: { agent_session_id: 'chat-v' } }),
        'callback add': json({ id: 'cb-2' }),
      },
    })
    assert.deepEqual(boss.argvs().at(-1), [
      'callback',
      'add',
      '12',
      'checks_passed_ready',
      '--chat',
      'chat-v',
      '--message',
      '/boss-verify 12',
      '--expires-in',
      '7d',
      '--on-transition',
      '--json',
    ])
    assert.equal(result.mode, 'transition')
  })

  it('reuses a live verify chat: no chat new, no chat send', async () => {
    const { result, boss } = await run(['arm-verify', '--pr', '12'], {
      routes: {
        'cron ls': json(verifyJobs),
        ...chatsRoute([{ agent_session_id: 'chat-v', title: 'verify', status: 'IDLE' }]),
        'callback list': json([]),
        'callback add': json({ id: 'cb-3' }),
      },
    })
    const argvs = boss.argvs()
    assert.deepEqual(argvs[2], [
      'callback',
      'list',
      '--chat',
      'chat-v',
      '--pr',
      '12',
      '--trigger',
      'checks_passed_ready',
      '--state',
      'active',
      '--json',
    ])
    assert.equal(
      argvs.some((a) => a[0] === 'chat' && a[1] === 'new'),
      false,
    )
    assert.equal(
      argvs.some((a) => a[0] === 'chat' && a[1] === 'send'),
      false,
    )
    assert.equal(result.action, 'armed')
    assert.equal(result.chatCreated, false)
  })

  it('a STOPPED verify chat is not reused', async () => {
    const { boss } = await run(['arm-verify', '--pr', '12'], {
      routes: {
        'cron ls': json(verifyJobs),
        ...chatsRoute([{ agent_session_id: 'chat-dead', title: 'verify', status: 'STOPPED' }]),
        'chat new sess-1': json({ chat: { agent_session_id: 'chat-v2' } }),
        'callback add': json({ id: 'cb-4' }),
      },
    })
    assert.deepEqual(boss.argvs()[2], [
      'chat',
      'new',
      'sess-1',
      '--title',
      'verify',
      '--role',
      'verify',
      '--json',
    ])
  })

  it('an active row for that chat and PR ⇒ already-armed, no callback add', async () => {
    const { result, boss } = await run(['arm-verify', '--pr', '12'], {
      routes: {
        'cron ls': json(verifyJobs),
        ...chatsRoute([{ agent_session_id: 'chat-v', title: 'verify', status: 'IDLE' }]),
        'callback list': json([
          {
            id: 'cb-old',
            pr_number: 12,
            target_chat_id: 'chat-v',
            trigger: 'checks_passed_ready',
            state: 'active',
          },
        ]),
      },
    })
    assert.equal(result.action, 'already-armed')
    assert.equal(result.callbackId, 'cb-old')
    assert.equal(
      boss.argvs().some((a) => a[0] === 'callback' && a[1] === 'add'),
      false,
    )
  })

  it('no enabled verify job ⇒ none/no-verify-cron, nothing else read', async () => {
    const { result, boss } = await run(['arm-verify', '--pr', '12'], {
      routes: { 'cron ls': json([job({ prompt: '/boss-verify-extra' })]) },
    })
    assert.equal(`${result.action}/${result.reason}`, 'none/no-verify-cron')
    assert.equal(boss.calls.length, 1)
  })

  it('a merged or human record arms nothing', async () => {
    for (const reasonRecord of [
      { action: 'merged', reason: 'merged-by-verify' },
      { action: 'skipped', reason: 'human:needs-review' },
    ]) {
      const { result, boss } = await run(['arm-verify', '--pr', '12', '--completion', completion], {
        fs: fakeFs({ [completion]: JSON.stringify(reasonRecord) }),
        routes: { 'cron ls': json(verifyJobs) },
      })
      assert.equal(result.action, 'none', JSON.stringify(reasonRecord))
      assert.equal(boss.calls.length, 1)
    }
  })

  it('an absent or unparseable record arms state-matched', async () => {
    for (const files of [{}, { [completion]: '{not json' }]) {
      const { result } = await run(['arm-verify', '--pr', '12', '--completion', completion], {
        fs: fakeFs(files),
        routes: {
          'cron ls': json(verifyJobs),
          ...chatsRoute([]),
          'chat new sess-1': json({ chat: { agent_session_id: 'chat-v' } }),
          'callback add': json({ id: 'cb' }),
        },
      })
      assert.equal(result.mode, 'state')
    }
  })

  it('an epic child arms nothing and calls nothing', async () => {
    const { exitCode, result, boss } = await run(['arm-verify', '--pr', '12'], {
      env: managedEnv({ BOSS_CRON_JOB_ID: '' }),
    })
    assert.equal(exitCode, 0)
    assert.equal(`${result.action}/${result.reason}`, 'skipped/epic-child')
    assert.equal(boss.calls.length, 0)
  })
})

describe('phase', () => {
  it('issues exactly session phase <name>', async () => {
    const { result, boss } = await run(['phase', 'building'], {
      routes: { 'session phase building': { stdout: 'phase set: building\n' } },
    })
    assert.deepEqual(boss.argvs(), [['session', 'phase', 'building']])
    assert.equal(`${result.action}/${result.phase}`, 'set/building')
  })

  it('works without BOSS_REPO_ID', async () => {
    const { result } = await run(['phase', 'planning'], {
      env: managedEnv({ BOSS_REPO_ID: '' }),
      routes: { 'session phase planning': { stdout: 'phase set: planning\n' } },
    })
    assert.equal(result.action, 'set')
  })

  it('an older CLI without the subcommand ⇒ skipped/phase-unsupported, exit 0', async () => {
    const { exitCode, result } = await run(['phase', 'building'], {
      routes: {
        'session phase': { code: 1, stderr: 'Error: unknown command "phase" for "boss session"\n' },
      },
    })
    assert.equal(exitCode, 0)
    assert.equal(`${result.action}/${result.reason}`, 'skipped/phase-unsupported')
  })
})

describe('non-fatal: every failure exits 0 with one JSON line', () => {
  const failures = {
    'non-zero exit': { code: 1, stderr: 'rpc error: unavailable\n' },
    'unparseable JSON': { stdout: 'not json' },
    ENOENT: { enoent: true },
    timeout: { timeout: true },
  }

  for (const [name, response] of Object.entries(failures)) {
    it(`a ${name} on cron ls ⇒ error`, async () => {
      for (const argv of [['arm-verify', '--pr', '3']]) {
        const { exitCode, result } = await run(argv, { routes: { 'cron ls': response } })
        assert.equal(exitCode, 0, argv.join(' '))
        assert.equal(result.action, 'error', argv.join(' '))
        assert.match(result.reason, /^cron-ls-/)
      }
    })
  }

  it('a timeout on callback add ⇒ error with reason timeout', async () => {
    const { exitCode, result } = await run(['arm-verify', '--pr', '12'], {
      routes: {
        'cron ls': json(verifyJobs),
        'chats sess-1': json({ chats: [] }),
        'chat new sess-1': json({ chat: { agent_session_id: 'chat-v' } }),
        'callback add': { timeout: true },
      },
    })
    assert.equal(exitCode, 0)
    assert.equal(`${result.action}/${result.reason}`, 'error/callback-add-timeout')
  })

  it('a failed phase call ⇒ error', async () => {
    for (const response of Object.values(failures).filter((r) => !r.stdout)) {
      const { exitCode, result } = await run(['phase', 'building'], {
        routes: { 'session phase': response },
      })
      assert.equal(exitCode, 0)
      assert.equal(result.action, 'error')
    }
  })

  it('each arm-verify step failing ⇒ error, never armed', async () => {
    const base = {
      'cron ls': json(verifyJobs),
      'chats sess-1': json({ chats: [] }),
      'chat new sess-1': json({ chat: { agent_session_id: 'chat-v' } }),
      'callback add': json({ id: 'cb' }),
    }
    for (const broken of ['chats sess-1', 'chat new sess-1', 'callback add']) {
      for (const response of Object.values(failures)) {
        const { exitCode, result } = await run(['arm-verify', '--pr', '12'], {
          routes: { ...base, [broken]: response },
        })
        assert.equal(exitCode, 0)
        assert.equal(result.action, 'error', `${broken} ${JSON.stringify(response)}`)
      }
    }
    const reuse = {
      ...base,
      'chats sess-1': json({ chats: [{ agent_session_id: 'chat-v', title: 'verify' }] }),
    }
    for (const response of Object.values(failures)) {
      const { result } = await run(['arm-verify', '--pr', '12'], {
        routes: { ...reuse, 'callback list': response },
      })
      assert.equal(result.action, 'error')
    }
  })

  it('unmanaged, missing binary and missing repo id are skips that call nothing', async () => {
    for (const argv of [
      ['arm-verify', '--pr', '1'],
      ['phase', 'x'],
    ]) {
      const unmanaged = await run(argv, { env: managedEnv({ BOSS_SESSION_ID: '' }) })
      assert.equal(unmanaged.exitCode, 0)
      assert.equal(`${unmanaged.result.action}/${unmanaged.result.reason}`, 'skipped/not-managed')
      assert.equal(unmanaged.boss.calls.length, 0)

      const noBinary = await run(argv, {
        resolveBoss: () => ({ ok: false, path: null, reason: 'BOSS_BIN is unset' }),
      })
      assert.equal(noBinary.exitCode, 0)
      assert.equal(`${noBinary.result.action}/${noBinary.result.reason}`, 'skipped/no-boss-binary')
      assert.equal(noBinary.boss.calls.length, 0)
    }
    for (const argv of [['arm-verify', '--pr', '1']]) {
      const noRepo = await run(argv, { env: managedEnv({ BOSS_REPO_ID: '' }) })
      assert.equal(noRepo.exitCode, 0)
      assert.equal(`${noRepo.result.action}/${noRepo.result.reason}`, 'skipped/no-repo-id')
      assert.equal(noRepo.boss.calls.length, 0)
    }
  })

  it('--dry-run prints the argv and issues no boss call', async () => {
    for (const argv of [
      ['arm-verify', '--pr', '7', '--dry-run'],
      ['phase', 'building', '--dry-run'],
    ]) {
      const { exitCode, result, boss } = await run(argv)
      assert.equal(exitCode, 0)
      assert.equal(result.action, 'dry-run', argv.join(' '))
      assert.ok(Array.isArray(result.argv) && result.argv.length > 0)
      assert.equal(boss.calls.length, 0, argv.join(' '))
    }
  })

  it('usage errors exit 64 and print nothing on stdout', async () => {
    for (const argv of [
      [],
      ['bogus'],
      ['arm-verify'],
      ['arm-verify', '--pr', 'abc'],
      ['arm-verify', '--pr', '0'],
      ['phase'],
      ['phase', 'a', 'b'],
      ['phase', 'x', '--stage', 'plan'],
    ]) {
      const { exitCode, line } = await run(argv)
      assert.equal(exitCode, EXIT_USAGE, JSON.stringify(argv))
      assert.equal(line, '')
    }
  })

  it('the real CLI exits 64 on a usage error and 0 with one JSON line when unmanaged', () => {
    const env = { PATH: process.env.PATH }
    const usage = spawnSync(process.execPath, [SCRIPT, 'bogus'], { env, encoding: 'utf8' })
    assert.equal(usage.status, EXIT_USAGE)
    assert.equal(usage.stdout, '')
    const unmanaged = spawnSync(process.execPath, [SCRIPT, 'phase', 'building'], {
      env,
      encoding: 'utf8',
    })
    assert.equal(unmanaged.status, 0)
    assert.deepEqual(JSON.parse(unmanaged.stdout), {
      verb: 'phase',
      action: 'skipped',
      reason: 'not-managed',
    })
  })
})

it('blocked verdict rearms only on a transition', () => {
  assert.deepEqual(armDecision({ action: 'skipped', reason: 'blocked:conflict' }), {
    arm: 'transition',
    reason: 'blocked',
  })
})

it('send-back descriptions distinguish both terminal failures from parks and live claims', () => {
  for (const value of ['defect: 2 finding(s)', 'blocked: 1 P2', 'blocked: conflict, needs human'])
    assert.equal(isSendBack(value), true)
  for (const value of ['needs human: no-receipt', 'waiting: ci', 'verified', null])
    assert.equal(isSendBack(value), false)
})

describe('arm-reverify', () => {
  const head = 'abcdef1234567890'
  const argv = ['arm-reverify', '--pr', '12', '--head', head]
  const chat = (id, over = {}) => ({
    agent_session_id: id,
    role: 'verify',
    title: 'verify',
    status: 'IDLE',
    ...over,
  })
  const row = (id, target, over = {}) => ({
    id,
    target_chat_id: target,
    pr_number: 12,
    trigger: 'checks_passed_ready',
    state: 'active',
    ...over,
  })
  const routes = (chats = [], rows = []) => ({
    'cron ls': json(verifyJobs),
    'chats sess-1': json({ chats }),
    'callback list': json(rows),
    'chat new sess-1': json({ chat: chat('new') }),
    'callback add': json({ id: 'fresh' }),
    'callback remove': { stdout: 'Removed callback old-cb.\n' },
  })
  const flag = (args, name) => args[args.indexOf(name) + 1]

  it('creates the first verify chat and state-matched callback with the pushed head group', async () => {
    const { result, boss, exitCode } = await run(argv, { routes: routes() })
    assert.equal(exitCode, 0)
    assert.equal(result.action, 'armed')
    assert.equal(result.chatTitle, 'verify')
    assert.equal(result.head, head)
    assert.equal(result.chatCreated, true)
    const calls = boss.argvs()
    assert.deepEqual(calls[2], [
      'callback',
      'list',
      '--pr',
      '12',
      '--trigger',
      'checks_passed_ready',
      '--state',
      'active',
      '--json',
    ])
    assert.deepEqual(calls[3], [
      'chat',
      'new',
      'sess-1',
      '--title',
      'verify',
      '--role',
      'verify',
      '--json',
    ])
    assert.equal(flag(calls[4], '--group'), 'reverify-12-abcdef123456')
    assert.equal(flag(calls[4], '--message'), '/boss-verify 12')
    assert.equal(flag(calls[4], '--expires-in'), '7d')
    assert.equal(calls[4].includes('--on-transition'), false)
    assert.deepEqual(result.superseded, [])
  })

  it('retires the old verify rearm only after arming verify 2', async () => {
    const { result, boss } = await run(argv, {
      routes: routes([chat('old')], [row('old-cb', 'old')]),
    })
    assert.equal(result.chatTitle, 'verify 2')
    assert.equal(flag(boss.argvs()[3], '--title'), 'verify 2')
    assert.deepEqual(boss.argvs().at(-1), ['callback', 'remove', 'old-cb', '--chat', 'old'])
    assert.deepEqual(result.superseded, ['old-cb'])
  })

  it('reuses an earlier pending repair chat and includes stopped chats in its ordinal', async () => {
    const chats = [
      chat('old'),
      chat('pending', { title: 'verify 2' }),
      chat('dead', { status: 'CHAT_STATUS_STOPPED' }),
    ]
    const rows = [row('prior', 'pending', { group_id: 'reverify-12-1234567' })]
    const reused = await run(argv, { routes: routes(chats, rows) })
    assert.equal(reused.result.chatId, 'pending')
    assert.equal(reused.result.chatTitle, 'verify 2')
    assert.equal(reused.result.chatCreated, false)
    assert.equal(
      reused.boss.argvs().some((a) => a[0] === 'chat'),
      false,
    )
    const stopped = await run(argv, {
      routes: routes(chats, [row('dead-cb', 'dead', { group_id: 'reverify-12-1234567' })]),
    })
    assert.equal(stopped.result.chatCreated, true)
    assert.equal(stopped.result.chatTitle, 'verify 4')
  })

  it('filters by active PR, trigger and this session verify chats including legacy titles', async () => {
    const rows = [
      row('keep', 'old'),
      row('foreign', 'foreign'),
      row('other-pr', 'old', { pr_number: 13 }),
      row('other-trigger', 'old', { trigger: 'checks_failed' }),
      row('fired', 'old', { state: 'delivered' }),
      row('build', 'build'),
    ]
    const { result } = await run(argv, {
      routes: routes(
        [chat('old', { role: '', title: ' Verify ' }), chat('build', { role: 'build' })],
        rows,
      ),
    })
    assert.deepEqual(result.superseded, ['keep'])
  })

  it('add failure preserves every old watch and remove failure still reports armed', async () => {
    const base = routes([chat('old')], [row('old-cb', 'old')])
    const failed = await run(argv, { routes: { ...base, 'callback add': { code: 1 } } })
    assert.equal(failed.result.action, 'error')
    assert.equal(
      failed.boss.argvs().some((a) => a[1] === 'remove'),
      false,
    )
    const removal = await run(argv, {
      routes: { ...base, 'callback remove': { code: 1, stderr: 'busy' } },
    })
    assert.equal(removal.result.action, 'armed')
    assert.deepEqual(removal.result.superseded, [])
    assert.equal(removal.result.supersedeFailed[0].id, 'old-cb')
    assert.match(removal.result.supersedeFailed[0].reason, /busy/)
  })

  it('honors consent, epic and managed skips and has a side-effect-free dry run', async () => {
    const disabled = await run(argv, { routes: { 'cron ls': json([]) } })
    assert.equal(`${disabled.result.action}/${disabled.result.reason}`, 'none/no-verify-cron')
    assert.equal(disabled.boss.calls.length, 1)
    for (const [env, reason] of [
      [managedEnv({ BOSS_CRON_JOB_ID: '' }), 'epic-child'],
      [managedEnv({ BOSS_SESSION_ID: '' }), 'not-managed'],
      [managedEnv({ BOSS_REPO_ID: '' }), 'no-repo-id'],
    ]) {
      const r = await run(argv, { env })
      assert.equal(r.result.reason, reason)
      assert.equal(r.boss.calls.length, 0)
    }
    const dry = await run([...argv, '--dry-run'])
    assert.equal(dry.result.action, 'dry-run')
    assert.equal(dry.boss.calls.length, 0)
    assert.ok(dry.result.argv.some((a) => a.includes('reverify-12-abcdef123456')))
    const noBoss = await run(argv, { resolveBoss: () => ({ ok: false }) })
    assert.equal(noBoss.result.reason, 'no-boss-binary')
  })

  it('rejects missing, short, long and non-hex head values with exit 64', async () => {
    for (const value of [null, 'abc123', 'g123456', 'a'.repeat(41)]) {
      const r = await run(['arm-reverify', '--pr', '12', ...(value ? ['--head', value] : [])])
      assert.equal(r.exitCode, EXIT_USAGE)
      assert.equal(r.line, '')
    }
  })

  it('build → verify send-back → repair → verify 2 delivers without a cron tick', async () => {
    const chats = [],
      callbacks = [],
      deliveries = [],
      calls = []
    const flagValue = (args, name) => (args.includes(name) ? flag(args, name) : '')
    const execFile = (_file, args, _options, cb) => {
      calls.push(args)
      let value
      if (args[0] === 'cron' && args[1] === 'ls') value = verifyJobs
      else if (args[0] === 'chats') value = { chats }
      else if (args[0] === 'chat' && args[1] === 'new') {
        const created = chat(`chat-${chats.length + 1}`, { title: flagValue(args, '--title') })
        chats.push(created)
        value = { chat: created }
      } else if (args[0] === 'callback' && args[1] === 'add') {
        value = row(`cb-${callbacks.length + 1}`, flagValue(args, '--chat'), {
          group_id: flagValue(args, '--group'),
          message: flagValue(args, '--message'),
        })
        callbacks.push(value)
      } else if (args[0] === 'callback' && args[1] === 'list')
        value = callbacks.filter((r) => r.state === 'active')
      else if (args[0] === 'callback' && args[1] === 'remove') {
        const target = callbacks.find(
          (r) => r.id === args[2] && r.target_chat_id === flagValue(args, '--chat'),
        )
        assert.ok(target)
        target.state = 'cancelled'
        value = { deleted: true }
      } else assert.fail(`unexpected call: ${args.join(' ')}`)
      process.nextTick(() => cb(null, JSON.stringify(value), ''))
      return { on() {} }
    }
    const deps = { env: managedEnv(), execFile, resolveBoss: () => ({ ok: true, path: BOSS }) }
    const built = JSON.parse((await main(['arm-verify', '--pr', '12'], deps)).line)
    assert.equal(built.action, 'armed')
    callbacks[0].state = 'delivered'
    await new Promise((resolve) =>
      execFile(BOSS, rearmArgv({ pr: 12, chat: built.chatId }), {}, resolve),
    )
    assert.equal(JSON.parse((await main(argv, deps)).line).action, 'armed')
    const active = callbacks.filter((r) => r.state === 'active')
    assert.equal(active.length, 1)
    const receiver = chats.find((c) => c.agent_session_id === active[0].target_chat_id)
    assert.equal(receiver.role, 'verify')
    assert.equal(receiver.title, 'verify 2')
    deliveries.push({ chat: receiver.title, message: active[0].message })
    active[0].state = 'delivered'
    assert.deepEqual(deliveries, [{ chat: 'verify 2', message: '/boss-verify 12' }])
    assert.equal(
      calls.some((a) => a[0] === 'cron' && a[1] !== 'ls'),
      false,
    )
  })
})

describe('routed verification ownership', () => {
  it('recognizes normalized working status', () => {
    for (const status of ['WORKING', ' CHAT_STATUS_WORKING ', 'working'])
      assert.equal(stageChain.chatIsBusy({ status }), true)
    for (const chat of [null, {}, { status: 'IDLE' }, { status: 'STOPPED' }])
      assert.equal(stageChain.chatIsBusy(chat), false)
  })
  it('keeps another head reverify group and callbacks outside its session or PR', () => {
    const base = {
      id: 'a',
      pr_number: 7,
      trigger: 'checks_passed_ready',
      state: 'active',
      target_chat_id: 'v',
    }
    const rows = [
      base,
      { ...base, id: 'same', group_id: 'reverify-7-abcdef123456' },
      { ...base, id: 'next', group_id: 'reverify-7-123456abcdef' },
      { ...base, id: 'foreign', target_chat_id: 'other' },
      { ...base, id: 'other-pr', pr_number: 8 },
    ]
    assert.deepEqual(
      stageChain.routedSupersedeRows(rows, ['v'], 7, 'abcdef123456ffff').map((row) => row.id),
      ['a', 'same'],
    )
  })
})
