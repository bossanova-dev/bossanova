/**
 * The proof page HTML from a page model: Why it's waiting, Risks, Changes, Evidence, Checklist,
 * Review notes, Files, Commits and Generation check. Every value is escaped here. The only script
 * is the static player beside the page, linked only when there is a recording for it to play, so a
 * page without media is one self-contained file.
 */
import { createHash } from 'node:crypto'
import { colourLine, escapeHtml, inlineHtml, languageOf } from './verify-proof-code.mjs'
import { mediaName } from './verify-proof-media.mjs'
import { GENERATED_PATH, fileAnchor } from './verify-proof-page-model.mjs'
import { PROOF_PLAYER_FILE } from './verify-proof-player.mjs'
import { PROOF_STYLE } from './verify-proof-style.mjs'

/**
 * @typedef {import('./verify-proof-page-model.mjs').PageModel} PageModel
 * @typedef {import('./verify-proof-snippet.mjs').DiffRow} DiffRow
 */

const e = escapeHtml
/** `m:ss`. @param {number} ms */
const clock = (ms) => {
  const total = Math.floor(ms / 1000)
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`
}
/** @param {number} n @param {string} one */
const plural = (n, one) => `${n} ${one}${n === 1 ? '' : 's'}`
const SIGN = Object.freeze({ add: '+', del: '−', ctx: ' ' })

/** One GitHub-style diff figure. @param {{path: string, rows: DiffRow[], adds: number, dels: number, start?: number, pinLine?: number | null, pinNote?: string, more?: string, id?: string}} diff */
function diffFigure(diff) {
  const lang = languageOf(diff.path)
  const firstOld = diff.rows.find((row) => row.old !== null)?.old ?? 0
  const firstNew = diff.start ?? diff.rows.find((row) => row.new !== null)?.new ?? 0
  const rows = diff.rows
    .map((row) => {
      const pinned = diff.pinLine != null && row.new === diff.pinLine ? ' pinned' : ''
      return `<tr class="${row.kind}${pinned}"><td class="n">${row.old ?? ''}</td><td class="n">${row.new ?? ''}</td><td class="t"><span class="sign">${SIGN[row.kind]}</span>${colourLine(row.text, lang)}</td></tr>`
    })
    .join('')
  const hunk = `<tr class="hunk"><td class="n"></td><td class="n"></td><td class="t">@@ -${firstOld} +${firstNew} @@</td></tr>`
  const note = diff.pinNote
    ? `<div class="pinnote"><span class="pinicon">!</span>${inlineHtml(diff.pinNote)}</div>`
    : ''
  return `<figure class="diff"${diff.id ? ` id="${diff.id}"` : ''}><figcaption><span class="path">${e(diff.path || 'excerpt')}</span><span class="stat"><b class="plus">+${diff.adds}</b> <b class="minus">−${diff.dels}</b></span></figcaption><div class="scroll"><table>${hunk}${rows}</table></div>${note}${diff.more ?? ''}</figure>`
}

/** @param {PageModel} m */
function header(m) {
  const status = m.status
  const split = m.split
    ? `<div class="split" aria-label="Lines added by kind"><div class="bar"><span class="k-logic"></span><span class="k-test"></span><span class="k-gen"></span></div><div class="key"><span>${m.split.logic.toLocaleString('en')} logic</span><span>${m.split.tests.toLocaleString('en')} tests</span><span>${m.split.generated.toLocaleString('en')} generated</span></div></div>`
    : ''
  const does = m.does.bullets.length
    ? `<ul class="does">${m.does.bullets.map((b) => `<li>${e(b)}</li>`).join('')}</ul>${m.does.source ? `<p class="dim does-source">${e(m.does.source)}</p>` : ''}`
    : '<p class="dim">What it does: intent unknown.</p>'
  return `<header class="pr">${m.health ? `<p class="health"><strong>Factory health:</strong> ${e(m.health)}</p>` : ''}<div class="crumbs"><span class="repo">${e(m.repo)}</span> #${m.pr}</div><h1>${inlineHtml(m.title)}</h1><div class="meta">${m.verdict ? `<span>${e(m.verdict)}</span>` : ''}<span><code>${m.head.slice(0, 7)}</code></span>${m.totals ? `<span><b class="ins">+${m.totals.adds.toLocaleString('en')}</b> <b class="rem">−${m.totals.dels.toLocaleString('en')}</b> in ${plural(m.totals.files, 'file')}</span>` : ''}${m.reviewUrl ? `<span><a href="${e(m.reviewUrl)}">Review this head</a></span>` : ''}</div>${does}<dl class="status"><div class="stat"><dt>Confidence</dt><dd>${e(status.confidence)}${status.rate ? ` <small>${e(status.rate)}</small>` : ''}</dd></div><div class="stat"><dt>Asks</dt><dd>${status.asks}</dd></div><div class="stat"><dt>Risks</dt><dd>${e(status.risks)}</dd></div><div class="stat"><dt>After merge</dt><dd>${status.afterMerge}</dd></div>${split}</dl>${m.title === m.headline ? '' : `<p class="dim headline">${e(m.headline)}</p>`}</header>`
}

