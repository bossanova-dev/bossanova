import json
import os
import pathlib
import socket
import subprocess
import unittest
from unittest import mock

import support


class RegisterTest(unittest.TestCase):
    def setUp(self):
        self.plugin = support.RenderedPlugin()
        self.addCleanup(self.plugin.cleanup)
        self.addCleanup(lambda: self.plugin.submodule("mcp_client").shared_client().close())

    def register(self, settings=None):
        ctx = support.FakeCtx(settings)
        self.plugin.module.register(ctx)
        return ctx

    def test_registers_one_prefixed_tool_per_tools_json_entry(self):
        ctx = self.register()
        self.assertEqual([t["name"] for t in ctx.tools], ["bossanova_" + t["name"] for t in support.TOOLS])
        for tool, source in zip(ctx.tools, support.TOOLS):
            self.assertEqual(tool["toolset"], "bossanova")
            self.assertEqual(tool["schema"]["name"], tool["name"])
            self.assertEqual(tool["schema"]["description"], source["description"])
            self.assertEqual(tool["schema"]["parameters"], source["inputSchema"])
            self.assertTrue(callable(tool["handler"]))
            self.assertTrue(callable(tool["check_fn"]))

    def test_registers_command_and_namespaced_skill(self):
        ctx = self.register()
        self.assertEqual([c["name"] for c in ctx.commands], ["bossanova"])
        self.assertEqual([s["name"] for s in ctx.skills], ["orchestrator"])
        path = ctx.skills[0]["path"]
        self.assertIsInstance(path, pathlib.Path)
        self.assertEqual(path.parts[-3:], ("skills", "orchestrator", "SKILL.md"))
        self.assertTrue(path.is_file())

    def test_register_starts_no_process_and_opens_no_socket(self):
        boom = AssertionError("register() must do no subprocess or network I/O")
        with mock.patch.object(subprocess, "Popen", side_effect=boom), mock.patch.object(
            socket, "socket", side_effect=boom
        ), mock.patch.object(socket, "create_connection", side_effect=boom):
            ctx = self.register()
        self.assertEqual(len(ctx.tools), len(support.TOOLS))
        self.assertEqual(self.plugin.starts(), 0)

    def test_missing_input_schema_defaults_to_an_empty_object(self):
        self.plugin.cleanup()
        self.plugin = support.RenderedPlugin(tools=[{"name": "bare", "description": "no schema"}])
        ctx = self.register()
        self.assertEqual(ctx.tools[0]["schema"]["parameters"], {"type": "object", "properties": {}})

    def test_check_fn_hides_tools_when_no_binary_resolves(self):
        ctx = self.register()
        check = ctx.tools[0]["check_fn"]
        self.assertTrue(check())
        ctx.settings["mcp_bin"] = os.path.join(self.plugin.root, "does-not-exist")
        self.assertFalse(check())
        ctx.settings["mcp_bin"] = ""  # empty falls back to the install file
        self.assertTrue(check())

    def test_check_fn_is_false_without_any_install_config(self):
        self.plugin.cleanup()
        self.plugin = support.RenderedPlugin(install=False)
        ctx = self.register()
        self.assertFalse(ctx.tools[0]["check_fn"]())

    def test_help_names_the_orchestrator_skill(self):
        ctx = self.register()
        text = ctx.commands[0]["handler"]("help")
        self.assertIn('skill_view("bossanova:orchestrator")', text)

    def test_status_reports_config_and_live_session_summary(self):
        ctx = self.register()
        text = ctx.commands[0]["handler"]("")
        self.assertIn("bossanova plugin 1.2.3", text)
        self.assertIn(self.plugin.binary, text)
        self.assertIn(self.plugin.socket, text)
        self.assertIn("(OS default)", text)
        self.assertIn("sessions: 3 (1 idle, 2 working)", text)

    def test_status_without_install_config_explains_the_fix(self):
        self.plugin.cleanup()
        self.plugin = support.RenderedPlugin(install=False)
        ctx = self.register()
        text = ctx.commands[0]["handler"]("status")
        self.assertIn("boss hermes install", text)

    def test_unknown_subcommand_falls_back_to_help(self):
        ctx = self.register()
        text = ctx.commands[0]["handler"]("frobnicate")
        self.assertIn("Unknown subcommand", text)
        self.assertIn("/bossanova status", text)


