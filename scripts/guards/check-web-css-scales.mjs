#!/usr/bin/env node

// Web stylesheets take radii, type, spacing, sizes, strokes, shadows, motion and stacking from
// the scale tokens in lib/ui-tokens/app.css, never a literal, so one edit there retunes the app.
// A custom property declared on :root is a token definition and may hold a literal. Zero, auto,
// keywords, percentages, fr and viewport units are layout, not scale steps, and always pass. The
// per-file, per-category counts in scripts/guards/web-css-scales-baseline.json only shrink.
//   node scripts/guards/check-web-css-scales.mjs              # the ratchet
//   node scripts/guards/check-web-css-scales.mjs --inventory  # every literal by category

import fs from 'node:fs'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { fileURLToPath } from 'node:url'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { cssDeclarations } from './css-declarations.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const BASELINE = 'scripts/guards/web-css-scales-baseline.json'
const SCAN = 'services/web/src'
const REMEDY =
  'use a scale token (var(--r-*), --fs-*, --sp-*, --h-ctl-*, --icon-*, --dur-*, --z-*, …) from lib/ui-tokens/app.css; add a step there when none fits'

const SIDES = '(?:-(?:top|right|bottom|left|block|inline)(?:-(?:start|end))?)?'
/** Property name to scale category; the first match wins. */
const CATEGORIES = new Map([
  ['radius', /^border(?:-(?:top|bottom|start|end)-(?:left|right|start|end))?-radius$/],
  ['font-size', /^font-size$/],
  ['font-weight', /^font-weight$/],
  ['line-height', /^line-height$/],
  ['letter-spacing', /^letter-spacing$/],
  [
    'space',
    new RegExp(
      `^(?:(?:padding|margin|scroll-padding|scroll-margin)${SIDES}|(?:row-|column-)?gap|inset${SIDES}|top|right|bottom|left)$`,
    ),
  ],
  [
    'size',
    /^(?:(?:min-|max-)?(?:width|height|inline-size|block-size)|flex(?:-basis)?|grid-template-(?:columns|rows)|grid-auto-(?:columns|rows))$/,
  ],
  [
    'stroke',
    new RegExp(`^(?:border${SIDES}(?:-width)?|outline(?:-width|-offset)?|column-rule-width)$`),
  ],
  ['shadow', /^box-shadow$/],
  ['motion', /^(?:transition|animation)(?:-(?:duration|delay|timing-function))?$/],
  ['z-index', /^z-index$/],
])

const CUSTOM_PROPERTY = /^-{2}/
const TOKEN_REFERENCE = /--[\w-]+/g
const COLOUR_FUNCTION =
  /\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color|color-mix)\((?:[^()]|\([^()]*\))*\)/gi
