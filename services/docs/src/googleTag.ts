// Google Analytics 4 browser tag (gtag.js) for the docs site.
//
// One GA4 property and one web stream cover bossanova.dev, app.bossanova.dev
// and docs.bossanova.dev; the contract (measurement id, consent, linker,
// sanitized page views, event allowlist) is documented in
// docs/analytics/google-analytics.md. This module mirrors
// services/web/src/analytics/googleTag.ts and
// services/marketing/src/lib/googleTag.ts; keep the three in step. It is a
// copy, not a shared package, because docs is its own pnpm workspace.
//
// gtag.js is loaded directly — no Tag Manager container. The tag loads only
// with a valid measurement id on a production bossanova.dev host, page views
// are sent by src/theme/Root.tsx on route change rather than by gtag's history
// listener, and every helper is a no-op until loadGoogleTag succeeds, so
// callers never branch on configuration.

export const GOOGLE_TAG_SCRIPT = 'https://www.googletagmanager.com/gtag/js'
export const GOOGLE_TAG_COOKIE_DOMAIN = 'bossanova.dev'
export const GOOGLE_TAG_LINKER_DOMAINS = [
  'bossanova.dev',
  'app.bossanova.dev',
  'docs.bossanova.dev',
] as const

const MEASUREMENT_ID_PATTERN = /^G-[A-Z0-9]+$/
// Campaign and ad-click parameters, plus `q`: the docs search page's query,
// which GA's enhanced-measurement site search reads from page_location.
const KEPT_QUERY_PARAM_PATTERN = /^(utm_[a-z]+|gclid|gbraid|wbraid|q)$/
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const EMAIL_PATTERN = /@/
// Opaque identifiers (WorkOS org ids, repo keys, tokens): long and containing a
// digit. Human route words ("sessions", "settings") never match.
const OPAQUE_ID_PATTERN = /^(?=.*\d)[A-Za-z0-9_-]{16,}$/
const REDACTED_SEGMENT = ':redacted'

// Consent Mode v2: everything denied until the update below. There is no
// consent surface yet, so the update grants by default (madverts parity); a
// future banner only has to replace CONSENT_GRANTED with the visitor's choice.
const CONSENT_DEFAULT = {
  ad_personalization: 'denied',
  ad_storage: 'denied',
  ad_user_data: 'denied',
  analytics_storage: 'denied',
  wait_for_update: 500,
}
const CONSENT_GRANTED = {
  ad_personalization: 'granted',
  ad_storage: 'granted',
  ad_user_data: 'granted',
  analytics_storage: 'granted',
}
const TAG_CONFIG = {
  cookie_domain: GOOGLE_TAG_COOKIE_DOMAIN,
  linker: { domains: [...GOOGLE_TAG_LINKER_DOMAINS] },
  send_page_view: false,
  transport_type: 'beacon',
}

/**
 * Funnel events forwarded to GA; everything else stays PostHog-only. Signups
 * and purchases are not here: bosso sends those server-side (Measurement
 * Protocol) so they count even when the browser never reports them.
 */
export const GOOGLE_UI_EVENTS = new Set([
  'app_cta_clicked',
  'signup_route_hit',
  'cloud_subscription_gate_viewed',
  'cloud_checkout_started',
  'cloud_checkout_returned',
])
const GOOGLE_EVENT_PARAMS = [
  'app',
  'entry_point',
  'link_path',
  'location',
  'product_area',
  'step',
] as const

type Gtag = (...args: unknown[]) => void

export interface GoogleTagWindow {
  document: Pick<Document, 'createElement' | 'head' | 'querySelector'>
  location: { hostname: string }
  dataLayer?: unknown[]
  gtag?: Gtag
}

interface GoogleTagState {
  gtag: Gtag
  measurementId: string
  lastPagePath?: string
}

const loaded = new WeakMap<object, GoogleTagState>()

/** True for bossanova.dev and its subdomains, excluding staging hosts. */
export function isGoogleTagHost(hostname: string): boolean {
  const host = hostname.toLowerCase()
  if (!(host === GOOGLE_TAG_COOKIE_DOMAIN || host.endsWith(`.${GOOGLE_TAG_COOKIE_DOMAIN}`))) {
    return false
  }
  const [firstLabel] = host.split('.')
  return !(firstLabel === 'staging' || firstLabel.endsWith('-staging'))
}

/**
 * Installs gtag.js once per window. Returns false (and installs nothing) when
 * the measurement id is missing or malformed or the host is not production.
 */
