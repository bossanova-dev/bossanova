import { describe, expect, it } from 'vitest';
import { parseArgs, resolveRoute } from '../router.js';

// Helper: simulates process.argv with node + script prefix
function argv(...args: string[]) {
  return ['node', 'cli.tsx', ...args];
}

describe('parseArgs', () => {
  it('defaults to "home" with no arguments', () => {
    const result = parseArgs(argv());
    expect(result.command).toBe('home');
    expect(result.positional).toEqual([]);
  });

  it('parses a simple command', () => {
    const result = parseArgs(argv('ls'));
    expect(result.command).toBe('ls');
  });

  it('parses command with positional arguments', () => {
    const result = parseArgs(argv('attach', 'abc123'));
    expect(result.command).toBe('attach');
    expect(result.positional).toEqual(['abc123']);
  });

  it('parses --flag as boolean', () => {
    const result = parseArgs(argv('ls', '--help'));
    expect(result.flags.help).toBe(true);
  });

  it('parses --key=value flags', () => {
    const result = parseArgs(argv('ls', '--format=json'));
    expect(result.flags.format).toBe('json');
  });

  it('parses repo subcommand', () => {
    const result = parseArgs(argv('repo', 'add'));
    expect(result.command).toBe('repo');
    expect(result.subcommand).toBe('add');
  });

  it('parses repo subcommand with positional', () => {
    const result = parseArgs(argv('repo', 'remove', 'repo-123'));
    expect(result.command).toBe('repo');
    expect(result.subcommand).toBe('remove');
    expect(result.positional).toEqual(['repo-123']);
  });

  it('parses new command with plan text', () => {
    // Shell passes quoted string as a single argument
    const result = parseArgs(argv('new', 'fix the login bug'));
    expect(result.command).toBe('new');
    expect(result.positional).toEqual(['fix the login bug']);
  });
});

describe('resolveRoute', () => {
  it('routes to home with no args', () => {
    const route = resolveRoute(parseArgs(argv()));
    expect(route).toEqual({ view: 'home' });
  });

  it('routes to help with --help flag', () => {
    const route = resolveRoute(parseArgs(argv('ls', '--help')));
    expect(route).toEqual({ view: 'help' });
  });

  it('routes to help command', () => {
    const route = resolveRoute(parseArgs(argv('help')));
    expect(route).toEqual({ view: 'help' });
  });

  it('routes to new with plan', () => {
    const route = resolveRoute(parseArgs(argv('new', 'fix the bug')));
    expect(route).toEqual({ view: 'new', plan: 'fix the bug' });
  });

  it('routes to new without plan', () => {
    const route = resolveRoute(parseArgs(argv('new')));
    expect(route).toEqual({ view: 'new', plan: undefined });
  });

  it('routes to ls', () => {
    const route = resolveRoute(parseArgs(argv('ls')));
    expect(route).toEqual({ view: 'ls' });
  });

  it('routes to attach with id', () => {
    const route = resolveRoute(parseArgs(argv('attach', 'abc')));
    expect(route).toEqual({ view: 'attach', sessionId: 'abc' });
  });

  it('returns error for attach without id', () => {
    const route = resolveRoute(parseArgs(argv('attach')));
    expect(route.view).toBe('error');
  });

  it('routes session actions (stop, pause, resume, logs, retry, close, rm)', () => {
    for (const action of ['stop', 'pause', 'resume', 'logs', 'retry', 'close', 'rm']) {
      const route = resolveRoute(parseArgs(argv(action, 's-1')));
      expect(route).toEqual({ view: 'session-action', action, sessionId: 's-1' });
    }
  });

  it('returns error for session actions without id', () => {
    const route = resolveRoute(parseArgs(argv('stop')));
    expect(route.view).toBe('error');
  });

  it('routes repo add', () => {
    const route = resolveRoute(parseArgs(argv('repo', 'add')));
    expect(route).toEqual({ view: 'repo-add' });
  });

  it('routes repo ls', () => {
    const route = resolveRoute(parseArgs(argv('repo', 'ls')));
    expect(route).toEqual({ view: 'repo-ls' });
  });

  it('routes repo remove with id', () => {
    const route = resolveRoute(parseArgs(argv('repo', 'remove', 'r-1')));
    expect(route).toEqual({ view: 'repo-remove', repoId: 'r-1' });
  });

  it('returns error for repo remove without id', () => {
    const route = resolveRoute(parseArgs(argv('repo', 'remove')));
    expect(route.view).toBe('error');
  });

  it('returns error for repo without subcommand', () => {
    const route = resolveRoute(parseArgs(argv('repo')));
    expect(route.view).toBe('error');
  });

  it('returns error for unknown command', () => {
    const route = resolveRoute(parseArgs(argv('bogus')));
    expect(route).toEqual({ view: 'error', message: 'Unknown command: bogus' });
  });
});
