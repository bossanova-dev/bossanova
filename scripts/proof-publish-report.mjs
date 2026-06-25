#!/usr/bin/env node

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

import { renderGallery } from './proof-lib.mjs';

/**
 * Converts an ISO 8601 timestamp string to `YYYYMMDD-HHMMSS`.
 * Parses the string directly with a regex to avoid timezone surprises.
 * @param {string} iso - e.g. '2026-06-23T08:44:48.123Z'
 * @returns {string} e.g. '20260623-084448'
 */
export function formatReportTimestamp(iso) {
  const match = String(iso ?? '').match(/^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})/);
  if (!match) {
    throw new Error(`malformed ISO timestamp: ${iso}`);
  }
  const [, yyyy, mm, dd, hh, min, ss] = match;
  return `${yyyy}${mm}${dd}-${hh}${min}${ss}`;
}

/**
 * Returns the report publish target configuration from env.
 * @param {Record<string, string|undefined>} env
 * @returns {{ enabled: boolean, repo: string|null, reason?: string }}
 */
export function reportTarget(env) {
  const repo = env.BOSS_PROOF_REPORT_REPO ?? 'recurser/bs-proof';
  if (env.BOSS_PROOF_REPORT === '0' || repo === '') {
    return { enabled: false, repo: null, reason: 'disabled' };
  }
  return { enabled: true, repo };
}

/**
 * Slugifies a string: lowercase, each run of non-[a-z0-9] → '-', trim '-',
 * fallback 'unknown' if empty.
 * @param {string} value
 * @returns {string}
 */
