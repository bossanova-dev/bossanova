import json
import os
import threading
import time
import unittest

import support


class McpClientTest(unittest.TestCase):
    def setUp(self):
        self.plugin = support.RenderedPlugin()
        self.addCleanup(self.plugin.cleanup)
        self.mod = self.plugin.submodule("mcp_client")
        self.client = self.mod.McpClient()
        self.addCleanup(self.client.close)
        self.spec = self.mod.LaunchSpec(self.plugin.binary, self.plugin.socket)

    def call(self, name, args=None, timeout=10):
        return self.client.call_tool(self.spec, name, args or {}, timeout)

    def test_lazy_start_and_reuse(self):
        self.assertEqual(self.plugin.starts(), 0, "constructing a client starts nothing")
        result = self.call("plain_text")
        self.assertEqual(result["content"][0]["text"], "hello from the fake server")
        self.call("plain_text")
        self.assertEqual(self.plugin.starts(), 1)
        self.assertEqual(self.client.starts, 1)

    def test_skips_notifications_and_server_requests(self):
        result = self.call("chatty")
        self.assertEqual(result["content"][0]["text"], "chatty done")

    def test_restarts_when_the_process_died_between_calls(self):
        self.call("plain_text")
        popen = self.client._proc.popen
        popen.kill()
        popen.wait(timeout=5)
        result = self.call("plain_text")
        self.assertEqual(result["content"][0]["text"], "hello from the fake server")
        self.assertEqual(self.plugin.starts(), 2)

    def test_restarts_once_when_the_pipe_is_broken(self):
        self.call("close_stdin")
        self.assertTrue(self.client._proc.alive(), "the fake keeps running with a dead stdin")
        result = self.call("plain_text")
        self.assertEqual(result["content"][0]["text"], "hello from the fake server")
        self.assertEqual(self.plugin.starts(), 2)

    def test_crash_raises_server_exited_with_stderr_detail(self):
        with self.assertRaises(self.mod.McpServerExited) as caught:
            self.call("crash")
        self.assertIn("status 3", str(caught.exception))
        self.assertIn("crashing on purpose", str(caught.exception))
        self.assertIsNone(self.client._proc)

    def test_timeout_kills_the_hung_server(self):
        started = time.monotonic()
        with self.assertRaises(self.mod.McpTimeout):
            self.call("hang", timeout=0.5)
        self.assertLess(time.monotonic() - started, 5)
        self.assertIsNone(self.client._proc)
        self.assertEqual(self.call("plain_text")["content"][0]["text"], "hello from the fake server")

    def test_protocol_error_raises_mcp_error(self):
        with self.assertRaises(self.mod.McpError) as caught:
            self.call("no_such_tool")
        self.assertIn("unknown tool no_such_tool", str(caught.exception))
        # A protocol error leaves the server healthy.
        self.call("plain_text")
        self.assertEqual(self.plugin.starts(), 1)

    def start_stalled_write(self):
        self.call("stop_reading")
        popen = self.client._proc.popen
        outcome = {}

        def write():
            started = time.monotonic()
            try:
                self.call("echo", {"text": "x" * (4 * 1024 * 1024)}, timeout=0.5)
            except Exception as exc:
                outcome["error"] = exc
            finally:
                outcome["elapsed"] = time.monotonic() - started

        thread = threading.Thread(target=write, daemon=True)

        def cleanup():
            # Kill before client.close(), even when the old synchronous write
            # hangs: closing its BufferedWriter first would hang teardown too.
            if popen.poll() is None:
                popen.kill()
            popen.wait(timeout=5)
            thread.join(timeout=5)

        self.addCleanup(cleanup)
        thread.start()
        return popen, thread, outcome

    def test_large_write_to_a_stalled_server_times_out_and_kills_it(self):
        popen, thread, outcome = self.start_stalled_write()
        thread.join(timeout=10)
        self.assertFalse(thread.is_alive(), "stalled stdin write exceeded its deadline")
        self.assertIsInstance(outcome.get("error"), self.mod.McpTimeout)
        self.assertIn("did not accept", str(outcome["error"]))
        self.assertLess(outcome["elapsed"], 5)
        self.assertIsNotNone(popen.poll())
        self.assertIsNone(self.client._proc)
        self.assertEqual(self.call("plain_text")["content"][0]["text"], "hello from the fake server")
        self.assertEqual(self.plugin.starts(), 2)

    def test_lock_is_released_for_a_waiting_caller(self):
        _, writer, outcome = self.start_stalled_write()
        deadline = time.monotonic() + 5
        while not self.client._lock.locked() and time.monotonic() < deadline:
            time.sleep(0.001)
        self.assertTrue(self.client._lock.locked(), "writer did not acquire the client lock")
        result = {}

        def call():
            try:
                result["value"] = self.call("plain_text", timeout=10)
            except Exception as exc:
                result["error"] = exc

        waiter = threading.Thread(target=call, daemon=True)
        waiter.start()
        waiter.join(timeout=10)
        self.assertFalse(waiter.is_alive(), "waiting caller remained blocked after the write deadline")
        writer.join(timeout=5)
        self.assertIsInstance(outcome.get("error"), self.mod.McpTimeout)
        self.assertNotIn("error", result)
        self.assertEqual(result["value"]["content"][0]["text"], "hello from the fake server")
        self.assertEqual(self.plugin.starts(), 2)

    def test_large_request_round_trips(self):
        arguments = {"text": "x" * (1024 * 1024)}
        result = self.call("echo", arguments)
        self.assertEqual(json.loads(result["content"][0]["text"]), arguments)

    def test_unstartable_binary_raises_mcp_error(self):
        spec = self.mod.LaunchSpec(os.path.join(self.plugin.root, "missing"), self.plugin.socket)
        with self.assertRaises(self.mod.McpError) as caught:
            self.client.call_tool(spec, "plain_text", {}, 5)
        self.assertIn("cannot start", str(caught.exception))

    def test_close_terminates_the_server(self):
        self.call("plain_text")
        popen = self.client._proc.popen
        self.client.close()
        self.assertIsNotNone(popen.poll())
        self.client.close()  # idempotent


class BuildEnvTest(unittest.TestCase):
    def setUp(self):
        self.plugin = support.RenderedPlugin()
        self.addCleanup(self.plugin.cleanup)
        self.mod = self.plugin.submodule("mcp_client")

    def test_settings_path_only_when_configured(self):
        base = {"PATH": "/usr/bin", "HOME": "/home/u", "SECRET": "x", "BOSS_SETTINGS_PATH": "/inherited"}
        self.assertEqual(self.mod.build_env("", base), {"PATH": "/usr/bin", "HOME": "/home/u"})
        self.assertEqual(
            self.mod.build_env("/s.json", base),
            {"PATH": "/usr/bin", "HOME": "/home/u", "BOSS_SETTINGS_PATH": "/s.json"},
        )

    def test_missing_path_falls_back_to_the_os_default(self):
        env = self.mod.build_env("", {})
        self.assertEqual(env, {"PATH": os.defpath})

    def test_launch_spec_argv_and_equality(self):
        a = self.mod.LaunchSpec("/bin/mcp", "/s.sock", "")
        self.assertEqual(a.argv(), ["/bin/mcp", "--socket", "/s.sock"])
        self.assertEqual(a, self.mod.LaunchSpec("/bin/mcp", "/s.sock", None))
        self.assertNotEqual(a, self.mod.LaunchSpec("/bin/mcp", "/s.sock", "/x.json"))


if __name__ == "__main__":
    unittest.main()
