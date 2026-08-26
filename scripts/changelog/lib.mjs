// Pure, network-free helpers for the changelog generator. Unit-tested in
// lib.test.mjs. All I/O lives in ../generate-changelog.mjs.

// Resolve the repo to query for release/tag/PR/compare data. In CI this is the
// repo the workflow runs in (GITHUB_REPOSITORY), where semantic-release has
// already cut the tag + GitHub Release and where the PRs actually live. Falls
// back to the canonical private repo for local/manual backfill runs.
export function resolveRepo(env = {}) {
  const fromCi = (env.GITHUB_REPOSITORY || '').trim()
  return fromCi || 'recurser/bossanova'
}

// Resolve the changelog entry date from a fetched GitHub Release object (or
// null when the release could not be fetched — the caller passes null on a
// missing-release/non-blocking fallback). Prefers the published timestamp, then
// the created timestamp, and finally `now` when neither is present or parses.
// Always returns a Date and never throws, so a missing or malformed release can
// never abort the non-blocking generator downstream (renderFrontmatter calls
// .toISOString(), which would throw on an Invalid Date).
export function releaseDate(rel, now = new Date()) {
  // Try each candidate in preference order, parsing before accepting, so a
  // malformed published_at falls through to a valid created_at (not straight to
  // `now`) — the `||` short-circuit would otherwise skip a good created_at.
  for (const stamp of [rel?.published_at, rel?.created_at]) {
    if (!stamp) continue
    const d = new Date(stamp)
    if (!Number.isNaN(d.getTime())) return d
  }
  return now
}

export function linearIdFromBranch(branchName) {
  if (!branchName) return null
  const m = branchName.match(/\bbos-(\d+)\b/i)
  return m ? `BOS-${m[1]}` : null
}

export function entryFilename(version) {
  return `${version.replace(/^v/, '')}.md`
}

