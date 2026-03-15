// --- Argument parsing ---

export interface ParsedArgs {
  command: string;
  subcommand?: string;
  positional: string[];
  flags: Record<string, string | boolean>;
}

export function parseArgs(argv: string[]): ParsedArgs {
  // Skip node + script path
  const args = argv.slice(2);
  const command = args[0] ?? 'home';
  const positional: string[] = [];
  const flags: Record<string, string | boolean> = {};
  let subcommand: string | undefined;

  // Commands that take a subcommand
  const subcommandCommands = new Set(['repo']);

  for (let i = 1; i < args.length; i++) {
    const arg = args[i];
    if (arg.startsWith('--')) {
      const eqIdx = arg.indexOf('=');
      if (eqIdx !== -1) {
        flags[arg.slice(2, eqIdx)] = arg.slice(eqIdx + 1);
      } else {
        flags[arg.slice(2)] = true;
      }
    } else if (!subcommand && subcommandCommands.has(command) && i === 1) {
      subcommand = arg;
    } else {
      positional.push(arg);
    }
  }

  return { command, subcommand, positional, flags };
}

// --- Route definitions ---

export type Route =
  | { view: 'home' }
  | { view: 'new'; plan?: string }
  | { view: 'ls' }
  | { view: 'attach'; sessionId: string }
  | { view: 'session-action'; action: string; sessionId: string }
  | { view: 'repo-add' }
  | { view: 'repo-ls' }
  | { view: 'repo-remove'; repoId: string }
  | { view: 'help' }
  | { view: 'error'; message: string };

export function resolveRoute(parsed: ParsedArgs): Route {
  if (parsed.flags.help || parsed.flags.h) {
    return { view: 'help' };
  }

  switch (parsed.command) {
    case 'home':
      return { view: 'home' };
    case 'new':
      return { view: 'new', plan: parsed.positional[0] };
    case 'ls':
      return { view: 'ls' };
    case 'attach':
      if (!parsed.positional[0]) return { view: 'error', message: 'Usage: boss attach <session-id>' };
      return { view: 'attach', sessionId: parsed.positional[0] };
    case 'stop':
    case 'pause':
    case 'resume':
    case 'logs':
    case 'retry':
    case 'close':
    case 'rm':
      if (!parsed.positional[0]) return { view: 'error', message: `Usage: boss ${parsed.command} <session-id>` };
      return { view: 'session-action', action: parsed.command, sessionId: parsed.positional[0] };
    case 'repo':
      switch (parsed.subcommand) {
        case 'add':
          return { view: 'repo-add' };
        case 'ls':
          return { view: 'repo-ls' };
        case 'remove':
          if (!parsed.positional[0]) return { view: 'error', message: 'Usage: boss repo remove <repo-id>' };
          return { view: 'repo-remove', repoId: parsed.positional[0] };
        default:
          return { view: 'error', message: 'Usage: boss repo <add|ls|remove>' };
      }
    case 'help':
    case '--help':
    case '-h':
      return { view: 'help' };
    default:
      return { view: 'error', message: `Unknown command: ${parsed.command}` };
  }
}
