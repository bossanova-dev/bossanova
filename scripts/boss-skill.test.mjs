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

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')

// The canonical committed home is the embedded skillinstall payload; the plugin
// copy is the `make copy-skills` mirror. Both are asserted so a partial edit trips
// this gate.
const BOSS_MIRRORS = [
  '../services/boss/internal/skillinstall/skills/boss',
  '../plugins/bossd-plugin-claude/skilldata/skills/boss',
]

const CANONICAL = read(`${BOSS_MIRRORS[0]}/SKILL.md`)

test('size ratchet', () => {
  // Ratchet = committed size rounded up to the next KiB. Never raise this
  // casually — a growing SKILL.md erodes the context budget of EVERY session on
  // the machine, because the boss core installs globally.
  //
  // Pre-split this file was 48624 bytes: the whole generated CLI reference was
  // inline. BOS-637 moved it to references/<group>.md behind an index table,
  // leaving the resident body at ~16.5 KiB. If the reference ever creeps back
  // inline this ratchet is what catches it — regenerating with `make gen-skill`
  // must not be able to grow the resident payload by ~30 KiB unnoticed.
  const RATCHET = 17408
  const bytes = Buffer.byteLength(CANONICAL, 'utf8')
  assert.ok(bytes <= RATCHET, `boss SKILL.md is ${bytes} bytes; must stay <= ${RATCHET}`)
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
      /^\| Reference\s+\| Read it when…\s+\|$/m,
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

// zeroChangeSection returns the bytes of the resident "sessions that change
// nothing" section — from its heading to the next top-level heading, or EOF.
// The slice is the point: asserting the option names anywhere in SKILL.md would
// pass with them scattered across unrelated sections, which is the shape this
// gate exists to reject. Moving any one row out of the section reds the test.
const ZERO_CHANGE_HEADING = '## Sessions that change nothing'

const zeroChangeSection = (skill, label) => {
  const start = skill.indexOf(ZERO_CHANGE_HEADING)
  assert.ok(start !== -1, `${label} must carry the ${ZERO_CHANGE_HEADING} heading`)

  // Position, not just presence: `make gen-skill` rewrites everything between
  // the markers wholesale, so hand-written prose placed above END GENERATED is
  // destroyed on the next regeneration.
  const endGenerated = skill.indexOf('<!-- END GENERATED -->')
  assert.ok(endGenerated !== -1, `${label} must carry the END GENERATED marker`)
  assert.ok(
    start > endGenerated,
    `${label}: the ${ZERO_CHANGE_HEADING} section must sit AFTER <!-- END GENERATED --> (${start} vs ${endGenerated}) or gen-skill will discard it`,
  )

  const next = skill.indexOf('\n## ', start + ZERO_CHANGE_HEADING.length)
  return next === -1 ? skill.slice(start) : skill.slice(start, next)
}

test('the resident body tells an agent how to run a session that changes nothing', () => {
  for (const dir of BOSS_MIRRORS) {
    const section = zeroChangeSection(read(`${dir}/SKILL.md`), `${dir}/SKILL.md`)

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
