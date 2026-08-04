// Tests for the session-launch context baseline harness (BOS-671).
//
// The harness's ONE impure step — spawning a real `claude -p` per variant — is
// injected, so this suite never makes an API call and is safe under
// `make test-scripts`. Everything asserted here is pure: the result parser, the
// variant-manifest validator, the delta arithmetic, and argv construction.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  MANIFEST_PATH,
  buildClaudeArgs,
  checkMcpAttachment,
  checkVariantBinaries,
  computeRows,
  formatTable,
  loadManifest,
  parseArgv,
  parseStreamResult,
  parseUsage,
  renderOutput,
  runMeasurement,
  scopeNotice,
  validateManifest,
  variantCarriesSessionContext,
  warmRunNotice,
} from './measure-context-baseline.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))

// runMeasurement writes one MCP config per variant. Hand it a scratch dir the
// test removes, so the suite leaves nothing behind in the system temp dir.
function scratchDir(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'context-baseline-test-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  return dir
}

// The default binary preflight stats `./bin/mcp`, a gitignored build artifact
// absent in CI. Tests that are not about the guard inject this no-op; the guard
// itself is covered directly by the checkVariantBinaries tests below.
const skipBinaryCheck = () => {}

function resultJson(usage) {
  return JSON.stringify({ type: 'result', subtype: 'success', result: 'ok', usage })
}

// One `--output-format stream-json` stdout: the system/init event, which is the
// only place a run reports which MCP servers attached, then the terminal result
// event carrying the usage. Real streams interleave hook and assistant events
// between the two; the parser tests below cover that, the spawn stubs do not
// need it.
function streamJson({ usage, mcpServers = [], resultEvent } = {}) {
  const init = { type: 'system', subtype: 'init', mcp_servers: mcpServers }
  const result = resultEvent ?? { type: 'result', subtype: 'success', result: 'ok', usage }
  return `${JSON.stringify(init)}\n${JSON.stringify(result)}\n`
}

// The happy path the attachment gate exists to let through: every server the
// variant declared, reported connected.
function attachedFor(variant) {
  return Object.keys(variant.mcpServers ?? {}).map((name) => ({ name, status: 'connected' }))
}

// A minimal valid manifest the rejection cases mutate one field at a time.
function baseManifest() {
  return {
    prompt: 'Reply with exactly: ok',
    baseline: 'status-quo',
    variants: [
      { name: 'status-quo', description: 'today', mcpServers: { boss: { command: './bin/mcp' } } },
      { name: 'no-mcp', description: 'none', strictMcpConfig: true, mcpServers: {} },
    ],
  }
}

test('parseUsage reports cache_creation_input_tokens verbatim', () => {
  const usage = parseUsage(resultJson({ cache_creation_input_tokens: 41234 }))
  assert.equal(usage.cacheCreation, 41234)
})

// The launch prefix is what a session pays before its first tool call, and it is
// paid whether the provider WROTE it to cache this turn, READ it from a warm one,
// or billed it as plain input past the last cache breakpoint. Summing all three
// fields is what makes the measure cache-state independent. The measured evidence
// (the ~5-minute cache window, the run that reported 0, the isolated re-run that
// did not) lives in the report, which owns those constants:
// docs/solutions/performance/2026-08-03-session-launch-context-baseline.md
test('parseUsage measures the launch prefix as input + cache_creation + cache_read', () => {
  const usage = parseUsage(
    resultJson({
      input_tokens: 23513,
      cache_creation_input_tokens: 41234,
      cache_read_input_tokens: 7,
    }),
  )
  assert.equal(usage.tokens, 64754)
  assert.equal(usage.cacheRead, 7)
  assert.equal(usage.input, 23513)
})

test('parseUsage counts a fully warm prefix rather than reporting it as free', () => {
  const usage = parseUsage(
    resultJson({ cache_creation_input_tokens: 0, cache_read_input_tokens: 110194 }),
  )
  assert.equal(usage.cacheCreation, 0)
  assert.equal(usage.tokens, 110194, 'a cache HIT still costs the full launch prefix')
})

test('parseUsage treats an absent cache_read_input_tokens or input_tokens as zero', () => {
  const usage = parseUsage(resultJson({ cache_creation_input_tokens: 12 }))
  assert.equal(usage.tokens, 12)
  assert.equal(usage.input, 0)
  assert.equal(usage.cacheRead, 0)
})

// `null` is ABSENT, not malformed: a real usage object emits an explicit null for
// a cache field that did not apply, which is a well-formed zero. The distinction
// is deliberate and easy to flip by accident, so pin it — including that a null
// cache field cannot mask a genuinely empty prefix (the zero guard still fires).
test('parseUsage treats an explicit null usage term as absent, not malformed', () => {
  const usage = parseUsage(
    resultJson({ input_tokens: 2, cache_creation_input_tokens: 12, cache_read_input_tokens: null }),
  )
  assert.equal(usage.cacheRead, 0)
  assert.equal(usage.tokens, 14)
  assert.throws(
    () =>
      parseUsage(
        resultJson({
          input_tokens: 0,
          cache_creation_input_tokens: 0,
          cache_read_input_tokens: null,
        }),
      ),
    /no session launches for free/,
    'a null cache field must not let an all-zero prefix through',
  )
})

// PRESENCE and VALUE are separate contracts for cache_creation_input_tokens:
// the KEY must exist (its disappearance means the result shape changed), but a
// key that is present-and-null is a well-formed zero. Conflating the two would
// hard-fail a legitimate FULLY WARM run — which is exactly the shape of the
// published table, where cache_creation is 0 on every row.
test('parseUsage accepts an explicitly null cache_creation on a warm run', () => {
  const usage = parseUsage(
    resultJson({
      input_tokens: 2,
      cache_creation_input_tokens: null,
      cache_read_input_tokens: 110160,
    }),
  )
  assert.equal(usage.cacheCreation, 0)
  assert.equal(
    usage.tokens,
    110162,
    'a warm turn that reports null cache_creation is still a real measurement',
  )
})

test('parseUsage still rejects a usage object missing the cache_creation key entirely', () => {
  assert.throws(
    () => parseUsage(resultJson({ input_tokens: 2, cache_read_input_tokens: 110160 })),
    /carries no cache_creation_input_tokens field at all/,
  )
})

