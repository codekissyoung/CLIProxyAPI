#!/bin/bash
# CLIProxyAPI - multi-host release script from the control host
#
# Build the binary once on the control host, distribute it to every target,
# switch the release symlink, restart the service, and verify /healthz. A
# failed host rolls itself back to its previous symlink target.
# Target host config files are not synchronized by this script.
#
# Usage:
#   ./server-deploy-all.sh                 # dry-run; only print the plan
#   ./server-deploy-all.sh --execute       # deploy to every target
#   ./server-deploy-all.sh --execute --target ice-server   # deploy one target
#   ./server-deploy-all.sh --list          # print target inventory
#
# Targets use SSH aliases from ~/.ssh/config; self means the local control host.

set -euo pipefail

log_info()  { echo "[INFO] $1"; }
log_warn()  { echo "[WARN] $1"; }
log_error() { echo "[ERROR] $1" >&2; }

# ── Target inventory ────────────────────────────────────────
# Format: "<display-name>|<SSH alias or self>|<IP>"
# Add a host by appending one line.
TARGETS=(
    "cheery-taste|self|103.91.219.75"
    "ice-server|ice-server|103.91.219.4"
    "ice-server-2|ice-server-2|45.59.131.169"
    "ice-do-db-1|ice-do-db-1|165.232.147.148"
    "ice-do-web-1|ice-do-web-1|164.90.149.100"
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

# ── 1. Build one artifact ───────────────────────────────────
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

# ── Local/remote command helpers ────────────────────────────
# run_on <ssh> <command...> runs simple commands; remote stdin is detached.
run_on() {
    local ssh="$1"; shift
    if [[ "$ssh" == "self" ]]; then
        "$@"
    else
        ssh -n "${SSH_OPTS[@]}" "$ssh" "$@"
    fi
}

run_script_on() {
    local ssh="$1"; shift
    if [[ "$ssh" == "self" ]]; then
        "$@"
    else
        ssh "${SSH_OPTS[@]}" "$ssh" "$@"
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

read_release_link() {
    local ssh="$1"
    local link_path="$BIN_DIR/cliproxyapi"
    if [[ "$ssh" == "self" ]]; then
        readlink "$link_path" 2>/dev/null || true
    else
        ssh -n "${SSH_OPTS[@]}" "$ssh" readlink "$link_path" 2>/dev/null || true
    fi
}

verify_release_link() {
    local name="$1" ssh="$2" expected="$3"
    local actual

    actual="$(read_release_link "$ssh")"
    if [[ "$actual" != "$expected" ]]; then
        log_error "   ✗ [$name] 软链校验失败: cliproxyapi -> ${actual:-<missing>}，预期 $expected"
        return 1
    fi

    log_info "   ✓ [$name] 软链校验通过: cliproxyapi -> $expected"
}

# Target-host deployment logic, passed to bash over stdin.
# Arguments: <bin_name> <keep>
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

# The control host has already copied the artifact to $BIN_DIR/$BIN_NAME.
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
# Keep only the latest N versioned binaries.
ls -t "$BIN_DIR/cliproxyapi."* 2>/dev/null | tail -n +$((KEEP+1)) | xargs -r rm -f
echo "[INFO]    ✓ 完成（保留最近 $KEEP 个版本）"
REMOTE
}

# ── 2. Distribute and deploy host by host ───────────────────
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

    # Connectivity and bin directory preflight.
    if ! run_on "$ssh" mkdir -p "$BIN_DIR"; then
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
    if ! run_script_on "$ssh" bash -s -- "$BIN_NAME" "$KEEP_RELEASES" <<< "$(remote_deploy_script)"; then
        log_error "   ✗ [$name] 部署失败（已尝试在该机回滚）"
        FAILED+=("$name")
        continue
    fi

    if ! verify_release_link "$name" "$ssh" "$BIN_NAME"; then
        FAILED+=("$name")
        continue
    fi

    DEPLOYED+=("$name")
done

# ── 3. Summary ──────────────────────────────────────────────
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
