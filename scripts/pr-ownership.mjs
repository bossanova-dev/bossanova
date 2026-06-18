#!/usr/bin/env node

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// Classify the open PR / branch state a bs-linear-implement run finds when it
// starts inside a worktree. The point is to tell apart two situations the old
// "open PR -> NO_CHANGE" guard conflated:
//
//   - the PR/branch is *this ticket's own* (bossd's draft-PR bootstrap, or a
//     prior run of this skill that already did work) -> adopt and resume; and
//   - a *foreign* process has committed real work to this shared worktree ->
//     never co-edit.
//
// A PR/branch with no real work yet (only bossd's placeholder commit) belongs to
// neither case: there is nothing to clobber, so it is always an adoptable
// bootstrap PR even when nothing names the ticket. Ownership only gates branches
// that already carry real file changes.
//
// Ownership mirrors bossd's matchPR() (plugins/bossd-plugin-linear/github.go):
// branch-name match is primary, a "[BOS-NN]" title substring is the fallback.
// We add a third signal: the "Linear issue: <url>" first body line that Step 7
// always writes. Like the sibling helpers this script is pure + dependency-free;
// the agent gathers the inputs (gh / Linear MCP / git) and passes them as JSON.

// bossd's draft-PR bootstrap commit subject (services/bossd/.../lifecycle.go).
// A branch whose only commit ahead of base is this placeholder carries no real
// work yet. Matched loosely so a "[#NN]"-tagged variant still counts as bootstrap.
export const BOOTSTRAP_COMMIT_SUBJECT = 'chore: [skip ci] create pull request';
const BOOTSTRAP_RE = /\[skip ci\]\s*create pull request/i;

// First body line a bs-linear-implement PR always carries (SKILL.md Step 7).
const LINEAR_ISSUE_LINE_RE = /^Linear issue:\s*(\S+)/;

function firstLine(text) {
  return typeof text === 'string' ? (text.split('\n', 1)[0] ?? '').trim() : '';
}

export function isBootstrapSubject(subject) {
  return BOOTSTRAP_RE.test(String(subject ?? ''));
}

// Subjects ahead of base with the bootstrap placeholder removed. Empty array
// means the branch carries no real implementation work yet.
export function realAheadSubjects(aheadSubjects) {
  return (Array.isArray(aheadSubjects) ? aheadSubjects : [])
    .map((s) => String(s ?? '').trim())
    .filter((s) => s.length > 0 && !isBootstrapSubject(s));
}

// Does an open PR belong to the target ticket? branch (primary) -> title -> body url.
export function isOwnedPR({ ticketId, issueBranch, issueUrl, pr } = {}) {
  if (!pr || typeof pr !== 'object') return false;
  const headBranch = typeof pr.headBranch === 'string' ? pr.headBranch : '';
  if (issueBranch && headBranch && headBranch === issueBranch) return true;
  const title = typeof pr.title === 'string' ? pr.title : '';
  if (ticketId && title.includes(`[${ticketId}]`)) return true;
  const bodyUrl = LINEAR_ISSUE_LINE_RE.exec(firstLine(pr.body))?.[1] ?? '';
  if (issueUrl && bodyUrl && bodyUrl === issueUrl) return true;
  return false;
}

// How much real (non-bootstrap) work the branch already carries. Complete vs
// partial is NOT decidable from commit subjects alone — that is the skill's job
// via the PR-body acceptance-criteria checklist — so this stays binary.
export function implementedState({ aheadSubjects } = {}) {
  return realAheadSubjects(aheadSubjects).length === 0 ? 'empty' : 'populated';
}

// Did HEAD advance during the preflight observation window? A live concurrent
// writer appending commits is the one case where adopting our own PR is unsafe.
export function isLiveWriter({ headBefore, headAfter } = {}) {
  const before = typeof headBefore === 'string' ? headBefore.trim() : '';
  const after = typeof headAfter === 'string' ? headAfter.trim() : '';
  if (!before || !after) return false;
  return before !== after;
}

