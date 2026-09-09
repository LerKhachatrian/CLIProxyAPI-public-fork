"""Windows binary-level passive quota-monitor staging; synthetic and loopback only.

Run with an immutable candidate, fresh absolute evidence directory and the two
staging ports. The fake generation endpoint emits a known assistant reply and
quota headers. Manual-only monitor calls must retain that exact observation
after the documented coalesced flush and an abrupt candidate restart, with no
extra upstream requests or unchanged-observation cache writes.
CONNECT is denied and counted: even an accidental provider check cannot escape
the synthetic loopback proxy. Optional initial-inventory proof expects one
deliberately denied synthetic monitoring attempt, then verifies its durable
failure/cadence across restart. No installed router, auth file or live key is used.
"""
from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import threading
import time
from urllib.parse import urlsplit


MONITOR = "/v0/management/codex/quota-monitor"
ACK = "SYNTHETIC_MONITOR_STAGE_ACK"
KEY = "synthetic-monitor-staging-only"
MANUAL = {"policy": {"usage_seconds": 0, "reset_seconds": 0, "gap_seconds": 10}}


def validate(candidate: Path, evidence: Path, proxy_port: int, capture_port: int):
    if os.name != "nt":
        raise ValueError("This executable staging command requires Windows")
    if not candidate.is_absolute() or not evidence.is_absolute():
        raise ValueError("Candidate and evidence paths must be absolute")
    if not candidate.is_file() or candidate.suffix.lower() != ".exe" or evidence.exists():
        raise ValueError("An existing candidate and fresh evidence directory are required")
    installed = Path(os.environ["USERPROFILE"]) / ".codex/cliproxy/codex-router/bin/cliproxyapi.exe"
    if candidate.resolve() == installed.resolve():
        raise ValueError("Never execute the installed live image as a fixture")
    if {proxy_port, capture_port} != {48318, 48319}:
        raise ValueError("Only distinct staging ports 48318 and 48319 are permitted")
    for port in (proxy_port, capture_port):
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", port))


def request(port, method, path, body=None, *, authenticated=True, raw=None, decode_json=True):
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
    headers = {"Content-Type": "application/json"}
    if authenticated:
        headers["Authorization"] = "Bearer " + KEY
    data = raw if raw is not None else json.dumps(body) if body is not None else None
    try:
        connection.request(method, path, data, headers)
        response = connection.getresponse()
        payload = response.read(2 * 1024 * 1024 + 1)
        if len(payload) > 2 * 1024 * 1024:
            raise AssertionError("Staging response exceeded the bounded contract")
        return response.status, json.loads(payload) if decode_json else payload
    finally:
        connection.close()


def cache_fingerprint(directory):
    paths = list(directory.iterdir())
    assert len(paths) <= 130 and all(path.is_file() for path in paths)
    result = {}
    for path in paths:
        if path.name == "owner.lock":
            continue
        assert path.stat().st_size <= 128 * 1024
        data = path.read_bytes()
        assert KEY.encode() not in data and ACK.encode() not in data
        result[path.name] = (hashlib.sha256(data).hexdigest(), path.stat().st_mtime_ns)
    return result


class Fixture(HTTPServer):
    def __init__(self, port):
        self.generations = 0
        self.forbidden = 0
        self.allow_initial_inventory = False
        self.initial_inventory_attempts = 0
        self.arm()
        super().__init__(("127.0.0.1", port), FixtureHandler)

    def arm(self):
        self.started = threading.Event()
        self.release_bootstrap = threading.Event()
        self.first_payload = threading.Event()
        self.release_completion = threading.Event()

    def release(self):
        self.release_bootstrap.set()
        self.release_completion.set()


