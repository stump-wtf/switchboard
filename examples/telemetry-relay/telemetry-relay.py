#!/usr/bin/env python3
"""telemetry-relay — distill error signals from the otlp-sink JSONL into a switchboard telemetry webhook.

The station's otlp-sink.py receives OTLP and appends one export per line to rotating JSONL under
~/.local/state/otlp-sink, compressing rotated files. This relay reads active and compressed files,
selects error spans and logs, redacts common credential forms, and POSTs bounded telemetry
envelopes to a Switchboard `telemetry` webhook. Stable event IDs make retries idempotent; repeated
failures are grouped into 15-minute windows with an eight-delivery hourly budget.
Only exports received within five minutes are eligible; missing receive timestamps fail closed.

Modes:
  --dry-run (default)  parse and print what it WOULD send; state is not advanced, nothing is POSTed
  --send               POST and record every emitted signal so each fires at most once

Env:
  TELEMETRY_WEBHOOK_URL  ingest URL of the switchboard telemetry webhook (required for --send)
  OTLP_SINK_DIR          where otlp-sink writes JSONL (default ~/.local/state/otlp-sink)
  TELEMETRY_STATE        path to the seen-key state file (default ~/.local/state/telemetry-relay/seen.json)
  TELEMETRY_MIN_SEVERITY  ERROR (default) or FATAL; numeric OTLP severity takes precedence
  TELEMETRY_MAX_AGE_SECONDS  300; reject older or undated exports (0 enables replay)
  TELEMETRY_GROUP_SECONDS    900; group similar failures (0 disables grouping)
  TELEMETRY_MAX_PER_HOUR     8; rolling budget persisted across scans

Stdlib only, like otlp-sink. Safe to cron: it reads-only in dry-run, and in --send a bounded
30-day state file plus Switchboard's delivery-ID dedup make retries idempotent.
"""
import argparse
import fcntl
import gzip
import hashlib
import json
import os
import re
import sys
import time
import math
import urllib.request
import urllib.error

SINK_DIR = os.path.expanduser(os.environ.get("OTLP_SINK_DIR", "~/.local/state/otlp-sink"))
WEBHOOK_URL = os.environ.get("TELEMETRY_WEBHOOK_URL", "")
STATE_PATH = os.path.expanduser(os.environ.get("TELEMETRY_STATE", "~/.local/state/telemetry-relay/seen.json"))
MIN_SEVERITY = (os.environ.get("TELEMETRY_MIN_SEVERITY", "ERROR") or "ERROR").upper()

# OTLP severity number cutoffs: ERROR = 17, FATAL = 21.
SEVERITY_MIN = {"ERROR": 3, "FATAL": 4}.get(MIN_SEVERITY)
if SEVERITY_MIN is None:
    raise ValueError("TELEMETRY_MIN_SEVERITY must be ERROR or FATAL")
ERROR_STATUS_CODE = 2  # OTLP StatusStatusCode_ERROR
SEEN_TTL_SECONDS = 30 * 24 * 60 * 60  # longer than the sink's 14-day source retention
MAX_AGE = int(os.environ.get("TELEMETRY_MAX_AGE_SECONDS", "300"))
GROUP_WINDOW = int(os.environ.get("TELEMETRY_GROUP_SECONDS", "900"))
MAX_PER_HOUR = int(os.environ.get("TELEMETRY_MAX_PER_HOUR", "8"))
if MAX_AGE < 0 or GROUP_WINDOW < 0 or MAX_PER_HOUR < 1:
    raise ValueError("invalid telemetry volume settings")
SUMMARY_LIMIT = 240
SECRET_VALUE = re.compile(
    r"(?i)\b(password|passwd|secret|token|api[_-]?key|authorization|access[_-]?key)\b[\"']?"
    r"(\s*[:=]\s*)(?:\"[^\"]*\"|'[^']*'|[^\s,;]+)"
)
BEARER_VALUE = re.compile(r"(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+")
JWT_VALUE = re.compile(r"\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b")


def safe_text(value, limit=SUMMARY_LIMIT):
    text = str(value or "")
    text = SECRET_VALUE.sub(r"\1\2[REDACTED]", text)
    text = BEARER_VALUE.sub("Bearer [REDACTED]", text)
    text = JWT_VALUE.sub("[REDACTED JWT]", text)
    text = "".join(ch if ch >= " " and ch != "\x7f" else " " for ch in text)
    text = " ".join(text.split())
    return text[:limit]


def stable_key(kind, *parts):
    identity = "\0".join(str(part or "") for part in parts).encode()
    return f"{kind}:{hashlib.sha256(identity).hexdigest()}"


