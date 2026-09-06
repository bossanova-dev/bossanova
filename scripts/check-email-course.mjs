#!/usr/bin/env node
// Mechanical gate for the trial onboarding email course under docs/email-course/.
//
// BOS-972 shipped seven emails whose only guard was human judgement, and the copy
// drifted into flat marketing prose that named a skill the product does not ship.
// BOS-1035 rewrote the course and added this checker so the next agent to touch
// these files cannot quietly regress the arc, the voice, or the accuracy.
//
// Nine rule groups, each independently reported:
//
//   structure    eight bodies, one `**Subject:**` line each, every subject and
//                send offset recorded in README.md, offsets exactly 0-6 plus 12.
//   length       teaching bodies are 350-450 prose words; the day-12 conversion
//                send is capped at 250.
//   concreteness every teaching body carries at least one fenced code block, so a
//                reader has something to run. Day 12 is exempt.
//   voice        the banned register from VOICE.md matches nowhere in the prose.
//   punctuation  a sentence contains at most one em dash. Separate understated
//                asides remain valid; only the rhythm-crutch cluster fails.
//   skill-names  every `boss-*` token names a skill the binary actually publishes.
//                The allowlist is PARSED from skills_manifest_test.go rather than
//                copied, so it tracks the source of truth instead of drifting from
//                it. That file is why this gate exists: the docs skill table lists
//                a core the publish set dropped, and an implementer reading the
//                docs would name a skill trial users never receive.
//   links        every docs.bossanova.dev URL resolves against services/docs/docs/,
//                anchors included, against the repo rather than the network — so a
//                dead link fails in CI instead of silently rotting. Relative links
//                BETWEEN course files (`](./day-2-plan.md)`) resolve against the
//                course directory: renaming a body and updating only one of the two
//                places README.md names it is the breakage the plan predicted.
//                Same-page anchors (`](#loops-setup)`) resolve against the file's
//                own headings, which is the one link shape a reader follows without
//                leaving the page and the one nothing else here could see. And the
//                host of every absolute URL has to be the documentation site: the
//                guide used to link readers at a Linear ticket they cannot open,
//                and an allowlist is what stops that returning.
//   contract     the trial-enrolment event the guide names is the one the
//                application emits. The name is PARSED from the Go constant in
//                trial_enrollment.go rather than copied, so prose that duplicates
//                it cannot drift silently -- the same failure every other rule
//                here exists to prevent. The group also fails on any surviving
//                BOS-974 forward reference: the guide used to point at that ticket
//                in thirteen places, eight of them inside `Send offset:` lines that
//                the structure rule passes regardless, which is exactly the shape a
//                reviewer skims past.
//   numbers      no price string in authored prose (the charged amount is unresolved) —
//                fenced blocks are excluded so a positional shell argument is not read
//                as a price, while inline code is still scanned; and 14 days is the
//                only trial length the course claims, anywhere in the body.
//
// Node built-ins only, and no network. Runs from any working directory: the repo
// root is walked up from this file, so `node scripts/check-email-course.mjs` and
// `cd scripts && node check-email-course.mjs` behave identically.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

export const COURSE_DIR = path.join('docs', 'email-course')
export const DOCS_DIR = path.join('services', 'docs', 'docs')
export const MANIFEST_TEST = path.join(
  'services',
  'boss',
  'internal',
  'skillinstall',
  'skills_manifest_test.go',
)

// The Go source of truth for the trial-enrolment event name, and the symbol to
// bind. Kept beside MANIFEST_TEST above for the same reason: this checker's job is
// to compare prose against a parsed source value, never against a second copy.
export const TRIAL_ENROLLMENT_GO = path.join('services', 'bosso', 'cmd', 'trial_enrollment.go')
export const TRIAL_EVENT_SYMBOL = 'stripeTrialStartedEvent'

