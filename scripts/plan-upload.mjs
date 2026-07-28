#!/usr/bin/env node

import crypto from 'node:crypto'
import { spawnSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { r2UploadCommand } from './proof-lib.mjs'
// The slug helper is portable and ships in boss-plan's toolbox; re-exported here
// so the repo-local publish path keeps a single import surface.
import { issueSlug } from '../skills-toolbox/plan-slug.mjs'

const repoRoot = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')

export { issueSlug }

export function generateToken() {
  return crypto.randomBytes(16).toString('hex')
}

export function planObjectKey(id, token) {
  return `plans/bossanova/${String(id).toUpperCase()}/${token}.md`
}

export function planPublicUrl(baseUrl, key) {
  const base = (baseUrl || 'https://proof.bossanova.dev').replace(/\/$/, '')
  return `${base}/${key}`
}

export function parsePlanUploadArgs(argv) {
  const args = { file: null, key: null, dryRun: false }
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    if (flag === '--file') {
      args.file = argv[(i += 1)]
    } else if (flag === '--key') {
      args.key = argv[(i += 1)]
    } else if (flag === '--dry-run') {
      args.dryRun = true
    }
  }
  if (!args.file) {
    throw new Error('--file <path> is required')
  }
  if (!args.key) {
    throw new Error('--key <object-key> is required')
  }
  return args
}

function defaultRunImpl([command, commandArgs]) {
  const result = spawnSync(command, commandArgs, {
    cwd: repoRoot,
    stdio: 'inherit',
    env: process.env,
  })
  if (result.status !== 0) {
    throw new Error(`${command} ${commandArgs.join(' ')} exited ${result.status}`)
  }
}

export function uploadPlanFile({
  file,
  key,
  env = process.env,
  dryRun = false,
  runImpl = defaultRunImpl,
}) {
  const bucket = env.BOSS_PROOF_R2_BUCKET
  if (!bucket) {
    throw new Error(
      'BOSS_PROOF_R2_BUCKET is required. Load repo-root .env first: set -a; . ./.env; set +a',
    )
  }
  const url = planPublicUrl(env.BOSS_PROOF_PUBLIC_BASE_URL, key)
  const command = r2UploadCommand({
    bucket,
    key,
    file,
    contentType: 'text/markdown; charset=utf-8',
  })
  if (dryRun) {
    console.error(`[dry-run] ${command[0]} ${command[1].join(' ')}`)
    return url
  }
  runImpl(command)
  return url
}

function main() {
  const { file, key, dryRun } = parsePlanUploadArgs(process.argv.slice(2))
  const url = uploadPlanFile({ file, key, env: process.env, dryRun })
  console.log(url)
}

const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
if (invokedDirectly) {
  try {
    main()
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
