#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  PROOF_MEDIA_TYPES,
  TUI_SURFACE_PREFIXES,
  WEB_UI_SURFACE_PREFIXES,
  buildManifest,
  bossE2eBuildCommand,
  browserCaptureCommand,
  classifyTuiSurface,
  deriveVerdictBlock,
  formatCaption,
  githubCommentCommand,
  introCardCommand,
  listProofCommentsCommand,
  mediaTypeForPath,
  minimizeCommentCommand,
  normalizeChangedFiles,
  normalizeRecipe,
  orderCapturesForReport,
  parseProofArgs,
  proofAncestorDirs,
  proofCommentMarker,
  proofRunPaths,
  proofUploadFiles,
  r2UploadCommand,
  renderComment,
  renderDeferredComment,
  renderGallery,
  resolveCatalogPath,
  selectOutdatedProofCommentIds,
  selectRecipes,
  trimTerminalBlankLines,
  terminalRenderCommand,
  terminalRenderManifestCommand,
  captionStripRenderCommand,
  tuiAgentBridgeBuildCommand,
  validateBrowserRoute,
  validateProofUploadRelativePath,
  validateRecipeId,
  webUiSurfacePresent,
} from './proof-lib.mjs'

const catalog = {
  version: 1,
  recipes: [
    { id: 'tui-home', surface: 'tui', title: 'TUI Home', privacy: 'fixture' },
    { id: 'web-sessions', surface: 'web', title: 'Web Sessions', privacy: 'fixture' },
    { id: 'marketing-home', surface: 'marketing', title: 'Marketing Home', privacy: 'fixture' },
  ],
  pathRules: [
    { name: 'TUI', patterns: ['services/boss/internal/views/'], recipeIds: ['tui-home'] },
    { name: 'Web', patterns: ['services/web/'], recipeIds: ['web-sessions'] },
    { name: 'Marketing', patterns: ['services/marketing/'], recipeIds: ['marketing-home'] },
  ],
}

test('normalizeChangedFiles trims blanks and strips leading dot slash', () => {
  assert.deepEqual(normalizeChangedFiles([' ./services/web/src/App.tsx ', '', null]), [
    'services/web/src/App.tsx',
  ])
})

test('selectRecipes maps changed paths to unique recipes in catalog order', () => {
  const selected = selectRecipes(catalog, [
    'services/web/src/App.tsx',
    'services/boss/internal/views/home.go',
    'services/web/src/pages/Sessions.tsx',
  ])

  assert.deepEqual(
    selected.map((recipe) => recipe.id),
    ['tui-home', 'web-sessions'],
  )
})

test('selectRecipes honors explicit recipe ids before diff rules', () => {
  const selected = selectRecipes(catalog, ['services/web/src/App.tsx'], ['marketing-home'])

  assert.deepEqual(
    selected.map((recipe) => recipe.id),
    ['marketing-home'],
  )
})

test('selectRecipes returns empty array when no visual paths match', () => {
  assert.deepEqual(selectRecipes(catalog, ['services/bossd/internal/server/server.go']), [])
})

test('selectRecipes rejects unknown recipe ids from matching path rules', () => {
  const invalidCatalog = {
    ...catalog,
    pathRules: [{ name: 'Broken', patterns: ['services/web/'], recipeIds: ['missing-recipe'] }],
  }

  assert.throws(
    () => selectRecipes(invalidCatalog, ['services/web/src/App.tsx']),
    /unknown proof recipe: missing-recipe/,
  )
})

test('formatCaption mirrors the TS overlay: collapse, 140-truncation, passthrough', () => {
  // Collapse multi-line / multi-space narration to a single line.
  assert.equal(formatCaption('  open\n  the   dashboard '), 'open the dashboard')
  // Truncate overly long narration with a U+2026 ellipsis to exactly 140 chars.
  const out = formatCaption('x'.repeat(200))
  assert.equal(out.length, 140)
  assert.equal(out.endsWith('…'), true)
  assert.equal(out, `${'x'.repeat(139)}…`)
  // A caption exactly at the 140 boundary is returned unchanged (no ellipsis).
  const exact = 'y'.repeat(140)
  assert.equal(formatCaption(exact), exact)
  // Empty / nullish input passes through to ''.
  assert.equal(formatCaption(''), '')
  assert.equal(formatCaption('   '), '')
  assert.equal(formatCaption(null), '')
  assert.equal(formatCaption(undefined), '')
})

test('buildManifest records commit, recipe status, and public base url', () => {
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '596',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.bossanova.dev/proof/repo/pr-596/abc1234/run-1',
    captures: [
      {
        recipeId: 'web-sessions',
        title: 'Web Sessions',
        surface: 'web',
        privacy: 'fixture',
        status: 'passed',
        fileName: 'web-sessions.png',
      },
    ],
  })

  assert.equal(manifest.commit, 'abc1234')
  assert.equal(
    manifest.captures[0].url,
    'https://proof.bossanova.dev/proof/repo/pr-596/abc1234/run-1/web-sessions.png',
  )
})

test('buildManifest encodes file name path segments without encoding slashes', () => {
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '596',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.bossanova.dev/proof/repo/pr-596/abc1234/run-1',
    captures: [
      {
        recipeId: 'web-sessions',
        title: 'Web Sessions',
        surface: 'web',
        privacy: 'fixture',
        status: 'passed',
        fileName: 'web-sessions/web sessions.png',
      },
    ],
  })

  assert.equal(
    manifest.captures[0].url,
    'https://proof.bossanova.dev/proof/repo/pr-596/abc1234/run-1/web-sessions/web%20sessions.png',
  )
})

test('proof comment upsert helpers build expected gh commands', () => {
  assert.equal(proofCommentMarker('597'), '<!-- bossanova-proof:pr-597 -->')
  assert.deepEqual(listProofCommentsCommand({ prNumber: '597' }), [
    'gh',
    ['pr', 'view', '597', '--json', 'comments'],
  ])
  const [bin, args] = minimizeCommentCommand({ commentId: 'IC_abc' })
  assert.equal(bin, 'gh')
  assert.deepEqual(args.slice(0, 3), ['api', 'graphql', '-f'])
  assert.match(args[3], /minimizeComment\(input:\{subjectId:\$id,classifier:OUTDATED\}\)/)
  assert.deepEqual(args.slice(4), ['-f', 'id=IC_abc'])
})

test('selectOutdatedProofCommentIds returns only visible marker comments', () => {
  const marker = proofCommentMarker('597')
  const commentsJson = JSON.stringify({
    comments: [
      { id: 'IC_keep_visible', body: `proof ${marker}`, isMinimized: false },
      { id: 'IC_already_hidden', body: `proof ${marker}`, isMinimized: true },
      { id: 'IC_other_comment', body: 'unrelated review note', isMinimized: false },
      { id: 'IC_second_visible', body: `${marker}\nrun 2`, isMinimized: false },
    ],
  })
  assert.deepEqual(selectOutdatedProofCommentIds({ commentsJson, marker }), [
    'IC_keep_visible',
    'IC_second_visible',
  ])
  // Accepts a pre-parsed object and tolerates malformed input.
  assert.deepEqual(selectOutdatedProofCommentIds({ commentsJson: { comments: [] }, marker }), [])
  assert.deepEqual(selectOutdatedProofCommentIds({ commentsJson: 'not json', marker }), [])
})

test('trimTerminalBlankLines crops blank edges but keeps internal blanks', () => {
  const screen = ['', '', '  Settings', '', '  Worktree base dir: /x', '', '', ''].join('\n')
  assert.equal(
    trimTerminalBlankLines(screen),
    ['  Settings', '', '  Worktree base dir: /x'].join('\n'),
  )
  // Whitespace-only lines count as blank; content is otherwise untouched.
  assert.equal(trimTerminalBlankLines('   \n\tbody\n   '), '\tbody')
  assert.equal(trimTerminalBlankLines(''), '')
  assert.equal(trimTerminalBlankLines('   \n  \n'), '')
})

