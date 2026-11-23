"""Offline unit tests for the example AGY usage recorder."""
import importlib.util
import pathlib
import unittest

MODULE_PATH = pathlib.Path(__file__).with_name("record-usage.py")
SPEC = importlib.util.spec_from_file_location("record_usage", MODULE_PATH)
recorder = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(recorder)


class RecorderTests(unittest.TestCase):
    def test_success_metadata_only(self):
        data = {
            "status": "SUCCESS", "response": "secret prompt/response",
            "conversation_id": "reused-conversation",
            "usage": {
                "input_tokens": 9557, "output_tokens": 54,
                "thinking_tokens": 45, "cache_read_tokens": 0,
            },
        }
        env = {"PI_SESSION_ID": "session-1", "PI_SESSION_FILE": "/local/private.jsonl"}
        event = recorder.make_event(data, "gemini-4-hydrogen-high", env, 1795395592000)
        self.assertEqual(event["input_tokens"], 9557)
        self.assertEqual(event["output_tokens"], 54)
        self.assertEqual(event["session_id"], "session-1")
        self.assertEqual(event["session_file"], "/local/private.jsonl")
        self.assertNotIn("response", event)
        self.assertNotIn("thinking_tokens", event)
        self.assertNotIn("conversation_id", event)
        self.assertEqual(event["ts"], 1795395592000)

    def test_each_invocation_gets_unique_id(self):
        data = {"status": "SUCCESS", "conversation_id": "same",
                "usage": {"input_tokens": 1}}
        a = recorder.make_event(data, "example", {})
        b = recorder.make_event(data, "example", {})
        self.assertNotEqual(a["id"], b["id"])

    def test_refuse_failed_or_missing_usage(self):
        self.assertIsNone(recorder.make_event({"status": "ERROR", "usage": {"input_tokens": 5}}, "x", {}))
        self.assertIsNone(recorder.make_event({"status": "SUCCESS", "usage": {}}, "x", {}))
        self.assertIsNone(recorder.make_event({"status": "SUCCESS"}, "x", {}))


if __name__ == "__main__":
    unittest.main()
