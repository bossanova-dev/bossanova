#!/usr/bin/env node

// BOS-248 skill-generalization self-parity gate. Bossanova dogfoods its own
// generalized skill machinery as the reference consumer: this gate re-runs the
// post-generalization deterministic skill signature over the SAME
// committed fixtures and asserts they still match the committed
// pre-generalization baseline snapshots (BOS-249's capture-baseline.mjs), AND
// asserts bossanova's authored dogfood extension skills are discovered with the
// exact shapes the generalized cores expect, and that no authored extension
// directory is left unclaimed. Green ⇒ prints a line containing
// `parity: OK` and exits 0; any mismatch ⇒ prints one or more lines each
// containing the token DRIFT plus the affected skill name, and exits 1.
//
// Part A (deterministic exact-match) is the load-bearing check: it re-derives
// each signature via capture-baseline.mjs and byte-compares stableStringify()
// against the committed baseline file. Part B (extension-discovery) asserts the
// structural contract the generalized cores rely on: the authored draft/methodology/
// knowledge/maintenance extensions and notes built-ins are discovered by name with zero
// skips, and their roles
// all resolve in ROLE_SCHEMAS — behaviour-shipping roles included, since BOS-744 derived
// discovery and validation from one EXTENSION_ROLES table. Part C (directory
// coverage) catches an authored directory no spec claims — see checkDirectoryCoverage.
//
// Node builtins only (no new deps).

import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'

import { BASELINE_DIR, REPO_ROOT, computeAll, stableStringify } from './capture-baseline.mjs'
import { ROLE_SCHEMAS, discoverExtensions } from '../../skills-toolbox/skill-extensions.mjs'

/** Snapshot filename → human skill label used in DRIFT messages. */
const SNAPSHOT_SKILL_LABELS = {
  'dag-schedule.snapshot.json': 'boss-epic',
}

/** Repo-authored extensions and published notes built-ins each core must discover. */
const EXPECTED_EXTENSIONS = [
  {
    core: 'boss-plan',
    role: 'plan-reviewer',
    skill: 'boss-plan',
    names: ['boss-plan-feature-map'],
  },
  {
    core: 'boss-plan',
    role: 'draft',
    skill: 'boss-plan',
    names: ['boss-plan-compound-engineering'],
  },
  {
    core: 'boss-build',
    role: 'notes',
    skill: 'boss-build',
    names: ['boss-build-notes'],
    builtin: true,
  },
  {
    core: 'boss-build',
    role: 'methodology',
    skill: 'boss-build',
    names: ['boss-build-ce'],
  },
  {
    core: 'boss-build',
    role: 'knowledge',
    skill: 'boss-build',
    names: ['boss-build-knowledge'],
  },
  {
    core: 'boss-plan',
    role: 'notes',
    skill: 'boss-plan',
    names: ['boss-plan-notes'],
    builtin: true,
  },
  {
    core: 'boss-epic',
    role: 'notes',
    skill: 'boss-epic',
    names: ['boss-epic-notes'],
    builtin: true,
  },
  {
    core: 'boss-repair',
    role: 'notes',
    skill: 'boss-repair',
    names: ['boss-repair-notes'],
    builtin: true,
  },
  // BOS-1507: naming boss-retro here also brings `.claude/skills/boss-retro-*` under the
  // directory-coverage check, which only covers cores that appear in a spec.
  {
    core: 'boss-retro',
    role: 'maintenance',
    skill: 'boss-retro',
    names: ['boss-retro-feature-map'],
  },
]

// --- Part A: deterministic exact-match --------------------------------------

/**
 * Re-derives each signature and byte-compares it against the committed baseline.
 * @param {{ baselineDir?: string, snapshots?: Record<string, unknown> }} [opts]
 * @returns {{ ok: boolean, findings: string[] }}
 */
export function checkDeterministicParity({
  baselineDir = BASELINE_DIR,
  snapshots = computeAll(),
} = {}) {
  const findings = []
  for (const [name, label] of Object.entries(SNAPSHOT_SKILL_LABELS)) {
    const fresh = stableStringify(snapshots[name])
    let committed
    try {
      committed = readFileSync(join(baselineDir, name), 'utf8')
    } catch (err) {
      findings.push(`DRIFT ${label} ${name}: baseline unreadable (${err.message})`)
      continue
    }
    if (fresh !== committed) {
      findings.push(
        `DRIFT ${label} ${name}: recomputed signature no longer matches committed baseline`,
      )
    }
  }
  return { ok: findings.length === 0, findings }
}

// --- Part B: extension-discovery structural assertions ----------------------

function sortedNames(extensions) {
  return extensions.map((e) => e.name).sort()
}

/**
 * Asserts the authored dogfood extensions are discovered with the exact shapes
 * the generalized cores expect.
 * @param {{ root?: string }} [opts]
 * @returns {Promise<{ ok: boolean, findings: string[] }>}
 */