test('parseProofArgs parses run command with explicit recipes', () => {
  assert.deepEqual(parseProofArgs(['run', '--recipe', 'web-sessions', '--recipe', 'tui-home']), {
    command: 'run',
    recipes: ['web-sessions', 'tui-home'],
    changedFiles: [],
    dryRun: false,
  })
})

test('parseProofArgs parses changed files and dry run', () => {
  assert.deepEqual(
    parseProofArgs(['plan', '--changed-file', 'services/web/src/App.tsx', '--dry-run']),
    {
      command: 'plan',
      recipes: [],
      changedFiles: ['services/web/src/App.tsx'],
      dryRun: true,
    },
  )
})

test('parseProofArgs defaults flags-only invocation to run command', () => {
  assert.deepEqual(parseProofArgs(['--changed-file', 'services/web/src/App.tsx', '--dry-run']), {
    command: 'run',
    recipes: [],
    changedFiles: ['services/web/src/App.tsx'],
    dryRun: true,
  })
})

test('parseProofArgs rejects recipe flag without value', () => {
  assert.throws(() => parseProofArgs(['run', '--recipe']), /--recipe requires a value/)
  assert.throws(() => parseProofArgs(['run', '--recipe', '--dry-run']), /--recipe requires a value/)
})

test('parseProofArgs rejects unknown arguments', () => {
  assert.throws(() => parseProofArgs(['run', '--unknown']), /unknown proof argument: --unknown/)
})

test('terminalRenderCommand runs through services/web playwright dependency', () => {
  assert.deepEqual(
    terminalRenderCommand({
      input: '.proof/tui-home/screen.txt',
      output: '.proof/tui-home/tui-home.png',
      title: 'TUI Home',
    }),
    [
      'pnpm',
      [
        '--dir',
        'services/web',
        'exec',
        'node',
        '../../scripts/proof-render-terminal.mjs',
        '--input',
        '.proof/tui-home/screen.txt',
        '--output',
        '.proof/tui-home/tui-home.png',
        '--title',
        'TUI Home',
      ],
    ],
  )
})

test('terminalRenderManifestCommand renders a batch via --manifest through services/web', () => {
  assert.deepEqual(
    terminalRenderManifestCommand({ manifest: '.proof/tui/.render-manifest.json' }),
    [
      'pnpm',
      [
        '--dir',
        'services/web',
        'exec',
        'node',
        '../../scripts/proof-render-terminal.mjs',
        '--manifest',
        '.proof/tui/.render-manifest.json',
      ],
    ],
  )
})

test('captionStripRenderCommand renders a width-sized strip in --strip mode through services/web', () => {
  assert.deepEqual(
    captionStripRenderCommand({
      caption: 'Opening cron list',
      width: 1120,
      output: '.proof/tui/caption-strip-0.png',
    }),
    [
      'pnpm',
      [
        '--dir',
        'services/web',
        'exec',
        'node',
        '../../scripts/proof-render-terminal.mjs',
        '--strip',
        '--width',
        '1120',
        '--output',
        '.proof/tui/caption-strip-0.png',
        '--caption',
        'Opening cron list',
      ],
    ],
  )
})

test('captionStripRenderCommand omits --caption for an empty caption (byte-compatible no-text strip)', () => {
  const [, args] = captionStripRenderCommand({ caption: '', width: 800, output: 'out.png' })
  assert.ok(!args.includes('--caption'), 'no --caption flag when caption is empty')
  assert.ok(args.includes('--strip'))
  assert.deepEqual(args.slice(-4), ['--width', '800', '--output', 'out.png'])
})

test('tuiAgentBridgeBuildCommand builds proof-tui-agent bridge with e2e tags', () => {
  assert.deepEqual(tuiAgentBridgeBuildCommand({ outBin: '/tmp/proof-tui-bridge' }), [
    'go',
    ['build', '-tags', 'e2e', '-o', '/tmp/proof-tui-bridge', './cmd/proof-tui-agent'],
    { cwd: 'services/boss' },
  ])
})

test('bossE2eBuildCommand builds boss e2e binary with e2e tags', () => {
  assert.deepEqual(bossE2eBuildCommand({ outBin: '/tmp/boss-e2e' }), [
    'go',
    ['build', '-tags', 'e2e', '-o', '/tmp/boss-e2e', './cmd'],
    { cwd: 'services/boss' },
  ])
})

test('browserCaptureCommand runs web proof through services/web dependencies', () => {
  assert.deepEqual(
    browserCaptureCommand({
      surface: 'web',
      recipePath: '.proof/web/recipe.json',
      outputDir: '.proof/web',
    }),
    [
      'pnpm',
      [
        '--dir',
        'services/web',
        'exec',
        'node',
        '../../scripts/proof-playwright-runner.mjs',
        '--surface',
        'web',
        '--recipe',
        '../../.proof/web/recipe.json',
        '--output-dir',
        '../../.proof/web',
      ],
    ],
  )
})

test('browserCaptureCommand runs marketing proof through services/marketing dependencies', () => {
  assert.deepEqual(
    browserCaptureCommand({
      surface: 'marketing',
      recipePath: '.proof/m/recipe.json',
      outputDir: '.proof/m',
    }),
    [
      'pnpm',
      [
        '--dir',
        'services/marketing',
        'exec',
        'node',
        '../../scripts/proof-playwright-runner.mjs',
        '--surface',
        'marketing',
        '--recipe',
        '../../.proof/m/recipe.json',
        '--output-dir',
        '../../.proof/m',
      ],
    ],
  )
})

test('browserCaptureCommand runs docs proof through services/docs dependencies', () => {
  assert.deepEqual(
    browserCaptureCommand({
      surface: 'docs',
      recipePath: '.proof/d/recipe.json',
      outputDir: '.proof/d',
    }),
    [
      'pnpm',
      [
        '--dir',
        'services/docs',
        'exec',
        'node',
        '../../scripts/proof-playwright-runner.mjs',
        '--surface',
        'docs',
        '--recipe',
        '../../.proof/d/recipe.json',
        '--output-dir',
        '../../.proof/d',
      ],
    ],
  )
})

// ── classifyTuiSurface: catalog-independent TUI path detection (BOS-115) ──────

test('classifyTuiSurface is true for any changed file under a TUI prefix', () => {
  for (const prefix of TUI_SURFACE_PREFIXES) {
    assert.equal(
      classifyTuiSurface([`${prefix}some/file.go`]),
      true,
      `expected ${prefix}* to classify as TUI`,
    )
  }
})

test('classifyTuiSurface covers the documented TUI prefixes', () => {
  assert.deepEqual(TUI_SURFACE_PREFIXES, [
    'services/boss/internal/views/',
    'services/boss/internal/tuitest/',
    'services/boss/internal/tuidriver/',
    'services/boss/internal/fixtures/',
    'services/boss/internal/client/',
    'services/boss/cmd/',
    'proto/',
  ])
})

test('classifyTuiSurface is false for web/marketing/docs and unrelated paths', () => {
  assert.equal(classifyTuiSurface(['services/web/src/App.tsx']), false)
  assert.equal(classifyTuiSurface(['services/marketing/src/pages/index.astro']), false)
  assert.equal(classifyTuiSurface(['services/docs/docs/guides/mcp.md']), false)
  assert.equal(classifyTuiSurface(['services/bossd/internal/server/server.go']), false)
  assert.equal(classifyTuiSurface(['README.md']), false)
  assert.equal(classifyTuiSurface([]), false)
  assert.equal(classifyTuiSurface(null), false)
})

test('classifyTuiSurface is true when ANY changed file is a TUI path (mixed diff)', () => {
  assert.equal(
    classifyTuiSurface(['services/web/src/App.tsx', 'services/boss/internal/views/home.go']),
    true,
  )
})

