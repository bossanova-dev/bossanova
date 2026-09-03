import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { chmodSync, mkdirSync, mkdtempSync, readdirSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { after, test } from 'node:test'
import {
  allowedAmounts,
  billingUnitOf,
  checkPriceParity,
  DECLARATIONS,
  EXEMPTIONS,
  hasUnbalancedFence,
  isRegexBackreference,
  PROSE_SURFACES,
  proseSurfaceCoverageFailure,
  readDeclarations,
  readStringDeclarations,
  scanBillingUnitLiterals,
  SCAN_DIRS,
  SCAN_FILES,
  scanPriceLiterals,
  stripFencedBlocks,
} from './check-price-parity.mjs'

const WEB_DECL = 'services/web/src/pricing.ts'
const MKT_DECL = 'services/marketing/src/lib/pricing.ts'
const WEB_RENDER = 'services/web/src/pages/Subscribe.tsx'
const MKT_CARDS = 'services/marketing/src/components/pricing/PricingCards.astro'
const MKT_PAGE = 'services/marketing/src/pages/pricing.astro'
const MKT_CARD = 'services/marketing/src/components/pricing/PricingCard.astro'

/**
 * A minimal but complete and PASSING tree. Each test mutates exactly one thing
 * and asserts the gate goes red — so a green result anywhere below would mean
 * the gate is blind to that defect, not that the fixture is fine.
 */
// Every fixture tree built below, removed together once the file finishes.
// mkdtempSync alone leaks one directory per call and nothing reclaims them;
// they had accumulated into the thousands under TMPDIR before this existed.
const FIXTURE_ROOTS = []

after(() => {
  for (const root of FIXTURE_ROOTS) {
    try {
      rmSync(root, { recursive: true, force: true })
    } catch (err) {
      // Announce rather than swallow. A fixture that cannot be removed is worth
      // seeing, but cleanup is not a gate: failing the suite here would report
      // a tidiness problem as a price-parity failure.
      process.stderr.write(`fixture cleanup failed for ${root}: ${err.message}\n`)
    }
  }
})

function fixtureRoot(overrides = {}) {
  const root = mkdtempSync(join(tmpdir(), 'price-parity-'))
  FIXTURE_ROOTS.push(root)
  const files = {
    [WEB_DECL]:
      "export const CLOUD_PRICE_USD_PER_MONTH = 7\nexport const CLOUD_BILLING_PERIOD_SUFFIX = '/month'\n",
    [MKT_DECL]:
      "export const CLOUD_PRICE_USD_PER_MONTH = 7\nexport const CLOUD_ANNUAL_DISCOUNT_PERCENT = 20\nexport const CLOUD_BILLING_PERIOD_SUFFIX = '/month'\n",
    [WEB_RENDER]:
      "import { CLOUD_PRICE_PER_MONTH } from '~/pricing'\nexport const P = CLOUD_PRICE_PER_MONTH\n",
    [MKT_CARDS]:
      "import { CLOUD_PRICE } from '~/lib/pricing'\n<PricingCard price={CLOUD_PRICE} />\n",
    [MKT_PAGE]:
      "import { CLOUD_PRICE_PER_MONTH } from '~/lib/pricing'\nconst d = `${CLOUD_PRICE_PER_MONTH}`\n",
    // Registered with `declaration: null`: it takes the price as props, so it
    // imports nothing — but both literal rules still apply to it.
    [MKT_CARD]: 'const { price, priceSuffix } = Astro.props\n',
    // Two statements because PROSE_SURFACES requires two: the brief states the
    // price to the evaluating reader and again in the summary, and a fixture
    // carrying one would make every test below red for the wrong reason.
    'PRODUCT.md': 'Cloud mode ($7/mo) is the hosted tier.\nEvaluate $7/mo per machine.\n',
    // Every SCAN_FILES entry must exist, and every SCAN_DIRS entry must hold at
    // least one matching file: the gate reports either shortfall rather than
    // skipping it, so an incomplete fixture would red everywhere.
    'proof/recipes/default.json': '{"recipes":[{"description":"the $7/month terms"}]}\n',
    ...overrides,
  }
  for (const [rel, body] of Object.entries(files)) {
    if (body === null) continue
    const full = join(root, rel)
    mkdirSync(dirname(full), { recursive: true })
    writeFileSync(full, body)
  }
  return root
}

const check = (overrides) => checkPriceParity({ root: fixtureRoot(overrides), exemptions: [] })

/**
 * PROSE_SURFACES requires PRODUCT.md to state the declared price at least
 * twice, so a fixture probing some OTHER rule prepends the two baseline
 * statements. Without it a test of the amount rules goes red for the prose
 * count instead — passing, but for the wrong reason.
 */
const brief = (body) =>
  `Cloud mode ($7/mo) is the hosted tier.\nEvaluate $7/mo per machine.\n${body}`

test('the fixture tree passes, so every red result below is the mutation', () => {
  const result = check()
  assert.equal(result.ok, true, result.failures.join('\n'))
})

test('rejects two declarations that disagree', () => {
  const result = check({
    [MKT_DECL]:
      'export const CLOUD_PRICE_USD_PER_MONTH = 9\nexport const CLOUD_ANNUAL_DISCOUNT_PERCENT = 20\n',
  })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /declares CLOUD_PRICE_USD_PER_MONTH = 9, but another surface declares 7/,
  )
})

test('rejects a render surface that writes the price as a literal', () => {
  const result = check({
    [WEB_RENDER]:
      "import { CLOUD_PRICE_PER_MONTH } from '~/pricing'\n<p>$7/month, cancel anytime</p>\n",
  })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /must derive its price from the declaration module, not write the literal \$7/,
  )
})

