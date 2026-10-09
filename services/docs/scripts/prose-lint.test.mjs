import { describe, expect, it } from 'vitest'

import { alertCounts, compareAlertCounts, valeVersion, versionAtLeast } from './prose-lint.mjs'

describe('Vale prose alert ratchet', () => {
  it('counts alerts by file and rule', () => {
    expect(
      alertCounts({
        'docs/guide.md': [
          { Check: 'BossanovaProse.EmDashCluster' },
          { Check: 'BossanovaProse.EmDashCluster' },
          { Check: 'BossanovaProse.FunctionWordBold' },
        ],
      }),
    ).toEqual({
      'docs/guide.md': {
        'BossanovaProse.EmDashCluster': 2,
        'BossanovaProse.FunctionWordBold': 1,
      },
    })
  })

  it('fails only alert counts above the per-file baseline', () => {
    const baseline = { 'docs/guide.md': { 'BossanovaProse.EmDashCluster': 2 } }
    expect(compareAlertCounts(baseline, baseline)).toEqual([])
    expect(
      compareAlertCounts({ 'docs/guide.md': { 'BossanovaProse.EmDashCluster': 3 } }, baseline),
    ).toEqual([
      {
        file: 'docs/guide.md',
        rule: 'BossanovaProse.EmDashCluster',
        actual: 3,
        baseline: 2,
      },
    ])
  })

  it('treats a new file or rule as a zero-alert baseline', () => {
    expect(
      compareAlertCounts({ 'docs/new.md': { 'BossanovaProse.BannedWords': 1 } }, {}),
    ).toHaveLength(1)
  })

  it('enforces the configured minimum version', () => {
    expect(versionAtLeast('3.18.0', '3.18.0')).toBe(true)
    expect(versionAtLeast('3.19.0', '3.18.0')).toBe(true)
    expect(versionAtLeast('4.0.0', '3.18.0')).toBe(true)
    expect(versionAtLeast('3.17.9', '3.18.0')).toBe(false)
  })
})

describe('Vale binary availability', () => {
  it('names make deps and VALE_BIN when the binary is missing', () => {
    expect(() => valeVersion('/does-not-exist/bos-1365-vale', '3.18.0')).toThrow(
      'Vale 3.18.0+ is not installed (looked for "/does-not-exist/bos-1365-vale"); run `make deps` (installs it via Homebrew) or set VALE_BIN',
    )
  })
})
