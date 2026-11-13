/**
 * The proof page HTML from a page model: the banner, then Risks, Decisions, one section per area
 * a person reviews, the captured evidence, Findings, After merge and Files, each only when it has
 * something to show, with each section the repository declared after the one it names, and the generation diagnostics that only `?debug` reveals. Every value is
 * escaped here. The only script is the static player beside the page, which every page links for
 * its theme, ticks, section links, lightbox and video; the page reads fine without it.
 */
import { createHash } from 'node:crypto'
import { colourLine, escapeHtml, languageOf, proseHtml } from './verify-proof-code.mjs'
import { mediaName } from './verify-proof-media.mjs'
import { blocksHtml } from './verify-proof-page-blocks.mjs'
import { fileAnchor } from './verify-proof-page-model.mjs'
import { PROOF_PLAYER_FILE } from './verify-proof-player.mjs'
import { PROOF_STYLE } from './verify-proof-style.mjs'

/**
 * @typedef {import('./verify-proof-page-model.mjs').PageModel} PageModel
 * @typedef {import('./verify-proof-page-model.mjs').AreaItem} AreaItem
 * @typedef {import('./verify-proof-page-model.mjs').Tick} Tick
 * @typedef {import('./verify-proof-page-model.mjs').Pill} Pill
 * @typedef {import('./verify-proof-snippet.mjs').DiffRow} DiffRow
 * @typedef {import('./verify-proof-page-model.mjs').DocumentPair} DocumentPair
 * @typedef {import('./verify-proof-page-sections.mjs').RepoSection} RepoSection
 * @typedef {import('./verify-proof-page-sections.mjs').SectionPart} SectionPart
 * @typedef {import('./verify-proof-media.mjs').ProofDocument} ProofDocument
 * @typedef {{id: string, nav: string, title: string, count: number, sub?: string, pill?: Pill, hot?: boolean, body: string}} Section
 */

const e = escapeHtml
/** `m:ss`. @param {number} ms */
const clock = (ms) => {
  const total = Math.floor(ms / 1000)
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`
}
/** @param {number} n @param {string} one */
const plural = (n, one) => `${n} ${one}${n === 1 ? '' : 's'}`
/** @param {number} n */
const num = (n) => n.toLocaleString('en')
const SIGN = Object.freeze({ add: '+', del: '−', ctx: ' ' })
/** @param {Pill} pill */
const pillHtml = (pill) => `<span class="pill ${pill.tone}">${e(pill.word)}</span>`
/** A path's last segment and line, the whole location as its title; the line always shows, and a long name loses its start. @param {string} at */
const location = (at) => {
  const [, path = at, line = ''] = /^(.*?)(:\d+(?:-\d+)?)?$/.exec(at) ?? []
  const name = path.slice(path.lastIndexOf('/') + 1)
  const shown = name.length > 30 ? `\u2026${name.slice(-29)}` : name
  return `<code class="loc" title="${e(at)}">${e(shown)}${e(line)}</code>`
}
/** Chips that jump to the items this one links to. @param {import('./verify-proof-page-model.mjs').Link[] | undefined} links */
const linkChips = (links) =>
  links?.length
    ? `<span class="links">${links.map((l) => `<a class="lk" href="${e(l.href)}" title="${e(l.title)}">${e(l.label)}</a>`).join('')}</span>`
    : ''
/** `+N −N` in the theme's add and remove colours. @param {number} adds @param {number} dels */
const diffstat = (adds, dels) =>
  `<span class="diffstat"><span class="a">+${num(adds)}</span> <span class="d">−${num(dels)}</span></span>`

/** One GitHub-style diff figure; `pinOld` marks a deleted line by its base-side number. @param {{path: string, rows: DiffRow[], adds: number, dels: number, start?: number, pinLine?: number | null, pinOld?: number, pinNote?: string, more?: string, id?: string}} diff @param {Readonly<Record<string, string>>} tickets */
function diffFigure(diff, tickets) {
  const lang = languageOf(diff.path)
  const firstOld = diff.rows.find((row) => row.old !== null)?.old ?? 0
  const firstNew = diff.start ?? diff.rows.find((row) => row.new !== null)?.new ?? 0
  const oldCount = diff.rows.filter((row) => row.old !== null).length
  const newCount = diff.rows.filter((row) => row.new !== null).length
  const rows = diff.rows
    .map((row) => {
      const pinned =
        (diff.pinLine != null && row.new === diff.pinLine) ||
        (diff.pinOld !== undefined && row.kind === 'del' && row.old === diff.pinOld)
          ? ' pinned'
          : ''
      const text = row.cut
        ? `${colourLine(row.text.slice(0, -1), lang)}<span class="cut">… line cut</span>`
        : colourLine(row.text, lang)
      return `<tr class="${row.kind}${pinned}"><td class="n">${row.old ?? ''}</td><td class="n">${row.new ?? ''}</td><td class="m">${SIGN[row.kind]}</td><td class="t">${text}</td></tr>`
    })
    .join('')
  const hunk = `<tr class="hunk"><td class="n"></td><td class="n"></td><td class="m"></td><td class="t">@@ -${firstOld},${oldCount} +${firstNew},${newCount} @@</td></tr>`
  const note = diff.pinNote ? `<div class="pinnote">${proseHtml(diff.pinNote, tickets)}</div>` : ''
  return `<figure class="code"${diff.id ? ` id="${diff.id}"` : ''}><figcaption><span class="path" title="${e(diff.path)}">${e(diff.path || 'excerpt')}</span>${diffstat(diff.adds, diff.dels)}</figcaption><div class="scroll"><table>${hunk}${rows}</table></div>${note}${diff.more ?? ''}</figure>`
}

/** Five squares split by the share of lines added and removed. @param {number} adds @param {number} dels */
function sizeBlocks(adds, dels) {
  const all = adds + dels
  const a = all ? Math.floor((adds / all) * 5) : 0
  const d = all ? Math.floor((dels / all) * 5) : 0
  return `<span class="blocks" aria-hidden="true">${[0, 1, 2, 3, 4].map((i) => `<i${i < a ? ' class="a"' : i < a + d ? ' class="d"' : ''}></i>`).join('')}</span>`
}

/** @param {PageModel} m */
function header(m) {
  const t = m.totals
  const blocks = t ? sizeBlocks(t.adds, t.dels) : ''
  const name = `${e(m.repo)} #${m.pr}`
  const prLink = m.prUrl
    ? `<a class="prlink" href="${e(m.prUrl)}" target="_blank" rel="noopener noreferrer">${name}</a>`
    : name
  const meta = [
    m.title ? (m.prUrl ? prLink : `<span>${name}</span>`) : '',
    t ? `<span>${diffstat(t.adds, t.dels)}${blocks} ${plural(t.files, 'file')}</span>` : '',
    `<code class="sha">${m.head.slice(0, 7)}</code>`,
    m.filesUrl ? `<a href="${e(m.filesUrl)}">Files</a>` : '',
    m.reviewUrl ? `<a href="${e(m.reviewUrl)}">Review this head</a>` : '',
  ]
  return `<header class="pr">${branchNotice(m.branch)}<h1>${m.title ? proseHtml(m.title, m.tickets) : prLink}</h1><div class="meta">${meta.join('')}</div></header>`
}

