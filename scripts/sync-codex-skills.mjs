import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

export const GENERATED_HEADER =
  '<!-- Generated from .claude/skills by make codex-skills. Do not edit directly. -->';

const BODY_REWRITES = [
  [/~\/\.claude\/skills\/bossanova\//g, '~/.codex/skills/bossanova/'],
  [/\bCLAUDE\.md\b/g, 'AGENTS.md'],
  [/\bClaude Code\b/g, 'Codex'],
  [/\bClaude agents\b/g, 'Codex agents'],
  [/\bClaude agent\b/g, 'Codex agent'],
  [/\bClaude\b/g, 'Codex'],
  [/\bTodoWrite\b/g, 'update_plan'],
  [/\bRead tool\b/g, 'file-reading tool'],
  [/`Read`/g, '`file-reading tool`'],
  [/\bEdit tool\b/g, 'apply_patch'],
  [/`Edit`/g, '`apply_patch`'],
  [/\bWrite tool\b/g, 'apply_patch'],
  [/`Write`/g, '`apply_patch`'],
  [/\bBash tool\b/g, 'shell command tool'],
  [/`Bash`/g, '`shell`'],
  [/`Grep`/g, '`search`'],
  [/`Glob`/g, '`file search`'],
  [/\bWebFetch\b/g, 'web fetch'],
  [/\bPlaywright MCP server\b/g, 'Codex browser automation'],
  [/\bPlaywright MCP\b/g, 'Codex browser automation'],
  // Skill/command references: Claude `/name` -> Codex `$name` (same ref, different prefix).
  // The name must start with a letter (so numeric score denominators like /20 are ignored)
  // and forbids slashes, so multi-segment paths (e.g. /Users/dave/x, docs/plans/x) never
  // match. Code-span form: `/skill-name` wrapped in backticks (backtick-closed).
  [/`\/((?:[a-z][a-z0-9-]*:)?[a-z][a-z0-9-]*)`/g, '`$$$1`'],
  // Prose / bold / italic / paren / bracket form, at a word boundary with no inner slash.
  // The leading slash must sit at a command boundary (start, whitespace, or ([*_), and the
  // token must end at a boundary that is not another slash, so or-constructs (`gh`/network),
  // redirects (2>/dev/null), URLs (https://...), and key combos ([y/enter]) are left intact.
  [/(^|[\s(\[*_])\/((?:[a-z][a-z0-9-]*:)?[a-z][a-z0-9-]*)(?=[\s)\]*_.,;:!?]|$)/gm, '$1$$$2'],
];

function normalizePath(filePath) {
  return filePath.split(path.sep).join('/');
}

function splitFrontmatter(markdown, filePath) {
  const match = /^---\r?\n([\s\S]*?)\r?\n---(?:\r?\n)?/.exec(markdown);

  if (!match) {
    throw new Error(`${filePath}: missing frontmatter`);
  }

  return {
    body: markdown.slice(match[0].length),
    frontmatter: match[1],
  };
}

function parseFrontmatter(markdown, filePath) {
  const { frontmatter } = splitFrontmatter(markdown, filePath);
  const values = new Map();

  for (const line of frontmatter.split(/\r?\n/)) {
    const match = /^([A-Za-z0-9_-]+):\s*(.*)$/.exec(line);

    if (match) {
      values.set(match[1], match[2].replace(/^['"]|['"]$/g, '').trim());
    }
  }

  const name = values.get('name');
  const description = values.get('description');

  if (!name) {
    throw new Error(`${filePath}: missing required frontmatter field: name`);
  }

  if (!description) {
    throw new Error(`${filePath}: missing required frontmatter field: description`);
  }

  return { description, name };
}

function rewriteBody(body) {
  const rewritten = BODY_REWRITES.reduce((current, [pattern, replacement]) => {
    return current.replace(pattern, replacement);
  }, body);

  return rewritten
    .replace(/\bAGENTS\.md`, `AGENTS\.md\b/g, 'AGENTS.md`, `CLAUDE.md')
    .replace(
      /`apply_patch`\/`apply_patch` "modified since read"/g,
      '`write`/`apply_patch` "modified since read"',
    );
}

export function rewriteClaudeSkillMarkdown(markdown, filePath = 'SKILL.md') {
  const { body, frontmatter } = splitFrontmatter(markdown, filePath);
  const rewrittenBody = rewriteBody(body).replace(/^\s+/, '');

  return `---
${frontmatter}
---

${GENERATED_HEADER}

${rewrittenBody}`;
}

function findSkillEntryPath(skillPath) {
  const entries = new Set(fs.readdirSync(skillPath));

  for (const fileName of ['SKILL.md', 'skill.md']) {
    if (entries.has(fileName)) {
      return path.join(skillPath, fileName);
    }
  }

  throw new Error(`${skillPath}: missing SKILL.md`);
}

export function collectSkillSources(sourceRoot) {
  if (!fs.existsSync(sourceRoot)) {
    throw new Error(`Skill source root does not exist: ${sourceRoot}`);
  }

  const names = new Set();
  return fs
    .readdirSync(sourceRoot, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .filter((entry) => !entry.name.startsWith('_'))
    .map((entry) => {
      const sourcePath = path.join(sourceRoot, entry.name);
      const entryPath = findSkillEntryPath(sourcePath);
      const metadata = parseFrontmatter(fs.readFileSync(entryPath, 'utf8'), entryPath);

      if (names.has(metadata.name)) {
        throw new Error(`Duplicate skill name: ${metadata.name}`);
      }

      names.add(metadata.name);

      return {
        dirName: entry.name,
        entryPath,
        metadata,
        sourcePath,
      };
    })
    .sort((left, right) => left.dirName.localeCompare(right.dirName));
}

// Build inputs for constructed skills (see scripts/construct-skills.mjs): the
// generated SKILL.md is the runtime artifact, so the template and manifest are
// dead weight in the codex copy and would re-drift on every regeneration.
const CONSTRUCT_BUILD_INPUTS = new Set(['construct.json', 'SKILL.md.tmpl']);

function copyRecursive(sourcePath, destPath, skippedPath) {
  if (CONSTRUCT_BUILD_INPUTS.has(path.basename(sourcePath))) {
    return;
  }

  const stat = fs.statSync(sourcePath);

  if (path.resolve(sourcePath) === path.resolve(skippedPath)) {
    return;
  }

  if (stat.isDirectory()) {
    fs.mkdirSync(destPath, { recursive: true });
    fs.chmodSync(destPath, stat.mode & 0o777);

    for (const entry of fs.readdirSync(sourcePath)) {
      copyRecursive(path.join(sourcePath, entry), path.join(destPath, entry), skippedPath);
    }

    return;
  }

  if (!stat.isFile()) {
    return;
  }

  fs.mkdirSync(path.dirname(destPath), { recursive: true });
  fs.copyFileSync(sourcePath, destPath);
  fs.chmodSync(destPath, stat.mode & 0o777);
}

function writeGeneratedSkills(skills, destRoot) {
  fs.rmSync(destRoot, { force: true, recursive: true });
  fs.mkdirSync(destRoot, { recursive: true });

  for (const skill of skills) {
    const destSkillPath = path.join(destRoot, skill.dirName);

    copyRecursive(skill.sourcePath, destSkillPath, skill.entryPath);

    const sourceMarkdown = fs.readFileSync(skill.entryPath, 'utf8');
    const destEntryPath = path.join(destSkillPath, 'SKILL.md');

    fs.writeFileSync(destEntryPath, rewriteClaudeSkillMarkdown(sourceMarkdown, skill.entryPath));
    fs.chmodSync(destEntryPath, fs.statSync(skill.entryPath).mode & 0o777);
  }
}

function listFiles(root) {
  const files = new Map();

  if (!fs.existsSync(root)) {
    return files;
  }

  function walk(currentPath) {
    for (const entry of fs.readdirSync(currentPath, { withFileTypes: true })) {
      const entryPath = path.join(currentPath, entry.name);

      if (entry.isDirectory()) {
        walk(entryPath);
      } else if (entry.isFile()) {
        files.set(normalizePath(path.relative(root, entryPath)), entryPath);
      }
    }
  }

  walk(root);

  return files;
}

export function compareDirectories(expectedRoot, actualRoot) {
  const expectedFiles = listFiles(expectedRoot);
  const actualFiles = listFiles(actualRoot);
  const differences = [];

  for (const [relativePath, expectedPath] of expectedFiles) {
    const actualPath = actualFiles.get(relativePath);

    if (!actualPath) {
      differences.push(`missing: ${relativePath}`);
      continue;
    }

    if (!fs.readFileSync(expectedPath).equals(fs.readFileSync(actualPath))) {
      differences.push(`content mismatch: ${relativePath}`);
    }

    const expectedExecutable = fs.statSync(expectedPath).mode & 0o111;
    const actualExecutable = fs.statSync(actualPath).mode & 0o111;

    if (expectedExecutable !== actualExecutable) {
      differences.push(`mode mismatch: ${relativePath}`);
    }
  }

  for (const relativePath of actualFiles.keys()) {
    if (!expectedFiles.has(relativePath)) {
      differences.push(`extra: ${relativePath}`);
    }
  }

  return differences.sort();
}

export function syncCodexSkills(options) {
  if (!fs.existsSync(options.sourceRoot)) {
    if (options.check) {
      return {
        changed: false,
        differences: [],
        reason: `Skill source root does not exist: ${options.sourceRoot}`,
        skipped: true,
        skills: [],
      };
    }

    throw new Error(`Skill source root does not exist: ${options.sourceRoot}`);
  }

  const skills = collectSkillSources(options.sourceRoot);

  if (!options.check) {
    writeGeneratedSkills(skills, options.destRoot);

    return {
      changed: true,
      differences: [],
      skills,
    };
  }

  const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'codex-skills-check-'));
  const tempDestRoot = path.join(tempRoot, 'skills');

  try {
    writeGeneratedSkills(skills, tempDestRoot);

    const differences = compareDirectories(tempDestRoot, options.destRoot);

    return {
      changed: differences.length > 0,
      differences,
      skills,
    };
  } finally {
    fs.rmSync(tempRoot, { force: true, recursive: true });
  }
}

function findRepoRoot(startPath) {
  let currentPath = path.resolve(startPath);
  let previousPath = '';

  while (currentPath !== previousPath) {
    if (
      fs.existsSync(path.join(currentPath, 'Makefile')) &&
      fs.existsSync(path.join(currentPath, '.claude', 'skills'))
    ) {
      return currentPath;
    }

    previousPath = currentPath;
    currentPath = path.dirname(currentPath);
  }

  throw new Error(`Could not find repo root from ${startPath}`);
}

function parseArgs(args) {
  let check = false;
  let root;

  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index];

    if (arg === '--check') {
      check = true;
    } else if (arg === '--root') {
      const value = args[index + 1];

      if (!value) {
        throw new Error('--root requires a value');
      }

      root = path.resolve(value);
      index += 1;
    } else {
      throw new Error(`Unknown argument: ${arg}`);
    }
  }

  return { check, root: root ?? findRepoRoot(process.cwd()) };
}

function runCli() {
  const { check, root } = parseArgs(process.argv.slice(2));
  const result = syncCodexSkills({
    check,
    destRoot: path.join(root, '.codex', 'skills'),
    sourceRoot: path.join(root, '.claude', 'skills'),
  });

  if (check && result.changed) {
    console.error('.codex/skills is stale. Run `make codex-skills`.');
    console.error(result.differences.slice(0, 25).join('\n'));

    if (result.differences.length > 25) {
      console.error(`...and ${String(result.differences.length - 25)} more differences`);
    }

    process.exitCode = 1;
    return;
  }

  if (result.skipped) {
    console.log(`Skipped Codex skills check: ${result.reason}`);
    return;
  }

  const action = check ? 'Verified' : 'Synced';
  console.log(`${action} ${String(result.skills.length)} Codex skills`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  runCli();
}
