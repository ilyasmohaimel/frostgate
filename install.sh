#!/bin/sh
# Frostgate installer — installs only the Frostgate binary, never private config or VPS credentials.
# Source: https://github.com/ilyasmohaimel/frostgate
# No telemetry, all detection is local.
set -eu

REPO="ilyasmohaimel/frostgate"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
BINARY="frostgate"
VERSION="${VERSION:-latest}"

# --- detection (local only, no telemetry) ---
OS_RAW="$(uname -s 2>/dev/null || echo unknown)"
OS="$(printf "%s" "$OS_RAW" | tr '[:upper:]' '[:lower:]')"
KERNEL="$(uname -r 2>/dev/null || echo unknown)"
ARCH_RAW="$(uname -m 2>/dev/null || echo unknown)"
ARCH="$ARCH_RAW"
SHELL_NAME="${SHELL:-unknown}"
DISTRO="unknown"
if [ -f /etc/os-release ]; then
  # shellcheck disable=SC1091
  . /etc/os-release 2>/dev/null || true
  DISTRO="${ID:-unknown} ${VERSION_ID:-}"
fi
# WSL detection
IS_WSL=0
if [ -f /proc/version ] && grep -qi microsoft /proc/version 2>/dev/null; then
  IS_WSL=1
fi
# MSYS / Git Bash detection (Windows)
IS_MSYS=0
case "$OS_RAW" in
  MINGW*|MSYS*|CYGWIN*) IS_MSYS=1; OS="windows" ;;
esac

case "$OS" in
  linux) OS="linux" ;;
  darwin) OS="darwin" ;;
  windows) OS="windows" ;;
  *) echo "unsupported OS: $OS_RAW ($OS) — only linux is first-class for prebuilt binaries; darwin can use go install" >&2; ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  armv7l|armv7) ARCH="arm" ;;
  *) echo "unsupported arch: $ARCH_RAW ($ARCH) on $OS" >&2; ;;
esac

echo "Detected: OS=$OS kernel=$KERNEL arch=$ARCH distro=$DISTRO shell=$SHELL_NAME" >&2
if [ "$IS_WSL" = "1" ]; then
  echo "WSL detected — treating as Linux" >&2
  OS="linux"
fi

# Existing installation detection
EXISTING_VERSION="none"
if [ -x "$INSTALL_DIR/$BINARY" ]; then
  EXISTING_VERSION="$("$INSTALL_DIR/$BINARY" --version 2>/dev/null || echo "unknown")"
  echo "Existing installation: $INSTALL_DIR/$BINARY ($EXISTING_VERSION)" >&2
else
  echo "No existing installation at $INSTALL_DIR/$BINARY" >&2
fi
if [ -x "$HOME/.local/bin/$BINARY" ] && [ "$INSTALL_DIR" != "$HOME/.local/bin" ]; then
  echo "Note: also found $HOME/.local/bin/$BINARY" >&2
fi

