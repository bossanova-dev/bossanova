"""Shared fixtures: a rendered plugin package in a temp dir, a fake Hermes ctx,
and an executable wrapper around the fake MCP server."""

import importlib.util
import itertools
import json
import os
import shutil
import stat
import sys
import tempfile

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))
PLUGIN_SRC = os.path.dirname(TESTS_DIR)
FAKE_SERVER = os.path.join(TESTS_DIR, "fake_mcp_server.py")

TOOLS = [
    {
        "name": "list_sessions",
        "description": "List bossanova sessions.",
        "inputSchema": {"type": "object", "properties": {"repo_id": {"type": "string"}}},
    },
    {
        "name": "get_session_statuses",
        "description": "Best status across a session's chats.",
        "inputSchema": {
            "type": "object",
            "properties": {"session_ids": {"type": "array", "items": {"type": "string"}}},
            "required": ["session_ids"],
        },
    },
    {"name": "echo", "description": "Echo the arguments.", "inputSchema": {"type": "object"}},
    {"name": "fail", "description": "Always fails.", "inputSchema": {"type": "object"}},
    {"name": "crash", "description": "Kills the server.", "inputSchema": {"type": "object"}},
    {"name": "hang", "description": "Never answers.", "inputSchema": {"type": "object"}},
    {"name": "env", "description": "Reports the server environment.", "inputSchema": {"type": "object"}},
]

_counter = itertools.count()


class FakeCtx(object):
    """Records every registration; get_config reads a plain dict."""

    def __init__(self, settings=None):
        self.settings = dict(settings or {})
        self.tools = []
        self.commands = []
        self.skills = []

    def register_tool(self, name, toolset, schema, handler, check_fn=None, **kwargs):
        self.tools.append(
            {"name": name, "toolset": toolset, "schema": schema, "handler": handler, "check_fn": check_fn}
        )

    def register_command(self, name, handler, description="", args_hint="", **kwargs):
        self.commands.append({"name": name, "handler": handler, "description": description, "args_hint": args_hint})

    def register_skill(self, name, path, description="", frontmatter=None):
        # Mirror Hermes's PluginContext.register_skill, which calls path.exists()
        # and so rejects a plain str path with AttributeError.
        if not path.exists():
            raise FileNotFoundError(path)
        self.skills.append({"name": name, "path": path})

    def get_config(self, key, default=None):
        return self.settings.get(key, default)

    def tool(self, name):
        for tool in self.tools:
            if tool["name"] == name:
                return tool
        raise KeyError(name)


def write_fake_binary(directory):
    """Write an executable that execs the fake server with this interpreter."""
    path = os.path.join(directory, "fake-boss-mcp")
    with open(path, "w", encoding="utf-8") as handle:
        handle.write('#!/bin/sh\nexec "%s" "%s" "$@"\n' % (sys.executable, FAKE_SERVER))
    os.chmod(path, os.stat(path).st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    return path


class RenderedPlugin(object):
    """A plugin package laid out the way the Go renderer writes it, loaded
    under a unique module name so tests never share module state."""

    def __init__(self, install=None, tools=None, version="1.2.3"):
        self.root = tempfile.mkdtemp(prefix="hermes-bossanova-")
        self.dir = os.path.join(self.root, "bossanova")
        os.makedirs(os.path.join(self.dir, "skills", "orchestrator"))
        for name in os.listdir(PLUGIN_SRC):
            if name.endswith(".py"):
                shutil.copy(os.path.join(PLUGIN_SRC, name), os.path.join(self.dir, name))
        shutil.copy(
            os.path.join(PLUGIN_SRC, "skills", "orchestrator", "SKILL.md"),
            os.path.join(self.dir, "skills", "orchestrator", "SKILL.md"),
        )
        self.binary = write_fake_binary(self.root)
        self.socket = os.path.join(self.root, "bossd.sock")
        if install is None:
            install = {"mcp_bin": self.binary, "socket_path": self.socket}
        self._write("tools.json", TOOLS if tools is None else tools)
        self._write("plugin.yaml", {"manifest_version": 2, "name": "bossanova", "version": version})
        if install is not False:
            self._write("boss_install.json", install)
        self.module_name = "hermes_bossanova_test_%d" % next(_counter)
        self.module = self._load()

    def _write(self, name, data):
        with open(os.path.join(self.dir, name), "w", encoding="utf-8") as handle:
            json.dump(data, handle)

    def _load(self):
        spec = importlib.util.spec_from_file_location(
            self.module_name, os.path.join(self.dir, "__init__.py"), submodule_search_locations=[self.dir]
        )
        module = importlib.util.module_from_spec(spec)
        sys.modules[self.module_name] = module
        spec.loader.exec_module(module)
        return module

    def submodule(self, name):
        return sys.modules["%s.%s" % (self.module_name, name)]

    def starts(self):
        try:
            with open(self.socket + ".starts", "r", encoding="utf-8") as handle:
                return len(handle.read().splitlines())
        except OSError:
            return 0

    def cleanup(self):
        for name in list(sys.modules):
            if name == self.module_name or name.startswith(self.module_name + "."):
                del sys.modules[name]
        shutil.rmtree(self.root, ignore_errors=True)
