"""Loopback TLS fixture for the SDK and release downloads on disposable CI runners."""
import base64
import json
import pathlib
import shutil
import ssl
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

root = pathlib.Path(sys.argv[1])
scale_set_labels = []


class Handler(BaseHTTPRequestHandler):
    def reply(self, status, body=None):
        self.send_response(status)
        if body is not None:
            self.send_header("Content-Type", "application/json")
        self.end_headers()
        if body is not None:
            self.wfile.write(json.dumps(body).encode())

    def do_POST(self):
        path = urlsplit(self.path).path
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        if path.endswith(("/access_tokens", "/registration-token")):
            return self.reply(201, {"token": "fixture", "expires_at": "2099-01-01T00:00:00Z"})
        if path == "/actions/runner-registration":
            claims = base64.urlsafe_b64encode(json.dumps({"exp": int(time.time()) + 3600}).encode())
            header = base64.urlsafe_b64encode(b'{"alg":"RS256","typ":"JWT"}')
            token = ".".join((header.decode().rstrip("="), claims.decode().rstrip("="), "eA"))
            return self.reply(200, {"url": "https://api.github.com", "token": token})
        if path.endswith("/sessions"):
            return self.reply(200, {
                "sessionId": "12345678-1234-1234-1234-123456789abc",
                "statistics": {},
                "messageQueueUrl": "https://api.github.com/queue",
                "messageQueueAccessToken": "fixture",
            })
        self.reply(404)

    def do_GET(self):
        path = urlsplit(self.path).path.rstrip("/")
        if path.endswith("/runnerscalesets"):
            name = parse_qs(urlsplit(self.path).query).get("name", ["ephemera-ubuntu-amd64"])[0]
            return self.reply(200, {"count": 1, "value": [{"id": 1, "name": name, "labels": scale_set_labels}]})
        if path == "/queue":
            time.sleep(1)
            return self.reply(204)
        prefix = "/illusions-lab/Ephemera/releases/"
        if path == prefix + "latest":
            tag = (root / "latest").read_text().strip()
            self.send_response(302)
            self.send_header("Location", prefix + "tag/" + tag)
            self.end_headers()
            return
        if path.startswith(prefix + "tag/"):
            return self.reply(200)
        if path.startswith(prefix + "download/"):
            parts = path.split("/")
            if len(parts) == 7 and parts[-1] in ("checksums.txt", "ephemera_linux_amd64.tar.gz", "ephemera_linux_arm64.tar.gz"):
                asset = root / parts[-2] / parts[-1]
                if asset.is_file():
                    self.send_response(200)
                    self.end_headers()
                    with asset.open("rb") as source:
                        shutil.copyfileobj(source, self.wfile)
                    return
        self.reply(404)

    def do_PATCH(self):
        path = urlsplit(self.path).path.rstrip("/")
        payload = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        if path.endswith("/runnerscalesets/1"):
            scale_set_labels[:] = payload["labels"]
            return self.reply(200, {"id": 1, "labels": scale_set_labels})
        self.reply(404)

    def do_DELETE(self):
        self.reply(204)


server = ThreadingHTTPServer(("127.0.0.1", 443), Handler)
tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
tls.load_cert_chain(root / "tls.pem", root / "tls.key")
server.socket = tls.wrap_socket(server.socket, server_side=True)
server.serve_forever()
