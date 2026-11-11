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
      toggle: (name, on = !classes.has(name)) => (on ? classes.add(name) : classes.delete(name)),
      has: (n) => classes.has(n),
      contains: (n) => classes.has(n),
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
function page(hash = '', { withVideo = true, framed = false } = {}) {
  const video = Object.assign(element({ clip: '0' }), {
    currentTime: 0,
    src: 'media/a.mp4',
    poster: '',
    plays: 0,
    paused: true,
    play() {
      this.plays++
      this.paused = false
      return Promise.reject(new Error('autoplay blocked'))
    },
    pause() {
      this.paused = true
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
  const target = Object.assign(element(), {
    scrolled: 0,
    closest: (selector) => (selector === 'details' ? area : null),
    scrollIntoView() {
      this.scrolled++
    },
  })
  const section = Object.assign(element(), {
    scrolled: 0,
    closest: () => null,
    scrollIntoView() {
      this.scrolled++
    },
  })
  const lightbox = (i) => Object.assign(element(), { id: `shot-${i}`, closest: () => null })
  const boxes = [lightbox(0), lightbox(1)]
  boxes.forEach((box) => box.classList.toggle('lightbox', true))
  const history = {
    calls: 0,
    urls: [],
    replaceState(_data, _unused, url) {
      this.calls++
      this.urls.push(url)
      throw new Error('SecurityError: opaque origin')
    },
  }
  const win = Object.assign(element(), {
    location: { hash, pathname: '/acme/edge/9/index.html', search: '' },
    history,
  })
  if (framed) win.top = {}
  else win.top = win
  const root = element()
  const ids = {
    ...(withVideo ? { 'proof-video': video } : {}),
    'proof-clip': label,
    'f-src-a-ts': target,
    evidence: section,
    'shot-0': boxes[0],
    'shot-1': boxes[1],
    'thumb-0': section,
    'thumb-1': section,
  }
  const listeners = {}
  const doc = {
    documentElement: root,
    defaultView: win,
    getElementById: (id) => ids[id] ?? null,
    querySelectorAll: (selector) => (selector === 'button[data-chapter]' ? buttons : []),
    addEventListener: (type, listener) => (listeners[type] ??= []).push(listener),
  }
  /** A link at `href`, inside the lightbox `inBox` when given, with class `cls`. */
  const link = (href, inBox = null, cls = '') => {
    const a = Object.assign(element(), {
      getAttribute: (name) => (name === 'href' ? href : null),
      closest: (selector) => (selector === 'a[href]' ? a : selector === '.lightbox' ? inBox : null),
    })
    if (cls) a.classList.toggle(cls, true)
    return a
  }
  /** Click `a` as the browser would; returns whether the default navigation was prevented. */
  const click = (a, extra = {}) => {
    const event = {
      target: a,
      defaultPrevented: false,
      button: 0,
      ...extra,
      preventDefault() {
        this.defaultPrevented = true
      },
    }
    for (const listener of listeners.click ?? []) listener(event)
    return event.defaultPrevented
  }
  const key = (name) => {
    for (const listener of listeners.keydown ?? []) listener({ key: name })
  }
  vm.runInNewContext(proofPlayerScript(), { document: doc })
  const current = () => buttons.findIndex((b) => b.classList.has('cur'))
  return {
    video,
    label,
    buttons,
    area,
    target,
    section,
    boxes,
    history,
    win,
    root,
    current,
    link,
    click,
    key,
  }
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

test('in-page links never navigate: a section link scrolls and only tries to replace the URL', () => {
  const { link, click, section, target, area, history, win } = page()
  assert.equal(click(link('#evidence')), true, 'the anchor navigation is prevented')
  assert.equal(section.scrolled, 1)
  assert.equal(
    history.calls,
    1,
    'replaceState is tried, never pushState, and its sandbox error is caught',
  )
  assert.equal(
    win.location.hash,
    '',
    'the hash never changes, so the review tab gets no Back entry',
  )
  assert.equal(click(link('#f-src-a-ts')), true)
  assert.equal(area.open, true, 'a link to code in a collapsed area opens it')
  assert.equal(target.scrolled, 1)
  assert.equal(click(link('#nowhere')), false, 'a link to no element is left to the browser')
  assert.equal(
    click(link('https://example.com/x')),
    false,
    'an outside link is left to the browser',
  )
  assert.equal(click(link('#evidence'), { metaKey: true }), false, 'a modified click is left alone')
})

test('a still opens its lightbox by class, pauses the video, and closing resumes it without scrolling', () => {
  const { link, click, boxes, video, section, key } = page()
  video.paused = false
  assert.equal(click(link('#shot-0')), true)
  assert.equal(boxes[0].classList.has('open'), true)
  assert.equal(video.paused, true, 'the video pauses while the lightbox is open')
  assert.equal(click(link('#shot-1', boxes[0])), true, 'next moves to the following still')
  assert.equal(boxes[0].classList.has('open'), false)
  assert.equal(boxes[1].classList.has('open'), true)
  assert.equal(video.paused, true, 'moving between stills keeps the video paused')
  assert.equal(click(link('#thumb-0', boxes[1])), true, 'close is a link inside the lightbox')
  assert.equal(boxes[1].classList.has('open'), false)
  assert.equal(section.scrolled, 0, 'closing the lightbox does not jump the page')
  assert.equal(video.paused, false, 'the video the lightbox paused plays again')
  click(link('#shot-0'))
  key('Escape')
  assert.equal(boxes[0].classList.has('open'), false, 'Escape closes the lightbox')
})

test('a paused video stays paused after a lightbox, and Full size toggles in place', () => {
  const { link, click, boxes, video } = page()
  click(link('#shot-0'))
  assert.equal(click(link('media/still.png', boxes[0], 'lb-full')), true)
  assert.equal(boxes[0].classList.has('full'), true, 'Full size shows the still at full width')
  click(link('#thumb-0', boxes[0]))
  assert.equal(boxes[0].classList.has('full'), false)
  assert.equal(video.plays, 0, 'a video that was not playing is not started')
})

test('on a page with no video the player still handles links and lightboxes, and touches no video', () => {
  const { link, click, section, boxes, video, key } = page('', { withVideo: false })
  assert.equal(click(link('#evidence')), true)
  assert.equal(section.scrolled, 1)
  assert.equal(click(link('#shot-0')), true)
  assert.equal(boxes[0].classList.has('open'), true)
  key('Escape')
  assert.equal(boxes[0].classList.has('open'), false)
  assert.equal(video.plays, 0)
})

test('a lightbox the URL opened before the script ran is adopted, so Close and Escape shut it', () => {
  for (const shut of ['close', 'escape']) {
    const { link, click, boxes, root, history, key } = page('#shot-1')
    assert.equal(boxes[1].classList.has('open'), true, 'the :target lightbox is adopted as open')
    assert.equal(
      root.classList.has('proof-js'),
      true,
      'the page stops showing a lightbox by :target',
    )
    if (shut === 'close') assert.equal(click(link('#thumb-1', boxes[1])), true)
    else key('Escape')
    assert.equal(boxes[1].classList.has('open'), false, `${shut} shuts the adopted lightbox`)
    assert.deepEqual(
      history.urls,
      ['/acme/edge/9/index.html'],
      'the #shot-1 hash is cleared, its error caught',
    )
  }
})

test('Back and Forward to a lightbox hash open and shut it', () => {
  const { boxes, win } = page('')
  win.location.hash = '#shot-0'
  win.fire('hashchange')
  assert.equal(boxes[0].classList.has('open'), true)
  win.location.hash = '#thumb-0'
  win.fire('hashchange')
  assert.equal(boxes[0].classList.has('open'), false, 'leaving the lightbox hash shuts it')
})

test('a framed page marks its root, so links a sandbox cannot follow give way to a note', () => {
  assert.equal(page('', { framed: true }).root.classList.has('proof-framed'), true)
  assert.equal(page('').root.classList.has('proof-framed'), false, 'a page in its own tab')
})
