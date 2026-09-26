#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'

import { assertExactSize, measureFile } from './size-ratchet-lib.mjs'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')
const requiredFiles = [
  'AGENTS.md',
  'CLAUDE.md',
  'docs/testing/agent-fast-tests.md',
  '.claude/skills/agent-fast-testing/SKILL.md',
]

// CLAUDE.md is loaded into every session, so its length is a standing cost paid by every
// run. This is an EXACT pin, not a ceiling: it reds when CLAUDE.md grows AND when it shrinks,
// because a shrink-only ceiling silently converts every trim into headroom the file can regrow
// into. To move it in either direction, do so deliberately in the same commit as the change and
// record the reason on this line. BOS-636 set it at 150.
// Raised to 174 for 6371e6bf3 ("docs(skills): authorise protocol-mandated subagent dispatch
// in this repo"), which added the 24-line standing subagent-dispatch grant. That grant only
// works if it is in every session's context, so it cannot be moved behind a link. It landed
// on main without this bookkeeping because `scripts` is `branches-ignore: main` on push and
// the commit went in without a PR — the breach first surfaced on the next PR to touch a
// path in the workflow's filter. BOS-882 removes the section; drop this back to 150 then.
// Raised to 176 for BOS-783, which added two "Commands whose result lies" bullets: fish's
// glob-abort on an unquoted option value (reads as zero hits, is not zero hits) and the
// unscoped `grep -r` context blowup under `services/docs`. Both are one-line entries in an
// existing section, and both describe a command whose result lies — the section every session
// must carry for the same reason it already carries its neighbours.
// BOS-768 converted the comparison from `<=` to exact equality without moving the number: 176
// was already the measured line count, so the conversion changed the gate's reach, not its pin.
// Raised to 178 for BOS-763, which added two one-line "Commands whose result lies" bullets for
// incidental go.work.sum churn and gofmt-after-scripted-Go-edits drift.
// Raised to 177 for BOS-771, which added one "Commands whose result lies" bullet: a clean
// git rebase exit proves textual mergeability only and must be followed by post-rebase gates.
// Rebased together, those additive bullets make the measured count 179.
// Raised to 180 for BOS-989, which added one "Commands whose result lies" bullet documenting the
// run-gate host-environment failure banner and exit code.
// Raised to 181 for BOS-1200, which added one "Commands whose result lies" bullet recording the
// MEASURED harness shell: the Bash tool evaluates zsh whatever the environment block advertises,
// zsh does not word-split an unquoted parameter expansion, and no shell state survives between
// tool calls. It has to live here rather than only in docs/: every other bullet in that section
// exists because a session that did not read it burned a run, and this one is the premise the
// neighbouring pipefail and option-glob bullets are written against. The detail — the measurement
// table, the BSD `mktemp`/`cat -A` hazards, the `set -a` sourcing rule — went to
// `docs/skills/README.md` § Shell portability instead, which is why this cost one line and not ten.
// BOS-1207 moved BOTH pins in one entry rather than opening a second parallel stack: lines 181 ->
// 193 and a NEW byte pin banked at 27160, for the resident "Authoring agent guidance — keep skills
// light" section (seven one-line rules, each a negative/positive pair, plus one link to
// `docs/skills/authoring.md`, where the eight worked negatives and the per-rule enforcement
// statements live). The rules have to be resident for the same reason their neighbours are: they
// govern the next authoring decision, and an agent that has to go and read them first has already
// made it. The byte pin is the point of the ticket, not bookkeeping around it — the line pin's own
// `residual` below has always declared line LENGTH uncovered, so a width-only edit moved bytes and
// passed unchanged. Both numbers are re-measured from disk with `wc -l -c CLAUDE.md`; neither is
// derived from the other, and a future edit re-banks both. Bytes re-banked 27160 -> 27209 by the
// BOS-1207 review fix (lines unchanged at 193): the "Budgets descend" rule sent re-banking to
// `scripts/size-ratchet-lib.mjs`, which holds no pin constant at all, instead of to the
// per-artifact test file — this one — that does.
// Re-banked 193 -> 180 lines / 27209 -> 27079 bytes by the prompt audit: "Session Completion"
// was a pre-boss-finalize duplicate (2026-04-20, twelve days older than the skill that owns the
// workflow) that had drifted into teaching `git pull --rebase` over a force-pushed squash and a
// blanket stash clear against a worktree-shared stack. Replaced by a pointer at boss-finalize plus
// the two mechanics generic advice gets wrong here. Budgets descend: this is a shrink, and the
// numbers are re-measured from disk with `wc -l -c CLAUDE.md`.
// Re-banked 27079 -> 27078 bytes by BOS-1277 (lines unchanged at 180). Two bullets were WIDENED —
// the piped-status trap from `make` to any pipeline, and the `grep -r` scoping bullet to name peer
// checkouts under `.claude/worktrees/` — and the added information was paid for out of the same two
// bullets rather than out of the pin, which is what "Budgets descend" asks for: the first pass cost
// 442 bytes and was tightened back until the file came out one byte smaller than it started. The
// measurements, counter-forms and the three new gate rules live in `docs/skills/README.md`
// § "Shell portability"; only the decision an agent needs in the moment is resident here.
const CLAUDE_MD_MAX_LINES = 180
const CLAUDE_MD_MAX_BYTES = 27078

