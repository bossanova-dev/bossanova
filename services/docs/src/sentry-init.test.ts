// biome-ignore-all lint/security/noSecrets: scrub tests intentionally contain fake token-shaped samples.

import { beforeEach, describe, expect, it, vi } from 'vitest';

const sentryMock = vi.hoisted(() => ({
  init: vi.fn(),
  setTag: vi.fn(),
}));

vi.mock('@sentry/react', () => sentryMock);

import { initSentry, scrub } from './sentry-init';

type BeforeSend = (event: Record<string, unknown>) => Record<string, unknown>;

function beforeSend(): BeforeSend {
  initSentry();
  const options = sentryMock.init.mock.calls.at(-1)?.[0] as { beforeSend?: BeforeSend } | undefined;
  if (!options?.beforeSend) {
    throw new Error('Sentry beforeSend was not registered');
  }
  return options.beforeSend;
}

beforeEach(() => {
  sentryMock.init.mockReset();
  sentryMock.setTag.mockReset();
});

describe('scrub', () => {
  it.each([
    {
      name: 'github ghp token',
      input: 'token ghp_AbCdEf0123456789AbCdEf0123456789AbCd is leaked',
      want: 'token [REDACTED] is leaked',
    },
    {
      name: 'github ghs token',
      input: 'Authorization: token ghs_AbCdEf0123456789AbCdEf0123456789AbCd',
      want: 'Authorization: token [REDACTED]',
    },
    {
      name: 'jwt',
      input:
        'bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dGhpc2lzbm90YXNpZ25hdHVyZWJ1dGl0c2xvbmdlbm91Z2g',
      want: 'bearer [REDACTED]',
    },
    {
      name: 'email',
      input: 'user person@example.invalid hit an error',
      want: 'user [REDACTED] hit an error',
    },
    {
      name: 'github fine-grained pat',
      input:
        'token github_pat_11EXAMPLE0abcdefghijklmnopqrstuvwxyz1234567890ABCDEFGHIJKLMNOPQRSTUVWXYZ leaked',
      want: 'token [REDACTED] leaked',
    },
    {
      name: 'authorization bearer',
      input: 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456',
      want: 'Authorization: Bearer [REDACTED]',
    },
    {
      name: 'bearer opaque',
      input: 'Bearer abcdefghijklmnopqrstuvwxyz123456 failed',
      want: 'Bearer [REDACTED] failed',
    },
    {
      name: 'authorization basic short padded',
      input: 'Authorization: Basic dXNlcjpwYXNz failed',
      want: 'Authorization: Basic [REDACTED] failed',
    },
    {
      name: 'authorization basic with equals padding',
      input: 'Authorization: Basic dXNlcjpwYXNzd29yZA== failed',
      want: 'Authorization: Basic [REDACTED] failed',
    },
    {
      name: 'authorization digest with spaced commas',
      input: 'Authorization: Digest username="u", realm="r", nonce="n", response="r1"',
      want: 'Authorization: Digest [REDACTED]',
    },
    {
      name: 'authorization mac with spaced commas',
      input:
        'Authorization: MAC id="h480djs93hd8", ts="1336363200", nonce="dj83hs9s", mac="bhCQXTVyfj5cmA9uKkPFx1zeOXM="',
      want: 'Authorization: MAC [REDACTED]',
    },
    {
      name: 'authorization aws4 with spaced commas',
      input:
        'Authorization: AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20211231/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc123',
      want: 'Authorization: AWS4-HMAC-SHA256 [REDACTED]',
    },
    {
      name: 'env secret equals',
      input: 'API_KEY=sk_live_abcdef123 failed',
      want: 'API_KEY=[REDACTED] failed',
    },
    {
      name: 'env secret colon',
      input: 'password: hunter2 was wrong',
      want: 'password: [REDACTED] was wrong',
    },
    {
      name: 'innocuous',
      input: 'ordinary error with no secrets',
      want: 'ordinary error with no secrets',
    },
    {
      name: 'empty',
      input: '',
      want: '',
    },
  ])('$name', ({ input, want }) => {
    expect(scrub(input)).toBe(want);
  });
});

describe('beforeSend', () => {
  it('scrubs stacktrace frame fields and clears frame vars', () => {
    const gitHubToken = 'ghp_AbCdEf0123456789AbCdEf0123456789AbCd';
    const bearerToken = 'abcdefghijklmnopqrstuvwxyz123456';
    const event = {
      exception: {
        values: [
          {
            stacktrace: {
              frames: [
                {
                  filename: `https://docs.example.invalid/main.js?token=${gitHubToken}`,
                  abs_path: `/Users/person/${gitHubToken}/main.tsx`,
                  function: 'render person@example.invalid',
                  module: `Authorization: Bearer ${bearerToken}`,
                  package: 'API_KEY=sk_live_abcdef123',
                  context_line: 'password: hunter2',
                  pre_context: [`token ${gitHubToken}`],
                  post_context: ['email person@example.invalid'],
                  vars: { token: gitHubToken },
                },
              ],
            },
          },
        ],
      },
      threads: [
        {
          stacktrace: {
            frames: [
              {
                filename: `Bearer ${bearerToken}`,
                vars: { token: gitHubToken },
              },
            ],
          },
        },
      ],
    };

    beforeSend()(event);

    const serialized = JSON.stringify(event);
    expect(serialized).not.toContain(gitHubToken);
    expect(serialized).not.toContain(bearerToken);
    expect(serialized).not.toContain('person@example.invalid');
    expect(serialized).not.toContain('hunter2');
    expect(serialized).toContain('[REDACTED]');

    const exceptionFrame = event.exception.values[0].stacktrace.frames[0];
    const threadFrame = event.threads[0].stacktrace.frames[0];
    expect(exceptionFrame.vars).toBeUndefined();
    expect(threadFrame.vars).toBeUndefined();
  });
});
