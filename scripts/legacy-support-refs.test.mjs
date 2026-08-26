// BOS-815 — no ACTIVE file may reference the retired Gstack tooling or the Superpowers plugin.
//
// The repo carries two genuinely different kinds of mention, and a scan that cannot tell them
// apart is useless in both directions: made strict it reds on preserved history, made loose it
// stops seeing the thing it exists to catch.
//
//   executable dependency — an extension, manifest, worktree policy, ignore rule, or active doc
//     that makes a reader or a process reach for `gstack` / `superpowers` to get its job done.
//     Nothing may be in this class. Every hit outside the allowlists below is treated as one.
//
//   historical evidence — a recorded terminal capture or an archived plan. BOS-815 orders these
//     preserved byte-for-byte, so the scan must exempt them by an explicit, self-cleaning
//     declaration rather than by softening the pattern.
//
// The exemptions are declared three ways, and only three:
//
//   HISTORICAL_PREFIXES — whole archived trees. Every file under them is history.
//   ALLOWLIST kind:'historical-fixture' — a byte-exact recorded capture whose bytes are the input
//     to a parser under test. Editing one would falsify the recording, not remove a dependency.
//   RETIREMENT_REFS — the removal code itself. Deleting the legacy name from state written before
//     BOS-815 requires naming it once; the exemption is pinned to exact line text, to the exact
//     number of times that text may occur, AND to the declaration that encloses it, so it cannot
//     cover anything but the migration that earned it. See its own comment below.
//
// There is deliberately no softer class than those. An earlier revision of this gate also carried a
// 'provenance' kind for ACTIVE files that merely cited where a historical artifact lived, and an
// 'absence-ratchet' kind for other tests that named the legacy strings in order to forbid them.
// Both were removed: a citation is still a mention, and a narrower per-surface ratchet that has to
// spell out the forbidden string is strictly weaker than this tree-wide scan, which already covers
// every file those ratchets covered. Exempting a file because it "only mentions it in a comment"
// is how a removal ticket quietly becomes permanent. If a new exemption looks necessary, the
// reference itself is what should go.
//
// Every allowlist entry is also required to still match. An entry whose file stopped mentioning
// the legacy names is stale and must be deleted — that is what keeps this list from rotting into
// a permanent blanket exemption the way a hand-maintained path list normally does.
//
// `gstack` is boundary-guarded: an unanchored substring also fires on innocent prose such as
// `stringStack` (.claude/skills/golang-pro/references/generics.md), and a ratchet that reds on a
// false positive gets weakened rather than obeyed.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

import { textLines, trackedFiles } from './tree-scan-lib.mjs'

const REPO_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')

// Per-line, so a failure can name `file:line`. The alternation is parenthesised because `|` binds
// looser than the lookbehind: the guard belongs to `gstack` alone (it exists to spare `stringStack`)
// and must not read as if it applied to both names.
const LEGACY_REF = /(?:(?<![a-z])gstack)|superpowers/i

/** Archived trees. Every tracked file below one of these is historical evidence by construction. */
const HISTORICAL_PREFIXES = [
  'docs/plans/', // archived plan documents, preserved verbatim
  'docs/superpowers/', // archived write-ups; the directory name itself is the record
  'plans/docs/', // legacy plan location, same contract as docs/plans/
  'services/marketing/public/screenshots/', // asciinema .cast recordings of real sessions
]

/** Active files that may still mention the legacy names, and why. */
const ALLOWLIST = [
  {
    path: 'plugins/bossd-plugin-codex/testdata/panes/question.txt',
    kind: 'historical-fixture',
    reason: 'byte-exact capture of a real codex pane; the parser under test reads these bytes',
  },
  {
    path: 'services/bossd/internal/tmux/testdata/panes/codex_approval_menu.txt',
    kind: 'historical-fixture',
    reason: 'byte-exact capture of a real approval menu; recorded input, not a dependency',
  },
]

