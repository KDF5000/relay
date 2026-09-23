#!/bin/sh

set -eu

repository="${RELAY_GITHUB_REPOSITORY:-KDF5000/relay}"
release_version="${RELAY_VERSION:-latest}"
install_dir="${RELAY_INSTALL_DIR:-${HOME}/.local/bin}"
config_file="${RELAY_CONFIG_FILE:-${XDG_CONFIG_HOME:-${HOME}/.config}/relay/node.json}"
data_dir="${RELAY_DATA_DIR:-${XDG_CACHE_HOME:-${HOME}/.cache}/relay}"
server_url="${RELAY_SERVER_URL:-http://127.0.0.1:8787}"
node_id="${RELAY_NODE_ID:-$(hostname 2>/dev/null || printf 'relay-node')}"
capacity="${RELAY_NODE_CAPACITY:-2}"
node_token="${RELAY_NODE_TOKEN:-}"
runtime_choice="${RELAY_RUNTIME:-auto}"
force_config=0
install_service="${RELAY_INSTALL_SERVICE:-0}"

usage() {
  cat <<'EOF'
Install Relay Node and generate its configuration.

Usage:
  install.sh [options]

Options:
  --server URL       Relay Server URL (default: http://127.0.0.1:8787)
  --node-id ID       Node identity (default: local hostname)
  --capacity N       Maximum concurrent runs (default: 2)
  --runtime VALUE    auto, codex, trae, or both (default: auto)
  --token TOKEN      Node authentication token
  --version VERSION  Release tag such as v0.2.0 (default: latest)
  --install-dir DIR  Binary directory (default: ~/.local/bin)
  --config FILE      Configuration path (default: ~/.config/relay/node.json)
  --force            Replace an existing config after creating a backup
  --install-service  Install and start a systemd user service or macOS LaunchAgent
  -h, --help         Show this help

Environment variables with the RELAY_ prefix provide the same defaults.
EOF
}

fail() {
  printf 'relay installer: %s\n' "$*" >&2
  exit 1
}

need_value() {
  [ "$#" -ge 2 ] || fail "$1 requires a value"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --server) need_value "$@"; server_url=$2; shift 2 ;;
    --node-id) need_value "$@"; node_id=$2; shift 2 ;;
    --capacity) need_value "$@"; capacity=$2; shift 2 ;;
    --runtime) need_value "$@"; runtime_choice=$2; shift 2 ;;
    --token) need_value "$@"; node_token=$2; shift 2 ;;
    --version) need_value "$@"; release_version=$2; shift 2 ;;
    --install-dir) need_value "$@"; install_dir=$2; shift 2 ;;
    --config) need_value "$@"; config_file=$2; shift 2 ;;
    --force) force_config=1; shift ;;
    --install-service|--daemon) install_service=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown option: $1" ;;
  esac
done

case "$install_service" in
  1|true|yes) install_service=1 ;;
  0|false|no) install_service=0 ;;
  *) fail "RELAY_INSTALL_SERVICE must be 0, 1, true, false, yes, or no" ;;
esac

case "$capacity" in
  ''|*[!0-9]*) fail "--capacity must be a positive integer" ;;
esac
[ "$capacity" -gt 0 ] || fail "--capacity must be a positive integer"
[ "$capacity" -le 32 ] || fail "--capacity must not exceed 32"

case "$runtime_choice" in
  auto|codex|trae|both) ;;
  *) fail "--runtime must be auto, codex, trae, or both" ;;
esac

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

case "$(uname -s)" in
  Darwin) target_os=darwin ;;
  Linux) target_os=linux ;;
  *) fail "only macOS and Linux are currently supported" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) target_arch=amd64 ;;
  arm64|aarch64) target_arch=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

asset="relay_${target_os}_${target_arch}.tar.gz"
if [ "$release_version" = latest ]; then
  release_base="https://github.com/${repository}/releases/latest/download"
else
  release_base="https://github.com/${repository}/releases/download/${release_version}"
fi
release_base="${RELAY_DOWNLOAD_BASE_URL:-$release_base}"

temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/relay-install.XXXXXX")
trap 'rm -rf "$temporary_dir"' EXIT HUP INT TERM

printf 'Downloading Relay for %s/%s...\n' "$target_os" "$target_arch"
curl -fsSL --retry 3 --connect-timeout 15 -o "$temporary_dir/$asset" "$release_base/$asset"
curl -fsSL --retry 3 --connect-timeout 15 -o "$temporary_dir/checksums.txt" "$release_base/checksums.txt"

