# frostgate

[![CI](https://github.com/ilyasmohaimel/frostgate/actions/workflows/ci.yml/badge.svg)](https://github.com/ilyasmohaimel/frostgate/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ilyasmohaimel/frostgate)](https://goreportcard.com/report/github.com/ilyasmohaimel/frostgate)

> Fast, local route toolkit — paste a proxy link and know which route actually works.

**Port open ≠ proxy healthy.** Frostgate tests traffic *through* your tunnel, not just to it. No account, no cloud, no telemetry — everything stays on your machine.

---

## What Frostgate does

- Paste proxy links directly — no JSON editing
- Import subscriptions (plain, base64, Clash YAML, sing-box JSON)
- End-to-end health checks (through sing-box)
- Latency / loss / jitter
- Real per-route download benchmarks
- Failure diagnosis
- Network / censorship-related signals
- Compare home vs dorm vs mobile networks
- Local history (bbolt, no CGO, no secrets)
- Explainable recommendations (reliability > speed)
- Live TUI + JSON/NDJSON automation

---

## Install

### Recommended

```sh
curl -fsSL https://raw.githubusercontent.com/ilyasmohaimel/frostgate/main/install.sh | sh

frostgate --version
frostgate --help

frostgate setup
```

Installs to `~/.local/bin/frostgate` (no `sudo`, no VPS contact). Detects OS/kernel/arch/distro/shell/existing install locally (no telemetry). Verifies `SHA256SUMS` when available with `sha256sum`/`shasum`/`openssl`.

On `linux-amd64` / `linux-arm64` uses prebuilt static binary. On `darwin`/other, `install.sh` falls back to `go install`.

### Via Go

```sh
go install github.com/ilyasmohaimel/frostgate/cmd/frostgate@latest
# binary in $(go env GOPATH)/bin or ~/go/bin
```

If `go install @latest` fails with `sumdb 404` right after release: `GOSUMDB=off GOPROXY=direct go install ...@latest`.

### Manual binary

```sh
mkdir -p ~/.local/bin
curl -fsSL https://github.com/ilyasmohaimel/frostgate/releases/latest/download/frostgate-linux-amd64 -o ~/.local/bin/frostgate
chmod +x ~/.local/bin/frostgate
# arm64: frostgate-linux-arm64
# Verify:
# curl -fsSL https://github.com/ilyasmohaimel/frostgate/releases/latest/download/SHA256SUMS | grep frostgate-linux-amd64
# sha256sum ~/.local/bin/frostgate
```

Only `linux-amd64` and `linux-arm64` have prebuilt binaries in v0.4.0. macOS/Windows prebuilt binaries are **planned, not yet first-class**. macOS users should use `go install`; Windows users should use WSL (treated as Linux).

### Verify PATH

```sh
frostgate --version
# if not found:
# bash/zsh: export PATH="$HOME/.local/bin:$PATH"
# fish:     fish_add_path ~/.local/bin
```

---

## Quick start (60 seconds)

```sh
curl -fsSL https://raw.githubusercontent.com/ilyasmohaimel/frostgate/main/install.sh | sh
frostgate setup
frostgate
```

Then:

```sh
frostgate benchmark
```

---

## Paste a proxy link and go

**Quote the URL** — query strings contain `&`, `?`, `#`.

```sh
frostgate add 'vless://YOUR_LINK'
frostgate add 'hysteria2://YOUR_LINK'
# hy2:// alias also works
```

Frostgate will:

- parse it (VLESS + REALITY + Vision, Hysteria2/salamander, IPv4/IPv6, fragment name, URL encoding, `hy2://`)
- validate required fields (UUID, server, port, REALITY pbk/sni, Hysteria password, etc.)
- derive a sensible route name (from `#fragment` or protocol)
- allow `--name my-reality` override
- dedupe via fingerprint (won't add same route 17 times if name changes)
- backup `~/.config/frostgate/` to `backup-<timestamp>/`
- update private `sing-box.json` + `config.yaml` (0600)
- `sing-box check`
- reload only the Frostgate sidecar (`127.0.0.1:9090`, never `0.0.0.0`)
- test the new route end-to-end
- report `latency/loss/jitter`

Example:

```
Importing route...
parsed VLESS + REALITY + Vision (jp-reality)
sing-box config valid
sidecar reloaded

jp-reality
Latency: 143ms
Loss:    0%

Route added.
```

If validation fails, the working config is rolled back. Duplicate:

```
Route already exists: primary-reality
```

No UUID/password/public-key is ever printed.

---

## Have a subscription?

```sh
frostgate import 'https://provider.example/subscription'
```

Frostgate:

- fetches with timeout, 2 MiB limit, 5-redirect limit, `User-Agent: frostgate/0.4.0`, `https` by default
- rejects `file://`, `data:`, `javascript:`; `http://` is allowed with context but `https` is preferred (no token logging)
- does **not** store the subscription URL (one-time import)
- decodes base64 subscriptions, parses Clash YAML and sing-box JSON where conversion is reliable
- deduplicates, validates, writes private sidecar config, reloads, tests and ranks

```
Imported 23 routes.

TESTING...

 1. JP Reality 03      UP     118ms
 2. HK Hysteria2       UP     124ms
 ...

20/23 usable.
```

Unsupported nodes are skipped with summary:

```
Imported: 17
Skipped: 4

Skipped:
- 2 Trojan routes: unsupported
- 1 malformed VLESS route
- 1 unsupported transport
```

Zero usable routes → failure (not silent success).

---

## Test before adding

```sh
frostgate test 'vless://...'
```

Temporary, isolated, **no persistence**:

- parses link
- creates `0700` temp dir + `0600` config, random `127.0.0.1` ports, no secrets on cmdline
- starts isolated sing-box, tests end-to-end, measures latency/jitter + download where possible
- cleans up on `SIGINT`/`SIGTERM`/timeout/panic — no temp secrets remain
- never modifies your Frostgate config

```
Protocol: VLESS + REALITY
Status:   HEALTHY
Loss:     0%
Median:   156ms
Jitter:   11ms
Download: 72.4 Mbps

This route was NOT saved.
Use `frostgate add` to keep it.
```

Share link is never echoed.

---

## Benchmark everything

```sh
frostgate benchmark
```

Measures **per route** (traffic really goes through that outbound via `127.0.0.1:2080` → `frostgate-selector`):

- status / loss / median / jitter / **real download Mbps**

~8 MiB per route by default (`https://speed.cloudflare.com/__down?bytes=8000000`), configurable via `benchmark.download_bytes` / `download_url`. Expect ~8 MB per route; README documents this. Large lists (e.g. 100 routes → ~800 MiB) prompt for confirmation `[y/N]` in interactive mode.

```
FROSTGATE BENCHMARK

ROUTE              STATUS   LOSS   MEDIAN   JITTER   DOWNLOAD
Hysteria2          UP         0%    171ms      8ms    126 Mbps
Primary Reality    UP         0%    278ms     14ms     81 Mbps
Backup 03          DEGRADED   5%    205ms     42ms     36 Mbps

BEST OVERALL: Hysteria2
...
```

There is **no** `frostgate benchmark --throughput` — speed is already included. For metered connections:

```sh
frostgate benchmark --no-speed
```

Throughput tests run **sequentially** (not 100 parallel) for accurate results; latency probes stay concurrent but bounded. After benchmark, an explainable recommendation is shown — you don't need to run `frostgate recommend` immediately, but it exists for later queries.

For network comparison:

```sh
frostgate benchmark --network home
frostgate benchmark --network dorm
frostgate benchmark --network mobile
frostgate compare
```

---

## Why is my proxy broken?

```sh
frostgate diagnose
frostgate diagnose primary-reality
```

Layered, evidence-based diagnosis:

- local network / interfaces
- system DNS
- direct internet / HTTPS `generate_204`
- sing-box binary / sidecar (`frostgate-singbox.service`) / controller auth
- outbound exists
- server hostname resolves / TCP/443 reachability
- end-to-end proxy URL test (authoritative)
- backup route comparison

```
PRIMARY-REALITY

Local internet            PASS
DNS                       PASS
Server TCP/443            PASS
sing-box sidecar          PASS
Controller                PASS
Outbound exists           PASS
End-to-end proxy          FAIL

Likely problem:
The server is reachable, but traffic cannot complete through the configured proxy outbound.

Possible area:
REALITY/VLESS authentication, transport configuration, server-side routing, or filtering on current network.

Backup:
Hysteria2 is working normally.
```

Wording is `likely`/`possible` unless the error proves the cause. Normal `frostgate` output may add a small hint; detailed diagnosis is behind `diagnose`.

Supports `--json`.

---

## Is my network blocking something?

```sh
frostgate network-check
```

**Signals, not mathematical proof.** Frostgate does not claim to "detect the GFW".

Checks:

- default connectivity / IPv4 / IPv6 / HTTPS / captive portal (204)
- system DNS vs trusted DoH (Cloudflare) — reports `Answers differ` not `POISONED` (CDNs legitimately differ)
- UDP where meaningful (but `QUIC: not directly tested` if not reliable)
- configured proxy routes
- destination reachability (configurable): `https://github.com/`, `https://www.wikipedia.org/`, `https://www.google.com/generate_204`, `neutral` target

```
FROSTGATE NETWORK CHECK

Connection
IPv4                    PASS
IPv6                    UNAVAILABLE
HTTPS                   PASS
Captive portal          NO

DNS
System resolver         PASS
DoH resolver            PASS
Answers differ          YES

Routes
Direct                  PASS
Reality                 PASS
Hysteria2               PASS

Destinations
GitHub direct           PASS
Google direct           FAIL
Google via Reality      PASS
Google via Hysteria2    PASS

Signals
- direct access differs by destination
- DNS answers differ from DoH
- both configured proxy routes work

Recommendation
Use Hysteria2 on this network: fastest healthy route today.
```

Destinations are configurable via `network_check.destinations`.

---

## Compare networks

```sh
frostgate benchmark --network home
frostgate benchmark --network dorm
frostgate benchmark --network mobile
frostgate compare
```

`--network <label>` writes `network=dorm` into history; `frostgate compare` queries last 30 days.

```
NETWORK COMPARISON

                    HOME       DORM       MOBILE
Reality
  Availability       100%        72%         91%
  Median             181ms      320ms       244ms
  Download            84Mb       18Mb        51Mb

Hysteria2
  Availability       100%       100%        100%
  Median             165ms      204ms       191ms
  Download           132Mb       76Mb        89Mb

Best:
Home       Hysteria2
Dorm       Hysteria2
Mobile     Hysteria2
```

Labels are explicit — no NetworkManager/SSID dependency. Windows/macOS portability preserved.

---

## History

Local-only, no cloud, no telemetry, no secrets.

```sh
frostgate history
frostgate history --since 7d
frostgate history --route primary-reality
frostgate history --network dorm
frostgate history --json
```

Storage: `$XDG_DATA_HOME/frostgate/history.db` or `~/.local/share/frostgate/history.db` (bbolt, pure Go, no CGO, static binaries, `0600`, atomic).

Stores: `timestamp/route/type/status/loss/median/min/max/mean/jitter/download/network/error-category` — **never** `UUID/password/VLESS URL/subscription URL/REALITY key/controller secret`.

Automatically records normal `frostgate` runs, `benchmark`, `network-check`. Watch mode rate-limits to one snapshot per `5m` (configurable via `history.watch_interval`).

```
LAST 24 HOURS

Reality
Availability      96.3%
Median            213ms
Jitter            17ms
Failures          11
Trend             getting worse

Hysteria2
Availability      99.9%
Median            174ms
Jitter             8ms
Failures           1
Trend             stable

Most reliable: Hysteria2
```

Retention: 90 days default (`history.retention: 90d`), auto-pruned.

---

## Recommend

```sh
frostgate recommend
# also shown after benchmark
```

Deterministic, explainable, not AI.

Inputs: health / loss / median / jitter / download / configured `priority` / history reliability / network-specific history.

Priority is a **preference, not a command**. Example:

- `Reality: 0% loss 280ms 80 Mbps prio 10` vs `Hysteria: 0% loss 170ms 125 Mbps prio 20` → `Hysteria best overall` (huge difference beats preference).
- `Reality: 180ms` vs `Hysteria: 170ms` both stable → preference may decide `Reality`.
- Uses ~30ms latency / 20 Mbps throughput "close enough" threshold, then priority tie-break.
- Unstable faster route loses: `120ms/20% loss` < `160ms/0% loss`.
- `down` never recommended; `degraded` only if no healthy.

```
RECOMMENDATION

Best overall:
Hysteria2

Why:
✓ 0% loss
✓ 171ms median
✓ 126 Mbps
✓ lowest jitter
✓ 99.9% availability over the last 7 days

Preferred configured route:
Reality

Reality is currently ~107ms slower and ~45 Mbps slower.

Most stable:
Hysteria2

Avoid:
US Backup
Reason: 34% loss

Confidence: high
Based on 53 measurements over 7 days
```

Confidence: `high` ≥50, `medium` ≥10, else `low`.

Categories: `Best overall`, `Fastest`, `Most stable`, `Best fallback`, `Avoid`, `Configured preferred`.

---

## Normal `frostgate` vs benchmark

```sh
frostgate          # fast, low bandwidth: loss/median/jitter only
frostgate benchmark # also downloads ~8 MiB per route
```

Don't burn data on every check.

---

## JSON automation

```sh
frostgate --json
frostgate --watch --json   # NDJSON
frostgate benchmark --json
frostgate diagnose primary-reality --json
frostgate network-check --json
frostgate history --json
frostgate recommend --json
```

`schema_version: 2` (+ `download_mbps`/`network`/`recommendation` where relevant). Never includes secrets. If this is a breaking change, schema version is incremented.

---

## Setup wizard

```sh
frostgate setup
```

- Preflight OS/arch, `0700` config dir, sing-box detection/install
- `1` Import links (`vless://`/`hysteria2://`/`hy2://`), `2` existing controller, `3` direct only
- Generates `controller-secret` (`0600`), `sing-box.json` (`0600`) with `127.0.0.1:9090` only, loopback `frostgate-mixed:2080` + `frostgate-selector`
- `config.yaml` (`0600`)
- Optional `frostgate-singbox.service` (`%h`, no secrets in ExecStart)
- Validates with `sing-box check`, `frostgate --check/--doctor`, real probe

Re-run is safe: `[a] Add route / [r] Reconfigure / [x] Reset (requires YES) / [c] Cancel` with `backup-<timestamp>/`.

Final screen:

```
Frostgate setup complete.

Routes:
✓ primary-reality
✓ hysteria2
✓ direct

Try:
  frostgate
  frostgate benchmark
  frostgate --watch

Useful:
  frostgate diagnose
  frostgate network-check
  frostgate history
```

Internally `setup` reuses the same `route` import logic as `frostgate add`.

---

## Advanced configuration (most users don't need to edit)

```yaml
defaults:
  timeout: 5s
  samples: 4
  sample_interval: 150ms
  healthy_latency: 800ms
  max_jitter: 80ms

singbox:
  controller: http://127.0.0.1:9090
  secret_file: ~/.config/frostgate/controller-secret
  test_url: https://www.gstatic.com/generate_204

benchmark:
  download_bytes: 8388608        # ~8 MiB per route
  download_url: https://speed.cloudflare.com/__down?bytes=8000000

history:
  enabled: true
  retention: 90d
  watch_interval: 5m

network_check:
  destinations:
    - name: github
      url: https://github.com/
    - name: google
      url: https://www.google.com/generate_204
    - name: wikipedia
      url: https://www.wikipedia.org/

endpoints:
  - name: direct
    type: http
    url: https://www.gstatic.com/generate_204
    priority: 100
  - name: primary-reality
    type: singbox
    outbound: primary-reality
    priority: 10
```

Manual config is the **advanced path** — most users should prefer `frostgate setup` / `add` / `import`.

---

## Existing sing-box

Link: `outbound: primary-reality` must exactly match `"tag": "primary-reality"` in `sing-box.json`.

For end-to-end latency, Frostgate uses controller `GET /proxies/<tag>/delay?url=...`. For throughput, it switches `PUT /proxies/frostgate-selector` and downloads via `127.0.0.1:2080` (loopback only) when using the managed sidecar. If you bring your own controller without that selector/mixed inbound, `Download: N/A` with a `doctor` hint — throughput is not faked.

---

## Secret handling

Share URLs **contain secrets** (UUID, password). Subscription URLs may contain tokens.

- **Quote them**: `frostgate add 'vless://...'`
- **Never commit** `~/.config/frostgate/` (`config.yaml`, `sing-box.json`, `controller-secret`, `mixed` inbound). Frostgate ships with only `203.0.113.0/24`, `example.com`, `YOUR_*` placeholders.
- Frostgate stores imported credentials only in your private local config (`0600`), never in Git.
- `history` excludes them (checked by tests). `logs`, `errors`, `history`, `JSON`, `TUI`, `tests`, `process args` are redacted via a central helper (no 8 slightly-different implementations).
- Temporary `frostgate test` configs use `0700` dir + `0600` file, random loopback ports, no creds on cmdline, and are deleted even on `SIGINT`/`SIGTERM`/panic.

---

## After installation

Frostgate is a tester, not a proxy. It doesn't route your apps.

- Without VPS: direct baseline (`https://www.gstatic.com/generate_204`) works immediately (`frostgate setup → 3`).
- With VPS: add your own VLESS/REALITY/Hysteria2 via `setup`/`add`/`import`; Frostgate's local sing-box sidecar does the tunneling.

```
Frostgate → local sing-box (127.0.0.1:9090) → your VPS → internet
```

---

## sing-box examples (placeholders only)

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
    "utls": { "enabled": true, "fingerprint": "chrome" },
    "reality": { "enabled": true, "public_key": "YOUR_REALITY_PUBLIC_KEY", "short_id": "YOUR_SHORT_ID" }
  },
  "packet_encoding": "xudp"
}
```

```json
{
  "type": "hysteria2",
  "tag": "hysteria-backup",
  "server": "YOUR_SERVER",
  "server_port": 443,
  "password": "YOUR_HYSTERIA_PASSWORD",
  "obfs": { "type": "salamander", "password": "YOUR_SALAMANDER_PASSWORD" },
  "tls": { "enabled": true, "server_name": "YOUR_SNI" }
}
```

---

## Controller security

`sing-box.json` → `experimental.clash_api.external_controller: "127.0.0.1:9090"` + `secret` (random, never `0.0.0.0`). Frostgate supports `singbox.secret`, `secret_file`, or `FROSTGATE_SINGBOX_SECRET` (env wins). Errors/JSON never dump secrets.

---

## Running the sidecar

```sh
sing-box check -c ~/.config/frostgate/sing-box.json
sing-box run -c ~/.config/frostgate/sing-box.json

# systemd user (Linux)
systemctl --user daemon-reload
systemctl --user enable --now frostgate-singbox.service
systemctl --user status frostgate-singbox.service --no-pager
ss -lntup | grep 9090   # 127.0.0.1:9090
```

Service uses `%h` (no secrets in ExecStart), `NoNewPrivileges`, `PrivateTmp`, `ProtectSystem=strict`, `ReadWritePaths=%h/.config/frostgate`.

---

## Verification

```sh
frostgate --check
frostgate --doctor
frostgate
frostgate --watch   # q quit, r refresh
frostgate benchmark
frostgate history
frostgate recommend
frostgate diagnose primary-reality
frostgate network-check
```

`--doctor` checks: config path/parse/permissions, test URL, DNS, sing-box version, controller/auth, outbound tags, loopback bind, sidecar state.

---

## Update

```sh
curl -fsSL https://raw.githubusercontent.com/ilyasmohaimel/frostgate/main/install.sh | sh
frostgate --version
```

Go users: `go install github.com/ilyasmohaimel/frostgate/cmd/frostgate@latest` (or `@vX.Y.Z`). Checksums: `SHA256SUMS` is published with `v0.4.0` (`frostgate-linux-amd64`, `frostgate-linux-arm64`).

```
sha256sum ~/.local/bin/frostgate
curl -fsSL https://github.com/ilyasmohaimel/frostgate/releases/latest/download/SHA256SUMS | grep frostgate
```

Only trust `https://github.com/ilyasmohaimel/frostgate/releases`.

---

## Uninstall

```sh
rm ~/.local/bin/frostgate
# optional:
# rm -rf ~/.config/frostgate   # WARNING: deletes config + sidecar + controller-secret

systemctl --user disable --now frostgate-singbox.service 2>/dev/null || true
rm -f ~/.config/systemd/user/frostgate-singbox.service
systemctl --user daemon-reload 2>/dev/null || true
```

---

## Limitations (honest)

- `network-check` gives signals, not mathematical proof of censorship.
- Speed results depend on benchmark target/network conditions; `history` is local.
- Subscription formats vary — unsupported protocols are skipped with summary, not hidden.
- `frostgate test`/`import` parsing supports `vless://`, `hysteria2://`, `hy2://` (REALITY/Vision common params); others (Trojan, SS, VMess) are reported as unsupported.
- Frostgate does not route normal device traffic, does not auto-failover (planned), no web dashboard.
- Windows/macOS prebuilt binaries are not first-class in v0.4.0 (planned: Windows + macOS Intel/Apple Silicon).

---

## Help

```
frostgate --help

Core:
  frostgate
  frostgate --watch

Setup:
  frostgate setup
  frostgate add <link> [--name name]
  frostgate import <subscription-url>
  frostgate test <link>

Analysis:
  frostgate benchmark [--network label] [--no-speed] [--json]
  frostgate diagnose [route] [--json]
  frostgate network-check [--json]
  frostgate compare [--json]
  frostgate recommend [--json]
  frostgate history [--since duration] [--route name] [--network label] [--json]

Diagnostics:
  frostgate --doctor
  frostgate --check
```

---

## Development

```sh
make check   # fmt + vet + test + race + build
make build
make dist    # linux-amd64 + arm64 static + SHA256SUMS
go test -race ./...
```

Single Go binary, static Linux builds, pure-Go `bbolt` (no CGO, no Docker/Redis/Postgres/Electron).

---

## Roadmap

- [x] End-to-end via sing-box Clash API
- [x] Live TUI (Bubble Tea), multi-sample loss/jitter, priority ranking
- [x] `frostgate setup` wizard, doctor, JSON v2, XDG
- [x] `frostgate add` / `import` / `test`
- [x] Real per-route download benchmark + `network-check` / `compare` / `history` / `recommend`
- [x] Installer hardening with SHA256SUMS, atomic replace
- [ ] First-class Windows + macOS Intel/Apple Silicon binaries/install (planned)
- [ ] Gated auto-failover (planned)
- [ ] Push alerts (ntfy) (planned)

## License

[MIT](./LICENSE) © Ilyas Mohaimel
