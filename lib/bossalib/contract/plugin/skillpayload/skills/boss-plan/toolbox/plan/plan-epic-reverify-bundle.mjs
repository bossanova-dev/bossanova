// File boundary for the pure epic reverify bundle builder. All paths belong to one run.
import { existsSync, readFileSync, readdirSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { randomUUID } from 'node:crypto'
import { basename, dirname, resolve } from 'node:path'
import { parseArgs } from 'node:util'
import {
  buildEpicReverifyBundle,
  EPIC_REVERIFY_BUNDLE_DEFECT as codes,
} from './plan-epic-phase25.mjs'
import {
  planScratchPath,
  planScratchFamily,
  RUN_SCRATCH_DIR_PREFIX,
} from './plan-scratch-paths.mjs'

/** Run the bundle verb; own all failures so unreadable input never becomes a resumable exit. */
export function runEpicReverifyBundle(argv) {
  let runDir, payload, runId
  try {
    const { values } = parseArgs({
      args: argv.slice(1),
      strict: true,
      allowPositionals: false,
      options: { 'run-scratch': { type: 'string' }, payload: { type: 'string' } },
    })
    if (!values['run-scratch'] || !values.payload)
      throw Error('requires --run-scratch and --payload')
    payload = JSON.parse(values.payload)
    if (payload === null || typeof payload !== 'object' || Array.isArray(payload))
      throw Error('--payload must be an object')
    runDir = resolve(values['run-scratch'])
    const name = basename(runDir)
    if (!name.startsWith(RUN_SCRATCH_DIR_PREFIX) || name === RUN_SCRATCH_DIR_PREFIX)
      throw Error('requires a run-<ID> directory')
    runId = name.slice(RUN_SCRATCH_DIR_PREFIX.length)
  } catch (error) {
    process.stderr.write(`epic-reverify-bundle: ${error.message}\n`)
    return { code: 2, reason: 'usage' }
  }
  let bundlePath, temporary
  const defects = []
  const notices = []
  try {
    const at = (family, parts = {}) => {
      const path = planScratchPath(
        runId,
        family,
        { issueId: payload.epicParentId, ...parts },
        dirname(runDir),
      )
      if (dirname(path) !== runDir || !planScratchFamily(family).pattern.test(basename(path)))
        throw Error('payload identity does not form a declared scratch path')
      return path
    }
    // Validate sentinel fields before deriving filenames from its parent id.
    const sentinel = buildEpicReverifyBundle({ payload })
    const invalid = sentinel.defects.filter((d) =>
      [codes.notEpic, codes.parentMissing, codes.childIdsMissing].some((code) => code === d.code),
    )
    if (typeof payload.epicParentId === 'string' && payload.epicParentId.trim())
      bundlePath = at('epic-reverify')
    if (invalid.length) defects.push(...invalid)
    else {
      const read = (file) => {
        try {
          return JSON.parse(readFileSync(file, 'utf8'))
        } catch (error) {
          if (error.code !== 'ENOENT')
            defects.push({
              code: codes.unreadable,
              file,
              message: 'the saved payload could not be read as JSON',
            })
          return undefined
        }
      }
      const parentPath = at('epic-reverify-payload', { part: 'parent' })
      const listingPath = at('epic-reverify-payload', { part: 'children' })
      const parentIssue = read(parentPath)
      const childrenListing = read(listingPath)
      const prefix = `${payload.epicParentId}.epic-reverify-payload-child-`
      const childFiles = readdirSync(runDir)
        .filter(
          (name) =>
            name.startsWith(prefix) &&
            planScratchFamily('epic-reverify-payload').pattern.test(name),
        )
        .sort()
        .map((name) => {
          const childId = name.slice(prefix.length, -'.json'.length)
          const file = at('epic-reverify-payload', { childId })
          return { file, childId, issue: read(file) }
        })
      const built = buildEpicReverifyBundle({ payload, parentIssue, childrenListing, childFiles })
      for (const defect of built.defects) {
        const file =
          defect.code === codes.parentPayloadMissing
            ? parentPath
            : defect.code === codes.childrenMissing
              ? listingPath
              : undefined
        defects.push({ ...defect, ...(file ? { file } : {}) })
      }
      notices.push(...built.notices)
      const warnings =
        childrenListing?.relationWarnings ?? childrenListing?.result?.relationWarnings
      for (const warning of Array.isArray(warnings) ? warnings : [])
        process.stderr.write(`${typeof warning === 'string' ? warning : JSON.stringify(warning)}\n`)
      if (!defects.length) {
        temporary = `${bundlePath}.${randomUUID()}.tmp`
        writeFileSync(temporary, `${JSON.stringify(built.bundle)}\n`, { flag: 'wx' })
        renameSync(temporary, bundlePath)
        const children = built.bundle.children.map((issue) => issue.identifier ?? issue.id)
        process.stdout.write(
          `${JSON.stringify({
            ok: true,
            bundlePath,
            parentId: payload.epicParentId,
            children,
            ...built.coverage,
            storedDescriptions: {
              parent: at('image-guard-stored'),
              children: Object.fromEntries(
                children.map((childId) => [childId, at('child-image-guard-stored', { childId })]),
              ),
            },
            notices,
          })}\n`,
        )
        return { code: 0, reason: 'ok' }
      }
    }
  } catch (error) {
    defects.push({
      code: codes.couldNotBuild,
      message: `could not build the saved epic bundle: ${error.message}`,
    })
  } finally {
    if (temporary && existsSync(temporary)) rmSync(temporary, { force: true })
  }
  if (bundlePath) {
    try {
      rmSync(bundlePath, { force: true })
    } catch (error) {
      defects.push({
        code: codes.couldNotBuild,
        file: bundlePath,
        message: `could not remove stale bundle: ${error.message}`,
      })
    }
  }
  for (const defect of defects) process.stderr.write(`${defect.code}: ${defect.message}\n`)
  process.stdout.write(`${JSON.stringify({ ok: false, defects })}\n`)
  return { code: 3, reason: defects[0]?.code ?? codes.couldNotBuild }
}
