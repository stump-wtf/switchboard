"""Exercise the relay against files and an HTTP receiver, including failed delivery."""
import gzip
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

RELAY = Path(__file__).with_name("telemetry-relay.py")


def logs(*records):
    return {"body": {"resourceLogs": [{"resource": {"attributes": [
        {"key": "service.name", "value": {"stringValue": "test-service"}}
    ]}, "scopeLogs": [{"logRecords": list(records)}]}]}}


def record(timestamp="1", number=17, text="ERROR", message="error"):
    out = {"severityNumber": number, "severityText": text,
           "body": {"stringValue": message}}
    if timestamp is not None:
        out["timeUnixNano"] = timestamp
    return out


class RelayTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.sink = self.root / "sink"
        self.sink.mkdir()
        self.state = self.root / "state" / "seen.json"
        self.received = []
        self.responses = []
        received, responses = self.received, self.responses

        class Receiver(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                received.append((self.headers["X-Delivery-Id"], body))
                code = responses.pop(0) if responses else 202
                self.send_response(code)
                self.end_headers()

        server = ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
        self.url = f"http://127.0.0.1:{server.server_port}/private-capability"
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)

    def write(self, data, name="logs-2026-10-11.000.jsonl"):
        path = self.sink / name
        path.write_text(json.dumps(data) + "\n")
        return path

    def run_relay(self, *args, **overrides):
        env = dict(os.environ, OTLP_SINK_DIR=str(self.sink),
                   TELEMETRY_STATE=str(self.state), TELEMETRY_WEBHOOK_URL=self.url,
                   TELEMETRY_MIN_SEVERITY="ERROR", TELEMETRY_MAX_AGE_SECONDS="0",
                   TELEMETRY_GROUP_SECONDS="0", TELEMETRY_MAX_PER_HOUR="8")
        env.update(overrides)
        return subprocess.run([sys.executable, str(RELAY), *args], env=env,
                              capture_output=True, text=True, timeout=10)

    def test_production_policy_skips_history_and_groups_repeats(self):
        old = logs(record(message="old failure")); old["received"] = time.time() - 86400
        self.write(old, "logs-old.jsonl")
        fresh = logs(record(timestamp="1"), record(timestamp="2")); fresh["received"] = time.time()
        self.write(fresh)
        options = dict(TELEMETRY_MAX_AGE_SECONDS="300", TELEMETRY_GROUP_SECONDS="900")
        result = self.run_relay("--send", **options)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.received), 1)
        self.assertEqual(self.received[0][1]["detail"]["occurrences"], 2)
        fresh["body"]["resourceLogs"][0]["scopeLogs"][0]["logRecords"].append(record(timestamp="3"))
        self.write(fresh)
        self.assertEqual(self.run_relay("--send", **options).returncode, 0)
        self.assertEqual(len(self.received), 1)

    def test_hourly_budget_survives_restart(self):
        self.write(logs(*(record(timestamp=str(i), message=f"failure {i}") for i in range(12))))
        self.assertEqual(self.run_relay("--send").returncode, 0)
        self.assertEqual(len(self.received), 8)
        self.assertEqual(self.run_relay("--send").returncode, 0)
        self.assertEqual(len(self.received), 8)

    def test_missing_receive_timestamp_is_not_backfilled(self):
        self.write(logs(record()))
        result = self.run_relay("--send", TELEMETRY_MAX_AGE_SECONDS="300")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.received, [])

    def test_numeric_severity_overrides_text(self):
        self.write(logs(record(number=9, text="ERROR"),
                        record(timestamp="2", number=17, text="INFO")))
        result = self.run_relay("--send")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.received), 1)
        self.assertEqual(self.received[0][1]["severity"], "ERROR")

    def test_fatal_filter_excludes_error_spans_and_accepts_text_fallback(self):
        self.write({"body": {"resourceSpans": [{"scopeSpans": [{"spans": [
            {"spanId": "s", "status": {"code": 2}}
        ]}]}]}}, "traces-2026-10-11.000.jsonl")
        self.write(logs(record(), record(timestamp="2", number=0, text="FATAL")))
        result = self.run_relay("--send", TELEMETRY_MIN_SEVERITY="FATAL")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.received), 1)
        self.assertEqual(self.received[0][1]["severity"], "FATAL")

    def test_tool_call_error_span_is_skipped_but_session_error_is_emitted(self):
        self.write({"body": {"resourceSpans": [{"scopeSpans": [{"spans": [
            {"spanId": "tool", "name": "bash", "status": {"code": 2},
             "attributes": [{"key": "agent.tool.name", "value": {"stringValue": "bash"}}]},
            {"spanId": "session", "name": "model timeout", "status": {"code": 2},
             "events": [{"name": "exception", "timeUnixNano": "1"}]}
        ]}]}]}}, "traces-2026-10-11.000.jsonl")
        result = self.run_relay("--send")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.received), 1)
        self.assertEqual(self.received[0][1]["summary"], "model timeout")

    def test_compression_preserves_timestamp_free_delivery_identity(self):
        path = self.write(logs(record(timestamp=None)))
        self.assertEqual(self.run_relay("--send").returncode, 0)
        with gzip.open(str(path) + ".gz", "wb") as target:
            target.write(path.read_bytes())
        path.unlink()
        result = self.run_relay("--send")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.received), 1)
        self.assertIn("0 delivered; 1 already seen or filtered", result.stdout)

    def test_failed_post_preserves_acknowledgements_and_retries_same_id(self):
        self.write(logs(record(timestamp="1"), record(timestamp="2")))
        self.responses.extend([202, 503])
        first = self.run_relay("--send")
        self.assertNotEqual(first.returncode, 0)
        self.assertNotIn("private-capability", first.stderr)
        self.assertEqual(len(json.loads(self.state.read_text())), 1)
        retry = self.run_relay("--send")
        self.assertEqual(retry.returncode, 0, retry.stderr)
        self.assertEqual(len(self.received), 3)
        self.assertEqual(self.received[1][0], self.received[2][0])
        self.assertEqual(len(json.loads(self.state.read_text())), 2)

    def test_missing_configuration_and_corrupt_state_fail_without_delivery(self):
        self.write(logs(record()))
        self.assertNotEqual(self.run_relay("--send", TELEMETRY_WEBHOOK_URL="").returncode, 0)
        self.state.parent.mkdir()
        self.state.write_text("not-json")
        result = self.run_relay("--send")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.received, [])

    def test_dry_run_does_not_write_state_or_send(self):
        self.write(logs(record(message="token=secret_value Bearer credential_value")))
        result = self.run_relay("--dry-run")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("secret_value", result.stdout)
        self.assertNotIn("credential_value", result.stdout)
        self.assertFalse(self.state.exists())
        self.assertEqual(self.received, [])

    def test_same_message_at_a_later_time_is_new_but_export_retry_is_not(self):
        self.write(logs(record(timestamp="1"), record(timestamp="1"),
                        record(timestamp="2")))
        result = self.run_relay("--send")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.received), 2)
        self.assertNotEqual(self.received[0][0], self.received[1][0])


if __name__ == "__main__":
    unittest.main()
