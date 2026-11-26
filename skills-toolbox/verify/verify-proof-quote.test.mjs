// The highlight helper's side of the conversation with the framing app. It runs from its own
// source text, as player.js runs it, against a fake page that only answers what the helper asks.
import test from 'node:test'
import assert from 'node:assert/strict'
import vm from 'node:vm'
import { proofQuote } from './verify-proof-quote.mjs'

const APP_ORIGIN = 'https://app.example'
/** Objects the helper built in its own vm realm, as plain data. */
const plain = (value) => JSON.parse(JSON.stringify(value))

function fakePage({ framed = true } = {}) {
  const listeners = { doc: {}, win: {} }
  const on = (bucket) => (type, fn) => {
    ;(bucket[type] ??= []).push(fn)
  }
  const attrs = new Map()
  const sent = []
  const app = { postMessage: (message, origin) => sent.push({ message, origin }) }
  const heading = {
    textContent: 'Riskiest  path',
    contains: () => false,
    compareDocumentPosition: () => 4,
  }
  let selection = null
  const win = {
    addEventListener: on(listeners.win),
    getSelection: () => selection,
    setTimeout: (fn) => {
      fn()
      return 1
    },
    clearTimeout: () => {},
  }
  win.parent = framed ? app : win
  const doc = {
    defaultView: win,
    documentElement: {
      hasAttribute: (name) => attrs.has(name),
      setAttribute: (name, value) => attrs.set(name, value),
    },
    head: { appendChild: () => {} },
    createElement: () => ({}),
    querySelectorAll: () => [heading],
    addEventListener: on(listeners.doc),
  }
  const fire = (bucket, type, event) => {
    for (const fn of listeners[bucket][type] ?? []) fn(event)
  }
  return {
    doc,
    sent,
    attrs,
    select(text) {
      const range = { startContainer: {}, commonAncestorContainer: { nodeType: 1 } }
      selection = {
        rangeCount: 1,
        isCollapsed: false,
        toString: () => text,
        getRangeAt: () => range,
        removeAllRanges: () => {
          selection = null
        },
      }
      fire('doc', 'pointerdown', { pointerType: 'mouse' })
      fire('doc', 'pointerup', { pointerType: 'mouse' })
    },
    fromApp: (data, source = app) => fire('win', 'message', { source, origin: APP_ORIGIN, data }),
  }
}

test('@sessions.AC277206 posts a finished highlight to the app only once the app has spoken, to its origin', () => {
  const page = fakePage()
  vm.runInNewContext(`(${proofQuote})(doc)`, { doc: page.doc })
  page.select('  holds the update lock  \n')
  assert.deepEqual(page.sent, [], 'nothing leaves before the app speaks')

  page.fromApp({ type: 'proof:quote:hello', v: 1 }, {})
  assert.deepEqual(page.sent, [], 'a message from another window is not the app')

  page.fromApp({ type: 'proof:quote:hello', v: 1 })
  assert.deepEqual(plain(page.sent), [
    {
      message: {
        type: 'proof:quote',
        v: 1,
        id: 'q1',
        text: 'holds the update lock',
        anchor: 'Riskiest path',
      },
      origin: APP_ORIGIN,
    },
    { message: { type: 'proof:quote:ready', v: 1 }, origin: APP_ORIGIN },
  ])
})

test('@sessions.AC277206 a page that is not framed, or already carries the helper, installs nothing', () => {
  const alone = fakePage({ framed: false })
  vm.runInNewContext(`(${proofQuote})(doc)`, { doc: alone.doc })
  assert.equal(alone.attrs.size, 0)

  const twice = fakePage()
  vm.runInNewContext(`(${proofQuote})(doc);(${proofQuote})(doc)`, { doc: twice.doc })
  twice.fromApp({ type: 'proof:quote:hello', v: 1 })
  twice.select('one quote')
  assert.equal(twice.sent.filter((s) => s.message.type === 'proof:quote').length, 1)
})
