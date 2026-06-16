# frostgate

> A fast, concurrent health-and-latency sentinel for self-hosted anti-censorship
> endpoints (VLESS-Reality, Hysteria 2, TCP/TLS/HTTP) — written in Go, ships as a
> single static ARM binary.

## Why

If you live behind a censoring firewall and rely on self-hosted escape routes — say a
VLESS + XTLS-Reality endpoint with a Hysteria 2 backup on a cheap cloud VM — the question
that matters every morning isn't *"is the internet up?"* It's *"which of my routes is
alive and fastest **right now**?"* Those links get throttled and blackholed
unpredictably, and switching to the slow one wastes your day.

`frostgate` answers that in under a second. It probes all your endpoints **in parallel**,
measures real connect/handshake latency, and prints a ranked table so you know which
outbound to use before you even open your proxy client.

## Demo

```
$ frostgate
ROUTE             TYPE   STATUS   LATENCY
tokyo-reality     tls    ● up      48ms
cloudflare-204    http   ● up      61ms
tokyo-hysteria    tcp    ● up     112ms
osaka-backup      tls    ✕ down      —    (timeout)

best route: tokyo-reality (48ms)
```

## Install

```sh
# with a Go toolchain
go install github.com/ilyasmohaimel/frostgate/cmd/frostgate@latest

# or grab a prebuilt static binary from Releases (linux/amd64, linux/arm64)
```

The ARM build is a single dependency-free binary — `scp` it straight onto an
ARM cloud VM or a Raspberry Pi and run it.

## Configure

```sh
cp frostgate.example.yaml frostgate.yaml
$EDITOR frostgate.yaml      # add your real endpoints (this file is gitignored)
```

See [`frostgate.example.yaml`](./frostgate.example.yaml) for the annotated schema.

## Usage

```sh
frostgate                       # probe once, print ranked table
frostgate --config path.yaml    # use a specific config
frostgate --json                # machine-readable output for scripts
frostgate --watch               # re-probe on an interval, live
frostgate --timeout 3s          # override the per-probe deadline
```

**Exit codes:** `0` all routes up · `1` some routes down · `2` all routes down ·
`3` config/usage error. Handy for cron and `&&` chains.

## How it works

frostgate fans every endpoint out onto its own goroutine, runs the probe for that
endpoint's `type`, and collects the timed results. Probes by type:

- **tcp** — time to establish a raw TCP connection
- **tls** — TCP connect **plus** a full TLS handshake (honoring `sni`)
- **http** — time to first response from an HTTP(S) GET

### Scope (honest version)

v0 measures **reachability and latency** — the practical "up and fast" signal. It does
**not** complete a full VLESS-Reality or Hysteria 2 protocol handshake; a port can answer
TLS while the proxy behind it is misconfigured. True end-to-end validation (driving
sing-box through a local SOCKS port and timing a request *through the tunnel*) is on the
roadmap below.

## Roadmap

- [ ] Real end-to-end checks: spin up sing-box with your config, time a request to
      `https://www.gstatic.com/generate_204` through the tunnel
- [ ] Live `--watch` TUI dashboard (Bubble Tea)
- [ ] Jitter / packet-loss sampling, not just single-shot latency
- [ ] Prometheus exporter
- [ ] Auto-failover: rewrite the active sing-box outbound to the best route
- [ ] Push alerts (ntfy / WhatsApp) when the primary drops

## License

[MIT](./LICENSE) © Ilyas Mohaimel
