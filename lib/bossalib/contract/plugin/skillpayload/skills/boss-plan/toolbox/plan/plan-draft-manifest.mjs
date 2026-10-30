import { readFileSync, statSync, writeFileSync, renameSync, rmSync } from 'node:fs'
import { basename, dirname, resolve } from 'node:path'
import { randomUUID } from 'node:crypto'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { planScratchFamily, planScratchPath } from './plan-scratch-paths.mjs'
import { issueSlug } from './plan-slug.mjs'

const GUARD_FAMILIES = [
  'child-image-guard-orig',
  'child-attachment-guard-orig',
  'child-image-guard-new',
]
const text = (value) => (typeof value === 'string' ? value.trim() : '')
const record = (value) => (value && typeof value === 'object' && !Array.isArray(value) ? value : {})
const paths = (value) => (Array.isArray(value) ? value : value == null ? [] : [value])

/**
 * Verify the artifact manifest before a worker publishes, and again before acceptance.
 * @param {{payload: Record<string, unknown>, planPath: string, runScratchDir: string}} input
 * @returns {{ok: boolean, epic: boolean, errors: {code: string, path: string}[]}}
 */
export function verifyDraftManifest({ payload, planPath, runScratchDir }) {
  const errors = []
  const fail = (code, path = '') => errors.push({ code, path })
  const dir = resolve(runScratchDir)
  const entries = (key) =>
    key === 'childPlanPaths' && !Array.isArray(payload[key])
      ? Object.values(record(payload[key]))
      : paths(payload[key])
  if (payload.epic) {
    for (const key of ['guardScratchPaths', 'epicSpecPaths']) {
      if (!Array.isArray(payload[key])) fail(key)
    }
    const parentId = text(payload.epicParentId)
    const ids = paths(payload.childIds).filter((id) => text(id))
    const specs = entries('epicSpecPaths').filter((p) => text(p))
    const guards = entries('guardScratchPaths').filter((p) => text(p))
    const plans = record(payload.childPlanPaths)
    const childPaths = ids.map((id) => plans[id]).filter((p) => text(p))
    const expectedSpec = resolve(
      dir,
      planScratchFamily('epic-spec').basename({ issueId: parentId }),
    )
    /** @type {{key: string, title: string}[]} */
    const children = []
    if (!specs.length) fail('epicSpecPaths')
    for (const path of specs) {
      if (resolve(path) !== expectedSpec) {
        fail('epicSpecPaths', path)
        continue
      }
      try {
        const spec = JSON.parse(readFileSync(path, 'utf8'))
        if (text(spec.parentId) !== parentId) fail('epicSpecPaths', path)
        for (const child of spec.children || []) {
          if (text(child.key) && text(child.title)) children.push(child)
          else fail('epicSpecPaths', path)
        }
      } catch {
        fail('epicSpecPaths', path)
      }
    }
    if (
      !parentId ||
      ids.some((childId) =>
        GUARD_FAMILIES.some(
          (family) =>
            !guards.some(
              (path) =>
                basename(path) ===
                planScratchFamily(family).basename({ issueId: parentId, childId }),
            ),
        ),
      )
    )
      fail('guardScratchPaths')
    if (
      !parentId ||
      !ids.length ||
      children.length !== ids.length ||
      Object.keys(plans).length !== ids.length ||
      childPaths.length !== ids.length ||
      new Set(childPaths.map((p) => resolve(p))).size !== ids.length
    )
      fail('childPlanPaths')
    const used = new Set()
    for (const id of ids) {
      const path = text(plans[id])
      if (!path) {
        fail('childPlanPaths', String(id))
        continue
      }
      const index = children.findIndex(
        ({ key, title }) =>
          basename(path) ===
          planScratchFamily('child-plan').basename({ parentId, key, slug: issueSlug(id, title) }),
      )
      if (index < 0 || used.has(index)) fail('childPlanPaths', path)
      else used.add(index)
    }
    if (used.size !== children.length) fail('childPlanPaths')
  } else if (!entries('planPath').some((p) => text(p))) fail('planPath')
  for (const key of [
    'planPath',
    'guardScratchPaths',
    'childPlanPaths',
    'epicSpecPaths',
    'attachmentHeaderPaths',
  ]) {
    for (const path of entries(key)) {
      if (!text(path)) {
        fail(key)
        continue
      }
      const absolute = resolve(path)
      try {
        const stat = statSync(absolute)
        const allowed = absolute === resolve(planPath) || dirname(absolute) === dir
        if (
          !allowed ||
          !stat.isFile() ||
          (!stat.size && !(key === 'guardScratchPaths' && /-guard-orig[.]md$/.test(absolute)))
        )
          fail(key, path)
      } catch {
        fail(key, path)
      }
    }
  }
  return { ok: errors.length === 0, epic: Boolean(payload.epic), errors }
}