/**
 * Active files that name a legacy string *only in order to delete it* from state written before
 * BOS-815 — a migration, not a dependency. bossd's $GIT_COMMON_DIR/info/exclude is only ever
 * appended to, so dropping a pattern from the managed list stops new repos from getting it while
 * every already-managed repo keeps honouring it forever; finishing the removal means naming the
 * retired string once, in the code that deletes it.
 *
 * This is deliberately NOT the third soft class the header rules out. It is not a whole-file
 * exemption: `lines` pins the exact trimmed text of every line in the file that may match, and the
 * scan treats anything else in that file as a violation. A new reference added to one of these
 * files still reds; deleting the migration makes the pinned lines stale and also reds. The
 * exemption therefore cannot outlive the code that earned it.
 *
 * `count` is what binds a pin to the OCCURRENCE that earned it rather than to the text. Line text
 * alone is not identifying: the retired pattern's own source line, `".superpowers/",`, is
 * byte-identical to what re-adding it to the ACTIVE bossdManagedExcludePatterns list would produce,
 * so a text-only exemption would cover the regression it exists to catch. Pinning the exact number
 * of occurrences means the N+1th identical line is reported instead of inheriting the exemption.
 * Raising a `count` is then an explicit, reviewable claim that another line exists solely to delete
 * the legacy name — which is exactly the decision that should not be made silently.
 *
 * `within` is what binds a pin to the CONSTRUCT that earned it rather than to the file. A count
 * catches an added copy, but it cannot see a RELOCATION or a SUBSTITUTION: moving the sole
 * `".superpowers/",` line out of the dead retiredGitInfoExcludePatterns list and into the live
 * bossdManagedExcludePatterns list leaves the text and the count identical while active support has
 * genuinely come back, so a (file, text, count) pin would follow the line into load-bearing code and
 * stay green. `within` names the declaration the exemption was granted to — `opens` is the exact RAW
 * text of the line that starts it and `closes` the exact raw text of the line that ends it — and an
 * occurrence outside that range is reported however many times it occurs.
 *
 * A line NUMBER was rejected as the anchor: it rots on every unrelated edit above the pin, and a
 * gate that reds for reasons unrelated to what it guards gets "fixed" by bumping the number, which
 * is how this file's header describes an exemption quietly becoming permanent. A count only moves
 * when the thing being counted moves. `within` is located by searching for its text, never by
 * position, for exactly the same reason: inserting an unrelated line above the pinned declaration
 * shifts the whole range with it and changes nothing. What it cannot survive is the declaration
 * being renamed, deleted, or duplicated — which is the pin going stale, and is meant to red.
 */
const RETIREMENT_REFS = [
  {
    path: 'services/bossd/internal/git/worktree.go',
    reason:
      'retiredGitInfoExcludePatterns — the entry ensureGitInfoExclude deletes from info/exclude ' +
      'for repos bossd managed before BOS-815',
    within: { opens: 'var retiredGitInfoExcludePatterns = []string{', closes: '}' },
    lines: [{ text: '".superpowers/",', count: 1 }],
  },
  {
    path: 'services/bossd/internal/git/worktree_test.go',
    reason:
      'proves the deletion above actually happens, and that look-alike user-authored lines survive it',
    within: {
      opens: 'func TestEnsureGitInfoExclude_PurgesRetiredPatterns(t *testing.T) {',
      closes: '}',
    },
    lines: [
      { text: 'const retired = ".superpowers/"', count: 1 },
      { text: 'const lookAlike = "my-superpowers-notes/"', count: 1 },
      {
        text: 'const comment = "# .superpowers/ is a user note, not a bossd-written pattern"',
        count: 1,
      },
    ],
  },
]

/** This file necessarily contains every string it forbids. */
const SELF = 'scripts/legacy-support-refs.test.mjs'

function isHistorical(path) {
  return HISTORICAL_PREFIXES.some((prefix) => path.startsWith(prefix))
}

