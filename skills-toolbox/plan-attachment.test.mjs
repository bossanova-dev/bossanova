import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { createServer } from 'node:http'
import { mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  decodeSpecAttachmentBody,
  putPlanAttachment,
  selectImplementationPlanAttachment,
  selectSupersededPlanAttachments,
} from './plan-attachment.mjs'

const SCRIPT_PATH = fileURLToPath(new URL('./plan-attachment.mjs', import.meta.url))
const TOOLBOX_ROOT = fileURLToPath(new URL('.', import.meta.url))
const USAGE =
  'usage: plan-attachment.mjs put <file> <url> <headers-json-file>\n' +
  '       plan-attachment.mjs decode <in-file> <out-file>\n'

// Built by concatenation so this file is not itself a match for the scan below.
const FORBIDDEN_GUARD = ['import', 'meta', 'main'].join('.')

function runCli(args) {
  return spawnSync(process.execPath, [SCRIPT_PATH, ...args], { encoding: 'utf8' })
}

// spawnSync would block this process's event loop, so an in-process HTTP server could never
// accept the request the child makes. Anything racing a server in this process must spawn async.
function runCliAsync(args) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [SCRIPT_PATH, ...args])
    let stdout = ''
    let stderr = ''
    // spawn() has no `encoding` option (that is spawnSync), so decode on the streams themselves
    // rather than relying on Buffer concatenation, which can split a multi-byte character.
    child.stdout.setEncoding('utf8')
    child.stderr.setEncoding('utf8')
    child.stdout.on('data', (chunk) => (stdout += chunk))
    child.stderr.on('data', (chunk) => (stderr += chunk))
    child.on('error', reject)
    child.on('close', (status) => resolve({ status, stdout, stderr }))
  })
}

test('selectImplementationPlanAttachment prefers the newest exact canonical title', () => {
  const attachment = selectImplementationPlanAttachment(
    [
      { id: 'old', title: 'Implementation plan (BOS-999)', createdAt: '2026-01-01T00:00:00Z' },
      { id: 'new', title: 'Implementation plan (BOS-999)', createdAt: '2026-02-01T00:00:00Z' },
      {
        id: 'other',
        title: 'BOS-999 notes',
        contentType: 'text/markdown',
        createdAt: '2026-03-01T00:00:00Z',
      },
    ],
    'BOS-999',
  )
  assert.equal(attachment.id, 'new')
})

test('selectImplementationPlanAttachment falls back to the newest Markdown attachment naming the issue', () => {
  const attachment = selectImplementationPlanAttachment(
    [
      {
        id: 'old',
        title: 'BOS-999 draft',
        mimeType: 'text/markdown',
        createdAt: '2026-01-01T00:00:00Z',
      },
      { id: 'new', title: 'BOS-999 final', filename: 'plan.md', createdAt: '2026-02-01T00:00:00Z' },
    ],
    'BOS-999',
  )
  assert.equal(attachment.id, 'new')
})

test('selectImplementationPlanAttachment returns null for non-plan attachments', () => {
  assert.equal(
    selectImplementationPlanAttachment(
      [{ id: 'x', title: 'screenshot', contentType: 'image/png' }],
      'BOS-999',
    ),
    null,
  )
})

test('selectSupersededPlanAttachments returns exact-title plans older than the kept attachment', () => {
  assert.deepEqual(
    selectSupersededPlanAttachments(
      [
        { id: 'old', title: 'Implementation plan (BOS-999)', createdAt: '2026-01-01T00:00:00Z' },
        { id: 'keep', title: 'Implementation plan (BOS-999)', createdAt: '2026-02-01T00:00:00Z' },
      ],
      { issueID: 'BOS-999', keepAttachmentId: 'keep' },
    ),
    ['old'],
  )
})

test('selectSupersededPlanAttachments keeps exact-title plans newer than the kept attachment', () => {
  assert.deepEqual(
    selectSupersededPlanAttachments(
      [
        { id: 'keep', title: 'Implementation plan (BOS-999)', createdAt: '2026-02-01T00:00:00Z' },
        { id: 'new', title: 'Implementation plan (BOS-999)', createdAt: '2026-03-01T00:00:00Z' },
      ],
      { issueID: 'BOS-999', keepAttachmentId: 'keep' },
    ),
    [],
  )
})

