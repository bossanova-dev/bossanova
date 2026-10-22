// @vitest-environment node
import { afterEach, expect, it, vi } from 'vitest'
import type { Plugin } from '@docusaurus/types'
import { sentryRelease } from '../src/vendor/web-helpers/sentry-init'

const mocks = vi.hoisted(() => ({
  sentryWebpackPlugin: vi.fn(() => ({ name: 'sentry' })),
  initSentry: vi.fn(),
  config: { customFields: {} as Record<string, unknown> },
}))
vi.mock('@sentry/webpack-plugin', () => ({ sentryWebpackPlugin: mocks.sentryWebpackPlugin }))
vi.mock('../src/sentry-init', () => ({ initSentry: mocks.initSentry }))
vi.mock('@generated/docusaurus.config', () => ({ default: mocks.config }))

afterEach(() => {
  vi.unstubAllEnvs()
  vi.resetModules()
  vi.clearAllMocks()
})

it.each([' sha123 ', undefined])(
  'passes the same final docs release to webpack and the browser for SHA %j',
  async (sha) => {
    vi.stubEnv('PUBLIC_BUILD_SHA', sha)
    vi.stubEnv('PUBLIC_BOSS_ENV', 'staging')
    vi.stubEnv('SENTRY_AUTH_TOKEN', 'token')
    vi.stubEnv('SENTRY_PROJECT', 'docs')
    const { default: config } = await import('../docusaurus.config')
    const release = sentryRelease('docs', sha)
    expect(config.customFields).toMatchObject({ sentryRelease: release, bossEnv: 'staging' })
    const factory = config.plugins?.find((entry) => typeof entry === 'function') as () => Plugin
    const plugin = factory()
    plugin.configureWebpack?.({}, false, {} as never, undefined)
    expect(mocks.sentryWebpackPlugin).toHaveBeenCalledWith(
      expect.objectContaining({ release: { name: release, inject: false } }),
    )
    mocks.config.customFields = config.customFields ?? {}
    await import('../src/clientModule')
    expect(mocks.initSentry).toHaveBeenCalledExactlyOnceWith({ env: 'staging', release })
  },
)
