import { describe, expect, it } from 'vitest'
import {
  GOOGLE_TAG_SCRIPT,
  GOOGLE_UI_EVENTS,
  type GoogleTagWindow,
  loadGoogleTag,
  sanitizePagePath,
  trackGoogleEvent,
  trackPageView,
} from './googleTag'

// The docs copy of the GA4 loader (docs/analytics/google-analytics.md). The web
// app's googleTag.test.ts covers the shared contract in full; this pins the
// parts Root.tsx relies on.

const MEASUREMENT_ID = 'G-TEST1234'

function fakeWindow(hostname = 'docs.bossanova.dev') {
  const appended: HTMLScriptElement[] = []
  const win: GoogleTagWindow = {
    location: { hostname },
    document: {
      createElement: ((tag: string) => document.createElement(tag)) as Document['createElement'],
      head: { appendChild: (node: HTMLScriptElement) => appended.push(node) } as never,
      querySelector: (() => appended[0] ?? null) as never,
    },
  }
  return { win, appended }
}

function commands(win: GoogleTagWindow): unknown[][] {
  return (win.dataLayer ?? []).map((entry) => Array.from(entry as ArrayLike<unknown>))
}

describe('docs googleTag', () => {
  it('loads once on docs.bossanova.dev and sends sanitized page views', () => {
    const { win, appended } = fakeWindow()

    expect(loadGoogleTag(MEASUREMENT_ID, win)).toBe(true)
    expect(loadGoogleTag(MEASUREMENT_ID, win)).toBe(true)
    expect(appended).toHaveLength(1)
    expect(appended[0].src).toBe(`${GOOGLE_TAG_SCRIPT}?id=${MEASUREMENT_ID}`)
    const before = commands(win).length

    trackPageView('/docs/getting-started?utm_source=hn&ref=x', win)
    trackPageView('/docs/getting-started', win)

    const pageLocation = 'https://docs.bossanova.dev/docs/getting-started?utm_source=hn'
    expect(commands(win).slice(before)).toEqual([
      ['set', { page_location: pageLocation }],
      ['event', 'page_view', { page_location: pageLocation, send_to: MEASUREMENT_ID }],
    ])
  })

  it.each([
    ['docs-staging.bossanova.dev', MEASUREMENT_ID],
    ['localhost', MEASUREMENT_ID],
    ['docs.bossanova.dev', undefined],
  ])('stays inert on %s with id %s', (host, id) => {
    const { win, appended } = fakeWindow(host)

    expect(loadGoogleTag(id, win)).toBe(false)
    trackPageView('/docs', win)
    expect(win.dataLayer).toBeUndefined()
    expect(appended).toHaveLength(0)
  })

  it('keeps the docs search query for GA site search', () => {
    expect(sanitizePagePath('/search?q=cron%20jobs&ref=nav')).toBe('/search?q=cron+jobs')
  })

  it('redacts identifier-like path segments', () => {
    expect(sanitizePagePath('/api/sessions/0b7c6a52-3f1e-4c0e-9f43-0d9f6d8c1a2b')).toBe(
      '/api/sessions/:redacted',
    )
  })
})

describe('docs googleTag funnel allowlist', () => {
  // Mirrors the web copy's GOOGLE_UI_EVENTS (BOS-1373); the three must agree.
  it('pins the same GA funnel allowlist as the web copy', () => {
    expect([...GOOGLE_UI_EVENTS].sort()).toEqual([
      'app_cta_clicked',
      'auth_redirect_started',
      'begin_checkout',
      'cloud_checkout_returned',
      'signup_route_hit',
      'view_item',
    ])
  })

  it('renames the plan offer and checkout to GA4 ecommerce names, with the item', () => {
    const { win } = fakeWindow()
    loadGoogleTag(MEASUREMENT_ID, win)
    const before = commands(win).length

    trackGoogleEvent('cloud_subscription_gate_viewed', { entry_point: 'gate' }, win)
    trackGoogleEvent('cloud_checkout_started', { entry_point: 'gate' }, win)

    const items = [{ item_id: 'bossanova_cloud', item_name: 'Bossanova Cloud' }]
    expect(commands(win).slice(before)).toEqual([
      ['event', 'view_item', { entry_point: 'gate', items, send_to: MEASUREMENT_ID }],
      ['event', 'begin_checkout', { entry_point: 'gate', items, send_to: MEASUREMENT_ID }],
    ])
  })
})
