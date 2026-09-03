#!/usr/bin/env node

// Gate: every DISPLAYED Bossanova Cloud price agrees, across build surfaces
// that cannot import from each other.
//
// The web app, the marketing site and PRODUCT.md are three separate build
// surfaces — two apps in the pnpm workspace plus a prose file that cannot
// import anything at all — so "one constant" cannot be enforced by the module
// graph. This gate is the seam that replaces it. It enforces two things:
//
//   1. Routing. Each app declares the price ONCE (services/web/src/pricing.ts,
//      services/marketing/src/lib/pricing.ts) and the surfaces that RENDER a
//      price import it rather than re-typing it. A render surface containing a
//      price literal at all is a failure, even a literal that happens to agree
//      today — agreeing-by-luck is the state this ticket exists to end.
//   2. Agreement. Every price-shaped literal left anywhere in the covered
//      trees — prose, comments, docstrings — states an amount the declarations
//      actually declare or derive. Comments drift silently; this is what
//      catches them.
//
// Scope: DISPLAYED price only. The CHARGED amount lives in Stripe behind
// BOSSO_STRIPE_CLOUD_PRICE_ID and is not readable at build time, so a green
// gate proves the surfaces agree with each other — never that a customer is
// charged what they are shown. Changing the CHARGED amount — minting the
// Stripe Price and migrating live subscriptions — is a separate job; changing
// the DISPLAYED amount is a one-line edit to each surface's declaration.
//
// This gate is the SOLE owner of the displayed price. That was briefly untrue:
// BOS-969 raised the price to $9 and, to sweep it safely, taught
// lib/bossalib/productparity to hold the same five surfaces to a hardcoded
// `const cloudMonthlyPrice`. Two owners for one property is the defect this
// ticket exists to remove — and a copy-to-copy literal check cannot survive
// the surfaces deriving their price from a declaration, so it would have gone
// red the moment routing landed. Its one assertion with no equivalent here,
// that PRODUCT.md states a price at all, is absorbed below as PROSE_SURFACES.
// Do not re-add a second price gate; extend this one.
//
// A note on the amounts in the docstrings below. Invented examples use `$7` —
// deliberately NOT the live price — because a docstring that quoted the live
// figure would be one more place stating it, which is the thing this gate
// exists to prevent, and would go stale the next time the price moved. `$7`
// was the live price until BOS-969; if that reads as staleness rather than as
// an example, pick another amount the product has never charged, not the
// current one. The single exception is the PROSE_SURFACES docstring, which
// quotes the deleted Go gate's own required substring `price="$9"`; a
// quotation cannot be re-denominated without becoming false, so it stays. That
// is the bar for adding another: quoting history, not illustrating a rule.
//
// A sibling gate owns adjacent ground; check it before extending this one:
//   - lib/bossalib/productparity (Go, Bazel data deps) gates the trial length
//     ("14-day") across these same files plus the Stripe checkout policy and
//     the TUI. Trial length is deliberately NOT duplicated here, and price is
//     deliberately not gated there.
//   - scripts/check-email-course.mjs bans a price string outright in the
//     onboarding course, because no send is allowed to state one. That tree is
//     therefore not scanned here — "no price" and "the right price" are
//     different rules and must not both own the same files.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

/**
 * Files that declare the price. Each must declare every listed `names` entry as
 * a bare `export const NAME = <integer>` line and every `strings` entry as a
 * bare `export const NAME = '<text>'` line. All declarations of
 * CLOUD_PRICE_USD_PER_MONTH must agree with each other, and so must all
 * declarations of CLOUD_BILLING_PERIOD_SUFFIX: a displayed price is an amount
 * AND a period, and either half drifting is the same defect.
 */
export const DECLARATIONS = [
  {
    path: 'services/web/src/pricing.ts',
    names: ['CLOUD_PRICE_USD_PER_MONTH'],
    strings: ['CLOUD_BILLING_PERIOD_SUFFIX'],
  },
  {
    path: 'services/marketing/src/lib/pricing.ts',
    names: ['CLOUD_PRICE_USD_PER_MONTH', 'CLOUD_ANNUAL_DISCOUNT_PERCENT'],
    strings: ['CLOUD_BILLING_PERIOD_SUFFIX'],
  },
]

/**
 * Files that render a price to a user. These must import from a declaration
 * module and must contain no price-shaped literal whatsoever.
 *
 * Hand-maintained: nothing discovers a new price-rendering component. One that
 * hard-codes the amount currently displayed is not caught by this list — it is
 * caught by the agreement check below the moment a declaration changes, which
 * is when it matters. Add the file here when such a component lands.
 */
export const RENDER_SURFACES = [
  { path: 'services/web/src/pages/Subscribe.tsx', declaration: 'pricing' },
  {
    path: 'services/marketing/src/components/pricing/PricingCards.astro',
    declaration: 'lib/pricing',
  },
  { path: 'services/marketing/src/pages/pricing.astro', declaration: 'lib/pricing' },
  // Receives the price as props rather than importing it, so no import is
  // required — but it is still the component that puts an amount and a period
  // on screen, and a default value hard-coded into either prop would render a
  // price no declaration authorised. `declaration: null` says "no import
  // required", never "unchecked": both literal rules below still apply.
  {
    path: 'services/marketing/src/components/pricing/PricingCard.astro',
    declaration: null,
  },
]

