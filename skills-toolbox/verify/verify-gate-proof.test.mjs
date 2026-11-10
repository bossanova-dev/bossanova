// End to end through `post`: a real repository whose base commit carries a stub proof-publish
// hook, a real trusted-tree extraction and a real hook process. Only gh and boss are faked.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { commandReader, main, materializeTrustedTree } from './verify-gate.mjs'
import { parseProofLine } from './verify-proof.mjs'
import { PAGE_OBJECT } from './verify-publish-hook.mjs'

const STUB = `import fs from 'node:fs'
import path from 'node:path'
const envelope = JSON.parse(fs.readFileSync(0, 'utf8'))
if (process.env.STUB_FAIL) process.exit(3)
const out = path.join(process.env.STUB_PROOF_OUT, envelope.prefix)
fs.mkdirSync(out, { recursive: true })
for (const object of envelope.objects)
  fs.copyFileSync(path.join(envelope.evidenceDir, object.path), path.join(out, object.name))
fs.writeFileSync(path.join(process.env.STUB_PROOF_OUT, 'envelope.json'), JSON.stringify(envelope))
console.error('upload chatter that must not be kept')
process.stdout.write(JSON.stringify({ ok: true, extension: envelope.extension, role: 'proof-publish', baseUrl: 'https://proof.example/' }))
`

function extension(root, name, script) {
  const dir = path.join(root, '.claude', 'skills', name)
  fs.mkdirSync(dir, { recursive: true })
  fs.writeFileSync(
    path.join(dir, 'SKILL.md'),
    `---\nname: ${name}\nx-boss-extension:\n  extends: boss-verify\n  role: proof-publish\n  publish: publish.mjs\n---\n\n# ${name}\n`,
  )
  fs.writeFileSync(path.join(dir, 'publish.mjs'), script)
}

/** A repository with a stub hook on its base and, on the head only, a hook that must never run. */
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
  extension(repo, 'boss-verify-stub-host', STUB)
  fs.mkdirSync(path.join(repo, 'proto'))
  fs.writeFileSync(path.join(repo, 'proto', 'api.proto'), 'syntax = "proto3";\n')
  git('add', '-A')
  git('commit', '-qm', 'base')
  const base = git('rev-parse', 'HEAD')
  extension(
    repo,
    'boss-verify-head-host',
    "import fs from 'node:fs'\nfs.writeFileSync(process.env.STUB_PROOF_OUT + '/HEAD_HOOK_RAN', '')\n",
  )
  fs.appendFileSync(path.join(repo, 'proto', 'api.proto'), 'message User { string email = 1; }\n')
  git('add', '-A')
  git('commit', '-qm', 'Add the user message')
  return { repo, base, head: git('rev-parse', 'HEAD') }
}

function world({ repo, head, comments = [] }) {
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
        title: 'Add the user message',
        body: 'Adds a user.\n\n## After merge\n\n- [ ] Regenerate the client',
        url: 'https://github.com/acme/widgets/pull/7',
        baseRefName: 'main',
      })
    if (command === 'gh' && args[0] === 'api') {
      const route = args[1]
      if (route === 'user') return ok({ login: 'verify' })
      if (route.includes('/statuses')) return ok([[]])
      if (route.includes('/comments?')) return ok([comments])
      if (route.includes('/permission')) return ok({ permission: 'write' })
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

function judgeFile(dir, { head, base }) {
  const file = path.join(dir, 'judge.json')
  fs.writeFileSync(
    file,
    JSON.stringify({
      verdict: 'pass',
      reason: 'verified',
      headSha: head,
      baseSha: base,
      confidence: {
        level: 'moderate',
        asks: [{ area: 'api-contract', label: 'API contract', source: 'builtin' }],
        reasons: ['Changes API contract. Reviewing this area would raise this.'],
        loweredBy: ['area-asks'],
        health: [],
      },
      facts: {
        areas: [
          {
            area: 'api-contract',
            label: 'API contract',
            paths: ['proto/api.proto'],
            defaultAsk: 'always',
            change: 'additive',
            items: ['Adds message User'],
          },
        ],
        receipt: { state: 'success' },
      },
    }),
  )
  return file
}

async function postPass(t, { env = {}, comments } = {}) {
  const { repo, base, head } = repository(t)
  const out = tempDir('verify-proof-out-', t)
  const w = world({ repo, head, comments })
  const trees = tempDir('verify-proof-trees-', t)
  const argv = ['post', '--pr', '7', '--head', head, '--verdict', 'pass', '--repo', 'acme/widgets']
  const result = await main([...argv, '--judge', judgeFile(out, { head, base })], {
    read: w.read,
    cwd: repo,
    env: { ...process.env, STUB_PROOF_OUT: out, ...env },
    loadConfig: () => ({}),
    // The real extraction, into a root this test owns and removes.
    trustedTree: (opts) => materializeTrustedTree({ ...opts, tmpRoot: trees }),
    adapter: { applyIssueWrites: async () => ({ outcome: 'ok', issue: { labels: [] } }) },
  })
  return { result, out, head, writes: w.writes }
}

test('post publishes the page through the base hook and notes the marker carrying it @sessions.AC248601', async (t) => {
  const { result, out, head, writes } = await postPass(t)
  const url = `https://proof.example/acme/widgets/7/${head}/index.html`
  assert.equal(result.posted, true)
  assert.deepEqual(result.proof, {
    recorded: true,
    level: 'moderate',
    note: 'posted',
    page: { published: true, url, publisher: 'hook' },
  })
  const page = fs.readFileSync(path.join(out, 'acme/widgets/7', head, PAGE_OBJECT), 'utf8')
  assert.match(page, /^<!doctype html>/)
  assert.ok(page.includes('+message User { string email = 1; }'), 'the asking area shows its diff')
  assert.ok(page.includes('Regenerate the client'), 'the after-merge item is listed')
  assert.equal(fs.existsSync(path.join(out, 'HEAD_HOOK_RAN')), false, 'the head hook never runs')
  const envelope = JSON.parse(fs.readFileSync(path.join(out, 'envelope.json'), 'utf8'))
  assert.equal(envelope.extension, 'boss-verify-stub-host')
  assert.equal(envelope.prefix, `acme/widgets/7/${head}/`)
  // The status first, then the one proof note: the verdict never waits on the page.
  assert.match(writes[0].route, /statuses/)
  const note = writes.find((w) => w.body?.includes('boss-proof-note'))
  assert.deepEqual(parseProofLine(note.body), { head, page: url, level: 'moderate' })
  assert.ok(!JSON.stringify(result).includes('upload chatter'))
})

test('a failing hook leaves the verdict posted and the note with a null page @sessions.AC248604', async (t) => {
  const { result, head, writes } = await postPass(t, { env: { STUB_FAIL: '1' } })
  assert.equal(result.posted, true)
  assert.deepEqual(result.proof.page, { published: false, reason: 'hook-failed:exit:3' })
  const note = writes.find((w) => w.body?.includes('boss-proof-note'))
  assert.deepEqual(parseProofLine(note.body), { head, page: null, level: 'moderate' })
})
