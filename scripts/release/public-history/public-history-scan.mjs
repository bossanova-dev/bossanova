// ---------------------------------------------------------------------------------------------
// audit: scanners

export const SECRET_FAMILIES = [
  {
    family: 'github-token',
    re: /\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{60,})\b/g,
  },
  { family: 'slack-token', re: /\bxox[abeprs]-[A-Za-z0-9-]{10,}\b/g },
  { family: 'aws-access-key', re: /\b(?:AKIA|ASIA)[0-9A-Z]{16}\b/g },
  { family: 'google-api-key', re: /\bAIza[0-9A-Za-z_-]{35}\b/g },
  { family: 'stripe-key', re: /\b(?:sk|rk)_live_[0-9A-Za-z]{16,}\b/g },
  { family: 'anthropic-key', re: /\bsk-ant-[A-Za-z0-9_-]{20,}/g },
  { family: 'openai-key', re: /\bsk-(?:proj-)?[A-Za-z0-9]{32,}\b/g },
  { family: 'linear-key', re: /\blin_(?:api|oauth)_[A-Za-z0-9]{32,}\b/g },
  { family: 'npm-token', re: /\bnpm_[A-Za-z0-9]{36}\b/g },
  { family: 'sentry-token', re: /\bsntr[ysu]_[A-Za-z0-9_=+/-]{40,}/g },
  {
    family: 'private-key',
    re: /-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY( BLOCK)?-----/g,
  },
  { family: 'jwt', re: /\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}/g },
  {
    family: 'credentialed-url',
    re: /\b[a-z][a-z0-9+.-]{1,15}:\/\/[^\s/:@'"`<>]{1,64}:[^\s/:@'"`<>]{1,128}@[A-Za-z0-9.-]+/g,
  },
]

const EMAIL_RE = /[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b/g
const HOME_RE = /\/(Users|home)\/([A-Za-z0-9._-]+)/g
const HOSTNAME_RE =
  /\b[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9-]+)*\.(?:svc\.cluster\.local|cluster\.local)\b|\b[a-z0-9-]+(?:\.[a-z0-9-]+)+\.internal\b/g

export function redact(value) {
  return `${value.slice(0, 6)}… (${value.length} chars)`
}

export function isBinary(data) {
  return data.subarray(0, 8192).includes(0)
}

export function bump(map, key, init) {
  const entry = map.get(key) ?? init()
  map.set(key, entry)
  return entry
}

// Tallies every address in text under its lower-cased domain: domain -> Map(address -> count).
export function countEmails(emails, text) {
  for (const match of text.matchAll(EMAIL_RE)) {
    const address = match[0]
    const domain = address.split('@').pop().toLowerCase()
    const perDomain = bump(emails, domain, () => new Map())
    perDomain.set(address, (perDomain.get(address) ?? 0) + 1)
  }
}

// Report-only scans: they never fail the audit, they feed the report's review sections.
export function newReportScans() {
  return {
    secretHits: new Map(), // family|match|path -> hit
    allowlisted: new Map(),
    emails: new Map(), // domain -> Map(address -> count)
    homes: new Map(), // user -> { count, commit, path }
    hostnames: new Map(), // host -> { count, commit, path }
  }
}

export function scanReportOnly(scans, allowlist, text, origin) {
  for (const { family, re } of SECRET_FAMILIES) {
    for (const match of text.matchAll(re)) {
      const value = match[0]
      const allow = allowlist.find((a) => a.pathRe.test(origin.path) && a.re.test(value))
      const target = allow ? scans.allowlisted : scans.secretHits
      const hit = bump(target, `${family}\0${value}\0${origin.path}`, () => ({
        family,
        sample: redact(value),
        commit: origin.commit,
        path: origin.path,
        allowedBy: allow?.text,
        count: 0,
      }))
      hit.count += 1
    }
  }
  countEmails(scans.emails, text)
  for (const match of text.matchAll(HOME_RE)) {
    const user = `/${match[1]}/${match[2]}`
    const entry = bump(scans.homes, user, () => ({
      count: 0,
      commit: origin.commit,
      path: origin.path,
    }))
    entry.count += 1
  }
  for (const match of text.matchAll(HOSTNAME_RE)) {
    const entry = bump(scans.hostnames, match[0], () => ({
      count: 0,
      commit: origin.commit,
      path: origin.path,
    }))
    entry.count += 1
  }
}