/**
 * Trees walked for stray price literals.
 *
 * Ratcheted like SCAN_FILES and SCAN_DIRS: an entry that walks nothing is a
 * coverage hole, not an empty tree, and is reported rather than skipped. A
 * renamed or mistyped root would otherwise cover zero files and keep the gate
 * green over source nobody reads.
 *
 * This is the ONE coverage list a caller can replace, through checkPriceParity's
 * `scanRoots` option. That seam runs the opposite way to `--no-exemptions`:
 * dropping an exemption can only ever make the gate stricter, whereas dropping a
 * root makes it cover less. So the seam is additive-only — every name below must
 * appear in whatever list is passed, and one that does not is a failure in its
 * own right. See the ratchet in checkPriceParity.
 */
export const SCAN_ROOTS = ['services/web/src', 'services/marketing/src']

/**
 * Individual files scanned for stray price literals, outside the app trees.
 *
 * These bypass SCAN_EXTENSIONS deliberately, so a file is covered because it is
 * named here rather than because of what it is called.
 */
export const SCAN_FILES = ['PRODUCT.md']

/**
 * Prose surfaces: files that state the price to a reader in words and cannot
 * import a declaration, so they must carry the literal — and must actually
 * carry it, `minimum` times.
 *
 * Every other rule in this file is an AGREEMENT rule, and agreement cannot see
 * this defect: a file that stops stating the price altogether disagrees with
 * nothing. RENDER_SURFACES has the mirror-image rule (a registered surface must
 * NOT write a literal) for the same reason — a surface that renders no price is
 * not a surface that renders the right one.
 *
 * The rule is here because it was somewhere else first. `lib/bossalib/
 * productparity` (BOS-969) held every price surface to a hard-coded Go
 * constant by substring — `price="$9"` had to appear verbatim in
 * PricingCards.astro. That gate could not survive this ticket: once a surface
 * DERIVES its price, the string it must contain is `{CLOUD_PRICE}`, and
 * checking for that is checking the import, which RENDER_SURFACES above
 * already does. Its one assertion with no equivalent here was that the product
 * brief states a price at all, in both the places it states one. That
 * assertion is what this list is, moved to the file that owns the declaration
 * instead of restating the amount a second time in Go.
 *
 * `minimum` is a ratchet, not a target. PRODUCT.md states the price twice —
 * once to the evaluating team lead, once in the product summary — and losing
 * either is a silent narrowing of what the brief tells a reader. Raising the
 * number as the brief grows needs no argument; lowering it is a claim that a
 * statement stopped being worth making, and belongs in review.
 *
 * A prose surface must also be SCANNED, or this becomes a presence check with
 * no agreement check behind it: the file would be required to state a price and
 * free to state the wrong one. checkPriceParity fails an entry no coverage list
 * actually READS — being named by one is not enough, since SCAN_DIRS and
 * SCAN_ROOTS also filter by extension.
 */
export const PROSE_SURFACES = [{ path: 'PRODUCT.md', minimum: 2 }]

/**
 * Directories every matching file of which is scanned for stray price
 * literals.
 *
 * `proof/recipes` is covered as a DIRECTORY, not as the one recipe that
 * happens to exist. A recipe DESCRIPTION is prose a human reads, nowhere near
 * the declaration and unable to import it — the same drift defect as a stale
 * comment — and that is true of a recipe added tomorrow exactly as much as of
 * `default.json`. Naming a single path could not say so: the hole reopens with
 * the next sibling file. No recipe states a price today, and the rule here is
 * that none ever starts to disagree.
 *
 * A missing directory, or one holding no matching file, is a FAILURE rather
 * than an empty scan. That is the same ratchet SCAN_FILES applies to a renamed
 * file: a coverage set allowed to shrink silently to nothing is precisely the
 * vacuity this gate exists to prevent.
 *
 * `.md` is covered alongside `.json` because the argument above is STRONGER for
 * `proof/recipes/README.md` than for a recipe's description field: it is 14 KB
 * of prose a human reads, and scanning the directory while skipping the one
 * file in it written purely for humans would have been a strange place to stop.
 */
export const SCAN_DIRS = [{ path: 'proof/recipes', extensions: ['.json', '.md'] }]

/**
 * File types read when scanning SCAN_ROOTS for stray price literals.
 *
 * `.bazel` is scanned rather than ignored: a BUILD file is hand-written text
 * like any other, and nothing about it makes a displayed price impossible.
 */
const SCAN_EXTENSIONS = new Set([
  '.ts',
  '.tsx',
  '.js',
  '.jsx',
  '.mjs',
  '.astro',
  '.md',
  '.css',
  '.html',
  '.bazel',
])

/**
 * File types deliberately NOT read, each because a price literal there could
 * not be a displayed price a human reads out of the source.
 *
 * This list exists only to satisfy the ratchet below. Being an allowlist,
 * SCAN_EXTENSIONS could never announce the file type it was missing: a
 * `pricing.mdx` or a `plans.yml` holding `$7/month` was skipped by the walk,
 * reported by nothing, and left the gate exiting 0 — the ticket's own drift
 * class, relocated to a file type nobody had listed. So every extension
 * actually present under SCAN_ROOTS must appear in one of these two sets, and
 * an unclassified one FAILS. Adding a file type is then a decision somebody
 * makes, rather than a hole that opens silently.
 */
