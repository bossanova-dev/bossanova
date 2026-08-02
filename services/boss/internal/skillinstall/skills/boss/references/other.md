<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Other

### `boss upgrade [flags]`

Check for and install Bossanova upgrades

**Flags:**

- `--check` — check for an upgrade without installing
- `--no-restart` — do not restart the daemon after upgrade
- `--version` — install a specific stable release tag (prereleases are not supported)
- `--yes` — install without interactive confirmation

```bash
boss upgrade --check
boss upgrade --yes
boss upgrade --version v1.2.4 --yes
boss upgrade --yes --no-restart
```

### `boss version`

Print version information

```bash
boss version
```
