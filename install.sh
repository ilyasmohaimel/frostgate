#!/bin/sh
set -eu

REPO="ilyasmohaimel/frostgate"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
BINARY="frostgate"

# Detect latest version via GitHub if not pinned
VERSION="${VERSION:-latest}"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
  linux) OS="linux" ;;
  darwin) OS="darwin" ;;
  *) echo "unsupported OS: $OS" >&2; exit 1 ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  armv7l) ARCH="arm" ;;
  *) echo "unsupported arch: $ARCH ($OS)" >&2; exit 1 ;;
esac

# Prefer binary from GitHub releases if available, fallback to go install
install_via_go() {
  echo "installing via go install..."
  if ! command -v go >/dev/null 2>&1; then
    echo "go not found; install Go or download binary from https://github.com/$REPO/releases" >&2
    exit 1
  fi
  go install "github.com/$REPO/cmd/frostgate@${VERSION}"
  echo "installed via go to $(go env GOPATH)/bin or ~/go/bin"
}

install_via_binary() {
  url="https://github.com/$REPO/releases/latest/download/${BINARY}-${OS}-${ARCH}"
  # also try without OS prefix for linux static naming
  if [ "$OS" = "linux" ] && [ "$ARCH" = "amd64" ]; then
    url="https://github.com/$REPO/releases/latest/download/${BINARY}-linux-amd64"
  elif [ "$OS" = "linux" ] && [ "$ARCH" = "arm64" ]; then
    url="https://github.com/$REPO/releases/latest/download/${BINARY}-linux-arm64"
  fi
  echo "trying binary download: $url"
  mkdir -p "$INSTALL_DIR"
  if command -v curl >/dev/null 2>&1; then
    if curl -fsSL "$url" -o "$INSTALL_DIR/$BINARY"; then
      chmod +x "$INSTALL_DIR/$BINARY"
      echo "installed $BINARY to $INSTALL_DIR/$BINARY"
      echo "run: $BINARY --help"
      return 0
    fi
  elif command -v wget >/dev/null 2>&1; then
    if wget -qO "$INSTALL_DIR/$BINARY" "$url"; then
      chmod +x "$INSTALL_DIR/$BINARY"
      echo "installed $BINARY to $INSTALL_DIR/$BINARY"
      return 0
    fi
  fi
  return 1
}

# Try binary first if VERSION is latest/release, else go
if [ "$VERSION" = "latest" ]; then
  if ! install_via_binary; then
    echo "binary download failed, falling back to go install..." >&2
    install_via_go
  fi
else
  install_via_go
fi

# Check PATH
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "NOTE: $INSTALL_DIR not in PATH. Add: export PATH=\"\$HOME/.local/bin:\$PATH\"" >&2 ;;
esac