function legacyHits(path, root = REPO_ROOT) {
  const lines = textLines(path, root)
  if (lines === null) return []
  const hits = []
  lines.forEach((line, i) => {
    if (LEGACY_REF.test(line)) hits.push([i + 1, line.trim()])
  })
  return hits
}

/**
 * The 1-based half-open line range `[start, end)` strictly inside the construct a retirement pin is
 * anchored to, or `{ error }` naming why the anchor no longer resolves.
 *
 * Located by exact RAW line text, never by position: `opens` must occur exactly once in the file
 * (twice is ambiguous, zero means the declaration was renamed or deleted — both are the pin going
 * stale), and the range ends at the first raw `closes` line after it. Matching the raw line rather
 * than a trimmed one is what makes `closes: '}'` name the end of a top-level Go declaration instead
 * of the first nested brace inside it.
 */
function pinnedScope(path, { opens, closes }, root = REPO_ROOT) {
  const lines = textLines(path, root)
  if (lines === null) return { error: `${path} could not be read as text` }
  const found = lines.filter((line) => line === opens).length
  if (found !== 1) {
    return {
      error:
        `${path} contains the anchor line ${JSON.stringify(opens)} ${found} time(s); a retirement ` +
        `pin must name exactly one declaration`,
    }
  }
  const open = lines.indexOf(opens)
  const close = lines.findIndex((line, i) => i > open && line === closes)
  if (close === -1) {
    return {
      error:
        `${path}: the anchor ${JSON.stringify(opens)} is never closed by ` +
        `${JSON.stringify(closes)}`,
    }
  }
  return { start: open + 2, end: close + 1 }
}

function inScope(scope, line) {
  return line >= scope.start && line < scope.end
}

test('BOS-815: no active file requires Gstack or Superpowers', () => {
  const files = trackedFiles(REPO_ROOT)
  // Guard the guard: a broken enumeration would make every assertion below vacuous.
  assert.ok(files.length >= 500, `expected the whole tracked tree, got ${files.length} files`)
  assert.ok(files.includes('.gitignore'), 'enumeration must reach dotfiles at the repo root')
  assert.ok(
    files.includes(SELF),
    'enumeration must reach this test file, or the self-exclusion below is untested',
  )

  const exempt = new Set(ALLOWLIST.map((entry) => entry.path))
  const pinned = new Map(RETIREMENT_REFS.map((entry) => [entry.path, entry]))
  const violations = []

  for (const path of files) {
    if (path === SELF || isHistorical(path) || exempt.has(path)) continue
    // A dependency can be recorded entirely in the PATHNAME — `support/superpowers-6.0.3/README.md`
    // is a vendored copy of the thing BOS-815 removed even if its bytes never repeat the name — and
    // the content scan below would never see it. Same exemptions, so an archived tree or an
    // allowlisted fixture is not reported twice.
    if (LEGACY_REF.test(path)) {
      violations.push({
        id: `${path}:path`,
        detail:
          `${path}\n    the tracked PATH matches the legacy pattern (its contents were not the ` +
          `problem). Rename or delete it.`,
      })
    }
    const entry = pinned.get(path)
    const allowed = entry && new Map(entry.lines.map(({ text, count }) => [text, count]))
    // Resolved once per file: the declaration the whole entry's exemption was granted to.
    const scope = entry && pinnedScope(path, entry.within)
    const hits = legacyHits(path)
    // Occurrences, not text, are what a retirement pin exempts. A pinned text is only skipped
    // while it occurs exactly as many times as the pin declares; an extra copy — which is
    // byte-identical to the original, so no per-line rule could tell them apart — takes the whole
    // text back out of the exemption and reports every one of its occurrences.
    const occurrences = new Map()
    for (const [, text] of hits) occurrences.set(text, (occurrences.get(text) ?? 0) + 1)

    for (const [line, text] of hits) {
      const expected = allowed?.get(text)
      const found = occurrences.get(text)
      let why
      if (expected === undefined) {
        why = allowed
          ? `${path} pins its retirement lines in RETIREMENT_REFS and this is not one of them.`
          : `${path} is not exempt: the file CONTENT matches the legacy pattern.`
      } else if (scope.error) {
        why =
          `${path} pins ${JSON.stringify(text)} to the declaration opening ` +
          `${JSON.stringify(entry.within.opens)}, but that anchor no longer resolves: ${scope.error}`
      } else if (expected !== found) {
        why =
          `${path} pins ${JSON.stringify(text)} in RETIREMENT_REFS for exactly ${expected} ` +
          `occurrence(s), but it appears ${found} times. Every occurrence is reported because ` +
          `they are byte-identical: nothing in the text says which one is the migration.`
      } else if (!inScope(scope, line)) {
        why =
          `${path} pins ${JSON.stringify(text)} inside ${JSON.stringify(entry.within.opens)} ` +
          `(lines ${scope.start}-${scope.end - 1}), but this occurrence is outside it. The ` +
          `exemption belongs to that declaration, not to the file: moving the line into live code ` +
          `leaves its text and its count identical, so the enclosing construct is the only thing ` +
          `that still says it is the migration.`
      } else {
        continue
      }
      violations.push({ id: `${path}:${line}`, detail: `${path}:${line}: ${text}\n    ${why}` })
    }
  }

  assert.deepEqual(
    violations.map((v) => v.id),
    [],
    violations
      .map(
        ({ detail }) =>
          `${detail}\n    ` +
          `Remove the reference. Only add it to ALLOWLIST in ${SELF} if the file is genuinely a ` +
          `'historical-fixture' — a byte-exact recorded capture whose bytes are parsed by a test — ` +
          `or to RETIREMENT_REFS if the line exists solely to delete the legacy name from state ` +
          `written before BOS-815. Never to keep an active file, doc, comment, or narrower ratchet ` +
          `green.`,
      )
      .join('\n'),
  )
})

