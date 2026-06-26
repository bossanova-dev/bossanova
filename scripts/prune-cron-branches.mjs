#!/usr/bin/env node
// prune-cron-branches.mjs — safely delete leftover cron-* branches from finished
// cron runs, after the bossd session that owned them is gone.
//
// Cron runs create a per-fire worktree branch (e.g. cron-bossanova-sweep-plan-<epoch>).
// When a run finalizes as a no-op the session row and worktree are removed, but the
// local branch ref can linger — hundreds accumulate over time. This prunes the
// genuinely-dead ones and is DRY-RUN BY DEFAULT.
//
// A branch is only deleted when it is unambiguously safe:
//   - its tip is already merged into the base branch (fully reachable), OR
//   - GitHub reports its PR as MERGED.
// Anything still referenced by a live bossd session, carrying an OPEN PR, or holding
// unmerged commits with no PR is KEPT or flagged for REVIEW — never auto-deleted.
//
// Usage:
//   node scripts/prune-cron-branches.mjs                 # dry run, report only
//   node scripts/prune-cron-branches.mjs --apply         # actually delete the safe ones
//   node scripts/prune-cron-branches.mjs --glob 'cron-*' --base main
//   node scripts/prune-cron-branches.mjs --db "/path/to/bossd.db"
//   node scripts/prune-cron-branches.mjs --no-gh         # skip GitHub PR lookups (offline)
//
// Requires: git. Optional: gh (PR state), sqlite3 (live-session guard). When sqlite3
// or the DB is unavailable the live-session guard cannot run, so the script refuses to
// delete unless --force is passed — preferring to leave branches over risking a live one.

import { execFileSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { homedir, platform } from 'node:os';
import { join } from 'node:path';

// classifyCronBranch is the pure decision core (unit-tested). Given what we know
// about a branch, it returns { action, reason } where action ∈ keep|delete|review.
export function classifyCronBranch({ hasLiveSession, prState, mergedIntoBase }) {
  if (hasLiveSession) return { action: 'keep', reason: 'live bossd session references branch' };
  if (prState === 'OPEN') return { action: 'keep', reason: 'open PR' };
  if (prState === 'MERGED') return { action: 'delete', reason: 'PR merged' };
  if (mergedIntoBase) return { action: 'delete', reason: 'tip merged into base' };
  if (prState === 'CLOSED')
    return { action: 'review', reason: 'PR closed unmerged — inspect before deleting' };
  return { action: 'review', reason: 'no PR and unmerged commits — inspect before deleting' };
}

function defaultDBPath() {
  // Mirrors services/bossd/internal/db/db.go DefaultDBPath().
  if (platform() === 'darwin') {
    return join(homedir(), 'Library', 'Application Support', 'bossanova', 'bossd.db');
  }
  return join(homedir(), '.config', 'bossanova', 'bossd.db');
}

function parseArgs(argv) {
  const opts = {
    apply: false,
    glob: 'cron-*',
    base: '',
    db: defaultDBPath(),
    gh: true,
    force: false,
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--apply') opts.apply = true;
    else if (a === '--force') opts.force = true;
    else if (a === '--no-gh') opts.gh = false;
    else if (a === '--glob') opts.glob = argv[++i];
    else if (a === '--base') opts.base = argv[++i];
    else if (a === '--db') opts.db = argv[++i];
    else if (a === '--help' || a === '-h') opts.help = true;
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function git(args, { allowFail = false } = {}) {
  try {
    return execFileSync('git', args, { encoding: 'utf8' }).trim();
  } catch (err) {
    if (allowFail) return '';
    throw err;
  }
}

function detectBase(explicit) {
  if (explicit) return explicit;
  // origin/HEAD → the repo's default branch; fall back to main/master.
  const head = git(['symbolic-ref', '--quiet', 'refs/remotes/origin/HEAD'], { allowFail: true });
  if (head) return head.replace('refs/remotes/origin/', '');
  for (const b of ['main', 'master']) {
    if (git(['rev-parse', '--verify', '--quiet', b], { allowFail: true })) return b;
  }
  return 'main';
}

function listCronBranches(glob) {
  const out = git(['for-each-ref', '--format=%(refname:short)', `refs/heads/${glob}`], {
    allowFail: true,
  });
  return out ? out.split('\n').filter(Boolean) : [];
}

function liveSessionBranches(dbPath) {
  // Returns a Set of branch_name values still referenced by a bossd session, or
  // null when the DB/sqlite3 is unavailable (caller treats null as "unknown").
  if (!existsSync(dbPath)) return null;
  try {
    const out = execFileSync('sqlite3', [dbPath, 'SELECT branch_name FROM sessions;'], {
      encoding: 'utf8',
    });
    return new Set(
      out
        .split('\n')
        .map((s) => s.trim())
        .filter(Boolean),
    );
  } catch {
    return null;
  }
}

function prStateFor(branch, useGh) {
  if (!useGh) return 'UNKNOWN';
  try {
    const out = execFileSync(
      'gh',
      ['pr', 'list', '--head', branch, '--state', 'all', '--json', 'state', '--limit', '1'],
      { encoding: 'utf8' },
    );
    const arr = JSON.parse(out);
    if (!arr.length) return 'NONE';
    return String(arr[0].state || '').toUpperCase(); // OPEN | CLOSED | MERGED
  } catch {
    return 'UNKNOWN';
  }
}

function mergedIntoBase(branch, base) {
  // True when every commit on `branch` is reachable from base (origin/base preferred).
  for (const ref of [`refs/remotes/origin/${base}`, base]) {
    if (!git(['rev-parse', '--verify', '--quiet', ref], { allowFail: true })) continue;
    const out = git(['branch', '--format=%(refname:short)', '--merged', ref], { allowFail: true });
    if (
      out
        .split('\n')
        .map((s) => s.trim())
        .includes(branch)
    )
      return true;
  }
  return false;
}

function help() {
  process.stdout.write(
    [
      'prune-cron-branches.mjs — delete leftover cron-* branches from finished runs (DRY RUN by default).',
      '',
      "  --apply            actually delete the branches classified 'delete' (default: report only)",
      '  --glob <pattern>   branch glob to scan (default: cron-*)',
      '  --base <branch>    base branch for the merged check (default: origin/HEAD or main)',
      '  --db <path>        bossd.db path for the live-session guard (default: platform default)',
      '  --no-gh            skip GitHub PR lookups (merged-into-base check only)',
      '  --force            allow deletes even when the live-session guard could not run',
      '  -h, --help         this help',
      '',
    ].join('\n'),
  );
}

function main() {
  const opts = parseArgs(process.argv.slice(2));
  if (opts.help) return help();

  const base = detectBase(opts.base);
  const branches = listCronBranches(opts.glob);
  if (!branches.length) {
    process.stdout.write(`No branches match ${opts.glob}. Nothing to do.\n`);
    return;
  }

  const live = liveSessionBranches(opts.db);
  const liveGuardActive = live !== null;
  if (!liveGuardActive) {
    process.stderr.write(
      `WARN: live-session guard inactive (no sqlite3 or DB at ${opts.db}). ` +
        `Deletes are blocked unless --force.\n`,
    );
  }

  const rows = branches.map((branch) => {
    const hasLiveSession = liveGuardActive ? live.has(branch) : false;
    const prState = prStateFor(branch, opts.gh);
    const merged = mergedIntoBase(branch, base);
    const { action, reason } = classifyCronBranch({
      hasLiveSession,
      prState,
      mergedIntoBase: merged,
    });
    return { branch, prState, merged, action, reason };
  });

  for (const r of rows) {
    process.stdout.write(
      `${r.action.toUpperCase().padEnd(6)} ${r.branch}  [pr=${r.prState} merged=${r.merged}]  ${r.reason}\n`,
    );
  }

  const deletable = rows.filter((r) => r.action === 'delete');
  const counts = rows.reduce((m, r) => ((m[r.action] = (m[r.action] || 0) + 1), m), {});
  process.stdout.write(
    `\nSummary: ${deletable.length} deletable, ${counts.keep || 0} kept, ${counts.review || 0} need review (base=${base}).\n`,
  );

  if (!opts.apply) {
    process.stdout.write("Dry run — re-run with --apply to delete the 'delete' branches.\n");
    return;
  }
  if (!liveGuardActive && !opts.force) {
    process.stderr.write(
      'Refusing to delete with the live-session guard inactive. Pass --force to override.\n',
    );
    process.exitCode = 1;
    return;
  }
  for (const r of deletable) {
    git(['branch', '-D', r.branch]);
    process.stdout.write(`deleted ${r.branch}\n`);
  }
}

// Only run when invoked directly, so the test file can import classifyCronBranch.
if (import.meta.url === `file://${process.argv[1]}`) {
  try {
    main();
  } catch (err) {
    process.stderr.write(`${err.message}\n`);
    process.exitCode = 1;
  }
}
