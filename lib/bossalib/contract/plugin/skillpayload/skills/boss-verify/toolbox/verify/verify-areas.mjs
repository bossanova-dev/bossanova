import { globToRegExp } from '../skill-config.mjs'

/**
 * @typedef {'always' | 'unless-proven' | 'never'} DefaultAsk
 * @typedef {{area: string, label: string, defaultAsk: DefaultAsk, globs: readonly string[]}} Area
 * @typedef {{path: string, previousPath?: string, status?: string, changes?: number, patch?: string}} ChangedFile
 * @typedef {{name: string, from: string, to: string}} Bump
 * @typedef {Bump & {path: string}} PathBump
 * @typedef {{area: string, label: string, paths: string[], defaultAsk: DefaultAsk, bumps?: PathBump[]}} AreaResult
 */

/** @param {string} area @param {string} label @param {DefaultAsk} defaultAsk @param {string[]} globs @returns {Area} */
const area = (area, label, defaultAsk, globs) =>
  Object.freeze({ area, label, defaultAsk, globs: Object.freeze(globs) })

/** Generic conventions in first-match precedence order. */
export const AREAS = Object.freeze([
  area('migrations', 'Migrations', 'always', [
    '**/migrations/**',
    '**/db/migrate/**',
    '**/*.sql',
    '**/*.prisma',
  ]),
  area('api-contract', 'API contract', 'always', [
    '**/*.proto',
    '**/*.graphql',
    '**/*.graphqls',
    '**/*.gql',
    '**/openapi*.yaml',
    '**/openapi*.yml',
    '**/openapi*.json',
    '**/swagger*.yaml',
    '**/swagger*.yml',
    '**/swagger*.json',
  ]),
  area('infra', 'Infrastructure', 'always', [
    '**/*.tf',
    '**/*.tfvars',
    '**/terragrunt.hcl',
    '**/k8s/**',
    '**/kubernetes/**',
    '**/helm/**',
    '**/charts/**',
    '**/kustomization.yaml',
    '**/kustomization.yml',
    '.github/workflows/**',
    '.github/actions/**',
    '.gitlab-ci.yml',
    '.circleci/**',
  ]),
  area('agent-rules', 'Agent rules', 'always', [
    '**/AGENTS.md',
    '**/CLAUDE.md',
    '**/GEMINI.md',
    '**/SKILL.md',
    '.claude/**',
    '.codex/**',
    '.cursor/rules/**',
    '.cursorrules',
    '.github/copilot-instructions.md',
    '**/.boss-*.json',
    '.mcp.json',
  ]),
  area('dependencies', 'Dependencies', 'always', [
    '**/package.json',
    '**/go.mod',
    '**/Cargo.toml',
    '**/pyproject.toml',
    '**/requirements*.txt',
    '**/Gemfile',
    '**/composer.json',
    '**/pom.xml',
    '**/build.gradle',
    '**/build.gradle.kts',
    '**/package-lock.json',
    '**/pnpm-lock.yaml',
    '**/yarn.lock',
    '**/bun.lockb',
    '**/go.sum',
    '**/go.work.sum',
    '**/Cargo.lock',
    '**/poetry.lock',
    '**/uv.lock',
    '**/Gemfile.lock',
    '**/composer.lock',
  ]),
  area('housekeeping', 'Housekeeping', 'never', [
    '**/*.test.*',
    '**/*.spec.*',
    '**/*_test.go',
    '**/test_*.py',
    '**/*_test.py',
    '**/__tests__/**',
    '**/__snapshots__/**',
    '**/testdata/**',
    '**/test/**',
    '**/tests/**',
    '**/*.md',
    '**/*.mdx',
    '**/*.rst',
    'docs/**',
    '**/LICENSE*',
    '**/CHANGELOG*',
    '**/gen/**',
    '**/generated/**',
    '**/__generated__/**',
    '**/*.generated.*',
    '**/*.gen.*',
    '**/*_gen.go',
    '**/zz_generated*',
    '**/*.pb.go',
    '**/*_pb2.py',
    '**/*_pb2_grpc.py',
    '**/*.snap',
  ]),
  area('ui', 'UI', 'unless-proven', [
    '**/*.tsx',
    '**/*.jsx',
    '**/*.vue',
    '**/*.svelte',
    '**/*.astro',
    '**/*.css',
    '**/*.scss',
    '**/*.sass',
    '**/*.less',
    '**/*.html',
    '**/components/**',
    '**/pages/**',
    '**/layouts/**',
    '**/styles/**',
  ]),
  area('other', 'Other changes', 'never', ['**']),
])

const matchers = AREAS.map((entry) => entry.globs.map(globToRegExp))
const dependencyIndex = AREAS.findIndex((entry) => entry.area === 'dependencies')
const housekeepingIndex = AREAS.findIndex((entry) => entry.area === 'housekeeping')
const uiIndex = AREAS.findIndex((entry) => entry.area === 'ui')
const otherIndex = AREAS.length - 1
const lockfile =
  /^(?:package-lock\.json|pnpm-lock\.yaml|yarn\.lock|bun\.lockb|go\.sum|go\.work\.sum|Cargo\.lock|poetry\.lock|uv\.lock|Gemfile\.lock|composer\.lock)$/

/** @param {string} path */
const classify = (path) =>
  matchers.findIndex((patterns) => patterns.some((pattern) => pattern.test(path)))
/** @param {string} a @param {string} b */
const compare = (a, b) => (a < b ? -1 : a > b ? 1 : 0)

/**
 * @param {string} filename @param {string} line @returns {[string, string] | null}
 */
