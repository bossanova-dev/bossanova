import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { discoverExtensions } from '../skill-extensions.mjs'
import { MAX_LEG_TIMEOUT_MINUTES } from './verify-leg-marker.mjs'

function discover(markers) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-leg-marker-'))
  try {
    for (const [name, lines] of Object.entries(markers)) {
      const dir = path.join(root, '.claude/skills', name)
      fs.mkdirSync(dir, { recursive: true })
      const marker = ['extends: boss-verify', ...lines].map((line) => `  ${line}`).join('\n')
      fs.writeFileSync(
        path.join(dir, 'SKILL.md'),
        `---\nname: ${name}\nx-boss-extension:\n${marker}\n---\n# ${name}\n`,
      )
    }
    return discoverExtensions({ core: 'boss-verify', root, builtinDir: null })
  } finally {
    fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 })
  }
}

const byName = (list) => Object.fromEntries(list.map((entry) => [entry.name, entry]))

test('verify descriptors carry a declared timeout and ignored writes, and omit them otherwise', () => {
  const { extensions, skipped } = discover({
    'boss-verify-recorder': ['role: verify', 'timeout: 20', 'writes: ignored'],
    'boss-verify-quoted': ['role: verify', `timeout: "${MAX_LEG_TIMEOUT_MINUTES}"`],
    'boss-verify-plain': ['role: verify'],
  })
  assert.deepEqual(skipped, [])
  const got = byName(extensions)
  assert.equal(got['boss-verify-recorder'].timeoutMinutes, 20)
  assert.equal(got['boss-verify-recorder'].writes, 'ignored')
  assert.equal(got['boss-verify-quoted'].timeoutMinutes, MAX_LEG_TIMEOUT_MINUTES)
  assert.equal('timeoutMinutes' in got['boss-verify-plain'], false)
  assert.equal('writes' in got['boss-verify-plain'], false)
})

test('an unusable timeout or writes value skips the extension as invalid', () => {
  const { extensions, skipped } = discover({
    'boss-verify-zero': ['role: verify', 'timeout: 0'],
    'boss-verify-over': ['role: verify', `timeout: ${MAX_LEG_TIMEOUT_MINUTES + 1}`],
    'boss-verify-fraction': ['role: verify', 'timeout: 2.5'],
    'boss-verify-word': ['role: verify', 'timeout: soon'],
    'boss-verify-everything': ['role: verify', 'writes: all'],
  })
  assert.deepEqual(extensions, [])
  assert.deepEqual(Object.fromEntries(skipped.map((s) => [s.name, [s.code, s.deliberate]])), {
    'boss-verify-zero': ['invalid-timeout', false],
    'boss-verify-over': ['invalid-timeout', false],
    'boss-verify-fraction': ['invalid-timeout', false],
    'boss-verify-word': ['invalid-timeout', false],
    'boss-verify-everything': ['invalid-writes', false],
  })
})

test('the leg keys are inert on a role other than verify', () => {
  const { extensions, skipped } = discover({
    'boss-verify-describer': [
      'role: proof-section',
      'timeout: soon',
      'writes: all',
      'sections:',
      '    - id: notes',
      '      title: Notes',
    ],
  })
  assert.deepEqual(skipped, [])
  assert.equal(extensions.length, 1)
  assert.equal('timeoutMinutes' in extensions[0], false)
  assert.equal('writes' in extensions[0], false)
})
