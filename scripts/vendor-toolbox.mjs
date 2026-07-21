// scripts/vendor-toolbox.mjs — copy the canonical skills-toolbox/ helpers into
// each consuming skill's toolbox/ subdir, or verify no drift (--check).
// Mirrors the sync-codex-skills drift-gate idiom. Node builtins only.
import { mkdirSync, readFileSync, writeFileSync, statSync, chmodSync, existsSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

export const VENDOR_MAP = {
  // boss-review is the only skill that runs the review-specific helpers (its detect,
  // cross-agent, and report phases), so they vendor into its toolbox alone (BOS-196).
  // skill-config.mjs (BOS-192) is a transitive dependency of bs-review-detect.mjs and
  // must be co-located for the bundled helper to resolve its `./skill-config.mjs` import.
  'boss-review': [
    'bs-review-caps.mjs',
    'bs-review-report.mjs',
    'bs-review-detect.mjs',
    'cross-review-lib.mjs',
    'codex-review.mjs',
    'claude-review.mjs',
    'skill-config.mjs',
  ],
  'boss-build': [
    'bs-run-sentinel.mjs',
    'worktree-lock.sh',
    'bs-review-caps.mjs',
    'bs-review-report.mjs',
    // skill-config.mjs (BOS-204) exposes validatePlanDescription, invoked in Step 4's
    // plan-contract check. boss-build ships to user repos via the embedded skillinstall
    // payload, which has no repo-root skills-toolbox/, so the helper must be co-located in
    // this skill's own toolbox rather than referenced from boss-review's copy.
    'skill-config.mjs',
  ],
  // dag-scheduler.mjs is the pure scheduling core bs-epic-lib.mjs re-exports
  // (BOS-197); it must ship alongside bs-epic-lib.mjs so the vendored copy's
  // `./dag-scheduler.mjs` import resolves. Its test file stays in skills-toolbox/
  // only (test files are not vendored, matching bs-epic-lib.test.mjs).
  'boss-epic': ['bs-run-sentinel.mjs', 'bs-epic-lib.mjs', 'dag-scheduler.mjs'],
  // skill-config.mjs exposes loadSkillConfig + isConfiguredForRepo, the direct deps of
  // boss-plan's Phase 0 preflight self-disable. boss-plan ships to user repos via the
  // embedded skillinstall payload (no repo-root skills-toolbox/), so the helper must be
  // co-located in this skill's own toolbox rather than referenced from another copy.
  'boss-plan': ['bs-run-sentinel.mjs', 'skill-config.mjs'],
  'bs-sweep-debt': ['bs-run-sentinel.mjs'],
  'bs-sweep-mutation': ['bs-run-sentinel.mjs'],
  'bs-sweep-security': ['bs-run-sentinel.mjs'],
  'bs-sweep-tests': ['bs-run-sentinel.mjs'],
}

// The published core skills whose canonical committed home is the embedded
// skillinstall payload (BOS-271), not .claude/skills. Their toolbox/ vendors into
// services/boss/internal/skillinstall/skills/<s>/toolbox/; every other VENDOR_MAP
// entry (the repo-local bs-sweep-*) still vendors into .claude/skills/<s>/toolbox/.
export const PUBLISHED_SKILLS = new Set(['boss-review', 'boss-build', 'boss-epic', 'boss-plan'])

export function vendorToolbox({ sourceRoot, skillsRoot, publishedRoot, check }) {
  // Each skill resolves to its own destination root: a published core → publishedRoot
  // (the skillinstall payload), everything else → skillsRoot (.claude/skills). When
  // publishedRoot is omitted every skill falls back to skillsRoot, preserving the
  // single-root unit-test contract.
  const rootFor = (skill) =>
    PUBLISHED_SKILLS.has(skill) && publishedRoot ? publishedRoot : skillsRoot
  const differences = []
  let changed = false
  let anyChecked = false
  for (const [skill, files] of Object.entries(VENDOR_MAP)) {
    const destRoot = rootFor(skill)
    // Public-mirror checkouts strip .claude/skills entirely (mirror-public.yml) but
    // keep the skillinstall payload, so --check skips only the skills whose own
    // destination root is absent rather than bailing on the whole run — a public
    // checkout still verifies the skillinstall-homed cores while skipping the
    // stripped .claude-local skills. Only relevant to --check: write mode runs in a
    // full checkout where both roots exist.
    if (check && !existsSync(destRoot)) continue
    anyChecked = true
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
  // both .claude/skills and the skillinstall payload): report a skip, not success.
  if (check && !anyChecked) {
    return { changed: false, differences: [], skipped: true }
  }
  return { changed, differences }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const check = process.argv.includes('--check')
  const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '..')
  const skillsRoot = join(repoRoot, '.claude', 'skills')
  const publishedRoot = join(repoRoot, 'services', 'boss', 'internal', 'skillinstall', 'skills')
  const res = vendorToolbox({
    sourceRoot: join(repoRoot, 'skills-toolbox'),
    skillsRoot,
    publishedRoot,
    check,
  })
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
