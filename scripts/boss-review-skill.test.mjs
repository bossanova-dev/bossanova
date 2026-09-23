// Section-scoped prose gate for the boss-review skill (BOS-742 task 2).
//
// Task 1 landed skills-toolbox/bs-review-triage.mjs — a deterministic helper that
// dedupes reviewer findings on (file, line, title) and promotes a group to
// must-fix on severity OR cross-reviewer convergence. This test pins the SKILL.md
// / core-methodology.md prose that wires Phase 5/6 to that helper, and pins the
// Phase 0 ledger template's `evidence:` field added alongside it.
//
// The assertions are SECTION-SCOPED: each slices the relevant heading-to-heading
// range with sliceSection, then asserts inside the slice. A whole-file
// assert.match would pass on a sentence anywhere in the document — including one
// this ticket did not touch — which is exactly the name-exact ratchet failure
// mode BOS-742 exists to prevent.
//
// Node built-ins only — cron worktrees are dependency-free.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { regionUntilNext, sectionRegion } from './gate-region-lib.mjs'
import { assertFalsifiable } from './prose-pin-lib.mjs'

const rootDir = fileURLToPath(new URL('..', import.meta.url))

// BOS-271 collapsed the published cores onto the boss-repair single-source
// topology: the canonical committed home is the embedded skillinstall payload.
// BOS-1212: the plugins/bossd-plugin-claude mirror is an rsync of that tree, and
// scripts/skill-mirror-generation.test.mjs asserts the generation once for the whole
// payload — so clauses are pinned against the canonical home only.
const REVIEW_CANONICAL = 'services/boss/internal/skillinstall/skills/boss-review'

const read = (relPath) => readFileSync(path.join(rootDir, relPath), 'utf8')

test('BOS-1265: Phase 6 selects and reports its test gate fail-safely', () => {
  const phase6 = sectionRegion(
    read(`${REVIEW_CANONICAL}/SKILL.md`),
    '## Phase 6',
    `${REVIEW_CANONICAL}/SKILL.md`,
  )
  assert.match(phase6, /decideTestSelection/)
  assert.match(phase6, /`report`\s+verbatim/)
  assert.match(phase6, /`narrow`[\s\S]{0,100}`commands\.testAffected`/)
  assert.match(
    phase6,
    /`full`,\s+an\s+unavailable\s+helper,\s+an\s+error,\s+or\s+an\s+uninterpretable\s+result[\s\S]{0,100}`commands\.testFull`/,
  )
})

