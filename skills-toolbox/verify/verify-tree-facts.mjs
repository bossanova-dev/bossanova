// Facts read from the base and head trees. Missing tools and unreadable contracts fail closed.
import { spawnSync } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { discoverExtensions } from '../skill-extensions.mjs'
import { pathParkFacts } from './verify-path-parks.mjs'
import { runTool } from './verify-diff.mjs'
import { compareProtoModules, findTool } from './verify-describe-protos.mjs'
import { describeAreas, runDescribers } from './verify-describe.mjs'
import { graphqlSchemaChanges, parseGraphqlSchema } from './verify-graphql-diff.mjs'
import { MAX_ITEMS, MAX_TEXT_CHARS } from './verify-proof.mjs'

/** Is a commit local, fetching from origin once when needed? */
export function ensureCommit(git, sha) {
  const commit = `${sha}^{commit}`
  if (git('git', ['cat-file', '-e', commit]).ok) return true
  git('git', ['fetch', '--no-tags', '--quiet', 'origin', sha])
  return git('git', ['cat-file', '-e', commit]).ok
}

/** @param {{run?: import('./verify-diff.mjs').Run, cwd?: string, shas: string[], paths: string[]}} options */
export function dbtProjectRoots({ run = spawnSync, cwd, shas, paths }) {
  const roots = new Set()
  const visited = new Set()
  for (const file of paths.filter((file) => file.endsWith('.sql'))) {
    for (let dir = path.posix.dirname(file); ; dir = path.posix.dirname(dir)) {
      if (!visited.has(dir)) {
        visited.add(dir)
        const config = dir === '.' ? 'dbt_project.yml' : `${dir}/dbt_project.yml`
        if (
          shas.some(
            (sha) => sha && runTool(run, 'git', ['cat-file', '-e', `${sha}:${config}`], { cwd }).ok,
          )
        )
          roots.add(dir)
      }
      if (dir === '.') break
    }
  }
  return [...roots].sort()
}

const graphql = (file) => /\.(?:graphqls?|gql)$/.test(file)
const openapi = (file) => /^(?:openapi|swagger).*\.(?:ya?ml|json)$/.test(path.posix.basename(file))
const bounded = (reasons) =>
  [
    ...new Set(
      reasons.map((reason) =>
        String(reason)
          .replace(/[\r\n\u2028\u2029]+/g, ' ')
          .slice(0, MAX_TEXT_CHARS),
      ),
    ),
  ].slice(0, MAX_ITEMS)

/** Compute the contract change independently of any installed describer. */
export function apiSchemaChange({
  run = spawnSync,
  cwd,
  mergeBase,
  headSha,
  paths,
  previousPaths = [],
  env = process.env,
  tmp = tmpdir(),
}) {
  const reasons = []
  let breaking = false
  const fail = (reason) => {
    breaking = true
    reasons.push(reason)
  }
  const git = (args) => runTool(run, 'git', args, { cwd })
  const names = [...new Set([...paths, ...previousPaths])]
  const result = () => ({ change: breaking ? 'breaking' : 'additive', reasons: bounded(reasons) })
  if (
    !mergeBase ||
    !headSha ||
    ![mergeBase, headSha].every((sha) => git(['cat-file', '-e', `${sha}^{commit}`]).ok)
  ) {
    fail('schema trees unreadable')
    return result()
  }
  const read = (sha, file) => {
    const shown = git(['show', `${sha}:${file}`])
    if (shown.ok) return shown.stdout
    const listed = git(['ls-tree', '-z', sha, '--', file])
    if (!listed.ok || listed.stdout.length > 0) fail(`schema unreadable: ${file}`)
    return null
  }
  try {
    const gql = names.filter(graphql)
    if (gql.length) {
      const schemas = [mergeBase, headSha].map((sha) => {
        const pieces = gql.map((file) => read(sha, file)).filter((text) => text !== null)
        return parseGraphqlSchema(pieces.join('\n'))
      })
      if (!schemas[0].ok || !schemas[1].ok) fail(`GraphQL schema unreadable: ${gql.join(', ')}`)
      else {
        const changes = graphqlSchemaChanges(schemas[0].schema, schemas[1].schema)
        for (const reason of changes.breaking) fail(reason)
        reasons.push(...changes.additive)
      }
    }
    const protos = names.filter((file) => file.endsWith('.proto'))
    if (protos.length) {
      const comparisons = compareProtoModules(
        { area: { paths: protos }, baseSha: mergeBase, headSha },
        { run, cwd, env, tmp },
      )
      for (const entry of comparisons) {
        if (entry.state === 'unchecked' || entry.state === 'breaking')
          fail(`Proto ${entry.dir}: ${entry.reason || entry.violations[0] || entry.state}`)
        else reasons.push(`Proto ${entry.dir}: ${entry.state}`)
      }
      // A contract removed or renamed away must not disappear behind module discovery.
      for (const file of protos)
        if (read(mergeBase, file) !== null && read(headSha, file) === null)
          fail(`Proto contract removed: ${file}`)
    }
    for (const file of names.filter(openapi)) {
      const before = read(mergeBase, file)
      const after = read(headSha, file)
      if (after === null) {
        fail(`OpenAPI contract removed: ${file}`)
        continue
      }
      if (before === null) {
        reasons.push(`OpenAPI contract added: ${file}`)
        continue
      }
      const bin = findTool('oasdiff', 'OASDIFF', env)
      if (!bin) {
        fail(`OpenAPI schema unreadable: ${file} (no oasdiff)`)
        continue
      }
      const work = mkdtempSync(path.join(tmp, 'verify-oasdiff-'))
      try {
        const baseFile = path.join(work, `base-${path.basename(file)}`)
        const headFile = path.join(work, `head-${path.basename(file)}`)
        writeFileSync(baseFile, before)
        writeFileSync(headFile, after)
        const compared = runTool(run, bin, ['breaking', baseFile, headFile, '--format', 'json'], {
          cwd,
        })
        let entries
        try {
          entries = JSON.parse(compared.stdout)
        } catch {}
        if (!compared.ok || !Array.isArray(entries)) fail(`OpenAPI schema unreadable: ${file}`)
        else if (entries.length) fail(`OpenAPI breaking changes: ${file}`)
        else reasons.push(`OpenAPI compatible: ${file}`)
      } finally {
        rmSync(work, { recursive: true, force: true, maxRetries: 3 })
      }
    }
    for (const file of names)
      if (!graphql(file) && !openapi(file) && !file.endsWith('.proto'))
        fail(`schema format unreadable: ${file}`)
  } catch {
    fail('schema comparison unreadable')
  }
  return result()
}

