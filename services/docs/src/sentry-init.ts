import * as Sentry from '@sentry/react'
import { sentryInitOptions } from './vendor/web-helpers/sentry-init'

// The docs site's Sentry wiring. The scrubber, the noise filters and the shared
// options (PII posture, sample rates) live in lib/web-helpers/src/sentry-init.ts,
// which this site builds from a generated copy (src/vendor/web-helpers/); this
// file holds only what is the docs site's own: its DSN, its dev no-op and its
// `app` tag. scripts/headers.test.mjs reads the DSN host from this file.

const dsn =
  'https://f2047aedfb788b237eaa08d0a692fc3d@o4511396716871680.ingest.de.sentry.io/4511396756062288'

export function initSentry(opts: { env?: string; release?: string } = {}): void {
  // A dev-server HMR crash created a production issue (BOS-1170). Keep local
  // browser errors on the developer's machine instead of sending them to Sentry.
  if (process.env.NODE_ENV === 'development') {
    return
  }

  Sentry.init(
    sentryInitOptions({
      dsn,
      environment: opts.env,
      release: opts.release,
      // The docs site does no first-party JSON.parse, so `SyntaxError:
      // Unexpected token` is client-environment noise here (BOS-383).
      dropParseNoise: true,
    }),
  )
  Sentry.setTag('app', 'docs')
}
