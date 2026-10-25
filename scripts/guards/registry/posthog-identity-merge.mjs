export default {
  id: 'posthog-identity-merge',
  rule: 'Neither PostHog init sets a persistence option that breaks the anonymous-to-identified merge, and the workflows agree on one PostHog token and host.',
  command: 'node --test scripts/guards/check-posthog-identity-merge.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'services/marketing/src/components/Analytics.astro',
    'services/web/src/shared/lib/analytics/PostHogProvider.tsx',
    '.github/workflows/**',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-posthog-identity-merge.test.mjs',
    test: 'a quoted option key is detected',
  },
}
