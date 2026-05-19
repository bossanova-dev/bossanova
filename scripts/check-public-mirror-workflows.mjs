#!/usr/bin/env node

import fs from 'node:fs'

const mirrorWorkflow = '.github/workflows/mirror-public.yml'

if (!fs.existsSync(mirrorWorkflow)) {
  console.log('Public mirror workflow check skipped; mirror workflow not present.')
  process.exit(0)
}

const mirror = fs.readFileSync(mirrorWorkflow, 'utf8')

const requiredPublicWorkflows = [
  '.github/workflows/ci.yml',
  '.github/workflows/test-boss.yml',
  '.github/workflows/test-bossd.yml',
  '.github/workflows/test-lib-bossalib.yml',
  '.github/workflows/test-proto.yml',
  '.github/workflows/test-plugin-claude.yml',
  '.github/workflows/test-plugin-codex.yml',
  '.github/workflows/test-plugin-dependabot.yml',
  '.github/workflows/test-plugin-linear.yml',
  '.github/workflows/test-plugin-repair.yml',
]

const missing = requiredPublicWorkflows.filter((workflow) => !mirror.includes(workflow))

if (missing.length > 0) {
  console.error('Public mirror is missing test workflows for public repo code:')
  for (const workflow of missing) {
    console.error(`  - ${workflow}`)
  }
  process.exit(1)
}

console.log(`Public mirror workflows OK (${requiredPublicWorkflows.length} checked)`)
