"""A minimal MCP stdio client for the bossanova MCP server.

It spawns ``<mcp_bin> --socket <socket>`` with an explicit environment, speaks
newline-delimited JSON-RPC 2.0 (``initialize`` -> ``notifications/initialized``
-> ``tools/call``), serialises calls behind one lock, bounds every call with a
timeout, restarts the process once when it has died or its pipe is broken, and
terminates it at interpreter exit.

Standard library only.
"""

import atexit
import collections
import json
import os
import queue
import subprocess
import threading
import time

PROTOCOL_VERSION = "2025-06-18"
CLIENT_INFO = {"name": "hermes-bossanova", "version": "1"}

# Stop waiting for a process that ignored terminate() after this long.
_STOP_GRACE_SECONDS = 2.0
# How many trailing stderr lines to keep for error messages.
_STDERR_TAIL = 20

_EOF = object()


class McpError(Exception):
    """A tool call could not be completed."""


class McpTimeout(McpError):
    """The server did not answer within the call's timeout."""


class McpServerExited(McpError):
    """The server process exited or closed its pipes before answering."""


class _PipeBroken(Exception):
    """Writing a request to the server failed."""


class LaunchSpec(object):
    """How to start the MCP server: binary, bossd socket and settings file."""

    __slots__ = ("mcp_bin", "socket_path", "settings_path")

    def __init__(self, mcp_bin, socket_path, settings_path=""):
        self.mcp_bin = mcp_bin
        self.socket_path = socket_path
        self.settings_path = settings_path or ""

    def _key(self):
        return (self.mcp_bin, self.socket_path, self.settings_path)

    def __eq__(self, other):
        return isinstance(other, LaunchSpec) and self._key() == other._key()

    def __ne__(self, other):
        return not self.__eq__(other)

    def __hash__(self):
        return hash(self._key())

    def argv(self):
        return [self.mcp_bin, "--socket", self.socket_path]

    def env(self, base=None):
        return build_env(self.settings_path, base)


def build_env(settings_path, base=None):
    """Return the MCP process environment: PATH, HOME, and BOSS_SETTINGS_PATH
    only when a settings path is configured. Nothing else from Hermes's
    environment leaks into the server."""
    if base is None:
        base = os.environ
    env = {"PATH": base.get("PATH") or os.defpath}
    home = base.get("HOME")
    if home:
        env["HOME"] = home
    if settings_path:
        env["BOSS_SETTINGS_PATH"] = settings_path
    return env


class _Process(object):
    """One running server process plus the threads draining its pipes."""

    def __init__(self, spec):
        self.spec = spec
        self.responses = queue.Queue()
        self.stderr_tail = collections.deque(maxlen=_STDERR_TAIL)
        try:
            self.popen = subprocess.Popen(
                spec.argv(),
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=spec.env(),
                close_fds=True,
                start_new_session=True,
            )
        except OSError as exc:
            raise McpError("cannot start %s: %s" % (spec.mcp_bin, exc))
        self._threads = [
            threading.Thread(target=self._read_stdout, name="bossanova-mcp-stdout", daemon=True),
            threading.Thread(target=self._read_stderr, name="bossanova-mcp-stderr", daemon=True),
        ]
        for thread in self._threads:
            thread.start()

    def _read_stdout(self):
        try:
            for raw in self.popen.stdout:
                line = raw.strip()
                if not line:
                    continue
                try:
                    message = json.loads(line.decode("utf-8"))
                except ValueError:
                    continue  # not a JSON-RPC frame; the server logs to stderr
                if isinstance(message, dict):
                    self.responses.put(message)
        except (OSError, ValueError):
            pass
        finally:
            self.responses.put(_EOF)

    def _read_stderr(self):
        try:
            for raw in self.popen.stderr:
                text = raw.decode("utf-8", "replace").rstrip()
                if text:
                    self.stderr_tail.append(text)
        except (OSError, ValueError):
            pass

    def alive(self):
        return self.popen.poll() is None

    def send(self, message):
        data = (json.dumps(message, separators=(",", ":")) + "\n").encode("utf-8")
        try:
            self.popen.stdin.write(data)
            self.popen.stdin.flush()
        except (BrokenPipeError, OSError, ValueError) as exc:
            raise _PipeBroken(str(exc))

    def describe_exit(self):
        code = self.popen.poll()
        detail = "; ".join(list(self.stderr_tail)[-3:])
        msg = "bossanova MCP server exited"
        if code is not None:
            msg += " with status %s" % code
        if detail:
            msg += ": " + detail
        return msg

    def stop(self):
        try:
            self.popen.stdin.close()
        except (OSError, ValueError):
            pass
        if self.popen.poll() is None:
            try:
                self.popen.terminate()
                self.popen.wait(timeout=_STOP_GRACE_SECONDS)
            except subprocess.TimeoutExpired:
                self.popen.kill()
                try:
                    self.popen.wait(timeout=_STOP_GRACE_SECONDS)
                except subprocess.TimeoutExpired:
                    pass
            except OSError:
                pass
        for stream in (self.popen.stdout, self.popen.stderr):
            try:
                stream.close()
            except (OSError, ValueError):
                pass