const IGNORED_EXTENSIONS = new Set([
  '.png',
  '.jpg',
  '.jpeg',
  '.gif',
  '.webp',
  '.avif',
  '.svg',
  '.ico',
  '.woff',
  '.woff2',
  '.ttf',
  '.otf',
  '.eot',
  '.mp4',
  '.webm',
  '.pdf',
  '.map',
  '.snap',
  '.lock',
])
/**
 * Directory names pruned from a SCAN_ROOTS walk, matched bare at any depth.
 *
 * A SCAN_ROOTS entry is a whole source tree, and source trees carry build
 * output inside them. Reading `node_modules` or a bundler's `dist` would make
 * the gate report a price no human wrote and no reader ever sees, in a file
 * regenerated on the next build — noise the gate cannot act on. `gen` holds
 * protobuf output for the same reason: generated wire types are not a
 * displayed-price surface, and a price appearing there would need fixing in
 * the `.proto` anyway.
 *
 * Unlike the ratchets around it this list is NOT self-checking, and the trade
 * is worth stating where the list is rather than several hundred lines away at
 * `walk`. An entry matching nothing costs nothing; the real cost is that a
 * hand-written directory which happens to be named `build` is pruned in
 * silence, and the only signal is a smaller count in the summary line. That is
 * tolerable only because these seven are build-tool conventions nobody picks by
 * accident for source. Adding a name here widens what the gate cannot see, so a
 * new entry owes the same argument.
 *
 * Pruning applies to SCAN_ROOTS only. A SCAN_DIRS entry is hand-registered on
 * the promise that every file in it is scanned, so it is walked with
 * `skipBuildDirs: false`. Both halves are pinned: see the tests "a build-named
 * subdirectory inside a SCAN_DIRS entry is still read" and "a build-named
 * subdirectory inside a SCAN_ROOTS tree is pruned".
 */
const SKIP_DIRS = new Set(['node_modules', 'dist', 'build', '.astro', '.vite', 'coverage', 'gen'])

/**
 * Files excused from the extension ratchet by NAME rather than by type.
 *
 * `.DS_Store` is binary OS metadata no human wrote, and it has no extension, so
 * without this it would red the gate on any macOS developer who opened a
 * scanned directory in Finder. Naming it is the point: the walk used to skip
 * every dot-prefixed entry as a class, which excused this file and a hand-
 * written `.hidden/case.json` alike. One named entry is reviewable; a class
 * skip is a hole.
 *
 * Unlike the extension lists, this one is not ratcheted: nothing fails when an
 * entry stops matching anything, so a stale name sits here silently. That is
 * the right trade for a list this short — a ratchet on it would red the gate on
 * a machine that simply has no `.DS_Store` — but it does mean the list is
 * maintained by review, not by the gate.
 */
const IGNORED_FILENAMES = new Set(['.DS_Store'])

/**
 * Price-shaped literals that are real, reviewed, and must NOT be rewritten to
 * the canonical amount. Every entry must still match something: a stale
 * exemption is itself a failure, so this list cannot quietly accumulate.
 */
// Intentionally empty today. The one entry it held was an illustrative `$42/mo`
// in a PricingCard.astro docstring; registering that file as a render surface
// made the example the only price literal on a surface that may hold none, so
// the docstring was reworded instead of exempted. The mechanism stays, tested
// on both paths (honoured, and ratcheted when stale), because the next reviewed
// literal should be recorded here rather than quietly tolerated.
export const EXEMPTIONS = []

const PRICE_LITERAL = /\$\d[\d,]*(?:\.\d+)?/g

/**
 * True when a `$<digits>` match is a regular-expression backreference in a
 * template literal, e.g. the `$1` of `` .replace(re, `$1${redacted}`) ``.
 *
 * This is a STRUCTURAL rule, not an allowlist of files, and it takes BOTH
 * halves: the match counts as a backreference only when it sits on a
 * `.replace(` line AND is spliced directly against an interpolation.
 *
 * Adjacency to `${` alone was the original rule and it was not enough. It
 * exempted a real price written next to any interpolation, so a render surface
 * containing `` `$7${'/month'}` `` scanned clean — and unlike a plain stale
 * literal that form is invisible at EVERY amount, so no later price change
 * would ever surface it. The `.replace(` call is what actually distinguishes a
 * substitution pattern from a displayed price; every backreference in this repo
 * is written inline in its own `.replace(` call.
 *
 * The `.replace(` must OPEN BEFORE the match, not merely share the line. A
 * trailing call — `` `$7${suffix}`.replace(/,/g, '') `` — is a formatting
 * one-liner around a displayed price, and a line-scoped test exempted it.
 * All four real redaction sites open the call first, so requiring the order
 * costs nothing and removes the only plausible way back into this exemption.
 *
 * Two further tempting rules are wrong and deliberately absent — "preceded by a
 * quote" would exempt the real defect `price="$7"`, and "followed by /mo" would
 * miss it, because that surface passes its suffix as a separate prop.
 */
export function isRegexBackreference(line, index, literal) {
  const call = line.indexOf('.replace(')
  if (call === -1 || call > index) return false
  return line.slice(index + literal.length, index + literal.length + 2) === '${'
}

/**
 * True when a markdown file's ``` fences do not pair up.
 *
 * stripFencedBlocks() below toggles on every fence line, so an ODD number of
 * them blanks everything from the stray fence to end of file: the gate stops
 * reading and still exits 0. A missing closing fence is a realistic edit in
 * prose non-engineers maintain, and a gate that silently goes quiet is the
 * always-green failure this ticket exists to prevent — so an unbalanced fence
 * is reported as a failure rather than absorbed.
 */
export function hasUnbalancedFence(text) {
  let open = false
  for (const line of text.split('\n')) {
    if (/^\s*```/.test(line)) open = !open
  }
  return open
}

/**
 * Blank out fenced code blocks while preserving line numbering. Only used for
 * markdown, and for the reason check-email-course.mjs documents: a positional
 * shell argument (`awk '{print $1}'`) in a snippet is not a price, and matching
 * it would red the gate with a message the author cannot act on. Inline
 * backticked spans are deliberately still scanned, so a prose `$7/mo` fails.
 */
export function stripFencedBlocks(text) {
  let fenced = false
  return text
    .split('\n')
    .map((line) => {
      if (/^\s*```/.test(line)) {
        fenced = !fenced
        return ''
      }
      return fenced ? '' : line
    })
    .join('\n')
}

