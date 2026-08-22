#!/bin/sh
# Frostgate installer integration harness.
# Exercises install.sh against mocked curl/wget/go and a fixture release tree,
# entirely inside a temporary HOME. No network access, no real installation.
set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="$SCRIPT_DIR/../install.sh"
WORK="$(mktemp -d /tmp/frostgate-install-test-XXXXXX)"
# Test-only debugging hook: FT_KEEP=1 preserves the isolated fixture directory.
if [ "${FT_KEEP:-}" = 1 ]; then trap 'true' EXIT; else trap 'rm -rf "$WORK"' EXIT; fi

PASS=0
FAIL=0
say() { printf '%s\n' "$*" >&2; }
check() {
  name="$1"; ok="$2"
  if [ "$ok" = "1" ]; then
    PASS=$((PASS+1)); say "PASS: $name"
  else
    FAIL=$((FAIL+1)); say "FAIL: $name"
  fi
}

# --- syntax gate ---
if sh -n "$INSTALL_SH" 2>/dev/null; then
  check "install.sh syntax (sh -n)" 1
else
  check "install.sh syntax (sh -n)" 0
fi

# --- fixtures ---
FIXTURE="$WORK/fixture"
mkdir -p "$FIXTURE"

cat > "$FIXTURE/good-binary" <<'EOF'
#!/bin/sh
echo "frostgate vTEST"
exit 0
EOF
chmod +x "$FIXTURE/good-binary"

cat > "$FIXTURE/broken-binary" <<'EOF'
#!/bin/sh
exit 1
EOF
chmod +x "$FIXTURE/broken-binary"

GOOD_SUM="$(sha256sum "$FIXTURE/good-binary" | awk '{print $1}')"
BAD_SUM="$(sha256sum "$FIXTURE/broken-binary" | awk '{print $1}')"

make_sums() { # $1 = hash, $2 = file
  printf '%s  frostgate-linux-amd64\n' "$1" > "$2"
}

# --- shims (scripts created BEFORE symlinking tools so nothing is clobbered) ---
SHIMS="$WORK/shims"
mkdir -p "$SHIMS"

for tool in sh bash grep awk sed head tail cat mktemp chmod mv cp rm uname tr env mkdir sleep dirname basename sha256sum shasum openssl; do
  src="$(command -v "$tool" 2>/dev/null || true)"
  [ -n "$src" ] && ln -sf "$src" "$SHIMS/$tool"
done

