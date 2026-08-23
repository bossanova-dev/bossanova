// Contract test for the unattended bs-sweep-releases recorder (BOS-706).
//
// It pins the source skill's cron-only, all-unseen recording boundary.  The
// generated Codex mirror is checked separately after `make codex-skills`.

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const skillPath = path.join(repoRoot, '.claude/skills/bs-sweep-releases/SKILL.md')
const gatePath = path.join(repoRoot, '.claude/skills/bs-sweep-releases/gate/gate.mjs')
const corePath = path.join(repoRoot, 'scripts/sweep-releases-gate.mjs')

const read = (file) => fs.readFileSync(file, 'utf8')
const skill = () => read(skillPath)

function frontmatter(body, key) {
  const match = body.match(new RegExp(`^${key}:\\s*(.+)$`, 'm'))
  assert.ok(match, `frontmatter must declare ${key}`)
  return match[1]
}

function bashBlocks(body) {
  const blocks = []
  let open = false
  let lines = []
  for (const line of body.split('\n')) {
    if (line === '```bash') {
      assert.equal(open, false, 'bash fence cannot nest')
      open = true
      lines = []
    } else if (line === '```' && open) {
      blocks.push(lines.join('\n'))
      open = false
    } else if (open) {
      lines.push(line)
    }
  }
  assert.equal(open, false, 'bash fence must close')
  return blocks
}

function nodeHeredocPayloads(body) {
  const payloads = []
  for (const block of bashBlocks(body)) {
    const matches = block.matchAll(/node --input-type=module <<'NODE'\n([\s\S]*?)\nNODE/g)
    for (const match of matches) payloads.push(match[1])
  }
  return payloads
}

function phase3NodePayload(body) {
  const match = body.match(
    /## Phase[ ]3:[\s\S]*?```bash\n[\s\S]*?node[ ]--input-type=module[ ]<<'NODE'\n([\s\S]*?)\nNODE\n```/,
  )
  assert.ok(match, 'phase 3 node heredoc must exist')
  return match[1]
}

function writeFakeBoss(dir, output = '[]') {
  const fake = path.join(dir, 'boss')
  fs.writeFileSync(fake, `#!/bin/sh\nprintf '%s\\n' '${output}'\n`)
  fs.chmodSync(fake, 0o755)
  return fake
}

function writeFakeGh(dir) {
  const fake = path.join(dir, 'gh')
  fs.writeFileSync(
    fake,
    `#!/bin/sh
case "$*" in
  *'/comments?'*)
    printf '%s\\n' '[[{"user":{"login":"chatgpt-codex-connector[bot]"},"path":"services/example.go","line":12,"body":"![P1 Badge](x) Release regression","html_url":"https://example.test/org/repo/pull/1#discussion_r1"}]]'
    ;;
  *'base=staging'*)
    printf 'HTTP/1.1 200 OK\\r\\n\\r\\n'
    printf '%s\\n' '[{"number":1,"base":{"ref":"staging"},"created_at":"2099-01-01T00:00:00Z","updated_at":"2099-01-01T00:00:00Z","state":"open"}]'
    ;;
  *)
    printf 'HTTP/1.1 200 OK\\r\\n\\r\\n[]'
    ;;
esac
`,
  )
  fs.chmodSync(fake, 0o755)
  return fake
}

function runGate({ bossBin, path: pathEntries, cwd }) {
  return spawnSync(process.execPath, [gatePath], {
    cwd,
    env: { BOSS_BIN: bossBin, PATH: pathEntries },
    encoding: 'utf8',
  })
}

test('renamed source skill and its gate exist', () => {
  assert.ok(fs.existsSync(skillPath), 'bs-sweep-releases source skill must exist')
  assert.ok(fs.existsSync(gatePath), 'bs-sweep-releases cron gate must exist')
  assert.ok(fs.existsSync(corePath), 'sweep-releases pure core must exist')
  assert.equal(fs.existsSync(path.join(repoRoot, '.claude/skills/bs-sweep-review')), false)
})

test('frontmatter makes this a cron-only recorder with no tracker MCP surface', () => {
  const body = skill()
  assert.equal(frontmatter(body, 'name'), 'bs-sweep-releases')
  assert.equal(frontmatter(body, 'disable-model-invocation'), 'true')
  const tools = frontmatter(body, 'allowed-tools')
    .split(',')
    .map((value) => value.trim())
  assert.deepEqual(tools, ['Bash', 'Read', 'Glob', 'Grep', 'Skill'])
  assert.doesNotMatch(body, /mcp__bossanova-linear__/)
  assert.doesNotMatch(body, /save_issue|list_issues|LINEAR_API_KEY/)
})

