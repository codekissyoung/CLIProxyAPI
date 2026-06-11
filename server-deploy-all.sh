#!/bin/bash
# CLIProxyAPI - 多机统一发布脚本（控制机集中编排）
#
# 在控制机（本机）构建一次二进制产物，分发到清单中的每台目标机，
# 逐台切软链 + 重启 + healthz 健康检查；某台失败则自动回滚到该机原版本。
# 各台目标机的配置文件（cliproxyapi.yaml）不在此脚本同步，由各机自管。
#
# 用法:
#   ./server-deploy-all.sh                 # dry-run，仅打印将要执行的操作（默认）
#   ./server-deploy-all.sh --execute       # 真正发布到清单中所有目标机
#   ./server-deploy-all.sh --execute --target ice-server   # 只发某一台
#   ./server-deploy-all.sh --list          # 打印目标机清单
#
# 目标机通过 SSH 别名访问，需在 ~/.ssh/config 中可达；控制机用本地 self 标识。

set -euo pipefail

log_info()  { echo "[INFO] $1"; }
log_warn()  { echo "[WARN] $1"; }
log_error() { echo "[ERROR] $1" >&2; }

# ── 目标机清单 ──────────────────────────────────────────────
# 每项: "<显示名>|<SSH别名或 self>|<IP>"
# self 表示控制机本身（本地执行，不走 SSH）。新增机器只需加一行。
TARGETS=(
    "cheery-taste|self|103.91.219.75"
    "ice-server|ice-server|103.91.219.4"
)

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEPLOY_DIR="/home/iec/deploy"
BIN_DIR="$DEPLOY_DIR/bin"
SERVICE_NAME="cliproxyapi"
CONFIG_FILE="$DEPLOY_DIR/etc/cliproxyapi.yaml"
BUILD_TMP="/tmp/cliproxyapi-deploy-all-$$"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
BIN_NAME="cliproxyapi.$TIMESTAMP"
KEEP_RELEASES=10

SSH_OPTS=(-o ConnectTimeout=8 -o BatchMode=yes)

EXECUTE=0
ONLY_TARGET=""

usage() {
    sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'
}

while [[ "$#" -gt 0 ]]; do
    case "$1" in
        --execute) EXECUTE=1; shift ;;
        --target)  ONLY_TARGET="${2:-}"; [[ -n "$ONLY_TARGET" ]] || { log_error "--target 需要一个值"; exit 2; }; shift 2 ;;
        --list)
            printf '%-16s %-14s %s\n' "NAME" "SSH" "IP"
            for entry in "${TARGETS[@]}"; do
                IFS='|' read -r name ssh ip <<< "$entry"
                printf '%-16s %-14s %s\n' "$name" "$ssh" "$ip"
            done
            exit 0 ;;
        -h|--help) usage; exit 0 ;;
        *) log_error "未知参数: $1"; usage >&2; exit 2 ;;
    esac
done

# ── 1. 构建一次产物 ─────────────────────────────────────────
mkdir -p "$BUILD_TMP"
trap 'rm -rf "$BUILD_TMP"' EXIT

echo "========================================"
echo "  CLIProxyAPI 多机统一发布"
echo "  控制机: $(hostname)"
echo "  版本:   $TIMESTAMP"
echo "  模式:   $([[ "$EXECUTE" == 1 ]] && echo 真实发布 || echo 'DRY-RUN（加 --execute 才真跑）')"
echo "========================================"
echo ""

log_info "1. 在控制机构建二进制（仅构建一次，所有目标机共用同一产物）..."
cd "$SCRIPT_DIR"
CGO_ENABLED=0 go build -o "$BUILD_TMP/$BIN_NAME" ./cmd/server/
ARTIFACT="$BUILD_TMP/$BIN_NAME"
log_info "   ✓ 构建完成: $BIN_NAME ($(du -h "$ARTIFACT" | cut -f1))"
echo ""

# ── 远程/本地执行封装 ────────────────────────────────────────
# run_on <ssh> <command...>  —— self 走本地 bash -c，否则走 ssh
run_on() {
    local ssh="$1"; shift
    if [[ "$ssh" == "self" ]]; then
        bash -c "$*"
    else
        ssh "${SSH_OPTS[@]}" "$ssh" "$*"
    fi
}

# copy_to <ssh> <local-file> <remote-path>
copy_to() {
    local ssh="$1" src="$2" dst="$3"
    if [[ "$ssh" == "self" ]]; then
        cp "$src" "$dst"
    else
        scp "${SSH_OPTS[@]}" "$src" "$ssh:$dst" >/dev/null
    fi
}

