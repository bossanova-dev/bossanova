#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { region } from './gate-region-lib.mjs'

const rootDir = fileURLToPath(new URL('..', import.meta.url))
const REPAIR_MIRRORS = [
  'services/boss/internal/skillinstall/skills/boss-repair',
  'plugins/bossd-plugin-claude/skilldata/skills/boss-repair',
]

const skillText = (dir) => fs.readFileSync(path.join(rootDir, dir, 'SKILL.md'), 'utf8')

test('BOS-771: Strategy A handles generated artifacts and additive registries', () => {
  for (const dir of REPAIR_MIRRORS) {
    const strategy = region(skillText(dir), '#### Strategy A: Merge Conflicts', '#### Strategy B:')
    assert.match(strategy, /generated\s+artifact[\s\S]*regenerate[\s\S]*never\s+hand-edit/i)
    assert.match(
      strategy,
      /additive-vs-additive[\s\S]*append-only\s+registry[\s\S]*keep\s+BOTH\s+sides/,
    )
  }
})

test('BOS-771: Strategy A runs post-rebase checks after the whole rebase', () => {
  for (const dir of REPAIR_MIRRORS) {
    const strategy = region(skillText(dir), '#### Strategy A: Merge Conflicts', '#### Strategy B:')
    assert.match(strategy, /commands\.postRebase/)
    assert.match(
      strategy,
      /After\s+the\s+whole\s+rebase\s+completes[\s\S]*not\s+at\s+the\s+individual\s+conflicting\s+commit/,
    )
    assert.match(
      strategy,
      /grep\s+the\s+post-rebase\s+tree\s+for\s+the\s+OLD\s+shape[\s\S]*files\s+the\s+base\s+added/,
    )
    assert.match(strategy, /run\s+the\s+affected\s+module's\s+tests/)
  }
})

test('BOS-1002: installed-skill gate derives the current tree and degrades for an old boss CLI', () => {
  for (const dir of REPAIR_MIRRORS) {
    const skill = skillText(dir)
    assert.match(skill, /BOSS_SKILLS_HOME/, dir)
    assert.match(skill, /boss-repair\/toolbox/, dir)
    assert.match(skill, /skills\s+check\s+--gate/, dir)
    assert.match(
      skill,
      /case "\$O" in[\s\S]{0,120}\*--gate\*\) node "\$BOSS_REPAIR_TOOLBOX\/toolbox-drift\.mjs"/,
      dir,
    )
    // BOS-1105 flipped skills drift from BLOCKING to advisory: drift is bookkeeping, so the gate
    // reports it and the run continues. A totally missing install still blocks (asserted
    // separately); only the drift arm warns.
    assert.match(
      skill,
      /warning:\s+installed\s+boss\s+skills\s+drift\s+from\s+checkout\s+source/,
      dir,
    )
    assert.match(skill, /bookkeeping\s+only,\s+work\s+state\s+unaffected/, dir)
    assert.doesNotMatch(
      skill,
      /BLOCKED:\s+installed\s+boss\s+skills\s+differ\s+from\s+checkout\s+source/,
      dir,
    )
  }
})
