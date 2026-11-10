/**
 * The proof page as data: every fact the HTML shows, assembled from the boss-proof/1 record and
 * its page input, plus an optional agent spec, media manifest and snippet resolver. Without the
 * optional parts the model is built from the record alone, deterministically.
 */
import { createHash } from 'node:crypto'
import { CONFIDENCE_NOT_COMPUTED } from './verify-proof.mjs'
import { validateProofSpec } from './verify-proof-spec.mjs'
import { validateProofMedia } from './verify-proof-media.mjs'
import { parseUnifiedDiff } from './verify-proof-snippet.mjs'

export const UI_AREA = 'ui'
export const NO_UI_RECORDING = 'No recording of the changed UI'
const TEST_PATH = /(_test\.go$|\.test\.|\.spec\.|__tests__\/|(^|\/)e2e\/)/
export const GENERATED_PATH =
  /((^|\/)gen(erated)?\/|_gen\.go$|\.pb\.go$|\.snap$|(^|\/)(pnpm-lock\.yaml|yarn\.lock|package-lock\.json|go\.sum)$)/

/**
 * @typedef {import('./verify-proof.mjs').Proof} Proof
 * @typedef {import('./verify-proof-page.mjs').PageInput} PageInput
 * @typedef {import('./verify-proof-spec.mjs').ProofSpec} ProofSpec
 * @typedef {import('./verify-proof-media.mjs').ProofMedia} ProofMedia
 * @typedef {import('./verify-proof-media.mjs').Clip} Clip
 * @typedef {import('./verify-proof-snippet.mjs').Snippet} Snippet
 * @typedef {import('./verify-proof-snippet.mjs').SnippetResolver} SnippetResolver
 * @typedef {import('./verify-proof-snippet.mjs').FileDiff} FileDiff
 * @typedef {{path: string, adds: number | null, dels: number | null}} ChangedFile
 * @typedef {{spec?: unknown, media?: unknown, mediaSize?: (file: string) => number | null, snippet?: SnippetResolver, files?: unknown}} PageExtras
 * @typedef {{title: string, detail?: string, href?: string, link?: string, proof?: boolean}} WaitingItem
 * @typedef {{level: string, title: string, body?: string, items?: string[], ifItMatters?: string, area?: string, path?: string, line?: number}} RiskItem
 * @typedef {{label: string, files: FileDiff[], text: string | null, remaining: number}} AreaExcerpt
 * @typedef {{kind: string, url: string, caption: string, inline: boolean, poster?: string}} AreaMedia
 * @typedef {{id: string, name: string, summary: string, facts: {text: string, flag: boolean}[], tag: string,
 *   snippets: (Snippet & {pinNote?: string})[], excerpts: AreaExcerpt[], media: AreaMedia[]}} AreaItem
 * @typedef {{clip: Clip, index: number}} HeadClip
 * @typedef {{base: import('./verify-proof-media.mjs').Still, head: import('./verify-proof-media.mjs').Still}} StillPair
 * @typedef {ReturnType<typeof buildPageModel>} PageModel
 */

/** The files the caller says the PR changes, keeping only well-formed entries.
 * @param {unknown} value @returns {ChangedFile[] | null} */
function changedFiles(value) {
  if (!Array.isArray(value)) return null
  const count = (/** @type {unknown} */ n) =>
    Number.isSafeInteger(n) && Number(n) >= 0 ? Number(n) : null
  return value
    .filter((f) => f && typeof f === 'object' && typeof f.path === 'string' && f.path)
    .map((f) => ({ path: f.path, adds: count(f.adds), dels: count(f.dels) }))
}

/** An area's facts, its API change class first. @param {import('./verify-proof.mjs').ProofArea} area */
export const areaItems = (area) => [
  ...(area.change
    ? [
        `API change: ${area.change}${area.change === 'breaking' && area.changeReasons?.length ? ` \u2014 ${area.changeReasons.join('; ')}` : ''}`,
      ]
    : []),
  ...area.items,
]

/** The id a path's quoted diff carries, so a risk can link to it. @param {string} path */
export const fileAnchor = (path) =>
  `f-${path
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '')}-${createHash('sha256').update(path).digest('hex').slice(0, 6)}`

/**
 * @param {{proof: Proof, input: PageInput, view: ReturnType<typeof import('./verify-proof-page.mjs').proofView>, extras?: PageExtras}} options
 */
