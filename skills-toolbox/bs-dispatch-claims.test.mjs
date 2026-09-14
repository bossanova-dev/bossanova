import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, mkdirSync, readFileSync, symlinkSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { CLAIM_KINDS, CLAIM_VERDICTS, verifyDispatchClaims } from './bs-dispatch-claims.mjs'

const scriptPath = fileURLToPath(new URL('./bs-dispatch-claims.mjs', import.meta.url))

const TIP_TREE = '4b825dc642cb6eb9a060e54bf8d69288fbee4904'
const OTHER_TREE = 'aa11bb22cc33dd44ee55ff6677889900aabbccdd'
const FULL_SHA = '0123456789abcdef0123456789abcdef01234567'

/**
 * An injected git runner. The helper must never shell out during these tests —
 * every git answer here is a table lookup, so a regression that reaches the real
 * binary shows up as an unexpected-args failure rather than as a silent pass in
 * whatever repository happens to host the run.
 */
function gitFake({ objects = {}, tipTree = TIP_TREE, ambiguous = [] } = {}) {
  return (args) => {
    const spec = args[args.length - 1]
    const quiet = args.includes('--quiet')
    if (spec === 'HEAD^{tree}') {
      return tipTree
        ? { ok: true, stdout: `${tipTree}\n`, stderr: '' }
        : { ok: false, stdout: '', stderr: 'fatal: not a git repository' }
    }
    const object = /^(.+)\^\{object\}$/.exec(spec)
    if (object) {
      // MEASURED against the real binary in this repository. `--quiet` suppresses the
      // AMBIGUITY diagnostic exactly as it suppresses the not-found one, so an ambiguous
      // prefix and a nonexistent SHA are byte-identical there — exit 1, empty stdout, EMPTY
      // stderr. Only the re-ask without `--quiet` separates them, and it separates them by
      // git's own word: `error: short object ID <prefix> is ambiguous` (exit 128) against a
      // bare `fatal: Needed a single revision` (exit 128) for the one that simply is not
      // there. A fake that let `--quiet` leak the diagnostic would make this test pass
      // against a module that never re-asks.
      if (ambiguous.includes(object[1])) {
        return quiet
          ? { ok: false, stdout: '', stderr: '' }
          : {
              ok: false,
              stdout: '',
              stderr:
                `error: short object ID ${object[1]} is ambiguous\n` +
                'hint: The candidates are:\nfatal: Needed a single revision\n',
            }
      }
      const full = objects[object[1]]
      if (!full && !quiet) {
        return { ok: false, stdout: '', stderr: 'fatal: Needed a single revision\n' }
      }
      // MEASURED against the real binary: `rev-parse --verify --quiet <rev>^{object}`
      // exits 1 with EMPTY stderr for a revision this repository does not have, and
      // 128 with `fatal: not a git repository` when there is no repository to ask.
      // A fake that writes a diagnostic on the first shape would make the helper read
      // an ordinary "no such object" as "git could not answer".
      return full
        ? { ok: true, stdout: `${full}\n`, stderr: '' }
        : { ok: false, stdout: '', stderr: '' }
    }
    return { ok: false, stdout: '', stderr: `unexpected git args: ${args.join(' ')}` }
  }
}

function withRepo(run) {
  const root = mkdtempSync(join(tmpdir(), 'bs-dispatch-claims-'))
  try {
    return run(root)
  } finally {
    rmSync(root, { recursive: true, force: true })
  }
}

function writeRepoFile(root, file, contents) {
  const path = join(root, file)
  mkdirSync(join(path, '..'), { recursive: true })
  writeFileSync(path, contents)
}

function runCli(args = [], opts = {}) {
  const res = spawnSync(process.execPath, [scriptPath, ...args], { encoding: 'utf8', ...opts })
  return { stdout: res.stdout.trim(), stderr: res.stderr.trim(), status: res.status }
}

// ---------------------------------------------------------------------------
// Shape: one record per claim, drawn from the closed verdict vocabulary.
// ---------------------------------------------------------------------------

