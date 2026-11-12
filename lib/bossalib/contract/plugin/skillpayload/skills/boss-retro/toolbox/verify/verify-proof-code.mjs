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

// Identifiers in prose, which the page sets as code so they skim: ./commands, paths, file names,
// fn(), ENV_VARS, snake_case, camelCase and obj.member names, and permission scopes.
const IDENTIFIER = new RegExp(
  [
    String.raw`\.\/[\w./-]+`,
    String.raw`[\w.*-]+(?:\/[\w.*-]+){2,}\/?`,
    String.raw`[\w.-]+\/[\w.-]+\.[a-z]\w*`,
    String.raw`\b[\w-]+\.(?:sql|go|ts|tsx|js|mjs|cjs|py|ya?ml|json|md|graphqls?|tf|sh|css|html|proto)\b`,
    String.raw`\b[A-Za-z_]\w*\(\)`,
    String.raw`\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b`,
    // table.column before snake_case, so `stg_users.is_claims_staff` is one identifier.
    String.raw`\b[a-z_]{3,}\.[a-z_]{3,}\b`,
    String.raw`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`,
    String.raw`\b[A-Za-z]\w*\.[a-z]+[A-Z]\w*\b`,
    String.raw`\b[A-Z][a-z]+[A-Z]\w*(?:\.\w+)?\b`,
    String.raw`\b[a-z]+[A-Z][A-Za-z0-9]*(?:\.\w+)*\b`,
    String.raw`\b[a-z]+:[a-z_]+\b`,
    String.raw`(?<![\w/])\.[a-z][\w-]*\.[\w.-]*\w`,
  ].join('|'),
  'g',
)
// Brand and product names that are camel-cased words, not identifiers.
const PLAIN_WORDS = new Set([
  'GraphQL',
  'PostgreSQL',
  'JavaScript',
  'TypeScript',
  'GitHub',
  'GitLab',
  'LinkedIn',
  'YouTube',
  'macOS',
  'iOS',
  'iPadOS',
  'OpenAI',
  'PagerDuty',
])
const PROSE_TOKEN = new RegExp(String.raw`\b([A-Z][A-Z0-9]+-\d+)\b|${IDENTIFIER.source}`, 'g')

/**
 * Escaped prose for the page: each `code span` and each bare identifier as code, and each ticket
 * id `tickets` has a link for as that link. Escaping comes first, so the patterns only ever add
 * markup around escaped text.
 * @param {string | undefined} text @param {Readonly<Record<string, string>>} [tickets]
 */
export function proseHtml(text, tickets = {}) {
  return (text ?? '')
    .split(/(`[^`]+`)/)
    .map((part, i) =>
      i % 2
        ? `<code>${escapeHtml(part.slice(1, -1))}</code>`
        : escapeHtml(part).replace(PROSE_TOKEN, (match, ticket) => {
            if (ticket)
              return tickets[ticket]
                ? `<a class="tk" href="${escapeHtml(tickets[ticket])}">${ticket}</a>`
                : ticket
            return PLAIN_WORDS.has(match) ? match : `<code>${match}</code>`
          }),
    )
    .join('')
}

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
