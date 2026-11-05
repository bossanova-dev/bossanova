#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'

/**
 * Map module-relative Go packages to Bazel patterns. Any uncertain token widens
 * the entire selection, so a scoped facade run never silently omits a package.
 * @param {{moduleRoot: string, goTestPackages?: string, repoRoot: string}} input
 */
export function bazelScopeTargets({ moduleRoot, goTestPackages, repoRoot }) {
  const whole = (reason) => ({ narrowed: false, patterns: [`//${moduleRoot}/...`], reason })
  const tokens = goTestPackages?.trim().split(/\s+/).filter(Boolean) ?? []
  if (!tokens.length) return whole('no package scope')
  const ignoreFile = path.join(repoRoot, '.bazelignore')
  const ignored = fs.existsSync(ignoreFile)
    ? fs
        .readFileSync(ignoreFile, 'utf8')
        .split(/\r?\n/)
        .map((line) => line.split('#')[0].trim())
        .filter(Boolean)
    : []
  const patterns = []
  for (const token of tokens) {
    if (token === './...') return whole('whole-module scope')
    const recursive = token.endsWith('/...')
    const relative = token === '.' ? '' : token.slice(2, recursive ? -4 : undefined)
    if (
      token !== '.' &&
      (!token.startsWith('./') ||
        !relative ||
        relative
          .split('/')
          .some((part) => !/^[A-Za-z0-9_.-]+$/.test(part) || part === '..' || part === '.'))
    ) {
      return whole('unmappable package scope')
    }
    const pkg = relative ? `${moduleRoot}/${relative}` : moduleRoot
    if (ignored.some((entry) => pkg === entry || pkg.startsWith(`${entry}/`))) {
      return whole('package is ignored by Bazel')
    }
    const dir = path.join(repoRoot, pkg)
    if (!['BUILD.bazel', 'BUILD'].some((file) => fs.existsSync(path.join(dir, file)))) {
      return whole('package has no BUILD file')
    }
    patterns.push(`//${pkg}${recursive ? '/...' : ':all'}`)
  }
  return { narrowed: true, patterns: [...new Set(patterns)], reason: 'mapped package scope' }
}

function main() {
  const { values } = parseArgs({ options: { module: { type: 'string' } }, strict: true })
  if (!values.module) throw new Error('--module is required')
  const repoRoot = fileURLToPath(new URL('../../', import.meta.url))
  const result = bazelScopeTargets({
    moduleRoot: values.module,
    goTestPackages: process.env.GO_TEST_PACKAGES,
    repoRoot,
  })
  console.log(result.patterns.join(' '))
}

if (isMainModule(import.meta.url)) main()
