import { test } from 'node:test'
import assert from 'node:assert/strict'
import { renderHtml } from './proof-render-terminal.mjs'

test('renderHtml emits a blue caption bar when caption is provided', () => {
  const html = renderHtml({ title: 'boss', text: 'home', caption: 'Opening cron list' })
  assert.match(html, /Opening cron list/)
  assert.match(html, /__proof-tui-caption/)
})

test('renderHtml omits the caption bar when caption is empty', () => {
  const html = renderHtml({ title: 'boss', text: 'home', caption: '' })
  assert.doesNotMatch(html, /__proof-tui-caption/)
})

test('renderHtml escapes caption text', () => {
  const html = renderHtml({ title: 'boss', text: 'home', caption: '<script>x</script>' })
  assert.doesNotMatch(html, /<script>x<\/script>/)
  assert.match(html, /&lt;script&gt;/)
})

test('renderHtml bounds an over-long caption to <=140 chars ending with an ellipsis', () => {
  const long = 'A'.repeat(200)
  const html = renderHtml({ title: 'boss', text: 'home', caption: long })
  // The full 200-char string must NOT appear; the bar carries a 139-char slice + '…'.
  assert.doesNotMatch(html, new RegExp('A'.repeat(200)))
  assert.match(html, new RegExp(`${'A'.repeat(139)}…`))
  // Extract the caption bar text and assert it is <=140 chars and ends with '…'.
  const m = html.match(/__proof-tui-caption[^>]*>([^<]*)</)
  assert.ok(m, 'caption bar must be present')
  assert.ok(m[1].length <= 140, `caption text must be <=140 chars (was ${m[1].length})`)
  assert.equal(m[1].endsWith('…'), true)
})

test('renderHtml collapses a multi-line / multi-space caption to a single line', () => {
  const html = renderHtml({ title: 'boss', text: 'home', caption: 'open\n  the   dashboard' })
  const m = html.match(/__proof-tui-caption[^>]*>([^<]*)</)
  assert.ok(m, 'caption bar must be present')
  assert.equal(m[1], 'open the dashboard')
  assert.doesNotMatch(m[1], /\n/)
  assert.doesNotMatch(m[1], /  /)
})

test('renderHtml omits the caption bar for a whitespace-only caption', () => {
  const html = renderHtml({ title: 'boss', text: 'home', caption: '   ' })
  assert.doesNotMatch(html, /__proof-tui-caption/)
})
