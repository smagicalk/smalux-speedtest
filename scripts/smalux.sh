#!/usr/bin/env bash
# Smalux Speedtest installer and service manager.
#
# The script deliberately keeps credentials out of command-line arguments and
# downloads only the latest GitHub Release package for the current Linux arch.
# It requires root, curl, tar, sha256sum, and systemd on the target host.
set -Eeuo pipefail

readonly REPOSITORY="smagicalk/smalux-speedtest"
readonly SERVICE_USER="smalux"
readonly INSTALL_ROOT="/opt/smalux-speedtest"
readonly CONFIG_DIR="/etc/smalux-speedtest"
readonly DATA_DIR="/var/lib/smalux-speedtest"
readonly DOWNLOAD_DIR="/var/cache/smalux-speedtest"
readonly SERVER_SERVICE="smalux-server.service"
readonly CLIENT_SERVICE="smalux-client.service"

log() { printf '[smalux] %s\n' "$*"; }
die() { printf '[smalux] error: %s\n' "$*" >&2; exit 1; }
need_command() { command -v "$1" >/dev/null 2>&1 || die "缺少依赖：$1"; }

require_root() {
  [[ "${EUID}" -eq 0 ]] || die "请使用 root 运行此脚本。"
  need_command curl
  need_command tar
  need_command sha256sum
  need_command systemctl
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *) die "不支持的 Linux 架构：$(uname -m)，目前支持 amd64 和 arm64。" ;;
  esac
}

prompt_default() {
  local label="$1" default="$2" value
  if [[ -n "$default" ]]; then
    read -r -p "$label [$default]: " value
    printf '%s' "${value:-$default}"
  else
    read -r -p "$label: " value
    printf '%s' "$value"
  fi
}

prompt_secret() {
  local label="$1" value
  read -r -s -p "$label: " value
  printf '\n' >&2
  printf '%s' "$value"
}

confirm() {
  local answer
  read -r -p "$1 [y/N]: " answer
  [[ "$answer" =~ ^[Yy]$ ]]
}

# systemd EnvironmentFile supports quoted values but does not use shell variable
# expansion. Reject line breaks and escape the two characters that are special in
# a double-quoted value so passwords and Tokens cannot inject another setting.
systemd_env_value() {
  local value="$1"
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || die "配置值不能包含换行符。"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "$value"
}

latest_release() {
  local response
  response="$(curl --fail --silent --show-error --location \
    --header 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/${REPOSITORY}/releases/latest")" \
    || die "无法读取 GitHub Releases。"
  RELEASE_TAG="$(printf '%s' "$response" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  [[ "$RELEASE_TAG" =~ ^v[0-9A-Za-z._-]+$ ]] || die "GitHub 没有可用的正式 Release。"
}