export async function checkExtensionParity({ root = REPO_ROOT } = {}) {
  const findings = []
  for (const spec of EXPECTED_EXTENSIONS) {
    const { core, role, skill } = spec
    const expected = spec.names
    const { extensions, skipped } = discoverExtensions({
      core,
      role,
      root,
      ...(spec.builtin
        ? {
            builtinDir: join(
              REPO_ROOT,
              'services/boss/internal/skillinstall/skills',
              core,
              'extensions',
            ),
          }
        : {}),
    })
    // In THIS repo's curated dogfood tree, every skip is drift. There is no exemption, and the
    // absence of one is deliberate — see the `no x-boss-extension marker still reports DRIFT` case
    // in parity-gate.test.mjs, which pins it.
    //
    // BOS-744 removed a cross-role exemption from here that could never fire: it required a
    // `wrong-role` skip whose NAME appeared among EXPECTED_EXTENSIONS' cross-role entries, but
    // `discoverExtensions` `continue`s a sibling declaring a KNOWN other role without recording it
    // at all, and pushes `wrong-role` ONLY for an unknown declared role — while every name it
    // matched against has a known role. The two halves were mutually exclusive. (It also keyed on a
    // `/^role "/` regex over `reason`, the re-derivation the skip-code contract exists to end.)
    //
    // Note this is the OPPOSITE policy to a core's own ledger, and correctly so. A core runs in an
    // arbitrary consumer checkout, where a markerless same-prefix skill is somebody else's helper
    // and reporting it cries wolf forever — hence `deliberate`. This gate runs over one known tree
    // whose contents are authored here, so an unexplained same-prefix directory IS the drift.
    const badSkips = skipped
    if (badSkips.length > 0) {
      const detail = badSkips.map((s) => `${s.name} (${s.reason})`).join(', ')
      findings.push(`DRIFT ${skill} ${core}/${role}: unexpected skipped extension(s): ${detail}`)
    }
    const got = sortedNames(extensions)
    if (JSON.stringify(got) !== JSON.stringify([...expected].sort())) {
      findings.push(
        `DRIFT ${skill} ${core}/${role}: discovered [${got.join(', ')}], expected [${[...expected].sort().join(', ')}]`,
      )
    }
    if (!ROLE_SCHEMAS[role]) {
      findings.push(`DRIFT ${skill} ${core}/${role}: ROLE_SCHEMAS has no schema for role "${role}"`)
    }
  }

  return { ok: findings.length === 0, findings }
}

// --- Part C: directory coverage ---------------------------------------------

/**
 * BOS-852: asserts every authored `.claude/skills/<core>-*` directory is claimed by exactly
 * one EXPECTED_EXTENSIONS spec.
 *
 * This is the ONLY check that catches a `boss-build-foo` whose marker declares a valid but
 * UNSPECCED known role (say `role: surface` under `extends: boss-build`): it is silently
 * `continue`d by discovery
 * (skills-toolbox/skill-extensions.mjs), so it never lands in `skipped` for badSkips to flag
 * and never lands in `extensions` for the names check to miss. Without this it is invisible.
 *
 * @param {{ root?: string }} [opts]
 * @returns {{ ok: boolean, findings: string[] }}
 */
export function checkDirectoryCoverage({ root = REPO_ROOT } = {}) {
  const findings = []
  let entries
  try {
    entries = readdirSync(join(root, '.claude', 'skills'), { withFileTypes: true })
  } catch {
    // No skills dir -> nothing to cover. Mirrors discoverExtensions' own no-op rather than
    // throwing, so a consumer checkout without repo-local extensions stays green.
    return { ok: true, findings }
  }
  for (const core of new Set(EXPECTED_EXTENSIONS.map((s) => s.core))) {
    // Claim COUNTS, not a set: "claimed by exactly one spec" also rules out two specs
    // listing the same directory, which would hide a role mismatch behind a passing name check.
    const claims = new Map()
    for (const spec of EXPECTED_EXTENSIONS) {
      if (spec.core !== core) continue
      for (const name of spec.names) {
        claims.set(name, (claims.get(name) ?? 0) + 1)
      }
    }
    // The trailing hyphen in the prefix is load-bearing: a standalone `.claude/skills/<core>`
    // skill is not a `<core>-` extension, so it must not be demanded of any spec.
    for (const entry of entries) {
      if (!entry.isDirectory() || !entry.name.startsWith(`${core}-`)) continue
      const claimed = claims.get(entry.name) ?? 0
      if (claimed === 0) {
        findings.push(
          `DRIFT ${core} ${entry.name}: directory is not claimed by any EXPECTED_EXTENSIONS spec (add a spec for its role, or fix its x-boss-extension marker)`,
        )
      } else if (claimed > 1) {
        findings.push(
          `DRIFT ${core} ${entry.name}: directory is claimed by ${claimed} EXPECTED_EXTENSIONS specs, expected exactly one`,
        )
      }
    }
  }
  return { ok: findings.length === 0, findings }
}

// --- Part D: orchestration + CLI --------------------------------------------

/**
 * Runs the deterministic exact-match (A), extension-discovery (B), and directory-coverage (C)
 * checks over the wired tree.
 * @param {{ root?: string, baselineDir?: string }} [opts]
 * @returns {Promise<{ ok: boolean, findings: string[] }>}
 */
export async function runParityGate({ root = REPO_ROOT, baselineDir = BASELINE_DIR } = {}) {
  const deterministic = checkDeterministicParity({ baselineDir })
  const extension = await checkExtensionParity({ root })
  const coverage = checkDirectoryCoverage({ root })
  const findings = [...deterministic.findings, ...extension.findings, ...coverage.findings]
  return { ok: findings.length === 0, findings }
}

export async function main() {
  const { ok, findings } = await runParityGate()
  if (ok) {
    process.stdout.write('parity: OK\n')
    return 0
  }
  for (const finding of findings) process.stdout.write(`${finding}\n`)
  return 1
}

import { isMainModule } from '../../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  main().then((code) => {
    process.exitCode = code
  })
}