test('every input claim yields exactly one four-field record with a closed verdict', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/real.js', 'one\ntwo\nthree\n')
    const claims = [
      { kind: 'path', claim: 'src/real.js:2' },
      { kind: 'path', claim: 'src/gone.js' },
      { kind: 'git-object', claim: FULL_SHA },
      { kind: 'tree', claim: TIP_TREE },
      { kind: 'not-a-kind', claim: 'whatever' },
    ]
    const records = verifyDispatchClaims(claims, { repoRoot, git: gitFake() })
    assert.equal(records.length, claims.length)
    for (const [index, record] of records.entries()) {
      assert.deepEqual(Object.keys(record).sort(), ['claim', 'kind', 'reason', 'verdict'])
      assert.ok(CLAIM_VERDICTS.includes(record.verdict), `unknown verdict: ${record.verdict}`)
      assert.equal(record.claim, claims[index].claim)
      assert.equal(record.kind, claims[index].kind)
      assert.equal(typeof record.reason, 'string')
      assert.notEqual(record.reason, '')
    }
  })
})

test('the exported vocabularies are the three kinds and the three verdicts', () => {
  assert.deepEqual([...CLAIM_KINDS].sort(), ['git-object', 'path', 'tree'])
  assert.deepEqual([...CLAIM_VERDICTS].sort(), ['refuted', 'unverifiable', 'verified'])
})

test('a claim list that is not an array adjudicates nothing rather than throwing', () => {
  assert.deepEqual(verifyDispatchClaims(null, { git: gitFake() }), [])
  assert.deepEqual(verifyDispatchClaims('src/real.js', { git: gitFake() }), [])
})

// ---------------------------------------------------------------------------
// kind: path
// ---------------------------------------------------------------------------

test('a path that resolves, with a line the file actually has, is verified', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/real.js', 'one\ntwo\nthree\n')
    const [bare, located] = verifyDispatchClaims(
      [
        { kind: 'path', claim: 'src/real.js' },
        { kind: 'path', claim: 'src/real.js:3' },
      ],
      { repoRoot, git: gitFake() },
    )
    assert.equal(bare.verdict, 'verified')
    assert.equal(located.verdict, 'verified')
  })
})

test('BOS-1096: a fabricated directory under a line number that exists elsewhere is refuted', () => {
  withRepo((repoRoot) => {
    // The real file, at the very line the claim cites. The claim names the same
    // basename under a directory that does not exist — the shape a triage
    // subagent produced when it returned correct line numbers under invented
    // paths. Resolution must decide the CLAIM's path, never a same-named file.
    writeRepoFile(repoRoot, 'services/app/handler.go', 'a\nb\nc\nd\ne\n')
    const [record] = verifyDispatchClaims(
      [{ kind: 'path', claim: 'services/invented/handler.go:4' }],
      { repoRoot, git: gitFake() },
    )
    assert.equal(record.verdict, 'refuted')
    assert.match(record.reason, /services\/invented\/handler\.go/)
  })
})

test('a line beyond the end of a file that does exist is refuted and names the line count', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/short.js', 'one\ntwo\n')
    const [record] = verifyDispatchClaims([{ kind: 'path', claim: 'src/short.js:99' }], {
      repoRoot,
      git: gitFake(),
    })
    assert.equal(record.verdict, 'refuted')
    assert.match(record.reason, /3 line/)
  })
})

test('a path escaping the repo root is unverifiable, never verified and never refuted', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/real.js', 'one\n')
    const [escaping, absolute] = verifyDispatchClaims(
      [
        { kind: 'path', claim: '../outside/real.js:1' },
        { kind: 'path', claim: '/etc/passwd' },
      ],
      { repoRoot, git: gitFake() },
    )
    assert.equal(escaping.verdict, 'unverifiable')
    assert.equal(absolute.verdict, 'unverifiable')
  })
})

test('with a MISSING repo root every path claim is unverifiable', () => {
  const [record] = verifyDispatchClaims([{ kind: 'path', claim: 'src/real.js' }], {
    repoRoot: null,
    git: gitFake(),
  })
  assert.equal(record.verdict, 'unverifiable')
})

