#!/usr/bin/env node
import { readFileSync, writeFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import {
  loadSkillConfig,
  markdownH2Heading,
  planDescriptionSections,
  planSectionsForDescriptionMode,
  scanFences,
} from '../skill-config.mjs'

/** Fold a contract section without touching the terminal original-notes snapshot. */
export function foldSection(config, markdown, heading, body) {
  if (!body.trim()) throw new Error('Section body must not be blank')
  const contract = planSectionsForDescriptionMode(config, 'child-plan')
  const canonicalHeading = (raw) => planDescriptionSections(config, raw)[0]?.heading
  const canonical = canonicalHeading(heading)
  const rank = contract.findIndex((section) => section.heading === canonical)
  if (rank < 0 || rank === contract.length - 1) {
    throw new Error('Heading must name a non-terminal contract section')
  }
  const headings = []
  const offsets = []
  let offset = 0
  for (const line of markdown.split('\n')) {
    offsets.push(offset)
    offset += line.length + 1
  }
  for (const { line, index } of scanFences(markdown).lines) {
    const raw = markdownH2Heading(line)
    if (!raw) continue
    const name = canonicalHeading(raw)
    headings.push({ heading: name, offset: offsets[index] })
    if (name === contract[contract.length - 1].heading) break
  }
  const wrapper = headings.at(-1)
  if (wrapper?.heading !== contract[contract.length - 1].heading) {
    throw new Error('Missing unfenced original-notes wrapper')
  }
  const existingIndex = headings.findIndex((section) => section.heading === canonical)
  const existing = headings[existingIndex]
  const next = existing
    ? headings[existingIndex + 1]
    : headings.find(
        (section) => contract.findIndex((entry) => entry.heading === section.heading) > rank,
      )
  const start = existing?.offset ?? next.offset
  const prefix = markdown.slice(0, start)
  const separator = prefix.endsWith('\n\n') || !prefix ? '' : prefix.endsWith('\n') ? '\n' : '\n\n'
  return {
    text: prefix + separator + `${canonical}\n\n${body.trim()}\n\n` + markdown.slice(next.offset),
    action: existing ? 'replaced' : 'inserted',
    before: next.heading,
  }
}

export function main(args = process.argv.slice(2)) {
  const { values, positionals } = parseArgs({
    args,
    strict: true,
    allowPositionals: true,
    options: {
      heading: { type: 'string' },
      'body-file': { type: 'string' },
      plan: { type: 'string' },
      description: { type: 'string' },
      json: { type: 'boolean' },
    },
  })
  const command = positionals[0]
  if (
    positionals.length !== 1 ||
    command !== 'fold' ||
    !values.heading ||
    !values['body-file'] ||
    !values.plan ||
    !values.description
  ) {
    throw new Error(
      'Usage: plan-fold-section.mjs fold --heading <heading> --body-file <file> --plan <file> --description <file> [--json]',
    )
  }
  const config = loadSkillConfig()
  const body = readFileSync(values['body-file'], 'utf8')
  const plan = foldSection(config, readFileSync(values.plan, 'utf8'), values.heading, body)
  const description = foldSection(
    config,
    readFileSync(values.description, 'utf8'),
    values.heading,
    body,
  )
  writeFileSync(values.plan, plan.text)
  writeFileSync(values.description, description.text)
  const result = {
    plan: { action: plan.action, before: plan.before },
    description: { action: description.action, before: description.before },
  }
  if (values.json) process.stdout.write(`${JSON.stringify(result)}\n`)
}

if (isMainModule(import.meta.url)) {
  try {
    main()
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
