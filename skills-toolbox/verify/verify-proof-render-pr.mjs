// `verify-proof-page.mjs render-pr`: the proof page for one head of a local checkout, or for a
// committed fixture, built by the same record, sidecar and page code the gate publishes with.
// With no judge output the judge is synthesised from the diff: the base tree's review policy and
// the built-in areas decide the asks, and the level says the review never ran. Given `--pr` on a
// checkout whose origin is a forge repository, the branch notice is read from that forge. The
// sections the page carries are the ones the base commit's `proof-section` extensions declare.
// `--extensions <checkout>` puts another tree of the same repository in the base tree's place: its
// review policy, its describers, its sections and, given `--pr` on a forge checkout, its media.
// `--inventory <file>` writes what the page carries, for `inventory-diff` to compare.
import { spawnSync } from 'node:child_process'
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
  mkdirSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { reviewAreas } from './verify-areas.mjs'
import { readBranchContext } from './verify-branch-context.mjs'
import { confidence } from './verify-confidence.mjs'
import { describeAreas, refineDescription } from './verify-describe.mjs'
import { readChangedFiles, runTool } from './verify-diff.mjs'
import {
  MEDIA_DIR,
  collectProofMedia,
  containedSize,
  pageAssets,
  stageAssets,
  validateMediaIn,
} from './verify-media-hook.mjs'
import { discoverExtensions } from '../skill-extensions.mjs'
import { buildProofRecord } from './verify-proof-build.mjs'
import { baseProofSections } from './verify-proof-sections.mjs'
import { proofInventory } from './verify-proof-inventory.mjs'
import { proofPageModel, proofPlayerScript } from './verify-proof-page.mjs'
import { renderPageHtml } from './verify-proof-page-html.mjs'
import { proofPageInput } from './verify-proof-publish.mjs'
import {
  contextResolver,
  gitChangedFiles,
  gitFileDiffs,
  lineChecker,
  snippetResolver,
} from './verify-proof-snippet.mjs'
import { PAGE_OBJECT } from './verify-publish-hook.mjs'
import { reviewPolicyFromBlock } from './verify-review-policy.mjs'
import { dbtProjectRoots, treeAreaFacts } from './verify-tree-facts.mjs'

export const FIXTURES_DIR = fileURLToPath(new URL('./testdata/proof-pages/', import.meta.url))
export const USAGE = [
  'Usage: verify-proof-page.mjs render-pr (--repo <checkout> --base <ref> --head <ref> [--pr <n>] [--judge <file>] [--intent <file>] [--describe <file>] [--spec <file>] [--media <file>] [--extensions <checkout>] [--inventory <file>] | --fixture <name>) --out <dir>',
  '--extensions runs the describe, section and media scripts of the checkout it names: use it only on a checkout you trust.',
].join('\n')
// Fixture commits are an hour apart from this instant, so their SHAs never change.
const FIXTURE_EPOCH = 1767258000
const FIXTURE_ENV = {
  GIT_AUTHOR_NAME: 'Fixture Author',
  GIT_AUTHOR_EMAIL: 'author@example.invalid',
  GIT_COMMITTER_NAME: 'Fixture Author',
  GIT_COMMITTER_EMAIL: 'author@example.invalid',
  GIT_CONFIG_GLOBAL: '/dev/null',
  GIT_CONFIG_NOSYSTEM: '1',
}

/**
 * @typedef {{message: string, files: Record<string, string[] | null>}} FixtureCommit
 *   A file whose lines are null is deleted in that commit.
 * @typedef {{repo: string, pr: number, commits: FixtureCommit[]}} FixtureRepo
 * @typedef {import('./verify-media-hook.mjs').MediaOutcome} MediaOutcome
 * @typedef {(request: {repo: string, pr: number, headSha: string, baseSha: string, dir: string,
 *   cwd: string, trustedTree: () => {ok: boolean, root?: string}}) => MediaOutcome} CollectMedia
 * @typedef {{cwd: string, base: string, head: string, name?: string, pr?: number, judge?: string,
 *   intent?: string, describe?: string, spec?: string, media?: string, out: string,
 *   branch?: import('./verify-proof-page.mjs').BranchContext | null, generatedAt?: string,
 *   extensions?: string, inventory?: string, collectMedia?: CollectMedia}} RenderPrOptions
 *   `describe` maps a judged area id to the proof-describe results its describers wrote.
 *   `extensions` is a checkout whose config and extensions stand in for the base tree's.
 *   `collectMedia` runs that checkout's `proof-media` extension when no `media` file is given.
 */

