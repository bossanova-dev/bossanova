import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import os from 'node:os'
import { execFileSync } from 'node:child_process'
import { parseNote } from './retro-notes.mjs'
import {
  inventory,
  mirrorFamilies,
  detectProsePins,
  weightSignals,
  rulesEntries,
  pickRuleOfRun,
  recordState,
  toRetroNotes,
  gateCompare,
  runCli,
} from './guidance-audit.mjs'
const scan = (text, file = 'x.test.mjs') => detectProsePins([{ file, text }])
const read = "const skill = readFileSync('SKILL.md', 'utf8')\n"
const fixture = (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'guidance-test-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  return dir
}

test('JS phrase, heading, equality, count and prohibition literals are pins', () => {
  const result = scan(
    read +
      `assert.match(skill, /phrase/)
assert.ok(skill.includes('words'))
expect(skill).toContain('words')
assert.match(skill, /^## Heading/m)
assert.equal(skill, 'fixed')
assert.ok(skill.split('\\n').length < 12)
assert.equal(Buffer.byteLength(skill), 20)
assert.doesNotMatch(skill, /forbidden/)
assert.ok(!skill.includes('forbidden'))`,
  )
  assert.deepEqual(
    result.map((p) => p.kind),
    [
      'phrase',
      'phrase',
      'phrase',
      'heading',
      'equality',
      'size',
      'size',
      'prohibition',
      'prohibition',
    ],
  )
  assert.equal(result[0].line, 2)
  assert.equal(
    scan('\n' + read + 'assert.match(skill, /phrase/)')[0].fingerprint,
    result[0].fingerprint,
  )
})
test('direct reads and stat size are pins', () => {
  assert.equal(scan(`assert.match(readFileSync('CLAUDE.md','utf8'), /word/)`).length, 1)
  assert.equal(
    scan(`const stat = statSync('CLAUDE.md')\nassert.equal(stat.size, 200)`).at(0).kind,
    'size',
  )
})
test('reader taint and region propagation work in JS and Go packages', () => {
  assert.equal(
    scan(`function readSkill() { return readFileSync('SKILL.md', 'utf8') }
const skill = readSkill()
const region = sectionRegion(skill, '## Work')
assert.match(region, /instruction/)`).length,
    1,
  )
  const files = [
    {
      file: 'pkg/reader_test.go',
      text: `func readSkill(t *testing.T) string {
 b, _ := os.ReadFile("SKILL.md")
 return string(b)
}`,
    },
    {
      file: 'pkg/pins_test.go',
      text: `func TestWords(t *testing.T) {
 skill := readSkill(t)
 if !strings.Contains(skill, "words") { t.Fatal("missing") }
 if regexp.MustCompile("^## Heading").MatchString(skill) { t.Fatal("missing") }
 if len(skill) != 12 { t.Fatal("size") }
 if skill != "fixed" { t.Fatal("different") }
}`,
    },
  ]
  assert.deepEqual(
    detectProsePins(files).map((p) => p.kind),
    ['phrase', 'heading', 'size', 'equality'],
  )
})
test('frontmatter, resolution, parity, computed needles and waivers are allowed', () => {
  assert.deepEqual(
    scan(
      read +
        `const metadata = parseFrontmatter(skill)
assert.equal(metadata.name, 'name')
assert.ok(existsSync(resolve(skill)))
assert.ok(skill.includes(ref))
const other = readFileSync('SKILL.md','utf8')
assert.equal(skill, other)
// guidance-audit: allow
assert.match(skill, /exception/)
assert.match(skill, /inline/) // guidance-audit: allow`,
    ),
    [],
  )
})
test('temporary fixture paths and tree-wide prohibitions are allowed', () => {
  assert.deepEqual(
    scan(`const root = mkdtempSync(join(tmpdir(), 'test-'))
const dest = join(root, 'skills')
const skill = readFileSync(join(dest,'SKILL.md'),'utf8')
assert.equal(skill, 'fixture')`),
    [],
  )
  assert.deepEqual(
    scan(`function tmpDir() { return mkdtempSync(join(tmpdir(),'test-')) }
const root = tmpDir()
assert.equal(readFileSync(join(root,'SKILL.md'),'utf8'), 'fixture')`),
    [],
  )
  assert.deepEqual(
    scan(`for (const file of files) {
 const skill = readFileSync(file + '/SKILL.md','utf8')
 assert.doesNotMatch(skill, /unsafe/)
}`),
    [],
  )
  assert.deepEqual(
    scan(
      `func TestFixtures(t *testing.T) {
 dir := t.TempDir()
 root := filepath.Join(dir,"skills")
 b, err := os.ReadFile(filepath.Join(root,"SKILL.md"))
 if string(b) != "fixture" { t.Fatal(err) }
}`,
      'x_test.go',
    ),
    [],
  )
})
test('weight measures growth, new and removed families, with mirror collapse', () => {
  const families = mirrorFamilies([
    { file: '.claude/skills/a/SKILL.md', text: 'one\ntwo' },
    { file: '.codex/skills/a/SKILL.md', text: 'rewritten mirror' },
    { file: 'CLAUDE.md', text: 'rules' },
  ])
  assert.equal(families.length, 2)
  const signal = weightSignals(families, {
    takenAt: 'then',
    runId: 'old',
    families: { 'a/SKILL.md': { bytes: 2, lines: 1 }, removed: { bytes: 1, lines: 1 } },
  })
  assert.equal(signal.weights.find((w) => w.key === 'a/SKILL.md').deltaBytes, 5)
  assert.equal(signal.weights.find((w) => w.key === 'CLAUDE.md').new, true)
  assert.deepEqual(signal.removed, ['removed'])
  assert.equal(signal.hotspots[0].signal, 'growth')
})
test('fences and paragraphs repeat across different families, never mirror copies', () => {
  const text = 'a sufficiently long paragraph with words\n\n```sh\none\ntwo\n```\n'
  const copies = [
    { file: '.claude/skills/a/SKILL.md', text },
    { file: '.codex/skills/a/SKILL.md', text },
  ]
  assert.equal(
    weightSignals(mirrorFamilies(copies), null, { minFenceLines: 2, minParagraphChars: 20 })
      .repeatedBlocks.length,
    0,
  )
  copies.push({ file: '.claude/skills/b/SKILL.md', text })
  const result = weightSignals(mirrorFamilies(copies), null, {
    minFenceLines: 2,
    minParagraphChars: 20,
  })
  assert.equal(result.repeatedBlocks.length, 1)
  assert.equal(result.duplicatedParagraphs.length, 1)
  assert.equal(result.repeatedBlocks[0].occurrences.length, 2)
})
test('rules rotate deterministically, complete cycles and revisit edits', () => {
  const entries = rulesEntries([
    { file: 'CLAUDE.md', text: '# Rules\n\n- first\n  continued\n- second\n\nA paragraph.' },
  ])
  assert.equal(entries.length, 3)
  assert.deepEqual(pickRuleOfRun(entries), pickRuleOfRun(entries))
  let rotation = { visited: [], cycle: 0 }
  const picks = []
  for (let i = 0; i < entries.length; i++) {
    const pick = pickRuleOfRun(entries, rotation)
    picks.push(pick.entry.id)
    rotation = { cycle: pick.cycle, visited: [...pick.visited, pick.entry.id] }
  }
  assert.equal(new Set(picks).size, 3)
  assert.equal(pickRuleOfRun(entries, rotation).cycle, 1)
  const edited = rulesEntries([{ file: 'CLAUDE.md', text: '- changed\n- second\n\nA paragraph.' }])
  assert.equal(pickRuleOfRun(edited, rotation).entry.text, '- changed')
})
test('inventory respects tracked files and dedupes symlinked rules', (t) => {
  const root = fixture(t)
  fs.writeFileSync(path.join(root, 'CLAUDE.md'), 'rules')
  fs.symlinkSync('CLAUDE.md', path.join(root, 'AGENTS.md'))
  fs.writeFileSync(path.join(root, 'untracked.test.mjs'), read)
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', 'CLAUDE.md', 'AGENTS.md'], { cwd: root })
  const files = inventory(root)
  assert.equal(files.rules.length, 1)
  assert.equal(files.tests.length, 0)
})
test('notes replay hot-spot history and use retro note field prefixes', () => {
  const entries = rulesEntries([{ file: 'CLAUDE.md', text: 'A rule.' }])
  const report = {
    prosePins: scan(read + 'assert.match(skill, /phrase/)'),
    weight: {
      weights: [{ key: 'a', bytes: 20, lines: 2 }],
      hotspots: [{ hotspotKey: 'a:growth', file: 'SKILL.md', signal: 'growth' }],
    },
    ruleOfRun: pickRuleOfRun(entries),
  }
  const state = { hotspots: { 'a:growth': ['r1', 'r2'] } }
  const notes = toRetroNotes(report, state, 'r3')
  assert.equal(notes.length, 5)
  assert.deepEqual(
    notes.filter((n) => n.kind === 'weight').map((n) => n.body.match(/Run: (.*)/)[1]),
    ['r1', 'r2', 'r3'],
  )
  for (const note of notes) assert.equal(parseNote(note).run, note.body.match(/Run: (.*)/)[1])
  for (const note of notes)
    for (const prefix of ['Where:', 'Why it matters:', 'Suggested fix:', 'Run:'])
      assert.ok(note.body.split('\n').some((line) => line.startsWith(prefix)))
  assert.equal(notes[0].repeatExempt, true)
  const recorded = recordState(report, state, 'r3', 'now')
  assert.equal(recorded.snapshot.takenAt, 'now')
  assert.deepEqual(recorded.hotspots['a:growth'], ['r1', 'r2', 'r3'])
})
test('gate compares counts, not line positions, and removed pins are stale', () => {
  const pins = scan(read + 'assert.match(skill, /phrase/)')
  assert.equal(gateCompare(pins, { pins }).added.length, 0)
  assert.equal(gateCompare([...pins, ...pins], { pins }).added.length, 1)
  assert.equal(gateCompare([], { pins }).stale.length, 1)
})
test('CLI is read-only except record, handles corrupt state, and gates baseline changes', (t) => {
  const root = fixture(t),
    stateDir = path.join(root, 'state'),
    baseline = path.join(root, 'baseline.json')
  fs.writeFileSync(path.join(root, 'CLAUDE.md'), 'A rule.')
  fs.writeFileSync(path.join(root, 'a.test.mjs'), read + 'assert.match(skill, /phrase/)')
  const args = ['--root', root, '--state-dir', stateDir],
    outputs = [],
    warnings = [],
    io = { out: (s) => outputs.push(s), warn: (s) => warnings.push(s) }
  assert.equal(runCli([...args, '--json'], io), 0)
  assert.equal(fs.existsSync(stateDir), false)
  assert.equal(runCli([...args, '--record', '--run-id', 'r1'], io), 0)
  const stateFile = path.join(stateDir, 'guidance-audit.json'),
    before = fs.readFileSync(stateFile, 'utf8')
  assert.equal(runCli([...args, '--notes', '--run-id', 'dry'], io), 0)
  assert.equal(fs.readFileSync(stateFile, 'utf8'), before)
  assert.equal(runCli([...args, '--write-baseline', baseline], io), 0)
  assert.equal(runCli([...args, '--gate', '--baseline', baseline], io), 0)
  fs.appendFileSync(path.join(root, 'a.test.mjs'), '\nassert.match(skill, /another/)')
  assert.equal(runCli([...args, '--gate', '--baseline', baseline], io), 1)
  fs.writeFileSync(path.join(root, 'a.test.mjs'), '')
  assert.equal(runCli([...args, '--gate', '--baseline', baseline], io), 0)
  assert.ok(warnings.some((s) => s.includes('stale baseline')))
  fs.writeFileSync(stateFile, 'broken')
  assert.equal(runCli([...args], io), 0)
  assert.equal(fs.readFileSync(stateFile, 'utf8'), 'broken')
  assert.ok(warnings.some((s) => s.includes('ignoring state')))
  assert.equal(runCli([...args, '--record'], io), 2)
})

