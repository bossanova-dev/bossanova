#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { CI_WATCH_REASONS } from '../skills-toolbox/callback/ci-watch.mjs'
import { region } from './gate-region-lib.mjs'

const rootDir = fileURLToPath(new URL('..', import.meta.url))
// BOS-1212: the plugin mirror is an rsync of this tree (`make copy-skills`), and
// scripts/skill-mirror-generation.test.mjs asserts that generation once for the whole
// payload. Re-asserting each clause against the copy proved only that a copy copied.
const REPAIR_CANONICAL = 'services/boss/internal/skillinstall/skills/boss-repair'

const skillText = (dir) => fs.readFileSync(path.join(rootDir, dir, 'SKILL.md'), 'utf8')

test('BOS-1265: repair re-resolves and reports a fail-safe selection every iteration', () => {
  const gateDiscovery = region(
    skillText(REPAIR_CANONICAL),
    '1.3 Identify Project Gate Commands',
    '### Phase 2:',
  )
  assert.match(gateDiscovery, /decideTestSelection/)
  assert.match(gateDiscovery, /returned\s+`report`\s+verbatim/)
  assert.match(gateDiscovery, /`narrow`[\s\S]{0,100}`commands\.testAffected`/)
  assert.match(
    gateDiscovery,
    /`full`,\s+an\s+unavailable\s+helper,\s+an\s+error,\s+or\s+an\s+uninterpretable\s+result[\s\S]{0,100}`commands\.testFull`/,
  )
  assert.match(gateDiscovery, /Re-resolve\s+on\s+every\s+iteration/)
})

test('BOS-771: Strategy A handles generated artifacts and additive registries', () => {
  {
    const dir = REPAIR_CANONICAL
    const strategy = region(skillText(dir), '#### Strategy A: Merge Conflicts', '#### Strategy B:')
    assert.match(strategy, /generated\s+artifact[\s\S]*regenerate[\s\S]*never\s+hand-edit/i)
    assert.match(
      strategy,
      /additive-vs-additive[\s\S]*append-only\s+registry[\s\S]*keep\s+BOTH\s+sides/,
    )
  }
})

test('BOS-771: Strategy A runs post-rebase checks after the whole rebase', () => {
  {
    const dir = REPAIR_CANONICAL
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
  {
    const dir = REPAIR_CANONICAL
    const skill = skillText(dir)
    assert.match(skill, /BOSS_SKILLS_HOME/, dir)
    assert.match(skill, /boss-repair\/toolbox/, dir)
    assert.match(skill, /skills\s+check\s+--gate/, dir)
    assert.match(
      skill,
      /case "\$O" in[\s\S]{0,120}\*--gate\*\) node "\$BOSS_REPAIR_TOOLBOX\/toolbox-drift\.mjs"/,
      dir,
    )
    // BOS-1105 flipped skills drift from BLOCKING to advisory; BOS-1280 split that one flat
    // advisory along the kind and direction the gate already reports. The body delegates the
    // severity decision instead of re-deriving it in prose...
    assert.match(
      skill,
      // prose-pin: literal-space ok — a shell invocation in a fenced block, not rewrappable prose
      /\*\)[\s\S]{0,200}node "\$BOSS_REPAIR_TOOLBOX\/skill-drift-verdict\.mjs" classify --status 1/,
      dir,
    )
    // ...and the hand-rolled remedy extraction it replaced is gone.
    assert.doesNotMatch(skill, /sed -n 's\/\^  run/, dir)
    // The advisory wording moved WITH the decision: assert it in the copy an installed run loads.
    const verdict = fs.readFileSync(
      path.join(rootDir, dir, 'toolbox', 'skill-drift-verdict.mjs'),
      'utf8',
    )
    assert.match(verdict, /warning:\s+installed\s+boss\s+skills\s+drift\s+from\s+checkout\s+source/)
    assert.match(verdict, /bookkeeping\s+only,\s+work\s+state\s+unaffected/)
    assert.doesNotMatch(
      skill,
      /BLOCKED:\s+installed\s+boss\s+skills\s+differ\s+from\s+checkout\s+source/,
      dir,
    )
  }
})