test('parseUsage tolerates surrounding whitespace', () => {
  const stdout = `\n  ${resultJson({ cache_creation_input_tokens: 12 })}  \n`
  assert.equal(parseUsage(stdout).tokens, 12)
})

// Table-driven: every malformed shape must THROW with the raw output embedded,
// never silently report 0 — a silent zero would read as "this variant is free",
// the worst possible failure for a measurement harness.
const MALFORMED = [
  {
    label: 'non-JSON stdout',
    stdout: 'Invalid API key · Please run /login',
    needle: 'Invalid API key · Please run /login',
  },
  {
    label: 'empty stdout',
    stdout: '   \n',
    needle: 'no stdout',
  },
  {
    label: 'JSON result with no usage object',
    stdout: JSON.stringify({ type: 'result', result: 'ok' }),
    needle: '"type":"result"',
  },
  {
    label: 'usage without cache_creation_input_tokens',
    stdout: resultJson({ cache_read_input_tokens: 9 }),
    needle: 'cache_read_input_tokens',
  },
  {
    label: 'cache_creation_input_tokens is not a number',
    stdout: resultJson({ cache_creation_input_tokens: 'lots' }),
    needle: 'lots',
  },
  // ABSENT means 0 (the field is genuinely optional), but PRESENT-BUT-NOT-A-NUMBER
  // is malformed. Coercing the latter to 0 silently DROPS the term: a string
  // "110160" under cache_read once yielded tokens=4, which would publish as a
  // six-figure saving.
  // Needles must be unique to the ECHOED PAYLOAD, per the rule stated below:
  // `non-numeric cache_read_input_tokens` appears in the throw's own prose, so it
  // would stay green if `: ${text}` were dropped.
  {
    label: 'a present-but-non-numeric cache_read_input_tokens',
    stdout: resultJson({ cache_creation_input_tokens: 2, cache_read_input_tokens: '110160' }),
    needle: '"cache_read_input_tokens":"110160"',
  },
  {
    label: 'a present-but-non-numeric input_tokens',
    stdout: resultJson({ cache_creation_input_tokens: 2, input_tokens: [] }),
    needle: '"input_tokens":[]',
  },
  {
    // The needle must be unique to the ECHOED PAYLOAD. `cache_creation_input_tokens`
    // would not be: the zero-guard names all three fields in its own prose, so
    // deleting the `: ${text}` suffix from that throw would leave this case green
    // while the test's name still claimed the raw output was embedded.
    label: 'a launch prefix of zero tokens',
    stdout: resultJson({ cache_creation_input_tokens: 0, cache_read_input_tokens: 0 }),
    needle: '"subtype":"success"',
  },
]

for (const { label, stdout, needle } of MALFORMED) {
  test(`parseUsage rejects ${label} with the raw output in the message`, () => {
    assert.throws(
      () => parseUsage(stdout),
      (err) => {
        assert.ok(err instanceof Error, 'throws an Error')
        assert.ok(
          err.message.includes(needle),
          `message must carry the raw output; got: ${err.message}`,
        )
        return true
      },
    )
  })
}

test('validateManifest accepts the committed variant manifest', () => {
  const manifest = validateManifest(loadManifest(MANIFEST_PATH))
  assert.ok(manifest.variants.length >= 5)
  assert.ok(manifest.prompt.length > 0)
})

test('the committed manifest carries the variants the epic is measured against', () => {
  const manifest = validateManifest(loadManifest(MANIFEST_PATH))
  const names = manifest.variants.map((v) => v.name)
  for (const required of [
    'no-mcp',
    'curated-boss-only',
    'status-quo',
    'status-quo-minus-dup',
    'no-agents',
  ]) {
    assert.ok(names.includes(required), `manifest must define the ${required} variant`)
  }
})

// The plan's sharpest risk: --strict-mcp-config with NO --mcp-config strips the
// boss server too, so `no-mcp` must ship an empty-but-valid config rather than
// the bare flag. Pinned in the manifest itself, not just in the validator.
test('the no-mcp variant is strict AND carries an empty-but-valid mcpServers object', () => {
  const manifest = validateManifest(loadManifest(MANIFEST_PATH))
  const noMcp = manifest.variants.find((v) => v.name === 'no-mcp')
  assert.equal(noMcp.strictMcpConfig, true)
  assert.deepEqual(noMcp.mcpServers, {})
})