test('rejects a render surface that no longer imports the declaration', () => {
  const result = check({ [MKT_CARDS]: '<PricingCard price={somethingElse} />\n' })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /does not import from the 'lib\/pricing' declaration module/,
  )
})

test('rejects prose stating an amount the declarations do not support', () => {
  const result = check({ 'PRODUCT.md': 'Cloud mode ($9/mo) is the hosted tier.\n' })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /PRODUCT\.md:1: \$9 disagrees with the declared price/)
})

test('rejects a comment that drifts even where the code is correct', () => {
  const result = check({
    [MKT_DECL]:
      '// Cloud is $9/month.\nexport const CLOUD_PRICE_USD_PER_MONTH = 7\nexport const CLOUD_ANNUAL_DISCOUNT_PERCENT = 20\n',
  })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /\$9 disagrees with the declared price/)
})

test('rejects a missing declaration file', () => {
  const result = check({ [WEB_DECL]: null })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /price declaration file is missing/)
})

test('rejects a declaration the textual parser cannot read', () => {
  const result = check({ [WEB_DECL]: 'export const CLOUD_PRICE_USD_PER_MONTH = getPrice()\n' })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /does not declare `export const CLOUD_PRICE_USD_PER_MONTH = <integer>`/,
  )
})

test('only the displayed monthly amount is admitted', () => {
  assert.deepEqual([...allowedAmounts(7)], [7])
  assert.equal(check({ 'PRODUCT.md': brief('Monthly is $7.\n') }).ok, true)
  // $67 is the amount the 20% annual discount derives, and admitting it was the
  // widest hole in this gate: nothing displays an annual amount, so it widened
  // what passed without widening what was covered — and it is the likeliest
  // wrong number in the repo, being the one derived from the price.
  assert.equal(check({ 'PRODUCT.md': brief('Annual is $67.\n') }).ok, false)
  assert.equal(check({ 'PRODUCT.md': brief('Annual is $68.\n') }).ok, false)
})

test('the monthly amount written per year is caught', () => {
  const perYear = check({ 'PRODUCT.md': 'Cloud mode is $7 per year.\n' })
  assert.equal(perYear.ok, false)
  assert.match(
    perYear.failures.join('\n'),
    /PRODUCT\.md:1: \$7 is the declared MONTHLY amount but is written per year/,
  )
  // A correctly-paired amount and a bare amount both stay green: the rule
  // judges an explicit contradiction, never the absence of a unit.
  assert.equal(check({ 'PRODUCT.md': brief('Monthly is $7/mo, or $7 a month.\n') }).ok, true)
  assert.equal(check({ 'PRODUCT.md': brief('It costs $7 up front.\n') }).ok, true)
})

test('a billing period the gate cannot read fails rather than passing unchecked', () => {
  // `billingUnitOf` used to return null for BOTH "no period stated" and "a
  // period stated in words I do not know", so `$7/fortnight` — an amount
  // displayed against a period contradicting the declaration — was waved
  // through by the same branch that correctly ignores a bare `$7`.
  assert.equal(billingUnitOf('/fortnight'), 'unrecognised')
  assert.equal(billingUnitOf(' per quarter'), 'unrecognised')
  assert.equal(billingUnitOf(' up front'), null)
  const result = check({ 'PRODUCT.md': 'Cloud mode is $7/fortnight.\n' })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /PRODUCT\.md:1: \$7 states a billing period this gate cannot read/,
  )
})

test('billingUnitOf reads the suffix, and only a real unit', () => {
  assert.equal(billingUnitOf('/mo) is the hosted tier'), 'month')
  assert.equal(billingUnitOf('/month, cancel anytime'), 'month')
  assert.equal(billingUnitOf(' per month'), 'month')
  assert.equal(billingUnitOf(' a year'), 'year')
  assert.equal(billingUnitOf('/yr'), 'year')
  assert.equal(billingUnitOf(' up front'), null)
  assert.equal(billingUnitOf(''), null)
  // Not a unit: `monthly` must be a whole word, and a later unit on the line
  // belongs to a later literal, not to this one.
  assert.equal(billingUnitOf('nth'), null)
  assert.equal(billingUnitOf(' and $67/yr'), null)
})

test('a stale exemption fails rather than lingering', () => {
  const result = checkPriceParity({
    root: fixtureRoot(),
    exemptions: [{ path: 'services/web/src/gone.ts', literal: '$42', reason: 'no longer present' }],
  })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /exemption for \$42 no longer matches anything; delete it/,
  )
})

test('regex backreferences in template literals are not prices', () => {
  const redaction = '  .replace(reAuthHeader, `$1${redacted}`)'
  assert.equal(isRegexBackreference(redaction, redaction.indexOf('$1'), '$1'), true)
  const literal = '        price="$7"'
  assert.equal(isRegexBackreference(literal, literal.indexOf('$7'), '$7'), false)
  assert.deepEqual(scanPriceLiterals(redaction), [])
  assert.equal(
    check({
      'services/web/src/sentry-init.ts': `${redaction}\n  .replace(reBearer, \`$2\${redacted}\`)\n`,
    }).ok,
    true,
  )
})

test('a price spliced against an interpolation is not mistaken for a backreference', () => {
  // The original rule was "followed by `${`" alone, and it let this through: the
  // render surface still imports the declaration, so the import check passes, and
  // the literal was skipped as if it were a `.replace()` backreference. Worse than
  // an ordinary stale literal — this form reads clean at EVERY amount, so no later
  // price change would ever surface it.
  const smuggled = "export const P = `$9${'/month'}`"
  assert.equal(isRegexBackreference(smuggled, smuggled.indexOf('$9'), '$9'), false)
  const result = check({
    [WEB_RENDER]: `import { CLOUD_PRICE_PER_MONTH } from '~/pricing'\n${smuggled}\n`,
  })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /must derive its price from the declaration module, not write the literal \$9/,
  )
})

