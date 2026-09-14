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
  USAGE_ERROR_TOKEN,
  verifyPlanAttachment,
} from './plan-attachment.mjs'

const SCRIPT_PATH = fileURLToPath(new URL('./plan-attachment.mjs', import.meta.url))
const TOOLBOX_ROOT = fileURLToPath(new URL('.', import.meta.url))
const USAGE =
  `${USAGE_ERROR_TOKEN}\n` +
  'usage: plan-attachment.mjs put <file> <url> <headers-json-file>\n' +
  '       plan-attachment.mjs verify <local-file> <signed-url>\n' +
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

test('selectSupersededPlanAttachments throws instead of silently superseding nothing', () => {
  // The shape the tracker actually returns: {id, title, subtitle, url} and no createdAt. Before
  // this threw, every comparison was `0 < 0`, the function returned [] and the run reported a
  // clean supersede over a stale duplicate it had never looked at.
  assert.throws(
    () =>
      selectSupersededPlanAttachments(
        [
          { id: 'stale', title: 'Implementation plan (BOS-999)', url: 'https://t/1' },
          { id: 'keep', title: 'Implementation plan (BOS-999)', url: 'https://t/2' },
        ],
        { issueID: 'BOS-999', keepAttachmentId: 'keep' },
      ),
    /plan-attachment: unorderable supersede candidates for BOS-999[\s\S]*keepJustFinalized/,
  )
})

test('selectSupersededPlanAttachments supersedes the tracker shape when the caller just finalized keep', () => {
  // The one real caller's escape: it finalized `keep` moments ago in this same run, which no
  // timestamp on this payload can express. Criterion 3 -- the stale id is still selected.
  assert.deepEqual(
    selectSupersededPlanAttachments(
      [
        { id: 'stale', title: 'Implementation plan (BOS-999)', url: 'https://t/1' },
        { id: 'keep', title: 'Implementation plan (BOS-999)', url: 'https://t/2' },
        { id: 'notes', title: 'BOS-999 design notes', contentType: 'text/markdown' },
        { id: 'other', title: 'Implementation plan (BOS-123)' },
      ],
      { issueID: 'BOS-999', keepAttachmentId: 'keep', keepJustFinalized: true },
    ),
    ['stale'],
  )
})

test('selectSupersededPlanAttachments never deletes a newer attachment on the declaration', () => {
  // The concurrent-run hazard the declaration must not swallow: a peer run finalized its own plan
  // between this run's finalize and this list read, so its row is NEWER than anything this run
  // wrote. It carries a timestamp; the stale duplicate does not. Only the unorderable one is swept.
  const concurrent = new Date(Date.now() + 60_000).toISOString()
  assert.deepEqual(
    selectSupersededPlanAttachments(
      [
        { id: 'stale', title: 'Implementation plan (BOS-999)', url: 'https://t/1' },
        { id: 'keep', title: 'Implementation plan (BOS-999)', url: 'https://t/2' },
        { id: 'concurrent', title: 'Implementation plan (BOS-999)', createdAt: concurrent },
      ],
      { issueID: 'BOS-999', keepAttachmentId: 'keep', keepJustFinalized: true },
    ),
    ['stale'],
  )
})

test('selectSupersededPlanAttachments still orders timestamped rows under the declaration', () => {
  // With both rows orderable the declaration changes nothing: older goes, newer stays.
  assert.deepEqual(
    selectSupersededPlanAttachments(
      [
        { id: 'old', title: 'Implementation plan (BOS-999)', createdAt: '2026-01-01T00:00:00Z' },
        { id: 'keep', title: 'Implementation plan (BOS-999)', createdAt: '2026-02-01T00:00:00Z' },
        { id: 'new', title: 'Implementation plan (BOS-999)', createdAt: '2026-03-01T00:00:00Z' },
      ],
      { issueID: 'BOS-999', keepAttachmentId: 'keep', keepJustFinalized: true },
    ),
    ['old'],
  )
})

test('selectSupersededPlanAttachments still fails closed on a missing keep without ordering', () => {
  // The declaration is not a bypass of the fail-closed path: no keep row means no supersede,
  // timestamps or not.
  assert.deepEqual(
    selectSupersededPlanAttachments([{ id: 'stale', title: 'Implementation plan (BOS-999)' }], {
      issueID: 'BOS-999',
      keepAttachmentId: 'keep',
      keepJustFinalized: true,
    }),
    [],
  )
})

