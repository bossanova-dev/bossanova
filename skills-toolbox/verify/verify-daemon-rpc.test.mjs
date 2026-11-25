import assert from 'node:assert/strict'
import fs from 'node:fs'
import http from 'node:http'
import path from 'node:path'
import { once } from 'node:events'
import { test } from 'node:test'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { daemonUnary } from './verify-daemon-rpc.mjs'
import { daemonRefusal } from './verify-proof-publisher.mjs'

test('daemon RPC preserves proof-check and evidence refusal reasons', async (t) => {
  const dir = tempDir('proof-rpc-', t)
  const socket = path.join(dir, 'd.sock')
  fs.writeFileSync(path.join(dir, 'bossd.token'), 'local-test-token')
  const server = http.createServer((req, res) => {
    assert.equal(req.headers.authorization, 'Bearer local-test-token')
    const proof = req.url.endsWith('/PostProofCheckRun')
    res.writeHead(412, {
      'content-type': 'application/json',
      [proof ? 'Bossd-Proof-Check-Reason' : 'Bossd-Evidence-Reason']: proof
        ? 'checks-not-granted'
        : 'pages-disabled',
    })
    res.end(JSON.stringify({ code: 'failed_precondition' }))
  })
  server.listen(socket)
  await once(server, 'listening')
  t.after(() => new Promise((resolve, reject) => server.close((e) => (e ? reject(e) : resolve()))))
  for (const [method, reason] of [
    ['PostProofCheckRun', 'checks-not-granted'],
    ['UploadEvidence', 'pages-disabled'],
  ]) {
    await assert.rejects(daemonUnary(socket, method), (error) => {
      assert.equal(error.code, 'failed_precondition')
      assert.equal(error.reason, reason)
      return true
    })
  }
})

test('daemon RPC exposes the evidence limit headers, undefined when absent', async (t) => {
  const dir = tempDir('proof-rpc-limit-', t)
  const socket = path.join(dir, 'd.sock')
  fs.writeFileSync(path.join(dir, 'bossd.token'), 'local-test-token')
  let limited = true
  const server = http.createServer((_req, res) => {
    res.writeHead(429, {
      'content-type': 'application/json',
      'Bossd-Evidence-Reason': 'over-limit',
      ...(limited
        ? { 'Bossd-Evidence-Limit': 'file-bytes', 'Bossd-Evidence-Limit-Max': '5242880' }
        : {}),
    })
    res.end(JSON.stringify({ code: 'resource_exhausted' }))
  })
  server.listen(socket)
  await once(server, 'listening')
  t.after(() => new Promise((resolve, reject) => server.close((e) => (e ? reject(e) : resolve()))))
  for (const [on, limit, limitMax] of [
    [true, 'file-bytes', '5242880'],
    [false, undefined, undefined],
  ]) {
    limited = on
    await assert.rejects(daemonUnary(socket, 'UploadEvidence'), (error) => {
      assert.deepEqual(
        [error.code, error.reason, error.limit, error.limitMax],
        ['resource_exhausted', 'over-limit', limit, limitMax],
      )
      return true
    })
  }
})

test('an unavailable upload names its code and failing stage, never the daemon message', async (t) => {
  const dir = tempDir('proof-rpc-stage-', t)
  const socket = path.join(dir, 'd.sock')
  fs.writeFileSync(path.join(dir, 'bossd.token'), 'local-test-token')
  const server = http.createServer((_req, res) => {
    res.writeHead(503, {
      'content-type': 'application/json',
      'Bossd-Evidence-Reason': 'object-upload-failed',
    })
    res.end(JSON.stringify({ code: 'unavailable', message: 'private upstream detail' }))
  })
  server.listen(socket)
  await once(server, 'listening')
  t.after(() => new Promise((resolve, reject) => server.close((e) => (e ? reject(e) : resolve()))))
  const error = await daemonUnary(socket, 'UploadEvidence').catch((thrown) => thrown)
  assert.deepEqual(daemonRefusal({ kind: 'hosted' }, error), {
    ok: false,
    reason: 'upload-failed',
    detail: 'unavailable (object-upload-failed): UploadEvidence → HTTP 503',
  })
})
