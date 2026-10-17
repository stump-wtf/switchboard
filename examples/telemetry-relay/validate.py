#!/usr/bin/env python3
"""Require an OTLP probe to reach the scheduled relay, queue, and worker.

The worker must acknowledge probes as described in README.md. This validator
never runs the relay itself: a stopped or incorrectly configured job must fail.
"""
import argparse
import json
from pathlib import Path
import time
import urllib.error
import urllib.request
import uuid


class MCP:
    def __init__(self, url, token):
        self.url = url
        self.headers = {"Authorization": "Bearer " + token,
                        "Content-Type": "application/json",
                        "Accept": "application/json, text/event-stream"}

    def request(self, method, params=None, request_id=1):
        body = {"jsonrpc": "2.0", "method": method}
        if request_id is not None:
            body["id"] = request_id
        if params is not None:
            body["params"] = params
        with urllib.request.urlopen(urllib.request.Request(
                self.url, data=json.dumps(body).encode(), headers=self.headers), timeout=20) as response:
            if response.headers.get("Mcp-Session-Id"):
                self.headers["Mcp-Session-Id"] = response.headers["Mcp-Session-Id"]
            raw = response.read().decode()
        if not raw.strip():
            return None
        if raw.startswith(("event:", "data:")):
            raw = [line[6:] for line in raw.splitlines() if line.startswith("data: ")][-1]
        out = json.loads(raw)
        if "error" in out:
            raise RuntimeError("MCP request failed")
        return out.get("result")

    def __enter__(self):
        self.request("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                     "clientInfo": {"name": "telemetry-validator", "version": "1"}})
        self.request("notifications/initialized", request_id=None)
        return self

    def __exit__(self, *args):
        try:
            with urllib.request.urlopen(urllib.request.Request(
                    self.url, method="DELETE", headers=self.headers), timeout=10):
                pass
        except (OSError, urllib.error.HTTPError):
            pass

    def tool(self, name, arguments):
        result = self.request("tools/call", {"name": name, "arguments": arguments})
        if result.get("isError"):
            raise RuntimeError("MCP tool failed")
        if result.get("structuredContent") is not None:
            return result["structuredContent"]
        return json.loads(next(c["text"] for c in result["content"] if c["type"] == "text"))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--vend-record", type=Path, required=True,
                        help="private JSON containing mcp_url and token; never printed")
    parser.add_argument("--mcp-url", help="optional local URL for that same endpoint")
    parser.add_argument("--otlp-url", default="http://127.0.0.1:4318/v1/logs")
    parser.add_argument("--timeout", type=float, default=300)
    parser.add_argument("--probe-id", type=uuid.UUID,
                        help="resume checking an existing probe without injecting another")
    args = parser.parse_args()
    try:
        grant = json.loads(args.vend_record.read_text())
        probe = str(args.probe_id or uuid.uuid4())
        summary = "telemetry relay deployment probe " + probe
        export = {"resourceLogs": [{"resource": {"attributes": [
            {"key": "service.name", "value": {"stringValue": "telemetry-relay.validation"}}
        ]}, "scopeLogs": [{"logRecords": [{
            "timeUnixNano": str(time.time_ns()), "severityNumber": 17,
            "severityText": "ERROR", "body": {"stringValue": summary}
        }]}]}]}
        with MCP(args.mcp_url or grant["mcp_url"], grant["token"]) as mcp:
            if not args.probe_id:
                with urllib.request.urlopen(urllib.request.Request(
                        args.otlp_url, data=json.dumps(export).encode(),
                        headers={"Content-Type": "application/json"}), timeout=20) as response:
                    if not 200 <= response.status < 300:
                        raise RuntimeError("OTLP receiver rejected the probe")
                print("Injected OTLP error probe " + probe, flush=True)
            else:
                print("Checking existing OTLP error probe " + probe, flush=True)
            deadline, last = time.monotonic() + args.timeout, None
            while time.monotonic() < deadline:
                todos = mcp.tool("list_todos", {"limit": 200})["todos"]
                # list_todos deliberately omits payloads. Fetch the small set of
                # candidate IDs instead of mistaking an absent payload for no probe.
                candidates = [mcp.tool("get_todo", {"id": t["id"]}) for t in todos
                              if t.get("source") == "telemetry" and probe in t.get("title", "")]
                matches = [t for t in candidates
                           if (t.get("payload") or {}).get("service") == "telemetry-relay.validation"
                           and (t.get("payload") or {}).get("summary") == summary]
                if len(matches) > 1:
                    raise RuntimeError("probe created duplicate todos")
                if matches:
                    todo = matches[0]
                    state = todo["state"]
                    if state != last:
                        print("Probe todo " + todo["id"] + ": " + state, flush=True)
                        last = state
                    if state == "done":
                        receipt = mcp.tool("get_todo", {"id": todo["id"]})
                        acknowledgement = receipt.get("result")
                        if isinstance(acknowledgement, str):
                            acknowledgement = json.loads(acknowledgement)
                        if not isinstance(acknowledgement, dict) or acknowledgement.get("validation") != "acknowledged":
                            raise RuntimeError("worker completed probe without expected acknowledgement")
                        print("PASS: scheduled relay, durable todo, and worker acknowledgement", flush=True)
                        return 0
                    if state == "failed":
                        raise RuntimeError("worker failed the probe")
                time.sleep(2)
        print("FAIL: no worker acknowledgement before timeout; inspect relay job and queue")
    except urllib.error.HTTPError as err:
        print(f"FAIL: HTTP {err.code}; capability URLs are omitted")
    except (OSError, ValueError, KeyError, RuntimeError) as err:
        # Never emit a network exception's raw text: it can contain credentials.
        print(f"FAIL: {type(err).__name__}; inspect receiver, grant, job, and worker")
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
