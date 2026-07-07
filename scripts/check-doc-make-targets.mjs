#!/usr/bin/env node

// Guardrail: every `make <target>` documented in the project instructions must
// resolve to a target the Makefile actually defines. The Makefile is a churn
// hotspot; without this check a target rename or removal silently breaks the
// `make` commands documented for contributors and AI agents in CLAUDE.md,
// AGENTS.md, README.md, and the checked-in agent skills under the source skill
// roots (whose SKILL.md files tell agents to run targets like `make
// codex-skills-check` and `make mutate-survivors`). Generated Codex skill copies
// under .codex are excluded: they are derived from .claude/skills by
// scripts/sync-codex-skills.mjs.
//
// Exercised by scripts/check-doc-make-targets.test.mjs and runnable via
// `node scripts/check-doc-make-targets.mjs`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// Docs whose `make` commands must stay valid.
const DOC_FILES = ['CLAUDE.md', 'AGENTS.md', 'README.md']
const SKILL_DOC_ROOTS = [
  '.claude/skills',
  'plugins/bossd-plugin-claude/skilldata/skills',
  'services/boss/internal/skillinstall/skills',
]

// The Makefile generates per-plugin targets via `$(call define-plugin-<verb>)`
// foreach loops. Map each macro to the prefix of the target it instantiates so
// documented targets like `make test-claude` resolve without hand-listing them.
const PLUGIN_MACRO_PREFIXES = {
  'define-plugin-build': 'build-',
  'define-plugin-lint': 'lint-',
  'define-plugin-mutate': 'mutate-',
  'define-plugin-test': 'test-',
}

const MAKE_OPTIONS_WITH_VALUES = new Set([
  '-C',
  '--directory',
  '-f',
  '--file',
  '--makefile',
  '-I',
  '--include-dir',
])

function normalizeMakeDirectory(directory, value) {
  if (!value || path.isAbsolute(value)) return value || directory
  return path.normalize(path.join(directory, value))
}

function normalizeMakefilePath(directory, value) {
  if (!value) return path.join(directory, 'Makefile')
  if (path.isAbsolute(value)) return value
  return path.normalize(path.join(directory, value))
}

