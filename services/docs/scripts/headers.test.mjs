// Cloudflare Pages security headers for the docs site (BOS-1590). The CSP pins
// a sha256 for each inline script, and the browser blocks any inline script
// without one. Docusaurus decides which scripts to inline and minifies them, so
// the only trustworthy source of those bytes is a real build.
//
// This test reads build/ rather than building: `make test` (the CI docs job,
// .github/workflows/test-docs.yml) runs `pnpm build` and then this file, so CI
// always checks a fresh build without paying for a second one. A missing
// build/ is a failure, never a skip. The inline scripts do not depend on the
// build env (production, staging and bare builds emit the same two), so the
// bare CI build covers what both deploys ship. vitest.config.ts keeps this file
// out of `pnpm test:unit`, which runs before the build.
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { join, relative } from 'node:path'
import { before, test } from 'node:test'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('..', import.meta.url))
const repoRoot = join(root, '../..')
const buildDir = join(root, 'build')
const headers = readFileSync(join(root, 'static/_headers'), 'utf8')

const cspValue = (() => {
  const line = headers
    .split('\n')
    .map((l) => l.trim())
    .find((l) => l.toLowerCase().startsWith('content-security-policy:'))
  return line ? line.slice(line.indexOf(':') + 1).trim() : ''
})()

function sources(name) {
  const found = cspValue
    .split(';')
    .map((d) => d.trim())
    .find((d) => d === name || d.startsWith(`${name} `))
  return found ? found.split(/\s+/).slice(1) : []
}

// CSP3 host-source matching, enough for the sources this file uses: scheme,
// host with an optional leading `*.` (which never matches the bare domain), and
// an optional path that is a prefix when it ends in `/` and exact otherwise.
function sourceMatches(source, url) {
  const match = /^(https|wss):\/\/(\*\.)?([^/]+)(\/.*)?$/.exec(source)
  if (!match) {
    return false
  }
  const [, scheme, wildcard, host, path] = match
  const target = new URL(url)
  if (target.protocol !== `${scheme}:`) {
    return false
  }
  if (wildcard ? !target.hostname.endsWith(`.${host}`) : target.hostname !== host) {
    return false
  }
  if (!path) {
    return true
  }
  return path.endsWith('/') ? target.pathname.startsWith(path) : target.pathname === path
}

function allows(name, url) {
  return sources(name).some((source) => sourceMatches(source, url))
}

function htmlFiles(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = join(dir, entry.name)
    if (entry.isDirectory()) {
      return htmlFiles(full)
    }
    return entry.name.endsWith('.html') ? [full] : []
  })
}

