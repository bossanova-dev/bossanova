// scripts/publish/adapter.mjs
// Pluggable plan-publish adapter: the single choke point bs-plan Phase 4 uses to
// upload a plan file and get back a public URL, so a consuming repo can target
// its own object store or a private host without editing the skill body.
// Mirrors scripts/tracker/adapter.mjs (BOS-190). node builtins only.
import { createR2PublishAdapter } from './r2.mjs'

/**
 * @typedef {object} PublishAdapter
 * @property {string} name
 * @property {(args: { issueId: string, file: string, dryRun?: boolean }) => Promise<{ url: string, key: string }>} publish
 */

export const PUBLISH_CAPABILITIES = ['publish']

// name -> create* factory. Change the store, register a factory here.
const REGISTRY = new Map([['r2', createR2PublishAdapter]])

export function assertConforms(adapter) {
  for (const cap of PUBLISH_CAPABILITIES) {
    if (typeof adapter?.[cap] !== 'function') {
      throw new Error(`publish adapter missing capability: ${cap}`)
    }
  }
  return adapter
}

export function resolvePublishAdapter(env = process.env) {
  const name = env.PLAN_PUBLISH || 'r2'
  const create = REGISTRY.get(name)
  if (!create) {
    throw new Error(`unknown publish adapter: ${name}`)
  }
  return assertConforms(create({ env }))
}
