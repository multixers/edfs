#!/usr/bin/env bash
#
# One-command install/upgrade for edfs (the eidos filesystem driver).
#
#   curl -fsSL https://raw.githubusercontent.com/multixers/edfs/main/install.sh | sudo bash
#
# Downloads the latest release binary to /usr/local/bin/edfs. Re-run any time to
# upgrade. Also ensures fuse3 (fusermount3) — needed for unprivileged mounts;
# harmless when edfs runs as root.
set -euo pipefail

REPO="multixers/edfs"
BIN="/usr/local/bin/edfs"
ASSET="edfs-linux-amd64"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root (pipe to 'sudo bash')" >&2
  exit 1
fi

echo "==> downloading latest ${REPO} binary"
curl -fsSL -o "$BIN" "https://github.com/${REPO}/releases/latest/download/${ASSET}"
chmod +x "$BIN"

if command -v apt-get >/dev/null 2>&1; then
  echo "==> ensuring fuse3"
  apt-get install -y fuse3 >/dev/null 2>&1 \
    || { apt-get update -qq && apt-get install -y fuse3 >/dev/null 2>&1; } \
    || echo "   (fuse3 not installed — fine if edfs runs as root)"
fi

echo "==> installed ${BIN}"