// The section of README.md that carries the payload contract, and the line inside
// it that binds the event name. Both are matched explicitly so that losing either
// is a violation rather than a silently unchecked pass.
export const CONTRACT_SECTION_HEADING = 'Loops setup'
const TRIGGERING_EVENT_RE = /^-\s+Triggering event:\s*`([^`]+)`/m

// Every backticked snake_case token in the guide. The contract rule compares each
// one against the parsed Go constant so that a rename cannot redden the single
// `- Triggering event:` line while twelve further copies keep asserting the old
// name. Genuine payload field names are not event names, so they are named here
// rather than left to widen the pattern into uselessness.
const EVENT_TOKEN_RE = /`([a-z][a-z0-9]*(?:_[a-z0-9]+)+)`/g
export const NON_EVENT_SNAKE_TOKENS = new Set(['stripe_subscription_id'])

// Ticket references of any kind. This started life pinned to the single ticket
// this directory used to defer to (BOS-974), which made it vacuous the moment the
// guide deferred to a different one instead -- README.md shipped an uncaught
// `(BOS-969)` under exactly that hole. Any surviving mention means the guide is
// still deferring to a tracker rather than naming the contract.
const FORWARD_REFERENCE_RE = /\bBOS-\d+\b/g

export const EXPECTED_OFFSETS = [0, 1, 2, 3, 4, 5, 6, 12]
export const RECAP_OFFSET = 12
export const TEACHING_MIN_WORDS = 350
export const TEACHING_MAX_WORDS = 450
export const RECAP_MAX_WORDS = 250
export const TRIAL_DAYS = 14

const BANNED_WORDS_STYLE = path.join(
  'services',
  'docs',
  'styles',
  'BossanovaProse',
  'BannedWords.yml',
)

// The Vale rule is the shared register for the docs site and email course. Parse
// its token list instead of copying it here, so either prose gate changing the
// register changes both surfaces in the same commit.
export function parseBannedPhrases(source) {
  const block = source.match(/^tokens:\s*\n((?:\s+- [^\n]+\n?)+)/m)?.[1]
  if (!block) throw new Error('BannedWords.yml: non-empty tokens list not found')
  const tokens = block.split(/\r?\n/).filter(Boolean)
  if (tokens.some((line) => !/^  - [a-z]+(?: [a-z]+)*$/.test(line))) {
    throw new Error('BannedWords.yml: tokens must be unquoted lowercase words without comments')
  }
  return tokens.map((line) => line.slice(4))
}

export const BANNED_PHRASES = parseBannedPhrases(
  fs.readFileSync(
    path.join(findRepoRoot(path.dirname(fileURLToPath(import.meta.url))), BANNED_WORDS_STYLE),
    'utf8',
  ),
)

const DOC_URL_RE = /https:\/\/docs\.bossanova\.dev(\/[^\s)\]"'`]*)?/g
const SKILL_TOKEN_RE = /\bboss(?:-[a-z0-9]+)+/g
const PRICE_RE = /\$\s*\d/

// Trial-length claims only. A bare "12 days" is a send offset, not a trial length,
// so proximity to the word "trial" is not enough — the claim has to be phrased as one.
//
// Stored as SOURCES, not as shared /g literals. A /g regex carries a mutable
// `lastIndex`, `RegExp.prototype.test` advances it, and `String.matchAll` seeds its
// clone FROM it — so a shared literal that the README claim-present check tests
// (below) starts the next file's scan part-way in, and a violation sitting before
// that offset is silently missed. That is invisible in a one-shot CLI run and
// order-dependent across the unit tests, which is the worst shape for the one gate
// whose whole purpose is that it cannot quietly pass. Build a fresh regex per use.
const TRIAL_LENGTH_SOURCES = [
  String.raw`\b(\d+)[-\s]day\s+trial\b`,
  String.raw`\btrial\s+(?:is|lasts|runs(?:\s+for)?)\s+(?:\*\*)?(\d+)\s*days?\b`,
  String.raw`\btrial\s+length[^.\n]{0,24}?(\d+)\s*days?`,
]

/** A fresh, unshared trial-length matcher — never a module-level /g literal. */
export function trialLengthPatterns() {
  return TRIAL_LENGTH_SOURCES.map((source) => new RegExp(source, 'gi'))
}

