import { beforeEach, describe, expect, it, vi } from 'vitest'

const sentryMock = vi.hoisted(() => ({
  init: vi.fn(),
  setTag: vi.fn(),
}))

vi.mock('@sentry/react', () => sentryMock)

import { initSentry } from './sentry-init'

// The docs wrapper only: its DSN, its dev no-op, its `app` tag and the filters
// it opts into. The scrubber, the noise filters and the shared options are
// pinned once, in lib/web-helpers/test/sentry-init.test.mjs, against the source
// src/vendor/web-helpers/ is generated from byte for byte.

type BeforeSend = (event: Record<string, unknown>) => Record<string, unknown> | null
type InitOptions = Record<string, unknown> & { beforeSend?: BeforeSend }

const DOCS_SENTRY_PROJECT_RE = /@o4511396716871680\.ingest\.de\.sentry\.io\/4511396756062288$/

function initOptions(opts?: { env?: string; release?: string }): InitOptions {
  initSentry(opts)
  const options = sentryMock.init.mock.calls.at(-1)?.[0] as InitOptions | undefined
  if (!options) {
    throw new Error('Sentry.init was not called')
  }
  return options
}

function beforeSend(): BeforeSend {
  const options = initOptions()
  if (!options.beforeSend) {
    throw new Error('Sentry beforeSend was not registered')
  }
  return options.beforeSend
}

function eventWithPrimaryException(
  type: string,
  value: string,
  frames: Array<Record<string, unknown>>,
): Record<string, unknown> {
  return {
    exception: {
      values: [{ type, value, stacktrace: { frames } }],
    },
  }
}

beforeEach(() => {
  sentryMock.init.mockReset()
  sentryMock.setTag.mockReset()
})

describe('initSentry', () => {
  it('does not initialize Sentry from the development server', () => {
    const nodeEnv = process.env.NODE_ENV
    process.env.NODE_ENV = 'development'

    initSentry()

    process.env.NODE_ENV = nodeEnv
    expect(sentryMock.init).not.toHaveBeenCalled()
    expect(sentryMock.setTag).not.toHaveBeenCalled()
  })

  it('initializes Sentry on a production build, tagged as the docs site', () => {
    initSentry()
    expect(sentryMock.init).toHaveBeenCalledTimes(1)
    expect(sentryMock.setTag).toHaveBeenCalledWith('app', 'docs')
  })

  it('reports to the docs Sentry project', () => {
    expect(initOptions().dsn).toMatch(DOCS_SENTRY_PROJECT_RE)
  })

  it('passes the site config env and build SHA through, defaulting to production', () => {
    expect(initOptions()).toMatchObject({ environment: 'production', release: undefined })
    expect(initOptions({ env: 'staging', release: 'abc123' })).toMatchObject({
      environment: 'staging',
      release: 'abc123',
    })
  })

  // SDK v11 replaced sendDefaultPii with dataCollection, whose every field
  // defaults to collecting. Omitting it would silently start inferring user IPs.
  it('opts out of every default PII collection category', () => {
    const options = initOptions()
    expect(options.sendDefaultPii).toBeUndefined()
    expect(options.dataCollection).toEqual({
      userInfo: false,
      cookies: false,
      httpHeaders: false,
      httpBodies: [],
      urlQueryParams: false,
    })
  })
})

describe('beforeSend', () => {
  it('drops third-party beacon noise', () => {
    const event = {
      exception: {
        values: [
          { stacktrace: { frames: [{ filename: 'https://example.invalid/beacon.min.js' }] } },
        ],
      },
    }
    expect(beforeSend()(event)).toBeNull()
  })

  // The docs site opts into the parse-noise filter (BOS-383); the web app does not.
  it('drops a SyntaxError "Unexpected token", which the docs site treats as noise', () => {
    const event = eventWithPrimaryException('SyntaxError', "Unexpected token '{'", [
      { filename: 'https://docs.bossanova.dev/assets/js/common.js' },
    ])
    expect(beforeSend()(event)).toBeNull()
  })

  it('keeps and scrubs a first-party TypeError', () => {
    const gitHubToken = 'ghp_AbCdEf0123456789AbCdEf0123456789AbCd'
    const event = eventWithPrimaryException('TypeError', 'x is not a function', [
      { filename: `https://docs.bossanova.dev/assets/main.js?token=${gitHubToken}` },
    ])

    const result = beforeSend()(event)

    expect(result).toBe(event)
    expect(JSON.stringify(result)).not.toContain(gitHubToken)
    expect(JSON.stringify(result)).toContain('[REDACTED]')
  })
})