/** @param {string} cwd @param {string[]} args */
function git(cwd, args) {
  const out = runTool(spawnSync, 'git', ['--literal-pathspecs', ...args], { cwd })
  return out.ok ? out.stdout : null
}

/** @param {string} file */
function readJson(file) {
  try {
    return JSON.parse(readFileSync(file, 'utf8'))
  } catch (error) {
    throw new Error(`${file}: ${error instanceof Error ? error.message : String(error)}`)
  }
}

/** The full SHA of commit `ref` in `cwd`. @param {string} cwd @param {string} ref @param {string} flag */
function commit(cwd, ref, flag) {
  const sha = git(cwd, ['rev-parse', '--verify', '--quiet', `${ref}^{commit}`])?.trim()
  if (!sha) throw new Error(`${flag} ${ref}: not a commit in ${cwd}`)
  return sha
}

/** `owner/name` from the checkout's origin remote, else `local/<directory>`. @param {string} cwd */
function repoSlug(cwd) {
  const url = git(cwd, ['remote', 'get-url', 'origin'])?.trim() ?? ''
  const match = /[/:]([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+?)(?:\.git)?\/?$/.exec(url)
  return match ? match[1] : `local/${basename(cwd).replace(/[^A-Za-z0-9_.-]+/g, '-') || 'repo'}`
}

/**
 * A judge output for a head no judge saw: the areas the gate would detect, with the trusted tree's
 * `verifyDefaults` review areas, and the confidence that follows when the review is missing. The
 * trusted tree is `trustedRoot` when given, else commit `base`. With `describers`, every area is
 * described from `trustedRoot` the way the gate describes it.
 * @param {{cwd: string, base: string, head: string, trustedRoot?: string, describers?: boolean,
 *   pr?: number}} range
 */
export function synthesizeJudge({ cwd, base, head, trustedRoot, describers = false, pr = 1 }) {
  const files = readChangedFiles({ baseSha: base, headSha: head, cwd })
  let block = null
  try {
    const text =
      trustedRoot === undefined
        ? git(cwd, ['show', `${base}:.boss-skills.json`])
        : readFileSync(join(trustedRoot, '.boss-skills.json'), 'utf8')
    block = JSON.parse(text ?? 'null')?.verifyDefaults
  } catch {
    // An unreadable or missing config reviews like no config, as the gate treats it.
  }
  const review = reviewPolicyFromBlock(block)
  let areas
  if (describers && trustedRoot !== undefined) {
    // The gate's files are the forge's: `filename` and `previous_filename`.
    const forgeFiles = files.map(({ path, previousPath, ...rest }) => ({
      ...rest,
      filename: path,
      ...(previousPath ? { previous_filename: previousPath } : {}),
    }))
    areas = treeAreaFacts(
      forgeFiles,
      { review },
      { baseSha: base, headSha: head, cwd, trustedRoot, pr: { number: pr, title: '' } },
    ).areas
  } else {
    const paths = files.flatMap((file) =>
      file.previousPath ? [file.path, file.previousPath] : [file.path],
    )
    const dbtRoots = dbtProjectRoots({ cwd, shas: [base, head], paths })
    areas = reviewAreas(files, review, { dbtRoots }).areas
  }
  return {
    verdict: 'human',
    headSha: head,
    baseSha: base,
    confidence: confidence({ areas, evidence: { review: 'missing' } }),
    facts: { areas },
  }
}

/**
 * The sections commit `base` declares, read from a throwaway extraction of its tree, as the gate
 * reads its trusted tree, or the sections `trustedRoot` declares when given.
 * @param {string} cwd @param {string} base @param {string} [trustedRoot]
 */
function declaredSections(cwd, base, trustedRoot) {
  if (trustedRoot !== undefined)
    return baseProofSections({
      baseSha: base,
      trustedTree: () => ({ ok: true, root: trustedRoot }),
      discover: discoverExtensions,
    })
  const dir = mkdtempSync(join(tmpdir(), 'proof-base-tree-'))
  try {
    return baseProofSections({
      baseSha: base,
      trustedTree: () => {
        const archive = join(dir, 'tree.tar')
        const root = join(dir, 'tree')
        mkdirSync(root)
        if (git(cwd, ['archive', '--format=tar', '-o', archive, base]) === null)
          return { ok: false }
        return runTool(spawnSync, 'tar', ['-xf', archive, '-C', root], {}).ok
          ? { ok: true, root }
          : { ok: false }
      },
      discover: discoverExtensions,
    })
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
}

/**
 * The judge with each area's describer results merged in, as the gate merges them. A result the
 * gate would reject, or one for an area the judge lacks, fails the render with its first error.
 * @param {Record<string, any>} judged @param {string} file
 */
function withDescriptions(judged, file) {
  const raw = readJson(file)
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw))
    throw new Error(`${file}: expected a map of area ids to describer results`)
  const areas = Array.isArray(judged.facts?.areas) ? judged.facts.areas : []
  for (const id of Object.keys(raw))
    if (!areas.some((/** @type {{area: string}} */ area) => area.area === id))
      throw new Error(`${file}: ${id} is not a judged area`)
  return {
    ...judged,
    facts: {
      ...judged.facts,
      areas: areas.map((/** @type {{area: string}} */ area) => {
        const results = raw[area.area]
        if (results === undefined) return area
        if (!Array.isArray(results)) throw new Error(`${file}: ${area.area}: expected a list`)
        const outcomes = results.map((description, index) => {
          const errors = refineDescription(description)
          if (errors.length) throw new Error(`${file}: ${area.area}.${index}: ${errors[0]}`)
          return { extension: description.extension, area: area.area, index, ok: true, description }
        })
        return describeAreas([area], outcomes).areas[0]
      }),
    },
  }
}

