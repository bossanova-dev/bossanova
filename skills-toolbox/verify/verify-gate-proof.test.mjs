// End to end through the real verbs: a real repository whose base commit carries a stub
// proof-publish hook and a stub proof-media extension, a real trusted-tree extraction and real
// extension processes. Only gh and boss are faked.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { commandReader, main, materializeTrustedTree } from './verify-gate.mjs'
import { NO_UI_RECORDING } from './verify-proof-page-model.mjs'
import { proofPlayerScript } from './verify-proof-player.mjs'
import { parseProofLine } from './verify-proof.mjs'
import { PAGE_OBJECT, PLAYER_OBJECT } from './verify-publish-hook.mjs'

const PUBLISH = `import fs from 'node:fs'
import path from 'node:path'
const envelope = JSON.parse(fs.readFileSync(0, 'utf8'))
if (process.env.STUB_FAIL) process.exit(3)
const out = path.join(process.env.STUB_PROOF_OUT, envelope.prefix)
for (const object of envelope.objects) {
  fs.mkdirSync(path.dirname(path.join(out, object.name)), { recursive: true })
  fs.copyFileSync(path.join(envelope.evidenceDir, object.path), path.join(out, object.name))
}
fs.appendFileSync(path.join(process.env.STUB_PROOF_OUT, 'calls.jsonl'), JSON.stringify(envelope) + '\\n')
console.error('upload chatter that must not be kept')
process.stdout.write(JSON.stringify({ ok: true, extension: envelope.extension, role: 'proof-publish', baseUrl: 'https://proof.example/' }))
`

// Writes one clip, its still and a symlink to a file outside the evidence directory, then prints
// a manifest naming all three; the symlinked still must never be published.
const MEDIA = `import fs from 'node:fs'
import path from 'node:path'
const envelope = JSON.parse(fs.readFileSync(0, 'utf8'))
console.error('download chatter that must not be kept')
if (process.env.STUB_NO_MEDIA) process.exit(0)
const dir = envelope.evidenceDir
fs.writeFileSync(path.join(dir, 'head-0.mp4'), 'synthetic video')
fs.writeFileSync(path.join(dir, 'head-0-still-0.png'), 'synthetic still')
fs.symlinkSync(process.env.STUB_SECRET_FILE, path.join(dir, 'leak.png'))
process.stdout.write(JSON.stringify({
  schema: 'proof-media/1',
  head: envelope.headSha,
  clips: [{
    id: 'head-0', side: 'head', title: 'A user signs up', file: 'head-0.mp4', durationMs: 4000,
    chapters: [{ atMs: 500, caption: 'The form opens' }],
    stills: [
      { file: 'head-0-still-0.png', caption: 'The form', atMs: 900 },
      { file: 'leak.png', caption: 'Elsewhere' },
    ],
  }],
}))
`

function extension(root, name, role, key, script) {
  const dir = path.join(root, '.claude', 'skills', name)
  fs.mkdirSync(dir, { recursive: true })
  fs.writeFileSync(
    path.join(dir, 'SKILL.md'),
    `---\nname: ${name}\nx-boss-extension:\n  extends: boss-verify\n  role: ${role}\n  ${key}: run.mjs\n---\n\n# ${name}\n`,
  )
  fs.writeFileSync(path.join(dir, 'run.mjs'), script)
}

/** A repository with stub hooks on its base and, on the head only, hooks that must never run. */
function repository(t) {
  const repo = tempDir('verify-proof-repo-', t)
  const git = (...args) =>
    execFileSync('git', ['-c', 'commit.gpgsign=false', ...args], {
      cwd: repo,
      encoding: 'utf8',
    }).trim()
  git('init', '-q')
  git('config', 'user.email', 'verify@example.com')
  git('config', 'user.name', 'verify')
  extension(repo, 'boss-verify-stub-host', 'proof-publish', 'publish', PUBLISH)
  extension(repo, 'boss-verify-stub-media', 'proof-media', 'media', MEDIA)
  fs.mkdirSync(path.join(repo, 'proto'))
  fs.writeFileSync(path.join(repo, 'proto', 'api.proto'), 'syntax = "proto3";\n')
  fs.mkdirSync(path.join(repo, 'web'))
  fs.writeFileSync(path.join(repo, 'web', 'form.tsx'), 'export const Form = () => null\n')
  git('add', '-A')
  git('commit', '-qm', 'base')
  const base = git('rev-parse', 'HEAD')
  const ran =
    "import fs from 'node:fs'\nfs.writeFileSync(process.env.STUB_PROOF_OUT + '/HEAD_HOOK_RAN', '')\n"
  extension(repo, 'boss-verify-head-host', 'proof-publish', 'publish', ran)
  extension(repo, 'boss-verify-head-media', 'proof-media', 'media', ran)
  fs.appendFileSync(path.join(repo, 'proto', 'api.proto'), 'message User { string email = 1; }\n')
  fs.writeFileSync(
    path.join(repo, 'web', 'form.tsx'),
    'export const Form = () => (\n  <form>\n    <input name="email" />\n    <button>Sign up</button>\n  </form>\n)\n',
  )
  git('add', '-A')
  git('commit', '-qm', 'Add the sign-up form')
  return { repo, base, head: git('rev-parse', 'HEAD') }
}

