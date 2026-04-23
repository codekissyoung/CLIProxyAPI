#!/bin/bash
# CLIProxyAPI profiler watcher deployment script
#
# Usage:
#   ./profiler-build.sh   # Build + deploy + restart profiler watcher

set -euo pipefail

log_info()  { echo "[INFO] $1"; }
log_error() { echo "[ERROR] $1"; }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEPLOY_DIR="/home/iec/deploy"
BIN_DIR="$DEPLOY_DIR/bin"
ETC_DIR="$DEPLOY_DIR/etc"
SERVICE_NAME="cliproxy-profiler"
CONFIG_FILE="$ETC_DIR/cliproxy-profiler.yaml"
UNIT_SOURCE="$SCRIPT_DIR/examples/cliproxy-profiler/cliproxy-profiler.service"
UNIT_TARGET="/etc/systemd/system/$SERVICE_NAME.service"
BUILD_TMP="/tmp/$SERVICE_NAME-build-$$"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
PREVIOUS_TARGET="$(readlink "$BIN_DIR/$SERVICE_NAME" 2>/dev/null || true)"

GO_BIN="$(command -v go)"
COMMIT="$(git -C "$SCRIPT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
if [ -n "$(git -C "$SCRIPT_DIR" status --porcelain --untracked-files=normal 2>/dev/null || true)" ]; then
    COMMIT="$COMMIT-dirty"
fi
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "========================================"
echo "  CLIProxyAPI profiler deployment"
echo "  Host: $(hostname)"
echo "  Version: $TIMESTAMP"
echo "========================================"
echo ""

mkdir -p "$BIN_DIR"
mkdir -p "$BUILD_TMP"
trap 'rm -rf "$BUILD_TMP"' EXIT

wait_for_active() {
    for i in {1..15}; do
        if systemctl is-active --quiet "$SERVICE_NAME"; then
            return 0
        fi
        sleep 2
    done
    return 1
}

rollback() {
    if [ -z "$PREVIOUS_TARGET" ]; then
        log_error "   No previous profiler binary available for rollback"
        return 1
    fi
    if [ ! -e "$BIN_DIR/$PREVIOUS_TARGET" ]; then
        log_error "   Rollback target missing: $BIN_DIR/$PREVIOUS_TARGET"
        return 1
    fi

    log_info "   Rolling back to previous profiler build: $PREVIOUS_TARGET"
    ln -sfn "$PREVIOUS_TARGET" "$BIN_DIR/$SERVICE_NAME"
    sudo systemctl restart "$SERVICE_NAME"
}

if [ ! -f "$CONFIG_FILE" ]; then
    log_error "Profiler config not found: $CONFIG_FILE"
    log_info "Copy and adjust $SCRIPT_DIR/examples/cliproxy-profiler/cliproxy-profiler.example.yaml first"
    exit 1
fi

if [ ! -f "$UNIT_SOURCE" ]; then
    log_error "Profiler systemd unit template not found: $UNIT_SOURCE"
    exit 1
fi

# 1. Build the new profiler binary.
log_info "1. Building $SERVICE_NAME..."
cd "$SCRIPT_DIR"
env -u GOROOT "$GO_BIN" build \
    -ldflags "-X main.Version=$TIMESTAMP -X main.Commit=$COMMIT -X main.BuildDate=$BUILD_DATE" \
    -o "$BUILD_TMP/$SERVICE_NAME" ./cmd/cliproxy-profiler/
log_info "   Build complete"

# 2. Validate the live wiring before deployment.
log_info "2. Running preflight check against live config..."
"$BUILD_TMP/$SERVICE_NAME" -config "$CONFIG_FILE" -check
log_info "   Preflight check passed"

# 3. Publish the versioned binary and refresh the stable symlink.
log_info "3. Deploying profiler binary..."
cp "$BUILD_TMP/$SERVICE_NAME" "$BIN_DIR/$SERVICE_NAME.$TIMESTAMP"
ln -sfn "$SERVICE_NAME.$TIMESTAMP" "$BIN_DIR/$SERVICE_NAME"
log_info "   ✓ $SERVICE_NAME -> $SERVICE_NAME.$TIMESTAMP"

# 4. Install the systemd unit and reload systemd.
log_info "4. Installing systemd unit..."
sudo install -m 0644 "$UNIT_SOURCE" "$UNIT_TARGET"
sudo systemctl daemon-reload
sudo systemctl enable "$SERVICE_NAME" >/dev/null
log_info "   ✓ systemd unit installed"

# 5. Restart the profiler watcher and verify it stays active.
log_info "5. Restarting $SERVICE_NAME..."
if ! sudo systemctl restart "$SERVICE_NAME"; then
    log_error "   ✗ Service restart failed, attempting automatic rollback..."
    sudo systemctl status "$SERVICE_NAME" --no-pager || true
    if rollback && wait_for_active; then
        log_error "   ✗ New profiler build failed, rolled back to $PREVIOUS_TARGET"
    else
        log_error "   ✗ New profiler build failed and automatic rollback also failed"
        sudo systemctl status "$SERVICE_NAME" --no-pager || true
    fi
    exit 1
fi

if wait_for_active; then
    log_info "   ✓ $SERVICE_NAME is active"
else
    log_error "   ✗ Service did not remain active, attempting automatic rollback..."
    sudo systemctl status "$SERVICE_NAME" --no-pager || true
    if rollback && wait_for_active; then
        log_error "   ✗ New profiler build failed post-start checks, rolled back to $PREVIOUS_TARGET"
    else
        log_error "   ✗ New profiler build failed post-start checks and rollback also failed"
        sudo systemctl status "$SERVICE_NAME" --no-pager || true
    fi
    exit 1
fi

# 6. Remove old profiler binaries, keeping the latest 10.
log_info "6. Cleaning old profiler binaries (keep latest 10)..."
ls -t "$BIN_DIR/$SERVICE_NAME."* 2>/dev/null | tail -n +11 | xargs -r rm -f
log_info "   ✓ Cleanup complete"

ROLLBACK_HINT="No previous deployed profiler binary recorded"
if [ -n "$PREVIOUS_TARGET" ]; then
    ROLLBACK_HINT="ln -sfn $PREVIOUS_TARGET $BIN_DIR/$SERVICE_NAME && sudo systemctl restart $SERVICE_NAME"
fi

echo ""
log_info "========================================"
log_info "  Deployment complete! Current version: $TIMESTAMP"
log_info "========================================"
echo ""
echo "Version:          $("$BIN_DIR/$SERVICE_NAME" -version)"
echo "Service status:   sudo systemctl status $SERVICE_NAME"
echo "Logs:             journalctl -u $SERVICE_NAME -f"
echo "Capture output:   ls -lah /home/iec/deploy/log/cliproxy-profiler"
echo "Quick rollback:   $ROLLBACK_HINT"
echo ""