// Trimmed source excerpts from the suites removed by 3ef0e741e (#2741).
test('recall: deleted boss skill suite', () => {
  const excerpt =
    "const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')\n" +
    'const CANONICAL = read(`${BOSS_CANONICAL}/SKILL.md`)\n' +
    "assert.match(CANONICAL, /^---\\r?\\nname: boss\\r?\\n/, 'frontmatter must declare name: boss')"
  assert.ok(scan(excerpt).length > 0)
})
test('recall: deleted plan skill suite', () => {
  const excerpt =
    "const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')\n" +
    'const SKILL = read(`${CORE}/SKILL.md`)\n' +
    'assert.match(SKILL, /self-edited/)'
  assert.ok(scan(excerpt).length > 0)
})
test('recall: deleted Go boss skill suite', () => {
  const excerpt = `func readEmbeddedBossSkill(t *testing.T) string {
 skillBytes, err := SkillsFS.ReadFile("skills/boss/SKILL.md")
 if err != nil { t.Fatal(err) }
 return string(skillBytes)
}
func TestBossSkillDocumentsBroadcasts(t *testing.T) {
 skill := readEmbeddedBossSkill(t)
 assertContains(t, skill, "boss broadcast")
 assertContains(t, skill, "send_broadcast")
}`
  assert.equal(scan(excerpt, 'boss_skill_test.go').length, 2)
})

