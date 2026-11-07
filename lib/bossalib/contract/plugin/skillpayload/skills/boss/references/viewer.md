<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Viewer Devices

### `boss viewer`

Manage local viewer device approvals

### `boss viewer approve <id> [flags]`

Approve a viewer using its browser fingerprint

**Flags:**

- `--fingerprint` — Fingerprint displayed by the viewer browser (required)

### `boss viewer list [flags]`

List viewer devices and local approvals

**Flags:**

- `--json` — Emit a stable JSON schema instead of a table

### `boss viewer revoke <id>`

Revoke a local viewer approval

### `boss viewer status`

Show the daemon fingerprint
