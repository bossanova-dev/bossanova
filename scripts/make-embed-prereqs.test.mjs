#!/usr/bin/env node

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// BOS-676: several binaries embed non-Go payload files via `go:embed`. Go's build
// cache keys on those file contents, but `make` does not: if the payload files are
// not prerequisites of the binary's rule, editing one leaves the linked binary
// untouched and `make build` silently ships a stale payload. This gate walks the
// live payload directories on disk and asserts every discovered file is a
// prerequisite of the target that embeds it, so a payload file added under a
// known directory (or a misspelled `find` path that expands to nothing) fails
// loudly instead of quietly dropping the edge from the build graph.
//
// That per-target walk only covers the embed sites named in `embedTargets`
// below, so a brand-new `go:embed` somewhere else would reintroduce the bug with
// this gate still green. `embedSites` closes that hole: it pins the complete set
// of `go:embed` directives in the tree, so adding a seventh site reds here until
// it is either wired into the build graph or consciously recorded as exempt.

const bossoGoMod = path.join(repoRoot, 'services/bosso/go.mod')
// services/bosso is stripped from the public mirror of this repo, so its rule
// only exists where the module does.
const hasBosso = fs.existsSync(bossoGoMod)

const isSQL = (name) => name.endsWith('.sql')

/**
 * Targets whose prerequisite list is checked against a live walk of the payload
 * on disk. `bin/bossd-plugin-claude` is deliberately absent: its embedded tree
 * (`plugins/bossd-plugin-claude/skilldata/skills`) is a generated mirror, so the
 * contract there is the `copy-skills` prerequisite asserted separately below.
 */
const embedTargets = [
  {
    target: 'bin/boss',
    // go:embed all:skills — every file type, recursively.
    payloads: [{ dir: 'services/boss/internal/skillinstall/skills', recursive: true }],
  },
  {
    target: 'bin/bossd',
    payloads: [{ dir: 'services/bossd/migrations', filter: isSQL }],
  },
  {
    target: 'bin/bosso',
    payloads: [
      { dir: 'services/bosso/migrations', filter: isSQL },
      { dir: 'services/bosso/migrations_postgres', filter: isSQL },
    ],
    requiresBosso: true,
  },
  {
    target: 'bin/bossd-plugin-opencode',
    payloads: [{ file: 'plugins/bossd-plugin-opencode/bossd-question.js' }],
  },
]

const goals = [
  ...embedTargets.filter((entry) => hasBosso || !entry.requiresBosso).map((entry) => entry.target),
  'bin/bossd-plugin-claude',
]

/**
 * Ask make to print its rule database for the given goals without running any
 * recipe. Naming the goals on the command line is what forces make to run the
 * implicit-rule search for them; without it a pattern-matched target prints as
 * `# Not a target:` with no prerequisites.
 *
 * Question mode (`-q`) exits 1 by design when a goal is out of date, so 0 and 1
 * are both success here. Status 2 is a real make error — and make still prints a
 * full rule database on stdout when it fails, so a length check alone would read
 * a broken Makefile as a healthy one.
 */
function makeDatabase() {
  const result = spawnSync('make', ['-qp', ...goals], {
    cwd: repoRoot,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  })
  assert.equal(result.error, undefined, `failed to run make: ${result.error}`)
  assert.ok(
    result.status === 0 || result.status === 1,
    `make -qp failed (status ${result.status}): ${result.stderr}`,
  )
  assert.ok(
    result.stdout && result.stdout.length > 0,
    `make -qp produced no output (stderr: ${result.stderr})`,
  )
  return result.stdout
}

const database = makeDatabase()

/**
 * Extract the expanded prerequisites of one target from the rule database. A
 * stanza line has the shape `<target>: <prereq> <prereq> ...`, optionally with
 * an order-only `| <prereq>` tail. Anchoring on the exact target plus a colon
 * matters: `bin/boss` is a prefix of `bin/bossd`, and a `bin/boss :=` variable
 * assignment must not be mistaken for a rule.
 */
function prerequisitesFor(target) {
  const stanza = new RegExp(`^${target.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}:(?!=)(.*)$`, 'm')
  const match = database.match(stanza)
  assert.ok(match, `make rule database has no stanza for ${target}`)
  const [before] = match[1].split('|')
  return new Set(before.trim().split(/\s+/).filter(Boolean))
}

