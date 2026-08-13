#!/usr/bin/env bash
# Converts an SVG into AppIcon.icns for sfync-notifier.app.
# Usage: ./make_icon.sh path/to/icon.svg
set -euo pipefail
cd "$(dirname "$0")"

SVG="${1:?usage: make_icon.sh <icon.svg>}"
ICONSET="AppIcon.iconset"

rm -rf "$ICONSET"
mkdir "$ICONSET"

for size in 16 32 128 256 512; do
  sips -s format png -z "$size" "$size" "$SVG" --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
  double=$((size * 2))
  sips -s format png -z "$double" "$double" "$SVG" --out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
done

iconutil -c icns "$ICONSET" -o AppIcon.icns
rm -rf "$ICONSET"

echo "Generated AppIcon.icns — run ./build.sh to rebuild the app with it."
