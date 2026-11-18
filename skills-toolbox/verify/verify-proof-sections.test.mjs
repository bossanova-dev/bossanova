import test from 'node:test'
import assert from 'node:assert/strict'
import {
  SECTION_INSTRUCTIONS_CHARS,
  baseProofSections,
  hiddenSectionIds,
  parseSectionsMarker,
  readProofSections,
  sectionInstructions,
} from './verify-proof-sections.mjs'

test('an after that names no core anchor is left out; an area anchor and a core one are kept', () => {
  assert.deepEqual(
    parseSectionsMarker([
      { id: 'infra', title: 'Infrastructure', after: 'a-migrations' },
      { id: 'testing', title: 'How to test it', after: 'ticket' },
      { id: 'notes', title: 'Notes', after: 'sec-infra' },
    ]),
    [
      { id: 'infra', title: 'Infrastructure', after: 'a-migrations' },
      { id: 'testing', title: 'How to test it', after: 'ticket' },
      { id: 'notes', title: 'Notes' },
    ],
  )
})

test('a repo section replaces a built-in pack silently, with repo sections first', () => {
  const result = readProofSections([
    {
      name: 'pack',
      builtin: true,
      sections: [
        { id: 'testing', title: 'Pack testing' },
        { id: 'deploy', title: 'Deploy' },
      ],
      instructions: 'Pack instructions.',
    },
    {
      name: 'repo',
      sections: [
        { id: 'testing', title: 'Repo testing' },
        { id: 'infra', title: 'Infra' },
      ],
      instructions: 'Repo instructions.',
    },
  ])
  assert.deepEqual(result, {
    sections: [
      { id: 'testing', title: 'Repo testing', instructions: 'Repo instructions.' },
      { id: 'infra', title: 'Infra', instructions: 'Repo instructions.' },
      { id: 'deploy', title: 'Deploy', instructions: 'Pack instructions.' },
    ],
    problems: [],
  })
})

test('duplicate built-in ids are dropped and named', () => {
  const result = readProofSections([
    { name: 'first', builtin: true, sections: [{ id: 'testing', title: 'First' }] },
    { name: 'second', builtin: true, sections: [{ id: 'testing', title: 'Second' }] },
  ])
  assert.equal(result.sections[0].title, 'First')
  assert.deepEqual(result.problems, ['sections: second declares testing again, dropped'])
})

test('hidden ids remove repo overrides and built-ins, and unknown ids are named', () => {
  for (const builtin of [true, false]) {
    const result = readProofSections(
      [{ name: 'section', builtin, sections: [{ id: 'testing', title: 'Testing' }] }],
      { hidden: ['testing', 'nope'] },
    )
    assert.deepEqual(result, {
      sections: [],
      problems: ['sections: proofDefaults.sections hides nope, which nothing declares'],
    })
  }
})

test('a hidden pack id with no packs discovered adds no problem', () => {
  assert.deepEqual(readProofSections([], { hidden: ['deploy', 'testing'] }), {
    sections: [],
    problems: [],
  })
})

test('hiddenSectionIds keeps only literal false with a valid id', () => {
  assert.deepEqual(
    hiddenSectionIds({
      proofDefaults: { sections: { testing: false, deploy: true, infra: 'false', Bad_Id: false } },
    }),
    ['testing'],
  )
  for (const sections of [undefined, null, false, 'testing', ['testing']])
    assert.deepEqual(hiddenSectionIds({ proofDefaults: { sections } }), [])
  assert.deepEqual(hiddenSectionIds(null), [])
})

test('base config is loaded only from the trusted root and hides its sections', () => {
  const roots = []
  const result = baseProofSections({
    baseSha: 'base',
    cwd: '/head',
    trustedTree: () => ({ ok: true, root: '/trusted' }),
    discover: () => ({
      extensions: [{ name: 'repo', sections: [{ id: 'infra', title: 'Infra' }] }],
    }),
    loadConfig: (root) => {
      roots.push(root)
      return { proofDefaults: { sections: { infra: false } } }
    },
  })
  assert.deepEqual(roots, ['/trusted'])
  assert.deepEqual(result, { sections: [], problems: [] })
})

test('defensive config read failure keeps sections and adds a diagnostic', () => {
  const result = baseProofSections({
    baseSha: 'base',
    trustedTree: () => ({ ok: true, root: '/trusted' }),
    discover: () => ({
      extensions: [{ name: 'repo', sections: [{ id: 'infra', title: 'Infra' }] }],
    }),
    loadConfig: () => {
      throw new Error('read failed after discovery')
    },
  })
  assert.deepEqual(result, {
    sections: [{ id: 'infra', title: 'Infra', instructions: '' }],
    problems: ['sections: the base config could not be read'],
  })
})

test('a title over 40 characters or on two lines fails the whole declaration', () => {
  assert.equal(parseSectionsMarker([{ id: 'infra', title: 't'.repeat(41) }]), null)
  assert.equal(parseSectionsMarker([{ id: 'infra', title: 'Two\nlines' }]), null)
  assert.equal(parseSectionsMarker('infra'), null)
})

test('a section id an earlier extension declared is dropped and named', () => {
  const { sections, problems } = readProofSections([
    {
      name: 'boss-verify-a',
      sections: [{ id: 'infra', title: 'Infrastructure' }],
      instructions: 'From a.',
    },
    {
      name: 'boss-verify-b',
      sections: [
        { id: 'infra', title: 'Deploy' },
        { id: 'testing', title: 'Testing', after: 'ticket' },
      ],
      instructions: 'From b.',
    },
  ])
  assert.deepEqual(sections, [
    { id: 'infra', title: 'Infrastructure', instructions: 'From a.' },
    { id: 'testing', title: 'Testing', after: 'ticket', instructions: 'From b.' },
  ])
  assert.deepEqual(problems, ['sections: boss-verify-b declares infra again, dropped'])
})

test('instructions are trimmed and cut to 12,000 characters', () => {
  assert.equal(sectionInstructions('\n  Fill infra.\n\n'), 'Fill infra.')
  const cut = sectionInstructions('x'.repeat(SECTION_INSTRUCTIONS_CHARS + 50))
  assert.equal(cut.length, SECTION_INSTRUCTIONS_CHARS)
  assert.ok(cut.endsWith('…'))
})

test('the sections come from the trusted tree for the base, and an unavailable one declares none', () => {
  /** @type {unknown[]} */
  const asked = []
  const found = baseProofSections({
    baseSha: 'b'.repeat(40),
    trustedTree: ({ baseSha }) => {
      asked.push(baseSha)
      return { ok: true, root: '/trusted' }
    },
    discover: ({ core, role, root }) => {
      asked.push([core, role, root])
      return {
        extensions: [
          { name: 'boss-verify-x', sections: [{ id: 'infra', title: 'I' }], instructions: 'Do.' },
        ],
      }
    },
  })
  assert.deepEqual(asked, ['b'.repeat(40), ['boss-verify', 'proof-section', '/trusted']])
  assert.deepEqual(found, {
    sections: [{ id: 'infra', title: 'I', instructions: 'Do.' }],
    problems: [],
  })
  const none = baseProofSections({
    baseSha: 'b'.repeat(40),
    trustedTree: () => ({ ok: false }),
    discover: () => {
      throw new Error('never asked')
    },
  })
  assert.deepEqual(none, { sections: [], problems: ['sections: the base tree is unavailable'] })
})