// Pull `make <target>` invocations out of Markdown code (fenced blocks and
// inline backtick spans) while ignoring prose, so "make sure" never registers
// as a target. A bare `make` with no target (the default build) is skipped.
export function extractDocumentedMakeInvocations(markdown) {
  const invocations = []
  const addFromCode = (code) => {
    for (const match of code.matchAll(/\bmake(?:\s+([^\n#;&|]+))?/g)) {
      const goals = match[1]
      if (!goals) continue

      let skipNextOptionValue = false
      let optionNeedingValue = ''
      let directory = '.'
      let explicitMakefile = ''
      let makefile = normalizeMakefilePath(directory, explicitMakefile)
      const targets = []
      for (const goal of goals.trim().split(/\s+/)) {
        if (skipNextOptionValue) {
          if (optionNeedingValue === '-C' || optionNeedingValue === '--directory') {
            directory = normalizeMakeDirectory(directory, goal)
            makefile = normalizeMakefilePath(directory, explicitMakefile)
          } else if (
            optionNeedingValue === '-f' ||
            optionNeedingValue === '--file' ||
            optionNeedingValue === '--makefile'
          ) {
            explicitMakefile = goal
            makefile = normalizeMakefilePath(directory, explicitMakefile)
          }
          skipNextOptionValue = false
          optionNeedingValue = ''
          continue
        }

        if (/^[A-Za-z_][A-Za-z0-9_]*=/.test(goal)) continue
        if (goal.startsWith('--directory=')) {
          directory = normalizeMakeDirectory(directory, goal.slice('--directory='.length))
          makefile = normalizeMakefilePath(directory, explicitMakefile)
          continue
        }
        if (goal.startsWith('--file=')) {
          explicitMakefile = goal.slice('--file='.length)
          makefile = normalizeMakefilePath(directory, explicitMakefile)
          continue
        }
        if (goal.startsWith('--makefile=')) {
          explicitMakefile = goal.slice('--makefile='.length)
          makefile = normalizeMakefilePath(directory, explicitMakefile)
          continue
        }
        if (/^-C.+/.test(goal)) {
          directory = normalizeMakeDirectory(directory, goal.slice('-C'.length))
          makefile = normalizeMakefilePath(directory, explicitMakefile)
          continue
        }
        if (/^-f.+/.test(goal)) {
          explicitMakefile = goal.slice('-f'.length)
          makefile = normalizeMakefilePath(directory, explicitMakefile)
          continue
        }
        if (/^-I.+/.test(goal)) {
          continue
        }
        if (/^-/.test(goal)) {
          skipNextOptionValue = MAKE_OPTIONS_WITH_VALUES.has(goal)
          optionNeedingValue = goal
          continue
        }

        if (/^[a-z][a-z0-9-]*$/.test(goal)) targets.push(goal)
      }

      for (const target of targets) {
        invocations.push({ directory, makefile, target })
      }
    }
  }

  let inFence = false
  for (const line of markdown.split('\n')) {
    if (/^\s*```/.test(line)) {
      inFence = !inFence
      continue
    }

    if (inFence) {
      addFromCode(line)
      continue
    }

    for (const match of line.matchAll(/`([^`]+)`/g)) {
      addFromCode(match[1])
    }
  }

  return invocations
}

export function extractDocumentedTargets(markdown) {
  return new Set(extractDocumentedMakeInvocations(markdown).map(({ target }) => target))
}

function wildcardConditionActive(line, baseDir) {
  if (!baseDir) return true

  const condition = /^\s*(ifn?eq)\s*\(\s*\$\(wildcard\s+([^)]+)\)\s*,\s*([^)]*)\s*\)/.exec(line)
  if (!condition) return true

  const [, directive, pattern, expected] = condition
  const normalizedPattern = pattern.trim()
  const expectedValue = expected.trim()
  const exists = fs.existsSync(path.join(baseDir, normalizedPattern))
  const wildcardValue = exists ? normalizedPattern : ''

  return directive === 'ifneq' ? wildcardValue !== expectedValue : wildcardValue === expectedValue
}

// Collect literal target names from a Makefile: every `.PHONY` entry (handling
// backslash line continuations) plus every explicit `name:` rule head. Variable
// assignments (`:=`), computed rules (`$(...)`), and pattern rules (`%`) are not
// concrete documentable targets and are excluded.
export function extractMakefileTargets(makefile, baseDir = undefined) {
  const targets = new Set()
  const lines = makefile.split('\n')
  const activeStack = []

  const isActive = () => activeStack.every(Boolean)

  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]

    if (/^\s*ifn?eq\b/.test(line)) {
      activeStack.push(isActive() && wildcardConditionActive(line, baseDir))
      continue
    }
    if (/^\s*endif\b/.test(line)) {
      activeStack.pop()
      continue
    }
    if (!isActive()) continue

    const phony = /^\.PHONY:\s*(.*)$/.exec(line)
    if (phony) {
      let names = phony[1]
      // Gather continuation lines joined with a trailing backslash.
      while (names.endsWith('\\')) {
        names = names.slice(0, -1)
        index += 1
        names += ` ${lines[index] ?? ''}`
      }
      for (const name of names.split(/\s+/)) {
        if (name) targets.add(name)
      }
      continue
    }

    const rule = /^([A-Za-z][A-Za-z0-9_.-]*):(?!=)/.exec(line)
    if (rule) {
      targets.add(rule[1])
    }
  }

  return targets
}

// Synthesize the per-plugin targets the Makefile generates via its
// `define-plugin-<verb>` macros, but only for macros actually present in the
// Makefile, so the expansion stays truthful to what make can build.
export function expandGeneratedTargets(makefile, pluginNames) {
  const targets = new Set()

  for (const [macro, prefix] of Object.entries(PLUGIN_MACRO_PREFIXES)) {
    if (!makefile.includes(`define ${macro}`)) continue
    for (const plugin of pluginNames) {
      targets.add(`${prefix}${plugin}`)
    }
  }

  return targets
}

// Extract the literal `<prefix>$(N):` target heads a `define <macro> ... endef`
// block instantiates (e.g. `debt-deadcode-$(2):` -> `debt-deadcode-`). Reading
// the prefixes from the macro body keeps the expansion truthful to the Makefile
// instead of hand-listing them. Returns [] when the macro is absent.
function macroTargetPrefixes(makefile, macro) {
  const start = makefile.indexOf(`define ${macro}`)
  if (start === -1) return []

  const end = makefile.indexOf('\nendef', start)
  const body = makefile.slice(start, end === -1 ? undefined : end)

  const prefixes = []
  for (const match of body.matchAll(/^([a-z][a-z0-9-]*)\$\([0-9]+\):/gm)) {
    prefixes.push(match[1])
  }
  return prefixes
}

// Synthesize the per-module debt scanners the Makefile generates via its
// `define-debt-targets` macro (`make debt-<kind>-<module>`), expanded over the
// module short names so documented examples like `make debt-deadcode-bossd`
// resolve without hand-listing every kind/module pair.
export function expandDebtTargets(makefile, moduleShortNames) {
  const targets = new Set()

  for (const prefix of macroTargetPrefixes(makefile, 'define-debt-targets')) {
    for (const module of moduleShortNames) {
      targets.add(`${prefix}${module}`)
    }
  }

  return targets
}

// Documented targets that have no matching Makefile target, sorted for stable
// output.
export function findUndefinedDocumentedTargets(documented, defined) {
  return [...documented].filter((target) => !defined.has(target)).sort()
}

function formatDocumentedMakeInvocation({ directory, makefile, target }) {
  const defaultMakefile = normalizeMakefilePath(directory)
  const directoryOption = directory === '.' ? '' : ` -C ${directory}`
  const makefileOption = makefile === defaultMakefile ? '' : ` -f ${makefile}`
  return `make${directoryOption}${makefileOption} ${target}`
}

function discoverPluginNames(repoRoot) {
  const pluginsDir = path.join(repoRoot, 'plugins')
  if (!fs.existsSync(pluginsDir)) return []

  return fs
    .readdirSync(pluginsDir, { withFileTypes: true })
    .filter(
      (entry) =>
        entry.isDirectory() &&
        entry.name.startsWith('bossd-plugin-') &&
        fs.existsSync(path.join(pluginsDir, entry.name, 'go.mod')),
    )
    .map((entry) => entry.name.replace(/^bossd-plugin-/, ''))
}

// The short module names the Makefile's debt macro iterates over, mirroring
// `MODULES := $(patsubst %/go.mod,%,$(wildcard lib/*/go.mod services/*/go.mod
// plugins/*/go.mod))` and its `$(patsubst bossd-plugin-%,%,$(notdir ...))`
// short-name transform.
export function discoverModuleShortNames(repoRoot) {
  const shortNames = new Set()

  for (const parent of ['lib', 'services', 'plugins']) {
    const parentDir = path.join(repoRoot, parent)
    if (!fs.existsSync(parentDir)) continue

    for (const entry of fs.readdirSync(parentDir, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue
      if (!fs.existsSync(path.join(parentDir, entry.name, 'go.mod'))) continue
      shortNames.add(entry.name.replace(/^bossd-plugin-/, ''))
    }
  }

  return [...shortNames]
}

// Find every checked-in source skill doc. These are agent-facing instructions,
// so their `make` commands must stay runnable just like the root docs. Returns
// absolute paths in stable (sorted) order. The .codex tree is intentionally not
// walked: those skills are generated copies.
export function discoverSkillDocs(repoRoot) {
  const docs = []
  const walk = (dir) => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const entryPath = path.join(dir, entry.name)
      if (entry.isDirectory()) {
        walk(entryPath)
      } else if (entry.isFile() && entry.name === 'SKILL.md') {
        docs.push(entryPath)
      }
    }
  }
  for (const root of SKILL_DOC_ROOTS.map((skillRoot) => path.join(repoRoot, skillRoot))) {
    if (fs.existsSync(root)) walk(root)
  }

  return docs.sort()
}

