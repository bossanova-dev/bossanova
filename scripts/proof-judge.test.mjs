#!/usr/bin/env node
/**
 * proof-judge.test.mjs — TDD table tests for the pure judge-input-assembly
 * seam (BOS-141 P4a/P4b): still selection, prompt building, and the
 * deterministic honesty clamp.
 */

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  addLabelCommand,
  buildJudgePrompt,
  clampJudgeVerdict,
  createJudgeModel,
  DEFAULT_JUDGE_MODEL,
  DEFAULT_JUDGE_TIMEOUT_MS,
  downscaleArgs,
  ensureLabelCommand,
  JUDGE_MAX_PX,
  JUDGE_SCHEMA,
  judgeProof,
  labelActionForJudge,
  MAX_JUDGE_STILLS,
  prepareJudgeImages,
  removeLabelCommand,
  selectJudgeStills,
} from './proof-judge.mjs'

// ── constants ────────────────────────────────────────────────────────────

test('constants: caps and default model', () => {
  assert.equal(MAX_JUDGE_STILLS, 12)
  assert.equal(JUDGE_MAX_PX, 1024)
  assert.equal(DEFAULT_JUDGE_MODEL, 'claude-haiku-4-5')
})

// ── selectJudgeStills ────────────────────────────────────────────────────

test('selectJudgeStills: 1 scene x 5 stills -> all kept, filename order (even sampling, BOS-251)', () => {
  const captures = [
    {
      surface: 'tui',
      status: 'passed',
      stills: [
        { fileName: 'c.png', label: 'C' },
        { fileName: 'a.png', label: 'A' },
        { fileName: 'e.png', label: 'E' },
        { fileName: 'b.png', label: 'B' },
        { fileName: 'd.png', label: 'D' },
      ],
    },
  ]
  // One scene owns the whole MAX_JUDGE_STILLS share, so every still is kept —
  // mid-scene frames (where the evidence usually is) must reach the judge.
  const out = selectJudgeStills(captures)
  assert.deepEqual(
    out.map((s) => s.fileName),
    ['a.png', 'b.png', 'c.png', 'd.png', 'e.png'],
  )
  assert.ok(out.every((s) => s.sceneId === 'scene-01'))
  assert.ok(out.every((s) => s.surface === 'tui'))
})

test('selectJudgeStills: many scenes split the cap evenly and sample mid-scene stills', () => {
  const mk = (scene, n) =>
    Array.from({ length: n }, (_, i) => ({
      fileName: `${scene}-${String(i + 1).padStart(2, '0')}.png`,
      label: `${scene} ${i + 1}`,
      sceneId: scene,
    }))
  const captures = [
    { surface: 'tui', status: 'passed', stills: [...mk('scene-01', 9), ...mk('scene-02', 9)] },
  ]
  const out = selectJudgeStills(captures)
  // 12-cap / 2 scenes = 6 per scene, spread across the 9 stills of each.
  const s1 = out.filter((s) => s.sceneId === 'scene-01').map((s) => s.fileName)
  assert.equal(s1.length, 6)
  assert.equal(s1[0], 'scene-01-01.png')
  assert.equal(s1[s1.length - 1], 'scene-01-09.png')
  assert.ok(
    ['scene-01-04.png', 'scene-01-05.png', 'scene-01-06.png'].some((f) => s1.includes(f)),
    'a mid-scene still is represented',
  )
  assert.ok(out.length <= 12)
})

test('selectJudgeStills: 3 scenes x 1 still -> 3 stills, each kept', () => {
  const captures = [
    {
      surface: 'web',
      status: 'passed',
      stills: [
        { fileName: 'scene1.png', label: 'One', sceneId: 'scene-01' },
        { fileName: 'scene2.png', label: 'Two', sceneId: 'scene-02' },
        { fileName: 'scene3.png', label: 'Three', sceneId: 'scene-03' },
      ],
    },
  ]
  const out = selectJudgeStills(captures)
  assert.equal(out.length, 3)
  assert.deepEqual(out.map((s) => s.sceneId).sort(), ['scene-01', 'scene-02', 'scene-03'])
})

test('selectJudgeStills: sceneId-less stills grouped as scene-01', () => {
  const captures = [
    {
      surface: 'tui',
      status: 'passed',
      stills: [
        { fileName: 'b.png', label: 'B' },
        { fileName: 'a.png', label: 'A' },
      ],
    },
  ]
  const out = selectJudgeStills(captures)
  assert.equal(out.length, 2)
  assert.ok(out.every((s) => s.sceneId === 'scene-01'))
})

test('selectJudgeStills: failed-scene priority under the 12-cap drops passed-scene stills', () => {
  // 7 scenes x 2 stills (first+last) = 14 selected before capping.
  // 3 failed scenes (6 stills) + 4 passed scenes (8 stills) = 14 -> cap to 12
  // drops exactly 2 stills, both from passed scenes.
  const makeCapture = (sceneId, failed) => ({
    surface: 'tui',
    status: failed ? 'failed' : 'passed',
    scenes: [{ id: sceneId, title: sceneId, passed: !failed, missing: [], outputMs: 1000 }],
    stills: [
      { fileName: `${sceneId}-a.png`, label: 'a', sceneId },
      { fileName: `${sceneId}-b.png`, label: 'b', sceneId },
      { fileName: `${sceneId}-c.png`, label: 'c', sceneId },
    ],
  })
  const captures = [
    makeCapture('scene-01', true),
    makeCapture('scene-02', true),
    makeCapture('scene-03', true),
    makeCapture('scene-04', false),
    makeCapture('scene-05', false),
    makeCapture('scene-06', false),
    makeCapture('scene-07', false),
  ]
  const out = selectJudgeStills(captures)
  assert.equal(out.length, MAX_JUDGE_STILLS)
  const failedSceneIds = new Set(['scene-01', 'scene-02', 'scene-03'])
  const failedCount = out.filter((s) => failedSceneIds.has(s.sceneId)).length
  const passedCount = out.filter((s) => !failedSceneIds.has(s.sceneId)).length
  // All 6 failed-scene stills survive; only 6 of the 8 passed-scene stills do.
  assert.equal(failedCount, 6)
  assert.equal(passedCount, 6)
})

