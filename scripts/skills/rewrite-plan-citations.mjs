#!/usr/bin/env node
import fs from 'node:fs'
import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'

export const GENERATED = [
  'lib/bossalib/gen/',
  'services/web/src/gen/',
  'services/docs/openapi/',
  '.codex/skills/',
  'lib/bossalib/contract/plugin/skillpayload/skills/',
]
export const INTENTIONAL = [
  ['services/bossd/internal/db/db_test.go', 'opaque database fixture'],
  ['services/bossd/internal/db/workflow_store_test.go', 'opaque database fixture'],
  ['services/bossd/internal/plugin/repair_chat_visibility_test.go', 'opaque session fixture'],
  ['services/bossd/internal/session/finalize_test.go', 'opaque finalize fixture'],
  ['lib/bossalib/contract/plugin/statusdetect/testdata/', 'recorded terminal captures'],
  ['plugins/bossd-plugin-codex/testdata/', 'recorded terminal captures'],
  ['services/marketing/public/screenshots/', 'recorded terminal captures'],
  ['scripts/guards/check-skill-shell.test.mjs', 'shell scanner fixture'],
  ['scripts/skills/sync-codex-skills.mjs', 'multi-segment path example'],
  ['scripts/skills/sync-codex-skills.test.mjs', 'multi-segment path fixture'],
  ['skills-toolbox/plan/plan-contract-guard.test.mjs', 'plan path fixture'],
  ['skills-toolbox/plan/plan-scratch-paths.test.mjs', 'plan path fixture'],
  ['skills-toolbox/plan/plan-deps-lib.test.mjs', 'plan path fixture'],
  ['scripts/skills/bs-plan-ce-skill.test.mjs', 'peer plan cleanup fixtures'],
  ['.claude/skills/boss-plan-compound-engineering/SKILL.md', 'upstream CE default path'],
  [
    'services/bosso/migrations_postgres/000004_users_email_unique.sql',
    'applied migration is immutable',
  ],
  ['experiments/', 'owned by BOS-1716'],
  ['scripts/skills/rewrite-plan-citations.mjs', 'codemod matching rules and exclusions'],
  ['scripts/skills/rewrite-plan-citations.test.mjs', 'codemod fixture inputs'],
]
const DELETED = [
  'docs/plans/',
  'plans/',
  'docs/seo/',
  'docs/analysis/',
  'docs/assets/',
  'docs/testing/bos-371-fast-local-dev-proof.md',
  'docs/security/audit-2026-bos-34-web.md',
]
const matches = (file, entry) => (entry.endsWith('/') ? file.startsWith(entry) : file === entry)
export function exclusionFor(file) {
  if (GENERATED.some((entry) => matches(file, entry)))
    return { kind: 'GENERATED', reason: 'regenerated from source' }
  const intentional = INTENTIONAL.find(([entry]) => matches(file, entry))
  if (intentional) return { kind: 'INTENTIONAL', reason: intentional[1] }
  if (DELETED.some((entry) => matches(file, entry)))
    return { kind: 'DELETED', reason: 'removed by this cleanup' }
  return null
}

// Match a citation's path, optional surrounding code ticks, and line range.
const TOKEN = '`?(?:\\.\\./|/)*docs/plans/[^\\s`),;\\]"<>]+(?::\\d+(?:-\\d+)?)?`?'
const tokenRegex = (flags) => new RegExp(TOKEN, flags)
export function rewriteCitations(source) {
  let rewritten = source.replace(tokenRegex('g'), (token) => {
    const id = token.match(/\bBOS-\d+/i)
    // A period ending a sentence is punctuation, rather than part of the path.
    return id ? id[0].toUpperCase() + (token.endsWith('.') ? '.' : '') : token
  })
  rewritten = rewritten.replace(/^(module:\s*)([^\n]*)/gm, (line, prefix, list) => {
    const entries = list.split(',').map((entry) => entry.trim())
    return entries.includes('docs/plans')
      ? prefix + entries.filter((entry) => entry !== 'docs/plans').join(', ')
      : line
  })
  const citation = '`?(?:\\.\\./|/)*docs/plans/[^\\s`),;\\]"<>]+?\\.md(?::\\d+(?:-\\d+)?)?`?'
  return rewritten
    .split('\n')
    .map((line) => {
      if (new RegExp('^\\s*[-*]\\s+' + citation + '(?:\\s+[—-]\\s+.*)?\\s*$').test(line))
        return null
      line = line.replace(
        new RegExp(' ?\\((?:(?:see|Plan:|Spec lives in)\\s+)?' + citation + '\\)', 'gi'),
        '',
      )
      line = line.replace(new RegExp('\\(' + citation + ',\\s*', 'g'), '(')
      line = line.replace(new RegExp('\\bSee\\s+' + citation + '(?:\\s+for\\s+[^.]*|)\\.', 'g'), '')
      line = line.replace(new RegExp(' (?:in|of) ' + citation + '(?=[,.;:]|\\s|$)', 'g'), '')
      return line.replace(/^(\s*\/\/)\.(?=\s|$)/, '$1')
    })
    .filter((line) => line !== null)
    .join('\n')
}

export function main(args = process.argv.slice(2)) {
  const { values } = parseArgs({
    args,
    strict: true,
    options: { write: { type: 'boolean' }, check: { type: 'boolean' } },
  })
  if (values.write && values.check) throw new Error('--write and --check are mutually exclusive')
  const files = execFileSync('git', ['ls-files', '-z'], { encoding: 'utf8' })
    .split('\0')
    .filter(Boolean)
  let remaining = 0
  for (const file of files) {
    const exclusion = exclusionFor(file)
    if (exclusion || !fs.existsSync(file)) {
      if (
        values.check &&
        exclusion?.kind === 'INTENTIONAL' &&
        fs.existsSync(file) &&
        fs.readFileSync(file, 'utf8').includes('docs/plans/')
      )
        console.log(`INTENTIONAL ${file} — ${exclusion.reason}`)
      continue
    }
    const original = fs.readFileSync(file, 'utf8')
    if (!original.includes('docs/plans')) continue
    const result = rewriteCitations(original)
    if (!values.check && result !== original) {
      console.log(`${values.write ? 'WRITE' : 'DRY RUN'} ${file}`)
      if (values.write) fs.writeFileSync(file, result)
    }
    // Check the original bytes in --check: a safely rewritable citation is still outstanding.
    const inspected = values.check ? original : result
    inspected.split('\n').forEach((line, index) => {
      if (line.includes('docs/plans/')) {
        remaining++
        console.error(`${file}:${index + 1}: ${line.trim()}`)
      }
    })
  }
  return values.check && remaining ? 1 : 0
}
if (isMainModule(import.meta.url)) process.exitCode = main()