/**
 * Words that introduce a billing period after an amount: `$7/month`,
 * `$7 per month`, `$7 a month`, `$7 billed monthly`, `$7 every month`.
 */
const UNIT_SEPARATOR_WORDS = String.raw`(?:\/|\s+(?:per|a|billed|every|each)\s+)`

/**
 * An optional per-unit qualifier sitting between the separator and the period.
 *
 * Real pricing copy interposes the thing being billed for — `$7/user/month`,
 * `$7 per machine per month`, `$7 per seat/month`. Requiring the period word
 * IMMEDIATELY after the separator made every one of those read as a period the
 * gate could not parse, so the fail-closed rule below reported them as drift.
 * That is the "over-eager gate gets switched off by the next person who hits
 * it" failure the plan's own risk list names, and it would have fired on
 * PRODUCT.md the first time `$7/mo per machine` was reworded.
 */
const UNIT_QUALIFIER = String.raw`(?:[A-Za-z]+\s*\/|per\s+[A-Za-z]+\s*(?:\/|\s+per\s+|\s+))?`

const unitPattern = (words) =>
  new RegExp(String.raw`^\s*(?:${UNIT_SEPARATOR_WORDS}|\s+)\s*${UNIT_QUALIFIER}\s*${words}\b`, 'i')

const MONTHLY_UNIT = unitPattern(String.raw`(?:months|month|monthly|mos|mo)`)
const ANNUAL_UNIT = unitPattern(String.raw`(?:years|year|yearly|annually|annual|yrs|yr)`)

/**
 * A suffix that plainly announces a billing period, whatever word follows.
 *
 * Deliberately narrower than the separators above, which also accept bare
 * whitespace: only a slash or an explicit period-introducing word counts here,
 * so a word simply following a price (`$7 today`) stays unread. Widening it to
 * any whitespace would turn ordinary prose into gate failures for no drift
 * caught.
 */
const UNIT_SEPARATOR = new RegExp(String.raw`^\s*${UNIT_SEPARATOR_WORDS}\s*[A-Za-z]`)

/**
 * The billing period a price literal is written against — `'month'`,
 * `'year'`, or `null` when the text does not say.
 *
 * An amount alone cannot say which period it is quoted against, so a price can
 * disagree with the declaration while its digits agree — `$7/year` beside a
 * monthly declaration of $7. Reading the suffix is what makes the agreement
 * check about the displayed price rather than about the digits.
 *
 * A spelling this function cannot read fails CLOSED, as `'unrecognised'`.
 * Returning `null` there would have been the same silent gap as an unlisted
 * file extension: `$7/mth` or `$7 per annum` skipped the period comparison
 * entirely, and nothing said so, so a reader could not tell a checked line
 * from an unchecked one. A new spelling must now be added on purpose.
 *
 * `null` remains for a suffix that asserts no period at all — a bare `$7`, or
 * `$7, cancel anytime`. That genuinely says nothing about a period, and there
 * is nothing to compare.
 *
 * The residue is stated rather than implied: a period introduced by something
 * other than a slash or one of the words above still reads as ABSENT, not as
 * unrecognised. `$7 p/m`, `$7 p.a.`, `$7 PCM` and `$7 (billed annually)` all
 * return `null` and go uncompared. Widening the separator to reach them means
 * reading arbitrary punctuation as a period announcement, which reds ordinary
 * prose; the trade is deliberate, and the way to close a case that matters is
 * to name its spelling above.
 */
export function billingUnitOf(suffix) {
  if (MONTHLY_UNIT.test(suffix)) return 'month'
  if (ANNUAL_UNIT.test(suffix)) return 'year'
  if (UNIT_SEPARATOR.test(suffix)) return 'unrecognised'
  return null
}

/** Every price-shaped literal in `text`, with 1-based line numbers. */
export function scanPriceLiterals(text, { markdown = false } = {}) {
  const found = []
  const source = markdown ? stripFencedBlocks(text) : text
  source.split('\n').forEach((line, i) => {
    for (const m of line.matchAll(PRICE_LITERAL)) {
      if (isRegexBackreference(line, m.index, m[0])) continue
      const suffix = line.slice(m.index + m[0].length)
      found.push({ line: i + 1, literal: m[0], text: line.trim(), unit: billingUnitOf(suffix) })
    }
  })
  return found
}

/** Parse bare `export const NAME = <integer>` declarations out of a module. */
export function readDeclarations(text) {
  const out = new Map()
  for (const m of text.matchAll(/^export const (\w+) = (\d+)$/gm)) {
    out.set(m[1], Number(m[2]))
  }
  return out
}

/**
 * Parse bare `export const NAME = '<string>'` declarations out of a module.
 *
 * A displayed price is an amount AND a period, and only the amount half was
 * ever compared across surfaces. The period was declared per surface and
 * checked by nothing: setting the marketing site's suffix to `/year` while the
 * web app kept `/month` left the two rendering different prices with the gate
 * exiting 0. The string carries no `$`, so no amount phase saw it, and a
 * declaration module is not a render surface, so the bare-period rule did not
 * either — this gate's own subject, in the half it did not cross-check.
 */
export function readStringDeclarations(text) {
  const out = new Map()
  for (const m of text.matchAll(/^export const (\w+) = '([^']*)'$/gm)) {
    out.set(m[1], m[2])
  }
  return out
}

/**
 * The set of amounts a declared price legitimises — just the monthly one.
 *
 * The discounted annual figure used to be admitted too, and it was the single
 * widest hole in this gate: nothing on either site displays an annual amount
 * (the pricing card shows a saving PERCENTAGE), so admitting it widened what
 * passed without widening what was covered. Worse, it is the likeliest wrong
 * number in the repo, being the one derived from the price — so the amount a
 * stray literal is most apt to state was the one the gate waved through.
 *
 * Admit an amount here only once something displays it.
 */