const BRANCH_ICON =
  '<svg class="bi" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><circle cx="4.5" cy="3.5" r="1.6"/><circle cx="4.5" cy="12.5" r="1.6"/><circle cx="11.5" cy="5.5" r="1.6"/><path d="M4.5 5.1v5.8M11.5 7.1c0 2.4-2 3-7 3.6"/></svg>'
const STACK_ICON =
  '<svg class="bi" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M8 2 14 5 8 8 2 5z"/><path d="M2 8l6 3 6-3M2 11l6 3 6-3"/></svg>'

/**
 * Where the PR merges, above its title: stacked on another open PR (by its base or by the commits
 * it carries) or merging into a branch that is not the default. Nothing for a PR into the default
 * branch that carries no one else's work.
 * @param {PageModel['branch']} b
 */
function branchNotice(b) {
  if (!b) return ''
  const parent = b.stackedOn[0]
  const link = (/** @type {typeof parent} */ p) =>
    `<a href="${e(p.url)}" target="_blank" rel="noopener noreferrer">#${p.pr}</a>`
  const notice = (
    /** @type {string} */ kind,
    /** @type {string} */ head,
    /** @type {string} */ body,
  ) =>
    `<div class="bnotice ${kind}">${kind === 'stacked' ? STACK_ICON : BRANCH_ICON}<div><b>${head}</b><span>${body}</span></div></div>`
  if (parent?.how === 'base')
    return notice(
      'stacked',
      `Stacked on ${link(parent)}.`,
      `It merges into <code>${e(parent.branch)}</code>, not <code>${e(b.default)}</code>. Review it after ${link(parent)} merges.`,
    )
  if (parent) {
    const behind = parent.total - parent.shared
    return notice(
      'stacked',
      `Carries ${parent.shared} of ${link(parent)}'s ${plural(parent.total, 'commit')}.`,
      `Merging this ships them too${behind ? `, and its copy is ${plural(behind, 'commit')} behind` : ''}. Review it after ${link(parent)} merges.`,
    )
  }
  if (b.base !== b.default)
    return notice(
      'branch',
      `Merges into <code>${e(b.base)}</code>.`,
      `Not the default branch <code>${e(b.default)}</code>.`,
    )
  return ''
}

