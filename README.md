# frostgate

[![CI](https://github.com/ilyasmohaimel/frostgate/actions/workflows/ci.yml/badge.svg)](https://github.com/ilyasmohaimel/frostgate/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ilyasmohaimel/frostgate)](https://goreportcard.com/report/github.com/ilyasmohaimel/frostgate)

> Fast, client-side network route sentinel — tells you which proxy route actually works end-to-end, how fast it is, and which one to use.

If you rely on self-hosted escape routes (VLESS+REALITY, Hysteria2, XHTTP) the question isn't "is TCP/443 open?" — it's "can my traffic traverse this proxy and reach the internet?" A port can answer TLS while REALITY authentication, routing, or egress is broken. Frostgate distinguishes **port health != proxy health**.

## Demo

```
$ frostgate
ROUTE              TYPE     STATUS      LOSS   MEDIAN  JITTER  DETAIL
primary-reality    singbox  ● up          0%    121ms      9ms
backup-hysteria2   singbox  ● up          0%    137ms     14ms
xhttp-fallback     singbox  ● degraded   25%    201ms     51ms  loss 25%
direct-baseline    http     ● up          0%     18ms      2ms

best route: primary-reality (121ms)
```

Best route selection prioritizes **reliability over raw speed** — a 0% loss route at 115ms outranks a 25% loss route at 100ms.

## Why not a generic uptime dashboard?

Frostgate is **client-side, proxy-aware, and local**:

- Tests traffic **through** the proxy, not just to it
- Measures loss, jitter, median/min/max, not single-shot latency
- Ranks routes to answer "which should I use now?"
- Runs as a single binary with a TUI and JSON for scripts

It complements Gatus/Uptime Kuma — it does not replace them.

## Architecture

```
Frostgate ──► sing-box (Clash API, 127.0.0.1:9090) ──► outbound (REALITY/Hysteria2/XHTTP) ──► internet target
   │                                         │
   └─► direct http/tcp/tls checks ───────────┘
```

- **Layer 1 (basic):** `tcp` (connect), `tls` (handshake), `http` (GET via direct)
- **Layer 2 (end-to-end):** `singbox` — `GET /proxies/<tag>/delay?url=...` through the selected outbound, proving authentication, transport, and egress

Frostgate does **not** reimplement VLESS/REALITY/Hysteria2/XHTTP — sing-box is the engine.

## Install

One command (no Go needed if you grab the binary):

```sh
# via helper script (installs to ~/.local/bin, tries binary then go install)
curl -fsSL https://raw.githubusercontent.com/ilyasmohaimel/frostgate/main/install.sh | sh

# or directly via Go (always works)
go install github.com/ilyasmohaimel/frostgate/cmd/frostgate@latest

# or download static binary manually
curl -fsSL https://github.com/ilyasmohaimel/frostgate/releases/latest/download/frostgate-linux-amd64 -o ~/.local/bin/frostgate && chmod +x ~/.local/bin/frostgate
curl -fsSL https://github.com/ilyasmohaimel/frostgate/releases/latest/download/frostgate-linux-arm64 -o ~/.local/bin/frostgate && chmod +x ~/.local/bin/frostgate
```

Build locally:

```sh
git clone https://github.com/ilyasmohaimel/frostgate && cd frostgate
make build          # ./frostgate
make dist           # static linux/amd64 + arm64
```

Prebuilt static binaries handle ARM cloud VMs and Raspberry Pi.

## Config

Config discovery (first existing wins):

1. `--config /path.yaml`
2. `$XDG_CONFIG_HOME/frostgate/config.yaml`
3. `~/.config/frostgate/config.yaml`
4. `./frostgate.yaml`

Private config lives **outside** the repo (`~/.config/frostgate/`), mode `600`.

```sh
mkdir -p ~/.config/frostgate
cp frostgate.example.yaml ~/.config/frostgate/config.yaml
$EDITOR ~/.config/frostgate/config.yaml
```

See [`frostgate.example.yaml`](./frostgate.example.yaml). Minimal example:

```yaml
defaults:
  timeout: 5s
  samples: 4
  sample_interval: 150ms

singbox:
  controller: http://127.0.0.1:9090
  test_url: https://www.gstatic.com/generate_204

endpoints:
  - name: primary-reality
    type: singbox
    outbound: primary-reality
    priority: 10
  - name: direct-baseline
    type: http
    url: https://www.gstatic.com/generate_204
    priority: 100
```

`http` endpoints accept `url:` or legacy `host`/`port`/`path`. `singbox` endpoints need `outbound:` tag present in sing-box. Per-route overrides: `timeout`, `samples`, `sample_interval`, `healthy_latency`, `max_jitter`, `max_loss`, `priority`.

Credentials: `singbox.secret` or `singbox.secret_file` or `FROSTGATE_SINGBOX_SECRET` env.

## Sing-box sidecar (recommended)

Frostgate can use your existing sing-box, or run a dedicated sidecar:

- Config: `~/.config/frostgate/sing-box.json` (mode `600`, loopback controller only)
- Service: `~/.config/systemd/user/frostgate-singbox.service` (`127.0.0.1:9090`, no `0.0.0.0`)

```sh
sing-box check -c ~/.config/frostgate/sing-box.json
systemctl --user daemon-reload
systemctl --user enable --now frostgate-singbox.service
systemctl --user status frostgate-singbox.service --no-pager
ss -lntup | grep 9090   # must be 127.0.0.1
```

The sidecar is for **testing only**, not a system-wide proxy/TUN.

## Usage

```sh
frostgate                          # one-shot ranked table
frostgate --config path.yaml
frostgate --timeout 3s             # override
frostgate --json                   # indented JSON, schema_version 2
frostgate --watch                  # live TUI
frostgate --watch --json           # NDJSON (one JSON per interval, no ANSI)
frostgate --doctor                 # diagnostics
frostgate doctor                   # same
frostgate --check                  # validate config
frostgate --version
frostgate --verbose                # debug errors (no secrets)
```

### Watch TUI

Live Bubble Tea dashboard, handles resize, `q`/`Ctrl+C` quit, `r` refresh. No flicker, terminal restored on exit.

### JSON / automation

`--json` emits v2:

```json
{
  "schema_version": 2,
  "timestamp": "2026-08-21T18:00:00Z",
  "results": [{
    "name": "primary-reality",
    "type": "singbox",
    "status": "up",
    "attempted": 4, "succeeded": 4, "failed": 0,
    "loss_pct": 0,
    "median_ms": 121, "jitter_ms": 9,
    "priority": 10
  }],
  "best": "primary-reality",
  "diagnosis": "hint: ..."
}
```

`--watch --json` is strict NDJSON — no ANSI, no headers, safe for `jq`.

### Exit codes

- `0` all up (usable)
- `1` mixed/degraded — at least one up but some degraded/down
- `2` all down — no usable route
- `3` config/usage error

Degraded counts as `1`, not `0`, so scripts don't miss instability.

## Doctor

```sh
frostgate --doctor
```

Checks: config path/parse/permissions, test URL, DNS, `sing-box` binary/version, controller reachable/auth, outbound tags, bind is loopback, sidecar service state. Never prints secrets.

## Security

- Private configs outside git, `chmod 600`
- Controller must be `127.0.0.1` only, never `0.0.0.0`
- Use `secret`/`secret_file` + `Authorization: Bearer` if exposing; store outside repo
- JSON/debug never dump UUIDs, keys, passwords, or controller tokens
- `frostgate.example.yaml` contains only `203.0.113.0/24` / `example.com` placeholders

## Limitations

- Results reflect **current client network**. A pass in Morocco does not guarantee a pass through the GFW — test from the target network for truth.
- End-to-end URL (`generate_204`) must be reachable; override `singbox.test_url` if needed.
- Some censorship is destination-specific; no single probe proves universal reachability.
- Frostgate **recommends** the best route but does not auto-failover (yet).
- Measurements fluctuate — prefer repeated samples and loss/jitter over single latency.

## Development

```sh
make check      # fmt + vet + test + race + build
make build
make dist
go test -race ./...
```

History, Prometheus, notifications, and careful auto-failover are planned but not in this release.

## Roadmap

- [x] Real end-to-end checks via sing-box Clash API
- [x] Live TUI (Bubble Tea)
- [x] Multi-sample loss/jitter, median/min/max
- [x] Priority-aware ranking (reliability > speed)
- [x] Doctor, JSON v2, NDJSON watch, XDG config, verbose/debug
- [ ] Route history / persistent metrics
- [ ] Prometheus exporter
- [ ] Push alerts (ntfy etc.)
- [ ] Gated auto-failover (manual approval)

## License

[MIT](./LICENSE) © Ilyas Mohaimel
