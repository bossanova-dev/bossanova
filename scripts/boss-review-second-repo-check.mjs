// Second-repo validation harness for the extracted boss-review skill (BOS-196).
//
// Proves the generic-core + repo-local-extension model works OUTSIDE bossanova:
// scaffold a throwaway git repo, drop in ONLY the skill's bundled toolbox/ (no
// repo-root scripts/), plug in a repo-local `<core>-*` review extension via the
// BOS-193 discovery convention, and demonstrate self-containment + discovery +
// ordering + envelope validation + graceful degradation there.
//
// Design note (scope + honesty): a *live* boss-review run dispatches Skill/Task
// subagents and cross-agent CLIs that need interactive auth and the agent harness
// — not reproducible inside an implementation cron. So this harness validates
// everything an autonomous agent *can* deterministically prove: (1) the six
// load-bearing helpers run from toolbox/ with no repo-root scripts/; (2) a
// repo-local `<core>-*` extension carrying the `x-boss-extension` marker is
// discovered, ordered, and its `lens` result envelope validated; (3) discovery
// no-ops when the extension is absent (graceful degradation / inline-fallback
// path). A live subagent review in a real external repo is a non-blocking
// follow-up, not an acceptance gate here.
//
// Node built-ins only (cron/agent worktrees are dependency-free).
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { discoverExtensions, validateResult } from './skill-extensions.mjs'
import { VENDOR_MAP } from './vendor-toolbox.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const REPO_ROOT = path.resolve(HERE, '..')
// The second repo names the core skill generically (`boss-review`) to prove the
// extraction is portable under any name — the extension prefix and `extends`
// marker are keyed to it, so `boss-review-*` extensions extend `boss-review`.
const CORE = 'boss-review'
const EXT = `${CORE}-fixturelens`

// The six load-bearing helpers the skill bundles into its toolbox. cross-review-lib
// is a pure library imported by codex-review/claude-review, so exercising a probe
// transitively proves it resolves from the toolbox too.
const SIX_HELPERS = [
  'bs-review-caps.mjs',
  'bs-review-report.mjs',
  'bs-review-detect.mjs',
  'cross-review-lib.mjs',
  'codex-review.mjs',
  'claude-review.mjs',
]

const PROBE_TOKENS = /^(ready|not_installed|not_authed|error)$/

// The exact files boss-review bundles into its toolbox/, per the vendor manifest —
// the six load-bearing helpers plus skill-config.mjs (a transitive import). Sourcing
// by this list rather than readdir keeps the fallback below from copying unrelated
// helpers or *.test.mjs when reading from the canonical skills-toolbox/ source.
const BUNDLED_FILES = VENDOR_MAP['boss-review']

// Resolve where to read the bundled helper sources from. boss-review is a published
// core whose canonical committed home is the skillinstall payload (BOS-271), where its
// toolbox/ is vendored. If that is somehow absent, fall back to the canonical
// skills-toolbox/ — byte-identical to the vendored copies (enforced by
// `vendor-toolbox --check`), so the self-containment proof is unchanged.
function resolveSourceToolbox() {
  const vendored = path.join(
    REPO_ROOT,
    'services',
    'boss',
    'internal',
    'skillinstall',
    'skills',
    'boss-review',
    'toolbox',
  )
  return fs.existsSync(vendored) ? vendored : path.join(REPO_ROOT, 'skills-toolbox')
}

function runNode(file, args, cwd, input) {
  const res = spawnSync('node', [file, ...args], {
    cwd,
    input: input ?? undefined,
    encoding: 'utf8',
  })
  return { code: res.status, out: (res.stdout ?? '').trim(), err: (res.stderr ?? '').trim() }
}

function fixtureSkillMd() {
  // Minimal repo-local review-lens extension. The frontmatter marker is what
  // discoverExtensions() keys on; the body is a trivial rubric.
  return `---
name: ${EXT}
description: Fixture review-lens extension for the second-repo validation harness.
x-boss-extension:
  extends: ${CORE}
  role: lens
  order: 10
---

# Fixture lens

Review through a trivial fixture rubric: confirm each changed file compiles and
carries a test. This exists only to exercise repo-local extension discovery.
`
}

function sampleLensEnvelope() {
  // A well-formed role:"lens" result envelope (BOS-193 contract): every item
  // carries the ROLE_SCHEMAS.lens keys and the envelope declares ok/extension/role.
  return {
    ok: true,
    extension: EXT,
    role: 'lens',
    items: [
      {
        severity: 'Suggestion',
        file: 'a.go',
        line: 1,
        title: 'Fixture finding',
        detail: 'A representative lens finding used to validate the envelope shape.',
      },
    ],
  }
}

function sampleReportInput() {
  // The clean-report shape bs-review-report.mjs renders from.
  return {
    rounds: 1,
    status: 'clean',
    summary: 'second-repo self-containment smoke',
    verdict: {
      assessment: 'Sound',
      evidence: 'All gates green',
      confidence: 'High',
      testing_assessment: 'Unnecessary',
      recommendation: 'Approve',
    },
    evidenceRows: [],
    gates: [],
    mustfix: { found: 0, fixed: 0, verified: 0, unresolved: 0, items: [] },
    leaveAsIs: [],
    suggestions: [],
    security: [],
    issuesHeadline: '0 must-fix',
  }
}