test('the committed manifest names no secret values, only env-var references', () => {
  const raw = fs.readFileSync(MANIFEST_PATH, 'utf8')
  // A referenced key is fine (${LINEAR_API_KEY}); an inlined one is not.
  assert.ok(!/Bearer\s+(?!\$\{)/.test(raw), 'no inlined bearer token in the manifest')
  assert.ok(!raw.includes(HERE), 'no absolute machine-local paths in the manifest')
  // `!raw.includes(HERE)` only catches THIS checkout's scripts dir. Any other
  // absolute path — /Users/someone/bin/mcp, a /var/... daemon socket — is just
  // as machine-local and just as unportable, so assert on the shape.
  const manifest = loadManifest(MANIFEST_PATH)
  for (const variant of manifest.variants) {
    for (const [server, spec] of Object.entries(variant.mcpServers ?? {})) {
      const where = `${variant.name}/${server}`
      for (const field of ['command', 'url']) {
        const value = spec?.[field]
        if (typeof value !== 'string') continue
        assert.ok(
          !value.startsWith('/'),
          `${where} "${field}" must be repo-relative, not an absolute machine-local path: ${value}`,
        )
      }
      for (const header of Object.values(spec?.headers ?? {})) {
        assert.match(
          String(header),
          /\$\{[A-Z0-9_]+\}/,
          `${where} header must reference a secret by env-var name, never inline it`,
        )
      }
    }
  }
})

const REJECTIONS = [
  {
    label: 'an unknown top-level key',
    mutate: (m) => {
      m.baselineVariant = 'status-quo'
    },
    needle: 'baselineVariant',
  },
  {
    label: 'an unknown variant key',
    mutate: (m) => {
      m.variants[0].mcpConfig = {}
    },
    needle: 'mcpConfig',
  },
  {
    label: 'duplicate variant names',
    mutate: (m) => {
      m.variants[1].name = 'status-quo'
    },
    needle: 'duplicate',
  },
  {
    label: 'a baseline naming no variant',
    mutate: (m) => {
      m.baseline = 'nope'
    },
    needle: 'nope',
  },
  {
    label: 'an empty variant list',
    mutate: (m) => {
      m.variants = []
    },
    needle: 'variants',
  },
  {
    label: 'strictMcpConfig without an mcpServers object',
    mutate: (m) => {
      delete m.variants[1].mcpServers
    },
    needle: 'strict',
  },
  {
    label: 'a variant with no name',
    mutate: (m) => {
      delete m.variants[1].name
    },
    needle: 'name',
  },
  {
    // Every strictness decision downstream is `=== true`, so a truthy non-boolean
    // reads as NON-strict: the variant would silently merge repo-root and
    // user-scoped MCP config and measure a shape nobody asked for, after paying
    // for it. "true" is the shape a hand-edited manifest actually produces.
    label: 'a string where strictMcpConfig should be a boolean',
    mutate: (m) => {
      m.variants[1].strictMcpConfig = 'true'
    },
    needle: 'strictMcpConfig',
  },
  {
    label: 'a number where strictMcpConfig should be a boolean',
    mutate: (m) => {
      m.variants[1].strictMcpConfig = 1
    },
    needle: 'must be a boolean',
  },
  {
    label: 'a non-array extraArgs',
    mutate: (m) => {
      m.variants[0].extraArgs = '--model sonnet'
    },
    needle: 'extraArgs',
  },
  {
    label: 'a non-string entry inside extraArgs',
    mutate: (m) => {
      m.variants[0].extraArgs = ['--model', 7]
    },
    needle: 'non-empty strings',
  },
  {
    // A second --strict-mcp-config smuggled in through extraArgs would bypass
    // the "strict with no --mcp-config strips the boss server too" guard — the
    // plan's sharpest risk — while the variant's own keys still looked innocent.
    label: 'a reserved flag smuggled in through extraArgs',
    mutate: (m) => {
      m.variants[0].extraArgs = ['--strict-mcp-config']
    },
    needle: 'this runner owns that flag',
  },
  {
    // `claude --help` documents the `=`-joined form, so this is the shape a
    // manifest author actually writes. An exact-string guard would wave it
    // through — and --mcp-config is VARIADIC, so a second one ADDS servers
    // rather than erroring: the variant would measure a strictly larger surface
    // than it declares, exit 0, and publish.
    label: 'a reserved flag in its =-joined form',
    mutate: (m) => {
      m.variants[0].extraArgs = ['--mcp-config=/tmp/extra.json']
    },
    needle: 'this runner owns that flag',
  },
  {
    // --verbose is what makes --print emit the event stream the attachment gate
    // reads, so it belongs to the runner just as --output-format does.
    label: 'the stream flags the attachment gate depends on',
    mutate: (m) => {
      m.variants[0].extraArgs = ['--verbose']
    },
    needle: 'this runner owns that flag',
  },
  {
    // extraArgs is appended last, so for a variant with mcpServers and nothing
    // else it lands right after the --mcp-config path, whose variadic list would
    // swallow a bare leading word instead of reading it as a new flag.
    label: 'an extraArgs list whose first entry is not a flag',
    mutate: (m) => {
      m.variants[0].extraArgs = ['sonnet', '--model']
    },
    needle: 'must start with a flag',
  },
  {
    // The one door checkVariantBinaries does not watch: it keys off `command`,
    // so a spec declaring NEITHER command nor url is skipped by the preflight,
    // written to the config file, and tolerated by claude — which carries on
    // with a smaller tool surface and bills a run nobody can trust.
    label: 'an MCP server spec declaring neither a command nor a url',
    mutate: (m) => {
      m.variants[0].mcpServers = { boss: {} }
    },
    needle: 'declares neither a non-empty "command"',
  },
  {
    label: 'an MCP server spec declaring both a command and a url',
    mutate: (m) => {
      m.variants[0].mcpServers = { boss: { command: './bin/mcp', url: 'https://x.example/mcp' } }
    },
    needle: 'declares both a "command" and a "url"',
  },
  {
    label: 'an MCP server spec whose command is whitespace only',
    mutate: (m) => {
      m.variants[0].mcpServers = { boss: { command: '   ' } }
    },
    needle: 'declares neither a non-empty "command"',
  },
  {
    label: 'an MCP server spec that is not an object',
    mutate: (m) => {
      m.variants[0].mcpServers = { boss: './bin/mcp' }
    },
    needle: 'must be an object',
  },
  {
    // Claude Code validates each --mcp-config entry at startup and SKIPS the
    // ones it cannot classify — a `url` with no `type` among them — then carries
    // on and exits 0. Same silent under-measurement as a missing binary, and the
    // startup warning is suppressed for a caller that captures stderr.
    label: 'a remote MCP server declaring a url but no type',
    mutate: (m) => {
      m.variants[0].mcpServers = { remote: { url: 'https://mcp.linear.app/mcp' } }
    },
    needle: 'needs "http" or "sse"',
  },
  {
    label: 'a remote MCP server declaring an unrecognised type',
    mutate: (m) => {
      m.variants[0].mcpServers = { remote: { type: 'banana', url: 'https://x.example/mcp' } }
    },
    needle: 'needs "http" or "sse"',
  },
  {
    label: 'a stdio MCP server mislabelled with a remote type',
    mutate: (m) => {
      m.variants[0].mcpServers = { boss: { type: 'http', command: './bin/mcp' } }
    },
    needle: 'must omit "type" or set it to "stdio"',
  },
  {
    label: 'a non-string MCP server type',
    mutate: (m) => {
      m.variants[0].mcpServers = { boss: { type: 7, command: './bin/mcp' } }
    },
    needle: '"type" must be a string',
  },
]

// The shapes the committed manifest and repo-root .mcp.json actually use must
// all survive the transport/type cross-check: stdio entries omit `type`, remote
// ones carry `"type": "http"`, and `sse` is a legitimate remote transport too.
test('validateManifest accepts every legitimate MCP transport shape', () => {
  const shapes = {
    stdioNoType: { command: './bin/mcp' },
    stdioTyped: { type: 'stdio', command: './bin/mcp', args: ['--flag'] },
    http: { type: 'http', url: 'https://mcp.linear.app/mcp' },
    sse: { type: 'sse', url: 'https://x.example/sse' },
  }
  for (const [label, spec] of Object.entries(shapes)) {
    const manifest = baseManifest()
    manifest.variants[0].mcpServers = { [label]: spec }
    assert.doesNotThrow(() => validateManifest(manifest), `${label} must be accepted`)
  }
})

for (const { label, mutate, needle } of REJECTIONS) {
  test(`validateManifest rejects ${label}`, () => {
    const manifest = baseManifest()
    mutate(manifest)
    assert.throws(
      () => validateManifest(manifest),
      (err) => {
        assert.ok(
          err.message.toLowerCase().includes(needle.toLowerCase()),
          `message must name the offending item ${needle}; got: ${err.message}`,
        )
        return true
      },
    )
  })
}

test('computeRows reports each variant delta against the chosen baseline', () => {
  const rows = computeRows(
    [
      { name: 'status-quo', tokens: 40000, input: 0, cacheCreation: 40000, cacheRead: 0 },
      { name: 'no-mcp', tokens: 25000, input: 0, cacheCreation: 0, cacheRead: 25000 },
    ],
    'status-quo',
  )
  assert.deepEqual(rows, [
    {
      name: 'status-quo',
      tokens: 40000,
      input: 0,
      cacheCreation: 40000,
      cacheRead: 0,
      delta: 0,
      isBaseline: true,
      sessionContext: false,
    },
    {
      name: 'no-mcp',
      tokens: 25000,
      input: 0,
      cacheCreation: 0,
      cacheRead: 25000,
      delta: -15000,
      isBaseline: false,
      sessionContext: false,
    },
  ])
})

// A variant LARGER than the baseline must surface a positive delta. Clamping it
// to 0 (or reporting the absolute value as a saving) would report a regression
// as a win — the whole point of the harness is to catch that.
test('computeRows keeps a positive delta when a variant is larger than the baseline', () => {
  const rows = computeRows(
    [
      { name: 'status-quo', tokens: 30000, input: 2, cacheCreation: 29998, cacheRead: 0 },
      { name: 'bigger', tokens: 31500, input: 2, cacheCreation: 31498, cacheRead: 0 },
    ],
    'status-quo',
  )
  assert.equal(rows[1].delta, 1500)
})

test('computeRows throws when the baseline variant has no measured result', () => {
  assert.throws(
    () => computeRows([{ name: 'no-mcp', tokens: 1 }], 'status-quo'),
    /status-quo/,
    'the error must name the missing baseline',
  )
})

// Feed formatTable the row shape computeRows ACTUALLY emits — `input` included.
// Omitting it renders the literal string "undefined" in the input_tokens column
// while every assertion below still passes, leaving that column unasserted.
test('formatTable renders signed deltas and the baseline marker', () => {
  const table = formatTable(
    computeRows(
      [
        { name: 'status-quo', tokens: 40000, input: 23513, cacheCreation: 16487, cacheRead: 0 },
        { name: 'no-mcp', tokens: 25000, input: 2, cacheCreation: 0, cacheRead: 24998 },
        { name: 'bigger', tokens: 41000, input: 2, cacheCreation: 40998, cacheRead: 0 },
      ],
      'status-quo',
    ),
  )
  assert.match(table, /status-quo/)
  assert.match(table, /-15000/)
  assert.match(table, /\+1000/)
  assert.match(table, /baseline/)
  assert.match(table, /cache_creation_input_tokens/, 'the AC names this column explicitly')
  assert.match(table, /23513/, 'the input_tokens column must render its value, not "undefined"')
  assert.ok(!table.includes('undefined'), 'no column may render as "undefined"')
})

// stream-json, not json, and NOT a cosmetic difference: the plain `json` result
// object carries no `mcp_servers` key, so only the stream's init event says
// which MCP servers attached. Reverting this pair of flags would silently
// disarm checkMcpAttachment, so pin them.
test('buildClaudeArgs asks for the event stream the attachment gate reads', () => {
  const args = buildClaudeArgs(
    { name: 'curated-boss-only', mcpServers: { boss: { command: './bin/mcp' } } },
    { prompt: 'hi', mcpConfigPath: '/tmp/x.json' },
  )
  assert.deepEqual(args, [
    '-p',
    'hi',
    '--output-format',
    'stream-json',
    '--verbose',
    '--mcp-config',
    '/tmp/x.json',
  ])
})

test('buildClaudeArgs adds --strict-mcp-config only alongside a written config', () => {
  const args = buildClaudeArgs(
    { name: 'no-mcp', strictMcpConfig: true, mcpServers: {} },
    { prompt: 'hi', mcpConfigPath: '/tmp/empty.json' },
  )
  assert.deepEqual(args, [
    '-p',
    'hi',
    '--output-format',
    'stream-json',
    '--verbose',
    '--mcp-config',
    '/tmp/empty.json',
    '--strict-mcp-config',
  ])
})

test('buildClaudeArgs refuses a strict variant with no config file to point at', () => {
  assert.throws(
    () => buildClaudeArgs({ name: 'no-mcp', strictMcpConfig: true }, { prompt: 'hi' }),
    /strict/i,
  )
})

test('buildClaudeArgs threads a variant agents override', () => {
  const args = buildClaudeArgs({ name: 'no-agents', agents: '{}' }, { prompt: 'hi' })
  assert.deepEqual(args, [
    '-p',
    'hi',
    '--output-format',
    'stream-json',
    '--verbose',
    '--agents',
    '{}',
  ])
})

// The report's top-priority next measurement is an --append-system-prompt
// variant, and the one below it is --setting-sources. Neither has a dedicated
// manifest key, so without extraArgs the documented "add a variant without
// touching the runner" property would be false for exactly the cases the report
// tells the next child to reach for.
test('buildClaudeArgs appends extraArgs verbatim, after the flags it owns', () => {
  const args = buildClaudeArgs(
    { name: 'with-append', extraArgs: ['--append-system-prompt', 'You are unattended.'] },
    { prompt: 'hi' },
  )
  assert.deepEqual(args, [
    '-p',
    'hi',
    '--output-format',
    'stream-json',
    '--verbose',
    '--append-system-prompt',
    'You are unattended.',
  ])
})

// The variant above declares no config path, no strict flag and no agents, so
// moving the extraArgs push to the TOP of buildClaudeArgs would leave it green
// while breaking the ordering its name claims. Pin the full argv with all four
// present.
test('buildClaudeArgs orders extraArgs after every flag it owns', () => {
  const args = buildClaudeArgs(
    {
      name: 'everything',
      strictMcpConfig: true,
      mcpServers: {},
      agents: '{}',
      extraArgs: ['--model', 'some-model-id'],
    },
    { prompt: 'hi', mcpConfigPath: '/tmp/x.json' },
  )
  assert.deepEqual(args, [
    '-p',
    'hi',
    '--output-format',
    'stream-json',
    '--verbose',
    '--mcp-config',
    '/tmp/x.json',
    '--strict-mcp-config',
    '--agents',
    '{}',
    '--model',
    'some-model-id',
  ])
})

test('renderOutput emits the table by default and the documented JSON envelope with --json', () => {
  const rows = computeRows(
    [
      { name: 'status-quo', tokens: 40000, input: 2, cacheCreation: 39998, cacheRead: 0 },
      { name: 'no-mcp', tokens: 25000, input: 2, cacheCreation: 0, cacheRead: 24998 },
    ],
    'status-quo',
  )
  assert.match(renderOutput(rows, { baseline: 'status-quo' }), /delta vs baseline/)
  // Key names, not just "some JSON": a typo in `baseline`/`rows` would otherwise
  // ship silently, since main()'s only other exercise is a real billed run.
  const parsed = JSON.parse(renderOutput(rows, { baseline: 'status-quo', json: true }))
  assert.equal(parsed.baseline, 'status-quo')
  // The scope caveat must ride INSIDE the envelope: a --json consumer pipes
  // stdout and would never see a stderr-only disclaimer, which is how an
  // MCP-only subtotal gets quoted as the whole launch prefix.
  assert.match(parsed.scope, /--append-system-prompt/)
  assert.deepEqual(
    parsed.rows.map((r) => r.sessionContext),
    [false, false],
  )
  assert.deepEqual(
    parsed.rows.map((r) => [r.name, r.tokens, r.delta]),
    [
      ['status-quo', 40000, 0],
      ['no-mcp', 25000, -15000],
    ],
  )
})

// A fully warm run reads `cache_creation_input_tokens: 0` AND `input_tokens: 0`
// on every row, which a reader scanning "variant → cache_creation → delta" would
// mistake for a broken measurement. (The published table is the PARTLY cached
// case, not this one — its rows carry `input: 2`; see the test below.)
test('warmRunNotice fires only when every row reported a zero cache_creation', () => {
  const warm = [
    { name: 'a', cacheCreation: 0, cacheRead: 110160, input: 0 },
    { name: 'b', cacheCreation: 0, cacheRead: 89653, input: 0 },
  ]
  const mixed = [
    { name: 'a', cacheCreation: 0, cacheRead: 110160, input: 0 },
    { name: 'b', cacheCreation: 41234, cacheRead: 0, input: 0 },
  ]
  assert.match(String(warmRunNotice(warm)), /fully warm run/)
  assert.equal(warmRunNotice(mixed), null, 'a cold row means the column is informative')
  assert.equal(warmRunNotice([]), null)
})

// A run with cache_creation 0 AND cache_read 0 — the whole prefix billed as plain
// input_tokens — satisfies the zero-column condition while being the exact
// OPPOSITE of warm. A notice that can state the inverse of the truth is the same
// defect it exists to prevent, one level down.
test('warmRunNotice does not call an uncached run warm', () => {
  const uncached = [
    { name: 'a', cacheCreation: 0, cacheRead: 0, input: 110162 },
    { name: 'b', cacheCreation: 0, cacheRead: 0, input: 89655 },
  ]
  const notice = String(warmRunNotice(uncached))
  assert.doesNotMatch(notice, /warm/, 'nothing was read from cache, so it is not a warm run')
  assert.match(notice, /billed as plain input_tokens/)
})

// The middle state, and the one the published table is actually in: the prefix
// came from cache EXCEPT whatever sat past the last breakpoint, which is billed
// as plain input_tokens. Calling that "the whole prefix was read from an existing
// cache" is the same over-claim as calling an uncached run warm — the report
// records a measured run carrying ~23.5k in that term, so it is the common shape.
test('warmRunNotice does not call a partly-cached run fully warm', () => {
  const partly = [
    { name: 'a', cacheCreation: 0, cacheRead: 110160, input: 2 },
    { name: 'b', cacheCreation: 0, cacheRead: 89653, input: 23511 },
  ]
  const notice = String(warmRunNotice(partly))
  assert.doesNotMatch(notice, /whole prefix/, 'part of the prefix was not read from cache')
  assert.doesNotMatch(notice, /fully warm/)
  assert.match(notice, /23513 token\(s\)/, 'it must report how much was billed as plain input')
})

test('runMeasurement measures every variant through the injected spawn', async (t) => {
  const tmpDir = scratchDir(t)
  const seen = []
  const tokensByVariant = { 'status-quo': 40000, 'no-mcp': 25000 }
  const spawn = ({ args, variant }) => {
    seen.push(variant.name)
    return {
      status: 0,
      stdout: streamJson({
        usage: { cache_creation_input_tokens: tokensByVariant[variant.name] },
        mcpServers: attachedFor(variant),
      }),
      stderr: '',
      args,
    }
  }
  const rows = await runMeasurement({
    manifest: validateManifest(baseManifest()),
    spawn,
    tmpDir,
    checkBinaries: skipBinaryCheck,
  })
  assert.deepEqual(seen, ['status-quo', 'no-mcp'])
  assert.deepEqual(
    rows.map((r) => r.delta),
    [0, -15000],
  )
})

// The failure the first real run produced: variant 2 ran inside variant 1's
// ~5-minute ephemeral cache window with a byte-identical prefix, so its
// cache_creation came back 0. Scored on cache_creation alone that reads as a
// 40,000-token saving; scored on the launch prefix it is correctly a 0 delta.
test('runMeasurement scores a warm variant at its full prefix, not as a free launch', async (t) => {
  const tmpDir = scratchDir(t)
  const usageByVariant = {
    'status-quo': { cache_creation_input_tokens: 40000, cache_read_input_tokens: 0 },
    'no-mcp': { cache_creation_input_tokens: 0, cache_read_input_tokens: 40000 },
  }
  const spawn = ({ variant }) => ({
    status: 0,
    stdout: streamJson({ usage: usageByVariant[variant.name], mcpServers: attachedFor(variant) }),
    stderr: '',
  })
  const rows = await runMeasurement({
    manifest: validateManifest(baseManifest()),
    spawn,
    tmpDir,
    checkBinaries: skipBinaryCheck,
  })
  assert.deepEqual(
    rows.map((r) => r.delta),
    [0, 0],
  )
  assert.equal(rows[1].tokens, 40000)
})

test('runMeasurement keeps a variant config inside the scratch dir', async (t) => {
  const tmpDir = scratchDir(t)
  const manifest = validateManifest({
    prompt: 'hi',
    baseline: 'a/../../b',
    variants: [{ name: 'a/../../b', mcpServers: {} }],
  })
  const paths = []
  const spawn = ({ mcpConfigPath }) => {
    paths.push(mcpConfigPath)
    return {
      status: 0,
      stdout: streamJson({ usage: { cache_creation_input_tokens: 5 } }),
      stderr: '',
    }
  }
  await runMeasurement({ manifest, spawn, tmpDir, checkBinaries: skipBinaryCheck })
  assert.equal(path.dirname(paths[0]), tmpDir, 'a variant name must not escape the scratch dir')
})

test('runMeasurement fails loudly when a variant result carries no usage', async (t) => {
  const tmpDir = scratchDir(t)
  const spawn = ({ variant }) => ({
    status: 0,
    stdout: streamJson({ resultEvent: { type: 'result' }, mcpServers: attachedFor(variant) }),
    stderr: '',
  })
  await assert.rejects(
    () =>
      runMeasurement({
        manifest: validateManifest(baseManifest()),
        spawn,
        tmpDir,
        checkBinaries: skipBinaryCheck,
      }),
    (err) => {
      assert.match(err.message, /status-quo/, 'names the variant that failed')
      assert.match(err.message, /"type":"result"/, 'embeds the raw output')
      return true
    },
  )
})

// ---------------------------------------------------------------------------
// The remote-MCP attachment gate.
//
// Claude Code exits 0 with a valid `usage` when a remote server fails to
// authenticate or connect, which is how the published 2026-08-03 run recorded
// itself as clean while Linear and Sentry were never there. checkVariantBinaries
// cannot see this: it keys off a stdio `command`, and a remote server has none.
// ---------------------------------------------------------------------------

test('parseStreamResult finds the init and result events among the noise around them', () => {
  const stdout = [
    // A hook `system` event: same type, no mcp_servers. Keying off `type` alone
    // would pick this one and then report "no mcp_servers array".
    JSON.stringify({ type: 'system', subtype: 'hook_started', hook_name: 'x' }),
    'not json at all — a banner claude printed',
    JSON.stringify({
      type: 'system',
      subtype: 'init',
      mcp_servers: [{ name: 'boss', status: 'connected' }],
    }),
    JSON.stringify({ type: 'assistant', message: { content: [] } }),
    JSON.stringify({
      type: 'result',
      subtype: 'success',
      usage: { cache_creation_input_tokens: 7 },
    }),
    '',
  ].join('\n')
  const { mcpServers, resultLine } = parseStreamResult(stdout)
  assert.deepEqual(mcpServers, [{ name: 'boss', status: 'connected' }])
  assert.equal(parseUsage(resultLine).tokens, 7)
})

test('parseStreamResult refuses a stream missing the events the gate depends on', () => {
  const init = JSON.stringify({ type: 'system', subtype: 'init', mcp_servers: [] })
  const result = JSON.stringify({ type: 'result', usage: { cache_creation_input_tokens: 1 } })
  assert.throws(() => parseStreamResult(''), /no stdout/)
  assert.throws(() => parseStreamResult(result), /no system\/init event/)
  assert.throws(() => parseStreamResult(init), /no result event/)
  // An init event with no mcp_servers key must NOT be read as "nothing declared,
  // nothing to check" — that would silently disarm the gate.
  assert.throws(
    () => parseStreamResult(`${JSON.stringify({ type: 'system', subtype: 'init' })}\n${result}`),
    /cannot verify attachment/,
  )
})

test('checkMcpAttachment accepts a run where every reported server connected', () => {
  assert.doesNotThrow(() =>
    checkMcpAttachment({ name: 'v', mcpServers: { boss: { command: './bin/mcp' } } }, [
      { name: 'boss', status: 'connected' },
    ]),
  )
  assert.doesNotThrow(() => checkMcpAttachment({ name: 'no-mcp', mcpServers: {} }, []))
})

// The published run's exact failure: Sentry answered `needs-auth`, contributed
// no tools, and the run recorded a number anyway.
test('checkMcpAttachment rejects a declared server that did not connect', () => {
  assert.throws(
    () =>
      checkMcpAttachment(
        {
          name: 'status-quo-minus-dup',
          mcpServers: { boss: { command: './bin/mcp' }, 'bossanova-sentry': { url: 'https://x' } },
        },
        [
          { name: 'boss', status: 'connected' },
          { name: 'bossanova-sentry', status: 'needs-auth' },
        ],
      ),
    (err) => {
      assert.match(err.message, /status-quo-minus-dup/, 'names the variant')
      assert.match(err.message, /bossanova-sentry/, 'names the server')
      assert.match(err.message, /needs-auth/, 'quotes the status it actually reported')
      return true
    },
  )
})

// "No server reports an error" is a DIFFERENT and weaker contract: a spec claude
// rejects during config validation is dropped from the list entirely, so the
// only way to catch it is to require every declared name to be present.
test('checkMcpAttachment rejects a declared server that is absent from the list', () => {
  assert.throws(
    () =>
      checkMcpAttachment(
        { name: 'v', mcpServers: { boss: { command: './bin/mcp' }, ghost: { url: 'https://x' } } },
        [{ name: 'boss', status: 'connected' }],
      ),
    /declares MCP server "ghost" but the session reported no server by that name/,
  )
})

// A NON-strict variant merges repo-root .mcp.json and any user-scoped config, so
// the surface it measures includes servers it never declared — and those were
// exactly the ones that sat out the published run. Gating only the declared set
// would leave that hole open.
test('checkMcpAttachment rejects an undeclared merged-in server that did not connect', () => {
  assert.throws(
    () =>
      checkMcpAttachment({ name: 'status-quo', mcpServers: { boss: { command: './bin/mcp' } } }, [
        { name: 'boss', status: 'connected' },
        { name: 'bossanova-linear', status: 'failed' },
      ]),
    /bossanova-linear.*"failed"/s,
  )
})

test('runMeasurement refuses to record a variant whose MCP surface did not attach', async (t) => {
  const tmpDir = scratchDir(t)
  const spawn = ({ variant }) => ({
    status: 0,
    stdout: streamJson({
      usage: { cache_creation_input_tokens: 40000 },
      mcpServers:
        variant.name === 'status-quo'
          ? [{ name: 'boss', status: 'needs-auth' }]
          : attachedFor(variant),
    }),
    stderr: '',
  })
  await assert.rejects(
    () =>
      runMeasurement({
        manifest: validateManifest(baseManifest()),
        spawn,
        tmpDir,
        checkBinaries: skipBinaryCheck,
      }),
    (err) => {
      assert.match(err.message, /status-quo/)
      assert.match(err.message, /not "connected"/)
      // One prefix, not two: the gate names the variant itself, so runMeasurement
      // must not wrap it again.
      assert.equal(err.message.match(/variant "status-quo"/g).length, 1)
      return true
    },
  )
})

// ---------------------------------------------------------------------------
// Scope: bossd's --append-system-prompt session context is in no row unless a
// variant asks for it, and the harness must say so itself — prose in the report
// does not travel with a --json payload.
// ---------------------------------------------------------------------------

test('variantCarriesSessionContext reads the flag, in both its forms', () => {
  assert.equal(variantCarriesSessionContext({ name: 'a' }), false)
  assert.equal(variantCarriesSessionContext({ name: 'a', extraArgs: ['--model', 'x'] }), false)
  assert.equal(
    variantCarriesSessionContext({ name: 'a', extraArgs: ['--append-system-prompt', 'ctx'] }),
    true,
  )
  assert.equal(
    variantCarriesSessionContext({ name: 'a', extraArgs: ['--append-system-prompt=ctx'] }),
    true,
    'the =-joined form supplies the same text',
  )
  assert.equal(
    variantCarriesSessionContext({ name: 'a', extraArgs: ['--append-system-prompt-file', '/f'] }),
    true,
  )
})

test('scopeNotice names the rows measured without bossd session context', () => {
  const notice = scopeNotice([
    { name: 'status-quo', sessionContext: false },
    { name: 'with-context', sessionContext: true },
  ])
  assert.match(String(notice), /1 of 2/)
  assert.match(String(notice), /status-quo/)
  assert.ok(!String(notice).includes('with-context'), 'a labelled row needs no disclaimer')
  assert.match(String(notice), /subtotal/, 'says what the unlabelled rows actually are')
  assert.equal(
    scopeNotice([{ name: 'a', sessionContext: true }]),
    null,
    'nothing to disclaim once every row carries it',
  )
})

test('runMeasurement labels each row with whether it carried the session context', async (t) => {
  const tmpDir = scratchDir(t)
  const manifest = baseManifest()
  manifest.variants[1].extraArgs = ['--append-system-prompt', 'boss session context']
  const spawn = ({ variant }) => ({
    status: 0,
    stdout: streamJson({
      usage: { cache_creation_input_tokens: 40000 },
      mcpServers: attachedFor(variant),
    }),
    stderr: '',
  })
  const rows = await runMeasurement({
    manifest: validateManifest(manifest),
    spawn,
    tmpDir,
    checkBinaries: skipBinaryCheck,
  })
  assert.deepEqual(
    rows.map((r) => [r.name, r.sessionContext]),
    [
      ['status-quo', false],
      ['no-mcp', true],
    ],
  )
})

test('runMeasurement surfaces a non-zero claude exit with its stderr', async (t) => {
  const tmpDir = scratchDir(t)
  const spawn = () => ({ status: 1, stdout: '', stderr: 'claude: not logged in' })
  await assert.rejects(
    () =>
      runMeasurement({
        manifest: validateManifest(baseManifest()),
        spawn,
        tmpDir,
        checkBinaries: skipBinaryCheck,
      }),
    /not logged in/,
  )
})

// `stderr || stdout` would short-circuit: on the timeout path stderr always
// carries the "timed out after" prefix, so the partial stdout that path
// deliberately preserves would never reach the operator — and claude prints its
// most actionable diagnostic to stdout.
test('runMeasurement surfaces BOTH streams of a failed claude, not just stderr', async (t) => {
  const tmpDir = scratchDir(t)
  const spawn = () => ({
    status: -1,
    stdout: 'Invalid API key · Please run /login',
    stderr: 'timed out after 120000ms: spawnSync claude ETIMEDOUT',
  })
  await assert.rejects(
    () =>
      runMeasurement({
        manifest: validateManifest(baseManifest()),
        spawn,
        tmpDir,
        checkBinaries: skipBinaryCheck,
      }),
    (err) => {
      assert.match(err.message, /timed out after 120000ms/, 'keeps the stderr diagnosis')
      assert.match(err.message, /Please run \/login/, 'and the stdout diagnosis')
      return true
    },
  )
})

// Claude Code TOLERATES an MCP server that fails to launch — it carries on with
// a smaller tool surface — so a missing `./bin/mcp` (a gitignored `make build`
// artifact) would make every delta collapse toward 0 while the run still exits
// 0, reporting "the boss MCP is free". That is the same silent-under-report
// parseUsage guards against, one level up.
test('checkVariantBinaries rejects a variant whose stdio MCP command is missing', () => {
  assert.throws(
    () =>
      checkVariantBinaries(
        { name: 'status-quo', mcpServers: { boss: { command: './bin/mcp' } } },
        { repoRoot: '/repo', exists: () => false },
      ),
    (err) => {
      assert.match(err.message, /status-quo/, 'names the variant')
      assert.match(err.message, /boss/, 'names the server')
      assert.match(err.message, /bin\/mcp/, 'names the missing binary')
      return true
    },
  )
})

test('checkVariantBinaries resolves a relative command against the repo root, not cwd', () => {
  const statted = []
  checkVariantBinaries(
    { name: 'status-quo', mcpServers: { boss: { command: './bin/mcp' } } },
    {
      repoRoot: '/repo',
      exists: (p) => {
        statted.push(p)
        return true
      },
    },
  )
  assert.deepEqual(statted, [path.join('/repo', 'bin', 'mcp')])
})

// A bare `npx` is resolved by the OS from PATH and cannot be cheaply stat'd;
// only separator-bearing commands are paths this harness can verify. HTTP
// servers declare a url, not a command, and are likewise not stattable.
test('checkVariantBinaries skips PATH-resolved and http servers rather than false-failing', () => {
  const statted = []
  checkVariantBinaries(
    {
      name: 'mixed',
      mcpServers: {
        viaPath: { command: 'npx' },
        remote: { type: 'http', url: 'https://mcp.linear.app/mcp' },
        local: { command: './bin/mcp' },
      },
    },
    {
      repoRoot: '/repo',
      exists: (p) => {
        statted.push(p)
        return true
      },
    },
  )
  assert.deepEqual(statted, [path.join('/repo', 'bin', 'mcp')])
})

// The separator test must be `/[\\/]/`, never `path.sep`: on win32 path.sep is
// `\`, so a manifest written with the portable `./bin/mcp` would read as having
// "no separator" and the guard would silently degrade to a no-op — the very
// failure it exists to prevent. On a POSIX runner `path.sep` and this regex agree
// for forward slashes, so only a BACKSLASH path distinguishes them.
test('checkVariantBinaries treats a backslash as a path separator too (win32-shaped)', () => {
  const statted = []
  assert.throws(
    () =>
      checkVariantBinaries(
        { name: 'v', mcpServers: { boss: { command: '.\\bin\\mcp' } } },
        {
          repoRoot: '/repo',
          exists: (p) => {
            statted.push(p)
            return false
          },
        },
      ),
    /not an executable file/,
    'a backslash-separated command must be checked, not skipped as PATH-resolved',
  )
  assert.equal(statted.length, 1, 'the guard must actually have stat-ed it')
})

test('checkVariantBinaries accepts a variant declaring no mcpServers at all', () => {
  assert.doesNotThrow(() => checkVariantBinaries({ name: 'no-agents' }, { exists: () => false }))
})

// The property is that the preflight covers EVERY variant before ANY is
// spawned. A stub that throws on the FIRST variant cannot distinguish that from
// a per-variant check inside the measurement loop, so throw on the SECOND: with
// the check hoisted, spawned stays 0; with it inside the loop, variant 1 has
// already been billed.
// runMeasurement is exported, and validateManifest's only other call site is
// main(). A caller reaching runMeasurement directly — the next child adding a
// variant — would otherwise get the executability preflight and none of the
// SHAPE checks, and spawn a variant whose smuggled --mcp-config measures a
// larger surface than it declares.
test('runMeasurement validates the manifest itself rather than trusting its caller', async (t) => {
  const tmpDir = scratchDir(t)
  let spawned = 0
  const manifest = baseManifest()
  manifest.variants[0].extraArgs = ['--mcp-config=/tmp/extra.json']
  await assert.rejects(
    () =>
      runMeasurement({
        manifest, // deliberately NOT passed through validateManifest first
        spawn: () => {
          spawned += 1
          return {
            status: 0,
            stdout: streamJson({ usage: { cache_creation_input_tokens: 1 } }),
            stderr: '',
          }
        },
        tmpDir,
        checkBinaries: skipBinaryCheck,
      }),
    /this runner owns that flag/,
  )
  assert.equal(spawned, 0, 'an invalid manifest must never reach a billed claude call')
})

test('runMeasurement preflights every variant before spawning any of them', async (t) => {
  const tmpDir = scratchDir(t)
  let spawned = 0
  const spawn = () => {
    spawned += 1
    return {
      status: 0,
      stdout: streamJson({ usage: { cache_creation_input_tokens: 1 } }),
      stderr: '',
    }
  }
  await assert.rejects(
    () =>
      runMeasurement({
        manifest: validateManifest(baseManifest()),
        spawn,
        tmpDir,
        checkBinaries: (variant) => {
          if (variant.name === 'no-mcp') throw new Error('bin/mcp does not exist')
        },
      }),
    /bin\/mcp does not exist/,
  )
  assert.equal(
    spawned,
    0,
    'a preflight failure on ANY variant must precede the first billed claude call',
  )
})

// Every other checkVariantBinaries test injects `exists`, so without this one the
// production default (statSync + X_OK) would never execute.
test('checkVariantBinaries rejects a directory and a non-executable file for real', (t) => {
  const dir = scratchDir(t)
  const plainFile = path.join(dir, 'not-executable')
  fs.writeFileSync(plainFile, '#!/bin/sh\n', { mode: 0o644 })
  const execFile = path.join(dir, 'runnable')
  fs.writeFileSync(execFile, '#!/bin/sh\n', { mode: 0o755 })

  const check = (command) =>
    checkVariantBinaries({ name: 'v', mcpServers: { boss: { command } } }, { repoRoot: dir })

  assert.throws(() => check('./missing-entirely'), /not an executable file/)
  assert.throws(
    () => check('./not-executable'),
    /not an executable file/,
    'a non-+x file cannot run',
  )
  assert.throws(() => check('./'), /not an executable file/, 'a directory is not a command')
  assert.doesNotThrow(() => check('./runnable'))
})

test('parseUsage refuses an errored result rather than recording its truncated usage', () => {
  const stdout = JSON.stringify({
    type: 'result',
    subtype: 'error_during_execution',
    is_error: true,
    usage: { cache_creation_input_tokens: 12 },
  })
  assert.throws(
    () => parseUsage(stdout),
    (err) => {
      assert.match(err.message, /errored result/)
      assert.match(err.message, /error_during_execution/, 'embeds the raw output')
      return true
    },
  )
})

// Falling through to the default manifest would spend one real API call per
// variant measuring a config the caller believes they overrode, and report it
// as success.
test('parseArgv rejects a --manifest with no path rather than silently defaulting', () => {
  assert.throws(() => parseArgv(['--manifest']), /--manifest needs a path/)
  assert.throws(() => parseArgv(['--manifest', '--json']), /--manifest needs a path/)
})

test('parseArgv reads a manifest path and the --json flag in any order', () => {
  assert.deepEqual(parseArgv(['--json', '--manifest', '/tmp/m.json']), {
    manifest: '/tmp/m.json',
    json: true,
  })
  assert.deepEqual(parseArgv(['--manifest', '/tmp/m.json']), {
    manifest: '/tmp/m.json',
    json: false,
  })
  assert.equal(parseArgv([]).manifest, MANIFEST_PATH)
})

test('parseArgv rejects an unknown argument', () => {
  assert.throws(() => parseArgv(['--variants', 'x']), /unknown argument/)
})
