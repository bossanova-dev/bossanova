#!/usr/bin/env node
import { execFileSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { extensionRootsFor, globToRegExp, loadSkillConfig } from '../skill-config.mjs'
import { reviewPolicyFromBlock } from '../verify/verify-review-policy.mjs'
import { predictPathParks } from '../verify/verify-path-parks.mjs'
import { extractKeyChangeAreas } from './plan-deps-lib.mjs'

export const PREDICTED_PARK_MARKER = '- Predicted verify park:'
const HUMAN_TEXT = 'An agent can build this change, but verify will hold the merge for a person.'
const HUMAN_AGENT = '- Agent-friendly: needs-human (see "Why this needs a human")'
const GENERATED_AGENT = `${HUMAN_AGENT}\n<!-- predicted-verify-park-agent-friendly -->`

/** Resolve extracted areas against the index without losing case on planned new files. */
export function keyChangePaths(
  config,
  description,
  { trackedFiles = [], moduleRoots = [], rootFiles = [] } = {},
) {
  const { areas, pathSpecs = [] } = extractKeyChangeAreas(config, description, {
    moduleRoots,
    rootFiles,
    preservePaths: true,
  })
  const paths = pathSpecs.flatMap((spec) => {
    const area = spec.toLowerCase()
    const glob = spec.includes('*') ? globToRegExp(area) : null
    const matches = trackedFiles.filter((path) => {
      const lower = path.toLowerCase()
      return glob ? glob.test(lower) : lower === area || lower.startsWith(`${area}/`)
    })
    return matches.length ? matches : [spec]
  })
  return { areas, paths: [...new Set(paths)] }
}

export function predictVerifyPark(config, description, deps = {}) {
  const { areas, paths } = keyChangePaths(config, description, deps)
  const { codes, labels } = predictPathParks(
    paths,
    {
      review: reviewPolicyFromBlock(config.verifyDefaults),
    },
    { extensionRoots: extensionRootsFor(config), appliesScripts: [] },
  )
  return {
    predicted: codes.length > 0,
    codes,
    labels,
    areas,
    action: config.planDefaults?.predictedVerifyPark ?? 'rank-last',
  }
}

/** Reconcile generated annotation only before the untouched original-notes boundary. */
export function annotatePrediction(source, prediction) {
  const boundary = /^## Original notes\s*$/m.exec(source)?.index ?? source.length
  const original = source.slice(boundary)
  let content = source.slice(0, boundary)
  content = content.replace(/^- Predicted verify park:[^\r\n]*\r?\n/gm, '')
  content = content.replace(GENERATED_AGENT, '- Agent-friendly: yes')
  let legacyGeneratedAgent = false
  content = content.replace(
    /^## Why this needs a human[ \t]*\n([\s\S]*?)(?=^## |(?![\s\S]))/m,
    (section, body) => {
      if (!body.includes(HUMAN_TEXT)) return section
      const remaining = body
        .replace(`${HUMAN_TEXT}\n`, '')
        .replace(/^Predicted reasons:.*\n/m, '')
        .trim()
      legacyGeneratedAgent = !remaining && /^Predicted reasons:/m.test(body)
      return remaining ? `## Why this needs a human\n\n${remaining}\n\n` : ''
    },
  )
  if (legacyGeneratedAgent) content = content.replace(HUMAN_AGENT, '- Agent-friendly: yes')
  if (!prediction.predicted) return content + original
  const details = `${prediction.codes.join(', ')} · ${prediction.labels.join(', ')} (from ${prediction.areas.join(', ')})`
  if (!/^## Planning\s*$/m.test(content)) content = `${content.trimEnd()}\n\n## Planning\n\n`
  content = content.replace(
    /(^## Planning[ \t]*\n)([\s\S]*?)(?=^## |(?![\s\S]))/m,
    (_, heading, body) =>
      `${heading}\n${body.trim() ? `${body.trim()}\n` : ''}${PREDICTED_PARK_MARKER} ${details}\n\n`,
  )
  if (prediction.action === 'needs-human') {
    const human = `## Why this needs a human\n\n${HUMAN_TEXT}\nPredicted reasons: ${details}.\n\n`
    if (/^## Why this needs a human\s*$/m.test(content)) {
      content = content.replace(
        /(^## Why this needs a human\s*\n)/m,
        (_, heading) => `${heading}\n${HUMAN_TEXT}\nPredicted reasons: ${details}.\n`,
      )
    } else content = content.replace(/^## Planning/m, () => `${human}## Planning`)
    content = content.replace(/^- Agent-friendly: yes[ \t]*$/m, GENERATED_AGENT)
  }
  return content + original
}

function main() {
  const { values, positionals } = parseArgs({
    allowPositionals: true,
    strict: true,
    options: {
      description: { type: 'string' },
      plan: { type: 'string' },
      json: { type: 'boolean' },
    },
  })
  const command = positionals[0]
  if (positionals.length !== 1 || command !== 'annotate' || !values.description || !values.plan)
    throw new Error(
      'usage: plan-park-prediction.mjs annotate --description <file> --plan <file> [--json]',
    )
  const config = loadSkillConfig()
  const description = readFileSync(values.description, 'utf8')
  const trackedFiles = execFileSync('git', ['ls-files', '-z'], { encoding: 'utf8' })
    .split('\0')
    .filter(Boolean)
  const prediction = predictVerifyPark(config, description, {
    trackedFiles,
    rootFiles: trackedFiles.filter((path) => !path.includes('/')),
  })
  for (const path of new Set([values.description, values.plan])) {
    const source = readFileSync(path, 'utf8')
    const updated = annotatePrediction(source, prediction)
    if (source !== updated) writeFileSync(path, updated)
  }
  const { predicted, codes, labels, action } = prediction
  process.stdout.write(`${JSON.stringify({ predicted, codes, labels, action })}\n`)
}
if (isMainModule(import.meta.url)) main()
