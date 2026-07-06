// scripts/vendor-toolbox.mjs — copy the canonical skills-toolbox/ helpers into
// each consuming skill's toolbox/ subdir, or verify no drift (--check).
// Mirrors the sync-codex-skills / construct-skills drift-gate idiom. Node builtins only.
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
  'boss-implement': [
    'bs-run-sentinel.mjs',
    'worktree-lock.sh',
    'bs-review-caps.mjs',
    'bs-review-report.mjs',
    // skill-config.mjs (BOS-204) exposes validatePlanDescription, invoked in Step 4's
    // plan-contract check. boss-implement ships to user repos via the embedded skillinstall
    // payload, which has no repo-root skills-toolbox/, so the helper must be co-located in
    // this skill's own toolbox rather than referenced from boss-review's copy.
    'skill-config.mjs',
  ],
  // dag-scheduler.mjs is the pure scheduling core bs-epic-lib.mjs re-exports
  // (BOS-197); it must ship alongside bs-epic-lib.mjs so the vendored copy's
  // `./dag-scheduler.mjs` import resolves. Its test file stays in skills-toolbox/
  // only (test files are not vendored, matching bs-epic-lib.test.mjs).
  'boss-epic': ['bs-run-sentinel.mjs', 'bs-epic-lib.mjs', 'dag-scheduler.mjs'],
  'boss-plan': ['bs-run-sentinel.mjs'],
  'bs-sweep-debt': ['bs-run-sentinel.mjs'],
  'bs-sweep-mutation': ['bs-run-sentinel.mjs'],
  'bs-sweep-security': ['bs-run-sentinel.mjs'],
}

export function vendorToolbox({ sourceRoot, skillsRoot, check }) {
  // Public-mirror checkouts strip .claude/skills entirely (mirror-public.yml), so
  // --check would flag every vendored destination as missing and break `make test`
  // there. Skip when the skills root is absent, mirroring codex-skills-check's skip
  // when its source root has been stripped. Only relevant to --check: `make
  // vendor-toolbox` (write mode) runs in a full checkout where the root exists.
  if (check && !existsSync(skillsRoot)) {
    return { changed: false, differences: [], skipped: true }
  }
  const differences = []
  let changed = false
  for (const [skill, files] of Object.entries(VENDOR_MAP)) {
    for (const file of files) {
      const src = join(sourceRoot, file)
      const dest = join(skillsRoot, skill, 'toolbox', file)
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
  return { changed, differences }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const check = process.argv.includes('--check')
  const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '..')
  const skillsRoot = join(repoRoot, '.claude', 'skills')
  const res = vendorToolbox({
    sourceRoot: join(repoRoot, 'skills-toolbox'),
    skillsRoot,
    check,
  })
  if (res.skipped) {
    process.stdout.write(
      `Skipped vendored toolbox check: ${skillsRoot} is absent (skills stripped)\n`,
    )
  } else if (check && res.changed) {
    process.stderr.write('Vendored skill toolboxes are stale. Run `make vendor-toolbox`.\n')
    process.stderr.write(res.differences.join('\n') + '\n')
    process.exit(1)
  } else {
    process.stdout.write(`${check ? 'Verified' : 'Vendored'} skill toolboxes\n`)
  }
}
