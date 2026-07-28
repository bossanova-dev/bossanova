#!/usr/bin/env node

import { execFileSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

import { collectChangedFiles, normalizePath } from './format-affected.mjs'

const BIOME_PACKAGES = [
  { dir: 'services/web', root: 'services/web/' },
  { dir: 'services/marketing', root: 'services/marketing/' },
]
const WORKSPACE_TOOLING_FILES = new Set(['package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml'])

/** Return Biome packages whose complete lint needs to run for this change set. */
export function selectBiomePackages(files) {
  const changed = [...new Set(files.map(normalizePath).filter(Boolean))]
  if (changed.some((file) => WORKSPACE_TOOLING_FILES.has(file))) {
    return BIOME_PACKAGES.map(({ dir }) => dir)
  }
  return BIOME_PACKAGES.filter(({ root }) => changed.some((file) => file.startsWith(root))).map(
    ({ dir }) => dir,
  )
}

export function run(argv = process.argv.slice(2), opts = {}) {
  const files = opts.files ?? (argv.length > 0 ? argv : collectChangedFiles())
  const packages = selectBiomePackages(files)
  const log = opts.log ?? ((line) => console.log(line))
  const exec =
    opts.exec ??
    ((dir) => execFileSync('pnpm', ['--dir', dir, 'run', 'lint'], { stdio: 'inherit' }))

  for (const dir of packages) {
    log(`lint-web-affected: ${dir}`)
    try {
      exec(dir)
    } catch {
      return 1
    }
  }
  return 0
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exit(run())
}