test('the recorder has no dry-run or interactive branch and writes Boss improvement notes only', () => {
  const body = skill()
  assert.doesNotMatch(body, /dry[- ]run|--no-dry-run/i)
  assert.doesNotMatch(body, /interactive|ask(?: the)? user|prompt/i)
  assert.match(body, /"\$BOSS"\s+notes\s+ls --tag\s+improvement --json --limit\s+0/)
  assert.match(body, /boss\s+notes\s+add .* --tag\s+improvement --json/)
  assert.doesNotMatch(body, /boss\s+notes (?:add|ls)[^\n]*(?!improvement)--tag\s+(?!improvement)/)
  assert.match(body, /one .*note.*per .*unseen.*cluster/i)
  assert.match(body, /all[- ]unseen/i)
})

test('release selection and exact marker dedupe are delegated to the pure core', () => {
  const body = skill()
  assert.match(body, /scripts\/sweep-releases-gate\.mjs/)
  assert.match(body, /selectUnseenFindings/)
  assert.match(body, /fetchTrackedReleaseMarkers/)
  assert.match(body, /state=all/i)
  assert.match(body, /staging/)
  assert.match(body, /production/)
  assert.match(body, /seven.*UTC.*day|seven-day/i)
  assert.match(body, /Release\s+review: v2:<base64url-anchor>/)
  assert.match(body, /exact.*last\s+line/i)
  assert.match(body, /immediately\s+before\s+each\s+write|pre-write\s+re-check/i)
  assert.match(body, /fail-closed/i)
})

test('untrusted review text cannot create markers and neither remote review surface is mutated', () => {
  const body = skill()
  assert.match(body, /untrusted/i)
  assert.match(body, /sanitize|separator|U\+2028|U\+2029/i)
  assert.doesNotMatch(body, /\bgh\s+(?:pr|api)\s+(?:comment|review|merge|edit|close|reopen)/)
  assert.doesNotMatch(body, /\bgh\s+api\s+--method\s+(?:POST|PATCH|PUT|DELETE)/i)
  assert.doesNotMatch(body, /Linear|linear/i)
})

