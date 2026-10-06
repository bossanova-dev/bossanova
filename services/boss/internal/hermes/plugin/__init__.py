"""bossanova: every bossanova MCP tool as a native Hermes tool.

``register(ctx)`` reads ``tools.json`` (rendered by ``boss hermes install``
from the installed ``boss`` binary) and registers each tool as
``bossanova_<name>`` in the ``bossanova`` toolset, plus the ``/bossanova``
command and the ``orchestrator`` skill. Registration does no subprocess or
network I/O; the MCP server process starts on the first tool call and every
call is proxied to it over stdio.

Standard library only.
"""

import json
import os
import pathlib

from . import config as _config
from . import mcp_client as _mcp_client
from . import proxy as _proxy

PLUGIN_NAME = "bossanova"
TOOLSET = "bossanova"
TOOL_PREFIX = "bossanova_"
COMMAND_NAME = "bossanova"
SKILL_NAME = "orchestrator"
TOOLS_FILE = "tools.json"
MANIFEST_FILE = "plugin.yaml"

PLUGIN_DIR = os.path.dirname(os.path.abspath(__file__))
# Hermes calls path.exists() on the skill path, so it must be a Path, not a str.
SKILL_PATH = pathlib.Path(PLUGIN_DIR, "skills", SKILL_NAME, "SKILL.md")

_EMPTY_SCHEMA = {"type": "object", "properties": {}}


def load_tools(plugin_dir=PLUGIN_DIR):
    """Return the rendered tool definitions: a list of {name, description, inputSchema}."""
    path = os.path.join(plugin_dir, TOOLS_FILE)
    with open(path, "r", encoding="utf-8") as handle:
        tools = json.load(handle)
    if not isinstance(tools, list):
        raise ValueError("%s must hold a JSON array" % path)
    return [tool for tool in tools if isinstance(tool, dict) and tool.get("name")]


def plugin_version(plugin_dir=PLUGIN_DIR):
    """Return the version recorded in plugin.yaml, or "unknown".

    The renderer writes plugin.yaml as JSON (a YAML subset), so the standard
    library can read it without a YAML dependency.
    """
    try:
        with open(os.path.join(plugin_dir, MANIFEST_FILE), "r", encoding="utf-8") as handle:
            manifest = json.load(handle)
    except (OSError, ValueError):
        return "unknown"
    if isinstance(manifest, dict) and manifest.get("version"):
        return str(manifest["version"])
    return "unknown"


