// scripts/security-sweep-gate.mjs
// Pure-core decision logic for the bs-security-sweep skill. NO side effects,
// NO imports beyond node builtins — the cron worktree is dependency-free.

const SEVERITY_WEIGHT = { critical: 400, high: 300, medium: 200, low: 100 };

export function scoreAlert(alert) {
  const sev = alert?.security_advisory?.severity ?? 'low';
  const scopeBonus = alert?.dependency?.scope === 'runtime' ? 10 : 0;
  return (SEVERITY_WEIGHT[sev] ?? 0) + scopeBonus;
}

export function parseSemverMajor(version) {
  const m = String(version ?? '').match(/(\d+)/);
  return m ? Number(m[1]) : 0;
}

// Upper bound of a vulnerable_version_range like ">= 0.16.0, < 2.0.10".
function upperBoundMajor(range) {
  const m = String(range ?? '').match(/<\s*=?\s*([0-9][^\s,]*)/);
  return m ? parseSemverMajor(m[1]) : null;
}

export function isMajorBump(vulnerableRange, fixedVersion) {
  const fixedMajor = parseSemverMajor(fixedVersion);
  const upper = upperBoundMajor(vulnerableRange);
  // No usable upper bound → be conservative and treat as major (defer it).
  if (upper === null) return true;
  return fixedMajor > upper;
}

// append to scripts/security-sweep-gate.mjs
const ghsaOf = (a) => a?.security_advisory?.ghsa_id ?? '';
const pkgOf = (a) => a?.dependency?.package?.name ?? '';
const groupKey = (a) => `${a?.dependency?.package?.ecosystem} ${a?.dependency?.manifest_path}`;
const fixedOf = (a) => a?.security_vulnerability?.first_patched_version?.identifier ?? null;
const rangeOf = (a) => a?.security_vulnerability?.vulnerable_version_range ?? '';

export function dedupeAgainstPRs(alerts, openPRs = []) {
  const sentinels = new Set();
  const depBranchText = [];
  for (const pr of openPRs) {
    for (const m of String(pr.body ?? '').matchAll(/bs-security-sweep:ghsa:([A-Za-z0-9-]+)/g)) {
      sentinels.add(m[1]);
    }
    if (String(pr.headRefName ?? '').startsWith('dependabot/')) depBranchText.push(pr.headRefName);
  }
  return alerts.filter((a) => {
    if (sentinels.has(ghsaOf(a))) return false;
    const pkg = pkgOf(a);
    if (pkg && depBranchText.some((b) => b.includes(pkg))) return false;
    return true;
  });
}

export function selectBatch(alerts, openPRs = [], opts = {}) {
  const maxBatch = opts.maxBatch ?? 10;
  const fresh = dedupeAgainstPRs(alerts, openPRs);

  // Group by (ecosystem, manifest).
  const groups = new Map();
  for (const a of fresh) {
    const k = groupKey(a);
    if (!groups.has(k)) groups.set(k, []);
    groups.get(k).push(a);
  }
  if (groups.size === 0) return { manifest: null, ecosystem: null, batch: [], deferred: [], dropped: [] };

  // Rank groups: total score desc, count desc, manifest path asc.
  const ranked = [...groups.entries()]
    .map(([k, list]) => ({ k, list, score: list.reduce((s, a) => s + scoreAlert(a), 0) }))
    .sort((x, y) =>
      y.score - x.score ||
      y.list.length - x.list.length ||
      (x.list[0].dependency.manifest_path < y.list[0].dependency.manifest_path ? -1 : 1));

  const chosen = ranked[0].list;
  const ecosystem = chosen[0].dependency.package.ecosystem;
  const manifest = chosen[0].dependency.manifest_path;

  const dropped = [];
  const deferred = [];
  const fixable = [];
  for (const a of chosen) {
    const fixed = fixedOf(a);
    if (!fixed) { dropped.push({ number: a.number, ghsa: ghsaOf(a), reason: 'no patched version' }); continue; }
    if (isMajorBump(rangeOf(a), fixed)) { deferred.push({ number: a.number, ghsa: ghsaOf(a), reason: 'major version bump' }); continue; }
    fixable.push(a);
  }
  fixable.sort((x, y) => scoreAlert(y) - scoreAlert(x));
  const batch = fixable.slice(0, maxBatch);
  for (const a of fixable.slice(maxBatch)) deferred.push({ number: a.number, ghsa: ghsaOf(a), reason: 'over per-run cap' });

  return { manifest, ecosystem, batch, deferred, dropped };
}

// append to scripts/security-sweep-gate.mjs
export function classifyOutcome(repairResult, mergeable, reviewDecision) {
  if (repairResult === 'max-attempts' || repairResult === 'blocked') return 'escalate';
  if (repairResult === 'green' && mergeable === 'MERGEABLE' && reviewDecision !== 'CHANGES_REQUESTED') return 'green';
  return 'retry';
}

const STATE_SENTINEL = '<!-- bs-security-sweep:state -->';

export function parseState(body = '') {
  const text = String(body ?? '');
  const num = (re) => { const m = text.match(re); return m ? m[1] : null; };
  return {
    attempts: Number(num(/attempts:\s*(\d+)/) ?? 0),
    lastSha: num(/lastSha:\s*([0-9a-f]+)/) ?? '',
    lastOutcome: num(/lastOutcome:\s*(\w[\w-]*)/) ?? '',
  };
}

export function renderState({ attempts, lastSha, lastOutcome, batchGhsas = [], updatedAt }) {
  const ghsaLines = batchGhsas.map((g) => `<!-- bs-security-sweep:ghsa:${g} -->`).join('\n');
  return [
    STATE_SENTINEL,
    '🔒 **bs-security-sweep** automated security fix PR.',
    '',
    '```',
    `attempts: ${Number(attempts)}`,
    `lastSha: ${lastSha}`,
    `lastOutcome: ${lastOutcome}`,
    `updatedAt: ${updatedAt}`,
    '```',
    ghsaLines,
  ].join('\n');
}

// append to scripts/security-sweep-gate.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

export function runCli(argv, { readFile = (f) => readFileSync(f, 'utf8') } = {}) {
  const [cmd, ...rest] = argv;
  const json = (f) => JSON.parse(readFile(f));
  switch (cmd) {
    case 'score':
      return JSON.stringify(json(rest[0]).map((a) => ({ number: a.number, score: scoreAlert(a) })));
    case 'select-batch':
      return JSON.stringify(selectBatch(json(rest[0]), json(rest[1]), { maxBatch: rest[2] ? Number(rest[2]) : 10 }));
    case 'dedupe':
      return JSON.stringify(dedupeAgainstPRs(json(rest[0]), json(rest[1])).map((a) => a.number));
    case 'classify':
      return JSON.stringify(classifyOutcome(rest[0], rest[1], rest[2] === 'null' ? null : rest[2]));
    case 'render-state':
      return renderState(JSON.parse(rest[0]));
    case 'parse-state':
      return JSON.stringify(parseState(readFile(rest[0])));
    default:
      throw new Error(`unknown subcommand: ${cmd}`);
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try {
    process.stdout.write(runCli(process.argv.slice(2)));
    process.stdout.write('\n');
  } catch (err) {
    process.stderr.write(`security-sweep-gate: ${err.message}\n`);
    process.exit(1);
  }
}
