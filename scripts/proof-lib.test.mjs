#!/usr/bin/env node

import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';

import {
  PROOF_MEDIA_TYPES,
  buildManifest,
  browserCaptureCommand,
  classifySecretRisk,
  githubCommentCommand,
  listProofCommentsCommand,
  mediaTypeForPath,
  minimizeCommentCommand,
  normalizeChangedFiles,
  parseProofArgs,
  proofCommentMarker,
  proofRunPaths,
  proofUploadFiles,
  r2UploadCommand,
  renderComment,
  selectOutdatedProofCommentIds,
  selectRecipes,
  trimTerminalBlankLines,
  terminalRenderCommand,
  tuiCaptureCommand,
  tuiVideoCaptureCommand,
  validateBrowserRoute,
  validateProofUploadRelativePath,
  validateRecipeId,
} from './proof-lib.mjs';

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
};

test('normalizeChangedFiles trims blanks and strips leading dot slash', () => {
  assert.deepEqual(normalizeChangedFiles([' ./services/web/src/App.tsx ', '', null]), [
    'services/web/src/App.tsx',
  ]);
});

test('selectRecipes maps changed paths to unique recipes in catalog order', () => {
  const selected = selectRecipes(catalog, [
    'services/web/src/App.tsx',
    'services/boss/internal/views/home.go',
    'services/web/src/pages/Sessions.tsx',
  ]);

  assert.deepEqual(
    selected.map((recipe) => recipe.id),
    ['tui-home', 'web-sessions'],
  );
});

test('selectRecipes honors explicit recipe ids before diff rules', () => {
  const selected = selectRecipes(catalog, ['services/web/src/App.tsx'], ['marketing-home']);

  assert.deepEqual(
    selected.map((recipe) => recipe.id),
    ['marketing-home'],
  );
});

test('selectRecipes returns empty array when no visual paths match', () => {
  assert.deepEqual(selectRecipes(catalog, ['services/bossd/internal/server/server.go']), []);
});

test('selectRecipes rejects unknown recipe ids from matching path rules', () => {
  const invalidCatalog = {
    ...catalog,
    pathRules: [{ name: 'Broken', patterns: ['services/web/'], recipeIds: ['missing-recipe'] }],
  };

  assert.throws(
    () => selectRecipes(invalidCatalog, ['services/web/src/App.tsx']),
    /unknown proof recipe: missing-recipe/,
  );
});

test('classifySecretRisk detects common secret-shaped values', () => {
  assert.equal(classifySecretRisk('normal UI text').risk, 'none');
  assert.equal(classifySecretRisk('token=ghp_1234567890abcdefghijklmnopqrstuvwxyz').risk, 'high');
  assert.equal(classifySecretRisk('sk-proj-1234567890abcdefghijklmnopqrst').risk, 'high');
  assert.equal(classifySecretRisk('password: hunter2').risk, 'high');
});

test('classifySecretRisk ignores git SHAs but detects base64-looking tokens', () => {
  assert.equal(classifySecretRisk('e3b03071ddf4b6c7eb69b73b2b50e914b206065f').risk, 'none');
  assert.equal(
    classifySecretRisk('docs/plans/2026-05-29-gke-orchestrator-review-gap').risk,
    'none',
  );
  assert.equal(classifySecretRisk('MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY').risk, 'high');
  assert.equal(classifySecretRisk('YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXorLzAxMjM=').risk, 'high');
});

test('classifySecretRisk flags slash-containing high-entropy tokens (AWS secret keys)', () => {
  // A base64 token with a `/` but mixed case must NOT be excused as a file path.
  assert.equal(classifySecretRisk('wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY').risk, 'high');
  // Genuine lowercase repo paths are still treated as safe, even when long.
  assert.equal(
    classifySecretRisk('services/boss/internal/skillinstall/skills/bs-proof/skill').risk,
    'none',
  );
});

