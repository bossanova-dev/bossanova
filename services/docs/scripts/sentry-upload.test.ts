import { afterEach, describe, expect, it, vi } from 'vitest'
import { sentryUploadOptions, sentryUploadPlugin } from './sentry-upload'

const webpack = vi.hoisted(() => ({ sentryWebpackPlugin: vi.fn(() => ({ name: 'sentry' })) }))
vi.mock('@sentry/webpack-plugin', () => webpack)

afterEach(() => {
  vi.restoreAllMocks()
  webpack.sentryWebpackPlugin.mockClear()
})

describe('docs Sentry source maps', () => {
  it.each([undefined, '', '  '])('skips missing token %j', (token) => {
    expect(
      sentryUploadOptions({ SENTRY_AUTH_TOKEN: token, SENTRY_PROJECT: 'docs' }, 'docs-sha'),
    ).toMatchObject({ enabled: false, reason: 'SENTRY_AUTH_TOKEN is unset' })
  })

  it.each([undefined, '', '  '])('skips missing project %j', (project) => {
    expect(
      sentryUploadOptions({ SENTRY_AUTH_TOKEN: 'token', SENTRY_PROJECT: project }, 'docs-sha'),
    ).toMatchObject({ enabled: false, reason: 'SENTRY_PROJECT is unset' })
  })

  it('trims upload credentials and keeps the runtime release authoritative', () => {
    const result = sentryUploadOptions(
      { SENTRY_AUTH_TOKEN: ' token ', SENTRY_PROJECT: ' docs ', SENTRY_ORG: ' org ' },
      'docs-sha',
    )
    expect(result).toMatchObject({
      enabled: true,
      options: {
        authToken: 'token',
        project: 'docs',
        org: 'org',
        telemetry: false,
        release: { name: 'docs-sha', inject: false },
        sourcemaps: { filesToDeleteAfterUpload: 'build/**/*.map' },
      },
    })
  })

  it('allows an omitted organization and an unset release', () => {
    expect(
      sentryUploadOptions({ SENTRY_AUTH_TOKEN: 'token', SENTRY_PROJECT: 'docs' }, undefined),
    ).toMatchObject({
      enabled: true,
      options: { org: undefined, release: { name: undefined, inject: false } },
    })
  })

  it('warns without exposing upload errors or secrets', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const result = sentryUploadOptions(
      { SENTRY_AUTH_TOKEN: 'secret', SENTRY_PROJECT: 'docs' },
      'docs-sha',
    )
    if (!result.enabled) throw new Error('upload disabled')
    expect(() =>
      result.options.errorHandler?.(new Error('secret internal server detail')),
    ).not.toThrow()
    expect(warn).toHaveBeenCalledExactlyOnceWith(
      '::warning::Docs Sentry source-map upload failed; continuing build.',
    )
  })

  it('instantiates only for the client bundle and enables hidden maps there', () => {
    const plugin = sentryUploadPlugin(
      { SENTRY_AUTH_TOKEN: 'token', SENTRY_PROJECT: 'docs' },
      'docs-sha',
    )()
    expect(plugin.configureWebpack({}, true)).toEqual({})
    expect(webpack.sentryWebpackPlugin).not.toHaveBeenCalled()
    expect(plugin.configureWebpack({}, false)).toEqual({
      devtool: 'hidden-source-map',
      plugins: [{ name: 'sentry' }],
    })
    expect(webpack.sentryWebpackPlugin).toHaveBeenCalledTimes(1)
  })

  it('logs a skipped upload once and leaves both webpack bundles unchanged', () => {
    const log = vi.spyOn(console, 'info').mockImplementation(() => {})
    const plugin = sentryUploadPlugin({}, undefined)()
    expect(plugin.configureWebpack({}, true)).toEqual({})
    expect(plugin.configureWebpack({}, false)).toEqual({})
    expect(webpack.sentryWebpackPlugin).not.toHaveBeenCalled()
    expect(log).toHaveBeenCalledExactlyOnceWith(
      'Docs Sentry source-map upload skipped: SENTRY_AUTH_TOKEN is unset',
    )
  })
})