# 目标机上执行的部署逻辑（切软链 + 重启 + healthz + 失败回滚）。
# 作为脚本字符串通过 stdin 传给目标机的 bash，参数: <bin_name> <keep>
remote_deploy_script() {
    cat <<'REMOTE'
set -euo pipefail
BIN_NAME="$1"
KEEP="$2"
DEPLOY_DIR="/home/iec/deploy"
BIN_DIR="$DEPLOY_DIR/bin"
SERVICE_NAME="cliproxyapi"
CONFIG_FILE="$DEPLOY_DIR/etc/cliproxyapi.yaml"
HEALTH_PORT="8317"
if [ -f "$CONFIG_FILE" ]; then
    P="$(awk '/^port:[[:space:]]*/ {print $2; exit}' "$CONFIG_FILE" 2>/dev/null || true)"
    [ -n "${P:-}" ] && HEALTH_PORT="$P"
fi
HEALTH_URL="http://127.0.0.1:$HEALTH_PORT/healthz"
PREVIOUS_TARGET="$(readlink "$BIN_DIR/cliproxyapi" 2>/dev/null || true)"

wait_for_health() {
    for i in $(seq 1 15); do
        curl -sf "$HEALTH_URL" >/dev/null 2>&1 && return 0
        sleep 2
    done
    return 1
}
rollback() {
    [ -n "$PREVIOUS_TARGET" ] || { echo "[ERROR]    ✗ 无可回滚的上一版本"; return 1; }
    [ -e "$BIN_DIR/$PREVIOUS_TARGET" ] || { echo "[ERROR]    ✗ 回滚目标不存在: $PREVIOUS_TARGET"; return 1; }
    echo "[INFO]    ↺ 回滚到上一版本: $PREVIOUS_TARGET"
    ln -sfn "$PREVIOUS_TARGET" "$BIN_DIR/cliproxyapi"
    sudo systemctl restart "$SERVICE_NAME"
}

# 产物已由控制机 scp 到 $BIN_DIR/$BIN_NAME
[ -f "$BIN_DIR/$BIN_NAME" ] || { echo "[ERROR]    ✗ 未找到已分发的产物: $BIN_DIR/$BIN_NAME"; exit 1; }
chmod +x "$BIN_DIR/$BIN_NAME"
ln -sfn "$BIN_NAME" "$BIN_DIR/cliproxyapi"
echo "[INFO]    ✓ cliproxyapi -> $BIN_NAME (原: ${PREVIOUS_TARGET:-无})"

if ! sudo systemctl restart "$SERVICE_NAME"; then
    echo "[ERROR]    ✗ 重启失败，尝试回滚..."
    if rollback && wait_for_health; then echo "[ERROR]    ✗ 已回滚到 $PREVIOUS_TARGET"; else echo "[ERROR]    ✗ 回滚也失败"; fi
    exit 1
fi
if wait_for_health; then
    echo "[INFO]    ✓ 健康检查通过 ($HEALTH_URL)"
else
    echo "[ERROR]    ✗ 健康检查失败，尝试回滚..."
    if rollback && wait_for_health; then echo "[ERROR]    ✗ 已回滚到 $PREVIOUS_TARGET"; else echo "[ERROR]    ✗ 回滚也失败"; fi
    exit 1
fi
# 清理旧版本，保留最近 N 个
ls -t "$BIN_DIR/cliproxyapi."* 2>/dev/null | tail -n +$((KEEP+1)) | xargs -r rm -f
echo "[INFO]    ✓ 完成（保留最近 $KEEP 个版本）"
REMOTE
}

# ── 2. 逐台分发 + 部署 ──────────────────────────────────────
log_info "2. 分发到目标机..."
FAILED=()
DEPLOYED=()
for entry in "${TARGETS[@]}"; do
    IFS='|' read -r name ssh ip <<< "$entry"
    if [[ -n "$ONLY_TARGET" && "$ONLY_TARGET" != "$name" ]]; then
        continue
    fi

    echo ""
    echo "── [$name] ($ip) ──────────────────────"

    if [[ "$EXECUTE" != 1 ]]; then
        log_info "   [dry-run] 将 scp $BIN_NAME -> $name:$BIN_DIR/"
        log_info "   [dry-run] 将在 $name 切软链 -> $BIN_NAME，重启 $SERVICE_NAME，校验 healthz，失败回滚"
        continue
    fi

    # 可达性 + bin 目录
    if ! run_on "$ssh" "mkdir -p '$BIN_DIR'"; then
        log_error "   ✗ [$name] 不可达或 bin 目录创建失败，跳过"
        FAILED+=("$name")
        continue
    fi

    log_info "   分发产物..."
    if ! copy_to "$ssh" "$ARTIFACT" "$BIN_DIR/$BIN_NAME"; then
        log_error "   ✗ [$name] scp 失败"
        FAILED+=("$name")
        continue
    fi

    log_info "   切软链 + 重启 + 健康检查..."
    if run_on "$ssh" "bash -s -- '$BIN_NAME' '$KEEP_RELEASES'" <<< "$(remote_deploy_script)"; then
        DEPLOYED+=("$name")
    else
        log_error "   ✗ [$name] 部署失败（已尝试在该机回滚）"
        FAILED+=("$name")
    fi
done

# ── 3. 汇总 ─────────────────────────────────────────────────
echo ""
echo "========================================"
if [[ "$EXECUTE" != 1 ]]; then
    log_info "DRY-RUN 结束，未做任何改动。加 --execute 真正发布。"
    exit 0
fi
log_info "发布完成: 成功 ${#DEPLOYED[@]} 台 / 失败 ${#FAILED[@]} 台  版本 $TIMESTAMP"
[[ ${#DEPLOYED[@]} -gt 0 ]] && log_info "  ✓ 成功: ${DEPLOYED[*]}"
if [[ ${#FAILED[@]} -gt 0 ]]; then
    log_error "  ✗ 失败: ${FAILED[*]}"
    exit 1
fi
echo "========================================"