test('with a repo root that is present but UNREADABLE every path claim is unverifiable', () => {
  withRepo((repoRoot) => {
    // The sibling above covers a root that is absent from the options. This is the other
    // half of the same rule, and the half that failed: a root string that is PRESENT but
    // names nothing readable sailed past the missing/blank guard, so every path beneath it
    // resolved as absent and EVERY claim — true or not — was struck `refuted`. A failed
    // read is a failure of this CHECK, never evidence the claim is false.
    writeRepoFile(repoRoot, 'src/real.js', 'one\ntwo\n')
    const claims = [
      { kind: 'path', claim: 'src/real.js:1' },
      { kind: 'path', claim: 'anything.md:1' },
    ]
    const absentRoot = verifyDispatchClaims(claims, {
      repoRoot: join(repoRoot, 'no-such-root'),
      git: gitFake(),
    })
    // A regular file is emphatically present, and is still no root to resolve against.
    const fileRoot = verifyDispatchClaims(claims, {
      repoRoot: join(repoRoot, 'src/real.js'),
      git: gitFake(),
    })
    assert.equal(absentRoot.length + fileRoot.length, 4)
    for (const record of [...absentRoot, ...fileRoot]) {
      assert.equal(record.verdict, 'unverifiable', record.reason)
      assert.notEqual(record.verdict, 'refuted', record.reason)
    }
  })
})

test('a line number below one is a coordinate no file can have and never verifies', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/real.js', 'one\ntwo\nthree\n')
    // `:0` is the shape that used to pass: the resolver only asked whether the line
    // was PAST the end of the file, so any file with at least zero lines satisfied it
    // and the record read "resolves and has at least 0 line(s)". The two neighbours are
    // controls — the corrected rule must not swallow a real line or spare a missing one.
    const [zero, negative, first, beyond] = verifyDispatchClaims(
      [
        { kind: 'path', claim: 'src/real.js:0' },
        { kind: 'path', file: 'src/real.js', line: -3 },
        { kind: 'path', claim: 'src/real.js:1' },
        { kind: 'path', claim: 'src/real.js:99999' },
      ],
      { repoRoot, git: gitFake() },
    )
    assert.equal(zero.verdict, 'refuted')
    assert.equal(negative.verdict, 'refuted')
    assert.equal(first.verdict, 'verified')
    assert.equal(beyond.verdict, 'refuted')
  })
})

test('a symlink whose target leaves the repo root is unverifiable, not an in-repo coordinate', () => {
  withRepo((repoRoot) => {
    const outside = mkdtempSync(join(tmpdir(), 'bs-dispatch-claims-outside-'))
    try {
      // Lexical confinement alone is not confinement: `readFileSync` follows the link,
      // so this claim used to resolve, read, and be reported as an in-repo coordinate.
      writeFileSync(join(outside, 'secret.txt'), 'one\ntwo\n')
      symlinkSync(join(outside, 'secret.txt'), join(repoRoot, 'escape-link.txt'))
      // The control: a link that stays INSIDE the root is an ordinary coordinate and
      // must still verify, or the fix has simply banned symlinks.
      writeRepoFile(repoRoot, 'src/real.js', 'one\ntwo\n')
      symlinkSync(join(repoRoot, 'src/real.js'), join(repoRoot, 'inside-link.js'))
      const [escaping, inside] = verifyDispatchClaims(
        [
          { kind: 'path', claim: 'escape-link.txt:1' },
          { kind: 'path', claim: 'inside-link.js:1' },
        ],
        { repoRoot, git: gitFake() },
      )
      assert.equal(escaping.verdict, 'unverifiable')
      assert.notEqual(escaping.verdict, 'verified')
      assert.equal(inside.verdict, 'verified')
    } finally {
      rmSync(outside, { recursive: true, force: true })
    }
  })
})

