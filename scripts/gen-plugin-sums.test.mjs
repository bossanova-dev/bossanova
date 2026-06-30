import { createHash } from 'node:crypto'
import assert from 'node:assert'
import test from 'node:test'
import { pluginSumLines } from './gen-plugin-sums.mjs'

test('hashes only non-platform-suffixed plugin binaries, sorted', () => {
  const files = {
    'bossd-plugin-claude': Buffer.from('claude-bin'),
    'bossd-plugin-repair': Buffer.from('repair-bin'),
    'bossd-plugin-claude-darwin-arm64': Buffer.from('cross'),
    'plugins.sum': Buffer.from('old'),
    bossd: Buffer.from('not-a-plugin'),
  }
  const listdir = () => Object.keys(files)
  const read = (p) => files[p.split('/').pop()]
  const lines = pluginSumLines('/fake', listdir, read)
  assert.equal(lines.length, 2)
  const claudeHash = createHash('sha256').update(files['bossd-plugin-claude']).digest('hex')
  assert.equal(lines[0], `${claudeHash}  bossd-plugin-claude`)
  assert.ok(lines[1].endsWith('  bossd-plugin-repair'))
})

test('emits lines in the "<64-hex>  <basename>" shape loadPluginSums parses', () => {
  const files = { 'bossd-plugin-codex': Buffer.from('codex-bin') }
  const lines = pluginSumLines(
    '/fake',
    () => Object.keys(files),
    (p) => files[p.split('/').pop()],
  )
  assert.equal(lines.length, 1)
  // Mirror the parser in lib/bossalib/config/pluginsum.go: fields split on
  // whitespace, exactly two, first field exactly 64 lowercase-hex chars.
  const fields = lines[0].split(/\s+/)
  assert.equal(fields.length, 2)
  assert.match(fields[0], /^[0-9a-f]{64}$/)
  assert.equal(fields[1], 'bossd-plugin-codex')
})

test('returns no lines when the directory has no plugin binaries', () => {
  const files = { bossd: Buffer.from('x'), 'plugins.sum': Buffer.from('old') }
  const lines = pluginSumLines(
    '/fake',
    () => Object.keys(files),
    (p) => files[p.split('/').pop()],
  )
  assert.deepEqual(lines, [])
})