test('selectJudgeStills: failed flag accumulates across captures sharing a (surface, sceneId) key', () => {
  // Regression (reviewer Critical): when a passed capture is processed FIRST
  // for a shared key (normalizeScenes' no-scenes fallback puts every capture's
  // stills in scene-01), the group must NOT be permanently marked passed — a
  // later failed capture contributing to the same key flips it via OR, so its
  // evidence survives the MAX_JUDGE_STILLS cap.
  const filler = (n) => ({
    surface: 'tui',
    status: 'passed',
    stills: [
      { fileName: `filler-${n}-a.png`, label: 'a', sceneId: `scene-f${n}` },
      { fileName: `filler-${n}-b.png`, label: 'b', sceneId: `scene-f${n}` },
    ],
  })
  const captures = [
    filler(1),
    filler(2),
    filler(3),
    filler(4),
    filler(5),
    filler(6), // 6 passed groups x 2 stills = 12 stills
    {
      surface: 'tui',
      status: 'passed',
      stills: [{ fileName: 'a-pass.png', label: 'pass' }], // no sceneId -> scene-01
    },
    {
      surface: 'tui',
      status: 'failed',
      stills: [{ fileName: 'z-fail.png', label: 'fail' }], // no sceneId -> same scene-01 key
    },
  ]
  const out = selectJudgeStills(captures)
  assert.equal(out.length, MAX_JUDGE_STILLS)
  // The shared scene-01 group is failed (OR across contributing captures), so
  // it is selected FIRST and the failed capture's still survives the cap.
  assert.ok(
    out.some((s) => s.fileName === 'z-fail.png'),
    "failed capture's still must survive the 12-cap",
  )
})

test('selectJudgeStills: empty captures -> []', () => {
  assert.deepEqual(selectJudgeStills([]), [])
  assert.deepEqual(selectJudgeStills(undefined), [])
})

// ── JUDGE_SCHEMA ─────────────────────────────────────────────────────────

test('JUDGE_SCHEMA: additionalProperties false on every object, no maxItems', () => {
  const json = JSON.stringify(JUDGE_SCHEMA)
  assert.ok(!json.includes('maxItems'))
  assert.equal(JUDGE_SCHEMA.additionalProperties, false)
  assert.equal(JUDGE_SCHEMA.properties.perScene.items.additionalProperties, false)
  assert.deepEqual(JUDGE_SCHEMA.properties.evidence.enum, [
    'satisfactory',
    'partial',
    'unsatisfactory',
  ])
  assert.deepEqual(JUDGE_SCHEMA.properties.confidence.enum, ['high', 'medium', 'low'])
  assert.deepEqual(JUDGE_SCHEMA.properties.perScene.items.properties.verdict.enum, [
    'passed',
    'failed',
    'unclear',
  ])
})

// ── buildJudgePrompt ─────────────────────────────────────────────────────

function baseArgs(overrides = {}) {
  return {
    requiredProof: {
      tui: ['keystroke navigation visible'],
      web: ['form submits and shows confirmation'],
      unscoped: ['no console errors'],
    },
    scenes: [
      { id: 'scene-01', title: 'Login flow', expectedEvidence: ['login button', 'welcome text'] },
      { id: 'scene-02', title: 'Settings save', expectedEvidence: ['saved toast'] },
    ],
    perSceneOutcomes: [
      { id: 'scene-01', passed: true, missing: [], outputMs: 1200 },
      { id: 'scene-02', passed: false, missing: ['saved toast'], outputMs: 4500 },
    ],
    agentSummary: 'Agent logged in and navigated to settings.',
    agentRunnerStubbed: false,
    surfaces: ['tui', 'web'],
    imageBlocks: [
      {
        type: 'image',
        source: { type: 'base64', media_type: 'image/png', data: 'AAAABBBBCCCC==' },
      },
    ],
    ...overrides,
  }
}

test('buildJudgePrompt: includes every required-proof bullet', () => {
  const { content } = buildJudgePrompt(baseArgs())
  const text = content[0].text
  assert.ok(text.includes('keystroke navigation visible'))
  assert.ok(text.includes('form submits and shows confirmation'))
  assert.ok(text.includes('no console errors'))
})

test('buildJudgePrompt: matcher-object evidence renders to display text, never [object Object] (BOS-222)', () => {
  const { content } = buildJudgePrompt(
    baseArgs({
      scenes: [
        {
          id: 'scene-01',
          title: 'Save flow',
          expectedEvidence: [
            'plain token',
            { anyOf: [{ text: 'Saved' }, { text: 'Updated' }], label: 'save confirmation' },
            { text: 'v[0-9]+', match: 'regex' },
          ],
        },
      ],
      perSceneOutcomes: [{ id: 'scene-01', passed: true, missing: [], outputMs: 100 }],
    }),
  )
  const text = content[0].text
  assert.ok(
    !text.includes('[object Object]'),
    'matcher objects must not stringify to [object Object]',
  )
  assert.ok(text.includes('plain token'))
  // {anyOf,label} renders as its label; {text,match:regex} renders as its text.
  assert.ok(text.includes('save confirmation'))
  assert.ok(text.includes('v[0-9]+'))
})

