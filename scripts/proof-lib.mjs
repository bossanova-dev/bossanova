#!/usr/bin/env node

import path from 'node:path';

export function normalizeChangedFiles(files) {
  return files
    .filter((file) => file !== null && file !== undefined)
    .map((file) => String(file).trim().replaceAll('\\', '/').replace(/^\.\//, ''))
    .filter(Boolean);
}

export function selectRecipes(catalog, changedFiles, explicitRecipeIds = []) {
  const recipesById = new Map(catalog.recipes.map((recipe) => [recipe.id, recipe]));
  const selectedIds = [];

  if (explicitRecipeIds.length > 0) {
    for (const id of explicitRecipeIds) {
      if (!recipesById.has(id)) {
        throw new Error(`unknown proof recipe: ${id}`);
      }
      selectedIds.push(id);
    }
    return uniqueInCatalogOrder(catalog.recipes, selectedIds);
  }

  const normalized = normalizeChangedFiles(changedFiles);
  for (const rule of catalog.pathRules) {
    if (normalized.some((file) => rule.patterns.some((pattern) => file.startsWith(pattern)))) {
      for (const id of rule.recipeIds) {
        if (!recipesById.has(id)) {
          throw new Error(`unknown proof recipe: ${id}`);
        }
        selectedIds.push(id);
      }
    }
  }

  return uniqueInCatalogOrder(catalog.recipes, selectedIds);
}

// Named credential shapes. Order does not matter; any match is high risk.
// Keep the `reason` labels generic — they surface in PR comments and error
// messages, so they must never echo the matched secret itself.
const SECRET_PATTERNS = [
  /\bgh[pousr]_[A-Za-z0-9_]{20,}\b/, // GitHub token
  /\bgithub_pat_[A-Za-z0-9_]{20,}\b/, // GitHub fine-grained PAT
  /\bsk-[A-Za-z0-9-]{20,}\b/, // OpenAI / Stripe style key
  /\b(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA)[0-9A-Z]{16}\b/, // AWS access key id
  /\bxox[baprs]-[A-Za-z0-9-]{10,}\b/, // Slack token
  /\bAIza[0-9A-Za-z_-]{35}\b/, // Google API key
  /\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\b/, // JWT
  /-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----/, // PEM private key
  /\b(password|passwd|token|secret|api[_-]?key)\s*[:=]\s*\S+/i, // labelled secret
];

export function classifySecretRisk(text) {
  const input = String(text ?? '');
  if (SECRET_PATTERNS.some((pattern) => pattern.test(input))) {
    return { risk: 'high', reason: 'credential-pattern' };
  }

  const base64CandidatePattern =
    /(?:^|[^A-Za-z0-9+/_=-])([A-Za-z0-9+/_-]{40,}={0,2})(?=$|[^A-Za-z0-9+/_=-])/g;
  for (const candidate of input.matchAll(base64CandidatePattern)) {
    const value = candidate[1];
    if (/^[A-Fa-f0-9]+$/.test(value)) {
      continue; // hex digests / git SHAs
    }
    if (looksLikeFilePath(value)) {
      continue;
    }
    return { risk: 'high', reason: 'high-entropy-token' };
  }

  return { risk: 'none' };
}

// A slash-containing candidate is only excused as a file path when it reads
// like one. Two cases:
//
//   - Anchored paths — absolute (`/…`), relative (`./…`, `../…`), or
//     home-relative (`~/…`) — are filesystem paths, never credentials, so we
//     excuse them even when a segment is mixed-case (e.g. a macOS temp dir
//     such as `/var/folders/…/TestFoo123/…`). This is safe because every
//     named credential shape in SECRET_PATTERNS is matched first, so a real
//     token embedded as a path segment is still flagged.
//   - Unanchored slash-joined blobs stay subject to the strict lowercase
//     rule. An AWS secret access key can legitimately contain `/` but never
//     starts with one, so it remains flagged before reaching a public bucket.
//
// base64 padding (`=`) and `+` never appear in real paths, so their presence
// disqualifies a candidate from the path exemption in either case.
function looksLikeFilePath(value) {
  if (!value.includes('/')) {
    return false;
  }
  if (/[+=]/.test(value)) {
    return false;
  }
  const anchored = /^(?:\/|\.\.?\/|~\/)/.test(value);
  if (!anchored && /[A-Z]/.test(value)) {
    return false;
  }
  const segmentPattern = anchored ? /^[A-Za-z0-9._-]+$/ : /^[a-z0-9._-]+$/;
  return value.split('/').every((segment) => segment === '' || segmentPattern.test(segment));
}

export function buildManifest({ commit, prNumber, runId, publicBaseUrl, captures }) {
  const normalizedBase = publicBaseUrl.replace(/\/$/, '');
  const publicLiveCapture = captures.some((capture) => capture.privacy === 'live');
  return {
    version: 1,
    generatedAt: new Date().toISOString(),
    commit,
    prNumber: String(prNumber),
    runId,
    publicBaseUrl: normalizedBase,
    publicLiveCapture,
    captures: captures.map((capture) => ({
      ...capture,
      url:
        capture.status === 'passed' && capture.fileName
          ? `${normalizedBase}/${encodePathSegments(validateProofUploadRelativePath(capture.fileName))}`
          : capture.url,
    })),
  };
}

export function renderComment({ marker, manifest }) {
  const lines = [
    marker,
    '## Proof of implementation',
    '',
    `Commit: \`${manifest.commit}\``,
    `Run: \`${manifest.runId}\``,
  ];

  if (manifest.publicLiveCapture) {
    lines.push('', '**PUBLIC LIVE CAPTURE:** one or more screenshots came from live state.');
  }

  // One vertical block per capture. A table wastes horizontal space (the
  // scarce axis in a PR comment), so labels stack above a full-width image.
  for (const capture of manifest.captures) {
    const body =
      capture.status === 'passed'
        ? `![${escapeMarkdown(capture.title)}](${capture.url})`
        : escapeMarkdown(capture.error ?? 'capture failed');
    lines.push(
      '',
      `### ${escapeMarkdown(capture.title)}`,
      '',
      // Two trailing spaces force a hard line break so the labels stack.
      `**Surface:** ${escapeMarkdown(capture.surface)}  `,
      `**Status:** ${escapeMarkdown(capture.status)}`,
      '',
      body,
    );
  }

  lines.push('', `Manifest: ${manifest.publicBaseUrl}/manifest.json`);
  return `${lines.join('\n')}\n`;
}

function uniqueInCatalogOrder(recipes, selectedIds) {
  const selected = new Set(selectedIds);
  const emitted = new Set();
  const out = [];
  for (const recipe of recipes) {
    if (selected.has(recipe.id) && !emitted.has(recipe.id)) {
      emitted.add(recipe.id);
      out.push(recipe);
    }
  }
  return out;
}

function encodePathSegments(fileName) {
  return String(fileName)
    .split('/')
    .map((segment) => encodeURIComponent(segment))
    .join('/');
}

function escapeMarkdown(value) {
  return String(value ?? '')
    .replaceAll('|', '\\|')
    .replaceAll('\n', ' ');
}

export function parseProofArgs(argv) {
  const [firstArg, ...tail] = argv;
  const command = firstArg && !firstArg.startsWith('--') ? firstArg : 'run';
  const rest = command === firstArg ? tail : argv;
  const parsed = {
    command,
    recipes: [],
    changedFiles: [],
    dryRun: false,
  };

  for (let i = 0; i < rest.length; i += 1) {
    const arg = rest[i];
    if (arg === '--recipe') {
      parsed.recipes.push(requireValue(rest, i, arg));
      i += 1;
      continue;
    }
    if (arg === '--changed-file') {
      parsed.changedFiles.push(requireValue(rest, i, arg));
      i += 1;
      continue;
    }
    if (arg === '--dry-run') {
      parsed.dryRun = true;
      continue;
    }
    throw new Error(`unknown proof argument: ${arg}`);
  }

  return parsed;
}

function requireValue(args, index, flag) {
  const value = args[index + 1];
  if (!value || value.startsWith('--')) {
    throw new Error(`${flag} requires a value`);
  }
  return value;
}

export function terminalRenderCommand({ input, output, title }) {
  return [
    'pnpm',
    [
      '--dir',
      'services/web',
      'exec',
      'node',
      '../../scripts/proof-render-terminal.mjs',
      '--input',
      input,
      '--output',
      output,
      '--title',
      title,
    ],
  ];
}

export function browserCaptureCommand({ surface, recipePath, outputDir }) {
  const serviceDir = surface === 'marketing' ? 'services/marketing' : 'services/web';
  return [
    'pnpm',
    [
      '--dir',
      serviceDir,
      'exec',
      'node',
      '../../scripts/proof-playwright-runner.mjs',
      '--surface',
      surface,
      '--recipe',
      relativeFromService(serviceDir, recipePath),
      '--output-dir',
      relativeFromService(serviceDir, outputDir),
    ],
  ];
}

export function tuiCaptureCommand({ recipePath, outputDir }) {
  return [
    'go',
    ['test', './internal/tuitest', '-run', '^TestProofCapture$', '-count=1', '-timeout', '120s'],
    {
      cwd: 'services/boss',
      env: {
        BOSS_PROOF_RECIPE: relativeFromService('services/boss', recipePath),
        BOSS_PROOF_OUTPUT_DIR: relativeFromService('services/boss', outputDir),
      },
    },
  ];
}

export function proofRunPaths({ prNumber, commit, runId, token }) {
  // `token` is an optional random segment that keeps the public URL from being
  // fully derivable from the PR number and commit alone. The local and public
  // paths stay parallel so uploadBundle's relative-path mapping holds.
  const suffix = token
    ? `pr-${prNumber}/${commit}/${runId}/${token}`
    : `pr-${prNumber}/${commit}/${runId}`;
  return {
    localDir: `.proof/${suffix}`,
    publicPrefix: `proof/bossanova/${suffix}`,
  };
}

export function r2UploadCommand({ bucket, key, file, contentType }) {
  return [
    'pnpm',
    [
      'dlx',
      'wrangler@3.90.0',
      'r2',
      'object',
      'put',
      `${bucket}/${key}`,
      '--file',
      file,
      '--content-type',
      contentType,
    ],
  ];
}

export function githubCommentCommand({ prNumber, bodyFile }) {
  return ['gh', ['pr', 'comment', String(prNumber), '--body-file', bodyFile]];
}

// Hidden marker embedded in every proof comment so prior runs can be found and
// collapsed. Keep in sync with the marker passed to renderComment.
export function proofCommentMarker(prNumber) {
  return `<!-- bossanova-proof:pr-${prNumber} -->`;
}

// Lists a PR's comments as JSON (id + body + isMinimized) for upsert lookup.
export function listProofCommentsCommand({ prNumber }) {
  return ['gh', ['pr', 'view', String(prNumber), '--json', 'comments']];
}

const MINIMIZE_COMMENT_MUTATION =
  'mutation($id:ID!){minimizeComment(input:{subjectId:$id,classifier:OUTDATED})' +
  '{minimizedComment{isMinimized}}}';

// Collapses a comment as "Outdated" via GitHub's GraphQL minimizeComment
// mutation. commentId is the GraphQL node id (the `IC_…` id from listProofComments).
export function minimizeCommentCommand({ commentId }) {
  return [
    'gh',
    ['api', 'graphql', '-f', `query=${MINIMIZE_COMMENT_MUTATION}`, '-f', `id=${commentId}`],
  ];
}

// Pure selector: given the JSON printed by listProofCommentsCommand, return the
// node ids of proof comments that carry `marker` and are not already hidden.
// Used to collapse prior runs before posting the fresh comment.
export function selectOutdatedProofCommentIds({ commentsJson, marker }) {
  let parsed;
  try {
    parsed = typeof commentsJson === 'string' ? JSON.parse(commentsJson) : commentsJson;
  } catch {
    return [];
  }
  const comments = parsed?.comments ?? [];
  return comments
    .filter((c) => c && !c.isMinimized && typeof c.body === 'string' && c.body.includes(marker))
    .map((c) => c.id)
    .filter(Boolean);
}

// Crop blank rows from the top and bottom of a captured terminal screen so the
// rendered PNG sizes to its actual content. The TUI always dumps a full
// fixed-height screen (e.g. 36 rows), leaving short screens with large blank
// margins. Internal blank lines (between sections) are preserved.
export function trimTerminalBlankLines(text) {
  const lines = String(text ?? '').split('\n');
  let start = 0;
  let end = lines.length;
  while (start < end && lines[start].trim() === '') {
    start += 1;
  }
  while (end > start && lines[end - 1].trim() === '') {
    end -= 1;
  }
  return lines.slice(start, end).join('\n');
}

export function proofUploadFiles({ manifest, localDir }) {
  const files = [
    {
      file: path.join(localDir, 'manifest.json'),
      relative: 'manifest.json',
      contentType: 'application/json',
    },
  ];

  for (const capture of manifest.captures ?? []) {
    if (capture.status !== 'passed' || !capture.fileName) {
      continue;
    }

    if (!String(capture.fileName).endsWith('.png')) {
      continue;
    }
    const relative = validateProofUploadRelativePath(capture.fileName);

    files.push({
      file: path.join(localDir, ...relative.split('/')),
      relative,
      contentType: 'image/png',
    });
  }

  return files;
}

export function validateRecipeId(id) {
  const value = String(id ?? '');
  if (!/^[a-z0-9][a-z0-9-]*$/.test(value)) {
    throw new Error(`invalid proof recipe id: ${value || '<missing>'}`);
  }
  return value;
}

export function validateProofUploadRelativePath(fileName) {
  const relative = String(fileName ?? '').replaceAll('\\', '/');
  const segments = relative.split('/');
  if (
    !relative.endsWith('.png') ||
    relative.startsWith('/') ||
    /^[A-Za-z]:\//.test(relative) ||
    segments.some((segment) => !segment || segment === '.' || segment === '..')
  ) {
    throw new Error(`invalid proof upload path: ${relative || '<missing>'}`);
  }
  return relative;
}

export function validateBrowserRoute(route) {
  const value = String(route ?? '');
  if (
    !value.startsWith('/') ||
    value.startsWith('//') ||
    value.includes('\\') ||
    /[\u0000-\u001F\u007F]/.test(value)
  ) {
    throw new Error(`proof browser route must be relative: ${value || '<missing>'}`);
  }
  return value;
}

function relativeFromService(serviceDir, target) {
  if (!target.startsWith('.')) {
    return target;
  }
  return path.posix.relative(serviceDir, target.replaceAll('\\', '/'));
}
