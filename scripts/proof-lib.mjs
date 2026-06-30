#!/usr/bin/env node

import path from 'node:path'

// Single source of truth for proof artifact media types. Adding a new
// uploadable artifact type is one entry here — the upload content-type, the
// manifest URL encoding, and the path validator all derive from this map.
export const PROOF_MEDIA_TYPES = {
  png: 'image/png',
  webm: 'video/webm',
  gif: 'image/gif',
  mp4: 'video/mp4',
}

/** True when the media type is a video (webm or mp4). */
function isVideoMediaType(mt) {
  return mt === 'webm' || mt === 'mp4'
}

export function mediaTypeForPath(fileName) {
  const ext = String(fileName ?? '')
    .toLowerCase()
    .split('.')
    .pop()
  const contentType = PROOF_MEDIA_TYPES[ext]
  if (!contentType) {
    throw new Error(`unsupported proof media extension: ${ext || '<none>'}`)
  }
  return contentType
}

export function normalizeChangedFiles(files) {
  return files
    .filter((file) => file !== null && file !== undefined)
    .map((file) => String(file).trim().replaceAll('\\', '/').replace(/^\.\//, ''))
    .filter(Boolean)
}

/**
 * Normalizes a recipe so browser (web/marketing/docs) surfaces prefer video.
 * - TUI is agent-only (no TUI recipes in the catalog); any stray surface:'tui'
 *   recipe passes through unchanged — browser-style video normalization never
 *   applies to it.
 * - A browser recipe with capture unset becomes a video; capture 'image' or
 *   'still' is preserved verbatim (it is not 'video', so every downstream
 *   `capture === 'video'` check reads it as a still). Preserving the opt-out
 *   value — rather than stripping the key — keeps the function idempotent: the
 *   runner re-normalizes the recipe.json that proof.mjs already wrote, and a
 *   stripped key would re-default an explicit `capture: 'still'` recipe to video.
 * - A resulting video recipe with no non-empty steps but a `route` gets default
 *   steps: goto the route, settle, then scroll the whole page (so the full page
 *   is always captured). The poster .png the video branch emits is the "both" still.
 * Pure and idempotent.
 * @param {object} recipe
 * @returns {object}
 */
export function normalizeRecipe(recipe) {
  if (!recipe || recipe.surface === 'tui') {
    return recipe
  }
  const optedOutStill = recipe.capture === 'still' || recipe.capture === 'image'
  if (optedOutStill) {
    return recipe
  }
  const hasSteps = Array.isArray(recipe.steps) && recipe.steps.length > 0
  const normalized = { ...recipe, capture: 'video' }
  if (!hasSteps && recipe.route) {
    normalized.steps = [
      { action: 'goto', route: recipe.route, caption: recipe.title },
      { action: 'wait', timeoutMs: 800 },
      { action: 'scroll', fullPage: true, caption: 'Scroll through the page' },
    ]
  }
  return normalized
}

/**
 * Path prefixes that identify a TUI (boss) change. BOS-115 makes the TUI proof
 * surface agent-only and catalog-independent: surface detection keys off these
 * prefixes rather than the (now-removed) TUI recipe pathRules. Kept as a single
 * exported source of truth so the classifier and its test cannot drift.
 * @type {readonly string[]}
 */
export const TUI_SURFACE_PREFIXES = [
  'services/boss/internal/views/',
  'services/boss/internal/tuitest/',
  'services/boss/internal/tuidriver/',
  'services/boss/internal/fixtures/',
  'services/boss/internal/client/',
  'services/boss/cmd/',
  'proto/',
]

/**
 * Pure path classifier: true when ANY changed file lives under a TUI prefix.
 * Independent of the recipe catalog, so a boss-only diff routes to the agentic
 * TUI proof path even though the catalog no longer holds any TUI recipes.
 *
 * Note: a mixed diff that touches BOTH a TUI prefix and a web/marketing/docs
 * surface classifies as TUI (any-match wins). This differs from the old
 * recipe-surface rule (which only inferred TUI when every matched recipe was
 * TUI); it is intentional — a PR that changes a TUI view should prove that view
 * via the agent rather than fall through to the web/recipe path.
 * @param {string[]|null|undefined} changedFiles
 * @returns {boolean}
 */
export function classifyTuiSurface(changedFiles) {
  const normalized = normalizeChangedFiles(changedFiles ?? [])
  return normalized.some((file) => TUI_SURFACE_PREFIXES.some((prefix) => file.startsWith(prefix)))
}

export function selectRecipes(catalog, changedFiles, explicitRecipeIds = []) {
  const recipesById = new Map(catalog.recipes.map((recipe) => [recipe.id, recipe]))
  const selectedIds = []

  if (explicitRecipeIds.length > 0) {
    for (const id of explicitRecipeIds) {
      if (!recipesById.has(id)) {
        throw new Error(`unknown proof recipe: ${id}`)
      }
      selectedIds.push(id)
    }
    return uniqueInCatalogOrder(catalog.recipes, selectedIds)
  }

  const normalized = normalizeChangedFiles(changedFiles)
  for (const rule of catalog.pathRules) {
    if (normalized.some((file) => rule.patterns.some((pattern) => file.startsWith(pattern)))) {
      for (const id of rule.recipeIds) {
        if (!recipesById.has(id)) {
          throw new Error(`unknown proof recipe: ${id}`)
        }
        selectedIds.push(id)
      }
    }
  }

  return uniqueInCatalogOrder(catalog.recipes, selectedIds)
}

export function buildManifest({
  commit,
  prNumber,
  runId,
  publicBaseUrl,
  captures,
  agentRunnerStubbed,
  title,
  verdict,
  genAiLive = false,
  agentSummary = null,
  brief = null,
}) {
  const normalizedBase = publicBaseUrl.replace(/\/$/, '')
  const publicLiveCapture = captures.some((capture) => capture.privacy === 'live')
  return {
    version: 1,
    generatedAt: new Date().toISOString(),
    commit,
    prNumber: String(prNumber),
    runId,
    ...(title ? { title } : {}),
    ...(verdict ? { verdict } : {}),
    genAiLive,
    ...(agentSummary ? { agentSummary } : {}),
    ...(brief ? { brief } : {}),
    publicBaseUrl: normalizedBase,
    publicLiveCapture,
    ...(agentRunnerStubbed ? { agentRunnerStubbed: true } : {}),
    captures: captures.map((capture) => {
      const mediaType = capture.mediaType ?? 'png'
      // Normalize captured stills to public URLs whenever they exist, even when
      // the primary media is missing (e.g. video conversion failed but Playwright
      // still produced screenshots) — those stills are uploaded, so they must
      // stay linkable evidence instead of being dropped.
      const stills =
        Array.isArray(capture.stills) && capture.stills.length > 0
          ? capture.stills.map((still) => ({
              ...still,
              url: `${normalizedBase}/${encodePathSegments(validateProofUploadRelativePath(still.fileName))}`,
            }))
          : null
      if (!capture.fileName) {
        return { ...capture, mediaType, ...(stills ? { stills } : {}) }
      }
      // Surface media URLs whenever file names exist, regardless of status, so
      // Unsatisfactory agent runs are reviewable without re-running.
      const url = `${normalizedBase}/${encodePathSegments(validateProofUploadRelativePath(capture.fileName))}`
      const out = { ...capture, mediaType, url }
      if (isVideoMediaType(capture.mediaType)) {
        out.videoUrl = url
        if (capture.posterFileName) {
          out.posterUrl = `${normalizedBase}/${encodePathSegments(validateProofUploadRelativePath(capture.posterFileName))}`
        }
      }
      if (stills) {
        out.stills = stills
      }
      return out
    }),
  }
}

/**
 * Evidence/Confidence verdict block (ported from wc-proof lib/publish.mjs).
 * Evidence is Satisfactory only when the run passed AND media exists.
 * Confidence is Low when not satisfactory, Medium when a gen-AI feature was
 * demoed UI-only (brief.genAi && !genAiLive), else High.
 * @param {{verdict?:string, captures?:Array<{fileName?:string,posterFileName?:string,stills?:Array<object>}>, genAiLive?:boolean, brief?:{genAi?:boolean}}} manifest
 * @returns {{evidence:string, confidence:string, evidenceOk:boolean, confidenceOk:boolean}}
 */
export function deriveVerdictBlock(manifest) {
  const hasMedia = (manifest.captures ?? []).some((c) =>
    Boolean(c.fileName || c.posterFileName || (c.stills?.length ?? 0) > 0),
  )
  const satisfactory = manifest.verdict === 'passed' && hasMedia
  const evidence = satisfactory ? 'Satisfactory' : 'Unsatisfactory'
  let confidence = 'Low'
  if (satisfactory) confidence = manifest.brief?.genAi && !manifest.genAiLive ? 'Medium' : 'High'
  return {
    evidence,
    confidence,
    evidenceOk: evidence !== 'Unsatisfactory',
    confidenceOk: confidence !== 'Low',
  }
}

/** The two ✅/❌ Evidence + Confidence lines, shared by the comment and the gallery README. */
export function verdictBlockLines(manifest) {
  const v = deriveVerdictBlock(manifest)
  return [
    `${v.evidenceOk ? '✅' : '❌'} **Evidence:** ${v.evidence}`,
    '',
    `${v.confidenceOk ? '✅' : '❌'} **Confidence:** ${v.confidence}`,
  ]
}

/**
 * Minimal sticky PR comment: a single prominent gallery link, the title, the
 * run metadata line, an optional agent summary, and the Evidence/Confidence
 * block. No inline media (the gallery README is the clickthrough). No Verdict
 * line (it dupes Evidence).
 */
export function renderComment({ marker, manifest }) {
  const lines = [marker]
  if (manifest.reportUrl) lines.push(`### [📸 Proof gallery](${manifest.reportUrl})`, '')
  else if (manifest.publicBaseUrl)
    lines.push(`### [📸 Proof manifest](${manifest.publicBaseUrl}/manifest.json)`, '')
  lines.push(`**${escapeMarkdown(manifest.title ?? 'Proof of implementation')}**`, '')
  const genAi = manifest.genAiLive ? 'live providers' : 'not live (UI-only demo)'
  lines.push(
    `**Commit:** \`${manifest.commit}\`  **Run:** ${manifest.runId}  **Gen-AI:** ${genAi}`,
    '',
  )
  if (manifest.publicLiveCapture) {
    lines.push('**PUBLIC LIVE CAPTURE:** one or more screenshots came from live state.', '')
  }
  if (manifest.agentSummary) {
    lines.push(
      '<details><summary>Agent summary</summary>',
      '',
      manifest.agentSummary,
      '',
      '</details>',
      '',
    )
  }
  for (const l of verdictBlockLines(manifest)) lines.push(l)
  return `${lines.join('\n')}\n`
}

/**
 * Neutral "proof deferred" comment for environment limitations. Unlike
 * renderComment it never emits the ❌ Evidence/Confidence verdict block — a
 * deferral is not a failed change.
 * @param {{ marker: string, manifest: object, reasonCode: string, recaptureHint?: string }} opts
 * @returns {string}
 */
/**
 * The human-readable body sentence for a deferred-proof reason. The TUI
 * agent-only path (BOS-115) surfaces a clear, actionable message when no API
 * key is present; every other reason keeps the generic environment-limitation
 * wording.
 * @param {string} reasonCode
 * @returns {string}
 */
function deferredReasonMessage(reasonCode) {
  if (reasonCode === 'agent-unavailable') {
    return (
      'agentic TUI proof unavailable: agent mode is not enabled in this environment ' +
      '(set PROOF_ANTHROPIC_API_KEY, or unset BOSS_PROOF_MODE=recipe). ' +
      'The TUI surface is proved by an agent driving the real TUI, so there is no recipe ' +
      'fallback. This is an environment limitation, not a problem with the change.'
    )
  }
  return `bs-proof could not complete the agent proof this run (${reasonCode}). This is an environment limitation, not a problem with the change.`
}

export function renderDeferredComment({ marker, manifest, reasonCode, recaptureHint }) {
  const lines = [marker]
  if (manifest.reportUrl) lines.push(`### [📸 Proof gallery](${manifest.reportUrl})`, '')
  else if (manifest.publicBaseUrl)
    lines.push(`### [📸 Proof manifest](${manifest.publicBaseUrl}/manifest.json)`, '')
  else lines.push('')
  lines.push('### Proof deferred')
  lines.push('')
  lines.push(deferredReasonMessage(reasonCode))
  if (manifest.commit) lines.push('', `**Commit:** \`${manifest.commit}\``)
  if (recaptureHint) {
    lines.push('', 'Re-capture from a dev environment:', '', '```bash', recaptureHint, '```')
  }
  return `${lines.join('\n')}\n`
}

/**
 * Orders captures for the gallery report: video captures first (so the most
 * informative artifact leads), then the rest. Stable within each group. Pure.
 * @param {Array<{mediaType?: string}>} captures
 * @returns {Array<object>}
 */
export function orderCapturesForReport(captures) {
  const list = captures ?? []
  const videos = list.filter((c) => isVideoMediaType(c.mediaType))
  const rest = list.filter((c) => !isVideoMediaType(c.mediaType))
  return [...videos, ...rest]
}

/**
 * Renders a Markdown README for the bs-proof report repo. Pure — no fs/env/Date.
 * @param {{ manifest: object }} opts
 * @returns {string}
 */
export function renderGallery({ manifest }) {
  const captureCount = (manifest.captures ?? []).length
  const lines = [
    `# Proof report — PR ${manifest.prNumber}`,
    '',
    `- Commit: \`${manifest.commit}\``,
    `- Run: \`${manifest.runId}\``,
    `- Generated: ${manifest.generatedAt}`,
    `- Captures: ${captureCount}`,
  ]

  lines.push('')
  for (const l of verdictBlockLines(manifest)) lines.push(l)
  if (manifest.agentRunnerStubbed)
    lines.push('', '_agent-runner stubbed; UI + orchestration + persistence exercised._')

  // Render a list of stills as blank-line-separated markdown images.
  const pushStills = (stills) => {
    for (let i = 0; i < stills.length; i++) {
      lines.push(`![${escapeMarkdown(stills[i].label)}](${stills[i].url})`)
      if (i < stills.length - 1) {
        lines.push('')
      }
    }
  }

  for (const capture of orderCapturesForReport(manifest.captures)) {
    lines.push('', `## ${escapeMarkdown(capture.title)}`, '')
    lines.push(
      `**Surface:** ${escapeMarkdown(capture.surface)} · **Status:** ${escapeMarkdown(capture.status)}`,
      '',
    )

    if (capture.status !== 'passed') {
      lines.push(escapeMarkdown(capture.error ?? 'capture failed'))
    }

    const stills = capture.stills ?? []
    if (isVideoMediaType(capture.mediaType) && (capture.videoUrl || capture.url)) {
      const poster = capture.posterUrl ?? capture.url
      if (capture.status !== 'passed') lines.push('')
      lines.push(
        `[![${escapeMarkdown(capture.title)}](${poster})](${capture.videoUrl ?? capture.url})`,
        '',
        '▶ Video',
      )
      if (stills.length > 0) {
        lines.push('', '### Step screenshots', '')
        pushStills(stills)
      }
    } else if (capture.url) {
      if (capture.status !== 'passed') lines.push('')
      lines.push(`![${escapeMarkdown(capture.title)}](${capture.url})`)
    } else if (stills.length > 0) {
      // No linkable primary media (e.g. video conversion failed) but Playwright
      // captured stills — surface them directly so there is still linked evidence.
      if (capture.status !== 'passed') lines.push('')
      pushStills(stills)
    }
  }

  return `${lines.join('\n')}\n`
}

function uniqueInCatalogOrder(recipes, selectedIds) {
  const selected = new Set(selectedIds)
  const emitted = new Set()
  const out = []
  for (const recipe of recipes) {
    if (selected.has(recipe.id) && !emitted.has(recipe.id)) {
      emitted.add(recipe.id)
      out.push(recipe)
    }
  }
  return out
}

function encodePathSegments(fileName) {
  return String(fileName)
    .split('/')
    .map((segment) => encodeURIComponent(segment))
    .join('/')
}

function escapeMarkdown(value) {
  return String(value ?? '')
    .replaceAll('|', '\\|')
    .replaceAll('\n', ' ')
}

export function parseProofArgs(argv) {
  const [firstArg, ...tail] = argv
  // A completely empty invocation prints usage rather than silently running a
  // real (uploading, comment-posting) capture. Flags-only invocations still
  // default to `run` so documented `--recipe`/`--dry-run` usage is unchanged.
  const command =
    argv.length === 0 ? 'help' : firstArg && !firstArg.startsWith('--') ? firstArg : 'run'
  const rest = command === firstArg ? tail : argv
  const parsed = {
    command,
    recipes: [],
    changedFiles: [],
    dryRun: false,
  }

  for (let i = 0; i < rest.length; i += 1) {
    const arg = rest[i]
    if (arg === '--recipe') {
      parsed.recipes.push(requireValue(rest, i, arg))
      i += 1
      continue
    }
    if (arg === '--changed-file') {
      parsed.changedFiles.push(requireValue(rest, i, arg))
      i += 1
      continue
    }
    if (arg === '--dry-run') {
      parsed.dryRun = true
      continue
    }
    throw new Error(`unknown proof argument: ${arg}`)
  }

  return parsed
}

function requireValue(args, index, flag) {
  const value = args[index + 1]
  if (!value || value.startsWith('--')) {
    throw new Error(`${flag} requires a value`)
  }
  return value
}

/**
 * Bound a per-turn narration string for display as a single-line proof caption.
 * Mirrors the TS overlay's formatCaption (services/web/tests/e2e/agent/overlay.ts)
 * EXACTLY: collapse all whitespace runs to single spaces, trim, and truncate to
 * 140 chars with a U+2026 ellipsis ('…', length 1) so a truncated result is
 * exactly 140 chars. Whitespace-only/empty/nullish input collapses to ''. This
 * is display-only — secret scanning must always see the full untruncated text.
 * @param {unknown} text
 * @returns {string}
 */
export function formatCaption(text) {
  const collapsed = String(text ?? '')
    .trim()
    .replace(/\s+/g, ' ')
  return collapsed.length <= 140 ? collapsed : `${collapsed.slice(0, 139)}…`
}

export function terminalRenderCommand({ input, output, title, caption }) {
  const args = [
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
  ]
  // Append the caption only when it is a non-empty string so the no-caption
  // invocation stays byte-identical (backward compatible).
  if (typeof caption === 'string' && caption.length > 0) {
    args.push('--caption', caption)
  }
  return ['pnpm', args]
}

export function tuiAgentBridgeBuildCommand({ outBin }) {
  return [
    'go',
    ['build', '-tags', 'e2e', '-o', outBin, './cmd/proof-tui-agent'],
    { cwd: 'services/boss' },
  ]
}

export function bossE2eBuildCommand({ outBin }) {
  return ['go', ['build', '-tags', 'e2e', '-o', outBin, './cmd'], { cwd: 'services/boss' }]
}

// Maps a browser proof surface to the package that owns its Playwright config
// and dev/preview webServer: docs → Docusaurus site, marketing → Astro site,
// anything else → the Vite web app.
export function browserServiceDir(surface) {
  if (surface === 'marketing') return 'services/marketing'
  if (surface === 'docs') return 'services/docs'
  return 'services/web'
}

export function browserCaptureCommand({ surface, recipePath, outputDir }) {
  const serviceDir = browserServiceDir(surface)
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
  ]
}

export function introCardCommand({ surface, out, width, height, label, title = '' }) {
  const serviceDir = browserServiceDir(surface)
  return [
    'pnpm',
    [
      '--dir',
      serviceDir,
      'exec',
      'node',
      '../../scripts/proof-render-intro-card.mjs',
      '--out',
      relativeFromService(serviceDir, out),
      '--width',
      String(width),
      '--height',
      String(height),
      '--label',
      label,
      '--title',
      title,
    ],
  ]
}

/**
 * Ancestor directories of a proof run dir, deepest first, ending at the
 * `.proof` dir itself (the run dir's own path is NOT included). Returns []
 * when the path has no `.proof` segment. Pure — string-only.
 * @param {string} localDir
 * @returns {string[]}
 */
export function proofAncestorDirs(localDir) {
  const parts = String(localDir ?? '')
    .replaceAll('\\', '/')
    .replace(/\/+$/, '')
    .split('/')
  const proofIdx = parts.indexOf('.proof')
  if (proofIdx === -1) return []
  const out = []
  for (let end = parts.length - 1; end > proofIdx; end -= 1) {
    out.push(parts.slice(0, end).join('/'))
  }
  return out
}

/**
 * Resolves the recipe-catalog path. An `override` (typically from
 * `BOSS_PROOF_CATALOG`) lets an ad-hoc/experimental recipe set be trialled
 * without editing the committed catalog: an absolute path is used as-is, a
 * relative one is resolved against the current working directory. With no
 * override, falls back to `proof/recipes/default.json` under `repoRoot`. Pure —
 * no fs/env/Date (the env value is passed in by the caller).
 * @param {string} repoRoot
 * @param {string} [override]
 * @returns {string}
 */
export function resolveCatalogPath(repoRoot, override) {
  const trimmed = String(override ?? '').trim()
  if (trimmed) {
    return path.isAbsolute(trimmed) ? trimmed : path.resolve(trimmed)
  }
  return path.join(repoRoot, 'proof', 'recipes', 'default.json')
}

export function proofRunPaths({ prNumber, commit, runId, token }) {
  // `token` is an optional random segment that keeps the public URL from being
  // fully derivable from the PR number and commit alone. The local and public
  // paths stay parallel so uploadBundle's relative-path mapping holds.
  const suffix = token
    ? `pr-${prNumber}/${commit}/${runId}/${token}`
    : `pr-${prNumber}/${commit}/${runId}`
  return {
    localDir: `.proof/${suffix}`,
    publicPrefix: `proof/bossanova/${suffix}`,
  }
}

export function r2UploadCommand({ bucket, key, file, contentType }) {
  return [
    'pnpm',
    [
      'dlx',
      // Pinned to a v4 line: v3 prints an "out-of-date, update to v4" warning on
      // every object put. NOTE: v4 changed `r2 object put` to default to the
      // LOCAL miniflare store — `--remote` is REQUIRED to write the real bucket
      // (without it the upload silently lands in a local simulator and the public
      // URL 404s).
      'wrangler@4.42.0',
      'r2',
      'object',
      'put',
      `${bucket}/${key}`,
      '--file',
      file,
      '--content-type',
      contentType,
      '--remote',
    ],
  ]
}

export function githubCommentCommand({ prNumber, bodyFile }) {
  return ['gh', ['pr', 'comment', String(prNumber), '--body-file', bodyFile]]
}

// Hidden marker embedded in every proof comment so prior runs can be found and
// collapsed. Keep in sync with the marker passed to renderComment.
export function proofCommentMarker(prNumber) {
  return `<!-- bossanova-proof:pr-${prNumber} -->`
}

// Lists a PR's comments as JSON (id + body + isMinimized) for upsert lookup.
export function listProofCommentsCommand({ prNumber }) {
  return ['gh', ['pr', 'view', String(prNumber), '--json', 'comments']]
}

const MINIMIZE_COMMENT_MUTATION =
  'mutation($id:ID!){minimizeComment(input:{subjectId:$id,classifier:OUTDATED})' +
  '{minimizedComment{isMinimized}}}'

// Collapses a comment as "Outdated" via GitHub's GraphQL minimizeComment
// mutation. commentId is the GraphQL node id (the `IC_…` id from listProofComments).
export function minimizeCommentCommand({ commentId }) {
  return [
    'gh',
    ['api', 'graphql', '-f', `query=${MINIMIZE_COMMENT_MUTATION}`, '-f', `id=${commentId}`],
  ]
}

// Pure selector: given the JSON printed by listProofCommentsCommand, return the
// node ids of proof comments that carry `marker` and are not already hidden.
// Used to collapse prior runs before posting the fresh comment.
export function selectOutdatedProofCommentIds({ commentsJson, marker }) {
  let parsed
  try {
    parsed = typeof commentsJson === 'string' ? JSON.parse(commentsJson) : commentsJson
  } catch {
    return []
  }
  const comments = parsed?.comments ?? []
  return comments
    .filter((c) => c && !c.isMinimized && typeof c.body === 'string' && c.body.includes(marker))
    .map((c) => c.id)
    .filter(Boolean)
}

// Crop blank rows from the top and bottom of a captured terminal screen so the
// rendered PNG sizes to its actual content. The TUI always dumps a full
// fixed-height screen (e.g. 36 rows), leaving short screens with large blank
// margins. Internal blank lines (between sections) are preserved.
export function trimTerminalBlankLines(text) {
  const lines = String(text ?? '').split('\n')
  let start = 0
  let end = lines.length
  while (start < end && lines[start].trim() === '') {
    start += 1
  }
  while (end > start && lines[end - 1].trim() === '') {
    end -= 1
  }
  return lines.slice(start, end).join('\n')
}

export function proofUploadFiles({ manifest, localDir }) {
  const files = [
    {
      file: path.join(localDir, 'manifest.json'),
      relative: 'manifest.json',
      contentType: 'application/json',
    },
  ]

  const pushArtifact = (fileName) => {
    if (!fileName) return
    const relative = validateProofUploadRelativePath(fileName)
    files.push({
      file: path.join(localDir, ...relative.split('/')),
      relative,
      contentType: mediaTypeForPath(relative),
    })
  }

  for (const capture of manifest.captures ?? []) {
    // Upload media whenever a fileName is present (passed OR failed), so
    // Unsatisfactory agent runs remain reviewable. Recipe failures never reach
    // this path with a fileName because proof.mjs deletes their artifacts
    // upstream before buildManifest is called.
    pushArtifact(capture.fileName)
    pushArtifact(capture.posterFileName)
    for (const still of capture.stills ?? []) {
      pushArtifact(still.fileName)
    }
  }

  return files
}

export function validateRecipeId(id) {
  const value = String(id ?? '')
  if (!/^[a-z0-9][a-z0-9-]*$/.test(value)) {
    throw new Error(`invalid proof recipe id: ${value || '<missing>'}`)
  }
  return value
}

export function validateProofUploadRelativePath(fileName) {
  const relative = String(fileName ?? '').replaceAll('\\', '/')
  const segments = relative.split('/')
  const ext = relative.toLowerCase().split('.').pop()
  if (
    !Object.prototype.hasOwnProperty.call(PROOF_MEDIA_TYPES, ext) ||
    relative.startsWith('/') ||
    /^[A-Za-z]:\//.test(relative) ||
    segments.some((segment) => !segment || segment === '.' || segment === '..')
  ) {
    throw new Error(`invalid proof upload path: ${relative || '<missing>'}`)
  }
  return relative
}

export function validateBrowserRoute(route) {
  const value = String(route ?? '')
  if (
    !value.startsWith('/') ||
    value.startsWith('//') ||
    value.includes('\\') ||
    /[\u0000-\u001F\u007F]/.test(value)
  ) {
    throw new Error(`proof browser route must be relative: ${value || '<missing>'}`)
  }
  return value
}

function relativeFromService(serviceDir, target) {
  if (!target.startsWith('.')) {
    return target
  }
  return path.posix.relative(serviceDir, target.replaceAll('\\', '/'))
}