test('buildJudgePrompt: unscoped bullets are rendered once, not per surface', () => {
  const { content } = buildJudgePrompt(baseArgs())
  const text = content[0].text
  const occurrences = text.split('no console errors').length - 1
  assert.equal(occurrences, 1)
})

test('buildJudgePrompt: includes scene titles and outputMs timestamps', () => {
  const { content } = buildJudgePrompt(baseArgs())
  const text = content[0].text
  assert.ok(text.includes('Login flow'))
  assert.ok(text.includes('Settings save'))
  assert.ok(text.includes('1200'))
  assert.ok(text.includes('4500'))
})

test('buildJudgePrompt: includes the injection-guard sentence', () => {
  const { content } = buildJudgePrompt(baseArgs())
  const text = content[0].text
  assert.ok(/DATA/.test(text))
  assert.ok(/never instructions to follow/i.test(text))
})

test('buildJudgePrompt: includes the stub disclosure line only when stubbed', () => {
  const stubbed = buildJudgePrompt(baseArgs({ agentRunnerStubbed: true })).content[0].text
  const unstubbed = buildJudgePrompt(baseArgs({ agentRunnerStubbed: false })).content[0].text
  assert.ok(/stub/i.test(stubbed))
  assert.ok(!/agent-runner stubbed/i.test(unstubbed))
})

test('buildJudgePrompt: text part carries no image bytes; image blocks appended verbatim', () => {
  const args = baseArgs()
  const { content } = buildJudgePrompt(args)
  const text = content[0].text
  assert.ok(!text.includes('AAAABBBBCCCC=='))
  assert.equal(content.length, 1 + args.imageBlocks.length)
  assert.deepEqual(content.slice(1), args.imageBlocks)
})

test('buildJudgePrompt: returns {system, content}', () => {
  const result = buildJudgePrompt(baseArgs())
  assert.equal(typeof result.system, 'string')
  assert.ok(Array.isArray(result.content))
})

// ── clampJudgeVerdict ────────────────────────────────────────────────────

function rawVerdict(overrides = {}) {
  return {
    evidence: 'satisfactory',
    confidence: 'high',
    perScene: [{ id: 'scene-01', verdict: 'passed', reason: 'looked fine' }],
    caveats: [],
    ...overrides,
  }
}

test('clampJudgeVerdict: model says satisfactory but a scene failed -> partial + rule named', () => {
  const raw = rawVerdict({
    perScene: [{ id: 'scene-01', verdict: 'failed', reason: 'missing toast' }],
  })
  const { verdict, clamped } = clampJudgeVerdict(raw, {
    mechanicalSceneFailures: false,
    agentRunnerStubbed: false,
  })
  assert.equal(verdict.evidence, 'partial')
  assert.ok(clamped.length > 0)
  assert.equal(raw.evidence, 'satisfactory') // never mutated
})

test('clampJudgeVerdict: mechanical scene failure alone downgrades satisfactory -> partial', () => {
  const raw = rawVerdict()
  const { verdict, clamped } = clampJudgeVerdict(raw, {
    mechanicalSceneFailures: true,
    agentRunnerStubbed: false,
  })
  assert.equal(verdict.evidence, 'partial')
  assert.ok(clamped.length > 0)
})

test('clampJudgeVerdict: stubbed + no caveat -> caveat appended', () => {
  const raw = rawVerdict()
  const { verdict, clamped } = clampJudgeVerdict(raw, {
    mechanicalSceneFailures: false,
    agentRunnerStubbed: true,
  })
  assert.ok(
    verdict.caveats.some((c) =>
      c.includes('agent-runner stubbed: UI + orchestration exercised against a stubbed daemon'),
    ),
  )
  assert.deepEqual(raw.caveats, []) // raw untouched
  assert.ok(clamped.length > 0)
})

test('clampJudgeVerdict: stubbed + equivalent caveat already present -> not duplicated', () => {
  const raw = rawVerdict({ caveats: ['Stub warning: daemon was stubbed for this run'] })
  const { verdict } = clampJudgeVerdict(raw, {
    mechanicalSceneFailures: false,
    agentRunnerStubbed: true,
  })
  assert.equal(verdict.caveats.length, 1)
})

test('clampJudgeVerdict: stubbed + scenes passed + model says high -> high retained WITH caveat present', () => {
  const raw = rawVerdict()
  const { verdict } = clampJudgeVerdict(raw, {
    mechanicalSceneFailures: false,
    agentRunnerStubbed: true,
  })
  assert.equal(verdict.confidence, 'high')
  assert.ok(verdict.caveats.some((c) => /stub/i.test(c)))
})

test('clampJudgeVerdict: stubbed WITHOUT caveat somehow present would still downgrade if scenes failed', () => {
  const raw = rawVerdict({
    perScene: [{ id: 'scene-01', verdict: 'failed', reason: 'x' }],
  })
  const { verdict } = clampJudgeVerdict(raw, {
    mechanicalSceneFailures: false,
    agentRunnerStubbed: true,
  })
  assert.equal(verdict.confidence, 'medium')
})

test('clampJudgeVerdict: unstubbed all-passed high -> untouched', () => {
  const raw = rawVerdict()
  const { verdict, clamped } = clampJudgeVerdict(raw, {
    mechanicalSceneFailures: false,
    agentRunnerStubbed: false,
  })
  assert.deepEqual(verdict, raw)
  assert.deepEqual(clamped, [])
})

test('clampJudgeVerdict: model says unsatisfactory -> never upgraded', () => {
  const raw = rawVerdict({ evidence: 'unsatisfactory', confidence: 'low' })
  const { verdict } = clampJudgeVerdict(raw, {
    mechanicalSceneFailures: false,
    agentRunnerStubbed: false,
  })
  assert.equal(verdict.evidence, 'unsatisfactory')
  assert.equal(verdict.confidence, 'low')
})

