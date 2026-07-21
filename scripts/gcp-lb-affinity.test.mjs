import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'

const terraformURL = new URL('../infra/modules/gcp-lb/main.tf', import.meta.url)

test('bosso load balancer has no session-affinity correctness dependency', async () => {
  const source = await readFile(terraformURL, 'utf8')
  assert.doesNotMatch(source, /session_affinity\s*=\s*"CLIENT_IP"/)
  assert.doesNotMatch(source, /same pod|MUST reach the same pod|Pin by source IP/i)
})
