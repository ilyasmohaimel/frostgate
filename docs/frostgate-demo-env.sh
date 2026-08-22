#!/usr/bin/env bash
# Source this from docs/frostgate-demo.tape to prepare a local-only demo.
set -eu

FROSTGATE_DEMO_DIR="${FROSTGATE_DEMO_DIR:-$(mktemp -d)}"
export FROSTGATE_DEMO_DIR
FROSTGATE_DEMO_PORT_FILE="$FROSTGATE_DEMO_DIR/port"
export FROSTGATE_DEMO_PORT_FILE
FROSTGATE_DEMO_BIN="$FROSTGATE_DEMO_DIR/frostgate"
export FROSTGATE_DEMO_BIN

cleanup_frostgate_demo() {
  if [ -n "${FROSTGATE_DEMO_PID:-}" ]; then
    kill "$FROSTGATE_DEMO_PID" 2>/dev/null || true
  fi
}

python3 - "$FROSTGATE_DEMO_PORT_FILE" <<'PY' &
import http.server
import json
import sys

port_file = sys.argv[1]

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        if self.path == "/generate_204":
            self.send_response(204)
            self.end_headers()
            return
        if self.path == "/proxies":
            body = {"proxies": {
                "primary-reality": {"type": "vless"},
                "backup-hysteria2": {"type": "hysteria2"},
            }}
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(body).encode())
            return
        if self.path.startswith("/proxies/primary-reality/delay"):
            delay = 42
        elif self.path.startswith("/proxies/backup-hysteria2/delay"):
            delay = 81
        else:
            self.send_response(404)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"delay": delay}).encode())

server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
with open(port_file, "w", encoding="utf-8") as f:
    f.write(str(server.server_port))
server.serve_forever()
PY
FROSTGATE_DEMO_PID=$!
export FROSTGATE_DEMO_PID

while [ ! -s "$FROSTGATE_DEMO_PORT_FILE" ]; do sleep 0.05; done
FROSTGATE_DEMO_PORT="$(cat "$FROSTGATE_DEMO_PORT_FILE")"
export FROSTGATE_DEMO_PORT

go build -ldflags '-X main.version=demo' -o "$FROSTGATE_DEMO_BIN" ./cmd/frostgate
cat >"$FROSTGATE_DEMO_DIR/config.yaml" <<EOF
defaults:
  timeout: 1s
  samples: 1
  sample_interval: 1ms
singbox:
  controller: http://127.0.0.1:${FROSTGATE_DEMO_PORT}
  test_url: http://127.0.0.1:${FROSTGATE_DEMO_PORT}/generate_204
history:
  enabled: false
endpoints:
  - name: direct-baseline
    type: http
    url: http://127.0.0.1:${FROSTGATE_DEMO_PORT}/generate_204
    expected_status: 204
    priority: 100
  - name: primary-reality
    type: singbox
    outbound: primary-reality
    priority: 10
  - name: backup-hysteria2
    type: singbox
    outbound: backup-hysteria2
    priority: 20
EOF
FROSTGATE_DEMO_CONFIG="$FROSTGATE_DEMO_DIR/config.yaml"
export FROSTGATE_DEMO_CONFIG
frostgate() {
  "$FROSTGATE_DEMO_BIN" "$@" --config "$FROSTGATE_DEMO_CONFIG"
}
PS1='$ '
