/**
 * The proof page as data: every fact the HTML shows, assembled from the boss-proof/1 record and
 * its page input, plus an optional agent spec, media manifest and snippet resolver. Without the
 * optional parts the model is built from the record alone, deterministically.
 */
import { createHash } from 'node:crypto'
import { CONFIDENCE_NOT_COMPUTED, capText } from './verify-proof.mjs'
import { validateProofSpec } from './verify-proof-spec.mjs'
import { validateProofMedia } from './verify-proof-media.mjs'
import { MAX_LINE_CHARS, parseUnifiedDiff } from './verify-proof-snippet.mjs'

export const UI_AREA = 'ui'
export const NO_UI_RECORDING = 'No recording of the changed UI'
const TEST_PATH = /(_test\.go$|\.test\.|\.spec\.|__tests__\/|(^|\/)e2e\/)/
const NAME_CHARS = 40
const MAX_RECORD_FACTS = 6
const MAX_NOTES = 20
export const FILES_MISSING =
  'The changed-file list was missing, so the Files list is empty and no spec risk or snippet could be checked'
const UNKNOWN_FILES = /the changed files are unknown, so it cannot be checked$/
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
 * @typedef {{spec?: unknown, media?: unknown, mediaSize?: (file: string) => number | null, snippet?: SnippetResolver, hasLine?: (path: string, line: number) => boolean | null, files?: unknown}} PageExtras
 *   `files: null` says git was asked for the changed files and failed; leaving it out says there is no git.
 * @typedef {{title: string, detail?: string, href?: string, link?: string, proof?: boolean, high?: boolean}} WaitingItem
 * @typedef {{level: string, title: string, body?: string, items?: string[], ifItMatters?: string, label?: string, path?: string, line?: number}} RiskItem
 * @typedef {{label: string, files: FileDiff[], text: string | null, remaining: number}} AreaExcerpt
 * @typedef {{kind: string, url: string, caption: string, inline: boolean, poster?: string}} AreaMedia
 * @typedef {{id: string, name: string, fullName: string, summary: string, facts: {text: string, flag: boolean}[], moreFacts: string[], tag: string,
 *   snippets: (Snippet & {pinNote?: string})[], excerpts: AreaExcerpt[], media: AreaMedia[]}} AreaItem
 * @typedef {{clip: Clip, index: number}} HeadClip
 * @typedef {{base: import('./verify-proof-media.mjs').Still, head: import('./verify-proof-media.mjs').Still}} StillPair
 * @typedef {import('./verify-proof-media.mjs').ProofDocument} ProofDocument
 * @typedef {{head: ProofDocument, base: ProofDocument | null}} DocumentPair
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
        `API change: ${area.change}${area.change === 'breaking' && area.changeReasons?.length ? ` (${area.changeReasons.join('; ')})` : ''}`,
      ]
    : []),
  ...area.items,
]

/** Note text for comparing a spec note with a record note. @param {string} text */
const noteKey = (text) =>
  text
    .toLowerCase()
    .replace(/\s+/g, ' ')
    .replace(/[\s.]+$/, '')
    .trim()

/** A reviewer that did not run is a reason to wait; a failed security review is a high one.
 * @param {{agent: string, outcome: string, detail?: string}} model @returns {WaitingItem | null} */
