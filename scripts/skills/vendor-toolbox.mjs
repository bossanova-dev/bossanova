// scripts/skills/vendor-toolbox.mjs — copy the canonical skills-toolbox/ helpers into
// each consuming skill's toolbox/ subdir, or verify no drift (--check).
// Mirrors the sync-codex-skills drift-gate idiom. Node builtins only.
import {
  existsSync,
  mkdirSync,
  readFileSync,
  statSync,
  writeFileSync,
  chmodSync,
  readdirSync,
  rmSync,
} from 'node:fs'
import { join, dirname, relative } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { withSkillSourceRewriteLock } from './skill-source-rewrite-lock.mjs'

export { withSkillSourceRewriteLock } from './skill-source-rewrite-lock.mjs'

// Each skill declares helpers invoked by path (including shell/dynamic loads).
// Relative ESM dependencies are discovered by expandImportClosure before copying.
export const VENDOR_MAP = {
  'boss-retro': [
    'boss-binary.mjs',
    'bs-dispatch-await.mjs',
    'bs-run-sentinel.mjs',
    'cron-gates/boss-retro.mjs',
    'retro/guidance-audit.mjs',
    'plan-attachment.mjs',
    'retro/retro-classes.mjs',
    'retro/retro-notes.mjs',
    'retro/retro-run.mjs',
    'retro/retro-signals.mjs',
    'retro/retro-write.mjs',
    'skill-extensions.mjs',
    'tracker/cli.mjs',
  ],
  'boss-release': [
    'cron-gates/boss-release.mjs',
    'release/release-gate.mjs',
    'skill-extensions.mjs',
    'stage-chain.mjs',
  ],
  'boss-review': [
    'review/claude-review.mjs',
    'review/codex-review.mjs',
    'review-verdict.mjs',
    'skill-config.mjs',
  ],
  'boss-build': [
    'build/acceptance-retry.mjs',
    'base-drift.mjs',
    'boss-binary.mjs',
    'bossd-present.mjs',
    'bs-record-notes.mjs',
    'callback/adapter.mjs',
    'callback/boss.mjs',
    'callback/ci-watch.mjs',
    'ci-wait.mjs',
    'commit-status.mjs',
    // finalize's tag injector loads this helper by path.
    'commit-work-predicate.mjs',
    'build/completion-gate.mjs',
    'cron-gates/boss-build.mjs',
    'finalize/cli.mjs',
    'finalize/push-branch.mjs',
    'finalize/route-contract.mjs',
    'build/followup-route.mjs',
    'linear-deps-lib.mjs',
    'linear-gate-lib.mjs',
    'notes-record.mjs',
    'plan-attachment.mjs',
    'plan-recheck.mjs',
    'post-rebase-audit.mjs',
    'build/pr-body.mjs',
    'pr-check-state.mjs',
    'build/pr-ownership.mjs',
    'build/ready-gate.mjs',
    'remove-bossd-stop-hooks.mjs',
    'review-verdict.mjs',
    'selection.mjs',
    'session/boss.mjs',
    'skill-config.mjs',
    'skill-drift-verdict.mjs',
    'skill-extensions.mjs',
    'stage-chain.mjs',
    'toolbox-drift.mjs',
    'tracker/adapter.mjs',
    'tracker/cli.mjs',
    'tracker/preflight.mjs',
    'verify/verify-gate.mjs',
    'worktree-lock.sh',
    'worktree-state.mjs',
  ],
  'boss-epic': [
    'boss-binary.mjs',
    // Compatibility roots retain the existing epic toolbox entrypoints.
    'bs-dispatch-await.mjs',
    'epic/bs-epic-lib.mjs',
    'bs-record-notes.mjs',
    'callback/adapter.mjs',
    'callback/boss.mjs',
    'callback/ci-watch.mjs',
    'callback/epic-target.mjs',
    'epic/dag-scheduler.mjs',
    'epic/epic-driver.mjs',
    'epic/merge-eligibility.mjs',
    'pr-check-state.mjs',
    'epic/progress-comment.mjs',
    'session/adapter.mjs',
    'session/boss.mjs',
    'skill-config.mjs',
    'skill-extensions.mjs',
    'tracker/adapter.mjs',
    'tracker/cli.mjs',
    'tracker/preflight.mjs',
  ],
  'boss-plan': [
    'plan/boss-plan-env.sh',
    'bs-dispatch-await.mjs',
    'bs-record-notes.mjs',
    'bs-run-sentinel.mjs',
    'cron-gates/boss-plan.mjs',
    'notes-record.mjs',
    'plan-attachment.mjs',
    'plan/plan-child-fanout.mjs',
    'plan/plan-contract-guard.mjs',
    'plan/plan-deps-lib.mjs',
    'plan/plan-epic-lib.mjs',
    'plan/plan-epic-phase25.mjs',
    'plan/plan-image-guard.mjs',
    'plan/plan-run-guards.mjs',
    'plan/plan-issue-guards.mjs',
    'plan/plan-scratch-paths.mjs',
    'plan/plan-scratch-reap.mjs',
    'plan/plan-secret-scan.mjs',
    'plan/plan-slug.mjs',
    'plan/plan-writeback-verify.mjs',
    'selection.mjs',
    'session/session-self-archive.mjs',
    'skill-config.mjs',
    'skill-drift-verdict.mjs',
    'skill-extensions.mjs',
    'stage-chain.mjs',
    'toolbox-drift.mjs',
    'tracker/cli.mjs',
  ],
  'boss-repair': [
    'bs-dispatch-await.mjs',
    'bs-record-notes.mjs',
    'repair/bs-repair-derivations.mjs',
    'callback/adapter.mjs',
    'callback/ci-watch.mjs',
    'ci-wait.mjs',
    'commit-status.mjs',
    'notes-record.mjs',
    'post-rebase-audit.mjs',
    'pr-check-state.mjs',
    'repair/receipt-carry.mjs',
    'skill-config.mjs',
    'skill-extensions.mjs',
    'stage-chain.mjs',
    'worktree-state.mjs',
    'worktree-sync.mjs',
  ],
  'boss-finalize': [
    'bs-dispatch-await.mjs',
    'commit-work-predicate.mjs',
    'pr-check-state.mjs',
    'worktree-state.mjs',
  ],
  'boss-verify': [
    'cron-gates/boss-verify.mjs',
    'selection.mjs',
    'skill-extensions.mjs',
    'stage-chain.mjs',
    'verify/verify-gate.mjs',
    'verify/verify-route.mjs',
    'worktree-sync.mjs',
  ],
  'bs-sweep-debt': ['bs-run-sentinel.mjs'],
  'bs-sweep-dependabot': ['bs-run-sentinel.mjs'],
}