export function allowedAmounts(monthly) {
  return new Set([monthly])
}

/** Repo-relative, forward-slashed path — the form every failure message uses. */
function relativeTo(root, full) {
  return path.relative(root, full).split(path.sep).join('/')
}

function amountOf(literal) {
  return Number(literal.slice(1).replaceAll(',', ''))
}

/**
 * Every file below `dir`, unfiltered by extension.
 *
 * The extension filter deliberately lives at the call site instead: the
 * ratchet needs to see the file types it is NOT going to read, which a walk
 * that dropped them could not report.
 *
 * Dot-prefixed names are NOT skipped. Skipping them was one line of convenience
 * that bought a silent hole: `proof/recipes/.hidden/case.json` holding a
 * disagreeing price scanned clean, in a directory registered precisely so that
 * every file in it would be read. The OS droppings that skip existed for are
 * named in IGNORED_FILENAMES instead, where they are reviewable.
 *
 * `skipBuildDirs` exists because the two callers want opposite things. A
 * SCAN_ROOTS tree is a source tree with build output living inside it, so
 * `node_modules`/`dist`/`gen` must be pruned or the walk reads generated code.
 * A SCAN_DIRS entry names a small hand-curated directory OUTRIGHT, and pruning
 * there means a directory a human deliberately registered is silently only
 * partly read — `proof/recipes/build/case.json` would be skipped by a name
 * chosen for a build tree it is not.
 */
function walk(dir, acc, { skipBuildDirs = true } = {}) {
  let entries
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true })
  } catch (err) {
    // An absent directory is the CALLER's business: each of the two entry-point
    // callers ratchets that into a message of its own, naming the list that has
    // gone stale. (The recursive call below is a caller too, and does NOT — a
    // nested ENOENT there is a mid-walk race, since readdirSync listed the entry
    // moments earlier, and is deliberately tolerated.) Anything else — a
    // permission error, a path that turned out to be a file — means files that
    // should have been read were not, and swallowing it here would report full
    // coverage of a tree this function never opened. Fail closed, record WHICH
    // directory failed so the caller does not have to name the top of the walk,
    // and let the caller attribute it to a list.
    if (err.code !== 'ENOENT') {
      if (err.walkPath === undefined) err.walkPath = dir
      throw err
    }
    return acc
  }
  for (const entry of entries) {
    const full = path.join(dir, entry.name)
    if (entry.isDirectory()) {
      if (!skipBuildDirs || !SKIP_DIRS.has(entry.name)) walk(full, acc, { skipBuildDirs })
    } else {
      acc.push(full)
    }
  }
  return acc
}

/**
 * A billing period written without an amount beside it — `"/month"`,
 * `'per year'`, or the bare `/month` sitting in Astro markup.
 *
 * Declaring the suffix constant stops today's drift but cannot keep it stopped:
 * `PRICE_LITERAL` only matches text carrying a `$`, so a hand-typed `"/month"`
 * beside a derived amount is invisible to every other phase of this gate, and
 * the next contributor to retype it gets a green run. A displayed price is an
 * amount AND a period, so the period half needs a rule of its own.
 *
 * Anchored to the whole string on purpose: prose that merely contains "month"
 * is normal copy, and matching it would make the rule unusable.
 */
const PERIOD_WORD = String.raw`(?:months|month|monthly|mos|mo|years|year|yearly|annually|annual|yrs|yr)`

const BARE_BILLING_UNIT = new RegExp(
  [
    // A whole quoted string that is nothing but a period: `"/month"`, `'per year'`.
    String.raw`(["'\`])\s*\/?\s*(?:per\s+)?${PERIOD_WORD}\.?\s*\1`,
    // The same period sitting UNQUOTED in template markup. A component renders
    // `{price}/month` as text, not as a string literal, so the quoted form
    // above never saw it — and appending exactly that to PricingCard.astro
    // scanned clean while putting a hand-written period on screen. Only the
    // hand-registered render surfaces are read this way, so the risk of
    // catching an unrelated `/monthly-report` path is bounded to a short list
    // and fails loudly rather than silently.
    //
    // The slash may not itself be the second half of a `//` line-comment
    // marker. A render surface carries prose explaining WHY it derives the
    // period, and `// annual figure would advertise...` is that prose, not a
    // displayed period — a comment renders nothing. Without the lookbehind the
    // rule fired on the comment marker and the gate blocked its own
    // documentation. This narrows the slash, not the period words: a period
    // hand-written anywhere a single slash precedes it, comment or markup, is
    // still caught, and nothing is skipped silently.
    String.raw`(?<!\/)\/\s*${PERIOD_WORD}\b`,
    String.raw`\bper\s+${PERIOD_WORD}\b`,
  ].join('|'),
  'gi',
)

/** Every bare billing-period string in `text`, with 1-based line numbers. */
export function scanBillingUnitLiterals(text) {
  const found = []
  text.split('\n').forEach((line, i) => {
    for (const m of line.matchAll(BARE_BILLING_UNIT)) {
      found.push({ line: i + 1, literal: m[0], text: line.trim() })
    }
  })
  return found
}