/**
 * `dir` as an absolute path when it is a directory holding a `.git` entry, else a usage error.
 * @param {string} dir
 */
function extensionsCheckout(dir) {
  const root = resolve(dir)
  let ok = false
  try {
    ok = statSync(root).isDirectory() && existsSync(join(root, '.git'))
  } catch {
    // A missing directory is the same usage error as a directory that is not a checkout.
  }
  if (!ok) throw new Error(`--extensions ${dir}: not a checkout with a .git entry\n${USAGE}`)
  return root
}

/**
 * Write `index.html`, `player.js` and the media the page names into `out`, and the page's
 * inventory to `inventory` when given. The base is the merge base of `base` and `head`, as the
 * gate diffs it. Returns the range it rendered, and why a collected page has no media.
 * @param {RenderPrOptions} options
 */
export function renderPr({
  cwd,
  base: baseRef,
  head: headRef,
  name,
  pr = 1,
  judge,
  intent,
  describe,
  spec,
  media,
  out,
  branch,
  generatedAt,
  extensions,
  inventory,
  collectMedia,
}) {
  const trustedRoot = extensions === undefined ? undefined : extensionsCheckout(extensions)
  const head = commit(cwd, headRef, '--head')
  commit(cwd, baseRef, '--base')
  const base = git(cwd, ['merge-base', baseRef, head])?.trim()
  if (!base) throw new Error(`--base ${baseRef}: no merge base with ${head}`)
  let judged
  if (judge === undefined)
    judged = synthesizeJudge({
      cwd,
      base,
      head,
      trustedRoot,
      describers: describe === undefined,
      pr,
    })
  else {
    const raw = readJson(judge)
    if (raw === null || typeof raw !== 'object' || Array.isArray(raw))
      throw new Error(`${judge}: expected a judge output object`)
    if (raw.headSha !== undefined && String(raw.headSha).toLowerCase() !== head)
      throw new Error(`${judge}: judged head ${raw.headSha} is not ${head}`)
    judged = { ...raw, headSha: head, baseSha: base }
  }
  if (describe !== undefined) judged = withDescriptions(judged, describe)
  const slug = name ?? repoSlug(cwd)
  const built = buildProofRecord({
    judged,
    intent: intent === undefined ? null : readJson(intent),
    repo: slug,
    pr,
  })
  if (!built.ok)
    throw new Error(
      `${judge ?? 'synthesised judge'}: ${built.reason}${built.errors ? `\n${built.errors.join('\n')}` : ''}`,
    )
  const run = (/** @type {string[]} */ args) => git(cwd, args)
  const input = proofPageInput({
    proof: built.proof,
    areas: built.areas,
    judged,
    git: (/** @type {string[]} */ args) => {
      const output = run(args)
      return { ok: output !== null, output }
    },
    prBody: '',
    repo: slug,
    pr,
    branch,
  })
  const range = { base, head, run }
  const diffs = gitFileDiffs(range)
  /** @type {import('./verify-proof-page-model.mjs').PageExtras} */
  const extras = {
    snippet: snippetResolver(diffs),
    context: contextResolver(diffs),
    hasLine: lineChecker(diffs),
    files: gitChangedFiles(range),
    areas: built.areas.map((area) => ({ name: area.name, paths: area.paths })),
    ...(generatedAt ? { generatedAt } : {}),
  }
  const declared = declaredSections(cwd, base, trustedRoot)
  if (declared.sections.length)
    extras.sections = declared.sections.map(({ instructions, ...section }) => section)
  if (declared.problems.length) extras.sectionProblems = declared.problems
  if (spec !== undefined) extras.spec = readJson(spec)
  let mediaDir = media === undefined ? '' : dirname(media)
  let manifest = null
  /** @type {string | undefined} */
  let mediaReason
  let temp = null
  try {
    if (media !== undefined) {
      extras.media = readJson(media)
      extras.mediaSize = containedSize(mediaDir)
      manifest = validateMediaIn(extras.media, head, mediaDir).media
    } else if (trustedRoot !== undefined && collectMedia) {
      temp = mkdtempSync(join(tmpdir(), 'proof-media-'))
      const outcome = collectMedia({
        repo: slug,
        pr,
        headSha: head,
        baseSha: base,
        dir: temp,
        cwd,
        trustedTree: () => ({ ok: true, root: trustedRoot }),
      })
      mediaReason = outcome.reason
      if (outcome.media) {
        mediaDir = temp
        extras.media = outcome.media
        extras.mediaSize = containedSize(temp)
        manifest = outcome.media
      }
    }
    const model = proofPageModel(input, extras)
    const html = renderPageHtml(model)
    mkdirSync(out, { recursive: true })
    rmSync(join(out, MEDIA_DIR), { recursive: true, force: true, maxRetries: 3 })
    stageAssets(pageAssets(manifest, mediaDir, proofPlayerScript()), out)
    writeFileSync(join(out, PAGE_OBJECT), html)
    if (inventory !== undefined) {
      mkdirSync(dirname(inventory), { recursive: true })
      writeFileSync(inventory, `${JSON.stringify(proofInventory(model), null, 2)}\n`)
    }
  } finally {
    if (temp) rmSync(temp, { recursive: true, force: true, maxRetries: 3 })
  }
  return { base, head, name: slug, ...(mediaReason ? { mediaReason } : {}) }
}