test('BOS-815: a tracked symlink is scanned as its target PATH, not the target file', (t) => {
  const root = mkdtempSync(join(tmpdir(), 'bos815-symlink-'))
  t.after(() => rmSync(root, { recursive: true, force: true }))

  // The shape the tree-wide scan has to catch: an active link reaching into a retired plugin
  // directory. The file it points at is deliberately clean, so following the link — what the
  // scanner did before — sees nothing to report even though the dependency is right there in the
  // link text. `AGENTS.md -> CLAUDE.md` is the same construct, already tracked in this repo.
  const target = join('legacy-superpowers-plugin', 'notes.md')
  mkdirSync(join(root, 'legacy-superpowers-plugin'))
  writeFileSync(join(root, target), 'this file itself names nothing retired\n')
  symlinkSync(target, join(root, 'link.md'))

  assert.deepEqual(
    legacyHits('link.md', root),
    [[1, target]],
    'the link text stored in Git must be scanned, not the bytes of the file it resolves to',
  )
  // Control on the same tree: the target read on its own turn is clean, so the hit above can only
  // have come from the link text — following the link would have reported nothing at all.
  assert.deepEqual(legacyHits(target, root), [], 'the target file is clean by construction')

  // A regular file must still be read by content, not mistaken for a link.
  writeFileSync(join(root, 'plain.md'), 'first\nneeds superpowers to build\n')
  assert.deepEqual(legacyHits('plain.md', root), [[2, 'needs superpowers to build']])
})