/** The confidence banner: level, why, the risks, what is not proven, one row per reviewed area. @param {PageModel} m */
function banner(m) {
  const b = m.banner
  const rows = (/** @type {string} */ key, /** @type {string[]} */ items) =>
    items.map((item, i) => `<li><span class="k">${i ? '' : key}</span>${item}</li>`).join('')
  const risks = m.risks.map(
    (r) =>
      `<a href="#${r.id}"><span class="dot ${r.tone}" title="${e(r.level)}"></span>${proseHtml(r.title)}</a>`,
  )
  const unproven = b.unproven.map((text) => `<span>${proseHtml(text, m.tickets)}</span>`)
  const signals = b.signals
    .map(
      (s) =>
        `<${s.href ? `a class="r" href="${e(s.href)}"` : 'div class="r"'}><span class="k">${e(s.label)}</span><span class="v">${proseHtml(s.summary)}</span><span class="st">${pillHtml(s.pill)}</span></${s.href ? 'a' : 'div'}>`,
    )
    .join('')
  return `<section class="banner lv-${b.tone}" aria-label="Confidence"><div class="lv"><b>${e(b.word)}</b>${b.level === 'unrated' ? '' : '<span>confidence</span>'}</div><p class="reason">${proseHtml(b.reason, m.tickets)}</p>${risks.length ? `<ul class="glist rsum">${rows('Risks', risks)}</ul>` : ''}${unproven.length ? `<ul class="glist gaps">${rows('Not proven', unproven)}</ul>` : ''}${signals ? `<div class="sig">${signals}</div>` : ''}</section>`
}

/** @param {PageModel} m @returns {Section | null} */
function risks(m) {
  if (!m.risks.length) return null
  const body = m.risks
    .map((r) => {
      const at = r.at
        ? m.quoted.has(r.at.path)
          ? `<a class="lk" href="#${fileAnchor(r.at.path)}">${location(`${r.at.path}:${r.at.line}`)}</a>`
          : location(`${r.at.path}:${r.at.line}`)
        : ''
      const items = r.items?.length
        ? `<ul class="facts">${r.items.map((i) => `<li>${proseHtml(i, m.tickets)}</li>`).join('')}</ul>`
        : ''
      return `<div class="item" id="${r.id}"><div class="hd">${pillHtml({ word: r.level, tone: r.tone })}<div class="q">${proseHtml(r.title, m.tickets)}</div>${at}${linkChips(r.links)}</div>${r.impact ? `<p class="p">${proseHtml(r.impact, m.tickets)}</p>` : ''}${items}${r.guard ? `<div class="guard"><b>Guarded by</b><span>${proseHtml(r.guard, m.tickets)}</span></div>` : ''}</div>`
    })
    .join('')
  const levels = ['high', 'medium']
    .map((level) => [level, m.risks.filter((r) => r.level === level).length])
    .filter(([, n]) => n)
    .map(([level, n]) => `${n} ${level}`)
  return {
    id: 'risks',
    nav: 'Risks',
    title: 'Risks',
    count: m.risks.length,
    ...(levels.length ? { sub: levels.join(', '), hot: true } : {}),
    body: `<div class="items">${body}</div>`,
  }
}

/** The code around a location, its line pinned. @param {import('./verify-proof-snippet.mjs').Context} code @param {Readonly<Record<string, string>>} tickets */
const codeAt = (code, tickets) =>
  diffFigure(
    {
      ...code,
      ...(code.focus.side === 'new' ? { pinLine: code.focus.line } : { pinOld: code.focus.line }),
    },
    tickets,
  )

/** A decision a person ticks, with the suggested answer and its code; a ticked one folds to one line. @param {Tick} item @param {Readonly<Record<string, string>>} tickets */
const decision = (item, tickets) =>
  `<div class="item tick${item.done ? ' done' : ''}" id="${item.id}" data-tick="${item.id}"><div class="hd"><input type="checkbox" class="check" aria-labelledby="${item.id}-q"${item.done ? ' checked' : ''}><div class="q" id="${item.id}-q">${proseHtml(item.text, tickets)}</div><span class="decided">decided</span>${linkChips(item.links)}</div>${item.why ? `<p class="p">${proseHtml(item.why, tickets)}</p>` : ''}${item.suggestion ? `<div class="guard"><b>Suggested</b><span>${proseHtml(item.suggestion, tickets)}</span></div>` : ''}${item.code ? codeAt(item.code, tickets) : ''}</div>`

/** @param {PageModel} m @returns {Section | null} */
function decisions(m) {
  if (!m.decisions.length) return null
  const open = m.decisions.filter((d) => !d.done).length
  return {
    id: 'decide',
    nav: 'Decisions',
    title: 'Decisions for you',
    count: m.decisions.length,
    hot: open > 0,
    body: `<div class="items">${m.decisions.map((d) => decision(d, m.tickets)).join('')}</div>`,
  }
}

/** One captured or record media item. @param {import('./verify-proof-page-model.mjs').AreaMedia} item */
const recordMedia = (item) =>
  item.inline
    ? `<figure class="media">${item.kind === 'video' ? `<video controls preload="none" src="${e(item.url)}"${item.poster ? ` poster="${e(item.poster)}"` : ''}></video>` : `<a class="shot" href="${e(item.url)}" data-cap="${e(item.caption)}"><img src="${e(item.url)}" alt="${e(item.caption)}"></a>`}<figcaption>${e(item.caption)}</figcaption></figure>`
    : `<p><a href="${e(item.url)}">${e(item.caption)}</a></p>`

