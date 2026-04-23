#!/usr/bin/env bash

set -euo pipefail

NODE_MAJOR="${NODE_MAJOR:-24}"
NODE_VERSION="${NODE_VERSION:-24.14.1}"
NODE_DEB_VERSION="${NODE_DEB_VERSION:-24.14.1-1nodesource1}"
HOLD_PACKAGE="${HOLD_PACKAGE:-1}"

if ! command -v apt-get >/dev/null 2>&1; then
  echo "This script requires apt-get." >&2
  exit 1
fi

if ! command -v sudo >/dev/null 2>&1; then
  echo "This script requires sudo." >&2
  exit 1
fi

echo "Installing Node.js ${NODE_VERSION} from NodeSource (${NODE_MAJOR}.x)..."

sudo apt-get update
sudo apt-get install -y ca-certificates curl gnupg
sudo mkdir -p /etc/apt/keyrings

curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
  | sudo gpg --dearmor --yes -o /etc/apt/keyrings/nodesource.gpg

printf '%s\n' \
  "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
  | sudo tee /etc/apt/sources.list.d/nodesource.list >/dev/null

sudo apt-get update
sudo apt-get install -y "nodejs=${NODE_DEB_VERSION}"

if [[ "${HOLD_PACKAGE}" == "1" ]]; then
  sudo apt-mark hold nodejs
fi

echo "Verification:"
node -v
npm -v
which node
