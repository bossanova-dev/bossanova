import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  SOURCE_NOTES_COMMENT_MARKER,
  sourceNotesCommentBody,
  isSourceNotesComment,
  selectSourceNotesComment,
  sourceNotesAttachmentFromComment,
  recordTextFromComment,
  PLAN_COMMENT_MARKER,
  planCommentBody,
  isPlanComment,
  planTextFromComment,
  selectPlanComment,
  planAttachmentFromComment,
} from './github-plan.mjs'
import {
  selectReadablePlanAttachment,
  selectImplementationPlanAttachment,
} from '../plan-attachment.mjs'

test('marked plan preserves exact UTF-8 and CRLF bytes, recognizing only the first line', () => {
  const text = `Résumé 日本語\r\n${PLAN_COMMENT_MARKER}\nend\n`
  assert.equal(planTextFromComment(planCommentBody(text)), text)
  assert.equal(isPlanComment(`quoted\n${PLAN_COMMENT_MARKER}`), false)
  assert.throws(() => planTextFromComment('ordinary comment'))
  assert.equal(isPlanComment(`${PLAN_COMMENT_MARKER} extra\ntext`), false)
})
test('selection refuses ambiguity and exposes a readable canonical attachment', () => {
  const comment = {
    id: '9',
    authorAssociation: 'MEMBER',
    body: planCommentBody('plan'),
    url: 'https://github.com/a/b/issues/42#issuecomment-9',
    createdAt: 'now',
    updatedAt: 'later',
  }
  assert.deepEqual(selectPlanComment([]), { status: 'none' })
  assert.deepEqual(selectPlanComment([comment]), { status: 'one', comment })
  assert.deepEqual(selectPlanComment([{ ...comment, id: '10' }, comment]), {
    status: 'conflict',
    ids: ['9', '10'],
  })
  const attachment = planAttachmentFromComment(comment, 'GH-42')
  assert.equal(selectReadablePlanAttachment([attachment], 'GH-42').id, '9')
  assert.equal(selectImplementationPlanAttachment([attachment], 'GH-42').id, '9')
})
test('comment limit includes the marker and rejects overflow without truncating', () => {
  const overhead = PLAN_COMMENT_MARKER.length + 1
  assert.equal(planCommentBody('x'.repeat(65536 - overhead)).length, 65536)
  assert.throws(
    () => planCommentBody('x'.repeat(65537 - overhead)),
    /65537.*65536.*marker.*not written/,
  )
})

test('only repository owners, members and collaborators supply authoritative plans', () => {
  const body = planCommentBody('trusted plan')
  for (const authorAssociation of ['OWNER', 'MEMBER', 'COLLABORATOR']) {
    const trusted = { id: '1', body, authorAssociation }
    const outsiders = [
      'NONE',
      'CONTRIBUTOR',
      'FIRST_TIMER',
      'FIRST_TIME_CONTRIBUTOR',
      'MANNEQUIN',
      undefined,
    ].map((authorAssociation, i) => ({ id: String(i + 2), body, authorAssociation }))
    assert.deepEqual(selectPlanComment(outsiders), { status: 'none' })
    assert.deepEqual(selectPlanComment([...outsiders, trusted]), {
      status: 'one',
      comment: trusted,
    })
  }
})

test('source notes preserve exact content, share trusted conflict rules and enforce marker-inclusive size', () => {
  const text = `Résumé 日本語\r\n${SOURCE_NOTES_COMMENT_MARKER}\nend\n`
  const body = sourceNotesCommentBody(text)
  assert.equal(recordTextFromComment(body), text)
  assert.equal(isSourceNotesComment(`quoted\n${body}`), false)
  assert.equal(isSourceNotesComment(`${SOURCE_NOTES_COMMENT_MARKER} extra`), false)
  const trusted = { id: '9', body, authorAssociation: 'MEMBER' }
  assert.deepEqual(selectSourceNotesComment([{ ...trusted, authorAssociation: 'NONE' }]), {
    status: 'none',
  })
  assert.deepEqual(selectSourceNotesComment([trusted]), { status: 'one', comment: trusted })
  assert.deepEqual(selectSourceNotesComment([{ ...trusted, id: '10' }, trusted]), {
    status: 'conflict',
    ids: ['9', '10'],
  })
  const attachment = sourceNotesAttachmentFromComment(trusted, 'GH-42')
  assert.equal(attachment.title, 'Source notes (GH-42)')
  assert.equal(selectImplementationPlanAttachment([attachment], 'GH-42'), null)
  const overhead = SOURCE_NOTES_COMMENT_MARKER.length + 1
  assert.equal(sourceNotesCommentBody('x'.repeat(65536 - overhead)).length, 65536)
  assert.throws(() => sourceNotesCommentBody('x'.repeat(65537 - overhead)), /size 65537.*65536/)
})
