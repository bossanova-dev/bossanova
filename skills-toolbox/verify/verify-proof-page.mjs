/** Portable, script-free proof page and matching comment renderer. */
import { createHash } from 'node:crypto'
import { readFileSync, writeFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { validateProof, checkProofUrl, LEVELS, MAX_TEXT_CHARS } from './verify-proof.mjs'

export const MAX_EXCERPT_LINES = 30
export const MAX_LINE_CHARS = 200
export const MAX_ASKS_BEFORE_SPLIT = 3
export const MAX_TIMELINE = 20
export const MAX_AFTER_MERGE = 20
export const MAX_COMMENT_BLOCK_CHARS = 40_000
/** @type {Readonly<Record<string, string>>} */
export const LEVEL_GUIDANCE = Object.freeze({
  high: 'Ready for your decision',
  moderate: 'Inspect the asks before deciding',
  low: 'Review the gaps before deciding',
  blocked: 'Resolve the blockers first',
  'cannot-assess': 'Gather evidence before deciding',
})
/**
 * @typedef {import('./verify-proof.mjs').Proof} Proof
 * @typedef {{proof: Proof, asks?: Record<string, {diff?: string, plan?: string}>, rates?: Record<string, {fixed: number, total: number}>, timeline?: {at: string, text: string}[], afterMerge?: string[], filesUrl?: string}} PageInput
 * @typedef {{label: string, lines: string[], total: number, remaining: number}} Excerpt
 * @typedef {{label: string, items: string[], excerpts: Excerpt[], media: {kind: string, url: string, caption: string, inline: boolean, poster?: string}[]}} Ask
 * @typedef {{title: string, bullets: string[], folded?: boolean, asks?: Ask[], state?: 'known'|'unknown', source?: string}} Section
 */
/** @param {unknown} value @returns {value is Record<string, unknown>} */
const object = (value) =>
  value !== null &&
  typeof value === 'object' &&
  !Array.isArray(value) &&
  (Object.getPrototypeOf(value) === Object.prototype || Object.getPrototypeOf(value) === null)
/** @param {Record<string, unknown>} value @param {string[]} keys @param {string} path @param {string[]} errors */
function keys(value, keys, path, errors) {
  for (const key of Object.keys(value))
    if (!keys.includes(key)) errors.push(`${path}.${key}: unknown key`)
}
/** @param {unknown} value */
const short = (value) =>
  typeof value === 'string' && value.length > 0 && value.length <= MAX_TEXT_CHARS
/** Strictly validate untrusted sidecars before constructing either output.
 * @param {unknown} value @returns {{ok: boolean, errors: string[], input: PageInput | null}}
 */
export function validatePageInput(value) {
  /** @type {string[]} */
  const errors = []
  if (!object(value)) return { ok: false, errors: ['input: expected object'], input: null }
  keys(value, ['proof', 'asks', 'rates', 'timeline', 'afterMerge', 'filesUrl'], 'input', errors)
  const proof = validateProof(value.proof)
  errors.push(...proof.errors)
  if (Object.hasOwn(value, 'filesUrl')) checkProofUrl(value.filesUrl, 'filesUrl', errors)
  for (const name of ['asks', 'rates']) {
    if (!Object.hasOwn(value, name)) continue
    const map = value[name]
    if (!object(map)) {
      errors.push(`${name}: expected map`)
      continue
    }
    for (const [key, entry] of Object.entries(map)) {
      if (!object(entry)) {
        errors.push(`${name}.${key}: expected object`)
        continue
      }
      if (name === 'asks') {
        if (!proof.proof?.confidence.asks.includes(key))
          errors.push(`asks.${key}: not an asking area`)
        keys(entry, ['diff', 'plan'], `asks.${key}`, errors)
        for (const field of ['diff', 'plan'])
          if (Object.hasOwn(entry, field) && typeof entry[field] !== 'string')
            errors.push(`asks.${key}.${field}: expected string`)
      } else {
        if (!LEVELS.includes(key)) errors.push(`rates.${key}: unknown level`)
        keys(entry, ['fixed', 'total'], `rates.${key}`, errors)
        if (
          !Number.isSafeInteger(entry.fixed) ||
          !Number.isSafeInteger(entry.total) ||
          Number(entry.fixed) < 0 ||
          Number(entry.total) < 1 ||
          Number(entry.fixed) > Number(entry.total)
        )
          errors.push(`rates.${key}: invalid counts`)
      }
    }
  }
  for (const [name, cap] of /** @type {[string, number][]} */ ([
    ['timeline', MAX_TIMELINE],
    ['afterMerge', MAX_AFTER_MERGE],
  ])) {
    if (!Object.hasOwn(value, name)) continue
    const entries = value[name]
    if (!Array.isArray(entries) || entries.length > cap) {
      errors.push(`${name}: expected at most ${cap} entries`)
      continue
    }
    for (const entry of entries) {
      if (name === 'afterMerge') {
        if (!short(entry)) errors.push('afterMerge: invalid text')
        continue
      }
      if (!object(entry)) {
        errors.push('timeline: expected object')
        continue
      }
      keys(entry, ['at', 'text'], 'timeline', errors)
      if (
        !short(entry.text) ||
        typeof entry.at !== 'string' ||
        !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)$/.test(entry.at) ||
        !Number.isFinite(Date.parse(entry.at)) ||
        new Date(`${entry.at.slice(0, 10)}T00:00:00Z`).toISOString().slice(0, 10) !==
          entry.at.slice(0, 10)
      )
        errors.push('timeline: invalid timestamp or text')
    }
  }
  return {
    ok: errors.length === 0,
    errors,
    input: errors.length ? null : /** @type {PageInput} */ (value),
  }
}
/** @param {string} value */
export const escapeHtml = (value) =>
  value.replace(
    /[&<>"']/g,
    (char) =>
      /** @type {Record<string,string>} */ ({
        '&': '&amp;',
        '<': '&lt;',
        '>': '&gt;',
        '"': '&quot;',
        "'": '&#39;',
      })[char],
  )
/** @param {string} value */
const markdown = (value) =>
  value
    .replace(/[\r\n\u2028\u2029]+/g, ' ')
    .replace(/[&<>]/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' })[char] || char)
    .replace(/[\\`*_\[\]|]/g, '\\$&')
/** A markdown destination never exposes parentheses or tag delimiters.
 * @param {string} value */
const destination = (value) =>
  new URL(value).href.replace(
    /[()<>"'`\\]/g,
    (char) => `%${char.charCodeAt(0).toString(16).toUpperCase()}`,
  )
/** @param {unknown} value */
export function proofView(value) {
  const result = validatePageInput(value)
  if (!result.input) throw new TypeError(result.errors.join('\n'))
  const input = result.input
  const { proof } = input
  const level = proof.confidence.level
  const display =
    level === 'cannot-assess' ? 'Cannot assess' : level[0].toUpperCase() + level.slice(1)
  const headline = `Confidence: ${display} — ${LEVEL_GUIDANCE[level]}`
  const rate = input.rates?.[level]
  /** @type {Section[]} */
  const sections = [
    {
      title: 'Confidence',
      bullets: [
        headline,
        ...(rate
          ? [
              `later fixed: ${((100 * rate.fixed) / rate.total).toFixed(1)}%, ${rate.fixed} of ${rate.total}`,
            ]
          : []),
      ],
    },
    {
      title: 'What it does',
      state: proof.whatItDoes.source === 'unknown' ? 'unknown' : 'known',
      bullets: proof.whatItDoes.bullets.length ? [...proof.whatItDoes.bullets] : ['intent unknown'],
      ...(proof.whatItDoes.source === 'unknown'
        ? {}
        : {
            source: `Source: ${proof.whatItDoes.source}${proof.whatItDoes.ref ? ` (${proof.whatItDoes.ref})` : ''}`,
          }),
    },
  ]
  if (level !== 'high')
    sections.push({
      title: 'Why not high',
      bullets: [
        ...proof.confidence.loweredBy.map((reason) => reason.text),
        ...(proof.review?.blockers.map(
          (blocker) => `${blocker.title}${blocker.where ? ` (${blocker.where})` : ''}`,
        ) || []),
      ],
    })
  const pageOrigin = proof.page ? new URL(proof.page).origin : null
  const asks = proof.confidence.asks.map((name) => {
    const area = /** @type {import('./verify-proof.mjs').ProofArea} */ (
      proof.areas.find((area) => area.name === name)
    )
    /** @type {Excerpt[]} */
    const excerpts = []
    for (const field of ['diff', 'plan']) {
      const text = input.asks?.[name]?.[/** @type {'diff'|'plan'} */ (field)]
      if (text === undefined) continue
      const lines = text.split(/\r\n|\n|\r/)
      excerpts.push({
        label: field,
        lines: lines
          .slice(0, MAX_EXCERPT_LINES)
          .map((line) =>
            line.length > MAX_LINE_CHARS ? `${line.slice(0, MAX_LINE_CHARS - 1)}…` : line,
          ),
        total: lines.length,
        remaining: Math.max(0, lines.length - MAX_EXCERPT_LINES),
      })
    }
    return {
      label: area.label || area.name,
      items: [...area.items],
      excerpts,
      media: area.media.map((media) => ({
        kind: media.kind,
        url: media.url,
        caption: media.caption || media.kind,
        inline: pageOrigin !== null && new URL(media.url).origin === pageOrigin,
        ...(media.poster && new URL(media.poster).origin === pageOrigin
          ? { poster: media.poster }
          : {}),
      })),
    }
  })
  const split =
    asks.length > MAX_ASKS_BEFORE_SPLIT ||
    asks.some((ask) => ask.excerpts.some((excerpt) => excerpt.remaining > 0))
  sections.push({ title: 'Asks', bullets: split ? ['split suggested'] : [], asks })
  sections.push({
    title: 'Checked for you',
    folded: true,
    bullets: [
      ...proof.areas
        .filter((area) => !area.ask)
        .flatMap((area) => [
          `${area.label || area.name}: ${area.proven ? 'proven' : 'not shown'}`,
          ...area.items,
        ]),
      ...(proof.review?.models.map((model) => `${model.agent}: ${model.outcome}`) || []),
      ...(proof.review?.notes || []),
    ],
  })
  if (input.timeline?.length)
    sections.push({
      title: 'Timeline',
      bullets: input.timeline.map((entry) => `${entry.at}: ${entry.text}`),
    })
  if (input.afterMerge?.length)
    sections.push({ title: 'After merge', bullets: [...input.afterMerge] })
  return { level, headline, page: proof.page, filesUrl: input.filesUrl, sections }
}
const STYLE = `:root{color-scheme:light dark;font:16px/1.5 system-ui,sans-serif}body{max-width:60rem;margin:2rem auto;padding:0 1rem;background:#fff;color:#222}h1{font-size:1.6rem}h2{font-size:1.25rem}pre{overflow:auto;padding:1rem;background:#f3f3f3}img,video{max-width:100%}details{margin:1rem 0}summary{cursor:pointer;font-weight:600}li{margin:.3rem 0}a{color:#1659b8}@media(prefers-color-scheme:dark){body{background:#171717;color:#eee}pre{background:#282828}a{color:#95bdff}}`
/** @param {unknown} input */
export function renderProofHtml(input) {
  const view = proofView(input)
  /** @param {string[]} items */
  const bullets = (items) =>
    items.length ? `<ul>${items.map((item) => `<li>${escapeHtml(item)}</li>`).join('')}</ul>` : ''
  const files = view.filesUrl ? ` <a href="${escapeHtml(view.filesUrl)}">Files</a>` : ''
  const sections = view.sections
    .map((section) => {
      const content =
        bullets(section.bullets) +
        (section.source ? `<p>${escapeHtml(section.source)}</p>` : '') +
        (section.asks || [])
          .map(
            (ask) =>
              `<h3>${escapeHtml(ask.label)}</h3>${bullets(ask.items)}${ask.excerpts.map((excerpt) => `<h4>${escapeHtml(excerpt.label)}</h4><pre>${escapeHtml(excerpt.lines.join('\n'))}</pre>${excerpt.remaining ? bullets([`${excerpt.remaining} more lines`]) + files : ''}`).join('')}${ask.media.map((media) => (media.inline ? `<figure>${media.kind === 'video' ? `<video controls preload="none" src="${escapeHtml(media.url)}"${media.poster ? ` poster="${escapeHtml(media.poster)}"` : ''}></video>` : `<img src="${escapeHtml(media.url)}" alt="${escapeHtml(media.caption)}">`}<figcaption>${escapeHtml(media.caption)}</figcaption></figure>` : `<ul><li><a href="${escapeHtml(media.url)}">${escapeHtml(media.caption)}</a></li></ul>`)).join('')}`,
          )
          .join('')
      return section.folded
        ? `<details><summary>${escapeHtml(section.title)}</summary>${content}</details>`
        : `<section><h2>${escapeHtml(section.title)}</h2>${content}</section>`
    })
    .join('')
  const hash = createHash('sha256').update(STYLE).digest('base64')
  return `<!doctype html><html lang="en"><head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'sha256-${hash}'; img-src 'self'; media-src 'self'; base-uri 'none'; form-action 'none'"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Proof</title><style>${STYLE}</style></head><body><main>${sections}</main></body></html>`
}
/** @param {unknown} input */
export function renderProofComment(input) {
  const view = proofView(input)
  const collapsed = new Set()
  /** @param {string[]} items */
  const bullets = (items) => items.map((item) => `- ${markdown(item)}`).join('\n')
  const files = view.filesUrl ? ` [Files](${destination(view.filesUrl)})` : ''
  const excerpts = view.sections
    .flatMap((section) => section.asks || [])
    .flatMap((ask) => ask.excerpts)
  function render() {
    const sections = view.sections
      .map((section) => {
        const content = [
          bullets(section.bullets),
          ...(section.source ? [markdown(section.source)] : []),
          ...(section.asks || []).map((ask) =>
            [
              `### ${markdown(ask.label)}`,
              bullets(ask.items),
              ...ask.excerpts.map((excerpt) => {
                if (collapsed.has(excerpt))
                  return `- ${markdown(excerpt.label)}: ${excerpt.total} lines${files}`
                const text = excerpt.lines.join('\n').replace(/<!--/g, '<\u200b!--')
                const runs = text.match(/`+/g) || []
                const fence = '`'.repeat(Math.max(3, ...runs.map((run) => run.length + 1)))
                // Fences preserve literal source; neutralize comment markers for raw-body readers.
                return `- ${markdown(excerpt.label)}\n\n${fence}${excerpt.label === 'diff' ? 'diff' : ''}\n${text}\n${fence}${excerpt.remaining ? `\n\n- ${excerpt.remaining} more lines${files}` : ''}`
              }),
              ...ask.media.map(
                (media) => `- [${markdown(media.caption)}](${destination(media.url)})`,
              ),
            ]
              .filter(Boolean)
              .join('\n\n'),
          ),
        ]
          .filter(Boolean)
          .join('\n\n')
        return section.folded
          ? `<details>\n<summary>${markdown(section.title)}</summary>\n\n${content}\n\n</details>`
          : `## ${markdown(section.title)}\n\n${content}`
      })
      .join('\n\n')
    return `<details>\n<summary>${markdown(view.headline)}</summary>\n\n${view.page ? `[Full page](${destination(view.page)})\n\n` : ''}${sections}\n\n</details>`
  }
  let output = render()
  for (const excerpt of [...excerpts].reverse()) {
    if (output.length <= MAX_COMMENT_BLOCK_CHARS) break
    collapsed.add(excerpt)
    output = render()
  }
  if (output.length > MAX_COMMENT_BLOCK_CHARS)
    throw new RangeError('Proof comment exceeds size budget after collapsing excerpts')
  return output
}

if (isMainModule(import.meta.url)) {
  try {
    const { positionals, values } = parseArgs({
      strict: true,
      allowPositionals: true,
      options: {
        proof: { type: 'string' },
        input: { type: 'string' },
        html: { type: 'string' },
        comment: { type: 'string' },
      },
    })
    if (positionals.length !== 1 || positionals[0] !== 'render' || !values.proof || !values.html)
      throw new Error(
        'Usage: verify-proof-page.mjs render --proof <file> [--input <file>] --html <out> [--comment <out>]',
      )
    const proof = JSON.parse(readFileSync(values.proof, 'utf8'))
    const sidecar = values.input ? JSON.parse(readFileSync(values.input, 'utf8')) : {}
    if (!object(sidecar)) throw new TypeError('input: expected sidecar object')
    if (Object.hasOwn(sidecar, 'proof'))
      throw new TypeError('input.proof: proof belongs in --proof')
    const input = { ...sidecar, proof }
    const html = renderProofHtml(input)
    const comment = values.comment ? renderProofComment(input) : null
    writeFileSync(values.html, html)
    if (values.comment && comment !== null) writeFileSync(values.comment, comment)
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = 2
  }
}