/** The committed fixture names. */
export function fixtureNames() {
  try {
    return readdirSync(FIXTURES_DIR, { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map((entry) => entry.name)
      .sort()
  } catch {
    return []
  }
}

/**
 * Build fixture `name`'s repository in the empty directory `dir`, one commit per entry an hour
 * apart, and return its range. The first commit is the base and the last the head.
 * @param {string} name @param {string} dir
 */
export function buildFixtureRepo(name, dir) {
  /** @type {FixtureRepo} */
  const fixture = readJson(join(FIXTURES_DIR, name, 'repo.json'))
  const env = { ...process.env, ...FIXTURE_ENV }
  /** @param {string[]} args @param {Record<string, string>} [extra] */
  const run = (args, extra = {}) => {
    const out = spawnSync(
      'git',
      ['-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=/dev/null', ...args],
      {
        cwd: dir,
        encoding: 'utf8',
        env: { ...env, ...extra },
      },
    )
    if (out.status !== 0) throw new Error(`fixture ${name}: git ${args[0]} failed: ${out.stderr}`)
    return out.stdout.trim()
  }
  mkdirSync(dir, { recursive: true })
  run(['init', '--quiet', '--initial-branch=main'])
  const shas = fixture.commits.map((entry, index) => {
    for (const [file, lines] of Object.entries(entry.files)) {
      if (lines === null) {
        rmSync(join(dir, file), { force: true, maxRetries: 3 })
        continue
      }
      mkdirSync(dirname(join(dir, file)), { recursive: true })
      writeFileSync(join(dir, file), `${lines.join('\n')}\n`)
    }
    run(['add', '--all'])
    const date = `${FIXTURE_EPOCH + index * 3600} +0000`
    run(['commit', '--quiet', '-m', entry.message], {
      GIT_AUTHOR_DATE: date,
      GIT_COMMITTER_DATE: date,
    })
    return run(['rev-parse', 'HEAD'])
  })
  return {
    base: shas[0],
    head: shas[shas.length - 1],
    name: fixture.repo,
    pr: fixture.pr,
    // The head's commit time, so a fixture page is the same bytes on every render.
    generatedAt: new Date((FIXTURE_EPOCH + (shas.length - 1) * 3600) * 1000).toISOString(),
  }
}

/**
 * Render fixture `name` into `out` from a throwaway build of its repository, with each of its
 * `judge.json`, `intent.json`, `describe.json`, `spec.json` and `media/media.json` that exists.
 * @param {string} name @param {string} out
 */
export function renderFixture(name, out) {
  if (!fixtureNames().includes(name))
    throw new Error(
      `--fixture ${name}: unknown fixture; fixtures: ${fixtureNames().join(', ') || 'none'}`,
    )
  const root = join(FIXTURES_DIR, name)
  const present = new Set(readdirSync(root))
  /** @param {string} file */
  const part = (file) => (present.has(file.split('/')[0]) ? join(root, file) : undefined)
  const dir = mkdtempSync(join(tmpdir(), `proof-fixture-${name}-`))
  try {
    const range = buildFixtureRepo(name, dir)
    return renderPr({
      cwd: dir,
      ...range,
      judge: part('judge.json'),
      intent: part('intent.json'),
      describe: part('describe.json'),
      spec: part('spec.json'),
      media: part(`${MEDIA_DIR}/media.json`),
      out,
    })
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
}

/** The `render-pr` verb. @param {string[]} args the arguments after the verb */
export function renderPrCli(args) {
  const { values } = parseArgs({
    args,
    strict: true,
    options: {
      repo: { type: 'string' },
      base: { type: 'string' },
      head: { type: 'string' },
      pr: { type: 'string' },
      judge: { type: 'string' },
      intent: { type: 'string' },
      describe: { type: 'string' },
      spec: { type: 'string' },
      media: { type: 'string' },
      extensions: { type: 'string' },
      inventory: { type: 'string' },
      fixture: { type: 'string' },
      out: { type: 'string' },
    },
  })
  const { fixture, out, ...rest } = values
  if (!out) throw new Error(USAGE)
  if (fixture !== undefined) {
    if (Object.keys(rest).length) throw new Error(`--fixture takes only --out\n${USAGE}`)
    return renderFixture(fixture, out)
  }
  if (!rest.repo || !rest.base || !rest.head) throw new Error(USAGE)
  const pr = rest.pr === undefined ? 1 : Number(rest.pr)
  if (!Number.isSafeInteger(pr) || pr < 1)
    throw new Error(`--pr ${rest.pr}: expected a positive integer`)
  const { repo, base, head, ...files } = rest
  const slug = repoSlug(repo)
  const forge = rest.pr !== undefined && !slug.startsWith('local/')
  if (files.extensions !== undefined) extensionsCheckout(files.extensions)
  const branch = forge
    ? readBranchContext({ read: checkoutReader(repo), repo: slug, pr })
    : undefined
  /** @type {CollectMedia | undefined} */
  const collectMedia = forge
    ? (request) => collectProofMedia({ ...request, read: checkoutReader(repo), run: spawnSync })
    : undefined
  const rendered = renderPr({ ...files, cwd: repo, base, head, pr, out, branch, collectMedia })
  if (rendered.mediaReason) process.stderr.write(`proof-media: ${rendered.mediaReason}\n`)
  return rendered
}

/**
 * A reader in the gate's shape for commands run in `cwd`.
 * @param {string} cwd @returns {import('./verify-branch-context.mjs').Read}
 */
function checkoutReader(cwd) {
  return (command, args, { input, json = false, raw = false } = {}) => {
    const out = runTool(spawnSync, command, args, { cwd, input })
    const output = raw ? out.stdout : out.stdout.trim()
    if (!json) return { ok: out.ok, output }
    try {
      return { ok: out.ok, output, payload: JSON.parse(out.stdout) }
    } catch {
      return { ok: false, output }
    }
  }
}
