// scripts/vendor-toolbox.mjs — copy the canonical skills-toolbox/ helpers into
// each consuming skill's toolbox/ subdir, or verify no drift (--check).
// Mirrors the sync-codex-skills drift-gate idiom. Node builtins only.
import { existsSync, mkdirSync, readFileSync, statSync, writeFileSync, chmodSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { withSkillSourceRewriteLock } from './skill-source-rewrite-lock.mjs'

export { withSkillSourceRewriteLock } from './skill-source-rewrite-lock.mjs'

export const VENDOR_MAP = {
  // boss-review is the only skill that runs the review-specific helpers (its detect,
  // cross-agent, and report phases), so they vendor into its toolbox alone (BOS-196).
  // skill-config.mjs (BOS-192) is a transitive dependency of bs-review-detect.mjs and
  // must be co-located for the bundled helper to resolve its `./skill-config.mjs` import.
  'boss-review': [
    'main-module.mjs',
    // bs-dispatch-await.mjs (BOS-1024) centralizes awaited-dispatch classification
    // and batch planning. It imports bs-run-sentinel.mjs for terminal artefacts and
    // dag-scheduler.mjs for fail-closed dependency waves, so all three ship together.
    'bs-dispatch-await.mjs',
    // bs-dispatch-batch-audit.mjs (BOS-1027) powers Phase 7's report-only transcript self-audit;
    // it must resolve from installed boss-review toolboxes in user repos with no repo-root
    // skills-toolbox/.
    'bs-dispatch-batch-audit.mjs',
    'bs-run-sentinel.mjs',
    'dag-scheduler.mjs',
    'bs-review-caps.mjs',
    'bs-review-ledger.mjs',
    'bs-review-triage.mjs',
    'bs-review-report.mjs',
    'bs-review-detect.mjs',
    'cross-review-lib.mjs',
    'codex-review.mjs',
    'claude-review.mjs',
    'skill-config.mjs',
    'skill-extensions.mjs',
  ],
  'boss-build': [
    'main-module.mjs',
    // bs-dispatch-await.mjs (BOS-1024) is the same-core await contract pointer
    // the skill prose cites; it imports bs-run-sentinel.mjs and dag-scheduler.mjs,
    // both already ship here for the review sentinel and tracker scheduler.
    'bs-dispatch-await.mjs',
    'bs-run-sentinel.mjs',
    'worktree-lock.sh',
    'bs-review-caps.mjs',
    'bs-review-ledger.mjs',
    'bs-review-report.mjs',
    // BOS-1020: the Step 6 review loop re-checks base drift at every round boundary, so the
    // detector must resolve inside an installed boss-build toolbox — the review stack runs in
    // user repos that have no repo-root skills-toolbox/ to reach back into.
    'base-drift.mjs',
    // skill-config.mjs (BOS-204) exposes validatePlanDescription, invoked in Step 4's
    // plan-contract check. boss-build ships to user repos via the embedded skillinstall
    // payload, which has no repo-root skills-toolbox/, so the helper must be co-located in
    // this skill's own toolbox rather than referenced from boss-review's copy.
    'skill-config.mjs',
    'plan-attachment.mjs',
    // Tracker operations are part of boss-build's installed runtime. Keep the
    // seam and its pure Linear helpers co-located with the skill, including the
    // bs-epic scheduler transitively imported by tracker/linear.mjs.
    // adapter-core.mjs is the tracker-agnostic contract adapter.mjs and linear.mjs
    // both import; without it neither resolves in an installed toolbox.
    'tracker/adapter-core.mjs',
    'tracker/adapter.mjs',
    'tracker/linear.mjs',
    'tracker/cli.mjs',
    // Preflight classifies a failed tracker read as "the repo never declared this MCP server for
    // this harness" vs "declared but not answering". The session runner does not configure MCP
    // servers, so that distinction is the difference between fixing the repo and fixing
    // credentials — it must ship in boss-build's own installed toolbox, which cannot reach into
    // another skill's copy.
    'tracker/preflight.mjs',
    'linear-gate-lib.mjs',
    'linear-deps-lib.mjs',
    'linear-claim.mjs',
    'bs-epic-lib.mjs',
    'dag-scheduler.mjs',
    // Callback + finalize reference seams are executable in every boss-build
    // installation, including their transitive helpers.
    'callback/adapter.mjs',
    'callback/boss.mjs',
    'bossd-present.mjs',
    // boss-binary.mjs (BOS-785) backs callbacksAvailable's second conjunct — the
    // `boss` CLI must actually be a resolvable executable, not just implied by
    // BOSS_SESSION_ID. adapter.mjs imports it directly, so every skill that vendors
    // callback/adapter.mjs must vendor this too or the import cannot resolve.
    'boss-binary.mjs',
    'finalize/adapter.mjs',
    'finalize/cli.mjs',
    'finalize/boss-finalize.mjs',
    'finalize/route-contract.mjs',
    'skill-extensions.mjs',
    'pr-ownership.mjs',
    'remove-bossd-stop-hooks.mjs',
    'cron-gates/boss-build.mjs',
    // Preflight drift probe: an installed toolbox can silently fall behind this source tree
    // (the install is a copy, not a link), so the skill compares the two at startup.
    'toolbox-drift.mjs',
    // Preflight names bossEpicTransportPreflight to choose the MCP or CLI carrier, so the
    // module must ship in boss-build's own toolbox — an installed core cannot reach into
    // boss-epic's copy, which may not be installed at all. It imports the session seam's
    // adapter, so both files ship or neither resolves.
    'session/adapter.mjs',
    'session/boss.mjs',
  ],
  // dag-scheduler.mjs is the pure scheduling core bs-epic-lib.mjs re-exports
  // (BOS-197); it must ship alongside bs-epic-lib.mjs so the vendored copy's
  // `./dag-scheduler.mjs` import resolves. Its test file stays in skills-toolbox/
  // only (test files are not vendored, matching bs-epic-lib.test.mjs).
  // progress-comment.mjs (BOS-517) is the single-comment epic-progress toolbox:
  // state-schema validation, byte-stable markdown rendering, and find-by-marker
  // upsert planning, so boss-epic can maintain its epic-progress tracker comment
  // as one comment edited in place instead of ad-hoc /tmp scripts. Its test file
  // stays in skills-toolbox/ only (test files are never vendored).
  'boss-epic': [
    'main-module.mjs',
    // bs-dispatch-await.mjs (BOS-1024) is the same-core await contract pointer
    // the epic prose cites; its bs-run-sentinel.mjs and dag-scheduler.mjs imports
    // already ship here for repair outcomes and child scheduling.
    'bs-dispatch-await.mjs',
    'bs-run-sentinel.mjs',
    'bs-epic-lib.mjs',
    'dag-scheduler.mjs',
    'progress-comment.mjs',
    // The tracker seam is executable in a consuming repo, so ship its
    // descriptor, helpers, and config dependency beside the epic driver.
    // adapter-core.mjs is the tracker-agnostic contract adapter.mjs and linear.mjs
    // both import; without it neither resolves in an installed toolbox.
    'tracker/adapter-core.mjs',
    'tracker/adapter.mjs',
    'tracker/linear.mjs',
    'tracker/cli.mjs',
    // boss-epic reaches the tracker through the same MCP-backed operation map as
    // boss-build, so it inherits the same failure: a repo that has not declared the
    // configured server for the harness it is running gets a mid-run "tool not found".
    // Preflight separates "the repo never declared it" (fix the REPO) from "declared but
    // it did not answer" (fix CREDENTIALS). An installed core cannot reach into another
    // skill's toolbox, so boss-epic needs its own copy or the distinction is unavailable
    // to it.
    'tracker/preflight.mjs',
    'linear-gate-lib.mjs',
    'linear-deps-lib.mjs',
    'linear-claim.mjs',
    'skill-config.mjs',
    // Callback + session reference seams are executable in every boss-epic
    // installation, including callback's bossd-presence dependency.
    'callback/adapter.mjs',
    'callback/boss.mjs',
    'callback/epic-target.mjs',
    'bossd-present.mjs',
    // See boss-build above: adapter.mjs's boss-binary dependency ships with every
    // copy of the adapter or the vendored copy fails to resolve its import.
    'boss-binary.mjs',
    'session/adapter.mjs',
    'session/boss.mjs',
    'skill-extensions.mjs',
  ],
  // skill-config.mjs exposes loadSkillConfig + isConfiguredForRepo, the direct deps of
  // boss-plan's Phase 0 preflight self-disable. boss-plan ships to user repos via the
  // embedded skillinstall payload (no repo-root skills-toolbox/), so the helper must be
  // co-located in this skill's own toolbox rather than referenced from another copy.
  // plan-epic-lib.mjs (epic validation + intra-epic wiring), plan-epic-phase25.mjs (BOS-652:
  // Phase 2.5's epic-parent preconditions and step-4 first-write sequence —
  // detectEpicParent, epicSpecRecoveryGate, stalePlanAttachmentSweep, epicPhase25WritePlan;
  // it imports plan-epic-lib.mjs, so the two must ship together for its relative import to
  // resolve), plan-image-guard.mjs (the
  // image-parity STOP gate), plan-contract-guard.mjs (BOS-741: the producer-side plan-contract
  // STOP gate — missing/unknown/out-of-order sections, placeholder residue, a description that is
  // not the plan, and tool-call residue in the plan file; it imports skill-config.mjs and
  // main-module.mjs, both already vendored here), plan-run-guards.mjs (BOS-769: idempotence,
  // bounded-metadata and premise-drift guards; boss-plan needs its own published copy because a
  // consuming repo has no repo-root skills-toolbox/), plan-deps-lib.mjs (BOS-776: the entire Phase 4
  // step 5 dependency decision — key-change area extraction, the overlap predicate and the
  // seven-rung edge ladder — lifted out of prose an agent could skip; it imports skill-config.mjs
  // and nothing else, which is what keeps this published payload's import closure inside
  // itself) and plan-slug.mjs (the plan-path slug) are boss-plan's
  // deterministic planning core: pure, node-builtin-only helpers the SKILL invokes by
  // path. They ship in the toolbox so a consuming repo never has to re-derive them.
  // Their *.test.mjs / *.demo.mjs siblings are never vendored.
  'boss-plan': [
    'main-module.mjs',
    // bs-dispatch-await.mjs (BOS-1024) is the same-core await contract pointer
    // the drafting-dispatch prose cites; it imports the sentinel and scheduler.
    'bs-dispatch-await.mjs',
    'bs-run-sentinel.mjs',
    'dag-scheduler.mjs',
    'skill-config.mjs',
    'plan-attachment.mjs',
    'plan-epic-lib.mjs',
    'plan-epic-phase25.mjs',
    'plan-image-guard.mjs',
    'plan-contract-guard.mjs',
    'plan-run-guards.mjs',
    'plan-deps-lib.mjs',
    'plan-scratch-reap.mjs',
    'plan-slug.mjs',
    'skill-extensions.mjs',
    // Preflight drift probe: an installed toolbox can silently fall behind this source tree
    // (the install is a copy, not a link), so the skill compares the two at startup.
    'toolbox-drift.mjs',
    // boss-plan-env.sh (BOS-1102) is the toolbox preamble itself: every Bash block in the
    // skill sources it to resolve BOSS_SKILLS_HOME/BOSS_PLAN_TOOLBOX instead of repeating an
    // eight-line probe. It is sourced rather than executed, so it stays 0644, and it must
    // ship inside the toolbox it resolves — the source line names its path directly.
    'boss-plan-env.sh',
  ],
  // session/boss.mjs backs the transport preflight boss-repair runs before Phase 1; like
  // boss-build's copy it ships here because a published core must resolve its own helpers,
  // and it brings session/adapter.mjs with it — the import it cannot resolve without.
  'boss-repair': [
    'main-module.mjs',
    // bs-dispatch-await.mjs (BOS-1024) is the same-core await contract pointer
    // the repair-dispatch prose cites; it imports the sentinel and scheduler.
    'bs-dispatch-await.mjs',
    'bs-run-sentinel.mjs',
    'dag-scheduler.mjs',
    'skill-extensions.mjs',
    // skill-config.mjs exposes notesSampleRate, which the post-terminal notes phase reads to
    // take its per-run sampling roll. boss-repair installs into user repos that have no
    // repo-root skills-toolbox/, and it cannot reach into another core's copy — that core may
    // not be installed at all — so the helper ships in its own toolbox. It is the last of the
    // five notes-taking cores to need it; the other four already vendor it for other callers.
    'skill-config.mjs',
    // Preflight drift probe used only when no boss CLI is available for the
    // fail-closed `boss skills check --gate` path.
    'toolbox-drift.mjs',
    // BOS-1106: the Phase-3 monitoring loop waits on pending checks. That wait is
    // callback-first — arm the one-shot watches when callbacksAvailable(env) is true and
    // fall back to a bounded poll when it is not — so the callback seam and its transitive
    // deps must resolve inside an installed boss-repair toolbox. boss-repair installs into
    // user repos with no repo-root skills-toolbox/ and cannot reach into boss-build's copy,
    // which may not be installed at all. adapter.mjs imports boss-binary.mjs directly and
    // boss.mjs reads bossd presence, so all four ship together or none of them resolve.
    'callback/adapter.mjs',
    'callback/boss.mjs',
    'bossd-present.mjs',
    'boss-binary.mjs',
    'session/adapter.mjs',
    'session/boss.mjs',
  ],
  'boss-finalize': [
    'main-module.mjs',
    // bs-dispatch-await.mjs (BOS-1024) is the same-core await contract pointer
    // the finalize-dispatch prose cites; it imports the sentinel and scheduler.
    'bs-dispatch-await.mjs',
    'bs-run-sentinel.mjs',
    'dag-scheduler.mjs',
  ],
  'bs-sweep-debt': ['main-module.mjs', 'bs-run-sentinel.mjs'],
  'bs-sweep-mutation': ['main-module.mjs', 'bs-run-sentinel.mjs'],
  'bs-sweep-security': ['main-module.mjs', 'bs-run-sentinel.mjs'],
  'bs-sweep-tests': ['main-module.mjs', 'bs-run-sentinel.mjs'],
}

// The published core skills whose canonical committed home is the embedded
// skillinstall payload (BOS-271), not .claude/skills. Their toolbox/ vendors into
// services/boss/internal/skillinstall/skills/<s>/toolbox/; every other VENDOR_MAP
// entry (the repo-local bs-sweep-*) still vendors into .claude/skills/<s>/toolbox/.
export const PUBLISHED_SKILLS = new Set([
  'boss-review',
  'boss-build',
  'boss-epic',
  'boss-plan',
  'boss-repair',
  'boss-finalize',
])

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

import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  const check = process.argv.includes('--check')
  const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '..')
  const skillsRoot = join(repoRoot, '.claude', 'skills')
  const publishedRoot = join(repoRoot, 'services', 'boss', 'internal', 'skillinstall', 'skills')
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
    process.stderr.write(
      'Vendored skill toolboxes are stale. Run `make vendor-toolbox` (it also refreshes the plugin mirror).\n',
    )
    process.stderr.write(res.differences.join('\n') + '\n')
    process.exit(1)
  } else {
    process.stdout.write(`${check ? 'Verified' : 'Vendored'} skill toolboxes\n`)
  }
}