/** @param {PageModel} m @param {AreaItem} a @param {(path: string) => string | undefined} anchor @returns {Section} */
function area(m, a, anchor) {
  const filesLink = m.filesUrl ? ` in <a href="${e(m.filesUrl)}">Files</a>` : ''
  const asks = a.asks.length
    ? `<ul class="asks">${a.asks.map((w) => `<li><b>${proseHtml(w.title, m.tickets)}</b>${w.detail ? `<span>${proseHtml(w.detail, m.tickets)}</span>` : ''}</li>`).join('')}</ul>`
    : ''
  const facts = a.facts.length
    ? `<ul class="facts">${a.facts.map((f) => `<li${f.flag ? ' class="flag"' : ''}>${proseHtml(f.text, m.tickets)}</li>`).join('')}</ul>`
    : ''
  const snippets = a.snippets
    .map((s) => diffFigure({ ...s, id: anchor(s.path) }, m.tickets))
    .join('')
  const excerpts = a.excerpts
    .map((x) => {
      const more = x.remaining
        ? `<div class="more">${plural(x.remaining, 'more line')}${filesLink}</div>`
        : ''
      // An excerpt whose every file is quoted above adds nothing.
      if (x.text === null)
        return x.files
          .map((f, i) =>
            diffFigure({ ...f, ...(i === x.files.length - 1 ? { more } : {}) }, m.tickets),
          )
          .join('')
      return `<h3 class="sub">${e(x.label)}</h3><pre class="plain">${e(x.text)}</pre>${more}`
    })
    .join('')
  return {
    id: `a-${a.id}`,
    nav: a.name,
    title: a.name,
    count: 0,
    pill: a.pill,
    hot: a.pill.tone !== 'muted',
    body: `${a.summary ? `<p class="lead">${proseHtml(a.summary, m.tickets)}</p>` : ''}${asks}${facts}${blocksHtml(a.blocks, m.tickets)}${snippets}${excerpts}${a.media.map(recordMedia).join('')}`,
  }
}

/** A thumbnail that opens the lightbox, or the image itself without the script. @param {string} file @param {string} caption @param {string} [label] */
const shot = (file, caption, label = caption) =>
  `<a class="shot" href="${e(mediaName(file))}" data-cap="${e(caption)}"><img src="${e(mediaName(file))}" alt="${e(caption)}" loading="lazy" decoding="async"><span>${e(label)}</span></a>`

/**
 * One rendered document: its title, page count and PDF links, then its pages; a paired document
 * shows base and head pages side by side by page number, with a gap where a side lacks one.
 * @param {DocumentPair} pair
 */
function documentHtml({ head, base }) {
  const counted =
    base && base.pageCount !== head.pageCount
      ? `${plural(head.pageCount, 'page')}, ${base.pageCount} before`
      : plural(head.pageCount, 'page')
  const links = `<a href="${e(mediaName(head.file))}">Download PDF</a>${base ? `<a href="${e(mediaName(base.file))}">Download base PDF</a>` : ''}<span class="framed-note dim">To download the PDF, open this page in its own tab.</span>`
  /** @param {ProofDocument} doc @param {string} label @param {number} n */
  const page = (doc, label, n) => {
    const p = doc.pages.find((x) => x.page === n)
    return p
      ? shot(p.file, p.caption, `${label} · ${p.caption}`)
      : `<span class="nopage">${label} · no page ${n}</span>`
  }
  const numbers = [...new Set([...(base?.pages ?? []), ...head.pages].map((p) => p.page))].sort(
    (x, y) => x - y,
  )
  const pages = base
    ? numbers
        .map((n) => `<div class="pair">${page(base, 'Before', n)}${page(head, 'After', n)}</div>`)
        .join('')
    : `<div class="thumbs">${head.pages.map((p) => shot(p.file, p.caption)).join('')}</div>`
  return `<article class="doc"><header><b>${e(head.title)}</b><span class="dim">${counted}</span>${links}</header>${pages}</article>`
}

