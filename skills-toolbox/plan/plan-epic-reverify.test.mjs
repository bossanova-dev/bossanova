import assert from 'node:assert/strict'
import { test } from 'node:test'
import { buildEpicReverifyBundle, EPIC_REVERIFY_BUNDLE_DEFECT } from './plan-epic-phase25.mjs'

const issue = (identifier, extra = {}) => ({
  id: `uuid-${identifier}`,
  identifier,
  title: identifier,
  status: 'Todo',
  labels: [{ name: 'epic' }],
  attachments: [],
  links: [],
  ...extra,
})
const input = () => ({
  payload: { epic: true, epicParentId: 'ABC-1', childIds: ['ABC-2'] },
  parentIssue: issue('ABC-1'),
  childrenListing: { children: [issue('ABC-2', { parentId: 'uuid-ABC-1' })] },
  childFiles: [],
})

test('bundle projects only safe fields and normalizes MCP and GraphQL shapes', () => {
  const value = input()
  value.parentIssue = issue('ABC-1', {
    description: 'private description',
    comments: ['secret'],
    assignee: 'secret',
    attachments: [
      {
        id: 'a',
        title: 'Plan',
        url: 'https://uploads.linear.app/a?signature=secret',
        metadata: 'private',
      },
    ],
    links: [{ url: 'https://user:password@example.com/a?token=secret' }],
  })
  const built = buildEpicReverifyBundle(value)
  assert.equal(built.ok, true)
  assert.deepEqual(
    Object.keys(built.bundle.parent).sort(),
    ['id', 'identifier', 'title', 'state', 'labels', 'attachments', 'links'].sort(),
  )
  assert.equal(built.bundle.parent.attachments[0].url, 'https://uploads.linear.app/a')
  for (const secret of [
    'signature=',
    'description',
    'comments',
    'assignee',
    'password',
    'token=secret',
  ])
    assert.ok(!JSON.stringify(built.bundle).includes(secret), secret)
  const gql = input()
  gql.parentIssue = {
    ...value.parentIssue,
    status: undefined,
    state: { name: 'Todo' },
    labels: { nodes: [{ name: 'epic' }] },
    attachments: { nodes: value.parentIssue.attachments },
    links: { nodes: value.parentIssue.links },
  }
  assert.deepEqual(buildEpicReverifyBundle(gql).bundle, built.bundle)
})

test('bundle coverage gaps are notices and both child sources merge', () => {
  const value = input()
  value.payload.childIds.push('ABC-4')
  value.childFiles = [{ childId: 'ABC-3', issue: issue('ABC-3', { parentId: 'uuid-ABC-1' }) }]
  const built = buildEpicReverifyBundle(value)
  assert.equal(built.ok, true)
  assert.deepEqual(built.coverage, { unhydratedChildIds: ['ABC-4'], unlistedChildren: ['ABC-3'] })
  assert.equal(built.bundle.children.length, 2)
})

test('fallback hydration requires positive parent membership, unlike the live listing', () => {
  for (const extra of [{}, { parentId: null }]) {
    const value = input()
    value.childrenListing.children = []
    value.childFiles = [{ childId: 'ABC-2', issue: issue('ABC-2', extra) }]
    const built = buildEpicReverifyBundle(value)
    assert.equal(built.ok, true)
    assert.deepEqual(built.bundle.children, [])
    assert.deepEqual(built.coverage.unhydratedChildIds, ['ABC-2'])
    assert.ok(built.notices.some((n) => n.code === 'child-parent-unreadable'))
  }
  const value = input()
  delete value.childrenListing.children[0].parentId
  assert.equal(buildEpicReverifyBundle(value).bundle.children.length, 1)
})

test('every bundle defect has a minimal behavioral example', () => {
  const cases = {
    'payload-not-epic': (v) => {
      v.payload.epic = false
    },
    'payload-parent-missing': (v) => {
      delete v.payload.epicParentId
    },
    'payload-childids-missing': (v) => {
      v.payload.childIds = []
    },
    'parent-payload-missing': (v) => {
      delete v.parentIssue
    },
    'children-payload-missing': (v) => {
      delete v.childrenListing
    },
    'payload-unreadable': (v) => {
      v.childFiles = [{ childId: 'ABC-3', file: 'broken.json', error: 'invalid JSON' }]
    },
    'payload-wrapped': (v) => {
      v.parentIssue = { issue: v.parentIssue }
    },
    'payload-not-issue': (v) => {
      v.childrenListing.children = [null]
    },
    'parent-payload-mismatch': (v) => {
      v.parentIssue = issue('ABC-9')
    },
    'child-payload-mismatch': (v) => {
      v.childFiles = [{ childId: 'ABC-3', issue: issue('ABC-9') }]
    },
    'child-duplicate': (v) => {
      v.childFiles = [{ childId: 'ABC-2', issue: issue('ABC-2') }]
    },
    'child-parent-mismatch': (v) => {
      v.childrenListing.children[0].parentId = 'another'
    },
    'could-not-build': (v) => {
      Object.defineProperty(v, 'payload', {
        get() {
          throw Error('hostile')
        },
      })
    },
  }
  assert.deepEqual(Object.values(EPIC_REVERIFY_BUNDLE_DEFECT).sort(), Object.keys(cases).sort())
  for (const [code, change] of Object.entries(cases)) {
    const value = input()
    change(value)
    const built = buildEpicReverifyBundle(value)
    assert.equal(built.ok, false, code)
    assert.ok(
      built.defects.some((d) => d.code === code),
      `${code}: ${JSON.stringify(built)}`,
    )
    assert.equal(built.bundle, null)
  }
  assert.deepEqual(buildEpicReverifyBundle(input()).defects, [])
  for (const value of [
    null,
    [],
    {
      get payload() {
        throw Error('hostile')
      },
    },
  ]) {
    assert.doesNotThrow(() => {
      assert.equal(buildEpicReverifyBundle(value).ok, false)
    })
  }
})
