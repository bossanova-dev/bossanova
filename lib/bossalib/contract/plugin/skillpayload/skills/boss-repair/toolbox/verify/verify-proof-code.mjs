/** HTML escaping and the light, line-at-a-time syntax colour the proof page's diffs use. */

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

/** Escaped prose with each `code span` as <code>. @param {string | undefined} text */
export const inlineHtml = (text) =>
  (text ?? '')
    .split(/(`[^`]+`)/)
    .map((part, i) => (i % 2 ? `<code>${escapeHtml(part.slice(1, -1))}</code>` : escapeHtml(part)))
    .join('')

/** @type {Readonly<Record<string, {keywords: Set<string>, hash: boolean}>>} */
const LANGUAGES = Object.freeze({
  go: {
    hash: false,
    keywords: new Set(
      'func return if else type struct var const switch case default nil package import range for defer go map chan interface break continue'.split(
        ' ',
      ),
    ),
  },
  ts: {
    hash: false,
    keywords: new Set(
      'const let var function return if else export import from type interface await async new null undefined true false class extends for of in while switch case default throw try catch finally typeof'.split(
        ' ',
      ),
    ),
  },
  graphql: {
    hash: true,
    keywords: new Set(
      'extend type input enum query mutation subscription scalar union interface implements fragment on'.split(
        ' ',
      ),
    ),
  },
  hcl: {
    hash: true,
    keywords: new Set(
      'resource data module variable output locals provider terraform true false null'.split(' '),
    ),
  },
  sh: {
    hash: true,
    keywords: new Set(
      'if then else elif fi for in do done while case esac function return local export'.split(' '),
    ),
  },
})
/** @type {Readonly<Record<string, string>>} */
const EXTENSIONS = Object.freeze({
  go: 'go',
  ts: 'ts',
  tsx: 'ts',
  js: 'ts',
  jsx: 'ts',
  mjs: 'ts',
  cjs: 'ts',
  graphql: 'graphql',
  graphqls: 'graphql',
  tf: 'hcl',
  hcl: 'hcl',
  sh: 'sh',
  bash: 'sh',
})

/** The colour language for a path, or '' for plain text. @param {string} path */
export function languageOf(path) {
  const name = path.slice(path.lastIndexOf('/') + 1)
  const dot = name.lastIndexOf('.')
  return dot < 0 ? '' : (EXTENSIONS[name.slice(dot + 1).toLowerCase()] ?? '')
}

const TOKEN = /\/\/.*$|"(?:[^"\\]|\\.)*"?|'(?:[^'\\]|\\.)*'?|`[^`]*`?|@\w+/
const HASH_TOKEN = /\/\/.*$|#.*$|"(?:[^"\\]|\\.)*"?|'(?:[^'\\]|\\.)*'?|`[^`]*`?|@\w+/

/** Keywords and Capitalised calls in a stretch with no string or comment.
 * @param {string} text @param {{keywords: Set<string>}} language */
function words(text, language) {
  return text
    .split(/([A-Za-z_][A-Za-z0-9_]*)/)
    .map((part, i, parts) => {
      if (i % 2 === 0) return escapeHtml(part)
      if (language.keywords.has(part)) return `<span class="k">${part}</span>`
      if (/^[A-Z]/.test(part) && (parts[i + 1] ?? '').startsWith('('))
        return `<span class="f">${part}</span>`
      return part
    })
    .join('')
}

/**
 * One source line as escaped HTML with keyword, string, comment and call spans. Colour is per
 * line, so a string or comment that spans lines colours only where it is visible.
 * @param {string} text @param {string} lang
 */
export function colourLine(text, lang) {
  const language = LANGUAGES[lang]
  if (!language) return escapeHtml(text)
  let out = ''
  let rest = text
  while (rest) {
    const match = (language.hash ? HASH_TOKEN : TOKEN).exec(rest)
    if (!match) break
    const token = match[0]
    out += words(rest.slice(0, match.index), language)
    const comment = token.startsWith('//') || token.startsWith('#')
    out += `<span class="${comment ? 'c' : token.startsWith('@') ? 'k' : 's'}">${escapeHtml(token)}</span>`
    rest = rest.slice(match.index + token.length)
  }
  return out + words(rest, language)
}