/** The captured evidence: clips with chapters, before and after pairs, stills, documents and API calls. @param {PageModel} m */
function evidence(m) {
  const ev = m.evidence
  /** @type {string[]} */
  const parts = []
  if (ev.heads.length) {
    const first = ev.heads[0].clip
    const poster = (/** @type {import('./verify-proof-media.mjs').Clip} */ clip) =>
      clip.poster
        ? mediaName(clip.poster)
        : clip.stills.length
          ? mediaName(clip.stills[0].file)
          : ''
    const chapters = ev.heads
      .map(({ clip, index }) => {
        const data = `data-clip="${index}" data-src="${e(mediaName(clip.file))}" data-poster="${e(poster(clip))}" data-title="${e(clip.title)}"`
        const starts = clip.chapters.length ? clip.chapters : [{ atMs: 0, caption: 'Start' }]
        return `<li class="grp">${e(clip.title)}</li>${starts.map((ch) => `<li><button type="button" data-chapter="" ${data} data-t="${(ch.atMs / 1000).toFixed(2)}"><span class="ts">${clock(ch.atMs)}</span><span class="ct">${e(ch.caption)}</span></button></li>`).join('')}`
      })
      .join('')
    const total = ev.heads.reduce((n, { clip }) => n + clip.durationMs, 0)
    parts.push(
      `<div class="player"><div class="screen"><video id="proof-video" data-clip="0" controls playsinline preload="metadata" src="${e(mediaName(first.file))}"${poster(first) ? ` poster="${e(poster(first))}"` : ''}></video></div><div class="chapters"><h3>Chapters <span>${clock(total)}</span></h3><ol>${chapters}</ol></div></div><p class="nowplaying">Playing <b id="proof-clip">${e(first.title)}</b></p>`,
    )
  }
  if (ev.pairs.length)
    parts.push(
      `<h3 class="sub">Before and after</h3>${ev.pairs.map((p) => `<div class="pair">${shot(p.base.file, p.base.caption, `Before · ${p.base.caption}`)}${shot(p.head.file, p.head.caption, `After · ${p.head.caption}`)}</div>`).join('')}`,
    )
  if (ev.stills.length)
    parts.push(
      `<div class="thumbs">${ev.stills.map((s) => shot(s.file, s.caption)).join('')}</div>`,
    )
  if (ev.documents.length)
    parts.push(
      `<h3 class="sub" id="documents">Documents</h3>${ev.documents.map(documentHtml).join('')}`,
    )
  for (const t of ev.transcripts) {
    const ok = t.status < 400
    parts.push(
      `<article class="tx"><header><span class="pill ${ok ? 'success' : 'warning'}">${t.status}</span><b>${proseHtml(t.title)}</b><span class="dim">${t.ms} ms</span></header>${t.note ? `<p class="dim">${proseHtml(t.note)}</p>` : ''}<div class="txcols"><div><h4>Request</h4><pre>${e(JSON.stringify(t.request, null, 2))}</pre></div><div><h4>Response</h4><pre>${e(JSON.stringify(t.response, null, 2))}</pre></div></div>${t.rows ? `<h4>Rows written</h4><pre class="rows">${e(t.rows)}</pre>` : ''}</article>`,
    )
  }
  return parts.join('')
}

/** @type {Readonly<Record<string, Pill>>} */
const FINDING_STATE = Object.freeze({
  blocks: { word: 'blocks merge', tone: 'danger' },
  open: { word: 'open', tone: 'warning' },
  note: { word: 'note', tone: 'muted' },
  fixed: { word: 'fixed', tone: 'success' },
})
/** @param {string} severity */
const severityTone = (severity) =>
  /^P[01]$/.test(severity) ? 'danger' : severity === 'P2' ? 'warning' : 'muted'

/** @param {PageModel} m @returns {Section | null} */
function findings(m) {
  if (!m.findings.length) return null
  const body = m.findings
    .map((f) => {
      const pills = `<span class="pills">${f.severity ? pillHtml({ word: f.severity, tone: severityTone(f.severity) }) : ''}${pillHtml(FINDING_STATE[f.state])}</span>`
      const code = f.code ? codeAt(f.code, m.tickets) : ''
      return `<div class="item" id="${f.id}"><div class="hd">${pills}<div class="q">${proseHtml(f.text, m.tickets)}</div>${f.at && !f.code ? location(f.at) : ''}${linkChips(f.links)}</div>${f.detail ? `<p class="p">${proseHtml(f.detail, m.tickets)}</p>` : ''}${code}</div>`
    })
    .join('')
  const blocking = m.findings.filter((f) => f.state === 'blocks').length
  const open = m.findings.filter((f) => f.state === 'open').length
  const sub = [
    blocking ? `${blocking} block${blocking === 1 ? 's' : ''} merge` : '',
    open ? `${open} open` : '',
  ].filter(Boolean)
  return {
    id: 'findings',
    nav: 'Findings',
    title: 'Review findings',
    count: m.findings.length,
    ...(sub.length ? { sub: sub.join(', '), hot: true } : {}),
    body: `<div class="items">${body}</div>`,
  }
}

/** @type {Readonly<Record<string, string>>} */
const CRITERION_TONE = Object.freeze({
  done: 'success',
  unverified: 'warning',
  missed: 'danger',
  skipped: 'muted',
})
/** @type {Readonly<Record<string, string>>} */
const VERDICT_TONE = Object.freeze({
  complete: 'success',
  partial: 'warning',
  incomplete: 'danger',
})