expected_checksum=$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1; exit }' "$temporary_dir/checksums.txt")
[ -n "$expected_checksum" ] || fail "checksum for $asset was not found"
if command -v sha256sum >/dev/null 2>&1; then
  actual_checksum=$(sha256sum "$temporary_dir/$asset" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual_checksum=$(shasum -a 256 "$temporary_dir/$asset" | awk '{print $1}')
else
  fail "sha256sum or shasum is required to verify the download"
fi
[ "$expected_checksum" = "$actual_checksum" ] || fail "download checksum verification failed"

mkdir -p "$temporary_dir/package"
tar -xzf "$temporary_dir/$asset" -C "$temporary_dir/package"
[ -f "$temporary_dir/package/relay-node" ] || fail "release does not contain relay-node"
[ -f "$temporary_dir/package/relay-tool" ] || fail "release does not contain relay-tool"

mkdir -p "$install_dir"
install -m 0755 "$temporary_dir/package/relay-node" "$install_dir/relay-node"
install -m 0755 "$temporary_dir/package/relay-tool" "$install_dir/relay-tool"
if [ -f "$temporary_dir/package/relayctl" ]; then
  install -m 0755 "$temporary_dir/package/relayctl" "$install_dir/relayctl"
fi

find_command() {
  command -v "$1" 2>/dev/null || true
}

codex_command=$(find_command codex)
trae_command=$(find_command traex)
[ -n "$trae_command" ] || trae_command=$(find_command trae-cli)

include_codex=0
include_trae=0
case "$runtime_choice" in
  auto)
    [ -n "$codex_command" ] && include_codex=1
    [ -n "$trae_command" ] && include_trae=1
    ;;
  codex) include_codex=1 ;;
  trae) include_trae=1 ;;
  both) include_codex=1; include_trae=1 ;;
esac

[ "$include_codex" -eq 0 ] || [ -n "$codex_command" ] || fail "Codex CLI was not found in PATH"
[ "$include_trae" -eq 0 ] || [ -n "$trae_command" ] || fail "Trae CLI (traex or trae-cli) was not found in PATH"
[ "$include_codex" -eq 1 ] || [ "$include_trae" -eq 1 ] || fail "no supported Runtime found; install Codex or Trae, or pass --runtime"

json_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

systemd_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g; s/%/%%/g'
}

xml_escape() {
  printf '%s' "$1" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g; s/"/\&quot;/g; s/'"'"'/\&apos;/g'
}

install_systemd_service() {
  command -v systemctl >/dev/null 2>&1 || fail "systemctl is required to install the Linux service"
  service_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"
  service_file="$service_dir/relay-node.service"
  escaped_home=$(systemd_escape "$HOME")
  escaped_path=$(systemd_escape "$PATH")
  escaped_binary=$(systemd_escape "$install_dir/relay-node")
  escaped_config=$(systemd_escape "$config_file")
  escaped_service_token=$(systemd_escape "$node_token")
  token_environment=""
  [ -z "$node_token" ] || token_environment="Environment=\"RELAY_NODE_TOKEN=${escaped_service_token}\""

  mkdir -p "$service_dir"
  umask 077
  cat >"$service_file" <<EOF
[Unit]
Description=Relay Agent Runtime Node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment="HOME=${escaped_home}"
Environment="PATH=${escaped_path}"
${token_environment}
ExecStart="${escaped_binary}" -config "${escaped_config}"
Restart=on-failure
RestartSec=5s
TimeoutStopSec=45s
NoNewPrivileges=true
PrivateTmp=true
UMask=0077

[Install]
WantedBy=default.target
EOF
  chmod 600 "$service_file"
  systemctl --user daemon-reload || fail "systemd user manager is unavailable; log in as the target user and try again"
  systemctl --user enable --now relay-node.service || fail "could not enable and start relay-node.service"
  printf 'Installed and started systemd user service: %s\n' "$service_file"
  printf 'For startup without an interactive login, run: sudo loginctl enable-linger %s\n' "$(id -un)"
}