test('detector fixture strings do not taint their host tests', () => {
  const host =
    "const read = \"const skill = readFileSync('SKILL.md','utf8')\"\n" +
    "const excerpt = `const skill = readFileSync('SKILL.md','utf8')\nassert.match(skill, /word/)`\n" +
    'assert.equal(scan(read + excerpt).length, 1)'
  assert.deepEqual(scan(host), [])
})
test('inventory includes a reader from a non-test Go sibling', (t) => {
  const root = fixture(t)
  fs.writeFileSync(
    path.join(root, 'reader.go'),
    'func load() string { b, _ := os.ReadFile("SKILL.md"); return string(b) }',
  )
  fs.writeFileSync(
    path.join(root, 'x_test.go'),
    'func TestText(t *testing.T) {\n skill := load()\n if !strings.Contains(skill,"word") { t.Fatal("missing") }\n}',
  )
  assert.equal(detectProsePins(inventory(root).tests).length, 1)
})

test('Go error-only readers do not taint error messages', () => {
  assert.deepEqual(
    detectProsePins([
      {
        file: 'reader.go',
        readerOnly: true,
        text: `func validate() error {
 b, err := os.ReadFile("SKILL.md")
 return err
}`,
      },
      {
        file: 'x_test.go',
        text: `func TestError(t *testing.T) {
 err := validate()
 if !strings.Contains(err.Error(), "missing") { t.Fatal(err) }
}`,
      },
    ]),
    [],
  )
})