test('classifySecretRisk excuses anchored paths with mixed-case segments', () => {
  // Regression: the TUI settings screen renders an absolute worktree base dir.
  // On macOS a `t.TempDir()` path carries mixed-case, high-entropy segments
  // (e.g. `TestProofCapture144004542`) that must not be mistaken for a token.
  assert.equal(
    classifySecretRisk(
      '/var/folders/51/xwb_1fmx3_j526snzrksdqmr0000gp/T/TestProofCapture144004542/001/.bossanova/worktrees',
    ).risk,
    'none',
  );
  // Home-relative and explicitly-relative anchored paths are excused too.
  assert.equal(classifySecretRisk('~/.bossanova/worktrees/MyRepo-Feature').risk, 'none');
  assert.equal(classifySecretRisk('./services/boss/internal/Views/RepoSettings').risk, 'none');
  // Anchoring does not blanket-excuse: a named credential pattern embedded in
  // a path is still flagged because SECRET_PATTERNS is matched first.
  assert.equal(
    classifySecretRisk('/home/runner/work/token=ghp_1234567890abcdefghijklmnopqrstuvwxyz').risk,
    'high',
  );
});

test('classifySecretRisk detects AWS, Slack, Google, JWT, and PEM credentials', () => {
  assert.equal(classifySecretRisk('AKIAIOSFODNN7EXAMPLE').risk, 'high');
  assert.equal(classifySecretRisk('ASIAIOSFODNN7EXAMPLE').risk, 'high');
  assert.equal(classifySecretRisk('xoxb-2222222222-3333333333-abcdEFGH').risk, 'high');
  assert.equal(classifySecretRisk('AIzaSyA1234567890abcdefghijklmnopqrstuvw').risk, 'high');
  assert.equal(
    classifySecretRisk(
      'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U',
    ).risk,
    'high',
  );
  assert.equal(classifySecretRisk('-----BEGIN RSA PRIVATE KEY-----').risk, 'high');
});

test('classifySecretRisk reports a category reason rather than raw regex source', () => {
  const result = classifySecretRisk('token=ghp_1234567890abcdefghijklmnopqrstuvwxyz');
  assert.equal(result.risk, 'high');
  assert.ok(result.reason && !result.reason.includes('/'), 'reason should be a stable label');
  assert.equal(classifySecretRisk('normal UI text').reason, undefined);
});

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
  });

  assert.equal(manifest.commit, 'abc1234');
  assert.equal(
    manifest.captures[0].url,
    'https://proof.bossanova.dev/proof/repo/pr-596/abc1234/run-1/web-sessions.png',
  );
});

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
  });

  assert.equal(
    manifest.captures[0].url,
    'https://proof.bossanova.dev/proof/repo/pr-596/abc1234/run-1/web-sessions/web%20sessions.png',
  );
});

