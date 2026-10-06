"""A fake bossanova MCP server speaking newline-delimited JSON-RPC over stdio.

Invoked as ``fake_mcp_server.py --socket <path>``. ``<path>`` doubles as a
state prefix: every start appends a line to ``<path>.starts`` so tests can
count restarts. Tool behaviour is chosen by tool name.
"""

import json
import os
import sys
import time


def _socket_path(argv):
    for i, arg in enumerate(argv):
        if arg == "--socket" and i + 1 < len(argv):
            return argv[i + 1]
    return ""


def _send(message):
    sys.stdout.write(json.dumps(message) + "\n")
    sys.stdout.flush()


def _text(text, is_error=False):
    result = {"content": [{"type": "text", "text": text}]}
    if is_error:
        result["isError"] = True
    return result


def _call(name, arguments, socket_path):
    if name == "echo":
        return _text(json.dumps(arguments, sort_keys=True))
    if name == "plain_text":
        return _text("hello from the fake server")
    if name == "structured":
        return {
            "content": [{"type": "text", "text": "ignored when structured content is present"}],
            "structuredContent": {"ok": True, "args": arguments},
        }
    if name == "fail":
        return _text("backend said no", is_error=True)
    if name == "crash":
        sys.stderr.write("fake server crashing on purpose\n")
        sys.stderr.flush()
        os._exit(3)
    if name == "hang":
        time.sleep(3600)
    if name == "env":
        return _text(
            json.dumps(
                {
                    "BOSS_SETTINGS_PATH": os.environ.get("BOSS_SETTINGS_PATH"),
                    "HERMES_PLUGIN_TEST_LEAK": os.environ.get("HERMES_PLUGIN_TEST_LEAK"),
                    "PATH_SET": bool(os.environ.get("PATH")),
                    "argv": sys.argv[1:],
                    "socket": socket_path,
                }
            )
        )
    if name == "chatty":
        # A notification and a server-initiated request before the answer:
        # the client must skip both and still match its own response.
        _send({"jsonrpc": "2.0", "method": "notifications/message", "params": {"level": "info", "data": "hi"}})
        _send({"jsonrpc": "2.0", "id": "srv-1", "method": "ping"})
        return _text("chatty done")
    if name == "list_sessions":
        return _text(json.dumps([{"id": "s1"}, {"id": "s2"}, {"id": "s3"}]))
    if name == "get_session_statuses":
        ids = arguments.get("session_ids") or []
        statuses = {"s1": 1, "s2": 2, "s3": 1}
        return _text(json.dumps([{"session_id": i, "status": statuses.get(i, 0)} for i in ids]))
    return None


def main():
    socket_path = _socket_path(sys.argv[1:])
    if socket_path:
        with open(socket_path + ".starts", "a", encoding="utf-8") as handle:
            handle.write("start\n")
    close_stdin_after_reply = False
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        message = json.loads(line)
        method = message.get("method")
        if "id" not in message or method is None:
            continue  # notifications and replies to our own requests
        if method == "initialize":
            params = message.get("params") or {}
            _send(
                {
                    "jsonrpc": "2.0",
                    "id": message["id"],
                    "result": {
                        "protocolVersion": params.get("protocolVersion"),
                        "capabilities": {"tools": {}},
                        "serverInfo": {"name": "fake-bossanova", "version": "0"},
                    },
                }
            )
            continue
        if method != "tools/call":
            _send({"jsonrpc": "2.0", "id": message["id"], "error": {"code": -32601, "message": "no such method"}})
            continue
        params = message.get("params") or {}
        name = params.get("name")
        if name == "close_stdin":
            close_stdin_after_reply = True
            result = _text("closing stdin")
        else:
            result = _call(name, params.get("arguments") or {}, socket_path)
        if result is None:
            _send({"jsonrpc": "2.0", "id": message["id"], "error": {"code": -32602, "message": "unknown tool %s" % name}})
            continue
        if close_stdin_after_reply:
            # Close stdin before acknowledging, so the client cannot write its
            # next request into a still-open pipe; that write then breaks the
            # pipe while poll() still reports the process running.
            sys.stdin.close()
            os.close(0)
            _send({"jsonrpc": "2.0", "id": message["id"], "result": result})
            time.sleep(3600)
        _send({"jsonrpc": "2.0", "id": message["id"], "result": result})


if __name__ == "__main__":
    main()
