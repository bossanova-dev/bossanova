import { globToRegExp } from '../skill-config.mjs'
import { isPureMoveFile, renamePairs } from './verify-moves.mjs'

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
  const match = version.replace(/^[\s^~>=<v]+/, '').match(/^(\d+)(?:\.(\d+))?(?:\.(\d+))?/)
  return match ? [Number(match[1]), Number(match[2] ?? 0), Number(match[3] ?? 0)] : null
}

/**
 * The level of a version change, from the first differing numeric component: a 0.x minor change
 * is `major`, and unparsable or numerically equal versions are `unknown`.
 * @param {string} from @param {string} to @returns {'major' | 'minor' | 'patch' | 'unknown'}
 */
export function bumpLevel(from, to) {
  const a = numericVersion(from)
  const b = numericVersion(to)
  if (!a || !b) return 'unknown'
  if (a[0] !== b[0] || (a[0] === 0 && a[1] !== b[1])) return 'major'
  if (a[1] !== b[1]) return 'minor'
  return a[2] !== b[2] ? 'patch' : 'unknown'
}

/**
 * Every direct dependency a manifest patch changes, adds or removes, paired by name. Lockfiles
 * carry no direct entries; Maven, Gradle, Gemfile and Composer manifests are unreadable.
 * @param {string} path @param {string | undefined} patch
 * @returns {{readable: boolean, changes: Bump[], added: string[], removed: string[]}}
 */
export function dependencyChanges(path, patch) {
  const filename = path.split('/').at(-1) ?? path
  const none = { changes: [], added: [], removed: [] }
  if (lockfile.test(filename)) return { readable: true, ...none }
  if (
    typeof patch !== 'string' ||
    /^(?:Gemfile|composer\.json|pom\.xml|build\.gradle(?:\.kts)?)$/.test(filename)
  ) {
    return { readable: false, ...none }
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
  const changes = []
  for (const [name, from] of removed) {
    const to = added.get(name)
    if (to !== undefined && to !== from) changes.push({ name, from, to })
  }
  changes.sort((a, b) => compare(a.name, b.name))
  return {
    readable: true,
    changes,
    added: [...added.keys()].filter((name) => !removed.has(name)).sort(compare),
    removed: [...removed.keys()].filter((name) => !added.has(name)).sort(compare),
  }
}

/** Upward major (or 0.x minor) changes; additions, removals and nonnumeric versions are not bumps.
 * @param {string} path @param {string | undefined} patch
 * @returns {{readable: boolean, bumps: Bump[]}}
 */
export function dependencyBumps(path, patch) {
  const { readable, changes } = dependencyChanges(path, patch)
  const bumps = changes.filter(({ from, to }) => {
    const oldVersion = numericVersion(from)
    const newVersion = numericVersion(to)
    return (
      oldVersion &&
      newVersion &&
      (newVersion[0] > oldVersion[0] ||
        (newVersion[0] === 0 && oldVersion[0] === 0 && newVersion[1] > oldVersion[1]))
    )
  })
  return { readable, bumps }
}

// npm lifecycle scripts that run on `npm install` (`prepublish` only on older npm).
export const INSTALL_SCRIPTS = Object.freeze([
  'preinstall',
  'install',
  'postinstall',
  'preprepare',
  'prepare',
  'postprepare',
  'prepublish',
  'dependencies',
])
// A script value is a string, which keeps a `"dependencies": {` section line out. With -U0 there is
// no enclosing object to read, so a dependency literally named `install` still asks: conservative.
const INSTALL_SCRIPT = new RegExp(`^\\s*"(?:${INSTALL_SCRIPTS.join('|')})"\\s*:\\s*"`)

/** Does a package.json patch add, remove or change a script that runs on install?
 * @param {string} path @param {string | undefined} patch */
export function installScriptChanged(path, patch) {
  if ((path.split('/').at(-1) ?? path) !== 'package.json' || typeof patch !== 'string') return false
  return patch
    .split('\n')
    .some(
      (line) =>
        /^[+-]/.test(line) && !/^(---|\+\+\+)/.test(line) && INSTALL_SCRIPT.test(line.slice(1)),
    )
}

/** Partition changed names, joining overlapping rename records so each name appears once.
 * @param {readonly (string | ChangedFile)[]} files @returns {AreaResult[]}
 */
export function detectAreas(files) {
  const pairs = renamePairs(files)
  /** @type {Map<string, {paths: Set<string>, index: number}>} */
  const groups = new Map()
  /** @type {PathBump[]} */
  const bumps = []
  let unreadable = false
  for (const file of files) {
    const entry = typeof file === 'string' ? { path: file } : file
    const paths = entry.previousPath ? [entry.path, entry.previousPath] : [entry.path]
    let index = Math.min(...paths.map(classify))
    // A pure move (a zero-change rename, or a file whose only changed lines are imports a rename
    // explains) is housekeeping, but only where a move is inherently safe.
    if (
      (index === uiIndex || index === otherIndex) &&
      ((entry.status === 'renamed' && entry.changes === 0) || isPureMoveFile(entry, pairs))
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
      // A changed install script runs code at install time, so it asks like an unreadable manifest.
      unreadable ||= !parsed.readable || installScriptChanged(manifest ?? entry.path, entry.patch)
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

/** Apply configured areas to connected rename groups before the built-in catalogue.
 * @param {readonly (string | ChangedFile)[]} files
 * @param {import('./verify-review-policy.mjs').ReviewPolicy} review
 * @returns {{areas: (AreaResult & {source: 'config' | 'builtin'})[], omitted: string[]}}
 */
export function reviewAreas(files, review) {
  const configured = review.alwaysReview.map((entry) => ({
    entry,
    patterns: entry.globs.map(globToRegExp),
  }))
  const never = review.neverReview.map(globToRegExp)
  /** @type {Map<string, Set<string>>} */
  const names = new Map()
  for (const file of files) {
    const entry = typeof file === 'string' ? { path: file } : file
    const paths = entry.previousPath ? [entry.path, entry.previousPath] : [entry.path]
    const group = new Set(paths.flatMap((path) => [...(names.get(path) ?? [path])]))
    for (const path of group) names.set(path, group)
  }
  /** @type {Map<string, number>} */
  const assigned = new Map()
  const omitted = new Set()
  const buckets = configured.map(() => new Set())
  for (const group of new Set(names.values())) {
    const paths = [...group]
    const index = configured.findIndex(({ patterns }) =>
      paths.some((path) => patterns.some((re) => re.test(path))),
    )
    if (index >= 0)
      for (const path of paths) {
        buckets[index].add(path)
        assigned.set(path, index)
      }
    else if (paths.every((path) => never.some((re) => re.test(path))))
      for (const path of paths) omitted.add(path)
  }
  const remaining = files.filter((file) => {
    const path = typeof file === 'string' ? file : file.path
    return !assigned.has(path) && !omitted.has(path)
  })
  /** @type {(AreaResult & {source: 'config' | 'builtin'})[]} */
  const areas = configured.flatMap(({ entry }, index) =>
    buckets[index].size
      ? [
          {
            area: `config:${entry.name}${configured.slice(0, index).some((prior) => prior.entry.name === entry.name) ? `:${index}` : ''}`,
            label: entry.name,
            paths: [...buckets[index]].sort(compare),
            defaultAsk: 'always',
            source: 'config',
          },
        ]
      : [],
  )
  areas.push(
    ...detectAreas(remaining).map((entry) => ({
      ...entry,
      source: /** @type {const} */ ('builtin'),
    })),
  )
  return { areas, omitted: [...omitted].sort(compare) }
}