function world({ repo, base, head }) {
  const writes = []
  const real = commandReader({ cwd: repo })
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  const read = (command, args, opts) => {
    if (command === 'boss' && args[0] === 'pr') return ok({ head_sha: head, checks: [] })
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'view')
      return ok({
        headRefOid: head,
        state: 'OPEN',
        isDraft: false,
        title: 'Add the sign-up form',
        body: 'Tracker issue: https://tracker.example/issue/ACME-1\n\nAdds a user.\n\n## After merge\n\n- [ ] Regenerate the client',
        url: 'https://github.com/acme/widgets/pull/7',
        baseRefName: 'main',
        statusCheckRollup: [{ name: 'test', status: 'COMPLETED', conclusion: 'SUCCESS' }],
      })
    if (command === 'gh' && args[0] === 'api') {
      const route = args[1]
      if (route === 'user') return ok({ login: 'verify' })
      if (route.includes('/statuses')) return ok([[]])
      if (route.includes('/comments?')) return ok([[]])
      if (route.includes('/permission')) return ok({ permission: 'write' })
      if (route.includes('/contents/'))
        return { ok: false, payload: { status: '404' }, error: 'HTTP 404', exit: 1, output: '' }
      if (route.endsWith('/commits/main')) return ok({ sha: base })
      if (route === 'repos/acme/widgets') return ok({ default_branch: 'main' })
      if (route === '--method') {
        const body = args.find((arg) => arg.startsWith('body='))?.slice(5)
        writes.push({ method: args[2], route: args[3], ...(body ? { body } : {}) })
        return ok({
          html_url: `https://github.com/acme/widgets/pull/7#issuecomment-${writes.length}`,
        })
      }
    }
    if (command === 'gh' || command === 'boss')
      throw new Error(`unexpected ${command} ${args.join(' ')}`)
    return real(command, args, opts)
  }
  return { writes, read }
}

const AREAS = [
  {
    area: 'api-contract',
    label: 'API contract',
    paths: ['proto/api.proto'],
    defaultAsk: 'always',
    change: 'additive',
    items: ['Adds message User'],
  },
  { area: 'ui', label: 'UI', paths: ['web/form.tsx'], defaultAsk: 'unless-proven', items: [] },
]

function judgeFile(dir, { head, base, verdict = 'human' }) {
  const file = path.join(dir, `judge-${verdict}.json`)
  fs.writeFileSync(
    file,
    JSON.stringify({
      verdict,
      reason: verdict === 'human' ? 'human-review:api-contract' : 'verified',
      ...(verdict === 'human' ? { reasons: ['human-review:api-contract'] } : {}),
      headSha: head,
      baseSha: base,
      confidence: {
        level: 'moderate',
        asks: [{ area: 'api-contract', label: 'API contract', source: 'builtin' }],
        reasons: ['Changes API contract. Reviewing this area would raise this.'],
        loweredBy: ['area-asks'],
        health: [],
      },
      facts: { areas: AREAS, receipt: { state: 'success' } },
    }),
  )
  return file
}

/** The spec a proof-author would write for this head, from the bundle it was handed. */
function authorSpec(bundle, head) {
  return {
    schema: 'proof-spec/1',
    head,
    title: 'Let a user sign up with an email address',
    risks: [
      {
        level: 'medium',
        title: 'The client needs regenerating',
        body: 'The `User` message is new.',
        label: 'API',
        at: { path: 'proto/api.proto', line: 2 },
      },
    ],
    areas: [
      {
        id: bundle.areas.find((a) => a.label === 'UI').id,
        name: 'Sign-up form',
        summary: 'A form with one email field.',
        facts: [{ text: 'One input, one button', flag: false }],
        snippets: [{ path: 'web/form.tsx', start: 1, end: 6, pinLine: 3, pinNote: 'The field.' }],
      },
    ],
    uncertain: ['Whether the form validates the address'],
  }
}

