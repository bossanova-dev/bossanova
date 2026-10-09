import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { describe, it } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  CALL_TIMEOUT_MS,
  EXIT_USAGE,
  NEXT_STAGE,
  RUN_NOW_TIMEOUT_MS,
  STAGE_SKILL,
  VERIFY_CHAT_TITLE,
  armDecision,
  hasStageJob,
  invokedSkill,
  isEpicChild,
  main,
  parseRunNow,
  pickVerifyChat,
  resolveNext,
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

describe('resolveNext', () => {
  it('one enabled match ⇒ run-now with its id', () => {
    assert.deepEqual(resolveNext({ stage: 'plan', jobs: [job({ id: 'b1' })] }), {
      action: 'run-now',
      jobId: 'b1',
      nextStage: 'build',
      reason: '',
      matches: ['b1'],
    })
  })

  it('no jobs ⇒ none/no-job', () => {
    const r = resolveNext({ stage: 'plan', jobs: [] })
    assert.equal(`${r.action}/${r.reason}`, 'none/no-job')
  })

  it('only disabled matches ⇒ none/disabled', () => {
    const r = resolveNext({ stage: 'plan', jobs: [job({ enabled: false })] })
    assert.equal(`${r.action}/${r.reason}`, 'none/disabled')
  })

  it('two enabled matches ⇒ ambiguous with both ids', () => {
    const r = resolveNext({ stage: 'plan', jobs: [job({ id: 'a' }), job({ id: 'b' })] })
    assert.equal(r.action, 'ambiguous')
    assert.deepEqual(r.matches, ['a', 'b'])
    assert.equal(r.jobId, '')
  })

  it('release is terminal', () => {
    const r = resolveNext({ stage: 'release', jobs: [job()] })
    assert.equal(`${r.action}/${r.reason}`, 'none/terminal-stage')
    assert.equal(r.nextStage, null)
  })

  it('a non-array listing ⇒ none/unreadable-jobs', () => {
    for (const jobs of [null, undefined, { jobs: [] }, 'x']) {
      const r = resolveNext({ stage: 'plan', jobs })
      assert.equal(`${r.action}/${r.reason}`, 'none/unreadable-jobs')
    }
  })

  it('each stage matches exactly the next stage skill', () => {
    const jobs = [
      job({ id: 'p', prompt: '/boss-plan' }),
      job({ id: 'b', prompt: '/boss-build' }),
      job({ id: 'v', prompt: '$boss-verify' }),
      job({ id: 'r', prompt: '/boss-release' }),
      job({ id: 'ce', prompt: '/boss-build-ce' }),
    ]
    assert.equal(resolveNext({ stage: 'plan', jobs }).jobId, 'b')
    assert.equal(resolveNext({ stage: 'build', jobs }).jobId, 'v')
    assert.equal(resolveNext({ stage: 'verify', jobs }).jobId, 'r')
    assert.deepEqual(NEXT_STAGE, {
      plan: 'build',
      build: 'verify',
      verify: 'release',
      release: null,
    })
    assert.equal(STAGE_SKILL.verify, 'boss-verify')
  })

  it('prefix-named jobs never match', () => {
    for (const prompt of ['/boss-build-ce', '/boss-builder', 'https://x/boss-build', 'boss-build'])
      assert.equal(resolveNext({ stage: 'plan', jobs: [job({ prompt })] }).reason, 'no-job', prompt)
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

  it('non-arrays yield null', () => {
    assert.equal(pickVerifyChat(null), null)
    assert.equal(pickVerifyChat({ chats: [] }), null)
  })
})

describe('armDecision', () => {
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

describe('isEpicChild and parseRunNow', () => {
  it('unattended without a cron job id is an epic child', () => {
    assert.equal(isEpicChild({ BOSS_UNATTENDED: 'true' }), true)
    assert.equal(isEpicChild({ BOSS_CRON: 'true' }), true)
    assert.equal(isEpicChild({ BOSS_UNATTENDED: 'true', BOSS_CRON_JOB_ID: 'j' }), false)
    assert.equal(isEpicChild({}), false)
  })

  it('reads both run-now lines', () => {
    assert.deepEqual(parseRunNow('Started session s-9 for cron job j-1\n'), {
      action: 'started',
      sessionId: 's-9',
    })
    assert.deepEqual(parseRunNow('Fire skipped for cron job j-1: overlap_prev_active\n'), {
      action: 'skipped',
      reason: 'overlap_prev_active',
    })
    assert.deepEqual(parseRunNow('???'), { action: 'error', reason: 'unrecognized-output' })
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

describe('next / run-next', () => {
  it('run-next issues exactly cron ls then cron run-now <id> and reports started', async () => {
    const { exitCode, result, boss } = await run(['run-next', '--stage', 'plan'], {
      routes: {
        'cron ls': json([job({ id: 'job-b' })]),
        'cron run-now job-b': { stdout: 'Started session s-42 for cron job job-b\n' },
      },
    })
    assert.equal(exitCode, 0)
    assert.deepEqual(boss.argvs(), [
      ['cron', 'ls', '--repo', 'repo-1', '--json'],
      ['cron', 'run-now', 'job-b'],
    ])
    assert.equal(boss.calls[0].file, BOSS)
    assert.equal(boss.calls[0].timeout, CALL_TIMEOUT_MS)
    assert.equal(boss.calls[1].timeout, RUN_NOW_TIMEOUT_MS)
    assert.equal(result.action, 'started')
    assert.equal(result.sessionId, 's-42')
    assert.equal(result.jobId, 'job-b')
  })

  it('run-next reports a scheduler skip (overlap) as skipped', async () => {
    const { result } = await run(['run-next', '--stage', 'build'], {
      routes: {
        'cron ls': json(verifyJobs),
        'cron run-now job-verify': {
          stdout: 'Fire skipped for cron job job-verify: overlap_prev_active\n',
        },
      },
    })
    assert.equal(result.action, 'skipped')
    assert.equal(result.reason, 'overlap_prev_active')
  })

  it('run-next with no matching job runs nothing after the listing', async () => {
    const { result, boss } = await run(['run-next', '--stage', 'plan'], {
      routes: { 'cron ls': json([]) },
    })
    assert.equal(`${result.action}/${result.reason}`, 'none/no-job')
    assert.equal(boss.calls.length, 1)
  })

  it('run-next on two matching jobs is ambiguous, warns and fires nothing', async () => {
    const { result, boss, warnings } = await run(['run-next', '--stage', 'plan'], {
      routes: { 'cron ls': json([job({ id: 'a' }), job({ id: 'b' })]) },
    })
    assert.equal(result.action, 'ambiguous')
    assert.deepEqual(result.matches, ['a', 'b'])
    assert.equal(boss.calls.length, 1)
    assert.match(warnings, /2 enabled cron jobs run \/boss-build/)
  })

  it('next only reads and never fires', async () => {
    const { result, boss } = await run(['next', '--stage', 'plan'], {
      routes: { 'cron ls': json([job({ id: 'b1' })]) },
    })
    assert.equal(result.verb, 'next')
    assert.equal(result.action, 'run-now')
    assert.equal(result.jobId, 'b1')
    assert.equal(boss.calls.length, 1)
  })

  it('release is terminal even with jobs listed', async () => {
    const { result } = await run(['run-next', '--stage', 'release'], {
      routes: { 'cron ls': json([job()]) },
    })
    assert.equal(`${result.action}/${result.reason}`, 'none/terminal-stage')
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
      ['chat', 'new', 'sess-1', '--title', 'verify', '--json'],
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
    assert.deepEqual(boss.argvs()[2], ['chat', 'new', 'sess-1', '--title', 'verify', '--json'])
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
      for (const argv of [
        ['next', '--stage', 'plan'],
        ['run-next', '--stage', 'plan'],
        ['arm-verify', '--pr', '3'],
      ]) {
        const { exitCode, result } = await run(argv, { routes: { 'cron ls': response } })
        assert.equal(exitCode, 0, argv.join(' '))
        assert.equal(result.action, 'error', argv.join(' '))
        assert.match(result.reason, /^cron-ls-/)
      }
    })
  }

  it('a timeout on cron run-now ⇒ error with reason timeout', async () => {
    const { exitCode, result } = await run(['run-next', '--stage', 'plan'], {
      routes: { 'cron ls': json([job({ id: 'b' })]), 'cron run-now b': { timeout: true } },
    })
    assert.equal(exitCode, 0)
    assert.equal(`${result.action}/${result.reason}`, 'error/timeout')
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
      ['next', '--stage', 'plan'],
      ['run-next', '--stage', 'build'],
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
    for (const argv of [
      ['next', '--stage', 'plan'],
      ['run-next', '--stage', 'build'],
      ['arm-verify', '--pr', '1'],
    ]) {
      const noRepo = await run(argv, { env: managedEnv({ BOSS_REPO_ID: '' }) })
      assert.equal(noRepo.exitCode, 0)
      assert.equal(`${noRepo.result.action}/${noRepo.result.reason}`, 'skipped/no-repo-id')
      assert.equal(noRepo.boss.calls.length, 0)
    }
  })

  it('--dry-run prints the argv and issues no boss call', async () => {
    for (const argv of [
      ['next', '--stage', 'plan', '--dry-run'],
      ['run-next', '--stage', 'plan', '--dry-run'],
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
      ['next'],
      ['next', '--stage', 'deploy'],
      ['run-next', '--stage'],
      ['arm-verify'],
      ['arm-verify', '--pr', 'abc'],
      ['arm-verify', '--pr', '0'],
      ['phase'],
      ['phase', 'a', 'b'],
      ['phase', 'x', '--stage', 'plan'],
      ['next', '--stage', 'plan', '--pr', '1'],
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
