#!/usr/bin/env node

import crypto from 'node:crypto'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// Marker prefix for claim comments. The agent posts formatClaimComment(token)
// on the issue, waits a few seconds for racers' comments to land, re-reads all
// comments, and asks this script whether its token won.
export const CLAIM_MARKER = 'bs-implement-claim'

// Anchor the trailing boundary so a malformed/crafted comment body with extra
// hex (e.g. a 40-char string) can't have its first 32 chars captured as a token.
const CLAIM_RE = new RegExp(`${CLAIM_MARKER}:([0-9a-f]{32})(?![0-9a-f])`)
const CLAIM_COMMENT_SUFFIX = ' (bs-implement run claiming this ticket)'
const SESSION_ID_RE = /^[A-Za-z0-9._:-]+$/

export function generateRunToken() {
  return crypto.randomBytes(16).toString('hex')
}

export function formatClaimComment(token, sessionId = null) {
  let ownerSuffix = ''
  if (typeof sessionId === 'string' && sessionId.trim() !== '') {
    const normalized = sessionId.trim()
    if (!SESSION_ID_RE.test(normalized)) {
      throw new Error(`invalid claim session id: ${sessionId}`)
    }
    ownerSuffix = ` owner:${normalized}`
  }
  return `🔒 ${CLAIM_MARKER}:${token} (bs-implement run claiming this ticket)${ownerSuffix}`
}

// Extract { token, createdAt, sessionId } from claim comments; ignore everything else.
export function parseClaimComments(comments) {
  const claims = []
  for (const c of comments || []) {
    const body = typeof c?.body === 'string' ? c.body : ''
    const match = body.match(CLAIM_RE)
    if (!match) continue
    const lineTail = body.slice((match.index ?? 0) + match[0].length).split('\n', 1)[0]
    let sessionId = null
    const ownerEligible = body.startsWith(`🔒 ${CLAIM_MARKER}:${match[1]}`)
    if (ownerEligible && lineTail.startsWith(CLAIM_COMMENT_SUFFIX)) {
      const suffixTail = lineTail.slice(CLAIM_COMMENT_SUFFIX.length)
      if (suffixTail.trim() !== '') {
        const ownerMatch = suffixTail.match(/^ owner:([A-Za-z0-9._:-]+)\s*$/)
        if (!ownerMatch) {
          claims.push({ token: match[1], createdAt: String(c.createdAt), sessionId: null })
          continue
        }
        sessionId = ownerMatch[1]
      }
    }
    claims.push({ token: match[1], createdAt: String(c.createdAt), sessionId })
  }
  return claims
}

function parseInstant(value, label) {
  const ms = Date.parse(value)
  if (Number.isNaN(ms)) throw new Error(`invalid ${label}: ${value}`)
  return ms
}

function positiveWindow(value, label) {
  if (value === undefined || value === null) return null
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`${label} must be a positive number of milliseconds`)
  }
  return value
}

function sessionLastActivityMs(session) {
  if (!session || typeof session !== 'object') return null
  const value =
    session.lastActivityAt ?? session.lastAgentActivityAt ?? session.last_agent_activity_at
  if (value === undefined || value === null || value === '') return null
  return parseInstant(value, 'session last activity')
}

function isForfeited(claim, createdAtMs, options) {
  if (!options) return false
  if (typeof options !== 'object') throw new Error('claim liveness options must be an object')
  const inactiveAfterMs = positiveWindow(options.inactiveAfterMs, 'inactiveAfterMs')
  const commentAgeAfterMs = positiveWindow(options.commentAgeAfterMs, 'commentAgeAfterMs')
  if (options.forfeitByCommentAge === true && commentAgeAfterMs === null) {
    throw new Error('commentAgeAfterMs must be supplied when forfeitByCommentAge is true')
  }
  const sessions =
    options.sessions && typeof options.sessions === 'object' ? options.sessions : null
  if (claim.sessionId && sessions && Object.hasOwn(sessions, claim.sessionId)) {
    const session = sessions[claim.sessionId]
    const lastActivityMs = sessionLastActivityMs(session)
    if (session && typeof session === 'object' && lastActivityMs === null) {
      throw new Error(`session last activity is required for known claim owner: ${claim.sessionId}`)
    }
    if (inactiveAfterMs !== null && lastActivityMs !== null) {
      const nowMs = parseInstant(options.now, 'claim liveness now')
      return nowMs - lastActivityMs > inactiveAfterMs
    }
    if (lastActivityMs !== null) return false
  }
  if (
    inactiveAfterMs === null &&
    !(options.forfeitByCommentAge === true && commentAgeAfterMs !== null)
  ) {
    return false
  }
  if (options.forfeitByCommentAge === true && commentAgeAfterMs !== null) {
    const nowMs = parseInstant(options.now, 'claim liveness now')
    return nowMs - createdAtMs > commentAgeAfterMs
  }
  return false
}

// Pure first-writer-wins over claims that survived caller-supplied liveness checks:
// provably inactive owners forfeit first, optional comment-age forfeiture second,
// then earliest createdAt wins with the original token tie-break.
export function claimWinner(claims, options = null) {
  if (!claims || claims.length === 0) return null
  const sorted = claims
    .map((claim) => {
      const createdAtMs = parseInstant(claim.createdAt, 'claim createdAt')
      return { ...claim, createdAtMs }
    })
    .filter((claim) => !isForfeited(claim, claim.createdAtMs, options))
  if (sorted.length === 0) return null
  sorted.sort((a, b) => {
    if (a.createdAtMs !== b.createdAtMs) return a.createdAtMs - b.createdAtMs
    if (a.token === b.token) return 0
    return a.token < b.token ? -1 : 1
  })
  return sorted[0].token
}

export function isClaimWon(claims, myToken, options = null) {
  const winner = claimWinner(claims, options)
  if (winner === null) return null
  return winner === myToken
}

// CLI:
//   node linear-claim.mjs token
//     -> prints a fresh run token (stdout)
//   node linear-claim.mjs verdict --me <token> --comments <json-array>
//     -> exit 0 if won, exit 3 if lost. <json-array> is the issue's comments
//        ([{ body, createdAt }, ...]) gathered by the agent via the Linear MCP.
function main(argv) {
  const [cmd, ...rest] = argv
  if (cmd === 'token') {
    process.stdout.write(`${generateRunToken()}\n`)
    return
  }
  if (cmd === 'verdict') {
    let me = null
    let commentsJson = null
    for (let i = 0; i < rest.length; i += 1) {
      if (rest[i] === '--me') me = rest[(i += 1)]
      else if (rest[i] === '--comments') commentsJson = rest[(i += 1)]
    }
    if (!me) throw new Error('--me <token> is required')
    if (!commentsJson) throw new Error('--comments <json-array> is required')
    const claims = parseClaimComments(JSON.parse(commentsJson))
    const won = isClaimWon(claims, me)
    if (won === true) {
      console.log('WON')
      process.exitCode = 0
    } else if (won === null) {
      console.log('NO_WINNER')
      process.exitCode = 4
    } else {
      console.log(`LOST (winner: ${claimWinner(claims) ?? 'none'})`)
      process.exitCode = 3
    }
    return
  }
  throw new Error(`unknown command: ${cmd ?? '(none)'} (expected "token" or "verdict")`)
}

import { isMainModule } from './main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)

if (invokedDirectly) {
  try {
    main(process.argv.slice(2))
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
