import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { putPlanAttachment, selectImplementationPlanAttachment } from './plan-attachment.mjs'

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
