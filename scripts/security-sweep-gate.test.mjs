// scripts/security-sweep-gate.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { scoreAlert, isMajorBump, parseSemverMajor } from './security-sweep-gate.mjs';

test('scoreAlert ranks by severity then runtime scope', () => {
  const crit = { security_advisory: { severity: 'critical' }, dependency: { scope: 'runtime' } };
  const lowDev = { security_advisory: { severity: 'low' }, dependency: { scope: 'development' } };
  const medRun = { security_advisory: { severity: 'medium' }, dependency: { scope: 'runtime' } };
  assert.ok(scoreAlert(crit) > scoreAlert(medRun));
  assert.ok(scoreAlert(medRun) > scoreAlert(lowDev));
});

test('parseSemverMajor reads the leading integer', () => {
  assert.equal(parseSemverMajor('2.0.10'), 2);
  assert.equal(parseSemverMajor('v3.1.0'), 3);
  assert.equal(parseSemverMajor('0.16.0'), 0);
});

test('isMajorBump flags a fix that crosses a major boundary', () => {
  // vulnerable_version_range "< 2.0.10", fix "2.0.10" → same major (2) → not major
  assert.equal(isMajorBump('>= 0.16.0, < 2.0.10', '2.0.10'), false);
  // vulnerable "< 5.0.0", fix "6.0.1" → crosses to major 6 → major bump
  assert.equal(isMajorBump('< 5.0.0', '6.0.1'), true);
});

// append to scripts/security-sweep-gate.test.mjs
import { selectBatch, dedupeAgainstPRs } from './security-sweep-gate.mjs';

const mkAlert = (over = {}) => ({
  number: over.number ?? 1,
  security_advisory: { severity: over.severity ?? 'high', ghsa_id: over.ghsa ?? 'GHSA-x' },
  dependency: {
    scope: over.scope ?? 'runtime',
    package: { ecosystem: over.eco ?? 'npm', name: over.pkg ?? 'lodash' },
    manifest_path: over.manifest ?? 'pnpm-lock.yaml',
  },
  security_vulnerability: {
    vulnerable_version_range: over.range ?? '< 4.17.21',
    first_patched_version: over.fixed === null ? null : { identifier: over.fixed ?? '4.17.21' },
  },
});

test('dedupeAgainstPRs drops alerts already covered by an open dependabot PR or sweep sentinel', () => {
  const alerts = [
    mkAlert({ number: 1, pkg: 'lodash' }),
    mkAlert({ number: 2, pkg: 'axios', ghsa: 'GHSA-y' }),
  ];
  const prs = [
    { number: 50, headRefName: 'dependabot/npm_and_yarn/lodash-4.17.21', body: '' },
    { number: 51, headRefName: 'feature/x', body: 'fixes <!-- bs-security-sweep:ghsa:GHSA-y -->' },
  ];
  assert.deepEqual(
    dedupeAgainstPRs(alerts, prs).map((a) => a.number),
    [],
  );
});

test('selectBatch picks one manifest, excludes majors and unfixable, caps size', () => {
  const alerts = [
    mkAlert({ number: 1, manifest: 'pnpm-lock.yaml', severity: 'critical' }),
    mkAlert({ number: 2, manifest: 'pnpm-lock.yaml', severity: 'high' }),
    mkAlert({ number: 3, manifest: 'services/docs/pnpm-lock.yaml', severity: 'low' }),
    mkAlert({ number: 4, manifest: 'pnpm-lock.yaml', range: '< 5.0.0', fixed: '6.0.0' }), // major
    mkAlert({ number: 5, manifest: 'pnpm-lock.yaml', fixed: null }), // unfixable
  ];
  const out = selectBatch(alerts, [], { maxBatch: 10 });
  assert.equal(out.manifest, 'pnpm-lock.yaml');
  assert.deepEqual(out.batch.map((a) => a.number).sort(), [1, 2]);
  assert.ok(out.deferred.some((d) => d.number === 4));
  assert.ok(out.dropped.some((d) => d.number === 5));
});

test('selectBatch returns empty batch when nothing qualifies', () => {
  const out = selectBatch([mkAlert({ fixed: null })], [], {});
  assert.deepEqual(out.batch, []);
});

// append to scripts/security-sweep-gate.test.mjs
import { classifyOutcome, parseState, renderState } from './security-sweep-gate.mjs';