test('a .replace() that trails the price does not exempt it', () => {
  // Sharing a line is not enough: a formatting call written AFTER the literal
  // is a displayed price being tidied, not a substitution pattern. All four
  // real redaction sites open `.replace(` before their backreference.
  const trailing = "export const label = `$7${suffix}`.replace(/,/g, '')"
  assert.equal(isRegexBackreference(trailing, trailing.indexOf('$7'), '$7'), false)
  assert.deepEqual(
    scanPriceLiterals(trailing).map((h) => h.literal),
    ['$7'],
  )
})

test('the backreference rule does not exempt a quoted or suffixless price', () => {
  // Two rules that would have looked plausible and are deliberately absent:
  // "preceded by a quote" would exempt price="$7", and "followed by /mo"
  // would miss it, because that surface passes its suffix as a separate prop.
  assert.deepEqual(
    scanPriceLiterals('        price="$7"').map((h) => h.literal),
    ['$7'],
  )
})

test('readDeclarations reads only bare integer exports', () => {
  const values = readDeclarations(
    'export const A = 7\nexport const B = 20\nexport const C = `${A}`\nconst D = 9\n',
  )
  assert.deepEqual(
    [...values],
    [
      ['A', 7],
      ['B', 20],
    ],
  )
})

test('a shell snippet in markdown is not read as a price, but prose still is', () => {
  const md = "Run:\n\n```sh\nawk '{print $1}' log\n```\n\nCloud mode ($9/mo).\n"
  assert.equal(stripFencedBlocks(md).includes('$1}'), false)
  assert.equal(stripFencedBlocks(md).split('\n').length, md.split('\n').length)
  assert.deepEqual(
    scanPriceLiterals(md, { markdown: true }).map((h) => h.literal),
    ['$9'],
  )
  const result = check({ 'PRODUCT.md': md })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /PRODUCT\.md:7: \$9 disagrees/)
})

test('an unbalanced code fence fails loudly instead of silencing the rest of the file', () => {
  // stripFencedBlocks toggles on every fence line, so an odd count blanks everything
  // from the stray fence to EOF. Left unreported that is a gate which quietly stops
  // reading and still exits 0 — the always-green failure this ticket exists to end.
  const md = 'Cloud is $7/mo before.\n\n```sh\necho hi\n\nCloud is $9/mo after.\n'
  assert.equal(hasUnbalancedFence(md), true)
  assert.equal(hasUnbalancedFence('Fine.\n\n```sh\necho hi\n```\n\nCloud is $7/mo.\n'), false)
  // Proof the blinding is real: the scan alone cannot see the $9 below the fence.
  assert.deepEqual(
    scanPriceLiterals(md, { markdown: true }).map((h) => h.literal),
    ['$7'],
  )
  const result = check({ 'PRODUCT.md': md })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /PRODUCT\.md: unbalanced .* code fence/)
})

test('the real repository tree agrees with itself', () => {
  const result = checkPriceParity()
  assert.equal(result.ok, true, result.failures.join('\n'))
})

test('a proof recipe description that names a stale amount is caught', () => {
  // Recipe descriptions are prose a human reads, far from the declaration, and
  // nothing else in the repo would catch one going stale. SCAN_FILES covers the
  // file; this proves the coverage is real rather than a constant nobody reads.
  const result = check({
    'proof/recipes/default.json':
      '{"recipes":[{"description":"the $9/month cancellation terms"}]}\n',
  })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /proof\/recipes\/default\.json.*\$9/)
})

test('a SCAN_FILES entry that has gone missing is reported, not skipped', () => {
  // Absent this check the tree below is CLEAN: the declarations and render
  // surfaces are all intact, so a renamed PRODUCT.md would drop out of the
  // gate's coverage permanently while the gate kept exiting 0.
  const result = check({ 'PRODUCT.md': null })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /PRODUCT\.md: scanned file is missing; restore it or remove it from SCAN_FILES/,
  )
})

test('a NEW sibling recipe naming a stale amount is caught, not just default.json', () => {
  // The coverage is the DIRECTORY, not the one recipe that exists today. A
  // hand-maintained single path would scan this file's neighbour and never look
  // at the file itself — the drift shape reopening with the next recipe added.
  const result = check({
    'proof/recipes/nightly.json':
      '{"recipes":[{"description":"the $9/month cancellation terms"}]}\n',
  })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /proof\/recipes\/nightly\.json.*\$9/)
})

test('a SCAN_DIRS directory that has gone missing is reported, not skipped', () => {
  // Deleting the only recipe removes the directory too. Absent this check the
  // tree is CLEAN, so the recipes would drop out of coverage permanently while
  // the gate kept exiting 0 — the SCAN_FILES ratchet, applied to a directory.
  const result = check({ 'proof/recipes/default.json': null })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /proof\/recipes: scanned directory is missing; restore it or remove it from SCAN_DIRS/,
  )
})

test('a SCAN_DIRS directory holding no matching file is reported, not an empty scan', () => {
  const result = check({
    'proof/recipes/default.json': null,
    'proof/recipes/notes.txt': 'Recipes live here.\n',
  })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /proof\/recipes: scanned directory holds no \.json\/\.md file/,
  )
})

