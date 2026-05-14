import type { ReactNode } from 'react';
import { useEffect, useRef } from 'react';
import { useLocation } from '@docusaurus/router';
import useDocusaurusContext from '@docusaurus/useDocusaurusContext';
import posthog from 'posthog-js';

const defaultHost = 'https://k.bossanova.dev';

type PostHogCustomFields = {
  posthogHost?: unknown;
  posthogProjectToken?: unknown;
};

export default function Root({ children }: { children: ReactNode }) {
  const location = useLocation();
  const hasTrackedInitialPageview = useRef(false);
  const { siteConfig } = useDocusaurusContext();
  const customFields = siteConfig.customFields as PostHogCustomFields;
  const token =
    typeof customFields.posthogProjectToken === 'string'
      ? customFields.posthogProjectToken
      : undefined;
  const host =
    typeof customFields.posthogHost === 'string' ? customFields.posthogHost : defaultHost;

  useEffect(() => {
    if (!token) {
      return;
    }

    posthog.init(token, {
      api_host: host,
      autocapture: false,
      capture_pageleave: false,
      capture_pageview: false,
      disable_session_recording: true,
      loaded: (client) => {
        client.capture('$pageview');
      },
    });
  }, [host, token]);

  useEffect(() => {
    if (!token) {
      return;
    }

    if (!hasTrackedInitialPageview.current) {
      hasTrackedInitialPageview.current = true;
      return;
    }

    posthog.capture('$pageview');
  }, [location.pathname, location.search, token]);

  return <>{children}</>;
}
