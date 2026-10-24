#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { writeWorktreeEnv } from './write-worktree-env.mjs'

const LOCAL_FILES = [
  '.node-version',
  '.bazelrc.user',
  '.env',
  '.config',
  '.compound-engineering/config.local.yaml',
]

function statWithoutSymlink(file) {
  let stat
  try {
    stat = fs.lstatSync(file)
  } catch (err) {
    if (err.code === 'ENOENT') return null
    throw err
  }
  if (stat.isSymbolicLink()) throw new Error(`refusing symlink: ${file}`)
  return stat
}

// Check each component, including the root, before inspecting or writing an entry.
function inspectPath(root, relative) {
  const parts = relative.split(path.sep)
  let current = root
  let stat = statWithoutSymlink(current)
  if (!stat?.isDirectory()) throw new Error(`not a directory: ${root}`)
  for (const [index, part] of parts.entries()) {
    current = path.join(current, part)
    stat = statWithoutSymlink(current)
    if (!stat) return null
    if (index < parts.length - 1 && !stat.isDirectory()) {
      throw new Error(`not a directory: ${current}`)
    }
  }
  return stat
}

/**
 * @param {string} root
 * @param {string} relative
 * @param {{ prune?: (relative: string) => boolean, skipUnsafe?: boolean, warn?: (message: string) => void }} options
 */
function inspectTree(
  root,
  relative,
  { prune = () => false, skipUnsafe = false, warn = () => {} } = {},
) {
  if (prune(relative)) return []
  // The root and ancestors have already been checked. Only nested source config
  // entries may be skipped; destination paths always use the strict inspection.
  let stat
  if (skipUnsafe) {
    try {
      stat = fs.lstatSync(path.join(root, relative))
    } catch (err) {
      if (err.code === 'ENOENT') return []
      throw err
    }
    if (stat.isSymbolicLink() || (!stat.isFile() && !stat.isDirectory())) {
      const kind = stat.isSymbolicLink() ? 'symlink' : 'non-regular entry'
      warn(`warning: skipping ${kind} in source .config: ${relative}`)
      return []
    }
  } else stat = inspectPath(root, relative)
  if (!stat) return []
  if (stat.isFile()) return [{ relative, directory: false }]
  if (!stat.isDirectory())
    throw new Error(`not a regular file or directory: ${path.join(root, relative)}`)
  return [
    { relative, directory: true },
    ...fs
      .readdirSync(path.join(root, relative))
      .flatMap((name) => inspectTree(root, path.join(relative, name), { prune, skipUnsafe, warn })),
  ]
}

function copyRegular(from, to) {
  // Recheck final file components when opening; do not follow a replaced file symlink.
  const source = fs.openSync(from, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW)
  try {
    const stat = fs.fstatSync(source)
    if (!stat.isFile()) throw new Error(`not a regular file: ${from}`)
    const destination = fs.openSync(
      to,
      fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_NOFOLLOW,
      stat.mode & 0o777,
    )
    try {
      if (!fs.fstatSync(destination).isFile()) throw new Error(`not a regular file: ${to}`)
      fs.ftruncateSync(destination, 0)
      fs.writeFileSync(destination, fs.readFileSync(source))
    } finally {
      fs.closeSync(destination)
    }
  } finally {
    fs.closeSync(source)
  }
}

export function copyWorktreeLocalFiles({ repoDir, worktreeDir, envKeys }, io = {}) {
  const log = io.log ?? ((message) => process.stdout.write(`${message}\n`))
  const repo = path.resolve(repoDir)
  const worktree = path.resolve(worktreeDir)
  const filesystemRoot = path.parse(repo).root
  // Both roots may themselves be reached through a symlinked parent.
  inspectPath(filesystemRoot, path.relative(filesystemRoot, repo))
  const worktreeFilesystemRoot = path.parse(worktree).root
  inspectPath(worktreeFilesystemRoot, path.relative(worktreeFilesystemRoot, worktree))
  const pending = []
  // Validate the whole copy set first, so a refusal cannot leave partial local secrets.
  for (const relative of LOCAL_FILES) {
    const source = inspectPath(repo, relative)
    if (!source) {
      log(`No ${relative} in ${repo} — skipping`)
      continue
    }
    if (relative !== '.config' && !source.isFile()) {
      throw new Error(`not a regular file: ${path.join(repo, relative)}`)
    }
    if (relative === '.config' && !source.isDirectory()) {
      throw new Error(`not a directory: ${path.join(repo, relative)}`)
    }
    const prune = (entry) => entry === path.join('.config', 'accounts')
    if (relative === '.config')
      log('Skipping .config/accounts (managed agent credential homes) — not copied')
    const entries = inspectTree(repo, relative, {
      prune,
      skipUnsafe: relative === '.config',
      warn: log,
    })
    // Existing config entries are checked too, even if the source does not replace them.
    inspectTree(worktree, relative, { prune })
    for (const entry of entries) {
      const destination = inspectPath(worktree, entry.relative)
      if (destination && (entry.directory ? !destination.isDirectory() : !destination.isFile())) {
        throw new Error(`incompatible destination: ${path.join(worktree, entry.relative)}`)
      }
    }
    pending.push({ relative, entries })
  }
  for (const { relative, entries } of pending) {
    for (const entry of entries) {
      const from = path.join(repo, entry.relative)
      const to = path.join(worktree, entry.relative)
      inspectPath(repo, entry.relative)
      inspectPath(worktree, entry.relative)
      if (entry.directory) fs.mkdirSync(to, { recursive: true })
      else {
        fs.mkdirSync(path.dirname(to), { recursive: true })
        if (relative === '.env') writeWorktreeEnv({ from, to, keys: envKeys }, io)
        else copyRegular(from, to)
      }
    }
    if (relative !== '.env') log(`Copied ${relative} into ${worktree}`)
  }
}

export function main(argv = process.argv.slice(2)) {
  let values
  try {
    ;({ values } = parseArgs({
      args: argv,
      strict: true,
      options: {
        'repo-dir': { type: 'string' },
        'worktree-dir': { type: 'string' },
        'env-keys': { type: 'string' },
      },
    }))
    for (const name of ['repo-dir', 'worktree-dir', 'env-keys']) {
      if (values[name] === undefined) throw new Error(`--${name} is required`)
    }
  } catch (err) {
    process.stderr.write(`setup-worktree-copy: ${err.message}\n`)
    return 2
  }
  try {
    copyWorktreeLocalFiles({
      repoDir: values['repo-dir'],
      worktreeDir: values['worktree-dir'],
      envKeys: values['env-keys'],
    })
    return 0
  } catch (err) {
    process.stderr.write(`setup-worktree-copy: ${err.message}\n`)
    return 1
  }
}

if (isMainModule(import.meta.url)) process.exitCode = main()
