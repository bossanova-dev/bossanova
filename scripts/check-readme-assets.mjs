#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'

const repoRoot = process.cwd()
const readmePath = path.join(repoRoot, 'README.md')
const readme = fs.readFileSync(readmePath, 'utf8')

const localTargets = new Set()

function isLocalTarget(target) {
  if (!target || target.startsWith('#')) return false
  if (/^[a-z][a-z0-9+.-]*:/i.test(target)) return false
  if (target.startsWith('//')) return false
  return true
}

function normalizeTarget(rawTarget) {
  const withoutAnchor = rawTarget.split('#')[0]
  const withoutQuery = withoutAnchor.split('?')[0]
  return decodeURIComponent(withoutQuery).trim()
}

let inFence = false

for (const line of readme.split('\n')) {
  if (/^\s*```/.test(line)) {
    inFence = !inFence
    continue
  }
  if (inFence) continue

  for (const match of line.matchAll(/!\[[^\]]*]\(([^)]+)\)/g)) {
    const target = normalizeTarget(match[1])
    if (isLocalTarget(target)) localTargets.add(target)
  }

  for (const match of line.matchAll(/(?<!!)\[[^\]]+]\(([^)]+)\)/g)) {
    const target = normalizeTarget(match[1])
    if (isLocalTarget(target)) localTargets.add(target)
  }

  for (const match of line.matchAll(/<img\b[^>]*\bsrc=["']([^"']+)["'][^>]*>/gi)) {
    const target = normalizeTarget(match[1])
    if (isLocalTarget(target)) localTargets.add(target)
  }
}

const missing = [...localTargets].filter((target) => {
  const targetPath = path.resolve(repoRoot, target)
  return !targetPath.startsWith(repoRoot + path.sep) || !fs.existsSync(targetPath)
})

if (missing.length > 0) {
  console.error('README references missing local assets:')
  for (const target of missing) {
    console.error(`  - ${target}`)
  }
  process.exit(1)
}

console.log(`README local assets OK (${localTargets.size} checked)`)
