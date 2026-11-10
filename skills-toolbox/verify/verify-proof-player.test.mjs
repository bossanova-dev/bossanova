import test from 'node:test'
import assert from 'node:assert/strict'
import vm from 'node:vm'
import { PROOF_PLAYER_FILE, proofPlayerScript } from './verify-proof-player.mjs'

/** A fake element with the attributes, classes and listeners the player touches. */
function element(dataset = {}) {
  const listeners = {}
  const classes = new Set()
  return {
    dataset: { ...dataset },
    attributes: {},
    classList: {
      toggle: (name, on) => (on ? classes.add(name) : classes.delete(name)),
      has: (n) => classes.has(n),
    },
    setAttribute(name, value) {
      this.attributes[name] = value
    },
    removeAttribute(name) {
      delete this.attributes[name]
    },
    addEventListener(type, listener, options) {
      ;(listeners[type] ??= []).push({ listener, once: options?.once })
    },
    fire(type) {
      const queued = listeners[type] ?? []
      listeners[type] = queued.filter((entry) => !entry.once)
      for (const entry of queued) entry.listener()
    },
  }
}

/** A page with two clips: clip 0 has chapters at 1 s and 5 s, clip 1 one chapter at 2 s. */
function page(hash = '') {
  const video = Object.assign(element({ clip: '0' }), {
    currentTime: 0,
    src: 'media/a.mp4',
    poster: '',
    plays: 0,
    play() {
      this.plays++
      return Promise.reject(new Error('autoplay blocked'))
    },
  })
  const label = { textContent: 'Clip A' }
  const chapter = (clip, t, src, title) =>
    element({ chapter: '', clip, t, src, poster: `${src}.png`, title })
  const buttons = [
    chapter('0', '1.00', 'media/a.mp4', 'Clip A'),
    chapter('0', '5.00', 'media/a.mp4', 'Clip A'),
    chapter('1', '2.00', 'media/b.mp4', 'Clip B'),
  ]
  const area = { open: false }
  const target = {
    scrolled: 0,
    closest: () => area,
    scrollIntoView() {
      this.scrolled++
    },
  }
  const win = Object.assign(element(), { location: { hash } })
  const ids = { 'proof-video': video, 'proof-clip': label, 'f-src-a-ts': target }
  const doc = {
    defaultView: win,
    getElementById: (id) => ids[id] ?? null,
    querySelectorAll: (selector) => (selector === 'button[data-chapter]' ? buttons : []),
  }
  vm.runInNewContext(proofPlayerScript(), { document: doc })
  const current = () => buttons.findIndex((b) => b.classList.has('cur'))
  return { video, label, buttons, area, target, win, current }
}

test('player.js is the published name', () => {
  assert.equal(PROOF_PLAYER_FILE, 'player.js')
})

test('a chapter on the current clip seeks, plays and marks only itself', async () => {
  const { video, buttons, current } = page()
  assert.equal(current(), -1)
  buttons[1].fire('click')
  assert.equal(video.currentTime, 5)
  assert.equal(video.plays, 1)
  assert.equal(video.src, 'media/a.mp4')
  assert.equal(current(), 1)
  assert.equal(buttons[1].attributes['aria-current'], 'true')
  assert.equal(buttons[0].attributes['aria-current'], undefined)
  video.currentTime = 1.5
  video.fire('timeupdate')
  assert.equal(current(), 0)
  await new Promise((resolve) => setImmediate(resolve))
})

test('a chapter on another clip switches source, then seeks once its metadata loads', () => {
  const { video, label, buttons, current } = page()
  buttons[2].fire('click')
  assert.equal(video.src, 'media/b.mp4')
  assert.equal(video.poster, 'media/b.mp4.png')
  assert.equal(video.dataset.clip, '1')
  assert.equal(label.textContent, 'Clip B')
  assert.equal(video.plays, 0)
  video.fire('loadedmetadata')
  assert.equal(video.currentTime, 2)
  assert.equal(video.plays, 1)
  assert.equal(current(), 2)
  video.currentTime = 5.5
  video.fire('timeupdate')
  assert.equal(current(), 2, 'clip 0 chapters stay unmarked while clip 1 plays')
})

test('a link to code in a collapsed area opens it on load and on hash change', () => {
  const opened = page('#f-src-a-ts')
  assert.equal(opened.area.open, true)
  assert.equal(opened.target.scrolled, 1)
  const later = page('')
  assert.equal(later.area.open, false)
  later.win.location.hash = '#f-src-a-ts'
  later.win.fire('hashchange')
  assert.equal(later.area.open, true)
})

test('a later chapter click while a clip loads wins over the click that switched it', () => {
  const { video, buttons, current } = page()
  buttons[2].fire('click')
  buttons[2].dataset.t = '6.00'
  buttons[2].fire('click')
  video.fire('loadedmetadata')
  assert.equal(video.currentTime, 6)
  assert.equal(video.plays, 1)
  assert.equal(current(), 2)
  video.fire('loadedmetadata')
  assert.equal(video.plays, 1, 'a later load of the same clip does not seek again')
})
