import json
import threading
import unittest
from http.client import HTTPConnection
from pathlib import Path
import importlib.util


MODULE_PATH = Path(__file__).with_name("model.py")
SPEC = importlib.util.spec_from_file_location("tiny_agent_lab_model", MODULE_PATH)
model = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(model)


class ModelFixtureTests(unittest.TestCase):
    def test_hello_uses_latest_user_marker(self):
        result = model.complete({"model": "tiny-lab", "messages": [
            {"role": "user", "content": "old LAB_HELLO_old"},
            {"role": "assistant", "content": "done"},
            {"role": "user", "content": "LAB_HELLO_new"},
        ]})
        self.assertEqual(result["choices"][0]["message"]["content"], "LAB_DONE LAB_HELLO_new")

    def test_question_requires_advertised_tool_and_returns_canonical_batch(self):
        body = {"messages": [{"role": "user", "content": "LAB_QUESTION_123"}], "tools": [{"type": "function", "function": {"name": "clarify"}}]}
        call = model.complete(body)["choices"][0]["message"]["tool_calls"][0]
        self.assertEqual(call["function"]["name"], "clarify")
        args = json.loads(call["function"]["arguments"])
        self.assertEqual(args["questions"][0]["choices"], ["Continue", "Stop"])
        self.assertFalse(args["questions"][0]["multi_select"])
        error = model.complete({"messages": body["messages"]})["choices"][0]["message"]["content"]
        self.assertIn("clarify tool is unavailable", error)

    def test_reply_quote_does_not_replace_current_scenario(self):
        body = {"messages": [{"role": "user", "content":
            '[Replying to your previous message: "LAB_DONE LAB_HELLO_old"]\n\nLAB_QUESTION_new'}],
            "tools": [{"type": "function", "function": {"name": "clarify"}}]}
        message = model.complete(body)["choices"][0]["message"]
        arguments = json.loads(message["tool_calls"][0]["function"]["arguments"])
        self.assertIn("LAB_QUESTION_new", arguments["questions"][0]["question"])

    def test_multi_and_approval_tool_results_complete_turn(self):
        multi = model.complete({"messages": [{"role": "user", "content": "LAB_MULTI_123"}], "tools": [{"type": "function", "function": {"name": "clarify"}}]})
        args = json.loads(multi["choices"][0]["message"]["tool_calls"][0]["function"]["arguments"])
        self.assertTrue(args["questions"][0]["multi_select"])
        approval = model.complete({"messages": [{"role": "user", "content": "LAB_APPROVAL_123"}], "tools": [{"type": "function", "function": {"name": "execute_code"}}]})
        approval_call = approval["choices"][0]["message"]["tool_calls"][0]
        self.assertEqual(json.loads(approval_call["function"]["arguments"])["code"], "print('tinyagent-lab')")
        done = model.complete({"messages": [
            {"role": "user", "content": "LAB_APPROVAL_123"},
            {"role": "assistant", "content": None, "tool_calls": [approval_call]},
            {"role": "tool", "tool_call_id": approval_call["id"], "content": '{"stdout":"tinyagent-lab\\n"}'},
        ]})
        self.assertIn("tinyagent-lab", done["choices"][0]["message"]["content"])

    def test_mention_requires_explicit_operator_hex_and_media_is_visible(self):
        operator = "a" * 64
        mentioned = model.complete({"messages": [{"role": "user", "content": f"LAB_MENTION_123 @{operator}"}]})
        self.assertEqual(mentioned["choices"][0]["message"]["content"], f"LAB_DONE LAB_MENTION_123 @{operator}")
        absent = model.complete({"messages": [{"role": "user", "content": "LAB_MENTION_124 @operator"}]})
        self.assertEqual(absent["choices"][0]["message"]["content"], "LAB_DONE LAB_MENTION_124")
        media = model.complete({"messages": [{"role": "user", "content": "LAB_MEDIA_123 [file: tinyagent-attachment-notes.txt]"}]})
        self.assertEqual(media["choices"][0]["message"]["content"], "LAB_DONE LAB_MEDIA_123 MEDIA_RECEIVED tinyagent-attachment-notes.txt")

    def test_http_health_models_and_sse(self):
        server = model.ThreadingHTTPServer(("127.0.0.1", 0), model.Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            conn = HTTPConnection("127.0.0.1", server.server_port)
            conn.request("GET", "/healthz")
            self.assertEqual(conn.getresponse().status, 200)
            conn.close()
            conn = HTTPConnection("127.0.0.1", server.server_port)
            body = json.dumps({"model": "tiny-lab", "stream": True, "messages": [{"role": "user", "content": "LAB_HELLO_sse"}]}).encode()
            conn.request("POST", "/v1/chat/completions", body, {"Content-Type": "application/json"})
            response = conn.getresponse()
            payload = response.read().decode()
            self.assertEqual(response.status, 200)
            self.assertIn('LAB_DONE LAB_HELLO_sse', payload)
            self.assertIn("data: [DONE]", payload)
            conn.close()
        finally:
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    unittest.main()
