"""A model server for the end-to-end run: answers like llama-server, slowly.

llama-swap starts it as a model's runtime (`${fake-server} --port N --model
X ...`) and checks /health. A chat completion streams `max_tokens` chunks,
one every `FAKE_DELAY` seconds (0.1), so a test can act while a reply is
still being written, and ends with llama-server's `timings`. It never touches
the GPU.
"""

import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DELAY = float(os.environ.get("FAKE_DELAY", "0.1"))


def arg(name, default=None):
    if name in sys.argv:
        return sys.argv[sys.argv.index(name) + 1]
    return default


PORT = int(arg("--port"))
MODEL = arg("--model", "")


def timings(n):
    """llama-server's account of a request, with fixed numbers a test can find."""
    return {"cache_n": 4, "prompt_n": 12, "prompt_ms": 30.0, "prompt_per_second": 400.0,
            "predicted_n": n, "predicted_ms": DELAY * 1000 * n, "predicted_per_second": 1 / DELAY}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def send_json(self, code, body):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path == "/health":
            self.send_json(200, {"status": "ok"})
        elif self.path == "/v1/models":
            self.send_json(200, {"data": [{"id": MODEL, "object": "model"}]})
        elif self.path == "/args":
            self.send_json(200, {"argv": sys.argv[1:]})
        else:
            self.send_json(404, {"error": "not found"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        n = int(body.get("max_tokens", 5))
        if not body.get("stream"):
            time.sleep(DELAY * n)
            self.send_json(200, {"choices": [{"message": {"role": "assistant", "content": "x" * n}}], "timings": timings(n)})
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        def chunk(data):
            raw = f"data: {data}\n\n".encode()
            self.wfile.write(f"{len(raw):x}\r\n".encode() + raw + b"\r\n")
            self.wfile.flush()

        try:
            for i in range(n):
                chunk(json.dumps({"choices": [{"delta": {"content": str(i)}}]}))
                time.sleep(DELAY)
            chunk(json.dumps({"choices": [{"delta": {}, "finish_reason": "stop"}], "timings": timings(n)}))
            chunk("[DONE]")
            self.wfile.write(b"0\r\n\r\n")
        except (BrokenPipeError, ConnectionResetError):
            pass


ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
