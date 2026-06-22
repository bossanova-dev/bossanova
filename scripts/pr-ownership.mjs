#!/usr/bin/env node

import path from 'node:path';
import { fileURLToPath } from 'node:url';

// Extract the open PR number from a `gh pr view`/`gh pr list` payload so a
// bs-linear-implement run can find the PR it should push to. Pure and
// dependency-free: the agent gathers the gh JSON and passes it as a flag.

// Normalize a `gh pr view`/`gh pr list` payload (object or array) into the shape
// we need. gh names the branch field `headRefName`; an array is reduced to the
// first non-closed entry.
function normalizePr(raw) {
  let pr = raw;
  if (Array.isArray(pr))
    pr = pr.find((p) => p && p.state !== 'CLOSED' && p.state !== 'MERGED') ?? pr[0] ?? null;
  if (!pr || typeof pr !== 'object') return null;
  return {
    number: pr.number,
    title: pr.title,
    body: pr.body,
    headBranch: pr.headBranch ?? pr.headRefName ?? '',
  };
}

function parseFlags(rest) {
  const flags = {};
  for (let i = 0; i < rest.length; i += 1) {
    const key = rest[i];
    if (typeof key !== 'string' || !key.startsWith('--')) continue;
    const next = rest[i + 1];
    // Boolean flag when the next token is another --flag or absent.
    if (typeof next !== 'string' || next.startsWith('--')) {
      flags[key.slice(2)] = true;
    } else {
      flags[key.slice(2)] = next;
      i += 1;
    }
  }
  return flags;
}

// CLI:
//   node scripts/pr-ownership.mjs number --pr-json <gh-json|"">
//     -> prints the first open PR number, or blank when there is no open PR
function main(argv) {
  const [cmd, ...rest] = argv;
  const flags = parseFlags(rest);

  if (cmd === 'number') {
    const pr =
      flags['pr-json'] && flags['pr-json'] !== ''
        ? normalizePr(JSON.parse(flags['pr-json']))
        : null;
    process.stdout.write(pr?.number ? `${pr.number}\n` : '\n');
    return;
  }

  throw new Error(`unknown command: ${cmd ?? '(none)'} (expected "number")`);
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
