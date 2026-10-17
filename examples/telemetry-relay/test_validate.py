"""The list API omits payloads; the validator must inspect candidate todos."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class ValidatorTests(unittest.TestCase):
    def test_reads_payload_and_acknowledgement_from_get_todo(self):
        self.check_acknowledgement({"validation": "acknowledged"}, True)

    def test_accepts_json_string_acknowledgement(self):
        self.check_acknowledgement('{"validation":"acknowledged"}', True)

    def test_rejects_wrong_or_malformed_acknowledgement(self):
        for value in ("not JSON", '{"validation":"superseded"}', []):
            with self.subTest(value=value):
                self.check_acknowledgement(value, False)

    def check_acknowledgement(self, acknowledgement, expect_pass):
        todo, calls = {}, []

        class Server(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_DELETE(self):
                self.send_response(200)
                self.end_headers()

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if self.path == "/v1/logs":
                    summary = body["resourceLogs"][0]["scopeLogs"][0]["logRecords"][0]["body"]["stringValue"]
                    todo.update(id="td_probe", source="telemetry", kind="webhook", state="done",
                                title=summary, payload={"service": "telemetry-relay.validation", "summary": summary},
                                result=acknowledgement)
                    result = {}
                elif body["method"] == "tools/call":
                    tool = body["params"]["name"]
                    calls.append(tool)
                    if tool == "list_todos":
                        data = {"todos": [{k: todo[k] for k in ("id", "source", "kind", "state", "title")}]}
                    elif tool == "get_todo":
                        data = todo
                    else:
                        raise AssertionError(tool)
                    result = {"jsonrpc": "2.0", "id": body["id"],
                              "result": {"structuredContent": data}}
                else:
                    result = {"jsonrpc": "2.0", "id": body.get("id"), "result": {}}
                self.send_response(200)
                self.send_header("Mcp-Session-Id", "test-session")
                self.end_headers()
                self.wfile.write(json.dumps(result).encode())

        server = ThreadingHTTPServer(("127.0.0.1", 0), Server)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        with tempfile.TemporaryDirectory() as tmp:
            grant = Path(tmp) / "grant.json"
            origin = f"http://127.0.0.1:{server.server_port}"
            grant.write_text(json.dumps({"mcp_url": origin + "/mcp/test", "token": "test-token"}))
            result = subprocess.run([sys.executable, str(Path(__file__).with_name("validate.py")),
                                     "--vend-record", str(grant), "--otlp-url", origin + "/v1/logs",
                                     "--timeout", "3"], env=os.environ,
                                    capture_output=True, text=True, timeout=10)
        if expect_pass:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("PASS:", result.stdout)
        else:
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("FAIL:", result.stdout)
            self.assertNotIn("Traceback", result.stderr)
        self.assertIn("get_todo", calls)
        self.assertNotIn("test-token", result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
