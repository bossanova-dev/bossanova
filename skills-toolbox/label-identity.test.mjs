import { test } from 'node:test'
import assert from 'node:assert/strict'
import { labelKey, resolveLabelIdentity } from './label-identity.mjs'

test('labelKey normalises case, whitespace, hyphens and underscores and is null-safe', () => {
  assert.equal(labelKey(null), '')
  assert.equal(labelKey(undefined), '')
  assert.equal(labelKey(''), '')
  assert.equal(labelKey('   -_-  '), '')
  assert.equal(labelKey('Feature'), 'feature')
  assert.equal(labelKey('Agent-Build'), 'agentbuild')
  assert.equal(labelKey('agent_build'), 'agentbuild')
  assert.equal(labelKey('  Agent  _ - Build  '), 'agentbuild')
})

test('resolveLabelIdentity returns the existing spelling for case and separator variants', () => {
  assert.equal(resolveLabelIdentity('Feature', ['feature']), 'feature')
  assert.equal(resolveLabelIdentity('agent_build', ['Agent-Build']), 'Agent-Build')
  assert.equal(resolveLabelIdentity('agent build', ['agent-build']), 'agent-build')
})

test('resolveLabelIdentity accepts {name} objects and mixed lists', () => {
  assert.equal(resolveLabelIdentity('feature', [{ name: 'FEATURE' }]), 'FEATURE')
  assert.equal(
    resolveLabelIdentity('agent_build', [
      null,
      undefined,
      {},
      { name: 42 },
      { name: 'Agent-Build' },
    ]),
    'Agent-Build',
  )
})

test('resolveLabelIdentity returns the first matching existing spelling in order', () => {
  assert.equal(resolveLabelIdentity('FEATURE', ['feature', 'Feature']), 'feature')
  assert.equal(
    resolveLabelIdentity('agent-build', [{ name: 'Agent_Build' }, 'agent-build']),
    'Agent_Build',
  )
})

test('resolveLabelIdentity returns null on no match, empty/non-array existing, or blank/non-string name', () => {
  assert.equal(resolveLabelIdentity('bug', ['feature', 'chore']), null)
  assert.equal(resolveLabelIdentity('feature', []), null)
  assert.equal(resolveLabelIdentity('feature', null), null)
  assert.equal(resolveLabelIdentity('feature', undefined), null)
  assert.equal(resolveLabelIdentity('feature', /** @type {any} */ ('feature')), null)
  assert.equal(resolveLabelIdentity('', ['feature']), null)
  assert.equal(resolveLabelIdentity('   ', ['feature']), null)
  assert.equal(resolveLabelIdentity('---___', ['---', 'feature']), null)
  assert.equal(resolveLabelIdentity(null, ['feature']), null)
  assert.equal(resolveLabelIdentity(undefined, ['feature']), null)
  assert.equal(resolveLabelIdentity(123, ['123']), null)
})