async function setup(t, env = {}) {
  const { repo, base, head } = repository(t)
  const out = tempDir('verify-proof-out-', t)
  const secret = path.join(tempDir('verify-proof-secret-', t), 'id.png')
  fs.writeFileSync(secret, 'outside the evidence directory')
  const w = world({ repo, base, head })
  const trees = tempDir('verify-proof-trees-', t)
  const deps = {
    read: w.read,
    cwd: repo,
    env: { ...process.env, STUB_PROOF_OUT: out, STUB_SECRET_FILE: secret, ...env },
    loadConfig: () => ({}),
    // The real extraction, into a root this test owns and removes.
    trustedTree: (opts) => materializeTrustedTree({ ...opts, tmpRoot: trees }),
    adapter: { applyIssueWrites: async () => ({ outcome: 'ok', issue: { labels: [] } }) },
  }
  return { repo, base, head, out, w, deps }
}

const post = (head, verdict, extra) =>
  [
    ['post', '--pr', '7', '--head', head, '--verdict', verdict, '--repo', 'acme/widgets'],
    verdict === 'human' ? ['--reason', 'human-review:api-contract'] : [],
    extra,
  ].flat()

const calls = (out) =>
  fs.existsSync(path.join(out, 'calls.jsonl'))
    ? fs
        .readFileSync(path.join(out, 'calls.jsonl'), 'utf8')
        .trim()
        .split('\n')
        .map((line) => JSON.parse(line))
    : []

test('a parked head gets the bundle, the author spec, its media and player, then its page @sessions.AC248601 @sessions.AC248607 @sessions.AC248608', async (t) => {
  const { head, base, out, w, deps } = await setup(t)
  const run = tempDir('verify-proof-run-', t)
  const judge = judgeFile(run, { head, base })
  const bundled = await main(
    [
      'proof-bundle',
      '--pr',
      '7',
      '--head',
      head,
      '--judge',
      judge,
      '--out',
      run,
      '--repo',
      'acme/widgets',
    ],
    deps,
  )
  assert.equal(bundled.written, true)
  assert.equal(w.writes.length, 0, 'proof-bundle never writes to the forge')
  const bundle = JSON.parse(fs.readFileSync(bundled.bundle, 'utf8'))
  assert.deepEqual(bundle.pr.title, 'Add the sign-up form')
  assert.deepEqual(
    bundle.files.filter((f) => !f.path.startsWith('.claude/')).map((f) => [f.path, f.adds]),
    [
      ['proto/api.proto', 1],
      ['web/form.tsx', 6],
    ],
  )
  assert.deepEqual(bundle.ci, [{ name: 'test', state: 'passed' }])
  assert.deepEqual(bundle.repoSettings, { canAutoMerge: false })
  assert.deepEqual(bundle.media.clips[0].stills, [{ caption: 'The form', atMs: 900 }])
  assert.deepEqual(bundle.media.dropped, [
    { at: 'clips[0].stills[1]', reason: 'file: leak.png is missing' },
  ])
  assert.ok(!JSON.stringify(bundle).includes('chatter'))
  fs.writeFileSync(bundled.spec, JSON.stringify(authorSpec(bundle, head)))

  const result = await main(
    post(head, 'human', [
      '--judge',
      judge,
      '--proof-spec',
      bundled.spec,
      '--proof-media',
      bundled.proofMedia,
    ]),
    deps,
  )
  const prefix = `acme/widgets/7/${head}/`
  const url = `https://proof.example/${prefix}${PAGE_OBJECT}`
  assert.equal(result.posted, true)
  assert.deepEqual(result.proof.page, { published: true, url, publisher: 'hook' })
  assert.deepEqual(
    calls(out).map((call) => call.objects.map((o) => [o.name, o.contentType])),
    [
      [
        ['media/head-0.mp4', 'video/mp4'],
        ['media/head-0-still-0.png', 'image/png'],
        [PLAYER_OBJECT, 'text/javascript; charset=utf-8'],
      ],
      [[PAGE_OBJECT, 'text/html; charset=utf-8']],
    ],
    'media and the player land before the page, which goes alone',
  )
  const published = (name) => fs.readFileSync(path.join(out, prefix, name), 'utf8')
  assert.equal(published('media/head-0.mp4'), 'synthetic video')
  assert.equal(published(PLAYER_OBJECT), proofPlayerScript())
  assert.equal(fs.existsSync(path.join(out, prefix, 'media/leak.png')), false)
  const page = published(PAGE_OBJECT)
  assert.ok(
    page.includes('<h1>Let a user sign up with an email address</h1>'),
    'the spec titles it',
  )
  assert.ok(page.includes('<video id="proof-video"'), 'the head clip plays')
  assert.ok(page.includes(`<script src="${PLAYER_OBJECT}" defer></script>`))
  assert.ok(page.includes("script-src 'self'"))
  assert.match(
    page,
    /<figure class="diff" id="f-web-form-tsx-[0-9a-f]{6}">.*<tr class="add pinned"><td class="n"><\/td><td class="n">3<\/td>/,
    'the spec snippet is quoted from git and pins its line',
  )
  assert.ok(!page.includes(NO_UI_RECORDING), 'the changed UI has a recording')
  assert.equal(fs.existsSync(path.join(out, 'HEAD_HOOK_RAN')), false, 'no head hook runs')
  const note = w.writes.find((x) => x.body?.includes('boss-proof-note'))
  assert.deepEqual(parseProofLine(note.body), { head, page: url, level: 'moderate' })
  assert.ok(!JSON.stringify(result).includes('chatter'))
})