/**
 * Stage evidence under the created child's identifier. Read every source before writing.
 * @param {{runId: string, parentId: string, childId: string, descriptionFile: string, adopted?: boolean}} input
 * @returns {string[]}
 */
export function stageChildGuards({ runId, parentId, childId, descriptionFile, adopted = false }) {
  const description = readFileSync(descriptionFile)
  if (!description.length) throw new Error('child description is empty')
  const source = (family) =>
    adopted ? Buffer.alloc(0) : readFileSync(planScratchPath(runId, family, { issueId: parentId }))
  const contents = [source('image-guard-orig'), source('attachment-guard-orig'), description]
  const targets = GUARD_FAMILIES.map((family) =>
    planScratchPath(runId, family, { issueId: parentId, childId }),
  )
  const temporary = targets.map((path) => `${path}.${randomUUID()}.tmp`)
  try {
    temporary.forEach((path, i) => writeFileSync(path, contents[i], { flag: 'wx' }))
    temporary.forEach((path, i) => renameSync(path, targets[i]))
  } finally {
    temporary.forEach((path) => rmSync(path, { force: true }))
  }
  return targets
}

function main() {
  try {
    const { positionals, values } = parseArgs({
      allowPositionals: true,
      strict: true,
      options: {
        payload: { type: 'string' },
        'plan-path': { type: 'string' },
        'run-scratch': { type: 'string' },
        label: { type: 'string' },
        'run-id': { type: 'string' },
        parent: { type: 'string' },
        child: { type: 'string' },
        description: { type: 'string' },
        adopted: { type: 'boolean' },
      },
    })
    const required = (key) => {
      const value = values[key]
      if (typeof value !== 'string' || !value) throw new Error(`missing --${key}`)
      return value
    }
    if (positionals.length !== 1) throw new Error('expected verify or stage-child')
    const command = positionals[0]
    if (command === 'verify') {
      const payload = JSON.parse(required('payload'))
      if (!payload || typeof payload !== 'object' || Array.isArray(payload))
        throw new Error('--payload must be an object')
      const verdict = verifyDraftManifest({
        payload,
        planPath: required('plan-path'),
        runScratchDir: required('run-scratch'),
      })
      console.log(JSON.stringify(verdict))
      for (const error of verdict.errors)
        console.error(
          `${values.label || 'dispatch-failure'}: sentinel ok but artifact missing/empty or wrong path (${error.code}) — no Linear write, aborting`,
        )
      return verdict.ok ? 0 : 1
    }
    if (command === 'stage-child') {
      const input = {
        runId: required('run-id'),
        parentId: required('parent'),
        childId: required('child'),
        descriptionFile: required('description'),
        adopted: values.adopted,
      }
      try {
        console.log(stageChildGuards(input).join('\n'))
        return 0
      } catch (error) {
        console.error(error.message)
        return 1
      }
    }
    throw new Error('expected verify or stage-child')
  } catch (error) {
    console.error(`plan-draft-manifest: ${error.message}`)
    return 2
  }
}

if (isMainModule(import.meta.url)) process.exitCode = main()