// classify -> 'none' | 'foreign' | 'bootstrap-only' | 'owned'.
//   none           no open PR and no real branch-ahead work
//   foreign        a PR/branch carrying *real work* that matches no ownership
//                  signal -> someone else's changes, never co-edit
//   bootstrap-only only bossd's placeholder commit ahead (no real file changes)
//                  -> fresh path, reuse the bootstrap PR instead of creating one
//   owned          owned PR/branch with real work ahead -> resume/adopt
//
// Foreign protection is about not clobbering another process's *work*. An empty
// PR/branch (only the bootstrap placeholder ahead, no real file changes) holds
// nothing to clobber, so it is always adoptable — even when its branch/title/body
// name no ticket (e.g. a human-named "security-review" branch + draft PR). Only a
// branch that already carries real work has to prove ownership before we touch it.
export function classifyPR({
  ticketId,
  sessionBranch,
  issueBranch,
  issueUrl,
  pr,
  aheadSubjects,
} = {}) {
  const hasRealWork = implementedState({ aheadSubjects }) === 'populated';
  if (!pr) {
    if (!hasRealWork) return 'none';
    const session = typeof sessionBranch === 'string' ? sessionBranch.trim() : '';
    const issue = typeof issueBranch === 'string' ? issueBranch.trim() : '';
    if (session && issue && session !== issue) return 'foreign';
    return 'owned';
  }
  // An open PR with no real work is a reusable bootstrap PR regardless of whether
  // it names the ticket; only a PR holding real work must pass the ownership test.
  if (!hasRealWork) return 'bootstrap-only';
  if (!isOwnedPR({ ticketId, issueBranch, issueUrl, pr })) return 'foreign';
  return 'owned';
}

// Normalize a `gh pr view`/`gh pr list` payload (object or array) into the shape
// the pure functions expect. gh names the branch field `headRefName`.
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
//   node scripts/pr-ownership.mjs classify --ticket BOS-23 --issue-branch <b> \
//     --session-branch <b> --issue-url <url> --pr-json <gh-json|""> \
//     ( --ahead-subjects-json <json-array> | --ahead-subjects-stdin )
//     -> prints owned | bootstrap-only | foreign | none  (stdout, exit 0)
//   git log --pretty=%s "$BASE_BRANCH..HEAD" | node scripts/pr-ownership.mjs classify \
//     ... --ahead-subjects-stdin    # newline-delimited subjects on stdin (no JSON wrangling)
//   node scripts/pr-ownership.mjs number --pr-json <gh-json|"">
//     -> prints the first open PR number, or blank when there is no open PR
//   node scripts/pr-ownership.mjs live --head-before <sha> --head-after <sha>
//     -> exit 0 if a live writer was detected (HEAD moved), exit 3 otherwise
function main(argv) {
  const [cmd, ...rest] = argv;
  const flags = parseFlags(rest);

  if (cmd === 'classify') {
    const pr =
      flags['pr-json'] && flags['pr-json'] !== ''
        ? normalizePr(JSON.parse(flags['pr-json']))
        : null;
    let aheadSubjects = [];
    if ('ahead-subjects-stdin' in flags) {
      aheadSubjects = fs.readFileSync(0, 'utf8').split('\n');
    } else if (flags['ahead-subjects-json']) {
      aheadSubjects = JSON.parse(flags['ahead-subjects-json']);
    }
    process.stdout.write(
      `${classifyPR({
        ticketId: flags.ticket ?? '',
        sessionBranch: flags['session-branch'] ?? '',
        issueBranch: flags['issue-branch'] ?? '',
        issueUrl: flags['issue-url'] ?? '',
        pr,
        aheadSubjects,
      })}\n`,
    );
    return;
  }

  if (cmd === 'number') {
    const pr =
      flags['pr-json'] && flags['pr-json'] !== ''
        ? normalizePr(JSON.parse(flags['pr-json']))
        : null;
    process.stdout.write(pr?.number ? `${pr.number}\n` : '\n');
    return;
  }

  if (cmd === 'live') {
    const live = isLiveWriter({ headBefore: flags['head-before'], headAfter: flags['head-after'] });
    console.log(live ? 'LIVE' : 'STATIC');
    // Exit 0 == LIVE (caller bails NO_CHANGE), exit 3 == STATIC (proceed). The
    // STATIC code is deliberately 3, distinct from the generic error exit 1
    // below, so a caller never reads a launch/parse failure as "static/proceed".
    process.exitCode = live ? 0 : 3;
    return;
  }

  throw new Error(`unknown command: ${cmd ?? '(none)'} (expected "classify" or "live")`);
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
