#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import {
  SHELL_INFO_STRINGS,
  SKILL_ROOTS,
  checkSkillShellInRepo,
  extractFencedBlocks,
  findUnsafeGhFileBodyWrites,
  findUnsafeGhBody,
  findInertGuards,
  findMultiGlobRemovals,
  findSkillMarkdownFiles,
  findUnterminatedHeredoc,
  normalizePlaceholders,
} from './check-skill-shell.mjs'

const md = (...lines) => `${lines.join('\n')}\n`

// Build a throwaway repo containing only the files given, keyed by repo-relative path.
function makeRepo(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'skill-shell-'))
  for (const [rel, contents] of Object.entries(files)) {
    const full = path.join(root, rel)
    fs.mkdirSync(path.dirname(full), { recursive: true })
    fs.writeFileSync(full, contents)
  }
  return root
}

const claudeSkill = (name) => path.join('.claude', 'skills', name, 'SKILL.md')

test('findUnsafeGhBody rejects interpolated inline gh reply bodies and permits literal or file forms', async () => {
  assert.equal(
    findUnsafeGhBody('gh api repos/o/r/pulls/1/comments/2/replies -f body="Fixed: `make test`"')
      .length,
    1,
  )
  assert.equal(
    findUnsafeGhBody(
      String.raw`gh api repos/o/r/pulls/1/comments/2/replies --raw-field body="Fixed: $(make test)"`,
    ).length,
    1,
  )
  assert.equal(findUnsafeGhBody('echo ok; gh api x -f body="Fixed: `make test`"').length, 1)
  assert.equal(findUnsafeGhBody('gh api x --field=body="Fixed: `make test`"').length, 1)
  assert.equal(findUnsafeGhBody(String.raw`gh api x --raw-field=body=$(make test)`).length, 1)
  assert.equal(
    findUnsafeGhBody('gh api x -f body=Fixed:<(some-command)').length,
    1,
    'input process substitution',
  )
  assert.equal(
    findUnsafeGhBody('gh api x -f body=Fixed:>(some-command)').length,
    1,
    'output process substitution',
  )
  assert.equal(findUnsafeGhBody('gh api x -f=body="Fixed: `make test`"').length, 1)
  assert.equal(findUnsafeGhBody('gh api x -fbody="Fixed: `make test`"').length, 1)
  assert.equal(findUnsafeGhBody(String.raw`gh api x -Fbody="Fixed: $(make test)"`).length, 1)
  assert.equal(
    findUnsafeGhBody('gh api x -ifbody="Fixed: `make test`"').length,
    1,
    'clustered -i plus -f',
  )
  assert.equal(
    findUnsafeGhBody(String.raw`gh api x -iFbody="Fixed: $(make test)"`).length,
    1,
    'clustered -i plus -F',
  )
  assert.equal(
    findUnsafeGhBody('gh api x -if body="Fixed: `make test`"').length,
    1,
    'clustered -i plus split -f field',
  )
  assert.equal(
    findUnsafeGhBody(String.raw`gh api x -iF body="Fixed: $(make test)"`).length,
    1,
    'clustered -i plus split -F field',
  )
  assert.equal(findUnsafeGhBody('gh api x -f"body=Fixed: `make test`"').length, 1)
  assert.equal(findUnsafeGhBody('gh api x "-fbody=Fixed: `make test`"').length, 1)
  assert.equal(findUnsafeGhBody(String.raw`gh api x -F"body=Fixed: $(make test)"`).length, 1)
  assert.equal(findUnsafeGhBody('gh api x "--field=body=Fixed: `make test`"').length, 1)
  assert.equal(
    findUnsafeGhBody(String.raw`gh api x --raw-field"=body=Fixed: $(make test)"`).length,
    1,
  )
  assert.equal(findUnsafeGhBody(String.raw`gh api x "-f"body=$(make test)`).length, 1)
  assert.equal(findUnsafeGhBody('/usr/bin/gh api x -f body="Fixed: `make test`"').length, 1)
  assert.equal(
    findUnsafeGhBody('env -u GH_TOKEN /usr/bin/gh api x -f body="Fixed: `make test`"').length,
    1,
  )
  assert.equal(
    findUnsafeGhBody('sudo -u root command -p gh api x -f body="Fixed: `make test`"').length,
    1,
  )
  assert.equal(findUnsafeGhBody('sudo -Eu root gh api x -f body="Fixed: `make test`"').length, 1)
  assert.equal(findUnsafeGhBody('env -iu GH_TOKEN gh api x -f body="Fixed: `make test`"').length, 1)
  assert.equal(
    findUnsafeGhBody('env -S gh api x -f body="Fixed: `make test`"').length,
    1,
    'env split-string command',
  )
  assert.equal(
    findUnsafeGhBody(`env --split-string="gh api x -f 'body=Fixed: $(date)'"`).length,
    1,
    'attached env split-string command',
  )
  assert.equal(
    findUnsafeGhBody(`env -S "gh api x -f 'body=Fixed: $(date)'"`).length,
    1,
    'quoted env split-string command',
  )
  assert.equal(findUnsafeGhBody(['gh api x -f body="Fixed:', '`make test`"'].join('\n')).length, 1)
  assert.equal(findUnsafeGhBody('gh api x -F body=@"$REPLY_BODY"`make test`').length, 1)
  assert.equal(
    findUnsafeGhBody(['gh api x -F body=@- <<EOF', 'Fixed: `make test`', 'EOF'].join('\n')).length,
    1,
    'stdin body supplied by heredoc',
  )
  assert.equal(
    findUnsafeGhBody(['gh api x -Fbody=@- <<EOF', 'Fixed: `make test`', 'EOF'].join('\n')).length,
    1,
    'combined stdin body option',
  )
  assert.equal(
    findUnsafeGhBody(
      ['gh --hostname github.com api x -F body=@- <<EOF', 'Fixed: `make test`', 'EOF'].join('\n'),
    ).length,
    1,
    'inherited gh hostname before stdin body',
  )
  assert.equal(
    findUnsafeGhBody('gh --hostname github.com api x -f body="Fixed: `make test`"').length,
    1,
    'inherited gh hostname before inline body',
  )
  assert.equal(
    findUnsafeGhBody('gh -R recurser/bossanova api x -f body="Fixed: `make test`"').length,
    1,
    'inherited gh repo before inline body',
  )
  assert.equal(
    findUnsafeGhBody(
      'gh --repo=recurser/bossanova --hostname github.com api x -f body="Fixed: `make test`"',
    ).length,
    1,
    'multiple inherited gh options before inline body',
  )
  assert.deepEqual(
    findUnsafeGhBody(['cat <<EOF; gh api x -F body=@-', 'literal', 'EOF'].join('\n')),
    [],
    'heredoc on another command segment does not supply gh stdin',
  )
  assert.equal(
    findUnsafeGhBody(['cat <<EOF | gh api x -F body=@-', 'Fixed: `make test`', 'EOF'].join('\n'))
      .length,
    1,
    'an upstream pipeline heredoc supplies gh stdin',
  )
  assert.deepEqual(
    findUnsafeGhBody(['gh api x 3<<EOF -F body=@-', 'literal', 'EOF'].join('\n')),
    [],
    'non-stdin gh heredoc does not supply @-',
  )
  assert.equal(
    findUnsafeGhBody(['gh api x 0<<EOF -F body=@-', 'literal', 'EOF'].join('\n')).length,
    1,
    'explicit stdin gh heredoc remains unsafe',
  )
  assert.equal(findUnsafeGhBody('out=$(gh api x -f body="Fixed: `make test`")').length, 1)
  assert.equal(findUnsafeGhBody(String.raw`gh api x -f body='prefix'$(make test)`).length, 1)
  assert.equal(findUnsafeGhBody("gh api x -f body='prefix'`make test`").length, 1)
  assert.deepEqual(findUnsafeGhBody("gh api x -f body='Fixed: `make test`'"), [])
  assert.deepEqual(findUnsafeGhBody("gh api x -f'body=Fixed: `make test`'"), [])
  assert.deepEqual(findUnsafeGhBody("gh api x '-fbody=Fixed: `make test`'"), [])
  assert.deepEqual(findUnsafeGhBody("gh api x --raw-field'=body=Fixed: $(make test)'"), [])
  assert.deepEqual(findUnsafeGhBody("gh api x '-f'body='Fixed: `make test`'"), [])
  assert.deepEqual(findUnsafeGhBody('gh api x -f body="Fixed: \\`make test\\`"'), [])
  assert.deepEqual(findUnsafeGhBody("gh api x --field='body=Fixed: `make test`'"), [])
  assert.deepEqual(findUnsafeGhBody("gh api x -f body=$'Fixed: `make test` $(date)'"), [])
  assert.deepEqual(findUnsafeGhBody("gh api x --field=$'body=Fixed: `make test` $(date)'"), [])
  assert.deepEqual(findUnsafeGhBody("gh api x -f body='Fixed: <(some-command)'"), [])
  assert.deepEqual(findUnsafeGhBody('gh api x -f body=Fixed:\\<(some-command)'), [])
  assert.deepEqual(findUnsafeGhBody('gh api x -F body=@"$REPLY_BODY"'), [])
  assert.equal(
    findUnsafeGhBody('gh api x -f body=@"$REPLY_BODY"').length,
    1,
    'raw fields do not dereference @file body values',
  )
  assert.equal(
    findUnsafeGhBody('gh api x --raw-field body=@/tmp/reply').length,
    1,
    'long raw fields do not dereference @file body values',
  )
  assert.equal(
    findUnsafeGhBody(['gh api x -f body=@- <<EOF', 'Fixed: literal', 'EOF'].join('\n')).length,
    1,
    'raw stdin-looking values report only as raw fields',
  )
  assert.deepEqual(findUnsafeGhBody('gh api x --field body="Fixed: documented behavior"'), [])
  assert.deepEqual(findUnsafeGhBody('echo gh api x -f body="Fixed: `make test`"'), [])

  const repoRoot = makeRepo({
    [claudeSkill('unsafe-gh-body')]: md('```bash', 'gh api x -f body="Fixed: `make test`"', '```'),
  })
  try {
    const findings = await checkSkillShellInRepo(repoRoot)
    assert.deepEqual(
      findings.map(({ file, line, kind }) => ({ file, line, kind })),
      [{ file: claudeSkill('unsafe-gh-body'), line: 2, kind: 'gh-body-interpolation' }],
    )
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }

  const nestedRepoRoot = makeRepo({
    [claudeSkill('nested-unsafe-gh-body')]: md(
      '```bash',
      'out=$(',
      'gh api x -f body="Fixed: `make test`"',
      ')',
      '```',
    ),
  })
  try {
    const findings = await checkSkillShellInRepo(nestedRepoRoot)
    assert.deepEqual(
      findings.map(({ file, line, kind }) => ({ file, line, kind })),
      [{ file: claudeSkill('nested-unsafe-gh-body'), line: 3, kind: 'gh-body-interpolation' }],
    )
  } finally {
    fs.rmSync(nestedRepoRoot, { recursive: true, force: true })
  }
})

test('findUnsafeGhFileBodyWrites rejects heredocs for gh @file reply bodies', async () => {
  const unsafe = [
    'REPLY_BODY="$(mktemp)"',
    'cat >"$REPLY_BODY" <<EOF',
    'Fixed: `make test`',
    'EOF',
    'gh api x -F body=@"$REPLY_BODY"',
  ].join('\n')
  assert.deepEqual(findUnsafeGhFileBodyWrites(unsafe), [{ lineOffset: 1, variable: 'REPLY_BODY' }])

  const quoted = unsafe.replace('<<EOF', "<<'EOF'")
  assert.deepEqual(findUnsafeGhFileBodyWrites(quoted), [{ lineOffset: 1, variable: 'REPLY_BODY' }])
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(unsafe.replace('body=@"$REPLY_BODY"', 'body=@"${REPLY_BODY}"')),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'braced body-file variable',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(unsafe.replace('>"$REPLY_BODY"', '>"${REPLY_BODY}"')),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'braced body-file producer',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      unsafe.replace('cat >"$REPLY_BODY" <<EOF', 'tee "$REPLY_BODY" <<EOF'),
    ),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'tee body-file producer',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      unsafe.replace('cat >"$REPLY_BODY" <<EOF', 'tee -a "$REPLY_BODY" <<EOF'),
    ),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'tee with options remains a body-file producer',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      unsafe.replace('cat >"$REPLY_BODY" <<EOF', 'tee /tmp/copy "$REPLY_BODY" <<EOF'),
    ),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'tee checks every body-file destination',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      unsafe.replace('cat >"$REPLY_BODY" <<EOF', 'cat >|"$REPLY_BODY" <<EOF'),
    ),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'noclobber output redirection remains a body-file producer',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      unsafe.replace('cat >"$REPLY_BODY" <<EOF', 'cat <<EOF | tee "$REPLY_BODY"'),
    ),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'piped tee body-file producer',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      unsafe.replace('cat >"$REPLY_BODY" <<EOF', 'dd of="$REPLY_BODY" <<EOF'),
    ),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'dd body-file producer',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'REPLY_BODY="$(mktemp)"',
        "cat <<EOF; printf '%s\\n' 'Fixed: literal' >\"$REPLY_BODY\"",
        'unrelated heredoc payload',
        'EOF',
        'gh api x -F body=@"$REPLY_BODY"',
      ].join('\n'),
    ),
    [],
    'a heredoc does not taint a later command segment output redirection',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      unsafe.replace(
        'gh api x -F body=@"$REPLY_BODY"',
        'gh --hostname github.com api x -F body=@"$REPLY_BODY"',
      ),
    ),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'inherited gh hostname before body-file use',
  )
  for (const bodyFile of ['"$REPLY_BODY"', '"${REPLY_BODY}"']) {
    for (const field of [
      `-Fbody=@${bodyFile}`,
      `-F=body=@${bodyFile}`,
      `--field=body=@${bodyFile}`,
      `-iFbody=@${bodyFile}`,
      `-iF body=@${bodyFile}`,
    ]) {
      assert.deepEqual(
        findUnsafeGhFileBodyWrites(unsafe.replace('-F body=@"$REPLY_BODY"', field)),
        [{ lineOffset: 1, variable: 'REPLY_BODY' }],
        field,
      )
    }
  }
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'REPLY_BODY="$(mktemp)"',
        "printf '%s\\n' 'Fixed: `make test`' >\"$REPLY_BODY\"",
        'gh api x -F body=@"$REPLY_BODY"',
      ].join('\n'),
    ),
    [],
  )
  for (const safeProducer of [
    'cat <<<\'Fixed: literal\' >"$REPLY_BODY"',
    "printf '%s\\n' '<<EOF' >\"$REPLY_BODY\"",
  ]) {
    assert.deepEqual(
      findUnsafeGhFileBodyWrites(
        ['REPLY_BODY="$(mktemp)"', safeProducer, 'gh api x -F body=@"$REPLY_BODY"'].join('\n'),
      ),
      [],
      safeProducer,
    )
  }
  const substituted = unsafe.replace(
    'gh api x -F body=@"$REPLY_BODY"',
    'result=$(gh api x -F body=@"$REPLY_BODY")',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(substituted),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'gh body file inside command substitution',
  )
  const substitutedProducer = [
    'REPLY_BODY="$(mktemp)"',
    'ignored=$(cat >"$REPLY_BODY" <<EOF',
    'Fixed: `make test`',
    'EOF',
    ')',
    'gh api x -F body=@"$REPLY_BODY"',
  ].join('\n')
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(substitutedProducer),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'body-file producer inside command substitution',
  )
  const literalPath = [
    'cat >/tmp/reply <<EOF',
    'Fixed: `make test`',
    'EOF',
    'gh api x -F body=@/tmp/reply',
  ].join('\n')
  assert.deepEqual(findUnsafeGhFileBodyWrites(literalPath), [{ lineOffset: 0, path: '/tmp/reply' }])
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      literalPath
        .replace('>/tmp/reply', '>"/tmp/reply"')
        .replace('body=@/tmp/reply', 'body=@"/tmp/reply"'),
    ),
    [{ lineOffset: 0, path: '/tmp/reply' }],
    'quoted literal body-file path',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      literalPath
        .replace('>/tmp/reply', ">'/tmp/reply'")
        .replace('body=@/tmp/reply', "body=@'/tmp/reply'"),
    ),
    [{ lineOffset: 0, path: '/tmp/reply' }],
    'single-quoted literal body-file path',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      literalPath
        .replace('>/tmp/reply', '>"/tmp/reply file"')
        .replace('body=@/tmp/reply', 'body=@"/tmp/reply file"'),
    ),
    [{ lineOffset: 0, path: '/tmp/reply file' }],
    'quoted literal body-file path containing spaces',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'cat >"$TMPDIR/reply" <<EOF',
        'Fixed: `make test`',
        'EOF',
        'gh api x -F body=@"${TMPDIR}/reply"',
      ].join('\n'),
    ),
    [{ lineOffset: 0, path: '$TMPDIR/reply' }],
    'equivalent variable-plus-suffix body-file path',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'cat >"$TMPDIR"/reply <<EOF',
        'Fixed: `make test`',
        'EOF',
        'gh api x -F body=@"$TMPDIR"/reply',
      ].join('\n'),
    ),
    [{ lineOffset: 0, path: '$TMPDIR/reply' }],
    'partially quoted variable-plus-suffix body-file path',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'cat >"${TMPDIR:-/tmp}/reply" <<EOF',
        'Fixed: `make test`',
        'EOF',
        'gh api x -F body=@"${TMPDIR:-/tmp}/reply"',
      ].join('\n'),
    ),
    [{ lineOffset: 0, path: '${TMPDIR:-/tmp}/reply' }],
    'parameter-default body-file path',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'REPLY_BODY="$(mktemp)"',
        'printf %s safe >"$REPLY_BODY" | cat <<EOF',
        'unrelated heredoc payload',
        'EOF',
        'gh api x -F body=@"$REPLY_BODY"',
      ].join('\n'),
    ),
    [],
    'a body-file redirection before a pipeline heredoc is not tainted',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'REPLY_BODY="$(mktemp)"',
        'cat >"$REPLY_BODY" <<EOF',
        'Fixed: `make test`',
        'EOF',
        'gh api x -F body=@- <"$REPLY_BODY"',
      ].join('\n'),
    ),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'stdin body redirects are traced to their file producer',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'REPLY_BODY="$(mktemp)"',
        '{ cat <<EOF; printf "%s\\n" done; } >"$REPLY_BODY"',
        'Fixed: `make test`',
        'EOF',
        'gh api x -F body=@"$REPLY_BODY"',
      ].join('\n'),
    ),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'group redirection carries heredoc output into the body file',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'REPLY_BODY="$(mktemp)"',
        '{',
        'cat <<EOF',
        'Fixed: `make test`',
        'EOF',
        '} >"$REPLY_BODY"',
        'gh api x -F body=@"$REPLY_BODY"',
      ].join('\n'),
    ),
    [{ lineOffset: 2, variable: 'REPLY_BODY' }],
    'group redirection carries a multiline heredoc into the body file',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'REPLY_BODY="$(mktemp)"',
        '( cat <<EOF; printf "%s\\n" done; ) >"$REPLY_BODY"',
        'Fixed: `make test`',
        'EOF',
        'gh api x -F body=@"$REPLY_BODY"',
      ].join('\n'),
    ),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'subshell group redirection carries heredoc output into the body file',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'REPLY_BODY="$(mktemp)"',
        'if cat <<EOF; then',
        'Fixed: `make test`',
        'EOF',
        '  printf ok',
        'fi >"$REPLY_BODY"',
        'gh api x -F body=@"$REPLY_BODY"',
      ].join('\n'),
    ),
    [{ lineOffset: 1, variable: 'REPLY_BODY' }],
    'conditional group redirection carries heredoc output into the body file',
  )
  for (const [open, close] of [
    ['{', '}'],
    ['(', ')'],
  ]) {
    assert.deepEqual(
      findUnsafeGhFileBodyWrites(
        [
          'REPLY_BODY="$(mktemp)"',
          `${open} cat <<EOF`,
          'Fixed: `make test`',
          'EOF',
          `${close} | tee "$REPLY_BODY"`,
          'gh api x -F body=@"$REPLY_BODY"',
        ].join('\n'),
      ),
      [{ lineOffset: 1, variable: 'REPLY_BODY' }],
      `${open} ... ${close} pipeline carries heredoc output into the body file`,
    )
  }
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(
      [
        'GROUP_OUTPUT="$(mktemp)"',
        'REPLY_BODY="$(mktemp)"',
        '{',
        'cat <<EOF',
        'unrelated heredoc payload',
        'EOF',
        '} >"$GROUP_OUTPUT"; printf %s safe >"$REPLY_BODY"',
        'gh api x -F body=@"$REPLY_BODY"',
      ].join('\n'),
    ),
    [],
    'a later non-heredoc redirection is not tainted by an earlier brace group',
  )
  assert.deepEqual(
    findUnsafeGhFileBodyWrites(unsafe.replace('-F body=@"$REPLY_BODY"', '-f body=@"$REPLY_BODY"')),
    [],
    'raw fields do not consume body files',
  )
  for (const incomplete of ['gh api x -F', 'gh api x --field', 'gh api x -iF']) {
    assert.deepEqual(
      findUnsafeGhFileBodyWrites(incomplete),
      [],
      `${incomplete} without a field operand is ignored`,
    )
  }

  const repoRoot = makeRepo({
    [claudeSkill('unsafe-gh-file-body')]: md('```bash', ...unsafe.split('\n'), '```'),
  })
  try {
    const findings = await checkSkillShellInRepo(repoRoot)
    assert.deepEqual(
      findings.map(({ file, line, kind }) => ({ file, line, kind })),
      [{ file: claudeSkill('unsafe-gh-file-body'), line: 3, kind: 'gh-body-file-interpolation' }],
    )
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 1. Opening line number.
test('extractFencedBlocks reports the 1-based line of the OPENING fence', () => {
  const blocks = extractFencedBlocks(md('intro', '', '```bash', 'echo hi', '```', 'tail'))
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].startLine, 3)
  assert.equal(blocks[0].info, 'bash')
  assert.equal(blocks[0].terminated, true)
  assert.equal(blocks[0].body, 'echo hi')
})