class HandlerRoundTripTest(unittest.TestCase):
    def setUp(self):
        self.plugin = support.RenderedPlugin()
        self.addCleanup(self.plugin.cleanup)
        self.addCleanup(lambda: self.plugin.submodule("mcp_client").shared_client().close())
        self.ctx = support.FakeCtx()
        self.plugin.module.register(self.ctx)

    def call(self, tool, args=None):
        out = self.ctx.tool("bossanova_" + tool)["handler"](args if args is not None else {})
        self.assertIsInstance(out, str)
        return json.loads(out)

    def test_round_trip_through_the_fake_server(self):
        self.assertEqual(self.call("echo", {"a": 1, "b": "two"}), {"result": {"a": 1, "b": "two"}})
        self.assertEqual(self.plugin.starts(), 1)
        self.assertEqual(self.call("echo", {"again": True}), {"result": {"again": True}})
        self.assertEqual(self.plugin.starts(), 1, "the server must be reused across calls")

    def test_handler_accepts_hermes_kwargs(self):
        out = self.ctx.tool("bossanova_echo")["handler"]({"x": 1}, task_id="t1", session_id="s1")
        self.assertEqual(json.loads(out), {"result": {"x": 1}})

    def test_is_error_maps_to_error_json(self):
        self.assertEqual(self.call("fail"), {"error": "backend said no"})

    def test_crash_maps_to_error_json_and_next_call_restarts(self):
        out = self.call("crash")
        self.assertIn("error", out)
        self.assertIn("exited", out["error"])
        self.assertEqual(out.get("hint"), "run boss hermes status")
        self.assertEqual(self.call("echo", {"after": "crash"}), {"result": {"after": "crash"}})
        self.assertEqual(self.plugin.starts(), 2)

    def test_hung_server_times_out_into_error_json_and_recovers(self):
        self.ctx.settings["tool_timeout_seconds"] = "1"
        out = self.call("hang")
        self.assertIn("error", out)
        self.assertIn("did not answer", out["error"])
        self.assertEqual(self.call("echo", {"after": "hang"}), {"result": {"after": "hang"}})
        self.assertEqual(self.plugin.starts(), 2)

    def test_unknown_tool_protocol_error_maps_to_error_json(self):
        plugin = self.plugin.module.Plugin(self.ctx, self.plugin.dir)
        handler = self.plugin.submodule("proxy").make_handler("no_such_tool", plugin.call)
        out = json.loads(handler({}))
        self.assertIn("unknown tool no_such_tool", out["error"])
        self.assertEqual(out.get("hint"), "run boss hermes status")

    def test_missing_binary_maps_to_error_json(self):
        self.ctx.settings["mcp_bin"] = os.path.join(self.plugin.root, "missing-binary")
        out = self.call("echo")
        self.assertIn("not an executable file", out["error"])
        self.assertEqual(self.plugin.starts(), 0)

    def test_non_object_arguments_map_to_error_json(self):
        out = json.loads(self.ctx.tool("bossanova_echo")["handler"](["not", "an", "object"]))
        self.assertIn("must be a JSON object", out["error"])

    def test_settings_path_reaches_the_server_only_when_configured(self):
        with mock.patch.dict(os.environ, {"HERMES_PLUGIN_TEST_LEAK": "secret"}):
            env = self.call("env")["result"]
            self.assertIsNone(env["BOSS_SETTINGS_PATH"])
            self.assertIsNone(env["HERMES_PLUGIN_TEST_LEAK"], "only PATH/HOME/BOSS_SETTINGS_PATH pass through")
            self.assertTrue(env["PATH_SET"])
            self.assertEqual(env["argv"], ["--socket", self.plugin.socket])

            self.ctx.settings["settings_path"] = "/tmp/custom-settings.json"
            env = self.call("env")["result"]
            self.assertEqual(env["BOSS_SETTINGS_PATH"], "/tmp/custom-settings.json")
            self.assertEqual(self.plugin.starts(), 2, "a changed launch config restarts the server")


if __name__ == "__main__":
    unittest.main()