test('renderComment uses inline images and updates as a sticky comment', () => {
  const body = renderComment({
    marker: '<!-- bossanova-proof:pr-596 -->',
    manifest: {
      commit: 'abc1234',
      prNumber: '596',
      runId: 'run-1',
      publicLiveCapture: true,
      captures: [
        {
          title: 'Web Sessions',
          surface: 'web',
          privacy: 'live',
          status: 'passed',
          url: 'https://proof.bossanova.dev/a.png',
        },
        {
          title: 'TUI Home',
          surface: 'tui',
          privacy: 'fixture',
          status: 'failed',
          error: 'timeout waiting for Add dark mode',
        },
      ],
    },
  });

  assert.match(body, /<!-- bossanova-proof:pr-596 -->/);
  assert.match(body, /PUBLIC LIVE CAPTURE/);
  assert.match(body, /!\[Web Sessions\]\(https:\/\/proof\.bossanova\.dev\/a\.png\)/);
  assert.match(body, /timeout waiting for Add dark mode/);
  // Vertical per-capture layout: stacked labels, no table, no privacy field.
  assert.match(body, /### Web Sessions\n\n\*\*Surface:\*\* web {2}\n\*\*Status:\*\* passed/);
  assert.ok(!/Privacy/i.test(body), 'comment should not include a privacy field');
  assert.ok(!/\| Surface \|/.test(body), 'comment should not use a table layout');
});

test('proof comment upsert helpers build expected gh commands', () => {
  assert.equal(proofCommentMarker('597'), '<!-- bossanova-proof:pr-597 -->');
  assert.deepEqual(listProofCommentsCommand({ prNumber: '597' }), [
    'gh',
    ['pr', 'view', '597', '--json', 'comments'],
  ]);
  const [bin, args] = minimizeCommentCommand({ commentId: 'IC_abc' });
  assert.equal(bin, 'gh');
  assert.deepEqual(args.slice(0, 3), ['api', 'graphql', '-f']);
  assert.match(args[3], /minimizeComment\(input:\{subjectId:\$id,classifier:OUTDATED\}\)/);
  assert.deepEqual(args.slice(4), ['-f', 'id=IC_abc']);
});

test('selectOutdatedProofCommentIds returns only visible marker comments', () => {
  const marker = proofCommentMarker('597');
  const commentsJson = JSON.stringify({
    comments: [
      { id: 'IC_keep_visible', body: `proof ${marker}`, isMinimized: false },
      { id: 'IC_already_hidden', body: `proof ${marker}`, isMinimized: true },
      { id: 'IC_other_comment', body: 'unrelated review note', isMinimized: false },
      { id: 'IC_second_visible', body: `${marker}\nrun 2`, isMinimized: false },
    ],
  });
  assert.deepEqual(selectOutdatedProofCommentIds({ commentsJson, marker }), [
    'IC_keep_visible',
    'IC_second_visible',
  ]);
  // Accepts a pre-parsed object and tolerates malformed input.
  assert.deepEqual(selectOutdatedProofCommentIds({ commentsJson: { comments: [] }, marker }), []);
  assert.deepEqual(selectOutdatedProofCommentIds({ commentsJson: 'not json', marker }), []);
});

test('trimTerminalBlankLines crops blank edges but keeps internal blanks', () => {
  const screen = ['', '', '  Settings', '', '  Worktree base dir: /x', '', '', ''].join('\n');
  assert.equal(
    trimTerminalBlankLines(screen),
    ['  Settings', '', '  Worktree base dir: /x'].join('\n'),
  );
  // Whitespace-only lines count as blank; content is otherwise untouched.
  assert.equal(trimTerminalBlankLines('   \n\tbody\n   '), '\tbody');
  assert.equal(trimTerminalBlankLines(''), '');
  assert.equal(trimTerminalBlankLines('   \n  \n'), '');
});

test('parseProofArgs parses run command with explicit recipes', () => {
  assert.deepEqual(parseProofArgs(['run', '--recipe', 'web-sessions', '--recipe', 'tui-home']), {
    command: 'run',
    recipes: ['web-sessions', 'tui-home'],
    changedFiles: [],
    dryRun: false,
  });
});

test('parseProofArgs parses changed files and dry run', () => {
  assert.deepEqual(
    parseProofArgs(['plan', '--changed-file', 'services/web/src/App.tsx', '--dry-run']),
    {
      command: 'plan',
      recipes: [],
      changedFiles: ['services/web/src/App.tsx'],
      dryRun: true,
    },
  );
});

test('parseProofArgs defaults flags-only invocation to run command', () => {
  assert.deepEqual(parseProofArgs(['--changed-file', 'services/web/src/App.tsx', '--dry-run']), {
    command: 'run',
    recipes: [],
    changedFiles: ['services/web/src/App.tsx'],
    dryRun: true,
  });
});

test('parseProofArgs rejects recipe flag without value', () => {
  assert.throws(() => parseProofArgs(['run', '--recipe']), /--recipe requires a value/);
  assert.throws(
    () => parseProofArgs(['run', '--recipe', '--dry-run']),
    /--recipe requires a value/,
  );
});

test('parseProofArgs rejects unknown arguments', () => {
  assert.throws(() => parseProofArgs(['run', '--unknown']), /unknown proof argument: --unknown/);
});

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
  );
});

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
  );
});

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
  );
});

