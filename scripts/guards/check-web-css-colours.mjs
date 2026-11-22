#!/usr/bin/env node

// Web stylesheets name their colours through design tokens (lib/ui-tokens and the web app's
// :root token layer in services/web/src/app/index.css), never a literal. A hex, rgb()/hsl()-style
// or named colour is allowed only as a custom property declared on :root (a token definition);
// anywhere else it counts against its file. The per-file counts in
// scripts/guards/web-css-colours-baseline.json only shrink.

import fs from 'node:fs'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { fileURLToPath } from 'node:url'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { cssDeclarations } from './css-declarations.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const BASELINE = 'scripts/guards/web-css-colours-baseline.json'
const SCAN = 'services/web/src'
const REMEDY =
  'use a colour token (var(--…)); add one to the :root token layer in services/web/src/app/index.css when none fits'

const HEX = /#[0-9a-f]{3,8}\b/gi
const FUNCTIONAL = /\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(/gi
// The CSS named colours, less transparent and currentcolor, which name no colour of their own.
const NAMED = new Set(
  `aliceblue antiquewhite aqua aquamarine azure beige bisque black blanchedalmond blue blueviolet
  brown burlywood cadetblue chartreuse chocolate coral cornflowerblue cornsilk crimson cyan darkblue
  darkcyan darkgoldenrod darkgray darkgreen darkgrey darkkhaki darkmagenta darkolivegreen darkorange
  darkorchid darkred darksalmon darkseagreen darkslateblue darkslategray darkslategrey darkturquoise
  darkviolet deeppink deepskyblue dimgray dimgrey dodgerblue firebrick floralwhite forestgreen
  fuchsia gainsboro ghostwhite gold goldenrod gray green greenyellow grey honeydew hotpink indianred
  indigo ivory khaki lavender lavenderblush lawngreen lemonchiffon lightblue lightcoral lightcyan
  lightgoldenrodyellow lightgray lightgreen lightgrey lightpink lightsalmon lightseagreen
  lightskyblue lightslategray lightslategrey lightsteelblue lightyellow lime limegreen linen magenta
  maroon mediumaquamarine mediumblue mediumorchid mediumpurple mediumseagreen mediumslateblue
  mediumspringgreen mediumturquoise mediumvioletred midnightblue mintcream mistyrose moccasin
  navajowhite navy oldlace olive olivedrab orange orangered orchid palegoldenrod palegreen
  paleturquoise palevioletred papayawhip peachpuff peru pink plum powderblue purple rebeccapurple red
  rosybrown royalblue saddlebrown salmon sandybrown seagreen seashell sienna silver skyblue slateblue
  slategray slategrey snow springgreen steelblue tan teal thistle tomato turquoise violet wheat white
  whitesmoke yellow yellowgreen`.split(/\s+/),
)
// Named colours are read only where a colour is the value; elsewhere a word such as `tan` or
// `red` could be an animation or grid-area name.
const COLOUR_PROPERTY =
  /^(?:color|background(?:-color)?|border(?:-(?:top|right|bottom|left|block|inline)(?:-(?:start|end))?)?(?:-color)?|outline(?:-color)?|box-shadow|text-shadow|fill|stroke|caret-color|accent-color|column-rule(?:-color)?|text-decoration(?:-color)?|scrollbar-color|--.*)$/

// A token reference names a token, not a colour: `var(--red)` is a token.
const TOKEN_REFERENCE = /--[\w-]+/g

function literalsIn(property, value) {
  const words = value.replace(TOKEN_REFERENCE, ' ').match(/[a-z]+/gi) ?? []
  const named = COLOUR_PROPERTY.test(property)
    ? words.filter((word) => NAMED.has(word.toLowerCase())).length
    : 0
  return (value.match(HEX) ?? []).length + (value.match(FUNCTIONAL) ?? []).length + named
}

/**
 * The colour literals a stylesheet spells out, outside the token layer.
 *
 * @param {string} css
 * @returns {{line: number, property: string}[]}
 */
export function colourLiterals(css) {
  return cssDeclarations(css)
    .filter((declaration) => !declaration.tokenDefinition)
    .flatMap(({ property, value, line }) =>
      Array.from({ length: literalsIn(property, value) }, () => ({ line, property })),
    )
}

/**
 * Compare per-file counts with the baseline: a count above its entry is a new literal, one
 * below it a stale entry to lower.
 */
export function compareBaseline(counts, baseline) {
  if (!baseline || Array.isArray(baseline) || typeof baseline !== 'object') {
    return [{ kind: 'invalid-baseline', message: `${BASELINE} must be a path-to-count object` }]
  }
  const failures = []
  const keys = Object.keys(baseline)
  if (keys.some((key, index) => index > 0 && keys[index - 1] > key)) {
    failures.push({ kind: 'invalid-baseline', message: `${BASELINE} must stay sorted` })
  }
  for (const [file, count] of Object.entries(baseline)) {
    if (!Number.isSafeInteger(count) || count <= 0) {
      failures.push({ kind: 'invalid-baseline', file, message: `${file}: count must be positive` })
    } else if ((counts[file] ?? 0) < count) {
      failures.push({
        kind: 'stale-entry',
        file,
        message: `${file}: ${counts[file] ?? 0} colour literals left, baseline ${count}; lower the entry (remove it at zero)`,
      })
    }
  }
  for (const [file, count] of Object.entries(counts)) {
    if (count > (baseline[file] ?? 0)) {
      failures.push({
        kind: 'new-instance',
        file,
        message: `${file}: ${count} colour literals exceed baseline ${baseline[file] ?? 0}; ${REMEDY}`,
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
  const counts = {}
  const lines = {}
  for (const file of files) {
    const found = colourLiterals(fs.readFileSync(path.join(repoRoot, file), 'utf8'))
    if (found.length) {
      counts[file] = found.length
      lines[file] = found.map((literal) => literal.line)
    }
  }
  return { counts, lines }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: { 'write-baseline': { type: 'boolean', default: false } },
    strict: true,
  })
  const { counts, lines } = scanStylesheets()
  const baselinePath = path.join(REPO_ROOT, BASELINE)
  if (values['write-baseline'])
    fs.writeFileSync(baselinePath, `${JSON.stringify(counts, null, 2)}\n`)
  const failures = compareBaseline(counts, JSON.parse(fs.readFileSync(baselinePath, 'utf8')))
  if (failures.length) {
    for (const failure of failures) {
      const at = failure.kind === 'new-instance' ? ` (lines ${lines[failure.file].join(', ')})` : ''
      console.error(`${failure.message}${at}`)
    }
    process.exitCode = 1
  } else {
    const total = Object.values(counts).reduce((sum, count) => sum + count, 0)
    console.log(
      `Web CSS colours OK (${total} baselined literals in ${Object.keys(counts).length} files)`,
    )
  }
}
