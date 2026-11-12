// The player runs from its own source text against a small DOM parsed from a page the renderer
// produced, so every id, class and attribute it looks for is the one the page really carries.
import test from 'node:test'
import assert from 'node:assert/strict'
import vm from 'node:vm'
import { withDigest } from './verify-proof.mjs'
import { renderProofHtml } from './verify-proof-page.mjs'
import { PROOF_PLAYER_FILE, proofPlayerScript } from './verify-proof-player.mjs'

const HEAD = '3'.repeat(40)
const VOID = new Set(['img', 'input', 'meta', 'br', 'hr', 'link', 'source', 'path'])
const RAW = new Set(['script', 'style'])
const ENTITIES = { lt: '<', gt: '>', quot: '"', '#39': "'", amp: '&' }
const decode = (text) => text.replace(/&(lt|gt|quot|#39|amp);/g, (_, name) => ENTITIES[name])

/** One compound selector (`tag#id.class[attr]`) against one element. */
function compound(el, selector) {
  const m = /^([a-z][a-z0-9]*)?((?:[#.][\w-]+|\[[\w-]+\])*)$/.exec(selector)
  if (!m) throw new Error(`the fake DOM does not support the selector ${selector}`)
  if (m[1] && el.tagName !== m[1]) return false
  for (const [, kind, name] of m[2].matchAll(/([#.[])([\w-]+)\]?/g))
    if (
      kind === '#'
        ? el.getAttribute('id') !== name
        : kind === '.'
          ? !el.classList.contains(name)
          : !el.attrs.has(name)
    )
      return false
  return true
}

/** A selector list with descendant combinators, matched right to left. */
function matches(el, selector) {
  return selector.split(',').some((alternative) => {
    const parts = alternative.trim().split(/\s+/)
    if (!compound(el, parts.at(-1))) return false
    let at = el.parentElement
    for (let i = parts.length - 2; i >= 0; i--) {
      while (at && !compound(at, parts[i])) at = at.parentElement
      if (!at) return false
      at = at.parentElement
    }
    return true
  })
}

class El {
  constructor(doc, tagName, attrs = {}) {
    this.ownerDocument = doc
    this.tagName = tagName
    this.attrs = new Map(Object.entries(attrs))
    this.children = []
    this.parentElement = null
    this.text = ''
    this.listeners = {}
    this.style = {}
    this.scrolled = 0
    this.hidden = this.attrs.has('hidden')
    this.checked = this.attrs.has('checked')
    this.disabled = this.attrs.has('disabled')
    this.open = this.attrs.has('open')
    this.dataset = new Proxy(
      {},
      {
        get: (_, key) => this.getAttribute(`data-${String(key)}`) ?? undefined,
        set: (_, key, value) => {
          this.setAttribute(`data-${String(key)}`, String(value))
          return true
        },
      },
    )
    const classes = () => new Set((this.getAttribute('class') ?? '').split(/\s+/).filter(Boolean))
    const write = (set) => this.setAttribute('class', [...set].join(' '))
    this.classList = {
      contains: (name) => classes().has(name),
      toggle: (name, force) => {
        const set = classes()
        const on = force === undefined ? !set.has(name) : Boolean(force)
        if (on) set.add(name)
        else set.delete(name)
        write(set)
        return on
      },
    }
  }
  get src() {
    return this.getAttribute('src') ?? ''
  }
  set src(value) {
    this.setAttribute('src', value)
  }
  get poster() {
    return this.getAttribute('poster') ?? ''
  }
  set poster(value) {
    this.setAttribute('poster', value)
  }
  get alt() {
    return this.getAttribute('alt') ?? ''
  }
  set alt(value) {
    this.setAttribute('alt', value)
  }
  get textContent() {
    return this.text + this.children.map((child) => child.textContent).join('')
  }
  set textContent(value) {
    this.children = []
    this.text = String(value)
  }
  getAttribute(name) {
    return this.attrs.has(name) ? this.attrs.get(name) : null
  }
  setAttribute(name, value) {
    this.attrs.set(name, String(value))
  }
  removeAttribute(name) {
    this.attrs.delete(name)
  }
  appendChild(child) {
    child.parentElement = this
    this.children.push(child)
    return child
  }
  contains(node) {
    for (let at = node; at; at = at.parentElement) if (at === this) return true
    return false
  }
  matches(selector) {
    return matches(this, selector)
  }
  closest(selector) {
    for (let at = this; at; at = at.parentElement) if (matches(at, selector)) return at
    return null
  }
  *descendants() {
    for (const child of this.children) {
      yield child
      yield* child.descendants()
    }
  }
  querySelectorAll(selector) {
    return [...this.descendants()].filter((el) => matches(el, selector))
  }
  querySelector(selector) {
    return this.querySelectorAll(selector)[0] ?? null
  }
  focus() {
    this.ownerDocument.activeElement = this
  }
  scrollIntoView() {
    this.scrolled++
  }
  getBoundingClientRect() {
    return { left: 0, top: 0, width: 0, height: 0 }
  }
  addEventListener(type, listener, options) {
    ;(this.listeners[type] ??= []).push({ listener, once: options?.once })
  }
  fire(type, event = {}) {
    const queued = this.listeners[type] ?? []
    this.listeners[type] = queued.filter((entry) => !entry.once)
    for (const entry of queued) entry.listener(event)
  }
}

/** A DOM from the page's own markup; a tag closed out of order fails the test. */
function parsePage(html) {
  const doc = {
    activeElement: null,
    listeners: {},
    createElement: (tag) => new El(doc, tag),
    getElementById: (id) => doc.documentElement.querySelector(`#${id}`),
    querySelectorAll: (selector) => doc.documentElement.querySelectorAll(selector),
    addEventListener: (type, listener) => (doc.listeners[type] ??= []).push(listener),
  }
  const top = new El(doc, '#document')
  let current = top
  const token =
    /<!--[\s\S]*?-->|<!doctype[^>]*>|<\/([a-z0-9]+)>|<([a-z0-9]+)((?:\s+[\w:-]+(?:="[^"]*")?)*)\s*\/?>|([^<]+)/gi
  for (let m = token.exec(html); m; m = token.exec(html)) {
    const [, close, open, attrText, text] = m
    if (text !== undefined) current.text += decode(text)
    else if (close) {
      assert.equal(close, current.tagName, `</${close}> closes <${current.tagName}>`)
      current = current.parentElement
    } else if (open) {
      const attrs = Object.fromEntries(
        [...attrText.matchAll(/([\w:-]+)(?:="([^"]*)")?/g)].map(([, k, v]) => [k, decode(v ?? '')]),
      )
      const el = current.appendChild(new El(doc, open, attrs))
      if (RAW.has(open)) {
        const end = html.indexOf(`</${open}>`, token.lastIndex)
        el.text = html.slice(token.lastIndex, end)
        token.lastIndex = end + open.length + 3
      } else if (!VOID.has(open)) current = el
    }
  }
  assert.ok(current === top, 'every element is closed')
  doc.documentElement = top.querySelector('html')
  doc.body = top.querySelector('body')
  return doc
}

/** The rendered page: a UI area with two clips and stills, one undisclosed change and ticks. */
function pageHtml({ media = true } = {}) {
  const proof = withDigest({
    schema: 'boss-proof/1',
    repo: 'acme/edge',
    pr: 9,
    head: HEAD,
    base: '4'.repeat(40),
    confidence: {
      level: 'moderate',
      asks: ['ui'],
      loweredBy: [{ code: 'gap', text: 'Totals need a person' }],
    },
    whatItDoes: { source: 'pr-body', bullets: ['Shows the basket total'] },
    areas: [
      {
        name: 'ui',
        label: 'UI',
        ask: true,
        items: ['ui fact'],
        undisclosed: true,
        unmentioned: ['Basket.total'],
        media: [],
        proven: false,
      },
    ],
    review: { models: [{ agent: 'reviewer', outcome: 'ran' }], blockers: [], notes: [] },
    health: [],
    page: null,
  })
  const clip = (id, title, chapters) => ({
    id,
    side: 'head',
    title,
    file: `${id}.mp4`,
    durationMs: 10_000,
    chapters,
    stills: [{ file: `${id}-still.png`, caption: `${title} still` }],
  })
  return renderProofHtml(
    {
      proof,
      afterMerge: ['Watch the rollout'],
      filesUrl: 'https://github.com/acme/edge/pull/9/files',
    },
    {
      spec: {
        schema: 'proof-spec/1',
        head: HEAD,
        waiting: [{ title: 'Try a basket' }, { title: 'Read the diff' }],
      },
      ...(media
        ? {
            media: {
              schema: 'proof-media/1',
              head: HEAD,
              clips: [
                clip('head-0', 'Basket', [
                  { atMs: 1_000, caption: 'Basket opens' },
                  { atMs: 5_000, caption: 'Basket totals' },
                ]),
                clip('head-1', 'Checkout', [{ atMs: 2_000, caption: 'Checkout opens' }]),
              ],
              transcripts: [],
            },
          }
        : {}),
    },
  )
}

/** A storage that records what the player writes. */
function memoryStorage(entries = {}) {
  const map = new Map(Object.entries(entries))
  return {
    map,
    getItem: (key) => (map.has(key) ? map.get(key) : null),
    setItem: (key, value) => map.set(key, String(value)),
  }
}

/**
 * Runs player.js on the rendered page. `search` and `hash` are the URL's, `storage` the window's
 * localStorage (a getter that throws when it is `'throws'`), and `before` edits the DOM first.
 */
function page({
  html = pageHtml(),
  search = '',
  hash = '',
  framed = false,
  storage = memoryStorage(),
  before = () => {},
  now = null,
} = {}) {
  const doc = parsePage(html)
  const history = {
    urls: [],
    replaceState(_data, _unused, url) {
      this.urls.push(url)
      throw new Error('SecurityError: opaque origin')
    },
  }
  const win = Object.assign(new El(doc, '#window'), {
    location: { hash, search, pathname: '/acme/edge/9/index.html' },
    history,
    getComputedStyle: () => ({
      paddingLeft: '32px',
      paddingRight: '32px',
      paddingTop: '0px',
      paddingBottom: '32px',
    }),
  })
  if (storage === 'throws')
    Object.defineProperty(win, 'localStorage', {
      get: () => {
        throw new Error('SecurityError: storage is blocked')
      },
    })
  else win.localStorage = storage
  const parent = {
    posted: [],
    postMessage(message, targetOrigin) {
      this.posted.push([message, targetOrigin])
    },
  }
  win.top = framed ? parent : win
  win.parent = framed ? parent : win
  doc.defaultView = win
  const video = doc.getElementById('proof-video')
  if (video)
    Object.assign(video, {
      currentTime: 0,
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
  const stage = doc.getElementById('lb-stage')
  if (stage) Object.assign(stage, { clientWidth: 1000, clientHeight: 800 })
  before(doc)
  /** The clock the page reads, stopped at `now` when given. */
  class Clock extends Date {
    static now() {
      return now ?? Date.now()
    }
  }
  vm.runInNewContext(proofPlayerScript(), { document: doc, URLSearchParams, Date: Clock })
  /** Click `el` as the browser would; returns whether the default action was prevented. */
  const click = (el, extra = {}) => {
    const event = {
      target: el,
      defaultPrevented: false,
      button: 0,
      clientX: 0,
      clientY: 0,
      ...extra,
      preventDefault() {
        this.defaultPrevented = true
      },
    }
    for (const listener of doc.listeners.click ?? []) listener(event)
    return event.defaultPrevented
  }
  const key = (name, extra = {}) => {
    const event = {
      key: name,
      defaultPrevented: false,
      ...extra,
      preventDefault() {
        this.defaultPrevented = true
      },
    }
    for (const listener of doc.listeners.keydown ?? []) listener(event)
    return event.defaultPrevented
  }
  const $ = (selector) => {
    const found = doc.documentElement.querySelector(selector)
    assert.ok(found, `the page has ${selector}`)
    return found
  }
  const buttons = doc.querySelectorAll('button[data-chapter]')
  const current = () => buttons.findIndex((b) => b.classList.contains('cur'))
  const lb = doc.getElementById('lb')
  const img = doc.getElementById('lb-img')
  return {
    doc,
    win,
    parent,
    history,
    storage,
    root: doc.documentElement,
    video,
    label: doc.getElementById('proof-clip'),
    buttons,
    current,
    lb,
    img,
    stage,
    zoom: doc.getElementById('lb-zoom'),
    shut: doc.getElementById('lb-close'),
    prev: doc.getElementById('lb-prev'),
    next: doc.getElementById('lb-next'),
    click,
    key,
    $,
  }
}

/** Opens the lightbox from its first screenshot and loads an image of `width` by `height`. */
function openShot(p, width, height) {
  const shot = p.$('a.shot')
  assert.equal(p.click(shot.querySelector('img')), true, 'the screenshot link does not navigate')
  Object.assign(p.img, { naturalWidth: width, naturalHeight: height, complete: true })
  p.img.fire('load')
  return shot
}

test('player.js is the published name', () => {
  assert.equal(PROOF_PLAYER_FILE, 'player.js')
})

test('?theme picks light or dark and ignores anything else; the root always gets proof-js', () => {
  for (const [search, theme] of [
    ['?theme=light', 'light'],
    ['?theme=dark', 'dark'],
    ['?theme=blue', null],
    ['', null],
  ]) {
    const { root } = page({ search })
    assert.equal(root.getAttribute('data-theme'), theme, search || 'no query')
    assert.equal(root.classList.contains('proof-js'), true)
  }
})

test('?debug reveals the generation diagnostics and nothing else does', () => {
  assert.equal(page({ search: '?debug' }).root.classList.contains('proof-debug'), true)
  assert.equal(page({ search: '?theme=dark&debug=1' }).root.classList.contains('proof-debug'), true)
  assert.equal(page({ search: '' }).root.classList.contains('proof-debug'), false)
})

test('a tick restores from this browser, is written on change and folds a decided item', () => {
  const prefix = `proof:acme/edge#9@${HEAD}:`
  const storage = memoryStorage({ [`${prefix}decide-1`]: '1', [`${prefix}decide-2`]: '0' })
  const p = page({ storage })
  const first = p.$('#decide-1')
  const second = p.$('#decide-2')
  assert.equal(first.querySelector('input.check').checked, true, 'a stored 1 ticks the item')
  assert.equal(first.classList.contains('done'), true)
  assert.equal(second.querySelector('input.check').checked, false, 'a stored 0 unticks it')
  assert.equal(second.classList.contains('done'), false)
  const after = p.$('#after-1')
  assert.equal(after.classList.contains('done'), false, 'nothing stored leaves the page as it is')
  const box = after.querySelector('input.check')
  box.checked = true
  box.fire('change')
  assert.equal(storage.map.get(`${prefix}after-1`), '1')
  assert.equal(after.classList.contains('done'), true)
  first.classList.toggle('open', true)
  const firstBox = first.querySelector('input.check')
  firstBox.checked = false
  firstBox.fire('change')
  assert.equal(storage.map.get(`${prefix}decide-1`), '0')
  assert.equal(first.classList.contains('done'), false)
  assert.equal(first.classList.contains('open'), false, 'a change folds the item again')
})

test('ticks still toggle when storage is missing, blocked or full', () => {
  const full = {
    getItem: () => {
      throw new Error('blocked')
    },
    setItem: () => {
      throw new Error('QuotaExceededError')
    },
  }
  for (const storage of [undefined, 'throws', full]) {
    const p = page({ storage })
    const item = p.$('#decide-1')
    const box = item.querySelector('input.check')
    box.checked = true
    assert.doesNotThrow(() => box.fire('change'))
    assert.equal(item.classList.contains('done'), true, String(storage))
  }
})

test('a click on a decided item opens it for reference, but not a click on its box or a link', () => {
  const p = page({ storage: memoryStorage({ [`proof:acme/edge#9@${HEAD}:decide-2`]: '1' }) })
  const done = p.$('#decide-2')
  assert.equal(done.classList.contains('done'), true, 'the stored tick decided it')
  p.click(done.querySelector('.q'))
  assert.equal(done.classList.contains('open'), true)
  p.click(done.querySelector('.hd'))
  assert.equal(done.classList.contains('open'), false, 'a second click folds it')
  p.click(done.querySelector('input.check'))
  assert.equal(done.classList.contains('open'), false, 'the tick box is left to tick')
  const link = done.querySelector('.q').appendChild(p.doc.createElement('a'))
  link.setAttribute('href', 'https://example.com/x')
  p.click(link)
  assert.equal(done.classList.contains('open'), false, 'a link inside it is left to navigate')
  const open = p.$('#decide-1')
  p.click(open.querySelector('.q'))
  assert.equal(open.classList.contains('open'), false, 'an undecided item has nothing to unfold')
})

test('a screenshot opens the lightbox at its own size, shrunk only to fit the stage', () => {
  const p = page()
  assert.equal(p.lb.hidden, true, 'the lightbox starts hidden')
  const shot = openShot(p, 1872, 768)
  assert.equal(p.lb.hidden, false)
  assert.equal(p.img.src, shot.getAttribute('href'))
  assert.ok(p.doc.getElementById('lb-cap').textContent.startsWith(shot.getAttribute('data-cap')))
  assert.deepEqual(
    [p.img.style.width, p.img.style.height],
    ['936px', '384px'],
    'shrunk to the stage less its padding',
  )
  assert.equal(p.zoom.disabled, false)
  p.click(p.img)
  assert.deepEqual([p.img.style.width, p.img.style.height], ['1872px', '768px'], 'full size')
  assert.equal(p.zoom.getAttribute('aria-pressed'), 'true')
  assert.equal(p.lb.hidden, false, 'a click on the image never closes it')
  p.click(p.img)
  assert.equal(p.img.style.width, '936px', 'a second click fits it again')

  const small = page()
  openShot(small, 400, 300)
  assert.deepEqual(
    [small.img.style.width, small.img.style.height],
    ['400px', '300px'],
    'never upscaled',
  )
  assert.equal(small.zoom.disabled, true, 'nothing to zoom')
  small.click(small.img)
  assert.equal(small.img.style.width, '400px', 'a click on an unshrunk image changes nothing')
  assert.equal(small.zoom.getAttribute('aria-pressed'), 'false', 'and claims no full size')
})

test('Escape, a click outside the image and the close button shut the lightbox and refocus its opener', () => {
  for (const shut of ['escape', 'outside', 'button']) {
    const p = page()
    const shot = openShot(p, 1872, 768)
    assert.ok(p.doc.activeElement === p.shut, 'focus moves into the lightbox')
    if (shut === 'escape') assert.equal(p.key('Escape'), true)
    else if (shut === 'outside') p.click(p.stage)
    else p.click(p.shut.querySelector('svg'))
    assert.equal(p.lb.hidden, true, shut)
    assert.equal(p.img.getAttribute('src'), null, `${shut} unloads the image`)
    assert.ok(p.doc.activeElement === shot, `${shut} returns focus to the screenshot`)
  }
})

test('Tab cycles only the lightbox buttons that are enabled', () => {
  const p = page()
  openShot(p, 1872, 768)
  assert.equal(p.key('Tab'), true)
  assert.ok(p.doc.activeElement === p.next, 'Previous is disabled on the first screenshot')
  p.key('Tab')
  assert.ok(p.doc.activeElement === p.zoom)
  p.key('Tab')
  assert.ok(p.doc.activeElement === p.shut)
  p.key('Tab', { shiftKey: true })
  assert.ok(p.doc.activeElement === p.zoom)
  const small = page()
  openShot(small, 400, 300)
  small.key('Tab')
  assert.ok(small.doc.activeElement === small.next)
  small.key('Tab')
  assert.ok(small.doc.activeElement === small.shut, 'a disabled Full size button is skipped')
  small.key('Escape')
  assert.equal(small.key('Tab'), false, 'a closed lightbox leaves Tab to the page')
})

test('Left and Right step through the screenshots in page order, stop at the ends and refit each one', () => {
  const p = page()
  const shots = p.doc.querySelectorAll('a.shot')
  assert.equal(shots.length, 2, 'the page has two screenshots')
  assert.equal(p.key('ArrowRight'), false, 'a closed lightbox leaves the arrows to the page')
  openShot(p, 1872, 768)
  assert.equal(p.prev.disabled, true, 'nothing before the first')
  assert.equal(p.next.disabled, false)
  p.click(p.img)
  assert.equal(p.img.style.width, '1872px', 'full size before the step')
  assert.equal(p.key('ArrowRight'), true, 'the arrow is taken from the page')
  assert.equal(p.img.src, shots[1].getAttribute('href'))
  assert.ok(
    p.doc.getElementById('lb-cap').textContent.startsWith(shots[1].getAttribute('data-cap')),
  )
  Object.assign(p.img, { naturalWidth: 1872, naturalHeight: 768, complete: true })
  p.img.fire('load')
  assert.equal(p.img.style.width, '936px', 'each step starts at fit size')
  assert.equal(p.zoom.getAttribute('aria-pressed'), 'false')
  assert.equal([p.prev.disabled, p.next.disabled].join(), 'false,true')
  p.key('ArrowRight')
  assert.equal(p.img.src, shots[1].getAttribute('href'), 'no wrap past the last')
  p.click(p.prev)
  assert.equal(p.img.src, shots[0].getAttribute('href'), 'the Previous button steps back')
  p.key('ArrowLeft')
  assert.equal(p.img.src, shots[0].getAttribute('href'), 'no wrap before the first')
  p.key('Escape')
  assert.ok(p.doc.activeElement === shots[0], 'focus returns to the screenshot that opened it')
})

test('a page with one screenshot has no Previous or Next buttons', () => {
  const html = renderProofHtml(
    {
      proof: withDigest({
        schema: 'boss-proof/1',
        repo: 'acme/edge',
        pr: 9,
        head: HEAD,
        base: '4'.repeat(40),
        confidence: { level: 'moderate', asks: ['ui'], loweredBy: [{ code: 'gap', text: 'x' }] },
        whatItDoes: { source: 'unknown', bullets: [] },
        areas: [
          {
            name: 'ui',
            label: 'UI',
            ask: true,
            items: ['ui fact'],
            undisclosed: false,
            media: [],
            proven: false,
          },
        ],
        review: { models: [], blockers: [], notes: [] },
        health: [],
        page: null,
      }),
    },
    {
      media: {
        schema: 'proof-media/1',
        head: HEAD,
        clips: [
          {
            id: 'a',
            side: 'head',
            title: 'A',
            file: 'a.mp4',
            durationMs: 1000,
            chapters: [],
            stills: [{ file: 'a.png', caption: 'A still' }],
          },
        ],
        transcripts: [],
      },
      mediaSize: () => 10,
    },
  )
  assert.ok(html.includes('id="lb-zoom"'), 'the lightbox is there')
  assert.ok(!html.includes('id="lb-prev"') && !html.includes('id="lb-next"'))
})

test('an open lightbox pauses a playing video and closing resumes it; a paused one stays paused', () => {
  const p = page()
  p.video.paused = false
  openShot(p, 1872, 768)
  assert.equal(p.video.paused, true)
  p.key('Escape')
  assert.equal(p.video.paused, false)
  assert.equal(p.video.plays, 1)
  const still = page()
  openShot(still, 1872, 768)
  still.key('Escape')
  assert.equal(still.video.plays, 0, 'a video that was not playing is not started')
})

test('a chapter on the current clip seeks, plays and marks only itself', async () => {
  const { video, buttons, current } = page()
  assert.equal(current(), -1)
  buttons[1].fire('click')
  assert.equal(video.currentTime, 5)
  assert.equal(video.plays, 1)
  assert.equal(video.src, 'media/head-0.mp4')
  assert.equal(current(), 1)
  assert.equal(buttons[1].getAttribute('aria-current'), 'true')
  assert.equal(buttons[0].getAttribute('aria-current'), null)
  video.currentTime = 1.5
  video.fire('timeupdate')
  assert.equal(current(), 0)
  await new Promise((resolve) => setImmediate(resolve))
})

test('a chapter on another clip switches source, then seeks once its metadata loads', () => {
  const { video, label, buttons, current } = page()
  buttons[2].fire('click')
  assert.equal(video.src, 'media/head-1.mp4')
  assert.equal(video.poster, 'media/head-1-still.png')
  assert.equal(video.dataset.clip, '1')
  assert.equal(label.textContent, 'Checkout')
  assert.equal(video.plays, 0)
  video.fire('loadedmetadata')
  assert.equal(video.currentTime, 2)
  assert.equal(video.plays, 1)
  assert.equal(current(), 2)
  video.currentTime = 5.5
  video.fire('timeupdate')
  assert.equal(current(), 2, 'clip 0 chapters stay unmarked while clip 1 plays')
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

/** Puts an element with id `deep` inside a collapsed <details>, as a Files group holds rows. */
const collapsed = (doc) => {
  const box = doc.body.appendChild(doc.createElement('details'))
  box.appendChild(doc.createElement('div')).setAttribute('id', 'deep')
}

test('a link to an element in a collapsed group opens it on load and on hash change', () => {
  const opened = page({ hash: '#deep', before: collapsed })
  const deep = opened.doc.getElementById('deep')
  assert.equal(deep.parentElement.open, true)
  assert.equal(deep.scrolled, 1)
  const later = page({ before: collapsed })
  const target = later.doc.getElementById('deep')
  assert.equal(target.parentElement.open, false)
  later.win.location.hash = '#deep'
  later.win.fire('hashchange')
  assert.equal(target.parentElement.open, true)
})

test('in-page links never navigate: a banner risk link scrolls and only tries to replace the URL', () => {
  const p = page({ before: collapsed })
  const risk = p.$('section.banner a')
  assert.equal(risk.getAttribute('href'), '#risk-1', 'the banner leads with its risk')
  assert.equal(p.click(risk.querySelector('span')), true, 'the anchor navigation is prevented')
  assert.equal(p.doc.getElementById('risk-1').scrolled, 1)
  assert.deepEqual(p.history.urls, ['#risk-1'], 'replaceState is tried and its error caught')
  assert.equal(p.win.location.hash, '', 'the hash never changes, so the review tab gets no Back')
  const link = (href) => {
    const a = p.doc.body.appendChild(p.doc.createElement('a'))
    a.setAttribute('href', href)
    return a
  }
  assert.equal(p.click(link('#deep')), true)
  assert.equal(p.doc.getElementById('deep').parentElement.open, true, 'a collapsed group opens')
  assert.equal(p.click(link('#nowhere')), false, 'a link to no element is left to the browser')
  assert.equal(p.click(link('https://example.com/x')), false, 'an outside link is left alone')
  assert.equal(p.click(link('#risk-1'), { metaKey: true }), false, 'a modified click is left alone')
})

test('on a page with no video the player still handles links and lightboxes, and touches no video', () => {
  const html = pageHtml({ media: false }).replace(
    '<script src="player.js"',
    '<a class="shot" href="media/one.png" data-cap="One"><img src="media/one.png" alt="One"></a><div class="lb" id="lb" hidden><div class="lb-bar"><div class="lb-cap" id="lb-cap"></div><button class="iconbtn" id="lb-zoom" type="button"></button><button class="iconbtn" id="lb-close" type="button"></button></div><div class="lb-stage" id="lb-stage"><img id="lb-img" alt=""></div></div><script src="player.js"',
  )
  const p = page({ html })
  assert.ok(p.video === null)
  assert.equal(p.buttons.length, 0)
  assert.equal(p.click(p.$('section.banner a')), true)
  openShot(p, 1872, 768)
  assert.equal(p.lb.hidden, false)
  p.key('Escape')
  assert.equal(p.lb.hidden, true)
})

test('a framed page marks its root, so links a sandbox cannot follow give way to a note', () => {
  assert.equal(page({ framed: true }).root.classList.contains('proof-framed'), true)
  assert.equal(page().root.classList.contains('proof-framed'), false, 'a page in its own tab')
})

test('a page with its own storage posts nothing to its parent or itself, framed or in its own tab', () => {
  for (const framed of [true, false]) {
    const { win, parent } = page({ framed })
    win.postMessage = () => assert.fail('the page posted to its own window')
    win.fire('load')
    if (framed) win.fire('message', { source: parent, origin: APP, data: THEME_DARK })
    assert.deepEqual(parent.posted, [], framed ? 'a framed page' : 'a page in its own tab')
  }
})

const APP = 'https://app.example'
const THEME_DARK = { type: 'proof:theme', theme: 'dark' }
/** What the page posted to its parent, as plain data from outside the page's realm. */
const sent = (p) => JSON.parse(JSON.stringify(p.parent.posted))

test('a framed page without storage keeps its ticks in the app, and only after the app speaks', () => {
  const prefix = `proof:acme/edge#9@${HEAD}:`
  const p = page({ framed: true, storage: 'throws' })
  const keys = p.doc
    .querySelectorAll('[data-tick]')
    .map((el) => prefix + el.getAttribute('data-tick'))
  assert.ok(keys.includes(`${prefix}decide-1`) && keys.includes(`${prefix}after-1`))
  assert.deepEqual(p.parent.posted, [], 'nothing is sent before the app names its origin')
  const forger = {}
  p.win.fire('message', { source: forger, origin: 'https://evil.example', data: THEME_DARK })
  assert.deepEqual(p.parent.posted, [], 'another window cannot name the origin')
  p.win.fire('message', { source: p.parent, origin: APP, data: THEME_DARK })
  assert.deepEqual(
    sent(p),
    keys.map((key) => [{ type: 'proof:ticks:get', key }, APP]),
    'one get per item, sent only to the app',
  )
  const answer = (key, value, source = p.parent) =>
    p.win.fire('message', { source, origin: APP, data: { type: 'proof:ticks', key, value } })
  const first = p.$('#decide-1')
  const second = p.$('#decide-2')
  answer(`${prefix}decide-1`, '1', forger)
  assert.equal(first.querySelector('input.check').checked, false, 'a forged answer is ignored')
  answer(`${prefix}decide-1`, 'yes')
  assert.equal(first.querySelector('input.check').checked, false, 'a malformed value is ignored')
  answer(`${prefix}decide-1`, '1')
  assert.equal(first.querySelector('input.check').checked, true, 'a stored 1 ticks the item')
  assert.equal(first.classList.contains('done'), true)
  answer(`${prefix}decide-2`, '0')
  assert.equal(second.querySelector('input.check').checked, false, 'a stored 0 unticks it')
  assert.equal(second.classList.contains('done'), false)
  const after = p.$('#after-1')
  const box = after.querySelector('input.check')
  box.checked = true
  box.fire('change')
  assert.deepEqual(sent(p).at(-1), [
    { type: 'proof:ticks:set', key: `${prefix}after-1`, value: '1' },
    APP,
  ])
  answer(`${prefix}after-1`, '0')
  assert.equal(box.checked, true, 'an answer that lands after the reader ticked does not undo it')
})

test('a tick set before the app speaks is sent once it does', () => {
  const p = page({ framed: true, storage: 'throws' })
  const box = p.$('#after-1').querySelector('input.check')
  box.checked = true
  box.fire('change')
  assert.deepEqual(p.parent.posted, [])
  p.win.fire('message', { source: p.parent, origin: APP, data: THEME_DARK })
  assert.deepEqual(sent(p).at(-1), [
    { type: 'proof:ticks:set', key: `proof:acme/edge#9@${HEAD}:after-1`, value: '1' },
    APP,
  ])
})

test('the framing app sets the theme, and null falls back to the URL theme or the system', () => {
  const theme = (p, value, source = p.parent) =>
    p.win.fire('message', { source, origin: APP, data: { type: 'proof:theme', theme: value } })
  const p = page({ framed: true })
  theme(p, 'light')
  assert.equal(p.root.getAttribute('data-theme'), 'light')
  theme(p, 'blue')
  assert.equal(p.root.getAttribute('data-theme'), 'light', 'an unknown theme is ignored')
  theme(p, 'dark', {})
  assert.equal(p.root.getAttribute('data-theme'), 'light', 'another window cannot set it')
  theme(p, 'dark')
  assert.equal(p.root.getAttribute('data-theme'), 'dark')
  theme(p, null)
  assert.equal(p.root.getAttribute('data-theme'), null, 'null follows the system')
  const pinned = page({ framed: true, search: '?theme=light' })
  theme(pinned, 'dark')
  assert.equal(pinned.root.getAttribute('data-theme'), 'dark')
  theme(pinned, null)
  assert.equal(pinned.root.getAttribute('data-theme'), 'light', 'null falls back to ?theme')
  const alone = page()
  assert.equal(alone.win.listeners.message, undefined, 'a page in its own tab hears no messages')
})

test('the PR link opens in a new tab and the page never intercepts it, framed or not', () => {
  for (const framed of [true, false]) {
    const p = page({ framed })
    const pr = p.$('a.prlink')
    assert.equal(pr.getAttribute('href'), 'https://github.com/acme/edge/pull/9')
    assert.equal(pr.getAttribute('target'), '_blank')
    assert.equal(pr.getAttribute('rel'), 'noopener noreferrer')
    assert.equal(p.click(pr), false, 'the browser follows the link')
    assert.equal(p.parent.posted.length, 0)
  }
})

test('the footer time reads in the reader time zone, with how long ago it was', (t) => {
  const zone = process.env.TZ
  t.after(() => {
    if (zone === undefined) delete process.env.TZ
    else process.env.TZ = zone
  })
  process.env.TZ = 'Asia/Tokyo'
  const at = '2026-11-12T23:59:00.000Z'
  const html = pageHtml().replace(
    /<time datetime="[^"]+">[^<]+<\/time>/,
    `<time datetime="${at}">${at}</time>`,
  )
  const ages = [
    [0, 'just now'],
    [60_000, '1 minute ago'],
    [2 * 3_600_000 + 59 * 60_000, '2 hours ago'],
    [3 * 86_400_000, '3 days ago'],
  ]
  for (const [later, age] of ages) {
    const p = page({ html, now: Date.parse(at) + later })
    const time = p.$('time')
    assert.equal(time.textContent, `13 Nov 2026, 08:59 · ${age}`)
    assert.equal(time.getAttribute('title'), at, 'the raw time stays as its title')
  }
  process.env.TZ = 'UTC'
  assert.equal(
    page({ html, now: Date.parse(at) }).$('time').textContent,
    '12 Nov 2026, 23:59 · just now',
  )
  const bad = page({ html: html.replaceAll(at, 'not a time') })
  assert.equal(bad.$('time').textContent, 'not a time', 'an unreadable time is left alone')
})