// The published core skills whose canonical committed home is the embedded
// skillpayload tree (BOS-271, BOS-1722), not .claude/skills. Their toolbox/ vendors into
// lib/bossalib/contract/plugin/skillpayload/skills/<s>/toolbox/; every other VENDOR_MAP
// entry (the repo-local bs-sweep-*) still vendors into .claude/skills/<s>/toolbox/.
export const PUBLISHED_SKILLS = new Set([
  'boss-retro',
  'boss-release',
  'boss-review',
  'boss-build',
  'boss-epic',
  'boss-plan',
  'boss-repair',
  'boss-finalize',
  'boss-verify',
])

// Follow the same relative import forms used by the canonical Node helpers. Shell
// helpers and non-literal dynamic loads remain explicit roots in VENDOR_MAP.
export function expandImportClosure(sourceRoot, roots) {
  const seen = new Set()
  const walk = (file) => {
    if (seen.has(file)) return
    seen.add(file)
    const absolute = join(sourceRoot, file)
    if (!file.endsWith('.mjs') || !existsSync(absolute)) return
    const src = readFileSync(absolute, 'utf8')
    // Match `from` independently of line-leading import/export so multiline
    // specifier lists work too. Over-inclusion keeps the installed copy closed.
    const specs = [
      ...src.matchAll(/\bfrom\s*['"](\.[^'"]+)['"]/g),
      ...src.matchAll(/(?:^|\n)\s*import\s*['"](\.[^'"]+)['"]/g),
      ...src.matchAll(/\bimport\(\s*['"](\.[^'"]+)['"]\s*\)/g),
    ]
    for (const [, spec] of specs) {
      walk(relative(sourceRoot, join(dirname(absolute), spec)))
    }
  }
  for (const file of roots) walk(file)
  return [...seen].sort()
}

function copiedFiles(root, prefix = '') {
  if (!existsSync(root)) return []
  return readdirSync(root, { withFileTypes: true }).flatMap((entry) => {
    const name = prefix + entry.name
    return entry.isDirectory() ? copiedFiles(join(root, entry.name), name + '/') : [name]
  })
}