export function checkDocMakeTargets(repoRoot = process.cwd()) {
  const makefile = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')

  const defined = new Set([
    ...extractMakefileTargets(makefile, repoRoot),
    ...expandGeneratedTargets(makefile, discoverPluginNames(repoRoot)),
    ...expandDebtTargets(makefile, discoverModuleShortNames(repoRoot)),
  ])
  const definedByMakefile = new Map([[normalizeMakefilePath('.'), defined]])

  const getDefinedTargets = ({ directory, makefile }) => {
    if (definedByMakefile.has(makefile)) return definedByMakefile.get(makefile)

    const makefilePath = path.isAbsolute(makefile) ? makefile : path.join(repoRoot, makefile)
    const baseDir = path.dirname(makefilePath)
    const makefileTargets = fs.existsSync(makefilePath)
      ? extractMakefileTargets(
          fs.readFileSync(makefilePath, 'utf8'),
          baseDir || path.join(repoRoot, directory),
        )
      : new Set()
    definedByMakefile.set(makefile, makefileTargets)
    return makefileTargets
  }

  const docPaths = [
    ...DOC_FILES.map((docName) => path.join(repoRoot, docName)),
    ...discoverSkillDocs(repoRoot),
  ]

  const documented = []
  for (const docPath of docPaths) {
    if (!fs.existsSync(docPath)) continue
    for (const invocation of extractDocumentedMakeInvocations(fs.readFileSync(docPath, 'utf8'))) {
      documented.push(invocation)
    }
  }

  const undefinedTargets = documented
    .filter((invocation) => !getDefinedTargets(invocation).has(invocation.target))
    .map(formatDocumentedMakeInvocation)
    .sort()

  if (undefinedTargets.length > 0) {
    console.error('Docs reference make targets that the Makefile does not define:')
    for (const target of undefinedTargets) {
      console.error(`  - ${target}`)
    }
    console.error('Update the docs or the Makefile so documented commands stay runnable.')
    return false
  }

  console.log(`Doc make targets OK (${documented.length} documented targets checked)`)
  return true
}

const invokedDirectly =
  process.argv[1] &&
  fs.realpathSync(process.argv[1]) === fs.realpathSync(fileURLToPath(import.meta.url))

if (invokedDirectly) {
  if (!checkDocMakeTargets()) process.exit(1)
}