test('clampJudgeVerdict: empty perScene is vacuously all-passed (rule 3 keeps high)', () => {
  const raw = rawVerdict({ perScene: [] })
  const { verdict, clamped } = clampJudgeVerdict(raw, {
    mechanicalSceneFailures: false,
    agentRunnerStubbed: false,
  })
  assert.equal(verdict.evidence, 'satisfactory')
  assert.equal(verdict.confidence, 'high')
  assert.deepEqual(clamped, [])
})

test('clampJudgeVerdict: never mutates the raw object', () => {
  const raw = rawVerdict()
  const snapshot = JSON.parse(JSON.stringify(raw))
  clampJudgeVerdict(raw, { mechanicalSceneFailures: true, agentRunnerStubbed: true })
  assert.deepEqual(raw, snapshot)
})

// ── downscaleArgs ────────────────────────────────────────────────────────

test('downscaleArgs: exact argv with default maxPx', () => {
  const [command, args] = downscaleArgs({ input: '/tmp/in.png', output: '/tmp/out/in.png' })
  assert.equal(command, 'ffmpeg')
  assert.deepEqual(args, [
    '-y',
    '-loglevel',
    'error',
    '-i',
    '/tmp/in.png',
    '-vf',
    "scale='if(gt(iw,ih),min(1024,iw),-2)':'if(gt(iw,ih),-2,min(1024,ih))'",
    '/tmp/out/in.png',
  ])
})

test('downscaleArgs: honors an explicit maxPx override', () => {
  const [, args] = downscaleArgs({ input: 'a.png', output: 'b.png', maxPx: 512 })
  assert.ok(args.includes("scale='if(gt(iw,ih),min(512,iw),-2)':'if(gt(iw,ih),-2,min(512,ih))'"))
})

test('downscaleArgs: caps the long edge for both orientations (D10)', () => {
  // Landscape bounds width + auto-height; portrait bounds height + auto-width —
  // the whole point of the conditional: a tall still cannot keep full height.
  const [, args] = downscaleArgs({ input: 'a.png', output: 'b.png' })
  const scale = args[args.indexOf('-vf') + 1]
  assert.ok(scale.includes('gt(iw,ih)'), 'orientation-conditional scale expression')
  assert.ok(scale.includes('min(1024,iw)') && scale.includes('min(1024,ih)'), 'both edges bounded')
})

// ── prepareJudgeImages ───────────────────────────────────────────────────

function makeStub({ succeed = true, files = {} } = {}) {
  const calls = []
  const exec = (command, args) => {
    calls.push([command, args])
    return succeed ? { status: 0 } : { status: null, error: new Error('ENOENT') }
  }
  const readFile = (filePath) => {
    if (!(filePath in files)) throw new Error(`no such file: ${filePath}`)
    return files[filePath]
  }
  return { exec, readFile, calls }
}

test('prepareJudgeImages: zero stills -> {imageBlocks:[], caveats:[]} without invoking exec', () => {
  const stub = makeStub()
  const result = prepareJudgeImages({
    stills: [],
    localDir: '/local',
    exec: stub.exec,
    readFile: stub.readFile,
  })
  assert.deepEqual(result, { imageBlocks: [], caveats: [] })
  assert.equal(stub.calls.length, 0)
})

test('prepareJudgeImages: success path reads the downscaled output and base64-encodes it', () => {
  const downscaled = Buffer.from('downscaled-bytes')
  const stub = makeStub({
    succeed: true,
    files: { '/local/judge/a.png': downscaled },
  })
  const result = prepareJudgeImages({
    stills: [{ fileName: 'a.png', label: 'A', sceneId: 'scene-01', surface: 'tui' }],
    localDir: '/local',
    exec: stub.exec,
    readFile: stub.readFile,
  })
  assert.equal(stub.calls.length, 1)
  const [command, args] = stub.calls[0]
  assert.equal(command, 'ffmpeg')
  assert.ok(args.includes('/local/a.png'))
  assert.ok(args.includes('/local/judge/a.png'))
  assert.deepEqual(result, {
    imageBlocks: [
      {
        type: 'image',
        source: {
          type: 'base64',
          media_type: 'image/png',
          data: downscaled.toString('base64'),
        },
      },
    ],
    caveats: [],
  })
})

test('prepareJudgeImages: failing exec + small original (<=400KB) -> falls back to original bytes', () => {
  const original = Buffer.alloc(1024, 1) // 1KB, well under the 400KB threshold
  const stub = makeStub({
    succeed: false,
    files: { '/local/a.png': original },
  })
  const result = prepareJudgeImages({
    stills: [{ fileName: 'a.png', label: 'A', sceneId: 'scene-01', surface: 'tui' }],
    localDir: '/local',
    exec: stub.exec,
    readFile: stub.readFile,
  })
  assert.deepEqual(result, {
    imageBlocks: [
      {
        type: 'image',
        source: {
          type: 'base64',
          media_type: 'image/png',
          data: original.toString('base64'),
        },
      },
    ],
    caveats: [],
  })
})

test('prepareJudgeImages: failing exec + large original (>400KB) -> dropped with a caveat', () => {
  const original = Buffer.alloc(400 * 1024 + 1, 1) // 1 byte over the threshold
  const stub = makeStub({
    succeed: false,
    files: { '/local/big.png': original },
  })
  const result = prepareJudgeImages({
    stills: [{ fileName: 'big.png', label: 'Big', sceneId: 'scene-01', surface: 'tui' }],
    localDir: '/local',
    exec: stub.exec,
    readFile: stub.readFile,
  })
  assert.deepEqual(result, {
    imageBlocks: [],
    caveats: ['still big.png omitted (downscale unavailable)'],
  })
})