// 14bi. Inert guard rule — only guards whose failure branch can fall through are rejected.
test('findInertGuards reports a test guard even when a later command exits', () => {
  assert.deepEqual(
    findInertGuards(['test -x tool || echo "tool missing"', 'test -r config', 'exit 1'].join('\n')),
    [
      { lineOffset: 0, guard: 'test -x tool || echo "tool missing"' },
      { lineOffset: 1, guard: 'test -r config' },
    ],
  )
})

test('findInertGuards ignores final test and bracket commands', () => {
  assert.deepEqual(findInertGuards(['echo setup', 'test -f tool'].join('\n')), [])
  assert.deepEqual(findInertGuards(['echo setup', '[ -f tool ]'].join('\n')), [])
})

test('findInertGuards accepts failure branches that exit or return', () => {
  assert.deepEqual(
    findInertGuards(
      ['[ -x tool ] || exit 1', 'command -v tool || { echo missing; return 1; }'].join('\n'),
    ),
    [],
  )
})

test('findInertGuards accepts an AND guard whose OR fallback exits', () => {
  assert.deepEqual(findInertGuards('test -f missing && echo ok || exit 1\necho later'), [])
})

test('findInertGuards does not borrow an exit from after a braced fallback', () => {
  assert.deepEqual(findInertGuards('test -f tool || { echo missing; }; exit 0'), [
    { lineOffset: 0, guard: 'test -f tool || { echo missing; }; exit 0' },
  ])
})

test('findInertGuards does not treat quoted fallback diagnostics as exits', () => {
  assert.deepEqual(
    findInertGuards(`test -f tool || { printf '%s\\n' 'missing\nexit 1'; }; echo later`),
    [{ lineOffset: 0, guard: `test -f tool || { printf '%s\\n' 'missing\nexit 1'; }; echo later` }],
  )
})

test('findInertGuards reports OR guards in blocks headed by set -e', () => {
  assert.deepEqual(
    findInertGuards(
      ['# shell setup', 'set -euo pipefail', 'test -x tool || echo missing'].join('\n'),
    ),
    [{ lineOffset: 2, guard: 'test -x tool || echo missing' }],
  )
  assert.deepEqual(findInertGuards('set -e; test -x tool || echo missing'), [
    { lineOffset: 0, guard: 'set -e; test -x tool || echo missing' },
  ])
})

test('findInertGuards accepts an errexit-terminating OR fallback', () => {
  for (const fallback of ['false', '{ false; }', '( false )']) {
    const body = ['set -e', `test -f missing || ${fallback}`, 'echo later'].join('\n')
    assert.deepEqual(findInertGuards(body), [], fallback)
  }
})

test('findInertGuards does not treat a negated fallback as errexit-terminating', () => {
  const body = ['set -e', 'test -f missing || ! true', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 1, guard: 'test -f missing || ! true' }])
})

test('findInertGuards accepts fallback assignments that clamp values', () => {
  const clamp = '[ "$value" -ge 1 ] || value=$minimum'
  assert.deepEqual(findInertGuards(`${clamp}\necho later`), [])

  const unsafe = '[ -f required ] || echo missing\necho later'
  assert.deepEqual(findInertGuards(unsafe), [
    { lineOffset: 0, guard: '[ -f required ] || echo missing' },
  ])
})

test('findInertGuards ignores documentation placeholder fallbacks', () => {
  assert.deepEqual(findInertGuards('[ ... ] || <stop — see below>\necho later'), [])

  const unsafe = '[ -f required ] || echo missing\necho later'
  assert.deepEqual(findInertGuards(unsafe), [
    { lineOffset: 0, guard: '[ -f required ] || echo missing' },
  ])
})

test('findInertGuards recognizes equivalent errexit option forms', () => {
  for (const option of ['set -o errexit', 'set -E -e', 'shopt -s -o errexit']) {
    assert.deepEqual(findInertGuards(`${option}\ntest -f tool\necho later`), [], option)
  }
})

test('findInertGuards requires shopt -o for set options', () => {
  const invalidErrexit = ['shopt -s errexit', 'test -f missing', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(invalidErrexit), [{ lineOffset: 1, guard: 'test -f missing' }])

  const invalidPipefail = [
    'shopt -s pipefail',
    'set -e',
    'test -f missing | cat',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(invalidPipefail), [
    { lineOffset: 2, guard: 'test -f missing | cat' },
  ])
})

test('findInertGuards respects shopt disabling errexit', () => {
  const body = ['set -e', 'shopt -u -o errexit', 'test -f missing', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f missing' }])
})

test('findInertGuards respects reordered and combined shopt errexit disables', () => {
  for (const option of ['shopt -o -u errexit', 'shopt -ou errexit']) {
    const body = ['set -e', option, 'test -f missing', 'echo later'].join('\n')
    assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f missing' }], option)
  }
})

test('findInertGuards tracks pipefail changes made through shopt', () => {
  assert.deepEqual(
    findInertGuards(
      ['set -e', 'shopt -s -o pipefail', 'test -f missing | cat', 'echo later'].join('\n'),
    ),
    [],
  )
  assert.deepEqual(
    findInertGuards(
      [
        'set -e',
        'set -o pipefail',
        'shopt -u -o pipefail',
        'test -f missing | cat',
        'echo later',
      ].join('\n'),
    ),
    [{ lineOffset: 3, guard: 'test -f missing | cat' }],
  )
})

test('findInertGuards keeps subshell option changes out of the parent shell', () => {
  assert.deepEqual(findInertGuards('( set -e )\ntest -f missing\necho later'), [
    { lineOffset: 1, guard: 'test -f missing' },
  ])
  assert.deepEqual(findInertGuards('set -e\n( set +e )\ntest -f missing\necho later'), [])
})

test('findInertGuards keeps multiline subshell option changes out of the parent shell', () => {
  assert.deepEqual(
    findInertGuards(['set -e', '(', 'set +e', ')', 'test -f missing', 'echo later'].join('\n')),
    [],
  )
  assert.deepEqual(
    findInertGuards(['(', 'set -e', ')', 'test -f missing', 'echo later'].join('\n')),
    [{ lineOffset: 3, guard: 'test -f missing' }],
  )
})

test('findInertGuards keeps multiline subshell function definitions out of the parent shell', () => {
  const body = [
    'set -e',
    '(',
    '  disable() { set +e; }',
    ')',
    'disable || true',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards reports OR guards despite set -e', () => {
  assert.deepEqual(findInertGuards('set -e\ntest -x tool || echo missing\necho later'), [
    { lineOffset: 1, guard: 'test -x tool || echo missing' },
  ])
})

test('findInertGuards ignores guard-looking heredoc payloads', () => {
  assert.deepEqual(
    findInertGuards(["cat <<'EOF'", 'test -x tool || echo missing', 'EOF'].join('\n')),
    [],
  )
})

test('findInertGuards keeps multiline quoted strings inert', () => {
  assert.deepEqual(findInertGuards('echo "quoted\ntest -f tool\n"\necho later'), [])
  assert.deepEqual(findInertGuards('test -f tool || {\n  echo "}"\n  exit 1\n}\necho later'), [])
  assert.deepEqual(findInertGuards('test -f tool || { echo missing;\n  exit 1\n}\necho later'), [])
})

test('findInertGuards rejects exits scoped to a subshell fallback', () => {
  assert.deepEqual(findInertGuards('test -f tool || { (exit 1); echo continued; }; echo later'), [
    { lineOffset: 0, guard: 'test -f tool || { (exit 1); echo continued; }; echo later' },
  ])
})

test('findInertGuards rejects exits piped or backgrounded inside a fallback', () => {
  for (const body of [
    'test -f tool || { exit 1 | cat; echo continued; }; echo outer',
    'test -f tool || { exit 1 & wait; echo continued; }; echo outer',
  ]) {
    assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: body }], body)
  }
})

test('findInertGuards rejects direct exits piped or backgrounded after a guard', () => {
  for (const body of [
    'test -f tool || exit 1 | cat; echo continued',
    'test -f tool || exit 1 & wait; echo continued',
  ]) {
    assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: body }], body)
  }
})

test('findInertGuards accepts a direct exit before later operators', () => {
  assert.deepEqual(findInertGuards('test -f tool || exit 1; echo later'), [])
})

test('findInertGuards rejects a braced fallback piped or backgrounded after its closing brace', () => {
  for (const body of [
    'test -f tool || { exit 1; } | cat; echo continued',
    'test -f tool || { return 1; } & wait; echo continued',
  ]) {
    assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: body }], body)
  }
})

test('findInertGuards rejects a braced fallback whose terminator is conditional', () => {
  const body = 'test -f tool || { echo missing || exit 1; }; echo continued'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: body }])
})

test('findInertGuards keeps a logical operator across a physical newline', () => {
  for (const operator of ['false &&', 'true ||', 'true |']) {
    const body = [
      'test -f tool || { ' + operator,
      'exit 1',
      'echo continued',
      '}; echo outer',
    ].join('\n')
    assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: body }], operator)
  }
})

test('findInertGuards accepts an unconditional exit after a fallback list', () => {
  assert.deepEqual(findInertGuards('test -f tool || { echo missing || true\nexit 1\n}'), [])
})

test('findInertGuards rejects fallback exits nested in compound commands', () => {
  for (const body of [
    'test -f tool || { if false; then; exit 1; fi; echo continued; }; echo outer',
    'test -f tool || { while false; do exit 1; done; echo continued; }; echo outer',
    'test -f tool || { until true; do exit 1; done; echo continued; }; echo outer',
    'test -f tool || { for x in; do exit 1; done; echo continued; }; echo outer',
    'test -f tool || { select x in; do exit 1; done; echo continued; }; echo outer',
    'test -f tool || { { exit 1; } | cat; echo continued; }; echo outer',
    'test -f tool || { case x in x) exit 1;; esac; echo continued; }; echo outer',
    'test -f tool || { function stop { exit 1; }; echo continued; }; echo outer',
    'test -f tool || { stop() { exit 1; }; echo continued; }; echo outer',
  ]) {
    assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: body }], body)
  }
})

test('findInertGuards keeps descriptor redirections on terminating commands', () => {
  assert.deepEqual(findInertGuards('test -f tool || { exit 1 2>&1; }'), [])
  assert.deepEqual(findInertGuards('test -f tool || { return 1 <&0; }'), [])
})

test('findInertGuards ignores comment braces while finding a fallback terminator', () => {
  assert.deepEqual(findInertGuards('test -f tool || {\n  # explain }\n  exit 1\n}\necho later'), [])
})

test('findInertGuards joins comments before a braced fallback', () => {
  assert.deepEqual(findInertGuards('test -f tool ||\n# explain\n{ exit 1; }\necho later'), [])
})

test('findInertGuards keeps multiline conditional predicates out of top-level guard analysis', () => {
  assert.deepEqual(
    findInertGuards(
      [
        'if',
        '  test -f optional || echo missing',
        'then',
        '  echo ready',
        'fi',
        'test -f required || echo missing',
        'echo later',
      ].join('\n'),
    ),
    [{ lineOffset: 5, guard: 'test -f required || echo missing' }],
  )
})

test('findInertGuards recognizes inline conditional terminators', () => {
  const body = [
    'if test -f optional; then echo ready; fi',
    'test -f required || echo missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [
    { lineOffset: 1, guard: 'test -f required || echo missing' },
  ])
})

test('findInertGuards inspects an inline conditional body', () => {
  const body = 'if true; then test -f missing; echo later; fi'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: 'test -f missing; echo later' }])
})

test('findInertGuards resumes after an inline conditional closer', () => {
  const body = 'if true; then echo ready; fi; test -f required || echo missing; echo later'
  assert.deepEqual(findInertGuards(body), [
    { lineOffset: 0, guard: 'test -f required || echo missing; echo later' },
  ])
})

test('findInertGuards resumes after a braced inline conditional body', () => {
  const body = 'if true; then { :; }; fi; test -f required || echo missing; echo later'
  assert.deepEqual(findInertGuards(body), [
    { lineOffset: 0, guard: 'test -f required || echo missing; echo later' },
  ])
})

test('findInertGuards resumes after inline loop closers', () => {
  for (const opener of ['while false', 'until true']) {
    const body = `${opener}; do :; done; test -f required || echo missing; echo later`
    assert.deepEqual(findInertGuards(body), [
      { lineOffset: 0, guard: 'test -f required || echo missing; echo later' },
    ])
  }
})

test('findInertGuards resumes through consecutive inline compounds', () => {
  const body =
    'if true; then :; fi; while false; do :; done; test -f required || echo missing; echo later'
  assert.deepEqual(findInertGuards(body), [
    { lineOffset: 0, guard: 'test -f required || echo missing; echo later' },
  ])
})

test('findInertGuards ignores quoted and commented inline compound closers', () => {
  assert.deepEqual(
    findInertGuards(
      'if true; then :; fi; echo "data; fi; test -f fake || echo missing"; echo later',
    ),
    [],
  )
  assert.deepEqual(
    findInertGuards('if true; then :; fi; # fi; test -f fake || echo missing\necho later'),
    [],
  )
})

test('findInertGuards does not treat another conditional branch as later flow', () => {
  const body = ['if true; then', 'test -f tool', 'else', 'false', 'fi'].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards recognizes conditionals after function and group prefixes', () => {
  for (const body of [
    ['check() { if true; then', 'test -f tool', 'else', 'echo fallback', 'fi', '}'].join('\n'),
    ['{ if true; then', 'test -f tool', 'else', 'echo fallback', 'fi', '}'].join('\n'),
  ]) {
    assert.deepEqual(findInertGuards(body), [], body)
  }
})

test('findInertGuards analyzes command substitutions', () => {
  assert.deepEqual(findInertGuards('value=$(command -v tool || echo missing)'), [
    { lineOffset: 0, guard: 'command -v tool || echo missing' },
  ])
})

test('findInertGuards ignores comment parentheses in command substitutions', () => {
  const body = ['value=$(', '# explanation (', 'test -f tool', 'echo later', ')'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f tool' }])
})

test('findInertGuards analyzes legacy backtick substitutions', () => {
  assert.deepEqual(findInertGuards('value=`command -v tool || echo missing`'), [
    { lineOffset: 0, guard: 'command -v tool || echo missing' },
  ])
})

test('findInertGuards stops at comments after command separators', () => {
  assert.deepEqual(findInertGuards('echo ok;# note || { echo ignored; }'), [])
})

test('findInertGuards resets errexit inside command substitutions', () => {
  assert.deepEqual(findInertGuards('set -e; value=$(test -f tool; echo later); echo outer'), [
    { lineOffset: 0, guard: 'test -f tool; echo later' },
  ])
})

test('findInertGuards reports a final substitution assertion when the enclosing body continues', () => {
  assert.deepEqual(findInertGuards('value=$(test -f tool)\necho later'), [
    { lineOffset: 0, guard: 'test -f tool' },
  ])
})

test('findInertGuards reports bare guards followed on the same command list', () => {
  for (const body of [
    'test -n "$X"; export X',
    'echo pre; test -n "$X"\nexport X',
    'test -f tool && echo found; echo later',
    'test -f tool && echo found\necho later',
  ]) {
    assert.equal(findInertGuards(body).length, 1, body)
  }
  assert.deepEqual(findInertGuards('test -f tool && echo found'), [])
  assert.deepEqual(findInertGuards('test -f tool && echo found || exit 1'), [])
})

test('findInertGuards preserves a bare guard status through argumentless terminators', () => {
  const body = ['check() {', '  test -f required', '  return', '}'].join('\n')
  assert.deepEqual(findInertGuards(body), [])
  assert.deepEqual(findInertGuards(body.replace('  return', '  return 0')), [
    { lineOffset: 1, guard: 'test -f required' },
  ])
})

test('findInertGuards keeps AND-list guards subject to analysis under errexit', () => {
  const body = ['set -e', 'test -f tool && echo found', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 1, guard: 'test -f tool && echo found' }])
})

test('findInertGuards keeps non-final pipeline guards subject to analysis under errexit', () => {
  const body = 'set -e; test -f missing | cat; echo later'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: body }])
})

test('findInertGuards lets pipefail make non-final pipeline guards terminate under errexit', () => {
  const body = 'set -euo pipefail; test -f missing | cat; echo later'
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards preserves pipefail in selected compound and substitution scopes', () => {
  const conditional = [
    'set -euo pipefail',
    'if true; then',
    '  test -f missing | cat',
    '  echo later',
    'fi',
  ].join('\n')
  assert.deepEqual(findInertGuards(conditional), [])

  const substitution = [
    'set -euo pipefail',
    'shopt -s inherit_errexit',
    'value=$(test -f missing | cat; echo later)',
  ].join('\n')
  assert.deepEqual(findInertGuards(substitution), [])
})

test('findInertGuards propagates errexit exemptions into grouped OR branches', () => {
  const body = ['set -e', '{ test -f missing; echo later; } || echo recovered'].join('\n')
  assert.deepEqual(findInertGuards(body), [
    { lineOffset: 1, guard: '{ test -f missing; echo later; } || echo recovered' },
  ])
})

test('findInertGuards keeps negated guards subject to analysis under errexit', () => {
  const body = ['set -e', '! test -f tool', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 1, guard: '! test -f tool' }])
})

test('findInertGuards selects literal negated conditional branches', () => {
  const skipped = [
    'set -e',
    'if ! true; then',
    '  set +e',
    'fi',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(skipped), [])

  const selected = [
    'set -e',
    'if ! false; then',
    '  set +e',
    'fi',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(selected), [{ lineOffset: 4, guard: 'test -f missing' }])
})

test('findInertGuards accepts a bare guard as the final command in a compound construct', () => {
  assert.deepEqual(findInertGuards('if true; then\n  test -f tool\nfi'), [])
  assert.deepEqual(findInertGuards('{ test -f tool; }'), [])
})

test('findInertGuards inspects guards after same-line braced group openers', () => {
  const body = '{ test -f missing; echo continued; }'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: body }])
})

