import type { ReactNode } from 'react'
import { useEffect, useRef } from 'react'
import { useLocation } from '@docusaurus/router'
import useDocusaurusContext from '@docusaurus/useDocusaurusContext'
import posthog from 'posthog-js'
import { loadGoogleTag, trackPageView } from '../googleTag'

const defaultHost = 'https://k.bossanova.dev'

type AnalyticsCustomFields = {
  ga4MeasurementId?: unknown
  posthogHost?: unknown
  posthogProjectToken?: unknown
}

export default function Root({ children }: { children: ReactNode }) {
  const location = useLocation()
  const hasTrackedInitialPageview = useRef(false)
  const { siteConfig } = useDocusaurusContext()
  const customFields = siteConfig.customFields as AnalyticsCustomFields
  const token =
    typeof customFields.posthogProjectToken === 'string'
      ? customFields.posthogProjectToken
      : undefined
  const host = typeof customFields.posthogHost === 'string' ? customFields.posthogHost : defaultHost
  const ga4MeasurementId =
    typeof customFields.ga4MeasurementId === 'string' ? customFields.ga4MeasurementId : undefined

  // GA4 page views are sent here on every route change (gtag's own history
  // listener is off so the path can be sanitized first). loadGoogleTag is a
  // no-op without a production id and host, and so is trackPageView after it.
  useEffect(() => {
    loadGoogleTag(ga4MeasurementId)
    trackPageView(`${location.pathname}${location.search}`)
  }, [ga4MeasurementId, location.pathname, location.search])

  useEffect(() => {
    if (!token) {
      return
    }

    posthog.init(token, {
      api_host: host,
      autocapture: false,
      capture_pageleave: false,
      capture_pageview: false,
      disable_session_recording: true,
      loaded: (client) => {
        client.capture('$pageview')
      },
    })
  }, [host, token])

  useEffect(() => {
    if (!token) {
      return
    }

    if (!hasTrackedInitialPageview.current) {
      hasTrackedInitialPageview.current = true
      return
    }

    posthog.capture('$pageview')
  }, [location.pathname, location.search, token])

  return <>{children}</>
}
