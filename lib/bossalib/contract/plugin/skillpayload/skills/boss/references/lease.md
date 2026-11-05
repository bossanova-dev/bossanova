<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Tracker Issue Leases

### `boss lease`

Acquire or release a tracker issue lease

### `boss lease acquire [flags]`

Acquire or renew a tracker issue lease

**Flags:**

- `--holder` — Holder token (32 hexadecimal characters) (required)
- `--issue` — Tracker-qualified issue key (for example linear:ENG-123) (required)
- `--json` — Emit the stable lease JSON schema
- `--purpose` — Lease purpose: plan or build (required)
- `--repo-origin` — Repository origin URL (default: git remote get-url origin)
- `--ttl` — Lease duration, sent as whole seconds (default: 2h0m0s)

### `boss lease release [flags]`

Release a tracker issue lease

**Flags:**

- `--holder` — Holder token (32 hexadecimal characters) (required)
- `--issue` — Tracker-qualified issue key (for example linear:ENG-123) (required)
- `--json` — Emit the stable lease JSON schema
- `--purpose` — Lease purpose: plan or build (required)
- `--repo-origin` — Repository origin URL (default: git remote get-url origin)