/** Repo-root-relative POSIX path, the form make prints prerequisites in. */
function relativePath(absolute) {
  return path.relative(repoRoot, absolute).split(path.sep).join('/')
}

function walkPayload(payload) {
  if (payload.file) {
    const absolute = path.join(repoRoot, payload.file)
    return fs.existsSync(absolute) ? [payload.file] : []
  }
  const root = path.join(repoRoot, payload.dir)
  if (!fs.existsSync(root)) return []
  const found = []
  const stack = [root]
  while (stack.length > 0) {
    const dir = stack.pop()
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const absolute = path.join(dir, entry.name)
      if (entry.isDirectory()) {
        if (payload.recursive) stack.push(absolute)
        continue
      }
      if (!entry.isFile()) continue
      if (payload.filter && !payload.filter(entry.name)) continue
      found.push(relativePath(absolute))
    }
  }
  return found.sort()
}

function payloadFiles(entry) {
  return entry.payloads.flatMap(walkPayload).sort()
}

/**
 * The payload directories that must also be prerequisites. A file list catches
 * edits but not deletions: an unlinked payload file simply drops out of the
 * list, every survivor keeps its old mtime, and make calls the binary up to date
 * while its embed still carries the deleted file. Unlinking bumps the containing
 * directory's mtime, so the directories are what make the deletion visible.
 *
 * A payload named as a single `file` is exempt: `go:embed <one file>` fails the
 * compile when that file disappears, which is already loud.
 */
function payloadDirs(payload) {
  if (!payload.dir) return []
  const root = path.join(repoRoot, payload.dir)
  if (!fs.existsSync(root)) return []
  const found = [payload.dir]
  const stack = payload.recursive ? [root] : []
  while (stack.length > 0) {
    const dir = stack.pop()
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue
      const absolute = path.join(dir, entry.name)
      found.push(relativePath(absolute))
      stack.push(absolute)
    }
  }
  return found.sort()
}

for (const entry of embedTargets) {
  test(`${entry.target} lists every embedded payload file as a prerequisite`, (t) => {
    if (entry.requiresBosso && !hasBosso) {
      // The public mirror strips services/bosso; there is no rule to check.
      t.skip('services/bosso/go.mod is absent (public mirror)')
      return
    }

    const discovered = payloadFiles(entry)
    // An empty walk means the payload moved or the directory name is wrong —
    // exactly the silent-drift case this gate exists to catch.
    assert.ok(
      discovered.length > 0,
      `no embedded payload files found on disk for ${entry.target} (looked in ` +
        `${entry.payloads.map((p) => p.dir ?? p.file).join(', ')})`,
    )

    // Make splits prerequisites on whitespace, so a path containing any
    // whitespace — space, tab, or a newline that `$(shell find)` folds to a
    // space — cannot be expressed in the graph at all.
    const unrepresentable = discovered.filter((file) => /\s/.test(file))
    assert.deepEqual(
      unrepresentable,
      [],
      `embedded payload paths for ${entry.target} contain whitespace, which make ` +
        `cannot express as prerequisites`,
    )

    const prerequisites = prerequisitesFor(entry.target)
    const missing = discovered.filter((file) => !prerequisites.has(file))
    assert.deepEqual(
      missing,
      [],
      `${entry.target} embeds ${discovered.length} payload file(s) but ${missing.length} ` +
        `are not prerequisites of its make rule, so editing them does not relink it:\n` +
        missing.map((file) => `  ${file}`).join('\n'),
    )

    const dirs = entry.payloads.flatMap(payloadDirs).sort()
    const missingDirs = dirs.filter((dir) => !prerequisites.has(dir))
    assert.deepEqual(
      missingDirs,
      [],
      `${entry.target} does not list ${missingDirs.length} payload director(ies) as ` +
        `prerequisites, so DELETING a payload file leaves no newer prerequisite and ` +
        `make ships an embed that still contains it:\n` +
        missingDirs.map((dir) => `  ${dir}`).join('\n'),
    )
  })
}

