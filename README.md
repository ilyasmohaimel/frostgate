# frostgate

[![CI](https://github.com/ilyasmohaimel/frostgate/actions/workflows/ci.yml/badge.svg)](https://github.com/ilyasmohaimel/frostgate/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ilyasmohaimel/frostgate)](https://goreportcard.com/report/github.com/ilyasmohaimel/frostgate)

> Fast, local route sentinel — tells you which of *your* proxy routes actually works end-to-end and which to use.

**Port open ≠ proxy healthy.** Frostgate tests traffic *through* your tunnel, not just to it.

---

## 1-minute first-time flow

```sh
# 1. install (binary only, no VPS needed)
curl -fsSL https://raw.githubusercontent.com/ilyasmohaimel/frostgate/main/install.sh | sh

# 2. verify
frostgate --version
frostgate --help

# 3. setup (interactive — creates your private config)
frostgate setup

# 4. check
frostgate --doctor
frostgate
frostgate --watch   # live TUI, q to quit
```

Installing Frostgate **does not** give you a proxy, does not configure a VPS, and does not grant access to anyone else's server. You supply your own infrastructure (or just test direct internet).

---

## Install

### Recommended (binary)

```sh
curl -fsSL https://raw.githubusercontent.com/ilyasmohaimel/frostgate/main/install.sh | sh
```

Installs to `~/.local/bin/frostgate` (no `sudo`, no private config, no VPS contact). The script tries a prebuilt static binary for `linux-amd64` / `linux-arm64`; on `darwin` or other arches it falls back to `go install`.

### Via Go

```sh
go install github.com/ilyasmohaimel/frostgate/cmd/frostgate@latest
# binary lands in $(go env GOPATH)/bin or ~/go/bin
```

If `go install @latest` fails with `sumdb 404` right after a release, use `GOSUMDB=off GOPROXY=direct go install ...@latest` once — sumdb catches up within minutes.

### Manual binary

```sh
mkdir -p ~/.local/bin
curl -fsSL https://github.com/ilyasmohaimel/frostgate/releases/latest/download/frostgate-linux-amd64 -o ~/.local/bin/frostgate
chmod +x ~/.local/bin/frostgate
# arm64: frostgate-linux-arm64
```

Only `linux-amd64` and `linux-arm64` have prebuilt binaries. macOS users should use `go install` or `install.sh` (which will automatically use `go install`).

### Verify

```sh
frostgate --version
frostgate --help
```

If `~/.local/bin` is not in `PATH`:

```sh
# bash/zsh
export PATH="$HOME/.local/bin:$PATH"
# fish
fish_add_path ~/.local/bin
```

---

## After installation

Frostgate is a **tester**, not a proxy. It does not route your normal traffic.

- **Without a VPS:** you can still check direct internet.
- **With a VPS:** Frostgate can test your VLESS/REALITY, Hysteria2, etc. through a local `sing-box` — you provide the VPS and credentials.

---

## Easiest first test (no VPS needed)

Frostgate works out of the box with a direct baseline. The setup wizard can create this for you, or do it manually:

```sh
mkdir -p ~/.config/frostgate
cat > ~/.config/frostgate/config.yaml <<'EOF'
defaults:
  timeout: 5s
  samples: 4
  sample_interval: 150ms
  healthy_latency: 800ms
  max_jitter: 80ms
endpoints:
  - name: direct
    type: http
    url: https://www.gstatic.com/generate_204
    priority: 100
EOF
chmod 600 ~/.config/frostgate/config.yaml
```

Then:

```sh
frostgate --check   # validates config
frostgate           # one real measurement
```

Expected:

```
ROUTE     TYPE  STATUS  LOSS  MEDIAN  JITTER
direct    http  ● up      0%     25ms      3ms

best route: direct (25ms)
```

If this works, Frostgate itself is ready. Add proxy routes next.

---

## Connect Frostgate to your own VPS

```
Frostgate → local sing-box (Clash API 127.0.0.1:9090) → your VPS
                                                  → your REALITY  → internet
                                                  → your Hysteria2 → internet
```

**You need your own VPS** (Oracle, Hetzner, any). Frostgate never contains or fetches anyone else's VPS, UUID, or keys. Each user pastes *their own* share links during `frostgate setup`.

### Server vs client

```text
VPS (server):
  Xray/VLESS+REALITY  (tcp/443)
  Hysteria2           (udp/443)

Your PC (client):
  Frostgate
  sing-box  (sidecar or your existing client)
```

VPS credentials belong in **your private** `~/.config/frostgate/sing-box.json` (mode `0600`), never in Git.

---

## Recommended: `frostgate setup`

This is the primary way — it creates everything safely with `0700`/`0600` permissions and never logs your share links.

```sh
frostgate setup
```

What it does:

1. **Preflight** — checks OS/arch, creates `~/.config/frostgate` (`0700`), detects `sing-box`, offers to install stable `sing-box` if missing (via `pacman` on Arch or static binary on Linux; on macOS it points to docs).
2. **Choose mode**
   - `1` Import proxy route(s) — paste `vless://` / `hysteria2://` / `hy2://`
   - `2` Use existing sing-box controller (advanced)
   - `3` Direct only
3. **Import** — paste one or more share links, name each route (`reality-primary`, `hysteria-backup`), add another? `[y/N]`. Direct baseline (`https://www.gstatic.com/generate_204`) is added by default.
4. **Controller** — generates a random secret to `~/.config/frostgate/controller-secret` (`0600`), binds Clash API to `127.0.0.1:9090` only (never `0.0.0.0`).
5. **Generate** — writes `sing-box.json` (`0600`) and `config.yaml` (`0600`) with your outbound tags.
6. **Sidecar** — on Linux, offers to install `~/.config/systemd/user/frostgate-singbox.service` (`%h`, no secrets in ExecStart) and `systemctl --user enable --now`.
7. **Validate** — runs `sing-box check`, `frostgate --check`, `frostgate --doctor`, and a real probe. It won't say “complete” if doctor still fails.

Re-running `frostgate setup` is safe: it detects existing config and offers `[a] Add route / [r] Reconfigure / [x] Reset (requires YES) / [c] Cancel`, backing up to `~/.config/frostgate/backup-<timestamp>/` before destructive changes.

After:

```sh
frostgate --doctor
frostgate
```

---

## Manual alternative (advanced)

If you already have a working `sing-box`, you can reuse it:

```yaml
# ~/.config/frostgate/config.yaml
singbox:
  controller: http://127.0.0.1:9090
  secret_file: ~/.config/frostgate/controller-secret   # or FROSTGATE_SINGBOX_SECRET
  test_url: https://www.gstatic.com/generate_204
endpoints:
  - name: reality
    type: singbox
    outbound: reality-primary   # must match sing-box outbound tag
    priority: 10
  - name: hysteria2
    type: singbox
    outbound: hysteria-backup
    priority: 20
  - name: direct
    type: http
    url: https://www.gstatic.com/generate_204
    priority: 100
```

`secret_file` supports `~` (expanded) or absolute path. You can also set `singbox.secret` directly (less recommended) or `FROSTGATE_SINGBOX_SECRET` env.

---

## sing-box examples (placeholders only)

Use **current stable sing-box** syntax. Replace `YOUR_*` with your values — nothing here connects to any real server.

### VLESS + REALITY

```json
{
  "type": "vless",
  "tag": "reality-primary",
  "server": "YOUR_SERVER",
  "server_port": 443,
  "uuid": "YOUR_UUID",
  "flow": "xtls-rprx-vision",
  "tls": {
    "enabled": true,
    "server_name": "YOUR_SNI",
    "utls": {
      "enabled": true,
      "fingerprint": "chrome"
    },
    "reality": {
      "enabled": true,
      "public_key": "YOUR_REALITY_PUBLIC_KEY",
      "short_id": "YOUR_SHORT_ID"
    }
  },
  "packet_encoding": "xudp"
}
```

### Hysteria2

```json
{
  "type": "hysteria2",
  "tag": "hysteria-backup",
  "server": "YOUR_SERVER",
  "server_port": 443,
  "password": "YOUR_HYSTERIA_PASSWORD",
  "obfs": {
    "type": "salamander",
    "password": "YOUR_SALAMANDER_PASSWORD"
  },
  "tls": {
    "enabled": true,
    "server_name": "YOUR_SNI",
    "insecure": false
  }
}
```

### If you already have sing-box

Just ensure your existing `sing-box.json` has a `tag` for each outbound you want tested, expose the Clash API on `127.0.0.1`, and point Frostgate at that controller/tags. `frostgate setup → 2` does exactly this without touching your sing-box config.

### Dedicated sidecar (recommended)

```
~/.config/frostgate/
├── config.yaml          0600
├── sing-box.json        0600  ← your proxy credentials here
└── controller-secret    0600
```

Permissions:

```sh
chmod 700 ~/.config/frostgate
chmod 600 ~/.config/frostgate/config.yaml
chmod 600 ~/.config/frostgate/sing-box.json
chmod 600 ~/.config/frostgate/controller-secret
```

Frostgate never commits these; `.gitignore` ignores local `frostgate.yaml`.

---

## Controller security

`sing-box.json` must contain:

```json
"experimental": {
  "clash_api": {
    "external_controller": "127.0.0.1:9090",
    "secret": "YOUR_RANDOM_SECRET"
  }
}
```

Never `0.0.0.0` for normal use — the Clash API exposes control locally. Frostgate `setup` generates a random secret to `controller-secret` and uses `secret_file`.

Frostgate supports `singbox.secret`, `singbox.secret_file`, or env `FROSTGATE_SINGBOX_SECRET` (env wins if set). Errors and JSON never dump UUIDs, keys, or tokens.

---

## Outbound tags

`outbound: primary-reality` in `config.yaml` must exactly match `"tag": "primary-reality"` in `sing-box.json`. That's the only link.

---

## Running the sidecar

**Manual:**

```sh
sing-box check -c ~/.config/frostgate/sing-box.json
sing-box run -c ~/.config/frostgate/sing-box.json
```

**Systemd user service** (Linux, `%h` — no secrets in args, no root):

```ini
# ~/.config/systemd/user/frostgate-singbox.service
[Unit]
Description=Frostgate sing-box sidecar
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
ExecStart=%h/.local/bin/sing-box run -c %h/.config/frostgate/sing-box.json
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%h/.config/frostgate
[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now frostgate-singbox.service
systemctl --user status frostgate-singbox.service --no-pager
ss -lntup | grep 9090   # must be 127.0.0.1:9090
```

`frostgate setup` creates this for you when you answer `Y`. On macOS/other OS, `setup` falls back to manual instructions.

---

## Verify everything

```sh
frostgate --check    # config valid?
frostgate --doctor   # sing-box + controller + outbounds + permissions
frostgate            # one real measurement
frostgate --watch    # live TUI (q to quit, r to refresh)
```

`--doctor` checks: config path/parse/permissions, test URL, DNS, `sing-box` version, controller reachable/auth, outbound tags, bind is loopback, sidecar state. Never prints secrets.

---

## Output

```
ROUTE     TYPE     STATUS  LOSS  MEDIAN  JITTER  DETAIL
reality   singbox  ● up      0%    180ms      8ms
hysteria  singbox  ● up      0%    145ms      5ms
direct    http     ● up      0%     25ms      2ms

best route: reality (180ms)
```

- `STATUS` — usable? `up` / `degraded` / `down`
- `LOSS` — failed samples / total (4 samples, 150ms apart)
- `MEDIAN` — typical latency (median of successes)
- `JITTER` — mean abs diff between consecutive successes; `0% loss + low jitter = stable`
- `BEST ROUTE` — Frostgate recommends most reliable (health → loss → priority → median → jitter)

`frostgate --json` emits `schema_version: 2` with `attempted/succeeded/failed/loss_pct/min/median/max/mean/jitter_ms/priority/category`. `frostgate --watch --json` is NDJSON (one JSON per interval, no ANSI).

---

## Important limitation

Frostgate **tests** routes; it does not proxy your apps. Your normal client (`Hiddify`, `v2rayN`, `sing-box` etc.) still carries traffic. Frostgate just tells you which outbound is healthy and fastest *now*. It does not auto-failover.

A pass in Morocco ≠ a pass through the GFW. Test from the target network for truth. No single URL proves universal reachability.

---

## Normal usage

```sh
frostgate                # quick check
frostgate --watch        # live dashboard
frostgate --doctor       # validate setup
frostgate --json         # for scripts
frostgate --watch --json | jq .
```

See `frostgate --help` for `--config`, `--timeout`, `--interval`, `--verbose`.

---

## Update

Rerun the installer — it atomically replaces the binary:

```sh
curl -fsSL https://raw.githubusercontent.com/ilyasmohaimel/frostgate/main/install.sh | sh
frostgate --version
```

Go users: `go install github.com/ilyasmohaimel/frostgate/cmd/frostgate@latest` (or `@vX.Y.Z`).

### Checksums

Releases currently publish only the binaries (`frostgate-linux-amd64`, `frostgate-linux-arm64`). Future releases will publish `SHA256SUMS` alongside them. You can always verify a download with:

```sh
sha256sum ~/.local/bin/frostgate
curl -fsSL https://github.com/ilyasmohaimel/frostgate/releases/latest/download/SHA256SUMS 2>/dev/null | grep frostgate || echo "no SHA256SUMS yet"
```

Do not trust a checksum file that is not from `https://github.com/ilyasmohaimel/frostgate/releases`.

---

## Uninstall

```sh
rm ~/.local/bin/frostgate          # binary
# optional: keep or remove private config
#   keeps:  ~/.config/frostgate stays intact
#   remove: rm -rf ~/.config/frostgate   # WARNING: deletes your Frostgate config + sing-box sidecar + controller-secret

# if you installed the sidecar service
systemctl --user disable --now frostgate-singbox.service 2>/dev/null || true
rm -f ~/.config/systemd/user/frostgate-singbox.service
systemctl --user daemon-reload 2>/dev/null || true
```

---

## Your proxy credentials are yours

Frostgate ships with **no** VPS, UUID, REALITY keys, Hysteria passwords, or controller secrets. Example files use `example.com`, `203.0.113.0/24`, `YOUR_*` placeholders. Never commit your real `config.yaml`, `sing-box.json`, or `controller-secret`. The installer only installs the binary and never contacts any VPS.

---

## Development

```sh
make check   # fmt + vet + test + race + build
make build
make dist    # linux-amd64 + arm64 static
go test -race ./...
```

## Roadmap

- [x] End-to-end via sing-box Clash API
- [x] Live TUI (Bubble Tea)
- [x] Multi-sample loss/jitter
- [x] Priority-aware ranking
- [x] `frostgate setup` wizard
- [x] Doctor, JSON v2, XDG
- [ ] History / Prometheus
- [ ] Push alerts (ntfy)
- [ ] Gated auto-failover

## License

[MIT](./LICENSE) © Ilyas Mohaimel