/** Does the PR satisfy its ticket: each acceptance criterion with its status and evidence. @param {PageModel} m @returns {Section | null} */
function ticket(m) {
  const t = m.ticket
  if (!t) return null
  const chip = (/** @type {import('./verify-proof-page-model.mjs').Evidence} */ ev) =>
    ev.href
      ? `<a class="evc" href="${e(ev.href)}" title="${e(ev.title ?? '')}">${e(ev.kind)}: ${e(ev.label)}</a>`
      : `<span class="evc" title="${e(ev.label)}">${e(ev.kind)}: <code>${e(ev.label)}</code></span>`
  const rows = t.criteria
    .map(
      (c) =>
        `<tr id="${e(c.id)}"><td class="id">${e(c.label)}</td><td>${proseHtml(c.text, m.tickets)}${linkChips(c.links)}${c.note ? `<div class="dim">${proseHtml(c.note, m.tickets)}</div>` : ''}${c.evidence.length ? `<div class="ev">${c.evidence.map(chip).join('')}</div>` : ''}</td><td class="st">${pillHtml({ word: c.status, tone: CRITERION_TONE[c.status] })}</td></tr>`,
    )
    .join('')
  const open = t.criteria.filter((c) => c.status !== 'done').length
  const list = (/** @type {string} */ title, /** @type {string[]} */ items) =>
    items.length
      ? `<h3 class="sub">${title}</h3><ul class="facts">${items.map((i) => `<li>${i}</li>`).join('')}</ul>`
      : ''
  const skipped = t.skipped.map(
    (k) =>
      `${proseHtml(k.text, m.tickets)}<span class="dim"> · ${proseHtml(k.why, m.tickets)}${k.followUp ? ` · follow-up ${proseHtml(k.followUp, m.tickets)}` : ''}</span>`,
  )
  const contradictions = t.contradictions.map((text) => proseHtml(text, m.tickets))
  return {
    id: 'ticket',
    nav: 'Ticket',
    title: `Does it satisfy ${t.id ?? 'the ticket'}?`,
    count: t.criteria.length,
    ...(t.verdict ? { pill: { word: t.verdict, tone: VERDICT_TONE[t.verdict] } } : {}),
    ...(open ? { sub: `${open} not done`, hot: true } : {}),
    body: `${t.summary ? `<p class="lead">${proseHtml(t.summary, m.tickets)}</p>` : ''}<div class="box"><table class="tbl ac">${rows}</table></div>${list('Skipped', skipped)}${list('Contradicts the PR', contradictions)}`,
  }
}

/** @param {PageModel} m @returns {Section | null} */
function afterMerge(m) {
  if (!m.afterMerge.length) return null
  return {
    id: 'after',
    nav: 'After merge',
    title: 'After merge',
    count: m.afterMerge.length,
    body: `<div class="todos">${m.afterMerge.map((a) => `<label class="todo tick${a.done ? ' done' : ''}" id="${a.id}" data-tick="${a.id}"><input type="checkbox" class="check"${a.done ? ' checked' : ''}><span>${proseHtml(a.text, m.tickets)}</span></label>`).join('')}</div>`,
  }
}

/**
 * One part of a repository section. A checklist is ruled rows on a shared grid: the group label
 * column, the box, the number, the text with its context above and what to expect below, the link
 * chips and the tag. A table aligns its pill columns right; facts are labelled rows.
 * @param {SectionPart} part @param {Readonly<Record<string, string>>} tickets
 */
function sectionPart(part, tickets) {
  const prose = (/** @type {string} */ text) => proseHtml(text, tickets)
  if (part.kind === 'checklist') {
    const rows = part.items
      .map((t, i) => {
        const rich = Boolean(t.context || t.expect)
        const cls = `si tick${rich ? ' rich' : ''}${t.group && i ? ' first' : ''}`
        return `<div class="${cls}" id="${t.id}" data-tick="${t.id}">${part.grouped ? `<span class="g">${t.group ? e(t.group) : ''}</span>` : ''}<input type="checkbox" class="check" aria-labelledby="${t.id}-t">${part.numbered ? `<span class="no">${i + 1}</span>` : ''}<div class="hd">${t.context ? `<span class="cx">${prose(t.context)}</span>` : ''}<span class="it" id="${t.id}-t">${prose(t.text)}</span>${t.expect ? `<span class="ex"><b>Expect</b><span>${prose(t.expect)}</span></span>` : ''}</div>${linkChips(t.links)}${t.tag ? `<span class="st">${pillHtml(t.tag)}</span>` : ''}</div>`
      })
      .join('')
    return `<div class="cl${part.grouped ? ' grp' : ''}">${rows}</div>`
  }
  const heading = part.title
    ? `<h3 class="sub">${e(part.title)}${part.kind === 'table' && part.note ? `<span class="h3n">${prose(part.note)}</span>` : ''}</h3>`
    : ''
  if (part.kind === 'facts')
    return `${heading}<ul class="kv">${part.rows.map((r) => `<li><span class="k${r.tone ? ` ${r.tone}` : ''}">${e(r.label)}</span><span>${prose(r.text)}</span></li>`).join('')}</ul>`
  const right = (/** @type {number} */ i) => (part.pills[i] ? ' class="pc"' : '')
  const cell = (
    /** @type {{text: string, note?: string, pill?: string, code?: boolean}} */ c,
    /** @type {number} */ i,
  ) => {
    const text = c.pill
      ? pillHtml({ word: c.text, tone: c.pill })
      : c.code
        ? c.text
            .split('\n')
            .map((line) => `<span class="mono">${e(line)}</span>`)
            .join('')
        : prose(c.text)
    return `<td${right(i)}>${text}${c.note ? `<div class="sn">${prose(c.note)}</div>` : ''}</td>`
  }
  const head = `<thead><tr>${part.columns.map((c, i) => `<th${right(i)}>${e(c)}</th>`).join('')}</tr></thead>`
  return `${heading}<div class="box"><div class="tblwrap"><table class="tbl stbl">${head}<tbody>${part.rows.map((row) => `<tr>${row.map(cell).join('')}</tr>`).join('')}</tbody></table></div></div>${!part.title && part.note ? `<p class="sn">${prose(part.note)}</p>` : ''}`
}

