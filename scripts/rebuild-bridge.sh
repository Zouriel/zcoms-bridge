#!/usr/bin/env bash
# Rebuild the zcoms-bridge component from source, reinstall the binary, and
# restart its user service so source changes (e.g. commerce routing / /help /
# agent seeds) go live. Run this on a machine that has the Go toolchain and the
# zcoms-bridge source checked out.
#
# Safe by design: `set -e` means a failed build aborts BEFORE the restart, so a
# broken build never replaces a working bridge.
set -euo pipefail

REPO_DIR="${REPO_DIR:-$HOME/personal/Zcoms/zcoms-bridge}"
BIN="${BIN:-$HOME/.local/bin/zcoms-bridge}"
UNIT="zcoms-bridge.service"

if ! command -v go >/dev/null 2>&1; then
	echo "[rebuild] ERROR: Go toolchain not found on PATH — cannot build here." >&2
	echo "[rebuild] Run this on your build machine/VPS agent (the one that built the components)." >&2
	exit 1
fi

cd "$REPO_DIR"
echo "[rebuild] pulling latest…"
git pull --ff-only

echo "[rebuild] building -> $BIN"
go build -trimpath -ldflags="-s -w" -o "$BIN" .

echo "[rebuild] restarting $UNIT…"
systemctl --user restart "$UNIT"
sleep 1
systemctl --user --no-pager --lines=0 status "$UNIT" | head -4
echo "[rebuild] done — bridge is live with the latest build."