test('BOS-1243: claim adjudication names an action for the operator-error exit too', () => {
  {
    const dir = REPAIR_CANONICAL
    const pass = region(
      skillText(dir),
      'Adjudicate the cited evidence mechanically',
      '**Round freshness',
    )
    // The three verdicts each carry an action. The CLI's fourth outcome is exit 2 —
    // bad usage or an unreadable claim list — where nothing was adjudicated at all,
    // and an empty record list there reads exactly like "nothing was refuted". Without
    // a named action the prose contract fails green on its own operator error.
    // ONE pin, not two: the prose-pin budget is a real cost and this clause is the whole
    // contract — the exit code and the action it obliges.
    assert.match(pass, /exit\s+2[\s\S]{0,240}(never\s+a\s+verdict|not\s+a\s+verdict)/i, dir)
  }
})

test('BOS-1279: the CI observation gate names both reasons the helper can report as `unknown`', () => {
  // Read from the helper's exported vocabulary, not restated — the same token pin as the sibling
  // boss-build table. `unknown` is a single state with two remedies, and the gate previously routed
  // every `unknown` to the bounded poll, which cannot settle a trigger list that was never handed in.
  const gate = region(skillText(REPAIR_CANONICAL), '**CI observation gate.**', 'Arm at most once')
  for (const reason of [CI_WATCH_REASONS.UNREADABLE, CI_WATCH_REASONS.NO_REQUIRED_TRIGGERS]) {
    assert.ok(gate.includes(reason), `the CI observation gate must name ${reason}`)
  }
})

// BOS-1284 U3: a model copying a commit literal out of this body authors exactly what it reads.
// A scope-less one authors a scope-less commit, and the finalize amend then refuses that commit
// and leaves the WHOLE branch untagged — one bad message blocks every commit's tag. Assert the
// CLASS is empty rather than pinning the two literals that were fixed: a third literal added
// later is the same defect, and a two-string pin would not see it.
test('BOS-1284: no conventional-commit literal in the body is authored without a scope', () => {
  const body = skillText(REPAIR_CANONICAL)
  const scopeless =
    // prose-pin: literal-space ok — a `git commit -m "…"` literal in a fenced block
    /git commit[^\n"]*-m "(build|chore|ci|deps|devs|docs|feat|fix|perf|refactor|revert|style|test):/g
  assert.deepEqual(
    body.match(scopeless) ?? [],
    [],
    'every `git commit -m "type(scope): …"` literal must carry a scope',
  )
  // Non-vacuity: the regex must actually match the shape it is looking for.
  assert.match('git commit -m "fix: no scope here"', scopeless)
  // And the two literals this ticket fixed are still present, scoped — so the class check above
  // cannot pass by the literals simply having been deleted.
  assert.ok(body.includes('git commit -m "fix(rebase): re-apply conflict resolution'), 'rebase')
  assert.ok(body.includes('git commit -m "style(format): apply formatting fixes"'), 'formatter')
})

test('BOS-1288: Strategy C step 3 classifies the fix shape and adjudicates the mutant set', () => {
  const strategyC = region(
    skillText(REPAIR_CANONICAL),
    '#### Strategy C: Review Feedback',
    '### Phase 3: Verify and Monitor',
  )
  // RULE NAME, not a sentence: the obligation is now a call, so what has to survive a rewrite of
  // the surrounding prose is the helper's name and the structural lead that routes a run into it.
  assert.match(strategyC, /bs-mutation-obligations\.mjs/, REPAIR_CANONICAL)
  assert.match(strategyC, /Classify\s+the\s+fix's\s+shape\s+first/, REPAIR_CANONICAL)
  // One mutation is no longer a sufficient proof for anything but a `replacement`, and `satisfied`
  // is the only verdict that lets the round commit. Both halves are the contract; a body that keeps
  // the call but drops the proceeding verdict is back to running the first mutant and stopping.
  assert.match(strategyC, /only\s+verdict\s+that\s+clears\s+the\s+commit/, REPAIR_CANONICAL)
  // The standalone tightening bullet folded INTO the `tightening` shape. A second prose copy is
  // exactly the drift this ticket removed, so its return is a finding rather than a duplication.
  assert.doesNotMatch(strategyC, /A\s+fix\s+that\s+tightens\s+a\s+guard/, REPAIR_CANONICAL)
})
