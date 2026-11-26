/**
 * The proof page's highlight helper, appended to `player.js`. When a reader finishes selecting text
 * in a page framed by the review app, it marks the selection and posts the text to the app, which
 * opens a reply to it beside the frame. Marks are re-styled only by the ids the helper issued; the
 * app's own messages carry ids, states and a scroll position, nothing else.
 *
 * The app trusts none of this: a hostile page can drop the helper, fake it or post its own messages,
 * so the app checks every message itself (services/web/src/features/sessions/lib/proofReplies.ts).
 * The helper only spares the reader from copying and pasting.
 *
 * Like `proofPlayer`, `proofQuote` runs in the browser from its own source text, so it references
 * nothing outside its body, and the document is untyped here.
 */

/** @param {any} doc the page's document */
export function proofQuote(doc) {
  const win = doc.defaultView
  const app = win && win.parent !== win ? win.parent : null
  if (!app || doc.documentElement.hasAttribute('data-proof-quote')) return
  doc.documentElement.setAttribute('data-proof-quote', '1')
  const MAX_TEXT = 1200
  const MAX_ANCHOR = 80
  const STATES = ['active', 'draft', 'sent']

  // Messages wait for the app's first one, whose origin then pins where they go. A sandboxed app
  // reports the opaque origin "null", the only case that falls back to '*'; nothing sent is secret.
  /** @type {string | null} */
  let target = null
  /** @type {object[]} */
  const outbox = []
  const post = (/** @type {object} */ m) => {
    if (target) app.postMessage(m, target)
    else outbox.push(m)
  }

  // A proof page's CSP admits only its own hashed stylesheet, so a <style> element would be
  // refused; a constructed sheet is CSSOM, which CSP does not govern. Other pages take either.
  // Linear's muted amber for a passage someone is commenting on, translucent so it reads on a
  // light or dark page; the open reply's passage a little stronger, a sent one fainter.
  const css = [
    'mark[data-proof-quote]{background:rgba(214,168,40,.28);color:inherit;border-radius:2px;cursor:pointer}',
    'mark[data-proof-quote][data-state="active"]{background:rgba(214,168,40,.45)}',
    'mark[data-proof-quote][data-state="sent"]{background:rgba(214,168,40,.14)}',
    'mark[data-proof-quote].proof-quote-flash{animation:proof-quote-flash 1.1s ease}',
    '@keyframes proof-quote-flash{0%,40%{background:rgba(214,168,40,.6)}}',
    '@media (prefers-reduced-motion:reduce){mark[data-proof-quote].proof-quote-flash{animation:none}}',
  ].join('\n')
  const Sheet = win.CSSStyleSheet
  if (Sheet && 'adoptedStyleSheets' in doc) {
    const sheet = new Sheet()
    sheet.replaceSync(css)
    doc.adoptedStyleSheets = [...doc.adoptedStyleSheets, sheet]
  } else if (doc.head) {
    const style = doc.createElement('style')
    style.textContent = css
    doc.head.appendChild(style)
  }

  /** Marks by the id they were posted under, one element per text node a selection crossed. @type {Map<string, any[]>} */
  const marks = new Map()
  let seq = 0

  /** The text of the last heading before `node`, so the agent knows which section a quote is from. */
  const headingBefore = (/** @type {any} */ node) => {
    let best = ''
    for (const h of doc.querySelectorAll('h1, h2, h3, h4')) {
      if (h.contains(node) || h.compareDocumentPosition(node) & 4)
        best = h.innerText ?? h.textContent ?? ''
      else break
    }
    return best.replace(/\s+/g, ' ').trim().slice(0, MAX_ANCHOR)
  }

  /** Wraps each text node the range crosses in a mark, so a highlight may span elements. */
  const wrap = (/** @type {any} */ range, /** @type {string} */ id) => {
    const { startContainer: sc, startOffset: so, endContainer: ec, endOffset: eo } = range
    const common = range.commonAncestorContainer
    const root = common.nodeType === 3 ? common.parentNode : common
    const walker = doc.createTreeWalker(root, 4, {
      acceptNode: (/** @type {any} */ n) =>
        range.intersectsNode(n) && n.data.trim() && !n.parentElement.closest('script, style')
          ? 1
          : 3,
    })
    const nodes = []
    for (let n = walker.nextNode(); n; n = walker.nextNode()) nodes.push(n)
    const els = []
    for (const node of nodes) {
      let n = node
      const start = n === sc ? so : 0
      let end = n === ec ? eo : n.data.length
      if (end <= start) continue
      if (start > 0) {
        n = n.splitText(start)
        end -= start
      }
      if (end < n.data.length) n.splitText(end)
      if (!n.data.trim()) continue
      const m = doc.createElement('mark')
      m.setAttribute('data-proof-quote', id)
      n.parentNode.insertBefore(m, n)
      m.appendChild(n)
      els.push(m)
    }
    if (els.length) marks.set(id, els)
  }

  const unwrap = (/** @type {string} */ id) => {
    for (const m of marks.get(id) ?? []) {
      const parent = m.parentNode
      if (!parent) continue
      m.replaceWith(...m.childNodes)
      parent.normalize()
    }
    marks.delete(id)
  }

  const capture = () => {
    const sel = win.getSelection()
    if (!sel || !sel.rangeCount || sel.isCollapsed) return
    const text = String(sel)
      .replace(/[ \t]+\n/g, '\n')
      .replace(/\n{3,}/g, '\n\n')
      .trim()
    if (text.length < 2) return
    const range = sel.getRangeAt(0)
    const id = `q${++seq}`
    const anchor = headingBefore(range.startContainer)
    try {
      wrap(range, id)
    } catch {
      /* The quote still goes; only its highlight is lost. */
    }
    sel.removeAllRanges()
    post({ type: 'proof:quote', v: 1, id, text: text.slice(0, MAX_TEXT), anchor })
  }

  // A highlight is finished on a mouse or pen release, once Shift-arrow selection settles, or once a
  // touch selection stops changing: until then its handles are still being dragged.
  let pointer = 'mouse'
  /** @type {any} */
  let timer = 0
  const later = (/** @type {number} */ ms) => {
    win.clearTimeout(timer)
    timer = win.setTimeout(capture, ms)
  }
  doc.addEventListener('pointerdown', (/** @type {any} */ e) => {
    pointer = e.pointerType || 'mouse'
    win.clearTimeout(timer)
  })
  doc.addEventListener('pointerup', (/** @type {any} */ e) => {
    if (e.pointerType !== 'touch') later(0)
  })
  doc.addEventListener('keyup', (/** @type {any} */ e) => {
    if (e.shiftKey || e.key === 'Shift') later(400)
  })
  doc.addEventListener('selectionchange', () => {
    if (pointer === 'touch') later(900)
  })
  doc.addEventListener('click', (/** @type {any} */ e) => {
    const m = e.target && e.target.closest ? e.target.closest('mark[data-proof-quote]') : null
    if (!m) return
    const sel = win.getSelection()
    if (!sel || sel.isCollapsed)
      post({ type: 'proof:quote:open', v: 1, id: m.getAttribute('data-proof-quote') })
  })

  win.addEventListener('message', (/** @type {any} */ event) => {
    if (event.source !== app) return
    const data = event.data
    if (!data || typeof data !== 'object') return
    if (!target) {
      target = event.origin && event.origin !== 'null' ? event.origin : '*'
      for (const m of outbox.splice(0)) app.postMessage(m, target)
    }
    if (data.type === 'proof:quote:hello') post({ type: 'proof:quote:ready', v: 1 })
    else if (data.type === 'proof:quote:marks' && Array.isArray(data.marks)) {
      /** @type {Map<string, any>} */
      const keep = new Map()
      for (const m of data.marks) if (m && typeof m.id === 'string') keep.set(m.id, m)
      for (const id of [...marks.keys()]) {
        const m = keep.get(id)
        if (!m) {
          unwrap(id)
          continue
        }
        for (const el of marks.get(id) ?? [])
          el.setAttribute('data-state', STATES.includes(m.state) ? m.state : 'draft')
      }
    } else if (data.type === 'proof:quote:focus' && typeof data.id === 'string') {
      const els = marks.get(data.id)
      if (!els) return
      const at = typeof data.at === 'number' && data.at >= 0 && data.at <= 1 ? data.at : 0.3
      const top = els[0].getBoundingClientRect().top + win.scrollY - win.innerHeight * at
      const smooth = !win.matchMedia('(prefers-reduced-motion: reduce)').matches
      win.scrollTo({ top, behavior: smooth ? 'smooth' : 'auto' })
      for (const el of els) {
        el.classList.remove('proof-quote-flash')
        void el.offsetWidth
        el.classList.add('proof-quote-flash')
      }
    }
  })
}