function dependencyEntry(filename, line) {
  if (filename === 'package.json') {
    const entry = line.match(/^\s*"([^"\n]+)"\s*:\s*"([^"\n]+)"/)
    return entry && !['name', 'version'].includes(entry[1]) ? [entry[1], entry[2]] : null
  }
  if (filename === 'go.mod') {
    const entry = line.match(/^\s*(?:require\s+)?([^\s]+)\s+(v\d[^\s]*)/)
    return entry ? [entry[1].replace(/\/v\d+$/, ''), entry[2]] : null
  }
  if (filename.endsWith('.toml')) {
    const entry = line.match(
      /^\s*["']?([\w.-]+)["']?\s*=\s*(?:\{\s*version\s*=\s*)?["']([^"']+)["']/,
    )
    if (entry) return ['name', 'version'].includes(entry[1]) ? null : [entry[1], entry[2]]
  }
  const requirement = line.match(
    /^\s*["']?([\w.-]+)(?:\[[^\]]+\])?\s*((?:===|==|>=|<=|~=|!=|>|<)[^\s"',;]+)/,
  )
  return requirement ? [requirement[1], requirement[2]] : null
}

/** @param {string} version */
function numericVersion(version) {
  const match = version.replace(/^[\s^~>=<v]+/, '').match(/^(\d+)(?:\.(\d+))?/)
  return match ? [Number(match[1]), Number(match[2] ?? 0)] : null
}

/** Pair removed and added entries by name; additions, removals and nonnumeric versions are not bumps.
 * @param {string} path @param {string | undefined} patch
 * @returns {{readable: boolean, bumps: Bump[]}}
 */
export function dependencyBumps(path, patch) {
  const filename = path.split('/').at(-1) ?? path
  if (lockfile.test(filename)) return { readable: true, bumps: [] }
  if (
    typeof patch !== 'string' ||
    /^(?:Gemfile|composer\.json|pom\.xml|build\.gradle(?:\.kts)?)$/.test(filename)
  ) {
    return { readable: false, bumps: [] }
  }
  /** @type {Map<string, string>} */
  const removed = new Map()
  /** @type {Map<string, string>} */
  const added = new Map()
  for (const line of patch.split('\n')) {
    if (!/^[+-]/.test(line) || /^(---|\+\+\+)/.test(line)) continue
    const entry = dependencyEntry(filename, line.slice(1))
    if (entry) (line[0] === '-' ? removed : added).set(...entry)
  }
  /** @type {Bump[]} */
  const bumps = []
  for (const [name, from] of removed) {
    const to = added.get(name)
    if (to === undefined) continue
    const oldVersion = numericVersion(from)
    const newVersion = numericVersion(to)
    if (
      oldVersion &&
      newVersion &&
      (newVersion[0] > oldVersion[0] ||
        (newVersion[0] === 0 && oldVersion[0] === 0 && newVersion[1] > oldVersion[1]))
    ) {
      bumps.push({ name, from, to })
    }
  }
  bumps.sort((a, b) => compare(a.name, b.name))
  return { readable: true, bumps }
}

/** Partition changed names, joining overlapping rename records so each name appears once.
 * @param {readonly (string | ChangedFile)[]} files @returns {AreaResult[]}
 */
export function detectAreas(files) {
  /** @type {Map<string, {paths: Set<string>, index: number}>} */
  const groups = new Map()
  /** @type {PathBump[]} */
  const bumps = []
  let unreadable = false
  for (const file of files) {
    const entry = typeof file === 'string' ? { path: file } : file
    const paths = entry.previousPath ? [entry.path, entry.previousPath] : [entry.path]
    let index = Math.min(...paths.map(classify))
    if (
      entry.status === 'renamed' &&
      entry.changes === 0 &&
      (index === uiIndex || index === otherIndex)
    ) {
      index = housekeepingIndex
    }
    const existing = [
      ...new Set(
        paths.flatMap((path) => {
          const prior = groups.get(path)
          return prior ? [prior] : []
        }),
      ),
    ].sort((a, b) => b.paths.size - a.paths.size)
    const group = existing[0] ?? { paths: new Set(), index }
    group.index = Math.min(index, ...existing.map((prior) => prior.index))
    for (const prior of existing.slice(1)) {
      for (const path of prior.paths) {
        group.paths.add(path)
        groups.set(path, group)
      }
    }
    for (const path of paths) {
      group.paths.add(path)
      groups.set(path, group)
    }
    if (index === dependencyIndex) {
      const manifest = paths.find((path) => classify(path) === dependencyIndex)
      const parsed = dependencyBumps(manifest ?? entry.path, entry.patch)
      unreadable ||= !parsed.readable
      bumps.push(...parsed.bumps.map((bump) => ({ path: entry.path, ...bump })))
    }
  }
  const uniqueBumps = [...new Map(bumps.map((bump) => [JSON.stringify(bump), bump])).values()].sort(
    (a, b) =>
      compare(a.path, b.path) ||
      compare(a.name, b.name) ||
      compare(a.from, b.from) ||
      compare(a.to, b.to),
  )
  return AREAS.flatMap((entry, index) => {
    const paths = [...groups]
      .filter(([, group]) => group.index === index)
      .map(([path]) => path)
      .sort(compare)
    if (paths.length === 0) return []
    /** @type {AreaResult} */
    const result = { area: entry.area, label: entry.label, paths, defaultAsk: entry.defaultAsk }
    if (index === dependencyIndex) {
      result.defaultAsk = unreadable || uniqueBumps.length > 0 ? 'always' : 'never'
      result.bumps = uniqueBumps
    }
    return [result]
  })
}
