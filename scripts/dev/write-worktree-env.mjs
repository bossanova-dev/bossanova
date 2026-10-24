#!/usr/bin/env node

// Writes a worktree's .env as an allowlisted subset of the checkout's .env (BOS-1512).
// `make setup-worktree` calls this instead of copying the whole file, so an agent session only
// ever sees the keys named in the Makefile's WORKTREE_ENV_KEYS — not every secret a developer
// keeps in the checkout.
//
//   node scripts/dev/write-worktree-env.mjs --from <checkout .env> --to <worktree .env> --keys A,B,C
//
// A line is kept only when it parses as `[export ]KEY=...` (the grammar bossd's dotenv loader
// accepts) and KEY is allowlisted; kept lines are copied byte-for-byte in source order. Comments,
// blank lines, unparseable lines and every other key are dropped. A missing --from is not an
// error: nothing is written. The write is atomic and the file is mode 0600. Output names key
// counts and paths only — values are secrets and are never printed.

import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'

const KEY_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/

// Splits "A, B,,C" into a set of valid key names plus the names that are not valid identifiers.
export function parseKeyList(csv) {
  const keys = new Set()
  const invalid = []
  for (const raw of csv.split(',')) {
    const name = raw.trim()
    if (name === '') continue
    if (KEY_NAME.test(name)) keys.add(name)
    else if (!invalid.includes(name)) invalid.push(name)
  }
  return { keys, invalid }
}

// Returns the assignment's key, or null for a comment, blank or unparseable line. Mirrors
// dotenv.Load in services/bossd/internal/adapter/dotenv so both readers agree on what a key is.
function assignmentKey(line) {
  let text = line.trim()
  if (text === '' || text.startsWith('#')) return null
  if (text.startsWith('export ')) text = text.slice('export '.length).trim()
  const eq = text.indexOf('=')
  if (eq < 0) return null
  const key = text.slice(0, eq).trim()
  return KEY_NAME.test(key) ? key : null
}

// Pure core: filters .env source text down to the allowlisted lines. `dropped` counts the
// non-blank, non-comment lines that were not kept (other keys and unparseable lines).
export function filterEnvSource(source, allowed) {
  const lines = source.match(/[^\n]*\n|[^\n]+$/g) ?? []
  let output = ''
  let kept = 0
  let dropped = 0
  for (const line of lines) {
    const trimmed = line.trim()
    if (trimmed === '' || trimmed.startsWith('#')) continue
    const key = assignmentKey(line)
    if (key !== null && allowed.has(key)) {
      output += line
      kept++
    } else {
      dropped++
    }
  }
  return { output, kept, dropped }
}

// Writes `content` to `to` via a temp file in the same directory and a rename, mode 0600.
function writeAtomic(to, content) {
  const dir = path.dirname(to)
  const tmp = path.join(dir, `.${path.basename(to)}.tmp-${process.pid}-${Date.now()}`)
  try {
    fs.writeFileSync(tmp, content, { mode: 0o600, flag: 'wx' })
    fs.chmodSync(tmp, 0o600)
    fs.renameSync(tmp, to)
  } catch (err) {
    fs.rmSync(tmp, { force: true })
    throw err
  }
}

export function writeWorktreeEnv({ from, to, keys }, io = {}) {
  const log = io.log ?? ((message) => process.stdout.write(`${message}\n`))
  const warn = io.warn ?? ((message) => process.stderr.write(`${message}\n`))

  const { keys: allowed, invalid } = parseKeyList(keys)
  for (const name of invalid) {
    warn(`write-worktree-env: ignoring invalid allowlist key name "${name}"`)
  }

  let source
  try {
    source = fs.readFileSync(from, 'utf8')
  } catch (err) {
    if (err.code === 'ENOENT') {
      log(`No .env in ${path.dirname(from)} — skipping`)
      return { written: false, kept: 0, dropped: 0 }
    }
    throw err
  }

  const { output, kept, dropped } = filterEnvSource(source, allowed)
  writeAtomic(to, output)
  log(`Wrote ${kept} allowlisted .env keys into ${to} (dropped ${dropped})`)
  return { written: true, kept, dropped }
}

export function main(argv = process.argv.slice(2)) {
  let values
  try {
    ;({ values } = parseArgs({
      args: argv,
      options: {
        from: { type: 'string' },
        to: { type: 'string' },
        keys: { type: 'string' },
      },
    }))
    for (const name of ['from', 'to', 'keys']) {
      if (values[name] === undefined) throw new Error(`--${name} is required`)
    }
  } catch (err) {
    process.stderr.write(`write-worktree-env: ${err.message}\n`)
    process.stderr.write(
      'usage: write-worktree-env.mjs --from <path> --to <path> --keys <A,B,...>\n',
    )
    return 2
  }

  try {
    writeWorktreeEnv(values)
    return 0
  } catch (err) {
    // fs errors name the path and the operation, never file contents.
    process.stderr.write(`write-worktree-env: ${err.message}\n`)
    return 1
  }
}

if (isMainModule(import.meta.url)) {
  process.exitCode = main()
}