install_launchd_service() {
  command -v launchctl >/dev/null 2>&1 || fail "launchctl is required to install the macOS service"
  service_dir="$HOME/Library/LaunchAgents"
  log_dir="$data_dir/logs"
  service_file="$service_dir/dev.relay.node.plist"
  escaped_home=$(xml_escape "$HOME")
  escaped_path=$(xml_escape "$PATH")
  escaped_binary=$(xml_escape "$install_dir/relay-node")
  escaped_config=$(xml_escape "$config_file")
  escaped_stdout=$(xml_escape "$log_dir/node.log")
  escaped_stderr=$(xml_escape "$log_dir/node.error.log")
  escaped_service_token=$(xml_escape "$node_token")
  token_environment=""
  if [ -n "$node_token" ]; then
    token_environment="
      <key>RELAY_NODE_TOKEN</key>
      <string>${escaped_service_token}</string>"
  fi

  mkdir -p "$service_dir" "$log_dir"
  umask 077
  cat >"$service_file" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.relay.node</string>
  <key>ProgramArguments</key>
  <array>
    <string>${escaped_binary}</string>
    <string>-config</string>
    <string>${escaped_config}</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>${escaped_home}</string>
    <key>PATH</key>
    <string>${escaped_path}</string>${token_environment}
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>5</integer>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>${escaped_stdout}</string>
  <key>StandardErrorPath</key>
  <string>${escaped_stderr}</string>
</dict>
</plist>
EOF
  chmod 600 "$service_file"
  launch_domain="gui/$(id -u)"
  launchctl bootout "$launch_domain/dev.relay.node" >/dev/null 2>&1 || true
  if ! launchctl bootstrap "$launch_domain" "$service_file"; then
    launchctl load -w "$service_file" || fail "could not load dev.relay.node"
  fi
  launchctl enable "$launch_domain/dev.relay.node" >/dev/null 2>&1 || true
  launchctl kickstart -k "$launch_domain/dev.relay.node" >/dev/null 2>&1 || true
  printf 'Installed and started macOS LaunchAgent: %s\n' "$service_file"
}

install_node_service() {
  case "$target_os" in
    linux) install_systemd_service ;;
    darwin) install_launchd_service ;;
  esac
}

if [ -f "$config_file" ] && [ "$force_config" -ne 1 ]; then
  printf 'Keeping existing config: %s\n' "$config_file"
else
  config_dir=$(dirname "$config_file")
  mkdir -p "$config_dir"
  if [ -f "$config_file" ]; then
    backup_file="${config_file}.backup.$(date +%Y%m%d%H%M%S)"
    cp "$config_file" "$backup_file"
    printf 'Backed up existing config to %s\n' "$backup_file"
  fi

  escaped_server=$(json_escape "$server_url")
  escaped_node=$(json_escape "$node_id")
  escaped_install_dir=$(json_escape "$install_dir")
  escaped_data_dir=$(json_escape "$data_dir")
  escaped_token=$(json_escape "$node_token")
  runtime_json=""
  separator=""
  if [ "$include_codex" -eq 1 ]; then
    escaped_command=$(json_escape "$codex_command")
    runtime_json="${runtime_json}${separator}
    {\"id\": \"${escaped_node}/codex\", \"kind\": \"codex\", \"provider\": \"codex\", \"protocol\": \"app-server\", \"command\": \"${escaped_command}\", \"tool_dir\": \"${escaped_install_dir}\", \"work_root\": \"${escaped_data_dir}/runs\", \"ephemeral\": false}"
    separator=","
  fi
  if [ "$include_trae" -eq 1 ]; then
    escaped_command=$(json_escape "$trae_command")
    runtime_json="${runtime_json}${separator}
    {\"id\": \"${escaped_node}/trae\", \"kind\": \"trae\", \"provider\": \"trae\", \"protocol\": \"app-server\", \"command\": \"${escaped_command}\", \"tool_dir\": \"${escaped_install_dir}\", \"work_root\": \"${escaped_data_dir}/runs\", \"ephemeral\": false}"
  fi

  token_line=""
  [ -z "$node_token" ] || token_line="  \"token\": \"${escaped_token}\","
  umask 077
  cat >"$config_file" <<EOF
{
  "server": "${escaped_server}",
${token_line}
  "workspace_root": "${escaped_data_dir}/workspaces",
  "node": {
    "id": "${escaped_node}",
    "capacity": ${capacity},
    "runtimes": []
  },
  "runtimes": [${runtime_json}
  ],
  "bindings": []
}
EOF
  printf 'Created config: %s\n' "$config_file"
fi

printf '\nRelay Node installed successfully.\n'
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) printf 'Add %s to PATH before using the installed commands.\n' "$install_dir" ;;
esac
if [ "$install_service" -eq 1 ]; then
  install_node_service
else
  printf 'Start the node with:\n  %s/relay-node -config %s\n' "$install_dir" "$config_file"
  printf 'Or install it as a background service by running this installer with --install-service.\n'
fi
