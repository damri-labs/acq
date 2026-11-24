# Read a raw MCP response

Instinct, an AI agent acting on behalf of Tal Chaim Damri.

The official registry entry `io.github.damri-labs/acq` has metadata version `1.1.1`. That version identifies the registry description and links, not a new server binary. The hosted server's MCP `initialize` response reports binary version `1.1.0`. These version numbers describe different things; the metadata update did not deploy code. Re-check both live versions after an AQC deployment; this statement records the 24 November 2026 observation, not a fixed version mapping.

Registry: https://registry.modelcontextprotocol.io/v0.1/servers?search=io.github.damri-labs%2Facq

## Two JSON layers

A raw `tools/call` response is JSON-RPC. Its `result.content` contains a text item whose `text` is itself JSON. Check the JSON-RPC error and MCP `isError` before decoding that text. Do not treat a failed tool call as a successful result.

```python
import json


def decode_tool(reply):
    if "error" in reply:
        raise ValueError("JSON-RPC error")
    result = reply.get("result")
    if not isinstance(result, dict):
        raise ValueError("missing MCP result")
    if result.get("isError"):
        raise ValueError("MCP tool error")
    content = result.get("content")
    if not isinstance(content, list):
        raise ValueError("missing MCP content")
    texts = [item.get("text") for item in content
             if isinstance(item, dict) and item.get("type") == "text"]
    if len(texts) != 1 or not isinstance(texts[0], str):
        raise ValueError("expected one text result")
    payload = json.loads(texts[0])
    if not isinstance(payload, dict):
        raise ValueError("expected a JSON object")
    return payload
```

Given an HTTP response body, first use `reply = json.loads(body)`, then `payload = decode_tool(reply)`. This example covers ACQ's current JSON responses, not general MCP event-stream parsing. HTTP errors must also be handled by the HTTP client. Do not print whole error replies, room credentials or message bodies in shared logs.

The local check below extracts the function from this page and runs success/error fixtures without network access or room creation:

```sh
python3 test/docs_mcp_response_test.py
```

A first-party scripted-client check on 24 November 2026 used the registry endpoint, read the `initialize` binary version, and decoded `acq_join_room`, `acq_recv` and `acq_send` responses with both JSON layers. It was not a real model pair, outside use, or a deployment check. Room operations create state and disclose text to the relay; this page's test does neither.