// Exercise the six bundled helpers from inside the scaffolded repo. Returns
// { helpersRun, runnableOk }. A helper "runs" when it is present in the toolbox
// and produces its expected self-contained output with no repo-root scripts/.
function exerciseHelpers(repoDir, toolbox) {
  const present = SIX_HELPERS.every((f) => fs.existsSync(path.join(toolbox, f)))
  if (!present) return { helpersRun: 0, runnableOk: false }

  const detectPath = path.join(toolbox, 'bs-review-detect.mjs')
  const detect = runNode(detectPath, [], repoDir, 'a.go\nservices/web/x.tsx\nreadme.md\n')
  let detectOk = false
  try {
    const arr = JSON.parse(detect.out)
    // matchLenses (post-BOS-195) returns MatchedLens[]; a .go file must match the go lens,
    // a docs file must match none. This replaced the pre-BOS-195 {go,tui,web} boolean shape.
    detectOk =
      Array.isArray(arr) &&
      arr.some((e) => e.lens === 'go') &&
      !arr.some((e) => Array.isArray(e.files) && e.files.includes('readme.md'))
  } catch {
    detectOk = false
  }

  const secondVoice = runNode(detectPath, ['--second-voice', 'claude'], repoDir)
  const caps = path.join(toolbox, 'bs-review-caps.mjs')
  const rounds = runNode(caps, ['rounds'], repoDir)
  const sentinel = runNode(caps, ['sentinel', 'clean'], repoDir)
  const codexProbe = runNode(path.join(toolbox, 'codex-review.mjs'), ['probe'], repoDir)
  const claudeProbe = runNode(path.join(toolbox, 'claude-review.mjs'), ['probe'], repoDir)

  const reportInput = path.join(repoDir, 'report-input.json')
  fs.writeFileSync(reportInput, JSON.stringify(sampleReportInput()))
  const report = runNode(path.join(toolbox, 'bs-review-report.mjs'), ['--in', reportInput], repoDir)

  const runnableOk =
    detectOk &&
    secondVoice.out === 'codex' &&
    rounds.out === '3' &&
    sentinel.out === 'bs-review clean: no open must-fix findings.' &&
    PROBE_TOKENS.test(codexProbe.out) &&
    PROBE_TOKENS.test(claudeProbe.out) &&
    report.out.includes('<!-- bs-review -->')

  return { helpersRun: SIX_HELPERS.length, runnableOk }
}

export async function runSecondRepoCheck() {
  // realpathSync: on macOS mkdtemp returns a /var -> /private/var symlink, and the
  // helpers' `import.meta.url === file://${process.argv[1]}` CLI guard only fires on
  // the resolved path. Resolve it so the scaffolded invocation matches on every OS.
  const repoDir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'boss-review-2nd-')))
  try {
    // The "second repository".
    spawnSync('git', ['init', '-q'], { cwd: repoDir })

    const skillsDir = path.join(repoDir, '.claude', 'skills')
    const toolbox = path.join(skillsDir, CORE, 'toolbox')
    fs.mkdirSync(toolbox, { recursive: true })

    // Copy ONLY the bundled toolbox — deliberately no repo-root scripts/, proving
    // the skill needs nothing but its own package.
    const srcToolbox = resolveSourceToolbox()
    for (const file of BUNDLED_FILES) {
      fs.copyFileSync(path.join(srcToolbox, file), path.join(toolbox, file))
    }

    // Plug in the repo-local review-lens extension.
    const extDir = path.join(skillsDir, EXT)
    fs.mkdirSync(extDir, { recursive: true })
    fs.writeFileSync(path.join(extDir, 'SKILL.md'), fixtureSkillMd())

    const noRepoScripts = !fs.existsSync(path.join(repoDir, 'scripts'))

    // (1) self-containment
    const { helpersRun, runnableOk } = exerciseHelpers(repoDir, toolbox)
    const selfContained = runnableOk && noRepoScripts

    // (2) discovery + ordering
    const discovered = discoverExtensions({ core: CORE, root: repoDir, role: 'lens' })
    const discoveredExtensions = discovered.extensions

    // (3) envelope validation
    const envelopeValid = validateResult(sampleLensEnvelope(), 'lens').ok === true

    // (4) graceful degradation: remove the extension, discovery no-ops
    fs.rmSync(extDir, { recursive: true, force: true })
    const withoutExt = discoverExtensions({ core: CORE, root: repoDir, role: 'lens' })
    const noopWithoutExtension = withoutExt.extensions.length === 0

    return {
      selfContained,
      helpersRun,
      noRepoScripts,
      discoveredExtensions,
      envelopeValid,
      noopWithoutExtension,
    }
  } finally {
    fs.rmSync(repoDir, { recursive: true, force: true })
  }
}

export async function main() {
  const r = await runSecondRepoCheck()
  const pass =
    r.selfContained &&
    r.helpersRun === SIX_HELPERS.length &&
    r.discoveredExtensions.length === 1 &&
    r.discoveredExtensions[0].name === EXT &&
    r.envelopeValid &&
    r.noopWithoutExtension

  const lines = [
    `self-contained: ${r.selfContained}`,
    `helpers run: ${r.helpersRun}`,
    `repo-root scripts/ absent: ${r.noRepoScripts}`,
    `discovered: ${r.discoveredExtensions.map((e) => e.name).join(', ') || '(none)'}`,
    `envelope valid: ${r.envelopeValid}`,
    `no-op without extension: ${r.noopWithoutExtension}`,
    `second-repo validation: ${pass ? 'PASS' : 'FAIL'}`,
  ]
  process.stdout.write(lines.join('\n') + '\n')
  return pass ? 0 : 1
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().then((code) => process.exit(code))
}