// The seven authoring rules, pinned by NAME rather than by the sentence that states them. That is
// the point of rule 4 applied to this file: a rule name is what a reader cites and what the two
// documents must agree on, while the sentence around it is expected to be rewritten. Multi-word
// phrases are joined with `\s+` so a rewrap cannot red these — see docs/skills/README.md
// § Pinning skill prose.
const AUTHORING_RULES = [
  { name: 'Helper beats prose', pattern: /Helper\s+beats\s+prose/ },
  { name: 'Invariant beats procedure', pattern: /Invariant\s+beats\s+procedure/ },
  { name: 'Budgets descend', pattern: /Budgets\s+descend/ },
  { name: 'Test behaviour, not sentences', pattern: /Test\s+behaviour,\s+not\s+sentences/ },
  {
    name: 'Incidents do not automatically become prose',
    pattern: /Incidents\s+do\s+not\s+automatically\s+become\s+prose/,
  },
  {
    name: 'The hot path is a budget, not a scratchpad',
    pattern: /The\s+hot\s+path\s+is\s+a\s+budget,\s+not\s+a\s+scratchpad/,
  },
  { name: 'Generated artefacts are generated', pattern: /Generated\s+artefacts\s+are\s+generated/ },
]

const AUTHORING_DOC = 'docs/skills/authoring.md'

function readRepoFile(relPath) {
  return fs.readFileSync(path.join(repoRoot, relPath), 'utf8')
}