download_release() {
  local arch="$1" archive checksums expected actual url
  latest_release
  archive="smalux-speedtest-${RELEASE_TAG}-linux-${arch}.tar.gz"
  mkdir -p "$DOWNLOAD_DIR"
  url="https://github.com/${REPOSITORY}/releases/download/${RELEASE_TAG}/${archive}"
  log "下载 ${archive}"
  curl --fail --silent --show-error --location --retry 3 -o "${DOWNLOAD_DIR}/${archive}" "$url" \
    || die "Release 下载失败：${url}"
  curl --fail --silent --show-error --location --retry 3 \
    -o "${DOWNLOAD_DIR}/SHA256SUMS" \
    "https://github.com/${REPOSITORY}/releases/download/${RELEASE_TAG}/SHA256SUMS" \
    || die "SHA256SUMS 下载失败。"
  checksums="${DOWNLOAD_DIR}/SHA256SUMS"
  expected="$(awk -v name="$archive" '$2 == name || $2 == "*" name {print $1; exit}' "$checksums")"
  [[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || die "SHA256SUMS 中没有找到 ${archive}。"
  actual="$(sha256sum "${DOWNLOAD_DIR}/${archive}" | awk '{print $1}')"
  [[ "$actual" == "$expected" ]] || die "下载包校验失败。"
  RELEASE_ARCHIVE="${DOWNLOAD_DIR}/${archive}"
  log "Release ${RELEASE_TAG} 校验通过"
}

prepare_user() {
  if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system --home-dir "$DATA_DIR" --create-home --shell /usr/sbin/nologin "$SERVICE_USER"
  fi
  install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$DATA_DIR"
  install -d -o root -g root -m 0755 "$INSTALL_ROOT" "$CONFIG_DIR" "$DOWNLOAD_DIR"
}

install_binaries() {
  local staging="$DOWNLOAD_DIR/extract-${RANDOM}"
  rm -rf "$staging"
  mkdir -p "$staging"
  tar -xzf "$RELEASE_ARCHIVE" -C "$staging"
  [[ -x "$staging/smalux-server" ]] || die "Release 中缺少 smalux-server。"
  [[ -x "$staging/smalux-client" ]] || die "Release 中缺少 smalux-client。"
  # Install to sibling files and rename atomically. Existing processes keep their
  # old executable inode until systemd restarts the selected service.
  install -m 0755 "$staging/smalux-server" "$INSTALL_ROOT/.smalux-server.new"
  install -m 0755 "$staging/smalux-client" "$INSTALL_ROOT/.smalux-client.new"
  mv -f "$INSTALL_ROOT/.smalux-server.new" "$INSTALL_ROOT/smalux-server"
  mv -f "$INSTALL_ROOT/.smalux-client.new" "$INSTALL_ROOT/smalux-client"
  rm -rf "$staging"
}

write_server_unit() {
  cat >"/etc/systemd/system/${SERVER_SERVICE}" <<EOF
[Unit]
Description=Smalux Speedtest Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${DATA_DIR}
EnvironmentFile=-${CONFIG_DIR}/server.env
ExecStart=${INSTALL_ROOT}/smalux-server
Restart=on-failure
RestartSec=3s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF
}

write_client_unit() {
  cat >"/etc/systemd/system/${CLIENT_SERVICE}" <<EOF
[Unit]
Description=Smalux Speedtest Client
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${DATA_DIR}
EnvironmentFile=${CONFIG_DIR}/client.env
ExecStart=${INSTALL_ROOT}/smalux-client
Restart=on-failure
RestartSec=3s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF
}

install_server() {
  local arch listen password insecure database
  arch="$(detect_arch)"
  listen="$(prompt_default 'Server 监听地址' '0.0.0.0:8080')"
  password="$(prompt_secret '初始管理员密码（至少 8 个字符）')"
  [[ "${#password}" -ge 8 ]] || die "管理员密码至少需要 8 个字符。"
  database="${DATA_DIR}/smalux.db"
  insecure="false"
  if confirm '是否允许远程明文 ws://（公网环境不建议）'; then insecure="true"; fi

  prepare_user
  download_release "$arch"
  install_binaries
  cat >"${CONFIG_DIR}/server.env" <<EOF
SMALUX_LISTEN=$(systemd_env_value "$listen")
SMALUX_DATABASE=$(systemd_env_value "$database")
SMALUX_ALLOW_INSECURE_WS=$(systemd_env_value "$insecure")
SMALUX_ADMIN_PASSWORD=$(systemd_env_value "$password")
EOF
  chmod 0600 "${CONFIG_DIR}/server.env"
  write_server_unit
  systemctl daemon-reload
  systemctl enable --now "$SERVER_SERVICE"
  sleep 2
  if curl --fail --silent --show-error http://127.0.0.1:"${listen##*:}"/healthz >/dev/null 2>&1; then
    # The password is only needed for first database bootstrap. Keep the env file
    # for future settings but remove the secret after a successful health check.
    sed -i '/^SMALUX_ADMIN_PASSWORD=/d' "${CONFIG_DIR}/server.env"
    chmod 0600 "${CONFIG_DIR}/server.env"
  else
    systemctl --no-pager --full status "$SERVER_SERVICE" || true
    die "Server 未通过健康检查；初始密码仍保留在 server.env，修复后重启即可继续初始化。"
  fi
  log "Server 已安装并启动，访问地址：http://<服务器地址>:${listen##*:}"
}

install_client() {
  local arch server token
  arch="$(detect_arch)"
  server="$(prompt_default 'Server WebSocket 地址' 'wss://example.com/ws/client')"
  token="$(prompt_secret 'Client Token')"
  [[ -n "$token" ]] || die "Client Token 不能为空。"
  prepare_user
  download_release "$arch"
  install_binaries
  cat >"${CONFIG_DIR}/client.env" <<EOF
SMALUX_CLIENT_TOKEN=$(systemd_env_value "$token")
SMALUX_SERVER_URL=$(systemd_env_value "$server")
EOF
  chmod 0600 "${CONFIG_DIR}/client.env"
  # The client binary reads the token from the environment; the wrapper keeps
  # command-line process listings free of credentials and user-supplied values.
  write_client_unit
  systemctl daemon-reload
  systemctl enable --now "$CLIENT_SERVICE"
  log "Client 已安装并启动；名称和标签请在 Server 控制台维护。"
}

disable_service() {
  local service="$1"
  systemctl stop "$service" >/dev/null 2>&1 || true
  systemctl disable "$service" >/dev/null 2>&1 || true
}

stop_service() {
  systemctl stop "$1" >/dev/null 2>&1 || true
}

install_command() {
  local role
  require_root
  role="$(prompt_default '安装角色（server/client）' 'server')"
  case "$role" in
    server) install_server ;;
    client) install_client ;;
    *) die '角色只能是 server 或 client。' ;;
  esac
}

