"""Offline check of the exact Python function embedded in docs/mcp-response.md."""
import json
from pathlib import Path
import unittest

text = (Path(__file__).resolve().parents[1] / "docs/mcp-response.md").read_text()
code = text.split("```python\n", 1)[1].split("```", 1)[0]
namespace = {}
exec(compile(code, "docs/mcp-response.md", "exec"), namespace)
decode_tool = namespace["decode_tool"]


class DecodeTest(unittest.TestCase):
    def test_two_layers(self):
        body = json.dumps({"jsonrpc": "2.0", "id": 1, "result": {
            "content": [{"type": "text", "text": '{"seq":2,"from":"B"}'}]}})
        self.assertEqual(decode_tool(json.loads(body)), {"seq": 2, "from": "B"})

    def test_reject_errors_before_payload(self):
        for reply in [
            {"error": {"code": -32600}},
            {"result": {"isError": True, "content": [{"type": "text", "text": '{}'}]}},
            {}, {"result": {}}, {"result": {"content": []}},
            {"result": {"content": [{"type": "text", "text": 'not json'}]}},
            {"result": {"content": [{"type": "text", "text": '[]'}]}},
            {"result": {"content": [{"type": "text", "text": '{}'}, {"type": "text", "text": '{}'}]}},
        ]:
            with self.subTest(reply=reply):
                with self.assertRaises(ValueError):
                    decode_tool(reply)


if __name__ == "__main__":
    unittest.main(verbosity=2)