test('a real directory is not a falsehood, and an unreadable path is not a refutation', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/real.js', 'one\n')
    // A self-referential link: the read fails with ELOOP rather than ENOENT, so the
    // path is one this check cannot decide — an I/O failure is never a refutation.
    symlinkSync(join(repoRoot, 'loop'), join(repoRoot, 'loop'))
    const [directory, directoryLine, loop] = verifyDispatchClaims(
      [
        { kind: 'path', claim: 'src' },
        { kind: 'path', claim: 'src:3' },
        { kind: 'path', claim: 'loop' },
      ],
      { repoRoot, git: gitFake() },
    )
    // The directory is really there: branding it refuted is the opposite error from
    // verifying line 0, and just as wrong.
    assert.notEqual(directory.verdict, 'refuted')
    assert.equal(directory.verdict, 'verified')
    // A directory has no lines, so a LINE on one is still false.
    assert.equal(directoryLine.verdict, 'refuted')
    assert.equal(loop.verdict, 'unverifiable')
  })
})

test('a structured file/line claim is adjudicated without a lossy string round trip', () => {
  withRepo((repoRoot) => {
    // A caller that already holds the coordinate (the findings validator does) passes
    // it structured. The file's own name ends in `:2`, so formatting it into
    // "<file>:<line>" and re-splitting on the trailing group would resolve the wrong
    // path entirely.
    writeRepoFile(repoRoot, 'src/odd:2.js', 'one\ntwo\nthree\n')
    const [structured] = verifyDispatchClaims([{ kind: 'path', file: 'src/odd:2.js', line: 3 }], {
      repoRoot,
      git: gitFake(),
    })
    assert.equal(structured.verdict, 'verified')
    assert.equal(structured.claim, 'src/odd:2.js:3')
  })
})

// ---------------------------------------------------------------------------
// kind: git-object
// ---------------------------------------------------------------------------

test('BOS-1199: a full SHA naming no object is refuted, and a short SHA that resolves is verified', () => {
  withRepo((repoRoot) => {
    const git = gitFake({ objects: { '0123456': FULL_SHA } })
    const [invented, short] = verifyDispatchClaims(
      [
        { kind: 'git-object', claim: FULL_SHA },
        { kind: 'git-object', claim: '0123456' },
      ],
      { repoRoot, git },
    )
    assert.equal(invented.verdict, 'refuted')
    assert.equal(short.verdict, 'verified')
    // "carrying the full object name": the short SHA's record must say which
    // object it resolved to, or a reader cannot tell a real prefix from a
    // fabricated tail that happens to share one.
    assert.match(short.reason, new RegExp(FULL_SHA))
  })
})

test('a claim that is not an object name at all is refuted without consulting git', () => {
  let called = false
  const git = () => {
    called = true
    return { ok: true, stdout: `${FULL_SHA}\n`, stderr: '' }
  }
  const [record] = verifyDispatchClaims([{ kind: 'git-object', claim: 'not-a-sha' }], {
    repoRoot: process.cwd(),
    git,
  })
  assert.equal(record.verdict, 'refuted')
  assert.equal(called, false, 'a malformed object name must not be handed to git')
})

test('absent git never refutes an object claim — only git answering "no" does', () => {
  // MEASURED: outside a repository the same command exits 128 with
  // `fatal: not a git repository (or any of the parent directories): .git`, where an
  // object this repository simply does not have exits 1 with EMPTY stderr. Both used
  // to land on `refuted`, so a worktree with no git decided every claim false.
  const noRepository = () => ({
    ok: false,
    stdout: '',
    stderr: 'fatal: not a git repository (or any of the parent directories): .git\n',
  })
  const [record] = verifyDispatchClaims([{ kind: 'git-object', claim: FULL_SHA }], {
    repoRoot: process.cwd(),
    git: noRepository,
  })
  assert.equal(record.verdict, 'unverifiable')
  assert.notEqual(record.verdict, 'refuted')
})

// ---------------------------------------------------------------------------
// kind: tree
// ---------------------------------------------------------------------------