/**
 * A prose surface must also be reached by a coverage list, or its presence rule
 * becomes a requirement to state a price with no rule about which price. Split
 * out so it can be tested against a list that is NOT covered — the branch is
 * unreachable through the module constants, which is the point of asserting it.
 *
 * Being NAMED by a list is not enough; the walk must actually read the file.
 * Each list is re-derived from EVERY predicate that list's own walk applies,
 * because a prefix test alone answers a different question:
 *
 * - SCAN_FILES names paths outright and bypasses both extension and pruning, so
 *   naming is sufficient there.
 * - SCAN_DIRS entries are walked with `skipBuildDirs: false` (each is
 *   hand-registered on the promise that every file in it is read), so prefix
 *   plus that entry's own extension list is the whole predicate.
 * - SCAN_ROOTS are walked with pruning ON, so prefix and extension are only two
 *   of three: a path with a `SKIP_DIRS` segment below the root — `dist/`,
 *   `build/`, `node_modules/` — is never opened at all. Checking prefix and
 *   extension alone reports such a file covered, which is exactly the hole this
 *   function exists to close, reopened one level down: required to state a
 *   price, and free to state the wrong one because nothing reads it.
 *
 * A prose surface whose extension a list does not read — or one sitting in
 * IGNORED_EXTENSIONS — fails the same way, for the same reason.
 */
export function proseSurfaceCoverageFailure(
  rel,
  {
    scanFiles = SCAN_FILES,
    scanDirs = SCAN_DIRS,
    scanRoots = SCAN_ROOTS,
    scanExtensions = SCAN_EXTENSIONS,
    skipDirs = SKIP_DIRS,
  } = {},
) {
  const ext = path.extname(rel)
  // Directory segments only: the final component is the file name, and a FILE
  // called `build` is read, not pruned — `walk` tests `SKIP_DIRS` only on
  // entries where `isDirectory()` holds.
  const prunedUnder = (root) =>
    rel
      .slice(root.length + 1)
      .split('/')
      .slice(0, -1)
      .some((segment) => skipDirs.has(segment))
  const covered =
    scanFiles.includes(rel) ||
    scanDirs.some((d) => rel.startsWith(`${d.path}/`) && d.extensions.includes(ext)) ||
    scanRoots.some((r) => rel.startsWith(`${r}/`) && scanExtensions.has(ext) && !prunedUnder(r))
  if (covered) return null
  return `${rel}: registered as a prose surface but read by no coverage list, so it is required to state a price and free to state the wrong one; add it to SCAN_FILES, or to SCAN_DIRS/SCAN_ROOTS with an extension those lists read and no pruned directory (${[...skipDirs].join(', ')}) in its path`
}