class McpClient(object):
    """A lazily started, self-restarting connection to the bossanova MCP server.

    Thread-safe: one lock serialises every request, so exactly one call is in
    flight on the pipe at a time.
    """

    def __init__(self):
        self._lock = threading.Lock()
        self._proc = None
        self._next_id = 0
        self.starts = 0

    def call_tool(self, spec, name, arguments, timeout):
        """Call one tool and return the raw CallToolResult dict.

        Raises McpError (or a subclass) on any failure; never returns None.
        """
        params = {"name": name, "arguments": arguments if arguments is not None else {}}
        with self._lock:
            restarted = self._ensure_started(spec, timeout)
            try:
                return self._request("tools/call", params, timeout)
            except _PipeBroken:
                if restarted:
                    raise McpServerExited(self._exit_message())
            # The pipe broke under a process that had looked alive: restart once.
            self._stop_locked()
            self._ensure_started(spec, timeout)
            try:
                return self._request("tools/call", params, timeout)
            except _PipeBroken:
                raise McpServerExited(self._exit_message())

    def close(self):
        """Terminate the server process, if any. Safe to call repeatedly."""
        acquired = self._lock.acquire(timeout=_STOP_GRACE_SECONDS)
        try:
            self._stop_locked()
        finally:
            if acquired:
                self._lock.release()

    # -- internals; every method below runs with self._lock held ------------

    def _ensure_started(self, spec, timeout):
        """Start (or restart) the server when needed; True when it started."""
        if self._proc is not None and (self._proc.spec != spec or not self._proc.alive()):
            self._stop_locked()
        if self._proc is not None:
            return False
        self._proc = _Process(spec)
        self.starts += 1
        try:
            self._request(
                "initialize",
                {
                    "protocolVersion": PROTOCOL_VERSION,
                    "capabilities": {},
                    "clientInfo": CLIENT_INFO,
                },
                timeout,
            )
            self._proc.send({"jsonrpc": "2.0", "method": "notifications/initialized"})
        except _PipeBroken:
            message = self._exit_message()
            self._stop_locked()
            raise McpServerExited(message)
        except McpError:
            self._stop_locked()
            raise
        return True

    def _exit_message(self):
        if self._proc is None:
            return "bossanova MCP server is not running"
        # Give the exit status a moment to land so the message can carry it.
        deadline = time.monotonic() + 0.5
        while self._proc.popen.poll() is None and time.monotonic() < deadline:
            time.sleep(0.01)
        return self._proc.describe_exit()

    def _request(self, method, params, timeout):
        proc = self._proc
        self._next_id += 1
        request_id = self._next_id
        proc.send({"jsonrpc": "2.0", "id": request_id, "method": method, "params": params})
        deadline = time.monotonic() + timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                self._stop_locked()
                raise McpTimeout("bossanova MCP server did not answer %s within %gs" % (method, timeout))
            try:
                message = proc.responses.get(timeout=remaining)
            except queue.Empty:
                continue
            if message is _EOF:
                text = self._exit_message()
                self._stop_locked()
                raise McpServerExited(text)
            if "method" in message:
                if "id" in message:
                    # A server-initiated request; this client offers no capabilities.
                    try:
                        proc.send(
                            {
                                "jsonrpc": "2.0",
                                "id": message["id"],
                                "error": {"code": -32601, "message": "method not supported by client"},
                            }
                        )
                    except _PipeBroken:
                        pass
                continue  # notifications are ignored
            if message.get("id") != request_id:
                continue  # a stale reply to an abandoned request
            if "error" in message:
                error = message.get("error") or {}
                if isinstance(error, dict):
                    text = error.get("message") or json.dumps(error)
                else:
                    text = str(error)
                raise McpError("%s failed: %s" % (method, text))
            result = message.get("result")
            if not isinstance(result, dict):
                raise McpError("%s returned a malformed result" % method)
            return result

    def _stop_locked(self):
        proc, self._proc = self._proc, None
        if proc is not None:
            proc.stop()


_FALLBACK_LOCK = threading.Lock()
_FALLBACK_CLIENT = []


def _new_client():
    client = McpClient()
    atexit.register(client.close)
    return client


def _fallback_accessor():
    if _FALLBACK_CLIENT:
        return _FALLBACK_CLIENT[0]
    with _FALLBACK_LOCK:
        if not _FALLBACK_CLIENT:
            _FALLBACK_CLIENT.append(_new_client())
        return _FALLBACK_CLIENT[0]


try:  # Hermes ships a thread-safe lazy singleton helper; prefer it when present.
    from plugins.plugin_utils import lazy_singleton as _lazy_singleton  # type: ignore
except Exception:  # not running inside Hermes, or an older Hermes
    _lazy_singleton = None

# shared_client() returns the process-wide McpClient, creating it on first use.
# Creating the client starts no process; the server starts on the first call.
if _lazy_singleton is not None:
    try:
        shared_client = _lazy_singleton(_new_client)
    except Exception:
        shared_client = _fallback_accessor
else:
    shared_client = _fallback_accessor