test('selectSupersededPlanAttachments does not throw when there is nothing to order', () => {
  // A timestamp-less keep with no exact-title candidates is not an ordering failure: there is
  // no decision to get wrong, so the loud path must not fire on the common healthy shape.
  assert.deepEqual(
    selectSupersededPlanAttachments([{ id: 'keep', title: 'Implementation plan (BOS-999)' }], {
      issueID: 'BOS-999',
      keepAttachmentId: 'keep',
    }),
    [],
  )
})

test('putPlanAttachment surfaces the rejected upload response body beside the status', async () => {
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-'))
  const file = join(directory, 'plan.md')
  writeFileSync(file, '# plan\n')
  try {
    await assert.rejects(
      putPlanAttachment({
        file,
        uploadURL: 'https://uploads.example/signed',
        headers: {},
        fetchImpl: async () => ({
          status: 403,
          text: async () => '<Error><Code>ExpiredToken</Code></Error>',
        }),
      }),
      (error) => {
        // The status prefix the existing assertion pins still leads the message.
        assert.match(error.message, /^signed attachment upload returned 403/)
        assert.match(error.message, /ExpiredToken/)
        return true
      },
    )
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})

test('putPlanAttachment caps the quoted rejection body in BYTES, not characters', async () => {
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-'))
  const file = join(directory, 'plan.md')
  writeFileSync(file, '# plan\n')
  try {
    // 4000 three-byte characters: 4000 UTF-16 code units, 12000 bytes. A character-counted cap of
    // 2048 lets all 12000 bytes through, which is the byte-vs-character confusion this module's
    // own upload contract exists to prevent.
    const body = '\u4e2d'.repeat(4000)
    await assert.rejects(
      putPlanAttachment({
        file,
        uploadURL: 'https://uploads.example/signed',
        headers: {},
        fetchImpl: async () => ({ status: 413, text: async () => body }),
      }),
      (error) => {
        assert.match(error.message, /^signed attachment upload returned 413/)
        assert.match(error.message, /… \(truncated\)$/)
        const quoted = error.message.replace(/^signed attachment upload returned 413: /, '')
        const kept = quoted.replace(/… \(truncated\)$/, '')
        assert.ok(
          Buffer.byteLength(kept, 'utf8') <= 2048,
          `quoted body kept ${Buffer.byteLength(kept, 'utf8')} bytes, above the 2048-byte cap`,
        )
        // Whole characters only: a mid-character cut must not be quoted back as U+FFFD.
        assert.equal(kept.includes('\uFFFD'), false)
        return true
      },
    )
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})

test('putPlanAttachment bounds a streaming rejection body without buffering all of it', async () => {
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-'))
  const file = join(directory, 'plan.md')
  writeFileSync(file, '# plan\n')
  let delivered = 0
  let cancelled = false
  const chunk = Buffer.from('x'.repeat(1024))
  try {
    await assert.rejects(
      putPlanAttachment({
        file,
        uploadURL: 'https://uploads.example/signed',
        headers: {},
        fetchImpl: async () => ({
          status: 403,
          // An effectively endless body: a reader that keeps going is the unbounded read.
          body: {
            getReader: () => ({
              read: async () => {
                delivered += chunk.length
                return { done: false, value: chunk }
              },
              cancel: async () => {
                cancelled = true
              },
            }),
          },
          text: async () => {
            throw new Error('text() must not be used when a stream is readable')
          },
        }),
      }),
      (error) => {
        assert.match(error.message, /^signed attachment upload returned 403/)
        assert.match(error.message, /… \(truncated\)$/)
        return true
      },
    )
    assert.ok(delivered <= 2048 + chunk.length, `read ${delivered} bytes past the cap`)
    assert.equal(cancelled, true, 'the transfer must be cancelled rather than drained')
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})

test('putPlanAttachment still reports a body-less rejection as the bare status', async () => {
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-'))
  const file = join(directory, 'plan.md')
  writeFileSync(file, '# plan\n')
  try {
    await assert.rejects(
      putPlanAttachment({
        file,
        uploadURL: 'https://uploads.example/signed',
        headers: {},
        fetchImpl: async () => ({
          status: 500,
          text: async () => {
            throw new Error('stream already consumed')
          },
        }),
      }),
      /^Error: signed attachment upload returned 500$/,
    )
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})

test('verifyPlanAttachment matches identical bytes and reports a one-byte difference', async () => {
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-verify-'))
  const file = join(directory, 'plan.md')
  writeFileSync(file, '# plan\nstored bytes\n')
  try {
    const same = await verifyPlanAttachment({
      file,
      url: 'https://uploads.example/signed',
      fetchImpl: async () => ({
        status: 200,
        arrayBuffer: async () => Buffer.from('# plan\nstored bytes\n'),
      }),
    })
    assert.equal(same.match, true)
    assert.equal(same.local.sha256, same.fetched.sha256)

    const off = await verifyPlanAttachment({
      file,
      url: 'https://uploads.example/signed',
      fetchImpl: async () => ({
        status: 200,
        // One byte short: the measured buffer-versus-file defect, which a non-empty check passes.
        arrayBuffer: async () => Buffer.from('# plan\nstored bytes'),
      }),
    })
    assert.equal(off.match, false)
    assert.equal(off.local.bytes - off.fetched.bytes, 1)
    assert.notEqual(off.local.sha256, off.fetched.sha256)
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})

test('verifyPlanAttachment rejects an unauthorized read-back instead of scoring its body', async () => {
  // The unsigned GraphQL attachment URL answers 401 with a short JSON body, which a
  // non-empty-content check reads as a small-but-present attachment.
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-verify-'))
  const file = join(directory, 'plan.md')
  writeFileSync(file, '# plan\n')
  try {
    await assert.rejects(
      verifyPlanAttachment({
        file,
        url: 'https://tracker.example/attachment',
        fetchImpl: async () => ({ status: 401, text: async () => '{"error":"unauthorized"}' }),
      }),
      /attachment read-back fetch returned 401[\s\S]*unauthorized/,
    )
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
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

test('CLI separates a usage error from a transport failure by its stderr token alone', async () => {
  const usage = runCli(['put', 'plan.md', 'https://uploads.example/signed'])
  assert.equal(usage.status, 2)
  assert.equal(usage.stderr.split('\n')[0], USAGE_ERROR_TOKEN)

  // A real transport failure: nothing is listening, so a request WAS attempted and failed.
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-cli-'))
  const file = join(directory, 'plan.md')
  const headersFile = join(directory, 'headers.json')
  writeFileSync(file, '# plan\n')
  writeFileSync(headersFile, JSON.stringify({}))
  try {
    // Port 1 is privileged and unbound, so the connection is refused rather than served.
    const failure = await runCliAsync(['put', file, 'http://127.0.0.1:1/signed', headersFile])
    assert.equal(failure.status, 1)
    assert.equal(failure.stderr.includes(USAGE_ERROR_TOKEN), false)
    assert.notEqual(failure.stderr.trim(), '')
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})

test('CLI verify passes on matching bytes and names both sides on a one-byte difference', async () => {
  const directory = mkdtempSync(join(tmpdir(), 'plan-attachment-verify-cli-'))
  const file = join(directory, 'plan.md')
  writeFileSync(file, '# plan\nstored bytes\n')
  let served = '# plan\nstored bytes\n'
  const server = createServer((request, response) => {
    response.writeHead(200, { 'content-type': 'text/markdown' })
    response.end(served)
  })
  try {
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
    const { port } = server.address()
    const url = `http://127.0.0.1:${port}/signed`

    const pass = await runCliAsync(['verify', file, url])
    assert.equal(pass.status, 0)
    assert.match(pass.stdout, /^verify: match sha256=[0-9a-f]{64} bytes=20\n$/)
    assert.equal(pass.stderr, '')

    served = '# plan\nstored bytes'
    const fail = await runCliAsync(['verify', file, url])
    assert.equal(fail.status, 1)
    assert.equal(fail.stdout, '')
    assert.match(fail.stderr, /verify MISMATCH - local sha256=[0-9a-f]{64} bytes=20/)
    assert.match(fail.stderr, /differs from fetched sha256=[0-9a-f]{64} bytes=19/)
  } finally {
    await new Promise((resolve) => server.close(resolve))
    rmSync(directory, { recursive: true, force: true })
  }
})

test('CLI verify with a missing operand exits 2 with the usage token', () => {
  const result = runCli(['verify', 'plan.md'])
  assert.equal(result.status, 2)
  assert.equal(result.stderr, USAGE)
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
