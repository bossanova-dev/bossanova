#!/usr/bin/env node
// Release-time changelog generator. NON-BLOCKING: any failure logs to stderr
// and exits 0 so it can never fail a production release.
import { execFileSync } from 'node:child_process'
import { existsSync } from 'node:fs'
import { mkdir, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import Anthropic from '@anthropic-ai/sdk'
import {
  buildPrompt,
  dedupeTicketIds,
  entryFilename,
  linearIdFromBranch,
  parsePrNumbers,
  previousVersionTag,
  releaseDate,
  renderFrontmatter,
  resolveRepo,
  stripHtmlTags,
  summaryFromBody,
} from './changelog/lib.mjs'

const MODEL = 'claude-sonnet-4-6'
const REPO = resolveRepo(process.env)
const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const OUT_DIR = resolve(ROOT, 'services/marketing/src/content/changelog')

function gh(args) {
  return execFileSync('gh', args, { encoding: 'utf8', maxBuffer: 32 * 1024 * 1024 })
}

function parseArgs(argv) {
  const out = {}
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--version') out.version = argv[++i]
    else if (argv[i] === '--backfill') out.backfill = Number(argv[++i])
  }
  return out
}

function isPrereleaseVersion(version) {
  return /^v?\d+\.\d+\.\d+-/.test(version)
}

function previousTag(tag) {
  // The list-tags API order is not documented as semver-sorted, so derive the
  // predecessor by semver precedence (ignoring prereleases) rather than by the
  // raw list position.
  const tags = gh(['api', `repos/${REPO}/tags?per_page=100`])
  const names = JSON.parse(tags).map((t) => t.name)
  return previousVersionTag(names, tag)
}

function prsAndCommitsInRange(prevTag, tag) {
  // GitHub's compare endpoint requires a `base...head` range. With no known
  // predecessor (e.g. the very first release) there is nothing to diff against,
  // so return empty data and let the model synthesize from the release alone.
  if (!prevTag) return { prs: [], commits: [] }
  const range = `${prevTag}...${tag}`
  const log = gh(['api', `repos/${REPO}/compare/${range}`])
  const commits = JSON.parse(log).commits.map((c) => c.commit.message.split('\n')[0])
  const prNums = parsePrNumbers(commits)
  const prs = prNums.map((n) => {
    const pr = JSON.parse(gh(['api', `repos/${REPO}/pulls/${n}`]))
    return {
      number: Number(n),
      title: pr.title,
      headRefName: pr.head?.ref || '',
      body: pr.body || '',
    }
  })
  return { prs, commits }
}

async function fetchTickets(prs) {
  const ids = dedupeTicketIds(prs.map((p) => linearIdFromBranch(p.headRefName)).filter(Boolean))
  const tickets = []
  for (const id of ids) {
    try {
      const res = await fetch('https://api.linear.app/graphql', {
        method: 'POST',
        headers: { 'content-type': 'application/json', authorization: process.env.LINEAR_API_KEY },
        body: JSON.stringify({
          query: `query($id:String!){ issue(id:$id){ identifier title description } }`,
          variables: { id },
        }),
      })
      const json = await res.json()
      const issue = json?.data?.issue
      if (issue)
        tickets.push({ id: issue.identifier, title: issue.title, description: issue.description })
    } catch (err) {
      console.warn(`[changelog] Linear lookup failed for ${id}: ${err.message}`)
    }
  }
  return tickets
}

async function synthesize(version, prs, tickets, commits) {
  const anthropic = new Anthropic({ apiKey: process.env.ANTHROPIC_API_KEY.trim() })
  const prompt = buildPrompt({ version, prs, tickets, commits })
  const msg = await anthropic.messages.create({
    model: MODEL,
    max_tokens: 1500,
    messages: [{ role: 'user', content: prompt }],
  })
  return msg.content
    .map((b) => (b.type === 'text' ? b.text : ''))
    .join('')
    .trim()
}

async function generateOne(version) {
  const tag = version.startsWith('v') ? version : `v${version}`
  const outPath = resolve(OUT_DIR, entryFilename(version))
  if (isPrereleaseVersion(version)) {
    console.warn(`[changelog] skipped: prerelease ${version.replace(/^v/, '')} (non-blocking)`)
    return
  }
  if (existsSync(outPath)) {
    console.log(`[changelog] ${entryFilename(version)} already exists, skipping`)
    return
  }
  if (!process.env.ANTHROPIC_API_KEY?.trim()) {
    console.warn('[changelog] skipped: no ANTHROPIC_API_KEY (non-blocking)')
    return
  }
  // Defense-in-depth: the GitHub Release may not exist yet at changelog time
  // (or ever, for a manual backfill). A missing release must never abort
  // generation — log a non-blocking warning and fall back to the current date,
  // then continue synthesizing from the compare range.
  let rel = null
  try {
    rel = JSON.parse(gh(['api', `repos/${REPO}/releases/tags/${tag}`]))
  } catch (err) {
    console.warn(
      `[changelog] no GitHub release for ${tag} yet; using current date (non-blocking): ${err.message}`,
    )
  }
  // releaseDate is pure + unit-tested (changelog/lib.mjs): published_at ||
  // created_at || now, always a valid Date so renderFrontmatter can't throw.
  const date = releaseDate(rel)
  const { prs, commits } = prsAndCommitsInRange(previousTag(tag), tag)
  const tickets = await fetchTickets(prs)
  // Sanitize the model output before it is committed and deployed to the public
  // marketing site (no human review on the release path).
  const body = stripHtmlTags(
    await synthesize(version.replace(/^v/, ''), prs, tickets, commits),
  ).trim()
  if (!body) {
    // An empty/refused completion would publish a heading-only entry. Skip it
    // (consistent with the non-blocking design); a later run can backfill.
    console.warn(`[changelog] empty synthesis for v${version}; skipping write (non-blocking)`)
    return
  }
  const summary = summaryFromBody(body, `Release v${version}`)
  const fm = renderFrontmatter({ version, date, title: `v${version.replace(/^v/, '')}`, summary })
  await mkdir(OUT_DIR, { recursive: true })
  await writeFile(outPath, `${fm}\n${body}\n`, 'utf8')
  console.log(`[changelog] wrote ${entryFilename(version)}`)
}

async function main() {
  const args = parseArgs(process.argv.slice(2))
  if (args.backfill) {
    const tags = JSON.parse(gh(['api', `repos/${REPO}/releases?per_page=${args.backfill}`]))
    for (const r of tags) await generateOne(r.tag_name.replace(/^v/, ''))
  } else if (args.version) {
    await generateOne(args.version)
  } else {
    throw new Error('usage: --version <x.y.z> | --backfill <n>')
  }
}

main().catch((err) => {
  // NON-BLOCKING: never fail the release.
  console.error(`[changelog] generation failed (non-blocking): ${err.stack || err.message}`)
  process.exit(0)
})
