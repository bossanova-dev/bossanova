#!/usr/bin/env node

// Guardrail: `any` must not silently re-enter the TypeScript codebase through a
// suppression comment or an untyped cast. Biome's `noExplicitAny` rule already
// errors on bare `any` in normal code (services/web/biome.json), and tsconfig
// `strict` rejects it at compile time — but a file-level
// `// biome-ignore-all lint/suspicious/noExplicitAny: …` comment silences the
// rule for a whole file, and behind it `x as any` casts compile freely. BOS-335
// removed every such suppression + cast from services/web and services/marketing;
// this gate stops them coming back. It fails when, under services/web/src or
// services/marketing/src, it finds either (a) a `biome-ignore` /
// `biome-ignore-all` comment naming `noExplicitAny`, or (b) a literal, word-
// bounded `as any` cast. It deliberately does NOT ban unrelated `biome-ignore`
// comments (a legitimate escape hatch for ~140 other-rule suppressions), nor
// `as unknown` / `anyOf` / identifiers that merely contain "any".
//
// `make lint` (golangci-lint per Go module + docs) does not run web/marketing
// biome, so this gate is wired into the TS CI workflows (test-web.yml,
// test-marketing.yml) after their Lint step.
//
// Exercised by scripts/check-no-explicit-any.test.mjs and runnable via
// `node scripts/check-no-explicit-any.mjs [root...]`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const scriptDir = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(scriptDir, '..')

// Default scan roots: the two TypeScript source trees where `any` is forbidden.
const DEFAULT_ROOTS = [
  path.join(repoRoot, 'services/web/src'),
  path.join(repoRoot, 'services/marketing/src'),
]

// Only these directory names are pruned during the walk — generated/vendored
// trees that we neither own nor lint by hand.
const SKIP_DIRS = new Set(['node_modules', 'gen', 'dist', '.astro'])

// Source extensions worth scanning; skips images, fonts, etc.
const SCAN_EXTENSIONS = new Set(['.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs', '.astro', '.vue'])

// This gate's own files legitimately contain the literal `as any` / `noExplicitAny`
// tokens (in strings and regexes); never flag them if a root points at scripts/.
const OWN_FILES = new Set(['check-no-explicit-any.mjs', 'check-no-explicit-any.test.mjs'])

// Word-bounded `as any` so identifiers like `asanyfoo`, `as anyOf`, and
// `as unknown` are not false-matched.
const AS_ANY_RE = /\bas any\b/

function walk(dir, files) {
  let entries
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true })
  } catch {
    return
  }
  for (const entry of entries) {
    if (entry.isDirectory()) {
      if (SKIP_DIRS.has(entry.name)) continue
      walk(path.join(dir, entry.name), files)
    } else if (entry.isFile()) {
      if (OWN_FILES.has(entry.name)) continue
      if (!SCAN_EXTENSIONS.has(path.extname(entry.name))) continue
      files.push(path.join(dir, entry.name))
    }
  }
}

// Scan the given roots and return an array of offenders:
// { file, line, rule } where rule is 'noExplicitAny suppression' or 'as any cast'.
export function scanForExplicitAny(roots = DEFAULT_ROOTS) {
  const offenders = []

  for (const root of roots) {
    if (!fs.existsSync(root)) continue

    const files = []
    walk(root, files)

    for (const file of files.sort()) {
      const contents = fs.readFileSync(file, 'utf8')
      const lines = contents.split('\n')
      for (let i = 0; i < lines.length; i += 1) {
        const line = lines[i]
        const lineNumber = i + 1

        if (line.includes('biome-ignore') && line.includes('noExplicitAny')) {
          offenders.push({ file, line: lineNumber, rule: 'noExplicitAny suppression' })
        }
        if (AS_ANY_RE.test(line)) {
          offenders.push({ file, line: lineNumber, rule: 'as any cast' })
        }
      }
    }
  }

  return offenders
}

export function checkNoExplicitAny(roots = DEFAULT_ROOTS) {
  const offenders = scanForExplicitAny(roots)

  if (offenders.length > 0) {
    console.error('Found forbidden `any` escape hatches (BOS-335):')
    for (const { file, line, rule } of offenders) {
      console.error(`  - ${path.relative(repoRoot, file)}:${line} — ${rule}`)
    }
    console.error(
      'Remedy: type the fake with Partial<...> instead of `as any`, and remove the ' +
        '`noExplicitAny` suppression — do not swap in `as unknown as T`.',
    )
    return false
  }

  console.log('OK: no noExplicitAny suppressions or `as any` casts')
  return true
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)

if (invokedDirectly) {
  const roots = process.argv.slice(2)
  if (!checkNoExplicitAny(roots.length > 0 ? roots : DEFAULT_ROOTS)) process.exit(1)
}
