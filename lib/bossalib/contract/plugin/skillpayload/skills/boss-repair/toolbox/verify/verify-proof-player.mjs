/**
 * The proof page's only script, served beside index.html as `player.js` because the page CSP is
 * `script-src 'self'`. It wires each chapter button to the one <video>: switch clip, seek, play,
 * and mark the chapter now playing. It also handles every in-page link itself, so a page framed in
 * a review tab never adds that tab a Back entry: a section link scrolls (opening a collapsed area
 * when the target is code inside it), and a still opens and closes its lightbox by class, pausing
 * the video while it is open. Without the script the same links work as plain anchors, and a
 * lightbox shows by `:target`; once it runs, the root's `proof-js` class turns that off and the
 * lightbox the URL names, if any, is adopted, so Close and Escape always shut it.
 * `proofPlayer` runs in the browser from its own source text, so it must reference nothing
 * outside its body.
 */

export const PROOF_PLAYER_FILE = 'player.js'

/**
 * @typedef {{toggle(name: string, on?: boolean): unknown, contains(name: string): boolean}} PlayerClasses
 * @typedef {{dataset: Record<string, string | undefined>, classList: PlayerClasses,
 *   setAttribute(name: string, value: string): void, removeAttribute(name: string): void,
 *   addEventListener(type: string, listener: () => void, options?: {once?: boolean}): void}} PlayerElement
 * @typedef {PlayerElement & {currentTime: number, src: string, poster: string, paused: boolean,
 *   play(): unknown, pause(): void, scrollIntoView?(options?: {block?: string}): void}} PlayerVideo
 * @typedef {{textContent: string | null}} PlayerLabel
 * @typedef {{id: string, classList: PlayerClasses, scrollIntoView(): void, closest(selector: string): any}} PlayerTarget
 * @typedef {{getAttribute(name: string): string | null, closest(selector: string): any, classList: PlayerClasses}} PlayerLink
 * @typedef {{target: {closest?(selector: string): any} | null, defaultPrevented: boolean, button?: number,
 *   metaKey?: boolean, ctrlKey?: boolean, shiftKey?: boolean, altKey?: boolean, key?: string,
 *   preventDefault(): void}} PlayerEvent
 * @typedef {{getElementById(id: string): unknown, querySelectorAll(selector: string): Iterable<unknown>,
 *   addEventListener(type: string, listener: (event: PlayerEvent) => void): void,
 *   documentElement: {classList: PlayerClasses} | null,
 *   defaultView: {top: unknown, location: {hash: string, pathname: string, search: string}, history?: {replaceState(data: null, unused: string, url: string): void},
 *   addEventListener(type: string, listener: () => void): void} | null}} PlayerDocument
 */

/** @param {PlayerDocument} doc */
export function proofPlayer(doc) {
  const win = doc.defaultView
  const video = /** @type {PlayerVideo | null} */ (doc.getElementById('proof-video'))
  /** Scroll to an element, opening the collapsed area it sits in. @param {PlayerTarget} target */
  const reveal = (target) => {
    const box = target.closest('details')
    if (box && !box.open) box.open = true
    target.scrollIntoView()
  }
  const openTarget = () => {
    const id = win ? win.location.hash.slice(1) : ''
    const target = /** @type {PlayerTarget | null} */ (id ? doc.getElementById(id) : null)
    if (target && target.classList.contains('lightbox')) return open(target)
    close()
    const box = target && target.closest('details')
    if (!target || !box || box.open) return
    reveal(target)
  }
  /** @type {PlayerTarget | null} */
  let shown = null
  let resume = false
  const close = () => {
    if (!shown) return
    shown.classList.toggle('open', false)
    shown.classList.toggle('full', false)
    if (win && win.location.hash === `#${shown.id}`)
      try {
        win.history?.replaceState(null, '', win.location.pathname + win.location.search)
      } catch {
        /* A sandboxed frame may not rewrite its URL; the class alone keeps the lightbox shut. */
      }
    shown = null
    if (resume && video) {
      const playing = /** @type {{catch?: (f: () => void) => void} | undefined} */ (video.play())
      if (playing && typeof playing.catch === 'function') playing.catch(() => {})
    }
    resume = false
  }
  /** @param {PlayerTarget} box */
  const open = (box) => {
    if (shown) {
      shown.classList.toggle('open', false)
      shown.classList.toggle('full', false)
    } else {
      resume = Boolean(video && !video.paused)
      if (resume && video) video.pause()
    }
    box.classList.toggle('open', true)
    shown = box
  }
  if (win) win.addEventListener('hashchange', openTarget)
  openTarget()
  if (doc.documentElement) {
    doc.documentElement.classList.toggle('proof-js', true)
    // A sandboxed review frame cannot download, so its page swaps download links for a note.
    doc.documentElement.classList.toggle('proof-framed', Boolean(win && win.top !== win))
  }
  doc.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') close()
  })
  doc.addEventListener('click', (event) => {
    if (
      event.defaultPrevented ||
      event.button ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    )
      return
    const link = /** @type {PlayerLink | null} */ (event.target?.closest?.('a[href]') ?? null)
    if (!link) return
    const href = link.getAttribute('href') ?? ''
    const box = /** @type {PlayerTarget | null} */ (link.closest('.lightbox'))
    if (box && link.classList.contains('lb-full')) {
      event.preventDefault()
      box.classList.toggle('full')
      return
    }
    if (!href.startsWith('#')) return
    const target = /** @type {PlayerTarget | null} */ (doc.getElementById(href.slice(1)))
    if (!target) return
    event.preventDefault()
    if (target.classList.contains('lightbox')) return open(target)
    // A link inside an open lightbox only closes it; the page stays where it was.
    if (box) return close()
    close()
    reveal(target)
    try {
      win?.history?.replaceState(null, '', href)
    } catch {
      /* A sandboxed frame has an opaque origin and may not rewrite its URL. */
    }
  })
  if (!video) return
  const label = /** @type {PlayerLabel | null} */ (doc.getElementById('proof-clip'))
  const buttons = /** @type {PlayerElement[]} */ ([...doc.querySelectorAll('button[data-chapter]')])
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
    const playing = /** @type {{catch?: (f: () => void) => void} | undefined} */ (video.play())
    if (playing && typeof playing.catch === 'function') playing.catch(() => {})
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
