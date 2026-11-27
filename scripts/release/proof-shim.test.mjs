import assert from 'node:assert/strict'
import { createCipheriv, randomBytes } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import path from 'node:path'
import { after, before, test } from 'node:test'
import {
  contentSecurityPolicy,
  contentTypeFor,
} from '../../infra/modules/cf-user-uploads/worker.mjs'
import { proofPlayerScript } from '../../skills-toolbox/verify/verify-proof-player.mjs'

const checkout = path.resolve(import.meta.dirname, '../..')
const { chromium } = createRequire(path.join(checkout, 'services/web/package.json'))(
  '@playwright/test',
)
const proofseal = path.join(checkout, 'services/bossd/internal/proofseal')
// guidance-audit: allow
const LOADER = readFileSync(path.join(proofseal, 'loader/index.html'))
// guidance-audit: allow
const SHIM = readFileSync(path.join(proofseal, 'loader/proof-shim.js'))
const VECTORS = JSON.parse(
  readFileSync(path.join(proofseal, 'testdata/vectors.json'), 'utf8'),
).vectors.map((v) => ({ ...v, ciphertext: Buffer.from(v.ciphertext, 'hex') }))
const GOLDEN_KEY = VECTORS[0].fragmentKey

const HOST = 'https://acme.bossanova-user-uploads.com'
const PAGE_DIR = `/v1.1800000000.${'s'.repeat(43)}/${'Q2xpcFRva2VuMTIzNDU2Nzg'}/`
const FRAME_ANCESTORS = 'https://app.bossanova.dev'
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=',
  'base64',
)
const MISSING_KEY = 'This link is missing its key'

let browser
before(async () => {
  browser = await chromium.launch({ headless: true })
})
after(async () => {
  await browser?.close()
})

function seal(key, name, plain) {
  const nonce = randomBytes(12)
  const cipher = createCipheriv('aes-256-gcm', key, nonce)
  cipher.setAAD(Buffer.from(name))
  return Buffer.concat([
    Buffer.from([1]),
    nonce,
    cipher.update(plain),
    cipher.final(),
    cipher.getAuthTag(),
  ])
}

async function openPage(objects, fragment) {
  const stored = { 'index.html': LOADER, 'proof-shim.js': SHIM, ...objects }
  const context = await browser.newContext()
  const requested = []
  await context.route(`${HOST}/**`, (route) => {
    const url = new URL(route.request().url())
    requested.push(url.href)
    const name = decodeURIComponent(url.pathname.slice(PAGE_DIR.length)) || 'index.html'
    const body = url.pathname.startsWith(PAGE_DIR) ? stored[name] : undefined
    const headers = {
      'Content-Security-Policy': contentSecurityPolicy(FRAME_ANCESTORS),
      'X-Content-Type-Options': 'nosniff',
    }
    if (body === undefined || !contentTypeFor(name))
      return route.fulfill({ status: 404, headers, body: '' })
    return route.fulfill({
      status: 200,
      headers: { ...headers, 'Content-Type': contentTypeFor(name) },
      body,
    })
  })
  const page = await context.newPage()
  await page.addInitScript(() => {
    window.proofViolations = []
    addEventListener('securitypolicyviolation', (e) =>
      window.proofViolations.push(`${e.violatedDirective} ${e.blockedURI}`),
    )
    window.proofBlobs = {}
    const createObjectURL = URL.createObjectURL
    URL.createObjectURL = (blob) => {
      const url = createObjectURL(blob)
      window.proofBlobs[url] = blob
      return url
    }
  })
  await page.goto(`${HOST}${PAGE_DIR}${fragment}`)
  return { page, requested, close: () => context.close() }
}

const goldenObjects = () => Object.fromEntries(VECTORS.map((v) => [v.name, v.ciphertext]))

const blobOf = (page, selector, attr) =>
  page.$eval(
    selector,
    async (el, name) => {
      const blob = window.proofBlobs[el.getAttribute(name)]
      const bytes = [...new Uint8Array(await blob.arrayBuffer())]
      return { type: blob.type, hex: bytes.map((b) => b.toString(16).padStart(2, '0')).join('') }
    },
    attr,
  )

test('@sessions.AC278801 the reader opens the golden page and swaps its screenshot for a blob:', async (t) => {
  const { page, requested, close } = await openPage(goldenObjects(), `#k=${GOLDEN_KEY}`)
  t.after(close)
  await page.waitForSelector('img')
  assert.equal(await page.title(), 'Proof')
  assert.match(await page.getAttribute('img', 'src'), /^blob:https:\/\/acme\./)
  assert.deepEqual(await blobOf(page, 'img', 'src'), {
    type: 'image/png',
    hex: VECTORS.find((v) => v.name === 'screenshots/a.png.enc').plaintext,
  })
  assert.equal(await page.locator('[data-proof-state]').count(), 0)
  assert.ok(requested.some((u) => u.endsWith('/screenshots/a.png.enc')))
  assert.ok(
    requested.every((u) => !u.includes(GOLDEN_KEY)),
    'the key reached a request',
  )
  assert.deepEqual(await page.evaluate(() => window.proofViolations), [])
})