const NUMBER = /(?<![\w.#-])-?(?:\d*\.)?\d+(?:[a-z%]+)?(?![\w%(])/gi
// Easing keywords (ease-out, linear) are CSS vocabulary, like auto; a curve's numbers are a constant.
const EASING = /\b(?:cubic-bezier|steps)\([^)]*\)/gi
const LENGTH_UNITS = new Set(['px', 'em', 'rem'])
const TIME_UNITS = new Set(['s', 'ms'])

/** Value text with token names and colour functions blanked, offsets kept. */
function blankTokens(value) {
  const blank = (text) => text.replace(/[^\n]/g, ' ')
  return value.replace(COLOUR_FUNCTION, blank).replace(TOKEN_REFERENCE, blank)
}

/** A component-scoped custom property's category, read from its name. */
const CUSTOM_CATEGORIES = new Map([
  ['radius', /radius/],
  ['font-size', /font|-fs\b/],
  ['line-height', /line|-lh\b/],
  ['letter-spacing', /track|spacing/],
  ['space', /pad|gap|space|margin|inset|^--lp$/],
  ['stroke', /border|stroke/],
  ['motion', /dur|delay/],
])

/**
 * @param {Map<string, RegExp>} categories
 * @param {string} property
 */
function categoryOf(categories, property) {
  return [...categories].find(([, pattern]) => pattern.test(property))?.[0]
}

/** The kind of literal a number is under a category, or null when it is layout, not a step. */
function numberKind(category, text) {
  const [, magnitude, unit = ''] = /^(-?(?:\d*\.)?\d+)([a-z%]*)$/i.exec(text) ?? []
  if (magnitude === undefined || Number(magnitude) === 0) return null
  const u = unit.toLowerCase()
  if (category === 'motion') return TIME_UNITS.has(u) ? 'duration' : null
  if (category === 'z-index') return u === '' ? 'z-index' : null
  if (category === 'font-weight') return u === '' ? 'font-weight' : null
  if (category === 'line-height') return u === '' || LENGTH_UNITS.has(u) ? 'line-height' : null
  return LENGTH_UNITS.has(u) ? category : null
}

/** Literals in a `font` shorthand: weight, size and the line height after the slash. */
function fontShorthand(value, valueStart) {
  const found = []
  const slash = value.indexOf('/')
  for (const match of value.matchAll(NUMBER)) {
    const text = match[0]
    const at = match.index
    const unit = /[a-z%]*$/i.exec(text)[0].toLowerCase()
    let category = null
    if (slash !== -1 && at > slash) category = 'line-height'
    else if (unit === '' && /^[1-9]00$/.test(text)) category = 'font-weight'
    else if (LENGTH_UNITS.has(unit)) category = 'font-size'
    if (category) found.push({ category, text, start: valueStart + at })
  }
  return found
}

/**
 * The scale literals a stylesheet spells out, outside the token layer (or inside it too, with
 * `tokenDefinitions`).
 *
 * @param {string} css
 * @param {{tokenDefinitions?: boolean}} [options]
 * @returns {{line: number, selector: string, property: string, category: string, text: string, start: number,
 *   end: number, valueStart: number, value: string}[]}
 */
export function scaleLiterals(css, { tokenDefinitions = false } = {}) {
  const found = []
  for (const { property, value: raw, valueStart, line, tokenDefinition, stack } of cssDeclarations(
    css,
  )) {
    if (tokenDefinition && !tokenDefinitions) continue
    if (stack.some((prelude) => /^@keyframes\b/.test(prelude))) continue
    const value = blankTokens(raw)
    const literals = []
    if (property === 'font') {
      literals.push(...fontShorthand(value, valueStart))
    } else {
      const category = CUSTOM_PROPERTY.test(property)
        ? (categoryOf(CUSTOM_CATEGORIES, property) ?? 'size')
        : categoryOf(CATEGORIES, property)
      if (!category) continue
      for (const match of value.matchAll(NUMBER)) {
        const kind =
          numberKind(category, match[0]) ??
          (CUSTOM_PROPERTY.test(property) ? numberKind('motion', match[0]) : null)
        if (kind) literals.push({ category: kind, text: match[0], start: valueStart + match.index })
      }
      if (category === 'motion') {
        for (const match of value.matchAll(EASING)) {
          literals.push({ category: 'easing', text: match[0], start: valueStart + match.index })
        }
      }
    }
    const selector = stack.findLast((prelude) => !prelude.startsWith('@')) ?? ''
    for (const literal of literals) {
      found.push({
        line,
        selector,
        property,
        ...literal,
        end: literal.start + literal.text.length,
        valueStart,
        value: raw,
      })
    }
  }
  return found.sort((a, b) => a.start - b.start)
}

function countsOf(literals) {
  const counts = {}
  for (const { category } of literals) counts[category] = (counts[category] ?? 0) + 1
  return Object.fromEntries(Object.entries(counts).sort(([a], [b]) => (a < b ? -1 : 1)))
}

/**
 * Compare per-file, per-category counts with the baseline: a count above its entry is a new
 * literal, one below it a stale entry to lower.
 *
 * @param {Record<string, Record<string, number>>} counts
 * @param {unknown} baseline
 */
export function compareBaseline(counts, baseline) {
  if (!baseline || Array.isArray(baseline) || typeof baseline !== 'object') {
    return [{ kind: 'invalid-baseline', message: `${BASELINE} must be a path-to-counts object` }]
  }
  const failures = []
  const files = Object.keys(baseline)
  if (files.some((key, index) => index > 0 && files[index - 1] > key)) {
    failures.push({ kind: 'invalid-baseline', message: `${BASELINE} must stay sorted` })
  }
  const entries = (object) =>
    Object.entries(object).flatMap(([file, byCategory]) =>
      byCategory && typeof byCategory === 'object'
        ? Object.entries(byCategory).map(([category, count]) => ({ file, category, count }))
        : [{ file, category: '', count: byCategory }],
    )
  const baselined = (file, category) => baseline[file]?.[category] ?? 0
  for (const { file, category, count } of entries(baseline)) {
    if (!category || !Number.isSafeInteger(count) || count <= 0) {
      failures.push({
        kind: 'invalid-baseline',
        file,
        message: `${file}: each category count must be a positive integer`,
      })
    } else if ((counts[file]?.[category] ?? 0) < count) {
      failures.push({
        kind: 'stale-entry',
        file,
        category,
        message: `${file}: ${counts[file]?.[category] ?? 0} ${category} literals left, baseline ${count}; lower the entry (remove it at zero)`,
      })
    }
  }
  for (const { file, category, count } of entries(counts)) {
    if (count > baselined(file, category)) {
      failures.push({
        kind: 'new-instance',
        file,
        category,
        message: `${file}: ${count} ${category} literals exceed baseline ${baselined(file, category)}; ${REMEDY}`,
      })
    }
  }
  return failures
}

export function scanStylesheets(repoRoot = REPO_ROOT) {
  const files = execFileSync('git', ['ls-files', '-z', '--', `${SCAN}/*.css`], {
    cwd: repoRoot,
    encoding: 'utf8',
  })
    .split('\0')
    .filter(Boolean)
    .sort()
  /** @type {Record<string, Record<string, number>>} */
  const counts = {}
  /** @type {Record<string, ReturnType<typeof scaleLiterals>>} */
  const literals = {}
  for (const file of files) {
    const found = scaleLiterals(fs.readFileSync(path.join(repoRoot, file), 'utf8'))
    if (found.length) {
      counts[file] = countsOf(found)
      literals[file] = found
    }
  }
  return { counts, literals }
}

function printInventory(literals) {
  const byCategory = {}
  for (const [file, found] of Object.entries(literals)) {
    for (const literal of found) {
      const values = (byCategory[literal.category] ??= {})
      ;(values[literal.text] ??= []).push(`${file.slice(SCAN.length + 1)}:${literal.line}`)
    }
  }
  for (const [category, values] of Object.entries(byCategory).sort()) {
    const total = Object.values(values).reduce((sum, at) => sum + at.length, 0)
    console.log(`\n${category}: ${total}`)
    for (const [text, at] of Object.entries(values).sort((a, b) => b[1].length - a[1].length)) {
      console.log(`  ${String(at.length).padStart(4)}  ${text}  ${at.join(' ')}`)
    }
  }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: {
      'write-baseline': { type: 'boolean', default: false },
      inventory: { type: 'boolean', default: false },
    },
    strict: true,
  })
  const { counts, literals } = scanStylesheets()
  if (values.inventory) {
    printInventory(literals)
  } else {
    const baselinePath = path.join(REPO_ROOT, BASELINE)
    if (values['write-baseline'])
      fs.writeFileSync(baselinePath, `${JSON.stringify(counts, null, 2)}\n`)
    const failures = compareBaseline(counts, JSON.parse(fs.readFileSync(baselinePath, 'utf8')))
    if (failures.length) {
      for (const failure of failures) {
        const at =
          failure.kind === 'new-instance'
            ? ` (lines ${literals[failure.file]
                .filter((literal) => literal.category === failure.category)
                .map((literal) => literal.line)
                .join(', ')})`
            : ''
        console.error(`${failure.message}${at}`)
      }
      process.exitCode = 1
    } else {
      const total = Object.values(counts)
        .flatMap((byCategory) => Object.values(byCategory))
        .reduce((sum, count) => sum + count, 0)
      console.log(
        `Web CSS scales OK (${total} baselined literals in ${Object.keys(counts).length} files)`,
      )
    }
  }
}
