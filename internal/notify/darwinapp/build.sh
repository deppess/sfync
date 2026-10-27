#!/usr/bin/env bash
# Builds sfync-notifier.app from main.swift and ad-hoc code-signs it.
#
# Run this manually only when main.swift, Info.plist, or AppIcon.icns
# change — the compiled .app is committed to the repo and embedded into the
# sfync binary via go:embed, so `go build` itself never needs swiftc.
set -euo pipefail
cd "$(dirname "$0")"

APP="sfync-notifier.app"
BIN="$APP/Contents/MacOS/sfync-notifier"

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

# Deployment target: matches the Go toolchain's macOS floor (Go 1.27
# requires macOS 13) and LSMinimumSystemVersion in Info.plist. Without an
# explicit -target, swiftc targets the build machine's own macOS version and
# the helper silently fails to launch on anything older.
MIN_MACOS=13.0

# Universal binary: Apple Silicon and Intel. The arm64 slice must stay
# native — Apple is removing general Rosetta support starting with macOS 28
# and flags bundled helper components that lack it
# (https://support.apple.com/en-us/102527). The x86_64 slice is for Intel
# Macs only; it can be smoke-tested on Apple silicon with
# `arch -x86_64 <binary>` only while Rosetta is still available.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for ARCH in arm64 x86_64; do
  echo "Compiling main.swift ($ARCH, macOS $MIN_MACOS+)..."
  swiftc -O -whole-module-optimization -target "$ARCH-apple-macos$MIN_MACOS" \
    main.swift -o "$WORK/sfync-notifier-$ARCH"
done
lipo -create "$WORK/sfync-notifier-arm64" "$WORK/sfync-notifier-x86_64" -output "$BIN"
chmod 755 "$BIN"

cp Info.plist "$APP/Contents/Info.plist"
printf 'APPL????' > "$APP/Contents/PkgInfo"

if [ -f AppIcon.icns ]; then
  cp AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"
  # Wire the icon into Info.plist if it isn't already referenced.
  if ! grep -q CFBundleIconFile "$APP/Contents/Info.plist"; then
    /usr/libexec/PlistBuddy -c "Add :CFBundleIconFile string AppIcon" "$APP/Contents/Info.plist"
  fi
else
  echo "No AppIcon.icns found — building with the default system icon."
  echo "Run ./make_icon.sh <path-to-icon.svg> then re-run this script to brand it."
fi

echo "Ad-hoc signing..."
codesign --force --deep --sign - "$APP"
codesign --verify --verbose "$APP"

echo "Built $APP — remember to commit it."