test('tuiCaptureCommand passes recipe and output through environment', () => {
  assert.deepEqual(
    tuiCaptureCommand({ recipePath: '.proof/tui-home/recipe.json', outputDir: '.proof/tui-home' }),
    [
      'go',
      ['test', './internal/tuitest', '-run', '^TestProofCapture$', '-count=1', '-timeout', '120s'],
      {
        cwd: 'services/boss',
        env: {
          BOSS_PROOF_RECIPE: '../../.proof/tui-home/recipe.json',
          BOSS_PROOF_OUTPUT_DIR: '../../.proof/tui-home',
        },
      },
    ],
  );
});

test('proofRunPaths creates stable proof bundle paths', () => {
  assert.deepEqual(proofRunPaths({ prNumber: '596', commit: 'abc1234', runId: 'run-1' }), {
    localDir: '.proof/pr-596/abc1234/run-1',
    publicPrefix: 'proof/bossanova/pr-596/abc1234/run-1',
  });
});

test('proofRunPaths appends an optional random token segment', () => {
  assert.deepEqual(
    proofRunPaths({ prNumber: '596', commit: 'abc1234', runId: 'run-1', token: 'tok-xyz' }),
    {
      localDir: '.proof/pr-596/abc1234/run-1/tok-xyz',
      publicPrefix: 'proof/bossanova/pr-596/abc1234/run-1/tok-xyz',
    },
  );
});

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
        'wrangler@3.90.0',
        'r2',
        'object',
        'put',
        'bossanova-proof-production/proof/bossanova/pr-596/abc/run/a.png',
        '--file',
        '.proof/a.png',
        '--content-type',
        'image/png',
      ],
    ],
  );
});

test('githubCommentCommand posts body file with gh pr comment', () => {
  assert.deepEqual(githubCommentCommand({ prNumber: '596', bodyFile: '.proof/comment.md' }), [
    'gh',
    ['pr', 'comment', '596', '--body-file', '.proof/comment.md'],
  ]);
});

