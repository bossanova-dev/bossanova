<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## MCP Server

### `boss mcp`

Manage the local MCP server

Manages the local MCP server, which exposes the boss operations as MCP tools over Streamable HTTP for MCP-aware hosts. It runs as an auto-starting service (launchd on macOS, systemd on Linux) and proxies through the local bossd daemon's Unix socket.

### `boss mcp install [flags]`

Install the MCP server as an auto-starting service

Installs the MCP server as an auto-starting service and starts it. Use `--force` to overwrite an existing service file, and `--port` to change the loopback port (default 8765). The server listens on `http://127.0.0.1:<port>/mcp`.

**Flags:**

- `--force` — Overwrite existing service file
- `--port` — Loopback port for the MCP HTTP server (default: 8765)

```bash
boss mcp install
boss mcp install --force
boss mcp install --port 8888
```

### `boss mcp start`

Start the MCP server

```bash
boss mcp start
```

### `boss mcp status`

Show MCP server status

Reports the MCP service state (installed/running) plus the instances: inventory of every boss-mcp process owned by the current user (service, stray HTTP, session-owned, and orphaned counts).

```bash
boss mcp status
```

### `boss mcp stop`

Stop the MCP server

Stops the managed MCP service (leaving its service file in place) and also terminates stray `--http` daemons and orphaned session MCP servers owned by the current user, while leaving live session-owned servers running since each exits with its own chat. Idempotent.

```bash
boss mcp stop
```

### `boss mcp uninstall`

Uninstall the MCP server service

```bash
boss mcp uninstall
```
