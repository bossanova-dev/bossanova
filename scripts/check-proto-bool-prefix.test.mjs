#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import {
  checkProtoBoolPrefixes,
  discoverProtoFiles,
  extractBoolFields,
} from './check-proto-bool-prefix.mjs'

function captureConsole(run) {
  const out = []
  const err = []
  const originalLog = console.log
  const originalError = console.error
  console.log = (...args) => out.push(args.join(' '))
  console.error = (...args) => err.push(args.join(' '))
  try {
    const result = run()
    return { result, out, err }
  } finally {
    console.log = originalLog
    console.error = originalError
  }
}

function makeRepo() {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-proto-bool-prefix-'))
  fs.mkdirSync(path.join(repoRoot, 'proto', 'bossanova', 'v1'), { recursive: true })
  fs.mkdirSync(path.join(repoRoot, 'scripts'), { recursive: true })
  writeAllowlist(repoRoot, [])
  return repoRoot
}

function writeProto(repoRoot, name, contents) {
  const protoPath = path.join(repoRoot, 'proto', 'bossanova', 'v1', name)
  fs.writeFileSync(protoPath, contents)
  return protoPath
}

function writeAllowlist(repoRoot, allowed) {
  fs.writeFileSync(
    path.join(repoRoot, 'scripts', 'proto-bool-prefix-allowlist.json'),
    `${JSON.stringify({ allowed }, null, 2)}\n`,
  )
}

test('extractBoolFields reads bool field declarations and ignores comments and map values', () => {
  const fields = extractBoolFields(
    [
      'message Example {',
      '  bool is_ready = 1;',
      '  optional bool stale = 2;',
      '  // bool commented = 3;',
      '  /* bool blocked = 4; */',
      '  map<string, bool> flags = 5;',
      '}',
    ].join('\n'),
    'proto/bossanova/v1/example.proto',
  )

  assert.deepEqual(fields, [
    {
      file: 'proto/bossanova/v1/example.proto',
      line: 2,
      message: 'Example',
      name: 'is_ready',
      key: 'proto/bossanova/v1/example.proto:Example.is_ready',
    },
    {
      file: 'proto/bossanova/v1/example.proto',
      line: 3,
      message: 'Example',
      name: 'stale',
      key: 'proto/bossanova/v1/example.proto:Example.stale',
    },
  ])
})

test('checkProtoBoolPrefixes passes conforming fields', () => {
  const repoRoot = makeRepo()
  writeProto(repoRoot, 'example.proto', 'message Example {\n  bool is_ready = 1;\n}\n')

  const { result, out } = captureConsole(() => checkProtoBoolPrefixes(repoRoot))

  assert.equal(result, true)
  assert.match(
    out.join('\n'),
    /Proto bool prefixes OK \(1 bool fields checked, 0 legacy fields allowlisted\)/,
  )
})

test('checkProtoBoolPrefixes fails non-conforming unallowlisted fields with file, line and name', () => {
  const repoRoot = makeRepo()
  writeProto(repoRoot, 'example.proto', 'message Example {\n  bool ready = 1;\n}\n')

  const { result, err } = captureConsole(() => checkProtoBoolPrefixes(repoRoot))

  assert.equal(result, false)
  assert.match(
    err.join('\n'),
    /proto\/bossanova\/v1\/example\.proto:2: bool field "Example\.ready" must start with/,
  )
})

test('checkProtoBoolPrefixes accepts allowlisted legacy fields', () => {
  const repoRoot = makeRepo()
  writeProto(repoRoot, 'example.proto', 'message Example {\n  bool ready = 1;\n}\n')
  writeAllowlist(repoRoot, ['proto/bossanova/v1/example.proto:Example.ready'])

  const { result } = captureConsole(() => checkProtoBoolPrefixes(repoRoot))

  assert.equal(result, true)
})

test('checkProtoBoolPrefixes fails stale allowlist entries', () => {
  const repoRoot = makeRepo()
  writeProto(repoRoot, 'example.proto', 'message Example {\n  bool is_ready = 1;\n}\n')
  writeAllowlist(repoRoot, ['proto/bossanova/v1/example.proto:Example.ready'])

  const { result, err } = captureConsole(() => checkProtoBoolPrefixes(repoRoot))

  assert.equal(result, false)
  assert.match(
    err.join('\n'),
    /proto-bool-prefix-allowlist\.json: stale entry proto\/bossanova\/v1\/example\.proto:Example\.ready/,
  )
})

test('checkProtoBoolPrefixes rejects duplicate allowlist entries', () => {
  const repoRoot = makeRepo()
  writeProto(repoRoot, 'example.proto', 'message Example {\n  bool ready = 1;\n}\n')
  writeAllowlist(repoRoot, [
    'proto/bossanova/v1/example.proto:Example.ready',
    'proto/bossanova/v1/example.proto:Example.ready',
  ])

  const { result, err } = captureConsole(() => checkProtoBoolPrefixes(repoRoot))

  assert.equal(result, false)
  assert.match(
    err.join('\n'),
    /proto-bool-prefix-allowlist\.json: duplicate entry proto\/bossanova\/v1\/example\.proto:Example\.ready/,
  )
})

test('discoverProtoFiles returns sorted v1 proto files and ignores other files', () => {
  const repoRoot = makeRepo()
  writeProto(repoRoot, 'b.proto', 'message B {}\n')
  writeProto(repoRoot, 'a.proto', 'message A {}\n')
  fs.writeFileSync(path.join(repoRoot, 'proto', 'bossanova', 'v1', 'notes.txt'), 'x\n')

  assert.deepEqual(
    discoverProtoFiles(repoRoot).map((file) => path.basename(file)),
    ['a.proto', 'b.proto'],
  )
})