def severity_rank(number, text):
    # OTLP's numeric ranges are authoritative when supplied. Text handles emitters that omit them.
    try:
        number = int(number or 0)
    except (TypeError, ValueError):
        number = 0
    if 21 <= number <= 24:
        return 4
    if 17 <= number <= 20:
        return 3
    if number:
        return 0
    return {"ERROR": 3, "CRITICAL": 4, "FATAL": 4}.get(text, 0)


def iter_trace_errors(body, source_id):
    if SEVERITY_MIN > 3:
        return
    for rs in body.get("resourceSpans", []) or []:
        service = {a["key"]: a.get("value", {}).get("stringValue", "")
                   for a in (rs.get("resource", {}).get("attributes", []) or [])}.get("service.name", "")
        for ss in rs.get("scopeSpans", []) or []:
            for index, sp in enumerate(ss.get("spans", []) or []):
                status = sp.get("status", {}) or {}
                if status.get("code") != ERROR_STATUS_CODE:
                    continue
                attrs = {a["key"]: a.get("value", {}) for a in (sp.get("attributes") or [])}
                if "agent.tool.name" in attrs:
                    continue  # a failed tool call is routine agent work, not a station failure
                yield {
                    "service": safe_text(service, 80),
                    "signal": "trace",
                    "severity": "ERROR",
                    "summary": safe_text(status.get("message", "") or sp.get("name", "") or "span error"),
                    "detail": {"traceId": sp.get("traceId", ""), "spanId": sp.get("spanId", "")},
                    "key": stable_key("trace", service, sp.get("traceId"), sp.get("spanId"),
                                      source_id if not sp.get("spanId") else "", index,
                                      sp.get("startTimeUnixNano"), sp.get("name"), status.get("message")),
                }


def iter_log_errors(body, source_id):
    for rl in body.get("resourceLogs", []) or []:
        service = {a["key"]: a.get("value", {}).get("stringValue", "")
                   for a in (rl.get("resource", {}).get("attributes", []) or [])}.get("service.name", "")
        for sl in rl.get("scopeLogs", []) or []:
            for index, rec in enumerate(sl.get("logRecords", []) or []):
                if any(a.get("key") == "agent.tool.name" for a in rec.get("attributes", []) or []):
                    continue
                sev_num = rec.get("severityNumber", 0) or 0
                sev_text = (rec.get("severityText", "") or "").upper()
                rank = severity_rank(sev_num, sev_text)
                if rank < SEVERITY_MIN:
                    continue
                msg = rec.get("body", {}).get("stringValue", "") if isinstance(rec.get("body"), dict) else str(rec.get("body", ""))
                if not msg:
                    msg = sev_text or "log error"
                # Timestamp plus content dedups exporter retries while allowing the same message
                # to create a fresh todo when it happens at a later time. If the record has no
                # timestamp, its stable JSONL position distinguishes occurrences across scans.
                timestamp = rec.get("timeUnixNano") or rec.get("observedTimeUnixNano")
                if timestamp in ("0", 0):
                    timestamp = None
                identity = (service, timestamp, sev_num, msg) if timestamp else (source_id, index, service, sev_num, msg)
                severity = "FATAL" if rank == 4 else "ERROR"
                yield {
                    "service": safe_text(service, 80),
                    "signal": "log",
                    "severity": severity,
                    "summary": safe_text(msg),
                    "detail": {"severityNumber": sev_num},
                    "key": stable_key("log", *identity),
                }


def load_seen():
    try:
        with open(STATE_PATH) as f:
            saved = json.load(f)
            if not isinstance(saved, dict):
                raise ValueError("relay state must be a JSON object")
            now = time.time()
            return {
                key: (value if isinstance(value, (int, float)) else now)
                for key, value in saved.items()
                if now - (value if isinstance(value, (int, float)) else now) < SEEN_TTL_SECONDS
            }
    except FileNotFoundError:
        return {}


def save_seen(seen):
    d = os.path.dirname(STATE_PATH) or "."
    os.makedirs(d, exist_ok=True)
    tmp = STATE_PATH + ".tmp"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(seen, f)
    os.replace(tmp, STATE_PATH)


def emit(seen, signal, send):
    if signal["key"] in seen:
        return False
    body = json.dumps(
        {k: signal[k] for k in ("service", "signal", "severity", "summary", "detail")}
    ).encode()
    if send:
        req = urllib.request.Request(
            WEBHOOK_URL, data=body, method="POST",
            headers={"Content-Type": "application/json", "X-Delivery-Id": signal["key"]},
        )
        with urllib.request.urlopen(req, timeout=20) as resp:
            if not 200 <= resp.status < 300:
                raise RuntimeError(f"webhook returned HTTP {resp.status}")
    else:
        print(f"WOULD-SEND {body.decode()}")
    seen[signal["key"]] = time.time()
    if send:
        # Preserve acknowledged deliveries if a later POST fails or the process stops.
        save_seen(seen)
    return True