// ── webUiSurfacePresent: no-UI-surface pre-gate (BOS-118) ────────────────────

test('webUiSurfacePresent is true for any changed file under a web UI prefix', () => {
  for (const prefix of WEB_UI_SURFACE_PREFIXES) {
    assert.equal(
      webUiSurfacePresent([`${prefix}some/file.tsx`]),
      true,
      `expected ${prefix}* to count as a web UI surface`,
    )
  }
})

test('webUiSurfacePresent covers the documented web UI prefixes', () => {
  assert.deepEqual(WEB_UI_SURFACE_PREFIXES, [
    'services/web/src/',
    'services/web/index.html',
    'services/web/public/',
  ])
})

test('webUiSurfacePresent is false for changes with no demonstrable web surface', () => {
  // The proof scripts themselves (this very PR's shape).
  assert.equal(webUiSurfacePresent(['scripts/proof-agent.mjs', 'scripts/proof.mjs']), false)
  // The web agent/specs are tests, not app UI.
  assert.equal(webUiSurfacePresent(['services/web/tests/e2e/agent/runner.ts']), false)
  // Backend, docs, proto.
  assert.equal(webUiSurfacePresent(['services/bossd/internal/server/server.go']), false)
  assert.equal(webUiSurfacePresent(['docs/plans/BOS-118.md']), false)
  assert.equal(webUiSurfacePresent([]), false)
  assert.equal(webUiSurfacePresent(null), false)
})

test('webUiSurfacePresent is true when ANY changed file is a web UI path (mixed diff)', () => {
  assert.equal(
    webUiSurfacePresent(['scripts/proof.mjs', 'services/web/src/pages/SessionDetail.tsx']),
    true,
  )
})

test('renderDeferredComment carries the honest no-ui-surface reason and no red verdict', () => {
  const body = renderDeferredComment({
    marker: '<!-- m -->',
    manifest: { commit: 'abc1234', prNumber: '964', deferred: true },
    reasonCode: 'no-ui-surface',
  })
  assert.match(body, /No web UI surface to demonstrate/)
  assert.ok(!body.includes('❌'), 'no-surface note must not carry a red verdict')
  assert.ok(!body.includes('Scroll through the page'), 'no recipe-floor filler')
})

test('classifyTuiSurface normalizes leading ./ and backslashes', () => {
  assert.equal(classifyTuiSurface(['./services/boss/cmd/root.go']), true)
  assert.equal(classifyTuiSurface(['services\\boss\\cmd\\root.go']), true)
})

test('proofRunPaths creates stable proof bundle paths', () => {
  assert.deepEqual(proofRunPaths({ prNumber: '596', commit: 'abc1234', runId: 'run-1' }), {
    localDir: '.proof/pr-596/abc1234/run-1',
    publicPrefix: 'proof/bossanova/pr-596/abc1234/run-1',
  })
})

test('proofRunPaths appends an optional random token segment', () => {
  assert.deepEqual(
    proofRunPaths({ prNumber: '596', commit: 'abc1234', runId: 'run-1', token: 'tok-xyz' }),
    {
      localDir: '.proof/pr-596/abc1234/run-1/tok-xyz',
      publicPrefix: 'proof/bossanova/pr-596/abc1234/run-1/tok-xyz',
    },
  )
})

test('r2UploadCommand uses wrangler r2 object put with content type', () => {
  assert.deepEqual(
    r2UploadCommand({
      bucket: 'bossanova-proof-production',
      key: 'proof/bossanova/pr-596/abc/run/a.png',
      file: '.proof/a.png',
      contentType: 'image/png',
    }),
    [
      'pnpm',
      [
        'dlx',
        'wrangler@4.42.0',
        'r2',
        'object',
        'put',
        'bossanova-proof-production/proof/bossanova/pr-596/abc/run/a.png',
        '--file',
        '.proof/a.png',
        '--content-type',
        'image/png',
        '--remote',
      ],
    ],
  )
})

test('githubCommentCommand posts body file with gh pr comment', () => {
  assert.deepEqual(githubCommentCommand({ prNumber: '596', bodyFile: '.proof/comment.md' }), [
    'gh',
    ['pr', 'comment', '596', '--body-file', '.proof/comment.md'],
  ])
})

test('proofUploadFiles selects manifest and all captures with a fileName (passed or failed)', () => {
  // Failed captures with a fileName are now uploaded so Unsatisfactory agent
  // runs remain reviewable. Recipe failures delete their artifacts before
  // buildManifest is called, so fileName is absent for recipe failures in practice.
  assert.deepEqual(
    proofUploadFiles({
      localDir: '/repo/.proof/pr-596/abc/run',
      manifest: {
        captures: [
          { status: 'passed', fileName: 'marketing-home/marketing-home.png' },
          { status: 'failed', fileName: 'tui-home/tui-home.png' },
          { status: 'failed' }, // no fileName → skipped
        ],
      },
    }),
    [
      {
        file: '/repo/.proof/pr-596/abc/run/manifest.json',
        relative: 'manifest.json',
        contentType: 'application/json',
      },
      {
        file: '/repo/.proof/pr-596/abc/run/marketing-home/marketing-home.png',
        relative: 'marketing-home/marketing-home.png',
        contentType: 'image/png',
      },
      {
        file: '/repo/.proof/pr-596/abc/run/tui-home/tui-home.png',
        relative: 'tui-home/tui-home.png',
        contentType: 'image/png',
      },
    ],
  )
})

test('proofUploadFiles rejects traversal and absolute PNG capture paths', () => {
  const base = {
    localDir: '/repo/.proof/pr-596/abc/run',
    manifest: { captures: [{ status: 'passed', fileName: '../escape.png' }] },
  }

  assert.throws(() => proofUploadFiles(base), /invalid proof upload path/)
  assert.throws(
    () =>
      proofUploadFiles({
        ...base,
        manifest: { captures: [{ status: 'passed', fileName: '/tmp/escape.png' }] },
      }),
    /invalid proof upload path/,
  )
  assert.throws(
    () =>
      proofUploadFiles({
        ...base,
        manifest: { captures: [{ status: 'passed', fileName: 'C:\\tmp\\escape.png' }] },
      }),
    /invalid proof upload path/,
  )
})

test('proofUploadFiles rejects passed captures with unsupported media paths', () => {
  assert.throws(
    () =>
      proofUploadFiles({
        localDir: '/repo/.proof/pr-596/abc/run',
        manifest: { captures: [{ status: 'passed', fileName: 'web-sessions/recipe.json' }] },
      }),
    /invalid proof upload path/,
  )
})

test('validateProofUploadRelativePath allows nested PNG paths only', () => {
  assert.equal(
    validateProofUploadRelativePath('web-sessions/web sessions.png'),
    'web-sessions/web sessions.png',
  )
  assert.throws(() => validateProofUploadRelativePath('web-sessions/../x.png'), /invalid/)
  assert.throws(() => validateProofUploadRelativePath('web-sessions//x.png'), /invalid/)
  assert.throws(() => validateProofUploadRelativePath('web-sessions/recipe.json'), /invalid/)
})

test('validateRecipeId rejects path-like and empty recipe ids', () => {
  assert.equal(validateRecipeId('web-sessions'), 'web-sessions')
  assert.throws(() => validateRecipeId('bad/id'), /invalid proof recipe id/)
  assert.throws(() => validateRecipeId('../bad'), /invalid proof recipe id/)
  assert.throws(() => validateRecipeId(''), /invalid proof recipe id/)
})

test('validateBrowserRoute rejects external and protocol-relative routes', () => {
  assert.equal(validateBrowserRoute('/sessions'), '/sessions')
  assert.throws(() => validateBrowserRoute('http://localhost:3000'), /must be relative/)
  assert.throws(() => validateBrowserRoute('https://example.com'), /must be relative/)
  assert.throws(() => validateBrowserRoute('//example.com'), /must be relative/)
  assert.throws(() => validateBrowserRoute('/\\example.com'), /must be relative/)
  assert.throws(() => validateBrowserRoute('/\u0000example'), /must be relative/)
  assert.throws(() => validateBrowserRoute(''), /must be relative/)
})