{
  const mirror = REVIEW_CANONICAL
  const skill = read(`${mirror}/SKILL.md`)
  const coreMethodology = read(`${mirror}/references/core-methodology.md`)

  const phase5 = sectionRegion(skill, '## Phase 5', `${mirror}/SKILL.md`)
  const phase6 = sectionRegion(skill, '## Phase 6', `${mirror}/SKILL.md`)
  const phase7 = sectionRegion(skill, '## Phase 7', `${mirror}/SKILL.md`)
  const phase0 = sectionRegion(skill, '## Phase 0', `${mirror}/SKILL.md`)
  const phase1 = sectionRegion(skill, '## Phase 1', `${mirror}/SKILL.md`)
  const phaseR = sectionRegion(skill, '## Phase R', `${mirror}/SKILL.md`)
  const phaseD = sectionRegion(skill, '## Phase D', `${mirror}/SKILL.md`)
  const findingsContract = sectionRegion(skill, '## Findings contract', `${mirror}/SKILL.md`)
  const coreFindingsContract = sectionRegion(
    coreMethodology,
    '## Findings contract',
    `${mirror}/references/core-methodology.md`,
  )
  const convergenceLoop = sectionRegion(
    coreMethodology,
    '## Convergence loop',
    `${mirror}/references/core-methodology.md`,
  )
  const severityPolicy = sectionRegion(
    coreMethodology,
    '## Severity policy',
    `${mirror}/references/core-methodology.md`,
  )

  test(`[${mirror}] findings contract documents patch field and exact repo-root anchor rule`, () => {
    for (const section of [findingsContract, coreFindingsContract]) {
      assert.match(section, /"patch":\s*\{\s*"file":\s*"<repo-root-relative\s+path>"/)
      assert.match(section, /`file`\s+is\s+rooted\s+at\s+the\s+repository\s+root/)
      assert.match(section, /`old_string`\s+must\s+match\s+exactly\s+once/)
      assert.match(
        section,
        /rejects\s+rather\s+than\s+guesses\s+on\s+zero\s+or\s+multiple\s+matches/,
      )
      assert.match(section, /"patch": null[\s\S]{0,120}`patchReason`/)
    }
  })

  test(`[${mirror}] Phase 5 calls the triage helper's categorize verb and applies the coverage override to its pool`, () => {
    assert.match(phase5, /bs-review-triage\.mjs["'`]?\s+categorize/)
    // Clause-scoped, not token-scoped: this section already said "coverage
    // override" and "pool" BEFORE this ticket, so matching either token alone
    // would gate nothing. Pin the sentence that actually ties the override to
    // the helper's returned `pool`.
    assert.match(phase5, /Apply\s+that\s+override\s+over\s+the\s+returned\s+`pool`/)
  })

  test(`[${mirror}] Phase 5 passes compact file-backed triage inputs instead of the full lens payload`, () => {
    // A large diff can make LENSES_JSON exceed execve's argv limit. The helper needs
    // only indexed `skill` values, so Phase 0 persists that compact mapping and
    // Phase 5 passes its path rather than the complete matched-file payload.
    assert.match(phase0, /lenses\.map\(\(\{ skill \}\) => \(\{ skill \}\)\)/)
    assert.match(phase5, /--lens-entries-file\s+"\$RUN_TMP\/lens-entries\.json"/)
    assert.doesNotMatch(phase5, /categorize[^\n]*\$LENSES_JSON/)
  })

  test(`[${mirror}] Phase 5 treats a selected fallback reviewer with no output as unread`, () => {
    assert.match(phase0, /expected-reviewer-outputs\.json/)
    assert.match(phase5, /--expected-outputs-file/)
    assert.match(
      phase5,
      /run\s+is\s+unread\s+only\s+when\s+it\s+never\s+produces\s+the\s+reviewer\s+output\s+that\s+was\s+ultimately\s+required/,
    )
  })

  test(`[${mirror}] Phase 0 applies the default-round kill switch before ledger seeding`, () => {
    assert.match(phase0, /if \[ "\$\{BOSS_REVIEW_DEFAULT_ROUNDS:-1\}" = "0" \]/)
    assert.match(phase0, /DEFAULT_ROUNDS_JSON='\[\]'/)
    assert.match(
      phase0,
      /--populations[\s\S]{0,260}defaultRounds=JSON\.parse\(process\.env\.DEFAULT_ROUNDS_JSON\)/,
    )
  })

  test(`[${mirror}] Phase 0 rejects traversal-shaped run ids before ledger path assembly`, () => {
    assert.match(phase0, /RUN_ID="\$\{RUN_ID:-\$\(date \+%s\)-\$\$\}"/)
    assert.match(phase0, /\*\/*\|\*\\\\\*/)
    assert.match(phase0, /RUN_ID\s+must\s+be\s+one\s+filename\s+component/)
    assert.match(phase0, /ledger-\$RUN_ID\.json/)
  })

  test(`[${mirror}] Phase 0 resolves .git ledger dirs through Git for linked worktrees`, () => {
    assert.match(phase0, /BOSS_REVIEW_LEDGER_CONFIG_DIR=/)
    assert.match(phase0, /posix\.normalize\(reviewLedgerConfig\(loadSkillConfig\(\)\)\.dir\)/)
    assert.match(phase0, /\.git\)/)
    assert.match(phase0, /\.git\/\*/)
    assert.match(phase0, /git\s+rev-parse\s+--git-path/)
    assert.match(phase0, /git\s+rev-parse\s+--path-format=absolute\s+--git-dir/)
  })

  test(`[${mirror}] Phase 0 rejects symlinked ledger dirs before mkdir`, () => {
    assert.match(phase0, /realpathSync\(existing\)/)
    assert.match(phase0, /isSymbolicLink\(\)/)
    assert.match(phase0, /ledger\s+dir\s+must\s+not\s+pass\s+through\s+a\s+symlink/)
    assert.match(phase0, /isSymbolicLink\(\)[\s\S]{0,260}mkdir -p "\$BOSS_REVIEW_LEDGER_DIR"/)
  })

  test(`[${mirror}] declared-parallel phases write dispatch batch rosters before Task dispatch`, () => {
    assert.match(phase0, /dispatch-batches\.json/)
    for (const section of [phase1, phaseR, phaseD]) {
      assert.match(section, /dispatch-batches\.json/)
      assert.match(section, /planBatches/)
      assert.match(section, /maxWidth/)
      assert.match(section, /admitted\s+roster\s+size/)
      assert.match(section, /dispatch\s+batch\s+<n>\/<m>:\s+<ids>/)
      assert.match(section, /one\s+`Task`\s+call\s+per\s+member\s+of\s+wave\s+1/)
    }
  })

  test(`[${mirror}] Phase 5 documents the categorize panel block as verdict sample evidence`, () => {
    assert.match(phase5, /prints\s+\{mustFix,\s+pool,\s+invalid,\s+panel\}/)
    assert.match(phase5, /`panel`\s+block\s+naming\s+the\s+distinct\s+reviewers/)
    assert.match(phase5, /reported\s+findings/)
    assert.match(phase5, /returned\s+none/)
    assert.match(phase5, /rostered\s+output\s+that\s+never\s+arrived/)
    assert.match(phase5, /sample\s+behind\s+a\s+clean\s+verdict/)
  })

  test(`[${mirror}] Phase 5 distinguishes an unread findings file from a malformed item`, () => {
    // The helper's `categorize` verb reports a whole findings file it could not
    // read as an array, not just malformed items. Prose that lumps the two
    // together tells the operator to under-react to a reviewer whose entire
    // output went unread, so the distinction is pinned here.
    assert.match(phase5, /could\s+not\s+read\s+as\s+a\s+list\s+of\s+findings/)
    assert.match(phase5, /entire\s+output\s+went\s+unread/)
    assert.match(phase5, /not\s+partial\s+ones/)
    // A round whose reviewers' files never parsed yields zero must-fix. The
    // clean-exit rule must not read that as a clean round.
    assert.match(phase5, /no\s+`invalid`\s+entry\s+is\s+still\s+unrepaired/)
    assert.match(phase5, /Every\s+unrepaired\s+`invalid` entry\s+blocks\s+a\s+clean\s+verdict/)
  })

  test(`[${mirror}] Phase 5 clean exit cites the derived verdict owner`, () => {
    assert.match(phase5, /derived\s+verdict\s+owner/i)
    assert.match(phase5, /bs-review-caps\.mjs["'`]?\s+verdict\s+--in\s+"\$REPORT_JSON"/)
    assert.match(phase5, /clean\s+exit\s+is\s+mechanical/i)
  })

  test(`[${mirror}] Phase 5 says patch rejections stay invalid but route to the narrative remainder`, () => {
    assert.match(
      phase5,
      /malformed,\s+missing-file,\s+non-unique,\s+or\s+conflict-composed\s+`patch`/,
    )
    assert.match(
      phase5,
      /route\s+the\s+underlying\s+finding\s+through\s+the\s+narrative\s+must-fix\s+path/,
    )
  })

  test(`[${mirror}] Phase 5 qualifies convergence promotion on distinct reviewers`, () => {
    assert.match(phase5, /convergence/i)
    assert.match(phase5, /distinct\s+reviewers/i)
    // A rule worded merely "occurrences" would license same-lens double
    // counting (the same reviewer reporting the same finding twice). The
    // prose must tie the count to reviewers, never raw occurrences.
    assert.match(phase5, /never\s+occurrences/i)
  })

  test(`[${mirror}] Phase 5 categorizes once per complete review pass over pooled findings`, () => {
    assert.match(phase5, /once\s+per\s+review\s+pass/)
    assert.match(phase5, /pooled\s+findings\s+from\s+every\s+reviewer/)
    assert.match(
      phase5,
      /every\s+dispatched\s+reviewer\s+has\s+either\s+returned\s+or\s+been\s+recorded\s+as\s+a\s+ledger\s+skip/,
    )
    assert.match(phase5, /Never\s+categorize\s+a\s+partial\s+pass/)
    assert.match(phase5, /never\s+categorize\s+per\s+reviewer/)
    assert.match(phase5, /never\s+categorize\s+per\s+round\s+extension/)
  })

  test(`[${mirror}] Phase 6 adjudicates before fixing and requires evidence on a verified disposition`, () => {
    assert.match(phase6, /adjudicate\s+before\s+you\s+fix/i)
    assert.match(phase6, /confirmed\s+or\s+falsified/i)
    assert.match(phase6, /\bverified\b/)
    // A bare /evidence/ matched this section BEFORE this ticket ("never
    // clobbers a prior round's evidence"), so it gates nothing. Pin the clause
    // that binds the evidence to the `verified` disposition itself.
    assert.match(phase6, /the\s+`evidence`\s+that\s+settled\s+it/)
    assert.match(
      phase6,
      /`verified`\s+disposition\s+that\s+changes\s+no\s+files\s+records\s+only\s+the\s+ledger\s+entry/,
    )
  })

  test(`[${mirror}] Phase 6 applies patchable findings mechanically before narrative fix dispatch`, () => {
    assert.match(phase6, /Partition\s+the\s+must-fix\s+items\s+into\s+patchable\s+and\s+narrative/)
    assert.match(phase6, /Apply\s+patchable\s+findings\s+mechanically\s+in\s+the\s+orchestrator/)
    assert.match(
      phase6,
      /compose\s+overlapping\s+patches\s+in\s+one\s+file\s+into\s+the\s+helper's\s+single\s+exact\s+replacement/,
    )
    assert.match(
      phase6,
      /re-read\s+the\s+current\s+file\s+bytes\s+immediately\s+before\s+each\s+application/,
    )
    assert.match(
      phase6,
      /Dispatch\s+a\s+fresh\s+`general-purpose`\s+fix\s+subagent\s+\(awaited\)\s+\*\*only\*\*/,
    )
  })

  test(`[${mirror}] Phase 6 gates once per fix batch rather than per item or finding`, () => {
    assert.match(phase6, /one\s+fix\s+batch\s+for\s+the\s+review\s+pass/)
    assert.match(phase6, /no\s+gate\s+runs\s+per\s+item\s+or\s+per\s+finding/)
    assert.match(
      phase6,
      /run\s+the\s+affected\s+module\s+tests\/lint[\s\S]{0,220}exactly\s+once\s+for\s+the\s+batch/,
    )
    assert.match(phase6, /batch-close\s+gate\s+fails/)
    assert.match(phase6, /fix\s+forward\s+inside\s+the\s+same\s+batch/)
  })

  test(`[${mirror}] Phase 6 documents the interleaved-fix trigger and one-split cap`, () => {
    assert.match(phase6, /sole\s+interleaved-fix\s+exception/)
    assert.match(phase6, /intra-batch\s+ordering\s+dependency/)
    assert.match(
      phase6,
      /changes\s+the\s+file\s+or\s+bytes\s+cited\s+by\s+another\s+must-fix\s+item/,
    )
    assert.match(phase6, /split\s+the\s+pass\s+into\s+two\s+dependency-ordered\s+sub-batches/)
    assert.match(phase6, /cap\s+the\s+exception\s+at\s+one\s+split\s+per\s+pass/)
    assert.match(phase6, /record\s+the\s+trigger\s+and\s+member\s+sets\s+in\s+the\s+ledger/)
  })

  test(`[${mirror}] core-methodology convergence loop carries the same mechanical patch path`, () => {
    assert.match(
      convergenceLoop,
      /Partition\s+must-fix\s+items\s+into\s+patchable\s+and\s+narrative/,
    )
    assert.match(
      convergenceLoop,
      /Apply\s+patchable\s+findings\s+mechanically\s+in\s+the\s+orchestrator/,
    )
    assert.match(
      convergenceLoop,
      /Dispatch\s+a\s+fix\s+step\s+\*\*only\*\*\s+for\s+the\s+narrative\s+remainder/,
    )
  })

  test(`[${mirror}] core-methodology convergence loop gates once at batch close`, () => {
    assert.match(convergenceLoop, /one\s+fix\s+batch/)
    assert.match(convergenceLoop, /no\s+gate\s+runs\s+per\s+item\s+or\s+per\s+finding/)
    assert.match(convergenceLoop, /configured\s+gate\s+commands\s+once\s+at\s+batch\s+close/)
    assert.match(convergenceLoop, /intra-batch\s+ordering\s+dependency/)
    assert.match(convergenceLoop, /at\s+most\s+one\s+split\s+is\s+allowed\s+per\s+pass/)
  })

  test(`[${mirror}] Phase 7 report schema carries the patch summary tally`, () => {
    assert.match(
      phase7,
      /"patchSummary":\s*\{\s*"patchable":\s*P,\s*"narrative":\s*N,\s*"nullWithReason":\s*R\s*\}/,
    )
    assert.match(phase7, /patchable\/narrative\/null-with-reason\s+tally/)
  })

  test(`[${mirror}] Phase 7 records dispatch batch self-audit without changing outcome`, () => {
    assert.match(phase7, /bs-dispatch-batch-audit\.mjs/)
    assert.match(phase7, /--run-tmp\s+"\$RUN_TMP"/)
    assert.match(phase7, /dispatch\s+batch\s+self-audit/)
    assert.match(phase7, /report-only/)
    assert.match(phase7, /never\s+changes\s+the\s+review\s+exit\s+code/)
  })

  test(`[${mirror}] Phase 7 report schema carries panel and agreement evidence`, () => {
    assert.match(phase7, /"panel":\s*\{[\s\S]{0,220}"initial"[\s\S]{0,220}"reviewers"/)
    assert.match(phase7, /"agreement":\s*\{[\s\S]{0,260}"panelSize"[\s\S]{0,260}"vanishedFindings"/)
    assert.match(phase7, /Build\s+`panel`\s+from\s+each\s+round's\s+`categorize`\s+output/)
    assert.match(phase7, /initial\s+panel\s+and\s+terminal\s+panel\s+separately/)
    assert.match(phase7, /renders\s+panel\s+size,\s+initial-vs-terminal\s+panel/)
  })

  // BOS-798: `proseWrap: preserve` means prettier does not reflow prose, so a hand-split or
  // worker-inserted sentence leaves an orphan line mid-paragraph that `--check` calls correctly
  // formatted. The formatter cannot be the orchestrator's only markdown check, so Phase 6 must
  // say when the eyeball happens. The fix subagent commits its own work inside the dispatch, so
  // the timing has two halves and both are pinned: the subagent's brief carries the pre-commit
  // eyeball, and the orchestrator amends on return. A pin on "before the commit" alone would
  // freeze a timing this flow cannot reach.
  // Pins are `\s+`-tolerant: this paragraph rewraps whenever a neighbouring sentence is edited.
  test(`[${mirror}] Phase 6 eyeballs a returned markdown hunk and routes the pre-commit half to the subagent`, () => {
    assert.match(phase6, /returned\s+diff\s+touches\s+markdown/)
    assert.match(phase6, /the\s+moment\s+the\s+dispatch\s+returns/)
    assert.match(phase6, /delegating\s+the\s+edit\s+does\s+not\s+delegate\s+this/)
    // The timing must name the subagent's own commit, or the rule prescribes a moment that has
    // already passed by the time the orchestrator sees the hunk.
    assert.match(phase6, /subagent\s+commits\s+its\s+own\s+work/)
    assert.match(phase6, /put\s+the\s+eyeball\s+in\s+its\s+brief\s+too/)
    assert.match(phase6, /check\s+the\s+hunk\s+before\s+you\s+commit/)
    assert.match(phase6, /on\s+return\s+amend\s+its\s+commit/)
    // The orchestrator's half must stay reachable: the subagent may have committed more than
    // once, so an amend-only instruction is unperformable whenever its work is not the tip.
    assert.match(phase6, /add\s+a\s+follow-up\s+one\s+when\s+it\s+is\s+no\s+longer\s+the\s+tip/)
    // The *reason* must be resident, not just the instruction: a rule whose rationale is missing
    // is the first one dropped when the round is behind.
    assert.match(phase6, /`proseWrap: preserve`\s+does\s+not\s+reflow\s+prose/)
    assert.match(
      phase6,
      /orphan\s+line\s+mid-paragraph\s+that\s+`--check`\s+reports\s+as\s+correctly\s+formatted/,
    )
    // The table-cell half of the rule.
    assert.match(
      phase6,
      /run\s+the\s+formatter\s+immediately\s+after\s+editing\s+a\s+markdown\s+table\s+cell/,
    )
    assert.match(phase6, /churn\s+is\s+padding-only/)
  })

  test(`[${mirror}] Phase 6 hands '## Leave as-is' to the confirming round`, () => {
    // `## Leave as-is` and "confirming round" both appeared here pre-ticket;
    // the handoff sentence that joins them is the load-bearing addition.
    assert.match(
      phase6,
      /Feed\s+the\s+confirming\s+round\s+the\s+ledger's\s+`## Leave\s+as-is`\s+entries/,
    )
    assert.match(phase6, /factually\s+false/i)
    assert.match(phase6, /re-opens\s+the\s+finding/i)
  })

  test(`[${mirror}] confirming rounds rebuild their indexed lens identity map`, () => {
    assert.match(phase5, /fresh\s+`LENSES_JSON`/)
    assert.match(phase5, /full\s+confirming\s+surface/)
    assert.match(
      phase5,
      /union\s+of\s+newly\s+changed\s+files\s+and\s+the\s+cited\s+files\s+of\s+every\s+verified\s+finding/,
    )
    assert.match(phase5, /\$RUN_TMP\/round<N>\/lens-entries\.json/)
    assert.match(phase5, /Do\s+not\s+reuse\s+the\s+initial-round\s+mapping/)
    assert.match(phase5, /CURRENT_FINDINGS_DIR="\$RUN_TMP"/)
    assert.match(phase5, /CURRENT_FINDINGS_DIR="\$RUN_TMP\/round<N>"/)
    assert.match(phase7, /--findings-dir\s+"\$CURRENT_FINDINGS_DIR"/)
  })

  test(`[${mirror}] Phase 1 records timed-out lens dispatches distinctly`, () => {
    assert.match(phase1, /timed-out\s+dispatch\s+records\s+`outcome:\s+timed-out`/)
    assert.match(phase1, /--outcome\s+skipped-or-timed-out/)
  })

  test(`[${mirror}] Phase R records timed-out round dispatches distinctly`, () => {
    assert.match(phaseR, /`--outcome\s+timed-out`\s+for\s+timeouts/)
    assert.match(phaseR, /--outcome\s+"<skipped\|timed-out>"/)
  })

  test(`[${mirror}] Phase 6 cannot finish clean with malformed reviewer findings`, () => {
    assert.match(phase6, /zero\s+must-fix\s+\*\*and\s+zero\s+unrepaired\s+`invalid` entries\*\*/)
    assert.match(phase6, /report `capped`, never `clean`/)
  })

  test(`[${mirror}] Phase 6 documents vanished findings as disagreement evidence`, () => {
    assert.match(phase6, /Disappearance\s+guard/)
    assert.match(phase6, /vanishedFindings\(history\)/)
    assert.match(phase6, /must-fix\s+at\s+round\s+N/)
    assert.match(phase6, /absent\s+at\s+round\s+N\+1/)
    assert.match(phase6, /neither\s+`## Fixed`\s+nor\s+`## Leave\s+as-is`/)
    assert.match(phase6, /reviewer\s+disagreement/)
    assert.match(phase6, /Agreement\s+section/)
  })

  test(`[${mirror}] Phase 6 and core-methodology route oscillation through the caps helper`, () => {
    const oscillationGuard = sectionRegion(
      coreMethodology,
      '### Oscillation guard',
      `${mirror}/references/core-methodology.md`,
    )
    assert.match(phase6, /bs-review-caps\.mjs"\s+oscillation\s+--in/)
    assert.match(
      phase6,
      /build\s+`oscillation_json`\s+from\s+the\s+previous\s+and\s+current\s+categorized/,
    )
    assert.match(phase6, /intervening\s+ledger\s+`fixed`\/`verified`\s+dispositions/)
    assert.match(phase6, /write\s+it\s+to\s+`\$RUN_TMP\/round<N>\/oscillation\.json`/)
    assert.match(phase6, /deterministic\s+JSON\s+tuple\s+identity\s+`\[file,line,title\]`/)
    assert.match(oscillationGuard, /bs-review-caps\.mjs\s+oscillation\s+--in\s+<payload\.json>/)
    assert.match(oscillationGuard, /oscillation\s+payload\s+file/)
    assert.match(
      oscillationGuard,
      /deterministic\s+JSON\s+tuple\s+identity\s+`\[file,line,title\]`/,
    )
    assert.match(oscillationGuard, /fixed\/verified\s+dispositions/)
  })

  test(`[${mirror}] invalid-only rounds repair their owning reviewer before dispatching a fixer`, () => {
    assert.match(
      phase5,
      /repair\s+every\s+unrepaired `invalid` entry\s+through\s+its\s+owning\s+reviewer/,
    )
    assert.match(phase5, /same\s+round's\s+original\s+review\s+surface/)
    assert.match(
      phase5,
      /Do \*\*not\*\* dispatch\s+the\s+Phase\s+6\s+fixer\s+with\s+an\s+empty\s+must-fix\s+list/,
    )
    assert.match(phase5, /same\s+cap\s+and\s+ledger\s+history/)
  })

  test(`[${mirror}] Phase 7 makes malformed payloads and verification evidence durable`, () => {
    const phase7 = sectionRegion(skill, '## Phase 7', `${mirror}/SKILL.md`)
    // Every space is `\s+`: prettier rewraps this prose at 100 columns, so a
    // literal space here would make the gate fail on a reflow rather than on a
    // dropped guarantee. Both halves are asserted — the payload AND the source
    // that names whose output it was — because retaining one without the other
    // leaves invalid evidence that cannot be routed back to a reviewer.
    const malformedEvidence = /retained\s+reviewer\/output\s+source[\s\S]{0,80}malformed\s+payload/
    assert.match(phase7, malformedEvidence)
    assert.match(
      'retained reviewer/output source, payload parser diagnostics, and malformed payload',
      malformedEvidence,
      'the Phase 7 pin must allow a richer wording that keeps the semantic tokens',
    )
    assertFalsifiable({
      source: phase7,
      pattern: malformedEvidence,
      mutation: { find: /malformed\s+payload/g, replacement: 'payload' },
      label: `${mirror}/SKILL.md Phase 7 malformed-payload evidence`,
    })
    assert.match(
      phase7,
      /\"evidence\": \"<file:line[ ]read[ ]or[ ]command[ ]result[ ]that[ ]settled[ ]it>\"/,
    )
    assert.match(phase7, /including\s+each\s+verified\s+finding's\s+evidence/)
  })

  test(`[${mirror}] Phase 7 emits the terminal sentinel through the derived verdict owner`, () => {
    assert.match(
      phase7,
      /node\s+"\$BOSS_REVIEW_TOOLBOX\/bs-review-caps\.mjs"\s+verdict\s+--in\s+"\$REPORT_JSON"/,
    )
    assert.match(phase7, /The\s+report's\s+own\s+evidence\s+chooses\s+the\s+sentinel/)
    assert.match(phase7, /caller-supplied\s+`status`\s+disagrees\s+with\s+the\s+derived\s+verdict/)
    assert.match(phase7, /classify\s+--in[\s\S]{0,160}missing[\s\S]{0,80}non-clean/i)
  })

  test(`[${mirror}] Phase 7 derives confidence through the toolbox owner`, () => {
    assert.match(phase7, /bs-review-caps\.mjs\s+confidence\s+--in\s+"\$REPORT_JSON"/)
    assert.match(phase7, /displayed\s+Confidence\s+badge\s+is\s+derived/)
    assert.match(phase7, /panel\/agreement\s+evidence/)
    assert.match(phase7, /caller-supplied\s+`verdict\.confidence`/)
    assert.match(phase7, /confidence\s+contradiction\s+notice/)
    assert.match(phase7, /single-sample\s+panel\s+or\s+vanished\s+finding/)
    assert.match(phase7, /human\s+should\s+adjudicate/)
  })

  test(`[${mirror}] the Phase 0 ledger template's Leave as-is comment carries rationale: and evidence:`, () => {
    const leaveAsIsTemplate = regionUntilNext(phase0, '## Leave as-is', '```', `${mirror}/SKILL.md`)
    assert.match(leaveAsIsTemplate, /rationale:/)
    assert.match(leaveAsIsTemplate, /evidence:/)
  })

  test(`[${mirror}] core-methodology.md's severity policy states convergence promotion on distinct reviewers`, () => {
    assert.match(severityPolicy, /convergence\s+promotion/i)
    assert.match(severityPolicy, /distinct\s+reviewers/i)
  })

  test(`[${mirror}] core-methodology.md's confidence rubric is panel/agreement-derived`, () => {
    const confidenceRubric = sectionRegion(
      coreMethodology,
      '## Confidence rubric',
      `${mirror}/references/core-methodology.md`,
    )
    assert.match(confidenceRubric, /panel\s+that\s+produced\s+the\s+verdict/)
    assert.match(confidenceRubric, /terminal\s+panel\s+has\s+fewer\s+than\s+two\s+reviewers/)
    assert.match(confidenceRubric, /must-fix\s+at\s+round\s+N/)
    assert.match(confidenceRubric, /terminal\s+panel\s+is\s+smaller\s+than\s+the\s+initial\s+panel/)
    assert.match(confidenceRubric, /one\s+reviewer\s+alone\s+raised\s+a\s+must-fix/)
    assert.match(confidenceRubric, /terminal\s+panel\s+has\s+at\s+least\s+two\s+reviewers/)
  })

  // BOS-1197: the RECEIVING half of the funding disclosure. `funding-starved` was reserved in the
  // Phase 7 report schema and supplied by no caller, so the field shipped inert while every gate
  // read as satisfied. Now the caller writes it on every terminal sentinel payload of a starved
  // step, which changes what this skill owes: it copies the key through, and it reads ABSENCE as
  // "not starved" rather than "no caller implements it". Section-scoped to the caller-deadline
  // region, so a sentence anywhere else in the document cannot satisfy the pin.
  test(`[${mirror}] the funding reason is sourced from the caller payload, never synthesised`, () => {
    const callerDeadline = sectionRegion(
      skill,
      '## Caller deadline (wall-clock cap)',
      `${mirror}/SKILL.md`,
    )
    for (const [label, pattern] of [
      [
        'that the caller STATES the reason under a named interface, alongside the deadline',
        /\*\*states\*\*\s+`?STEP_6C_FUNDING_REASON`?\s+in\s+this\s+pass's\s+invocation[\s\S]{0,120}`?STEP_6C_DEADLINE`?/i,
      ],
      [
        'that the stated name becomes the payload key on every terminal write, this pass included',
        /`?funding\.reason`?\s+on\s+every\s+terminal\s+write,\s+including\s+the\s+writes\s+this\s+pass\s+makes\s+itself/i,
      ],
      [
        'that the reason is read back into the report metadata and rendered summary',
        /carry\s+it\s+in\s+the\s+report\s+metadata\s+and\s+rendered\s*\n?\s*summary/i,
      ],
      [
        'that ABSENCE means not starved, not unimplemented',
        /\*\*absence\*\*\s+means\s+the\s+step\s+was\s+not\s+starved\s+—\s+not\s+that\s+no\s+caller\s+implements\s+the\s+key/i,
      ],
      [
        'that the reason is never synthesised or re-derived from a clock',
        /do\s+not\s+synthesise\s+a\s+reason,\s+do\s+not\s+re-derive\s+one\s+from\s+a\s+clock/i,
      ],
      [
        'that the byte-stable sentinel line is still untouched',
        /do\s+not\s+alter\s+the\s+byte-stable\s+sentinel\s+line/i,
      ],
    ]) {
      assert.match(
        callerDeadline,
        pattern,
        `${mirror}/SKILL.md §Caller deadline must state ${label}`,
      )
    }

    // The report schema's own comment has to agree, or the two halves drift: a field documented as
    // "optional" invites a reader to omit a reason the caller did supply.
    assert.match(
      skill,
      /"funding":\s*\{\s*"reason":\s*"funding-starved"\s*\|\s*"funding-unpriced"\s*\},\s*\/\/\s*present\s+exactly\s+when\s+the\s+caller's\s+sentinel\s+payload\s+carried\s+one[\s\S]{0,260}never\s+changes\s+the\s+sentinel\s+bytes/,
      `${mirror}/SKILL.md's Phase 7 schema must document funding.reason as caller-sourced, not optional-and-unwired`,
    )

    // BEHAVIOURAL, not a sentence: the PRIMARY route is this pass's own run-file write, and it is
    // the only route that runs when the caller dispatches. A hardcoded `'{"provisional":false}'`
    // there drops the reason on every dispatched run while every prose pin above still passes —
    // which is exactly how the disclosure shipped inert. Assert the write BUILDS its payload from
    // the stated caller name through the verb, and that no hardcoded literal remains.
    const sentinelContract = sectionRegion(
      skill,
      '### Caller sentinel contract (when `RUN_DIR` / `RUN_ID` are supplied)',
      `${mirror}/SKILL.md`,
    )
    const built = /sentinel-payload\s+"\$\{STEP_6C_FUNDING_REASON:-\}"/
    assert.match(
      sentinelContract,
      built,
      `${mirror}/SKILL.md's caller sentinel contract must BUILD its payload from the stated funding reason — the primary route is this pass's own write`,
    )
    // The negative is anchored on the COMMAND, not on proximity. Its first form was a 400-char
    // window after the token `write`, which co-existed with a legitimate copy of the literal a
    // few lines below ("rather than writing `'{"provisional":false}'` by hand") and stayed green
    // only because "writing" is not the token "write" and no other "write" happened to fall
    // inside the window — brittle by luck, and re-wrapping the paragraph could have flipped it.
    // Scope it to the runnable fences instead: prose may name the bytes the verb prints, a
    // command may not produce them by hand.
    const contractFences = [...sentinelContract.matchAll(/```bash\n([\s\S]*?)```/g)]
      .map((m) => m[1])
      .join('\n')
    assert.ok(
      contractFences.includes('bs-run-sentinel.mjs'),
      `${mirror}/SKILL.md's caller sentinel contract must carry the runnable run-file write`,
    )
    assert.doesNotMatch(
      contractFences,
      /'\{"provisional":false\}'/,
      `${mirror}/SKILL.md's caller sentinel contract must not hand-write the payload literal in its runnable write — that is the drop`,
    )
    assertFalsifiable({
      source: contractFences,
      pattern: built,
      mutation: {
        find: /"\$\(node "\$CAPS" sentinel-payload "\$\{STEP_6C_FUNDING_REASON:-\}"\)"/,
        replacement: `'{"provisional":false}'`,
      },
      label: `${mirror}/SKILL.md: caller sentinel payload-build pin`,
    })

    // And the clock still is not a lawful cap cause. This is the negative the whole disclosure
    // exists to protect: a run may now REPORT that it was starved, which must never become a
    // fourth reason a terminal state may name for an open must-fix.
    assert.match(
      callerDeadline,
      /The\s+caller\s+deadline\s+is\s+the\s+disposition\s+for\s+the\s+skipped\s+leg,\s+not\s+for\s+an\s+open\s+must-fix/i,
      `${mirror}/SKILL.md must keep the caller deadline as a leg disposition, never a must-fix disposition`,
    )
    assert.match(
      callerDeadline,
      /exactly\s+one\s+of\s+these\s+three\s+causes/i,
      `${mirror}/SKILL.md must keep the lawful cap causes closed at three — the clock is not one of them`,
    )
  })

  // BOS-1213: this was four regexes quoting the caller-deadline block's budget numbers. The epic's
  // rule is that where a tested helper decides, the body carries the call, its inputs, its verdicts
  // and the action per verdict — and the coverage moves with the prose. So the numbers are checked
  // NUMERICALLY against the helper's own constants (a comparison, not a quote: a reworded comment
  // survives it, a changed number does not), and the decision itself is checked by RUNNING the
  // shipped CLI the body invokes.
  test(`[${mirror}] the caller-deadline constants equal the helper's, numerically`, async () => {
    const caps = await import(
      pathToFileURL(path.join(rootDir, mirror, 'toolbox/bs-review-caps.mjs')).href
    )
    const callerDeadline = sectionRegion(
      skill,
      '## Caller deadline (wall-clock cap)',
      `${mirror}/SKILL.md`,
    )
    const fence = [...callerDeadline.matchAll(/```\n([\s\S]*?)```/g)]
      .map((m) => m[1])
      .find((b) => /FIX_ROUND_SECONDS/.test(b))
    assert.ok(fence, `${mirror}/SKILL.md must price its deadline allowances in one fenced block`)

    const claim = (label, re) => {
      const m = fence.match(re)
      assert.ok(m, `${mirror}/SKILL.md's allowance block must state ${label}`)
      return Number(m[1])
    }
    assert.equal(
      claim('the fix round in minutes', /=\s*(\d+)\s+minutes/),
      caps.DEFAULT_FIX_ROUND_SECONDS / 60,
      `${mirror}: the block's per-round minute price must be the helper's round price in minutes`,
    )
    assert.equal(
      claim(
        'FIX_ROUND_SECONDS',
        /FIX_ROUND_SECONDS\s*=\s*FIX_ROUND_MINUTES\s*\*\s*60[^\n]*?=\s*(\d+)/,
      ),
      caps.DEFAULT_FIX_ROUND_SECONDS,
      `${mirror}: the gate's seconds-valued round price must be the helper's DEFAULT_FIX_ROUND_SECONDS`,
    )
    assert.equal(
      claim('MUSTFIX_OVERRUN_ROUNDS', /MUSTFIX_OVERRUN_ROUNDS\s*=\s*(\d+)/),
      caps.MUSTFIX_OVERRUN_ROUNDS,
      `${mirror}: the override allowance must be the helper's MUSTFIX_OVERRUN_ROUNDS`,
    )
    assert.equal(
      claim(
        'MUSTFIX_OVERRUN_SECONDS',
        /MUSTFIX_OVERRUN_SECONDS\s*=\s*MUSTFIX_OVERRUN_ROUNDS\s*\*\s*FIX_ROUND_SECONDS[\s\S]{0,160}?=\s*(\d+)/,
      ),
      caps.MUSTFIX_OVERRUN_SECONDS,
      `${mirror}: the reported overrun total must be the helper's MUSTFIX_OVERRUN_SECONDS`,
    )
    assert.equal(
      caps.MUSTFIX_OVERRUN_SECONDS,
      caps.MUSTFIX_OVERRUN_ROUNDS * caps.DEFAULT_FIX_ROUND_SECONDS,
      `${mirror}: the module's own MUSTFIX_OVERRUN_SECONDS must be the product it documents`,
    )
  })

  // The behaviour the deleted derivations described, proved by running the SHIPPED CLI under this
  // mirror rather than by matching a sentence. One case per member of the helper's closed reason
  // set, plus the two distinctions the body's gate turns on: `null` is "no deadline supplied" and
  // `0` is not, and the round cap is evaluated FIRST and is never overridden.
  test(`[${mirror}] every admit-fix-round verdict is produced by the shipped helper`, async () => {
    const capsPath = path.join(rootDir, mirror, 'toolbox/bs-review-caps.mjs')
    const caps = await import(pathToFileURL(capsPath).href)
    const price = caps.DEFAULT_FIX_ROUND_SECONDS
    const admit = (input) =>
      JSON.parse(
        execFileSync(process.execPath, [capsPath, 'admit-fix-round', JSON.stringify(input)], {
          encoding: 'utf8',
        }),
      )
    const open = { openMustFix: true, fixRoundSeconds: price, maxRounds: 3 }

    const cases = [
      // 1. the whole allowance remains -> ordinary admission, nothing charged to the overrun
      ['whole allowance remains', { ...open, remainingSeconds: price }, true, 'within-budget'],
      // ...and no deadline at all is no cap, never a deadline of 0
      ['no deadline supplied', { ...open, remainingSeconds: null }, true, 'within-budget'],
      // 2. below the allowance, an unattempted must-fix, override unspent -> the one overrun round
      [
        'unattempted must-fix below the allowance',
        { ...open, remainingSeconds: price - 1, unattemptedMustFix: true, overrunRoundsUsed: 0 },
        true,
        'mustfix-override',
      ],
      // 3. the same shape once the override has been spent
      [
        'override already spent',
        {
          ...open,
          remainingSeconds: price - 1,
          unattemptedMustFix: true,
          overrunRoundsUsed: caps.MUSTFIX_OVERRUN_ROUNDS,
        },
        false,
        'overrun-exhausted',
      ],
      // 3b. ...and the same shape again when the pass ITSELF caused the finding: the reserved
      //     regression round is drawn only after the general override is spent, and is bounded
      //     apart from it, so the pass can repair its own regression exactly once.
      [
        'self-inflicted must-fix, override already spent',
        {
          ...open,
          remainingSeconds: price - 1,
          unattemptedMustFix: true,
          overrunRoundsUsed: caps.MUSTFIX_OVERRUN_ROUNDS,
          selfInflictedMustFix: true,
          regressionRoundsUsed: 0,
        },
        true,
        'regression-reserved',
      ],
      // 3c. the reserve itself spent -> back to the terminal state above
      [
        'both allowances spent',
        {
          ...open,
          remainingSeconds: price - 1,
          unattemptedMustFix: true,
          overrunRoundsUsed: caps.MUSTFIX_OVERRUN_ROUNDS,
          selfInflictedMustFix: true,
          regressionRoundsUsed: caps.RESERVED_REGRESSION_ROUNDS,
        },
        false,
        'overrun-exhausted',
      ],
      // 4. below the allowance with every open must-fix already attempted
      [
        'every must-fix attempted',
        { ...open, remainingSeconds: price - 1, unattemptedMustFix: false },
        false,
        'all-attempted',
      ],
      // 5. nothing open to fix: the fixer is never dispatched on an empty must-fix list
      [
        'no open must-fix',
        { ...open, openMustFix: false, remainingSeconds: price * 10 },
        false,
        'no-open-mustfix',
      ],
      // 6. the round cap, evaluated FIRST: budget, an open must-fix and an unspent override
      //    together cannot override it
      [
        'round cap reached',
        {
          ...open,
          remainingSeconds: price * 10,
          roundsUsed: 3,
          unattemptedMustFix: true,
          overrunRoundsUsed: 0,
        },
        false,
        'round-cap',
      ],
    ]

    const reached = new Set()
    for (const [label, input, admitted, reason] of cases) {
      const got = admit(input)
      assert.deepEqual(
        got,
        { admit: admitted, reason },
        `${mirror}: ${label} must return ${JSON.stringify({ admit: admitted, reason })}`,
      )
      reached.add(got.reason)
    }
    assert.deepEqual(
      [...reached].sort(),
      [...caps.ADMIT_FIX_ROUND_REASONS].sort(),
      `${mirror}: the cases must exercise every verdict admitFixRound can return`,
    )

    // `0` is a spent budget, not an absent one — the distinction the body states and the one a
    // prose pin could never have proved. It must route through the bounded override, never to
    // free admission.
    assert.equal(
      admit({ ...open, remainingSeconds: 0, unattemptedMustFix: true, overrunRoundsUsed: 0 })
        .reason,
      'mustfix-override',
      `${mirror}: remainingSeconds 0 is a spent deadline, not an absent one`,
    )
    assert.equal(
      admit({ ...open, remainingSeconds: 0, unattemptedMustFix: false }).reason,
      'all-attempted',
      `${mirror}: remainingSeconds 0 must never reach within-budget`,
    )
  })

  // BOS-1213 Unit 4 — the drift this change exists to prevent, in BOTH directions. A verdict added
  // to the helper and not handled in the body would ship as an unhandled outcome; a verdict named
  // in the body that the helper cannot return is an action wired to nothing. Set equality over
  // substrings, not a regex over prose: it is an assertion about the verdict SET, so rewording the
  // action beside a verdict leaves it green and changing the set does not.
  test(`[${mirror}] the body handles exactly the verdict set the helper returns`, async () => {
    const caps = await import(
      pathToFileURL(path.join(rootDir, mirror, 'toolbox/bs-review-caps.mjs')).href
    )
    const callerDeadline = sectionRegion(
      skill,
      '## Caller deadline (wall-clock cap)',
      `${mirror}/SKILL.md`,
    )
    const named = [...callerDeadline.matchAll(/"reason":"([a-z-]+)"/g)].map((m) => m[1])
    assert.ok(
      named.length > 0,
      `${mirror}/SKILL.md §Caller deadline must quote the helper's verdicts verbatim`,
    )
    assert.deepEqual(
      [...new Set(named)].sort(),
      [...caps.ADMIT_FIX_ROUND_REASONS].sort(),
      `${mirror}/SKILL.md §Caller deadline must name exactly the verdicts admitFixRound can return — no more, no fewer`,
    )
    // Naming a verdict is not handling it: each must carry an action on its own line.
    for (const reason of caps.ADMIT_FIX_ROUND_REASONS) {
      const line = callerDeadline.split('\n').find((l) => l.includes(`"reason":"${reason}"`))
      assert.ok(line, `${mirror}/SKILL.md §Caller deadline must name the ${reason} verdict`)
      const action = line.split('→')[1] ?? ''
      assert.ok(
        action.trim().length > 0,
        `${mirror}/SKILL.md §Caller deadline must state what the run DOES on ${reason}`,
      )
    }
  })

  // BOS-1213 review: the derivation that defended this block was deleted with the rest, and the
  // block never had a pin of its own — so nothing red when its leading-zero strip was removed. The
  // rule says prove the BEHAVIOUR, so run the body's own fenced normalization under a real shell and
  // read `DEADLINE_LEG_SECONDS` back. `0600000` is the case that matters: a bare `$(( ))` would
  // read it in base 8 and price a 600 s leg at 197 s, floored to 300, while the dispatch it prices
  // still runs for ten minutes.
  test(`[${mirror}] the body's leg-allowance normalization prices every timeout shape`, () => {
    const callerDeadline = sectionRegion(
      skill,
      '## Caller deadline (wall-clock cap)',
      `${mirror}/SKILL.md`,
    )
    const block = [...callerDeadline.matchAll(/```bash\n([\s\S]*?)```/g)]
      .map((m) => m[1])
      .find((b) => /DEADLINE_LEG_SECONDS=\$\(\(/.test(b))
    assert.ok(
      block,
      `${mirror}/SKILL.md §Caller deadline must derive DEADLINE_LEG_SECONDS in one fenced bash block`,
    )

    const priced = (raw) =>
      execFileSync(
        '/bin/bash',
        [
          '-c',
          `${raw === null ? 'unset BOSS_SKILL_EXTENSION_TIMEOUT_MS' : ''}\n${block}\nprintf %s "$DEADLINE_LEG_SECONDS"`,
        ],
        {
          encoding: 'utf8',
          env:
            raw === null
              ? { PATH: process.env.PATH }
              : { PATH: process.env.PATH, BOSS_SKILL_EXTENSION_TIMEOUT_MS: raw },
        },
      )

    for (const [raw, want, why] of [
      [null, '300', 'an unset timeout takes the priced default'],
      ['', '300', 'an empty timeout takes the priced default'],
      ['abc', '300', 'a non-numeric timeout takes the priced default'],
      ['0x10', '300', 'a hex literal is not all digits, so it takes the priced default'],
      ['300000', '300', 'the default timeout prices the default leg'],
      ['600000', '600', 'a doubled timeout doubles the leg'],
      ['0600000', '600', 'a zero-padded timeout prices in base 10, never base 8'],
      ['0', '300', 'a zero timeout is no gate at all, so it takes the priced default'],
      ['00', '300', 'every all-zero spelling takes the priced default'],
      ['000', '300', 'every all-zero spelling takes the priced default'],
      ['08', '300', 'a sub-default timeout never lowers the floor'],
      ['900500', '901', 'a fractional second ceils, never truncates'],
    ]) {
      assert.equal(priced(raw), want, `${mirror}: ${why} (BOSS_SKILL_EXTENSION_TIMEOUT_MS=${raw})`)
    }
  })

  test(`[${mirror}] the added Phase 5/6 and severity-policy prose stays project-agnostic`, () => {
    const forbidden = /bossanova|linear|BOS-\d+/i
    assert.doesNotMatch(phase5, forbidden)
    assert.doesNotMatch(phase6, forbidden)
    assert.doesNotMatch(convergenceLoop, forbidden)
    assert.doesNotMatch(severityPolicy, forbidden)
  })
}

test('repo-local boss-review extensions restate patch for prose-class findings', () => {
  const rels = [
    '.claude/skills/boss-review-ce/SKILL.md',
    '.claude/skills/boss-review-crossmodel/SKILL.md',
    '.claude/skills/boss-review-golang/SKILL.md',
    '.claude/skills/boss-review-thermonuclear/SKILL.md',
    '.claude/skills/boss-review-tui/SKILL.md',
    '.claude/skills/boss-review-web/SKILL.md',
    '.claude/skills/thermonuclear-review/SKILL.md',
  ]
  for (const rel of rels) {
    const text = read(rel)
    assert.match(text, /"patch"/, `${rel} must name the patch field`)
    assert.match(text, /prose-class\s+findings/, `${rel} must name prose-class findings`)
    assert.match(text, /patchReason/, `${rel} must name the null-with-reason hatch`)
  }
})

// BOS-814 review P1: the repo-local `boss-review-ce` round extension keys its inline fallback on
// whether a CE review pass ACTUALLY RAN, not on whether the skill loaded. Keying it on absence
// alone fails open: `ce-code-review` can load and still perform no review — its nested reviewer
// dispatch can fail from this already-subagent-hosted round, or it can return skipped/empty
// output. Normalized into an `ok: true` envelope with empty `items[]`, that is indistinguishable
// from a clean round, so the core suppresses its own Tier 2/3 review and an unperformed pass
// silently retires the whole code-review round.
//
// Dual-mirrored: .claude is the source, .codex is regenerated by `make codex-skills`. Assert both
// so a hand-edit of one mirror, or a stale codex sync, trips the gate.
const REVIEW_CE_EXTENSION_DIRS = ['.claude/skills/boss-review-ce', '.codex/skills/boss-review-ce']

for (const skillDir of REVIEW_CE_EXTENSION_DIRS) {
  test(`[${skillDir}] the inline fallback covers dispatch failure and skipped CE, not just absence`, () => {
    const raw = read(`${skillDir}/SKILL.md`)
    const rubric = sectionRegion(raw, '## Inline fallback rubric', `${skillDir}/SKILL.md`).replace(
      /\s+/g,
      ' ',
    )

    // Each trigger is asserted on its own. A single "unavailable" match would pass on the
    // absence-only wording this test exists to forbid.
    for (const [label, pattern] of [
      ['skill unavailable on this harness', /unavailable\s+on\s+this\s+harness/i],
      [
        'nested reviewer dispatch cannot run or errors',
        /dispatch\s+cannot\s+run|dispatch\s+errors/i,
      ],
      ['CE returns skipped/empty/unparseable output', /skipped, empty, or\s+unparseable/i],
    ]) {
      assert.match(rubric, pattern, `${skillDir}/SKILL.md must route "${label}" to the inline pass`)
    }

    // And it must say why an empty envelope cannot stand in for an unperformed pass — without
    // this, a reader can still normalize a skipped CE result into a clean-looking round.
    assert.match(
      rubric,
      /suppresses\s+its\s+own\s+Tier\s+2\/3\s+review/i,
      `${skillDir}/SKILL.md must state that an empty envelope suppresses the core's own Tier 2/3 review`,
    )
  })

  // BOS-814: the fallback above makes an undispatchable CE round SAFE, not FUNCTIONAL. CE never
  // reviews inline — `ce-code-review` Stage 4 spawns one sub-agent per persona and its verification
  // gate spawns one validator per finding — so an `allowed-tools` without a dispatch primitive means
  // the CE path can never run at all. The round would degrade to the inline rubric on every single
  // invocation while every gate stayed green: a feature that is inert by construction, with
  // acceptance criterion (c) ("ce-code-review findings appear in the Boss report contract")
  // unsatisfiable in practice. Pin the primitive, not just the fallback.
  test(`[${skillDir}] frontmatter grants the dispatch primitive CE needs to spawn reviewers`, () => {
    const raw = read(`${skillDir}/SKILL.md`)
    // Scope to the frontmatter block. A whole-file match would pass on any prose mentioning Task.
    const fm = raw.match(/^---\n([\s\S]*?)\n---/)
    assert.ok(fm, `${skillDir}/SKILL.md must open with a YAML frontmatter block`)
    const allowed = fm[1].match(/^allowed-tools:\s*(.+)$/m)
    assert.ok(allowed, `${skillDir}/SKILL.md frontmatter must declare allowed-tools`)
    const tools = allowed[1].split(',').map((t) => t.trim())
    assert.ok(
      tools.includes('Task'),
      `${skillDir}/SKILL.md must grant Task in allowed-tools (got: ${tools.join(', ')}) — ce-code-review dispatches its persona reviewers as sub-agents, so without it the CE round is undispatchable and silently inert`,
    )
    assert.ok(
      tools.includes('Skill'),
      `${skillDir}/SKILL.md must keep Skill in allowed-tools — it loads compound-engineering:ce-code-review`,
    )
    // The grant is dispatch-only. Read-only must stay read-only, so the write primitives CE's
    // interactive mode would use must NOT leak in alongside it.
    for (const forbidden of ['Write', 'Edit', 'NotebookEdit']) {
      assert.ok(
        !tools.includes(forbidden),
        `${skillDir}/SKILL.md must NOT grant ${forbidden} — this round is read-only and must not mutate the worktree`,
      )
    }
    // And the reason must be written down, so a later reader does not "tidy away" the grant as
    // over-broad for a read-only round.
    // Flatten first: prettier wraps this prose at 100 columns, so any literal space in the pattern
    // is a line break waiting to happen.
    const flat = raw.replace(/\s+/g, ' ')
    assert.match(
      flat,
      /why\s+`Task`\s+is\s+in\s+this\s+extension's\s+`allowed-tools`/i,
      `${skillDir}/SKILL.md must explain why a read-only round carries Task`,
    )
    assert.match(
      flat,
      /inert\s+while\s+its\s+gates\s+stay\s+green/i,
      `${skillDir}/SKILL.md must name the inert-but-green failure mode withholding Task would cause`,
    )
  })

  // BOS-814 review P2: the extension advertises `plan:<path>` on the CE invocation, but nothing
  // hands it one — `boss-build` Step 6c invokes `boss-review` with no arguments and the core's
  // round envelope carries only mergeBase/head/changedFiles/runTmp/outPath. Without a deterministic
  // recovery rule the round silently drops CE's requirements-completeness pass on every build run,
  // while acceptance criteria were available all along. Pin the recovery rule, not just the arg.
  test(`[${skillDir}] the CE round can resolve plan:<path> without an envelope field`, () => {
    const raw = read(`${skillDir}/SKILL.md`)
    // Flatten: prettier wraps this prose at 100 columns, so a literal space in the pattern is a
    // line break waiting to happen.
    const flat = raw.replace(/\s+/g, ' ')
    for (const [label, pattern] of [
      [
        'the discovery command over the reviewed range',
        /git\s+diff\s+--name-only\s+--diff-filter=A[^`]*docs\/plans\/\*\.md/,
      ],
      ['the exactly-one match requirement', /resolves\s+to\s+\*\*exactly\s+one\*\*\s+file/i],
      ['the omit-rather-than-guess rule', /omit\s+`plan:`\s+entirely[\s\S]{0,120}never\s+guess/i],
    ]) {
      assert.match(
        flat,
        pattern,
        `${skillDir}/SKILL.md must state ${label} so the CE round recovers the plan path itself`,
      )
    }
  })
}

test('BOS-1288: the shared checklist classifies the fix shape before the per-mutant procedure', () => {
  const checklist = sectionRegion(
    read(`${REVIEW_CANONICAL}/references/falsification.md`),
    '## Shared checklist',
    `${REVIEW_CANONICAL}/references/falsification.md`,
  )
  // RULE NAME and STRUCTURAL LEAD only — the per-shape obligation lives in the helper, whose tests
  // are its specification, so nothing here restates which mutants a shape owes.
  assert.match(checklist, /bs-mutation-obligations\.mjs/)
  assert.match(checklist, /Classify\s+the\s+fix's\s+shape/)
  assert.match(checklist, /only\s+verdict\s+that\s+discharges\s+non-vacuity/)
  // The two discharge rules and the in-flight staging rule are the part an agent must have
  // resident: without them a green-expected mutant reads as optional and a probe reads as mandatory.
  assert.match(checklist, /Test-first-red\s+is\s+an\s+equal\s+discharge/)
  assert.match(checklist, /naming\s+the\s+layer\s+that\s+PARSES\s+the\s+input/)
  assert.match(checklist, /Stage\s+and\s+commit\s+nothing/)
})