export function vendorToolbox({ sourceRoot, skillsRoot, publishedRoot, check }) {
  // Each skill resolves to its own destination root: a published core → publishedRoot
  // (the skillpayload tree), everything else → skillsRoot (.claude/skills). When
  // publishedRoot is omitted every skill falls back to skillsRoot, preserving the
  // single-root unit-test contract.
  const rootFor = (skill) =>
    PUBLISHED_SKILLS.has(skill) && publishedRoot ? publishedRoot : skillsRoot
  const differences = []
  let changed = false
  let anyChecked = false
  for (const [skill, roots] of Object.entries(VENDOR_MAP)) {
    const destRoot = rootFor(skill)
    // Public-mirror checkouts strip .claude/skills entirely (mirror-public.yml) but
    // keep the skillpayload tree, so --check skips only the skills whose own
    // destination root is absent rather than bailing on the whole run — a public
    // checkout still verifies the skillpayload-homed cores while skipping the
    // stripped .claude-local skills. Only relevant to --check: write mode runs in a
    // full checkout where both roots exist.
    if (check && !existsSync(destRoot)) continue
    anyChecked = true
    const files = expandImportClosure(sourceRoot, roots)
    const expected = new Set(files)
    const toolboxRoot = join(destRoot, skill, 'toolbox')
    for (const file of copiedFiles(toolboxRoot)) {
      if (expected.has(file)) continue
      changed = true
      if (check) differences.push(`unexpected: ${skill}/toolbox/${file}`)
      else rmSync(join(toolboxRoot, file))
    }
    for (const file of files) {
      const src = join(sourceRoot, file)
      const dest = join(destRoot, skill, 'toolbox', file)
      const srcBuf = readFileSync(src)
      const srcMode = statSync(src).mode & 0o777
      let destBuf = null
      let destMode = null
      try {
        destBuf = readFileSync(dest)
        destMode = statSync(dest).mode & 0o777
      } catch {
        /* missing */
      }
      const contentMatch = destBuf && destBuf.equals(srcBuf)
      // Compare the executable bit too: worktree-lock.sh is invoked directly as $LOCK, so a
      // 0644 vendored copy fails at runtime with permission denied even with matching bytes.
      const modeMatch = destMode === srcMode
      const match = contentMatch && modeMatch
      if (check) {
        if (!match) {
          changed = true
          let reason
          if (!destBuf) {
            reason = 'missing'
          } else if (!contentMatch) {
            reason = 'content mismatch'
          } else {
            reason = 'mode mismatch'
          }
          differences.push(`${reason}: ${skill}/toolbox/${file}`)
        }
        continue
      }
      if (!match) {
        changed = true
        mkdirSync(dirname(dest), { recursive: true })
        writeFileSync(dest, srcBuf)
      }
      chmodSync(dest, srcMode) // preserve the +x bit on worktree-lock.sh
    }
  }
  // Nothing verified (every destination root stripped, e.g. a public mirror missing
  // both .claude/skills and the skillpayload tree): report a skip, not success.
  if (check && !anyChecked) {
    return { changed: false, differences: [], skipped: true }
  }
  return { changed, differences }
}

import { isMainModule } from '../../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  const check = process.argv.includes('--check')
  const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '../..')
  const skillsRoot = join(repoRoot, '.claude', 'skills')
  const publishedRoot = join(
    repoRoot,
    'lib',
    'bossalib',
    'contract',
    'plugin',
    'skillpayload',
    'skills',
  )
  const vendor = () =>
    vendorToolbox({
      sourceRoot: join(repoRoot, 'skills-toolbox'),
      skillsRoot,
      publishedRoot,
      check,
    })
  const res = check ? vendor() : withSkillSourceRewriteLock(repoRoot, vendor)
  if (res.skipped) {
    process.stdout.write(
      `Skipped vendored toolbox check: ${skillsRoot} and ${publishedRoot} are absent (skills stripped)\n`,
    )
  } else if (check && res.changed) {
    process.stderr.write('Vendored skill toolboxes are stale. Run `make vendor-toolbox`.\n')
    process.stderr.write(res.differences.join('\n') + '\n')
    process.exit(1)
  } else {
    process.stdout.write(`${check ? 'Verified' : 'Vendored'} skill toolboxes\n`)
  }
}
