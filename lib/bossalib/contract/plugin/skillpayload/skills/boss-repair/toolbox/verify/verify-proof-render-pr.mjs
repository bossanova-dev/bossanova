// `verify-proof-page.mjs render-pr`: the proof page for one head of a local checkout, or for a
// committed fixture, built by the same record, sidecar and page code the gate publishes with.
// With no judge output the judge is synthesised from the diff: the base tree's review policy and
// the built-in areas decide the asks, and the level says the review never ran.
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync, mkdirSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { reviewAreas } from './verify-areas.mjs'
import { confidence } from './verify-confidence.mjs'
import { readChangedFiles, runTool } from './verify-diff.mjs'
import {
  MEDIA_DIR,
  containedSize,
  pageAssets,
  stageAssets,
  validateMediaIn,
} from './verify-media-hook.mjs'
import { buildProofRecord } from './verify-proof-build.mjs'
import { proofPlayerScript, renderProofHtml } from './verify-proof-page.mjs'
import { proofPageInput } from './verify-proof-publish.mjs'
import {
  gitChangedFiles,
  gitFileDiffs,
  lineChecker,
  snippetResolver,
} from './verify-proof-snippet.mjs'
import { PAGE_OBJECT } from './verify-publish-hook.mjs'
import { reviewPolicyFromBlock } from './verify-review-policy.mjs'
import { dbtProjectRoots } from './verify-tree-facts.mjs'

export const FIXTURES_DIR = fileURLToPath(new URL('./testdata/proof-pages/', import.meta.url))
export const USAGE =
  'Usage: verify-proof-page.mjs render-pr (--repo <checkout> --base <ref> --head <ref> [--pr <n>] [--judge <file>] [--intent <file>] [--spec <file>] [--media <file>] | --fixture <name>) --out <dir>'
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
 * @typedef {{message: string, files: Record<string, string[]>}} FixtureCommit
 * @typedef {{repo: string, pr: number, commits: FixtureCommit[]}} FixtureRepo
 * @typedef {{cwd: string, base: string, head: string, name?: string, pr?: number, judge?: string,
 *   intent?: string, spec?: string, media?: string, out: string}} RenderPrOptions
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
 * A judge output for a head no judge saw: the areas the gate would detect, with the base tree's
 * `verifyDefaults` review areas, and the confidence that follows when the review is missing.
 * @param {{cwd: string, base: string, head: string}} range
 */
export function synthesizeJudge({ cwd, base, head }) {
  const files = readChangedFiles({ baseSha: base, headSha: head, cwd })
  let block = null
  try {
    block = JSON.parse(git(cwd, ['show', `${base}:.boss-skills.json`]) ?? 'null')?.verifyDefaults
  } catch {
    // An unreadable config reviews like no config, as the gate treats it.
  }
  const paths = files.flatMap((file) =>
    file.previousPath ? [file.path, file.previousPath] : [file.path],
  )
  const dbtRoots = dbtProjectRoots({ cwd, shas: [base, head], paths })
  const { areas } = reviewAreas(files, reviewPolicyFromBlock(block), { dbtRoots })
  return {
    verdict: 'human',
    headSha: head,
    baseSha: base,
    confidence: confidence({ areas, evidence: { review: 'missing' } }),
    facts: { areas },
  }
}

/**
 * Write `index.html`, `player.js` and the media the page names into `out`. The base is the merge
 * base of `base` and `head`, as the gate diffs it. Returns the range it rendered.
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
  spec,
  media,
  out,
}) {
  const head = commit(cwd, headRef, '--head')
  commit(cwd, baseRef, '--base')
  const base = git(cwd, ['merge-base', baseRef, head])?.trim()
  if (!base) throw new Error(`--base ${baseRef}: no merge base with ${head}`)
  let judged
  if (judge === undefined) judged = synthesizeJudge({ cwd, base, head })
  else {
    const raw = readJson(judge)
    if (raw === null || typeof raw !== 'object' || Array.isArray(raw))
      throw new Error(`${judge}: expected a judge output object`)
    if (raw.headSha !== undefined && String(raw.headSha).toLowerCase() !== head)
      throw new Error(`${judge}: judged head ${raw.headSha} is not ${head}`)
    judged = { ...raw, headSha: head, baseSha: base }
  }
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
  })
  const range = { base, head, run }
  const diffs = gitFileDiffs(range)
  /** @type {import('./verify-proof-page-model.mjs').PageExtras} */
  const extras = {
    snippet: snippetResolver(diffs),
    hasLine: lineChecker(diffs),
    files: gitChangedFiles(range),
  }
  if (spec !== undefined) extras.spec = readJson(spec)
  const mediaDir = media === undefined ? '' : dirname(media)
  let manifest = null
  if (media !== undefined) {
    extras.media = readJson(media)
    extras.mediaSize = containedSize(mediaDir)
    manifest = validateMediaIn(extras.media, head, mediaDir).media
  }
  const html = renderProofHtml(input, extras)
  mkdirSync(out, { recursive: true })
  rmSync(join(out, MEDIA_DIR), { recursive: true, force: true, maxRetries: 3 })
  stageAssets(pageAssets(manifest, mediaDir, proofPlayerScript()), out)
  writeFileSync(join(out, PAGE_OBJECT), html)
  return { base, head, name: slug }
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
  return { base: shas[0], head: shas[shas.length - 1], name: fixture.repo, pr: fixture.pr }
}

/**
 * Render fixture `name` into `out` from a throwaway build of its repository, with each of its
 * `judge.json`, `intent.json`, `spec.json` and `media/media.json` that exists.
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
      spec: { type: 'string' },
      media: { type: 'string' },
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
  return renderPr({ ...files, cwd: repo, base, head, pr, out })
}
