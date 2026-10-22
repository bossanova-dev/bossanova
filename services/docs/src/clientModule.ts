import siteConfig from '@generated/docusaurus.config'
import { initSentry } from './sentry-init'

const customFields = siteConfig.customFields as {
  bossEnv?: string
  sentryRelease?: string
}

initSentry({ env: customFields.bossEnv, release: customFields.sentryRelease })