test('BOS-1218: a tree equal to HEAD^{tree} is verified and any other tree is refuted as stale', () => {
  withRepo((repoRoot) => {
    const git = gitFake({ tipTree: TIP_TREE })
    const [tip, stale] = verifyDispatchClaims(
      [
        { kind: 'tree', claim: TIP_TREE },
        { kind: 'tree', claim: OTHER_TREE },
      ],
      { repoRoot, git },
    )
    assert.equal(tip.verdict, 'verified')
    assert.equal(stale.verdict, 'refuted')
    assert.match(stale.reason, /stale/)
    assert.match(stale.reason, new RegExp(TIP_TREE))
  })
})

test('BOS-1199: an abbreviated tree name never verifies as the tree the gates ran on', () => {
  withRepo((repoRoot) => {
    const git = gitFake({ tipTree: TIP_TREE })
    // "the tree the gates ran on" is an identity claim, and a prefix is satisfied by
    // every object sharing it. Hand-rolled `tip.startsWith(claim)` matching verified a
    // FOUR-hex prefix as the tip, which is precisely the evidence this check refuses.
    const records = verifyDispatchClaims(
      [
        { kind: 'tree', claim: TIP_TREE.slice(0, 4) },
        { kind: 'tree', claim: TIP_TREE.slice(0, 12) },
        { kind: 'tree', claim: TIP_TREE.slice(0, 39) },
      ],
      { repoRoot, git },
    )
    for (const record of records) {
      assert.equal(record.verdict, 'unverifiable', record.reason)
      assert.notEqual(record.verdict, 'verified')
    }
  })
})

test('a tree claim that is not an object name is refuted without consulting git', () => {
  // The shape check runs BEFORE git, matching the git-object sibling: an arbitrary
  // claim string is never handed to the process runner as a revision argument.
  let called = false
  const git = () => {
    called = true
    return { ok: true, stdout: `${TIP_TREE}\n`, stderr: '' }
  }
  const [record] = verifyDispatchClaims([{ kind: 'tree', claim: 'not-a-tree' }], {
    repoRoot: process.cwd(),
    git,
  })
  assert.equal(record.verdict, 'refuted')
  assert.equal(called, false, 'a malformed tree name must not be handed to git')
})

// ---------------------------------------------------------------------------
// Fail closed: no git, unknown kinds, malformed entries.
// ---------------------------------------------------------------------------

test('with no git runner every git-backed claim is unverifiable and none is verified', () => {
  withRepo((repoRoot) => {
    const records = verifyDispatchClaims(
      [
        { kind: 'git-object', claim: FULL_SHA },
        { kind: 'tree', claim: TIP_TREE },
      ],
      { repoRoot, git: null },
    )
    assert.deepEqual(
      records.map((record) => record.verdict),
      ['unverifiable', 'unverifiable'],
    )
  })
})

test('a git runner that cannot answer at all degrades to unverifiable, never to verified', () => {
  withRepo((repoRoot) => {
    const git = () => {
      throw new Error('spawn git ENOENT')
    }
    const records = verifyDispatchClaims(
      [
        { kind: 'git-object', claim: FULL_SHA },
        { kind: 'tree', claim: TIP_TREE },
      ],
      { repoRoot, git },
    )
    for (const record of records) assert.equal(record.verdict, 'unverifiable')
  })
})

test('a repository with no HEAD leaves the tree claim unverifiable rather than stale', () => {
  withRepo((repoRoot) => {
    const [record] = verifyDispatchClaims([{ kind: 'tree', claim: TIP_TREE }], {
      repoRoot,
      git: gitFake({ tipTree: null }),
    })
    assert.equal(record.verdict, 'unverifiable')
  })
})

test('an unknown kind and a malformed entry are unverifiable, never verified', () => {
  withRepo((repoRoot) => {
    const records = verifyDispatchClaims(
      [{ kind: 'sha', claim: FULL_SHA }, { kind: 'path' }, null, { kind: 'path', claim: '   ' }],
      { repoRoot, git: gitFake() },
    )
    assert.equal(records.length, 4)
    for (const record of records) assert.equal(record.verdict, 'unverifiable')
  })
})

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

