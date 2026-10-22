import type { Plugin } from '@docusaurus/types'
import { sentryWebpackPlugin } from '@sentry/webpack-plugin'

type UploadOptions = NonNullable<Parameters<typeof sentryWebpackPlugin>[0]>
type UploadEnvironment = Record<string, string | undefined>
type UploadDecision = { enabled: false; reason: string } | { enabled: true; options: UploadOptions }

export function sentryUploadOptions(
  env: UploadEnvironment,
  release: string | undefined,
): UploadDecision {
  const authToken = env.SENTRY_AUTH_TOKEN?.trim()
  const project = env.SENTRY_PROJECT?.trim()
  if (!authToken) return { enabled: false, reason: 'SENTRY_AUTH_TOKEN is unset' }
  if (!project) return { enabled: false, reason: 'SENTRY_PROJECT is unset' }
  return {
    enabled: true,
    options: {
      authToken,
      project,
      org: env.SENTRY_ORG?.trim() || undefined,
      telemetry: false,
      release: { name: release, inject: false },
      sourcemaps: { filesToDeleteAfterUpload: 'build/**/*.map' },
      errorHandler: () => {
        console.warn('::warning::Docs Sentry source-map upload failed; continuing build.')
      },
    },
  }
}

export function sentryUploadPlugin(env: UploadEnvironment, release: string | undefined) {
  const decision = sentryUploadOptions(env, release)
  return function docsSentryUpload() {
    if (!decision.enabled) {
      console.info(`Docs Sentry source-map upload skipped: ${decision.reason}`)
    }
    return {
      name: 'docs-sentry-upload',
      configureWebpack(_config: unknown, isServer: boolean) {
        if (isServer || !decision.enabled) return {}
        return {
          devtool: 'hidden-source-map',
          plugins: [sentryWebpackPlugin(decision.options)],
        }
      },
    } satisfies Plugin
  }
}