test('a SCAN_DIRS directory is read recursively, not just its top level', () => {
  // A flat read left `proof/recipes/sub/nested.json` unscanned while the doc
  // comment promised every matching file below the directory was covered.
  const result = check({ 'proof/recipes/sub/nested.json': '{"description":"$68/month"}\n' })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /proof\/recipes\/sub\/nested\.json:1: \$68/)
})

test('markdown beside the recipes is scanned too, not only the recipes', () => {
  // proof/recipes/README.md is 14 KB of prose a human reads. Scanning the
  // directory while skipping the one file in it written purely for humans
  // would have been a strange place to stop.
  const result = check({ 'proof/recipes/README.md': 'The trial costs $68/month.\n' })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /proof\/recipes\/README\.md:1: \$68/)
})

test('a file type neither scanned nor ignored fails, rather than being skipped', () => {
  // SCAN_EXTENSIONS is an allowlist, so it could never announce the file type
  // it was missing: a `pricing.yml` holding the price was skipped by the walk,
  // reported by nothing, and left the gate exiting 0 — this ticket's own drift
  // class, relocated to a file type nobody had listed.
  const result = check({ 'services/web/src/plans.yml': 'cloud: $7/month\n' })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /services\/web\/src\/plans\.yml: file type '\.yml' is neither scanned nor ignored/,
  )
  // Classifying it either way clears the failure; which way is a decision
  // somebody makes, rather than a hole that opens silently.
  assert.equal(check({ 'services/web/src/logo.svg': '<svg/>\n' }).ok, true)
})

test('a BUILD file is scanned like any other hand-written text', () => {
  // `.bazel` was absent from the allowlist while four BUILD.bazel files sat
  // under the scan roots, so they were read by nothing.
  const result = check({ 'services/web/src/BUILD.bazel': '# costs $68/month\n' })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /services\/web\/src\/BUILD\.bazel:1: \$68/)
})

test('a render surface may not hand-write the billing period either', () => {
  // PRICE_LITERAL only matches text carrying a `$`, so a hand-typed "/month"
  // beside a correctly-derived amount was invisible to every other phase.
  const result = check({
    [MKT_CARDS]:
      'import { CLOUD_PRICE } from \'~/lib/pricing\'\n<PricingCard price={CLOUD_PRICE} priceSuffix="/month" />\n',
  })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /PricingCards\.astro:2: render surface must derive the billing period from the declaration module/,
  )
})

test('scanBillingUnitLiterals matches a whole quoted period, never prose', () => {
  const found = (text) => scanBillingUnitLiterals(text).map((h) => h.literal)
  assert.deepEqual(found('a = "/month"'), ['"/month"'])
  assert.deepEqual(found("a = 'per year'"), ["'per year'"])
  assert.deepEqual(found('a = `monthly`'), ['`monthly`'])
  assert.deepEqual(found('a = "/mo."'), ['"/mo."'])
  // Anchored to the whole string on purpose: prose that merely contains the
  // word is normal copy, and matching it would make the rule unusable.
  assert.deepEqual(found('a = "billed every month, cancel anytime"'), [])
  assert.deepEqual(found('a = "monthly report"'), [])
  assert.deepEqual(found('const monthly = 1'), [])
  assert.deepEqual(
    scanBillingUnitLiterals('x\ny = "/year"').map((h) => h.line),
    [2],
  )
})

test('scanBillingUnitLiterals also reads an unquoted period in template markup', () => {
  const found = (text) => scanBillingUnitLiterals(text).map((h) => h.literal)
  // A component renders the period as TEXT, never as a string literal. The
  // quoted rule above could not see this, so appending it to a registered
  // render surface put a hand-written period on screen and scanned clean.
  assert.deepEqual(found('<span>{price}/month</span>'), ['/month'])
  assert.deepEqual(found('<p>$7 per year</p>'), ['per year'])
  assert.deepEqual(found('<p>{price} / mo</p>'), ['/ mo'])
  // Still silent on prose that only mentions the word, and on an import path.
  assert.deepEqual(found("import x from '~/lib/pricing'"), [])
  assert.deepEqual(found('<p>billed every month, cancel anytime</p>'), [])
  assert.deepEqual(found('const monthlyTotal = 1'), [])
})

test('a `//` comment marker is not a billing-period separator', () => {
  const found = (text) => scanBillingUnitLiterals(text).map((h) => h.literal)
  // A render surface carries prose explaining why it derives the period, and
  // the second slash of the comment marker used to read as the period's
  // separator -- so the rule fired on the comment and the gate blocked its own
  // documentation. A comment renders nothing, so there is no displayed period
  // here to catch. Regression for the real line that tripped it, added to
  // PricingCards.astro by the $7 -> $9 change.
  assert.deepEqual(found('// annual figure would advertise a yearly amount'), [])
  assert.deepEqual(found('// monthly price moves'), [])
  assert.deepEqual(found('<a href="https://monthly-report.example">x</a>'), [])
  // The narrowing is to the SLASH, not to the period words: one slash before a
  // period is still caught, comment or not, and so is every other alternative.
  assert.deepEqual(found('// bills them /month regardless'), ['/month'])
  assert.deepEqual(found('// bills them per month regardless'), ['per month'])
  assert.deepEqual(found('<span>{price}/month</span>'), ['/month'])
})

test('coverage names PRODUCT.md by path and the proof recipes by directory', () => {
  assert.ok(SCAN_FILES.includes('PRODUCT.md'))
  assert.ok(
    SCAN_DIRS.some(
      (d) =>
        d.path === 'proof/recipes' &&
        d.extensions.includes('.json') &&
        d.extensions.includes('.md'),
    ),
  )
})