/** @param {PageModel} m */
function waiting(m) {
  const items = m.waiting
    .map(
      (w) =>
        `<li${w.proof ? ' class="proof"' : ''}><div><div class="t">${inlineHtml(w.title)}</div>${w.detail ? `<div class="d">${inlineHtml(w.detail)}</div>` : ''}</div>${w.href ? `<a class="x" href="${e(w.href)}">${e(w.link ?? '')}</a>` : '<span class="x"></span>'}</li>`,
    )
    .join('')
  return `<section id="waiting"><h2>Why it's waiting</h2>${items ? `<ol class="reasons">${items}</ol>` : '<p class="dim">Nothing is holding it.</p>'}</section>`
}

/** @param {PageModel} m */
function risks(m) {
  const items = m.risks
    .map((r) => {
      const at = r.path ? `${r.path.slice(r.path.lastIndexOf('/') + 1)}:${r.line}` : ''
      const where = m.quoted.has(r.path ?? '')
        ? `<a class="where" href="#${fileAnchor(/** @type {string} */ (r.path))}"><span>${e(r.label ?? '')}</span>${e(at)}</a>`
        : `<span class="where dim"><span>${e(r.label ?? '')}</span>${e(at)}</span>`
      return `<li><span class="lvl ${e(r.level)}">${e(r.level[0].toUpperCase() + r.level.slice(1))}</span><div><h3>${inlineHtml(r.title)}</h3>${r.body ? `<p>${inlineHtml(r.body)}</p>` : ''}${r.items ? `<ul class="items">${r.items.map((i) => `<li>${e(i)}</li>`).join('')}</ul>` : ''}${r.ifItMatters ? `<p class="if">${inlineHtml(r.ifItMatters)}</p>` : ''}</div>${where}</li>`
    })
    .join('')
  return `<section id="risks"><h2>Risks <span class="n">${m.risks.length}</span></h2>${items ? `<ul class="risks">${items}</ul>` : '<p class="dim">Nothing found.</p>'}</section>`
}

