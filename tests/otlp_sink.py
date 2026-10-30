"""Tiny HTTPS OTLP sink for local tests. Records one JSON line per request:
{"path", "authorization", "body"} where body is the decoded JSON request.
Usage: otlp_sink.py <port> <certfile> <keyfile> <outfile> [post_status]
GET /api/v1/{nodes,namespaces,pods} answer like a kube-apiserver list with
limit=1 (3 nodes, 5 namespaces, 1 pod) for the probe test, pretty-printed as
the API server answers curl."""
import gzip
import http.server
import json
import ssl
import sys

port, cert, key, out = int(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4]
post_status = int(sys.argv[5]) if len(sys.argv) > 5 else 200
LISTS = {
    "/api/v1/nodes": {"kind": "NodeList", "metadata": {"remainingItemCount": 2}, "items": [{"metadata": {"name": "n1"}}]},
    "/api/v1/namespaces": {"kind": "NamespaceList", "metadata": {"remainingItemCount": 4}, "items": [{"metadata": {"name": "a"}}]},
    "/api/v1/pods": {"kind": "PodList", "metadata": {}, "items": [{"metadata": {"name": "p"}}]},
}


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = LISTS.get(self.path.split("?")[0])
        data = json.dumps(body or {}, indent=2).encode()
        self.send_response(200 if body else 404)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n)
        if self.headers.get("Content-Encoding") == "gzip":
            raw = gzip.decompress(raw)
        try:
            body = json.loads(raw or b"{}")
        except ValueError:
            body = {"_undecoded_bytes": len(raw)}
        with open(out, "a", encoding="utf-8") as f:
            f.write(json.dumps({"path": self.path,
                                "authorization": self.headers.get("Authorization"),
                                "body": body}) + "\n")
        self.send_response(post_status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *args):
        pass


srv = http.server.ThreadingHTTPServer(("0.0.0.0", port), Handler)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
srv.serve_forever()
