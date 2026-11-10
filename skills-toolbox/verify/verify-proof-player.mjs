/**
 * The proof page's only script, served beside index.html as `player.js` because the page CSP is
 * `script-src 'self'`. It wires each chapter button to the one <video>: switch clip, seek, play,
 * and mark the chapter now playing. It also opens a collapsed area when a link targets code inside
 * it. `proofPlayer` runs in the browser from its own source text, so it must reference nothing
 * outside its body.
 */

export const PROOF_PLAYER_FILE = 'player.js'

/**
 * @typedef {{dataset: Record<string, string | undefined>, classList: {toggle(name: string, on: boolean): void},
 *   setAttribute(name: string, value: string): void, removeAttribute(name: string): void,
 *   addEventListener(type: string, listener: () => void, options?: {once?: boolean}): void}} PlayerElement
 * @typedef {PlayerElement & {currentTime: number, src: string, poster: string, play(): unknown}} PlayerVideo
 * @typedef {{textContent: string | null}} PlayerLabel
 * @typedef {{getElementById(id: string): unknown, querySelectorAll(selector: string): Iterable<unknown>,
 *   defaultView: {location: {hash: string}, addEventListener(type: string, listener: () => void): void} | null}} PlayerDocument
 * @typedef {{open: boolean, scrollIntoView(): void, closest(selector: string): {open: boolean} | null}} PlayerTarget
 */

/** @param {PlayerDocument} doc */
export function proofPlayer(doc) {
  const win = doc.defaultView
  const openTarget = () => {
    const id = win ? win.location.hash.slice(1) : ''
    const target = /** @type {PlayerTarget | null} */ (id ? doc.getElementById(id) : null)
    const box = target && target.closest('details')
    if (!target || !box || box.open) return
    box.open = true
    target.scrollIntoView()
  }
  if (win) win.addEventListener('hashchange', openTarget)
  openTarget()
  const video = /** @type {PlayerVideo | null} */ (doc.getElementById('proof-video'))
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
