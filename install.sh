#!/usr/bin/env bash
# install.sh — one-line installer for macOS / Linux / WSL2.
#
# Typical usage:
#   curl -fsSL https://raw.githubusercontent.com/<owner>/a2abridge/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/<owner>/a2abridge/main/install.sh | bash -s -- --version v0.2.0
#
# Env overrides:
#   A2A_REPO         GitHub repo in owner/name form (default: vbcherepanov/a2abridge)
#   A2A_VERSION      tag to install (default: latest release)
#   A2A_PREFIX       install prefix (default: ~/.a2abridge)
#   A2A_NO_SERVICE   set to "1" to skip the supervisor install step
#   A2A_NO_IDE       set to "1" to skip writing IDE configs

set -euo pipefail

REPO="${A2A_REPO:-vbcherepanov/a2abridge}"
VERSION="${A2A_VERSION:-}"
PREFIX="${A2A_PREFIX:-$HOME/.a2abridge}"
APPLY="--apply"

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --prefix)  PREFIX="$2";  shift 2 ;;
    --dry-run) APPLY="";     shift ;;
    --help|-h)
      sed -n '2,15p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

# --- detect platform ---------------------------------------------------
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin|linux) ;;
  *) echo "unsupported OS for install.sh: $os (use install.ps1 on Windows)" >&2; exit 1 ;;
esac

# --- resolve version ---------------------------------------------------
if [ -z "$VERSION" ]; then
  echo "→ resolving latest release for $REPO"
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep -m1 '"tag_name"' \
    | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
fi
[ -z "$VERSION" ] && { echo "could not resolve latest version" >&2; exit 1; }
echo "→ installing a2abridge $VERSION ($os/$arch) into $PREFIX"

# --- download + extract ------------------------------------------------
TARBALL="a2abridge_${VERSION#v}_${os}_${arch}.tar.gz"
URL="https://github.com/$REPO/releases/download/$VERSION/$TARBALL"
TMP=$(mktemp -d -t a2abridge-install-XXXXXX)
trap 'rm -rf "$TMP"' EXIT

echo "→ downloading $URL"
if ! curl -fsSL "$URL" -o "$TMP/a2abridge.tar.gz"; then
  echo "download failed; verify a release exists at $URL" >&2
  exit 1
fi

mkdir -p "$PREFIX/bin"
tar -xzf "$TMP/a2abridge.tar.gz" -C "$TMP"
# Tarballs ship a single binary at the root.
mv "$TMP/a2abridge" "$PREFIX/bin/a2abridge"
chmod +x "$PREFIX/bin/a2abridge"

# --- register IDEs + skill + hook --------------------------------------
if [ "${A2A_NO_IDE:-0}" != "1" ]; then
  echo "→ registering MCP server in detected IDEs"
  "$PREFIX/bin/a2abridge" install $APPLY
fi

# --- service supervisor ------------------------------------------------
if [ -n "$APPLY" ] && [ "${A2A_NO_SERVICE:-0}" != "1" ]; then
  echo "→ installing directory daemon"
  "$PREFIX/bin/a2abridge" service install || \
    echo "  service install failed — you can retry with: $PREFIX/bin/a2abridge service install"
fi

# --- post-install summary ---------------------------------------------
cat <<EOF

a2abridge $VERSION installed.

  binary:  $PREFIX/bin/a2abridge
  doctor:  $PREFIX/bin/a2abridge doctor
  service: $PREFIX/bin/a2abridge service status

Add this to your shell profile so a2abridge is on PATH:

  export PATH="$PREFIX/bin:\$PATH"

Restart your IDEs (Claude Code, Codex, Cursor, ...) to pick up the new MCP server.
EOF