export function buildPageModel({ proof, input, view, extras = {} }) {
  const files = changedFiles(extras.files)
  const areaIds = proof.areas.map((area) => area.name)
  const specResult =
    extras.spec === undefined
      ? null
      : validateProofSpec(extras.spec, {
          head: proof.head,
          changedPaths: files?.map((f) => f.path),
          areaIds,
        })
  const mediaResult =
    extras.media === undefined
      ? null
      : validateProofMedia(extras.media, { head: proof.head, sizeOf: extras.mediaSize })
  const spec = specResult?.spec ?? null
  const media = mediaResult?.media ?? null
  /** @type {string[]} */
  const problems = [
    ...(specResult?.dropped ?? []).map((d) => `spec ${d.at}: ${d.reason}`),
    ...(mediaResult?.dropped ?? []).map((d) => `media ${d.at}: ${d.reason}`),
  ]
  const askSection = view.sections.find((section) => section.title === 'Asks')
  const asks = new Map((askSection?.asks ?? []).map((ask, i) => [proof.confidence.asks[i], ask]))

  const areas = proof.areas.map((area) => {
    const specArea = spec?.areas.find((entry) => entry.id === area.name)
    const ask = asks.get(area.name)
    /** @type {AreaItem['snippets']} */
    const snippets = []
    for (const request of specArea?.snippets ?? []) {
      if (!extras.snippet) {
        problems.push(
          `${request.path}:${request.start}-${request.end}: no git to quote it from, snippet dropped`,
        )
        continue
      }
      const quoted = extras.snippet(request)
      problems.push(...quoted.problems)
      if (quoted.snippet)
        snippets.push(
          request.pinNote && quoted.snippet.pinLine !== null
            ? { ...quoted.snippet, pinNote: request.pinNote }
            : quoted.snippet,
        )
    }
    const items = ask?.items ?? areaItems(area)
    return {
      id: area.name,
      name: specArea?.name ?? area.label ?? area.name,
      summary:
        specArea?.summary ??
        (area.ask
          ? 'Asks for a person.'
          : area.proven
            ? 'Checked: proven.'
            : 'Checked: not shown.'),
      facts: specArea?.facts ?? items.map((text) => ({ text, flag: false })),
      tag: area.ask ? 'asks you' : 'checked',
      snippets,
      excerpts: (ask?.excerpts ?? []).map((excerpt) => {
        const parsed = excerpt.label === 'diff' ? parseUnifiedDiff(excerpt.lines.join('\n')) : []
        return {
          label: excerpt.label,
          files: parsed,
          text: parsed.length ? null : excerpt.lines.join('\n'),
          remaining: excerpt.remaining,
        }
      }),
      media: ask?.media ?? [],
    }
  })

  const heads = (media?.clips ?? []).filter((clip) => clip.side === 'head')
  const bases = (media?.clips ?? []).filter((clip) => clip.side === 'base')
  /** @type {StillPair[]} */
  const pairs = bases.flatMap((base, i) =>
    (base.stills ?? []).flatMap((still, j) => {
      const head = heads[i]?.stills[j]
      return head ? [{ base: still, head }] : []
    }),
  )
  const recordVideo = proof.areas.some((area) =>
    area.media.some((m) => m.kind === 'video' && m.side !== 'base'),
  )
  const uiChanged = proof.areas.some((area) => area.name === UI_AREA)

  /** @type {WaitingItem[]} */
  const waiting = []
  if (uiChanged && heads.length === 0 && !recordVideo)
    waiting.push({
      title: NO_UI_RECORDING,
      detail: 'The UI area changed, and no clip shows it running at this head.',
      href: '#evidence',
      link: 'Evidence',
      proof: true,
    })
  const label = (/** @type {string} */ id) => areas.find((area) => area.id === id)?.name ?? id
  // The record's own reasons and blockers always show; an agent spec only adds to them.
  for (const blocker of proof.review?.blockers ?? [])
    waiting.push({ title: blocker.title, ...(blocker.where ? { detail: blocker.where } : {}) })
  for (const reason of proof.confidence.loweredBy)
    if (reason.code !== CONFIDENCE_NOT_COMPUTED) waiting.push({ title: reason.text })
  for (const item of spec?.waiting ?? [])
    waiting.push({
      title: item.title,
      ...(item.detail ? { detail: item.detail } : {}),
      ...(item.area ? { href: `#a-${item.area}`, link: label(item.area) } : {}),
    })

  /** @type {RiskItem[]} */
  const risks = [
    ...proof.areas.flatMap((area) => [
      ...(area.change === 'breaking'
        ? [
            {
              level: 'high',
              title: `Breaking API change in ${area.label ?? area.name}`,
              ...(area.changeReasons?.length ? { items: [...area.changeReasons] } : {}),
              area: area.label ?? area.name,
            },
          ]
        : []),
      ...(area.unmentioned?.length
        ? [
            {
              level: 'medium',
              title: `Not in the description: ${area.label ?? area.name}`,
              items: [...area.unmentioned],
              area: area.label ?? area.name,
            },
          ]
        : []),
    ]),
    ...(spec?.risks ?? []).map((risk) => ({
      level: risk.level,
      title: risk.title,
      body: risk.body,
      ...(risk.ifItMatters ? { ifItMatters: risk.ifItMatters } : {}),
      ...(risk.area ? { area: risk.area } : {}),
      path: risk.at.path,
      line: risk.at.line,
    })),
  ]

  const before = [
    ...(proof.review?.blockers ?? []).map((b) => ({ text: `Resolve: ${b.title}`, done: false })),
    ...(spec?.checklist.before.length
      ? spec.checklist.before
      : areas
          .filter((area) => area.tag === 'asks you')
          .map((area) => ({ text: `Inspect ${area.name}`, done: false }))),
  ]
  const after = spec?.checklist.after.length
    ? spec.checklist.after
    : (input.afterMerge ?? []).map((text) => ({ text, done: false }))

  /** @type {{severity: string, at?: string, text: string}[]} */
  const notes = [
    ...(spec?.notes ?? []),
    ...(proof.review?.notes ?? []).map((text) => ({ severity: '', text })),
    ...(proof.review?.models ?? []).map((model) => ({
      severity: '',
      text: `${model.agent}: ${model.outcome}${model.detail ? ` (${model.detail})` : ''}`,
    })),
  ]

  const totals = files && {
    files: files.length,
    adds: files.reduce((n, f) => n + (f.adds ?? 0), 0),
    dels: files.reduce((n, f) => n + (f.dels ?? 0), 0),
  }
  const added = (/** @type {RegExp} */ pattern) =>
    (files ?? []).filter((f) => pattern.test(f.path)).reduce((n, f) => n + (f.adds ?? 0), 0)
  const tests = added(TEST_PATH)
  const generated = added(GENERATED_PATH)
  const split =
    totals && totals.adds > 0
      ? { logic: Math.max(0, totals.adds - tests - generated), tests, generated }
      : null

  const levels = risks.map((risk) => risk.level)
  const riskText =
    ['high', 'medium', 'low', 'deploy']
      .filter((level) => levels.includes(level))
      .map((level) => `${levels.filter((l) => l === level).length} ${level}`)
      .join(', ') || 'none'
  const display =
    proof.confidence.level === 'cannot-assess'
      ? 'Cannot assess'
      : proof.confidence.level[0].toUpperCase() + proof.confidence.level.slice(1)
  const confidenceSection = view.sections.find((section) => section.title === 'Confidence')
  const what = view.sections.find((section) => section.title === 'What it does')

  /** @type {string[]} */
  const inputs = ['proof record']
  if (spec) inputs.push('agent spec')
  if (media) inputs.push('media manifest')
  return {
    repo: proof.repo,
    pr: proof.pr,
    head: proof.head,
    base: proof.base,
    digest: proof.digest,
    title: spec?.title ?? view.verdict ?? view.headline,
    verdict: spec?.title ? view.verdict : null,
    headline: view.headline,
    reviewUrl: input.reviewUrl ?? null,
    filesUrl: input.filesUrl ?? null,
    health: view.healthStrip,
    does: { bullets: what?.bullets ?? [], source: what?.source ?? null },
    status: {
      confidence: view.computed ? display : 'not computed',
      rate: view.computed ? (confidenceSection?.bullets[1] ?? null) : null,
      asks: proof.confidence.asks.length,
      risks: riskText,
      afterMerge: after.length,
    },
    totals,
    split,
    waiting,
    risks,
    quoted: new Set(areas.flatMap((area) => area.snippets.map((s) => s.path))),
    areas,
    splitSuggested: (askSection?.bullets ?? []).includes('split suggested'),
    evidence: {
      heads: heads.map((clip, index) => ({ clip, index })),
      pairs,
      stills: heads.flatMap((clip) => clip.stills),
      transcripts: media?.transcripts ?? [],
      source: media?.source ?? null,
      plan: spec?.evidencePlan ?? [],
      record: areas.flatMap((area) => area.media),
    },
    checklist: { before, after },
    notes,
    files: files ? [...files].sort((a, b) => (b.adds ?? 0) - (a.adds ?? 0)) : null,
    commits: input.timeline ?? [],
    generation: {
      inputs,
      problems,
      uncertain: spec?.uncertain ?? [],
      checked: Boolean(spec || media),
    },
  }
}
