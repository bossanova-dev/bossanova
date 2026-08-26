// Content/contract test for the boss skill (BOS-637).
//
// The boss skill is a published core: it installs GLOBALLY, so its resident
// SKILL.md is loaded in every repo on the machine. BOS-637 split the generated
// CLI reference out of that resident body — SKILL.md used to inline all ~92
// command sections (48,624 bytes / 1,171 lines) and now carries only the global
// flags, a routing directive and a 16-row index table pointing at
// references/<group>.md, which an agent opens on demand.
//
// This file is the resident byte ratchet plus the structural invariant that split
// creates: the generated region routes, it does not document. The generator's own
// correctness is gated in Go (skillgen's unit tests, TestSkillMatchesGenerated and
// TestGeneratedReferencesRenderEveryExtractedCommand); what is gated here is the
// thing those tests cannot see — that the resident payload stays small, and that a
// future change cannot quietly inline the command reference back into it.
//
// scripts/Makefile globs scripts/*.test.mjs, so `make test-scripts` runs this.
// `make test-smoke` globs only scripts/bs-*-skill.test.mjs and does NOT.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { sectionRegion } from './gate-region-lib.mjs'
import { assertArtifactSet, assertExactSize, measureFile } from './size-ratchet-lib.mjs'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const abs = (rel) => fileURLToPath(new URL(rel, import.meta.url))

// The canonical committed home is the embedded skillinstall payload; the plugin
// copy is the `make copy-skills` mirror. Both are asserted so a partial edit trips
// this gate.
const BOSS_MIRRORS = [
  '../services/boss/internal/skillinstall/skills/boss',
  '../plugins/bossd-plugin-claude/skilldata/skills/boss',
]

const CANONICAL = read(`${BOSS_MIRRORS[0]}/SKILL.md`)

test('size ratchet', () => {
  // Exact pin, not a ceiling. The pin used to be the committed size rounded up to
  // the next KiB, compared one-sidedly — which meant a reduction bought nothing:
  // it just became headroom the resident body could regrow into with nothing going
  // red. assertExactSize reds in BOTH directions, so a shrink is only cleared by
  // banking it here. Never raise this casually — a growing SKILL.md erodes the
  // context budget of EVERY session on the machine, because the boss core installs
  // globally.
  //
  // Pre-split this file was 48624 bytes: the whole generated CLI reference was
  // inline. BOS-637 moved it to references/<group>.md behind an index table,
  // leaving the resident body at ~17 KB. If the reference ever creeps back inline
  // this pin is what catches it — regenerating with `make gen-skill` must not be
  // able to grow the resident payload by ~30 KB unnoticed.
  const RATCHET = 17402 // exact measured resident body, re-measured 2026-08-19

  // This pin measures BOSS_MIRRORS[0]; the mirror test at the bottom of this file
  // compares BOSS_MIRRORS[1] against it. A shortened list would silently stop
  // gating one of the two, with nothing going red — this is what prevents that.
  assertArtifactSet(BOSS_MIRRORS, 2, 'BOSS_MIRRORS')

  assertExactSize({
    constFile: 'scripts/boss-skill.test.mjs',
    constName: 'RATCHET',
    expected: RATCHET,
    label: 'boss resident SKILL.md',
    measured: measureFile(abs(`${BOSS_MIRRORS[0]}/SKILL.md`)),
    path: 'services/boss/internal/skillinstall/skills/boss/SKILL.md',
    residual:
      'whether the resident body is any GOOD — only that it is this many bytes. A rewrite ' +
      'landing on the identical byte count passes, and the references/ files this body routes ' +
      'to are not measured at all, so content moved out of here is invisible to this pin',
  })
})

test('frontmatter identifies the skill', () => {
  assert.match(CANONICAL, /^---\r?\nname: boss\r?\n/, 'frontmatter must declare name: boss')
})

// generatedRegion returns the bytes between the gen-skill markers — the region
// `make gen-skill` rewrites wholesale. Assertions about what the generator emits
// belong here rather than against the whole file, so hand-written prose elsewhere
// in SKILL.md cannot satisfy them by accident.
const generatedRegion = (skill, label) => {
  const begin = skill.indexOf('<!-- BEGIN GENERATED')
  const end = skill.indexOf('<!-- END GENERATED -->')
  assert.ok(begin !== -1, `${label} must carry the BEGIN GENERATED marker`)
  assert.ok(end > begin, `${label} must carry the END GENERATED marker after BEGIN`)
  return skill.slice(begin, end)
}

