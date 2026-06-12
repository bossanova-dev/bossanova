#!/usr/bin/env node

import crypto from 'node:crypto';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// Marker prefix for claim comments. The agent posts formatClaimComment(token)
// on the issue, waits a few seconds for racers' comments to land, re-reads all
// comments, and asks this script whether its token won.
export const CLAIM_MARKER = 'bs-linear-implement-claim';

// Anchor the trailing boundary so a malformed/crafted comment body with extra
// hex (e.g. a 40-char string) can't have its first 32 chars captured as a token.
const CLAIM_RE = new RegExp(`${CLAIM_MARKER}:([0-9a-f]{32})(?![0-9a-f])`);

export function generateRunToken() {
  return crypto.randomBytes(16).toString('hex');
}

export function formatClaimComment(token) {
  return `🔒 ${CLAIM_MARKER}:${token} (bs-linear-implement run claiming this ticket)`;
}

// Extract { token, createdAt } from claim comments; ignore everything else.
export function parseClaimComments(comments) {
  const claims = [];
  for (const c of comments || []) {
    const match = typeof c?.body === 'string' ? c.body.match(CLAIM_RE) : null;
    if (match) claims.push({ token: match[1], createdAt: String(c.createdAt) });
  }
  return claims;
}

// Pure first-writer-wins: earliest createdAt, tie-broken by smallest token.
// Deterministic across all racers given the same comment set.
export function claimWinner(claims) {
  if (!claims || claims.length === 0) return null;
  const sorted = claims.map((claim) => {
    const createdAtMs = Date.parse(claim.createdAt);
    if (Number.isNaN(createdAtMs)) {
      throw new Error(`invalid claim createdAt: ${claim.createdAt}`);
    }
    return { ...claim, createdAtMs };
  });
  sorted.sort((a, b) => {
    if (a.createdAtMs !== b.createdAtMs) return a.createdAtMs - b.createdAtMs;
    if (a.token === b.token) return 0;
    return a.token < b.token ? -1 : 1;
  });
  return sorted[0].token;
}

export function isClaimWon(claims, myToken) {
  return claimWinner(claims) === myToken;
}

// CLI:
//   node scripts/linear-claim.mjs token
//     -> prints a fresh run token (stdout)
//   node scripts/linear-claim.mjs verdict --me <token> --comments <json-array>
//     -> exit 0 if won, exit 3 if lost. <json-array> is the issue's comments
//        ([{ body, createdAt }, ...]) gathered by the agent via the Linear MCP.
function main(argv) {
  const [cmd, ...rest] = argv;
  if (cmd === 'token') {
    process.stdout.write(`${generateRunToken()}\n`);
    return;
  }
  if (cmd === 'verdict') {
    let me = null;
    let commentsJson = null;
    for (let i = 0; i < rest.length; i += 1) {
      if (rest[i] === '--me') me = rest[(i += 1)];
      else if (rest[i] === '--comments') commentsJson = rest[(i += 1)];
    }
    if (!me) throw new Error('--me <token> is required');
    if (!commentsJson) throw new Error('--comments <json-array> is required');
    const claims = parseClaimComments(JSON.parse(commentsJson));
    if (isClaimWon(claims, me)) {
      console.log('WON');
      process.exitCode = 0;
    } else {
      console.log(`LOST (winner: ${claimWinner(claims) ?? 'none'})`);
      process.exitCode = 3;
    }
    return;
  }
  throw new Error(`unknown command: ${cmd ?? '(none)'} (expected "token" or "verdict")`);
}

const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);

if (invokedDirectly) {
  try {
    main(process.argv.slice(2));
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  }
}