test('@sessions.AC278801 a full page renders under the Worker CSP with its media, and only player.js runs', async (t) => {
  const key = randomBytes(32)
  const html = `<!doctype html><html lang="en" data-page="1"><head>
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src 'self'">
<title>Review · acme/web #12</title><style>main{color:rgb(1, 2, 3)}</style>
<script>window.inlineRan = true</script></head>
<body data-proof="acme/web#12"><main>
<a class="shot" href="screenshots/a.png"><img src="screenshots/a.png" alt="shot"></a>
<video id="proof-video" src="media/clip.webm" poster="screenshots/a.png"></video>
<button type="button" data-chapter="" data-clip="1" data-src="media/clip.webm" data-poster="screenshots/a.png" data-t="0">Start</button>
<a href="#files">Files</a><a href="https://github.com/acme/web/pull/12">PR</a>
</main><img src="screenshots/missing.png" alt="gone"><script src="player.js" defer></script></body></html>`
  const objects = {
    'page.html.enc': seal(key, 'page.html.enc', html),
    'screenshots/a.png.enc': seal(key, 'screenshots/a.png.enc', PNG),
    'media/clip.webm.enc': seal(key, 'media/clip.webm.enc', Buffer.from('webm')),
    'player.js': proofPlayerScript(),
  }
  const { page, close } = await openPage(objects, `#k=${key.toString('base64url')}`)
  t.after(close)
  await page.waitForFunction(() => document.documentElement.classList.contains('proof-js'))
  await page.waitForFunction(() => document.querySelector('main img').naturalWidth === 1)
  assert.equal(await page.title(), 'Review · acme/web #12')
  assert.equal(await page.getAttribute('html', 'data-page'), '1')
  assert.equal(await page.getAttribute('body', 'data-proof'), 'acme/web#12')
  assert.equal(await page.$eval('main', (el) => getComputedStyle(el).color), 'rgb(1, 2, 3)')
  const shot = await page.getAttribute('main img', 'src')
  for (const [selector, attr] of [
    ['a.shot', 'href'],
    ['video', 'poster'],
    ['button[data-chapter]', 'data-poster'],
  ])
    assert.equal(await page.getAttribute(selector, attr), shot, `${selector} ${attr}`)
  const clip = await page.getAttribute('video', 'src')
  assert.match(clip, /^blob:/)
  assert.equal(await page.getAttribute('button[data-chapter]', 'data-src'), clip)
  assert.deepEqual(await blobOf(page, 'video', 'src'), {
    type: 'video/webm',
    hex: Buffer.from('webm').toString('hex'),
  })
  assert.equal(await page.getAttribute('a[href^="#"]', 'href'), '#files')
  assert.equal(
    await page.getAttribute('a[href^="https:"]', 'href'),
    'https://github.com/acme/web/pull/12',
  )
  assert.equal(await page.getAttribute('img[alt="gone"]', 'src'), 'screenshots/missing.png')
  assert.equal(await page.evaluate(() => window.inlineRan), undefined)
  assert.equal(await page.locator('meta[http-equiv]').count(), 0)
  assert.deepEqual(
    await page.$$eval('script', (scripts) =>
      scripts.map((s) => new URL(s.src).pathname.split('/').pop()),
    ),
    ['proof-shim.js', 'player.js'],
  )
  assert.deepEqual(await page.evaluate(() => window.proofViolations), [])
})

for (const [name, fragment] of [
  ['an absent key', ''],
  ['a malformed key', '#k=not-a-key'],
  ['a key of the wrong length', `#k=${randomBytes(16).toString('base64url')}`],
  ['a wrong key', `#k=${randomBytes(32).toString('base64url')}`],
]) {
  test(`@sessions.AC278801 ${name} shows only the missing-key state`, async (t) => {
    const { page, close } = await openPage(goldenObjects(), fragment)
    t.after(close)
    await page.waitForSelector('[data-proof-state="missing-key"]:not([hidden])')
    assert.equal(await page.locator('[data-proof-state]:not([hidden])').count(), 1)
    assert.match(await page.textContent('body'), new RegExp(MISSING_KEY))
    assert.equal(await page.locator('img').count(), 0)
    assert.equal(await page.title(), 'Proof page')
  })
}

test('@sessions.AC278801 a key that arrives in the fragment later opens the page in place', async (t) => {
  const { page, close } = await openPage(goldenObjects(), '')
  t.after(close)
  await page.waitForSelector('[data-proof-state="missing-key"]:not([hidden])')
  await page.evaluate((k) => {
    location.hash = `k=${k}`
  }, GOLDEN_KEY)
  await page.waitForSelector('img')
  assert.equal(await page.title(), 'Proof')
})

test('in-page section links keep the key in the address, so a reload opens the page again', async (t) => {
  const key = randomBytes(32)
  const k = key.toString('base64url')
  const html = `<!doctype html><html><head><title>Sections</title></head><body><main>
<a id="to-files" href="#files">Files</a><a id="to-notes" href="#notes">Notes</a>
<section id="files" style="margin-top:3000px">files</section><section id="notes">notes</section>
</main><script src="player.js" defer></script></body></html>`
  const objects = {
    'page.html.enc': seal(key, 'page.html.enc', html),
    'player.js': proofPlayerScript(),
  }
  const { page, close } = await openPage(objects, `#k=${k}`)
  t.after(close)
  await page.waitForFunction(() => document.documentElement.classList.contains('proof-js'))
  await page.click('#to-files')
  assert.equal(new URLSearchParams(new URL(page.url()).hash.slice(1)).get('k'), k, 'player click')
  await page.evaluate(() => {
    location.hash = 'notes'
  })
  await page.waitForFunction((key) => location.hash.includes(`k=${key}`), k)
  await page.reload()
  await page.waitForSelector('#files')
  assert.equal(await page.title(), 'Sections')
  assert.equal(await page.locator('[data-proof-state]').count(), 0)
})

test('@sessions.AC278801 a page that cannot be fetched says so instead of blaming the key', async (t) => {
  const { 'page.html.enc': _page, ...rest } = goldenObjects()
  const { page, close } = await openPage(rest, `#k=${GOLDEN_KEY}`)
  t.after(close)
  await page.waitForSelector('[data-proof-state="unavailable"]:not([hidden])')
  assert.doesNotMatch(await page.innerText('body'), new RegExp(MISSING_KEY))
})
