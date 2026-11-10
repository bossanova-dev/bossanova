import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  MAX_MEDIA_OUTPUT_BYTES,
  collectProofMedia,
  pageAssets,
  readMediaDir,
  writeMediaManifest,
} from './verify-media-hook.mjs'
import { PLAYER_OBJECT } from './verify-publish-hook.mjs'

const HEAD = 'c'.repeat(40)
const BASE = 'd'.repeat(40)
const SECRET = 'media-SECRET-must-not-surface'

const manifest = (head = HEAD) => ({
  schema: 'proof-media/1',
  head,
  clips: [
    {
      id: 'head-0',
      side: 'head',
      title: 'A user signs up',
      file: 'head-0.mp4',
      poster: 'head-0.png',
      durationMs: 3_000,
      chapters: [{ atMs: 0, caption: 'Start' }],
      stills: [{ file: 'still.png', caption: 'The form' }],
    },
  ],
})

/** A trusted tree with one `proof-media` extension whose script is `script`. */
function tree(t, script, { name = 'boss-verify-media', extra = false } = {}) {
  const root = tempDir('media-tree-', t)
  const add = (ext, order) => {
    const dir = path.join(root, '.claude', 'skills', ext)
    fs.mkdirSync(dir, { recursive: true })
    fs.writeFileSync(
      path.join(dir, 'SKILL.md'),
      `---\nname: ${ext}\nx-boss-extension:\n  extends: boss-verify\n  role: proof-media\n  media: media.mjs\n  order: ${order}\n---\n`,
    )
    fs.writeFileSync(path.join(dir, 'media.mjs'), script)
  }
  add(name, 10)
  if (extra) add(`${name}-second`, 20)
  return root
}

const collect = (t, root, more = {}) =>
  collectProofMedia({
    repo: 'acme/widgets',
    pr: 7,
    headSha: HEAD,
    baseSha: BASE,
    dir: path.join(tempDir('media-out-', t), 'media'),
    cwd: root,
    read: () => ({ ok: false }),
    trustedTree: () => ({ ok: true, root }),
    ...more,
  })

const WRITES = `import fs from 'node:fs'
import path from 'node:path'
const envelope = JSON.parse(fs.readFileSync(0, 'utf8'))
for (const f of ['head-0.mp4', 'head-0.png', 'still.png']) fs.writeFileSync(path.join(envelope.evidenceDir, f), f)
console.error(${JSON.stringify(SECRET)})
process.stdout.write(JSON.stringify({ ...${JSON.stringify(manifest())}, head: envelope.headSha }))
`

test('the first proof-media extension by order fills the directory and its manifest is validated', (t) => {
  const outcome = collect(t, tree(t, WRITES, { extra: true }))
  assert.equal(outcome.reason, undefined)
  assert.equal(outcome.extension, 'boss-verify-media')
  assert.deepEqual(outcome.ignored, ['boss-verify-media-second'])
  assert.deepEqual(outcome.media.clips[0].stills, [{ file: 'still.png', caption: 'The form' }])
  assert.ok(!JSON.stringify(outcome).includes(SECRET), 'stderr is never kept')
  const out = path.dirname(outcome.dir)
  writeMediaManifest(outcome, out)
  const reread = readMediaDir(out, HEAD)
  assert.equal(reread.media.clips[0].file, 'media/head-0.mp4')
  assert.equal(readMediaDir(out, BASE).reason, 'invalid:captured for another head')
  assert.deepEqual(
    pageAssets(reread.media, out, 'player()').map((a) => a.name),
    ['media/head-0.mp4', 'media/head-0.png', 'media/still.png', PLAYER_OBJECT],
  )
})

test('the extension gets the envelope the contract names', (t) => {
  const seen = path.join(tempDir('media-seen-', t), 'envelope.json')
  const script = `import fs from 'node:fs'
fs.writeFileSync(${JSON.stringify(seen)}, fs.readFileSync(0, 'utf8'))
process.stdout.write('{}')
`
  const outcome = collect(t, tree(t, script))
  assert.equal(outcome.reason, 'invalid:not a proof-media/1 manifest')
  const envelope = JSON.parse(fs.readFileSync(seen, 'utf8'))
  assert.deepEqual(envelope, {
    role: 'proof-media',
    extension: 'boss-verify-media',
    repo: 'acme/widgets',
    pr: 7,
    headSha: HEAD,
    baseSha: BASE,
    evidenceDir: outcome.dir,
  })
})

test('every failure is a named reason and never media', (t) => {
  const cases = [
    ['exit:4', 'process.exit(4)\n'],
    ['not-json', `process.stdout.write(${JSON.stringify(SECRET)})\n`],
    ['oversize', `process.stdout.write('x'.repeat(${MAX_MEDIA_OUTPUT_BYTES + 1}))\n`],
    [
      'invalid:captured for another head',
      `process.stdout.write(JSON.stringify(${JSON.stringify(manifest(BASE))}))\n`,
    ],
  ]
  for (const [reason, script] of cases) {
    const outcome = collect(t, tree(t, script))
    assert.equal(outcome.reason, reason)
    assert.equal(outcome.media, null)
    assert.ok(!JSON.stringify(outcome).includes(SECRET), reason)
  }
  const slow = collect(t, tree(t, 'setTimeout(() => {}, 60_000)\n'), { timeoutMs: 200 })
  assert.ok(['timeout', 'signal'].includes(slow.reason), slow.reason)
  const none = collect(t, tempDir('media-empty-', t))
  assert.equal(none.reason, 'not-configured')
  const untrusted = collect(t, tree(t, WRITES), { trustedTree: () => ({ ok: false }) })
  assert.equal(untrusted.reason, 'trusted-tree-unavailable')
})

test('a page without a head clip needs no player', () => {
  const base = manifest()
  base.clips = []
  assert.deepEqual(pageAssets(base, '/evidence', 'player()'), [])
  assert.deepEqual(pageAssets(null, '/evidence', 'player()'), [])
})