test('lock, heartbeat, stale reclaim, scratch cleanup, clean tree, and nonfatal notes teardown are explicit', () => {
  const body = skill()
  assert.match(body, /every\s+shell\s+fence.*one\s+continuous\s+shell\s+session/i)
  assert.match(body, /mkdir/)
  assert.match(body, /bs-sweep-releases\.lock/)
  assert.match(body, /heartbeat/i)
  assert.match(body, /stale/i)
  assert.match(body, /LOCK_TOKEN/)
  assert.match(body, /GIT_COMMON_DIR="\$\(git[ ]rev-parse --git-common-dir\)"/)
  assert.match(body, /LOCK_DIR="\$GIT_COMMON_DIR\/bs-sweep-releases\.lock"/)
  assert.match(body, /umask[ ]077/)
  assert.match(body, /RUN_DIR="\$\(mktemp -d "\$\{TMPDIR:-\/tmp\}\/bs-sweep-releases\.XXXXXX"\)"/)
  assert.match(body, /NOTES_JSON="\$RUN_DIR\/notes\.json"/)
  assert.match(body, /COMMENTS_JSON="\$RUN_DIR\/comments\.json"/)
  assert.match(body, /rm -rf "\$RUN_DIR"/)
  assert.doesNotMatch(
    body,
    /\$\{TMPDIR:-\/tmp\}\/bs-sweep-releases-(?:notes|comments|selection|recheck)\.json/,
  )
  assert.match(body, /owner/)
  assert.match(body, /STALE_DIR/)
  assert.match(body, /STALE_TARGET="\$HEARTBEAT"/)
  assert.match(body, /if ! test -f "\$STALE_TARGET"; then[ ]STALE_TARGET="\$LOCK_DIR"; fi/)
  assert.match(body, /mv "\$LOCK_DIR" "\$STALE_DIR"/)
  assert.match(body, /lease[ ]lost/)
  assert.match(body, /utimesSync/)
  assert.match(
    body,
    // Fenced JS, and the point of the pin is that these four statements are on consecutive lines at
    // one indent level. `\s+` would match a blank line between them, so the gaps stay exact: `[ ]`
    // for the word gaps the gate flags, a plain `\n  ` for the indentation it does not.
    /const[ ]trackedMarkers = await[ ]fetchTrackedReleaseMarkers\(\{ notes: JSON\.parse\(snapshot\) \}\)\n  heartbeat\(\)\n  if \(isTrackedReleaseMarker\(candidate, trackedMarkers\)\) continue\n  await[ ]addNote\(candidate\)/,
  )
  assert.match(body, /setInterval\(/)
  assert.match(body, /child\.kill\('SIGKILL'\)/)
  assert.match(body, /Number\.isSafeInteger\(line\) && line > 0/)
  assert.match(body, /teardown\(\)/)
  assert.match(body, /raw.*JSON.*scratch|scratch.*raw.*JSON/i)
  assert.match(body, /rm -rf/)
  assert.match(body, /git\s+status --porcelain/)
  assert.match(body, /bs-record-notes/)
  assert.match(body, /non-fatal/i)
})

test('dedupe contract names the local lease boundary instead of claiming an impossible remote atomic create', () => {
  const body = skill()
  assert.match(body, /--idempotency-key.*candidate\.marker/)
  assert.match(body, /atomically\s+returns\s+the\s+existing\s+repo-scoped\s+note/i)
  assert.doesNotMatch(body, /no\s+conditional\/idempotent\s+create/i)
})

test('writer pages release pull requests by update time before applying the cutoff', () => {
  assert.match(skill(), /sort=updated&direction=desc/)
})

test('skill bash snippets are syntactically valid', () => {
  const scratch = path.join(os.tmpdir(), `bs-sweep-releases-${process.pid}.sh`)
  try {
    const blocks = bashBlocks(skill())
    assert.ok(blocks.length > 0, 'skill must contain executable bash snippets')
    for (const block of blocks) {
      fs.writeFileSync(scratch, block)
      execFileSync('bash', ['-n', scratch], { stdio: 'pipe' })
    }
  } finally {
    fs.rmSync(scratch, { force: true })
  }
})

test('phase 3 node heredoc payload parses as JavaScript', () => {
  const scratch = path.join(os.tmpdir(), `bs-sweep-releases-phase3-${process.pid}.mjs`)
  try {
    fs.writeFileSync(scratch, phase3NodePayload(skill()))
    execFileSync(process.execPath, ['--check', scratch], { stdio: 'pipe' })
  } finally {
    fs.rmSync(scratch, { force: true })
  }
})

test('executable skill sites use the resolved boss path instead of bare boss', () => {
  const executable = bashBlocks(skill()).join('\n')
  const payloads = nodeHeredocPayloads(skill())
  assert.ok(executable.length > 0, 'skill must contain executable shell snippets')
  assert.ok(payloads.length > 0, 'skill must contain executable node heredocs')
  assert.match(executable, /"\$BOSS"\s+notes\s+ls --tag\s+improvement --json --limit\s+0/)
  assert.match(executable, /spawn\(process\.env\.BOSS/)
  assert.match(executable, /execFileSync\(process\.env\.BOSS/)
  assert.doesNotMatch(executable, /(^|\n)\s*boss\s+notes\b/)
  assert.doesNotMatch(executable, /spawn\('boss'/)
  assert.doesNotMatch(executable, /execFileSync\('boss'/)
})

test('cron gate imports the release core and fails closed', () => {
  const gate = read(gatePath)
  assert.match(gate, /sweep-releases-gate\.mjs/)
  assert.match(gate, /resolveBossBinary/)
  assert.match(gate, /collectReleasePullRequests/)
  assert.match(gate, /selectUnseenFindings/)
  assert.match(gate, /fetchTrackedReleaseMarkers/)
  assert.match(
    gate,
    /json\(boss\.path, \['notes', 'ls', '--tag', 'improvement', '--json', '--limit', '0'\]\)/,
  )
  assert.match(gate, /\['notes', 'ls', '--tag', 'improvement', '--json', '--limit', '0'\]/)
  assert.match(gate, /sort=updated&direction=desc/)
  assert.match(gate, /fail-closed/i)
  assert.doesNotMatch(gate, /json\('boss'/)
  assert.doesNotMatch(gate, /linear|save_issue|list_issues/i)
  assert.doesNotMatch(gate, /\bgh\s+(?:pr|api)\s+(?:comment|review|merge|edit|close|reopen)/)
})

test('cron gate accepts file-level bot review anchors', () => {
  assert.match(read(gatePath), /comment\.subject_type === 'file'/)
})

test('live gate succeeds when BOSS_BIN names a healthy boss binary', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-sweep-releases-boss-ok-'))
  try {
    writeFakeGh(dir)
    const result = runGate({ bossBin: writeFakeBoss(dir), path: dir, cwd: dir })
    assert.equal(result.status, 0, result.stderr)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('live gate reports the resolver reason when no boss binary resolves at all', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-sweep-releases-boss-missing-'))
  const stale = path.join(dir, 'gone', 'bin', 'boss')
  try {
    writeFakeGh(dir)
    const result = runGate({ bossBin: stale, path: dir, cwd: dir })
    assert.notEqual(result.status, 0)
    assert.match(result.stderr, /no\s+usable\s+boss\s+executable/)
    assert.ok(result.stderr.includes(stale), `reason must name BOSS_BIN: ${result.stderr}`)
    assert.doesNotMatch(result.stderr, /ENOENT|spawnSync/)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('live gate falls back to PATH when BOSS_BIN names a deleted worktree', () => {
  const pathDir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-sweep-releases-boss-path-'))
  const reaped = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-sweep-releases-reaped-worktree-'))
  const stale = path.join(reaped, 'bin', 'boss')
  fs.rmSync(reaped, { recursive: true, force: true })
  try {
    writeFakeGh(pathDir)
    writeFakeBoss(pathDir)
    const result = runGate({ bossBin: stale, path: pathDir, cwd: pathDir })
    assert.equal(result.status, 0, result.stderr)
  } finally {
    fs.rmSync(pathDir, { recursive: true, force: true })
  }
})
