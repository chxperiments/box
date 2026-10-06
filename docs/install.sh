#!/bin/sh
# box installer.
#
#   curl -fsSL https://chxperiments.github.io/bluebox/install.sh | sh
#
# Downloads the release build for this platform and puts it on your PATH.
# Override where it lands with BOX_BIN_DIR, or pin a version with
# BOX_VERSION=v0.2.0.
set -eu

REPO="chxperiments/bluebox"
BIN_DIR="${BOX_BIN_DIR:-$HOME/.local/bin}"

die() { echo "install: $*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar  >/dev/null 2>&1 || die "tar is required"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) die "unsupported OS: $os (box builds for linux and darwin)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac

version="${BOX_VERSION:-}"
if [ -z "$version" ]; then
  # Resolve the latest tag without needing jq.
  version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$version" ] || die "could not determine the latest release. Set BOX_VERSION to pick one."
fi

asset="box_${version}_${os}_${arch}.tar.gz"
url="https://github.com/$REPO/releases/download/$version/$asset"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "install: fetching $version for $os/$arch"
curl -fsSL "$url" -o "$tmp/$asset" || die "could not download $url"
tar -xzf "$tmp/$asset" -C "$tmp" || die "could not unpack $asset"
[ -f "$tmp/box" ] || die "archive did not contain a box binary"

mkdir -p "$BIN_DIR"
# install(1) is not on every minimal system; fall back to cp.
if command -v install >/dev/null 2>&1; then
  install -m 0755 "$tmp/box" "$BIN_DIR/box"
else
  cp "$tmp/box" "$BIN_DIR/box" && chmod 0755 "$BIN_DIR/box"
fi

echo "install: box $version -> $BIN_DIR/box"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "install: note - $BIN_DIR is not on your PATH" ;;
esac

echo "install: box also needs podman and libkrun. See the README."