test('default catalog keeps canvas recipes on fixture privacy', () => {
  const catalogJson = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const canvasRecipes = catalogJson.recipes.filter((recipe) => recipe.canvas)
  assert.ok(canvasRecipes.length > 0, 'expected at least one canvas recipe to guard')
  for (const recipe of canvasRecipes) {
    assert.equal(
      recipe.privacy,
      'fixture',
      `canvas recipe ${recipe.id} must use fixture privacy (DOM scan cannot inspect canvas pixels)`,
    )
  }
})

test('recipe schema route pattern rejects control-character browser routes', () => {
  const schema = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/schema.json', import.meta.url), 'utf8'),
  )
  const pattern = new RegExp(schema.$defs.browserRecipe.allOf[1].properties.route.pattern, 'u')

  assert.equal(pattern.test('/sessions'), true)
  assert.equal(pattern.test('http://localhost:3000'), false)
  assert.equal(pattern.test('//example.com'), false)
  assert.equal(pattern.test('/\\example.com'), false)
  assert.equal(pattern.test('/\n\\example.com'), false)
  assert.equal(pattern.test('/\u0000admin'), false)
  assert.equal(pattern.test('/admin\u007F'), false)
})

test('browser video step schema requires action-specific fields', () => {
  const schema = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/schema.json', import.meta.url), 'utf8'),
  )
  const stepSchema = schema.$defs.browserRecipe.allOf[1].properties.steps.items
  const requirementsByAction = Object.fromEntries(
    stepSchema.allOf.map((rule) => [rule.if.properties.action.const, rule.then.required]),
  )

  assert.deepEqual(requirementsByAction.goto, ['route'])
  assert.deepEqual(requirementsByAction.click, ['selector'])
  assert.deepEqual(requirementsByAction.type, ['selector', 'value'])
})

test('buildManifest sets png url unchanged (regression) and webm poster/video urls', () => {
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '7',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/prefix',
    captures: [
      {
        recipeId: 'web-x',
        title: 'X',
        surface: 'web',
        privacy: 'fixture',
        status: 'passed',
        mediaType: 'png',
        fileName: 'web-x/web-x.png',
      },
      {
        recipeId: 'web-v',
        title: 'V',
        surface: 'web',
        privacy: 'fixture',
        status: 'passed',
        mediaType: 'webm',
        fileName: 'web-v/web-v.webm',
        posterFileName: 'web-v/web-v.png',
      },
    ],
  })
  const png = manifest.captures[0]
  const webm = manifest.captures[1]
  assert.equal(png.mediaType, 'png')
  assert.equal(png.url, 'https://proof.example.dev/prefix/web-x/web-x.png')
  assert.equal(webm.mediaType, 'webm')
  assert.equal(webm.url, 'https://proof.example.dev/prefix/web-v/web-v.webm')
  assert.equal(webm.videoUrl, 'https://proof.example.dev/prefix/web-v/web-v.webm')
  assert.equal(webm.posterUrl, 'https://proof.example.dev/prefix/web-v/web-v.png')
})

test('proofUploadFiles queues webm + poster with correct content-types, png still single', () => {
  const manifest = {
    captures: [
      { status: 'passed', mediaType: 'png', fileName: 'web-x/web-x.png' },
      {
        status: 'passed',
        mediaType: 'webm',
        fileName: 'web-v/web-v.webm',
        posterFileName: 'web-v/web-v.png',
      },
      { status: 'failed', mediaType: 'webm', fileName: undefined },
    ],
  }
  const files = proofUploadFiles({ manifest, localDir: '/tmp/proof' })
  const byRelative = Object.fromEntries(files.map((f) => [f.relative, f.contentType]))
  assert.equal(byRelative['manifest.json'], 'application/json')
  assert.equal(byRelative['web-x/web-x.png'], 'image/png')
  assert.equal(byRelative['web-v/web-v.webm'], 'video/webm')
  assert.equal(byRelative['web-v/web-v.png'], 'image/png')
  // failed capture contributes nothing
  assert.equal(files.filter((f) => f.relative.startsWith('undefined')).length, 0)
})

test('proofUploadFiles queues mp4 + poster with correct content-types', () => {
  const manifest = {
    captures: [
      {
        status: 'passed',
        mediaType: 'mp4',
        fileName: 'web-v/web-v.mp4',
        posterFileName: 'web-v/web-v.png',
      },
    ],
  }
  const files = proofUploadFiles({ manifest, localDir: '/tmp/proof' })
  const byRelative = Object.fromEntries(files.map((f) => [f.relative, f.contentType]))
  assert.equal(byRelative['web-v/web-v.mp4'], 'video/mp4')
  assert.equal(byRelative['web-v/web-v.png'], 'image/png')
  // the source webm is never published
  assert.equal(files.filter((f) => f.relative.endsWith('.webm')).length, 0)
})

test('buildManifest sets videoUrl+posterUrl for mp4 capture (like webm)', () => {
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '7',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/prefix',
    captures: [
      {
        recipeId: 'web-v',
        title: 'V',
        surface: 'web',
        privacy: 'fixture',
        status: 'passed',
        mediaType: 'mp4',
        fileName: 'web-v/web-v.mp4',
        posterFileName: 'web-v/web-v.png',
      },
    ],
  })
  const mp4 = manifest.captures[0]
  assert.equal(mp4.mediaType, 'mp4')
  assert.equal(mp4.url, 'https://proof.example.dev/prefix/web-v/web-v.mp4')
  assert.equal(mp4.videoUrl, 'https://proof.example.dev/prefix/web-v/web-v.mp4')
  assert.equal(mp4.posterUrl, 'https://proof.example.dev/prefix/web-v/web-v.png')
})

test('PROOF_MEDIA_TYPES maps the four supported extensions', () => {
  assert.equal(PROOF_MEDIA_TYPES.png, 'image/png')
  assert.equal(PROOF_MEDIA_TYPES.webm, 'video/webm')
  assert.equal(PROOF_MEDIA_TYPES.gif, 'image/gif')
  assert.equal(PROOF_MEDIA_TYPES.mp4, 'video/mp4')
})

test('mediaTypeForPath derives content-type from extension', () => {
  assert.equal(mediaTypeForPath('a/b/c.webm'), 'video/webm')
  assert.equal(mediaTypeForPath('poster.png'), 'image/png')
  assert.equal(mediaTypeForPath('clip.mp4'), 'video/mp4')
  assert.throws(() => mediaTypeForPath('file.exe'), /unsupported proof media/)
})

test('validateProofUploadRelativePath accepts webm, gif, mp4, rejects traversal and unknown ext', () => {
  assert.equal(validateProofUploadRelativePath('id/id.webm'), 'id/id.webm')
  assert.equal(validateProofUploadRelativePath('id/id.gif'), 'id/id.gif')
  assert.equal(validateProofUploadRelativePath('id/id.png'), 'id/id.png') // regression: still works
  assert.equal(validateProofUploadRelativePath('id/id.mp4'), 'id/id.mp4')
  assert.throws(() => validateProofUploadRelativePath('id/../x.png'), /invalid proof upload path/)
  assert.throws(() => validateProofUploadRelativePath('/abs/x.png'), /invalid proof upload path/)
  assert.throws(() => validateProofUploadRelativePath('id/id.exe'), /invalid proof upload path/)
})