/** A section the repository declared, filled from the spec. @param {PageModel} m @param {RepoSection} s @returns {Section} */
const repoSection = (m, s) => ({
  id: `sec-${s.id}`,
  nav: s.title,
  title: s.title,
  count: s.count,
  ...(s.sub ? { sub: s.sub } : {}),
  hot: s.open > 0,
  body: s.parts.map((part) => `<div class="sp">${sectionPart(part, m.tickets)}</div>`).join(''),
})

/**
 * The page's sections in order, dropping the empty ones: each repository section right after the
 * core section it names (after any declared before it), after the last area when it names an area
 * this page does not show, and otherwise just before Files.
 * @param {[string, Section | null][]} slots the core sections by anchor id, in page order
 * @param {{after: string | null, section: Section}[]} added
 * @returns {Section[]}
 */
function placeSections(slots, added) {
  const ids = slots.map(([id]) => id)
  /** @type {(Section | null)[][]} */
  const placed = slots.map(([, section]) => [section])
  const lastArea = ids.findLastIndex((id) => id.startsWith('a-'))
  for (const { after, section } of added) {
    let at = after === null ? -1 : ids.indexOf(after)
    if (at < 0 && after?.startsWith('a-')) at = lastArea >= 0 ? lastArea : ids.indexOf('ticket')
    if (at < 0) at = ids.indexOf('files') - 1
    placed[at].push(section)
  }
  return /** @type {Section[]} */ (placed.flat().filter(Boolean))
}

/**
 * The Files grid: one row per group and per file, counts right-aligned, every bar on the scale of
 * the largest group. Bar widths are classes, since the CSP allows no style attributes.
 * @param {PageModel} m @param {Set<number>} widths collects each width the page uses
 * @returns {Section | null}
 */
function files(m, widths) {
  if (!m.files?.length) return null
  const max = Math.max(1, ...m.files.map((g) => g.adds + g.dels))
  /** @param {number} n */
  const width = (n) => {
    const w = n ? Math.max(1, Math.round((n / max) * 100)) : 0
    widths.add(w)
    return w
  }
  /** @param {number | string} count @param {number} adds @param {number} dels @param {boolean} [binary] */
  const cells = (count, adds, dels, binary = false) =>
    `<span class="c2">${count}</span><span class="c3${adds ? '' : ' z'}">${binary ? 'bin' : `+${num(adds)}`}</span><span class="c4${dels ? '' : ' z'}">−${num(dels)}</span><span class="bar" title="${num(adds + dels)} lines"><i class="a bw-${width(adds)}"></i><i class="d bw-${width(dels)}"></i></span>`
  const body = m.files
    .map(
      (g) =>
        `<details class="fg"><summary class="frow"><span class="nm">${e(g.name)}</span>${cells(g.files.length, g.adds, g.dels)}</summary>${g.files.map((f) => `<div class="frow file">${f.href ? `<a class="nm" href="${e(f.href)}" target="_blank" rel="noopener noreferrer" title="${e(f.path)}">` : `<span class="nm" title="${e(f.path)}">`}<bdi>${e(f.path)}</bdi>${f.href ? '</a>' : '</span>'}${cells('', f.adds, f.dels, f.binary)}</div>`).join('')}</details>`,
    )
    .join('')
  const count = m.files.reduce((n, g) => n + g.files.length, 0)
  return {
    id: 'files',
    nav: 'Files',
    title: 'Files',
    count,
    body: `<div class="files"><div class="frow head"><span>Area</span><span>Files</span><span>Added</span><span>Removed</span><span>Size</span></div>${body}</div>`,
  }
}

/** What went into the page and what it left out; shown only with `?debug`. @param {PageModel} m */
function debug(m) {
  const d = m.debug
  const list = (/** @type {string} */ title, /** @type {string[]} */ items) =>
    items.length ? `<h3>${title}</h3><ul>${items.map((i) => `<li>${e(i)}</li>`).join('')}</ul>` : ''
  return `<section class="sec debug" id="debug" aria-label="Generation diagnostics"><h2>Generation</h2><p class="dim">Inputs: ${e(d.inputs.join(', '))}.</p>${list('Dropped', d.problems)}${list('The agent was unsure about', d.uncertain)}${list('Reviewers', d.reviewers)}${d.health ? `<p class="dim">Factory health: ${e(d.health)}.</p>` : ''}${d.verdict ? `<p class="dim">Gate: ${e(d.verdict)}.</p>` : ''}${d.source ? `<p class="dim">Media captured by ${e(d.source.kind)}: <a href="${e(d.source.url)}">${e(d.source.url)}</a></p>` : ''}</section>`
}