export function checkPriceParity({
  root = REPO_ROOT,
  exemptions = EXEMPTIONS,
  scanRoots = SCAN_ROOTS,
} = {}) {
  const failures = []
  const read = (rel) => {
    const full = path.join(root, rel)
    return fs.existsSync(full) ? fs.readFileSync(full, 'utf8') : null
  }

  // 1. Declarations exist, parse, and agree.
  const declared = new Map()
  // Populated by phase 2 and phase 3 alike, then ratcheted below: an exemption
  // matched by EITHER counts as live, or registering an exempted render surface
  // would report its own reviewed literal as a stale exemption.
  const usedExemptions = new Set()
  let monthly
  let declaredSuffix
  for (const decl of DECLARATIONS) {
    const text = read(decl.path)
    if (text === null) {
      failures.push(`${decl.path}: price declaration file is missing`)
      continue
    }
    const values = readDeclarations(text)
    const strings = readStringDeclarations(text)
    for (const name of decl.strings ?? []) {
      if (!strings.has(name)) {
        failures.push(`${decl.path}: does not declare \`export const ${name} = '<text>'\``)
        continue
      }
      declared.set(`${decl.path}:${name}`, strings.get(name))
      if (name === 'CLOUD_BILLING_PERIOD_SUFFIX') {
        const value = strings.get(name)
        // The declared amount is a MONTHLY one, so the declared period must
        // read as a month. A suffix the parser cannot classify fails rather
        // than passing unchecked, same as everywhere else in this gate.
        const unit = billingUnitOf(value)
        if (unit !== 'month') {
          failures.push(
            `${decl.path}: declares ${name} = '${value}', which reads as ${unit ?? 'no billing period at all'}; ` +
              'the declared amount is CLOUD_PRICE_USD_PER_MONTH, so the period must be a month',
          )
        }
        if (declaredSuffix === undefined) declaredSuffix = value
        else if (declaredSuffix !== value) {
          failures.push(
            `${decl.path}: declares ${name} = '${value}', but another surface declares '${declaredSuffix}'`,
          )
        }
      }
    }
    for (const name of decl.names) {
      if (!values.has(name)) {
        failures.push(`${decl.path}: does not declare \`export const ${name} = <integer>\``)
        continue
      }
      declared.set(`${decl.path}:${name}`, values.get(name))
      if (name === 'CLOUD_PRICE_USD_PER_MONTH') {
        if (monthly === undefined) monthly = values.get(name)
        else if (monthly !== values.get(name)) {
          failures.push(
            `${decl.path}: declares CLOUD_PRICE_USD_PER_MONTH = ${values.get(name)}, but another surface declares ${monthly}`,
          )
        }
      }
    }
  }
  if (monthly === undefined) {
    failures.push(
      'no surface declares CLOUD_PRICE_USD_PER_MONTH; cannot verify any displayed price',
    )
    return { ok: false, failures, declared, scanned: 0 }
  }
  const allowed = allowedAmounts(monthly)
  const periodOf = new Map([[monthly, 'month']])

  // 2. Render surfaces import the declaration and hold no literal.
  for (const surface of RENDER_SURFACES) {
    const text = read(surface.path)
    if (text === null) {
      failures.push(`${surface.path}: render surface is missing`)
      continue
    }
    if (
      surface.declaration !== null &&
      !new RegExp(`from ['"][^'"]*${surface.declaration}['"]`).test(text)
    ) {
      failures.push(
        `${surface.path}: renders a price but does not import from the '${surface.declaration}' declaration module`,
      )
    }
    // A render surface is held to the same reviewed-exemption list as every
    // other file. Without this the list was consulted only in phase 3, so
    // registering a surface that carries a reviewed literal was impossible and
    // the surface stayed unregistered — unchecked for the period too.
    const surfaceExempt = new Set(
      exemptions.filter((e) => e.path === surface.path).map((e) => e.literal),
    )
    for (const hit of scanPriceLiterals(text)) {
      if (surfaceExempt.has(hit.literal)) {
        usedExemptions.add(`${surface.path}:${hit.literal}`)
        continue
      }
      failures.push(
        `${surface.path}:${hit.line}: render surface must derive its price from the declaration module, not write the literal ${hit.literal} — ${hit.text}`,
      )
    }
    for (const hit of scanBillingUnitLiterals(text)) {
      failures.push(
        `${surface.path}:${hit.line}: render surface must derive the billing period from the declaration module, not write the literal ${hit.literal} — ${hit.text}`,
      )
    }
  }

  // 3. Every remaining literal states an amount the declarations legitimise.
  const files = []
  const rootFiles = []
  // The seam ratchet, and the reason injecting `scanRoots` cannot hide a defect.
  // Every other coverage list is a module constant a caller cannot touch; this
  // one is replaceable, and a replacement that DROPS a root scans less while
  // saying nothing — `scanRoots: []` would scan four files instead of 318 and
  // report agreement. Naming a declared root that is missing from the passed
  // list makes the seam additive-only, so a test can widen the corpus or mutate
  // an entry but can never quietly shrink it.
  for (const declared of SCAN_ROOTS) {
    if (scanRoots.includes(declared)) continue
    failures.push(
      `${declared}: declared in SCAN_ROOTS but missing from the scanned list, so that whole tree went unscanned; the scanRoots seam may add or mutate roots, never drop them`,
    )
  }
  for (const rel of scanRoots) {
    // The same ratchet SCAN_FILES and SCAN_DIRS carry, for the same reason. A
    // root that has been renamed or mistyped walks an absent directory, adds
    // nothing, and — without this — leaves the gate reporting success over a
    // tree it never read. A coverage list whose entries can quietly cover
    // nothing is the exact failure this gate exists to make impossible.
    const before = rootFiles.length
    try {
      walk(path.join(root, rel), rootFiles)
    } catch (err) {
      failures.push(
        `${rel}: scan root could not be read at ${relativeTo(root, err.walkPath || path.join(root, rel))} (${err.code || err.message}), so every price below it went unscanned`,
      )
      continue
    }
    if (rootFiles.length === before) {
      failures.push(
        `${rel}: scan root matched no files, so it covers nothing; restore it or remove it from SCAN_ROOTS`,
      )
    }
  }
  // Every file type present must be classified as scanned or ignored, so a new
  // one cannot slip past unread and unannounced. See IGNORED_EXTENSIONS.
  const unclassified = new Map()
  for (const full of rootFiles) {
    if (IGNORED_FILENAMES.has(path.basename(full))) continue
    const ext = path.extname(full)
    if (SCAN_EXTENSIONS.has(ext) || IGNORED_EXTENSIONS.has(ext)) continue
    if (!unclassified.has(ext)) unclassified.set(ext, relativeTo(root, full))
  }
  for (const [ext, example] of unclassified) {
    failures.push(
      `${example}: file type '${ext || '(no extension)'}' is neither scanned nor ignored, so a price written in it would go unread; add it to SCAN_EXTENSIONS or IGNORED_EXTENSIONS`,
    )
  }
  files.push(...rootFiles.filter((full) => SCAN_EXTENSIONS.has(path.extname(full))))
  for (const rel of SCAN_FILES) {
    const full = path.join(root, rel)
    // A named covered file that is absent is a coverage hole, not an empty
    // scan. Renaming PRODUCT.md would otherwise drop it from the gate forever
    // and stay green — the same silent-drop failure the exemption rules above
    // are written to avoid. DECLARATIONS and RENDER_SURFACES both announce a
    // missing file; this list announces it too.
    if (!fs.existsSync(full)) {
      failures.push(`${rel}: scanned file is missing; restore it or remove it from SCAN_FILES`)
      continue
    }
    files.push(full)
  }
  for (const dir of SCAN_DIRS) {
    const full = path.join(root, dir.path)
    if (!fs.existsSync(full)) {
      failures.push(
        `${dir.path}: scanned directory is missing; restore it or remove it from SCAN_DIRS`,
      )
      continue
    }
    // Recursive, matching the promise the doc comment makes. A flat read left
    // `proof/recipes/sub/nested.json` unscanned while the comment said every
    // matching file below the directory was covered — the hole this entry
    // exists to close, one directory level down. Build-directory names are NOT
    // pruned here, and neither are dot-prefixed names: this directory was
    // registered by hand, so every file in it is in scope (see `walk`).
    let found
    try {
      found = walk(full, [], { skipBuildDirs: false })
    } catch (err) {
      failures.push(
        `${dir.path}: scanned directory could not be read at ${relativeTo(root, err.walkPath || full)} (${err.code || err.message}), so every price below it went unscanned`,
      )
      continue
    }
    // Same ratchet the SCAN_ROOTS walk runs, for the same reason: a file type
    // that is neither listed by this entry nor globally ignored is UNREAD, and
    // an unread file is exactly what this gate exists to prevent. Adding
    // `recipe.yaml` beside the JSON must force a decision, not pass silently.
    const unread = new Map()
    for (const f of found) {
      if (IGNORED_FILENAMES.has(path.basename(f))) continue
      const ext = path.extname(f)
      if (dir.extensions.includes(ext) || IGNORED_EXTENSIONS.has(ext)) continue
      if (!unread.has(ext)) unread.set(ext, relativeTo(root, f))
    }
    for (const [ext, example] of unread) {
      failures.push(
        `${example}: file type '${ext || '(no extension)'}' is neither listed in the SCAN_DIRS entry for ${dir.path} nor globally ignored, so it is read by nothing; add it to the entry's extensions or to IGNORED_EXTENSIONS`,
      )
    }
    const matched = found.filter((f) => dir.extensions.includes(path.extname(f)))
    // An entry that matches nothing covers nothing, and would keep exiting 0
    // forever. Same reasoning as the missing-file branch above.
    if (matched.length === 0) {
      failures.push(
        `${dir.path}: scanned directory holds no ${dir.extensions.join('/')} file, so the entry covers nothing; restore them or remove it from SCAN_DIRS`,
      )
      continue
    }
    files.push(...matched)
  }
  for (const full of files.sort()) {
    const rel = relativeTo(root, full)
    if (RENDER_SURFACES.some((s) => s.path === rel)) continue
    const markdown = path.extname(full) === '.md'
    const text = fs.readFileSync(full, 'utf8')
    if (markdown && hasUnbalancedFence(text)) {
      failures.push(
        `${rel}: unbalanced \`\`\` code fence — every line after it is invisible to this gate, so a price stated below it would pass unread; close the fence`,
      )
    }
    for (const hit of scanPriceLiterals(text, { markdown })) {
      const exemption = exemptions.find((e) => e.path === rel && e.literal === hit.literal)
      if (exemption) {
        usedExemptions.add(`${exemption.path}:${exemption.literal}`)
        continue
      }
      const amount = amountOf(hit.literal)
      if (!allowed.has(amount)) {
        failures.push(
          `${rel}:${hit.line}: ${hit.literal} disagrees with the declared price (allowed: ${[...allowed].map((a) => `$${a}`).join(', ')}) — ${hit.text}`,
        )
        continue
      }
      // An allowed amount can still state the wrong price: `$7/year` beside a
      // monthly declaration of $7 agrees on digits and contradicts on period.
      // A bare `$7` asserts no period and is left alone.
      const period = periodOf.get(amount)
      if (hit.unit === 'unrecognised') {
        failures.push(
          `${rel}:${hit.line}: ${hit.literal} states a billing period this gate cannot read, so its agreement with the declaration is unchecked; use a spelling billingUnitOf() knows — ${hit.text}`,
        )
      } else if (hit.unit && period && hit.unit !== period) {
        failures.push(
          `${rel}:${hit.line}: ${hit.literal} is the declared ${period === 'month' ? 'MONTHLY' : 'ANNUAL'} amount but is written per ${hit.unit} — ${hit.text}`,
        )
      }
    }
  }

  // 3.5 Prose surfaces state the price, the declared number of times.
  for (const surface of PROSE_SURFACES) {
    const uncovered = proseSurfaceCoverageFailure(surface.path)
    if (uncovered) failures.push(uncovered)
    const text = read(surface.path)
    if (text === null) {
      failures.push(`${surface.path}: prose surface is missing`)
      continue
    }
    const markdown = path.extname(surface.path) === '.md'
    const stated = scanPriceLiterals(text, { markdown }).filter((hit) =>
      allowed.has(amountOf(hit.literal)),
    ).length
    if (stated < surface.minimum) {
      failures.push(
        `${surface.path}: states the declared price ${stated} time${stated === 1 ? '' : 's'}, expected at least ${surface.minimum}; a surface that stops stating the price agrees with every declaration, so nothing else here can see it go quiet`,
      )
    }
  }

  // 4. The exemption list ratchets: no entry may go stale.
  for (const e of exemptions) {
    if (!usedExemptions.has(`${e.path}:${e.literal}`)) {
      failures.push(`${e.path}: exemption for ${e.literal} no longer matches anything; delete it`)
    }
  }

  return { ok: failures.length === 0, failures, declared, scanned: files.length }
}

