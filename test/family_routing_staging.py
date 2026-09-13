"""Isolated binary-level family routing verification; synthetic accounts only."""
from __future__ import annotations

import argparse
import asyncio
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import http.client
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import threading
import time
from urllib.parse import urlsplit
import uuid

from aiohttp import web, WSMsgType
import yaml

KEY = "synthetic-family-staging-only"
ACK = "SYNTHETIC_FAMILY_STAGE_ACK"
PORT = 48318
CAPTURE = 48319
ACCOUNTS = {"a": 10, "b": 20, "c": 30, "d": 40, "exhausted": 100}


def digest(path):
    hasher = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            hasher.update(block)
    return hasher.hexdigest()


def safe_environment():
    allowed = {"systemroot", "windir", "path", "temp", "tmp", "userprofile", "localappdata", "appdata", "pathext", "comspec", "systemdrive"}
    return {key: value for key, value in os.environ.items() if key.lower() in allowed}


def assert_free(port):
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", port))


def validate_inputs(args):
    if os.name != "nt":
        raise ValueError("This executable staging command requires Windows")
    if not args.candidate.is_absolute() or not args.output.is_absolute():
        raise ValueError("Candidate and output paths must be absolute")
    candidate, output = args.candidate.resolve(), args.output.resolve()
    installed = Path(os.environ["USERPROFILE"]) / ".codex/cliproxy/codex-router/bin/cliproxyapi.exe"
    if candidate == installed.resolve():
        raise ValueError("Never execute the installed live image as a fixture")
    if not candidate.is_file() or candidate.suffix.lower() != ".exe" or output.exists():
        raise ValueError("An existing executable and fresh output directory are required")
    if bool(args.core) != bool(args.catalog):
        raise ValueError("Native core and model catalog must be supplied together")
    for path in (args.core, args.catalog):
        if path and (not path.is_absolute() or not path.is_file()):
            raise ValueError("Native inputs must be existing absolute files")
    if args.core and not (args.core.parent / "codex-code-mode-host.exe").is_file():
        raise ValueError("The native core requires its version-matched code-mode companion")
    if args.widget_root and (not args.widget_root.is_absolute() or not (args.widget_root / "app/reset_center/gateway.py").is_file()):
        raise ValueError("Widget source must be an absolute checkout with the real gateway")
    for port in (PORT, CAPTURE):
        assert_free(port)
    return candidate, output


def request(method, path, body=None, headers=None, timeout=15):
    connection = http.client.HTTPConnection("127.0.0.1", PORT, timeout=timeout)
    actual_headers = {"Content-Type": "application/json", "Authorization": "Bearer " + KEY}
    actual_headers.update(headers or {})
    try:
        connection.request(method, path, json.dumps(body) if body is not None else None, actual_headers)
        response = connection.getresponse()
        payload = response.read(8 * 1024 * 1024 + 1)
        assert len(payload) <= 8 * 1024 * 1024
        return response.status, payload
    finally:
        connection.close()


def json_request(method, path, body=None, headers=None, expected=200):
    status, payload = request(method, path, body, headers)
    assert status == expected, f"{method} {path} returned {status}: {payload[:300]!r}"
    return json.loads(payload)


def set_strategy(strategy):
    json_request("PUT", "/v0/management/routing/strategy", {"value": strategy})
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        status = json_request("GET", "/v0/management/routing/session-affinity")
        if status.get("effective_strategy") == strategy:
            return status
        time.sleep(.02)
    raise AssertionError("Requested routing strategy did not become effective: " + strategy)


def identity_headers(family, thread_id=None, parent=None, **extra):
    thread_id = thread_id or family
    metadata = {"session_id": family, "thread_id": thread_id, "agent_name": "/root" if parent is None else "/root/child", "thread_source": "user" if parent is None else "subagent", "turn_id": str(uuid.uuid4()), "context_window_id": str(uuid.uuid4()), **extra}
    headers = {"Session-Id": family, "Thread-Id": thread_id}
    if parent is not None:
        metadata.update({"parent_thread_id": parent, "subagent_kind": "thread_spawn"})
        headers["X-Codex-Parent-Thread-Id"] = parent
    headers["X-Codex-Turn-Metadata"] = json.dumps(metadata)
    return headers