test('selectSupersededPlanAttachments never deletes Markdown fallback title matches', () => {
  assert.deepEqual(
    selectSupersededPlanAttachments(
      [
        {
          id: 'notes',
          title: 'BOS-999 design notes',
          contentType: 'text/markdown',
          createdAt: '2026-01-01T00:00:00Z',
        },
        { id: 'keep', title: 'Implementation plan (BOS-999)', createdAt: '2026-02-01T00:00:00Z' },
      ],
      { issueID: 'BOS-999', keepAttachmentId: 'keep' },
    ),
    [],
  )
})

test('selectSupersededPlanAttachments ignores other issues and the kept id itself', () => {
  assert.deepEqual(
    selectSupersededPlanAttachments(
      [
        { id: 'other', title: 'Implementation plan (BOS-123)', createdAt: '2026-01-01T00:00:00Z' },
        { id: 'keep', title: 'Implementation plan (BOS-999)', createdAt: '2026-02-01T00:00:00Z' },
      ],
      { issueID: 'BOS-999', keepAttachmentId: 'keep' },
    ),
    [],
  )
})

test('selectSupersededPlanAttachments fails closed when the keep attachment is missing', () => {
  assert.deepEqual(
    selectSupersededPlanAttachments(
      [{ id: 'old', title: 'Implementation plan (BOS-999)', createdAt: '2026-01-01T00:00:00Z' }],
      { issueID: 'BOS-999', keepAttachmentId: 'keep' },
    ),
    [],
  )
})

test('selectSupersededPlanAttachments returns empty when there is no older plan attachment', () => {
  assert.deepEqual(
    selectSupersededPlanAttachments(
      [{ id: 'keep', title: 'Implementation plan (BOS-999)', createdAt: '2026-02-01T00:00:00Z' }],
      { issueID: 'BOS-999', keepAttachmentId: 'keep' },
    ),
    [],
  )
})