// Relative links between course files, e.g. `[day-2-plan.md](./day-2-plan.md)`.
// README.md carries sixteen of them across the arc table and the per-send Sequence
// definition, and DOC_URL_RE cannot see any of them — it only matches absolute
// docs.bossanova.dev URLs. Renaming a body is exactly the breakage the plan's Risks
// section predicted, so resolve these against the course directory too.
const RELATIVE_LINK_RE = /\]\(\.\/([^)#]+?\.md)(#[^)]*)?\)/g

// Same-page anchors, e.g. `[Loops setup](#loops-setup)`. Neither of the two link
// rules above can see one: it names no route and no file. The README leans on them
// to send a reader from the intro to the payload contract, so a heading rename
// silently strands that reader mid-page.
const SAME_PAGE_ANCHOR_RE = /\]\(#([^)\s]+)\)/g

// The host of every absolute URL in the course. Only the documentation site is
// allowed, because it is the only host this repository can verify a page on -- the
// route check above resolves it against services/docs/docs/. Anything else is
// unverifiable by construction, and the specific regression is a ticket link: the
// guide pointed trial readers at linear.app in two places, which no reader outside
// the company can open.
const ABSOLUTE_URL_RE = /\bhttps?:\/\/([^\s/)\]"'`]+)/g
export const ALLOWED_LINK_HOSTS = ['docs.bossanova.dev']

// ---------------------------------------------------------------------------
// Repo root

/** Walk up from `startDir` to the directory holding both `.git` and `Makefile`. */
export function findRepoRoot(startDir) {
  let dir = startDir
  for (;;) {
    if (fs.existsSync(path.join(dir, '.git')) && fs.existsSync(path.join(dir, 'Makefile'))) {
      return dir
    }
    const parent = path.dirname(dir)
    if (parent === dir) {
      throw new Error(`could not find a repo root above ${startDir}`)
    }
    dir = parent
  }
}

// ---------------------------------------------------------------------------
// Published skill set

/**
 * Parse the published skill set out of skills_manifest_test.go.
 *
 * Scoped to TestEmbeddedSkillManifestExcludesBossProof's `want` slice: the file
 * holds other `want := []string{...}` literals, and one of them deliberately names
 * an unpublished core. Fails closed rather than returning a partial allowlist.
 */
export function parsePublishedSkills(source) {
  const anchor = source.indexOf('func TestEmbeddedSkillManifestExcludesBossProof(')
  if (anchor === -1) {
    throw new Error('skills_manifest_test.go: TestEmbeddedSkillManifestExcludesBossProof not found')
  }
  const open = source.indexOf('want := []string{', anchor)
  if (open === -1) {
    throw new Error(
      'skills_manifest_test.go: no `want := []string{` inside TestEmbeddedSkillManifestExcludesBossProof',
    )
  }
  const close = source.indexOf('}', open)
  if (close === -1) {
    throw new Error('skills_manifest_test.go: unterminated `want` slice')
  }
  const names = [...source.slice(open, close).matchAll(/"([^"]+)"/g)].map((m) => m[1])
  if (names.length === 0) {
    throw new Error('skills_manifest_test.go: `want` slice parsed as empty')
  }
  return names
}

// ---------------------------------------------------------------------------
// Go string constants

/**
 * Parse a single Go string constant, failing closed.
 *
 * Modelled on parseGoConstant in check-settings-readiness-figure.mjs -- same
 * shape, same reason: throw, naming both file and symbol, on zero matches AND on
 * more than one, because a checker that cannot find its binding target must not
 * quietly proceed with nothing to compare against. It deliberately does NOT reuse
 * that function: it coerces the capture through `Number(...)`, which turns every
 * string constant into NaN.
 *
 * The pattern is line-anchored, so a qualified reference elsewhere
 * (`pkg.stripeTrialStartedEvent`) cannot masquerade as a second declaration; the
 * negative lookbehind states that intent even where the anchor already implies it.
 * An empty value throws too -- a zero-token event name would compare equal to a
 * guide that names nothing.
 *
 * @param {string} goSource Whole Go file text.
 * @param {{symbol: string, file: string}} spec
 * @returns {string} The unquoted constant value.
 */
export function parseGoStringConstant(goSource, { symbol, file }) {
  const escaped = symbol.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const pattern = new RegExp(
    String.raw`^[\t ]*(?:const[\t ]+)?(?<![\w.])${escaped}[\t ]*=[\t ]*"([^"\\]*)"[\t ]*$`,
    'gm',
  )
  const matches = [...String(goSource).matchAll(pattern)]

  if (matches.length === 0) {
    throw new Error(
      `Cannot parse ${symbol} from ${file}: no declaration of the form ` +
        `\`const ${symbol} = "..."\` matched. Either the constant was renamed or removed, ` +
        'or its declaration was reformatted past the pattern in check-email-course.mjs. ' +
        'Fix the pattern rather than deleting the check -- an unparsed constant leaves the ' +
        'event name the course documents with nothing binding it.',
    )
  }
  if (matches.length > 1) {
    throw new Error(
      `Ambiguous ${symbol} in ${file}: ${matches.length} declarations matched, so there is no ` +
        'single event name to compare the course against. Narrow the pattern in ' +
        'check-email-course.mjs.',
    )
  }
  const value = matches[0][1]
  if (value === '') {
    throw new Error(
      `Empty ${symbol} in ${file}: the constant parsed as an empty string, which would compare ` +
        'equal to a guide that names no event at all. Give it a value or remove the check.',
    )
  }
  return value
}

// ---------------------------------------------------------------------------
// Markdown helpers

/** Strip fenced code blocks, returning the prose that surrounds them. */
export function stripFencedBlocks(markdown) {
  const out = []
  let fenced = false
  for (const line of markdown.split('\n')) {
    if (/^\s*```/.test(line)) {
      fenced = !fenced
      continue
    }
    if (!fenced) out.push(line)
  }
  return out.join('\n')
}

/** Count fenced code blocks (opening fences). */
export function countFencedBlocks(markdown) {
  let opens = 0
  let fenced = false
  for (const line of markdown.split('\n')) {
    if (/^\s*```/.test(line)) {
      if (!fenced) opens += 1
      fenced = !fenced
    }
  }
  return opens
}

/**
 * The authored prose of an email body: everything except the fenced blocks, the H1
 * title, and the `**Subject:**` line. Pasted tool output is evidence, not voice, so
 * it is neither counted nor scanned for banned phrases.
 */
export function proseOf(markdown) {
  return stripFencedBlocks(markdown)
    .split('\n')
    .filter((l) => !/^#\s/.test(l) && !/^\*\*Subject:\*\*/.test(l))
    .join('\n')
}

/** Word count over prose: link text counts, link targets and backticks do not. */
export function countWords(prose) {
  return prose
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/`+/g, ' ')
    .split(/\s+/)
    .filter((tok) => /[A-Za-z0-9]/.test(tok)).length
}

/** Word-boundary matcher that treats an apostrophe as part of the word. */
export function bannedPhraseRegex(phrase) {
  const body = phrase
    .split(/\s+/)
    .map((w) => w.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
    .join('\\s+')
  return new RegExp(`(?<![A-Za-z0-9'])${body}(?![A-Za-z0-9'])`, 'gi')
}

/** Split prose at sentence-ending punctuation or paragraph boundaries. */
export function proseSentences(prose) {
  const protectedPeriod = '\uE000'
  const protectedProse = prose
    .replace(/\b(?:e\.g|i\.e|vs|etc)\./gi, (abbreviation, offset, source) => {
      const nextCharacter = source.slice(offset + abbreviation.length).match(/^\s+(\S)/u)?.[1]
      const finalPeriod = nextCharacter && /[\p{Ll}\d]/u.test(nextCharacter) ? protectedPeriod : '.'
      return abbreviation.slice(0, -1).replaceAll('.', protectedPeriod) + finalPeriod
    })
    .replace(/\b(?:Mr|Mrs|Ms|Dr|Prof|Sr|Jr|St)\.(?=\s+\p{L})/gu, (abbreviation) =>
      abbreviation.replace('.', protectedPeriod),
    )
    .replace(/\b(?:[A-Z]\.[ \t]*){2,}(?=[A-Z]\p{Ll})/gu, (initials) =>
      initials.replaceAll('.', protectedPeriod),
    )
    .replace(/\b[A-Z]\.(?=\s+[A-Z]\p{Ll})/gu, (initial) => initial.replace('.', protectedPeriod))

  return protectedProse
    .split(/(?<=[.!?])(?:\s+|$)|\n\s*\n|\n(?=\s*[-*]\s)/u)
    .map((sentence) => sentence.replaceAll(protectedPeriod, '.').trim())
    .filter(Boolean)
}

/** Paired dashes around code or a comma-separated list are structure, not rhythm. */
export function isStructuredDashAside(sentence) {
  const parts = sentence.split('—')
  if (parts.length !== 3) return false
  const aside = parts[1]
  return /`[^`]+`/.test(aside) || (aside.match(/,/g)?.length ?? 0) >= 2
}

/**
 * The body of a `## <heading>` section, up to the next heading of the same or a
 * higher level. Returns null when the section is absent, so the caller can report
 * that as a violation rather than scanning an empty string and passing.
 */
export function sectionBody(markdown, heading) {
  const lines = markdown.split('\n')
  const start = lines.findIndex((line) => line.trim() === `## ${heading}`)
  if (start === -1) return null
  const rest = lines.slice(start + 1)
  const end = rest.findIndex((line) => /^#{1,2}\s/.test(line))
  return (end === -1 ? rest : rest.slice(0, end)).join('\n')
}

/**
 * Every anchor a same-page link can reach inside one markdown file. Fenced blocks
 * are stripped first so a commented-out `# heading` pasted as terminal output does
 * not invent an anchor that a rendered page will not have.
 */
export function headingSlugs(markdown) {
  const slugs = new Set()
  for (const [, text] of stripFencedBlocks(markdown).matchAll(/^#{1,6}[\t ]+(.+?)[\t ]*$/gm)) {
    slugs.add(slugifyHeading(text))
  }
  return slugs
}

/** Approximate Docusaurus heading anchors closely enough for repo-local links. */
export function slugifyHeading(text) {
  const explicit = text.match(/\{#([^}]+)\}\s*$/)
  if (explicit) return explicit[1]
  return (
    text
      .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
      .replace(/[`*_]/g, '')
      .toLowerCase()
      .replace(/[^a-z0-9\s-]/g, '')
      .trim()
      // github-slugger (which Docusaurus uses) replaces each whitespace character
      // individually rather than collapsing runs, so 'add — the' becomes 'add--the'.
      .replace(/\s/g, '-')
  )
}

function frontMatterSlug(content) {
  if (!content.startsWith('---')) return null
  const end = content.indexOf('\n---', 3)
  if (end === -1) return null
  const m = content.slice(0, end).match(/^slug:\s*(.+?)\s*$/m)
  if (!m) return null
  return m[1].replace(/^['"]|['"]$/g, '')
}

/** Normalise a docs route to its comparable form (no leading or trailing slash). */
export function normaliseRoute(route) {
  return route.replace(/^\/+/, '').replace(/\/+$/, '')
}

/**
 * Build route -> anchors from a map of docs-relative path -> content.
 *
 * A front-matter `slug:` OVERRIDES the file path, which is the whole reason this is
 * not a filename check: guides/agent-plugins.md publishes as /guides/agent-runners,
 * and skills/overview.md as /skills. A path-only resolver reds those valid links.
 */
export function buildRoutesFromFiles(fileMap) {
  const routes = new Map()
  for (const [rel, content] of fileMap) {
    if (!/\.mdx?$/.test(rel)) continue
    const slug = frontMatterSlug(content)
    const route = normaliseRoute(slug ?? rel.replace(/\\/g, '/').replace(/\.mdx?$/, ''))
    const anchors = new Set()
    let fenced = false
    for (const line of content.split('\n')) {
      if (/^\s*```/.test(line)) {
        fenced = !fenced
        continue
      }
      if (fenced) continue
      const h = line.match(/^#{1,6}\s+(.*?)\s*$/)
      if (h) anchors.add(slugifyHeading(h[1]))
    }
    routes.set(route, anchors)
  }
  return routes
}

function readDocFiles(docsDir) {
  const files = new Map()
  const walk = (dir, prefix) => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const abs = path.join(dir, entry.name)
      const rel = prefix ? `${prefix}/${entry.name}` : entry.name
      if (entry.isDirectory()) walk(abs, rel)
      else if (/\.mdx?$/.test(entry.name)) files.set(rel, fs.readFileSync(abs, 'utf8'))
    }
  }
  walk(docsDir, '')
  return files
}

export function collectDocRoutes(docsDir) {
  return buildRoutesFromFiles(readDocFiles(docsDir))
}

// ---------------------------------------------------------------------------
// The rules

const BODY_RE = /^day-(\d+)-[a-z0-9-]+\.md$/

/**
 * @param {{
 *   files: Map<string,string>,
 *   routes: Map<string,Set<string>>,
 *   publishedSkills: string[],
 *   trialEnrolmentEvent: string,
 * }} input `trialEnrolmentEvent` is the value parsed out of the Go constant by
 *   `run()`; it is injected rather than read here so this function stays the pure
 *   half and every input is substitutable from a test.
 * @returns {{rule: string, file: string, message: string}[]}
 */
export function checkCourse({ files, routes, publishedSkills, trialEnrolmentEvent }) {
  const violations = []
  const fail = (rule, file, message) => violations.push({ rule, file, message })

  const bodies = [...files.keys()]
    .filter((name) => BODY_RE.test(name))
    .map((name) => ({ name, offset: Number(name.match(BODY_RE)[1]) }))
    .sort((a, b) => a.offset - b.offset)

  // --- structure -----------------------------------------------------------
  if (bodies.length !== EXPECTED_OFFSETS.length) {
    fail(
      'structure',
      '.',
      `expected ${EXPECTED_OFFSETS.length} email bodies, found ${bodies.length}`,
    )
  }
  const offsets = bodies.map((b) => b.offset)
  if (offsets.join(',') !== EXPECTED_OFFSETS.join(',')) {
    fail(
      'structure',
      '.',
      `send offsets are [${offsets.join(', ')}]; expected [${EXPECTED_OFFSETS.join(', ')}]`,
    )
  }

  const readme = files.get('README.md')
  if (readme === undefined) {
    fail('structure', 'README.md', 'README.md is missing')
  }
  if (!files.has('VOICE.md')) {
    fail('structure', 'VOICE.md', 'VOICE.md is missing')
  }

  for (const { name, offset } of bodies) {
    const content = files.get(name)
    const subject = content.match(/^\*\*Subject:\*\*\s*(\S.*?)\s*$/m)
    if (!subject) {
      fail('structure', name, 'no `**Subject:**` line')
    } else if (readme !== undefined) {
      const line = new RegExp(
        `^Subject: ${subject[1].replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`,
        'm',
      )
      if (!line.test(readme)) {
        fail(
          'structure',
          'README.md',
          `no \`Subject: ${subject[1]}\` line recording ${name}'s subject`,
        )
      }
    }
    if (readme !== undefined) {
      const offsetLine = new RegExp(`^Send offset: ${offset} days?\\b`, 'm')
      if (!offsetLine.test(readme)) {
        fail('structure', 'README.md', `no \`Send offset: ${offset} day(s)\` line for ${name}`)
      }
    }

    // --- length ------------------------------------------------------------
    const words = countWords(proseOf(content))
    if (offset === RECAP_OFFSET) {
      if (words > RECAP_MAX_WORDS) {
        fail('length', name, `${words} prose words; the recap send is capped at ${RECAP_MAX_WORDS}`)
      }
    } else if (words < TEACHING_MIN_WORDS || words > TEACHING_MAX_WORDS) {
      fail(
        'length',
        name,
        `${words} prose words; teaching sends must be ${TEACHING_MIN_WORDS}-${TEACHING_MAX_WORDS}`,
      )
    }

    // --- concreteness ------------------------------------------------------
    if (offset !== RECAP_OFFSET && countFencedBlocks(content) < 1) {
      fail(
        'concreteness',
        name,
        'no fenced code block; every teaching send needs something the reader runs',
      )
    }

    // --- voice -------------------------------------------------------------
    const prose = proseOf(content)
    for (const phrase of BANNED_PHRASES) {
      const hits = prose.match(bannedPhraseRegex(phrase))
      if (hits) {
        fail('voice', name, `banned phrase "${phrase}" appears ${hits.length}x — see VOICE.md`)
      }
    }

    // --- punctuation -------------------------------------------------------
    for (const sentence of proseSentences(prose)) {
      const dashes = sentence.match(/—/g)?.length ?? 0
      if (dashes > 1 && !isStructuredDashAside(sentence)) {
        fail('punctuation', name, `${dashes} em dashes in one sentence; use at most one`)
      }
    }
  }

  const allowed = new Set(publishedSkills)
  for (const [name, content] of files) {
    // --- skill names -------------------------------------------------------
    for (const token of new Set(content.match(SKILL_TOKEN_RE) ?? [])) {
      if (!allowed.has(token)) {
        fail(
          'skill-names',
          name,
          `\`${token}\` is not in the published skill set [${publishedSkills.join(', ')}]`,
        )
      }
    }

    // --- links -------------------------------------------------------------
    for (const [, rawPath] of content.matchAll(DOC_URL_RE)) {
      const [routePart, anchor] = (rawPath ?? '/').split('#')
      const route = normaliseRoute(routePart)
      if (!routes.has(route)) {
        fail(
          'links',
          name,
          `https://docs.bossanova.dev/${route} resolves to no page in ${DOCS_DIR}`,
        )
        continue
      }
      if (anchor && !routes.get(route).has(anchor)) {
        fail(
          'links',
          name,
          `https://docs.bossanova.dev/${route}#${anchor} names no heading on that page`,
        )
      }
    }

    // --- links (relative, between course files) ------------------------------
    for (const [, target] of content.matchAll(RELATIVE_LINK_RE)) {
      if (!files.has(target)) {
        fail('links', name, `./${target} names no file in ${COURSE_DIR}`)
      }
    }

    // --- links (same page) ---------------------------------------------------
    const slugs = headingSlugs(content)
    for (const [, anchor] of content.matchAll(SAME_PAGE_ANCHOR_RE)) {
      if (!slugs.has(anchor)) {
        fail(
          'links',
          name,
          `](#${anchor}) names no heading in ${name}; its anchors are ` +
            `[${[...slugs].join(', ')}]`,
        )
      }
    }

    // --- links (absolute host allowlist) -------------------------------------
    for (const [, host] of content.matchAll(ABSOLUTE_URL_RE)) {
      if (!ALLOWED_LINK_HOSTS.includes(host)) {
        fail(
          'links',
          name,
          `links to ${host}, which nothing here can verify; only ` +
            `[${ALLOWED_LINK_HOSTS.join(', ')}] is allowed in course copy`,
        )
      }
    }

    // --- contract (forward references) --------------------------------------
    // The guide once deferred the event name to BOS-974 in thirteen places. Eight
    // of those sat inside `Send offset:` lines, which the structure rule matches
    // with no end anchor and therefore passes either way -- so nothing mechanical
    // forced them out, and nothing mechanical would stop them coming back.
    //
    // Prose only, for the same reason the numbers and voice rules are prose-only: a
    // fenced `/boss-plan BOS-1042` is a worked example of the CLI, not the guide
    // deferring its own contract to a ticket. Inline code is still scanned, so a
    // prose `(BOS-969)` still fails.
    const forwardReferences = stripFencedBlocks(content).match(FORWARD_REFERENCE_RE) ?? []
    if (forwardReferences.length > 0) {
      fail(
        'contract',
        name,
        `${forwardReferences.length} forward reference(s) to a tracker ticket remain ` +
          `(${[...new Set(forwardReferences)].sort().join(', ')}); the event name and payload ` +
          'are settled and recorded in README.md, so name the contract here instead',
      )
    }

    // --- numbers -----------------------------------------------------------
    // Prose only, for the same reason the voice rule is prose-only: a fenced snippet's
    // positional argument (`awk '{print $1}'`) is not a price, and matching it would red
    // the gate with a message the author cannot act on. Inline code is still scanned —
    // stripFencedBlocks keeps backticked spans, so a prose `$29/mo` still fails.
    if (PRICE_RE.test(stripFencedBlocks(content))) {
      fail(
        'numbers',
        name,
        'a price string is present; the charged amount is unresolved and no send states one',
      )
    }
    // The trial-length half deliberately scans RAW content, unlike the price half above:
    // a wrong trial length is just as misleading inside a fenced block as in prose, and
    // no shell or code shape collides with `N-day trial` the way `$1` collides with a price.
    for (const pattern of trialLengthPatterns()) {
      for (const m of content.matchAll(pattern)) {
        if (Number(m[1]) !== TRIAL_DAYS) {
          fail('numbers', name, `claims a ${m[1]}-day trial; the trial is ${TRIAL_DAYS} days`)
        }
      }
    }
  }

  // --- contract (event name) -------------------------------------------------
  // Bounded at every narrowing stage. An unparsed constant, a missing section, or a
  // missing binding line each fail here rather than leaving a green run that
  // compared nothing -- which is the exact way the last gate added to this repo
  // shipped vacuous.
  if (typeof trialEnrolmentEvent !== 'string' || trialEnrolmentEvent === '') {
    fail(
      'contract',
      TRIAL_ENROLLMENT_GO,
      `no ${TRIAL_EVENT_SYMBOL} value was supplied, so the event name the course documents was ` +
        'not checked against anything; refusing to pass with nothing checked',
    )
  } else if (readme !== undefined) {
    const section = sectionBody(readme, CONTRACT_SECTION_HEADING)
    if (section === null) {
      fail(
        'contract',
        'README.md',
        `no \`## ${CONTRACT_SECTION_HEADING}\` section, so there is nowhere recording the event ` +
          `name to compare against ${TRIAL_EVENT_SYMBOL} in ${TRIAL_ENROLLMENT_GO}`,
      )
    } else {
      const named = section.match(TRIGGERING_EVENT_RE)
      if (!named) {
        fail(
          'contract',
          'README.md',
          `\`## ${CONTRACT_SECTION_HEADING}\` names no triggering event; add a ` +
            '`- Triggering event: `<name>`` line so the guide is bound to ' +
            `${TRIAL_EVENT_SYMBOL} in ${TRIAL_ENROLLMENT_GO}`,
        )
      } else if (named[1] !== trialEnrolmentEvent) {
        fail(
          'contract',
          'README.md',
          `documents the triggering event as \`${named[1]}\`, but ${TRIAL_EVENT_SYMBOL} in ` +
            `${TRIAL_ENROLLMENT_GO} is \`${trialEnrolmentEvent}\`; a name that does not match ` +
            'leaves every enrolment call succeeding and no email ever sending',
        )
      }
    }
  }

  // --- contract (every other copy of the event name) --------------------------
  // The rule above binds one line. The guide names the event thirteen times, so
  // binding only `- Triggering event:` would let a rename redden that line and
  // leave twelve copies still documenting the old name after it was fixed.
  if (
    typeof trialEnrolmentEvent === 'string' &&
    trialEnrolmentEvent !== '' &&
    readme !== undefined
  ) {
    const stale = new Set()
    for (const [, token] of readme.matchAll(EVENT_TOKEN_RE)) {
      if (NON_EVENT_SNAKE_TOKENS.has(token) || token === trialEnrolmentEvent) continue
      stale.add(token)
    }
    if (stale.size > 0) {
      fail(
        'contract',
        'README.md',
        `names ${[...stale]
          .sort()
          .map((t) => `\`${t}\``)
          .join(', ')} where the event name ` +
          `belongs, but ${TRIAL_EVENT_SYMBOL} in ${TRIAL_ENROLLMENT_GO} is ` +
          `\`${trialEnrolmentEvent}\`; every copy is bound, not just the ` +
          '`- Triggering event:` line, so a rename cannot leave stale copies behind ' +
          '(a genuine payload field name belongs in NON_EVENT_SNAKE_TOKENS)',
      )
    }
  }

  if (readme !== undefined) {
    const stated = trialLengthPatterns().some((p) => p.test(readme))
    if (!stated) {
      fail(
        'numbers',
        'README.md',
        `no trial-length claim to check; state the ${TRIAL_DAYS}-day trial explicitly`,
      )
    }
  }

  return violations
}

// ---------------------------------------------------------------------------
// Entry point

export function run(repoRoot) {
  const courseDir = path.join(repoRoot, COURSE_DIR)
  const files = new Map()
  for (const entry of fs.readdirSync(courseDir, { withFileTypes: true })) {
    if (entry.isFile() && entry.name.endsWith('.md')) {
      files.set(entry.name, fs.readFileSync(path.join(courseDir, entry.name), 'utf8'))
    }
  }
  return checkCourse({
    files,
    routes: collectDocRoutes(path.join(repoRoot, DOCS_DIR)),
    publishedSkills: parsePublishedSkills(
      fs.readFileSync(path.join(repoRoot, MANIFEST_TEST), 'utf8'),
    ),
    trialEnrolmentEvent: parseGoStringConstant(
      fs.readFileSync(path.join(repoRoot, TRIAL_ENROLLMENT_GO), 'utf8'),
      { symbol: TRIAL_EVENT_SYMBOL, file: TRIAL_ENROLLMENT_GO },
    ),
  })
}

function main() {
  const here = path.dirname(fileURLToPath(import.meta.url))
  const repoRoot = findRepoRoot(here)
  const violations = run(repoRoot)
  if (violations.length === 0) {
    console.log(`check-email-course: ${COURSE_DIR} passes all rules`)
    return
  }
  for (const v of violations) {
    console.error(`${path.join(COURSE_DIR, v.file)}: [${v.rule}] ${v.message}`)
  }
  console.error(
    `check-email-course: ${violations.length} violation(s) across ${
      new Set(violations.map((v) => v.rule)).size
    } rule group(s)`,
  )
  process.exitCode = 1
}

if (isMainModule(import.meta.url)) {
  main()
}
