import { describe, expect, it } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { extname, join, relative, resolve } from 'node:path';

const repoRoot = resolve(__dirname, '../../..');
const copyRoots = ['services/marketing', 'services/docs'];
const textExtensions = new Set(['.astro', '.md', '.mdx', '.ts', '.tsx']);
const ignoredDirs = new Set(['.astro', '.docusaurus', 'build', 'dist', 'node_modules']);

function copyFiles(dir: string): string[] {
  const entries = readdirSync(dir);
  const files: string[] = [];

  for (const entry of entries) {
    const path = join(dir, entry);
    const stat = statSync(path);

    if (stat.isDirectory()) {
      if (!ignoredDirs.has(entry)) {
        files.push(...copyFiles(path));
      }
      continue;
    }

    if (stat.isFile() && textExtensions.has(extname(entry)) && !entry.endsWith('.test.ts')) {
      files.push(path);
    }
  }

  return files;
}

function allCopy(): string {
  return copyRoots
    .flatMap((root) => copyFiles(join(repoRoot, root)))
    .map((file) => `\n# ${relative(repoRoot, file)}\n${readFileSync(file, 'utf8')}`)
    .join('\n');
}

function repoFile(path: string): string {
  return readFileSync(join(repoRoot, path), 'utf8');
}

describe('Codex first-class website and docs copy', () => {
  it('does not describe Codex as a future or coming-soon runner', () => {
    expect(allCopy()).not.toMatch(/Codex[^\n]{0,80}(coming soon|future|roadmap)/i);
  });

  it('describes remote web control as supporting Claude Code and Codex', () => {
    expect(allCopy()).toContain('Control remote Claude Code and Codex sessions from the web.');
    expect(allCopy()).not.toContain('Control remote Claude Code sessions from the web.');
    expect(allCopy()).not.toContain('control remote Claude Code sessions from anywhere');
  });

  it('keeps curl installer prerequisites aligned with Codex-only setups', () => {
    const installScript = repoFile('infra/install.sh');
    const marketingInstall = repoFile(
      'services/marketing/src/components/install/SystemRequirements.astro',
    );

    expect(repoFile('services/docs/docs/install.md')).toContain(
      'checks for a supported coding-agent CLI (`claude` or `codex`), GitHub',
    );
    expect(marketingInstall).toContain('One coding-agent CLI installed');
    expect(marketingInstall).not.toContain('installed for Codex sessions');
    expect(installScript).toContain('command -v claude');
    expect(installScript).toContain('command -v codex');
    expect(installScript).not.toContain('Claude Code CLI is required but not installed.');
  });

  it('documents Bossanova Codex plugin permission settings', () => {
    const securityDocs = repoFile('services/docs/docs/reference/security-and-permissions.md');
    const troubleshootingDocs = repoFile('services/docs/docs/help/troubleshooting.md');

    expect(securityDocs).toContain('plugins[codex].config.sandbox');
    expect(securityDocs).toContain('plugins[codex].config.approval');
    expect(securityDocs).toContain('dangerously_bypass_approvals_and_sandbox');
    expect(securityDocs).toContain('BOSS_PLUGIN_*');
    expect(troubleshootingDocs).toContain('plugins[codex].config.sandbox');
    expect(troubleshootingDocs).toContain('plugins[codex].config.approval');
    expect(troubleshootingDocs).not.toContain(
      'For Codex sessions, configure approvals and sandboxing in the Codex CLI.',
    );
  });
});