if (isMainModule(import.meta.url)) {
  // `--root <dir>` exists so the gate's own test can run this file as a real
  // subprocess against a diverged fixture and assert the EXIT CODE, not just the
  // returned failure list. Asserting the return value alone would still pass if
  // this block forgot to `process.exit(1)` — which is precisely the always-green
  // gate this ticket exists to prevent.
  //
  // A root is a COPY of this repository, so the real exemption list applies to
  // it unchanged — that is what lets a mutation probe run the true gate over a
  // copied tree and trust the verdict. `--no-exemptions` drops the list for the
  // synthetic fixture trees, which contain none of the reviewed literals and
  // would otherwise trip the stale-exemption ratchet. It only ever makes the
  // gate STRICTER, so it cannot hide a defect from the tests below.
  const rootFlag = process.argv.indexOf('--root')
  const root = rootFlag === -1 ? REPO_ROOT : process.argv[rootFlag + 1]
  // A missing value and a following FLAG are the same mistake: `--root
  // --no-exemptions` would otherwise scan a directory named `--no-exemptions`,
  // find nothing, and report the missing-file failures as if the tree were
  // broken. Say what is actually wrong instead.
  if (rootFlag !== -1 && (!root || root.startsWith('--'))) {
    process.stderr.write('--root requires a directory argument\n')
    process.exit(2)
  }
  const options = {}
  if (rootFlag !== -1) options.root = root
  if (process.argv.includes('--no-exemptions')) options.exemptions = []
  const result = checkPriceParity(options)
  if (!result.ok) {
    process.stderr.write(result.failures.join('\n') + '\n')
    process.exit(1)
  }
  process.stdout.write(
    `Verified displayed price agreement across ${result.declared.size} declarations and ${result.scanned} files\n`,
  )
}