test('the verify CLI prints one record per claim and exits non-zero when one is refuted', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/real.js', 'one\ntwo\n')
    const clean = join(repoRoot, 'clean.json')
    writeFileSync(clean, JSON.stringify([{ kind: 'path', claim: 'src/real.js:2' }]))
    const dirty = join(repoRoot, 'dirty.json')
    writeFileSync(
      dirty,
      JSON.stringify([
        { kind: 'path', claim: 'src/real.js' },
        { kind: 'path', claim: 'src/gone.js' },
      ]),
    )

    const ok = runCli(['verify', '--file', clean, '--repo-root', repoRoot])
    assert.equal(ok.status, 0, ok.stderr)
    assert.deepEqual(
      JSON.parse(ok.stdout).map((record) => record.verdict),
      ['verified'],
    )

    const bad = runCli(['verify', '--file', dirty, '--repo-root', repoRoot])
    assert.equal(bad.status, 1, bad.stderr)
    assert.deepEqual(
      JSON.parse(bad.stdout).map((record) => record.verdict),
      ['verified', 'refuted'],
    )
  })
})

test('the CLI reports usage and unreadable inputs as an operator error, not as a verdict', () => {
  withRepo((repoRoot) => {
    assert.equal(runCli([]).status, 2)
    assert.equal(runCli(['verify']).status, 2)
    assert.equal(runCli(['verify', '--file', join(repoRoot, 'absent.json')]).status, 2)
  })
})

// ---------------------------------------------------------------------------
// Two readings of one claim
// ---------------------------------------------------------------------------

test('a claim string that disagrees with its structured coordinate is unverifiable', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/real.js', 'one\ntwo\n')
    // `pathTarget` adjudicates the STRUCTURED coordinate while the record publishes the
    // caller's `claim` verbatim. Without the disagreement test this reads `src/real.js`
    // and stamps `verified` on a record naming `src/missing.js:1` — the fabricated
    // coordinate this module exists to catch, emitted by this module.
    const [record] = verifyDispatchClaims(
      [{ kind: 'path', claim: 'src/missing.js:1', file: 'src/real.js', line: 1 }],
      { repoRoot, git: null },
    )
    assert.equal(record.verdict, 'unverifiable')
    assert.equal(record.claim, 'src/missing.js:1')
    assert.match(record.reason, /disagrees with the structured coordinate src\/real\.js:1/)
  })
})

test('a disagreement is declined, never refuted, even when the structured file is absent', () => {
  withRepo((repoRoot) => {
    // The structured side names nothing real, so the refuting branch is live — and must
    // still lose to the disagreement, because which coordinate was claimed is unsettled.
    const [record] = verifyDispatchClaims(
      [{ kind: 'path', claim: 'src/real.js', file: 'src/gone.js', line: null }],
      { repoRoot, git: null },
    )
    assert.equal(record.verdict, 'unverifiable')
  })
})

test('a claim string that agrees with its structured coordinate still verifies', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/real.js', 'one\ntwo\n')
    // Both renderings of agreement: `file:line`, and the bare `file` when the line is
    // absent. Either spelling of "no line" (null, undefined) renders the same way.
    const records = verifyDispatchClaims(
      [
        { kind: 'path', claim: 'src/real.js:2', file: 'src/real.js', line: 2 },
        { kind: 'path', claim: 'src/real.js', file: 'src/real.js', line: null },
        { kind: 'path', claim: 'src/real.js', file: 'src/real.js' },
      ],
      { repoRoot, git: null },
    )
    assert.deepEqual(
      records.map((record) => record.verdict),
      ['verified', 'verified', 'verified'],
    )
    assert.equal(records[0].reason, 'src/real.js resolves and has at least 2 line(s)')
    assert.equal(records[1].reason, 'src/real.js resolves inside the repository root')
  })
})

// ---------------------------------------------------------------------------
// An ambiguous abbreviation is not a false claim
// ---------------------------------------------------------------------------