// SCAN_ROOTS is a coverage list like SCAN_FILES and SCAN_DIRS, and like them a
// typo'd or renamed entry must be loud. These inject the mutated list the way a
// bad edit to the constant would produce it: everything else in the tree stays
// valid, so the only thing that can red the gate is the root itself.
//
// A note for anyone adding to them: because the additive-only ratchet fires on
// any declared name absent from the passed list, MUTATING a root now reds for
// two reasons at once — the mutated entry's own failure and the dropped
// original's. Assert on the specific message you mean, never on `ok === false`
// alone, or the test will pass for the other reason once its subject regresses.

test('a scan root that matches nothing fails rather than silently covering nothing', () => {
  const result = checkPriceParity({
    root: fixtureRoot(),
    exemptions: [],
    scanRoots: ['services/web/src', 'services/marketing/srcc'],
  })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /services\/marketing\/srcc: scan root matched no files/)
  // The typo also DROPS `services/marketing/src` from the list, and that is a
  // separate failure from the typo'd entry matching nothing. Asserting it here
  // is what distinguishes the additive-only ratchet from the weaker "list is
  // non-empty" floor: under that floor this list is non-empty, so the ratchet
  // never fires and only the zero-match message survives.
  assert.match(
    result.failures.join('\n'),
    /services\/marketing\/src: declared in SCAN_ROOTS but missing from the scanned list/,
  )
})

test('a scan root that is not a directory fails loudly instead of scanning nothing', () => {
  // ENOTDIR, not ENOENT: the branch that must not swallow the error. A bare
  // `catch { return acc }` reads this as an empty tree and reports coverage.
  const result = checkPriceParity({
    root: fixtureRoot(),
    exemptions: [],
    scanRoots: ['services/web/src', 'PRODUCT.md'],
  })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /PRODUCT\.md: scan root could not be read/)
})

test('an injected scan list that drops a declared root fails instead of scanning less', () => {
  // The seam itself, not an entry in it. `scanRoots` is the only coverage list a
  // caller can replace, and a replacement that simply omits a root scans a
  // fraction of the corpus and reports agreement over it — the empty list is the
  // extreme: four files instead of the real tree, zero failures. Every OTHER
  // assertion in this file would still pass with that hole open, which is why
  // this one exists: it pins the seam's additive-only property directly.
  const result = checkPriceParity({ root: fixtureRoot(), exemptions: [], scanRoots: [] })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /services\/web\/src: declared in SCAN_ROOTS but missing from the scanned list/,
  )
  assert.match(
    result.failures.join('\n'),
    /services\/marketing\/src: declared in SCAN_ROOTS but missing from the scanned list/,
  )
})

test('an injected scan list that drops ONE declared root fails, not just an empty one', () => {
  // The empty list above is the extreme; this is the shape a real edit produces.
  // Pointing a fixture test at the single root it populates is the NATURAL way
  // to write one, and it silently drops the other tree — the whole marketing
  // corpus — while every other assertion in this file still passes. A floor that
  // only rejects the empty list admits this case, which is why the ratchet is
  // per-declared-name rather than a length check.
  const result = checkPriceParity({
    root: fixtureRoot(),
    exemptions: [],
    scanRoots: ['services/web/src'],
  })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /services\/marketing\/src: declared in SCAN_ROOTS but missing from the scanned list/,
  )
})

test('a SCAN_DIRS entry that is not a directory fails loudly instead of scanning nothing', () => {
  // The scanRoots walk and the SCAN_DIRS walk are two separate call sites of the
  // same rethrow, and only the first was pinned. `existsSync` is true for a
  // regular file, so this reaches the walk and must surface as ENOTDIR rather
  // than as an empty curated directory.
  const result = check({
    'proof/recipes/default.json': null,
    'proof/recipes': 'a file where the curated directory should be\n',
  })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /proof\/recipes: scanned directory could not be read at proof\/recipes \(ENOTDIR\)/,
  )
})

test('a nested unreadable directory is named, not the top of the walk', (t) => {
  // The failure above happens AT the registered entry, where "which directory
  // failed" and "which entry was registered" are the same string — so it cannot
  // tell whether the walk records the failing path or the caller just re-names
  // its own entry. This one can: the unreadable directory is two levels below
  // the scan root, so only a walk that carries the path out with the error can
  // put `services/web/src/locked` in the message instead of `services/web/src`.
  // An operator sent to a directory that reads fine has been told nothing.
  const root = fixtureRoot()
  const locked = join(root, 'services/web/src/locked')
  mkdirSync(locked, { recursive: true })
  chmodSync(locked, 0o000)
  t.after(() => chmodSync(locked, 0o755))
  // Root reads a 0o000 directory anyway, so the branch is unobservable there.
  // The repo's Go permission tests skip on the same condition. But a skip is
  // exactly the fail-open this whole gate exists to forbid, and `node --test`
  // exits 0 on one: scripts/Makefile runs the suite without a `# skipped 0`
  // check, so under root in CI the branch's only err.walkPath test would
  // evaporate behind a green build. Locally a skip is the honest ergonomic
  // answer; in CI, running as root is a broken runner, so say so out loud.
  if (typeof process.getuid === 'function' && process.getuid() === 0) {
    if (process.env.CI) {
      throw new Error('running as root in CI: this test cannot be skipped here')
    }
    t.skip('requires a non-root process: root can read a 0o000 directory')
    return
  }
  assert.throws(() => readdirSync(locked), /EACCES/)
  const result = checkPriceParity({ root, exemptions: [] })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /services\/web\/src: scan root could not be read at services\/web\/src\/locked \(EACCES\)/,
  )
})