test('introCardCommand runs through services/web for surface web', () => {
  assert.deepEqual(
    introCardCommand({
      surface: 'web',
      out: '.proof/intro.png',
      width: 1440,
      height: 900,
      label: 'bossanova#123',
      title: 'Add intro card',
    }),
    [
      'pnpm',
      [
        '--dir',
        'services/web',
        'exec',
        'node',
        '../../scripts/proof-render-intro-card.mjs',
        '--out',
        '../../.proof/intro.png',
        '--width',
        '1440',
        '--height',
        '900',
        '--label',
        'bossanova#123',
        '--title',
        'Add intro card',
      ],
    ],
  )
})

test('resolveCatalogPath falls back to the committed default catalog', () => {
  assert.equal(
    resolveCatalogPath('/repo', undefined),
    path.join('/repo', 'proof', 'recipes', 'default.json'),
  )
  assert.equal(
    resolveCatalogPath('/repo', '   '),
    path.join('/repo', 'proof', 'recipes', 'default.json'),
  )
})

test('resolveCatalogPath honors an absolute override verbatim', () => {
  assert.equal(resolveCatalogPath('/repo', '/tmp/experiment.json'), '/tmp/experiment.json')
})

test('resolveCatalogPath resolves a relative override to an absolute path', () => {
  const out = resolveCatalogPath('/repo', 'scratch/recipes.json')
  assert.equal(out, path.resolve('scratch/recipes.json'))
  assert.equal(path.isAbsolute(out), true)
})

test('proofAncestorDirs returns ancestors deepest-first down to .proof', () => {
  assert.deepEqual(proofAncestorDirs('.proof/pr-1/abc123/2026-01-01/tok'), [
    '.proof/pr-1/abc123/2026-01-01',
    '.proof/pr-1/abc123',
    '.proof/pr-1',
    '.proof',
  ])
})

test('proofAncestorDirs handles an absolute path and stops at .proof', () => {
  assert.deepEqual(proofAncestorDirs('/repo/.proof/pr-1/c/run/tok'), [
    '/repo/.proof/pr-1/c/run',
    '/repo/.proof/pr-1/c',
    '/repo/.proof/pr-1',
    '/repo/.proof',
  ])
})

test('proofAncestorDirs returns [] when no .proof segment exists', () => {
  assert.deepEqual(proofAncestorDirs('/tmp/whatever/run'), [])
})

test('introCardCommand defaults an omitted title to an empty string', () => {
  const [, args] = introCardCommand({
    surface: 'web',
    out: '.proof/intro.png',
    width: 1440,
    height: 900,
    label: 'bossanova#123',
  })
  const titleIdx = args.indexOf('--title')
  assert.notEqual(titleIdx, -1)
  assert.strictEqual(args[titleIdx + 1], '')
  // No undefined entries — spawn() rejects a non-string arg.
  assert.ok(args.every((a) => typeof a === 'string'))
})

test('introCardCommand runs through services/marketing for surface marketing', () => {
  assert.deepEqual(
    introCardCommand({
      surface: 'marketing',
      out: '.proof/intro.png',
      width: 1920,
      height: 1080,
      label: 'bossanova#456',
      title: 'Marketing intro',
    }),
    [
      'pnpm',
      [
        '--dir',
        'services/marketing',
        'exec',
        'node',
        '../../scripts/proof-render-intro-card.mjs',
        '--out',
        '../../.proof/intro.png',
        '--width',
        '1920',
        '--height',
        '1080',
        '--label',
        'bossanova#456',
        '--title',
        'Marketing intro',
      ],
    ],
  )
})

// ── Task C: stills in manifest, upload, comment, and renderGallery ──────────

test('buildManifest adds url to stills on a passed video capture', () => {
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '7',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/prefix',
    captures: [
      {
        recipeId: 'web-v',
        title: 'V',
        surface: 'web',
        privacy: 'fixture',
        status: 'passed',
        mediaType: 'mp4',
        fileName: 'web-v/web-v.mp4',
        posterFileName: 'web-v/web-v.png',
        stills: [{ fileName: 'web-v/01-open-home.png', label: 'Open home' }],
      },
    ],
  })
  const capture = manifest.captures[0]
  assert.ok(Array.isArray(capture.stills), 'stills array must be present')
  assert.equal(capture.stills.length, 1)
  assert.equal(capture.stills[0].fileName, 'web-v/01-open-home.png')
  assert.equal(capture.stills[0].label, 'Open home')
  assert.equal(capture.stills[0].url, 'https://proof.example.dev/prefix/web-v/01-open-home.png')
})

test('buildManifest adds urls to stills on a failed capture with media', () => {
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '7',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/prefix',
    captures: [
      {
        recipeId: 'web-v',
        title: 'V',
        surface: 'web',
        privacy: 'fixture',
        status: 'failed',
        mediaType: 'mp4',
        fileName: 'web-v/web-v.mp4',
        stills: [{ fileName: 'web-v/01-open-home.png', label: 'Open home' }],
      },
    ],
  })
  const capture = manifest.captures[0]
  assert.equal(
    capture.stills[0].url,
    'https://proof.example.dev/prefix/web-v/01-open-home.png',
    'failed capture stills must stay reviewable when uploaded',
  )
})

test('buildManifest adds urls to stills on a capture with no fileName (video conversion failed)', () => {
  // proof-agent builds this exact shape when Playwright captured stills but
  // ffmpeg left no mp4: mediaType mp4, status failed, NO fileName, but stills.
  // The stills are still uploaded, so they must keep linkable urls.
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '7',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/prefix',
    captures: [
      {
        recipeId: 'web-v',
        title: 'V',
        surface: 'web',
        privacy: 'fixture',
        status: 'failed',
        mediaType: 'mp4',
        error: 'agent passed but no converted video artifact was produced',
        stills: [{ fileName: 'web-v/01-open-home.png', label: 'Open home' }],
      },
    ],
  })
  const capture = manifest.captures[0]
  assert.ok(!('url' in capture), 'no primary url when fileName is absent')
  assert.ok(Array.isArray(capture.stills), 'stills must survive the no-fileName path')
  assert.equal(
    capture.stills[0].url,
    'https://proof.example.dev/prefix/web-v/01-open-home.png',
    'stills must stay linkable even with no primary media',
  )
})

test('buildManifest leaves image capture without stills key unchanged', () => {
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '7',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/prefix',
    captures: [
      {
        recipeId: 'web-x',
        title: 'X',
        surface: 'web',
        privacy: 'fixture',
        status: 'passed',
        mediaType: 'png',
        fileName: 'web-x/web-x.png',
      },
    ],
  })
  const capture = manifest.captures[0]
  assert.ok(!('stills' in capture), 'no stills key should be spuriously added')
})

test('proofUploadFiles includes stills for a passed video capture', () => {
  const manifest = {
    captures: [
      {
        status: 'passed',
        mediaType: 'mp4',
        fileName: 'web-v/web-v.mp4',
        posterFileName: 'web-v/web-v.png',
        stills: [
          { fileName: 'web-v/01-open-home.png', label: 'Open home' },
          { fileName: 'web-v/02-select-session.png', label: 'Select session' },
        ],
      },
    ],
  }
  const files = proofUploadFiles({ manifest, localDir: '/repo/.proof/run' })
  const byRelative = Object.fromEntries(files.map((f) => [f.relative, f]))
  assert.equal(byRelative['web-v/01-open-home.png'].contentType, 'image/png')
  assert.equal(byRelative['web-v/01-open-home.png'].file, '/repo/.proof/run/web-v/01-open-home.png')
  assert.equal(byRelative['web-v/02-select-session.png'].contentType, 'image/png')
  assert.equal(
    byRelative['web-v/02-select-session.png'].file,
    '/repo/.proof/run/web-v/02-select-session.png',
  )
})

