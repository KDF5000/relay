#!/bin/sh

set -eu

repository_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/relay-install-service-test.XXXXXX")
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

release_dir="$test_root/release"
package_dir="$test_root/package"
fake_bin="$test_root/fake-bin"
mkdir -p "$release_dir" "$package_dir" "$fake_bin"

for binary in relay-node relay-tool; do
  printf '#!/bin/sh\nexit 0\n' >"$package_dir/$binary"
  chmod 755 "$package_dir/$binary"
done
# Exercise the real configuration merge command while keeping Node/service mocks.
GOWORK=off go -C "$repository_root" build -o "$package_dir/relayctl" ./cmd/relayctl

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

for runtime_binary in codex traex claude pi opencode agy qodercli qoderclicn; do
  printf '#!/bin/sh\nexit 0\n' >"$fake_bin/$runtime_binary"
done

cat >"$fake_bin/systemctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_SERVICE_LOG"
EOF

cat >"$fake_bin/launchctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_SERVICE_LOG"
EOF

chmod 755 "$fake_bin/uname" "$fake_bin/curl" "$fake_bin/codex" "$fake_bin/traex" "$fake_bin/claude" "$fake_bin/pi" "$fake_bin/opencode" "$fake_bin/agy" "$fake_bin/qodercli" "$fake_bin/qoderclicn" "$fake_bin/systemctl" "$fake_bin/launchctl"

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
if grep -F '"sandbox"' "$linux_config" >/dev/null; then
  printf 'installer unexpectedly generated a sandbox setting\n' >&2
  exit 1
fi
if grep -F '"ephemeral": true' "$linux_config" >/dev/null; then
  printf 'installer unexpectedly enabled ephemeral runtime sessions\n' >&2
  exit 1
fi
test "$(stat -c '%a' "$linux_unit" 2>/dev/null || stat -f '%Lp' "$linux_unit")" = 600
grep -F 'ExecStart=' "$linux_unit" >/dev/null
grep -F 'daemon-reload' "$test_root/Linux-service.log" >/dev/null
grep -F 'restart relay-node.service' "$test_root/Linux-service.log" >/dev/null
grep -F 'enable relay-node.service' "$test_root/Linux-service.log" >/dev/null

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

all_home="$test_root/home-all"
mkdir -p "$all_home"
HOME="$all_home" \
  XDG_CONFIG_HOME="$all_home/.config" \
  XDG_CACHE_HOME="$all_home/.cache" \
  PATH="$fake_bin:$PATH" \
  FAKE_OS=Linux \
  FAKE_RELEASE_DIR="$release_dir" \
  FAKE_SERVICE_LOG="$test_root/all-service.log" \
  RELAY_DOWNLOAD_BASE_URL="https://example.invalid/release" \
  sh "$repository_root/install.sh" --runtime all
all_config="$all_home/.config/relay/node.json"
python3 -c 'import json,sys; c=json.load(open(sys.argv[1])); assert len(c["runtimes"]) == 8' "$all_config"
for provider in codex trae claude pi opencode antigravity qoder qwenwork; do
  grep -F "\"provider\": \"$provider\"" "$all_config" >/dev/null
done

for choice in opencode antigravity agy auto; do
  runtime_home="$test_root/home-$choice"
  mkdir -p "$runtime_home"
  HOME="$runtime_home" \
    XDG_CONFIG_HOME="$runtime_home/.config" \
    XDG_CACHE_HOME="$runtime_home/.cache" \
    PATH="$fake_bin:$PATH" \
    FAKE_OS=Linux \
    FAKE_RELEASE_DIR="$release_dir" \
    RELAY_DOWNLOAD_BASE_URL="https://example.invalid/release" \
    sh "$repository_root/install.sh" --runtime "$choice"
  python3 -c 'import json,sys; c=json.load(open(sys.argv[1])); p=[r["provider"] for r in c["runtimes"]]; expected = "antigravity" if sys.argv[2] in ("antigravity", "agy") else "opencode"; assert expected in p; assert len(p) == (8 if sys.argv[2] == "auto" else 1)' "$runtime_home/.config/relay/node.json" "$choice"
done

# QwenWork ships qoderclicn inside its app bundle rather than on PATH.
bundled_home="$test_root/home-bundled"
bundled_bin="$test_root/bundled-bin"
mkdir -p "$bundled_home/Applications/QwenWorkCN.app/Contents/Resources/bin" "$bundled_bin"
cp "$fake_bin/uname" "$fake_bin/curl" "$bundled_bin/"
cp "$fake_bin/qoderclicn" "$bundled_home/Applications/QwenWorkCN.app/Contents/Resources/bin/qoderclicn"
HOME="$bundled_home" \
  XDG_CONFIG_HOME="$bundled_home/.config" \
  XDG_CACHE_HOME="$bundled_home/.cache" \
  PATH="$bundled_bin:/usr/bin:/bin" \
  FAKE_OS=Darwin \
  FAKE_RELEASE_DIR="$release_dir" \
  RELAY_DOWNLOAD_BASE_URL="https://example.invalid/release" \
  sh "$repository_root/install.sh" --runtime qwenwork