test('findInertGuards does not use top-level commands after a function declaration', () => {
  const body = ['check_tool() {', '  test -f tool', '}', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards does not apply options declared in an uninvoked function', () => {
  const body = [
    'helper() {',
    '  set -e',
    '  test -f helper-input',
    '  echo helper-continued',
    '}',
    'test -f missing',
    'echo continued',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 5, guard: 'test -f missing' }])
})

test('findInertGuards uses the option state at a function invocation', () => {
  assert.deepEqual(
    findInertGuards(
      ['set -e', 'check() {', 'test -f missing', 'echo later', '}', 'set +e', 'check'].join('\n'),
    ),
    [{ lineOffset: 2, guard: 'test -f missing' }],
  )
  assert.deepEqual(
    findInertGuards(
      ['check() {', 'test -f missing', 'echo later', '}', 'set -e', 'check'].join('\n'),
    ),
    [],
  )
})

test('findInertGuards preserves option changes from a conditional predicate function', () => {
  const body = [
    'set -e',
    'disable() { set +e; }',
    'if true && disable; then :; fi',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 3, guard: 'test -f missing' }])
})

test('findInertGuards keeps backgrounded function options out of the outer shell', () => {
  const body = [
    'set -e',
    'disable() { set +e; }',
    'disable & wait || true',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards keeps inline function options out of the outer shell', () => {
  const body = 'helper() { set -e; }\ntest -f missing\necho later'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 1, guard: 'test -f missing' }])
})

test('findInertGuards distinguishes function braces from fallback groups', () => {
  const body = 'test -f tool || { function helper { :; }; exit 1; }'
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards scopes functions with parenthesized bodies', () => {
  assert.deepEqual(findInertGuards('check_tool() (\n  test -f tool\n)\necho later'), [])
})

test('findInertGuards reports substitution assertions masked by same-line fallthrough', () => {
  for (const body of [
    'value=$(test -f tool); echo later',
    'value=$(test -f tool) || echo fallback',
    'value=$(test -f tool) && echo found; echo later',
    'value=$(test -f tool) && echo found || echo fallback',
  ]) {
    assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: 'test -f tool' }], body)
  }
  assert.deepEqual(findInertGuards('value=$(test -f tool) || exit 1'), [])
})

test('findInertGuards reports a substitution masked by a later substitution in its assignment', () => {
  const body = 'value=$(test -f tool)$(echo ok)'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: 'test -f tool' }])
})

test('findInertGuards reports a substitution followed by a command after its assignment', () => {
  const body = 'value=$(test -f tool) echo later'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: 'test -f tool' }])
})

test('findInertGuards does not treat errexit inside a substitution as block-level errexit', () => {
  assert.deepEqual(findInertGuards('value=$(set -e; test -f tool || echo missing)\necho later'), [
    { lineOffset: 0, guard: 'set -e; test -f tool || echo missing' },
  ])
})

test('findInertGuards preserves errexit in substitutions when inherit_errexit is enabled', () => {
  const body = ['set -e', 'shopt -s inherit_errexit', 'value=$(test -f missing; echo later)'].join(
    '\n',
  )
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards forwards active function definitions into command substitutions', () => {
  const body = [
    'walk() {',
    '  if test "$1" = stop; then return 0; fi',
    '  echo "$(walk stop)"',
    '}',
    'walk start',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards ignores inherited errexit in substitutions of status-tested assignments', () => {
  const body = [
    'set -e',
    'shopt -s inherit_errexit',
    'value=$(test -f missing; echo later) || exit 1',
    'echo after',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f missing; echo later' }])
})

test('findInertGuards enables inherit_errexit in POSIX mode', () => {
  const body = ['set -e -o posix', 'value=$(test -f missing; echo later)'].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards respects a later disabling of errexit', () => {
  const body = ['set -e; set +e', 'test -f missing', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 1, guard: 'test -f missing' }])
})

test('findInertGuards applies same-line errexit changes before later guards', () => {
  const body = ['set -e', 'set +e; test -f missing; echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [
    { lineOffset: 1, guard: 'set +e; test -f missing; echo later' },
  ])
})

test('findInertGuards keeps brace-group command arguments out of preceding set options', () => {
  assert.deepEqual(findInertGuards('{ set -e; printf +e; }\ntest -f missing\necho later'), [])
  assert.deepEqual(findInertGuards('{ set +e; printf -e; }\ntest -f missing\necho later'), [
    { lineOffset: 1, guard: 'test -f missing' },
  ])
})

test('findInertGuards applies errexit options after compound command closers', () => {
  for (const body of [
    ['if true; then', '  :', 'fi; set -e', 'test -f missing', 'echo later'].join('\n'),
    ['while false; do', '  :', 'done; set -e', 'test -f missing', 'echo later'].join('\n'),
    ['case value in', '  value) : ;;', 'esac; set -e', 'test -f missing', 'echo later'].join('\n'),
  ])
    assert.deepEqual(findInertGuards(body), [], body)

  const disabled = [
    'set -e',
    'if true; then',
    '  :',
    'fi; set +e',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(disabled), [{ lineOffset: 4, guard: 'test -f missing' }])
})

test('findInertGuards applies options after a function closing delimiter', () => {
  const body = ['check() {', '  :', '}; set -e', 'test -f missing; echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards ignores option changes in skipped AND/OR branches', () => {
  assert.deepEqual(
    findInertGuards(['false && set -e', 'test -f missing', 'echo later'].join('\n')),
    [{ lineOffset: 1, guard: 'test -f missing' }],
  )
  assert.deepEqual(
    findInertGuards(['set -e', 'false && set +e', 'test -f missing', 'echo later'].join('\n')),
    [],
  )
})

test('findInertGuards carries skipped AND/OR state across physical lines', () => {
  const body = [
    'set +e',
    'command -v definitely_missing &&',
    'set -e',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 3, guard: 'test -f missing' }])
})

test('findInertGuards applies option changes in statically executed AND/OR branches', () => {
  const andBody = ['set -e', 'true && set +e', 'test -f missing', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(andBody), [{ lineOffset: 2, guard: 'test -f missing' }])

  const orBody = ['false || set +e', 'test -f missing', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(orBody), [{ lineOffset: 1, guard: 'test -f missing' }])
})

test('findInertGuards keeps pipeline option changes out of the parent shell', () => {
  const body = 'set -e | cat\ntest -f missing\necho later'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 1, guard: 'test -f missing' }])
})

test('findInertGuards only applies option builtins at their command word', () => {
  assert.deepEqual(findInertGuards("printf '%s\\n' set -e\ntest -f missing\necho later"), [
    { lineOffset: 1, guard: 'test -f missing' },
  ])
  assert.deepEqual(
    findInertGuards("set -e\nprintf '%s\\n' set +e\ntest -f missing\necho later"),
    [],
  )
})

test('findInertGuards keeps backgrounded option changes out of the parent shell', () => {
  assert.deepEqual(findInertGuards('set -e & wait\ntest -f missing\necho later'), [
    { lineOffset: 1, guard: 'test -f missing' },
  ])
  assert.deepEqual(findInertGuards('set -e\nset +e & wait\ntest -f missing\necho later'), [])
})

test('findInertGuards treats background operators as guard fallthrough', () => {
  const body = 'test -f missing & echo later'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: body }])
})

test('findInertGuards treats errexit-enabled background guards as fallthrough', () => {
  for (const body of [
    'set -e; test -f missing & echo later',
    ['set -e; test -f missing &', 'echo later'].join('\n'),
  ]) {
    assert.equal(findInertGuards(body).length, 1, body)
  }
})

test('findInertGuards scopes option changes in for and select loop bodies', () => {
  for (const loop of ['for x in; do', 'select x in; do']) {
    assert.deepEqual(findInertGuards(`${loop} set -e; done\ntest -f missing\necho later`), [
      { lineOffset: 1, guard: 'test -f missing' },
    ])
    assert.deepEqual(
      findInertGuards(`set -e\n${loop} set +e; done\ntest -f missing\necho later`),
      [],
    )
  }
})

test('findInertGuards applies option changes within selected conditional and case branches', () => {
  const conditional = ['if true; then', '  set -e', '  test -f missing', '  echo later', 'fi'].join(
    '\n',
  )
  assert.deepEqual(findInertGuards(conditional), [])

  const caseBody = [
    'case value in',
    '  value)',
    '    set -e',
    '    test -f missing',
    '    echo later',
    '    ;;',
    'esac',
  ].join('\n')
  assert.deepEqual(findInertGuards(caseBody), [])
})

test('findInertGuards closes while and until option scopes at done', () => {
  for (const loop of ['while false; do :; done', 'until true; do :; done']) {
    assert.deepEqual(findInertGuards(`${loop}\nset -e\ntest -f missing\necho later`), [])
    assert.deepEqual(findInertGuards(`set -e\n${loop}\nset +e\ntest -f missing\necho later`), [
      { lineOffset: 3, guard: 'test -f missing' },
    ])
  }
})

test('findInertGuards applies option changes in brace groups', () => {
  assert.deepEqual(findInertGuards(['{ set -e; }', 'test -f missing', 'echo later'].join('\n')), [])
  assert.deepEqual(
    findInertGuards(['set -e', '{ set +e; }', 'test -f missing', 'echo later'].join('\n')),
    [{ lineOffset: 2, guard: 'test -f missing' }],
  )
})

test('findInertGuards advances options per command inside inline brace groups', () => {
  assert.deepEqual(
    findInertGuards(['{ set -e; printf +e; }', 'test -f missing; echo later'].join('\n')),
    [],
  )
  assert.deepEqual(
    findInertGuards(['{ set +e; printf -e; }', 'test -f missing; echo later'].join('\n')),
    [{ lineOffset: 1, guard: 'test -f missing; echo later' }],
  )
})

test('findInertGuards applies option changes from conditional predicates', () => {
  const body = ['set -e', 'if set +e; then', '  :', 'fi', 'test -f missing', 'echo later'].join(
    '\n',
  )
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 4, guard: 'test -f missing' }])
})

test('findInertGuards ignores option changes from unselected conditional branches', () => {
  const body = ['set -e', 'if false; then set +e; fi', 'test -f missing', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards carries option changes out of a statically selected conditional branch', () => {
  const body = ['set -e', 'if true; then set +e; fi', 'test -f missing', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f missing' }])
})

test('findInertGuards ignores option changes from nonmatching case arms', () => {
  const body = ['set -e', 'case x in', 'y) set +e;;', 'esac', 'test -f missing', 'echo later'].join(
    '\n',
  )
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards treats multiline elif predicates as conditional syntax', () => {
  const body = [
    'set -e',
    'if false; then',
    '  :',
    'elif',
    '  test -f optional || echo missing',
    'then',
    '  :',
    'fi',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards applies option changes after multiline compound closers', () => {
  assert.deepEqual(
    findInertGuards(
      ['if true; then', '  :', 'fi; set -e', 'test -f missing; echo later'].join('\n'),
    ),
    [],
  )
  assert.deepEqual(
    findInertGuards(
      ['set -e', 'if true; then', '  :', 'fi; set +e', 'test -f missing; echo later'].join('\n'),
    ),
    [{ lineOffset: 4, guard: 'test -f missing; echo later' }],
  )
  assert.deepEqual(
    findInertGuards(
      ['while false; do', '  :', 'done; set -e', 'test -f missing; echo later'].join('\n'),
    ),
    [],
  )
  assert.deepEqual(
    findInertGuards(
      ['set -e', 'case x in', '  x) :;;', 'esac; set +e', 'test -f missing; echo later'].join('\n'),
    ),
    [{ lineOffset: 4, guard: 'test -f missing; echo later' }],
  )
})

test('findInertGuards keeps options before an attached else in the prior branch', () => {
  const body = ['if false; then', 'set -e; else', 'test -f missing', 'echo later', 'fi'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f missing' }])
})

test('findInertGuards applies options after an attached else to the else branch', () => {
  const body = ['if false; then', ':; else set -e', 'test -f missing', 'echo later', 'fi'].join(
    '\n',
  )
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards promotes options from a selected inline else branch', () => {
  const body = [
    'set -e',
    'if false; then :; else set +e; fi',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f missing' }])

  const sameLine = 'set -e; if false; then :; else set +e; fi; test -f missing; echo later'
  assert.deepEqual(findInertGuards(sameLine), [
    { lineOffset: 0, guard: 'test -f missing; echo later' },
  ])
})

test('findInertGuards applies options after an attached case terminator to the next arm', () => {
  const body = [
    'case z in',
    'y) :;; *) set -e',
    'test -f missing',
    'echo later',
    ';;',
    'esac',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])

  const disabled = [
    'set -e',
    'if false; then',
    ':; else set +e',
    'test -f missing',
    'echo later',
    'fi',
  ].join('\n')
  assert.deepEqual(findInertGuards(disabled), [{ lineOffset: 3, guard: 'test -f missing' }])
})

test('findInertGuards clears errexit for functions invoked from AND/OR lists', () => {
  const body = ['set -e', 'check() { test -f missing; echo later; }', 'check || exit 1'].join('\n')
  assert.deepEqual(findInertGuards(body), [
    { lineOffset: 1, guard: 'test -f missing; echo later;' },
  ])

  const finalCall = ['set -e', 'check() { test -f missing; echo later; }', 'false || check'].join(
    '\n',
  )
  assert.deepEqual(findInertGuards(finalCall), [])
})

test('findInertGuards clears errexit for negated function calls', () => {
  const body = ['set -e', 'check() { test -f missing; echo later; }', '! check'].join('\n')
  assert.deepEqual(findInertGuards(body), [
    { lineOffset: 1, guard: 'test -f missing; echo later;' },
  ])
})

test('findInertGuards resolves outer definitions in nested function calls', () => {
  const body = [
    'outer() { inner; }',
    'inner() { test -f missing; echo later; }',
    'set -e',
    'outer',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards clears errexit for functions invoked as conditional predicates', () => {
  const body = 'set -e; check() { set -e; test -f missing; echo later; }; if check; then :; fi'
  assert.deepEqual(findInertGuards(body), [
    { lineOffset: 0, guard: 'set -e; test -f missing; echo later;' },
  ])
})

test('findInertGuards inspects every conditional predicate function without recursing forever', () => {
  const predicateList =
    'set -e; check() { set -e; test -f missing; echo later; }; if true && check; then :; fi'
  assert.deepEqual(findInertGuards(predicateList), [
    { lineOffset: 0, guard: 'set -e; test -f missing; echo later;' },
  ])

  const recursivePredicate =
    'walk() { if test "$1" = stop; then return 0; fi; if walk stop; then :; fi; }; walk start'
  assert.doesNotThrow(() => findInertGuards(recursivePredicate))
})

test('findInertGuards inspects substitutions in conditional predicates', () => {
  const body =
    'set -e; shopt -s inherit_errexit; if value=$(test -f missing; echo later); then :; fi'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: 'test -f missing; echo later' }])
})

test('findInertGuards ignores option changes from a short-circuited predicate function', () => {
  const body = [
    'set -e',
    'disable() { set +e; }',
    'if false && disable; then :; fi',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards does not resolve command operands as functions', () => {
  const body = [
    'set -e',
    'disable() { set +e; }',
    'command disable || true',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards resolves functions only after their declaration', () => {
  const body = [
    'future_fn || true',
    'future_fn() { set -e; }',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f missing' }])
})

test('findInertGuards carries direct function option changes into the caller', () => {
  const disabled = [
    'set -e',
    'disable() { set +e; }',
    'disable',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(disabled), [{ lineOffset: 3, guard: 'test -f missing' }])

  const enabled = ['enable() { set -e; }', 'enable', 'test -f missing', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(enabled), [])
})

test('findInertGuards carries caller continuation into function bodies', () => {
  const body = 'check() { test -f missing; }; check; echo later'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: 'test -f missing;' }])
})

test('findInertGuards stops function option tracking after an unconditional return', () => {
  const body = [
    'set -e',
    'disable() { return 0; set +e; }',
    'disable',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards carries conditional function option changes into the caller', () => {
  const body = [
    'set -e',
    'disable() { set +e; }',
    'disable && true',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 3, guard: 'test -f missing' }])
})

test('findInertGuards carries option changes from a guaranteed AND/OR function RHS', () => {
  const body = [
    'set -e',
    'disable() { set +e; }',
    'true && disable',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 3, guard: 'test -f missing' }])
})

test('findInertGuards carries option changes from a semicolon-separated function call', () => {
  const body = [
    'set -e',
    'disable() { set +e; }',
    'echo ready; disable',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 3, guard: 'test -f missing' }])
})

test('findInertGuards applies only guaranteed predicate option effects', () => {
  const body = [
    'set -e',
    'enable() { set -e; }',
    'if enable && set +e; then :; fi',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards preserves enabled errexit across tested function calls', () => {
  const body = [
    'set -e',
    'probe() { :; }',
    'if probe; then :; fi',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards ignores predicate RHS option effects that cannot execute', () => {
  const body = [
    'set -e',
    'if test 1 = 2 && set +e; then :; fi',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards ignores function definitions in statically skipped branches', () => {
  const body = [
    'set -e',
    'if false; then',
    '  disable() { set +e; }',
    'fi',
    'disable || true',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards ignores definitions in statically nonmatching case arms', () => {
  const body = [
    'set -e',
    'case x in',
    'y)',
    'disable() { set +e; }',
    ';;',
    'esac',
    'disable || true',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards skips statically short-circuited predicate functions', () => {
  const body = [
    'set -e',
    'check() { set -e; test -f missing; echo later; }',
    'if false && check; then :; fi',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards keeps errexit ignored in tested compound bodies', () => {
  const body = 'set -e; if true; then test -f missing; echo later; fi || exit 1'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: 'test -f missing; echo later' }])
})

test('findInertGuards preserves options when a function call is short-circuited', () => {
  const body = 'set -e; disable() { set +e; }; false && disable; test -f missing; echo later'
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards reports guards inside conditional predicate lists', () => {
  const body = 'set -e; if test -f missing; echo later; then :; fi'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: 'test -f missing; echo later' }])
})

test('findInertGuards keeps multiline tested compounds errexit-exempt', () => {
  const body = ['set -e', 'if true; then', 'test -f missing', 'echo later', 'fi || exit 1'].join(
    '\n',
  )
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f missing' }])
})

test('findInertGuards keeps multiline tested brace and subshell groups errexit-exempt', () => {
  for (const [open, close] of [
    ['(', ')'],
    ['{', '}'],
  ]) {
    const body = [
      'set -e',
      open,
      'set -e',
      'test -f missing',
      'echo later',
      `${close} || true`,
    ].join('\n')
    assert.deepEqual(findInertGuards(body), [{ lineOffset: 3, guard: 'test -f missing' }])
  }
})

test('findInertGuards keeps redirection-bearing tested group closers errexit-exempt', () => {
  for (const [open, close, redirection] of [
    ['(', ')', '2>/dev/null'],
    ['{', '}', '>out'],
  ]) {
    const body = [
      'set -e',
      open,
      'test -f missing',
      'echo later',
      `${close} ${redirection} || true`,
    ].join('\n')
    assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f missing' }])
  }
})

test('findInertGuards inspects guards inside one-line subshell groups', () => {
  assert.deepEqual(findInertGuards('( test -f missing; echo later )'), [
    { lineOffset: 0, guard: 'test -f missing; echo later' },
  ])
})

test('findInertGuards resolves direct function calls after case-arm patterns', () => {
  const body = 'set -e; check() { test -f missing; echo later; }; case x in x) check;; esac'
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards keeps non-final pipeline compound bodies errexit-exempt', () => {
  const body = ['set -e', 'if true; then', 'test -f missing', 'echo later', 'fi | cat'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f missing' }])
})

test('findInertGuards keeps functions before the final pipeline command errexit-exempt', () => {
  const body = ['set -e', 'check() { test -f missing; }', 'check | cat', 'echo outer'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 1, guard: 'test -f missing;' }])
})

