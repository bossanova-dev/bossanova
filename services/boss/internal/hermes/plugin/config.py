"""Configuration resolution for the bossanova Hermes plugin.

Every value resolves in the same order:

1. a non-empty value from the Hermes settings form (``ctx.get_config(key)``,
   stored under ``plugins.entries.bossanova.settings``);
2. the value ``boss hermes install`` rendered into ``boss_install.json`` next
   to this file;
3. a built-in default, or a ``ConfigError`` telling the user to run
   ``boss hermes install`` when the value has no sensible default.

Standard library only; nothing here spawns a process or opens a socket.
"""

import json
import os
import shutil

INSTALL_FILE = "boss_install.json"

KEY_MCP_BIN = "mcp_bin"
KEY_SOCKET_PATH = "socket_path"
KEY_SETTINGS_PATH = "settings_path"
KEY_TOOL_TIMEOUT = "tool_timeout_seconds"

KEYS = (KEY_MCP_BIN, KEY_SOCKET_PATH, KEY_SETTINGS_PATH, KEY_TOOL_TIMEOUT)

DEFAULT_TOOL_TIMEOUT_SECONDS = 120.0

INSTALL_HINT = "run `boss hermes install` to (re)write the plugin's install config"


class ConfigError(Exception):
    """A required value resolved from neither the settings form nor the install file."""


class Resolved(object):
    """The effective plugin configuration.

    ``settings_path`` is empty when bossanova's OS-default settings file should
    be used; the MCP process then gets no ``BOSS_SETTINGS_PATH`` at all.
    """

    __slots__ = ("mcp_bin", "socket_path", "settings_path", "tool_timeout_seconds")

    def __init__(self, mcp_bin, socket_path, settings_path, tool_timeout_seconds):
        self.mcp_bin = mcp_bin
        self.socket_path = socket_path
        self.settings_path = settings_path
        self.tool_timeout_seconds = tool_timeout_seconds

    def __repr__(self):
        return "Resolved(mcp_bin=%r, socket_path=%r, settings_path=%r, tool_timeout_seconds=%r)" % (
            self.mcp_bin,
            self.socket_path,
            self.settings_path,
            self.tool_timeout_seconds,
        )


def load_install(plugin_dir):
    """Return the rendered install config as a dict, or {} when it is absent.

    A malformed file reads as {} too: the caller then reports the missing
    value with the install hint, which is the remedy for a corrupt file as
    well as a missing one.
    """
    path = os.path.join(plugin_dir, INSTALL_FILE)
    try:
        with open(path, "r", encoding="utf-8") as handle:
            data = json.load(handle)
    except (OSError, ValueError):
        return {}
    if not isinstance(data, dict):
        return {}
    return data


def _non_empty(value):
    if value is None:
        return None
    if isinstance(value, str):
        value = value.strip()
        return value or None
    return value


def _from_ctx(ctx, key):
    getter = getattr(ctx, "get_config", None)
    if getter is None:
        return None
    try:
        return _non_empty(getter(key, default=None))
    except Exception:  # a broken settings store must not break a tool call
        return None


def lookup(ctx, install, key):
    """Return the first non-empty value for key: settings form, then install file."""
    value = _from_ctx(ctx, key)
    if value is not None:
        return value
    return _non_empty(install.get(key))


def _timeout(value):
    if value is None or isinstance(value, bool):
        return DEFAULT_TOOL_TIMEOUT_SECONDS
    try:
        seconds = float(value)
    except (TypeError, ValueError):
        return DEFAULT_TOOL_TIMEOUT_SECONDS
    if seconds <= 0:
        return DEFAULT_TOOL_TIMEOUT_SECONDS
    return seconds


def resolve(ctx, plugin_dir):
    """Resolve the effective configuration, raising ConfigError when incomplete."""
    install = load_install(plugin_dir)
    mcp_bin = lookup(ctx, install, KEY_MCP_BIN)
    if mcp_bin is None:
        raise ConfigError("no bossanova MCP binary is configured (%s): %s" % (KEY_MCP_BIN, INSTALL_HINT))
    socket_path = lookup(ctx, install, KEY_SOCKET_PATH)
    if socket_path is None:
        raise ConfigError("no bossd socket path is configured (%s): %s" % (KEY_SOCKET_PATH, INSTALL_HINT))
    settings_path = lookup(ctx, install, KEY_SETTINGS_PATH) or ""
    timeout = _timeout(lookup(ctx, install, KEY_TOOL_TIMEOUT))
    return Resolved(str(mcp_bin), str(socket_path), str(settings_path), timeout)


def resolve_binary(mcp_bin):
    """Return the absolute path of an executable MCP binary, or None.

    A bare command name is looked up on PATH; a path must name an executable
    regular file. This is file-system inspection only, never execution.
    """
    if not mcp_bin:
        return None
    if os.path.dirname(mcp_bin) == "":
        return shutil.which(mcp_bin)
    path = os.path.abspath(os.path.expanduser(mcp_bin))
    if os.path.isfile(path) and os.access(path, os.X_OK):
        return path
    return None
