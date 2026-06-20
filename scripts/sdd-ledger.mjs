#!/usr/bin/env node
// scripts/sdd-ledger.mjs — durable, compaction-proof progress ledger for
// bs-linear-implement. Mirrors the SDD "Durable Progress" mechanism: the
// controller records the base..head commit range each implementer/fix subagent
// produces, so after a context reset it recognizes those commits as ITS OWN
// rather than a foreign peer's. Stored OUTSIDE the worktree (so it never trips
// the change gate) and keyed on the canonical worktree path (so it is found
// again from the path alone, with no remembered runid).
import crypto from 'node:crypto';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const LEDGER_HOME =
  process.env.BLI_LEDGER_HOME || path.join(os.homedir(), '.local/state/bossanova');

export function hashPath(p) {
  return crypto.createHash('sha1').update(p).digest('hex').slice(0, 12);
}

export function ledgerPathFor(canonTop, home = LEDGER_HOME) {
  const slug = path.basename(canonTop);
  return path.join(home, 'linear-implement', 'ledger', slug, `${slug}-${hashPath(canonTop)}.md`);
}

export function renderHeader({ ticket, branch, base }) {
  return [
    '# bs-linear-implement progress ledger',
    '',
    `- ticket: ${ticket}`,
    `- branch: ${branch}`,
    `- run base: ${base}`,
    '',
    '## Tasks',
    '',
  ].join('\n');
}

export function renderTaskLine({ task, base, head, status, note }) {
  const b = String(base || '');
  if (status === 'complete') {
    const h = String(head || '');
    const suffix = note ? `, ${note}` : '';
    return `Task ${task}: complete (commits ${b}..${h}${suffix})`;
  }
  return `Task ${task}: in-progress (base ${b})`;
}

const TASK_RE = /^Task ([\w-]+):/;

export function upsertTaskLine(content, task, line) {
  const lines = content.split('\n');
  let replaced = false;
  const out = lines.map((l) => {
    const m = l.match(TASK_RE);
    if (m && String(m[1]) === String(task)) {
      replaced = true;
      return line;
    }
    return l;
  });
  if (!replaced) {
    while (out.length && out[out.length - 1] === '') out.pop();
    out.push(line, '');
  }
  return out.join('\n');
}

export function parseTaskRanges(content) {
  const ranges = [];
  for (const l of content.split('\n')) {
    const m = l.match(/^Task ([\w-]+): complete \(commits ([0-9a-f]+)\.\.([0-9a-f]+)/);
    if (m) {
      const t = /^\d+$/.test(m[1]) ? Number(m[1]) : m[1];
      ranges.push({ task: t, base: m[2], head: m[3] });
    }
  }
  return ranges;
}

function gitTop() {
  return execFileSync('git', ['rev-parse', '--show-toplevel'], {
    encoding: 'utf8',
  }).trim();
}

function parseFlags(argv) {
  const f = {};
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i];
    const value = argv[i + 1];
    if (typeof key !== 'string' || !key.startsWith('--')) {
      throw new Error(`expected flag, got: ${key ?? '(none)'}`);
    }
    if (typeof value !== 'string' || value.startsWith('--')) {
      throw new Error(`missing value for ${key}`);
    }
    f[key.replace(/^--/, '')] = value;
  }
  return f;
}

function requireFlag(flags, name) {
  const value = flags[name];
  if (typeof value !== 'string' || value.trim() === '') {
    throw new Error(`missing required flag: --${name}`);
  }
  return value;
}

function requireSha(flags, name) {
  const value = requireFlag(flags, name).toLowerCase();
  if (!/^[0-9a-f]{40}$/.test(value)) {
    throw new Error(`invalid --${name}: expected full 40-character git SHA`);
  }
  return value;
}

function parseHeader(content) {
  const ticket = /^- ticket:\s*(.*)$/m.exec(content)?.[1]?.trim() ?? '';
  const branch = /^- branch:\s*(.*)$/m.exec(content)?.[1]?.trim() ?? '';
  const base = /^- run base:\s*(.*)$/m.exec(content)?.[1]?.trim() ?? '';
  return { ticket, branch, base };
}

function isPlaceholder(value) {
  return value === '' || value === '?' || value === 'pending';
}

function replaceHeader(content, header) {
  const marker = '## Tasks';
  const markerIndex = content.indexOf(marker);
  if (markerIndex === -1) return renderHeader(header);
  const afterMarker = content.slice(markerIndex + marker.length).replace(/^\r?\n*/, '');
  return `${renderHeader(header)}${afterMarker}`;
}

function reconcileHeader(content, requested) {
  const current = parseHeader(content);
  const next = { ...current };
  for (const key of ['ticket', 'branch', 'base']) {
    const value = requested[key];
    if (!value) continue;
    if (isPlaceholder(current[key])) {
      next[key] = value;
    } else if (current[key] !== value) {
      throw new Error(`ledger header mismatch: ${key} is ${current[key]}, got ${value}`);
    }
  }
  return replaceHeader(content, next);
}

function writeFileAtomic(file, content) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const tmp = path.join(path.dirname(file), `.${path.basename(file)}.${process.pid}.tmp`);
  fs.writeFileSync(tmp, content);
  fs.renameSync(tmp, file);
}

function validateRecordFlags(flags) {
  requireFlag(flags, 'task');
  const base = requireSha(flags, 'base');
  const status = requireFlag(flags, 'status');
  if (!['complete', 'in-progress'].includes(status)) {
    throw new Error(`invalid --status: ${status}`);
  }
  const out = { ...flags, base, status };
  if (status === 'complete') {
    out.head = requireSha(flags, 'head');
  }
  return out;
}

function main(argv) {
  const [cmd, ...rest] = argv;
  const file = ledgerPathFor(gitTop());

  if (cmd === 'path') {
    process.stdout.write(file + '\n');
    return;
  }
  if (cmd === 'show') {
    process.stdout.write(fs.existsSync(file) ? fs.readFileSync(file, 'utf8') : '');
    return;
  }
  if (cmd === 'init') {
    const f = parseFlags(rest);
    const header = {
      ticket: requireFlag(f, 'ticket'),
      branch: requireFlag(f, 'branch'),
      base: requireSha(f, 'base'),
    };
    if (fs.existsSync(file)) {
      if (header.ticket === 'pending') {
        process.stdout.write('RESUME\n');
        return;
      }
      const content = fs.readFileSync(file, 'utf8');
      writeFileAtomic(file, reconcileHeader(content, header));
      process.stdout.write('RESUME\n');
      return;
    }
    writeFileAtomic(file, renderHeader(header));
    process.stdout.write('INIT\n');
    return;
  }
  if (cmd === 'record') {
    const f = validateRecordFlags(parseFlags(rest));
    const content = fs.existsSync(file)
      ? fs.readFileSync(file, 'utf8')
      : renderHeader({ ticket: f.ticket || '?', branch: f.branch || '?', base: f.base || '?' });
    const line = renderTaskLine(f);
    writeFileAtomic(file, upsertTaskLine(content, f.task, line));
    process.stdout.write('RECORDED\n');
    return;
  }

  process.stderr.write('usage: sdd-ledger.mjs path|show|init|record\n');
  process.exit(2);
}

const invoked = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (invoked) {
  try {
    main(process.argv.slice(2));
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 1;
  }
}
