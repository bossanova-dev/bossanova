#!/usr/bin/env node
import { tempDir } from './test-tmpdir.mjs'

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  ALLOWLIST_BASELINE,
  readAllowlist,
  scanExecWaitDelay,
  validateAllowlist,
} from './check-exec-waitdelay.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

function fixtureRoot(files) {
  const root = tempDir('check-exec-waitdelay-')
  for (const [name, contents] of Object.entries(files)) {
    const full = path.join(root, name)
    fs.mkdirSync(path.dirname(full), { recursive: true })
    fs.writeFileSync(full, contents)
  }
  return root
}

function scanFixture(files, allowlist = []) {
  return scanExecWaitDelay({ roots: [fixtureRoot(files)], allowlist })
}

test('violating captured stderr site is flagged', () => {
  const offenders = scanFixture({
    'pkg/run.go': [
      'package pkg',
      'import (',
      '  "bytes"',
      '  "os/exec"',
      ')',
      'func run() error {',
      '  cmd := exec.CommandContext(ctx, "tool")',
      '  var stderr bytes.Buffer',
      '  cmd.Stderr = &stderr',
      '  return cmd.Run()',
      '}',
      '',
    ].join('\n'),
  })

  assert.equal(offenders.length, 1)
  assert.equal(offenders[0].kind, 'missing-waitdelay')
  assert.equal(offenders[0].function, 'run')
})

test('WaitDelay present passes', () => {
  const offenders = scanFixture({
    'pkg/run.go': [
      'package pkg',
      'import "os/exec"',
      'func run() error {',
      '  cmd := exec.CommandContext(ctx, "tool")',
      '  cmd.Stderr = writer',
      '  cmd.WaitDelay = waitDelay',
      '  return cmd.Run()',
      '}',
      '',
    ].join('\n'),
  })

  assert.deepEqual(offenders, [])
})

test('WaitDelay on one command does not mask reused variable without WaitDelay', () => {
  const offenders = scanFixture({
    'pkg/run.go': [
      'package pkg',
      'import "os/exec"',
      'func run() error {',
      '  cmd := exec.CommandContext(ctx, "first")',
      '  cmd.Stderr = writer',
      '  cmd.WaitDelay = waitDelay',
      '  if err := cmd.Run(); err != nil {',
      '    return err',
      '  }',
      '  cmd = exec.CommandContext(ctx, "second")',
      '  cmd.Stderr = writer',
      '  return cmd.Run()',
      '}',
      '',
    ].join('\n'),
  })

  assert.equal(offenders.length, 1)
  assert.equal(offenders[0].kind, 'missing-waitdelay')
  assert.equal(offenders[0].line, 11)
})

test('os.Stdout assignment is not flagged', () => {
  const offenders = scanFixture({
    'pkg/run.go': [
      'package pkg',
      'import (',
      '  "os"',
      '  "os/exec"',
      ')',
      'func run() error {',
      '  cmd := exec.Command("tool")',
      '  cmd.Stdout = os.Stdout',
      '  return cmd.Run()',
      '}',
      '',
    ].join('\n'),
  })

  assert.deepEqual(offenders, [])
})

test('allowlisted site passes', () => {
  const root = fixtureRoot({
    'pkg/run.go': [
      'package pkg',
      'import "os/exec"',
      'func run() error {',
      '  cmd := exec.Command("tool")',
      '  cmd.Stderr = writer',
      '  return cmd.Run()',
      '}',
      '',
    ].join('\n'),
  })
  const allowlist = [
    {
      file: path.relative(process.cwd(), path.join(root, 'pkg/run.go')).split(path.sep).join('/'),
      function: 'run',
      variable: 'cmd',
      reason: 'fixture deliberately exercises the allowlist',
    },
  ]
  const offenders = scanExecWaitDelay({ roots: [root], allowlist })

  assert.deepEqual(offenders, [])
})

test('empty allowlist reason is rejected', () => {
  const errors = validateAllowlist([
    { file: 'pkg/run.go', function: 'run', variable: 'cmd', reason: '  ' },
  ])

  assert.equal(errors.length, 1)
  assert.match(errors[0], /empty reason/)
})

test('a stale entry in the exec-waitdelay baseline file fails the gate', () => {
  const root = fixtureRoot({
    'pkg/run.go': [
      'package pkg',
      'import "os/exec"',
      'func run() error {',
      '  cmd := exec.Command("tool")',
      '  cmd.Stderr = writer',
      '  cmd.WaitDelay = time.Second',
      '  return cmd.Run()',
      '}',
      '',
    ].join('\n'),
  })
  const file = path.relative(REPO_ROOT, path.join(root, 'pkg/run.go')).split(path.sep).join('/')
  const baselineRoot = fixtureRoot({
    [ALLOWLIST_BASELINE]: `${JSON.stringify([`${file}:run:cmd — lacked WaitDelay once`], null, 2)}\n`,
  })
  const { allowlist, errors } = readAllowlist(baselineRoot)
  assert.deepEqual(errors, [])
  assert.deepEqual(allowlist, [
    { file, function: 'run', variable: 'cmd', reason: 'lacked WaitDelay once' },
  ])

  const offenders = scanExecWaitDelay({ roots: [root], allowlist })
  assert.equal(offenders.length, 1)
  assert.equal(offenders[0].kind, 'stale')
  assert.match(offenders[0].message, /run\.go:run:cmd no longer matches a captured-output site/)

  // A run over a root that does not hold the entry's file leaves the entry alone.
  assert.deepEqual(scanExecWaitDelay({ roots: [fixtureRoot({})], allowlist }), [])
})

test('an exec-waitdelay baseline entry without a reason is rejected', () => {
  const baselineRoot = fixtureRoot({
    [ALLOWLIST_BASELINE]: `${JSON.stringify(['pkg/run.go:run:cmd', 'pkg/run.go:cmd — no function'], null, 2)}\n`,
  })
  const { allowlist, errors } = readAllowlist(baselineRoot)
  assert.match(errors.join('\n'), /pkg\/run\.go:cmd — no function.*must read/)
  assert.match(validateAllowlist(allowlist).join('\n'), /pkg\/run\.go:run:cmd has an empty reason/)
})

test('unresolvable command construction is flagged rather than passed', () => {
  const offenders = scanFixture({
    'pkg/run.go': [
      'package pkg',
      'func run() error {',
      '  cmd := commandFactory(ctx, "tool")',
      '  cmd.Stderr = writer',
      '  return cmd.Run()',
      '}',
      '',
    ].join('\n'),
  })

  assert.equal(offenders.length, 1)
  assert.equal(offenders[0].kind, 'unresolved')
})

test('test and generated files are skipped', () => {
  const offenders = scanFixture({
    'pkg/run_test.go': [
      'package pkg',
      'func run() error {',
      '  cmd := commandFactory(ctx, "tool")',
      '  cmd.Stderr = writer',
      '  return cmd.Run()',
      '}',
      '',
    ].join('\n'),
    'pkg/gen.go': [
      '// Code generated by fixture. DO NOT EDIT.',
      'package pkg',
      'func run() error {',
      '  cmd := commandFactory(ctx, "tool")',
      '  cmd.Stderr = writer',
      '  return cmd.Run()',
      '}',
      '',
    ].join('\n'),
  })

  assert.deepEqual(offenders, [])
})