test('putPlanAttachment sends raw bytes and every signed header verbatim', async () => {
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-'))
  const file = join(directory, 'plan.md')
  const bytes = Buffer.from('# plan\nraw bytes \x00 stay exact\n')
  writeFileSync(file, bytes)
  const calls = []
  try {
    const status = await putPlanAttachment({
      file,
      uploadURL: 'https://uploads.example/signed',
      headers: { 'x-amz-meta-checksum': 'abc', 'content-type': 'text/markdown' },
      fetchImpl: async (url, init) => {
        calls.push({ url, init })
        return { status: 201 }
      },
    })
    assert.equal(status, 201)
    assert.equal(calls[0].url, 'https://uploads.example/signed')
    assert.equal(calls[0].init.method, 'PUT')
    assert.deepEqual(calls[0].init.headers, {
      'x-amz-meta-checksum': 'abc',
      'content-type': 'text/markdown',
    })
    assert.deepEqual(calls[0].init.body, bytes)
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})

test('putPlanAttachment reports a rejected signed upload', async () => {
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-'))
  const file = join(directory, 'plan.md')
  writeFileSync(file, '# plan\n')
  try {
    await assert.rejects(
      putPlanAttachment({
        file,
        uploadURL: 'https://uploads.example/signed',
        headers: {},
        fetchImpl: async () => ({ status: 403 }),
      }),
      /signed attachment upload returned 403/,
    )
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})

test('decodeSpecAttachmentBody passes plain JSON through unchanged', () => {
  const body = '{\n  "schemaVersion": 1,\n  "parentId": "BOS-1"\n}\n'
  assert.equal(decodeSpecAttachmentBody(body), body)
})

test('decodeSpecAttachmentBody decodes base64 JSON and preserves decoded text', () => {
  const decoded = '{\n  "schemaVersion": 1,\n  "parentId": "BOS-2"\n}\n'
  assert.equal(decodeSpecAttachmentBody(Buffer.from(decoded).toString('base64')), decoded)
})

test('decodeSpecAttachmentBody throws a named error for non-JSON attachment text', () => {
  assert.throws(
    () => decodeSpecAttachmentBody('not-json-and-not-base64-json'),
    /plan-attachment: invalid base64 spec attachment body/,
  )
})

test('decodeSpecAttachmentBody rejects base64 bodies that do not round-trip cleanly', () => {
  const encoded = Buffer.from('{"schemaVersion":1}').toString('base64')
  assert.throws(
    () => decodeSpecAttachmentBody(`${encoded}!!!!`),
    /plan-attachment: invalid base64 spec attachment body/,
  )
  assert.throws(
    () => decodeSpecAttachmentBody(`${encoded.slice(0, 4)} !!!! ${encoded.slice(4)}`),
    /plan-attachment: invalid base64 spec attachment body/,
  )
})

// The entry point below is the defect BOS-872 fixes: guarded by a property that reads
// `undefined` on older runtimes, the whole CLI block was dead code that exited 0 having
// uploaded nothing. These spawn the file for real, so a dead guard cannot pass them.

test('CLI runs the entry point: no arguments prints the usage line and exits 2', () => {
  const result = runCli([])
  assert.equal(result.status, 2)
  assert.equal(result.stderr, USAGE)
})

test('CLI runs the entry point: put with a missing operand exits 2 with the usage line', () => {
  const result = runCli(['put', 'plan.md', 'https://uploads.example/signed'])
  assert.equal(result.status, 2)
  assert.equal(result.stderr, USAGE)
})

test('CLI runs the entry point: an unrecognised command exits non-zero, never 0', () => {
  const result = runCli(['bogus'])
  assert.notEqual(result.status, 0)
  assert.equal(result.status, 2)
  assert.equal(result.stderr, USAGE)
})

test('CLI decode writes decoded JSON to the output path', () => {
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-decode-'))
  const input = join(directory, 'body.txt')
  const output = join(directory, 'spec.json')
  const decoded = '{\n  "schemaVersion": 1,\n  "parentId": "BOS-3"\n}\n'
  writeFileSync(input, Buffer.from(decoded).toString('base64'))
  try {
    const result = runCli(['decode', input, output])
    assert.equal(result.status, 0)
    assert.equal(result.stderr, '')
    assert.equal(readFileSync(output, 'utf8'), decoded)
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})

test('CLI decode with a missing operand exits 2 with the usage line', () => {
  const result = runCli(['decode', 'body.txt'])
  assert.equal(result.status, 2)
  assert.equal(result.stderr, USAGE)
})

test('CLI writes the HTTP status to stdout, so a caller can tell a real PUT from a skipped one', async () => {
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-cli-'))
  const file = join(directory, 'plan.md')
  const headersFile = join(directory, 'headers.json')
  writeFileSync(file, '# plan\nbody\n')
  writeFileSync(headersFile, JSON.stringify({ 'content-type': 'text/markdown' }))

  const received = []
  const server = createServer((request, response) => {
    const chunks = []
    request.on('data', (chunk) => chunks.push(chunk))
    request.on('end', () => {
      received.push({ method: request.method, body: Buffer.concat(chunks).toString('utf8') })
      response.writeHead(200)
      response.end()
    })
  })
  try {
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
    const { port } = server.address()
    const result = await runCliAsync(['put', file, `http://127.0.0.1:${port}/signed`, headersFile])
    assert.equal(result.status, 0)
    assert.equal(result.stdout.trim(), '200')
    assert.equal(received.length, 1)
    assert.equal(received[0].method, 'PUT')
    assert.equal(received[0].body, '# plan\nbody\n')
  } finally {
    await new Promise((resolve) => server.close(resolve))
    rmSync(directory, { recursive: true, force: true })
  }
})

test('plan-attachment.mjs guards its entry point with isMainModule, not the runtime-dependent property', () => {
  const source = readFileSync(SCRIPT_PATH, 'utf8')
  assert.match(source, /import \{ isMainModule \} from '\.\/main-module\.mjs'/)
  assert.match(source, /isMainModule\(import\.meta\.url\)/)
  assert.equal(source.includes(FORBIDDEN_GUARD), false)
})

function collectSourceModules(directory) {
  const found = []
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    if (entry.name === 'node_modules' || entry.name.startsWith('.')) continue
    const absolute = join(directory, entry.name)
    if (entry.isDirectory()) {
      found.push(...collectSourceModules(absolute))
    } else if (entry.name.endsWith('.mjs') && !entry.name.endsWith('.test.mjs')) {
      found.push(absolute)
    }
  }
  return found
}

test('no non-test module under skills-toolbox/ guards on the runtime-dependent property', () => {
  const modules = collectSourceModules(TOOLBOX_ROOT)
  const relativePaths = modules.map((absolute) =>
    relative(TOOLBOX_ROOT, absolute).split(sep).join('/'),
  )

  // Pin the scan as recursive: a non-recursive glob would miss subdirectory entry points.
  assert.ok(
    relativePaths.includes('tracker/cli.mjs'),
    `scan set must reach subdirectory entry points, got: ${relativePaths.join(', ')}`,
  )

  const offenders = modules.filter((absolute) =>
    readFileSync(absolute, 'utf8').includes(FORBIDDEN_GUARD),
  )
  assert.deepEqual(
    offenders.map((absolute) => relative(TOOLBOX_ROOT, absolute).split(sep).join('/')),
    [],
  )
})