test('proofUploadFiles selects manifest and passed PNG captures only', () => {
  assert.deepEqual(
    proofUploadFiles({
      localDir: '/repo/.proof/pr-596/abc/run',
      manifest: {
        captures: [
          { status: 'passed', fileName: 'marketing-home/marketing-home.png' },
          { status: 'failed', fileName: 'tui-home/tui-home.png' },
          { status: 'failed', fileName: 'web-sessions/recipe.json' },
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
    ],
  );
});

test('proofUploadFiles rejects traversal and absolute PNG capture paths', () => {
  const base = {
    localDir: '/repo/.proof/pr-596/abc/run',
    manifest: { captures: [{ status: 'passed', fileName: '../escape.png' }] },
  };

  assert.throws(() => proofUploadFiles(base), /invalid proof upload path/);
  assert.throws(
    () =>
      proofUploadFiles({
        ...base,
        manifest: { captures: [{ status: 'passed', fileName: '/tmp/escape.png' }] },
      }),
    /invalid proof upload path/,
  );
  assert.throws(
    () =>
      proofUploadFiles({
        ...base,
        manifest: { captures: [{ status: 'passed', fileName: 'C:\\tmp\\escape.png' }] },
      }),
    /invalid proof upload path/,
  );
});

test('proofUploadFiles rejects passed captures with unsupported media paths', () => {
  assert.throws(
    () =>
      proofUploadFiles({
        localDir: '/repo/.proof/pr-596/abc/run',
        manifest: { captures: [{ status: 'passed', fileName: 'web-sessions/recipe.json' }] },
      }),
    /invalid proof upload path/,
  );
});

test('validateProofUploadRelativePath allows nested PNG paths only', () => {
  assert.equal(
    validateProofUploadRelativePath('web-sessions/web sessions.png'),
    'web-sessions/web sessions.png',
  );
  assert.throws(() => validateProofUploadRelativePath('web-sessions/../x.png'), /invalid/);
  assert.throws(() => validateProofUploadRelativePath('web-sessions//x.png'), /invalid/);
  assert.throws(() => validateProofUploadRelativePath('web-sessions/recipe.json'), /invalid/);
});

test('validateRecipeId rejects path-like and empty recipe ids', () => {
  assert.equal(validateRecipeId('web-sessions'), 'web-sessions');
  assert.throws(() => validateRecipeId('bad/id'), /invalid proof recipe id/);
  assert.throws(() => validateRecipeId('../bad'), /invalid proof recipe id/);
  assert.throws(() => validateRecipeId(''), /invalid proof recipe id/);
});

test('validateBrowserRoute rejects external and protocol-relative routes', () => {
  assert.equal(validateBrowserRoute('/sessions'), '/sessions');
  assert.throws(() => validateBrowserRoute('http://localhost:3000'), /must be relative/);
  assert.throws(() => validateBrowserRoute('https://example.com'), /must be relative/);
  assert.throws(() => validateBrowserRoute('//example.com'), /must be relative/);
  assert.throws(() => validateBrowserRoute('/\\example.com'), /must be relative/);
  assert.throws(() => validateBrowserRoute('/\u0000example'), /must be relative/);
  assert.throws(() => validateBrowserRoute(''), /must be relative/);
});

test('default catalog keeps canvas recipes on fixture privacy', () => {
  const catalogJson = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  );
  const canvasRecipes = catalogJson.recipes.filter((recipe) => recipe.canvas);
  assert.ok(canvasRecipes.length > 0, 'expected at least one canvas recipe to guard');
  for (const recipe of canvasRecipes) {
    assert.equal(
      recipe.privacy,
      'fixture',
      `canvas recipe ${recipe.id} must use fixture privacy (DOM scan cannot inspect canvas pixels)`,
    );
  }
});

test('recipe schema route pattern rejects control-character browser routes', () => {
  const schema = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/schema.json', import.meta.url), 'utf8'),
  );
  const pattern = new RegExp(schema.$defs.browserRecipe.allOf[1].properties.route.pattern, 'u');

  assert.equal(pattern.test('/sessions'), true);
  assert.equal(pattern.test('http://localhost:3000'), false);
  assert.equal(pattern.test('//example.com'), false);
  assert.equal(pattern.test('/\\example.com'), false);
  assert.equal(pattern.test('/\n\\example.com'), false);
  assert.equal(pattern.test('/\u0000admin'), false);
  assert.equal(pattern.test('/admin\u007F'), false);
});

test('tui schema accepts steps and fixture fields', () => {
  const schema = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/schema.json', import.meta.url), 'utf8'),
  );
  const tui = schema.$defs.tuiRecipe.allOf[1].properties;
  assert.ok(tui.steps, 'tuiRecipe must declare a steps property');
  assert.ok(tui.fixture, 'tuiRecipe must declare a fixture property');
  assert.deepEqual(tui.fixture.enum, ['demo', 'login', 'onboarding']);
});

test('browser video step schema requires action-specific fields', () => {
  const schema = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/schema.json', import.meta.url), 'utf8'),
  );
  const stepSchema = schema.$defs.browserRecipe.allOf[1].properties.steps.items;
  const requirementsByAction = Object.fromEntries(
    stepSchema.allOf.map((rule) => [rule.if.properties.action.const, rule.then.required]),
  );

  assert.deepEqual(requirementsByAction.goto, ['route']);
  assert.deepEqual(requirementsByAction.click, ['selector']);
  assert.deepEqual(requirementsByAction.type, ['selector', 'value']);
});

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
  });
  const png = manifest.captures[0];
  const webm = manifest.captures[1];
  assert.equal(png.mediaType, 'png');
  assert.equal(png.url, 'https://proof.example.dev/prefix/web-x/web-x.png');
  assert.equal(webm.mediaType, 'webm');
  assert.equal(webm.url, 'https://proof.example.dev/prefix/web-v/web-v.webm');
  assert.equal(webm.videoUrl, 'https://proof.example.dev/prefix/web-v/web-v.webm');
  assert.equal(webm.posterUrl, 'https://proof.example.dev/prefix/web-v/web-v.png');
});

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
  };
  const files = proofUploadFiles({ manifest, localDir: '/tmp/proof' });
  const byRelative = Object.fromEntries(files.map((f) => [f.relative, f.contentType]));
  assert.equal(byRelative['manifest.json'], 'application/json');
  assert.equal(byRelative['web-x/web-x.png'], 'image/png');
  assert.equal(byRelative['web-v/web-v.webm'], 'video/webm');
  assert.equal(byRelative['web-v/web-v.png'], 'image/png');
  // failed capture contributes nothing
  assert.equal(files.filter((f) => f.relative.startsWith('undefined')).length, 0);
});