python3 -c 'import json,sys; r=json.load(open(sys.argv[1]))["runtimes"]; assert [x["provider"] for x in r] == ["qwenwork"]; assert r[0]["command"].endswith("QwenWorkCN.app/Contents/Resources/bin/qoderclicn"), r' "$bundled_home/.config/relay/node.json"

# Upgrading discovers new providers without replacing old configuration or
# service-only credentials. Run each upgrade twice to catch duplicate installs.
for os_name in Linux Darwin; do
  home_dir="$test_root/home-$os_name"
  config="$home_dir/.config/relay/node.json"
  if [ "$os_name" = Linux ]; then
    service_file="$home_dir/.config/systemd/user/relay-node.service"
  else
    service_file="$home_dir/Library/LaunchAgents/dev.relay.node.plist"
  fi
  python3 - "$config" <<'PYTEST'
import json,sys
p=sys.argv[1]
c=json.load(open(p))
c['node']['id']='stable-upgrade-node'
c['node']['capacity']=7
c['node']['labels']={'name':'preserved-machine'}
c['server']='https://existing-relay.example'
c.pop('token',None)  # Credentials live only in the existing service.
c['custom_future_field']={'keep':True}
c['bindings']=[{'name':'existing-capability'}]
r=c['runtimes'][0]
r['id']='stable-upgrade-node/custom-codex'
r['command']='/custom/codex'
r['model']='chosen-model'
r['env']={'CUSTOM_KEY':'preserved'}
r['args']=['custom-argument']
r['work_root']='/custom/runs'
json.dump(c,open(p,'w'),indent=2)
PYTEST
  cp "$config" "$test_root/$os_name-original.json"
  cp "$service_file" "$test_root/$os_name-original-service"
  for pass in 1 2; do
    HOME="$home_dir" \
      XDG_CONFIG_HOME="$home_dir/.config" \
      XDG_CACHE_HOME="$home_dir/.cache" \
      PATH="$fake_bin:$PATH" \
      FAKE_OS="$os_name" \
      FAKE_RELEASE_DIR="$release_dir" \
      FAKE_SERVICE_LOG="$test_root/$os_name-upgrade-service.log" \
      RELAY_DOWNLOAD_BASE_URL="https://example.invalid/release" \
      RELAY_NODE_TOKEN='' \
      sh "$repository_root/install.sh" --runtime auto --install-service
    if [ "$pass" = 1 ]; then
      cp "$config" "$test_root/$os_name-after-first-upgrade.json"
    else
      cmp "$config" "$test_root/$os_name-after-first-upgrade.json"
    fi
  done
  cmp "$service_file" "$test_root/$os_name-original-service"
  python3 - "$config" "$test_root/$os_name-original.json" <<'PYTEST'
import json,sys,glob
c=json.load(open(sys.argv[1]));before=json.load(open(sys.argv[2]))
r=c['runtimes'];assert len(r)==8,r
assert len({x['provider'] for x in r})==8
assert r[0]==before['runtimes'][0]
assert {k:v for k,v in c.items() if k!='runtimes'}=={k:v for k,v in before.items() if k!='runtimes'}
assert all(x['id']=='stable-upgrade-node/'+x['provider'] for x in r[1:])
assert all(x['work_root']=='/custom/runs' for x in r[1:])
backups=glob.glob(sys.argv[1]+'.backup.*');assert len(backups)==1,backups
assert json.load(open(backups[0]))==before
PYTEST
  if [ "$os_name" = Linux ]; then
    grep -F 'restart relay-node.service' "$test_root/$os_name-upgrade-service.log" >/dev/null
  else
    grep -F 'kickstart -k gui/' "$test_root/$os_name-upgrade-service.log" >/dev/null
  fi
done

# A configured Node still upgrades when installed CLIs are not on login PATH.
empty_bin="$test_root/no-runtime-bin"
mkdir -p "$empty_bin"
cp "$fake_bin/uname" "$fake_bin/curl" "$fake_bin/systemctl" "$empty_bin/"
cp "$linux_config" "$test_root/before-no-cli-upgrade.json"
HOME="$test_root/home-Linux" \
  XDG_CONFIG_HOME="$test_root/home-Linux/.config" \
  XDG_CACHE_HOME="$test_root/home-Linux/.cache" \
  PATH="$empty_bin:/usr/bin:/bin" \
  FAKE_OS=Linux \
  FAKE_RELEASE_DIR="$release_dir" \
  FAKE_SERVICE_LOG="$test_root/no-cli-service.log" \
  RELAY_DOWNLOAD_BASE_URL="https://example.invalid/release" \
  RELAY_NODE_TOKEN='' \
  sh "$repository_root/install.sh" --runtime auto --install-service
cmp "$linux_config" "$test_root/before-no-cli-upgrade.json"
grep -F 'restart relay-node.service' "$test_root/no-cli-service.log" >/dev/null

printf 'install service tests passed\n'