/** @param {PageModel} m */
function changes(m) {
  const filesLink = m.filesUrl ? ` <a href="${e(m.filesUrl)}">Files</a>` : ''
  const seen = new Set()
  /** The first figure for a path carries its anchor. @param {string} path */
  const anchor = (path) => {
    if (seen.has(path)) return undefined
    seen.add(path)
    return fileAnchor(path)
  }
  const areas = m.areas
    .map((a) => {
      const facts = a.facts
        .map((f) => (f.flag ? `<span class="warn">${inlineHtml(f.text)}</span>` : e(f.text)))
        .join(' · ')
      const snippets = a.snippets.map((s) => diffFigure({ ...s, id: anchor(s.path) })).join('')
      const excerpts = a.excerpts
        .map((x) => {
          const more = x.remaining
            ? `<div class="more">${x.remaining} more lines${filesLink}</div>`
            : ''
          if (x.files.length)
            return x.files
              .map((f, i) => diffFigure({ ...f, ...(i === x.files.length - 1 ? { more } : {}) }))
              .join('')
          return `<h4 class="sub">${e(x.label)}</h4><pre class="plain">${e(x.text ?? '')}</pre>${more}`
        })
        .join('')
      const media = a.media
        .map((item) =>
          item.inline
            ? `<figure>${item.kind === 'video' ? `<video controls preload="none" src="${e(item.url)}"${item.poster ? ` poster="${e(item.poster)}"` : ''}></video>` : `<img src="${e(item.url)}" alt="${e(item.caption)}">`}<figcaption>${e(item.caption)}</figcaption></figure>`
            : `<p><a href="${e(item.url)}">${e(item.caption)}</a></p>`,
        )
        .join('')
      const body = snippets + excerpts + media
      return `<details class="area" id="a-${e(a.id)}"${body ? ' open' : ''}><summary><span class="name">${e(a.name)}</span><span class="sum">${inlineHtml(a.summary)}${facts ? `<span class="facts">${facts}</span>` : ''}</span><span class="count">${e(a.tag)} <i class="chev"></i></span></summary>${body ? `<div class="body">${body}</div>` : ''}</details>`
    })
    .join('')
  const n = `${plural(m.areas.length, 'area')}${m.splitSuggested ? ' · split suggested' : ''}`
  return `<section id="changes"><h2>Changes <span class="n">${n}</span></h2>${areas ? `<div class="areas">${areas}</div>` : '<p class="dim">No review areas.</p>'}</section>`
}

/** @param {PageModel} m */
function evidence(m) {
  const ev = m.evidence
  /** @type {string[]} */
  const parts = []
  /** @type {string[]} */
  const counts = []
  let boxes = ''
  if (ev.heads.length) {
    const first = ev.heads[0].clip
    const poster = (/** @type {import('./verify-proof-media.mjs').Clip} */ clip) =>
      clip.poster
        ? mediaName(clip.poster)
        : clip.stills.length
          ? mediaName(clip.stills[clip.stills.length - 1].file)
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
      `<div class="evidence"><div class="screen"><video id="proof-video" data-clip="0" controls playsinline preload="metadata" src="${e(mediaName(first.file))}"${poster(first) ? ` poster="${e(poster(first))}"` : ''}></video></div><div class="chapters"><h3>Chapters <span>${clock(total)}</span></h3><ol>${chapters}</ol></div></div><p class="nowplaying">Playing <b id="proof-clip">${e(first.title)}</b></p>`,
    )
    counts.push(plural(ev.heads.length, 'recording'), plural(ev.stills.length, 'still'))
  }
  if (ev.pairs.length) {
    parts.push(
      `<h3 class="sub">Before and after</h3>${ev.pairs.map((p) => `<div class="pair"><figure><img src="${e(mediaName(p.base.file))}" alt="${e(p.base.caption)}"><figcaption>Before · ${e(p.base.caption)}</figcaption></figure><figure><img src="${e(mediaName(p.head.file))}" alt="${e(p.head.caption)}"><figcaption>After · ${e(p.head.caption)}</figcaption></figure></div>`).join('')}`,
    )
    counts.push('base compared')
  }
  if (ev.stills.length) {
    parts.push(
      `<div class="thumbs">${ev.stills.map((s, i) => `<a class="thumb" href="#shot-${i}"><img src="${e(mediaName(s.file))}" alt="${e(s.caption)}" loading="lazy"><span>${e(s.caption)}</span></a>`).join('')}</div>`,
    )
    boxes = ev.stills
      .map(
        (s, i) =>
          `<div class="lightbox" id="shot-${i}"><a class="lb-bg" href="#evidence" aria-label="Close"></a><figure><img src="${e(mediaName(s.file))}" alt="${e(s.caption)}"><figcaption><b>${e(s.caption)}</b><a class="lb-x" href="#evidence">Close</a></figcaption></figure>${i ? `<a class="lb-prev" href="#shot-${i - 1}" aria-label="Previous">‹</a>` : ''}${i < ev.stills.length - 1 ? `<a class="lb-next" href="#shot-${i + 1}" aria-label="Next">›</a>` : ''}</div>`,
      )
      .join('')
  }
  if (ev.transcripts.length) {
    parts.push(
      ev.transcripts
        .map((t) => {
          const ok = t.status < 400
          return `<article class="tx"><header><span class="${ok ? 'ok' : 'warn'}">${t.status}</span> <b>${inlineHtml(t.title)}</b><span class="dim">${t.ms} ms</span></header>${t.note ? `<p class="dim">${inlineHtml(t.note)}</p>` : ''}<div class="txcols"><div><h4>Request</h4><pre>${e(JSON.stringify(t.request, null, 2))}</pre></div><div><h4>Response</h4><pre>${e(JSON.stringify(t.response, null, 2))}</pre></div></div>${t.rows ? `<h4>Rows written</h4><pre class="rows">${e(t.rows)}</pre>` : ''}</article>`
        })
        .join(''),
    )
    counts.push(plural(ev.transcripts.length, 'API call'))
  }
  if (!parts.length && ev.record.length) {
    parts.push(
      ev.record
        .map((item) =>
          item.inline
            ? `<figure>${item.kind === 'video' ? `<video controls preload="none" src="${e(item.url)}"${item.poster ? ` poster="${e(item.poster)}"` : ''}></video>` : `<img src="${e(item.url)}" alt="${e(item.caption)}">`}<figcaption>${e(item.caption)}</figcaption></figure>`
            : `<p><a href="${e(item.url)}">${e(item.caption)}</a></p>`,
        )
        .join(''),
    )
    counts.push(plural(ev.record.length, 'item'))
  }
  if (!parts.length && ev.plan.length) {
    parts.push(
      `<ul class="risks plan">${ev.plan.map((p) => `<li><span class="lvl low">${e(p.kind)}</span><div><h3>${inlineHtml(p.claim)}</h3><p>${inlineHtml(p.how)}</p></div></li>`).join('')}</ul>`,
    )
    counts.push('planned, none captured')
  }
  if (!parts.length)
    parts.push('<p class="dim">No recording, still or API transcript for this head.</p>')
  const source = ev.source
    ? `<p class="evnote">Captured by ${e(ev.source.kind)}: <a href="${e(ev.source.url)}">${e(ev.source.url)}</a></p>`
    : ''
  return {
    html: `<section id="evidence"><h2>Evidence <span class="n">${e(counts.join(', ') || 'none captured')}</span></h2>${parts.join('')}${source}</section>`,
    boxes,
  }
}

