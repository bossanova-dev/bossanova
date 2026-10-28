import { readFileSync, realpathSync, rmSync, unlinkSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from './main-module.mjs'

/**
 * Classify an already-resolved target without filesystem access.
 * @param {string} target
 * @param {{ tmpRoots: string[] }} options
 */
export function classifyScratchTarget(target, { tmpRoots }) {
  if (!target) return { allowed: false, reason: 'empty target' }
  if (!path.isAbsolute(target)) return { allowed: false, reason: 'relative target' }
  const allowed = tmpRoots.some((root) => {
    const relative = path.relative(root, target)
    return (
      relative !== '' &&
      relative !== '..' &&
      !relative.startsWith(`..${path.sep}`) &&
      !path.isAbsolute(relative)
    )
  })
  return allowed
    ? { allowed: true }
    : { allowed: false, reason: 'target is not strictly inside a temp root' }
}

/** @param {unknown} error */
function missing(error) {
  return error instanceof Error && 'code' in error && error.code === 'ENOENT'
}

// Resolve existing ancestors too: a missing descendant of an escaping symlink is unsafe.
/** @param {string} target
 * @returns {{ resolved: string, absent: boolean }}
 */
function resolveTarget(target) {
  try {
    return { resolved: realpathSync(target), absent: false }
  } catch (error) {
    if (!missing(error)) throw error
    const parent = path.dirname(target)
    if (parent === target) throw error
    const ancestor = resolveTarget(parent)
    return { resolved: path.join(ancestor.resolved, path.basename(target)), absent: true }
  }
}

function defaultRoots() {
  return [
    ...new Set(
      [os.tmpdir(), '/tmp'].flatMap((root) => {
        try {
          return [realpathSync(root)]
        } catch {
          return []
        }
      }),
    ),
  ]
}

/**
 * @param {string[]} [argv]
 * @param {{ tmpRoots?: string[], write?: (line: string) => unknown }} [options]
 */
export function main(argv = process.argv.slice(2), options = {}) {
  let records
  try {
    const { values } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: false,
      options: {
        record: { type: 'string', multiple: true },
      },
    })
    records = values.record
    if (!records?.length || records.some((record) => !record))
      throw new Error('at least one --record <file> is required')
  } catch (error) {
    process.stderr.write(
      `scratch-remove: ${error instanceof Error ? error.message : String(error)}\n`,
    )
    return 64
  }
  const tmpRoots = (options.tmpRoots ?? defaultRoots()).map((root) => realpathSync(root))
  const write = options.write ?? ((line) => process.stdout.write(`${line}\n`))
  let refused = false
  for (const record of records) {
    let target = null
    try {
      let contents
      try {
        contents = readFileSync(record, 'utf8')
      } catch (error) {
        if (!missing(error)) throw error
        write(JSON.stringify({ record, path: null, outcome: 'absent' }))
        continue
      }
      target = contents.split('\n', 1)[0].trim()
      if (!target || !path.isAbsolute(target))
        throw new Error(target ? 'relative target' : 'empty target')
      const { resolved, absent } = resolveTarget(target)
      const verdict = classifyScratchTarget(resolved, { tmpRoots })
      if (!verdict.allowed) throw new Error(verdict.reason)
      if (!absent) rmSync(target, { recursive: true, force: true, maxRetries: 3 })
      try {
        unlinkSync(record)
      } catch (error) {
        if (!missing(error)) throw error
      }
      write(JSON.stringify({ record, path: target, outcome: absent ? 'absent' : 'removed' }))
    } catch (error) {
      refused = true
      write(
        JSON.stringify({
          record,
          path: target,
          outcome: 'refused',
          reason: error instanceof Error ? error.message : String(error),
        }),
      )
    }
  }
  return refused ? 2 : 0
}

if (isMainModule(import.meta.url)) process.exitCode = main()
