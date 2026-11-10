import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdirSync, rmSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { DEFAULT_CONFIG } from '../skill-config.mjs'
import {
  parseTargetRepoDeclaration,
  readOriginIdentity,
  resolveTargetContext,
  targetModuleRoots,
  targetRepoNotResolvedAdvisory,
} from './plan-target-repo.mjs'

const parse = (text) => parseTargetRepoDeclaration(DEFAULT_CONFIG, text)

test('target declaration accepts tracker bullets and repository identity spellings', () => {
  for (const value of [
    'Acme/Widgets',
    '`acme/widgets`',
    'https://github.com/ACME/widgets.git',
    'git@github.com:acme/widgets.git',
  ]) {
    for (const marker of ['-', '*']) {
      assert.deepEqual(parse(`## Planning\n${marker} Target repo: ${value}\n`), {
        declared: true,
        repo: 'acme/widgets',
      })
    }
  }
  assert.deepEqual(parse('## Planning\n- Target repo: acme/widgets\n* Target repo: ACME/WIDGETS'), {
    declared: true,
    repo: 'acme/widgets',
  })
})

test('target declaration ignores other sections, fences and original reporter notes', () => {
  assert.deepEqual(parse('- Target repo: acme/widgets\n## Summary\n- Target repo: acme/widgets'), {
    declared: false,
  })
  assert.deepEqual(
    parse(
      '## Planning\n```md\n- Target repo: acme/widgets\n```\n## Original notes\n## Planning\n- Target repo: acme/widgets',
    ),
    { declared: false },
  )
  for (const body of [
    '- Target repo: invalid',
    '- Target repo: ',
    '- Target repo: acme/widgets\n- Target repo: acme/other',
  ]) {
    const result = parse(`## Planning\n${body}`)
    assert.equal(result.declared, true)
    assert.ok(result.error)
  }
})

test('target context only admits a foreign root with the declared origin', () => {
  const local = '/checkout'
  const target = '/target'
  const readOrigin = (root) =>
    root === local ? 'https://github.com/acme/local.git' : 'git@github.com:ACME/Widgets.git'
  const context = (declaration, targetRoot) =>
    resolveTargetContext({ declaration, checkoutRoot: local, targetRoot, readOrigin })
  assert.deepEqual(context({ declared: false }), { kind: 'local', root: local })
  assert.deepEqual(context({ declared: true, repo: 'acme/local' }), { kind: 'local', root: local })
  assert.equal(context({ declared: true, error: 'bad' }).couldNotEvaluate, 'bad')
  assert.equal(context({ declared: true, repo: 'acme/widgets' }).root, null)
  assert.deepEqual(context({ declared: true, repo: 'acme/widgets' }, target), {
    kind: 'foreign',
    repo: 'acme/widgets',
    root: target,
  })
  const mismatch = context({ declared: true, repo: 'acme/elsewhere' }, target)
  assert.equal(mismatch.root, null)
  assert.match(mismatch.reason, /acme\/widgets/)
  assert.equal(targetRepoNotResolvedAdvisory(mismatch).code, 'advisory: target-repo-not-resolved')
  const unknown = resolveTargetContext({
    declaration: { declared: true, repo: 'acme/widgets' },
    checkoutRoot: local,
    readOrigin: () => null,
  })
  assert.equal(unknown.kind, 'local')
  assert.ok(unknown.couldNotEvaluate)
  const unreadableTarget = resolveTargetContext({
    declaration: { declared: true, repo: 'acme/widgets' },
    checkoutRoot: local,
    targetRoot: target,
    readOrigin: (root) => (root === local ? 'acme/local' : null),
  })
  assert.equal(unreadableTarget.root, null)
  assert.match(unreadableTarget.reason, /unreadable/)
})

test('origin and target module discovery read real repository fixtures', () => {
  const root = tempDir('target-repo-')
  try {
    assert.equal(readOriginIdentity(root), null)
    execFileSync('git', ['init', '-q', root])
    execFileSync('git', ['-C', root, 'remote', 'add', 'origin', 'git@github.com:ACME/Widgets.git'])
    assert.equal(readOriginIdentity(root), 'acme/widgets')
    mkdirSync(join(root, 'src'))
    writeFileSync(join(root, 'Makefile'), 'check:\n')
    assert.deepEqual(targetModuleRoots(root).sort(), ['Makefile', 'src'])
    assert.equal(
      readOriginIdentity(root, {
        exec: () => {
          throw new Error('no git')
        },
      }),
      null,
    )
  } finally {
    rmSync(root, { recursive: true, force: true, maxRetries: 5 })
  }
})