/** @param {PageModel} m */
function checklist(m) {
  const list = (/** @type {{text: string, done: boolean}[]} */ items) =>
    items
      .map(
        (i) =>
          `<li><span class="box${i.done ? ' done' : ''}"></span><span>${inlineHtml(i.text)}</span></li>`,
      )
      .join('') || '<li><span>Nothing.</span></li>'
  return `<section id="checklist"><h2>Checklist</h2><div class="checks"><div><h3>Before you approve</h3><ul>${list(m.checklist.before)}</ul></div><div><h3>After merge</h3><ul>${list(m.checklist.after)}</ul></div></div></section>`
}

/** @param {PageModel} m */
function notes(m) {
  if (!m.notes.length) return ''
  const items = m.notes
    .map(
      (n) =>
        `<li><span class="sev">${e(n.severity)}</span><code class="loc" title="${e(n.at ?? '')}">${e((n.at ?? '').slice((n.at ?? '').lastIndexOf('/') + 1))}</code><span>${inlineHtml(n.text)}</span></li>`,
    )
    .join('')
  return `<section id="notes"><h2>Review notes <span class="n">${m.notes.length}</span></h2><ul class="notes">${items}</ul></section>`
}

/** @param {PageModel} m */
function filesAndCommits(m) {
  const files = m.files
    ? `<details class="more"><summary>Files <span class="dim">${m.files.length}</span></summary><table class="ftable">${m.files.map((f) => `<tr${GENERATED_PATH.test(f.path) ? ' class="gen"' : ''}><td class="fp">${e(f.path)}</td><td class="num ins">${f.adds === null ? 'bin' : f.adds ? `+${f.adds}` : ''}</td><td class="num rem">${f.dels ? `−${f.dels}` : ''}</td></tr>`).join('')}</table></details>`
    : ''
  const link = m.filesUrl
    ? `<p class="dim"><a href="${e(m.filesUrl)}">Files</a> on the forge, the full diff.</p>`
    : ''
  const commits = m.commits.length
    ? `<details class="more"><summary>Commits <span class="dim">${m.commits.length}</span></summary><ul class="timeline">${m.commits.map((c) => `<li><time datetime="${e(c.at)}">${e(c.at.slice(0, 16).replace('T', ' '))}</time><span>${e(c.text)}</span></li>`).join('')}</ul></details>`
    : ''
  return files || link || commits ? `<section id="files">${files}${link}${commits}</section>` : ''
}

