import assert from 'node:assert/strict'
import fs from 'node:fs'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import { allocateLocalPort, isLoopbackPortFree, reserveServicePorts } from './serve-ports.mjs'

const META = {
  bosso: { localPort: 8080, httpsPort: 18080 },
  web: { localPort: 5173, httpsPort: 15173 },
  marketing: { localPort: 4321, httpsPort: 14321 },
  docs: { localPort: 3001, httpsPort: 13001 },
}

test('allocateLocalPort steps sequentially past busy or reserved ports', async () => {
  const reserved = new Set([5173])
  const busy = new Set([5174])
  const port = await allocateLocalPort(5173, reserved, async (p) => !busy.has(p))
  assert.equal(port, 5175)
  assert.ok(reserved.has(5175))
})

test('isLoopbackPortFree detects an occupied 127.0.0.1 port', async () => {
  const srv = net.createServer()
  await new Promise((resolve) => srv.listen(0, '127.0.0.1', resolve))
  const addr = srv.address()
  assert.ok(addr && typeof addr === 'object')
  try {
    assert.equal(await isLoopbackPortFree(addr.port), false)
  } finally {
    await new Promise((resolve) => srv.close(resolve))
  }
  assert.equal(await isLoopbackPortFree(addr.port), true)
})

test('reserveServicePorts prevents collisions across concurrent worktrees before ports bind', async () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'serve-leases-'))
  const leaseFile = path.join(tmp, 'leases.json')
  const alwaysFree = async () => true
  const livePids = new Set([1001, 1002])

  try {
    // Worktree 1 reserves preferred ports while its services are still compiling
    // (so socket probes still report every port as free).
    const first = await reserveServicePorts({
      services: ['bosso', 'web', 'marketing', 'docs'],
      serviceMeta: META,
      useTailscale: true,
      tailscaleHost: 'host.tail.ts.net',
      worktree: '/tmp/wt-1',
      pid: 1001,
      leaseFile,
      pidAlive: (pid) => livePids.has(pid),
      probePort: alwaysFree,
    })

    assert.deepEqual(first.localPorts, { bosso: 8080, web: 5173, marketing: 4321, docs: 3001 })
    assert.deepEqual(first.httpsPorts, { bosso: 18080, web: 15173, marketing: 14321, docs: 13001 })

    // Worktree 2 starts immediately in parallel; it must receive disjoint local
    // and Tailscale HTTPS ports even though Worktree 1 has not bound sockets yet.
    const second = await reserveServicePorts({
      services: ['bosso', 'web', 'marketing', 'docs'],
      serviceMeta: META,
      useTailscale: true,
      tailscaleHost: 'host.tail.ts.net',
      worktree: '/tmp/wt-2',
      pid: 1002,
      leaseFile,
      pidAlive: (pid) => livePids.has(pid),
      probePort: alwaysFree,
    })

    assert.deepEqual(second.localPorts, { bosso: 8081, web: 5174, marketing: 4322, docs: 3002 })
    assert.deepEqual(second.httpsPorts, { bosso: 18090, web: 15183, marketing: 14331, docs: 13011 })
    assert.equal(second.publicUrls.bosso, 'https://host.tail.ts.net:18090')
    assert.equal(second.publicUrls.web, 'https://host.tail.ts.net:15183')

    // Releasing Worktree 1 frees its ports for a third worktree.
    await first.release()
    livePids.delete(1001)
    livePids.add(1003)

    const third = await reserveServicePorts({
      services: ['bosso', 'web'],
      serviceMeta: META,
      useTailscale: false,
      tailscaleHost: '',
      worktree: '/tmp/wt-3',
      pid: 1003,
      leaseFile,
      pidAlive: (pid) => livePids.has(pid),
      probePort: alwaysFree,
    })

    assert.deepEqual(third.localPorts, { bosso: 8080, web: 5173 })
    assert.deepEqual(third.publicUrls, {
      bosso: 'http://127.0.0.1:8080',
      web: 'http://127.0.0.1:5173',
    })

    await second.release()
    await third.release()
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('reserveServicePorts prunes dead PIDs automatically', async () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'serve-leases-prune-'))
  const leaseFile = path.join(tmp, 'leases.json')
  const alwaysFree = async () => true

  try {
    fs.writeFileSync(
      leaseFile,
      JSON.stringify([
        {
          pid: 999999,
          worktree: '/tmp/crashed-wt',
          localPorts: { bosso: 8080, web: 5173 },
          httpsPorts: { bosso: 18080, web: 15173 },
          startedAt: new Date().toISOString(),
        },
      ]),
    )

    const res = await reserveServicePorts({
      services: ['bosso', 'web'],
      serviceMeta: META,
      useTailscale: false,
      tailscaleHost: '',
      worktree: '/tmp/wt-fresh',
      pid: 2001,
      leaseFile,
      pidAlive: (pid) => pid === 2001,
      probePort: alwaysFree,
    })

    assert.deepEqual(res.localPorts, { bosso: 8080, web: 5173 })
    await res.release()
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true, maxRetries: 3 })
  }
})
