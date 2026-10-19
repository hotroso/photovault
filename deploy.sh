#!/usr/bin/env bash
#
# deploy.sh — build và chạy photovault trên Ubuntu.
#
# Dùng:
#   ./deploy.sh              build (tidy + vet + build 3 binary) rồi CHẠY server
#   ./deploy.sh build        chỉ build, không chạy
#   ./deploy.sh run          chạy server (build nếu chưa có binary)
#   ./deploy.sh verify       build rồi chạy lệnh verify (kiểm tra toàn vẹn)
#   ./deploy.sh reindex      build rồi chạy lệnh reindex (dựng lại index từ đĩa)
#   ./deploy.sh install-service   cài systemd service để chạy nền + tự khởi động
#
# Mọi tham số kết nối (Postgres/Redis/IP/cổng/TLS) đọc từ file .env (xem .env.example).

set -euo pipefail

# ---- Thư mục dự án = nơi đặt script này ----
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

BIN_DIR="$SCRIPT_DIR/bin"
SERVER_BIN="$BIN_DIR/server"
VERIFY_BIN="$BIN_DIR/verify"
REINDEX_BIN="$BIN_DIR/reindex"

# ---- Màu cho log ----
c_info()  { printf '\033[1;34m[deploy]\033[0m %s\n' "$*"; }
c_ok()    { printf '\033[1;32m[deploy]\033[0m %s\n' "$*"; }
c_warn()  { printf '\033[1;33m[deploy]\033[0m %s\n' "$*"; }
c_err()   { printf '\033[1;31m[deploy]\033[0m %s\n' "$*" >&2; }

# ---- Kiểm tra Go ----
require_go() {
  if ! command -v go >/dev/null 2>&1; then
    c_err "Chưa cài Go. Cài bằng:  sudo snap install go --classic   (hoặc theo https://go.dev/dl)"
    exit 1
  fi
  c_info "Go: $(go version)"
}

# ---- Kiểm tra .env ----
require_env() {
  if [[ ! -f "$SCRIPT_DIR/.env" ]]; then
    if [[ -f "$SCRIPT_DIR/.env.example" ]]; then
      c_warn ".env chưa có — đang tạo từ .env.example. HÃY MỞ .env ĐIỀN GIÁ TRỊ THẬT rồi chạy lại."
      cp "$SCRIPT_DIR/.env.example" "$SCRIPT_DIR/.env"
      exit 1
    fi
    c_err "Thiếu cả .env lẫn .env.example."
    exit 1
  fi
}

# ---- Build ----
do_build() {
  require_go
  mkdir -p "$BIN_DIR"

  c_info "go mod tidy (tải dependency + sinh go.sum)…"
  go mod tidy

  c_info "go vet…"
  go vet ./...

  c_info "build server / verify / reindex…"
  # CGO tắt để ra binary tĩnh, dễ mang đi.
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$SERVER_BIN"  ./cmd/server
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$VERIFY_BIN"  ./cmd/verify
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$REINDEX_BIN" ./cmd/reindex

  c_ok "Build xong → $BIN_DIR"
}

ensure_built() {
  if [[ ! -x "$SERVER_BIN" ]]; then
    c_info "Chưa có binary — build trước."
    do_build
  fi
}

# ---- Chạy server ----
do_run() {
  require_env
  ensure_built
  c_info "Chạy server… (Ctrl+C để dừng)"
  exec "$SERVER_BIN" -env "$SCRIPT_DIR/.env"
}

do_verify() {
  require_env
  ensure_built
  c_info "Chạy verify…"
  exec "$VERIFY_BIN" -env "$SCRIPT_DIR/.env"
}

do_reindex() {
  require_env
  ensure_built
  c_info "Chạy reindex…"
  exec "$REINDEX_BIN" -env "$SCRIPT_DIR/.env"
}

# ---- Cài systemd service (chạy nền + tự khởi động cùng máy) ----
do_install_service() {
  require_env
  do_build

  local unit="/etc/systemd/system/photovault.service"
  local user; user="$(id -un)"

  c_info "Tạo systemd unit tại $unit (cần sudo)…"
  sudo tee "$unit" >/dev/null <<EOF
[Unit]
Description=photovault (backup RAW ảnh iPhone)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$user
WorkingDirectory=$SCRIPT_DIR
ExecStart=$SERVER_BIN -env $SCRIPT_DIR/.env
Restart=on-failure
RestartSec=3
# Bảo vệ cơ bản
NoNewPrivileges=true
ProtectSystem=full

[Install]
WantedBy=multi-user.target
EOF

  sudo systemctl daemon-reload
  sudo systemctl enable --now photovault.service
  c_ok "Đã cài và khởi động service. Xem log:  journalctl -u photovault -f"
}

# ---- Điều phối ----
cmd="${1:-deploy}"
case "$cmd" in
  deploy)          do_build; do_run ;;
  build)           do_build ;;
  run)             do_run ;;
  verify)          do_verify ;;
  reindex)         do_reindex ;;
  install-service) do_install_service ;;
  *)
    c_err "Lệnh không hợp lệ: $cmd"
    echo "Dùng: $0 [deploy|build|run|verify|reindex|install-service]" >&2
    exit 1
    ;;
esac