test('BOS-815: every retirement pin is still load-bearing', () => {
  const tracked = new Set(trackedFiles(REPO_ROOT))
  const seenPaths = new Set()
  for (const entry of RETIREMENT_REFS) {
    assert.ok(tracked.has(entry.path), `${entry.path} is pinned but no longer tracked`)
    // The scan looks entries up by path, so a second entry for the same file would silently
    // replace the first — including its `within` anchor.
    assert.ok(!seenPaths.has(entry.path), `${entry.path} appears twice in RETIREMENT_REFS`)
    seenPaths.add(entry.path)
    // A pin whose anchor no longer resolves is stale in exactly the way a wrong `count` is: it
    // stops describing the construct the exemption was granted to. Asserting it here means the
    // anchor cannot quietly drift into naming nothing (or naming two things) while the scan's
    // pinned lines happen to sit wherever they now are.
    const scope = pinnedScope(entry.path, entry.within)
    assert.equal(
      scope.error,
      undefined,
      `${entry.path}: the RETIREMENT_REFS anchor is stale — ${scope.error}`,
    )
    const hits = legacyHits(entry.path)
    const texts = hits.map(([, text]) => text)
    const scoped = hits.filter(([line]) => inScope(scope, line)).map(([, text]) => text)
    for (const { text, count } of entry.lines) {
      // Guard the pin itself: a zero or absent count would exempt every occurrence of the text,
      // which is the over-broad shape this pin exists to prevent.
      assert.ok(
        Number.isInteger(count) && count >= 1,
        `${entry.path}: pinned line ${JSON.stringify(text)} declares count ${count}; a retirement ` +
          `pin must name a positive whole number of occurrences`,
      )
      // A pin that matches nothing is a hole: it would silently keep covering the file after the
      // migration it describes is deleted or reworded. A pin that matches MORE lines than it
      // declares is the same hole in the other direction — it would extend the exemption to an
      // identical line that never earned it (e.g. the retired pattern re-added to an active list).
      assert.equal(
        texts.filter((seen) => seen === text).length,
        count,
        `${entry.path} contains the pinned line ${JSON.stringify(text)} a different number of ` +
          `times than the ${count} its RETIREMENT_REFS entry declares — update or delete the entry`,
      )
      // Same check against the anchored construct. A relocation keeps the file-wide count above
      // intact — that is the hole this closes — so the declaration has to be asked directly
      // whether it still holds the occurrences the pin claims for it.
      assert.equal(
        scoped.filter((seen) => seen === text).length,
        count,
        `${entry.path} declares ${count} occurrence(s) of the pinned line ${JSON.stringify(text)} ` +
          `inside ${JSON.stringify(entry.within.opens)}, but that declaration holds a different ` +
          `number — the line moved out of the construct its exemption was granted to`,
      )
      assert.ok(
        LEGACY_REF.test(text),
        `${entry.path}: pinned line ${JSON.stringify(text)} does not even match the legacy ` +
          `pattern, so pinning it exempts nothing`,
      )
    }
  }
})

test('BOS-815: every allowlist entry is still load-bearing', () => {
  const tracked = new Set(trackedFiles(REPO_ROOT))
  for (const entry of ALLOWLIST) {
    assert.ok(tracked.has(entry.path), `${entry.path} is allowlisted but no longer tracked`)
    assert.ok(
      legacyHits(entry.path).length > 0,
      `${entry.path} no longer mentions the legacy names — delete its ALLOWLIST entry rather than ` +
        `leaving a blanket exemption behind`,
    )
    // A whole-file exemption is only ever defensible for a recording. Widening this set is how the
    // gate would be talked back down to the per-surface ratchets BOS-815 replaced, so the kind is
    // asserted rather than merely documented.
    assert.equal(
      entry.kind,
      'historical-fixture',
      `${entry.path}: 'historical-fixture' is the only allowlist kind — an active file that wants ` +
        `an exemption should lose the reference instead`,
    )
  }
})

test('BOS-815: historical prefixes are non-empty and really historical', () => {
  const files = trackedFiles(REPO_ROOT)
  for (const prefix of HISTORICAL_PREFIXES) {
    const under = files.filter((path) => path.startsWith(prefix))
    assert.ok(under.length > 0, `${prefix} is exempted but matches no tracked file — delete it`)
    assert.ok(
      under.some((path) => legacyHits(path).length > 0),
      `${prefix} is exempted but nothing under it mentions the legacy names — delete it`,
    )
  }
})