export function loadGoogleTag(
  measurementId: string | undefined,
  win: GoogleTagWindow = globalThis as unknown as GoogleTagWindow,
): boolean {
  if (loaded.has(win)) {
    return true
  }
  const id = measurementId?.trim() ?? ''
  if (!(MEASUREMENT_ID_PATTERN.test(id) && isGoogleTagHost(win.location.hostname))) {
    return false
  }

  win.dataLayer = win.dataLayer ?? []
  const { dataLayer } = win
  // gtag.js reads the `arguments` object itself, not an array copy.
  const gtag: Gtag = function gtag() {
    dataLayer.push(arguments)
  }
  win.gtag = gtag

  gtag('consent', 'default', CONSENT_DEFAULT)
  gtag('consent', 'update', CONSENT_GRANTED)
  gtag('js', new Date())
  gtag('config', id, TAG_CONFIG)

  if (!win.document.querySelector('script[data-bossanova-google-tag]')) {
    const script = win.document.createElement('script')
    script.async = true
    script.src = `${GOOGLE_TAG_SCRIPT}?id=${encodeURIComponent(id)}`
    script.setAttribute('data-bossanova-google-tag', '')
    win.document.head.appendChild(script)
  }

  loaded.set(win, { gtag, measurementId: id })
  return true
}

/**
 * Reduces a path + query to what GA may see: identifier-like segments become
 * `:redacted`, and only campaign and ad-click parameters survive.
 */
export function sanitizePagePath(pathWithQuery: string): string {
  const queryStart = pathWithQuery.indexOf('?')
  const path = queryStart >= 0 ? pathWithQuery.slice(0, queryStart) : pathWithQuery
  const query = queryStart >= 0 ? pathWithQuery.slice(queryStart + 1) : ''

  const cleanPath = path
    .split('#')[0]
    .split('/')
    .map((segment) => {
      let decoded = segment
      try {
        decoded = decodeURIComponent(segment)
      } catch {
        return REDACTED_SEGMENT
      }
      return UUID_PATTERN.test(decoded) ||
        EMAIL_PATTERN.test(decoded) ||
        OPAQUE_ID_PATTERN.test(decoded)
        ? REDACTED_SEGMENT
        : segment
    })
    .join('/')

  const kept = new URLSearchParams()
  for (const [key, value] of new URLSearchParams(query.split('#')[0])) {
    if (KEPT_QUERY_PARAM_PATTERN.test(key)) {
      kept.append(key, value)
    }
  }
  const keptQuery = kept.toString()
  return keptQuery ? `${cleanPath || '/'}?${keptQuery}` : cleanPath || '/'
}

/**
 * Sends one page_view, skipping a repeat of the previous path. The repeat
 * check ignores the query, so a page that strips its own UTMs on mount (the
 * web app's /signup) is not counted twice.
 */
export function trackPageView(
  pathWithQuery: string,
  win: GoogleTagWindow = globalThis as unknown as GoogleTagWindow,
): void {
  const state = loaded.get(win)
  if (!state) {
    return
  }
  const pagePath = sanitizePagePath(pathWithQuery)
  const [pathOnly] = pagePath.split('?')
  if (state.lastPagePath === pathOnly) {
    return
  }
  state.lastPagePath = pathOnly
  // GA4 reports pages from page_location (page_path is a Universal Analytics
  // field it ignores), and every later event, enhanced measurement's included,
  // reads page_location from the live URL unless it is set. Setting the
  // sanitized URL keeps identifiers out of all of them.
  const pageLocation = `https://${win.location.hostname}${pagePath}`
  state.gtag('set', { page_location: pageLocation })
  state.gtag('event', 'page_view', {
    page_location: pageLocation,
    send_to: state.measurementId,
  })
}

/** Forwards an allowlisted UI event with only its allowlisted parameters. */
export function trackGoogleEvent(
  name: string,
  properties: Record<string, unknown> = {},
  win: GoogleTagWindow = globalThis as unknown as GoogleTagWindow,
): void {
  const state = loaded.get(win)
  if (!(state && GOOGLE_UI_EVENTS.has(name))) {
    return
  }
  const params: Record<string, string> = {}
  for (const key of GOOGLE_EVENT_PARAMS) {
    const value = properties[key]
    if (typeof value === 'string' && value !== '') {
      params[key] = value
    }
  }
  state.gtag('event', name, { ...params, send_to: state.measurementId })
}