test('parity compares operands independently of diagnostic messages', () => {
  for (const method of ['equal', 'strictEqual', 'deepEqual', 'deepStrictEqual']) {
    const source = read + "const other = readFileSync('SKILL.md','utf8')\n"
    assert.deepEqual(scan(source + 'assert.' + method + '(skill, other, "mirror parity")'), [])
    assert.equal(scan(source + 'assert.' + method + '(skill, "fixed", "mirror parity")').length, 1)
  }
})
test('resolution names in literal needles remain prose pins', () => {
  for (const needle of ['existsSync', 'os.Stat', 'fs.access', 'resolve', 'existsSync()']) {
    assert.equal(scan(read + 'assert.match(skill, /' + needle + '/)').length, 1)
    assert.equal(scan(read + 'assert.ok(skill.includes("' + needle + '"))').length, 1)
  }
  assert.deepEqual(scan(read + 'assert.ok(existsSync(resolve(skill)))'), [])
})
test('prohibition exemptions require a traversal-derived guidance path', () => {
  assert.equal(
    scan(
      'for (let retry=0; retry<3; retry++) {\n' +
        "const skill=readFileSync('CLAUDE.md','utf8')\n" +
        'assert.doesNotMatch(skill, /words/)\n}',
    ).length,
    1,
  )
  assert.equal(
    scan(
      'for (const file of files) readFileSync(file)\n' +
        "const skill=readFileSync('CLAUDE.md','utf8')\n" +
        'assert.doesNotMatch(skill, /words/)',
    ).length,
    1,
  )
  assert.deepEqual(
    scan(
      'for (const file of files) {\n' +
        "const guidancePath = file + '/SKILL.md'\n" +
        'const skill=readFileSync(guidancePath)\n' +
        'assert.doesNotMatch(skill, /words/)\n}',
    ),
    [],
  )
  assert.deepEqual(
    scan(
      'func TestPayload(t *testing.T) {\n' +
        'err := fs.WalkDir(SkillsFS, "skills", func(path string, d fs.DirEntry, err error) error {\n' +
        'data, err := fs.ReadFile(SkillsFS, filepath.Join(path, "SKILL.md"))\n' +
        'if strings.Contains(string(data), "unsafe") { t.Fatal("bad") }\n' +
        'return nil\n})\n}',
      'x_test.go',
    ),
    [],
  )
})
test('Go payload allowance depends on traversing the read path', () => {
  const source = (readPath) =>
    'func TestPayload(t *testing.T) {\n' +
    'err := fs.WalkDir(SkillsFS, "skills", func(path string, d fs.DirEntry, err error) error {\n' +
    'data, err := fs.ReadFile(SkillsFS, ' +
    readPath +
    ')\n' +
    'if strings.Contains(string(data), "unsafe") { t.Fatal("bad") }\n' +
    'return nil\n})\n}'
  assert.deepEqual(scan(source('filepath.Join(path, "SKILL.md")'), 'x_test.go'), [])
  assert.equal(scan(source('"SKILL.md"'), 'x_test.go').at(0).kind, 'prohibition')
})
test('valid JSON with malformed consumed state warns and behaves as absent', (t) => {
  const root = fixture(t),
    stateDir = path.join(root, 'state')
  fs.mkdirSync(stateDir)
  fs.writeFileSync(path.join(root, 'CLAUDE.md'), 'A rule.')
  const file = path.join(stateDir, 'guidance-audit.json')
  for (const state of [
    { version: 1, rotation: { visited: 'broken' } },
    { version: 1, rotation: { cycle: -1 } },
    { version: 1, hotspots: { growth: 'broken' } },
    { version: 1, snapshot: { families: { a: { bytes: 'bad', lines: 2 } } } },
    { version: 1, snapshot: [] },
    null,
  ]) {
    const bytes = JSON.stringify(state),
      warnings = [],
      output = []
    fs.writeFileSync(file, bytes)
    assert.equal(
      runCli(['--root', root, '--state-dir', stateDir, '--notes', '--run-id', 'dry'], {
        out: (s) => output.push(s),
        warn: (s) => warnings.push(s),
      }),
      0,
    )
    assert.ok(warnings.some((s) => s.includes('ignoring state')))
    assert.equal(JSON.parse(output[0]).at(-1).kind, 'rule-of-the-run')
    assert.equal(fs.readFileSync(file, 'utf8'), bytes)
  }
})