class FixtureHandler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_CONNECT(self):
        if (self.server.allow_initial_inventory and self.path == "chatgpt.com:443"
                and self.server.initial_inventory_attempts == 0):
            self.server.initial_inventory_attempts += 1
        else:
            self.server.forbidden += 1
        self.send_error(502, "Synthetic fixture refuses external connections")

    def do_GET(self):
        self.server.forbidden += 1
        self.send_error(405)

    def do_POST(self):
        target = urlsplit(self.path)
        if target.hostname not in (None, "127.0.0.1") or target.path not in ("/responses", "/v1/responses"):
            self.server.forbidden += 1
            self.send_error(403)
            return
        length = int(self.headers.get("Content-Length", "0"))
        if not 0 < length <= 4096 or self.server.generations >= 2:
            self.server.forbidden += 1
            self.send_error(400)
            return
        data = self.rfile.read(length)
        if b"SYNTHETIC_MONITOR_STAGE_REQUEST" not in data:
            self.server.forbidden += 1
            self.send_error(400)
            return
        self.server.generations += 1
        self.server.started.set()
        if not self.server.release_bootstrap.wait(10):
            self.server.forbidden += 1
            self.send_error(504)
            return
        payload = {"type": "response.completed", "response": {
            "id": "resp_synthetic_monitor", "object": "response", "status": "completed",
            "model": "gpt-5.6-sol", "output": [{"type": "message", "role": "assistant",
                "content": [{"type": "output_text", "text": ACK}]}],
            "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}}
        prefix = b'data: {"type":"response.output_text.delta","delta":"synthetic"}\n\n'
        encoded = ("data: " + json.dumps(payload) + "\n\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(prefix) + len(encoded)))
        # The first generation supplies useful passive quota. The held stream
        # deliberately supplies none: activity must not manufacture freshness.
        if self.server.generations == 1:
            for name, used, minutes, reset in (("primary", 25, 300, 15000), ("secondary", 40, 10080, 400000)):
                self.send_header(f"x-codex-{name}-used-percent", str(used))
                self.send_header(f"x-codex-{name}-window-minutes", str(minutes))
                self.send_header(f"x-codex-{name}-reset-after-seconds", str(reset))
            self.send_header("x-codex-plan-type", "pro")
        self.end_headers()
        self.wfile.write(prefix)
        self.wfile.flush()
        self.server.first_payload.set()
        if not self.server.release_completion.wait(10):
            self.server.forbidden += 1
            return
        self.wfile.write(encoded)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--candidate", required=True, type=Path)
    parser.add_argument("--evidence-dir", required=True, type=Path)
    parser.add_argument("--proxy-port", type=int, default=48318)
    parser.add_argument("--capture-port", type=int, default=48319)
    parser.add_argument("--initial-inventory", action="store_true",
                        help="Verify paced legacy bootstrap and no failed-attempt replay through a denying proxy")
    args = parser.parse_args()
    validate(args.candidate, args.evidence_dir, args.proxy_port, args.capture_port)
    candidate, evidence = args.candidate.resolve(), args.evidence_dir.resolve()
    evidence.mkdir(parents=True)
    (evidence / "auths").mkdir()
    config = evidence / "config.yaml"
    config.write_text(f'''host: "127.0.0.1"
port: {args.proxy_port}
auth-dir: "{(evidence / 'auths').as_posix()}"
api-keys: ["{KEY}"]
proxy-url: "http://127.0.0.1:{args.capture_port}"
remote-management:
  allow-remote: false
  secret-key: "{KEY}"
  disable-control-panel: true
commercial-mode: true
logging-to-file: false
request-retry: 0
codex-api-key:
  - api-key: "{KEY}"
    base-url: "http://127.0.0.1:{args.capture_port}"
    proxy-url: "http://127.0.0.1:{args.capture_port}"
    models:
      - name: "gpt-5.6-sol"
        alias: "gpt-5.6-sol"
''', encoding="utf-8")
    # Do not inherit ambient storage, database, proxy, token or profile settings.
    safe_names = {"systemroot", "windir", "path", "temp", "tmp", "userprofile",
                  "localappdata", "appdata", "pathext", "comspec", "systemdrive"}
    env = {key: value for key, value in os.environ.items() if key.lower() in safe_names}
    receipt = {"schema": "cliproxy.codex-monitor.staging.v1", "status": "running",
               "candidate_sha256": hashlib.sha256(candidate.read_bytes()).hexdigest(),
               "proxy_port": args.proxy_port, "capture_port": args.capture_port,
               "phases": [], "pids": [], "real_account_requests": 0}
    process = None
    outputs = []
    fixture = Fixture(args.capture_port)
    worker = threading.Thread(target=fixture.serve_forever, name="synthetic-monitor-fixture")
    worker.start()

    def stop():
        nonlocal process
        if process is not None:
            if process.poll() is None:
                process.terminate()  # Exact owned synthetic candidate; also proves crash recovery.
            process.wait(timeout=10)
            process = None

    def start():
        nonlocal process
        output = (evidence / f"candidate-{len(receipt['pids']) + 1}.log").open("wb")
        outputs.append(output)
        process = subprocess.Popen([str(candidate), "-config", str(config), "-local-model"],
            cwd=evidence, env=env, stdout=output, stderr=subprocess.STDOUT,
            creationflags=subprocess.CREATE_NO_WINDOW)
        receipt["pids"].append(process.pid)
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            assert process.poll() is None, "Synthetic candidate exited before readiness"
            try:
                if request(args.proxy_port, "GET", "/healthz")[0] == 200:
                    return
            except (OSError, ValueError, http.client.HTTPException):
                pass
            time.sleep(.1)
        raise AssertionError("Synthetic candidate did not become ready")

    def view(method="GET", body=None):
        status, result = request(args.proxy_port, method, MONITOR, body)
        assert status == 200 and result["schema"] == 1 and len(result["accounts"]) == 1
        assert result["pending"] == 0 and not result["in_flight"] and not result.get("attempted")
        row = result["accounts"][0]
        assert re.fullmatch(r"[0-9a-f]{64}", row["identity"])
        return row

    def held_generation(streamed, previous_usage=None):
        fixture.arm()
        with ThreadPoolExecutor(max_workers=1, thread_name_prefix="synthetic-generation") as client:
            future = client.submit(request, args.proxy_port, "POST", "/v1/responses", {
                "model": "gpt-5.6-sol", "input": "SYNTHETIC_MONITOR_STAGE_REQUEST", "stream": streamed},
                decode_json=not streamed)
            try:
                assert fixture.started.wait(5), "Generation never reached the loopback transport"
                # Actual transport, including before headers/first payload, is
                # activity. Both client lanes are manual-only throughout QA.
                waiting = view("POST", MANUAL)
                assert waiting["activity"] == "active"
                if previous_usage is not None:
                    assert waiting["usage"] == previous_usage
                fixture.release_bootstrap.set()
                assert fixture.first_payload.wait(5), "Synthetic first payload was not sent"
                assert not future.done(), "Held request retired at headers/first payload"
                deadline = None
                for _ in range(10):
                    row = view("POST", MANUAL)
                    assert row["activity"] == "active"
                    if previous_usage is not None:
                        assert row["usage"] == previous_usage
                        current = row["usage_schedule"]["due_at"]
                        assert deadline is None or deadline == current
                        deadline = current
            finally:
                fixture.release()
            status, reply = future.result(timeout=10)
        assert status == 200
        if streamed:
            events = [json.loads(line[6:]) for line in reply.decode().splitlines()
                      if line.startswith("data: ") and line != "data: [DONE]"]
            completed = [event["response"] for event in events if event.get("type") == "response.completed"]
            assert len(completed) == 1, "Missing completed synthetic stream"
            reply = completed[0]
        texts = [part.get("text") for item in reply.get("output", []) if item.get("role") == "assistant"
                 for part in item.get("content", []) if part.get("type") == "output_text"]
        assert texts == [ACK], "Missing actual synthetic assistant reply"
        assert view()["activity"] == "active", "Completion lost recent activity"
        receipt["phases"].append("held-stream-activity-without-new-quota" if streamed else "held-http-bootstrap-activity")

    try:
        start()
        assert request(args.proxy_port, "GET", MONITOR, authenticated=False)[0] == 401
        assert request(args.proxy_port, "POST", MONITOR, raw='{"refresh":"usage","refresh":"resets"}')[0] == 400
        initial = view("POST", MANUAL)
        assert not initial.get("usage") and not initial.get("resets")
        assert initial["activity"] == "likely_next"
        receipt["phases"].append("authenticated-manual-only-empty-observation-no-check")
        held_generation(False)
        observed = view()
        usage = observed["usage"]
        assert usage["source"] == "passive" and usage["plan_type"] == "pro"
        assert {window["kind"]: window["used_percent"] for window in usage["windows"]} == {"weekly": 40, "five_hour": 25}
        assert not observed.get("resets")
        captured = datetime.fromisoformat(usage["observed_at"])
        assert 0 <= (datetime.now(timezone.utc) - captured).total_seconds() < 15
        due = datetime.fromisoformat(observed["usage_schedule"]["due_at"])
        assert 300 <= (due - captured).total_seconds() < 600
        held_generation(True, usage)
        assert view()["usage"] == usage
        cache = evidence / ".config.yaml.quota-monitor-v1"
        # Passive display writes are coalesced for one minute; unlike provider
        # attempt floors they are not synchronously durable on every local view.
        # Prove the real flush boundary before testing abrupt-restart recovery.
        print(json.dumps({"phase": "waiting-for-coalesced-passive-cache-flush", "maximum_seconds": 65}), flush=True)
        deadline = time.monotonic() + 65
        while True:
            assert view()["usage"] == usage
            cache_fingerprint(cache)  # Bounds every file before reading it.
            entries = [json.loads(path.read_bytes()) for path in cache.glob("*.json")]
            if any(entry.get("usage") == usage for entry in entries):
                break
            assert time.monotonic() < deadline, "Passive display cache missed its local-view flush boundary"
            time.sleep(2)
        baseline = cache_fingerprint(cache)
        for _ in range(30):
            assert view("POST", MANUAL)["usage"] == usage
        assert cache_fingerprint(cache) == baseline, "Cached views rewrote unchanged state"
        receipt["phases"].append("normal-response-passive-capture-30-cached-views-no-writes")
        stop()
        start()
        restarted = view("POST", MANUAL)
        assert restarted["usage"] == usage and restarted["activity"] == "likely_next"
        assert restarted["usage_schedule"]["due_at"] == observed["usage_schedule"]["due_at"]
        assert cache_fingerprint(cache) == baseline
        assert fixture.generations == 2 and fixture.forbidden == 0
        receipt.update(status="passed", assistant_ack=ACK, synthetic_generations=fixture.generations,
                       forbidden_external_attempts=fixture.forbidden, monitor_provider_reads=0,
                       monitor_provider_reads_scope="manual-only-phase",
                       cache_files=len(baseline), repeated_views=30, unchanged_cache_writes=0,
                       passive_capture_preserved=True, protected_port_untouched=True,
                       active_fallback_seconds=(due - captured).total_seconds(),
                       activity_runtime_only=True, held_http_and_stream_passed=True)
        receipt["phases"].append("abrupt-restart-keeps-original-capture-and-manual-only")
        if args.initial_inventory:
            # Seed only this stopped, generated fixture. Never edit live or
            # writer-owned cache state to perform an upgrade.
            stop()
            entry_path = cache / (restarted["identity"] + ".json")
            before = cache_fingerprint(cache)
            legacy = json.loads(entry_path.read_bytes())
            assert legacy["schema"] == 1 and not legacy.get("resets")
            lane = legacy["reset_schedule"]
            assert lane["last_attempt"].startswith("0001-") and not lane.get("error")
            legacy_due = (datetime.now(timezone.utc) + timedelta(hours=23)).isoformat()
            lane["due_at"] = legacy_due
            entry_path.write_text(json.dumps(legacy), encoding="utf-8")
            assert process is None and cache_fingerprint(cache) != before
            start()
            manual = view("POST", MANUAL)
            assert datetime.fromisoformat(manual["reset_schedule"]["due_at"]) == datetime.fromisoformat(legacy_due)
            assert fixture.initial_inventory_attempts == fixture.forbidden == 0
            fixture.allow_initial_inventory = True
            auto = {"policy": {"usage_seconds": 0, "reset_seconds": 86400, "gap_seconds": 10}}
            status, checked = request(args.proxy_port, "POST", MONITOR, auto)
            assert status == 200 and checked["attempted"] == "resets" and checked["pending"] == 0
            assert fixture.initial_inventory_attempts == 1 and fixture.forbidden == 0
            checked_row = checked["accounts"][0]
            checked_lane = checked_row["reset_schedule"]
            attempt = datetime.fromisoformat(checked_lane["last_attempt"])
            next_due = datetime.fromisoformat(checked_lane["due_at"])
            assert 0 <= (datetime.now(timezone.utc) - attempt).total_seconds() < 15
            assert 24*3600 <= (next_due - attempt).total_seconds() < 26*3600
            assert checked_lane["error"] == "provider_unavailable" and not checked_row.get("resets")
            assert checked_row["usage"] == usage
            fixed = cache_fingerprint(cache)
            for _ in range(30):
                assert view("POST", auto)["reset_schedule"] == checked_lane
            assert cache_fingerprint(cache) == fixed
            stop()
            start()
            assert view("POST", auto)["reset_schedule"] == checked_lane
            assert fixture.initial_inventory_attempts == 1 and fixture.forbidden == 0
            assert cache_fingerprint(cache) == fixed
            receipt.update(initial_inventory_legacy_upgrade=True, initial_inventory_attempts=1,
                           initial_inventory_successful_provider_reads=0,
                           initial_inventory_denied_at_loopback=True,
                           initial_inventory_failure_restart_preserved=True,
                           initial_inventory_daily_delay_seconds=(next_due-attempt).total_seconds())
            receipt["phases"].append("legacy-initial-inventory-immediate-paced-claim-and-failed-restart-no-replay")
    except BaseException as error:
        receipt.update(status="failed", error_type=type(error).__name__)
        raise
    finally:
        fixture.release()
        stop()
        fixture.shutdown()
        fixture.server_close()
        worker.join(timeout=5)
        for output in outputs:
            output.close()
        receipt["workers_stopped"] = not worker.is_alive() and process is None
        (evidence / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(receipt), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