test('prepareJudgeImages: original exactly at the 400KB threshold is kept, not dropped', () => {
  const original = Buffer.alloc(400 * 1024, 1) // exactly the threshold
  const stub = makeStub({
    succeed: false,
    files: { '/local/edge.png': original },
  })
  const result = prepareJudgeImages({
    stills: [{ fileName: 'edge.png', label: 'Edge', sceneId: 'scene-01', surface: 'tui' }],
    localDir: '/local',
    exec: stub.exec,
    readFile: stub.readFile,
  })
  assert.equal(result.imageBlocks.length, 1)
  assert.deepEqual(result.caveats, [])
})

test('prepareJudgeImages: mixed stills -> success, fallback, and drop each handled independently', () => {
  const downscaled = Buffer.from('small-downscaled')
  const smallOriginal = Buffer.alloc(10, 2)
  const largeOriginal = Buffer.alloc(400 * 1024 + 10, 3)
  const calls = []
  const exec = (command, args) => {
    calls.push(args)
    const output = args[args.length - 1]
    // Only the first still's ffmpeg invocation succeeds.
    return output === '/local/judge/ok.png'
      ? { status: 0 }
      : { status: null, error: new Error('x') }
  }
  const files = {
    '/local/judge/ok.png': downscaled,
    '/local/small.png': smallOriginal,
    '/local/large.png': largeOriginal,
  }
  const readFile = (filePath) => {
    if (!(filePath in files)) throw new Error(`no such file: ${filePath}`)
    return files[filePath]
  }
  const result = prepareJudgeImages({
    stills: [
      { fileName: 'ok.png', label: 'OK', sceneId: 'scene-01', surface: 'tui' },
      { fileName: 'small.png', label: 'Small', sceneId: 'scene-01', surface: 'tui' },
      { fileName: 'large.png', label: 'Large', sceneId: 'scene-01', surface: 'tui' },
    ],
    localDir: '/local',
    exec,
    readFile,
  })
  assert.equal(calls.length, 3)
  assert.equal(result.imageBlocks.length, 2)
  assert.deepEqual(result.imageBlocks[0].source.data, downscaled.toString('base64'))
  assert.deepEqual(result.imageBlocks[1].source.data, smallOriginal.toString('base64'))
  assert.deepEqual(result.caveats, ['still large.png omitted (downscale unavailable)'])
})

test('prepareJudgeImages: exec throwing is treated as a failure, not an unhandled exception', () => {
  const original = Buffer.alloc(10, 4)
  const exec = () => {
    throw new Error('ffmpeg not found')
  }
  const readFile = (filePath) => {
    if (filePath === '/local/a.png') return original
    throw new Error(`no such file: ${filePath}`)
  }
  const result = prepareJudgeImages({
    stills: [{ fileName: 'a.png', label: 'A', sceneId: 'scene-01', surface: 'tui' }],
    localDir: '/local',
    exec,
    readFile,
  })
  assert.equal(result.imageBlocks.length, 1)
  assert.deepEqual(result.caveats, [])
})

test('prepareJudgeImages: default exec/readFile seams exist (not required in call)', () => {
  // Zero stills never touches the default seams, so this exercises that the
  // function is callable without exec/readFile at all.
  const result = prepareJudgeImages({ stills: [], localDir: '/local' })
  assert.deepEqual(result, { imageBlocks: [], caveats: [] })
})

// ── judgeProof (Task 3) ──────────────────────────────────────────────────

function makeModelStub({ response, error } = {}) {
  const calls = []
  const model = {
    async createMessage(args) {
      calls.push(args)
      if (error) throw error
      return response
    },
  }
  return { model, calls }
}

function rawVerdictJson(overrides = {}) {
  return JSON.stringify({
    evidence: 'satisfactory',
    confidence: 'high',
    perScene: [],
    caveats: [],
    ...overrides,
  })
}

const noOpPrepareImages = () => ({ imageBlocks: [], caveats: [] })

test('judgeProof: happy path returns a clamped verdict carrying the model name', async () => {
  const { model, calls } = makeModelStub({
    response: { content: [{ type: 'text', text: rawVerdictJson() }] },
  })
  const surfaceRuns = [
    {
      surface: 'tui',
      brief: { title: 'x', planRequiredProof: ['keystroke navigation visible'] },
      agentResult: { summary: 'agent did the thing' },
      captureShapes: [
        {
          surface: 'tui',
          status: 'passed',
          stills: [{ fileName: 'a.png', label: 'a' }],
          scenes: [{ id: 'scene-01', title: 'x', passed: true, missing: [], outputMs: 1000 }],
        },
      ],
    },
  ]
  const manifest = { agentRunnerStubbed: false, surfaces: [{ surface: 'tui', outcome: 'passed' }] }
  const result = await judgeProof({
    surfaceRuns,
    manifest,
    localDir: '/local',
    deps: { model, env: { PROOF_ANTHROPIC_API_KEY: 'sk-test' }, prepareImages: noOpPrepareImages },
  })
  assert.equal(result.unjudged, undefined)
  assert.equal(result.evidence, 'satisfactory')
  assert.equal(result.confidence, 'high')
  assert.equal(result.model, DEFAULT_JUDGE_MODEL)
  assert.deepEqual(result.clamped, [])
  assert.equal(calls.length, 1)
  assert.equal(calls[0].model, DEFAULT_JUDGE_MODEL)
})

test('judgeProof: model.createMessage throws -> unjudged with the error message as reason', async () => {
  const { model } = makeModelStub({ error: new Error('network down') })
  const result = await judgeProof({
    surfaceRuns: [
      {
        surface: 'tui',
        brief: {},
        agentResult: {},
        captureShapes: [
          { surface: 'tui', status: 'passed', stills: [{ fileName: 'a.png', label: 'a' }] },
        ],
      },
    ],
    manifest: { surfaces: [] },
    localDir: '/local',
    deps: { model, env: { PROOF_ANTHROPIC_API_KEY: 'sk-test' }, prepareImages: noOpPrepareImages },
  })
  assert.deepEqual(result, { unjudged: true, reason: 'network down' })
})