// The acceptance criterion is that the gate EXITS non-zero on a divergence.
// Every test above asserts `checkPriceParity()`'s return value, which is a
// strictly weaker claim: a `main()` block that computed the failures and then
// forgot to `process.exit(1)` would satisfy all of them while shipping an
// always-green gate. These two run the file as a real subprocess and read the
// status the CI shell would read.

const GATE = fileURLToPath(new URL('./check-price-parity.mjs', import.meta.url))

const runGate = (root) =>
  spawnSync(process.execPath, [GATE, '--root', root, '--no-exemptions'], { encoding: 'utf8' })

test('the gate process exits non-zero on a diverged tree', () => {
  const root = fixtureRoot({
    [MKT_DECL]:
      'export const CLOUD_PRICE_USD_PER_MONTH = 9\nexport const CLOUD_ANNUAL_DISCOUNT_PERCENT = 20\n',
  })
  const result = runGate(root)
  assert.equal(result.status, 1, `expected exit 1, got ${result.status}: ${result.stderr}`)
  assert.match(result.stderr, /disagrees|another surface declares/)
})

test('the gate process exits zero on a tree that agrees', () => {
  const result = runGate(fixtureRoot())
  assert.equal(result.status, 0, `expected exit 0, got ${result.status}: ${result.stderr}`)
  assert.match(result.stdout, /Verified displayed price agreement/)
})

// Every subprocess assertion above passes `--no-exemptions`, so the branch that
// loads the REAL list had no coverage at all: dropping it from the CLI, or
// misreading the flag, would not have failed a single test. This at least
// executes that branch.
//
// Honest about its strength: while EXEMPTIONS is empty the two paths are
// identical by construction, so today this cannot tell them apart — hardwiring
// `exemptions: []` here would still pass. It discriminates only once an
// exemption is recorded, which is exactly when the branch starts to matter.
test('omitting --no-exemptions runs the gate against the real exemption list', () => {
  const root = fixtureRoot()
  const expected = checkPriceParity({ root, exemptions: EXEMPTIONS })
  const result = spawnSync(process.execPath, [GATE, '--root', root], { encoding: 'utf8' })
  assert.equal(
    result.status === 0,
    expected.ok,
    `subprocess status ${result.status} contradicts in-process ok=${expected.ok}: ${result.stderr}`,
  )
})

// `--root --no-exemptions` used to scan a directory named `--no-exemptions`,
// report every scanned file as missing, and exit 1 — a flag-order slip
// presenting itself as a broken tree.
test('--root rejects a following flag instead of scanning it as a directory', () => {
  const result = spawnSync(process.execPath, [GATE, '--root', '--no-exemptions'], {
    encoding: 'utf8',
  })
  assert.equal(result.status, 2, `expected exit 2, got ${result.status}: ${result.stderr}`)
  assert.match(result.stderr, /--root requires a directory argument/)
})

// --- The billing PERIOD, cross-checked the way the amount always was ---------
//
// Every test in this block reds only because the two declaration modules can
// disagree about the period while agreeing about the amount. That was live:
// setting the marketing site's suffix to '/year' and leaving the web app's at
// '/month' left the two rendering different prices and the gate exiting 0.

// The cross-surface period check only runs over names a DECLARATIONS entry
// lists in `strings`. Dropping the key from an entry disables that check for
// that surface silently: '/month' against '/mo' then exits 0, because no rule
// is left to compare them. Nothing else in this file would notice.
test('every declaration entry submits its billing period to the cross-check', () => {
  for (const decl of DECLARATIONS) {
    assert.ok(
      decl.strings?.includes('CLOUD_BILLING_PERIOD_SUFFIX'),
      `${decl.path}: must list CLOUD_BILLING_PERIOD_SUFFIX in strings, or its declared period is compared against nothing`,
    )
  }
})

test('readStringDeclarations reads bare string exports, and only those', () => {
  const values = readStringDeclarations(
    [
      "export const CLOUD_BILLING_PERIOD_SUFFIX = '/month'",
      "export const NOT_BARE = withCall('/month')",
      "const UNEXPORTED = '/week'",
      "export const MULTILINE = '/month' + suffix",
    ].join('\n'),
  )
  assert.deepEqual([...values], [['CLOUD_BILLING_PERIOD_SUFFIX', '/month']])
})

test('rejects two declarations whose billing period disagrees', () => {
  const result = check({
    [MKT_DECL]:
      "export const CLOUD_PRICE_USD_PER_MONTH = 7\nexport const CLOUD_ANNUAL_DISCOUNT_PERCENT = 20\nexport const CLOUD_BILLING_PERIOD_SUFFIX = '/year'\n",
  })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /declares CLOUD_BILLING_PERIOD_SUFFIX = '\/year', but another surface declares '\/month'/,
  )
})

test('rejects a declared period that contradicts the monthly amount', () => {
  // Both surfaces agree — on the wrong period. Agreement alone is not enough:
  // the declared amount is a MONTHLY one, so a suffix reading as a year is a
  // displayed price no declaration authorises, however consistent.
  const result = check({
    [WEB_DECL]:
      "export const CLOUD_PRICE_USD_PER_MONTH = 7\nexport const CLOUD_BILLING_PERIOD_SUFFIX = '/year'\n",
    [MKT_DECL]:
      "export const CLOUD_PRICE_USD_PER_MONTH = 7\nexport const CLOUD_ANNUAL_DISCOUNT_PERCENT = 20\nexport const CLOUD_BILLING_PERIOD_SUFFIX = '/year'\n",
  })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /which reads as year; the declared amount is/)
})