/** @param {PageModel} m */
function generation(m) {
  const g = m.generation
  const problems = g.problems.length
    ? g.problems.map((p) => `<li>${e(p)}</li>`).join('')
    : `<li>${g.checked ? 'Every spec and media item passed validation, and every snippet is quoted from git.' : 'Built from the proof record alone: no agent spec or media manifest.'}</li>`
  const unsure = g.uncertain.length
    ? `<h3>The agent was unsure about</h3><ul>${g.uncertain.map((u) => `<li>${inlineHtml(u)}</li>`).join('')}</ul>`
    : ''
  return `<section class="qa${g.problems.length ? ' flagged' : ''}" id="generation"><h2>Generation check</h2><p class="dim">Inputs: ${e(g.inputs.join(', '))}.</p><ul>${problems}</ul>${unsure}</section>`
}

/** Per-page rules: the added-lines bar widths, which a CSP-hashed sheet cannot take as attributes.
 * @param {PageModel} m */
const pageRules = (m) =>
  m.split
    ? `\n.k-logic{flex:${m.split.logic}}.k-test{flex:${m.split.tests}}.k-gen{flex:${m.split.generated}}`
    : ''

/** @param {PageModel} m */
export function renderPageHtml(m) {
  const style = PROOF_STYLE + pageRules(m)
  const hash = createHash('sha256').update(style).digest('base64')
  const csp = `default-src 'none'; style-src 'sha256-${hash}'; script-src 'self'; img-src 'self'; media-src 'self'; base-uri 'none'; form-action 'none'`
  const ev = evidence(m)
  const sections = [
    waiting(m),
    risks(m),
    changes(m),
    ev.html,
    checklist(m),
    notes(m),
    filesAndCommits(m),
    generation(m),
  ].join('')
  const nav = [
    ['waiting', 'Waiting'],
    ['risks', 'Risks'],
    ['changes', 'Changes'],
    ['evidence', 'Evidence'],
    ['checklist', 'Checklist'],
    ...(m.notes.length ? [['notes', 'Notes']] : []),
    ...(sections.includes('<section id="files">') ? [['files', 'Files']] : []),
  ]
    .map(([id, text]) => `<a href="#${id}">${text}</a>`)
    .join('')
  return `<!doctype html><html lang="en"><head><meta http-equiv="Content-Security-Policy" content="${csp}"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="robots" content="noindex, nofollow, noarchive, nosnippet, noimageindex"><title>Proof · ${e(m.repo)} #${m.pr}</title><style>${style}</style></head><body><nav class="secnav" aria-label="Sections"><div class="wrap">${nav}<span class="sp"></span><span class="pin">${m.head.slice(0, 7)} on ${m.base.slice(0, 7)}</span></div></nav><main class="wrap">${header(m)}${sections}<footer><span>Head <code>${m.head.slice(0, 12)}</code> on <code>${m.base.slice(0, 12)}</code></span><span>Proof <code>${e(m.digest.slice(0, 19))}</code></span></footer></main>${ev.boxes}${m.evidence.heads.length ? `<script src="${PROOF_PLAYER_FILE}" defer></script>` : ''}</body></html>`
}
