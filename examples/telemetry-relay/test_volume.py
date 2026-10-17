"""Volume controls exercised without sockets, against real persisted scan state."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import time
import unittest
import urllib.error
from unittest.mock import patch


class VolumeTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location("relay", Path(__file__).with_name("telemetry-relay.py"))
        self.relay = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.relay)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.relay.SINK_DIR = str(self.root)
        self.relay.STATE_PATH = str(self.root / "seen.json")
        self.relay.WEBHOOK_URL = "http://test.invalid/private"
        self.sent = []
        self.relay.MAX_AGE = 300
        self.relay.GROUP_WINDOW = 900
        self.relay.MAX_PER_HOUR = 8

    def write(self, messages, received=None):
        records = [{"timeUnixNano": str(i + 1), "severityNumber": 17,
                    "body": {"stringValue": message}} for i, message in enumerate(messages)]
        envelope = {"received": time.time() if received is None else received,
                    "body": {"resourceLogs": [{"scopeLogs": [{"logRecords": records}]}]}}
        (self.root / "logs-test.jsonl").write_text(json.dumps(envelope) + "\n")

    def scan(self):
        def receive(request, timeout):
            self.sent.append((request.headers["X-delivery-id"], json.loads(request.data)))
            response = contextlib.nullcontext()
            response.status = 202
            return contextlib.nullcontext(response)
        with patch.object(self.relay.urllib.request, "urlopen", receive), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.relay.scan(True), 0)

    def test_old_exports_never_create_work(self):
        self.write(["historical incident"], time.time() - 86400)
        self.scan()
        self.assertEqual(self.sent, [])

    def test_repeated_failure_and_export_retry_make_one_task(self):
        self.write(["request 123 failed", "request 456 failed"])
        self.scan()
        self.assertEqual(len(self.sent), 1)

        self.assertEqual(self.sent[0][1]["detail"]["occurrences"], 2)
        self.write(["request 123 failed", "request 456 failed", "request 789 failed"])
        self.scan()
        self.assertEqual(len(self.sent), 1)

    def test_export_retry_across_window_boundary_still_makes_one_task(self):
        boundary = (int(time.time() // 900) + 1) * 900
        self.write(["runtime failure"], boundary - 1)
        with patch.object(self.relay.time, "time", return_value=boundary - 1):
            self.scan()
        self.write(["runtime failure"], boundary + 1)
        with patch.object(self.relay.time, "time", return_value=boundary + 1):
            self.scan()
        self.assertEqual(len(self.sent), 1)

    def test_budget_is_shared_across_scans_and_expires(self):
        self.write(["failure " + chr(65 + i) for i in range(12)])
        self.scan()
        self.assertEqual(len(self.sent), 8)
        self.scan()
        self.assertEqual(len(self.sent), 8)
        saved = json.loads(Path(self.relay.STATE_PATH).read_text())
        Path(self.relay.STATE_PATH).write_text(json.dumps({k: time.time() - 3601 for k in saved}))
        self.scan()
        self.assertEqual(len(self.sent), 12)

    def test_legacy_noise_does_not_starve_new_policy_runtime_alerts(self):
        legacy = {f"trace:legacy-{i}": time.time() for i in range(25)}
        Path(self.relay.STATE_PATH).write_text(json.dumps(legacy))
        self.write(["runtime " + chr(65 + i) for i in range(12)])
        self.scan()
        self.assertEqual(len(self.sent), 8)
        self.scan()
        self.assertEqual(len(self.sent), 8)
        saved = json.loads(Path(self.relay.STATE_PATH).read_text())
        self.assertTrue(all(key in saved for key in legacy))

    def test_tool_failures_are_filtered_and_runtime_failure_survives(self):
        spans = [{"spanId": "tool", "name": "telemetry claim failed", "status": {"code": 2},
                  "attributes": [{"key": "agent.tool.name", "value": {"stringValue": "claim"}}]},
                 {"spanId": "runtime", "name": "model timeout", "status": {"code": 2}}]
        envelope = {"received": time.time(), "body": {"resourceSpans": [{"scopeSpans": [{"spans": spans}]}]}}
        (self.root / "traces-test.jsonl").write_text(json.dumps(envelope) + "\n")
        self.scan()
        self.assertEqual([body["summary"] for _, body in self.sent], ["model timeout"])

    def test_failed_delivery_does_not_consume_budget_and_retries_same_group(self):
        self.write(["runtime failure"])
        delivery_ids = []

        def fail(request, timeout):
            delivery_ids.append(request.headers["X-delivery-id"])
            raise urllib.error.HTTPError(request.full_url, 503, "unavailable", {}, None)

        with patch.object(self.relay.urllib.request, "urlopen", fail):
            with self.assertRaises(urllib.error.HTTPError):
                self.relay.scan(True)
        self.assertFalse(Path(self.relay.STATE_PATH).exists())
        self.scan()
        self.assertEqual(self.sent[0][0], delivery_ids[0])
        self.assertEqual(sum(k.startswith("group:") for k in json.loads(Path(self.relay.STATE_PATH).read_text())), 1)

    def test_dry_run_never_consumes_production_budget(self):
        self.write(["runtime failure"])
        with patch.object(self.relay.urllib.request, "urlopen") as sender:
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(self.relay.scan(False), 0)
            sender.assert_not_called()
        self.assertFalse(Path(self.relay.STATE_PATH).exists())
        self.scan()
        self.assertEqual(len(self.sent), 1)

    def test_missing_and_future_receive_times_fail_closed(self):
        for received in (None, "not-a-timestamp", float("nan"), time.time() + 3600):
            self.write(["runtime failure"])
            path = self.root / "logs-test.jsonl"
            envelope = json.loads(path.read_text())
            envelope["received"] = received
            path.write_text(json.dumps(envelope) + "\n")
            self.scan()
        self.assertEqual(self.sent, [])

    def test_tool_error_log_is_filtered_without_filtering_runtime_log(self):
        self.write(["tool failure", "runtime failure"])
        path = self.root / "logs-test.jsonl"
        envelope = json.loads(path.read_text())
        records = envelope["body"]["resourceLogs"][0]["scopeLogs"][0]["logRecords"]
        records[0]["attributes"] = [{"key": "agent.tool.name", "value": {"stringValue": "bash"}}]
        path.write_text(json.dumps(envelope) + "\n")
        self.scan()
        self.assertEqual([body["summary"] for _, body in self.sent], ["runtime failure"])


if __name__ == "__main__":
    unittest.main()
