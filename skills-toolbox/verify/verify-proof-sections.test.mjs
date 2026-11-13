import test from 'node:test'
import assert from 'node:assert/strict'
import {
  SECTION_INSTRUCTIONS_CHARS,
  baseProofSections,
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
