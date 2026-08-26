import assert from 'node:assert/strict'

import { mutate, sectionRegion } from './gate-region-lib.mjs'

function requireText(name, value, fn) {
  if (typeof value !== 'string' || value.length === 0) {
    throw new Error(`prose-pin: ${fn} requires non-empty ${name}`)
  }
}

function requirePattern(pattern, fn) {
  if (!(pattern instanceof RegExp)) {
    throw new Error(`prose-pin: ${fn} requires a RegExp pattern`)
  }
}

function requireMutation(name, value, fn) {
  if (!value || typeof value !== 'object') {
    throw new Error(`prose-pin: ${fn} requires a ${name} mutation object`)
  }
  if (!(typeof value.find === 'string' || value.find instanceof RegExp)) {
    throw new Error(`prose-pin: ${fn} requires ${name}.find`)
  }
  if (typeof value.replacement !== 'string') {
    throw new Error(`prose-pin: ${fn} requires ${name}.replacement`)
  }
}

function fresh(pattern) {
  return new RegExp(pattern.source, pattern.flags.replaceAll('g', ''))
}

export function assertFalsifiable({ source, pattern, mutation, label = 'prose pin' } = {}) {
  requireText('source', source, 'assertFalsifiable')
  requirePattern(pattern, 'assertFalsifiable')
  requireMutation('mutation', mutation, 'assertFalsifiable')

  assert.match(source, fresh(pattern), `${label}: pin must match before falsification`)
  const mutated = mutate(source, mutation.find, mutation.replacement, `${label}: mutation`)
  assert.doesNotMatch(
    mutated,
    fresh(pattern),
    `${label}: pin survived its falsifying mutation, so it is vacuous`,
  )
}

export function assertProhibitionFires({
  source,
  pattern,
  violation,
  label = 'prose prohibition',
} = {}) {
  requireText('source', source, 'assertProhibitionFires')
  requirePattern(pattern, 'assertProhibitionFires')
  requireMutation('violation', violation, 'assertProhibitionFires')

  assert.doesNotMatch(source, fresh(pattern), `${label}: prohibition must hold before violation`)
  const planted = mutate(source, violation.find, violation.replacement, `${label}: violation`)
  assert.match(planted, fresh(pattern), `${label}: planted violation did not trip the prohibition`)
}

export function assertPolarityBounded({
  source,
  heading,
  rule,
  polarity,
  label = 'polarity-bound prose prohibition',
} = {}) {
  requireText('source', source, 'assertPolarityBounded')
  requireText('heading', heading, 'assertPolarityBounded')
  requirePattern(rule, 'assertPolarityBounded')
  requirePattern(polarity, 'assertPolarityBounded')

  const section = sectionRegion(source, heading, label)
  assert.match(section, fresh(rule), `${label}: bounded section must state the rule`)
  assert.match(section, fresh(polarity), `${label}: bounded section must state forbidden polarity`)
}