def scan(send):
    seen = load_seen()
    found = 0
    emitted = 0
    stale = 0
    limited = 0
    groups = {}
    occurrence_keys = set()
    now = time.time()
    try:
        names = sorted(os.listdir(SINK_DIR))
    except FileNotFoundError:
        print(f"relay: sink directory not found: {SINK_DIR}", file=sys.stderr)
        return 1
    for name in names:
        if not (name.endswith(".jsonl") or name.endswith(".jsonl.gz")):
            continue
        path = os.path.join(SINK_DIR, name)
        signal = name.split("-", 1)[0]
        opener = gzip.open if name.endswith(".gz") else open
        with opener(path, "rt") as f:
            for line_number, line in enumerate(f, 1):
                line = line.strip()
                if not line:
                    continue
                try:
                    rec = json.loads(line)
                except ValueError:
                    continue
                if not isinstance(rec, dict) or not isinstance(rec.get("body"), dict):
                    continue
                body = rec["body"]
                # Compression renames a source file; it does not create a new occurrence.
                source_id = f"{name.removesuffix('.gz')}:{line_number}"
                if signal == "traces":
                    errors = iter_trace_errors(body, source_id)
                elif signal == "logs":
                    errors = iter_log_errors(body, source_id)
                else:
                    continue
                for e in errors:
                    found += 1
                    received = rec.get("received")
                    try:
                        received = float(received)
                    except (TypeError, ValueError):
                        received = None
                    if MAX_AGE and (received is None or not math.isfinite(received) or
                                    received < now - MAX_AGE or received > now + 60):
                        stale += 1
                        continue
                    if e["key"] in seen or "seen:" + e["key"] in seen or e["key"] in occurrence_keys:
                        continue
                    occurrence_keys.add(e["key"])
                    if GROUP_WINDOW:
                        fingerprint = re.sub(r"\b[0-9a-f]{16,}\b|\b\d+\b", "#", e["summary"], flags=re.I)
                        bucket = int((received if received is not None else now) // GROUP_WINDOW)
                        key = stable_key("group", e["service"], e["signal"], e["severity"], fingerprint, bucket)
                    else:
                        key = e["key"]
                    occurrence_key = e["key"]
                    if key not in groups:
                        e["_occurrences"] = []
                        e["key"] = key
                        e["detail"] = dict(e["detail"], occurrences=0, windowSeconds=GROUP_WINDOW)
                        groups[key] = e
                    groups[key]["_occurrences"].append(occurrence_key)
                    groups[key]["detail"]["occurrences"] += 1
    for e in groups.values():
        if e["key"] in seen:
            if GROUP_WINDOW:
                seen.update({"seen:" + k: now for k in e["_occurrences"]})
            continue
        # Persisted delivery timestamps enforce the budget across runs and restarts.
        recent = sum(now - value < 3600 for key, value in seen.items()
                     if key.startswith("group:") or (not GROUP_WINDOW and not key.startswith("seen:")))
        if recent >= MAX_PER_HOUR:
            limited += 1
            continue
        emitted += bool(emit(seen, e, send))
        if GROUP_WINDOW:
            seen.update({"seen:" + k: now for k in e["_occurrences"]})
            if send:
                save_seen(seen)
    if send:
        save_seen(seen)
    print(f"relay: {found} matching signals; {emitted} {'delivered' if send else 'would send'}; "
          f"{found - emitted} already seen or filtered; {len(seen)} retained IDs; "
          f"{stale} stale; {len(groups)} groups; {limited} budget deferred")
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--send", action="store_true")
    modes.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    if args.send and not WEBHOOK_URL:
        print("relay: TELEMETRY_WEBHOOK_URL is required for --send", file=sys.stderr)
        return 1
    try:
        if not args.send:
            return scan(False)
        os.umask(0o077)
        os.makedirs(os.path.dirname(STATE_PATH) or ".", mode=0o700, exist_ok=True)
        with open(STATE_PATH + ".lock", "a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                print("relay: another send is running", file=sys.stderr)
                return 1
            return scan(True)
    except urllib.error.HTTPError as err:
        print(f"relay: webhook returned HTTP {err.code}; unacknowledged delivery will retry", file=sys.stderr)
    except urllib.error.URLError:
        # Exception text may include the token-bearing webhook URL.
        print("relay: webhook connection failed; unacknowledged delivery will retry", file=sys.stderr)
    except (OSError, ValueError, RuntimeError) as err:
        print(f"relay: {type(err).__name__}; scan failed (check sink and state)", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
