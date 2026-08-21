#!/bin/sh
# Frostgate installer — installs only the Frostgate binary, never private config or VPS credentials.
# Source: https://github.com/ilyasmohaimel/frostgate
set -eu

REPO="ilyasmohaimel/frostgate"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
BINARY="frostgate"
VERSION="${VERSION:-latest}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
  linux) OS="linux" ;;
  darwin) OS="darwin" ;;
  *) echo "unsupported OS: $OS (only linux and darwin are detected; use go install on other OS)" >&2; exit 1 ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  armv7l) ARCH="arm" ;;
  *) echo "unsupported arch: $ARCH ($OS)" >&2; exit 1 ;;
esac

install_via_go() {
  echo "installing via go install..."
  if ! command -v go >/dev/null 2>&1; then
    echo "go not found; install Go from https://go.dev/dl/ or download a release binary from https://github.com/$REPO/releases" >&2
    exit 1
  fi
  # Use direct proxy to avoid sumdb lag for fresh releases; sumdb will catch up
  if [ "$VERSION" = "latest" ]; then
    GOSUMDB=off GOPROXY=direct go install "github.com/$REPO/cmd/frostgate@latest" || go install "github.com/$REPO/cmd/frostgate@latest"
  else
    go install "github.com/$REPO/cmd/frostgate@${VERSION}"
  fi
  # Locate binary
  GOBIN="$(go env GOBIN 2>/dev/null || true)"
  GOPATH_BIN="$(go env GOPATH 2>/dev/null || echo "$HOME/go")/bin"
  if [ -n "$GOBIN" ] && [ -x "$GOBIN/$BINARY" ]; then
    echo "installed via go to $GOBIN/$BINARY"
  elif [ -x "$GOPATH_BIN/$BINARY" ]; then
    echo "installed via go to $GOPATH_BIN/$BINARY"
  else
    echo "installed via go (check GOPATH/bin)"
  fi
}

install_via_binary() {
  # Only linux has prebuilt binaries currently (linux-amd64, linux-arm64)
  # darwin users fall back to go install — we do not claim a darwin binary exists
  if [ "$OS" != "linux" ]; then
    echo "no prebuilt binary for $OS/$ARCH (only linux-amd64 and linux-arm64 are published); trying go install" >&2
    return 1
  fi
  case "$ARCH" in
    amd64) url="https://github.com/$REPO/releases/latest/download/${BINARY}-linux-amd64" ;;
    arm64) url="https://github.com/$REPO/releases/latest/download/${BINARY}-linux-arm64" ;;
    *) echo "no prebuilt binary for $OS/$ARCH" >&2; return 1 ;;
  esac

  echo "trying binary download: $url"
  mkdir -p "$INSTALL_DIR"

  tmp="$(mktemp "${INSTALL_DIR}/.${BINARY}.XXXXXX")"
  # Ensure temp is removed on failure; on success we move it
  trap 'rm -f "$tmp"' EXIT

  success=0
  if command -v curl >/dev/null 2>&1; then
    if curl -fsSL "$url" -o "$tmp"; then
      success=1
    fi
  elif command -v wget >/dev/null 2>&1; then
    if wget -qO "$tmp" "$url"; then
      success=1
    fi
  else
    echo "neither curl nor wget found" >&2
    return 1
  fi

  if [ "$success" -ne 1 ]; then
    echo "binary download failed" >&2
    return 1
  fi

  if [ ! -s "$tmp" ]; then
    echo "downloaded file is empty" >&2
    return 1
  fi

  # Basic sanity: ensure not HTML error page
  if head -c 4 "$tmp" | grep -q "<!DO" 2>/dev/null; then
    echo "downloaded file looks like HTML, not a binary" >&2
    return 1
  fi

  chmod +x "$tmp"
  # Atomic move
  mv -f "$tmp" "$INSTALL_DIR/$BINARY"
  # Remove trap's file already moved
  trap - EXIT
  echo "installed $BINARY to $INSTALL_DIR/$BINARY"
  echo "run: $BINARY --help  (ensure $INSTALL_DIR is in PATH)"
  return 0
}

# Main logic: prefer binary for linux, fallback to go
if [ "$VERSION" = "latest" ]; then
  if ! install_via_binary; then
    echo "binary not available for $OS/$ARCH or download failed, falling back to go install..." >&2
    install_via_go
  fi
else
  # Pinned version requested — use go for exact version, unless user explicitly wants binary
  install_via_go
fi

# PATH check
case ":${PATH:-}:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "NOTE: $INSTALL_DIR not in PATH." >&2
     echo "  bash/zsh: export PATH=\"\$HOME/.local/bin:\$PATH\"" >&2
     echo "  fish:     fish_add_path ~/.local/bin" >&2
     ;;
esac

echo "done. Verify with: $BINARY --version && $BINARY --help"