# GOSHIMS: same PATH plus the go shim (only used by the go-fallback case).
GOSHIMS="$WORK/goshims"
mkdir -p "$GOSHIMS"
for f in "$SHIMS"/*; do ln -sf "$f" "$GOSHIMS/$(basename "$f")"; done
ln -sf /bin/sh "$GOSHIMS/sh"

cat > "$SHIMS/curl" <<'EOF'
#!/bin/sh
url=""
out=""
prev=""
for a in "$@"; do
  case "$prev" in -o) out="$a" ;; esac
  prev="$a"
done
for a in "$@"; do
  case "$a" in http*) url="$a" ;; esac
done
[ -n "${FT_LOG:-}" ] && echo "curl $url" >> "$FT_LOG"
case "$url" in
  *SHA256SUMS)
    [ "${FT_MODE:-}" = "no-sums" ] && exit 22
    cp "$FT_SUMS" "$out"; exit 0 ;;
  *frostgate-linux-*)
    [ "${FT_MODE:-}" = "download-fail" ] && exit 22
    cp "$FT_BINARY" "$out"; exit 0 ;;
esac
exit 22
EOF

cat > "$SHIMS/wget" <<'EOF'
#!/bin/sh
url=""
out=""
prev=""
for a in "$@"; do
  case "$prev" in -O) out="$a" ;; esac
  prev="$a"
done
for a in "$@"; do
  case "$a" in http*) url="$a" ;; esac
done
case "$url" in
  *SHA256SUMS)
    [ "${FT_MODE:-}" = "no-sums" ] && exit 8
    cp "$FT_SUMS" "$out"; exit 0 ;;
  *frostgate-linux-*)
    [ "${FT_MODE:-}" = "download-fail" ] && exit 8
    cp "$FT_BINARY" "$out"; exit 0 ;;
esac
exit 8
EOF

GOBIN_DIR="$WORK/gobin"
mkdir -p "$GOBIN_DIR"
cat > "$GOSHIMS/go" <<'EOF'
#!/bin/sh
echo "${GOSUMDB-unset}" >> "${FT_GOSUMDB_LOG:-/dev/null}"
case "$1" in
  version) echo "go version test"; exit 0 ;;
  env)
    case "$2" in
      GOBIN) echo "${FT_GOBIN:-}" ;;
      GOPATH) echo "${FT_GOPATH:-}" ;;
      *) echo "" ;;
    esac
    exit 0 ;;
  install)
    mkdir -p "${FT_GOBIN:?}"
    cp "${FT_GO_SRC:?}" "$FT_GOBIN/frostgate"
    chmod +x "$FT_GOBIN/frostgate"
    exit 0 ;;
esac
exit 0
EOF

chmod +x "$SHIMS/curl" "$SHIMS/wget" "$GOSHIMS/go"

# Restricted PATH without any checksum utility.
NOCHECKSUM_PATH="$WORK/nocksum"
mkdir -p "$NOCHECKSUM_PATH"
for tool in sh bash grep awk sed head cat mktemp chmod mv cp rm uname tr env mkdir sleep dirname basename curl; do
  src="$(command -v "$tool" 2>/dev/null || true)"
  [ -n "$src" ] && ln -sf "$src" "$NOCHECKSUM_PATH/$tool"
done
ln -sf "$SHIMS/curl" "$NOCHECKSUM_PATH/curl"

CASE_N=0
run_installer() { # each arg: VAR=VALUE exported for the run
  CASE_N=$((CASE_N+1))
  ENVF="$WORK/envfile"
  : > "$ENVF"
  for kv in "$@"; do
    echo "export $kv" >> "$ENVF"
  done
  (
    . "$ENVF"
    export HOME="$WORK/home"
    sh "$INSTALL_SH"
  ) > "$WORK/out.$CASE_N.log" 2> "$WORK/err.$CASE_N.log"
}

reset_env() {
  rm -rf "$WORK/home" "$WORK/bin"
  mkdir -p "$WORK/home" "$WORK/bin"
}

BASE_ENV="PATH=$SHIMS"
GO_ENV="PATH=$GOSHIMS:/usr/bin:/bin"

# --- Case 1: valid binary + checksum -> success replaces binary ---
reset_env
SUMS="$WORK/sums.1"; make_sums "$GOOD_SUM" "$SUMS"
LOG="$WORK/curl1.log"; : > "$LOG"
run_installer "$BASE_ENV" \
  "INSTALL_DIR=$WORK/bin" \
  "FT_MODE=ok" "FT_SUMS=$SUMS" "FT_BINARY=$FIXTURE/good-binary" "FT_LOG=$LOG"
ok=0
if cmp -s "$WORK/bin/frostgate" "$FIXTURE/good-binary" && \
   "$WORK/bin/frostgate" --version 2>/dev/null | grep -q vTEST; then
  ok=1
fi
check "valid binary installs and --version works" $ok

# --- Case 2: checksum mismatch -> fail closed, old binary preserved ---
reset_env
printf '#!/bin/sh\necho frostgate vOLD\n' > "$WORK/bin/frostgate"
chmod +x "$WORK/bin/frostgate"
cp "$WORK/bin/frostgate" "$WORK/old.copy"
SUMS="$WORK/sums.2"; make_sums "$BAD_SUM" "$SUMS" # wrong hash on purpose
run_installer "$BASE_ENV" \
  "INSTALL_DIR=$WORK/bin" \
  "FT_MODE=ok" "FT_SUMS=$SUMS" "FT_BINARY=$FIXTURE/good-binary" "FT_LOG=/dev/null"
ok=0
cmp -s "$WORK/old.copy" "$WORK/bin/frostgate" && grep -q "checksum mismatch" "$WORK/err.2.log" && ok=1
check "checksum mismatch fails closed and preserves old binary byte-for-byte" $ok

# --- Case 3: missing SHA256SUMS -> fail closed ---
reset_env
printf '#!/bin/sh\necho frostgate vOLD\n' > "$WORK/bin/frostgate"
chmod +x "$WORK/bin/frostgate"
cp "$WORK/bin/frostgate" "$WORK/old3.copy"
run_installer "$BASE_ENV" \
  "INSTALL_DIR=$WORK/bin" \
  "FT_MODE=no-sums" "FT_SUMS=/nonexistent" "FT_BINARY=$FIXTURE/good-binary" "FT_LOG=/dev/null"
ok=0
cmp -s "$WORK/old3.copy" "$WORK/bin/frostgate" && grep -qi "fail" "$WORK/err.3.log" && ok=1
check "missing SHA256SUMS fails closed preserving old binary" $ok

# --- Case 4: sums exist but no entry for our binary ---
reset_env
SUMS="$WORK/sums.4"
printf '%s  some-other-file\n' "$GOOD_SUM" > "$SUMS"
run_installer "$BASE_ENV" \
  "INSTALL_DIR=$WORK/bin" \
  "FT_MODE=ok" "FT_SUMS=$SUMS" "FT_BINARY=$FIXTURE/good-binary" "FT_LOG=/dev/null"
ok=0
grep -q "no checksum entry" "$WORK/err.4.log" && [ ! -e "$WORK/bin/frostgate" ] && ok=1
check "missing checksum entry fails closed" $ok

# --- Case 5: no checksum utility -> fail closed ---
reset_env
SUMS="$WORK/sums.5"; make_sums "$GOOD_SUM" "$SUMS"
(
  export PATH="$NOCHECKSUM_PATH"
  export HOME="$WORK/home"
  export INSTALL_DIR="$WORK/bin"
  export FT_MODE=ok FT_SUMS="$SUMS" FT_BINARY="$FIXTURE/good-binary"
  unset FT_LOG
  sh "$INSTALL_SH"
) > "$WORK/out5.log" 2> "$WORK/err5.log"
ok=0
grep -qE "no sha256sum|failing closed" "$WORK/err5.log" && [ ! -e "$WORK/bin/frostgate" ] && ok=1
check "no checksum utility fails closed" $ok

# --- Case 6: download failure, go unavailable -> error, nothing installed ---
reset_env
NO_GO="$WORK/nogo"
mkdir -p "$NO_GO"
for tool in sh bash grep awk sed head cat mktemp chmod mv cp rm uname tr env mkdir sleep dirname basename curl; do
  src="$(command -v "$tool" 2>/dev/null || true)"
  [ -n "$src" ] && ln -sf "$src" "$NO_GO/$tool"
done
ln -sf "$SHIMS/curl" "$NO_GO/curl"
SUMS="$WORK/sums.6"; make_sums "$GOOD_SUM" "$SUMS"
(
  export PATH="$NO_GO"
  export HOME="$WORK/home"
  export INSTALL_DIR="$WORK/bin"
  export FT_MODE=download-fail FT_SUMS="$SUMS" FT_BINARY="$FIXTURE/good-binary"
  unset FT_LOG
  sh "$INSTALL_SH"
) > "$WORK/out6.log" 2> "$WORK/err6.log"
ok=0
[ ! -e "$WORK/bin/frostgate" ] && grep -qi "go not found\|download failed" "$WORK/err6.log" && ok=1
check "download failure without go leaves nothing installed" $ok

# --- Case 7: go fallback succeeds and never disables GOSUMDB ---
reset_env
SUMS="$WORK/sums.7"; make_sums "$GOOD_SUM" "$SUMS"
GOSUMDB_LOG="$WORK/gosumdb.log"; : > "$GOSUMDB_LOG"
(
  export PATH="$GOSHIMS"
  export HOME="$WORK/home"
  export INSTALL_DIR="$WORK/bin"
  export FT_MODE=download-fail FT_SUMS="$SUMS" FT_BINARY="$FIXTURE/good-binary"
  export FT_GOBIN="$GOBIN_DIR" FT_GOPATH="$WORK/gopath" FT_GO_SRC="$FIXTURE/good-binary"
  export FT_GOSUMDB_LOG="$GOSUMDB_LOG"
  unset GOSUMDB GOPROXY GOFLAGS
  sh "$INSTALL_SH"
) > "$WORK/out7.log" 2> "$WORK/err7.log"
ok=0
"$WORK/bin/frostgate" --version >/dev/null 2>&1 && ok=1
[ -s "$GOSUMDB_LOG" ] || ok=0 # proves go shim actually ran
grep -q "^off$" "$GOSUMDB_LOG" && ok=0
check "go fallback installs and does not disable GOSUMDB" $ok

# --- Case 8: downloaded binary failing --version keeps old binary ---
reset_env
printf '#!/bin/sh\necho frostgate vOLD\n' > "$WORK/bin/frostgate"
chmod +x "$WORK/bin/frostgate"
cp "$WORK/bin/frostgate" "$WORK/old8.copy"
SUMS="$WORK/sums.8"; make_sums "$BAD_SUM" "$SUMS" # valid hash OF THE BROKEN binary
run_installer "$BASE_ENV" \
  "INSTALL_DIR=$WORK/bin" \
  "FT_MODE=ok" "FT_SUMS=$SUMS" "FT_BINARY=$FIXTURE/broken-binary" "FT_LOG=/dev/null"
ok=0
cmp -s "$WORK/old8.copy" "$WORK/bin/frostgate" && grep -q "failed --version" "$WORK/err.5.log" && ok=1
check "binary failing --version rejected before replace, old kept intact" $ok

# --- Case 9: custom INSTALL_DIR honored ---
reset_env
SUMS="$WORK/sums.9"; make_sums "$GOOD_SUM" "$SUMS"
run_installer "$BASE_ENV" \
  "INSTALL_DIR=$WORK/custom-bin" \
  "FT_MODE=ok" "FT_SUMS=$SUMS" "FT_BINARY=$FIXTURE/good-binary" "FT_LOG=/dev/null"
ok=0
"$WORK/custom-bin/frostgate" --version >/dev/null 2>&1 && ok=1
check "custom INSTALL_DIR honored" $ok

# --- Case 10: PATH warning emitted when INSTALL_DIR not in PATH ---
reset_env
SUMS="$WORK/sums.10"; make_sums "$GOOD_SUM" "$SUMS"
run_installer "PATH=$SHIMS:/usr/bin:/bin" \
  "INSTALL_DIR=$WORK/notonpath-bin" \
  "FT_MODE=ok" "FT_SUMS=$SUMS" "FT_BINARY=$FIXTURE/good-binary" "FT_LOG=/dev/null"
ok=0
"$WORK/notonpath-bin/frostgate" --version >/dev/null 2>&1 && grep -q "not in PATH" "$WORK/err.7.log" && ok=1
check "PATH warning emitted when INSTALL_DIR not in PATH" $ok

say ""
say "installer harness: $PASS passed, $FAIL failed"
[ "$FAIL" = "0" ]
