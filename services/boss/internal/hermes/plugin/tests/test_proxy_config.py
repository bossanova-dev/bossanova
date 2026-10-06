import json
import os
import unittest

import support


class ProxyTest(unittest.TestCase):
    def setUp(self):
        self.plugin = support.RenderedPlugin()
        self.addCleanup(self.plugin.cleanup)
        self.proxy = self.plugin.submodule("proxy")

    def decode(self, result):
        return json.loads(self.proxy.result_to_json(result))

    def test_structured_content_wins(self):
        out = self.decode({"content": [{"type": "text", "text": "x"}], "structuredContent": {"a": 1}})
        self.assertEqual(out, {"result": {"a": 1}})

    def test_text_content_is_joined(self):
        out = self.decode({"content": [{"type": "text", "text": "one"}, {"type": "text", "text": "two"}]})
        self.assertEqual(out, {"result": "one\ntwo"})

    def test_json_text_is_decoded_not_double_encoded(self):
        out = self.decode({"content": [{"type": "text", "text": '[{"id": "s1"}]'}]})
        self.assertEqual(out, {"result": [{"id": "s1"}]})

    def test_non_text_content_is_noted(self):
        out = self.decode({"content": [{"type": "image", "data": "..."}]})
        self.assertEqual(out, {"result": "[image content omitted]"})

    def test_is_error(self):
        self.assertEqual(self.decode({"isError": True, "content": [{"type": "text", "text": "nope"}]}), {"error": "nope"})
        self.assertEqual(self.decode({"isError": True}), {"error": "the tool reported an error"})

    def test_malformed_result(self):
        out = self.decode(None)
        self.assertIn("malformed", out["error"])

    def test_handler_never_raises(self):
        def boom(name, args):
            raise RuntimeError("kaboom")

        out = json.loads(self.proxy.make_handler("x", boom)({}))
        self.assertEqual(out, {"error": "kaboom", "hint": "run boss hermes status"})

        def silent(name, args):
            raise ValueError()

        out = json.loads(self.proxy.make_handler("x", silent)(None))
        self.assertEqual(out["error"], "ValueError")

        def bad_result(name, args):
            return "not a dict"

        out = json.loads(self.proxy.make_handler("x", bad_result)({}))
        self.assertIn("malformed", out["error"])

    def test_handler_passes_tool_name_and_args(self):
        seen = []

        def call(name, args):
            seen.append((name, args))
            return {"content": [{"type": "text", "text": "ok"}]}

        self.proxy.make_handler("list_repos", call)({"a": 1})
        self.assertEqual(seen, [("list_repos", {"a": 1})])


class ConfigPrecedenceTest(unittest.TestCase):
    def setUp(self):
        self.plugin = support.RenderedPlugin(
            install={"mcp_bin": "/install/mcp", "socket_path": "/install/bossd.sock"}
        )
        self.addCleanup(self.plugin.cleanup)
        self.config = self.plugin.submodule("config")

    def resolve(self, settings=None):
        return self.config.resolve(support.FakeCtx(settings), self.plugin.dir)

    def test_install_file_supplies_values(self):
        r = self.resolve()
        self.assertEqual(r.mcp_bin, "/install/mcp")
        self.assertEqual(r.socket_path, "/install/bossd.sock")
        self.assertEqual(r.settings_path, "")
        self.assertEqual(r.tool_timeout_seconds, 120.0)

    def test_settings_form_beats_install_file(self):
        r = self.resolve(
            {
                "mcp_bin": "/form/mcp",
                "socket_path": "/form/bossd.sock",
                "settings_path": "/form/settings.json",
                "tool_timeout_seconds": 30,
            }
        )
        self.assertEqual(r.mcp_bin, "/form/mcp")
        self.assertEqual(r.socket_path, "/form/bossd.sock")
        self.assertEqual(r.settings_path, "/form/settings.json")
        self.assertEqual(r.tool_timeout_seconds, 30.0)

    def test_empty_form_values_fall_through(self):
        r = self.resolve({"mcp_bin": "", "socket_path": "   ", "settings_path": None, "tool_timeout_seconds": ""})
        self.assertEqual(r.mcp_bin, "/install/mcp")
        self.assertEqual(r.socket_path, "/install/bossd.sock")
        self.assertEqual(r.settings_path, "")
        self.assertEqual(r.tool_timeout_seconds, 120.0)

    def test_install_settings_path_is_used_when_present(self):
        self.plugin.cleanup()
        self.plugin = support.RenderedPlugin(
            install={"mcp_bin": "/m", "socket_path": "/s", "settings_path": "/install/settings.json"}
        )
        self.config = self.plugin.submodule("config")
        self.assertEqual(self.resolve().settings_path, "/install/settings.json")

    def test_invalid_timeouts_use_the_default(self):
        for value in ("soon", -5, 0, True):
            self.assertEqual(self.resolve({"tool_timeout_seconds": value}).tool_timeout_seconds, 120.0, value)

    def test_missing_values_name_the_install_command(self):
        self.plugin.cleanup()
        self.plugin = support.RenderedPlugin(install=False)
        self.config = self.plugin.submodule("config")
        with self.assertRaises(self.config.ConfigError) as caught:
            self.resolve()
        self.assertIn("boss hermes install", str(caught.exception))
        with self.assertRaises(self.config.ConfigError) as caught:
            self.resolve({"mcp_bin": "/form/mcp"})
        self.assertIn("socket_path", str(caught.exception))

    def test_malformed_install_file_reads_as_missing(self):
        with open(os.path.join(self.plugin.dir, "boss_install.json"), "w", encoding="utf-8") as handle:
            handle.write("{not json")
        with self.assertRaises(self.config.ConfigError):
            self.resolve()

    def test_ctx_without_get_config_or_with_a_broken_one(self):
        class NoConfig(object):
            pass

        class Broken(object):
            def get_config(self, key, default=None):
                raise RuntimeError("settings store unavailable")

        for ctx in (NoConfig(), Broken()):
            self.assertEqual(self.config.resolve(ctx, self.plugin.dir).mcp_bin, "/install/mcp")

    def test_resolve_binary(self):
        self.assertEqual(self.config.resolve_binary(self.plugin.binary), self.plugin.binary)
        self.assertIsNone(self.config.resolve_binary(os.path.join(self.plugin.root, "missing")))
        self.assertIsNone(self.config.resolve_binary(""))
        self.assertIsNone(self.config.resolve_binary(self.plugin.dir), "a directory is not a binary")
        self.assertIsNotNone(self.config.resolve_binary("sh"), "bare names resolve on PATH")


if __name__ == "__main__":
    unittest.main()
