/**
 * The proof page's only script, served beside index.html as `player.js` because the page CSP is
 * `script-src 'self'`. It applies `?theme=light|dark` and `?debug`, shows the footer's build time in
 * the reader's time zone, keeps each tick a reader sets
 * on a decision, an after-merge step or a repository section's item in this browser, takes the
 * theme of a review app that frames it, folds a decided item or a ticked step to one line, opens a
 * screenshot in a lightbox at its actual size, and drives the one <video> from its chapter list.
 * It handles in-page links itself, so a page framed in a review tab never adds that tab a Back
 * entry. Without the script the page still reads: links are plain anchors, a screenshot opens as
 * the image, and the theme follows the system.
 * `proofPlayer` runs in the browser from its own source text, so it must reference nothing
 * outside its body. The checked type set has no DOM library, so the document is untyped here.
 */

export const PROOF_PLAYER_FILE = 'player.js'

/** @param {any} doc the page's document */
export function proofPlayer(doc) {
  const win = doc.defaultView
  const root = doc.documentElement
  const params = new URLSearchParams(win ? win.location.search : '')
  const isTheme = (/** @type {unknown} */ t) => t === 'light' || t === 'dark'
  const urlTheme = params.get('theme')
  /** An explicit theme, else the URL's, else none, so the CSS follows the system. */
  const setTheme = (/** @type {unknown} */ t) => {
    const pick = isTheme(t) ? t : isTheme(urlTheme) ? urlTheme : null
    if (pick) root.setAttribute('data-theme', pick)
    else root.removeAttribute('data-theme')
  }
  setTheme(null)
  root.classList.toggle('proof-debug', params.has('debug'))
  root.classList.toggle('proof-js', true)
  // The footer's build time in the reader's own time zone, with its age: `13 Nov 2026, 08:59 ·
  // 2 hours ago`. The raw ISO time stays as the element's title.
  const MONTHS = [
    'Jan',
    'Feb',
    'Mar',
    'Apr',
    'May',
    'Jun',
    'Jul',
    'Aug',
    'Sep',
    'Oct',
    'Nov',
    'Dec',
  ]
  const pad = (/** @type {number} */ n) => String(n).padStart(2, '0')
  const count = (/** @type {number} */ n, /** @type {string} */ unit) =>
    `${n} ${unit}${n === 1 ? '' : 's'} ago`
  for (const time of doc.querySelectorAll('time[datetime]')) {
    const iso = time.getAttribute('datetime') ?? ''
    const at = new Date(iso)
    if (Number.isNaN(at.getTime())) continue
    const minutes = Math.max(0, Math.floor((Date.now() - at.getTime()) / 60000))
    const age =
      minutes < 1
        ? 'just now'
        : minutes < 60
          ? count(minutes, 'minute')
          : minutes < 1440
            ? count(Math.floor(minutes / 60), 'hour')
            : count(Math.floor(minutes / 1440), 'day')
    time.setAttribute('title', iso)
    time.textContent = `${at.getDate()} ${MONTHS[at.getMonth()]} ${at.getFullYear()}, ${pad(at.getHours())}:${pad(at.getMinutes())} · ${age}`
  }
  // A sandboxed review frame cannot download, so its page swaps download links for a note.
  root.classList.toggle('proof-framed', Boolean(win && win.top !== win))
  const video = doc.getElementById('proof-video')
  /** Play without surfacing an autoplay refusal. */
  const play = () => {
    const playing = video.play()
    if (playing && typeof playing.catch === 'function') playing.catch(() => {})
  }

  // Ticks live in this browser, per repository, PR, head and item. A sandboxed review frame has an
  // opaque origin and no storage, so it keeps them in the framing app's storage instead: it asks
  // with `proof:ticks:get` and `proof:ticks:set`, and the app answers a get with `proof:ticks`.
  // Messages wait for the app's first one, which names the origin to send them to, so ticks never
  // reach a window that is not the app. The app also sends `proof:theme`, its explicit theme or null.
  /** @type {any} */
  let store = null
  try {
    store = win ? win.localStorage : null
    if (store) store.getItem('proof:')
  } catch {
    store = null
  }
  const parent = win && win.parent !== win ? win.parent : null
  const bridged = !store && Boolean(parent)
  /** The app's origin once it has spoken; until then outgoing messages queue. @type {string|null} */
  let appOrigin = null
  /** @type {object[]} */
  const outbox = []
  const send = (/** @type {object} */ message) => {
    if (appOrigin) parent.postMessage(message, appOrigin)
    else outbox.push(message)
  }
  /** Items whose stored tick the app has not answered yet, by key. @type {Map<string, any>} */
  const awaiting = new Map()
  const isTick = (/** @type {unknown} */ v) => v === '1' || v === '0'
  const restore = (/** @type {any} */ item, /** @type {unknown} */ saved) => {
    if (!isTick(saved)) return
    const box = item.querySelector('input.check')
    box.checked = saved === '1'
    item.classList.toggle('done', box.checked)
  }
  const prefix = `proof:${doc.body ? (doc.body.getAttribute('data-proof') ?? '') : ''}:`
  for (const item of doc.querySelectorAll('[data-tick]')) {
    const box = item.querySelector('input.check')
    const key = prefix + item.getAttribute('data-tick')
    if (bridged) {
      awaiting.set(key, item)
      send({ type: 'proof:ticks:get', key })
    } else if (store) {
      restore(item, store.getItem(key))
    }
    box.addEventListener('change', () => {
      item.classList.toggle('done', box.checked)
      item.classList.toggle('open', false)
      const value = box.checked ? '1' : '0'
      awaiting.delete(key)
      if (bridged) send({ type: 'proof:ticks:set', key, value })
      else
        try {
          if (store) store.setItem(key, value)
        } catch {
          /* A full or blocked store keeps the tick for this visit only. */
        }
    })
  }
  if (parent)
    win.addEventListener('message', (/** @type {any} */ event) => {
      const data = event.source === parent ? event.data : null
      if (!data || typeof data !== 'object') return
      if (!appOrigin && event.origin && event.origin !== 'null') {
        appOrigin = event.origin
        for (const message of outbox.splice(0)) parent.postMessage(message, appOrigin)
      }
      if (data.type === 'proof:theme' && (data.theme === null || isTheme(data.theme)))
        setTheme(data.theme)
      else if (
        data.type === 'proof:ticks' &&
        awaiting.has(data.key) &&
        (data.value === null || isTick(data.value))
      ) {
        restore(awaiting.get(data.key), data.value)
        awaiting.delete(data.key)
      }
    })

  // The lightbox: actual size, shrunk only to fit; a click on a shrunk image shows it full size.
  const lb = doc.getElementById('lb')
  const stage = doc.getElementById('lb-stage')
  const img = doc.getElementById('lb-img')
  const cap = doc.getElementById('lb-cap')
  const zoom = doc.getElementById('lb-zoom')
  const shut = doc.getElementById('lb-close')
  const prev = doc.getElementById('lb-prev')
  const next = doc.getElementById('lb-next')
  /** Every screenshot on the page, in document order; arrows step through them. @type {any[]} */
  let shots = []
  let at = -1
  /** @type {any} */
  let opener = null
  let full = false
  let resume = false
  const scale = () => {
    const style = win && win.getComputedStyle ? win.getComputedStyle(stage) : null
    const pad = (/** @type {string} */ side) => (style ? parseFloat(style[side]) || 0 : 0)
    const w = stage.clientWidth - pad('paddingLeft') - pad('paddingRight')
    const h = stage.clientHeight - pad('paddingTop') - pad('paddingBottom')
    return Math.max(0.05, Math.min(1, w / (img.naturalWidth || 1), h / (img.naturalHeight || 1)))
  }
  const sync = () => {
    if (!img.naturalWidth) return
    const k = full ? 1 : scale()
    img.style.width = `${Math.round(img.naturalWidth * k)}px`
    img.style.height = `${Math.round(img.naturalHeight * k)}px`
    const shrunk = scale() < 0.999
    stage.classList.toggle('full', full)
    stage.classList.toggle('can-zoom', !full && shrunk)
    zoom.setAttribute('aria-pressed', String(full))
    zoom.disabled = !full && !shrunk
    cap.querySelector('span').textContent =
      `${img.naturalWidth}×${img.naturalHeight} · ${Math.round(k * 100)}%`
  }
  const show = (/** @type {number} */ index) => {
    at = index
    const link = shots[at]
    full = false
    stage.scrollLeft = 0
    stage.scrollTop = 0
    cap.textContent = link.getAttribute('data-cap') ?? ''
    cap.appendChild(doc.createElement('span'))
    img.alt = link.getAttribute('data-cap') ?? ''
    img.style.width = ''
    img.style.height = ''
    img.addEventListener('load', sync, { once: true })
    img.src = link.getAttribute('href')
    if (prev) prev.disabled = at === 0
    if (next) next.disabled = at === shots.length - 1
    if (img.complete) sync()
  }
  /** Step to the previous or next screenshot, stopping at either end. @param {number} by */
  const step = (by) => {
    const to = Math.min(shots.length - 1, Math.max(0, at + by))
    if (to !== at) show(to)
  }
  /** @param {any} link */
  const open = (link) => {
    opener = link
    shots = [...doc.querySelectorAll('a.shot')]
    show(Math.max(0, shots.indexOf(link)))
    lb.hidden = false
    doc.body.classList.toggle('lb-open', true)
    resume = Boolean(video && !video.paused)
    if (resume) video.pause()
    shut.focus()
  }
  const close = () => {
    if (!lb || lb.hidden) return
    lb.hidden = true
    img.removeAttribute('src')
    doc.body.classList.toggle('lb-open', false)
    if (opener && typeof opener.focus === 'function') opener.focus()
    opener = null
    if (resume && video) play()
    resume = false
  }
  /** @param {any} [event] */
  const toggleFull = (event) => {
    if (!full && scale() >= 0.999) return
    const r = img.getBoundingClientRect()
    const fx = event && r.width ? (event.clientX - r.left) / r.width : 0.5
    const fy = event && r.height ? (event.clientY - r.top) / r.height : 0.5
    full = !full
    sync()
    stage.scrollLeft = full ? fx * img.offsetWidth - stage.clientWidth / 2 : 0
    stage.scrollTop = full ? fy * img.offsetHeight - stage.clientHeight / 2 : 0
  }
  if (win && lb) win.addEventListener('resize', () => lb.hidden || sync())

  /** Scroll to an element, opening the collapsed group it sits in. @param {any} target */
  const reveal = (target) => {
    const box = target.closest('details')
    if (box && !box.open) box.open = true
    target.scrollIntoView()
  }
  const onHash = () => {
    const id = win ? win.location.hash.slice(1) : ''
    const target = id ? doc.getElementById(id) : null
    const box = target && target.closest('details')
    if (box && !box.open) reveal(target)
  }
  if (win) win.addEventListener('hashchange', onHash)
  onHash()

  doc.addEventListener('keydown', (/** @type {any} */ event) => {
    if (!lb || lb.hidden) return
    if (event.key === 'Escape') {
      event.preventDefault()
      close()
    } else if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
      event.preventDefault()
      step(event.key === 'ArrowLeft' ? -1 : 1)
    } else if (event.key === 'Tab') {
      const buttons = [prev, next, zoom, shut].filter((b) => b && !b.disabled)
      const at = buttons.indexOf(doc.activeElement)
      event.preventDefault()
      buttons[(at + (event.shiftKey ? -1 : 1) + buttons.length) % buttons.length].focus()
    }
  })
  doc.addEventListener('click', (/** @type {any} */ event) => {
    if (
      event.defaultPrevented ||
      event.button ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    )
      return
    const target = event.target
    if (!target || typeof target.closest !== 'function') return
    if (lb && !lb.hidden && lb.contains(target)) {
      if (target === img) toggleFull(event)
      else if (target.closest('#lb-zoom')) toggleFull()
      else if (target.closest('#lb-prev')) step(-1)
      else if (target.closest('#lb-next')) step(1)
      else close()
      return
    }
    // A decided item or ticked step is one line; a click on it, not on its box or a link, opens it
    // for reference.
    const head = target.closest('.item.done .hd, .si.rich.done .hd')
    if (head && !target.closest('input, a')) {
      head.parentElement.classList.toggle('open')
      return
    }
    const link = target.closest('a[href]')
    if (!link) return
    if (lb && link.classList.contains('shot')) {
      event.preventDefault()
      return open(link)
    }
    const href = link.getAttribute('href') ?? ''
    if (!href.startsWith('#')) return
    const dest = doc.getElementById(href.slice(1))
    if (!dest) return
    event.preventDefault()
    reveal(dest)
    try {
      win.history.replaceState(null, '', href)
    } catch {
      /* A sandboxed frame has an opaque origin and may not rewrite its URL. */
    }
  })

  if (!video) return
  const label = doc.getElementById('proof-clip')
  const buttons = [...doc.querySelectorAll('button[data-chapter]')]
  let clip = video.dataset.clip
  const mark = () => {
    const now = video.currentTime + 0.05
    let current = null
    for (const button of buttons)
      if (button.dataset.clip === clip && Number(button.dataset.t) <= now) current = button
    for (const button of buttons) {
      button.classList.toggle('cur', button === current)
      if (button === current) button.setAttribute('aria-current', 'true')
      else button.removeAttribute('aria-current')
    }
  }
  /** The time to seek to once a newly chosen clip has loaded; the latest click wins. */
  let pending = /** @type {number | null} */ (null)
  const seek = (/** @type {number} */ t) => {
    video.currentTime = t
    play()
    mark()
  }
  video.addEventListener('loadedmetadata', () => {
    if (pending === null) return
    const t = pending
    pending = null
    seek(t)
  })
  for (const button of buttons)
    button.addEventListener('click', () => {
      // A long chapter list can leave the video above the fold.
      if (typeof video.scrollIntoView === 'function') video.scrollIntoView({ block: 'nearest' })
      const t = Number(button.dataset.t)
      if (button.dataset.clip === clip) {
        if (pending === null) seek(t)
        else pending = t
        return
      }
      pending = t
      clip = button.dataset.clip
      video.dataset.clip = clip
      video.poster = button.dataset.poster ?? ''
      video.src = button.dataset.src ?? ''
      if (label) label.textContent = button.dataset.title ?? ''
    })
  video.addEventListener('timeupdate', mark)
  mark()
}

/** The text of `player.js`: `proofPlayer` applied to the page's document. */
export const proofPlayerScript = () => `(${proofPlayer.toString()})(document)\n`
