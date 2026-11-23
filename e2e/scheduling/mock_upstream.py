"""Mock OpenAI-compatible upstreams for end-to-end routing and billing tests.

One process, one HTTP server per port. Each port is a channel's upstream with
its own failure rate and delay, set through POST /__ctl and read through
GET /__stats. Every completion reports usage of 10 prompt and 20 completion tokens.
"""
import json, random, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORTS = [int(p) for p in sys.argv[1:]]
state = {p: {"fail": 0.0, "status": 503, "delay_ms": 0, "hits": 0, "ok": 0, "sessions": {}} for p in PORTS}
lock = threading.Lock()
rng = random.Random(42)

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *args): pass
    def _send(self, code, body, ctype="application/json"):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def do_GET(self):
        port = self.server.server_address[1]
        if self.path == "/__stats":
            with lock:
                return self._send(200, {str(p): {k: v for k, v in s.items()} for p, s in state.items()})
        if self.path.endswith("/models"):
            return self._send(200, {"object": "list", "data": [{"id": "e2e-model", "object": "model"}]})
        self._send(404, {"error": {"message": "not found"}})
    def do_POST(self):
        port = self.server.server_address[1]
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            body = json.loads(raw or b"{}")
        except Exception:
            body = {}
        if self.path == "/__ctl":
            with lock:
                for p in body.get("ports") or [port]:
                    for key in ("fail", "status", "delay_ms"):
                        if key in body:
                            state[p][key] = body[key]
                    if body.get("reset"):
                        state[p].update(hits=0, ok=0, sessions={})
            return self._send(200, {"ok": True})
        with lock:
            s = state[port]
            s["hits"] += 1
            fail = rng.random() < s["fail"]
            status, delay = s["status"], s["delay_ms"]
            session = body.get("prompt_cache_key") or self.headers.get("X-Session") or ""
            if session:
                s["sessions"][session] = s["sessions"].get(session, 0) + 1
        if delay:
            time.sleep(delay / 1000)
        if fail:
            return self._send(status, {"error": {"message": "mock upstream unavailable", "type": "server_error", "code": "mock_unavailable"}})
        with lock:
            state[port]["ok"] += 1
        usage = {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
        model = body.get("model", "e2e-model")
        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Connection", "close")
            self.end_headers()
            def chunk(obj):
                self.wfile.write(b"data: " + json.dumps(obj).encode() + b"\n\n"); self.wfile.flush()
            base = {"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": int(time.time()), "model": model}
            chunk({**base, "choices": [{"index": 0, "delta": {"role": "assistant", "content": "hello "}, "finish_reason": None}]})
            chunk({**base, "choices": [{"index": 0, "delta": {"content": f"from {port}"}, "finish_reason": None}]})
            chunk({**base, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
            chunk({**base, "choices": [], "usage": usage})
            self.wfile.write(b"data: [DONE]\n\n"); self.wfile.flush()
            self.close_connection = True
            return
        self._send(200, {"id": "chatcmpl-mock", "object": "chat.completion", "created": int(time.time()), "model": model,
                         "choices": [{"index": 0, "message": {"role": "assistant", "content": f"hello from {port}"}, "finish_reason": "stop"}],
                         "usage": usage})

servers = [ThreadingHTTPServer(("127.0.0.1", p), Handler) for p in PORTS]
for srv in servers[1:]:
    threading.Thread(target=srv.serve_forever, daemon=True).start()
print("mock upstreams on", PORTS, flush=True)
servers[0].serve_forever()
