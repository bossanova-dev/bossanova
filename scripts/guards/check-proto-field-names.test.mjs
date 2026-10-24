#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import {
  checkProtoFieldNames,
  discoverProtoFiles,
  extractFields,
} from './check-proto-field-names.mjs'

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
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-proto-field-names-'))
  fs.mkdirSync(path.join(repoRoot, 'proto', 'bossanova', 'v1'), { recursive: true })
  fs.mkdirSync(path.join(repoRoot, 'scripts/guards'), { recursive: true })
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
    path.join(repoRoot, 'scripts/guards', 'proto-field-names-baseline.json'),
    `${JSON.stringify(allowed, null, 2)}\n`,
  )
}

test('extractFields reads bool field declarations and ignores comments and map values', () => {
  const fields = extractFields(
    [
      'message Example {',
      '  bool is_ready = 1;',
      '  optional bool stale = 2;',
      '  // bool commented = 3;',
      '  /* bool blocked = 4; */',
      '  map<string, bool> flags = 5;',
      '  option deprecated = 6;',
      '  reserved 7;',
      '}',
    ].join('\n'),
    'proto/bossanova/v1/example.proto',
  )

  assert.deepEqual(fields, [
    {
      file: 'proto/bossanova/v1/example.proto',
      line: 2,
      message: 'Example',
      type: 'bool',
      isRepeated: false,
      name: 'is_ready',
      key: 'proto/bossanova/v1/example.proto:Example.is_ready',
    },
    {
      file: 'proto/bossanova/v1/example.proto',
      line: 3,
      message: 'Example',
      type: 'bool',
      isRepeated: false,
      name: 'stale',
      key: 'proto/bossanova/v1/example.proto:Example.stale',
    },
  ])
})