// Script elements whose content the browser executes. JSON data blocks
// (type="application/json", "application/ld+json") are not scripts to CSP.
const SCRIPT = /<script\b([^>]*)>([\s\S]*?)<\/script>/gi
const DATA_TYPE = /\stype\s*=\s*["']?application\/(?:ld\+)?json/i

const scan = { inline: new Map(), external: new Set(), handlers: [] }

before(() => {
  assert.ok(
    existsSync(join(buildDir, 'index.html')),
    `${buildDir} has no index.html: run \`pnpm build\` first (\`make test\` does)`,
  )
  for (const file of htmlFiles(buildDir)) {
    const html = readFileSync(file, 'utf8')
    for (const [, attrs, body] of html.matchAll(SCRIPT)) {
      const src = /\ssrc\s*=\s*["']?([^"'\s>]+)/i.exec(attrs)
      if (src) {
        scan.external.add(src[1])
      } else if (!DATA_TYPE.test(attrs)) {
        const hash = `'sha256-${createHash('sha256').update(body, 'utf8').digest('base64')}'`
        if (!scan.inline.has(hash)) {
          scan.inline.set(hash, relative(buildDir, file))
        }
      }
    }
    for (const [handler] of html.matchAll(/<[a-z][^>]*\son[a-z]+\s*=/gi)) {
      scan.handlers.push(`${relative(buildDir, file)}: ${handler}`)
    }
  }
})

test('static/_headers is one path block of lines Cloudflare Pages accepts', () => {
  assert.deepEqual(headers.match(/^\/\S*$/gm), ['/*'])
  // Cloudflare Pages rejects a _headers line over 2,000 characters.
  const long = headers.split('\n').filter((line) => line.length > 2000)
  assert.deepEqual(long, [], 'a _headers line is over the 2,000-character limit')
})

test('static/_headers sets the security headers for every path', () => {
  assert.match(headers, /X-Content-Type-Options:\s*nosniff/)
  assert.match(headers, /X-Frame-Options:\s*DENY/)
  assert.match(headers, /Referrer-Policy:\s*strict-origin-when-cross-origin/)
  assert.deepEqual(sources('default-src'), ["'self'"])
  assert.deepEqual(sources('frame-ancestors'), ["'none'"])
  assert.deepEqual(sources('object-src'), ["'none'"])
  assert.deepEqual(sources('base-uri'), ["'self'"])
  assert.ok(cspValue.split(';').some((d) => d.trim() === 'upgrade-insecure-requests'))
  assert.equal(sources('script-src').includes("'unsafe-inline'"), false)
  assert.equal(sources('script-src').includes("'unsafe-eval'"), false)
})

test('the build ships static/_headers unchanged', () => {
  const shipped = join(buildDir, '_headers')
  assert.ok(existsSync(shipped), 'build/ has no _headers')
  assert.equal(readFileSync(shipped, 'utf8'), headers, 'build/_headers differs: rebuild')
})

test('every inline script the site emits is allowed by a CSP hash', () => {
  assert.ok(
    scan.inline.size > 0,
    'the build emitted no inline script; is the scan still finding them?',
  )
  const allowed = new Set(sources('script-src'))
  const missing = [...scan.inline].filter(([hash]) => !allowed.has(hash))
  assert.deepEqual(
    missing.map(([hash, page]) => `${hash} (first seen in ${page})`),
    [],
    'add these to script-src in static/_headers',
  )
})

test('every CSP script hash matches a script the site still emits', () => {
  const stale = sources('script-src').filter((s) => s.startsWith("'sha256-") && !scan.inline.has(s))
  assert.deepEqual(stale, [], 'remove hashes of scripts the site no longer emits')
})

test('no inline event handler attributes, which a hash-based CSP blocks', () => {
  assert.deepEqual(scan.handlers, [])
})

test('external scripts are same-origin or from an allowed host', () => {
  const remote = [...scan.external].filter((src) => !/^\/(?!\/)/.test(src))
  for (const src of remote) {
    assert.ok(allows('script-src', src), `the build loads ${src}, which script-src does not allow`)
  }
})

function sourceText(path) {
  return readFileSync(join(root, path), 'utf8')
}

test('the CSP allows the third-party origins the site code uses', () => {
  const gtag = /GOOGLE_TAG_SCRIPT = '([^']+)'/.exec(sourceText('src/googleTag.ts'))?.[1]
  assert.ok(gtag, 'GOOGLE_TAG_SCRIPT not found in src/googleTag.ts')
  assert.ok(allows('script-src', `${gtag}?id=G-HEADERSTEST`), `GA4 loader ${gtag}`)

  const dsnHost = /'https:\/\/[^@']+@([^/']+)\//.exec(sourceText('src/sentry-init.ts'))?.[1]
  assert.ok(dsnHost, 'Sentry DSN not found in src/sentry-init.ts')
  assert.ok(allows('connect-src', `https://${dsnHost}/api/1/envelope/`), `Sentry ${dsnHost}`)

  // The PostHog host is a build-time env per deploy; every one the release
  // workflows set, plus the default in src/theme/Root.tsx, must be allowed.
  const hosts = new Set([/defaultHost = '([^']+)'/.exec(sourceText('src/theme/Root.tsx'))?.[1]])
  for (const env of ['production', 'staging']) {
    const workflow = readFileSync(
      join(repoRoot, `.github/workflows/perform-${env}-release.yml`),
      'utf8',
    )
    for (const [, host] of workflow.matchAll(/PUBLIC_POSTHOG_HOST:\s*(\S+)/g)) {
      hosts.add(host)
    }
  }
  assert.ok(hosts.size >= 2, `expected production and staging PostHog hosts, got ${[...hosts]}`)
  for (const host of hosts) {
    assert.ok(allows('connect-src', `${host}/e/`), `PostHog ${host} in connect-src`)
    assert.ok(allows('script-src', `${host}/static/surveys.js`), `PostHog ${host} in script-src`)
  }
})

// The requests gtag.js makes for this property, as a browser recorded them on
// the live sites: page views (the EU region1 host, and analytics.google.com plus
// www.google.com while Google signals is on), the signals beacon and the
// ga-audiences pixel.
test('the CSP allows the GA4 collection endpoints but not arbitrary GTM containers', () => {
  for (const url of [
    'https://region1.google-analytics.com/g/collect',
    'https://region1.analytics.google.com/g/collect',
    'https://analytics.google.com/g/collect',
    'https://www.google.com/g/collect',
    'https://stats.g.doubleclick.net/g/collect',
  ]) {
    assert.ok(allows('connect-src', url), `connect-src blocks ${url}`)
  }
  assert.ok(allows('img-src', 'https://www.google.com/ads/ga-audiences'), 'ga-audiences pixel')
  assert.ok(
    allows('script-src', 'https://www.googletagmanager.com/gtag/destination?id=AW-1'),
    'linked destinations',
  )
  assert.equal(allows('script-src', 'https://www.googletagmanager.com/gtm.js?id=GTM-ANY'), false)
})

test('the CSP allows what the bundled libraries load at runtime', () => {
  // asciinema-player compiles WebAssembly; Redoc starts its search worker from
  // a blob: URL and shows the Redocly logo from its CDN; the local search
  // plugin runs a same-origin worker.
  assert.ok(sources('script-src').includes("'wasm-unsafe-eval'"), 'asciinema-player')
  assert.ok(sources('worker-src').includes('blob:'), 'Redoc search worker')
  assert.ok(sources('worker-src').includes("'self'"), 'local search worker')
  assert.ok(allows('img-src', 'https://cdn.redoc.ly/redoc/logo-mini.svg'), 'Redocly logo')
})