test('renderGallery includes header with commit, runId, and pr number', () => {
  const md = renderGallery({
    manifest: {
      prNumber: '42',
      commit: 'deadbeef',
      runId: 'run-99',
      generatedAt: '2026-06-23T00:00:00.000Z',
      publicBaseUrl: 'https://x',
      captures: [],
    },
  })
  assert.match(md, /# Proof report — PR 42/)
  assert.match(md, /`deadbeef`/)
  assert.match(md, /`run-99`/)
  assert.match(md, /2026-06-23T00:00:00.000Z/)
  assert.ok(md.endsWith('\n'), 'must end with trailing newline')
})

test('renderGallery renders video capture with poster link and step screenshots', () => {
  const md = renderGallery({
    manifest: {
      prNumber: '7',
      commit: 'abc1234',
      runId: 'run-1',
      generatedAt: '2026-06-23T00:00:00.000Z',
      publicBaseUrl: 'https://x',
      captures: [
        {
          title: 'Flow',
          surface: 'web',
          status: 'passed',
          mediaType: 'mp4',
          url: 'https://x/v.mp4',
          videoUrl: 'https://x/v.mp4',
          posterUrl: 'https://x/v.png',
          stills: [{ fileName: 'web-v/01-a.png', label: 'Step A', url: 'https://x/01-a.png' }],
        },
      ],
    },
  })
  // poster→mp4 link
  assert.match(md, /\[!\[Flow\]\(https:\/\/x\/v\.png\)\]\(https:\/\/x\/v\.mp4\)/)
  assert.match(md, /▶ Video/)
  // step screenshots subsection
  assert.match(md, /### Step screenshots/)
  assert.match(md, /!\[Step A\]\(https:\/\/x\/01-a\.png\)/)
})

test('renderGallery surfaces stills when the primary video media is missing', () => {
  // Video conversion failed: mediaType mp4 but no url/videoUrl. The stills the
  // agent captured must still be rendered as linked evidence rather than dropped.
  const md = renderGallery({
    manifest: {
      prNumber: '7',
      commit: 'abc1234',
      runId: 'run-1',
      generatedAt: '2026-06-23T00:00:00.000Z',
      publicBaseUrl: 'https://x',
      captures: [
        {
          title: 'Flow',
          surface: 'web',
          status: 'failed',
          mediaType: 'mp4',
          error: 'no converted video artifact was produced',
          stills: [
            { fileName: 'web-v/01-a.png', label: 'Step A', url: 'https://x/01-a.png' },
            { fileName: 'web-v/02-b.png', label: 'Step B', url: 'https://x/02-b.png' },
          ],
        },
      ],
    },
  })
  assert.match(md, /no converted video artifact was produced/)
  assert.match(md, /!\[Step A\]\(https:\/\/x\/01-a\.png\)/)
  assert.match(md, /!\[Step B\]\(https:\/\/x\/02-b\.png\)/)
})

test('renderGallery renders image capture inline', () => {
  const md = renderGallery({
    manifest: {
      prNumber: '7',
      commit: 'abc1234',
      runId: 'run-1',
      generatedAt: '2026-06-23T00:00:00.000Z',
      publicBaseUrl: 'https://x',
      captures: [
        {
          title: 'Still',
          surface: 'web',
          status: 'passed',
          mediaType: 'png',
          url: 'https://x/p.png',
        },
      ],
    },
  })
  assert.match(md, /!\[Still\]\(https:\/\/x\/p\.png\)/)
})

test('renderGallery renders non-passed capture with error text', () => {
  const md = renderGallery({
    manifest: {
      prNumber: '7',
      commit: 'abc1234',
      runId: 'run-1',
      generatedAt: '2026-06-23T00:00:00.000Z',
      publicBaseUrl: 'https://x',
      captures: [
        {
          title: 'Broken',
          surface: 'web',
          status: 'failed',
          error: 'timeout after 30s',
        },
      ],
    },
  })
  assert.match(md, /timeout after 30s/)
})

test('renderGallery renders failed video capture media after the error text', () => {
  const md = renderGallery({
    manifest: {
      prNumber: '7',
      commit: 'abc1234',
      runId: 'run-1',
      generatedAt: '2026-06-23T00:00:00.000Z',
      publicBaseUrl: 'https://x',
      captures: [
        {
          title: 'Broken but reviewable',
          surface: 'web',
          status: 'failed',
          error: 'agent found a regression',
          mediaType: 'mp4',
          url: 'https://x/v.mp4',
          videoUrl: 'https://x/v.mp4',
          posterUrl: 'https://x/v.png',
          stills: [{ fileName: 'web-v/01-a.png', label: 'Step A', url: 'https://x/01-a.png' }],
        },
      ],
    },
  })

  assert.match(md, /agent found a regression/)
  assert.match(md, /\[!\[Broken but reviewable\]\(https:\/\/x\/v\.png\)\]\(https:\/\/x\/v\.mp4\)/)
  assert.match(md, /### Step screenshots/)
  assert.match(md, /!\[Step A\]\(https:\/\/x\/01-a\.png\)/)
})

test('renderGallery with zero captures does not throw and has header', () => {
  let md
  assert.doesNotThrow(() => {
    md = renderGallery({
      manifest: {
        prNumber: '0',
        commit: 'abc',
        runId: 'run-0',
        generatedAt: '2026-06-23T00:00:00.000Z',
        publicBaseUrl: 'https://x',
        captures: [],
      },
    })
  })
  assert.match(md, /# Proof report/)
})

// ── Task 3: gallery link + Evidence line ─────────────────────────────────────

const passed = (over = {}) => ({
  status: 'passed',
  fileName: 'a/a.png',
  title: 'A',
  surface: 'web',
  ...over,
})

test('renderGallery video capture with zero stills has no Step screenshots subsection', () => {
  const md = renderGallery({
    manifest: {
      prNumber: '7',
      commit: 'abc1234',
      runId: 'run-1',
      generatedAt: '2026-06-23T00:00:00.000Z',
      publicBaseUrl: 'https://x',
      captures: [
        {
          title: 'Flow',
          surface: 'web',
          status: 'passed',
          mediaType: 'mp4',
          url: 'https://x/v.mp4',
          videoUrl: 'https://x/v.mp4',
          posterUrl: 'https://x/v.png',
        },
      ],
    },
  })
  assert.ok(!md.includes('### Step screenshots'), 'no subsection when no stills')
})

// ── Task 5: failed-capture media upload ──────────────────────────────────────

test('proofUploadFiles: includes media for failed captures that have a fileName', () => {
  const files = proofUploadFiles({
    manifest: { captures: [{ status: 'failed', fileName: 'a/a.mp4', posterFileName: 'a/a.png' }] },
    localDir: '/tmp/x',
  })
  const rels = files.map((f) => f.relative)
  assert.ok(rels.includes('a/a.mp4'), 'mp4 must be included for failed capture with fileName')
  assert.ok(rels.includes('a/a.png'), 'poster must be included for failed capture with fileName')
})

test('proofUploadFiles: skips media for failed captures without a fileName', () => {
  const files = proofUploadFiles({
    manifest: { captures: [{ status: 'failed' }] },
    localDir: '/tmp/x',
  })
  const rels = files.map((f) => f.relative)
  // Only manifest.json should be present
  assert.deepEqual(rels, ['manifest.json'])
})

test('buildManifest: adds url for failed capture with fileName', () => {
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '7',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/prefix',
    captures: [
      {
        recipeId: 'web-v',
        title: 'V',
        surface: 'web',
        privacy: 'fixture',
        status: 'failed',
        mediaType: 'mp4',
        fileName: 'web-v/web-v.mp4',
        posterFileName: 'web-v/web-v.png',
        error: 'agent timed out',
      },
    ],
  })
  const capture = manifest.captures[0]
  assert.ok(capture.url, 'url must be set for failed capture with fileName')
  assert.equal(capture.url, 'https://proof.example.dev/prefix/web-v/web-v.mp4')
  assert.equal(capture.videoUrl, 'https://proof.example.dev/prefix/web-v/web-v.mp4')
  assert.equal(capture.posterUrl, 'https://proof.example.dev/prefix/web-v/web-v.png')
})

// ── Task 1: minimal PR comment (renderComment + deriveVerdictBlock) ──────────

const baseManifest = {
  commit: 'abc1234',
  runId: '2026-06-24T05-42-36-859Z',
  reportUrl: 'https://github.com/recurser/bs-proof/blob/main/x/README.md',
  title: '[BOS-56] Improve bs-proof',
  verdict: 'passed',
  genAiLive: false,
  brief: { genAi: false },
  publicLiveCapture: false,
  captures: [{ fileName: 'web-sessions/web-sessions.png' }],
}

test('deriveVerdictBlock: passed + media → Satisfactory/High', () => {
  const v = deriveVerdictBlock(baseManifest)
  assert.equal(v.evidence, 'Satisfactory')
  assert.equal(v.confidence, 'High')
  assert.equal(v.evidenceOk, true)
  assert.equal(v.confidenceOk, true)
})

test('deriveVerdictBlock: passed + stills-only media → Satisfactory/High', () => {
  const v = deriveVerdictBlock({
    ...baseManifest,
    captures: [{ stills: [{ fileName: 'tui-agent/frame-01.png' }] }],
  })
  assert.equal(v.evidence, 'Satisfactory')
  assert.equal(v.confidence, 'High')
  assert.equal(v.evidenceOk, true)
  assert.equal(v.confidenceOk, true)
})

test('deriveVerdictBlock: passed but no media → Unsatisfactory/Low', () => {
  const v = deriveVerdictBlock({ ...baseManifest, captures: [{}] })
  assert.equal(v.evidence, 'Unsatisfactory')
  assert.equal(v.confidence, 'Low')
})

test('deriveVerdictBlock: gen-AI demoed UI-only → Medium', () => {
  const v = deriveVerdictBlock({ ...baseManifest, brief: { genAi: true }, genAiLive: false })
  assert.equal(v.confidence, 'Medium')
})

test('renderComment: minimal shape, no Verdict, no inline media', () => {
  const body = renderComment({ marker: proofCommentMarker('788'), manifest: baseManifest })
  assert.match(body, /<!-- bossanova-proof:pr-788 -->/)
  assert.match(body, /### \[📸 Proof gallery\]\(/)
  assert.match(body, /\*\*\[BOS-56\] Improve bs-proof\*\*/)
  assert.match(body, /\*\*Gen-AI:\*\* not live \(UI-only demo\)/)
  assert.match(body, /✅ \*\*Evidence:\*\* Satisfactory/)
  assert.match(body, /✅ \*\*Confidence:\*\* High/)
  assert.doesNotMatch(body, /Verdict/)
  assert.doesNotMatch(body, /!\[/) // no embedded images
  assert.doesNotMatch(body, /Manifest:/) // no manifest footer
})

test('renderComment: falls back to manifest link when reportUrl is absent', () => {
  const body = renderComment({
    marker: proofCommentMarker('788'),
    manifest: {
      ...baseManifest,
      reportUrl: undefined,
      publicBaseUrl: 'https://proof.example.dev/proof/pr-788/run-1',
    },
  })
  assert.match(body, /### \[📸 Proof manifest\]\(/)
  assert.match(body, /manifest\.json/)
})

test('renderComment: agentSummary renders a collapsible block', () => {
  const body = renderComment({
    marker: proofCommentMarker('788'),
    manifest: { ...baseManifest, agentSummary: 'Agent navigated the session view.' },
  })
  assert.match(body, /<details><summary>Agent summary<\/summary>/)
  assert.match(body, /Agent navigated the session view\./)
})

test('buildManifest: carries title/verdict/genAiLive/agentSummary/brief', () => {
  const m = buildManifest({
    commit: 'abc1234',
    prNumber: 788,
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example/x',
    captures: [
      { recipeId: 'web-sessions', status: 'passed', fileName: 'web-sessions/web-sessions.png' },
    ],
    title: 'My PR',
    verdict: 'passed',
    agentSummary: 'did things',
    brief: { genAi: false },
  })
  assert.equal(m.title, 'My PR')
  assert.equal(m.verdict, 'passed')
  assert.equal(m.genAiLive, false) // always present so the Gen-AI line renders
  assert.equal(m.agentSummary, 'did things')
  assert.deepEqual(m.brief, { genAi: false })
})

// TUI mp4 gallery rendering — mediaType 'mp4' drives the poster-link + ▶ Video
// path in renderGallery regardless of surface. Pin the TUI surface contract here.
test('renderGallery: TUI mp4 capture renders play-button poster link and ▶ Video', () => {
  const md = renderGallery({
    manifest: {
      prNumber: '100',
      commit: 'abc1234',
      runId: 'run-1',
      generatedAt: '2026-06-26T00:00:00.000Z',
      publicBaseUrl: 'https://proof.example.dev/proof/pr-100/run-1',
      captures: [
        {
          title: 'TUI New Session Flow',
          surface: 'tui',
          status: 'passed',
          mediaType: 'mp4',
          url: 'https://proof.example.dev/proof/pr-100/run-1/tui-new-session-flow/tui-new-session-flow.mp4',
          videoUrl:
            'https://proof.example.dev/proof/pr-100/run-1/tui-new-session-flow/tui-new-session-flow.mp4',
          posterUrl:
            'https://proof.example.dev/proof/pr-100/run-1/tui-new-session-flow/tui-new-session-flow.png',
        },
      ],
    },
  })
  // poster→mp4 link
  assert.match(
    md,
    /\[!\[TUI New Session Flow\]\(https:\/\/proof\.example\.dev\/proof\/pr-100\/run-1\/tui-new-session-flow\/tui-new-session-flow\.png\)\]\(https:\/\/proof\.example\.dev\/proof\/pr-100\/run-1\/tui-new-session-flow\/tui-new-session-flow\.mp4\)/,
  )
  assert.match(md, /▶ Video/)
})

// Regression: TUI video uploads the kept mp4, not the deleted webm.
// finishVideo deletes the source .webm and keeps the .mp4 it builds.
// captureRecipe must return fileName pointing at the surviving artifact.
test('proofUploadFiles resolves TUI video capture fileName to video/mp4 content-type', () => {
  const files = proofUploadFiles({
    localDir: '/repo/.proof/pr-100/abc/run',
    manifest: {
      captures: [
        {
          status: 'passed',
          mediaType: 'mp4',
          fileName: 'tui-new-session-flow/tui-new-session-flow.mp4',
          posterFileName: 'tui-new-session-flow/tui-new-session-flow.png',
        },
      ],
    },
  })
  const videoEntry = files.find((f) => f.relative.endsWith('.mp4'))
  assert.ok(videoEntry, 'expected an mp4 entry in upload files')
  assert.equal(videoEntry.contentType, 'video/mp4')
  assert.equal(videoEntry.relative, 'tui-new-session-flow/tui-new-session-flow.mp4')
  // The deleted .webm must NOT appear in the upload list.
  assert.ok(
    !files.some((f) => f.relative.endsWith('.webm')),
    'deleted .webm must not appear in upload list',
  )
})

// BOS-115: TUI is agent-only — the recipe catalog no longer contains TUI
// recipes or TUI pathRules, so a boss/TUI diff matches ZERO recipes. The
// agentic TUI proof path (classifyTuiSurface) handles these changes instead.
test('default catalog selects zero recipes for TUI/boss diffs (agent-only)', () => {
  const catalogPath = path.join(
    path.dirname(fileURLToPath(import.meta.url)),
    '../proof/recipes/default.json',
  )
  const catalog = JSON.parse(fs.readFileSync(catalogPath, 'utf8'))
  for (const file of [
    'services/boss/internal/tuidriver/keybytes.go',
    'services/boss/internal/views/home.go',
    'services/boss/internal/client/cron.go',
    'services/boss/cmd/root.go',
    'proto/boss.proto',
  ]) {
    assert.deepEqual(
      selectRecipes(catalog, [file]),
      [],
      `${file} must match zero recipes (TUI is agent-only)`,
    )
  }
})

test('default catalog exposes no surface:"tui" recipes and no TUI pathRules', () => {
  const catalogPath = path.join(
    path.dirname(fileURLToPath(import.meta.url)),
    '../proof/recipes/default.json',
  )
  const catalog = JSON.parse(fs.readFileSync(catalogPath, 'utf8'))
  assert.equal(
    catalog.recipes.filter((r) => r.surface === 'tui').length,
    0,
    'no TUI recipes should remain in the catalog',
  )
  const recipeIds = new Set(catalog.recipes.map((r) => r.id))
  for (const rule of catalog.pathRules) {
    for (const id of rule.recipeIds) {
      assert.ok(recipeIds.has(id), `pathRule "${rule.name}" references missing recipe ${id}`)
      assert.notEqual(
        catalog.recipes.find((r) => r.id === id).surface,
        'tui',
        `pathRule "${rule.name}" must not reference a TUI recipe`,
      )
    }
  }
})

test('parseProofArgs defaults empty invocation to help (not run)', () => {
  assert.deepEqual(parseProofArgs([]), {
    command: 'help',
    recipes: [],
    changedFiles: [],
    dryRun: false,
  })
})

test('orderCapturesForReport puts videos first, stable within groups', () => {
  const captures = [
    { recipeId: 'a', mediaType: 'png' },
    { recipeId: 'b', mediaType: 'mp4' },
    { recipeId: 'c', mediaType: 'png' },
    { recipeId: 'd', mediaType: 'webm' },
  ]
  assert.deepEqual(
    orderCapturesForReport(captures).map((c) => c.recipeId),
    ['b', 'd', 'a', 'c'],
  )
})

test('orderCapturesForReport treats missing mediaType as non-video', () => {
  const captures = [{ recipeId: 'x' }, { recipeId: 'v', mediaType: 'mp4' }]
  assert.deepEqual(
    orderCapturesForReport(captures).map((c) => c.recipeId),
    ['v', 'x'],
  )
})

// ── Task 2: renderDeferredComment + renderComment regression ─────────────────

test('renderDeferredComment is neutral and omits the verdict block', () => {
  const body = renderDeferredComment({
    marker: '<!-- bossanova-proof:pr-1 -->',
    manifest: {
      prNumber: 1,
      commit: 'abc1234',
      runId: 'r1',
      captures: [],
      publicBaseUrl: 'https://proof.example.dev/proof/pr-1/abc1234/r1',
    },
    reasonCode: 'agent-incomplete',
    recaptureHint: 'node scripts/proof.mjs run --recipe tui-home',
  })
  assert.ok(!body.includes('❌'), 'no red verdict marker')
  assert.ok(!body.includes('Unsatisfactory'), 'no Unsatisfactory verdict')
  assert.ok(
    body.includes('node scripts/proof.mjs run --recipe tui-home'),
    'includes re-capture hint',
  )
  assert.ok(body.includes('/manifest.json'), 'links to manifest evidence when available')
})

test('renderDeferredComment agent-unavailable names the agent-mode remedies', () => {
  const body = renderDeferredComment({
    marker: '<!-- bossanova-proof:pr-2 -->',
    manifest: { prNumber: 2, commit: 'abc1234', deferred: true },
    reasonCode: 'agent-unavailable',
    recaptureHint: 'PROOF_ANTHROPIC_API_KEY=… BOSS_PROOF_MODE=agent node scripts/proof.mjs run',
  })
  assert.ok(
    body.includes('agentic TUI proof unavailable'),
    'states the agentic TUI proof is unavailable',
  )
  assert.ok(
    body.includes('set PROOF_ANTHROPIC_API_KEY'),
    'names the key as a remedy, not a false statement of fact',
  )
  assert.ok(
    body.includes('unset BOSS_PROOF_MODE=recipe'),
    'mentions recipe mode as the other reason agent mode is disabled',
  )
  assert.ok(!body.includes('❌'), 'no red verdict marker — a deferral is not a failed change')
  assert.ok(!body.includes('Unsatisfactory'), 'no Unsatisfactory verdict')
  assert.ok(
    body.includes('BOSS_PROOF_MODE=agent node scripts/proof.mjs run'),
    'includes the re-capture hint',
  )
})

test('renderComment still emits the ✅ verdict on a passing run', () => {
  const body = renderComment({
    marker: 'm',
    manifest: {
      verdict: 'passed',
      captures: [{ fileName: 'a.png' }],
      prNumber: 1,
      commit: 'c',
      runId: 'r',
    },
  })
  assert.ok(body.includes('✅'), 'passing run keeps the green verdict')
})

test('renderGallery renders the video capture before still captures', () => {
  const md = renderGallery({
    manifest: {
      prNumber: '1',
      commit: 'abc',
      runId: 'r',
      generatedAt: 't',
      captures: [
        {
          recipeId: 'still',
          title: 'A Still',
          surface: 'tui',
          status: 'passed',
          mediaType: 'png',
          url: 'https://x/s.png',
        },
        {
          recipeId: 'vid',
          title: 'A Video',
          surface: 'tui',
          status: 'passed',
          mediaType: 'mp4',
          url: 'https://x/v.mp4',
          videoUrl: 'https://x/v.mp4',
          posterUrl: 'https://x/v.png',
        },
      ],
    },
  })
  assert.ok(md.indexOf('## A Video') < md.indexOf('## A Still'), md)
})

// ── Task 3: normalizeRecipe ──────────────────────────────────────────────────

test('normalizeRecipe defaults a browser still recipe to video with synthesized steps', () => {
  const out = normalizeRecipe({
    id: 'web-sessions',
    surface: 'web',
    title: 'Web Sessions',
    privacy: 'fixture',
    route: '/',
  })
  assert.equal(out.capture, 'video')
  assert.equal(out.steps[0].action, 'goto')
  assert.equal(out.steps[0].route, '/')
  const scrollStep = out.steps.find((s) => s.action === 'scroll' && s.fullPage === true)
  assert.ok(scrollStep, 'fallback still scrolls the full page')
  assert.match(scrollStep.caption, /unavailable|overview|fallback|could not/i)
  assert.notStrictEqual(scrollStep.caption, 'Scroll through the page')
})

test('normalizeRecipe keeps an explicit still recipe a still', () => {
  const out = normalizeRecipe({
    id: 'web-account',
    surface: 'web',
    title: 'Account',
    privacy: 'fixture',
    route: '/settings/account',
    capture: 'still',
  })
  assert.notEqual(out.capture, 'video')
  assert.ok(!out.steps)
})

test('normalizeRecipe keeps a still recipe a still under double normalization', () => {
  // proof.mjs normalizes then writes recipe.json; the runner re-normalizes it.
  // A second pass must NOT flip an explicit still opt-out into a video.
  const still = {
    id: 'web-account',
    surface: 'web',
    title: 'Account',
    privacy: 'fixture',
    route: '/settings/account',
    capture: 'still',
  }
  const out = normalizeRecipe(normalizeRecipe(still))
  assert.notEqual(out.capture, 'video')
  assert.ok(!out.steps)
})

test('normalizeRecipe leaves an authored video recipe untouched (idempotent)', () => {
  const authored = {
    id: 'web-new-session-flow',
    surface: 'web',
    title: 'New session',
    privacy: 'fixture',
    capture: 'video',
    steps: [{ action: 'goto', route: '/sessions/new' }],
  }
  const out = normalizeRecipe(normalizeRecipe(authored))
  assert.equal(out.capture, 'video')
  assert.deepEqual(out.steps, authored.steps)
})

test('normalizeRecipe passes TUI recipes through unchanged', () => {
  const tui = { id: 'tui-home', surface: 'tui', title: 'Home', privacy: 'fixture' }
  assert.deepEqual(normalizeRecipe(tui), tui)
})