/** Resolve and verify the same schema trees for the gate and the proof page. */
export function schemaTrees({ baseSha, headSha, run = spawnSync, cwd }) {
  const git = (command, args) => runTool(run, command, args, { cwd })
  const available = [baseSha, headSha].every((sha) => sha && ensureCommit(git, sha))
  const merge = available ? git('git', ['merge-base', baseSha, headSha]) : null
  const mergeBase = merge?.ok ? merge.stdout.trim() : null
  // Verify both trees before using either to exempt SQL from migrations.
  const readable =
    mergeBase &&
    [mergeBase, headSha].every(
      (sha) => runTool(run, 'git', ['cat-file', '-e', `${sha}^{commit}`], { cwd }).ok,
    )
  return { mergeBase, readable: Boolean(readable) }
}

/** The gate's tree-aware path classification; no failures escape as exceptions. */
export function treeAreaFacts(files, policy, options = {}) {
  const {
    baseSha,
    headSha,
    run = spawnSync,
    cwd,
    trustedRoot,
    discover = discoverExtensions,
    env = process.env,
    pr,
  } = options
  const paths = files.flatMap((file) =>
    typeof file === 'string' ? [file] : [file.filename, file.previous_filename].filter(Boolean),
  )
  const { mergeBase, readable } = schemaTrees({ baseSha, headSha, run, cwd })
  const dbtRoots = readable ? dbtProjectRoots({ run, cwd, shas: [mergeBase, headSha], paths }) : []
  const facts = pathParkFacts(files, policy, { ...options, dbtRoots })
  const areas = facts.areas.map((area) => {
    if (area.source !== 'builtin' || area.area !== 'api-contract') return area
    const schema = apiSchemaChange({ run, cwd, mergeBase, headSha, paths: area.paths, env })
    const input = { ...area, change: schema.change, changeReasons: schema.reasons }
    const unavailable = () => ({
      ...input,
      change: 'breaking',
      changeReasons: bounded([...schema.reasons, 'describers unavailable']),
    })
    if (!trustedRoot) return unavailable()
    try {
      const discovery = discover({ core: 'boss-verify', role: 'proof-describe', root: trustedRoot })
      const outcomes = runDescribers({
        extensions: discovery.extensions,
        areas: [area],
        baseSha: mergeBase ?? baseSha,
        headSha,
        pr,
        cwd,
        env,
        run,
      })
      return describeAreas([input], outcomes).areas[0]
    } catch {
      return unavailable()
    }
  })
  return { ...facts, areas, dbtRoots }
}
