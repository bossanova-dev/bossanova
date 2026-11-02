import { spawnSync } from 'node:child_process'
import { lstatSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from './main-module.mjs'

// Renames one identifier across files without a shell loop: git lists the candidate files and Node
// rewrites them, so the command line holds no `$(…)` expansion for a permission parser to distrust.

const IDENTIFIER = /^[A-Za-z_][A-Za-z0-9_]*$/

/**
 * Replace every whole-word occurrence of `from` with `to`. Word characters match git grep's `-w`.
 * @param {string} text
 * @param {string} from
 * @param {string} to
 * @returns {{ text: string, replacements: number }}
 */
export function renameInText(text, from, to) {
  let replacements = 0
  const pattern = new RegExp(`(?<![A-Za-z0-9_])${from}(?![A-Za-z0-9_])`, 'g')
  const next = text.replace(pattern, () => {
    replacements += 1
    return to
  })
  return { text: next, replacements }
}

/**
 * @param {string[]} argv
 * @returns {{ from: string, to: string, paths: string[], excludes: string[], dryRun: boolean }}
 */
function parseOptions(argv) {
  const { values } = parseArgs({
    args: argv,
    strict: true,
    allowPositionals: false,
    options: {
      from: { type: 'string' },
      to: { type: 'string' },
      path: { type: 'string', multiple: true },
      exclude: { type: 'string', multiple: true },
      'dry-run': { type: 'boolean', default: false },
    },
  })
  const { from, to } = values
  if (!from || !to) throw new Error('--from <Old> and --to <New> are required')
  if (!IDENTIFIER.test(from) || !IDENTIFIER.test(to))
    throw new Error('--from and --to must be identifiers ([A-Za-z_][A-Za-z0-9_]*)')
  if (from === to) throw new Error('--from and --to must differ')
  const paths = values.path ?? ['.']
  const excludes = values.exclude ?? []
  if ([...paths, ...excludes].some((spec) => !spec))
    throw new Error('--path and --exclude need a non-empty pathspec')
  return { from, to, paths, excludes, dryRun: values['dry-run'] === true }
}

/**
 * List files with a whole-word literal hit, NUL-separated so any filename is safe.
 * @param {{ from: string, paths: string[], excludes: string[] }} options
 * @param {string} cwd
 * @returns {{ files: string[] | null, stderr: string }} `files` is null when git failed
 */
function listCandidates({ from, paths, excludes }, cwd) {
  const result = spawnSync(
    'git',
    [
      '-c',
      'grep.fullName=false',
      'grep',
      '-l',
      '-z',
      '-I',
      '-w',
      '-F',
      '--untracked',
      '--no-color',
      '-e',
      from,
      '--',
      ...paths,
      ...excludes.map((spec) => `:(exclude)${spec}`),
    ],
    { cwd, encoding: 'utf8', maxBuffer: 256 * 1024 * 1024 },
  )
  if (result.error) return { files: null, stderr: `${result.error.message}\n` }
  if (result.status === 1) return { files: [], stderr: '' }
  if (result.status !== 0) return { files: null, stderr: result.stderr || 'git grep failed\n' }
  return { files: result.stdout.split('\0').filter(Boolean), stderr: '' }
}

/**
 * @param {string[]} [argv]
 * @param {{ cwd?: string, write?: (line: string) => unknown, writeError?: (text: string) => unknown }} [deps]
 */
export function main(argv = process.argv.slice(2), deps = {}) {
  const cwd = deps.cwd ?? process.cwd()
  const write = deps.write ?? ((line) => process.stdout.write(`${line}\n`))
  const writeError = deps.writeError ?? ((text) => process.stderr.write(text))
  let options
  try {
    options = parseOptions(argv)
  } catch (error) {
    writeError(`rename-symbol: ${error instanceof Error ? error.message : String(error)}\n`)
    return 2
  }
  const listed = listCandidates(options, cwd)
  if (!listed.files) {
    writeError(listed.stderr)
    return 1
  }
  /** @type {{ path: string, replacements: number }[]} */
  const files = []
  try {
    for (const file of listed.files) {
      const absolute = path.resolve(cwd, file)
      // Writing through a symlink would edit its target, which may sit outside the pathspec.
      if (!lstatSync(absolute).isFile()) continue
      const original = readFileSync(absolute, 'utf8')
      const { text, replacements } = renameInText(original, options.from, options.to)
      if (replacements === 0) continue
      if (!options.dryRun) writeFileSync(absolute, text)
      files.push({ path: file, replacements })
    }
  } catch (error) {
    writeError(`rename-symbol: ${error instanceof Error ? error.message : String(error)}\n`)
    return 1
  }
  write(
    JSON.stringify({
      from: options.from,
      to: options.to,
      dryRun: options.dryRun,
      files,
      totalReplacements: files.reduce((sum, file) => sum + file.replacements, 0),
    }),
  )
  return 0
}

if (isMainModule(import.meta.url)) process.exitCode = main()
