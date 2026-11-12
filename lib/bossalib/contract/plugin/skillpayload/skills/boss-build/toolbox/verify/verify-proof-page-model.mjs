/**
 * The proof page as data: every fact the HTML shows, assembled from the boss-proof/1 record and
 * its page input, plus an optional agent spec, media manifest, snippet resolver and the areas'
 * paths. Without the optional parts the model is built from the record alone, deterministically.
 *
 * The page leads with a banner (confidence, one reason, the risks, what is not proven and one
 * signal per area a person reviews), then one section per kind of content, each present only
 * when it has something to show. Pipeline state (reviewer outcomes, factory health, what the
 * generator dropped) lives in `debug`, which the page shows only behind `?debug`.
 */
import { createHash } from 'node:crypto'
import { NO_SIGN_OFF_AREA } from './verify-area-ids.mjs'
import { CONFIDENCE_NOT_COMPUTED, capText } from './verify-proof.mjs'
import { TEST_PATH, ticketVerdict, validateProofSpec } from './verify-proof-spec.mjs'
import { validateProofMedia } from './verify-proof-media.mjs'
import { MAX_LINE_CHARS, parseUnifiedDiff } from './verify-proof-snippet.mjs'
import { locatedRisks } from './verify-describe-blocks.mjs'

export const UI_AREA = 'ui'
export const NO_UI_RECORDING = 'No recording of the changed UI'
const NAME_CHARS = 40
const MAX_FINDINGS = 20
/** Findings by state: what blocks merge first, what review fixed last. */
const FINDING_ORDER = ['blocks', 'open', 'note', 'fixed']
/** How far a risk's line may sit from a finding's for the two to link. */
const LINK_LINES = 10
export const FILES_MISSING =
  'The changed-file list was missing, so the Files list is empty and no spec risk or snippet could be checked'
const UNKNOWN_FILES = /the changed files are unknown, so it cannot be checked$/
export const GENERATED_PATH =
  /((^|\/)gen(erated)?\/|_gen\.go$|\.pb\.go$|\.snap$|(^|\/)(pnpm-lock\.yaml|yarn\.lock|package-lock\.json|go\.sum)$)/