test('classifyOutcome only greenlights a settled mergeable non-rejected PR', () => {
  assert.equal(classifyOutcome('green', 'MERGEABLE', 'APPROVED'), 'green');
  assert.equal(classifyOutcome('green', 'MERGEABLE', 'CHANGES_REQUESTED'), 'retry');
  assert.equal(classifyOutcome('green', 'UNKNOWN', null), 'retry');
  assert.equal(classifyOutcome('no-progress', 'CONFLICTING', null), 'retry');
  assert.equal(classifyOutcome('max-attempts', 'MERGEABLE', null), 'escalate');
  assert.equal(classifyOutcome('blocked', 'MERGEABLE', null), 'escalate');
});

test('renderState/parseState round-trips and embeds ghsa dedupe sentinels', () => {
  const body = renderState({
    attempts: 1,
    lastSha: 'abc1234',
    lastOutcome: 'retry',
    batchGhsas: ['GHSA-a', 'GHSA-b'],
    updatedAt: '2026-06-25T00:00:00Z',
  });
  assert.match(body, /<!-- bs-security-sweep:state -->/);
  assert.match(body, /bs-security-sweep:ghsa:GHSA-a/);
  const st = parseState(body);
  assert.equal(st.attempts, 1);
  assert.equal(st.lastSha, 'abc1234');
  assert.equal(st.lastOutcome, 'retry');
});

test('parseState defaults cleanly on an empty body', () => {
  assert.deepEqual(parseState(''), { attempts: 0, lastSha: '', lastOutcome: '' });
});

// append to scripts/security-sweep-gate.test.mjs
import { runCli } from './security-sweep-gate.mjs';

test('runCli select-batch reads files via injected reader and prints JSON', () => {
  const files = {
    'a.json': JSON.stringify([
      {
        number: 1,
        security_advisory: { severity: 'high', ghsa_id: 'GHSA-x' },
        dependency: {
          scope: 'runtime',
          package: { ecosystem: 'npm', name: 'lodash' },
          manifest_path: 'pnpm-lock.yaml',
        },
        security_vulnerability: {
          vulnerable_version_range: '< 4.17.21',
          first_patched_version: { identifier: '4.17.21' },
        },
      },
    ]),
    'p.json': '[]',
  };
  const out = runCli(['select-batch', 'a.json', 'p.json'], { readFile: (f) => files[f] });
  const parsed = JSON.parse(out);
  assert.equal(parsed.manifest, 'pnpm-lock.yaml');
  assert.deepEqual(
    parsed.batch.map((a) => a.number),
    [1],
  );
});

test('runCli classify maps args to an outcome', () => {
  assert.equal(JSON.parse(runCli(['classify', 'green', 'MERGEABLE', 'APPROVED'], {})), 'green');
});

test('runCli throws on unknown subcommand', () => {
  assert.throws(() => runCli(['bogus'], {}));
});

// append to scripts/security-sweep-gate.test.mjs
import { decideAction } from './security-sweep-gate.mjs';

test('decideAction watches a fresh head and resets the counter', () => {
  const d = decideAction({
    state: { attempts: 2, lastSha: 'old', lastOutcome: 'retry' },
    currentSha: 'new',
    maxAttempts: 3,
  });
  assert.equal(d.action, 'watch');
  assert.equal(d.reset, true);
  assert.equal(d.priorAttempts, 0);
});

test('decideAction escalates when the budget is spent at an unchanged head', () => {
  const d = decideAction({
    state: { attempts: 3, lastSha: 'same', lastOutcome: 'retry' },
    currentSha: 'same',
    maxAttempts: 3,
  });
  assert.equal(d.action, 'escalate');
  assert.equal(d.priorAttempts, 3);
});

test('decideAction keeps watching below the cap at the same head', () => {
  const d = decideAction({
    state: { attempts: 1, lastSha: 'same', lastOutcome: 'retry' },
    currentSha: 'same',
    maxAttempts: 3,
  });
  assert.equal(d.action, 'watch');
  assert.equal(d.priorAttempts, 1);
});

test('decideAction watches a first-ever run (no prior state)', () => {
  const d = decideAction({ state: null, currentSha: 'first', maxAttempts: 3 });
  assert.equal(d.action, 'watch');
  assert.equal(d.priorAttempts, 0);
  assert.equal(d.reset, false);
});

test('runCli decide-action reads the state file and prints the decision JSON', () => {
  const body = renderState({
    attempts: 3,
    lastSha: 'deadbee',
    lastOutcome: 'retry',
    batchGhsas: [],
    updatedAt: '2026-06-25T00:00:00Z',
  });
  const out = runCli(['decide-action', 's.md', 'deadbee', '3'], { readFile: () => body });
  const parsed = JSON.parse(out);
  assert.equal(parsed.action, 'escalate');
  assert.equal(parsed.priorAttempts, 3);
});