install_via_go() {
  echo "installing via go install..." >&2
  if ! command -v go >/dev/null 2>&1; then
    echo "go not found; install Go from https://go.dev/dl/ or download a release binary from https://github.com/$REPO/releases" >&2
    exit 1
  fi
  if [ "$VERSION" = "latest" ]; then
    go install "github.com/$REPO/cmd/frostgate@latest"
  else
    go install "github.com/$REPO/cmd/frostgate@${VERSION}"
  fi
  GOBIN="$(go env GOBIN 2>/dev/null || true)"
  GOPATH_BIN="$(go env GOPATH 2>/dev/null || echo "$HOME/go")/bin"
  src=""
  if [ -n "$GOBIN" ] && [ -x "$GOBIN/$BINARY" ]; then
    src="$GOBIN/$BINARY"
    echo "installed via go to $GOBIN/$BINARY" >&2
  elif [ -x "$GOPATH_BIN/$BINARY" ]; then
    src="$GOPATH_BIN/$BINARY"
    echo "installed via go to $GOPATH_BIN/$BINARY" >&2
  else
    echo "installed via go (check GOPATH/bin)" >&2
    return 0
  fi
  if [ "$src" != "$INSTALL_DIR/$BINARY" ]; then
    mkdir -p "$INSTALL_DIR"
    tmp_go="$(mktemp "${INSTALL_DIR}/.${BINARY}.go.XXXXXX")"
    trap 'rm -f "$tmp_go"' EXIT
    if cp "$src" "$tmp_go" 2>/dev/null; then
      chmod +x "$tmp_go"
      if "$tmp_go" --version >/dev/null 2>&1; then
        mv -f "$tmp_go" "$INSTALL_DIR/$BINARY"
        trap - EXIT
        echo "copied to $INSTALL_DIR/$BINARY" >&2
      else
        echo "go binary verification failed" >&2
        rm -f "$tmp_go"
        trap - EXIT
        return 1
      fi
    else
      echo "copy to $INSTALL_DIR failed" >&2
      trap - EXIT
      return 1
    fi
  fi
  if [ -x "$INSTALL_DIR/$BINARY" ] && ! "$INSTALL_DIR/$BINARY" --version >/dev/null 2>&1; then
    echo "installed binary failed version check" >&2
    return 1
  fi
}

verify_checksum() {
  bin_file="$1"
  sums_file="$2"
  expected_name="$3"
  if [ ! -f "$sums_file" ]; then
    echo "SHA256SUMS not available — failing closed (no verification)" >&2
    return 1
  fi
  line="$(grep -E " $expected_name\$|\\*$expected_name\$" "$sums_file" 2>/dev/null | head -n1 || true)"
  if [ -z "$line" ]; then
    echo "no checksum entry for $expected_name in SHA256SUMS — failing closed" >&2
    return 1
  fi
  expected="$(printf "%s" "$line" | awk '{print $1}')"
  if [ -z "$expected" ]; then
    echo "cannot parse checksum — failing closed" >&2
    return 1
  fi
  actual=""
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$bin_file" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$bin_file" | awk '{print $1}')"
  elif command -v openssl >/dev/null 2>&1; then
    actual="$(openssl dgst -sha256 "$bin_file" | awk '{print $NF}')"
  else
    echo "no sha256sum/shasum/openssl found — failing closed" >&2
    return 1
  fi
  if [ "$actual" = "$expected" ]; then
    echo "checksum verified: $expected_name" >&2
    return 0
  else
    echo "checksum mismatch for $expected_name" >&2
    echo "  expected: $expected" >&2
    echo "  actual:   $actual" >&2
    return 1
  fi
}