test('judgeProof: missing PROOF_ANTHROPIC_API_KEY -> unjudged without constructing the SDK', async () => {
  const calls = []
  const model = {
    async createMessage(args) {
      calls.push(args)
      return { content: [{ type: 'text', text: rawVerdictJson() }] }
    },
  }
  const result = await judgeProof({
    surfaceRuns: [{ surface: 'tui', brief: {}, agentResult: {}, captureShapes: [] }],
    manifest: {},
    localDir: '/local',
    deps: { model, env: {} },
  })
  assert.deepEqual(result, { unjudged: true, reason: 'missing-key' })
  assert.equal(calls.length, 0, 'model.createMessage must never be called on a missing key')
})

test('judgeProof: unparsable model response text -> unjudged', async () => {
  const { model } = makeModelStub({
    response: { content: [{ type: 'text', text: 'not json{{' }] },
  })
  const result = await judgeProof({
    surfaceRuns: [{ surface: 'tui', brief: {}, agentResult: {}, captureShapes: [] }],
    manifest: {},
    localDir: '/local',
    deps: { model, env: { PROOF_ANTHROPIC_API_KEY: 'sk-test' }, prepareImages: noOpPrepareImages },
  })
  assert.equal(result.unjudged, true)
  assert.ok(typeof result.reason === 'string' && result.reason.length > 0)
})

test('judgeProof: manifest.surfaces carrying a deferred entry triggers the mechanical-failure clamp', async () => {
  const { model } = makeModelStub({
    response: { content: [{ type: 'text', text: rawVerdictJson() }] },
  })
  const result = await judgeProof({
    surfaceRuns: [
      {
        surface: 'web',
        brief: {},
        agentResult: {},
        captureShapes: [
          { surface: 'web', status: 'passed', stills: [{ fileName: 'a.png', label: 'a' }] },
        ],
      },
    ],
    manifest: { surfaces: [{ surface: 'tui', outcome: 'deferred' }] },
    localDir: '/local',
    deps: { model, env: { PROOF_ANTHROPIC_API_KEY: 'sk-test' }, prepareImages: noOpPrepareImages },
  })
  assert.equal(result.evidence, 'partial')
  assert.ok(result.clamped.includes('evidence-downgraded-scene-failure'))
})

test('judgeProof: a failed captureShape.scenes entry triggers the mechanical-failure clamp', async () => {
  const { model } = makeModelStub({
    response: { content: [{ type: 'text', text: rawVerdictJson() }] },
  })
  const result = await judgeProof({
    surfaceRuns: [
      {
        surface: 'tui',
        brief: {},
        agentResult: {},
        captureShapes: [
          {
            surface: 'tui',
            status: 'passed',
            stills: [{ fileName: 'a.png', label: 'a' }],
            scenes: [{ id: 'scene-01', title: 'x', passed: false, missing: ['x'], outputMs: 1000 }],
          },
        ],
      },
    ],
    manifest: { surfaces: [{ surface: 'tui', outcome: 'passed' }] },
    localDir: '/local',
    deps: { model, env: { PROOF_ANTHROPIC_API_KEY: 'sk-test' }, prepareImages: noOpPrepareImages },
  })
  assert.equal(result.evidence, 'partial')
})

test('judgeProof: honors the BOSS_PROOF_JUDGE_MODEL env override', async () => {
  const { model, calls } = makeModelStub({
    response: { content: [{ type: 'text', text: rawVerdictJson() }] },
  })
  const result = await judgeProof({
    surfaceRuns: [{ surface: 'tui', brief: {}, agentResult: {}, captureShapes: [] }],
    manifest: {},
    localDir: '/local',
    deps: {
      model,
      env: { PROOF_ANTHROPIC_API_KEY: 'sk-test', BOSS_PROOF_JUDGE_MODEL: 'claude-sonnet-4-6' },
      prepareImages: noOpPrepareImages,
    },
  })
  assert.equal(result.model, 'claude-sonnet-4-6')
  assert.equal(calls[0].model, 'claude-sonnet-4-6')
})

test('judgeProof: default prepareImages seam works with zero stills (no fs access needed)', async () => {
  const { model } = makeModelStub({
    response: { content: [{ type: 'text', text: rawVerdictJson() }] },
  })
  const result = await judgeProof({
    surfaceRuns: [{ surface: 'tui', brief: {}, agentResult: {}, captureShapes: [] }],
    manifest: {},
    localDir: '/local',
    deps: { model, env: { PROOF_ANTHROPIC_API_KEY: 'sk-test' } },
  })
  assert.equal(result.evidence, 'satisfactory')
})

test('judgeProof: image caveats from prepareImages are appended to the verdict caveats', async () => {
  const { model } = makeModelStub({
    response: { content: [{ type: 'text', text: rawVerdictJson() }] },
  })
  const result = await judgeProof({
    surfaceRuns: [
      {
        surface: 'tui',
        brief: {},
        agentResult: {},
        captureShapes: [
          { surface: 'tui', status: 'passed', stills: [{ fileName: 'big.png', label: 'big' }] },
        ],
      },
    ],
    manifest: {},
    localDir: '/local',
    deps: {
      model,
      env: { PROOF_ANTHROPIC_API_KEY: 'sk-test' },
      prepareImages: () => ({
        imageBlocks: [],
        caveats: ['still big.png omitted (downscale unavailable)'],
      }),
    },
  })
  assert.ok(result.caveats.includes('still big.png omitted (downscale unavailable)'))
})