test('an ambiguous short object name is declined, not refuted', () => {
  withRepo((repoRoot) => {
    // `--quiet` hides the ambiguity, so this claim reaches the same failed answer a
    // nonexistent SHA does. Reading that as `refuted` publishes "names no object in this
    // repository" about a prefix that names several — terminal under R3, and false.
    const [record] = verifyDispatchClaims([{ kind: 'git-object', claim: '0000' }], {
      repoRoot,
      git: gitFake({ ambiguous: ['0000'] }),
    })
    assert.equal(record.verdict, 'unverifiable')
    assert.match(record.reason, /ambiguous abbreviation/)
  })
})

test('a genuinely absent object is still refuted, short or full', () => {
  withRepo((repoRoot) => {
    // The other side of the same discrimination: nothing here is ambiguous, so the re-ask
    // reports only `fatal: Needed a single revision` and the refutation stands.
    const records = verifyDispatchClaims(
      [
        { kind: 'git-object', claim: FULL_SHA },
        { kind: 'git-object', claim: 'beef' },
      ],
      { repoRoot, git: gitFake({ ambiguous: ['0000'] }) },
    )
    assert.deepEqual(
      records.map((record) => record.verdict),
      ['refuted', 'refuted'],
    )
    assert.match(records[0].reason, /names no object in this repository/)
  })
})

// ---------------------------------------------------------------------------
// An unexpected resolver code degrades to a verdict, never to a throw
// ---------------------------------------------------------------------------

/**
 * Load the module under test with `citation-coordinate.mjs` replaced.
 *
 * The module imports its resolver directly, and it SHOULD: an injection seam added to
 * production so a test can reach one branch is the module weakened to be testable. So the
 * real source is copied byte-for-byte into a scratch directory beside a stub resolver and
 * imported from there — what runs is the shipped text, with only its dependency swapped.
 */