class Fixture:
    def __init__(self):
        self.loop = asyncio.new_event_loop()
        self.thread = threading.Thread(target=self.run, name="family-staging-capture", daemon=True)
        self.ready = threading.Event()
        self.lock = threading.Lock()
        self.requests = []
        self.forbidden = []
        self.active = Counter()
        self.maximum = Counter()
        self.phase = "calibration"
        self.delay = 0
        self.native_mode = None
        self.native_steps = Counter()
        self.fail_once = set()
        self.break_after_output = set()
        self.websocket_error_after_output = set()
        self.hold = threading.Event()
        self.hold.set()
        self.widget_nonce = uuid.uuid4().hex + uuid.uuid4().hex
        self.management_requests = []
        self.switch_after_status = False
        self.thread.start()
        assert self.ready.wait(5), "Capture server failed to start"

    def run(self):
        asyncio.set_event_loop(self.loop)
        app = web.Application(client_max_size=8 * 1024 * 1024)
        app.router.add_route("*", "/{path:.*}", self.handle)
        self.runner = web.AppRunner(app, access_log=None)
        self.loop.run_until_complete(self.runner.setup())
        self.loop.run_until_complete(web.TCPSite(self.runner, "127.0.0.1", CAPTURE).start())
        self.ready.set()
        self.loop.run_forever()
        self.loop.run_until_complete(self.runner.cleanup())
        self.loop.close()

    def stop(self):
        self.loop.call_soon_threadsafe(self.loop.stop)
        self.thread.join(timeout=10)
        assert not self.thread.is_alive(), "Capture server still running"

    def rows(self, phase=None):
        with self.lock:
            return [dict(row) for row in self.requests if phase is None or row["phase"] == phase]

    def headers(self, account):
        headers = {"x-codex-plan-type": "pro"}
        for kind, used, minutes, seconds in (("primary", 5, 300, 15000), ("secondary", ACCOUNTS[account], 10080, 400000)):
            headers.update({f"x-codex-{kind}-used-percent": str(used), f"x-codex-{kind}-window-minutes": str(minutes), f"x-codex-{kind}-reset-after-seconds": str(seconds)})
        return headers

    def metadata(self, headers, payload):
        client = payload.get("client_metadata") or {}
        raw = client.get("x-codex-turn-metadata") or headers.get("X-Codex-Turn-Metadata", "{}")
        result = json.loads(raw) if isinstance(raw, str) else raw
        return result

    def events(self, account, payload, headers, transport):
        metadata = self.metadata(headers, payload)
        thread_id = metadata.get("thread_id")
        with self.lock:
            sequence = len(self.requests) + 1
            self.requests.append({"sequence": sequence, "phase": self.phase, "account": account, "family": metadata.get("session_id"), "thread_id": thread_id, "parent": metadata.get("parent_thread_id"), "agent_name": metadata.get("agent_name"), "window": metadata.get("context_window_id"), "transport": transport, "model": payload.get("model"), "tier": payload.get("service_tier"), "previous_response_id": payload.get("previous_response_id"), "prewarm": payload.get("generate") is False})
            if payload.get("generate") is not False:
                self.native_steps[thread_id] += 1
            step = self.native_steps[thread_id]
        response_id = "resp_family_stage_" + str(sequence)
        item = {"id": "msg_" + response_id, "type": "message", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": ACK, "annotations": []}]}
        name = metadata.get("agent_name", "/root")
        if self.native_mode and payload.get("generate") is not False and name in {"/root", "/root/child"}:
            child = "child" if name == "/root" else "grandchild"
            if step == 1:
                if self.native_mode == "resume":
                    method = "followup_task"
                    arguments = {"target": child, "message": "Continue the existing synthetic native family check. Reply with the fixture acknowledgment and do no external work."}
                else:
                    method = "spawn_agent"
                    root_id = metadata.get("session_id")
                    ancestry = [{"task_name": "/root", "session_id": root_id, "thread_id": root_id, "depth": 0}]
                    if name != "/root":
                        ancestry.append({"task_name": name, "session_id": thread_id, "thread_id": thread_id, "depth": 1})
                    seed = {"direct_parent_session_id": thread_id, "direct_parent_thread_id": thread_id, "ancestry": ancestry, "depth": len(ancestry), "role": "Synthetic " + child + " family routing check"}
                    arguments = {"task_name": child, "message": json.dumps(seed) + ". This is a local scripted-provider test. The child may create one scripted grandchild, append itself to the ancestry, and propagate it; the grandchild may not delegate. Do no shell, file, credential or external work.", "model": "gpt-6-astra", "reasoning_effort": "max", "fork_turns": "none"}
                item = {"id": "fc_" + response_id, "type": "function_call", "call_id": "call_" + response_id, "namespace": "collaboration", "name": method, "arguments": json.dumps(arguments)}
            elif step == 2:
                item = {"id": "fc_" + response_id, "type": "function_call", "call_id": "call_" + response_id, "namespace": "collaboration", "name": "wait_agent", "arguments": json.dumps({"timeout_ms": 3600000})}
        return [
            {"type": "response.created", "response": {"id": response_id, "object": "response", "status": "in_progress", "output": []}},
            {"type": "response.output_item.done", "output_index": 0, "item": item},
            {"type": "response.completed", "response": {"id": response_id, "object": "response", "status": "completed", "output": [item], "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
        ]

    async def handle(self, req):
        if req.path == "/__codex_widget_reset_fixture__" and req.method == "GET":
            return web.json_response({"fixture": "codex-widget-reset-center", "nonce": self.widget_nonce})
        # The real Widget gateway requires its QA marker. Relay only its exact
        # routing surfaces to the isolated real router; never manufacture data.
        management_routes = {
            ("GET", "/v0/management/auth-files"),
            ("GET", "/v0/management/routing/session-affinity"),
            ("PATCH", "/v0/management/auth-files/fields"),
            ("POST", "/v0/management/routing/session-affinity/reset"),
        }
        if (req.method, req.path) in management_routes:
            assert req.headers.get("Authorization") == "Bearer " + KEY
            body = await req.json() if req.can_read_body else None
            headers = {name: req.headers[name] for name in ("X-CLIProxy-Routing-Guard",) if name in req.headers}
            status, payload = await asyncio.to_thread(request, req.method, req.path, body, headers)
            self.management_requests.append({"method": req.method, "path": req.path, "guard": headers.get("X-CLIProxy-Routing-Guard", ""), "status": status})
            if self.switch_after_status and req.path == "/v0/management/routing/session-affinity" and req.method == "GET":
                self.switch_after_status = False
                await asyncio.to_thread(set_strategy, "family-balanced")
            return web.Response(status=status, body=payload, content_type="application/json")
        parts = req.path.strip("/").split("/")
        if len(parts) != 2 or parts[0] not in ACCOUNTS or parts[1] != "responses" or req.method not in {"POST", "GET"}:
            self.forbidden.append({"method": req.method, "path": req.path[:120]})
            return web.Response(status=403, text="External or unexpected route refused")
        account = parts[0]
        if req.headers.get("Upgrade", "").lower() == "websocket":
            socket_response = web.WebSocketResponse(max_msg_size=8 * 1024 * 1024, compress=False)
            socket_response.headers.update(self.headers(account))
            await socket_response.prepare(req)
            async for message in socket_response:
                if message.type != WSMsgType.TEXT:
                    continue
                payload = json.loads(message.data)
                events = self.events(account, payload, req.headers, "websocket")
                if account in self.websocket_error_after_output:
                    self.websocket_error_after_output.remove(account)
                    await socket_response.send_json(events[0])
                    await socket_response.send_json({"type": "response.output_text.delta", "delta": "synthetic WebSocket partial output"})
                    # Permit actual downstream delivery before the terminal frame.
                    await asyncio.sleep(.1)
                    await socket_response.send_json({"type": "error", "status": 429, "body": {"error": {"type": "usage_limit_reached", "message": "Synthetic post-output limit", "resets_in_seconds": 3600}}})
                    continue
                for event in events:
                    await socket_response.send_json(event)
            return socket_response
        if req.method != "POST":
            self.forbidden.append({"method": req.method, "path": req.path})
            return web.Response(status=405)
        payload = await req.json()
        events = self.events(account, payload, req.headers, "http")
        self.active[account] += 1
        self.maximum[account] = max(self.maximum[account], self.active[account])
        try:
            hold_deadline = time.monotonic() + 15
            while not self.hold.is_set():
                assert time.monotonic() < hold_deadline, "Synthetic upstream hold exceeded its bound"
                await asyncio.sleep(.01)
            if self.delay:
                await asyncio.sleep(self.delay)
            if account in self.fail_once:
                self.fail_once.remove(account)
                return web.json_response({"error": {"type": "usage_limit_reached", "code": "usage_limit_reached", "message": "Synthetic account usage limit", "resets_in_seconds": 3600}}, status=429, headers=self.headers(account))
            response = web.StreamResponse(status=200, headers={"Content-Type": "text/event-stream", **self.headers(account)})
            await response.prepare(req)
            if account in self.break_after_output:
                self.break_after_output.remove(account)
                await response.write(b'data: {"type":"response.output_text.delta","delta":"synthetic partial output"}\n\n')
            else:
                for event in events:
                    await response.write(("event: " + event["type"] + "\ndata: " + json.dumps(event) + "\n\n").encode())
            await response.write_eof()
            return response
        finally:
            self.active[account] -= 1


def main():
    if not __debug__:
        raise ValueError("Run this assertion-based verifier without Python optimization")
    parser = argparse.ArgumentParser()
    parser.add_argument("--candidate", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--core", type=Path)
    parser.add_argument("--catalog", type=Path)
    parser.add_argument("--widget-root", type=Path)
    parser.add_argument("--calibrate-only", action="store_true")
    args = parser.parse_args()
    candidate, output = validate_inputs(args)
    output.mkdir(parents=True)
    auth_dir = output / "auths"
    auth_dir.mkdir()
    (auth_dir / "synthetic-guard-target.json").write_text(json.dumps({"type": "codex", "email": "guard@example.invalid", "access_token": KEY, "expired": "2099-01-01T00:00:00Z", "disabled": True, "priority": 600}), encoding="utf-8")
    config = {"host": "127.0.0.1", "port": PORT, "auth-dir": str(auth_dir), "api-keys": [KEY], "remote-management": {"allow-remote": False, "secret-key": KEY, "disable-control-panel": True}, "commercial-mode": True, "logging-to-file": False, "request-retry": 0, "routing": {"strategy": "round-robin", "session-affinity": True, "family": {"state-file": str(output / "family-routing.json"), "max-concurrent-per-account": 4, "max-queued-per-account": 32, "admission-timeout": "5s"}}, "codex-api-key": []}
    for index, account in enumerate(ACCOUNTS):
        config["codex-api-key"].append({"api-key": KEY + "-" + account, "base-url": f"http://127.0.0.1:{CAPTURE}/{account}", "prefix": "seed-" + account, "priority": (index + 1) * 100, "websockets": True, "models": [{"name": model, "alias": model} for model in ("gpt-6-astra", "gpt-5.6-sol")]})
    config_path = output / "config.yaml"
    config_path.write_text(yaml.safe_dump(config, sort_keys=False), encoding="utf-8")
    environment = safe_environment()
    # Any unexpected default-network access is sent to the refusing local fixture.
    environment.update({"HTTP_PROXY": f"http://127.0.0.1:{CAPTURE}", "HTTPS_PROXY": f"http://127.0.0.1:{CAPTURE}", "NO_PROXY": "127.0.0.1,localhost"})
    receipt = {"schema": "family-routing.binary-staging.v1", "started_at": datetime.now(timezone.utc).isoformat(), "helper_sha256": digest(__file__), "candidate": str(candidate), "candidate_sha256": digest(candidate), "config_initial_sha256": digest(config_path), "calibration_only": args.calibrate_only, "ports": [PORT, CAPTURE], "pids": [], "phases": [], "real_provider_requests": 0, "failures": [], "passed": False}
    if args.core:
        receipt["native_inputs"] = {str(path.resolve()): digest(path) for path in (args.core, args.core.parent / "codex-code-mode-host.exe", args.catalog)}
    if args.widget_root:
        receipt["widget_inputs"] = {name: digest(args.widget_root / name) for name in ("app/cliproxy_quota_client.py", "app/reset_center/gateway.py", "app/reset_center/safety.py")}
    fixture = Fixture()
    process = None
    streams = []

    def phase(name, **evidence):
        receipt["phases"].append({"name": name, **evidence})
        print(json.dumps({"phase": name, **evidence}), flush=True)

    def start():
        nonlocal process
        stream = (output / f"candidate-{len(receipt['pids']) + 1}.log").open("wb")
        streams.append(stream)
        process = subprocess.Popen([str(candidate), "-config", str(config_path), "-local-model"], cwd=output, env=environment, stdout=stream, stderr=subprocess.STDOUT, creationflags=subprocess.CREATE_NO_WINDOW)
        receipt["pids"].append(process.pid)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            assert process.poll() is None, "Staging candidate exited before readiness"
            try:
                if request("GET", "/healthz", timeout=1)[0] == 200:
                    return
            except (OSError, ValueError, http.client.HTTPException):
                pass
            time.sleep(.1)
        raise AssertionError("Staging candidate did not become ready")

    def stop():
        nonlocal process
        if process is not None:
            if process.poll() is None:
                process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
            process = None

    def send(family, model="gpt-6-astra", thread_id=None, parent=None, tier=None, streamed=False):
        body = {"model": model, "input": "Synthetic family routing fixture", "stream": streamed}
        if tier:
            body["service_tier"] = tier
        status, payload = request("POST", "/v1/responses", body, identity_headers(family, thread_id, parent))
        assert status == 200, f"Synthetic generation returned {status}: {payload[:300]!r}"
        if streamed:
            events = [json.loads(line[6:]) for line in payload.decode().splitlines() if line.startswith("data: ") and line != "data: [DONE]"]
            completed = [event["response"] for event in events if event.get("type") == "response.completed"]
            assert len(completed) == 1
            result = completed[0]
        else:
            result = json.loads(payload)
        texts = [part.get("text") for item in result.get("output", []) if item.get("role") == "assistant" for part in item.get("content", []) if part.get("type") == "output_text"]
        assert texts == [ACK], "Missing actual synthetic assistant acknowledgment"

    native_identity = None

    def routing_status():
        return json_request("GET", "/v0/management/routing/session-affinity")["family_routing"]

    def await_status(predicate, explanation, timeout=3):
        deadline = time.monotonic() + timeout
        latest = None
        while time.monotonic() < deadline:
            latest = routing_status()
            if predicate(latest):
                return latest
            time.sleep(.02)
        raise AssertionError(explanation + ": " + repr(latest))

    @contextmanager
    def widget_gateway():
        assert args.widget_root and args.widget_root.is_absolute()
        widget_root = args.widget_root.resolve()
        assert (widget_root / "app/reset_center/gateway.py").is_file()
        environment_before = dict(os.environ)
        path_before = list(sys.path)
        try:
            sys.path.insert(0, str(widget_root))
            from app.cliproxy_quota_client import CLIProxyQuotaClient
            from app.qa_profile import activate_qa_profile
            from app.reset_center.safety import QA_NONCE_ENV
            activate_qa_profile(output / "widget-qa-profile")
            os.environ[QA_NONCE_ENV] = fixture.widget_nonce
            client = CLIProxyQuotaClient(base_url=f"http://127.0.0.1:{CAPTURE}", management_key=KEY, timeout=5)
            yield client.create_reset_center_gateway()
        finally:
            os.environ.clear()
            os.environ.update(environment_before)
            sys.path[:] = path_before

    def widget_checks():
        from dataclasses import asdict
        with widget_gateway() as gateway:
            from app.reset_center.safety import ResetActionError
            capability = gateway.affinity()
            assert capability.family_routing_enabled and not capability.legacy_routing_available
            targets = gateway.list_targets()
            target = next(t for t in targets if t.name == "synthetic-guard-target.json")
            original = target.priority
            before = routing_status()
            calls_before = len(fixture.management_requests)
            for operation in (gateway.rebind, lambda: gateway.patch_priority(target, original + 100, legacy_routing=True)):
                try:
                    operation()
                except ResetActionError:
                    pass
                else:
                    raise AssertionError("Widget authorized legacy routing during family mode")
            new_calls = fixture.management_requests[calls_before:]
            assert all(row["method"] == "GET" for row in new_calls)
            gateway.patch_priority(target, original + 100)
            assert next(t.priority for t in gateway.list_targets() if t.name == target.name) == original + 100
            gateway.patch_priority(target, original)
            assert next(t.priority for t in gateway.list_targets() if t.name == target.name) == original
            after = routing_status()
            assert (before["assigned_families"], before["members"]) == (after["assigned_families"], after["members"])
            # The real client must carry the guard even after a positive GET.
            # Activate family mode between the returned legacy GET and PATCH.
            set_strategy("round-robin")
            fixture.switch_after_status = True
            try:
                gateway.patch_priority(target, original + 100, legacy_routing=True)
            except ResetActionError as exc:
                assert exc.status_code == 409
            else:
                raise AssertionError("A stale Widget compatibility read authorized a priority write")
            assert fixture.management_requests[-1] == {"method": "PATCH", "path": "/v0/management/auth-files/fields", "guard": "legacy", "status": 409}
            assert next(t.priority for t in gateway.list_targets() if t.name == target.name) == original
            assert gateway.affinity().family_routing_enabled
            phase("actual-widget-gateway-family-preflight-manual-priority-and-mode-race", capability=asdict(capability), real_router_relay=True, mutations="two verified reversible synthetic priority writes; guarded race rejected")

    def queue_checks(family):
        fixture.phase = "bounded-queue-cancellation"
        fixture.hold.clear()
        connections = []
        count_before = len(fixture.rows(fixture.phase))
        def open_request():
            child = str(uuid.uuid4())
            connection = http.client.HTTPConnection("127.0.0.1", PORT, timeout=8)
            connections.append(connection)
            headers = {"Authorization": "Bearer " + KEY, "Content-Type": "application/json", **identity_headers(family, child, family)}
            connection.request("POST", "/v1/responses", json.dumps({"model": "gpt-6-astra", "input": "Synthetic bounded queue", "stream": False}), headers)
            return connection
        try:
            for _ in range(4):
                open_request()
            await_status(lambda s: s["in_flight"] == 4, "Four requests did not occupy the account")
            queued = [open_request() for _ in range(32)]
            saturated = await_status(lambda s: s["queued"] == 32, "Account queue did not reach its configured bound")
            overflow = open_request()
            response = overflow.getresponse()
            body = response.read(65536)
            assert response.status == 503 and b"family_queue_full" in body, (response.status, body[:300])
            overflow.close()
            for connection in queued:
                connection.close()
            await_status(lambda s: s["queued"] == 0 and s["in_flight"] == 4, "Queued cancellation leaked admission state")
            assert len(fixture.rows(fixture.phase)) - count_before == 4, "A queued or overflow request reached upstream"
            for connection in connections[:4]:
                connection.close()
            await_status(lambda s: s["queued"] == 0 and s["in_flight"] == 0, "Active cancellation leaked slots")
            fixture.hold.set()
            # Wait for the synthetic upstream's own cancelled handlers to retire.
            deadline = time.monotonic() + 3
            while any(fixture.active.values()) and time.monotonic() < deadline:
                time.sleep(.02)
            assert not any(fixture.active.values())
            send(family)
            phase("account-concurrency-queue-bound-and-cancellation", saturated=saturated, rejected_before_upstream=33, post_cancel_request_succeeded=True)
        finally:
            fixture.hold.set()
            for connection in connections:
                connection.close()

    def failover_checks(family):
        fixture.phase = "pre-output-failover"
        send(family)
        current = fixture.rows(fixture.phase)[-1]["account"]
        fixture.fail_once.add(current)
        count_before = len(fixture.rows(fixture.phase))
        send(family)
        attempts = fixture.rows(fixture.phase)[count_before:]
        assert len(attempts) == 2 and attempts[0]["account"] == current and attempts[1]["account"] != current, attempts
        replacement = attempts[1]["account"]
        child = str(uuid.uuid4())
        send(family, "gpt-5.6-sol", child, family, "priority", True)
        assert fixture.rows(fixture.phase)[-1]["account"] == replacement
        phase("pre-output-failure-moves-whole-family", previous_account=current, replacement_account=replacement, attempts=2)
        fixture.phase = "emitted-output-no-replay"
        fixture.break_after_output.add(replacement)
        code, payload = request("POST", "/v1/responses", {"model": "gpt-6-astra", "input": "Synthetic partial stream", "stream": True}, identity_headers(family))
        assert code == 200 and payload.count(b"synthetic partial output") == 1, (code, payload[:300])
        assert len(fixture.rows(fixture.phase)) == 1, "Stream was replayed after emitted output"
        assert b'"type":"response.completed"' not in payload and b'"type": "response.completed"' not in payload
        phase("emitted-stream-output-is-never-replayed", upstream_attempts=1, partial_output_copies=1)

    def websocket_rebind_checks():
        from websockets.exceptions import ConnectionClosed
        from websockets.sync.client import connect
        fixture.phase = "websocket-pinned-family-rebind"
        family = str(uuid.uuid4())
        headers = {"Authorization": "Bearer " + KEY, **identity_headers(family)}
        metadata = {"session_id": family, "thread_id": family, "x-codex-turn-metadata": headers["X-Codex-Turn-Metadata"]}
        body = {"type": "response.create", "model": "gpt-6-astra", "input": [{"role": "user", "content": [{"type": "input_text", "text": "Synthetic WebSocket family rebind"}]}], "client_metadata": metadata}
        def completed(connection):
            for _ in range(12):
                event = json.loads(connection.recv(timeout=8))
                assert event.get("type") != "error", event
                if event.get("type") == "response.completed":
                    return event["response"]["id"]
            raise AssertionError("WebSocket response exceeded the fixture event bound")
        with connect(f"ws://127.0.0.1:{PORT}/v1/responses", additional_headers=headers, open_timeout=5, close_timeout=2, compression=None) as connection:
            connection.send(json.dumps(body))
            previous = completed(connection)
            first = fixture.rows(fixture.phase)
            assert len(first) == 1 and first[0]["transport"] == "websocket"
            account = first[0]["account"]
            fixture.fail_once.add(account)
            send(family)
            attempts = fixture.rows(fixture.phase)
            assert len(attempts) == 3 and attempts[1]["account"] == account and attempts[2]["account"] != account
            replacement = attempts[2]["account"]
            connection.send(json.dumps({**body, "previous_response_id": previous}))
            try:
                unexpected = connection.recv(timeout=8)
            except ConnectionClosed as exc:
                assert exc.rcvd and exc.rcvd.code == 1012 and "requires HTTP replay" in exc.rcvd.reason, str(exc)
            else:
                raise AssertionError("Pinned continuation was replayed or returned an ordinary event: " + str(unexpected)[:300])
            assert len(fixture.rows(fixture.phase)) == 3, "Invalid pinned continuation reached an upstream account"
        with connect(f"ws://127.0.0.1:{PORT}/v1/responses", additional_headers=headers, open_timeout=5, close_timeout=2, compression=None) as connection:
            connection.send(json.dumps(body))
            resumed = completed(connection)
            final = fixture.rows(fixture.phase)
            assert len(final) == 4 and final[-1]["account"] == replacement
            assert final[-1]["previous_response_id"] is None
            phase("pinned-websocket-generation-change-requires-complete-replay", previous_account=account, replacement_account=replacement, refused_before_upstream=True, reconnect_succeeded=True)
            fixture.phase = "websocket-post-output-error"
            fixture.websocket_error_after_output.add(replacement)
            connection.send(json.dumps({**body, "previous_response_id": resumed}))
            output_events = []
            for _ in range(12):
                try:
                    event = json.loads(connection.recv(timeout=8))
                except ConnectionClosed as exc:
                    # Credential/quota terminal handling intentionally closes TCP
                    # without a close frame. That is observable 1006, not a 1012
                    # instruction to replay the turn on another account.
                    close_frame_received = exc.rcvd is not None
                    close_code = exc.rcvd.code if close_frame_received else 1006
                    assert close_code != 1012, "Post-output error requested a full replay: " + str(exc)
                    break
                output_events.append(event)
            else:
                raise AssertionError("Post-output WebSocket did not terminate within the event bound")
            assert sum(event.get("delta") == "synthetic WebSocket partial output" for event in output_events) == 1
            assert all(event.get("type") != "response.completed" for event in output_events)
            assert len(fixture.rows(fixture.phase)) == 1, "WebSocket stream was replayed after emitted output"
            phase("post-output-websocket-error-does-not-request-server-replay", upstream_attempts=1, partial_output_copies=1, close_code=close_code, close_frame_received=close_frame_received, client_auto_retry="not exercised by this raw transport check")

    def native_run(mode):
        nonlocal native_identity
        assert args.core and args.catalog
        profile = output / "native-profile"
        work = output / "native-workspace"
        if mode == "create":
            profile.mkdir()
            work.mkdir()
            native_config = '\n'.join([
                'approval_policy = "never"', 'default_permissions = ":danger-full-access"',
                'model = "gpt-6-astra"', 'model_reasoning_effort = "max"', 'model_provider = "family_stage"',
                'model_catalog_json = ' + json.dumps(str(args.catalog)),
                '[features]', 'code_mode = true', 'code_mode_only = true', 'multi_agent = true', 'multi_agent_v2 = true',
                '[model_providers.family_stage]', 'name = "Synthetic family staging"',
                f'base_url = "http://127.0.0.1:{PORT}/v1"', 'wire_api = "responses"',
                'requires_openai_auth = false', 'supports_websockets = true', 'env_key = "FAMILY_STAGE_PROXY_KEY"',
                'request_max_retries = 0', 'stream_max_retries = 0',
            ]) + '\n'
            (profile / "config.toml").write_text(native_config, encoding="utf-8")
        native_env = safe_environment()
        native_env.update({"CODEX_HOME": str(profile), "CODEX_SQLITE_HOME": str(profile), "CODEX_THREAD_IDLE_UNLOAD_DELAY_SECS": "15", "FAMILY_STAGE_PROXY_KEY": KEY})
        fixture.phase = "native-family-" + mode
        fixture.native_steps.clear()
        fixture.native_mode = mode
        command = [str(args.core.resolve()), "exec", "--skip-git-repo-check", "--json", "--color", "never", "-C", str(work)]
        if mode == "resume":
            command += ["resume", "--skip-git-repo-check", native_identity["family"]]
        command += ["This is a local synthetic family staging check. Reply with the fixture acknowledgment and do no external work."]
        child_process = subprocess.Popen(command, cwd=work, env=native_env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, creationflags=subprocess.CREATE_NO_WINDOW)
        try:
            stdout, stderr = child_process.communicate(timeout=70)
        except subprocess.TimeoutExpired:
            child_process.kill()
            stdout, stderr = child_process.communicate(timeout=10)
            raise
        finally:
            fixture.native_mode = None
        (output / f"native-{mode}.jsonl").write_bytes(stdout)
        (output / f"native-{mode}.stderr.txt").write_bytes(stderr)
        assert child_process.returncode == 0, f"Native fixture exited {child_process.returncode}"
        rows = fixture.rows("native-family-" + mode)
        generated = [row for row in rows if not row["prewarm"]]
        nodes = {row["agent_name"]: row for row in generated}
        assert set(nodes) == {"/root", "/root/child", "/root/child/grandchild"}, f"Native family incomplete: {set(nodes)}"
        family = nodes["/root"]["thread_id"]
        assert all(row["family"] == family for row in rows)
        assert nodes["/root/child"]["parent"] == family
        assert nodes["/root/child/grandchild"]["parent"] == nodes["/root/child"]["thread_id"]
        assert len({row["account"] for row in rows}) == 1
        assert all(row["transport"] == "websocket" for row in rows), "Native WS silently used an HTTP fallback"
        identity = {"family": family, "account": rows[0]["account"], "nodes": {name: row["thread_id"] for name, row in nodes.items()}}
        if mode == "create":
            native_identity = identity
        else:
            assert identity == native_identity, "Native cold-resume family or account changed"
        phase("native-websocket-family-" + mode, native_pid=child_process.pid, core_sha256=digest(args.core), requests=len(rows), **identity)

    try:
        start()
        fixture.phase = "legacy-quota-seeding"
        for account in ACCOUNTS:
            send(str(uuid.uuid4()), model="seed-" + account + "/gpt-6-astra")
        seeds = fixture.rows("legacy-quota-seeding")
        assert Counter(row["account"] for row in seeds) == Counter({a: 1 for a in ACCOUNTS})
        monitor = json_request("GET", "/v0/management/codex/quota-monitor")
        observed = [a.get("usage") for a in monitor["accounts"] if a.get("usage")]
        assert len(observed) == 5
        weekly = sorted(w["used_percent"] for usage in observed for w in usage["windows"] if w["kind"] == "weekly")
        assert weekly == sorted(ACCOUNTS.values()), f"Unexpected seeded weekly observations: {weekly}"
        phase("synthetic-five-account-quota-calibration", weekly_used_percent=weekly)
        if args.calibrate_only:
            if args.core:
                native_run("create")
                native_run("resume")
            assert not fixture.forbidden
            receipt["passed"] = True
        else:
            set_strategy("family-balanced")
            status = json_request("GET", "/v0/management/routing/session-affinity")
            assert status["family_routing_enabled"] and status["routing_guard_supported"]
            fixture.phase = "twenty-families"
            fixture.delay = .06
            families = [str(uuid.uuid4()) for _ in range(20)]
            with ThreadPoolExecutor(max_workers=20) as pool:
                futures = [pool.submit(send, family) for family in families]
                for future in futures:
                    future.result(timeout=20)
            fixture.delay = 0
            roots = fixture.rows("twenty-families")
            assert len(roots) == 20
            allocation = {row["family"]: row["account"] for row in roots}
            distribution = Counter(allocation.values())
            assert distribution == Counter({a: 5 for a in "abcd"}), f"Family distribution: {distribution}"
            assert max(fixture.maximum.values()) <= 4
            phase("twenty-families-across-four-accounts", families_by_account=dict(distribution), max_upstream_concurrency=dict(fixture.maximum))
            fixture.phase = "descendants-and-mixed-models"
            with ThreadPoolExecutor(max_workers=20) as pool:
                futures = []
                for family in families:
                    child, grandchild = str(uuid.uuid4()), str(uuid.uuid4())
                    futures.append(pool.submit(send, family, "gpt-5.6-sol", child, family, "priority", True))
                    futures.append(pool.submit(send, family, "gpt-6-astra", grandchild, child, None, False))
                for future in futures:
                    future.result(timeout=20)
            descendants = fixture.rows("descendants-and-mixed-models")
            assert len(descendants) == 40 and all(row["account"] == allocation[row["family"]] for row in descendants)
            phase("forty-descendants-retain-family-across-models-and-tiers", requests=len(descendants))
            before_guard = json_request("GET", "/v0/management/auth-files")
            json_request("PATCH", "/v0/management/auth-files/fields", {"name": "synthetic-guard-target.json", "priority": 700}, {"X-CLIProxy-Routing-Guard": "legacy"}, expected=409)
            json_request("POST", "/v0/management/routing/session-affinity/reset", headers={"X-CLIProxy-Routing-Guard": "legacy"}, expected=409)
            after_guard = json_request("GET", "/v0/management/auth-files")
            before_priorities = {a["name"]: a.get("priority") for a in before_guard["files"]}
            after_priorities = {a["name"]: a.get("priority") for a in after_guard["files"]}
            assert before_priorities == after_priorities
            assert json_request("GET", "/v0/management/routing/session-affinity")["family_routing"]["assigned_families"] == 20
            phase("legacy-automatic-writes-and-reset-rejected-with-zero-effects")
            if args.widget_root:
                widget_checks()
            if args.core:
                native_run("create")
            fixture.phase = "cold-router-restart"
            stop()
            start()
            for family in families:
                send(family)
            resumed = fixture.rows("cold-router-restart")
            assert len(resumed) == 20 and all(row["account"] == allocation[row["family"]] for row in resumed)
            phase("cold-router-restart-retains-twenty-family-assignments")
            if args.core:
                native_run("resume")
            fixture.phase = "manual-rebind"
            before = json_request("GET", "/v0/management/routing/session-affinity")["family_routing"]
            reset = json_request("POST", "/v0/management/routing/session-affinity/reset")
            after = json_request("GET", "/v0/management/routing/session-affinity")["family_routing"]
            expected_families = 20 + bool(args.core)
            assert reset["cleared_family_assignments"] == expected_families and after["assigned_families"] == 0 and after["members"] == before["members"]
            for family in families:
                send(family)
            assert len(fixture.rows("manual-rebind")) == 20
            phase("manual-rebind-preserves-membership-and-resumes")
            queue_checks(families[0])
            failover_checks(families[0])
            websocket_rebind_checks()
            family_rows = [row for row in fixture.rows() if row["phase"] != "legacy-quota-seeding"]
            assert all(row["account"] != "exhausted" for row in family_rows), "Confirmed weekly-exhausted account received a family-mode attempt"
            fixture.phase = "legacy-rollback"
            set_strategy("round-robin")
            assert not json_request("GET", "/v0/management/routing/session-affinity")["family_routing_enabled"]
            send(str(uuid.uuid4()))
            assert len(fixture.rows("legacy-rollback")) == 1
            phase("legacy-strategy-rollback-remains-usable")
            assert not fixture.forbidden, f"Unexpected upstream routes: {fixture.forbidden}"
            receipt["passed"] = True
    except Exception as exc:
        receipt["failures"].append(type(exc).__name__ + ": " + str(exc)[:1800])
    finally:
        cleanup_failures = []
        for label, cleanup in (("candidate", stop), ("capture", fixture.stop)):
            try:
                cleanup()
            except Exception as exc:
                cleanup_failures.append(label + ": " + type(exc).__name__ + ": " + str(exc)[:300])
        for stream in streams:
            stream.close()
        ports_released = True
        for port in (PORT, CAPTURE):
            try:
                assert_free(port)
            except OSError as exc:
                ports_released = False
                cleanup_failures.append("port " + str(port) + ": " + type(exc).__name__)
        if cleanup_failures:
            receipt["passed"] = False
            receipt["failures"].extend(cleanup_failures)
        receipt.update({"finished_at": datetime.now(timezone.utc).isoformat(), "requests": fixture.rows(), "forbidden_routes": fixture.forbidden, "owned_processes_stopped": (process is None or process.poll() is not None) and not fixture.thread.is_alive(), "ports_released": ports_released})
        (output / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({key: value for key, value in receipt.items() if key != "requests"}, indent=2))
    return 0 if receipt["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
