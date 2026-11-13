"""Disposable Amp API double, adapted from NapVM's real-CLI experiments.

Only synthetic tokens: bootstrap records the credential hash and returns 401.
This does not implement Amp's thread actor, setup engine or portals.
"""

import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import queue
import threading
import time
import uuid


class AmpMock:
    provider_token = "aop_live_" + "0" * 43

    def __init__(self, host):
        self.jobs, self.completed, self.bootstrap = queue.Queue(), {}, set()
        self.condition = threading.Condition()
        mock = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def reply(self, body, status=200):
                data = json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                try:
                    self.wfile.write(data)
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def do_GET(self):
                if self.path != "/api/orbs/bootstrap":
                    return self.reply({}, 404)
                digest = hashlib.sha256(
                    self.headers.get("Authorization", "").encode()
                ).hexdigest()
                with mock.condition:
                    mock.bootstrap.add(digest)
                    mock.condition.notify_all()
                self.reply({"error": "test stops at bootstrap discovery"}, 401)

            def do_POST(self):
                if self.headers.get("Authorization") != f"Bearer {mock.provider_token}":
                    return self.reply({}, 401)
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                method, params = body["method"], body["params"]
                if self.path != "/api/internal?" + method:
                    return self.reply({}, 404)
                if method == "orbSpawnerConnect":
                    self.reply(
                        {"ok": True, "result": {"connected": True, "other": None}}
                    )
                elif method == "orbSpawnerClaimJob":
                    try:
                        claim = mock.jobs.get(timeout=1)
                    except queue.Empty:
                        claim = None
                    self.reply({"ok": True, "result": {"claim": claim}})
                elif method == "orbSpawnerCompleteJob":
                    with mock.condition:
                        mock.completed[params["jobID"]] = params
                        mock.condition.notify_all()
                    self.reply({"ok": True})
                elif method == "orbSpawnerHeartbeatJob":
                    self.reply(
                        {
                            "ok": True,
                            "result": {
                                "held": True,
                                "claimDeadlineAt": int(time.time() * 1000) + 300000,
                            },
                        }
                    )
                elif method == "orbSpawnerWaitLogRead":
                    self.reply({"ok": True, "result": {"read": None, "closed": True}})
                elif method in (
                    "orbSpawnerDisconnect",
                    "orbSpawnerReleaseClaim",
                    "orbSpawnerSendLogChunk",
                ):
                    self.reply({"ok": True})
                else:
                    self.reply({"error": "unimplemented mock method"}, 404)

        self.server = ThreadingHTTPServer((host, 0), Handler)
        self.url = f"http://{host}:{self.server.server_port}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def submit(self, operation, orb_id, **fields):
        claim = {
            "id": str(uuid.uuid4()),
            "job": {"op": operation, "sandboxId": orb_id, **fields},
            "timeoutMs": 300000,
            "attempt": 1,
            "claimDeadlineAt": int(time.time() * 1000) + 300000,
        }
        self.jobs.put(claim)
        return claim["id"]

    def wait(self, predicate, timeout=180):
        with self.condition:
            if not self.condition.wait_for(predicate, timeout):
                raise TimeoutError("mock API observation did not arrive")

    def result(self, job_id):
        self.wait(lambda: job_id in self.completed)
        return self.completed[job_id]

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