install_via_binary() {
  if [ "$OS" != "linux" ]; then
    if [ "$OS" = "darwin" ]; then
      echo "macOS detected: prebuilt darwin binaries are not yet published (planned)." >&2
      echo "Use: go install github.com/$REPO/cmd/frostgate@latest  (requires Go)" >&2
      if command -v go >/dev/null 2>&1; then
        echo "Go found — falling back to go install" >&2
        return 1
      fi
      echo "Install Go from https://go.dev/dl/ then rerun, or wait for future macOS binaries." >&2
      return 1
    fi
    if [ "$OS" = "windows" ]; then
      if [ "$IS_MSYS" = "1" ]; then
        echo "Windows (MSYS/Git Bash) detected: native Windows binaries are planned but not yet first-class." >&2
        echo "If you are in WSL, installer will treat as Linux. Otherwise use a Linux VM or wait for Windows release." >&2
      fi
      return 1
    fi
    echo "no prebuilt binary for $OS/$ARCH (only linux-amd64 and linux-arm64 are published); trying go install" >&2
    return 1
  fi
  case "$ARCH" in
    amd64) bin_name="${BINARY}-linux-amd64" ;;
    arm64) bin_name="${BINARY}-linux-arm64" ;;
    *) echo "no prebuilt binary for $OS/$ARCH (only amd64/arm64)" >&2; return 1 ;;
  esac
  if [ "$VERSION" = "latest" ]; then
    url="https://github.com/$REPO/releases/latest/download/${bin_name}"
    sums_url="https://github.com/$REPO/releases/latest/download/SHA256SUMS"
  else
    # VERSION may be v0.4.0 or 0.4.0
    ver="$VERSION"
    case "$ver" in
      v*) ;;
      *) ver="v$ver" ;;
    esac
    url="https://github.com/$REPO/releases/download/${ver}/${bin_name}"
    sums_url="https://github.com/$REPO/releases/download/${ver}/SHA256SUMS"
  fi
  echo "trying binary download: $url" >&2
  mkdir -p "$INSTALL_DIR"
  tmp="$(mktemp "${INSTALL_DIR}/.${BINARY}.XXXXXX")"
  tmp_sums="$(mktemp "${INSTALL_DIR}/.SHA256SUMS.XXXXXX")"
  # cleanup on failure — but keep existing binary intact
  trap 'rm -f "$tmp" "$tmp_sums"' EXIT
  success=0
  if command -v curl >/dev/null 2>&1; then
    if curl -fsSL "$url" -o "$tmp"; then
      success=1
    fi
    # try sums
    curl -fsSL "$sums_url" -o "$tmp_sums" 2>/dev/null || true
  elif command -v wget >/dev/null 2>&1; then
    if wget -qO "$tmp" "$url"; then
      success=1
    fi
    wget -qO "$tmp_sums" "$sums_url" 2>/dev/null || true
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
  if head -c 4 "$tmp" | grep -q "<!DO" 2>/dev/null; then
    echo "downloaded file looks like HTML, not a binary" >&2
    return 1
  fi
  if ! verify_checksum "$tmp" "$tmp_sums" "$bin_name"; then
    echo "checksum verification failed — existing installation kept intact" >&2
    return 1
  fi
  chmod +x "$tmp"
  # Atomic move — existing binary stays intact until verify passes
  mv -f "$tmp" "$INSTALL_DIR/$BINARY"
  rm -f "$tmp_sums"
  trap - EXIT
  echo "installed $BINARY to $INSTALL_DIR/$BINARY" >&2
  return 0
}

# Main
if [ "$VERSION" = "latest" ]; then
  if ! install_via_binary; then
    echo "binary not available for $OS/$ARCH or download failed, falling back to go install..." >&2
    install_via_go
  fi
else
  # Pinned version: prefer go for exact version
  if [ "$OS" = "linux" ] && { [ "$ARCH" = "amd64" ] || [ "$ARCH" = "arm64" ]; }; then
    # try binary first for pinned version too
    if ! install_via_binary; then
      install_via_go
    fi
  else
    install_via_go
  fi
fi

# PATH check
case ":${PATH:-}:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "NOTE: $INSTALL_DIR not in PATH." >&2
     echo "  bash/zsh: export PATH=\"\$HOME/.local/bin:\$PATH\"" >&2
     echo "  fish:     fish_add_path ~/.local/bin" >&2
     ;;
esac

# Verify installed binary
if [ -x "$INSTALL_DIR/$BINARY" ]; then
  echo "" >&2
  echo "Frostgate $($INSTALL_DIR/$BINARY --version 2>/dev/null || echo "installed") installed successfully." >&2
  echo "" >&2
  echo "Path:" >&2
  echo "  $INSTALL_DIR/$BINARY" >&2
  echo "" >&2
  echo "Next:" >&2
  echo "  frostgate setup" >&2
  echo "" >&2
  echo "Then:" >&2
  echo "  frostgate" >&2
  echo "  frostgate benchmark" >&2
  echo "" >&2
else
  echo "installation may have landed in GOPATH/bin — check PATH" >&2
fi
echo "done. Verify with: $BINARY --version && $BINARY --help" >&2
