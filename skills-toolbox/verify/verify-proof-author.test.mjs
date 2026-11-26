import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  lastJsonObject,
  policyAllows,
  proofAuthorPromptCandidates,
  proofAuthorPromptPath,
  proofAuthorTimeoutMs,
  readProofPolicies,
  runProofAuthor,
} from './verify-proof-author.mjs'

const HEAD = 'c'.repeat(40)

const FAKE = `#!${process.execPath}
const fs = require('node:fs')
fs.writeFileSync(process.env.FAKE_LOG, JSON.stringify({ argv: process.argv.slice(2), cwd: process.cwd() }))
const spec = (head) => JSON.stringify({ schema: 'proof-spec/1', head, title: 'Adds {braces}' }, null, 2)
const mode = process.env.FAKE_AUTHOR
if (mode === 'hang') setInterval(() => {}, 1000)
else if (mode === 'prose') process.stdout.write('I could not finish the spec.\\n')
else if (mode === 'other') process.stdout.write(spec('d'.repeat(40)))
else if (mode === 'auth') { process.stderr.write('Invalid API key\\n'); process.exit(1) }
else process.stdout.write('Draft: {"schema": "x"}\\nFinal:\\n\`\`\`json\\n' + spec(process.env.FAKE_HEAD) + '\\n\`\`\`\\n')
`

function fixture(t, mode, extraEnv = {}) {
  const dir = tempDir('verify-proof-author-', t)
  const bin = path.join(dir, 'claude')
  fs.writeFileSync(bin, FAKE, { mode: 0o755 })
  const cwd = path.join(dir, 'head')
  fs.mkdirSync(cwd)
  const log = path.join(dir, 'argv.json')
  const input = {
    bundle: path.join(dir, 'bundle.json'),
    media: path.join(dir, 'media.json'),
    specPath: path.join(dir, 'spec.json'),
    headSha: HEAD,
    cwd,
    env: {
      PATH: process.env.PATH,
      BOSS_CLAUDE_BIN: bin,
      FAKE_AUTHOR: mode,
      FAKE_HEAD: HEAD,
      FAKE_LOG: log,
      ...extraEnv,
    },
  }
  return { input, log }
}

test('the author prints a spec for the head and it is written to the spec path', async (t) => {
  const { input, log } = fixture(t, 'spec')
  const result = await runProofAuthor(input)
  assert.deepEqual(result, { specPath: input.specPath })
  const written = JSON.parse(fs.readFileSync(input.specPath, 'utf8'))
  assert.deepEqual(written, { schema: 'proof-spec/1', head: HEAD, title: 'Adds {braces}' })
  const ran = JSON.parse(fs.readFileSync(log, 'utf8'))
  assert.equal(fs.realpathSync(ran.cwd), fs.realpathSync(input.cwd))
  assert.deepEqual(ran.argv.slice(0, 3), ['-p', '--permission-mode', 'plan'])
  const prompt = ran.argv.at(-1)
  for (const handed of [input.bundle, input.media, input.cwd, HEAD])
    assert.ok(prompt.includes(handed), `the prompt hands the agent ${handed}`)
})

test('an author that never finishes is stopped at the timeout and writes no spec', async (t) => {
  const { input } = fixture(t, 'hang', { BOSS_PROOF_AUTHOR_TIMEOUT_MS: '300' })
  assert.deepEqual(await runProofAuthor(input), { skipped: 'timeout' })
  assert.equal(fs.existsSync(input.specPath), false)
})

test('output with no spec, a spec for another head and an auth failure each skip', async (t) => {
  for (const [mode, skipped] of [
    ['prose', 'no-json'],
    ['other', 'other-head'],
    ['auth', 'agent-auth'],
  ]) {
    const { input } = fixture(t, mode)
    assert.deepEqual(await runProofAuthor(input), { skipped }, mode)
    assert.equal(fs.existsSync(input.specPath), false, `${mode} writes no spec`)
  }
})

test('no agent binary and no prompt skip before anything runs', async (t) => {
  const { input, log } = fixture(t, 'spec')
  const missing = { ...input, env: { ...input.env, BOSS_CLAUDE_BIN: '/nonexistent/claude' } }
  assert.deepEqual(await runProofAuthor(missing), { skipped: 'no-agent' })
  assert.deepEqual(await runProofAuthor({ ...input, promptPath: '/nonexistent/proof-author.md' }), {
    skipped: 'no-prompt',
  })
  assert.equal(fs.existsSync(log), false)
})

test('the prompt resolves from the source tree', () => {
  const prompt = proofAuthorPromptPath()
  assert.ok(prompt && fs.statSync(prompt).isFile())
})

test('a copy installed in another core finds the prompt in the sibling boss-verify core', (t) => {
  const root = tempDir('proof-author-installed-', t)
  const prompt = path.join(root, 'boss-verify', 'references', 'proof-author.md')
  fs.mkdirSync(path.dirname(prompt), { recursive: true })
  fs.writeFileSync(prompt, '# prompt')
  for (const core of ['boss-build', 'boss-verify'])
    assert.equal(
      proofAuthorPromptPath(
        proofAuthorPromptCandidates(path.join(root, core, 'toolbox', 'verify')),
      ),
      prompt,
    )
  fs.rmSync(prompt)
  assert.equal(
    proofAuthorPromptPath(
      proofAuthorPromptCandidates(path.join(root, 'boss-build', 'toolbox', 'verify')),
    ),
    null,
  )
})

test('the last JSON object wins, and braces inside strings do not end it', () => {
  assert.deepEqual(lastJsonObject('a {"x": 1} then {"y": "}{", "z": {"w": 2}} done'), {
    y: '}{',
    z: { w: 2 },
  })
  assert.equal(lastJsonObject('no object {here'), null)
  assert.equal(lastJsonObject('[1, 2]'), null)
})

test('the timeout override takes plain positive digits only', () => {
  const fallback = proofAuthorTimeoutMs({})
  assert.equal(proofAuthorTimeoutMs({ BOSS_PROOF_AUTHOR_TIMEOUT_MS: '120000' }), 120000)
  for (const raw of ['0', '1.5', '10abc', '-5', ''])
    assert.equal(proofAuthorTimeoutMs({ BOSS_PROOF_AUTHOR_TIMEOUT_MS: raw }), fallback, raw)
})

test('the policies come from the trusted base tree and default to all', () => {
  const trusted = () => ({ ok: true, root: '/trusted' })
  const read = (proofDefaults) =>
    readProofPolicies({
      baseSha: HEAD,
      trustedTree: trusted,
      loadConfig: () => ({ proofDefaults }),
    })
  assert.deepEqual(read({ author: 'parked', pages: 'all' }), { pages: 'all', author: 'parked' })
  assert.deepEqual(read({ author: 'none' }), { pages: 'all', author: 'none' })
  assert.deepEqual(read({ author: 'sometimes' }), { pages: 'all', author: 'all' })
  assert.deepEqual(
    readProofPolicies({
      baseSha: HEAD,
      trustedTree: () => ({ ok: false }),
      loadConfig: () => ({ proofDefaults: { author: 'none' } }),
    }),
    { pages: 'all', author: 'all' },
  )
  assert.equal(policyAllows('all', 'pass'), true)
  assert.equal(policyAllows('parked', 'human'), true)
  assert.equal(policyAllows('parked', 'pass'), false)
  assert.equal(policyAllows('none', 'human'), false)
})
