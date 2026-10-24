#!/usr/bin/env node
// guard-id: merging-doc-fresh

import fs from 'node:fs'
import path from 'node:path'
import { pathToFileURL } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { spliceRegion, renderDrift } from './generated-region.mjs'

const REPO_ROOT = path.resolve(import.meta.dirname, '../..')
const DEFAULT_DOC_FILE = 'docs/philosophy/merging.md'
const UPDATE_COMMAND = 'make merging-doc-update'
const SETTING_MEANINGS = {
  canAutoMerge: 'marks a passing draft PR ready for review; does not merge',
  canAutoMergeDependabot: 'lets Dependabot PRs auto-merge',
}
/** @typedef {{shared: readonly string[], session: readonly string[], human: readonly string[], alwaysHumanPaths: string[], humanReview: Record<string, string[]>, settings: Record<string, boolean | undefined>}} Sources */

/** @param {string} repoRoot @returns {Promise<Sources>} */
export async function loadSources(repoRoot) {
  const load = (file) => import(pathToFileURL(path.join(repoRoot, file)).href)
  const [shared, session, human, config] = await Promise.all([
    load('skills-toolbox/epic/merge-eligibility.mjs'),
    load('skills-toolbox/build/completion-gate.mjs'),
    load('skills-toolbox/verify/verify-gate.mjs'),
    load('skills-toolbox/skill-config.mjs'),
  ])
  const policy = config.loadSkillConfig({ cwd: repoRoot })
  const project = JSON.parse(fs.readFileSync(path.join(repoRoot, '.boss-project.json'), 'utf8'))
  return {
    shared: shared.SHARED_INELIGIBLE_REASONS,
    session: session.INELIGIBLE_REASONS,
    human: human.HUMAN_REASONS,
    alwaysHumanPaths: config.verifyAlwaysHumanPaths(policy),
    humanReview: config.verifyHumanReviewPaths(policy),
    settings: project.repo?.settings ?? {},
  }
}
const list = (items) =>
  items.length ? [...new Set(items)].map((item) => `- \`${item}\``).join('\n') : 'none'
/** @param {Sources} sources */
export function renderRegion(sources) {
  const group = (title, source, symbol, items) => [
    `## ${title}`,
    '',
    `Source: \`${source}\`, \`${symbol}\`.`,
    '',
    list(items),
    '',
  ]
  return [
    '<!-- prettier-ignore-start -->',
    ...group(
      'Every merge path',
      'skills-toolbox/epic/merge-eligibility.mjs',
      'SHARED_INELIGIBLE_REASONS',
      sources.shared,
    ),
    ...group(
      'Session merges also check',
      'skills-toolbox/build/completion-gate.mjs',
      'INELIGIBLE_REASONS',
      sources.session.filter((reason) => !sources.shared.includes(reason)),
    ),
    ...group(
      'Verify park codes',
      'skills-toolbox/verify/verify-gate.mjs',
      'HUMAN_REASONS',
      sources.human,
    ),
    '## Human-review paths',
    '',
    'Source: `skills-toolbox/skill-config.mjs`, `verifyAlwaysHumanPaths` and `verifyHumanReviewPaths`, reading `.boss-skills.json`.',
    '',
    '### alwaysHumanPaths',
    '',
    list(sources.alwaysHumanPaths),
    '',
    ...Object.entries(sources.humanReview).flatMap(([category, globs]) => [
      `### ${category}`,
      '',
      list(globs),
      '',
    ]),
    '## Repo settings',
    '',
    'Source: `.boss-project.json`, `repo.settings`.',
    '',
    ...Object.entries(SETTING_MEANINGS).map(
      ([key, meaning]) =>
        `- \`${key}\`: ${sources.settings[key] === undefined ? 'unset (false)' : sources.settings[key]} — ${meaning}.`,
    ),
    '<!-- prettier-ignore-end -->',
    '',
  ].join('\n')
}

/** @param {{repoRoot?: string, docFile?: string, sources?: Sources, mode: 'write' | 'check'}} options */
export async function mergingDoc({
  repoRoot = REPO_ROOT,
  docFile = DEFAULT_DOC_FILE,
  sources,
  mode,
}) {
  const file = path.resolve(repoRoot, docFile)
  let actual, expected
  try {
    actual = fs.readFileSync(file, 'utf8')
    expected = spliceRegion(
      actual,
      renderRegion(sources ?? (await loadSources(repoRoot))),
      'merge-reasons',
      UPDATE_COMMAND,
    )
  } catch (error) {
    return { ok: false, messages: [`${docFile}: ${error.message}`] }
  }
  if (mode === 'write') {
    if (actual !== expected) fs.writeFileSync(file, expected)
    return { ok: true, messages: [] }
  }
  return actual === expected
    ? { ok: true, messages: [] }
    : {
        ok: false,
        messages: [renderDrift({ actual, expected, label: docFile }, UPDATE_COMMAND)],
      }
}

if (isMainModule(import.meta.url)) {
  let values
  try {
    ;({ values } = parseArgs({
      options: {
        write: { type: 'boolean', default: false },
        check: { type: 'boolean', default: false },
        'doc-file': { type: 'string', default: DEFAULT_DOC_FILE },
      },
    }))
    if (values.write === values.check) throw new Error('choose exactly one mode')
  } catch (error) {
    process.stderr.write(
      `usage: node scripts/guards/merging-doc.mjs (--write | --check) [--doc-file <path>]\n${error.message}\n`,
    )
    process.exit(2)
  }
  const { ok, messages } = await mergingDoc({
    docFile: values['doc-file'],
    mode: values.write ? 'write' : 'check',
  })
  for (const message of messages)
    process.stderr.write(message.endsWith('\n') ? message : `${message}\n`)
  process.exit(ok ? 0 : 1)
}