test('rejects a declared period the parser cannot classify', () => {
  const result = check({
    [WEB_DECL]:
      "export const CLOUD_PRICE_USD_PER_MONTH = 7\nexport const CLOUD_BILLING_PERIOD_SUFFIX = '/fortnight'\n",
    [MKT_DECL]:
      "export const CLOUD_PRICE_USD_PER_MONTH = 7\nexport const CLOUD_ANNUAL_DISCOUNT_PERCENT = 20\nexport const CLOUD_BILLING_PERIOD_SUFFIX = '/fortnight'\n",
  })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /reads as unrecognised/)
})

test('rejects a declaration module that stops declaring the period at all', () => {
  const result = check({ [WEB_DECL]: 'export const CLOUD_PRICE_USD_PER_MONTH = 7\n' })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /does not declare `export const CLOUD_BILLING_PERIOD_SUFFIX = '<text>'`/,
  )
})

// --- A render surface that receives the price rather than importing it -------

test('a render surface with declaration null needs no import but keeps both literal rules', () => {
  const result = check({ [MKT_CARD]: 'const s = "$7/month"\n' })
  assert.equal(result.ok, false)
  const text = result.failures.join('\n')
  assert.match(text, /PricingCard\.astro:1: render surface must derive its price/)
  assert.doesNotMatch(text, /PricingCard\.astro: renders a price but does not import/)
})

test('a render surface may carry a reviewed exemption, and it is still ratcheted', () => {
  const exemption = {
    path: MKT_CARD,
    literal: '$42',
    reason: 'illustrative team total, not the Cloud price',
  }
  const live = checkPriceParity({
    root: fixtureRoot({ [MKT_CARD]: '/** e.g. "$42 for a 6-person team" */\n' }),
    exemptions: [exemption],
  })
  assert.equal(live.ok, true, live.failures.join('\n'))

  // The same exemption over a tree that no longer contains it must fail: a
  // stale exemption is an unreviewed hole waiting for a literal to reoccupy it.
  const stale = checkPriceParity({ root: fixtureRoot(), exemptions: [exemption] })
  assert.equal(stale.ok, false)
  assert.match(stale.failures.join('\n'), /exemption for \$42 no longer matches anything/)
})

// --- Billing periods as pricing copy actually writes them --------------------

test('billingUnitOf reads a per-unit qualifier between the amount and the period', () => {
  // Every one of these read as `unrecognised` before, so ordinary copy like
  // "$7/user/month" reported as drift. An over-eager gate gets switched off.
  for (const suffix of [
    '/user/month',
    ' per seat/month',
    ' per machine per month',
    '/ month',
    ' billed monthly',
    ' every month',
    ' monthly',
  ]) {
    assert.equal(billingUnitOf(suffix), 'month', suffix)
  }
  for (const suffix of [' billed annually', ' each year', ' per user per year', ' annually']) {
    assert.equal(billingUnitOf(suffix), 'year', suffix)
  }
})

test('billingUnitOf still fails closed on a period it cannot read, and stays quiet on prose', () => {
  for (const suffix of ['/fortnight', ' per quarter', ' per user, billed monthly']) {
    assert.equal(billingUnitOf(suffix), 'unrecognised', suffix)
  }
  // Absent, not unrecognised: nothing here announces a period, so there is
  // nothing to compare and a failure would be noise. The docstring names these.
  for (const suffix of [' up front', ' today', '', ' and $67/yr', ' p.a.', ' PCM', ' p/m']) {
    assert.equal(billingUnitOf(suffix), null, suffix)
  }
})

// --- SCAN_DIRS entries are read whole ---------------------------------------

test('a build-named subdirectory inside a SCAN_DIRS entry is still read', () => {
  // SKIP_DIRS exists to keep the gate out of build output inside SOURCE trees.
  // A SCAN_DIRS entry is a hand-registered directory, so a subdirectory that
  // happens to be called `build` is in scope — otherwise registering a
  // directory silently covers only part of it.
  const result = check({ 'proof/recipes/build/case.json': '{"note":"the $9/month terms"}\n' })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /recipes\/build\/case\.json:1: \$9/)
})

test('a build-named subdirectory inside a SCAN_ROOTS tree is pruned', () => {
  // The deliberate opposite of the case above, pinned so the asymmetry reads as
  // intended rather than as an oversight. A SCAN_ROOTS entry is a whole source
  // tree with build output living inside it, so pruning is what keeps the gate
  // from reporting prices no human wrote. Nothing held this half before:
  // dropping `'build'` from SKIP_DIRS, or flipping the walk's `skipBuildDirs`
  // default to false, both passed green. Either reds here now. (Adding a name
  // to SKIP_DIRS does not: this test only sees the seven already there.)
  const result = check({ 'services/web/src/build/bundle.ts': 'export const p = "$9/month"\n' })
  assert.equal(result.ok, true, result.failures.join('\n'))
})

test('a dot-prefixed subdirectory inside a SCAN_DIRS entry is still read', () => {
  // The walk used to skip every dot-prefixed entry as a class, so a recipe
  // filed under `.hidden/` was read by nothing and a disagreeing price there
  // exited 0 — in the one directory registered on the promise that every file
  // in it is scanned.
  const result = check({ 'proof/recipes/.hidden/case.json': '{"note":"the $99/month terms"}\n' })
  assert.equal(result.ok, false)
  assert.match(result.failures.join('\n'), /recipes\/\.hidden\/case\.json:1: \$99/)
})

test('an OS dropping is excused by name, not by being dot-prefixed', () => {
  // `.DS_Store` has no extension, so without IGNORED_FILENAMES the ratchet
  // above would red the gate for any macOS developer who opened the directory
  // in Finder. Excusing it by NAME keeps the class skip from coming back.
  const result = check({ 'proof/recipes/.DS_Store': 'binary junk\n' })
  assert.equal(result.ok, true, result.failures.join('\n'))
})