test('findInertGuards preserves errexit for functions in the final pipeline command', () => {
  const body = ['set -e', 'check() { test -f missing; echo later; }', 'echo x | check'].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards does not apply effects from a conditional function RHS', () => {
  const body = [
    'set -e',
    'disable() { set +e; }',
    'test 1 = 2 && disable',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards terminates recursive function inspection', () => {
  const body = 'set -e; recurse() { recurse; }; recurse'
  assert.doesNotThrow(() => findInertGuards(body))
})

test('findInertGuards stops parsing set options after --', () => {
  const body = ['set -- -e', 'test -f missing; echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 1, guard: 'test -f missing; echo later' }])
})

test('findInertGuards does not apply later inherit_errexit to earlier substitutions', () => {
  const body = ['set -e', 'value=$(test -f missing; echo later)', 'shopt -s inherit_errexit'].join(
    '\n',
  )
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 1, guard: 'test -f missing; echo later' }])
})

test('findInertGuards accepts exits in nested brace groups', () => {
  assert.deepEqual(
    findInertGuards('test -f tool || { { exit 1; }; echo continued; }; echo outer'),
    [],
  )
})

test('findInertGuards treats case-arm terminators as structural closers', () => {
  const body = ['case "$value" in', '*)', 'test -f tool', ';;', 'esac'].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards accepts a guard ended by an attached case terminator', () => {
  const body = ['case "$value" in', '*)', 'test -f tool;;', 'esac'].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards recognizes assignment-prefixed bare guards', () => {
  const body = ['MODE=strict test -f required', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [
    { lineOffset: 0, guard: 'MODE=strict test -f required' },
  ])
})

test('findInertGuards recognizes assignment-prefixed OR guards', () => {
  const body = 'MODE=strict test -f required || echo missing'
  assert.deepEqual(findInertGuards(body), [
    { lineOffset: 0, guard: 'MODE=strict test -f required || echo missing' },
  ])
})

test('findInertGuards scopes later commands to their case arm', () => {
  const body = [
    'case "$value" in',
    'first)',
    'test -f tool',
    ';;',
    '*)',
    'echo fallback',
    ';;',
    'esac',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])

  const disabled = [
    'set -e',
    'case z in',
    'y) :;; *) set +e',
    'test -f missing',
    'echo later',
    ';;',
    'esac',
  ].join('\n')
  assert.deepEqual(findInertGuards(disabled), [{ lineOffset: 3, guard: 'test -f missing' }])
})

test('findInertGuards recognizes guards after same-line case arm patterns', () => {
  const body = ['case x in', 'x) test -f missing', 'echo later', ';;', 'esac'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 1, guard: 'x) test -f missing' }])
})

test('findInertGuards starts a new case arm after an attached terminator', () => {
  const body = [
    'case x in',
    'x) :; set -e;;',
    '*) test -f missing',
    'echo later',
    ';;',
    'esac',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: '*) test -f missing' }])
})

test('findInertGuards recognizes case scopes after a function brace prefix', () => {
  const body = [
    'helper() { case "$value" in',
    'first)',
    'test -f tool',
    ';;',
    '*)',
    'echo fallback',
    ';;',
    'esac; }',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [])
})

test('findInertGuards reports substitution exits when the outer body continues', () => {
  assert.deepEqual(findInertGuards('value=$(test -f tool || exit 1)\necho later'), [
    { lineOffset: 0, guard: 'test -f tool || exit 1' },
  ])
})

test('findInertGuards preserves an outer substitution assignment exit guard', () => {
  assert.deepEqual(findInertGuards('value=$(test -f required) || exit 1\necho later'), [])
})

test('findInertGuards preserves an outer substitution assignment guard after redirections', () => {
  assert.deepEqual(findInertGuards('value=$(test -f required) >out || exit 1\necho later'), [])
  assert.deepEqual(findInertGuards('value=$(test -f required) >out 2>&1 || exit 1\necho later'), [])
})

test('findInertGuards does not promote exits from nested conditionals in brace fallbacks', () => {
  const body = 'test -f tool || { { if false; then exit 1; fi; }; echo continued; }; echo outer'
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 0, guard: body }])
})

test('findInertGuards keeps fallthrough case arms in one flow', () => {
  const body = ['case x in', 'x)', 'test -f missing', ';&', '*)', 'echo later', ';;', 'esac'].join(
    '\n',
  )
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 2, guard: 'test -f missing' }])
})

test('findInertGuards treats commands after a conditional as later flow', () => {
  const body = ['if true; then', 'test -f missing', 'fi', 'echo later'].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 1, guard: 'test -f missing' }])
})

test('findInertGuards resumes after a multiline elif predicate', () => {
  const body = [
    'if false; then',
    ':',
    'elif',
    'false',
    'then',
    ':',
    'fi',
    'test -f missing',
    'echo later',
  ].join('\n')
  assert.deepEqual(findInertGuards(body), [{ lineOffset: 7, guard: 'test -f missing' }])
})

test('findInertGuards reports assertions masked by an enclosing command', () => {
  assert.deepEqual(findInertGuards('echo "$(test -f required)"'), [
    { lineOffset: 0, guard: 'test -f required' },
  ])
})

test('checkSkillShellInRepo reports inert guards even when bash is optional', async () => {
  const repoRoot = makeRepo({
    [claudeSkill('inert-guard')]: md('```bash', 'test -x tool || echo missing', '```'),
  })
  try {
    const findings = await checkSkillShellInRepo(repoRoot, {
      env: { BOSS_SKILL_SHELL_OPTIONAL: '1' },
      hasBash: () => false,
      warn: () => {},
    })
    assert.deepEqual(findings, [
      {
        file: '.claude/skills/inert-guard/SKILL.md',
        line: 2,
        kind: 'inert-guard',
        message:
          'test -x tool || echo missing can fall through after failure; add set -e or exit/return in its failure branch',
      },
    ])
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 2. >=-length closing, four-backtick.
test('a four-backtick fence is not closed by an inner three-backtick block', () => {
  const blocks = extractFencedBlocks(
    md(
      '# Code Reviewer Prompt Template',
      '',
      '````',
      'Subagent (general-purpose):',
      '',
      '```bash',
      'echo inner',
      '```',
      '',
      'done',
      '````',
      '',
      'after',
    ),
  )
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].info, '')
  assert.equal(blocks[0].terminated, true)
  assert.ok(blocks[0].body.includes('echo inner'))
  assert.ok(blocks[0].body.includes('```bash'))
})

// 3. >=-length closing, three-backtick.
test('a three-backtick fence IS closed by a four-backtick run (>=, not ==)', () => {
  const blocks = extractFencedBlocks(md('```typescript', 'expect(x).toBe(3);', '````', 'after'))
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].terminated, true)
  assert.equal(blocks[0].body, 'expect(x).toBe(3);')
})

// 4. Unterminated.
test('an unterminated fence yields terminated:false and an unterminated finding', async () => {
  const blocks = extractFencedBlocks(md('```bash', 'echo hi', '', 'more prose'))
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].terminated, false)

  const repoRoot = makeRepo({
    [claudeSkill('broken')]: md('prose', '```bash', 'echo hi', '', 'trailing prose'),
  })
  try {
    const findings = await checkSkillShellInRepo(repoRoot)
    const unterminated = findings.filter((f) => f.kind === 'unterminated')
    assert.equal(unterminated.length, 1)
    assert.equal(unterminated[0].line, 2)
    assert.match(unterminated[0].message, /UNTERMINATED fence/)
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 5. List dedent + heredoc.
test('a 5-space-indented fence whose heredoc terminator carries the indent parses clean', async () => {
  const dedented = md(
    '3. After implementing changes:',
    '   - Commit with reference to review feedback:',
    '',
    '     ```bash',
    `     git commit -m "$(cat <<'EOF'`,
    '     fix(review): address feedback',
    '',
    '     EOF',
    '     )"',
    '     ```',
  )
  // The same body WITHOUT the dedent: what a naive extractor hands to bash.
  const control = md(
    '```bash',
    `git commit -m "$(cat <<'EOF'`,
    'fix(review): address feedback',
    '',
    '     EOF',
    '     )"',
    '```',
  )
  const repoRoot = makeRepo({
    [claudeSkill('indented')]: dedented,
    [claudeSkill('control')]: control,
  })
  try {
    const findings = await checkSkillShellInRepo(repoRoot)
    const files = findings.map((f) => f.file)
    assert.ok(
      !files.some((f) => f.includes('indented')),
      `dedented block should parse clean, got ${JSON.stringify(findings)}`,
    )
    assert.ok(
      files.some((f) => f.includes('control')),
      'the un-dedented control must fail, otherwise this test cannot fail',
    )
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 6. Blockquote container.
test('a blockquoted fence is extracted, its "> " stripped, and it parses clean', async () => {
  const source = md(
    '> **Dispatch prompt:**',
    '>',
    '> ```bash',
    '> if [ -f x ]; then',
    '>   echo yes',
    '> fi',
    '> ```',
  )
  const blocks = extractFencedBlocks(source)
  // A whitespace-only extractor returns [] here; asserting the block IS present pins the fix.
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].info, 'bash')
  assert.equal(blocks[0].prefix, '> ')
  assert.equal(blocks[0].body, 'if [ -f x ]; then\n  echo yes\nfi')

  const repoRoot = makeRepo({ [claudeSkill('quoted')]: source })
  try {
    assert.deepEqual(await checkSkillShellInRepo(repoRoot), [])
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 6b. A closer must stay in the opener's container.
test('a bare closing fence does not close a blockquoted fence', () => {
  // Markdown ends the blockquote at the un-prefixed line and reads the bare ``` as a NEW fence,
  // swallowing the rest of the document. Stripping each line's own prefix would instead report this
  // block as terminated and clean — the exact malformed-fence class the gate exists to catch.
  const blocks = extractFencedBlocks(
    md('> ```bash', '> echo hi', '```', '## Swallowed heading', 'prose'),
  )
  assert.equal(blocks.length, 2)
  // The quoted block ends WITH its container, which CommonMark treats as a complete code block, so
  // the un-prefixed lines stay out of the shell body. The bare fence is a new opener, and it is the
  // one left unterminated — the finding is still raised, against the fence that is actually broken.
  assert.equal(
    blocks[0].terminated,
    true,
    'the depth-1 fence ends with its blockquote, not on the depth-0 closer',
  )
  assert.equal(blocks[0].body, 'echo hi', 'the un-quoted lines are outside the quoted block')
  assert.equal(blocks[1].startLine, 3)
  assert.equal(blocks[1].terminated, false, 'the bare fence opens an unterminated block')
  assert.ok(
    !blocks.some((block) => block.terminated && block.body.includes('Swallowed')),
    'no terminated block may carry the swallowed document as shell',
  )

  // The mirror case: a `> `-prefixed closer must not close a fence opened outside the quote.
  const inverse = extractFencedBlocks(md('```bash', 'echo hi', '> ```', 'prose'))
  assert.equal(inverse.length, 1)
  assert.equal(inverse[0].terminated, false, 'a depth-1 closer must not close a depth-0 fence')
})

// 6c. Container matching is on blockquote depth, not on the prefix bytes.
test('whitespace variation inside the same blockquote container still closes the fence', () => {
  const blocks = extractFencedBlocks(md('>  ```bash', '> echo hi', '>```'))
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].terminated, true, 'same quote depth, different padding, must still close')
  assert.equal(blocks[0].info, 'bash')
})

// 6d. A closer may be indented at most three columns past its opener.
test('a closing fence indented four columns past the opener does not close it', () => {
  // CommonMark allows at most three spaces before a closing fence; at four it is code-block content
  // and the fence stays open, swallowing the document. Consuming unlimited indentation would call
  // that terminated and clean.
  const four = extractFencedBlocks(md('```bash', 'echo hi', '    ```', 'prose'))
  assert.equal(four.length, 1)
  assert.equal(four[0].terminated, false)

  const three = extractFencedBlocks(md('```bash', 'echo hi', '   ```', 'prose'))
  assert.equal(three[0].terminated, true, 'three columns is still a valid closer')

  // A tab counts to the next 4-column stop, so a lone leading tab is already too far.
  assert.equal(extractFencedBlocks(md('```bash', 'echo hi', '\t```'))[0].terminated, false)

  // The limit is RELATIVE to the opener: a 5-space list fence closed at 5 spaces is fine. The list
  // marker is load-bearing — a fence that deep is only a fence inside a container (see 6f).
  const listed = extractFencedBlocks(md('1. step', '', '     ```bash', '     echo hi', '     ```'))
  assert.equal(listed[0].terminated, true)
  assert.equal(
    extractFencedBlocks(md('1. step', '', '     ```bash', '     echo hi', '         ```'))[0]
      .terminated,
    false,
    'four columns past a 5-space opener is still too far',
  )
})

// 6e. A closer must stay inside the opener's LIST container, not just under a max indent.
test('a bare top-level closer does not close a list-indented fence', () => {
  // A fence indented four columns or more cannot be top level — at that indent an unfenced line is
  // an indented code block — so it necessarily sits in a list container. A closer dedented out of
  // that container exits the list in Markdown and opens a NEW top-level fence, swallowing the rest
  // of the document, while an upper bound alone reports the block terminated and clean.
  const blocks = extractFencedBlocks(
    md('1. item', '', '     ```bash', '     echo hi', '```', '## Swallowed', 'prose'),
  )
  assert.equal(blocks.length, 2)
  assert.equal(blocks[0].terminated, true, 'the nested block ends with the list item that holds it')
  assert.equal(blocks[0].body, 'echo hi', 'the dedented lines are outside the nested block')
  assert.equal(blocks[1].terminated, false, 'the dedented fence opens an unterminated block')
  assert.ok(
    !blocks.some((block) => block.terminated && block.body.includes('Swallowed')),
    'no terminated block may carry the swallowed document as shell',
  )

  // Controls: the window is the CONTAINER's content column, not a tolerance around the opener.
  // `1. item` puts content at column 3, so a closer at 3 closes even though the opener sits at 5 —
  // while one at 2 has left the list and does not. An opener-relative tolerance got the second of
  // these wrong, accepting any closer within three columns of the opener.
  assert.equal(
    extractFencedBlocks(md('1. item', '', '     ```bash', '     echo hi', '   ```'))[0].terminated,
    true,
    'the container content column closes it',
  )
  // Below the container column the line has exited the list, so it is not this block's closer at
  // all: it opens its own, and that one is what stays unterminated.
  const exited = extractFencedBlocks(md('1. item', '', '     ```bash', '     echo hi', '  ```'))
  assert.equal(exited.length, 2)
  assert.equal(exited[0].body, 'echo hi')
  assert.equal(exited[1].terminated, false)
  // A genuinely top-level fence (indent <= 3) is unconstrained below, as Markdown allows.
  assert.equal(extractFencedBlocks(md('   ```bash', 'echo hi', '```'))[0].terminated, true)
})

// 6f. An opener four columns deep is only a fence inside a list container.
test('a deeply indented fence with no list container is not a fence opener', async () => {
  // Four columns past top level is an INDENTED CODE BLOCK: the ```bash is literal text being shown,
  // often deliberately-malformed sample shell. Extracting it hands that sample to bash and rejects
  // valid Markdown.
  assert.deepEqual(
    extractFencedBlocks(md('prose', '', '    ```bash', '    if [ -f x ]; then', '    ```', '')),
    [],
  )

  // The same depth under a list item IS a fence — this is the shape the tree actually uses.
  const listed = extractFencedBlocks(md('1. step', '', '     ```bash', '     echo hi', '     ```'))
  assert.equal(listed.length, 1)
  assert.equal(listed[0].info, 'bash')
  assert.equal(listed[0].body, 'echo hi')

  // A list closes at a non-blank line back in column 0, so the fence below is no longer sheltered.
  assert.deepEqual(
    extractFencedBlocks(md('1. step', '', 'back to prose', '', '     ```bash', '     echo hi')),
    [],
  )

  // End to end: the indented sample is malformed shell, and must NOT be reported.
  const repoRoot = makeRepo({
    [claudeSkill('sample')]: md(
      'Shown as literal indented code:',
      '',
      '    ```bash',
      '    if [ -f x ]; then',
      '    ```',
    ),
  })
  try {
    assert.deepEqual(await checkSkillShellInRepo(repoRoot), [])
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 6g. The closer's bound is the opener's CONTAINER column, not a tolerance around the opener.
test('a bare top-level closer does not close a shallow list-nested fence', () => {
  // `- item` puts content at column 2, so a 2-space fence sits inside the item. A tolerance of
  // three columns around the OPENER makes the lower bound 0 here and accepts a top-level closer;
  // Markdown instead exits the list, leaves the fence unterminated, and reads the bare fence as a
  // new top-level opener that swallows the document.
  const blocks = extractFencedBlocks(
    md('- item', '', '  ```bash', '  echo hi', '```', '## Swallowed', 'prose'),
  )
  assert.equal(blocks.length, 2)
  assert.equal(blocks[0].terminated, true, 'the nested block ends with the list item that holds it')
  assert.equal(blocks[0].body, 'echo hi', 'the dedented lines are outside the nested block')
  assert.equal(blocks[1].terminated, false, 'the dedented fence opens an unterminated block')
  assert.ok(
    !blocks.some((block) => block.terminated && block.body.includes('Swallowed')),
    'no terminated block may carry the swallowed document as shell',
  )

  // Inside the container it closes normally, at the content column or up to three past it.
  assert.equal(
    extractFencedBlocks(md('- item', '', '  ```bash', '  x', '  ```'))[0].terminated,
    true,
  )
  assert.equal(
    extractFencedBlocks(md('- item', '', '  ```bash', '  x', '     ```'))[0].terminated,
    true,
  )
  assert.equal(
    extractFencedBlocks(md('- item', '', '  ```bash', '  x', '      ```'))[0].terminated,
    false,
    'four columns past the content column is indented code, not a closer',
  )
})

// 6h. Indentation BEFORE a container marker counts too.
test('indentation before a blockquote marker is validated, not skipped', () => {
  // The `>` itself sits four columns deep, so CommonMark reads the whole line as indented code —
  // a blockquote example being shown, not a blockquote. Measuring only after the last marker sees
  // one trailing space and extracts the deliberately-malformed sample inside it.
  assert.deepEqual(
    extractFencedBlocks(md('prose', '', '    > ```bash', '    > if [ -f x ]; then', '    > ```')),
    [],
  )
  // An ordinary blockquoted fence is unaffected.
  assert.equal(extractFencedBlocks(md('> ```bash', '> echo hi', '> ```'))[0].terminated, true)
})

// 6i. The standard `- ```bash ` form puts the fence on the marker's own line.
test('a fence on the same line as its list marker is recognised', () => {
  // Matching FENCE_OPEN against the unconsumed `- ```bash ` misses the opener entirely, and the
  // indented closer then reads as a new unterminated opener — so a valid, properly closed block
  // fails the gate.
  const blocks = extractFencedBlocks(md('- ```bash', '  echo hi', '  ```', 'after'))
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].info, 'bash')
  assert.equal(blocks[0].terminated, true)
  assert.equal(blocks[0].body, 'echo hi', 'the item content column is dedented off the body')

  // Numbered markers and nested indentation behave the same.
  const numbered = extractFencedBlocks(
    md('1. ```bash', '   if [ -f x ]; then', '     echo hi', '   fi', '   ```'),
  )
  assert.equal(numbered.length, 1)
  assert.equal(numbered[0].terminated, true)
  assert.equal(numbered[0].body, 'if [ -f x ]; then\n  echo hi\nfi')
})

// 6j. A list marker is only a container if it is itself within the window.
test('an indented-code list marker does not open a list container', () => {
  // `    - sample` at top level is indented code, not a list. Pushing it as a container would
  // legitimise the 6-space fence below it and extract the sample shell being shown.
  assert.deepEqual(
    extractFencedBlocks(
      md('prose', '', '    - sample', '', '      ```bash', '      if [ -f x ]; then', '      ```'),
    ),
    [],
  )
  // A marker that IS within the window still opens one.
  assert.equal(
    extractFencedBlocks(md('- sample', '', '  ```bash', '  echo hi', '  ```'))[0].terminated,
    true,
  )
})

// 6k. Nested blockquote markers may carry indentation.
test('a nested blockquote marker may be indented from the outer one', () => {
  // `>  > ```bash ` is a valid nested quote; a grammar allowing only one space after each `>` fails
  // to see the second marker, so the block is never extracted and its shell bypasses the gate.
  const blocks = extractFencedBlocks(md('>  > ```bash', '>  > echo hi', '>  > ```'))
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].info, 'bash')
  assert.equal(blocks[0].terminated, true)
  assert.equal(blocks[0].body, 'echo hi')
})

// 6l. The post-marker space and the nested marker's indentation are additive.
test('a nested blockquote marker may sit four columns after the outer one', () => {
  // CommonMark consumes one optional space after `>` and THEN allows the nested marker up to three
  // columns of indentation, so four spaces between markers is valid.
  const blocks = extractFencedBlocks(md('>    > ```bash', '>    > echo hi', '>    > ```'))
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].info, 'bash')
  assert.equal(blocks[0].terminated, true)
  assert.equal(blocks[0].body, 'echo hi')
})