async function withStubbedResolver(resolve, run) {
  const dir = mkdtempSync(join(tmpdir(), 'bs-dispatch-claims-stub-'))
  try {
    const here = fileURLToPath(new URL('.', import.meta.url))
    for (const name of ['bs-dispatch-claims.mjs', 'main-module.mjs']) {
      writeFileSync(join(dir, name), readFileSync(join(here, name), 'utf8'))
    }
    writeFileSync(
      join(dir, 'citation-coordinate.mjs'),
      'export function repoRootIsReadable() { return true }\n' +
        `export function resolveCitationCoordinate() { return ${JSON.stringify(resolve)} }\n`,
    )
    return await run(await import(pathToFileURL(join(dir, 'bs-dispatch-claims.mjs')).href))
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

test('a resolver result code this mapping does not know is unverifiable, not a throw', async () => {
  await withStubbedResolver({ ok: false, code: 'sideways', path: '/tmp/x' }, (module) => {
    withRepo((repoRoot) => {
      // Before the guard this fell through unconditionally into the unreadable branch,
      // which dereferences `error.message` — absent on this result. The TypeError escaped
      // `verifyDispatchClaims` (its try/catch wraps only the resolver call) and would have
      // crashed a whole triage run, which is not fail-closed.
      const [record] = module.verifyDispatchClaims([{ kind: 'path', claim: 'src/real.js' }], {
        repoRoot,
        git: null,
      })
      assert.equal(record.verdict, 'unverifiable')
      assert.match(record.reason, /unexpected result code: sideways/)
    })
  })
})

test('an unreadable result carrying no error object still explains itself', async () => {
  await withStubbedResolver({ ok: false, code: 'unreadable', path: '/tmp/x' }, (module) => {
    withRepo((repoRoot) => {
      const [record] = module.verifyDispatchClaims([{ kind: 'path', claim: 'src/real.js' }], {
        repoRoot,
        git: null,
      })
      assert.equal(record.verdict, 'unverifiable')
      assert.match(record.reason, /could not be read/)
    })
  })
})

// ---------------------------------------------------------------------------
// The real git path
// ---------------------------------------------------------------------------

test('the default git runner adjudicates against the real binary', (t) => {
  // Every other git test injects a fake, so `spawnGit` — the ONLY git path that runs in
  // production, and the one that decides real `git-object` and `tree` verdicts — was never
  // executed. A defect in its status/error/trim handling would ship green.
  const repoRoot = fileURLToPath(new URL('..', import.meta.url))
  const probe = spawnSync('git', ['rev-parse', '--show-toplevel'], {
    cwd: repoRoot,
    encoding: 'utf8',
  })
  if (probe.error || probe.status !== 0) {
    // Skip LOUDLY: a silent pass here is exactly the shape this test exists to remove.
    t.skip(`git unavailable in ${repoRoot}: ${probe.error?.message ?? probe.stderr.trim()}`)
    return
  }
  // Derived by running git, never hardcoded: a pinned SHA rots into a test that passes for
  // the wrong reason the moment the branch moves.
  const tip = spawnSync('git', ['rev-parse', 'HEAD^{tree}'], {
    cwd: repoRoot,
    encoding: 'utf8',
  }).stdout.trim()
  assert.match(tip, /^[0-9a-f]{40}$/)

  // No `git` key at all — this is the injection-free path.
  const records = verifyDispatchClaims(
    [
      { kind: 'tree', claim: tip },
      { kind: 'git-object', claim: tip },
      { kind: 'git-object', claim: FULL_SHA },
    ],
    { repoRoot },
  )
  assert.deepEqual(
    records.map((record) => record.verdict),
    ['verified', 'verified', 'refuted'],
  )
  assert.match(records[0].reason, new RegExp(`is HEAD\\^\\{tree\\} \\(${tip}\\)`))
  // Well-formed but invented: git knows no such object, so the claim is genuinely false.
  assert.match(records[2].reason, /names no object in this repository/)
})

// ---------------------------------------------------------------------------
// A line's SPELLING is not a fact about the claim
// ---------------------------------------------------------------------------

test('a structured line that is not a number is declined, not refuted', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/real.js', 'one\ntwo\nthree\n')
    // The file genuinely HAS a line 3, so this claim is true. Handed a string line it
    // failed `Number.isInteger` inside the resolver and was published as
    // `refuted: line 3 is not a line any file has` — a false statement about a true
    // claim. Reachable: the CLI adjudicates whatever a claims file holds.
    const [record] = verifyDispatchClaims(
      [{ kind: 'path', claim: 'src/real.js:3', file: 'src/real.js', line: '3' }],
      { repoRoot, git: null },
    )
    assert.equal(record.verdict, 'unverifiable')
    assert.match(record.reason, /given as a string, not a number/)
  })
})

test('an impossible NUMBER line is still refuted', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/real.js', 'one\ntwo\nthree\n')
    // The other side of the discrimination: 0, a negative and a fraction are coordinates
    // no file can have. That is a fact about the claim, not about how it was spelled.
    const records = verifyDispatchClaims(
      [
        { kind: 'path', claim: 'src/real.js:0', file: 'src/real.js', line: 0 },
        { kind: 'path', claim: 'src/real.js:-1', file: 'src/real.js', line: -1 },
        { kind: 'path', claim: 'src/real.js:1.5', file: 'src/real.js', line: 1.5 },
      ],
      { repoRoot, git: null },
    )
    assert.deepEqual(
      records.map((record) => record.verdict),
      ['refuted', 'refuted', 'refuted'],
    )
  })
})

test('the CLI does not refute a true coordinate whose line is spelled as a string', () => {
  withRepo((repoRoot) => {
    writeRepoFile(repoRoot, 'src/real.js', 'one\ntwo\nthree\n')
    const claims = join(repoRoot, 'claims.json')
    writeFileSync(
      claims,
      JSON.stringify([{ kind: 'path', claim: 'src/real.js:3', file: 'src/real.js', line: '3' }]),
    )
    const res = runCli(['verify', '--file', claims, '--repo-root', repoRoot])
    // Exit 1 is the CLI's code for "at least one claim is refuted". A true claim must
    // never produce it.
    assert.equal(res.status, 0, res.stdout)
    assert.deepEqual(
      JSON.parse(res.stdout).map((record) => record.verdict),
      ['unverifiable'],
    )
  })
})