// ── BOS-142: manifest-flag → clamp table (all-live / all-stub / mixed) ────────
//
// The run-level `manifest.agentRunnerStubbed` (computed upstream by
// capturesAgentRunnerStubbed) is the ONLY thing judgeProof reads for the stub
// caveat + confidence gate. An all-live run (flag unset/false) reaches High with
// NO stub caveat; a stubbed run (flag true) always carries the disclosure caveat.

function liveScene(id) {
  return { id, title: id, passed: true, missing: [], outputMs: 1000, agentRunnerStubbed: false }
}
function stubScene(id) {
  return { id, title: id, passed: true, missing: [], outputMs: 1000 }
}

async function judgeWith(manifest, scenes) {
  const { model } = makeModelStub({
    response: { content: [{ type: 'text', text: rawVerdictJson({ confidence: 'high' }) }] },
  })
  return judgeProof({
    surfaceRuns: [
      {
        surface: 'web',
        brief: { title: 'x' },
        agentResult: { summary: 'ok' },
        captureShapes: [
          { surface: 'web', status: 'passed', stills: [{ fileName: 'a.png', label: 'a' }], scenes },
        ],
      },
    ],
    manifest,
    localDir: '/local',
    deps: { model, env: { PROOF_ANTHROPIC_API_KEY: 'sk-test' }, prepareImages: noOpPrepareImages },
  })
}

test('judgeProof clamp table: all-live (manifest flag false) → High, NO stub caveat', async () => {
  const result = await judgeWith(
    { agentRunnerStubbed: false, surfaces: [{ surface: 'web', outcome: 'passed' }] },
    [liveScene('scene-01'), liveScene('scene-02')],
  )
  assert.equal(result.confidence, 'high')
  assert.ok(!result.caveats.some((c) => /stub/i.test(c)), 'no stub caveat on an all-live run')
  assert.ok(!result.clamped.includes('stub-caveat-appended'))
})

test('judgeProof clamp table: all-stub (manifest flag true) → caveat appended (High still reachable, disclosed)', async () => {
  const result = await judgeWith(
    { agentRunnerStubbed: true, surfaces: [{ surface: 'web', outcome: 'passed' }] },
    [stubScene('scene-01'), stubScene('scene-02')],
  )
  assert.ok(
    result.caveats.some((c) => /stub/i.test(c)),
    'stub caveat present on all-stub run',
  )
  assert.ok(result.clamped.includes('stub-caveat-appended'))
  assert.equal(result.confidence, 'high')
})

test('judgeProof clamp table: mixed (manifest flag true) → stub caveat still appended', async () => {
  const result = await judgeWith(
    { agentRunnerStubbed: true, surfaces: [{ surface: 'web', outcome: 'passed' }] },
    [liveScene('scene-01'), stubScene('scene-02')],
  )
  assert.ok(
    result.caveats.some((c) => /stub/i.test(c)),
    'a mixed run stays disclosed as stubbed',
  )
  assert.ok(result.clamped.includes('stub-caveat-appended'))
})

test('judgeProof: dedupes planRequiredProof bullets across surfaces before scoping them into the prompt', async () => {
  let capturedContent = null
  const model = {
    async createMessage(args) {
      capturedContent = args.messages[0].content[0].text
      return { content: [{ type: 'text', text: rawVerdictJson() }] }
    },
  }
  const surfaceRuns = [
    {
      surface: 'tui',
      brief: { planRequiredProof: ['keystroke navigation visible', 'no console errors'] },
      agentResult: {},
      captureShapes: [],
    },
    {
      surface: 'web',
      brief: { planRequiredProof: ['no console errors', 'form submits and shows confirmation'] },
      agentResult: {},
      captureShapes: [],
    },
  ]
  await judgeProof({
    surfaceRuns,
    manifest: {},
    localDir: '/local',
    deps: { model, env: { PROOF_ANTHROPIC_API_KEY: 'sk-test' }, prepareImages: noOpPrepareImages },
  })
  const occurrences = capturedContent.split('no console errors').length - 1
  assert.equal(occurrences, 1, 'a bullet shared by two surfaces must appear once, not duplicated')
  assert.ok(capturedContent.includes('keystroke navigation visible'))
  assert.ok(capturedContent.includes('form submits and shows confirmation'))
})

// ── createJudgeModel (Task 3) ────────────────────────────────────────────

test('constants: DEFAULT_JUDGE_TIMEOUT_MS is 120000', () => {
  assert.equal(DEFAULT_JUDGE_TIMEOUT_MS, 120_000)
})

test('createJudgeModel: aborted SDK call THROWS (unlike the driver, which degrades to a no-op)', async () => {
  const origTimeout = process.env.BOSS_PROOF_JUDGE_TIMEOUT_MS
  process.env.BOSS_PROOF_JUDGE_TIMEOUT_MS = '50'
  try {
    class HangingAnthropic {
      constructor() {
        this.messages = {
          create: (_body, { signal } = {}) =>
            new Promise((_, reject) => {
              if (signal?.aborted) {
                reject(new Error('aborted'))
                return
              }
              signal?.addEventListener('abort', () => reject(new Error('aborted')))
            }),
        }
      }
    }
    const model = createJudgeModel({ importer: async () => ({ default: HangingAnthropic }) })
    await assert.rejects(
      model.createMessage({
        model: 'claude-haiku-4-5',
        system: 'test',
        messages: [{ role: 'user', content: 'hi' }],
        maxTokens: 100,
      }),
    )
  } finally {
    if (origTimeout === undefined) delete process.env.BOSS_PROOF_JUDGE_TIMEOUT_MS
    else process.env.BOSS_PROOF_JUDGE_TIMEOUT_MS = origTimeout
  }
})