test('the generated region routes to per-group references instead of inlining them', () => {
  for (const dir of BOSS_MIRRORS) {
    const region = generatedRegion(read(`${dir}/SKILL.md`), `${dir}/SKILL.md`)

    // Global flags stay resident: they apply to every command, so deferring them
    // to a reference would cost a file read on every invocation.
    assert.ok(
      region.includes('## Global Flags'),
      `${dir}: generated region must keep ## Global Flags`,
    )

    // The routing directive is the whole point of the index: an agent must open
    // the reference rather than infer syntax from a one-line index row.
    assert.ok(
      region.includes('**Open the matching reference before using a command**'),
      `${dir}: generated region must carry the "open the reference" routing directive`,
    )

    // The index table itself.
    assert.ok(
      region.includes('## Command Groups'),
      `${dir}: generated region must carry the index heading`,
    )
    assert.match(
      region,
      /^\| Reference\s+\| Read\s+it\s+when…\s+\|$/m,
      `${dir}: index table needs its header row`,
    )
    const rows = region.match(/^\| `references\/[a-z][a-z0-9-]*\.md`\s+\| .+\|$/gm) ?? []
    assert.ok(
      rows.length >= 10,
      `${dir}: index table has ${rows.length} reference rows; expected the full group set`,
    )

    // The invariant the split creates: no command documentation is inline.
    // A `### \`boss …\`` heading here means the reference was re-inlined.
    assert.ok(
      !/^### `boss /m.test(region),
      `${dir}: generated region must not inline command sections — they belong in references/<group>.md`,
    )
  }
})

// ZERO_CHANGE_HEADING names the resident "sessions that change nothing"
// section. The slice is level-aware via sectionRegion().
// The slice is the point: asserting the option names anywhere in SKILL.md would
// pass with them scattered across unrelated sections, which is the shape this
// gate exists to reject. Moving any one row out of the section reds the test.
const ZERO_CHANGE_HEADING = '## Sessions that change nothing'

const assertZeroChangePlacement = (skill, label) => {
  // Position, not just presence: `make gen-skill` rewrites everything between
  // the markers wholesale, so hand-written prose placed above END GENERATED is
  // destroyed on the next regeneration.
  const start = skill.indexOf(ZERO_CHANGE_HEADING)
  assert.ok(start !== -1, `${label} must carry the ${ZERO_CHANGE_HEADING} heading`)
  const endGenerated = skill.indexOf('<!-- END GENERATED -->')
  assert.ok(endGenerated !== -1, `${label} must carry the END GENERATED marker`)
  assert.ok(
    start > endGenerated,
    `${label}: the ${ZERO_CHANGE_HEADING} section must sit AFTER <!-- END GENERATED --> (${start} vs ${endGenerated}) or gen-skill will discard it`,
  )
}

test('the resident body tells an agent how to run a session that changes nothing', () => {
  for (const dir of BOSS_MIRRORS) {
    const skill = read(`${dir}/SKILL.md`)
    assertZeroChangePlacement(skill, `${dir}/SKILL.md`)
    const section = sectionRegion(skill, ZERO_CHANGE_HEADING, `${dir}/SKILL.md`)

    for (const option of ['quick_chat', 'defer_pr', '--zero-output']) {
      assert.ok(
        section.includes(option),
        `${dir}: the ${ZERO_CHANGE_HEADING} section must name ${option} — an agent that cannot find all three here defaults to a worktree-and-PR session that finalizes blocked behind an empty draft PR`,
      )
    }

    // The table names the portable create_session field spellings, which read
    // the same on every host. Scoped to the hand-written section on purpose:
    // the generated references/*.md legitimately document the equivalent CLI
    // flags, so this bans the flag spellings here rather than payload-wide.
    for (const notAFlag of ['--quick-chat', '--defer-pr']) {
      assert.ok(
        !section.includes(notAFlag),
        `${dir}: the ${ZERO_CHANGE_HEADING} section must not name ${notAFlag} — this table names the create_session field spellings; the CLI flags belong to the generated command reference`,
      )
    }

    assert.ok(
      section.includes('`create_session`'),
      `${dir}: the ${ZERO_CHANGE_HEADING} section must label quick_chat/defer_pr as create_session fields`,
    )
  }
})

test('the plugin mirror is byte-identical to the canonical payload', () => {
  const [canonicalDir, pluginDir] = BOSS_MIRRORS
  assert.equal(
    read(`${pluginDir}/SKILL.md`),
    CANONICAL,
    `${pluginDir}/SKILL.md has drifted from ${canonicalDir}/SKILL.md — run \`make copy-skills\``,
  )
})