test('checkProtoFieldNames passes conforming fields', () => {
  const repoRoot = makeRepo()
  test.after(() => fs.rmSync(repoRoot, { recursive: true, force: true, maxRetries: 3 }))
  writeProto(repoRoot, 'example.proto', 'message Example {\n  bool is_ready = 1;\n}\n')

  const { result, out } = captureConsole(() => checkProtoFieldNames(repoRoot))

  assert.equal(result, true)
  assert.match(out.join('\n'), /Proto field names OK \(1 fields checked, 0 entries baselined/)
})

test('checkProtoFieldNames fails non-conforming unallowlisted fields with file, line and name', () => {
  const repoRoot = makeRepo()
  test.after(() => fs.rmSync(repoRoot, { recursive: true, force: true, maxRetries: 3 }))
  writeProto(repoRoot, 'example.proto', 'message Example {\n  bool ready = 1;\n}\n')

  const { result, err } = captureConsole(() => checkProtoFieldNames(repoRoot))

  assert.equal(result, false)
  assert.match(
    err.join('\n'),
    /proto\/bossanova\/v1\/example\.proto:2: bool-prefix: "Example\.ready" must start with/,
  )
})

test('checkProtoFieldNames accepts allowlisted legacy fields', () => {
  const repoRoot = makeRepo()
  test.after(() => fs.rmSync(repoRoot, { recursive: true, force: true, maxRetries: 3 }))
  writeProto(repoRoot, 'example.proto', 'message Example {\n  bool ready = 1;\n}\n')
  writeAllowlist(repoRoot, ['proto/bossanova/v1/example.proto:Example.ready'])

  const { result } = captureConsole(() => checkProtoFieldNames(repoRoot))

  assert.equal(result, true)
})

test('checkProtoFieldNames fails stale allowlist entries', () => {
  const repoRoot = makeRepo()
  test.after(() => fs.rmSync(repoRoot, { recursive: true, force: true, maxRetries: 3 }))
  writeProto(repoRoot, 'example.proto', 'message Example {\n  bool is_ready = 1;\n}\n')
  writeAllowlist(repoRoot, ['proto/bossanova/v1/example.proto:Example.ready'])

  const { result, err } = captureConsole(() => checkProtoFieldNames(repoRoot))

  assert.equal(result, false)
  assert.match(err.join('\n'), /Example\.ready no longer occurs/)
})

test('checkProtoFieldNames rejects duplicate allowlist entries', () => {
  const repoRoot = makeRepo()
  test.after(() => fs.rmSync(repoRoot, { recursive: true, force: true, maxRetries: 3 }))
  writeProto(repoRoot, 'example.proto', 'message Example {\n  bool ready = 1;\n}\n')
  writeAllowlist(repoRoot, [
    'proto/bossanova/v1/example.proto:Example.ready',
    'proto/bossanova/v1/example.proto:Example.ready',
  ])

  const { result, err } = captureConsole(() => checkProtoFieldNames(repoRoot))

  assert.equal(result, false)
  assert.match(err.join('\n'), /has duplicate entries/)
})

test('discoverProtoFiles returns sorted v1 proto files and ignores other files', () => {
  const repoRoot = makeRepo()
  test.after(() => fs.rmSync(repoRoot, { recursive: true, force: true, maxRetries: 3 }))
  writeProto(repoRoot, 'b.proto', 'message B {}\n')
  writeProto(repoRoot, 'a.proto', 'message A {}\n')
  fs.writeFileSync(path.join(repoRoot, 'proto', 'bossanova', 'v1', 'notes.txt'), 'x\n')

  assert.deepEqual(
    discoverProtoFiles(repoRoot).map((file) => path.basename(file)),
    ['a.proto', 'b.proto'],
  )
})

for (const [label, declaration, message, rule] of [
  ['a seeded integer count named widgets', 'int32 widgets = 1;', 'Example', 'count-suffix'],
  ['a duration missing units', 'int64 timeout = 1;', 'Example', 'unit-suffix'],
  ['a size missing units', 'int64 upload_size = 1;', 'Example', 'unit-suffix'],
  [
    'a model since timestamp',
    'google.protobuf.Timestamp since = 1;',
    'Example',
    'timestamp-suffix',
  ],
  ['a num prefix count', 'int32 num_widgets = 1;', 'Example', 'count-suffix'],
  ['a noncanonical unit', 'double latency_millis = 1;', 'Example', 'unit-suffix'],
]) {
  test(`checkProtoFieldNames fails ${label}`, (t) => {
    const root = makeRepo()
    t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
    writeProto(root, 'example.proto', `message ${message} {\n ${declaration}\n}\n`)
    const { result, err } = captureConsole(() => checkProtoFieldNames(root))
    assert.equal(result, false)
    assert.match(err.join('\n'), new RegExp(rule))
  })
}
for (const [label, declaration, message] of [
  ['request filter bounds', 'google.protobuf.Timestamp since = 1;', 'ListRequest'],
  ['command filter bounds', 'google.protobuf.Timestamp launched_after = 1;', 'ListCommand'],
  ['terminal dimensions', 'uint32 cols = 1;', 'Example'],
  ['pagination size', 'int32 page_size = 1;', 'Example'],
  ['count suffix', 'int32 retry_count = 1;', 'Example'],
  ['nonnumeric status', 'string status = 1;', 'Example'],
  ['duration milliseconds', 'int64 duration_ms = 1;', 'Example'],
  ['repeated numeric fields', 'repeated int32 widgets = 1;', 'Example'],
  ['nonplural endings', 'int32 status = 1;\n int32 progress = 2;\n int32 axis = 3;', 'Example'],
]) {
  test(`checkProtoFieldNames passes ${label}`, (t) => {
    const root = makeRepo()
    t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
    writeProto(root, 'example.proto', `message ${message} {\n ${declaration}\n}\n`)
    assert.equal(captureConsole(() => checkProtoFieldNames(root)).result, true)
  })
}
test('checkProtoFieldNames rejects unsorted baselines', (t) => {
  const root = makeRepo()
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  writeAllowlist(root, ['z:Example.b', 'a:Example.a'])
  const { result, err } = captureConsole(() => checkProtoFieldNames(root))
  assert.equal(result, false)
  assert.match(err.join('\n'), /must stay sorted/)
})

test('checkProtoFieldNames fails a seeded Timestamp field named created', (t) => {
  const root = makeRepo()
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  writeProto(
    root,
    'example.proto',
    'message Example {\n google.protobuf.Timestamp created = 1;\n}\n',
  )
  const { result, err } = captureConsole(() => checkProtoFieldNames(root))
  assert.equal(result, false)
  assert.match(err.join('\n'), /timestamp-suffix/)
})
