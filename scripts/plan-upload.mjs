#!/usr/bin/env node

import crypto from 'node:crypto'
import { spawnSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { r2UploadCommand } from './proof-lib.mjs'

const repoRoot = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')

export function issueSlug(id, title) {
  const titleSlug = String(title)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
  return `${String(id).toUpperCase()}-${titleSlug}`
}

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

function runCommand(commandTuple) {
  const [command, commandArgs] = commandTuple
  const result = spawnSync(command, commandArgs, {
    cwd: repoRoot,
    stdio: 'inherit',
    env: process.env,
  })
  if (result.status !== 0) {
    throw new Error(`${command} ${commandArgs.join(' ')} exited ${result.status}`)
  }
}

function main() {
  const { file, key, dryRun } = parsePlanUploadArgs(process.argv.slice(2))
  const bucket = process.env.BOSS_PROOF_R2_BUCKET
  if (!bucket) {
    throw new Error(
      'BOSS_PROOF_R2_BUCKET is required. Load repo-root .env first: set -a; . ./.env; set +a',
    )
  }
  const url = planPublicUrl(process.env.BOSS_PROOF_PUBLIC_BASE_URL, key)
  const command = r2UploadCommand({
    bucket,
    key,
    file,
    contentType: 'text/markdown; charset=utf-8',
  })
  if (dryRun) {
    console.error(`[dry-run] ${command[0]} ${command[1].join(' ')}`)
    console.log(url)
    return
  }
  runCommand(command)
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