const ICON = (/** @type {string} */ d) =>
  `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="${d}"/></svg>`

/** What the page shows and when it was built; the player turns the raw ISO time into the reader's local time. @param {PageModel} m */
const footer = (m) =>
  `<footer class="foot">Head <code>${m.head.slice(0, 12)}</code> on <code>${m.base.slice(0, 12)}</code>, proof <code>${e(m.digest.slice(0, 19))}</code> · Generated <time datetime="${e(m.generatedAt)}">${e(m.generatedAt)}</time></footer>`

/** The lightbox the player opens a screenshot in; with more than one, arrows step through them. @param {number} count */
const lightbox = (count) =>
  `<div class="lb" id="lb" hidden role="dialog" aria-modal="true" aria-label="Screenshot"><div class="lb-bar"><div class="lb-cap" id="lb-cap"></div>${count > 1 ? `<button class="iconbtn" id="lb-prev" type="button" title="Previous (Left arrow)" aria-label="Previous screenshot">${ICON('M10 3.5 5.5 8l4.5 4.5')}</button><button class="iconbtn" id="lb-next" type="button" title="Next (Right arrow)" aria-label="Next screenshot">${ICON('M6 3.5 10.5 8 6 12.5')}</button>` : ''}<button class="iconbtn" id="lb-zoom" type="button" aria-pressed="false" title="Full size"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M2.5 6V2.5H6M10 2.5h3.5V6M13.5 10v3.5H10M6 13.5H2.5V10"/></svg></button><button class="iconbtn" id="lb-close" type="button" title="Close (Esc)" aria-label="Close"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></div><div class="lb-stage" id="lb-stage"><img id="lb-img" alt=""></div></div>`

/** The bar widths the page uses, which a CSP-hashed sheet cannot take as attributes. @param {Set<number>} widths */
const pageRules = (widths) =>
  [...widths]
    .sort((a, b) => a - b)
    .map((w) => `.bw-${w}{width:${w}%}`)
    .join('')

/** @param {PageModel} m */
export function renderPageHtml(m) {
  const seen = new Set()
  /** The first figure for a path carries its anchor. @param {string} path */
  const anchor = (path) => {
    if (seen.has(path)) return undefined
    seen.add(path)
    return fileAnchor(path)
  }
  const shown = evidence(m)
  const areas = m.areas.map((a) => area(m, a, anchor))
  const ui = areas.find((s) => s.id === 'a-ui')
  if (shown && ui) ui.body += shown
  /** @type {Set<number>} */
  const widths = new Set()
  const sections = placeSections(
    [
      ['risks', risks(m)],
      ['decide', decisions(m)],
      ['ticket', ticket(m)],
      ...areas.map((a) => /** @type {[string, Section]} */ ([a.id, a])),
      [
        'evidence',
        shown && !ui
          ? { id: 'evidence', nav: 'Evidence', title: 'Evidence', count: 0, body: shown }
          : null,
      ],
      ['findings', findings(m)],
      ['after', afterMerge(m)],
      ['files', files(m, widths)],
    ],
    m.repoSections.map((s) => ({ after: s.after, section: repoSection(m, s) })),
  )
  const style = `${PROOF_STYLE}\n${pageRules(widths)}`
  const hash = createHash('sha256').update(style).digest('base64')
  const csp = `default-src 'none'; style-src 'sha256-${hash}'; script-src 'self'; img-src 'self'; media-src 'self'; base-uri 'none'; form-action 'none'`
  const nav = sections
    .map(
      (s) =>
        `<a href="#${s.id}"${s.hot ? ' class="hot"' : ''}>${e(s.nav)}${s.count ? `<span class="n">${s.count}</span>` : ''}</a>`,
    )
    .join('')
  const body = sections
    .map(
      (s) =>
        `<section class="sec" id="${s.id}"><h2>${e(s.title)}${s.count || s.sub ? `<span class="n">${[s.count ? String(s.count) : '', s.sub ? e(s.sub) : ''].filter(Boolean).join(' · ')}</span>` : ''}${s.pill ? pillHtml(s.pill) : ''}</h2>${s.body}</section>`,
    )
    .join('')
  const shots = body.split('class="shot"').length - 1
  const box = shots ? lightbox(shots) : ''
  return `<!doctype html><html lang="en"><head><meta http-equiv="Content-Security-Policy" content="${csp}"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="robots" content="noindex, nofollow, noarchive, nosnippet, noimageindex"><title>Proof · ${e(m.repo)} #${m.pr}</title><style>${style}</style></head><body data-proof="${e(`${m.repo}#${m.pr}@${m.head}`)}"><main class="wrap">${header(m)}${banner(m)}${nav ? `<nav class="secnav" aria-label="Sections">${nav}</nav>` : ''}${body}${debug(m)}${footer(m)}</main>${box}<script src="${PROOF_PLAYER_FILE}" defer></script></body></html>`
}