update_command() {
  local arch role
  require_root
  role="$(prompt_default '更新角色（server/client/both）' 'both')"
  case "$role" in
    server|client|both) ;;
    *) die '角色只能是 server、client 或 both。' ;;
  esac
  arch="$(detect_arch)"
  prepare_user
  download_release "$arch"
  install_binaries
  systemctl daemon-reload
  case "$role" in
    server) systemctl restart "$SERVER_SERVICE" ;;
    client) systemctl restart "$CLIENT_SERVICE" ;;
    both) systemctl restart "$SERVER_SERVICE" 2>/dev/null || true; systemctl restart "$CLIENT_SERVICE" 2>/dev/null || true ;;
  esac
  log "已更新到 ${RELEASE_TAG}"
}

uninstall_command() {
  require_root
  confirm '确认卸载 Smalux 服务和程序（数据库与配置默认保留）' || exit 0
  disable_service "$SERVER_SERVICE"
  disable_service "$CLIENT_SERVICE"
  rm -f "/etc/systemd/system/${SERVER_SERVICE}" "/etc/systemd/system/${CLIENT_SERVICE}"
  systemctl daemon-reload
  rm -rf "$INSTALL_ROOT" "$DOWNLOAD_DIR"
  if confirm '是否同时删除数据库、Token 配置和日志'; then
    rm -rf "$CONFIG_DIR" "$DATA_DIR"
  fi
  log '卸载完成。'
}

status_command() {
  require_root
  systemctl --no-pager --full status "$SERVER_SERVICE" "$CLIENT_SERVICE" || true
}

menu() {
  local choice
  while true; do
    printf '\nSmalux Speedtest 服务管理\n'
    printf '  1) 安装\n  2) 更新\n  3) 卸载\n  4) 状态\n  5) 退出\n'
    read -r -p '请选择 [1-5]: ' choice
    case "$choice" in
      1) install_command ;;
      2) update_command ;;
      3) uninstall_command ;;
      4) status_command ;;
      5) exit 0 ;;
      *) log '无效选项。' ;;
    esac
  done
}

case "${1:-menu}" in
  install) install_command ;;
  update) update_command ;;
  uninstall) uninstall_command ;;
  status) status_command ;;
  start) require_root; systemctl start "$SERVER_SERVICE" "$CLIENT_SERVICE" ;;
  stop) require_root; stop_service "$SERVER_SERVICE"; stop_service "$CLIENT_SERVICE" ;;
  menu) require_root; menu ;;
  *) printf '用法：%s [install|update|uninstall|status|start|stop|menu]\n' "$0"; exit 2 ;;
esac