test('createJudgeModel: forwards PROOF_ANTHROPIC_API_KEY, the JUDGE_SCHEMA structured output, and signal as a request option', async () => {
  const origKey = process.env.PROOF_ANTHROPIC_API_KEY
  process.env.PROOF_ANTHROPIC_API_KEY = 'sk-proof-test-key'
  try {
    let ctorOpts = null
    let createArgs = null
    class RecordingAnthropic {
      constructor(opts) {
        ctorOpts = opts
        this.messages = {
          create: (body, options) => {
            createArgs = { body, options }
            return Promise.resolve({ content: [{ type: 'text', text: '{}' }] })
          },
        }
      }
    }
    const model = createJudgeModel({ importer: async () => ({ default: RecordingAnthropic }) })
    await model.createMessage({
      model: 'claude-haiku-4-5',
      system: 'test',
      messages: [{ role: 'user', content: 'hi' }],
      maxTokens: 100,
    })
    assert.equal(ctorOpts?.apiKey, 'sk-proof-test-key')
    assert.equal(createArgs.body.signal, undefined, 'signal must not appear in the request body')
    assert.ok(createArgs.options?.signal instanceof AbortSignal, 'signal must be a request option')
    assert.deepEqual(createArgs.body.output_config, {
      format: { type: 'json_schema', schema: JUDGE_SCHEMA },
    })
  } finally {
    if (origKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY
    else process.env.PROOF_ANTHROPIC_API_KEY = origKey
  }
})

// ── proof-invalid label (BOS-141 D12, Task 5) ───────────────────────────────

test('ensureLabelCommand: exact argv', () => {
  assert.deepEqual(ensureLabelCommand(), [
    'gh',
    [
      'label',
      'create',
      'proof-invalid',
      '--color',
      'd93f0b',
      '--description',
      'proof judged not convincing',
      '--force',
    ],
  ])
})

test('addLabelCommand: exact argv', () => {
  assert.deepEqual(addLabelCommand({ prNumber: '123' }), [
    'gh',
    ['pr', 'edit', '123', '--add-label', 'proof-invalid'],
  ])
})

test('addLabelCommand: coerces a numeric prNumber to a string', () => {
  assert.deepEqual(addLabelCommand({ prNumber: 123 }), [
    'gh',
    ['pr', 'edit', '123', '--add-label', 'proof-invalid'],
  ])
})

test('removeLabelCommand: exact argv', () => {
  assert.deepEqual(removeLabelCommand({ prNumber: '123' }), [
    'gh',
    ['pr', 'edit', '123', '--remove-label', 'proof-invalid'],
  ])
})

test('removeLabelCommand: coerces a numeric prNumber to a string', () => {
  assert.deepEqual(removeLabelCommand({ prNumber: 123 }), [
    'gh',
    ['pr', 'edit', '123', '--remove-label', 'proof-invalid'],
  ])
})

// ── labelActionForJudge ──────────────────────────────────────────────────

test('labelActionForJudge: unsatisfactory evidence -> add', () => {
  assert.equal(
    labelActionForJudge({
      judge: { evidence: 'unsatisfactory' },
      shouldUpload: true,
      prNumber: '123',
    }),
    'add',
  )
})

test('labelActionForJudge: satisfactory evidence -> remove', () => {
  assert.equal(
    labelActionForJudge({
      judge: { evidence: 'satisfactory' },
      shouldUpload: true,
      prNumber: '123',
    }),
    'remove',
  )
})

test('labelActionForJudge: partial evidence -> remove', () => {
  assert.equal(
    labelActionForJudge({
      judge: { evidence: 'partial' },
      shouldUpload: true,
      prNumber: '123',
    }),
    'remove',
  )
})

test('labelActionForJudge: unjudged verdict -> remove', () => {
  assert.equal(
    labelActionForJudge({
      judge: { unjudged: true, reason: 'missing-key' },
      shouldUpload: true,
      prNumber: '123',
    }),
    'remove',
  )
})

test('labelActionForJudge: null judge -> remove', () => {
  assert.equal(labelActionForJudge({ judge: null, shouldUpload: true, prNumber: '123' }), 'remove')
})

test('labelActionForJudge: undefined judge -> remove', () => {
  assert.equal(labelActionForJudge({ shouldUpload: true, prNumber: '123' }), 'remove')
})

test('labelActionForJudge: shouldUpload false -> null even when unsatisfactory', () => {
  assert.equal(
    labelActionForJudge({
      judge: { evidence: 'unsatisfactory' },
      shouldUpload: false,
      prNumber: '123',
    }),
    null,
  )
})

test('labelActionForJudge: prNumber "local" -> null even when unsatisfactory', () => {
  assert.equal(
    labelActionForJudge({
      judge: { evidence: 'unsatisfactory' },
      shouldUpload: true,
      prNumber: 'local',
    }),
    null,
  )
})

test('labelActionForJudge: falsy prNumber -> null even when unsatisfactory', () => {
  assert.equal(
    labelActionForJudge({
      judge: { evidence: 'unsatisfactory' },
      shouldUpload: true,
      prNumber: undefined,
    }),
    null,
  )
  assert.equal(
    labelActionForJudge({
      judge: { evidence: 'unsatisfactory' },
      shouldUpload: true,
      prNumber: '',
    }),
    null,
  )
})

// ── BOS-251: surface-appropriateness framing ─────────────────────────────────

test('buildJudgePrompt: scopes judging to what the capture surface can display', async () => {
  const { buildJudgePrompt } = await import('./proof-judge.mjs')
  const { content } = buildJudgePrompt({
    requiredProof: { unscoped: ['unit tests pass with PASS output'] },
    surfaces: ['tui'],
  })
  const text = content[0].text
  assert.match(text, /Judge ONLY what this capture surface can display/)
  assert.match(text, /do not count\nit as missing evidence/)
})