test('renderComment embeds png inline (regression) and webm as poster link', () => {
  const body = renderComment({
    marker: '<!-- m -->',
    manifest: {
      commit: 'abc1234',
      runId: 'run-1',
      publicBaseUrl: 'https://proof.example.dev/prefix',
      publicLiveCapture: false,
      captures: [
        {
          title: 'Still',
          surface: 'web',
          status: 'passed',
          mediaType: 'png',
          url: 'https://x/p.png',
        },
        {
          title: 'Flow',
          surface: 'web',
          status: 'passed',
          mediaType: 'webm',
          url: 'https://x/v.webm',
          videoUrl: 'https://x/v.webm',
          posterUrl: 'https://x/v.png',
        },
      ],
    },
  });
  // png inline image
  assert.match(body, /!\[Still\]\(https:\/\/x\/p\.png\)/);
  // webm: clickable poster thumbnail linking to the video + caption
  assert.match(body, /\[!\[Flow\]\(https:\/\/x\/v\.png\)\]\(https:\/\/x\/v\.webm\)/);
  assert.match(body, /▶ Video/);
});

test('renderComment embeds gif inline like an image', () => {
  const body = renderComment({
    marker: '<!-- m -->',
    manifest: {
      commit: 'abc1234',
      runId: 'run-1',
      publicBaseUrl: 'https://x',
      publicLiveCapture: false,
      captures: [
        {
          title: 'Tui',
          surface: 'tui',
          status: 'passed',
          mediaType: 'gif',
          url: 'https://x/t.gif',
        },
      ],
    },
  });
  assert.match(body, /!\[Tui\]\(https:\/\/x\/t\.gif\)/);
});

test('PROOF_MEDIA_TYPES maps the three supported extensions', () => {
  assert.equal(PROOF_MEDIA_TYPES.png, 'image/png');
  assert.equal(PROOF_MEDIA_TYPES.webm, 'video/webm');
  assert.equal(PROOF_MEDIA_TYPES.gif, 'image/gif');
});

test('mediaTypeForPath derives content-type from extension', () => {
  assert.equal(mediaTypeForPath('a/b/c.webm'), 'video/webm');
  assert.equal(mediaTypeForPath('poster.png'), 'image/png');
  assert.throws(() => mediaTypeForPath('clip.mp4'), /unsupported proof media/);
});

test('validateProofUploadRelativePath accepts webm and gif, rejects traversal and unknown ext', () => {
  assert.equal(validateProofUploadRelativePath('id/id.webm'), 'id/id.webm');
  assert.equal(validateProofUploadRelativePath('id/id.gif'), 'id/id.gif');
  assert.equal(validateProofUploadRelativePath('id/id.png'), 'id/id.png'); // regression: still works
  assert.throws(() => validateProofUploadRelativePath('id/../x.png'), /invalid proof upload path/);
  assert.throws(() => validateProofUploadRelativePath('/abs/x.png'), /invalid proof upload path/);
  assert.throws(() => validateProofUploadRelativePath('id/id.exe'), /invalid proof upload path/);
});

test('tuiVideoCaptureCommand runs vhs against the tape', () => {
  assert.deepEqual(tuiVideoCaptureCommand({ tapePath: 'x.tape' }), ['vhs', ['x.tape']]);
});
