"""Map MCP tool results to the JSON strings Hermes tool handlers return.

A Hermes handler must return a JSON string for success and failure alike and
must never raise. ``make_handler`` builds such a handler around any callable
that performs the MCP call.

Standard library only.
"""

import json

HINT = "run boss hermes status"


def _dumps(payload):
    return json.dumps(payload, ensure_ascii=False)


def error_json(message, hint=HINT):
    """Return the error payload Hermes sees when a call could not complete."""
    payload = {"error": message or "unknown error"}
    if hint:
        payload["hint"] = hint
    return _dumps(payload)


def _content_text(content):
    parts = []
    for item in content or []:
        if not isinstance(item, dict):
            continue
        kind = item.get("type")
        if kind == "text":
            parts.append(str(item.get("text", "")))
        elif kind:
            parts.append("[%s content omitted]" % kind)
    return "\n".join(parts)


def _maybe_json(text):
    """Decode text that is itself JSON, so the result is not double-encoded."""
    stripped = text.strip()
    if not stripped or stripped[0] not in "{[":
        return text
    try:
        return json.loads(stripped)
    except ValueError:
        return text


def result_to_json(result):
    """Map an MCP CallToolResult dict to Hermes's JSON string.

    - ``isError`` -> ``{"error": <joined text>}``
    - ``structuredContent`` present -> ``{"result": <structuredContent>}``
    - otherwise -> ``{"result": <joined text content>}``, decoded when that
      text is itself a JSON object or array.
    """
    if not isinstance(result, dict):
        return error_json("malformed tool result from the bossanova MCP server")
    text = _content_text(result.get("content"))
    if result.get("isError"):
        return _dumps({"error": text or "the tool reported an error"})
    structured = result.get("structuredContent")
    if structured is not None:
        return _dumps({"result": structured})
    return _dumps({"result": _maybe_json(text)})


def make_handler(tool_name, call):
    """Return a Hermes handler for one MCP tool.

    ``call(tool_name, arguments)`` performs the MCP call and returns the
    CallToolResult dict, raising on failure. The handler never raises: every
    exception becomes an error JSON string carrying the status hint.
    """

    def handler(args=None, **kwargs):
        try:
            if args is None:
                args = {}
            if not isinstance(args, dict):
                return error_json("arguments for %s must be a JSON object" % tool_name, hint="")
            return result_to_json(call(tool_name, args))
        except Exception as exc:  # the Hermes contract: a handler never raises
            return error_json(str(exc) or type(exc).__name__)

    handler.__name__ = "bossanova_%s" % tool_name
    return handler