// Extract referenced PR numbers from commit subject lines, newest-first order
// preserved and de-duplicated. This repo's commit policy tags subjects with
// `[#123]` (see scripts/commit-message-policy.cjs), while GitHub's squash-merge
// default appends `(#123)`; accept both so the release-time generator pulls PR
// titles, branch names, and Linear context regardless of how a commit landed.
export function parsePrNumbers(commits) {
  const seen = new Set()
  for (const message of commits) {
    for (const m of String(message).matchAll(/[[(]#(\d+)[\])]/g)) seen.add(m[1])
  }
  return [...seen]
}

// Parse a `vX.Y.Z` (or `X.Y.Z`) tag name into a [major, minor, patch] tuple.
// Returns null for anything that is not a plain stable semver tag (e.g.
// prereleases like `v1.56.0-staging.1`), so those are ignored when picking a
// predecessor.
function parseStableSemver(name) {
  const m = /^v?(\d+)\.(\d+)\.(\d+)$/.exec(name)
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null
}

// Given a list of tag names in ANY order (the GitHub list-tags API order is not
// documented as semver- or chronologically-sorted) and a target tag, return the
// tag immediately older than `tag` by semver precedence, ignoring prereleases.
// Returns null when `tag` is the oldest known release or is unknown.
export function previousVersionTag(names, tag) {
  const target = parseStableSemver(tag)
  if (!target) return null
  const stable = names
    .map((name) => ({ name, v: parseStableSemver(name) }))
    .filter((t) => t.v)
    .sort((a, b) => b.v[0] - a.v[0] || b.v[1] - a.v[1] || b.v[2] - a.v[2])
  const idx = stable.findIndex((t) => t.v.join('.') === target.join('.'))
  return idx >= 0 && idx + 1 < stable.length ? stable[idx + 1].name : null
}

// Defense-in-depth for the release-time path: the LLM body is fed
// adversarially-influenced text (PR titles, branch names, commit subjects,
// Linear ticket descriptions) and is auto-committed and deployed to the public
// marketing site without human review. A changelog body is prose markdown and
// never needs raw HTML, so strip HTML tags (and whole script/style blocks)
// before writing. Markdown emphasis, inequalities (`a < b`), and autolinks
// (`<https://…>`) are preserved — only real tags (`<name ...>`, `</name>`) go.
export function stripHtmlTags(text) {
  return String(text)
    .replace(/<(script|style)\b[^>]*>[\s\S]*?<\/\1\s*>/gi, '')
    .replace(/<\/?[a-z][a-z0-9-]*(\s+[^<>]*?)?\/?>/gi, '')
}

function yamlString(value) {
  return `'${String(value).replace(/'/g, "''")}'`
}

export function renderFrontmatter({ version, date, title, summary }) {
  const iso = date instanceof Date ? date.toISOString() : new Date(date).toISOString()
  return [
    '---',
    `version: ${yamlString(version.replace(/^v/, ''))}`,
    `date: ${iso}`,
    `title: ${yamlString(title)}`,
    `summary: ${yamlString(summary)}`,
    '---',
    '',
  ].join('\n')
}

// Derive the frontmatter `summary` (a plain one-line teaser) from the generated
// markdown body. The body now opens directly with a "###" group, so the first
// content line is a bullet; strip list/bold/code markdown so the summary reads
// as prose. Falls back to `fallback` when no usable text is found.
export function summaryFromBody(body, fallback) {
  for (const raw of String(body).split('\n')) {
    let line = raw.trim()
    if (!line || line.startsWith('#')) continue
    line = line
      .replace(/^[-*]\s+/, '') // leading list marker
      .replace(/\*\*(.+?)\*\*/g, '$1') // bold
      .replace(/`([^`]+)`/g, '$1') // inline code
      .trim()
    if (line) return line.slice(0, 140) // gate-region-ok: summary display truncation, not a section gate
  }
  return fallback
}

export function dedupeTicketIds(ids) {
  const seen = new Set()
  const out = []
  for (const raw of ids) {
    const id = String(raw).toUpperCase()
    if (!seen.has(id)) {
      seen.add(id)
      out.push(id)
    }
  }
  return out
}

export function buildPrompt({ version, prs, tickets, commits }) {
  const prLines = prs.map((p) => `- #${p.number} ${p.title} (${p.headRefName})`).join('\n')
  const ticketLines = tickets
    .map((t) => `### ${t.id}: ${t.title}\n${(t.description || '').slice(0, 1500)}`)
    .join('\n\n')
  const commitLines = commits.map((c) => `- ${c}`).join('\n')
  return [
    `You are writing the public changelog entry for Bossanova release v${version}.`,
    'The reader has zero technical background in Bossanova and only superficial',
    'knowledge of what the product does. They are deciding whether the product is',
    'useful to them from the website, not auditing implementation details.',
    '',
    'Write like the Conductor changelog: informal but informative, concrete, and',
    'product-focused. Translate implementation details into outcomes a user can',
    'see, do, or feel. Prefer plain language like "You can now..." and "Boss now"',
    'over subsystem names. For fixes, prefer "Fixed an issue where..." followed by',
    'the user-visible problem that went away.',
    '',
    'SYNTHESIS RULES:',
    '- Combine related commits, PRs, and tickets into a small number of readable',
    '  product changes. Do NOT dump raw commits, PR numbers, branch names, ticket',
    '  IDs, or a one-bullet-per-internal-change inventory.',
    '- Apply a newsworthiness filter. Trivial copy changes, label tweaks, tooltip',
    '  edits, punctuation changes, redundant hint removals, and other wording-only',
    '  polish are not news. Omit them unless they are part of a larger user-visible',
    '  workflow improvement.',
    '- Example of what to omit: "The merging spinner in the session list now reads',
    '  a clean Merging PR... without the redundant (esc to return to list) hint."',
    '- Lead with what a user can do now, what became easier, or what failure they',
    '  no longer hit. Mention internals only when the user already sees that exact',
    '  label in the product.',
    '- Avoid implementation jargon such as RPC, daemon, endpoint, JSONL, read model,',
    '  bridge, protobuf, plugin lifecycle, cache internals, usage probe, OAuth',
    '  endpoint, AgentRunnerService, TUI proof driver, and settle timeout.',
    '- If a technical source says "ProbeRateLimit RPC", write something like "Boss',
    '  can now show when an account is close to its usage limit."',
    '- If a technical source says "daemon restart socket wait", write something',
    '  like "Restarting Boss is less likely to show a false connection error."',
    '',
    'STRUCTURE:',
    '- If there is one clear headline feature, start with a short "###" heading and',
    '  one or two short paragraphs explaining the user benefit.',
    '- Then use "### Improvements" and "### Fixes" sections when useful. Omit empty',
    '  sections. Use "### Features" only for genuinely user-facing new things.',
    '- Keep bullets short. Most bullets should fit on one line in the website.',
    '- It is okay to include a light, human aside if it clarifies the change, but',
    '  do not force jokes.',
    '',
    'VOICE:',
    '- Friendly, clear, and matter-of-fact. More product update than engineering',
    '  report.',
    '- Ban hype and filler: no "exciting", "powerful", "seamless", "robust",',
    '  "supercharge", "unleash", "delighted to", "we are thrilled", "game-changing",',
    '  "effortless", "blazing", "magic", or exclamation marks.',
    '- No "Highlights" heading or marketing lead-in. Do not say "this release',
    '  focuses on..." or "as part of our commitment to...".',
    '- Never use em dashes ("—") or "--". Use commas, colons, semicolons, periods,',
    '  or parentheses instead.',
    '- Only state what the provided sources support. Do not invent capabilities,',
    '  numbers, or guarantees.',
    '',
    'Output GitHub-flavored markdown for the BODY only: no frontmatter, no',
    'top-level "#" title, no closing remarks.',
    '',
    '## Merged pull requests',
    prLines || '(none)',
    '',
    '## Linked Linear tickets (plans/context)',
    ticketLines || '(none)',
    '',
    '## Other commits',
    commitLines || '(none)',
  ].join('\n')
}
