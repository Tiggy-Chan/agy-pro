#!/usr/bin/env sh
# One-line installer for agy-pro (Linux & macOS, amd64 & arm64)
# Usage: curl -fsSL https://raw.githubusercontent.com/Tiggy-Chan/agy-pro/main/install.sh | sh
set -eu

REPO="Tiggy-Chan/agy-pro"
INSTALL_DIR="${AGY_PRO_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${AGY_PRO_VERSION:-}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  linux|darwin) ;;
  *)
    echo "error: unsupported OS '$OS' (expected linux or darwin)" >&2
    exit 1
    ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    echo "error: unsupported architecture '$ARCH' (expected amd64 or arm64)" >&2
    exit 1
    ;;
esac

if [ -z "$VERSION" ]; then
  LATEST_URL="$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest")"
  VERSION="${LATEST_URL##*/}"
  if [ -z "$VERSION" ] || [ "$VERSION" = "releases" ] || [ "$VERSION" = "latest" ]; then
    echo "error: could not resolve latest release tag from github.com/${REPO}" >&2
    exit 1
  fi
fi

ASSET="agy-pro-${OS}-${ARCH}"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

echo "Downloading agy-pro ${VERSION} (${OS}/${ARCH})..."
curl -fsSL "${BASE_URL}/${ASSET}" -o "${TMP_DIR}/${ASSET}"
curl -fsSL "${BASE_URL}/checksums.txt" -o "${TMP_DIR}/checksums.txt"

EXPECTED="$(grep "  ${ASSET}\$" "${TMP_DIR}/checksums.txt" | awk '{print $1}')"
if [ -z "$EXPECTED" ]; then
  echo "error: checksum entry for ${ASSET} not found in checksums.txt" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "${TMP_DIR}/${ASSET}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL="$(shasum -a 256 "${TMP_DIR}/${ASSET}" | awk '{print $1}')"
else
  echo "error: sha256sum or shasum is required to verify download integrity" >&2
  exit 1
fi

if [ "$ACTUAL" != "$EXPECTED" ]; then
  echo "error: SHA-256 verification failed for ${ASSET}" >&2
  echo "  expected: ${EXPECTED}" >&2
  echo "  actual:   ${ACTUAL}" >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR"
install -m 0755 "${TMP_DIR}/${ASSET}" "${INSTALL_DIR}/agy-pro"
echo "Installed agy-pro ${VERSION} -> ${INSTALL_DIR}/agy-pro"

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    echo ""
    echo "Note: ${INSTALL_DIR} is not in your PATH yet. Add this line to your shell profile:"
    echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
    ;;
esac