class Plugin(object):
    """The registered plugin: config resolution and the MCP call path."""

    def __init__(self, ctx, plugin_dir=PLUGIN_DIR, client_factory=None):
        self.ctx = ctx
        self.plugin_dir = plugin_dir
        self._client_factory = client_factory or _mcp_client.shared_client

    def resolve(self):
        return _config.resolve(self.ctx, self.plugin_dir)

    def launch_spec(self, resolved):
        binary = _config.resolve_binary(resolved.mcp_bin)
        if binary is None:
            raise _mcp_client.McpError(
                "bossanova MCP binary %r is not an executable file: %s" % (resolved.mcp_bin, _config.INSTALL_HINT)
            )
        return _mcp_client.LaunchSpec(binary, resolved.socket_path, resolved.settings_path)

    def call(self, tool_name, arguments):
        resolved = self.resolve()
        spec = self.launch_spec(resolved)
        return self._client_factory().call_tool(spec, tool_name, arguments, resolved.tool_timeout_seconds)

    def available(self, *args, **kwargs):
        """check_fn: hide the tools when no MCP binary resolves. Never raises."""
        try:
            return _config.resolve_binary(self.resolve().mcp_bin) is not None
        except Exception:
            return False

    # -- /bossanova ---------------------------------------------------------

    def command(self, raw_args=""):
        """Handle `/bossanova [status|help]`. Never raises."""
        try:
            words = (raw_args or "").split()
            sub = words[0].lower() if words else "status"
            if sub == "help":
                return self.help_text()
            if sub == "status":
                return self.status_text()
            return "Unknown subcommand %r.\n\n%s" % (sub, self.help_text())
        except Exception as exc:
            return "bossanova: %s" % (str(exc) or type(exc).__name__)

    def help_text(self):
        return "\n".join(
            [
                "/bossanova status  show the MCP binary, socket, settings file, plugin version and live session statuses",
                "/bossanova help    show this help",
                "",
                "Every bossanova MCP tool is available as a `%s<name>` tool in the `%s` toolset." % (TOOL_PREFIX, TOOLSET),
                'For the operating guide (sessions, chats, cron, broadcasts, the confirm rule), load skill_view("%s:%s").'
                % (PLUGIN_NAME, SKILL_NAME),
            ]
        )

    def status_text(self):
        lines = ["bossanova plugin %s" % plugin_version(self.plugin_dir)]
        try:
            resolved = self.resolve()
        except _config.ConfigError as exc:
            lines.append("config: %s" % exc)
            return "\n".join(lines)
        binary = _config.resolve_binary(resolved.mcp_bin)
        lines.append("mcp binary:    %s%s" % (resolved.mcp_bin, "" if binary else "  (NOT FOUND)"))
        lines.append("bossd socket:  %s" % resolved.socket_path)
        lines.append("settings file: %s" % (resolved.settings_path or "(OS default)"))
        lines.append("tool timeout:  %gs" % resolved.tool_timeout_seconds)
        if binary is None:
            lines.append("sessions: unavailable (%s)" % _config.INSTALL_HINT)
            return "\n".join(lines)
        lines.append("sessions: %s" % self._session_summary())
        return "\n".join(lines)

    def _session_summary(self):
        try:
            sessions = _structured(self.call("list_sessions", {}))
            ids = [s.get("id") for s in sessions or [] if isinstance(s, dict) and s.get("id")]
            if not ids:
                return "none"
            statuses = _structured(self.call("get_session_statuses", {"session_ids": ids}))
        except Exception as exc:
            return "unavailable (%s)" % (str(exc) or type(exc).__name__)
        counts = {}
        for entry in statuses or []:
            if isinstance(entry, dict):
                label = _status_label(entry.get("status"))
                counts[label] = counts.get(label, 0) + 1
        breakdown = ", ".join("%d %s" % (n, label) for label, n in sorted(counts.items()))
        return "%d (%s)" % (len(ids), breakdown or "no status reported")


# ChatStatus enum values, as the MCP server's JSON carries them (numbers).
_STATUS_LABELS = {
    0: "unknown",
    1: "working",
    2: "idle",
    3: "stopped",
    4: "question",
    5: "limited",
    6: "waiting",
}


def _status_label(value):
    if value is None:
        return "unknown"
    if isinstance(value, str):
        return value.lower().replace("chat_status_", "")
    return _STATUS_LABELS.get(value, str(value))


def _structured(result):
    """Decode a CallToolResult into data, raising on an error result."""
    decoded = json.loads(_proxy.result_to_json(result))
    if "error" in decoded:
        raise _mcp_client.McpError(decoded["error"])
    return decoded.get("result")


def register(ctx):
    """Hermes entry point. Reads tools.json; starts no process and opens no socket."""
    plugin = Plugin(ctx)
    for tool in load_tools(plugin.plugin_dir):
        name = str(tool["name"])
        schema = {
            "name": TOOL_PREFIX + name,
            "description": tool.get("description") or "",
            "parameters": tool.get("inputSchema") or dict(_EMPTY_SCHEMA),
        }
        ctx.register_tool(
            name=TOOL_PREFIX + name,
            toolset=TOOLSET,
            schema=schema,
            handler=_proxy.make_handler(name, plugin.call),
            check_fn=plugin.available,
        )
    ctx.register_command(
        COMMAND_NAME,
        plugin.command,
        description="bossanova status and help",
        args_hint="[status|help]",
    )
    ctx.register_skill(SKILL_NAME, SKILL_PATH)
    return plugin