// Slice one `### ` section out of a markdown body by its heading. Returns the text from the
// heading to the next heading of the same or a higher level, so an assertion about "what sits
// under this rule" cannot be satisfied by prose belonging to a neighbouring rule.
function markdownSection(text, headingPattern) {
  const lines = text.split('\n')
  const start = lines.findIndex((line) => /^#{2,4} /.test(line) && headingPattern.test(line))
  if (start === -1) return ''
  const openLevel = lines[start].match(/^(#{2,4}) /)[1].length
  let end = lines.length
  for (let i = start + 1; i < lines.length; i += 1) {
    const match = lines[i].match(/^(#{2,4}) /)
    if (match && match[1].length <= openLevel) {
      end = i
      break
    }
  }
  return lines.slice(start, end).join('\n')
}

test('agent guidance points to the generated test command manifest', () => {
  for (const file of requiredFiles) {
    const text = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    assert.match(text, /docs\/testing\/test-command-manifest\.md/, file)
    assert.match(text, /make test-smoke/, file)
    assert.match(text, /make test-affected/, file)
  }
})

test('CLAUDE.md is exactly its pinned line count', () => {
  assertExactSize({
    constFile: 'scripts/check-agent-test-guidance.test.mjs',
    constName: 'CLAUDE_MD_MAX_LINES',
    expected: CLAUDE_MD_MAX_LINES,
    label: 'CLAUDE.md',
    measured: measureFile(path.join(repoRoot, 'CLAUDE.md'), { unit: 'lines' }),
    path: 'CLAUDE.md',
    remedy: 'Move situational detail into docs/ and leave a pointer, rather than raising the pin.',
    residual:
      'line LENGTH, which this pin still cannot see — a CLAUDE.md whose lines doubled in width ' +
      'costs twice as much context and passes THIS check unchanged. BOS-1207 closed that hole ' +
      'with the sibling CLAUDE_MD_MAX_BYTES pin below rather than by widening this one, so the ' +
      'two units stay independently re-bankable. What neither covers is what the lines SAY',
    unit: 'lines',
  })
})

test('CLAUDE.md is exactly its pinned byte size', () => {
  assertExactSize({
    constFile: 'scripts/check-agent-test-guidance.test.mjs',
    constName: 'CLAUDE_MD_MAX_BYTES',
    expected: CLAUDE_MD_MAX_BYTES,
    label: 'CLAUDE.md',
    measured: measureFile(path.join(repoRoot, 'CLAUDE.md'), { unit: 'bytes' }),
    path: 'CLAUDE.md',
    remedy: 'Move situational detail into docs/ and leave a pointer, rather than raising the pin.',
    residual:
      'what the bytes BUY — this pin measures the cost of the resident body and nothing about ' +
      'its worth. 25 KB of dense checkable rules and 25 KB of restated incident narration ' +
      'measure identically here; the authoring rules in CLAUDE.md are what govern that, and no ' +
      'size pin can',
    unit: 'bytes',
  })
})

test('CLAUDE.md carries every authoring rule by name', () => {
  const claudeMd = readRepoFile('CLAUDE.md')
  for (const rule of AUTHORING_RULES) {
    assert.match(claudeMd, rule.pattern, `CLAUDE.md is missing the rule "${rule.name}"`)
  }
})

test('CLAUDE.md links the authoring reference instead of inlining it', () => {
  const claudeMd = readRepoFile('CLAUDE.md')
  assert.match(claudeMd, /docs\/skills\/authoring\.md/, 'CLAUDE.md must link the authoring doc')
  assert.ok(
    fs.existsSync(path.join(repoRoot, AUTHORING_DOC)),
    `CLAUDE.md links ${AUTHORING_DOC}, which does not exist on disk`,
  )
  // The worked examples belong to the reference, not to resident context: that split IS the
  // "hot path is a budget" rule applied to the guidance's own output. Pin the structural leads
  // the reference uses, so a later edit that pastes the examples back into CLAUDE.md reds here.
  assert.doesNotMatch(
    claudeMd,
    /\*\*Counter-form\.\*\*/,
    'the worked examples belong in the reference, not in resident context',
  )
})

test('AGENTS.md is still a symlink to CLAUDE.md, so one edit reaches both agents', () => {
  // Guards against the "sync both files" fix: two real files drift, and the byte pin above would
  // then measure only one of them.
  const stat = fs.lstatSync(path.join(repoRoot, 'AGENTS.md'))
  assert.ok(stat.isSymbolicLink(), 'AGENTS.md must be a symlink, not a copy')
  assert.equal(fs.readlinkSync(path.join(repoRoot, 'AGENTS.md')), 'CLAUDE.md')
})

test('the authoring reference states an enforcement position under every rule', () => {
  const authoring = readRepoFile(AUTHORING_DOC)
  for (const rule of AUTHORING_RULES) {
    const section = markdownSection(authoring, rule.pattern)
    assert.notEqual(section, '', `${AUTHORING_DOC} has no section for the rule "${rule.name}"`)
    // Structural lead, not prose: every rule must SAY what enforces it — naming a gate or
    // recording that none does. Silence is the failure this asserts against.
    assert.match(
      section,
      /\*\*Enforcement\.\*\*/,
      `the rule "${rule.name}" states no enforcement position`,
    )
  }
})

test('the authoring reference carries eight worked negatives, each with a counter-form', () => {
  const authoring = readRepoFile(AUTHORING_DOC)
  const count = (pattern) => (authoring.match(pattern) ?? []).length
  assert.equal(count(/\*\*Negative\.\*\*/g), 8, 'expected eight worked negative examples')
  assert.equal(count(/\*\*Counter-form\.\*\*/g), 8, 'every negative needs its counter-form')
  assert.equal(count(/\*\*Rule\.\*\*/g), 8, 'every negative names the rule it demonstrates')
  // R5: a retained number ships with the command that reproduces it, so a figure that rots is
  // settled by re-running rather than by trusting the page.
  assert.equal(count(/\*\*Measured with\.\*\*/g), 8, 'every negative names how it was measured')
})

// Companions the overview must register in BOTH places: a Contents entry alone is invisible to a
// reader scanning the normative index, and an index entry alone to one reading top-down.
for (const companion of ['authoring.md', 'sweep-migration.md']) {
  test(`docs/skills/README.md registers ${companion} in Contents and Reference index`, () => {
    const readme = readRepoFile('docs/skills/README.md')
    assert.ok(
      fs.existsSync(path.join(repoRoot, 'docs/skills', companion)),
      `docs/skills/${companion} is registered but does not exist`,
    )
    const link = new RegExp(`\\(${companion.replace('.', '\\.')}\\)`)
    for (const heading of [/^## Contents/, /^## Reference index/]) {
      const section = markdownSection(readme, heading)
      assert.notEqual(section, '', `docs/skills/README.md has no ${heading} section`)
      assert.match(section, link, `docs/skills/README.md must link ${companion} from ${heading}`)
    }
  })
}

test('the authoring rules did not leak into a published core', () => {
  // R6: everything under skillinstall/skills/ extracts into every user's GLOBAL skill directory,
  // so a repo-specific authoring section landing there would surface in thousands of unrelated
  // repos. This ticket changes no published core; assert that mechanically rather than trusting
  // the diff to have stayed put.
  const publishedRoot = path.join(repoRoot, 'services/boss/internal/skillinstall/skills')
  const bodies = fs
    .readdirSync(publishedRoot, { recursive: true })
    .filter((entry) => String(entry).endsWith('SKILL.md'))
  assert.ok(bodies.length > 0, 'found no published SKILL.md bodies — the scan path moved')
  for (const body of bodies) {
    const text = fs.readFileSync(path.join(publishedRoot, String(body)), 'utf8')
    assert.doesNotMatch(
      text,
      /docs\/skills\/authoring\.md/,
      `published core ${body} references a repo-local doc path`,
    )
  }
})