function slug(value) {
  const s = String(value ?? '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
  return s || 'unknown';
}

/**
 * Builds the destination path within the report repo for this run.
 * @param {{ owner: string, sourceRepo: string, prNumber: string|undefined, branch: string, timestamp: string, runId: string }} opts
 * @returns {string} e.g. 'recurser-bossanova/pr-788/20260623-084448-abc123'
 */
export function proofReportDestPath({ owner, sourceRepo, prNumber, branch, timestamp, runId }) {
  const seg1 = `${owner}-${sourceRepo}`;
  const isNumericPr =
    typeof prNumber === 'string' && /^\d+$/.test(prNumber) && Number(prNumber) > 0;
  const seg2 = isNumericPr ? `pr-${prNumber}` : `branch-${slug(branch)}`;
  const seg3 = `${timestamp}-${runId}`;
  return `${seg1}/${seg2}/${seg3}`;
}

/**
 * Returns the GitHub blob URL for the README in the report repo.
 * @param {{ repo: string, destPath: string }} opts
 * @returns {string}
 */
export function buildReportUrl({ repo, destPath }) {
  return `https://github.com/${repo}/blob/main/${destPath}/README.md`;
}

/**
 * Orchestrates `git push` with rebase-retry via an injected `runGit`.
 * @param {{ runGit: (args: string[]) => { status: number }, maxRetries?: number }} opts
 * @returns {Promise<{ ok: boolean, attempts: number }>}
 */
export async function pushWithRebaseRetry({ runGit, maxRetries = 3 }) {
  let attempts = 0;
  while (attempts < maxRetries) {
    if (attempts > 0) runGit(['pull', '--rebase', 'origin', 'main']);
    const result = runGit(['push', 'origin', 'HEAD:main']);
    attempts += 1;
    if (result.status === 0) return { ok: true, attempts };
  }
  return { ok: false, attempts };
}

/**
 * Publishes a per-PR proof report (manifest.json + README.md) to a separate
 * git repo. Gracefully skips on disabled/clone/push failures so the core
 * R2 + PR-comment flow is never blocked.
 *
 * @param {{ manifest: object, identity: object, env?: object, deps?: object }} opts
 * @returns {Promise<{ ok: boolean, skipped: boolean, reason?: string, reportUrl?: string }>}
 */
export async function publishProofReport({ manifest, identity, env = process.env, deps = {} }) {
  let tmp;
  try {
    const {
      mkdtemp = (prefix) => fs.mkdtempSync(path.join(os.tmpdir(), prefix)),
      writeFile = (filePath, content) => fs.writeFileSync(filePath, content, 'utf8'),
      mkdirp = (dir) => fs.mkdirSync(dir, { recursive: true }),
      rmrf = (dir) => fs.rmSync(dir, { recursive: true, force: true }),
      cloneRepo = (repo, dir) =>
        spawnSync('git', ['clone', '--depth', '1', `https://github.com/${repo}.git`, dir], {
          encoding: 'utf8',
        }),
      runGit = (args, { cwd } = {}) => spawnSync('git', args, { cwd, encoding: 'utf8' }),
      log = console,
    } = deps;

    // Step 1: check if publishing is enabled.
    const target = reportTarget(env);
    if (!target.enabled) {
      return { ok: false, skipped: true, reason: target.reason };
    }
    const { repo } = target;

    // Step 2: derive timestamp, destPath, and the (deterministic) report URL.
    const timestamp = formatReportTimestamp(manifest.generatedAt);
    const destPath = proofReportDestPath({ ...identity, timestamp });
    const reportUrl = buildReportUrl({ repo, destPath });

    // Step 3: clone the report repo.
    tmp = mkdtemp('bs-proof-');
    const cloneResult = cloneRepo(repo, tmp);
    if (cloneResult.status !== 0) {
      log.warn(
        `[proof] report repo clone failed — skipping report publish (${cloneResult.stderr ?? ''})`,
      );
      rmrf(tmp);
      return { ok: false, skipped: true, reason: 'clone-failed' };
    }

    // Step 4: write manifest.json and README.md. The committed manifest carries
    // its own reportUrl (self-referential, deterministic from destPath) so the
    // git-native artifact is self-describing.
    const manifestWithUrl = { ...manifest, reportUrl };
    const destDir = path.join(tmp, destPath);
    mkdirp(destDir);
    writeFile(path.join(destDir, 'manifest.json'), `${JSON.stringify(manifestWithUrl, null, 2)}\n`);
    writeFile(path.join(destDir, 'README.md'), renderGallery({ manifest: manifestWithUrl }));

    // Step 5: git add + commit. Pass an author identity inline via `-c` so the
    // commit succeeds in fresh CI/agent environments that have no global
    // user.name/user.email configured (otherwise git aborts with "Author
    // identity unknown" and the report never publishes).
    const boundRunGit = (args) => runGit(args, { cwd: tmp });
    boundRunGit(['add', '-A']);
    const commitMsg = `proof: ${identity.owner}/${identity.sourceRepo}#${identity.prNumber} ${identity.runId}`;
    const commitResult = boundRunGit([
      '-c',
      'user.name=bossanova-proof',
      '-c',
      'user.email=proof@bossanova.dev',
      'commit',
      '-m',
      commitMsg,
    ]);
    if (commitResult.status !== 0) {
      log.warn('[proof] report commit failed — skipping report publish (nothing to commit?)');
      rmrf(tmp);
      return { ok: false, skipped: true, reason: 'commit-failed' };
    }

    // Step 6: push with rebase-retry.
    const pushResult = await pushWithRebaseRetry({ runGit: boundRunGit });
    if (!pushResult.ok) {
      log.warn(
        `[proof] report push failed after ${pushResult.attempts} attempts — skipping report publish`,
      );
      rmrf(tmp);
      return { ok: false, skipped: true, reason: 'push-failed' };
    }

    // Step 7: clean up and return.
    rmrf(tmp);
    return { ok: true, skipped: false, reportUrl };
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    (deps.log ?? console).warn(`[proof] unexpected error in publishProofReport: ${msg}`);
    if (tmp) {
      const rmrf = deps.rmrf ?? ((dir) => fs.rmSync(dir, { recursive: true, force: true }));
      try {
        rmrf(tmp);
      } catch {}
    }
    return { ok: false, skipped: true, reason: `error: ${msg}` };
  }
}
