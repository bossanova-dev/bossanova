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

// 1. Opening line number.
test('extractFencedBlocks reports the 1-based line of the OPENING fence', () => {
  const blocks = extractFencedBlocks(md('intro', '', '```bash', 'echo hi', '```', 'tail'))
  assert.equal(blocks.length, 1)
  assert.equal(blocks[0].startLine, 3)
  assert.equal(blocks[0].info, 'bash')
  assert.equal(blocks[0].terminated, true)
  assert.equal(blocks[0].body, 'echo hi')
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