/**
 * Every `go:embed` directive in the tree, as `<package dir> :: <patterns>`. This
 * is the inventory the per-target walks above are derived from — pinning it is
 * what makes a *new* embed site fail here rather than silently re-opening
 * BOS-676. When you add one, wire its payload into the build graph (a file list
 * on the embedding binary's make rule, or a generating prerequisite like
 * `copy-skills`), extend `embedTargets`, and only then add the line here.
 */
const embedSites = [
  'plugins/bossd-plugin-claude/skilldata :: all:skills',
  'plugins/bossd-plugin-opencode :: bossd-question.js',
  'services/boss/internal/skillinstall :: all:skills',
  'services/bossd/migrations :: *.sql',
  'services/bosso/migrations :: *.sql',
  'services/bosso/migrations_postgres :: *.sql',
]

/**
 * Scan the tracked tree for real `//go:embed` directives.
 *
 * Enumerating via `git ls-files` rather than walking the filesystem is
 * load-bearing, not a style choice (it is also what `scripts/check-go-format.mjs`
 * does): this repo keeps nested git worktrees under `.claude/worktrees/` and
 * `.worktrees/`, each a full checkout carrying its own copy of every `embed.go`.
 * A raw walk would discover them all and false-red the inventory pin below in
 * any checkout that has one — loudly blaming a nonexistent embed regression.
 * `git ls-files` lists only this checkout's tracked files, and skips submodule,
 * symlinked and unreadable-directory hazards for free.
 *
 * The match is anchored to the start of the line so that most prose mentioning
 * the directive in a doc comment is not counted (a `//go:embed` line that begins
 * a line inside a block comment would still count, but there is none today), and
 * `_test.go` files are excluded because a test fixture's embed never reaches a
 * shipped binary.
 */
function scanEmbedSites() {
  const directive = /^[ \t]*\/\/go:embed[ \t]+(.+?)[ \t]*$/gm
  const listed = spawnSync('git', ['ls-files', '-z', '--', '*.go'], {
    cwd: repoRoot,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  })
  assert.equal(listed.error, undefined, `failed to run git ls-files: ${listed.error}`)
  assert.equal(listed.status, 0, `git ls-files failed (status ${listed.status}): ${listed.stderr}`)
  const goFiles = listed.stdout.split('\0').filter(Boolean)
  assert.ok(goFiles.length > 0, 'git ls-files found no .go files, so the scan would be vacuous')

  const found = new Set()
  for (const file of goFiles) {
    if (file.endsWith('_test.go')) continue
    const source = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    if (!source.includes('//go:embed')) continue
    for (const match of source.matchAll(directive)) {
      // trimEnd() drops a trailing \r so a CRLF checkout does not diff on an
      // invisible character.
      found.add(`${path.posix.dirname(file)} :: ${match[1].trimEnd()}`)
    }
  }
  return [...found].sort()
}

test('every go:embed site in the tree is a known, build-graph-wired payload', () => {
  const discovered = scanEmbedSites()
  // The public mirror strips services/bosso, so its sites are only expected
  // where the module exists — same condition the bin/bosso rule lives under.
  const expected = embedSites.filter((site) => hasBosso || !/^services\/bosso[/ ]/.test(site))
  assert.deepEqual(
    discovered,
    expected,
    `the set of go:embed directives changed. A new embed site means a new ` +
      `non-Go payload that make will not rebuild on edit unless it is wired ` +
      `onto the embedding binary's rule (BOS-676). Wire it, extend ` +
      `embedTargets, then update embedSites.\n` +
      `  discovered: ${JSON.stringify(discovered, null, 2)}\n` +
      `  expected:   ${JSON.stringify(expected, null, 2)}`,
  )
})

test('bin/bossd-plugin-claude refreshes its embedded skill mirror before linking', () => {
  // The plugin embeds plugins/bossd-plugin-claude/skilldata/skills, which is a
  // generated mirror of the boss skill sources. Rather than pinning the mirror's
  // file list, require the phony `copy-skills` prerequisite that regenerates it
  // on every link.
  const prerequisites = prerequisitesFor('bin/bossd-plugin-claude')
  assert.ok(
    prerequisites.has('copy-skills'),
    `bin/bossd-plugin-claude must depend on copy-skills so its embedded skill ` +
      `mirror is refreshed before linking; prerequisites were: ` +
      `${[...prerequisites].join(' ')}`,
  )
})
