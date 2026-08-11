<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Plugins

### `boss agents [flags]`

List the agent runners the daemon loaded

Lists the agent runners the daemon loaded — the plugins that satisfy AgentRunnerService and can therefore back a session. This is narrower than `boss plugin list`, which reports every loaded plugin including task sources (linear, sentry) and automation reactors (dependabot, repair). Use this to check that the agent you are about to pass to `boss new --agent` is actually available; without a loaded agent runner the daemon stays healthy but session creation fails.

The default table shows NAME, VERSION and a SETTINGS count. Use --json for the full shape: each agent carries name, version and user_settings, and each setting carries key, label, description, default_value, type and allowed_values. `type` is the enum name (BOOL, STRING, ENUM, UNSPECIFIED). user_settings, allowed_values and the top-level agents list are always arrays, empty rather than null, so a driver can iterate them without a null check. Zero loaded agents is a valid answer: {"agents": []} with exit 0. A failure to reach the daemon exits 1 with the shared {error:{code, connect_code, message}} envelope.

Under --remote the answer is aggregated across every Ready daemon the orchestrator knows about, and the aggregate carries no per-daemon provenance — an agent in the list is loaded by at least one daemon, not necessarily by the one that will run your session. The aggregate is a plain concatenation in daemon order, so name is NOT unique in it: two Ready daemons that both load claude yield two claude rows and two JSON objects sharing that name. A driver keying by name silently collapses them, and one that counts gets the daemon count rather than the agent count — deduplicate client-side if you need a set. The CLI deliberately does not, because that would hide fleet composition and diverge from `boss plugin list` over the same aggregation. --host is not affected: it tunnels to a single daemon and reports only that daemon's runners.

**Flags:**

- `--json` — Emit machine-readable JSON, including each agent's user settings

```bash
boss agents
# Machine-readable, including each agent's user settings
boss agents --json
```

### `boss plugin`

Inspect installed plugins

### `boss plugin list`

Alias: `boss plugin ls`

List plugins the daemon attempted to load this run

```bash
boss plugin list
boss plugin ls
```
