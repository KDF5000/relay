#!/bin/sh

set -eu

repository_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/relay-install-service-test.XXXXXX")
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

release_dir="$test_root/release"
package_dir="$test_root/package"
fake_bin="$test_root/fake-bin"
mkdir -p "$release_dir" "$package_dir" "$fake_bin"

for binary in relay-node relay-tool relayctl; do
  printf '#!/bin/sh\nexit 0\n' >"$package_dir/$binary"
  chmod 755 "$package_dir/$binary"
done

for target_os in linux darwin; do
  asset="relay_${target_os}_arm64.tar.gz"
  tar -C "$package_dir" -czf "$release_dir/$asset" relay-node relay-tool relayctl
  if command -v sha256sum >/dev/null 2>&1; then
    checksum=$(sha256sum "$release_dir/$asset" | awk '{print $1}')
  else
    checksum=$(shasum -a 256 "$release_dir/$asset" | awk '{print $1}')
  fi
  printf '%s  %s\n' "$checksum" "$asset" >>"$release_dir/checksums.txt"
done

cat >"$fake_bin/uname" <<'EOF'
#!/bin/sh
case "$1" in
  -s) printf '%s\n' "$FAKE_OS" ;;
  -m) printf '%s\n' arm64 ;;
  *) exit 1 ;;
esac
EOF

cat >"$fake_bin/curl" <<'EOF'
#!/bin/sh
output=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output=$2; shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
cp "$FAKE_RELEASE_DIR/$(basename "$url")" "$output"
EOF

cat >"$fake_bin/codex" <<'EOF'
#!/bin/sh
exit 0
EOF

cat >"$fake_bin/systemctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_SERVICE_LOG"
EOF

cat >"$fake_bin/launchctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_SERVICE_LOG"
EOF

chmod 755 "$fake_bin/uname" "$fake_bin/curl" "$fake_bin/codex" "$fake_bin/systemctl" "$fake_bin/launchctl"

run_installer() {
  os_name=$1
  home_dir="$test_root/home-$os_name"
  service_log="$test_root/$os_name-service.log"
  mkdir -p "$home_dir"
  : >"$service_log"
  HOME="$home_dir" \
    XDG_CONFIG_HOME="$home_dir/.config" \
    XDG_CACHE_HOME="$home_dir/.cache" \
    PATH="$fake_bin:$PATH" \
    FAKE_OS="$os_name" \
    FAKE_RELEASE_DIR="$release_dir" \
    FAKE_SERVICE_LOG="$service_log" \
    RELAY_DOWNLOAD_BASE_URL="https://example.invalid/release" \
    RELAY_NODE_TOKEN='token-with-special-&<>"'"'"'' \
    sh "$repository_root/install.sh" \
      --runtime codex \
      --server https://relay.example.com \
      --install-service
}

run_installer Linux
linux_unit="$test_root/home-Linux/.config/systemd/user/relay-node.service"
linux_config="$test_root/home-Linux/.config/relay/node.json"
test -f "$linux_unit"
grep -F '"provider": "codex"' "$linux_config" >/dev/null
grep -F '"ephemeral": false' "$linux_config" >/dev/null
if grep -F '"ephemeral": true' "$linux_config" >/dev/null; then
  printf 'installer unexpectedly enabled ephemeral runtime sessions\n' >&2
  exit 1
fi
test "$(stat -c '%a' "$linux_unit" 2>/dev/null || stat -f '%Lp' "$linux_unit")" = 600
grep -F 'ExecStart=' "$linux_unit" >/dev/null
grep -F 'daemon-reload' "$test_root/Linux-service.log" >/dev/null
grep -F 'enable --now relay-node.service' "$test_root/Linux-service.log" >/dev/null

run_installer Darwin
mac_plist="$test_root/home-Darwin/Library/LaunchAgents/dev.relay.node.plist"
test -f "$mac_plist"
test "$(stat -c '%a' "$mac_plist" 2>/dev/null || stat -f '%Lp' "$mac_plist")" = 600
grep -F '<string>token-with-special-&amp;&lt;&gt;&quot;&apos;</string>' "$mac_plist" >/dev/null
grep -F 'bootstrap gui/' "$test_root/Darwin-service.log" >/dev/null
grep -F 'kickstart -k gui/' "$test_root/Darwin-service.log" >/dev/null
if command -v plutil >/dev/null 2>&1; then
  plutil -lint "$mac_plist" >/dev/null
fi

printf 'install service tests passed\n'