function reviewerWaiting(model) {
  if (model.outcome === 'ran') return null
  const agent =
    model.agent.toLowerCase() === 'review'
      ? 'Review'
      : `${model.agent[0].toUpperCase()}${model.agent.slice(1)} review`
  return {
    title: `${agent} ${model.outcome === 'failed' ? 'failed' : 'was skipped'}`,
    ...(model.detail ? { detail: model.detail } : {}),
    ...(model.outcome === 'failed' && /security/i.test(model.agent) ? { high: true } : {}),
  }
}

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
          hasLine: extras.hasLine,
        })
  const mediaResult =
    extras.media === undefined
      ? null
      : validateProofMedia(extras.media, { head: proof.head, sizeOf: extras.mediaSize })
  const spec = specResult?.spec ?? null
  const media = mediaResult?.media ?? null
  /** A dropped item's line, its kind named once. @param {string} kind @param {{at: string, reason: string}} d */
  const prefix = (kind, d) =>
    `${d.at === kind || d.at.startsWith(`${kind}.`) ? '' : `${kind} `}${d.at}: ${d.reason}`
  // One line says the file list is missing, in place of one line per item it dropped.
  const unchecked = (specResult?.dropped ?? []).filter((d) => UNKNOWN_FILES.test(d.reason))
  /** @type {string[]} */
  const problems = [
    ...(extras.files === null || unchecked.length
      ? [`${FILES_MISSING}${unchecked.length ? ` (${unchecked.length} dropped)` : ''}.`]
      : []),
    ...(specResult?.dropped ?? [])
      .filter((d) => !UNKNOWN_FILES.test(d.reason))
      .map((d) => prefix('spec', d)),
    ...(mediaResult?.dropped ?? []).map((d) => prefix('media', d)),
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
    const snippetPaths = new Set(snippets.map((s) => s.path))
    const fullName = specArea?.name ?? area.label ?? area.name
    return {
      id: area.name,
      name: capText(fullName, NAME_CHARS),
      fullName,
      summary:
        specArea?.summary ??
        (area.ask
          ? 'Asks for a person.'
          : area.proven
            ? 'Checked: proven.'
            : 'Checked: not shown.'),
      facts:
        specArea?.facts ?? items.slice(0, MAX_RECORD_FACTS).map((text) => ({ text, flag: false })),
      moreFacts: specArea ? [] : items.slice(MAX_RECORD_FACTS),
      tag: area.ask ? 'asks you' : 'checked',
      snippets,
      excerpts: (ask?.excerpts ?? []).map((excerpt) => {
        const parsed = excerpt.label === 'diff' ? parseUnifiedDiff(excerpt.lines.join('\n')) : []
        // A file the agent already quotes is not shown again, and a file's counts are the PR's.
        const shown = parsed
          .filter((f) => !snippetPaths.has(f.path))
          .map((f) => {
            const changed = files?.find((c) => c.path === f.path)
            return {
              ...f,
              ...(changed ? { adds: changed.adds ?? f.adds, dels: changed.dels ?? f.dels } : {}),
              rows: f.rows.map((row) =>
                row.text.length === MAX_LINE_CHARS - 1 && row.text.endsWith('\u2026')
                  ? { ...row, cut: true }
                  : row,
              ),
            }
          })
        return {
          label: excerpt.label,
          files: shown,
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
  const pairs = bases.flatMap((base) =>
    base.stills.flatMap((still, j) => {
      const head = heads.find((clip) => clip.id === base.pairs)?.stills[j]
      return head ? [{ base: still, head }] : []
    }),
  )
  const docs = media?.documents ?? []
  /** @type {DocumentPair[]} */
  const documents = docs
    .filter((doc) => doc.side === 'head')
    .map((head) => ({
      head,
      base: docs.find((doc) => doc.side === 'base' && doc.pairs === head.id) ?? null,
    }))
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
  const reviewers = (proof.review?.models ?? []).flatMap((model) => reviewerWaiting(model) ?? [])
  waiting.push(...reviewers.filter((w) => w.high), ...reviewers.filter((w) => !w.high))
  for (const reason of proof.confidence.loweredBy)
    if (reason.code !== CONFIDENCE_NOT_COMPUTED) waiting.push({ title: reason.text })
  for (const item of spec?.waiting ?? [])
    waiting.push({
      title: item.title,
      ...(item.detail ? { detail: item.detail } : {}),
      ...(item.area
        ? { href: `#a-${item.area}`, link: capText(label(item.area), NAME_CHARS) }
        : {}),
    })

  /** @type {RiskItem[]} */
  const risks = [
    ...proof.areas.flatMap((area) => [
      ...(area.change === 'breaking'
        ? [
            {
              level: 'high',
              title: `Breaking API change in ${capText(area.label ?? area.name, NAME_CHARS)}`,
              ...(area.changeReasons?.length ? { items: [...area.changeReasons] } : {}),
              label: capText(area.label ?? area.name, NAME_CHARS),
            },
          ]
        : []),
      ...(area.unmentioned?.length
        ? [
            {
              level: 'medium',
              title: `Not in the description: ${capText(area.label ?? area.name, NAME_CHARS)}`,
              items: [...area.unmentioned],
              label: capText(area.label ?? area.name, NAME_CHARS),
            },
          ]
        : []),
    ]),
    ...(spec?.risks ?? []).map((risk) => ({
      level: risk.level,
      title: risk.title,
      body: risk.body,
      ...(risk.ifItMatters ? { ifItMatters: risk.ifItMatters } : {}),
      ...(risk.label ? { label: risk.label } : {}),
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

  // Reviewer outcomes are in Why it's waiting. Record notes lead so agent notes never push one
  // past the cap, and a record note the spec restates shows once, as the record wrote it.
  const recordNotes = proof.review?.notes ?? []
  const recordKeys = new Set(recordNotes.map(noteKey))
  /** @type {{severity: string, at?: string, text: string}[]} */
  const allNotes = [
    ...recordNotes.map((text) => ({ severity: '', text })),
    ...(spec?.notes ?? []).filter((note) => !recordKeys.has(noteKey(note.text))),
  ]
  const notes = allNotes.slice(0, MAX_NOTES)

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
    title: spec?.title ?? view.headline,
    verdict: view.verdict,
    headline: view.headline,
    reviewUrl: input.reviewUrl ?? null,
    filesUrl: input.filesUrl ?? null,
    does: {
      bullets: what?.state === 'unknown' ? [] : (what?.bullets ?? []),
      source: what?.source ?? null,
    },
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
      documents,
      source: media?.source ?? null,
      plan: spec?.evidencePlan ?? [],
      record: areas.flatMap((area) => area.media),
    },
    checklist: { before, after },
    notes,
    notesMore: allNotes.length - notes.length,
    files: files ? [...files].sort((a, b) => (b.adds ?? 0) - (a.adds ?? 0)) : null,
    commits: input.timeline ?? [],
    generation: {
      inputs,
      problems,
      uncertain: spec?.uncertain ?? [],
      health: view.healthStrip,
      checked: Boolean(spec || media),
    },
  }
}
