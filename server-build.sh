#!/bin/bash
# CLIProxyAPI - 服务器构建部署脚本
#
# 用法:
#   ./server-build.sh   # 编译 + 部署 + 重启

set -euo pipefail

log_info()  { echo "[INFO] $1"; }
log_error() { echo "[ERROR] $1"; }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEPLOY_DIR="/home/iec/deploy"
BIN_DIR="$DEPLOY_DIR/bin"
SERVICE_NAME="cliproxyapi"
CONFIG_FILE="$DEPLOY_DIR/etc/cliproxyapi.yaml"
LOG_FILE="$DEPLOY_DIR/auths/logs/main.log"
BUILD_TMP="/tmp/cliproxyapi-build-$$"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
HEALTH_HOST="127.0.0.1"
HEALTH_PORT="8317"
PREVIOUS_TARGET="$(readlink "$BIN_DIR/cliproxyapi" 2>/dev/null || true)"

if [ -f "$CONFIG_FILE" ]; then
    PARSED_PORT="$(awk '/^port:[[:space:]]*/ {print $2; exit}' "$CONFIG_FILE" 2>/dev/null || true)"
    if [ -n "${PARSED_PORT:-}" ]; then
        HEALTH_PORT="$PARSED_PORT"
    fi
fi
HEALTH_URL="http://$HEALTH_HOST:$HEALTH_PORT/healthz"

echo "========================================"
echo "  CLIProxyAPI 构建脚本"
echo "  服务器: $(hostname)"
echo "  版本: $TIMESTAMP"
echo "========================================"
echo ""

mkdir -p "$BIN_DIR"
mkdir -p "$BUILD_TMP"
trap 'rm -rf "$BUILD_TMP"' EXIT

wait_for_health() {
    for i in {1..15}; do
        if curl -sf "$HEALTH_URL" > /dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    return 1
}

rollback() {
    if [ -z "$PREVIOUS_TARGET" ]; then
        log_error "   ✗ 未找到可回滚的上一版本"
        return 1
    fi
    if [ ! -e "$BIN_DIR/$PREVIOUS_TARGET" ]; then
        log_error "   ✗ 回滚目标不存在: $BIN_DIR/$PREVIOUS_TARGET"
        return 1
    fi

    log_info "   ↺ 回滚到上一版本: $PREVIOUS_TARGET"
    ln -sfn "$PREVIOUS_TARGET" "$BIN_DIR/cliproxyapi"
    sudo systemctl restart "$SERVICE_NAME"
}

# 1. 编译
log_info "1. 编译 CLIProxyAPI..."
cd "$SCRIPT_DIR"
CGO_ENABLED=0 go build -o "$BUILD_TMP/cliproxyapi" ./cmd/server/
log_info "   编译完成"

# 2. 部署二进制（带时间戳 + 软链）
log_info "2. 部署二进制..."
cp "$BUILD_TMP/cliproxyapi" "$BIN_DIR/cliproxyapi.$TIMESTAMP"
ln -sfn "cliproxyapi.$TIMESTAMP" "$BIN_DIR/cliproxyapi"
log_info "   ✓ cliproxyapi -> cliproxyapi.$TIMESTAMP"

# 3. 重启服务
log_info "3. 重启 $SERVICE_NAME 服务..."
if ! sudo systemctl restart "$SERVICE_NAME"; then
    log_error "   ✗ 服务重启失败，尝试自动回滚..."
    sudo systemctl status "$SERVICE_NAME" --no-pager || true
    if rollback && wait_for_health; then
        log_error "   ✗ 新版本启动失败，已自动回滚到 $PREVIOUS_TARGET"
    else
        log_error "   ✗ 新版本启动失败，自动回滚也失败"
        sudo systemctl status "$SERVICE_NAME" --no-pager || true
    fi
    exit 1
fi

if wait_for_health; then
    log_info "   ✓ 健康检查通过 ($HEALTH_URL)"
else
    log_error "   ✗ 健康检查失败，尝试自动回滚..."
    sudo systemctl status "$SERVICE_NAME" --no-pager || true
    if rollback && wait_for_health; then
        log_error "   ✗ 新版本健康检查失败，已自动回滚到 $PREVIOUS_TARGET"
    else
        log_error "   ✗ 新版本健康检查失败，自动回滚也失败"
        sudo systemctl status "$SERVICE_NAME" --no-pager || true
    fi
    exit 1
fi

# 4. 清理旧版本（保留最近 10 个）
log_info "4. 清理旧版本二进制（保留最近 10 个）..."
ls -t "$BIN_DIR/cliproxyapi."* 2>/dev/null | tail -n +11 | xargs -r rm -f
log_info "   ✓ 清理完成"

echo ""
log_info "========================================"
log_info "  部署完成！当前版本: $TIMESTAMP"
log_info "========================================"
echo ""
echo "查看服务状态:  sudo systemctl status $SERVICE_NAME"
echo "查看日志:      tail -f $LOG_FILE"
echo "快速回滚:      ln -sfn cliproxyapi.<旧版本号> $BIN_DIR/cliproxyapi && sudo systemctl restart $SERVICE_NAME"
echo ""
