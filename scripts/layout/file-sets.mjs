import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'

export const SCRIPT_TREES = ['scripts', 'skills-toolbox']

export function isFixturePath(file) {
  return (
    file
      .split('/')
      .some((part) => ['fixtures', 'testdata', 'assets', 'node_modules', '.git'].includes(part)) ||
    file.includes('skill-parity/baseline/')
  )
}

function walk(root, directory) {
  if (!fs.existsSync(path.join(root, directory))) return []
  return fs.readdirSync(path.join(root, directory), { withFileTypes: true }).flatMap((entry) => {
    const file = `${directory}/${entry.name}`
    if (isFixturePath(file)) return []
    if (entry.isDirectory()) return walk(root, file)
    return entry.isFile() ? [file] : []
  })
}

/** One recursive selection for the test, formatting and syntax runners. */
export function resolveFileSets(repoRoot) {
  const trees = SCRIPT_TREES.flatMap((tree) => walk(repoRoot, tree))
  const skills = '.claude/skills'
  const gates = fs.existsSync(path.join(repoRoot, skills))
    ? fs
        .readdirSync(path.join(repoRoot, skills))
        .flatMap((skill) => walk(repoRoot, `${skills}/${skill}/gate`))
    : []
  const verify = [
    ...walk(repoRoot, `${skills}/verify/bin`),
    ...walk(repoRoot, `${skills}/verify/lib`),
  ]
  const script = (file) => /\.(?:cjs|mjs)$/.test(file)
  const syntax = [
    ...new Set([
      ...trees.filter(script),
      ...gates.filter(script),
      ...verify.filter((file) => script(file) || file.startsWith(`${skills}/verify/bin/`)),
    ]),
  ].sort()
  return {
    test: [...trees, ...verify].filter((file) => file.endsWith('.test.mjs')).sort(),
    format: syntax,
    syntax,
  }
}

function main() {
  const { values, positionals } = parseArgs({
    strict: true,
    allowPositionals: true,
    options: {
      root: { type: 'string', default: process.cwd() },
      list: { type: 'boolean', default: false },
    },
  })
  const action = positionals[0]
  if (!['test', 'format', 'syntax'].includes(action) || positionals.length !== 1)
    throw new Error('usage: file-sets.mjs test|format|syntax [--root path] [--list]')
  const files = resolveFileSets(values.root)[action]
  if (!files.length) throw new Error(`${action}: no script files selected`)
  if (values.list) {
    console.log(JSON.stringify(files))
    return
  }
  if (action === 'syntax')
    throw new Error('syntax is a file-list only action; use lint-scripts.mjs to check syntax')
  // Preserve the existing suite boundaries: tests in the two trees share temporary
  // repository fixtures and must not race each other across tree roots.
  const batches =
    action === 'test'
      ? [...SCRIPT_TREES, '.claude/skills/verify'].map((tree) =>
          files.filter((file) => file.startsWith(`${tree}/`)),
        )
      : [files]
  for (const batch of batches.filter((batch) => batch.length)) {
    const reporter = /(?:^|\s)--test-reporter(?:=|\s|$)/.test(process.env.NODE_OPTIONS || '')
      ? []
      : ['--test-reporter=tap']
    const args =
      action === 'test'
        ? ['--test', ...reporter, ...batch]
        : [path.join(values.root, 'node_modules/prettier/bin/prettier.cjs'), '--write', ...batch]
    const result = spawnSync(process.execPath, args, { cwd: values.root, stdio: 'inherit' })
    if (result.error) throw result.error
    if (result.status !== 0) {
      process.exitCode = result.status ?? 1
      return
    }
  }
}

if (isMainModule(import.meta.url)) main()