const JUDGE_NOTE = /^(.+?) \(([^()\s]+)\): (.+)$/s
/** Plain clauses for the confidence reasons whose record text uses the pipeline's words. */
const REASON_TEXT = Object.freeze(
  /** @type {Record<string, string>} */ ({
    'evidence-missing': 'some of the evidence it needs is missing',
    'asks-over-3': 'it touches more than three parts a person reviews',
  }),
)
/** What Not proven says for an asking area that carries nothing to show. */
const UNSHOWN = Object.freeze(
  /** @type {Record<string, string>} */ ({
    'unproven-behavior': 'A behavior change that no test or recording shows',
  }),
)
const TICKET_ID = /^[A-Z][A-Z0-9]+-\d+$/
/** Rows on each side of a decision's line, so its code shows about ten lines. */
const DECISION_RADIUS = 5
/** The anchor of a ticket criterion, from its id. @param {string} id */
const criterionAnchor = (id) => `ac-${id.toLowerCase().replace(/[^a-z0-9]+/g, '-')}`
const TICKET_IN_URL = /\/([A-Z][A-Z0-9]+-\d+)(?:[/?#]|$)/

/**
 * The banner's level: its word and the tone the page colours it with.
 * @type {Readonly<Record<string, {word: string, tone: string}>>}
 */
export const LEVELS = Object.freeze({
  high: { word: 'High', tone: 'success' },
  moderate: { word: 'Moderate', tone: 'info' },
  low: { word: 'Low', tone: 'warning' },
  blocked: { word: 'Blocked', tone: 'danger' },
  unrated: { word: 'Not rated', tone: 'muted' },
})
/** Section order of the area sections: the database, then the API, security and UI. */
const AREA_RANK = Object.freeze({
  migrations: 0,
  'api-contract': 1,
  security: 2,
  ui: 3,
  [NO_SIGN_OFF_AREA]: 10,
})
/** @type {Readonly<Record<string, string>>} */
const RISK_TONE = Object.freeze({
  high: 'danger',
  medium: 'warning',
  low: 'muted',
})
const RISK_ORDER = ['high', 'medium', 'low']
/** The banner pill for each state a describer's signal can name; `none` shows no row. */
const SIGNAL_PILLS = Object.freeze(
  /** @type {Record<string, Pill>} */ ({
    safe: { word: 'safe', tone: 'success' },
    additive: { word: 'additive', tone: 'success' },
    proven: { word: 'proven', tone: 'success' },
    review: { word: 'to review', tone: 'warning' },
    manual: { word: 'manual step', tone: 'warning' },
    unproven: { word: 'unproven', tone: 'warning' },
    risky: { word: 'risky', tone: 'warning' },
    breaking: { word: 'breaking', tone: 'danger' },
  }),
)
/** @param {string} level */
const riskRank = (level) => {
  const i = RISK_ORDER.indexOf(level)
  return i < 0 ? RISK_ORDER.length : i
}
/** The file groups that follow the area groups, in order. */
const OTHER_GROUPS = ['No sign-off needed', 'Tests', 'Generated']

/**
 * @typedef {import('./verify-proof.mjs').Proof} Proof
 * @typedef {import('./verify-proof-page.mjs').PageInput} PageInput
 * @typedef {import('./verify-proof-media.mjs').Clip} Clip
 * @typedef {import('./verify-proof-snippet.mjs').Snippet} Snippet
 * @typedef {import('./verify-proof-snippet.mjs').SnippetResolver} SnippetResolver
 * @typedef {import('./verify-proof-snippet.mjs').FileDiff} FileDiff
 * @typedef {{path: string, adds: number | null, dels: number | null}} ChangedFile
 * @typedef {{spec?: unknown, media?: unknown, mediaSize?: (file: string) => number | null, snippet?: SnippetResolver, context?: import('./verify-proof-snippet.mjs').ContextResolver, hasLine?: (path: string, line: number) => boolean | null, files?: unknown, areas?: {name: string, paths: string[]}[], mergeBase?: string, checks?: string[] | null, generatedAt?: string}} PageExtras
 *   `files: null` says git was asked for the changed files and failed; leaving it out says there
 *   is no git. `areas` maps each record area name to the paths it covers, for the Files grid.
 *   `mergeBase` is the commit the files and snippets diff from, shown as the page's base.
 *   `context` quotes the lines around a finding's or a decision's location. `checks` names the
 *   checks that passed on the head, which a spec criterion may cite. `generatedAt` is when the
 *   page was built, as an ISO time; it defaults to now.
 * @typedef {{word: string, tone: string}} Pill
 * @typedef {{id: string, level: string, tone: string, title: string, impact?: string, items?: string[], guard?: string, at?: {path: string, line: number}, links?: Link[]}} RiskItem
 * @typedef {{label: string, summary: string, pill: Pill, href: string | null}} Signal
 * @typedef {import('./verify-proof-snippet.mjs').Context} Context
 * @typedef {{href: string, label: string, title: string}} Link
 * @typedef {{id: string, text: string, why?: string, suggestion?: string, code?: Context, done: boolean, links?: Link[]}} Tick
 * @typedef {'blocks' | 'open' | 'note' | 'fixed'} FindingState
 * @typedef {{id: string, ref: string, severity: string, state: FindingState, text: string, detail?: string, at?: string, code?: Context, links: Link[]}} Finding
 * @typedef {{kind: string, label: string, title?: string, href?: string}} Evidence
 * @typedef {{id: string, label: string, text: string, status: 'done' | 'unverified' | 'missed' | 'skipped', note?: string, evidence: Evidence[], links?: Link[]}} Criterion
 * @typedef {{id: string | null, url: string | null, verdict: import('./verify-proof-spec.mjs').TicketVerdict | null, summary?: string, criteria: Criterion[], skipped: import('./verify-proof-spec.mjs').SpecSkipped[], contradictions: string[]}} TicketFit
 * @typedef {{label: string, files: FileDiff[], text: string | null, remaining: number}} AreaExcerpt
 * @typedef {{kind: string, url: string, caption: string, inline: boolean, poster?: string}} AreaMedia
 * @typedef {import('./verify-describe-blocks.mjs').Block} Block
 * @typedef {import('./verify-describe-blocks.mjs').Described} Described
 * @typedef {{id: string, name: string, fullName: string, summary: string, asks: {title: string, detail?: string}[],
 *   changes: string[], facts: {text: string, flag: boolean}[], pill: Pill, blocks: Block[], snippets: (Snippet & {pinNote?: string})[], excerpts: AreaExcerpt[], media: AreaMedia[]}} AreaItem
 * @typedef {{clip: Clip, index: number}} HeadClip
 * @typedef {{base: import('./verify-proof-media.mjs').Still, head: import('./verify-proof-media.mjs').Still}} StillPair
 * @typedef {import('./verify-proof-media.mjs').ProofDocument} ProofDocument
 * @typedef {{head: ProofDocument, base: ProofDocument | null}} DocumentPair
 * @typedef {{path: string, adds: number, dels: number, binary: boolean, href: string | null}} FileRow
 * @typedef {{name: string, adds: number, dels: number, files: FileRow[]}} FileGroup
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

/** Note text for comparing two notes: lower case, no severity prefix, no closing stop. @param {string} text */
const noteKey = (text) =>
  text
    .toLowerCase()
    .replace(/^p[0-3]\s*[:·-]\s*/, '')
    .replace(/\s+/g, ' ')
    .replace(/[\s.]+$/, '')
    .trim()

/** The id a path's quoted diff carries, so a risk can link to it. @param {string} path */
export const fileAnchor = (path) =>
  `f-${path
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '')}-${createHash('sha256').update(path).digest('hex').slice(0, 6)}`

/** `a`, `a and b`, `a, b and c`. @param {string[]} items */
const joinList = (items) =>
  items.length < 2 ? (items[0] ?? '') : `${items.slice(0, -1).join(', ')} and ${items.at(-1)}`

/** A name as it reads mid-sentence: lower case unless it starts with an acronym. @param {string} name */
const midSentence = (name) =>
  /^[A-Z][a-z]*(\s|$)/.test(name) ? name[0].toLowerCase() + name.slice(1) : name

/**
 * One plain sentence saying why the page has its level, in the reader's words: what a person
 * reviews and anything else that lowered it. Never the pipeline's codes or guidance.
 * @param {Proof} proof @param {string | undefined} label the judge's own
 *   phrase for what the asking areas change, when it gave one
 */
export function bannerReason(proof, label) {
  if (proof.confidence.loweredBy.some((reason) => reason.code === CONFIDENCE_NOT_COMPUTED))
    return 'Confidence was not worked out for this head.'
  const blockers = proof.review?.blockers.length ?? 0
  if (proof.confidence.level === 'blocked')
    return `Review left ${blockers === 1 ? 'one blocker' : `${blockers} blockers`} to resolve before merging.`
  const asking = proof.areas.filter((area) => area.ask).map((area) => area.label ?? area.name)
  const codes = new Set(proof.confidence.loweredBy.map((reason) => reason.code))
  /** @type {string[]} */
  const clauses = []
  if (asking.length)
    clauses.push(
      `it changes ${label ? midSentence(label) : joinList(asking.map(midSentence))}, which a person reviews`,
    )
  for (const reason of proof.confidence.loweredBy) {
    if (reason.code === 'area-asks' || reason.code === CONFIDENCE_NOT_COMPUTED) continue
    if (reason.code === 'asks-over-3' && asking.length) continue
    if (reason.code === 'size-p90' && codes.has('size-p99')) continue
    clauses.push(REASON_TEXT[reason.code] ?? midSentence(reason.text.replace(/\.$/, '')))
  }
  if (!clauses.length) return 'Nothing it changes needs a person, and nothing lowered confidence.'
  const sentence =
    clauses.length > 1 ? `${clauses.slice(0, -1).join(', ')}, and ${clauses.at(-1)}` : clauses[0]
  return `${sentence[0].toUpperCase()}${sentence.slice(1)}.`
}

/**
 * @param {{proof: Proof, input: PageInput, view: ReturnType<typeof import('./verify-proof-page.mjs').proofView>, extras?: PageExtras}} options
 */
export function buildPageModel({ proof, input, view, extras = {} }) {
  const files = changedFiles(extras.files)
  const areaIds = proof.areas.map((area) => area.name)
  const review = input.review ?? { findings: [], acceptance: [] }
  const specResult =
    extras.spec === undefined
      ? null
      : validateProofSpec(extras.spec, {
          head: proof.head,
          changedPaths: files?.map((f) => f.path),
          areaIds,
          hasLine: extras.hasLine,
          checks: extras.checks ?? null,
          acceptance: review.acceptance,
          findingIds: review.findings.map((f) => f.id),
          addedPaths: files?.filter((f) => f.adds !== 0).map((f) => f.path),
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
  /** What an area's describers typed, from the page sidecar. @param {string} name @returns {Described} */
  const typed = (name) => input.described?.[name] ?? {}
  const askSection = view.sections.find((section) => section.title === 'Asks')
  const asks = new Map((askSection?.asks ?? []).map((ask, i) => [proof.confidence.asks[i], ask]))
  /** @type {Map<string, {title: string, detail?: string}[]>} */
  const waitingByArea = new Map()
  for (const item of spec?.waiting ?? [])
    if (item.area)
      waitingByArea.set(item.area, [
        ...(waitingByArea.get(item.area) ?? []),
        { title: item.title, ...(item.detail ? { detail: item.detail } : {}) },
      ])

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
    const quiet = area.name === NO_SIGN_OFF_AREA
    const blocks = (typed(area.name).blocks ?? []).filter((block) => block.kind !== 'steps')
    /** @type {Pill} */
    const pill = quiet
      ? { word: 'no sign-off', tone: 'muted' }
      : area.change === 'breaking'
        ? { word: 'breaking', tone: 'danger' }
        : area.ask
          ? { word: 'to review', tone: 'warning' }
          : { word: 'checked', tone: 'muted' }
    return {
      id: area.name,
      name: capText(fullName, NAME_CHARS),
      fullName,
      summary: specArea?.summary ?? '',
      changes: area.items,
      asks: waitingByArea.get(area.name) ?? [],
      facts: specArea?.facts ?? items.map((text) => ({ text, flag: false })),
      pill,
      // Steps join After merge; every other block renders in the area's section.
      blocks,
      // The no-sign-off area shows what changed there, and never asks or warns. An area a
      // describer typed blocks for shows them even when nobody reviews it.
      shown: area.ask || area.change === 'breaking' || quiet || blocks.length > 0,
      snippets,
      excerpts: (ask?.excerpts ?? []).map((excerpt) => {
        const parsed = excerpt.label === 'diff' ? parseUnifiedDiff(excerpt.lines.join('\n')) : []
        // A file the agent already quotes is not shown again, and a file's counts are the PR's.
        const shown = parsed
          .filter((f) => f.rows.length && !snippetPaths.has(f.path))
          .map((f) => {
            const changed = files?.find((c) => c.path === f.path)
            return {
              ...f,
              ...(changed ? { adds: changed.adds ?? f.adds, dels: changed.dels ?? f.dels } : {}),
              rows: f.rows.map((row) =>
                row.text.length === MAX_LINE_CHARS - 1 && row.text.endsWith('…')
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
  // The areas a person reviews, database first; each has a section when it has content.
  const reviewed = areas
    .filter((a) => a.shown)
    .map(({ shown, ...area }) => area)
    .sort(
      (a, b) =>
        (AREA_RANK[/** @type {keyof typeof AREA_RANK} */ (a.id)] ?? 9) -
        (AREA_RANK[/** @type {keyof typeof AREA_RANK} */ (b.id)] ?? 9),
    )
  const sections = reviewed.filter(
    (a) =>
      a.summary ||
      a.asks.length ||
      a.facts.length ||
      a.blocks.length ||
      a.snippets.length ||
      a.excerpts.some((x) => x.files.length || x.text !== null) ||
      a.media.length,
  )

  // A clip that names the files it proves, none of them this PR's, shows someone else's change.
  const changed = files ? new Set(files.map((f) => f.path)) : null
  const clips = (media?.clips ?? []).filter((clip) => {
    if (!clip.files || !changed || clip.files.some((path) => changed.has(path))) return true
    problems.push(`media clips.${clip.id}: proves none of the changed files`)
    return false
  })
  const heads = clips.filter((clip) => clip.side === 'head')
  const bases = clips.filter((clip) => clip.side === 'base')
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

  // What the PR claims and nothing proves: unproven claims, a changed UI no clip shows, and a
  // review that failed to run.
  /** @type {string[]} */
  const unproven = [
    ...(proof.whatItDoes.claims ?? []).flatMap((claim, i) =>
      claim.proof === 'unproven' && proof.whatItDoes.bullets[i]
        ? [`${proof.whatItDoes.bullets[i]}${claim.note ? `: ${claim.note}` : ''}`]
        : [],
    ),
    ...(uiChanged && heads.length === 0 && !recordVideo ? [NO_UI_RECORDING] : []),
    ...(proof.review?.models ?? [])
      .filter(
        (model) =>
          model.outcome === 'failed' || (model.outcome !== 'ran' && /security/i.test(model.agent)),
      )
      .map(
        (model) =>
          `The ${model.agent.toLowerCase().replace(/ review$/, '')} review ${model.outcome === 'failed' ? 'did not finish' : 'was skipped'}`,
      ),
  ]

  /** @param {string} level */
  const tone = (level) => RISK_TONE[level] ?? 'muted'
  const recordRisks = proof.areas.flatMap((area) => {
    if (area.name === NO_SIGN_OFF_AREA) return []
    const label = capText(area.label ?? area.name, NAME_CHARS)
    return [
      ...(area.change === 'breaking'
        ? [
            {
              level: 'high',
              title: `Breaking API change in ${label}`,
              ...(area.changeReasons?.length ? { items: [...area.changeReasons] } : {}),
            },
          ]
        : []),
      ...(area.unmentioned?.length
        ? [
            {
              level: 'medium',
              title: `The PR description doesn't mention the ${midSentence(label)} change`,
              items: [...area.unmentioned],
            },
          ]
        : []),
    ]
  })
  // A describer's risk shows only where the diff holds its location.
  const describedRisks = proof.areas.flatMap((area) => {
    const { kept, dropped } = locatedRisks(typed(area.name).risks ?? [], {
      changedPaths: changed,
      hasLine: extras.hasLine,
    })
    problems.push(...dropped.map((d) => `describe ${area.name} ${d.at}: ${d.reason}`))
    return kept
  })
  // The author's risks, then each describer risk the author did not already state: one at the
  // same lines, or with the same title.
  const authorRisks = spec?.risks ?? []
  const restated = (/** @type {import('./verify-describe-blocks.mjs').Risk} */ risk) =>
    authorRisks.some(
      (mine) =>
        noteKey(mine.title) === noteKey(risk.title) ||
        (mine.at?.path === risk.at.path && Math.abs(mine.at.line - risk.at.line) <= LINK_LINES),
    )
  /** @type {(Omit<RiskItem, 'id' | 'tone'> & {specId?: string, specLinks?: string[]})[]} */
  const unsorted = [
    ...recordRisks,
    ...authorRisks.map(({ id, links, ...risk }) => ({
      ...risk,
      ...(id ? { specId: id } : {}),
      specLinks: links,
    })),
    ...describedRisks.filter((risk) => !restated(risk)),
  ]
  const risks = unsorted
    .map((risk, i) => ({ ...risk, i }))
    .sort((a, b) => riskRank(a.level) - riskRank(b.level) || a.i - b.i)
    .map(({ i, ...risk }, n) => ({ id: `risk-${n + 1}`, tone: tone(risk.level), ...risk }))

  // Decisions: what the author says a person must judge, with the code it is about, and any
  // reason it gave that belongs to no area.
  /** @type {(Tick & {specId?: string, specLinks?: string[]})[]} */
  const decisions = [
    ...(spec?.decisions ?? []).map((d) => {
      const code = extras.context?.(d.at.path, d.at.line, DECISION_RADIUS)
      return {
        text: d.question,
        why: d.why,
        suggestion: d.suggestion,
        ...(code ? { code } : {}),
        done: false,
        specId: d.id,
        specLinks: d.links,
      }
    }),
    ...(spec?.waiting ?? [])
      .filter((item) => !item.area)
      .map((item) => ({
        text: item.title,
        ...(item.detail ? { why: item.detail } : {}),
        done: false,
      })),
  ].map((item, i) => ({ id: `decide-${i + 1}`, ...item }))
  const listed = spec?.checklist.after.length
    ? spec.checklist.after
    : (input.afterMerge ?? []).map((text) => ({ text, done: false }))
  // Each manual step a describer names, after the listed ones and once.
  const steps = [
    ...new Set(
      proof.areas.flatMap((area) =>
        (typed(area.name).blocks ?? []).flatMap((block) =>
          block.kind === 'steps' ? block.steps : [],
        ),
      ),
    ),
  ].filter((text) => !listed.some((item) => item.text === text))
  /** @type {Tick[]} */
  const afterMerge = [...listed, ...steps.map((text) => ({ text, done: false }))].map(
    (item, i) => ({ id: `after-${i + 1}`, text: item.text, done: item.done }),
  )

  // Findings: what review found, with the code around each location, then the record's other
  // blockers and notes and the agent's notes. One finding shows once: by id, and a repeat with no
  // location never shows beside the located one.
  /** @param {string} at @returns {Context | undefined} */
  const contextAt = (at) => {
    const located = /^(.+):(\d+)$/.exec(at)
    return (located && extras.context?.(located[1], Number(located[2]))) || undefined
  }
  const blockerAt = new Set(
    (proof.review?.blockers ?? []).map((b) => `${b.title}@${b.where ?? ''}`),
  )
  /** @type {(Omit<Finding, 'id' | 'ref' | 'links'> & {source?: string})[]} */
  const allFindings = [
    ...[...new Map(review.findings.map((f) => [f.id, f])).values()].map((f) => {
      const at = f.file ? (f.line ? `${f.file}:${f.line}` : f.file) : undefined
      const code = at ? contextAt(at) : undefined
      return {
        source: f.id,
        severity: f.severity,
        state: /** @type {FindingState} */ (
          f.status === 'open' && blockerAt.has(`${f.title}@${at ?? ''}`) ? 'blocks' : f.status
        ),
        text: f.title,
        ...(f.detail ? { detail: f.detail } : {}),
        ...(at ? { at } : {}),
        ...(code ? { code } : {}),
      }
    }),
    ...(proof.review?.blockers ?? []).map((b) => ({
      severity: '',
      state: /** @type {FindingState} */ ('blocks'),
      text: b.title,
      ...(b.where ? { at: b.where } : {}),
    })),
    ...(proof.review?.notes ?? []).map((text) => {
      // A judge note on a no-sign-off path reads `label (path): reason`.
      const judged = JUDGE_NOTE.exec(text)
      return judged
        ? {
            severity: '',
            state: /** @type {FindingState} */ ('note'),
            text: `${judged[1]}: ${judged[3]}`,
            at: judged[2],
          }
        : { severity: '', state: /** @type {FindingState} */ ('note'), text }
    }),
    ...(spec?.notes ?? []).map((note) => {
      const code = note.at ? contextAt(note.at) : undefined
      return {
        severity: note.severity,
        state: /** @type {FindingState} */ ('note'),
        text: note.text,
        ...(note.at ? { at: note.at } : {}),
        ...(code ? { code } : {}),
      }
    }),
  ]
  const locatedKeys = allFindings.filter((f) => f.at).map((f) => noteKey(f.text))
  const repeats = (/** @type {string} */ text) => {
    const key = noteKey(text)
    return locatedKeys.some((other) => other === key || other.includes(key) || key.includes(other))
  }
  const seen = new Set()
  const kept = allFindings
    .filter((f) => {
      const key = `${noteKey(f.text)}@${f.at ?? ''}`
      if (seen.has(key) || (!f.at && repeats(f.text))) return false
      seen.add(key)
      return true
    })
    .map((f, i) => ({ ...f, i }))
    .sort((a, b) => FINDING_ORDER.indexOf(a.state) - FINDING_ORDER.indexOf(b.state) || a.i - b.i)
    .slice(0, MAX_FINDINGS)
    .map(({ i, ...f }, n) => ({ id: `finding-${n + 1}`, ref: `F${n + 1}`, ...f }))

  // The PR itself, when the files link is a pull request's Files tab.
  const prUrl = /\/pull\/\d+\/files$/.test(input.filesUrl ?? '')
    ? (input.filesUrl ?? '').slice(0, -6)
    : null
  // GitHub anchors a file in a PR's changes view by the SHA-256 of its path; any other forge gets
  // its files view.
  /** @param {string} path */
  const fileHref = (path) =>
    prUrl?.startsWith('https://github.com/')
      ? `${prUrl}/changes#diff-${createHash('sha256').update(path).digest('hex')}`
      : (input.filesUrl ?? null)

  // The ticket's acceptance criteria: the author's, whose done ones the spec already checked, then
  // each row review certified that the author did not restate by id, done only when a check passed
  // on this head and missed when it failed here.
  const fit = spec?.ticketFit
  const restatedRows = new Set(fit?.criteria.map((c) => criterionAnchor(c.id)))
  /** @type {(Omit<Criterion, 'evidence'> & {check?: string, cited?: import('./verify-proof-spec.mjs').SpecEvidence[]})[]} */
  const criteria = [
    ...(fit?.criteria ?? []).map((c) => ({
      id: criterionAnchor(c.id),
      label: c.id,
      text: c.text,
      status: /** @type {Criterion['status']} */ (c.status),
      ...(c.note ? { note: c.note } : {}),
      cited: c.evidence,
    })),
    ...review.acceptance
      .filter((row) => !restatedRows.has(criterionAnchor(row.id)))
      .map((row) => {
        const here = row.head === proof.head
        /** @type {Criterion['status']} */
        const status =
          here && row.result === 'met' && row.check
            ? 'done'
            : here && row.result === 'unmet'
              ? 'missed'
              : 'unverified'
        return {
          id: criterionAnchor(row.id),
          label: row.id,
          text: row.text,
          status,
          ...(here ? {} : { note: `Checked on \`${row.head.slice(0, 7)}\`, an earlier head` }),
          ...(row.check ? { check: row.check } : {}),
        }
      }),
  ]

  // Links join a finding to the risk at its lines, and to each decision and criterion that names
  // its file, both ways; the author's links join the items they name the same way. A note never
  // links: it blocks nothing and would only add noise.
  /** @param {string} text @param {string} path */
  const names = (text, path) => {
    const at = text.indexOf(path)
    return (
      at >= 0 && !/[\w./-]/.test(text[at - 1] ?? '') && !/[\w/-]/.test(text[at + path.length] ?? '')
    )
  }
  /** @type {Map<string, Link[]>} */
  const links = new Map()
  /** @param {{id: string, ref: string, title: string}} a @param {{id: string, ref: string, title: string}} b */
  const join = (a, b) => {
    for (const [from, to] of [
      [a, b],
      [b, a],
    ]) {
      const have = links.get(from.id) ?? []
      if (from.id === to.id || have.some((link) => link.href === `#${to.id}`)) continue
      links.set(from.id, [
        ...have,
        { href: `#${to.id}`, label: to.ref, title: capText(to.title, 120) },
      ])
    }
  }
  const riskRefs = risks.map((r, n) => ({ ...r, ref: `R${n + 1}` }))
  const decisionRefs = decisions.map((d, n) => ({ ...d, ref: `D${n + 1}` }))
  for (const f of kept) {
    const path = f.at?.replace(/:\d+(?:-\d+)?$/, '')
    if (!path || f.state === 'note') continue
    const line = Number(/:(\d+)/.exec(f.at ?? '')?.[1] ?? NaN)
    const finding = { id: f.id, ref: f.ref, title: f.text }
    for (const r of riskRefs)
      if (r.at?.path === path && !(Math.abs(r.at.line - line) > LINK_LINES))
        join(finding, { id: r.id, ref: r.ref, title: r.title })
    for (const d of decisionRefs)
      if (names(`${d.text} ${d.why ?? ''}`, path))
        join(finding, { id: d.id, ref: d.ref, title: d.text })
    for (const c of criteria)
      if (names(`${c.text} ${c.check ?? ''}`, path))
        join(finding, { id: c.id, ref: c.label, title: c.text })
  }
  /** Each spec id and finding id, as the page item it became. @type {Map<string, {id: string, ref: string, title: string}>} */
  const bySpecId = new Map()
  for (const r of riskRefs) if (r.specId) bySpecId.set(r.specId, r)
  for (const d of decisionRefs) if (d.specId) bySpecId.set(d.specId, { ...d, title: d.text })
  for (const c of fit?.criteria ?? [])
    bySpecId.set(c.id, { id: criterionAnchor(c.id), ref: c.id, title: c.text })
  for (const f of kept)
    if (f.source) bySpecId.set(f.source, { id: f.id, ref: f.ref, title: f.text })
  for (const item of [...riskRefs, ...decisionRefs])
    for (const target of item.specLinks ?? []) {
      const to = bySpecId.get(target)
      if (to)
        join({ id: item.id, ref: item.ref, title: 'text' in item ? item.text : item.title }, to)
    }
  /** @type {Finding[]} */
  const findings = kept.map(({ source, ...f }) => ({ ...f, links: links.get(f.id) ?? [] }))
  /** The findings a criterion links to, which show as its evidence. @param {string} id */
  const found = (id) => (links.get(id) ?? []).filter((link) => link.href.startsWith('#finding-'))
  /** One cited piece of evidence as the chip the page shows. @param {import('./verify-proof-spec.mjs').SpecEvidence} ev @returns {Evidence} */
  const chip = (ev) => {
    if (ev.kind === 'finding') {
      const to = bySpecId.get(ev.ref)
      return to
        ? { kind: 'finding', label: to.ref, title: capText(to.title, 120), href: `#${to.id}` }
        : { kind: 'finding', label: ev.label }
    }
    if (ev.kind === 'test' || ev.kind === 'code') {
      const href = fileHref(ev.ref.replace(/:\d+$/, ''))
      return { kind: ev.kind, label: ev.label, title: ev.ref, ...(href ? { href } : {}) }
    }
    return { kind: ev.kind, label: ev.label }
  }
  /** @type {TicketFit | null} */
  const ticket = criteria.length
    ? {
        id: input.ticket?.id ?? null,
        url: input.ticket?.url ?? null,
        verdict: ticketVerdict(criteria),
        ...(fit?.summary ? { summary: fit.summary } : {}),
        criteria: criteria.map(({ check, cited, ...c }) => ({
          ...c,
          evidence: [
            ...(check ? [{ kind: 'check', label: check }] : []),
            ...(cited ?? []).map(chip),
            ...found(c.id)
              .filter((link) => !(cited ?? []).some((ev) => chip(ev).href === link.href))
              .map((link) => ({ kind: 'finding', ...link })),
          ],
          links: (links.get(c.id) ?? []).filter((link) => !found(c.id).includes(link)),
        })),
        skipped: fit?.skipped ?? [],
        contradictions: fit?.contradictions ?? [],
      }
    : null

  const totals = files && {
    files: files.length,
    adds: files.reduce((n, f) => n + (f.adds ?? 0), 0),
    dels: files.reduce((n, f) => n + (f.dels ?? 0), 0),
  }

  // Files: one group per area a person reviews, then the rest by kind, largest file first.
  // A no-sign-off file joins the files that need no sign-off.
  const askingNames = new Map(
    reviewed.filter((area) => area.id !== NO_SIGN_OFF_AREA).map((area) => [area.id, area.name]),
  )
  /** @param {string} path */
  const groupOf = (path) => {
    const owner = (extras.areas ?? []).find(
      (entry) => askingNames.has(entry.name) && entry.paths.includes(path),
    )
    if (owner) return `area:${owner.name}`
    if (GENERATED_PATH.test(path)) return 'Generated'
    if (TEST_PATH.test(path)) return 'Tests'
    return 'No sign-off needed'
  }
  /** @type {Map<string, FileGroup>} */
  const groups = new Map(
    [
      ...[...askingNames].map(([id, name]) => [`area:${id}`, name]),
      ...OTHER_GROUPS.map((name) => [name, name]),
    ].map(([key, name]) => [key, { name, adds: 0, dels: 0, files: [] }]),
  )
  for (const f of files ?? []) {
    const group = /** @type {FileGroup} */ (groups.get(groupOf(f.path)))
    const row = {
      path: f.path,
      adds: f.adds ?? 0,
      dels: f.dels ?? 0,
      binary: f.adds === null,
      href: fileHref(f.path),
    }
    group.files.push(row)
    group.adds += row.adds
    group.dels += row.dels
  }
  const fileGroups = [...groups.values()]
    .filter((group) => group.files.length)
    .map((group) => ({
      ...group,
      files: group.files.sort((a, b) => b.adds + b.dels - (a.adds + a.dels)),
    }))

  // Ticket ids link only to the judged ticket or where a link the record carries names the ticket.
  /** @type {Record<string, string>} */
  const tickets = input.ticket ? { [input.ticket.id]: input.ticket.url } : {}
  for (const link of proof.areas.flatMap((area) => area.links ?? [])) {
    const id = TICKET_ID.test(link.text) ? link.text : TICKET_IN_URL.exec(link.url)?.[1]
    if (id && !tickets[id]) tickets[id] = link.url
  }

  const level =
    !view.computed || proof.confidence.level === 'cannot-assess'
      ? 'unrated'
      : proof.confidence.level
  // One row per reviewed area, saying what changed there: the spec's summary, the area's facts,
  // else the files it covers. An area with nothing to say joins Not proven instead.
  /** @param {string} id */
  const fileNames = (id) => {
    const paths = new Set((extras.areas ?? []).find((entry) => entry.name === id)?.paths ?? [])
    return (files ?? [])
      .filter((f) => paths.has(f.path))
      .map((f) => f.path.slice(f.path.lastIndexOf('/') + 1))
  }
  /** @type {Signal[]} */
  const signals = []
  const keys = new Set()
  /** The signals an area's describers sent, which replace its row; each key shows once.
   * @param {string} id @param {string | null} href */
  const typedSignals = (id, href) =>
    (typed(id).signals ?? []).flatMap((signal) => {
      if (signal.state === 'none' || keys.has(signal.key)) return []
      keys.add(signal.key)
      return [
        { label: signal.label, summary: signal.summary, pill: SIGNAL_PILLS[signal.state], href },
      ]
    })
  for (const area of reviewed) {
    const href = sections.includes(area) ? `#a-${area.id}` : null
    const rows = typedSignals(area.id, href)
    if (rows.length) {
      signals.push(...rows)
      continue
    }
    const names = fileNames(area.id)
    const summary =
      (area.id === NO_SIGN_OFF_AREA && area.changes.join('; ')) ||
      area.summary ||
      area.facts.map((fact) => fact.text).join('; ') ||
      (names.length
        ? `Changes ${joinList(names.length > 3 ? [...names.slice(0, 3), `${names.length - 3} more`] : names)}`
        : '')
    if (summary)
      signals.push({
        label: area.name,
        summary,
        pill: area.pill,
        href,
      })
    else unproven.push(UNSHOWN[area.id] ?? `${area.name}: nothing on this page shows it`)
  }
  for (const area of proof.areas)
    if (!reviewed.some((entry) => entry.id === area.name))
      signals.push(...typedSignals(area.name, null))

  /** @type {string[]} */
  const inputs = ['proof record']
  if (spec) inputs.push('agent spec')
  if (media) inputs.push('media manifest')
  return {
    repo: proof.repo,
    pr: proof.pr,
    head: proof.head,
    generatedAt: extras.generatedAt ?? new Date().toISOString(),
    base: extras.mergeBase ?? proof.base,
    digest: proof.digest,
    title: spec?.title ?? null,
    reviewUrl: input.reviewUrl ?? null,
    filesUrl: input.filesUrl ?? null,
    branch: input.branch ?? null,
    prUrl,
    tickets,
    totals,
    banner: {
      level,
      ...LEVELS[/** @type {keyof typeof LEVELS} */ (level)],
      // The author's reason explains any level but a blocked or unrated one, which say why themselves.
      reason:
        spec?.reason && level !== 'blocked' && level !== 'unrated'
          ? spec.reason
          : bannerReason(proof, input.verdict?.label),
      unproven,
      signals,
    },
    risks: risks.map(({ specId, specLinks, ...r }) => ({ ...r, links: links.get(r.id) ?? [] })),
    decisions: decisions.map(({ specId, specLinks, ...d }) => ({
      ...d,
      links: links.get(d.id) ?? [],
    })),
    ticket,
    areas: sections,
    quoted: new Set(sections.flatMap((area) => area.snippets.map((s) => s.path))),
    evidence: {
      heads: heads.map((clip, index) => ({ clip, index })),
      pairs,
      stills: heads.flatMap((clip) => clip.stills),
      transcripts: media?.transcripts ?? [],
      documents,
    },
    findings,
    afterMerge,
    files: files ? fileGroups : null,
    debug: {
      inputs,
      problems,
      uncertain: spec?.uncertain ?? [],
      health: view.healthStrip,
      verdict: view.verdict,
      reviewers: (proof.review?.models ?? []).map(
        (model) => `${model.agent}: ${model.outcome}${model.detail ? ` (${model.detail})` : ''}`,
      ),
      source: media?.source ?? null,
    },
  }
}