test('the sweep path collects media itself and a missing spec leaves the deterministic page @sessions.AC248607 @sessions.AC248608', async (t) => {
  const { head, base, out, deps } = await setup(t)
  const judge = judgeFile(tempDir('verify-proof-run-', t), { head, base })
  const missing = path.join(out, 'no-spec.json')
  const result = await main(post(head, 'human', ['--judge', judge, '--proof-spec', missing]), deps)
  assert.equal(result.proof.page.published, true)
  assert.deepEqual(result.proof.media, { extension: 'boss-verify-stub-media', clips: 1 })
  const page = fs.readFileSync(path.join(out, `acme/widgets/7/${head}/${PAGE_OBJECT}`), 'utf8')
  assert.ok(
    page.includes('<li>spec: not a proof-spec/1 document</li>'),
    'the generation check names it',
  )
  assert.ok(page.includes('<video id="proof-video"'))
  assert.equal(calls(out).length, 2)
})

test('a changed UI area with no recording leads the page, which still gets the player first @sessions.AC248607', async (t) => {
  const { head, base, out, deps } = await setup(t, { STUB_NO_MEDIA: '1' })
  const judge = judgeFile(tempDir('verify-proof-run-', t), { head, base })
  const result = await main(post(head, 'human', ['--judge', judge]), deps)
  assert.deepEqual(result.proof.media, { reason: 'not-json' })
  assert.deepEqual(
    calls(out).map((call) => call.objects.map((o) => o.name)),
    [[PLAYER_OBJECT], [PAGE_OBJECT]],
  )
  const page = fs.readFileSync(path.join(out, `acme/widgets/7/${head}/${PAGE_OBJECT}`), 'utf8')
  assert.ok(
    page.includes(`<ol class="reasons"><li class="proof"><div><div class="t">${NO_UI_RECORDING}`),
  )
  assert.ok(page.includes(`<script src="${PLAYER_OBJECT}" defer></script>`))
  assert.ok(!page.includes('<video'), 'the player has no video to drive and does nothing for it')
})

test('a pass, blocked or defect head uploads nothing and notes the level with a null page @sessions.AC248606', async (t) => {
  const { head, base, out, w, deps } = await setup(t)
  const judge = judgeFile(tempDir('verify-proof-run-', t), { head, base, verdict: 'pass' })
  const result = await main(post(head, 'pass', ['--judge', judge]), deps)
  assert.equal(result.posted, true)
  assert.deepEqual(result.proof, {
    recorded: true,
    level: 'moderate',
    note: 'posted',
    page: { published: false, reason: 'not-parked' },
  })
  assert.deepEqual(calls(out), [], 'no hook ran')
  const note = w.writes.find((x) => x.body?.includes('boss-proof-note'))
  assert.deepEqual(parseProofLine(note.body), { head, page: null, level: 'moderate' })
})

test('a failing hook leaves the verdict posted and the note with a null page @sessions.AC248604', async (t) => {
  const { head, base, w, deps } = await setup(t, { STUB_FAIL: '1' })
  const judge = judgeFile(tempDir('verify-proof-run-', t), { head, base })
  const result = await main(post(head, 'human', ['--judge', judge]), deps)
  assert.equal(result.posted, true)
  assert.deepEqual(result.proof.page, { published: false, reason: 'hook-failed:exit:3' })
  const note = w.writes.find((x) => x.body?.includes('boss-proof-note'))
  assert.deepEqual(parseProofLine(note.body), { head, page: null, level: 'moderate' })
})