// 6m. CRLF checkouts.
test('CRLF line endings do not defeat fence matching', async () => {
  // `.gitattributes` sets no `text`/`eol` normalization, so a `core.autocrlf=true` checkout hands
  // this scanner CRLF files. A trailing `\r` fails FENCE_CLOSE's `[ \t]*$`, so every block reads as
  // unterminated and the whole tree turns red on line endings alone.
  const blocks = extractFencedBlocks('prose\r\n\r\n```bash\r\necho hi\r\n```\r\nafter\r\n')
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].info, 'bash')
  assert.equal(blocks[0].terminated, true)
  assert.equal(blocks[0].body, 'echo hi')

  // End to end: a CRLF skill file with valid shell must produce no findings at all.
  const repoRoot = makeRepo({
    [claudeSkill('crlf')]:
      '# Title\r\n\r\n```bash\r\nif [ -f x ]; then\r\n  echo hi\r\nfi\r\n```\r\n',
  })
  try {
    assert.deepEqual(await checkSkillShellInRepo(repoRoot), [])
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 7. Tilde fences.
test('tilde fences are extracted and do not close backtick fences (or vice versa)', () => {
  const tilde = extractFencedBlocks(md('~~~bash', 'echo hi', '~~~'))
  assert.equal(tilde.length, 1)
  assert.equal(tilde[0].info, 'bash')
  assert.equal(tilde[0].terminated, true)
  assert.equal(tilde[0].body, 'echo hi')

  const mixedA = extractFencedBlocks(md('```bash', 'echo hi', '~~~', 'still inside'))
  assert.equal(mixedA.length, 1)
  assert.equal(mixedA[0].terminated, false)
  assert.ok(mixedA[0].body.includes('~~~'))

  const mixedB = extractFencedBlocks(md('~~~bash', 'echo hi', '```', 'still inside'))
  assert.equal(mixedB.length, 1)
  assert.equal(mixedB[0].terminated, false)
  assert.ok(mixedB[0].body.includes('```'))
})

// 8. Placeholders.
test('normalizePlaceholders rewrites <…> words but leaves real redirection syntax intact', () => {
  assert.equal(normalizePlaceholders('git add <exact-files>'), 'git add PLACEHOLDER')
  assert.equal(
    normalizePlaceholders('boss chat show <session-id|chat-id>'),
    'boss chat show PLACEHOLDER',
  )
  assert.equal(normalizePlaceholders('echo <changed test file(s), and more>'), 'echo PLACEHOLDER')
  assert.equal(
    normalizePlaceholders("git commit -m \"$(cat <<'EOF'"),
    "git commit -m \"$(cat <<'EOF'",
  )
  assert.equal(normalizePlaceholders('cat <<<"herestring"'), 'cat <<<"herestring"')
  assert.equal(normalizePlaceholders('sort < infile'), 'sort < infile')
  assert.equal(normalizePlaceholders('diff <(a) <(b)'), 'diff <(a) <(b)')

  // A heredoc/herestring that ALSO redirects on the same line: the `>` is a match candidate for a
  // `<…>` starting at the heredoc's SECOND `<`. Rewriting it destroys the heredoc operator and
  // re-parses the literal body as live shell — a false POSITIVE the convention forbids.
  assert.equal(
    normalizePlaceholders("cat <<'EOF' > out.txt"),
    "cat <<'EOF' > out.txt",
    'a redirecting heredoc must survive normalization intact',
  )
  assert.equal(normalizePlaceholders('cat <<EOF >> log'), 'cat <<EOF >> log')
  assert.equal(normalizePlaceholders('cat <<<"x" > out.txt'), 'cat <<<"x" > out.txt')
  assert.equal(normalizePlaceholders('diff <(a) <(b) > d.txt'), 'diff <(a) <(b) > d.txt')
})

// 8b. The same defect end-to-end: a VALID redirecting heredoc whose literal body looks like shell.
test('a valid heredoc that redirects on its opening line is not reported as a syntax error', async () => {
  const repoRoot = makeRepo({
    [claudeSkill('heredoc')]: md(
      '```bash',
      "cat <<'EOF' > out.txt",
      'if [ -f x ]; then',
      'EOF',
      '```',
    ),
  })
  try {
    assert.deepEqual(
      await checkSkillShellInRepo(repoRoot),
      [],
      'bash -n accepts this block; the gate must not invent a syntax error by mangling the heredoc',
    )
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 9. Info-string gate.
test('only bash/sh/shell/zsh info strings are handed to bash -n', async () => {
  assert.deepEqual([...SHELL_INFO_STRINGS].sort(), ['bash', 'sh', 'shell', 'zsh'])
  assert.deepEqual(SKILL_ROOTS, ['services/boss/internal/skillinstall/skills', '.claude/skills'])

  const brokenShell = 'if [ -f x ]; then'
  const repoRoot = makeRepo({
    [claudeSkill('nonshell')]: md(
      '```json',
      brokenShell,
      '```',
      '```go',
      brokenShell,
      '```',
      '```text',
      brokenShell,
      '```',
      '```console',
      brokenShell,
      '```',
      '```',
      brokenShell,
      '```',
    ),
    [claudeSkill('shell')]: md('```bash', brokenShell, '```'),
  })
  try {
    const findings = await checkSkillShellInRepo(repoRoot)
    assert.equal(findings.length, 1)
    assert.ok(findings[0].file.includes('shell'))
    assert.ok(!findings[0].file.includes('nonshell'))
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 10. Genuine syntax error.
test('a genuine syntax error is reported with its file, fence line and kind:syntax', async () => {
  const repoRoot = makeRepo({
    [claudeSkill('bad')]: md('prose', '', '```bash', 'if [ -f x ]; then', 'echo hi', '```'),
  })
  try {
    const findings = await checkSkillShellInRepo(repoRoot)
    assert.equal(findings.length, 1)
    assert.equal(findings[0].kind, 'syntax')
    assert.equal(findings[0].line, 3)
    assert.match(findings[0].file, /bad[/\\]SKILL\.md$/)
    assert.ok(findings[0].message.length > 0)
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 11. Glob rule — positive.
test('findMultiGlobRemovals flags an rm carrying two unquoted globs', () => {
  const findings = findMultiGlobRemovals(
    'rm -f ".linear-plans/<ID>-child-"*.md ".linear-plans/<ID>"*.image-guard-*.md',
  )
  assert.equal(findings.length, 1)
  assert.equal(findings[0].command, 'rm')
  assert.equal(findings[0].lineOffset, 0)
  assert.deepEqual(findings[0].globs, [
    '".linear-plans/<ID>-child-"*.md',
    '".linear-plans/<ID>"*.image-guard-*.md',
  ])
})

// 12. Glob rule — continuation.
test('backslash continuations are joined so a split rm yields ONE finding with three globs', () => {
  const body = [
    'rm -f ".linear-plans/<ID>-child-"*.md ".linear-plans/<ID>"*.image-guard-*.md \\',
    '  ".linear-plans/<ID>"*.attachment-headers-*.json',
    'echo done',
  ].join('\n')
  const findings = findMultiGlobRemovals(body)
  assert.equal(findings.length, 1)
  assert.equal(findings[0].lineOffset, 0)
  assert.equal(findings[0].globs.length, 3)
})

// 13. Glob rule — quoted negative.
test('quoted find -name patterns are not globs and are never flagged', () => {
  assert.deepEqual(
    findMultiGlobRemovals(
      'find . -maxdepth 3 -type f \\( -name AGENTS.md -o -name Makefile -o -name "*.yml" -o -name "*.yaml" \\)',
    ),
    [],
  )
})

// 14. Glob rule — BOS-631 fix form negative.
test("BOS-631's single-quoted find -name fix form is not flagged", () => {
  assert.deepEqual(
    findMultiGlobRemovals(
      "[ -d .linear-plans ] && find .linear-plans -maxdepth 1 -type f -name '<ID>-child-*.md' -delete || true",
    ),
    [],
  )
  assert.deepEqual(
    findMultiGlobRemovals(
      "if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ID>*.md' -delete || RC=1; fi",
    ),
    [],
  )
})

// 14b. Glob rule — heredoc payloads are text, not commands.
test('a multi-glob line inside a heredoc payload is not flagged', () => {
  // The skill tree builds PR bodies and prompts with heredocs; the shell never expands those words.
  assert.deepEqual(
    findMultiGlobRemovals(
      ["cat <<'EOF' > /tmp/prompt.md", 'rm -f build/*.log tmp/*.tmp', 'EOF'].join('\n'),
    ),
    [],
  )
  // Unquoted and tab-stripping (`<<-`) delimiters behave the same.
  assert.deepEqual(
    findMultiGlobRemovals(['cat <<-EOF', '\trm -f build/*.log tmp/*.tmp', '\tEOF'].join('\n')),
    [],
  )

  // Controls, so the skip cannot be mistaken for the rule going silent: the SAME line outside a
  // heredoc is still flagged, and the rule resumes after the terminator.
  const outside = findMultiGlobRemovals('rm -f build/*.log tmp/*.tmp')
  assert.equal(outside.length, 1)

  const resumed = findMultiGlobRemovals(
    ["cat <<'EOF' > /tmp/prompt.md", 'inert payload', 'EOF', 'rm -f build/*.log tmp/*.tmp'].join(
      '\n',
    ),
  )
  assert.equal(resumed.length, 1, 'the rule must resume once the heredoc terminates')
  assert.equal(resumed[0].lineOffset, 3)

  // A herestring is not a heredoc and must not swallow the following line.
  const herestring = findMultiGlobRemovals(['cat <<<"x"', 'rm -f a*.log b*.tmp'].join('\n'))
  assert.equal(herestring.length, 1)
  assert.equal(herestring[0].lineOffset, 1)
})

// 14c. Glob rule — `$( )` is part of the word, not a segment terminator.
test('a command substitution does not split the rm segment', () => {
  const findings = findMultiGlobRemovals('rm -f $(make-prefix) build/*.log tmp/*.tmp')
  assert.equal(findings.length, 1, 'the globs sit in the same rm segment as the substitution')
  assert.equal(findings[0].command, 'rm')
  assert.deepEqual(findings[0].globs, ['build/*.log', 'tmp/*.tmp'])

  // Nested substitution, and one containing an unbalanced paren inside quotes.
  assert.equal(findMultiGlobRemovals('rm -f $(a $(b)) x*.log y*.tmp').length, 1)
  assert.equal(findMultiGlobRemovals('rm -f $(echo "a)b") x*.log y*.tmp').length, 1)

  // A genuine subshell still terminates the segment: each rm below carries one glob.
  assert.deepEqual(findMultiGlobRemovals('(rm -f a*.log) && rm -f b*.tmp'), [])
})

// 14d. Glob rule — `<<-` strips TABS only, matching the shell.
test('a <<- heredoc terminator is matched on tabs only, never spaces', () => {
  // bash does not accept a space-indented `EOF` as a `<<-` terminator, so the payload continues and
  // the `rm` below is inert text. Ending the heredoc there would report a valid block as failing —
  // a false POSITIVE, the one direction this gate must never take.
  assert.deepEqual(
    findMultiGlobRemovals(['cat <<-EOF', ' EOF', 'rm -f a*.log b*.tmp', 'EOF'].join('\n')),
    [],
  )

  // A tab-indented terminator DOES end it, so the rule is not simply going quiet.
  const ended = findMultiGlobRemovals(['cat <<-EOF', '\tEOF', 'rm -f a*.log b*.tmp'].join('\n'))
  assert.equal(ended.length, 1)
  assert.equal(ended[0].lineOffset, 2)
})

// 14e. Glob rule — a `<<` inside quotes or a comment is not a heredoc operator.
test('a <<DELIM inside quotes or a comment does not start a heredoc', () => {
  // A bogus heredoc blanks the real `rm` below it and silently drops the finding.
  for (const decoy of ["echo '<<EOF'", 'echo "<<EOF"', '# example: <<EOF']) {
    const findings = findMultiGlobRemovals([decoy, 'rm -f a*.log b*.tmp'].join('\n'))
    assert.equal(findings.length, 1, `\`${decoy}\` must not open a heredoc`)
    assert.equal(findings[0].lineOffset, 1)
  }

  // Controls: a real heredoc still masks, and a QUOTED delimiter is still a real heredoc.
  assert.deepEqual(
    findMultiGlobRemovals(['cat <<EOF', 'rm -f a*.log b*.tmp', 'EOF'].join('\n')),
    [],
  )
  assert.deepEqual(
    findMultiGlobRemovals(["cat <<'EOF'", 'rm -f a*.log b*.tmp', 'EOF'].join('\n')),
    [],
  )
})

// 14f. Glob rule — `$( )` resumes shell parsing even inside double quotes.
test('a heredoc inside a double-quoted command substitution is still masked', () => {
  // This is the shape the skill tree actually uses to build commit messages and PR bodies. Treating
  // the opening `"` as swallowing the rest of the line hides the `<<`, so the literal payload is
  // analyzed as commands — a false POSITIVE on the corpus's most common heredoc form.
  assert.deepEqual(
    findMultiGlobRemovals(['payload="$(cat <<EOF', 'rm -f a*.log b*.tmp', 'EOF', ')"'].join('\n')),
    [],
  )
  assert.deepEqual(
    findMultiGlobRemovals(
      ["git commit -m \"$(cat <<'EOF'", 'rm -f a*.log b*.tmp', 'EOF', ')"'].join('\n'),
    ),
    [],
  )

  // Still quote-aware: a `<<` that never leaves the double quotes is not an operator.
  const decoy = findMultiGlobRemovals(['echo "<<EOF"', 'rm -f a*.log b*.tmp'].join('\n'))
  assert.equal(decoy.length, 1)
})

// 14g. Glob rule — bracket expressions are globs too.
test('bracket expressions count as unquoted globs', () => {
  // zsh/fish abort the whole line on an unmatched bracket pattern exactly as for `*`/`?`.
  const findings = findMultiGlobRemovals('rm -f report.[12] temp.[ab]')
  assert.equal(findings.length, 1)
  assert.deepEqual(findings[0].globs, ['report.[12]', 'temp.[ab]'])

  // Quoted brackets are matched by the program, not expanded by the shell — never flagged.
  assert.deepEqual(findMultiGlobRemovals('rm -f "report.[12]" "temp.[ab]"'), [])
  // An array subscript inside a parameter expansion is not a glob.
  assert.deepEqual(findMultiGlobRemovals('rm -f ${a[0]} ${b[1]}'), [])
  // An unclosed `[` is not a bracket expression.
  assert.deepEqual(findMultiGlobRemovals('rm -f report.[12 temp.[ab'), [])
})

// 14h. Glob rule — `<<` inside an arithmetic expansion is a shift, not a heredoc.
test('an arithmetic shift is not a heredoc operator', () => {
  // A bogus heredoc here blanks the real `rm` below it, so the rule returns nothing while
  // `bash -n` passes — a finding lost with no trace.
  const substituted = findMultiGlobRemovals(['echo $((1 << 2))', 'rm -f a*.log b*.tmp'].join('\n'))
  assert.equal(substituted.length, 1)
  assert.equal(substituted[0].lineOffset, 1)

  const bare = findMultiGlobRemovals(['((x = 1 << 2))', 'rm -f a*.log b*.tmp'].join('\n'))
  assert.equal(bare.length, 1)
  assert.equal(bare[0].lineOffset, 1)
})

// 14i. Glob rule — a delimiter word may mix quoted and unquoted pieces.
test('a heredoc delimiter split across quotes is read as one word', () => {
  // bash removes the quotes and terminates on `EOF`; reading only `E` means the terminator is never
  // found and every later command in the block is silently blanked.
  assert.deepEqual(
    findMultiGlobRemovals(["cat <<'E'OF", 'rm -f a*.log b*.tmp', 'EOF'].join('\n')),
    [],
  )
  const after = findMultiGlobRemovals(
    ["cat <<'E'OF", 'payload', 'EOF', 'rm -f a*.log b*.tmp'].join('\n'),
  )
  assert.equal(after.length, 1, 'the terminator must be recognised so the rule resumes')
  assert.equal(after[0].lineOffset, 3)
})

// 14j. Glob rule — `${…}` is parameter expansion, not pathname expansion.
test('glob characters inside a parameter expansion are not pathname globs', () => {
  // Array subscripts and pattern operators are not expanded against the filesystem, so they cannot
  // exhibit the unmatched-glob abort — flagging them rejects valid shell.
  assert.deepEqual(findMultiGlobRemovals('rm -f ${left[*]} ${right[*]}'), [])
  assert.deepEqual(findMultiGlobRemovals('rm -f ${value#*} ${other%*}'), [])

  // A glob OUTSIDE the expansion still counts, so the guard cannot silence the rule wholesale.
  const real = findMultiGlobRemovals('rm -f ${dir}/a*.log ${dir}/b*.tmp')
  assert.equal(real.length, 1)
  assert.deepEqual(real[0].globs, ['${dir}/a*.log', '${dir}/b*.tmp'])
})

// 14k. An unterminated heredoc — the one defect `bash -n` does NOT report.
test('an unterminated heredoc is reported even though bash -n exits 0', async () => {
  // Measured: `bash -n` on a body of `cat <<EOF` exits 0 on both bash 5.3.9 (which prints a
  // "warning: here-document ... delimited by end-of-file" to stderr) and bash 3.2.57 (which prints
  // nothing at all). Exit status alone therefore cannot see this, and stderr alone is not portable.
  // The block's own heredoc scanner decides it instead, identically on every bash.
  assert.deepEqual(findUnterminatedHeredoc('cat <<EOF\necho hi'), { delim: 'EOF', lineOffset: 0 })
  assert.equal(findUnterminatedHeredoc('cat <<EOF\necho hi\nEOF'), null)
  assert.equal(findUnterminatedHeredoc('echo hi'), null)

  const repoRoot = makeRepo({
    [claudeSkill('hd')]: md('prose', '```bash', 'cat <<EOF', 'echo hi', '```'),
    [claudeSkill('ok')]: md('```bash', 'cat <<EOF', 'echo hi', 'EOF', '```'),
  })
  try {
    const findings = await checkSkillShellInRepo(repoRoot)
    assert.equal(
      findings.length,
      1,
      `expected exactly one finding, got ${JSON.stringify(findings)}`,
    )
    assert.equal(findings[0].kind, 'heredoc')
    assert.match(findings[0].file, /hd[/\\]SKILL\.md$/)
    assert.equal(findings[0].line, 3, 'points at the line carrying the unterminated operator')
    assert.match(findings[0].message, /EOF/)
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 14l. Glob rule — a quoted string spanning lines keeps its `<<` quoted.
test('quote state carries across lines so a quoted << is not a heredoc', () => {
  const body = ['echo "line one', '<<EOF still inside the string', '"', 'rm -f a*.log b*.tmp'].join(
    '\n',
  )

  // Resetting quote state per line reads that `<<EOF` as an operator, which now costs twice: a
  // bogus `heredoc` finding on a valid block, AND the real removal below it masked away.
  assert.equal(findUnterminatedHeredoc(body), null)
  const findings = findMultiGlobRemovals(body)
  assert.equal(findings.length, 1)
  assert.equal(findings[0].lineOffset, 3)

  // Controls: a real heredoc after a closed multiline string is still found, and a real heredoc
  // opened on a continued line still masks its payload.
  assert.deepEqual(
    findMultiGlobRemovals(['echo "one', '"', 'cat <<EOF', 'rm -f a*.log b*.tmp', 'EOF'].join('\n')),
    [],
  )
  assert.deepEqual(findUnterminatedHeredoc(['echo "one', '"', 'cat <<EOF'].join('\n')), {
    delim: 'EOF',
    lineOffset: 2,
  })
})

// 14m. Glob rule — a removal used directly as a control-flow condition.
test('a removal used as a control-flow condition is still checked', () => {
  // `then`, `do`, `else` and `elif` were already skipped; the keywords that OPEN the construct were
  // not, so the segment began with `if` and the `rm` after it was never examined.
  const conditional = findMultiGlobRemovals('if rm -f a*.log b*.tmp; then echo ok; fi')
  assert.equal(conditional.length, 1)
  assert.equal(conditional[0].command, 'rm')
  assert.deepEqual(conditional[0].globs, ['a*.log', 'b*.tmp'])

  assert.equal(findMultiGlobRemovals('while rm -f a*.log b*.tmp; do :; done').length, 1)
  assert.equal(findMultiGlobRemovals('until rm -f a*.log b*.tmp; do :; done').length, 1)

  // Control: the ordinary `if [ … ]` test form still resolves to `[`, not to a removal.
  assert.deepEqual(findMultiGlobRemovals('if [ -f a*.log ]; then rm -f b*.tmp; fi'), [])
})

// 14n. Glob rule — the TOKENIZER needs the same cross-line quote state as the heredoc scanner.
test('quote state carries across lines in the glob tokenizer', () => {
  // A quoted string spanning lines keeps its payload literal; tokenizing each line from a clean
  // quote state reads the middle line as a command and rejects a valid block.
  assert.deepEqual(
    findMultiGlobRemovals(['message="literal', 'rm -f a*.log b*.tmp', '"'].join('\n')),
    [],
  )
  assert.deepEqual(
    findMultiGlobRemovals(["note='literal", 'rm -f a*.log b*.tmp', "'"].join('\n')),
    [],
  )

  // Control: once the string closes, a real removal is still reported.
  const after = findMultiGlobRemovals(['message="literal', '"', 'rm -f a*.log b*.tmp'].join('\n'))
  assert.equal(after.length, 1)
  assert.equal(after[0].lineOffset, 2)
})

// 14o. Glob rule — `$'…'` is a quoting prefix, not part of the delimiter.
test('an ANSI-C quoted heredoc delimiter is read after quote removal', () => {
  // bash terminates `<<$'EOF'` on `EOF`; recording `$EOF` means the terminator never arrives, which
  // is now a reported `heredoc` finding on a block that parses and runs correctly.
  assert.equal(findUnterminatedHeredoc(["cat <<$'EOF'", 'body', 'EOF'].join('\n')), null)
  assert.deepEqual(
    findMultiGlobRemovals(["cat <<$'EOF'", 'rm -f a*.log b*.tmp', 'EOF'].join('\n')),
    [],
  )
  assert.equal(findUnterminatedHeredoc(['cat <<$"EOF"', 'body', 'EOF'].join('\n')), null)
})

// 14p. Glob rule — `<(…)` is a word, like `$(…)`.
test('a process substitution does not split the rm segment', () => {
  const findings = findMultiGlobRemovals('rm -f <(generate) a*.log b*.tmp')
  assert.equal(findings.length, 1)
  assert.equal(findings[0].command, 'rm')
  assert.deepEqual(findings[0].globs, ['a*.log', 'b*.tmp'])

  assert.equal(findMultiGlobRemovals('rm -f >(sink) a*.log b*.tmp').length, 1)
  // A genuine subshell still terminates the segment.
  assert.deepEqual(findMultiGlobRemovals('(rm -f a*.log) && rm -f b*.tmp'), [])
})

// 14q. Glob rule — a substitution left open at end of line continues the same command.
test('an unbalanced command substitution carries the command across lines', () => {
  // The `rm` and its globs sit in one invocation split by a newline inside `$(`. Ending the word at
  // the newline leaves the second line as its own segment, with no `rm` in it to check.
  const findings = findMultiGlobRemovals(
    ["rm -f $(printf '%s\\n'", 'prefix) a*.log b*.tmp'].join('\n'),
  )
  assert.equal(findings.length, 1)
  assert.equal(findings[0].command, 'rm')
  assert.deepEqual(findings[0].globs, ['a*.log', 'b*.tmp'])
  assert.equal(findings[0].lineOffset, 0, 'the finding belongs to the line carrying the rm')
})

// 14r. Glob rule — a `)` closing a nested subshell must not end the substitution.
test('a nested subshell inside $() does not end the substitution early', () => {
  // Popping at the first `)` restores the outer double quote too soon, so the quoted `<<EOF` that
  // follows is read as a heredoc operator and the valid block is failed as unterminated.
  const body = 'value="$( (true); echo "<<EOF"; echo done )"'
  assert.equal(findUnterminatedHeredoc(body), null)
  assert.deepEqual(findMultiGlobRemovals([body, 'rm -f a*.log b*.tmp'].join('\n')).length, 1)
})

// 14s. Glob rule — ANSI-C delimiters are decoded, not just unwrapped.
test('an ANSI-C heredoc delimiter is decoded before matching its terminator', () => {
  // bash decodes `$'E\x4fF'` to `EOF` and terminates there; recording the raw text reports a bogus
  // unterminated heredoc for a block that runs correctly.
  assert.equal(findUnterminatedHeredoc(["cat <<$'E\\x4fF'", 'body', 'EOF'].join('\n')), null)
  assert.deepEqual(
    findMultiGlobRemovals(["cat <<$'E\\x4fF'", 'rm -f a*.log b*.tmp', 'EOF'].join('\n')),
    [],
  )
  // Octal and named escapes decode too; a tab-bearing delimiter is still matched exactly.
  assert.equal(findUnterminatedHeredoc(["cat <<$'E\\117F'", 'body', 'EOF'].join('\n')), null)
})

// 14t. Glob rule — the legacy `$[…]` arithmetic form also holds a shift, not a heredoc.
test('legacy $[ ] arithmetic is not a heredoc operator', () => {
  const body = ['echo $[1 << 2]', 'rm -f a*.log b*.tmp'].join('\n')
  assert.equal(findUnterminatedHeredoc(body), null)
  const findings = findMultiGlobRemovals(body)
  assert.equal(findings.length, 1)
  assert.equal(findings[0].lineOffset, 1)
})

// 14u. Glob rule — a removal invoked through the `command` wrapper.
test('a removal invoked through `command` is still checked', () => {
  // The shell expands the patterns before the wrapper runs, so the abort hazard is identical.
  const wrapped = findMultiGlobRemovals('command rm -f a*.log b*.tmp')
  assert.equal(wrapped.length, 1)
  assert.equal(wrapped[0].command, 'rm')
  assert.deepEqual(wrapped[0].globs, ['a*.log', 'b*.tmp'])

  // Flag-only wrapper options are skipped on the way to the effective command.
  assert.equal(findMultiGlobRemovals('command -p rm -f a*.log b*.tmp').length, 1)
  assert.equal(findMultiGlobRemovals('builtin rm -f a*.log b*.tmp').length, 1)
  assert.equal(findMultiGlobRemovals('sudo rm -f a*.log b*.tmp').length, 1)

  // ACCEPTED false negative, pinned so it is a decision rather than a surprise: an option that takes
  // an ARGUMENT hides the command behind a non-option word. Skipping one word after every option
  // would swallow the `rm` in `command -p rm`, so telling these apart needs a per-wrapper option
  // table this rule does not carry. Erring toward missing a finding keeps the rule one-directional.
  assert.deepEqual(findMultiGlobRemovals('sudo -u ci rm -f a*.log b*.tmp'), [])

  // Control: an option-skipping wrapper must not turn a non-removal into one.
  assert.deepEqual(findMultiGlobRemovals('command -p echo a*.log b*.tmp'), [])
})

// 14v. Glob rule — an EMPTY heredoc delimiter is still an operator.
test('an empty heredoc delimiter is tracked, not dropped', () => {
  // `cat <<''` is valid and terminates on a blank line. Dropping the operator on a falsy delimiter
  // loses the one case bash also only warns about, so nothing at all would report it.
  assert.deepEqual(findUnterminatedHeredoc(["cat <<''", 'body'].join('\n')), {
    delim: '',
    lineOffset: 0,
  })
  // A blank line terminates it, so the payload is masked and the rule resumes after.
  assert.equal(findUnterminatedHeredoc(["cat <<''", 'body', ''].join('\n')), null)
  assert.deepEqual(findMultiGlobRemovals(["cat <<''", 'rm -f a*.log b*.tmp', ''].join('\n')), [])
})

// 14w. Glob rule — arithmetic expansion may span lines.
test('arithmetic expansion state carries across lines', () => {
  // `echo $((1 +` / `2 << 3))` is one expansion split by a newline; forgetting that arithmetic is
  // still open reads the second line's `<<` as a heredoc and fails a valid block.
  const body = ['echo $((1 +', '2 << 3))', 'rm -f a*.log b*.tmp'].join('\n')
  assert.equal(findUnterminatedHeredoc(body), null)
  const findings = findMultiGlobRemovals(body)
  assert.equal(findings.length, 1)
  assert.equal(findings[0].lineOffset, 2)
})

// 14x. Glob rule — `env` is a wrapper too.
test('a removal invoked through env is still checked', () => {
  assert.equal(findMultiGlobRemovals('env rm -f a*.log b*.tmp').length, 1)
  // Leading assignments were already skipped; the wrapper is what was missing.
  const assigned = findMultiGlobRemovals('env FOO=bar rm -f a*.log b*.tmp')
  assert.equal(assigned.length, 1)
  assert.equal(assigned[0].command, 'rm')
  assert.equal(findMultiGlobRemovals('env -i rm -f a*.log b*.tmp').length, 1)
})

// 14y. Glob rule — backticks resume command parsing, like `$(`.
test('a heredoc inside a backtick substitution is found', () => {
  // Legacy substitution inside a double-quoted word. Recognising only `$(` lets the outer quote
  // swallow the `<<END`, so nothing reports a heredoc that swallows the rest of the block.
  assert.deepEqual(findUnterminatedHeredoc('x="`cat <<END'), { delim: 'END', lineOffset: 0 })
  assert.equal(findUnterminatedHeredoc(['x="`cat <<END', 'payload', 'END', '`"'].join('\n')), null)
  // Inside SINGLE quotes a backtick is literal, so this is not a substitution.
  assert.equal(findUnterminatedHeredoc("x='`cat <<END'"), null)
})

// 14z. Glob rule — a delimiter word may cross an escaped newline.
test('a heredoc delimiter continued across an escaped newline is joined', () => {
  // bash removes the backslash-newline first, so the delimiter is `EOF`. Recording `EO` leaves a
  // terminator that never arrives, which is now a reported finding on a block bash accepts.
  assert.equal(findUnterminatedHeredoc(['cat <<EO\\', 'F', 'payload', 'EOF'].join('\n')), null)
  assert.deepEqual(
    findMultiGlobRemovals(['cat <<EO\\', 'F', 'rm -f a*.log b*.tmp', 'EOF'].join('\n')),
    [],
  )
})

// 14aa. Glob rule — arithmetic skipping must respect quoting.
test('arithmetic skipping ignores parens inside quotes', () => {
  // Counting the quoted `))` ends the region early, and the `<< 2` after it reads as a heredoc.
  const body = ["echo $(( $(echo '))') + 1 << 2 ))", 'rm -f a*.log b*.tmp'].join('\n')
  assert.equal(findUnterminatedHeredoc(body), null)
  const findings = findMultiGlobRemovals(body)
  assert.equal(findings.length, 1)
  assert.equal(findings[0].lineOffset, 1)
})

// 14ab. Glob rule — a braced group opener is syntax, not a command.
test('a braced group opener does not become the command word', () => {
  const grouped = findMultiGlobRemovals('{ rm -f a*.log b*.tmp; }')
  assert.equal(grouped.length, 1)
  assert.equal(grouped[0].command, 'rm')
  assert.deepEqual(grouped[0].globs, ['a*.log', 'b*.tmp'])
})

// 14ac. Glob rule — bash's backslash semantics inside a double-quoted delimiter.
test('a double-quoted heredoc delimiter honours backslash escapes', () => {
  // bash terminates `<<"E\"OF"` on `E"OF`. Treating the escaped quote as the end of the segment
  // records something else, so the terminator never arrives and a valid block is failed.
  assert.equal(findUnterminatedHeredoc(['cat <<"E\\"OF"', 'body', 'E"OF'].join('\n')), null)
  assert.deepEqual(
    findMultiGlobRemovals(['cat <<"E\\"OF"', 'rm -f a*.log b*.tmp', 'E"OF'].join('\n')),
    [],
  )
  // Single quotes are literal — a backslash there is part of the delimiter.
  assert.equal(findUnterminatedHeredoc(["cat <<'E\\OF'", 'body', 'E\\OF'].join('\n')), null)
})

// 14ad. Glob rule — a backtick substitution may span lines, like `$(`.
test('backtick substitution state carries across lines', () => {
  const findings = findMultiGlobRemovals(['rm -f `cmd', '` a*.log b*.tmp'].join('\n'))
  assert.equal(findings.length, 1)
  assert.equal(findings[0].command, 'rm')
  assert.deepEqual(findings[0].globs, ['a*.log', 'b*.tmp'])
  assert.equal(findings[0].lineOffset, 0, 'reported against the line carrying the rm')

  // Closed on one line, the globs are still counted.
  assert.equal(findMultiGlobRemovals('rm -f `cmd` a*.log b*.tmp').length, 1)
})

// 14ae. Glob rule — a delimiter word may contain substitution syntax, taken literally.
test('a delimiter word containing substitution syntax is consumed whole', () => {
  // Heredoc delimiters are not expanded, so bash's delimiter here is the literal text `$(echo)`.
  // Stopping at the `(` records `$`, and the terminator never arrives.
  assert.equal(findUnterminatedHeredoc(['cat <<$(echo)', 'body', '$(echo)'].join('\n')), null)
  assert.equal(findUnterminatedHeredoc(['cat <<`x`', 'body', '`x`'].join('\n')), null)
  assert.equal(findUnterminatedHeredoc(['cat <<$[1]', 'body', '$[1]'].join('\n')), null)
})

// 14af. Glob rule — `$[…]` holds multiplication, not pathname patterns.
test('legacy $[ ] arithmetic is not counted as a pathname glob', () => {
  // The heredoc scanner already skipped this region; the glob tokenizer did not.
  assert.deepEqual(findMultiGlobRemovals('rm -f $[1*2] c*.tmp'), [])
  // Control: two genuine globs alongside it still count.
  assert.equal(findMultiGlobRemovals('rm -f $[1*2] c*.tmp d*.log').length, 1)
})

// 14ag. Glob rule — a redirection may precede the command word.
test('leading redirections do not become the command word', () => {
  const attached = findMultiGlobRemovals('2>/dev/null rm -f a*.log b*.tmp')
  assert.equal(attached.length, 1)
  assert.equal(attached[0].command, 'rm')
  assert.deepEqual(attached[0].globs, ['a*.log', 'b*.tmp'])

  // The operator and its target may also be separate words.
  assert.equal(findMultiGlobRemovals('2> /dev/null rm -f a*.log b*.tmp').length, 1)
  assert.equal(findMultiGlobRemovals('>out rm -f a*.log b*.tmp').length, 1)

  // Control: a redirection AFTER the command must not shift which command is inspected.
  assert.deepEqual(findMultiGlobRemovals('2>/dev/null echo a*.log b*.tmp'), [])
})

// 14ah. Glob rule — `#` opens a comment only at a real word start.
test('escaped whitespace before # does not start a comment', () => {
  // `foo\ #` keeps the space AND the `#` in the word, so the `<<EOF` after it is a real operator.
  assert.deepEqual(findUnterminatedHeredoc('cat foo\\ # <<EOF'), { delim: 'EOF', lineOffset: 0 })
  // Control: an ordinary comment still ends the scan.
  assert.equal(findUnterminatedHeredoc('echo hi # <<EOF'), null)
})

// 14ai. Glob rule — a removal nested in a substitution is still a removal.
test('a removal nested in a substitution is analyzed', () => {
  // Pathname expansion happens inside the subshell too, so an unmatched pattern aborts the removal
  // exactly as it would at top level. Consuming the substitution as an opaque word hid it.
  const substituted = findMultiGlobRemovals('result=$(rm -f a*.log b*.tmp)')
  assert.equal(substituted.length, 1)
  assert.equal(substituted[0].command, 'rm')
  assert.deepEqual(substituted[0].globs, ['a*.log', 'b*.tmp'])

  assert.equal(findMultiGlobRemovals('result=`rm -f a*.log b*.tmp`').length, 1)
  // Reported against the line the substitution opens on.
  const later = findMultiGlobRemovals(['echo one', 'result=$(rm -f a*.log b*.tmp)'].join('\n'))
  assert.equal(later.length, 1)
  assert.equal(later[0].lineOffset, 1)
})

// 14aj. Glob rule — an escaped `$` is literal, not a parameter expansion.
test('an escaped dollar does not suppress a glob', () => {
  const escaped = findMultiGlobRemovals('rm -f \\${x*.log b*.tmp')
  assert.equal(escaped.length, 1)
  assert.deepEqual(escaped[0].globs, ['\\${x*.log', 'b*.tmp'])

  // Control: a genuine `${…}` still suppresses its own glob, leaving only one.
  assert.deepEqual(findMultiGlobRemovals('rm -f ${x*.log} b*.tmp'), [])
})

// 14ak. Extraction — a list inside a blockquote does not survive the blockquote.
test('a blockquoted list does not license a later top-level indented fence', () => {
  // Four-space indentation at top level is CommonMark indented CODE, not a fence. Retiring the
  // quoted list only by column leaves its content column behind and extracts the sample below —
  // which would fail the gate on a deliberately malformed shell example in a doc.
  // No intervening top-level prose: a line indented LESS than the item's content column already
  // retires it by column alone, which would make this pass whether or not the quote depth is
  // tracked. The fence must be the first thing after the quoted item for the depth to be load-bearing.
  const blocks = extractFencedBlocks(
    ['> 1. quoted item', '    ```bash', '    if [ ; then', '    ```', ''].join('\n'),
  )
  assert.deepEqual(blocks, [])
  // Control: the SAME indented fence inside a top-level list item really is a fence.
  const nested = extractFencedBlocks(
    ['1. item', '', '    ```bash', '    echo hi', '    ```', ''].join('\n'),
  )
  assert.equal(nested.length, 1)
  assert.equal(nested[0].body.trim(), 'echo hi')
})

// 14al. Heredoc — an unquoted terminator may itself be split by a line continuation.
test('a continued terminator closes an unquoted heredoc', () => {
  // bash removes the backslash-newline in an expanded body, so `EO\` + `F` is the terminator.
  assert.equal(findUnterminatedHeredoc(['cat <<EOF', 'payload', 'EO\\', 'F'].join('\n')), null)
  // Control: a QUOTED delimiter makes the body literal, so those are two payload lines and the
  // heredoc really is unterminated.
  assert.deepEqual(findUnterminatedHeredoc(["cat <<'EOF'", 'payload', 'EO\\', 'F'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
})

// 14am. Glob rule — backslash PARITY decides whether a `$` is escaped.
test('an even backslash run leaves the parameter expansion live', () => {
  // `\\` is one escaped backslash, so the `$` is live and `[*]` is an array subscript, not a glob.
  // Only `b*.tmp` is a real glob, so there is nothing to report.
  assert.deepEqual(findMultiGlobRemovals('rm -f \\\\${x[*]} b*.tmp'), [])
  // Control: an ODD run means the dollar is literal and both patterns expand (14aj covers the
  // single-backslash form; this pins the three-backslash one to the same parity rule).
  assert.equal(findMultiGlobRemovals('rm -f \\\\\\${x*.log b*.tmp').length, 1)
})

// 14an. Glob rule — `function name { … }` is a declaration, not the command.
test('a removal inside a function declaration is found', () => {
  const found = findMultiGlobRemovals('function cleanup { rm -f a*.log b*.tmp; }')
  assert.equal(found.length, 1)
  assert.equal(found[0].command, 'rm')
  assert.deepEqual(found[0].globs, ['a*.log', 'b*.tmp'])
  // The `name() { … }` form already worked; pin it so it stays working.
  assert.equal(findMultiGlobRemovals('cleanup() { rm -f a*.log b*.tmp; }').length, 1)
})

// 14ao. Extraction — ENTERING a blockquote retires top-level list state too.
test('a top-level list does not license an indented sample inside a later blockquote', () => {
  // Five columns after `>` is indented code within the quote, not a fence. Retiring only DEEPER
  // entries left the top-level item's column alive across the quote boundary.
  assert.deepEqual(
    extractFencedBlocks(
      ['1. item', '>     ```bash', '>     if [ ; then', '>     ```', ''].join('\n'),
    ),
    [],
  )
  // Controls, both directions: an ordinary blockquoted fence still extracts, and so does a fence
  // properly nested in a top-level list item.
  assert.equal(extractFencedBlocks(['> ```bash', '> echo hi', '> ```', ''].join('\n')).length, 1)
  assert.equal(
    extractFencedBlocks(['1. item', '', '    ```bash', '    echo hi', '    ```', ''].join('\n'))
      .length,
    1,
  )
})

// 14ap. Glob rule — a substitution body spanning physical lines is analyzed whole.
test('a multiline substitution body keeps its command', () => {
  // The newline falls inside the NESTED substitution, so bash sees one `rm`. Recording only the
  // line the outer substitution closes on discarded it and left two bare patterns.
  const found = findMultiGlobRemovals('x=$(rm -f $(printf x\n) a*.log b*.tmp)')
  assert.equal(found.length, 1)
  assert.equal(found[0].command, 'rm')
  assert.deepEqual(found[0].globs, ['a*.log', 'b*.tmp'])
  // Control: the single-line form is unaffected.
  assert.equal(findMultiGlobRemovals('result=$(rm -f a*.log b*.tmp)').length, 1)
})

// 14aq. Heredoc — `<<` inside a PARAMETER expansion is pattern text, not an operator.
test('a << inside ${…} does not open a heredoc', () => {
  // `echo ${x#<<EOF}` is valid bash that runs. Reading the `<<` as an operator records a delimiter
  // of `EOF}` and rejects the block — a false positive.
  assert.equal(findUnterminatedHeredoc('echo ${x#<<EOF}'), null)
  // Control: a real heredoc operator is still found.
  assert.deepEqual(findUnterminatedHeredoc(['cat <<EOF', 'payload'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
})

// 14ar. Heredoc — a delimiter substitution is balanced with quote awareness.
test('a quoted paren does not close a delimiter substitution', () => {
  // `cat <<$(printf ')')` is terminated by a literal `$(printf ')')` line. Counting the QUOTED `)`
  // as the closer records `$(printf '))` and rejects a block bash -n accepts.
  assert.equal(findUnterminatedHeredoc(["cat <<$(printf ')')", "$(printf ')')"].join('\n')), null)
})

// 14as. Glob rule — a substitution inside DOUBLE QUOTES is still command text.
test('a removal inside a double-quoted substitution is analyzed', () => {
  // The outer quotes do not suppress pathname expansion inside the substitution.
  const found = findMultiGlobRemovals('result="$(rm -f a*.log b*.tmp)"')
  assert.equal(found.length, 1)
  assert.deepEqual(found[0].globs, ['a*.log', 'b*.tmp'])
  assert.equal(findMultiGlobRemovals('result="`rm -f a*.log b*.tmp`"').length, 1)
  // Control: patterns quoted INSIDE the substitution are matched by the command, never expanded by
  // the shell, so they cannot abort it and must not be flagged.
  assert.deepEqual(findMultiGlobRemovals(`result="$(rm -f 'a*.log' 'b*.tmp')"`), [])
})

// 14at. Heredoc — single quotes keep a backslash-newline, so the delimiter word spans a line.
test('a single-quoted delimiter keeps its backslash-newline', () => {
  // Measured on bash 5.3.9: `cat <<'EO\` + newline + `F'` warns `wanted \`EO\` + newline + `F'` and
  // exits 0. Removing the pair as an ordinary line continuation reads the delimiter as `EOF`, so the
  // later `EOF` appears to close a heredoc bash keeps open and the rest of the block is accepted as
  // swallowed payload — the exact malformed block this check exists to catch.
  assert.deepEqual(findUnterminatedHeredoc(["cat <<'EO\\", "F'", 'payload', 'EOF'].join('\n')), {
    delim: 'EO\\\nF',
    lineOffset: 0,
  })
  // ANSI-C quoting behaves the same way.
  assert.deepEqual(findUnterminatedHeredoc(["cat <<$'EO\\", "F'", 'payload', 'EOF'].join('\n')), {
    delim: 'EO\\\nF',
    lineOffset: 0,
  })
  // A quoted segment left OPEN puts a newline in the delimiter for the same reason, and no single
  // body line can equal it.
  assert.deepEqual(findUnterminatedHeredoc(["cat <<'EOF", 'payload', 'EOF'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
  // Controls: bash DOES remove the pair everywhere else, so these blocks really are terminated and
  // must stay accepted.
  assert.equal(findUnterminatedHeredoc(['cat <<EO\\', 'F', 'payload', 'EOF'].join('\n')), null)
  assert.equal(findUnterminatedHeredoc(['cat <<"EO\\', 'F"', 'payload', 'EOF'].join('\n')), null)
})

// 14au. Glob rule — parameter expansions NEST, and none of their pattern text is a glob.
test('a nested parameter expansion suppresses globs to its own closing brace', () => {
  // `rm -f ${x#${y}*} b*.tmp` is valid bash carrying ONE pathname pattern: the `*` belongs to the
  // outer expansion's `#` pattern. Treating the inner `}` as the outer closer counts it as a second
  // glob and rejects a block `bash -n` accepts — a false positive, the direction this rule must
  // never fail in.
  assert.deepEqual(findMultiGlobRemovals('rm -f ${x#${y}*} b*.tmp'), [])
  assert.deepEqual(findMultiGlobRemovals('rm -f ${x:-${y#*}} ${a[*]} b*.tmp'), [])
  // Control: a glob AFTER the closing brace is still one, so two of them still report.
  const found = findMultiGlobRemovals('rm -f ${x#${y}}*.log b*.tmp')
  assert.equal(found.length, 1)
  assert.deepEqual(found[0].globs, ['${x#${y}}*.log', 'b*.tmp'])
})

// 14av. Glob rule — an option-LOOKING pattern is still a pattern.
test('a pattern spelled like an option is counted', () => {
  // `zsh -c 'rm -f -- -* a*.tmp'` aborts with `no matches found: -*`. Pathname expansion has no idea
  // the word resembles a flag, so discarding leading-`-` words hid the whole class.
  const found = findMultiGlobRemovals('rm -f -- -* a*.tmp')
  assert.equal(found.length, 1)
  assert.deepEqual(found[0].globs, ['-*', 'a*.tmp'])
  // Control: options carrying no pattern character were never globs, so nothing new is counted.
  assert.deepEqual(findMultiGlobRemovals('rm -rf --one-file-system a*.tmp'), [])
  assert.deepEqual(findMultiGlobRemovals("rm -f '-*' a*.tmp"), [])
})

// 15. bash absent fails closed.
test('a missing bash fails closed unless BOSS_SKILL_SHELL_OPTIONAL=1', async () => {
  const repoRoot = makeRepo({ [claudeSkill('ok')]: md('```bash', 'echo hi', '```') })
  try {
    const closed = await checkSkillShellInRepo(repoRoot, { hasBash: () => false, env: {} })
    assert.ok(closed.length > 0, 'must fail closed when bash is absent')
    assert.match(closed[0].message, /bash not found on PATH/)

    const warnings = []
    let skippedStats = null
    const open = await checkSkillShellInRepo(repoRoot, {
      hasBash: () => false,
      env: { BOSS_SKILL_SHELL_OPTIONAL: '1' },
      warn: (m) => warnings.push(m),
      onStats: (s) => (skippedStats = s),
    })
    assert.deepEqual(open, [])
    assert.equal(warnings.length, 1)
    assert.match(warnings[0], /bash not found on PATH/)
    // The opt-out returns no findings, so `skipped` is the ONLY thing standing between it and a
    // caller printing "N blocks parse clean via bash -n" for blocks bash never saw.
    assert.equal(skippedStats?.skipped, true, 'the opt-out must report stats.skipped')

    let ranStats = null
    await checkSkillShellInRepo(repoRoot, {
      runBashCheck: async () => ({ ok: true, message: '' }),
      onStats: (s) => (ranStats = s),
    })
    assert.equal(ranStats?.skipped, false, 'a real run must report stats.skipped === false')
    assert.equal(ranStats?.total, 1)
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

// 15b. The opt-out waives `bash -n` only — findings decided without bash must still surface.
test('BOSS_SKILL_SHELL_OPTIONAL=1 still reports findings that never needed bash', async () => {
  const repoRoot = makeRepo({
    [claudeSkill('globs')]: md('```bash', 'rm -f build/*.log tmp/*.tmp', '```'),
    [claudeSkill('open')]: md('```bash', 'echo hi'),
  })
  try {
    const found = await checkSkillShellInRepo(repoRoot, {
      hasBash: () => false,
      env: { BOSS_SKILL_SHELL_OPTIONAL: '1' },
      warn: () => {},
    })
    const kinds = found.map((f) => f.kind).sort()
    assert.deepEqual(
      kinds,
      ['multi-glob', 'unterminated'],
      'the opt-out must not swallow multi-glob/unterminated findings',
    )
    assert.ok(found.every((f) => f.kind !== 'missing-bash'))
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

test('findSkillMarkdownFiles walks every **/*.md under a root', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'skill-shell-walk-'))
  try {
    fs.mkdirSync(path.join(root, 'a', 'references'), { recursive: true })
    fs.writeFileSync(path.join(root, 'a', 'SKILL.md'), '')
    fs.writeFileSync(path.join(root, 'a', 'references', 'deep.md'), '')
    fs.writeFileSync(path.join(root, 'a', 'notes.txt'), '')
    const found = findSkillMarkdownFiles(root).map((f) => path.relative(root, f))
    assert.deepEqual(found.sort(), [
      path.join('a', 'SKILL.md'),
      path.join('a', 'references', 'deep.md'),
    ])
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})

test('a parameter expansion split across lines keeps its << as pattern text', () => {
  // `echo ${x#` / `<<EOF` / `}` is valid bash: the `<<EOF` is parameter-pattern text, and bash -n
  // exits 0 on it. A brace scan that forgets the expansion at end of line reads the second line as
  // ordinary command syntax and records a bogus unterminated `EOF` — a false positive that rejects
  // valid Markdown. The expansion must be closed on the THIRD line for the carried depth to be
  // load-bearing; a single-line `${x#<<EOF}` is already covered by the local scan.
  assert.equal(findUnterminatedHeredoc(['echo ${x#', '<<EOF', '}'].join('\n')), null)
  // Control: once the expansion closes, a `<<` on a later line is a real operator again — the state
  // must be released, not merely swallowed for the rest of the block.
  assert.deepEqual(findUnterminatedHeredoc(['echo ${x#', '}', 'cat <<EOF'].join('\n')), {
    delim: 'EOF',
    lineOffset: 2,
  })
})

test('a heredoc opener on a continued final line is still parsed', () => {
  // bash removes the backslash-newline, reads `cat <<EO`, warns that the heredoc is delimited by
  // end-of-file and exits 0 — so `bash -n` accepts this and only the block's own scanner can catch
  // it. Leaving the line in `pending` at end of input reports nothing and both checks pass.
  assert.deepEqual(findUnterminatedHeredoc('cat <<EO\\'), { delim: 'EO', lineOffset: 0 })
  // The delimiter word itself may be split by the escaped newline; the flushed logical line still
  // has to be `cat <<EOF`, opened at the FIRST physical line.
  assert.deepEqual(findUnterminatedHeredoc(['cat <<EO\\', 'F\\'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
  // Control: a continued line carrying no heredoc operator still reports nothing.
  assert.equal(findUnterminatedHeredoc('echo hi \\'), null)
})

test('more than four columns after a list marker is indented code, not a fence', () => {
  // CommonMark takes 1-4 columns after a marker as padding; a fifth means the item's content starts
  // with an INDENTED CODE BLOCK, so the apparent fence is literal text. Verified against
  // commonmark.js 0.31.2, which renders this as a plain <pre><code> containing ```bash. Consuming
  // all five columns extracts it as a shell sample and fails the gate on Markdown showing a
  // deliberately malformed example.
  assert.deepEqual(
    extractFencedBlocks(['-     ```bash', '      rm *.log', '      ```', ''].join('\n')),
    [],
  )
  // Control: four columns of padding is the maximum CommonMark still calls padding, and the same
  // fence there IS a shell block. This is the one-column boundary the cap must not overshoot.
  const padded = extractFencedBlocks(['-    ```bash', '     rm *.log', '     ```', ''].join('\n'))
  assert.deepEqual(
    padded.map((b) => b.info),
    ['bash'],
  )
  // The over-padded line still OPENS the list item, at a content column of marker + one space — so a
  // properly indented fence on a later line of the same item is found.
  const later = extractFencedBlocks(
    ['-     text', '  ```bash', '  rm *.log', '  ```', ''].join('\n'),
  )
  assert.deepEqual(
    later.map((b) => b.info),
    ['bash'],
  )
})

test('an ANSI-C escape bash does not recognize keeps its backslash', () => {
  // `$'\x'` has no hex digit after it, so bash leaves the two characters `\x` (verified with printf
  // on bash 5.3.9). Dropping the backslash records the delimiter `x`, which is wrong in BOTH
  // directions: it rejects the valid block below and accepts the malformed one after it.
  assert.equal(findUnterminatedHeredoc(["cat <<$'\\x'", '\\x'].join('\n')), null)
  assert.deepEqual(findUnterminatedHeredoc(["cat <<$'\\x'", 'x'].join('\n')), {
    delim: '\\x',
    lineOffset: 0,
  })
  // The same rule for a sequence that is not an escape at all: bash yields `\z`, not `z`.
  assert.equal(findUnterminatedHeredoc(["cat <<$'E\\zF'", 'E\\zF'].join('\n')), null)
  // Control: a WELL-FORMED escape is still decoded, so `<<$'E\x4fF'` terminates on `EOF`.
  assert.equal(findUnterminatedHeredoc(["cat <<$'E\\x4fF'", 'EOF'].join('\n')), null)
})

test('a command substitution inside a parameter expansion is re-entered', () => {
  // `${…}` suspends command parsing, but `$(…)` inside one RESUMES it: bash 5.3.9 warns
  // `command substitution: 1 unterminated here-document` on `echo ${x:-$(cat <<EOF)}` and exits 0,
  // swallowing every later line as payload. Stepping over the expansion whole hides that operator,
  // and the gate passes a block whose remaining commands never run — the founding failure this
  // check exists to catch. The legacy backtick form resumes parsing the same way.
  assert.deepEqual(findUnterminatedHeredoc(['echo ${x:-$(cat <<EOF)}', 'echo later'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
  assert.deepEqual(findUnterminatedHeredoc(['echo ${x:-`cat <<EOF`}', 'echo later'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
  // `$(` resumes parsing inside double quotes too, so the enclosing `"` must not re-hide it.
  assert.deepEqual(
    findUnterminatedHeredoc(['echo "${x:-$(cat <<EOF)}"', 'echo later'].join('\n')),
    { delim: 'EOF', lineOffset: 0 },
  )
  // Control: the substitution's heredoc is TERMINATED here, so nothing is reported — re-entering
  // must not manufacture a finding, and the expansion still closes on the brace after it.
  assert.equal(
    findUnterminatedHeredoc(['echo ${x:-$(cat <<EOF', 'hi', 'EOF', ')}', 'echo after'].join('\n')),
    null,
  )
  // Control: a `<<` in the expansion's own WORD is still text, not an operator.
  assert.equal(findUnterminatedHeredoc(['echo ${x:-<<EOF}', 'echo later'].join('\n')), null)
  // Control for the backtick delimiter rule: with NO substitution open around it, a backtick pair is
  // literal delimiter text — bash terminates this on the line `` `echo x` ``, not on `echo x`.
  assert.equal(findUnterminatedHeredoc(['cat <<`echo x`', '`echo x`'].join('\n')), null)
})

test('brace balancing honors escapes, quotes, and bare braces', () => {
  // bash balances `${…}` over SYNTACTIC braces only. In `echo ${x:-\}<<EOF}` the escaped `}` is
  // literal — bash prints `}<<EOF` and exits 0 — so closing the expansion there exposes the
  // following `<<` as a bogus operator and rejects valid Markdown. Quoted braces are literal in the
  // same way, whether single- or double-quoted.
  assert.equal(findUnterminatedHeredoc(['echo ${x:-\\}<<EOF}', 'echo later'].join('\n')), null)
  // Conversely a bare `{` opens NO level — only `${` does. bash closes `${x:-a{b}` at its single
  // brace and reads the `<<EOF` after it as a real operator (verified: it warns and swallows the
  // rest). Counting that `{` as nesting leaves the scanner stuck inside the expansion, silently
  // dropping every later finding in the block.
  assert.deepEqual(findUnterminatedHeredoc(['echo ${x:-a{b}<<EOF', 'echo later'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
  // A QUOTED brace is skipped, and the expansion closes on the next real one — so the `<<EOF` after
  // it is an operator in both quoting styles.
  assert.deepEqual(findUnterminatedHeredoc(["echo ${x:-'}'}<<EOF", 'echo later'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
  assert.deepEqual(findUnterminatedHeredoc(['echo ${x:-"}"}<<EOF', 'echo later'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
  // `${` nested inside the word DOES open a level, so the first `}` closes only the inner one.
  assert.equal(findUnterminatedHeredoc(['echo ${x:-${y:-<<EOF}}', 'echo later'].join('\n')), null)
  assert.deepEqual(findUnterminatedHeredoc(['echo ${x:-${y}}<<EOF', 'echo later'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
  // A word-initial `#` inside an expansion is text, not a comment (bash prints `a #b`), so it must
  // not blank the rest of the line and hide the operator that follows the closing brace.
  assert.deepEqual(findUnterminatedHeredoc(['echo ${x:-a #b}<<EOF', 'echo later'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
})

// 14ay. Heredoc scan — a backslash inside a delimiter substitution escapes the next character.
test('an escaped paren does not close a delimiter substitution', () => {
  // Measured on bash 5.3.9: `cat <<$(printf \))` terminates on a literal `$(printf \))` line, and
  // leaving that line out warns ``wanted `$(printf \))'`` — the backslash is part of the delimiter
  // and the `)` it escapes is NOT the closer. Counting it records `$(printf \)`, waits for a
  // terminator that never comes, and rejects a block bash -n accepts: a false positive.
  assert.equal(
    findUnterminatedHeredoc(String.raw`cat <<$(printf \))
payload
$(printf \))
echo after`),
    null,
  )
  // An escaped OPENER is equally not one.
  assert.equal(
    findUnterminatedHeredoc(String.raw`cat <<$(printf \()
$(printf \()`),
    null,
  )
  // The escape does not QUOTE the delimiter — bash still expands that body — and an escaped closer
  // inside the word does not save a block whose real terminator is absent.
  assert.deepEqual(
    findUnterminatedHeredoc(String.raw`cat <<$(printf \))
payload`),
    { delim: String.raw`$(printf \))`, lineOffset: 0 },
  )
})

// 14az. Glob rule — `${…}` is ended by its brace, not by whitespace or a newline.
test('a parameter expansion spans whitespace and physical lines', () => {
  // Measured on bash 5.3.9 with `rm` stubbed to print its argv: `rm -f ${x# *} a*.log` passes ONE
  // pathname pattern. Resetting the expansion depth at the space put the pattern's `*` back at top
  // level and reported `*}` plus `a*.log` as two globs — a false positive on valid shell.
  assert.deepEqual(findMultiGlobRemovals('rm -f ${x# *} a*.log'), [])
  // `;`, `|` and `(` are ordinary characters in there too — bash passes `a;b|c(d` as one argument —
  // so they must not emit operator tokens that split the segment and strand the `rm` behind them.
  const ops = findMultiGlobRemovals('rm -f ${x:-a;b|c(d} c*.log d*.tmp')
  assert.equal(ops.length, 1)
  assert.deepEqual(ops[0].globs, ['c*.log', 'd*.tmp'])
  // And the word must stay WHOLE, not merely lose its globs: bash runs `foo=${x:-a b} rm -f …` with
  // `foo=a b` as an assignment prefix. Ending the word at the space leaves `b}` as the next word,
  // which `commandWordIndex` then reads as the command and never reaches the `rm` — a false
  // negative on exactly the invocation this rule exists to report.
  const prefixed = findMultiGlobRemovals('foo=${x:-a b} rm -f c*.log d*.tmp')
  assert.equal(prefixed.length, 1)
  assert.equal(prefixed[0].command, 'rm')
  assert.deepEqual(prefixed[0].globs, ['c*.log', 'd*.tmp'])
  // The same expansion split across a newline is still ONE invocation: bash hands that `rm` `-f`,
  // the expansion, and TWO globs. Analyzing each physical line alone found neither.
  const found = findMultiGlobRemovals(['x=abc', 'rm -f ${x#', '*} a*.log b*.tmp'].join('\n'))
  assert.equal(found.length, 1)
  assert.equal(found[0].command, 'rm')
  assert.deepEqual(found[0].globs, ['a*.log', 'b*.tmp'])
  // Control: an `rm` written INSIDE a multi-line expansion is pattern text, never an invocation.
  assert.deepEqual(findMultiGlobRemovals(['echo ${x#', 'rm -f a*.log b*.tmp}'].join('\n')), [])
  // Control: a glob after the closing brace is still counted once the expansion has closed.
  const closed = findMultiGlobRemovals(
    ['rm -f ${x#', '*} a*.log', 'rm -f c*.log d*.tmp'].join('\n'),
  )
  assert.equal(closed.length, 1)
  assert.deepEqual(closed[0].globs, ['c*.log', 'd*.tmp'])
})

// 14ba. Glob rule — a heredoc DELIMITER is not subject to pathname expansion.
test('a glob character in a heredoc delimiter is not counted', () => {
  // Measured on bash 5.3.9 with `rm` stubbed to print its argv, in a directory holding `EOFxyz`:
  // `rm -f a*.log <<EOF*` / payload / `EOF*` passes `bash -n`, terminates on the LITERAL `EOF*`, and
  // hands `rm` only `-f a1.log a2.log` — the delimiter matched nothing on disk. Counting its `*`
  // alongside `a*.log` reported that valid block as a two-glob removal: a false positive, the one
  // direction this rule must never fail in.
  for (const op of ['<<EOF*', '<< EOF*', '<<-EOF*', '0<<EOF*']) {
    assert.deepEqual(
      findMultiGlobRemovals([`rm -f a*.log ${op}`, 'payload', 'EOF*'].join('\n')),
      [],
      `a \`${op}\` delimiter must not count as a glob`,
    )
  }
  // A here-STRING word is likewise expanded but never pathname-expanded.
  assert.deepEqual(findMultiGlobRemovals('rm -f a*.log <<<x*'), [])

  // Controls. Two real patterns still count with a heredoc attached...
  const both = findMultiGlobRemovals(['rm -f a*.log b*.tmp <<EOF*', 'payload', 'EOF*'].join('\n'))
  assert.equal(both.length, 1)
  assert.deepEqual(both[0].globs, ['a*.log', 'b*.tmp'])
  // ...and a word written AFTER a fused operator is an argument, not the delimiter.
  const after = findMultiGlobRemovals(['rm -f a*.log <<EOF b*.tmp', 'payload', 'EOF'].join('\n'))
  assert.equal(after.length, 1)
  assert.deepEqual(after[0].globs, ['a*.log', 'b*.tmp'])
  // A `<`/`>` target IS pathname-expanded, so it is still counted.
  assert.equal(findMultiGlobRemovals('rm -f a*.log > out*.txt').length, 1)
})

// 14bb. Glob rule — the command word is resolved after QUOTE REMOVAL.
test('an escaped or quoted command word is still a removal', () => {
  // Measured with `rm` stubbed on PATH: `\rm -f a*.log b*.tmp` and `"rm" -f a*.log b*.tmp` print
  // identical argv with BOTH patterns expanded, and under zsh with `b*.tmp` unmatched the `\rm` form
  // aborts with `no matches found` (status 1) — the exact failure this rule exists to report.
  // Comparing the RAW token saw `\rm` and `rm"` and let every one of these through.
  for (const word of ['\\rm', 'r\\m', '"rm"', "'rm'", '"/bin/rm"', '/bin/"rm"', "'/bin/rm'"]) {
    const findings = findMultiGlobRemovals(`${word} -f a*.log b*.tmp`)
    assert.equal(findings.length, 1, `\`${word}\` resolves to rm`)
    assert.equal(findings[0].command, 'rm')
    assert.deepEqual(findings[0].globs, ['a*.log', 'b*.tmp'])
  }
  assert.equal(findMultiGlobRemovals('"find" . -name a* -o -name b*').length, 1)
  // It composes with the wrapper skip, which reaches the command word behind `sudo`.
  assert.equal(findMultiGlobRemovals(String.raw`sudo \rm -f a*.log b*.tmp`).length, 1)

  // Quote removal must not WIDEN the set to commands that merely CONTAIN the name.
  for (const word of ['myrm', 'rm-helper', '"rm-helper"', String.raw`\myrm`]) {
    assert.deepEqual(findMultiGlobRemovals(`${word} -f a*.log b*.tmp`), [], `\`${word}\` is not rm`)
  }

  // Quote removal only, and never MORE than bash performs: `\\rm`, `'\rm'` and `"\rm"` each name a
  // command `\rm` — bash answers `\rm: command not found` — and `$RM` stays an unresolved expansion.
  for (const word of ['\\\\rm', "'\\rm'", '"\\rm"', '$RM', '"$RM"']) {
    assert.deepEqual(
      findMultiGlobRemovals(`${word} -f a*.log b*.tmp`),
      [],
      `\`${word}\` does not name rm`,
    )
  }
})

// 14bc. Heredoc — a bare `((` is a COMMAND, so quoted or expansion text never opens arithmetic.
test('a (( inside quotes or ${…} does not open an arithmetic region', () => {
  // Measured on bash 5.3.9: a block of `echo "(( example"` / `cat <<EOF` / payload warns
  // `here-document at line 2 delimited by end-of-file (wanted `EOF')` and exits 0 — so `bash -n`
  // cannot decide this and `findUnterminatedHeredoc` is the only backstop. Opening a region on the
  // quoted `((` consumed the closing `"` as arithmetic quoting, leaving a region no `))` ever closes;
  // every later line resumed inside it, the `cat <<EOF` was skipped, and the gate returned NO
  // findings for a block whose remaining commands are swallowed as payload — a false negative.
  const swallowed = ['cat <<EOF', 'payload', 'rm -rf /tmp/x'].join('\n')
  for (const opener of [
    'echo "(( example"', // double quotes
    "echo '(( example'", // single quotes
    'echo ${x:-((}', // parameter-expansion text
    'echo "${x:-((}"', // …and the same inside double quotes
  ]) {
    assert.deepEqual(
      findUnterminatedHeredoc([opener, swallowed].join('\n')),
      { delim: 'EOF', lineOffset: 1 },
      `\`${opener}\` must leave the heredoc after it visible`,
    )
  }

  // Controls — every genuine arithmetic form still consumes its `<<` as a left SHIFT.
  for (const arith of [
    '(( y = 1 << 4 ))', // the bare arithmetic COMMAND
    'echo $(( 1 << 2 ))', // arithmetic EXPANSION, live everywhere
    'echo "$(( 1 << 2 ))"', // …including inside double quotes
    'echo $[1 << 2]', // the deprecated `$[…]` form
    'echo "$[1 << 2]"',
    'echo "(( 1 << 2 ))"', // literal text: the `<<` is quoted, so also not an operator
  ]) {
    assert.equal(findUnterminatedHeredoc([arith, 'echo done'].join('\n')), null, arith)
  }
  // A genuine arithmetic region spanning a newline still suppresses the shift on its second line...
  assert.equal(findUnterminatedHeredoc(['(( 1 +', '2 << 3 ))', 'echo done'].join('\n')), null)
  // ...while a real heredoc opened after either form is still reported.
  assert.deepEqual(findUnterminatedHeredoc(['(( i++ ))', 'cat <<EOF', 'x'].join('\n')), {
    delim: 'EOF',
    lineOffset: 1,
  })
  assert.deepEqual(findUnterminatedHeredoc(['(( 1 +', '2 << 3 ))', 'cat <<EOF', 'x'].join('\n')), {
    delim: 'EOF',
    lineOffset: 2,
  })
})

// 14bd. Heredoc — the body starts after the COMMAND closes, not after the operator's physical line.
test('a heredoc queued on a command held open by a quote activates only once it closes', () => {
  // Measured on bash 5.3.9: the block below warns `here-document at line 3 delimited by end-of-file
  // (wanted `EOF')` and exits 0, and running it hands `cat` the single argument "\nEOF\n" — so the
  // `EOF` on line 2 is TEXT inside the multiline quoted argument, and the heredoc body only begins
  // after line 3. Activating on line 2 instead matched that text as the terminator, the heredoc
  // looked closed, and the commands after it were swallowed as payload and never checked. `bash -n`
  // only warns here, so `findUnterminatedHeredoc` is the only backstop.
  assert.deepEqual(
    findUnterminatedHeredoc(
      ['cat <<EOF "', 'EOF', '"', 'payload', 'rm -rf /tmp/should-not-run'].join('\n'),
    ),
    { delim: 'EOF', lineOffset: 0 },
  )

  // Controls — a command that closes on its own line must still open the body on the VERY next one.
  assert.equal(findUnterminatedHeredoc(['cat <<EOF', 'hi', 'EOF', 'echo done'].join('\n')), null)
  assert.deepEqual(findUnterminatedHeredoc(['cat <<EOF', 'hi'].join('\n')), {
    delim: 'EOF',
    lineOffset: 0,
  })
  // A backslash-continued OPENER still joins into one logical line and opens on `EOF`.
  assert.equal(
    findUnterminatedHeredoc(['cat <<EO\\', 'F', 'hi', 'EOF', 'echo done'].join('\n')),
    null,
  )
  // Two operators queued on one command keep bash's order: `A` first, then `B`.
  assert.equal(
    findUnterminatedHeredoc(['cat <<A <<B', 'a1', 'A', 'b1', 'B', 'echo done'].join('\n')),
    null,
  )
  assert.equal(
    findUnterminatedHeredoc(['cat <<A <<B', 'a1', 'A', 'b1', 'echo done'].join('\n')).delim,
    'B',
  )
  // …and once the quote really closes, a later `EOF` really does terminate the body.
  assert.equal(
    findUnterminatedHeredoc(['cat <<EOF "', 'EOF', '"', 'payload', 'EOF', 'echo done'].join('\n')),
    null,
  )
})

// 14be. Glob rule — brace expansion runs first, so one word can carry several pathname patterns.
test('brace-generated alternatives are counted as separate globs', () => {
  // Measured on bash 5.3.9 with `rm` stubbed on PATH and only `a1.log` on disk, `rm -f {a,b}*.log`
  // prints the argv `-f a1.log b*.log`; under zsh the unmatched alternative aborts the whole
  // removal with `no matches found: b*.log`, skipping the file the other alternative matched — the
  // exact failure this rule exists to prevent. Counting the source token once let it through.
  assert.deepEqual(findMultiGlobRemovals('rm -f {a,b}*.log'), [
    { lineOffset: 0, command: 'rm', globs: ['a*.log', 'b*.log'] },
  ])
  // A glob inside SOME alternatives counts only the alternatives that carry one.
  assert.deepEqual(findMultiGlobRemovals('rm -f {a*,b?}.log')[0].globs, ['a*.log', 'b?.log'])
  // Groups multiply.
  assert.deepEqual(findMultiGlobRemovals('rm -f {a,b}{c,d}*.log')[0].globs, [
    'ac*.log',
    'ad*.log',
    'bc*.log',
    'bd*.log',
  ])

  // Controls — none of these is a brace expansion producing two patterns, and reporting any of them
  // would be a false POSITIVE, the one direction this rule must never fail in.
  for (const line of [
    'rm -f {a,b}.log', // two literal names, no pattern at all
    'rm -f ${x}*.log', // parameter expansion, not brace expansion
    'rm -f "{a,b}*.log"', // quoted: literal text, and the `*` is not a glob either
    'rm -f \\{a,b\\}*.log', // escaped braces: one literal-braced pattern
    'rm -f {a}*.log', // no comma, so bash leaves `{a}` literal: one pattern
    'rm -f {a,b*.log', // never closed, so literal: one pattern
    'rm -f {a*,b}.log', // only the first alternative is a pattern
    'rm -f a*.log', // the plain single-glob baseline
  ]) {
    assert.deepEqual(findMultiGlobRemovals(line), [], line)
  }
  // …and the plain two-glob form this change is additive to still reports unchanged.
  assert.deepEqual(findMultiGlobRemovals('rm -f a*.log b*.log'), [
    { lineOffset: 0, command: 'rm', globs: ['a*.log', 'b*.log'] },
  ])
})

// 14bf. Glob rule — `$'…'` is ANSI-C quoting on the COMMAND word, not an expansion.
test('an ANSI-C quoted command word is resolved like any other quoting', () => {
  // Measured on bash 5.3.9 with `rm` stubbed on PATH and `a1.log a2.log b1.tmp b2.tmp` on disk,
  // `$'rm' -f a*.log b*.tmp` prints the argv `-f a1.log a2.log b1.tmp b2.tmp`: bash quote-removes
  // the word to the literal command `rm` and expands BOTH patterns, so this carries the same
  // unmatched-glob abort hazard a bare `rm` does. Stripping only the quotes left `$rm`, which
  // matches no command name and reported nothing.
  assert.deepEqual(findMultiGlobRemovals("$'rm' -f a*.log b*.tmp"), [
    { lineOffset: 0, command: 'rm', globs: ['a*.log', 'b*.tmp'] },
  ])
  // Escapes are decoded before quote removal, so the encoded spelling resolves too.
  assert.deepEqual(findMultiGlobRemovals("$'\\x72m' -f a*.log b*.tmp")[0].command, 'rm')
  // And the form combines with a path, as `"/bin/rm"` already did.
  assert.deepEqual(findMultiGlobRemovals("$'/bin/rm' -f a*.log b*.tmp")[0].command, 'rm')
  assert.deepEqual(findMultiGlobRemovals("$'find' . -name b*.tmp c*.txt")[0].command, 'find')

  // Controls — this is quote REMOVAL, never expansion. A parameter expansion has no quote to
  // remove and must survive as itself, matching no command: reporting any of these is a false
  // POSITIVE, since the variable's value is unknown and need not be a removal at all.
  for (const line of [
    '$rm -f a*.log b*.tmp',
    '${rm} -f a*.log b*.tmp',
    '$RM -f a*.log b*.tmp',
    '"$RM" -f a*.log b*.tmp',
    "$'ls' -f a*.log b*.tmp", // ANSI-C quoted, but not a removal command
    "$'rm' -f a*.log", // a single pattern is not the multi-glob hazard
  ]) {
    assert.deepEqual(findMultiGlobRemovals(line), [], line)
  }
  // An ANSI-C word ELSEWHERE on the line leaves the real command word alone.
  assert.deepEqual(findMultiGlobRemovals("echo $'hi' && rm -f a*.log b*.tmp")[0].command, 'rm')
})

// 14bg. Extractor — a fence stops matching once its own container has exited.
test('a dedented line ends a container-nested block before a later fence can close it', () => {
  // CommonMark ends a fenced code block at the end of its containing block, and a fence body is
  // never a paragraph, so no lazy continuation holds the list open across `outside`. The item ends
  // there, taking its code block with it, and the two-space ``` below is a NEW top-level opener.
  // Matching on depth and columns alone read that later fence as this block's closer: one block,
  // reported terminated and clean, carrying the prose as shell — and the genuinely broken fence
  // was never reported at all.
  const blocks = extractFencedBlocks(md('- ```bash', '  echo hi', 'outside', '  ```'))
  assert.equal(blocks.length, 2)
  assert.equal(blocks[0].info, 'bash')
  assert.equal(blocks[0].terminated, true)
  assert.equal(blocks[0].body, 'echo hi', 'the dedented prose is outside the shell body')
  assert.equal(blocks[1].startLine, 4)
  assert.equal(blocks[1].terminated, false, 'the trailing fence is the unterminated one')

  // Controls — the split needs a real container exit, and must not fire without one.
  const closed = extractFencedBlocks(md('- ```bash', '  echo hi', '  ```'))
  assert.equal(closed.length, 1, 'a properly closed list-nested block is untouched')
  assert.equal(closed[0].terminated, true)
  assert.equal(closed[0].body, 'echo hi')

  // A BLANK line never leaves a container, so it cannot split a block.
  assert.equal(
    extractFencedBlocks(md('- ```bash', '  echo hi', '', '  echo bye', '  ```'))[0].body,
    'echo hi\n\necho bye',
  )

  // A top-level fence has no container to exit, so a column-0 body line is just a body line.
  const top = extractFencedBlocks(md('```bash', 'echo hi', 'echo bye', '```'))
  assert.equal(top.length, 1)
  assert.equal(top[0].body, 'echo hi\necho bye')

  // A DEEPER blockquote is still inside the opener's container, not out of it.
  const deeper = extractFencedBlocks(md('> ```bash', '> echo hi', '> > nested', '> ```'))
  assert.equal(deeper.length, 1)
  assert.equal(deeper[0].terminated, true)
})

// 14bh. Extractor — a CommonMark lazy continuation does not end the list item.
test('a lazy paragraph continuation keeps the list item and its fence in scope', async () => {
  // `- item` opens an item with content at column 2 and a paragraph inside it; an unindented
  // continuation line is a LAZY continuation of that paragraph, so the item is still open. The
  // fence at four physical columns then sits two columns into the item's content — a legal fence.
  // Treating every dedent as an exit dropped the item, re-read the fence as top-level indented code
  // and skipped the block entirely, letting the malformed shell inside it bypass the gate.
  const blocks = extractFencedBlocks(
    md('- item', 'lazy continuation', '    ```bash', '    if [ x ; then', '    ```'),
  )
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].info, 'bash')
  assert.equal(blocks[0].body, 'if [ x ; then')

  // Control: with no dedent at all the same fence was already extracted — the fix restores parity.
  assert.equal(
    extractFencedBlocks(md('- item', '    ```bash', '    if [ x ; then', '    ```'))[0].body,
    'if [ x ; then',
  )

  // Controls — only a PARAGRAPH continues lazily. A blank line ends the paragraph, so what follows
  // is genuine top-level indented code and must stay unextracted; extracting it would hand a
  // deliberately-malformed sample to bash and reject valid Markdown.
  assert.deepEqual(
    extractFencedBlocks(
      md('- item', '', 'prose', '', '    ```bash', '    if [ x ; then', '    ```'),
    ),
    [],
  )
  // A dedented line that starts a BLOCK of its own is not a lazy continuation either: it really
  // does end the item, and the fence below it is top-level indented code once more.
  for (const interrupter of ['# heading', '---', '> quoted']) {
    assert.deepEqual(
      extractFencedBlocks(md('- item', interrupter, '    ```bash', '    if [ x ; then', '    ```')),
      [],
      interrupter,
    )
  }
  // A SIBLING item is the exception that proves the rule — it ends the first item but opens an
  // equivalent container, so the fence stays sheltered at the same content column.
  assert.equal(
    extractFencedBlocks(
      md('- item', '- other item', '    ```bash', '    if [ x ; then', '    ```'),
    )[0].body,
    'if [ x ; then',
  )

  // End to end: the previously-skipped block is now gated, and its malformed shell is reported.
  const repoRoot = makeRepo({
    [claudeSkill('lazy')]: md(
      '- item',
      'lazy continuation',
      '    ```bash',
      '    if [ x ; then',
      '    ```',
    ),
  })
  try {
    const findings = await checkSkillShellInRepo(repoRoot)
    assert.equal(findings.length, 1)
    assert.equal(findings[0].kind, 'syntax')
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})
