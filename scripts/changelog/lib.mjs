// Pure, network-free helpers for the changelog generator. Unit-tested in
// lib.test.mjs. All I/O lives in ../generate-changelog.mjs.

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
    if (line) return line.slice(0, 140)
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
    `You are the release engineer writing the public changelog entry for Bossanova`,
    `release v${version}. You are a trusted, factual record of what changed, not a`,
    'marketer. Readers are technical users who rely on this entry to understand a',
    'release accurately.',
    '',
    'Synthesize the underlying changes into themes a user understands. Do NOT dump',
    'raw commits, PR numbers, branch names, or ticket IDs. Group bullets under',
    '"### Features", "### Improvements", and "### Fixes", in that order, omitting',
    'any group that is empty. Use short, self-contained bullets, each stating',
    'plainly what changed and what it now lets the user do.',
    '',
    'VOICE (follow exactly):',
    '- Calm, precise, professional. State facts; do not sell. Describe what the',
    '  software does now, not how exciting or powerful it is.',
    '- Ban hype and filler: no "exciting", "powerful", "seamless", "robust",',
    '  "supercharge", "unleash", "delighted to", "we are thrilled", "game-changing",',
    '  "effortless", "blazing", "magic", or exclamation marks.',
    '- No editorializing or meta-commentary ("this release focuses on...", "as part',
    '  of our commitment to..."). Let the changes speak for themselves.',
    '- Do NOT open with a summary, lead-in, or "Highlights" paragraph that restates',
    '  the bullets. Begin directly with the first "###" group.',
    '- Never use em dashes ("—") or "--". Use commas, colons, semicolons, periods,',
    '  or parentheses instead.',
    '- Prefer plain present tense ("Sessions can now start from an existing PR").',
    '  Every word earns its place; cut anything that does not add information.',
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
