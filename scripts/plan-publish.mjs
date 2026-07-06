#!/usr/bin/env node
// scripts/plan-publish.mjs
// Thin CLI the bs-plan skill calls to publish a plan file: resolves the publish
// adapter (keyed on PLAN_PUBLISH, default r2) and prints the resulting public URL.
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { resolvePublishAdapter } from './publish/adapter.mjs'

export function parsePlanPublishArgs(argv) {
  const args = { issue: null, file: null, dryRun: false }
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    if (flag === '--issue') args.issue = argv[(i += 1)]
    else if (flag === '--file') args.file = argv[(i += 1)]
    else if (flag === '--dry-run') args.dryRun = true
  }
  if (!args.issue) throw new Error('--issue <id> is required')
  if (!args.file) throw new Error('--file <path> is required')
  return args
}

export async function runPlanPublish({ argv, env = process.env, resolve = resolvePublishAdapter }) {
  const { issue, file, dryRun } = parsePlanPublishArgs(argv)
  const adapter = resolve(env)
  const { url } = await adapter.publish({ issueId: issue, file, dryRun })
  return url
}

const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
if (invokedDirectly) {
  runPlanPublish({ argv: process.argv.slice(2) })
    .then((url) => console.log(url))
    .catch((error) => {
      console.error(error instanceof Error ? error.message : String(error))
      process.exitCode = 1
    })
}