test('a file type a SCAN_DIRS entry does not list fails rather than being skipped', () => {
  const result = check({ 'proof/recipes/case.yaml': 'note: the $7 terms\n' })
  assert.equal(result.ok, false)
  assert.match(
    result.failures.join('\n'),
    /'\.yaml' is neither listed in the SCAN_DIRS entry for proof\/recipes nor globally ignored/,
  )
})

test('a prose surface that stops stating the price fails, and one statement is not two', () => {
  const gone = check({ 'PRODUCT.md': 'Cloud mode is the hosted tier.\n' })
  assert.equal(gone.ok, false)
  assert.match(gone.failures.join('\n'), /PRODUCT\.md: states the declared price 0 times/)

  // The count is the point. Dropping ONE of the two statements leaves a brief
  // that still mentions a price, so every agreement rule in the gate stays
  // green — this is the only rule that sees it.
  const halved = check({ 'PRODUCT.md': 'Cloud mode ($7/mo) is the hosted tier.\n' })
  assert.equal(halved.ok, false)
  assert.match(halved.failures.join('\n'), /PRODUCT\.md: states the declared price 1 time,/)
})

test('a prose surface counts only statements that agree with the declaration', () => {
  // Two literals, one of them wrong. The disagreement is reported by phase 3,
  // and it must NOT also satisfy the presence rule: a brief stating $7 once and
  // $99 once states the declared price once.
  const result = check({
    'PRODUCT.md': 'Cloud mode ($7/mo) is the hosted tier.\nEnterprise is $99/mo.\n',
  })
  assert.equal(result.ok, false)
  const text = result.failures.join('\n')
  assert.match(text, /PRODUCT\.md: states the declared price 1 time,/)
  assert.match(text, /\$99 disagrees with the declared price/)
})

test('every prose surface is reached by a coverage list, so presence implies agreement', () => {
  // A presence check with no scan behind it would require the file to state a
  // price and leave it free to state the wrong one. Assert the registered set
  // is covered today, then prove the gate says so when one is not.
  for (const surface of PROSE_SURFACES) {
    assert.ok(
      SCAN_FILES.includes(surface.path) ||
        SCAN_DIRS.some((d) => surface.path.startsWith(`${d.path}/`)),
      `${surface.path} is a prose surface no coverage list reaches`,
    )
  }

  assert.equal(proseSurfaceCoverageFailure('PRODUCT.md'), null)
  assert.equal(proseSurfaceCoverageFailure('proof/recipes/README.md'), null)
  assert.match(
    proseSurfaceCoverageFailure('docs/pricing.md') ?? '',
    /read by no coverage list, so it is required to state a price and free to state the wrong one/,
  )
})

test('a prose surface a coverage list names but never reads is not covered', () => {
  // The subtler half of the same hole. A path-prefix test alone would call
  // these covered: they sit under a real coverage list. But SCAN_DIRS and
  // SCAN_ROOTS filter by extension, so the walk never opens them — presence
  // would be required and the amount unchecked, which is the thing the
  // function exists to prevent.
  assert.match(
    proseSurfaceCoverageFailure('proof/recipes/pricing.txt') ?? '',
    /read by no coverage list/,
    'an extension SCAN_DIRS does not list must not count as covered',
  )
  assert.match(
    proseSurfaceCoverageFailure('services/marketing/src/pricing.rst') ?? '',
    /read by no coverage list/,
    'an extension SCAN_ROOTS does not scan must not count as covered',
  )
  // And the positive control, so the assertions above are about the extension
  // rather than about the prefix silently failing to match.
  assert.equal(proseSurfaceCoverageFailure('proof/recipes/pricing.json'), null)
  assert.equal(proseSurfaceCoverageFailure('services/marketing/src/pricing.astro'), null)
})

test('a prose surface under a pruned directory is not covered', () => {
  // The third predicate. Prefix and extension both hold for these paths, so a
  // two-predicate check calls them covered — but SCAN_ROOTS are walked with
  // pruning ON, and `walk` never descends into a SKIP_DIRS segment. Reported
  // covered, read by nothing: required to state a price, free to state the
  // wrong one.
  for (const pruned of ['dist', 'build', 'node_modules', 'coverage', '.astro']) {
    assert.match(
      proseSurfaceCoverageFailure(`services/web/src/${pruned}/pricing.md`) ?? '',
      /read by no coverage list/,
      `a prose surface below a pruned ${pruned}/ must not count as covered`,
    )
  }
  // Nested deeper, so the check is about any directory segment on the path and
  // not only the one directly below the root.
  assert.match(
    proseSurfaceCoverageFailure('services/marketing/src/pages/dist/pricing.md') ?? '',
    /read by no coverage list/,
  )

  // Positive controls, so the assertions above are about pruning and not about
  // the prefix or the extension quietly failing to match.
  assert.equal(proseSurfaceCoverageFailure('services/web/src/pricing.md'), null)
  assert.equal(proseSurfaceCoverageFailure('services/marketing/src/pages/pricing.md'), null)
  // A FILE named `build` is read; `walk` prunes directories only.
  assert.equal(proseSurfaceCoverageFailure('services/web/src/build.md'), null)
  // And SCAN_DIRS entries are walked with `skipBuildDirs: false`, so the same
  // segment below one of those is still read — pruning is a SCAN_ROOTS
  // predicate, and re-deriving it for the wrong list would be its own bug.
  assert.equal(proseSurfaceCoverageFailure('proof/recipes/dist/pricing.md'), null)
})
