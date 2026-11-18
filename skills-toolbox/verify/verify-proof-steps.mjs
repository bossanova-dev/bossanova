/** Import-free Playwright steps. Copy with verify-proof-capture.mjs export. */
export const PROOF_CHAPTER = 'proof-chapter'
export const PROOF_STILL = 'proof-still'
export const MAX_CAPTION = 120

/** @param {string} caption */
function checkCaption(caption) {
  if (caption.length > MAX_CAPTION)
    throw new Error(`A proof caption is at most ${MAX_CAPTION} characters: "${caption}"`)
}

// A ring and caption drawn in the page itself, so the recording shows them.
const outline = async (page, target, caption, tone) => {
  await target.scrollIntoViewIfNeeded()
  const box = await target.boundingBox()
  if (!box) throw new Error(`Nothing to outline for "${caption}"`)
  await page.evaluate(
    ({ box, caption, tone }) => {
      // The recorder saves a frame only when the page repaints, so a static
      // ring can be missing from the video: the pulse keeps frames coming.
      if (!globalThis.document.getElementById('__proof_style')) {
        const style = globalThis.document.createElement('style')
        style.id = '__proof_style'
        style.textContent =
          '@keyframes __proofPulse{0%,100%{box-shadow:0 0 0 3px var(--ring-glow)}50%{box-shadow:0 0 0 7px var(--ring-glow)}}'
        globalThis.document.head.appendChild(style)
      }
      const color = tone === 'tap' ? '#4b9df8' : '#f4c267'
      const ring = globalThis.document.createElement('div')
      ring.id = '__proof_ring'
      Object.assign(ring.style, {
        animation: '__proofPulse 1.2s ease-in-out infinite',
        border: `3px solid ${color}`,
        borderRadius: '10px',
        height: `${box.height + 12}px`,
        left: `${box.x - 6}px`,
        pointerEvents: 'none',
        position: 'fixed',
        top: `${box.y - 6}px`,
        width: `${box.width + 12}px`,
        zIndex: '2147483647',
      })
      ring.style.setProperty('--ring-glow', `${color}55`)
      if (caption) {
        const label = globalThis.document.createElement('div')
        label.textContent = caption
        // Sized to the caption, not the ring, and kept on the screen side
        // of a ring near the right edge.
        Object.assign(label.style, {
          [box.y < 48 ? 'top' : 'bottom']: 'calc(100% + 6px)',
          [box.x + box.width / 2 > globalThis.window.innerWidth / 2 ? 'right' : 'left']: '0',
          background: color,
          borderRadius: '6px',
          color: '#0c0e14',
          font: '600 13px -apple-system, system-ui, sans-serif',
          maxWidth: `${Math.min(320, globalThis.window.innerWidth - 24)}px`,
          padding: '4px 8px',
          position: 'absolute',
          width: 'max-content',
        })
        ring.appendChild(label)
      }
      globalThis.document.body.appendChild(ring)
    },
    { box, caption, tone },
  )
}

const clearOutline = (page) =>
  page.evaluate(() => globalThis.document.getElementById('__proof_ring')?.remove())

/**
 * @param {{test: {info: Function}, enabled?: boolean, pacing?: {leadInMs?: number, holdStateMs?: number, holdTapMs?: number, typeDelayMs?: number}}} options
 */
export function createProof({ test, enabled = process.env.PROOF === '1', pacing = {} }) {
  const { leadInMs = 500, holdStateMs = 2500, holdTapMs = 800, typeDelayMs = 120 } = pacing
  const startedAt = new WeakMap()
  const annotate = (type, caption, file = undefined) => {
    const info = test.info()
    if (!startedAt.has(info)) startedAt.set(info, Date.now())
    info.annotations.push({
      type,
      description: JSON.stringify({ atMs: Date.now() - startedAt.get(info), caption, file }),
    })
  }
  return {
    begin() {
      if (enabled) startedAt.set(test.info(), Date.now())
    },
    async chapter(page, caption) {
      if (!enabled) return
      checkCaption(caption)
      annotate(PROOF_CHAPTER, caption)
      await page.waitForTimeout(leadInMs)
    },
    async show(page, target, caption) {
      if (!enabled) return
      checkCaption(caption)
      try {
        await outline(page, target, caption, 'state')
        const info = test.info()
        const file = info.outputPath(`still-${info.annotations.length}.png`)
        await page.screenshot({ path: file })
        annotate(PROOF_STILL, caption, file)
        await page.waitForTimeout(holdStateMs)
      } finally {
        await clearOutline(page)
      }
    },
    async tap(page, target, caption = '') {
      if (enabled) {
        checkCaption(caption)
        try {
          await outline(page, target, caption, 'tap')
          await page.waitForTimeout(holdTapMs)
        } finally {
          await clearOutline(page)
        }
      }
      await target.click()
    },
    async type(target, text) {
      if (enabled) await target.pressSequentially(text, { delay: typeDelayMs })
      else await target.fill(text)
    },
  }
}
