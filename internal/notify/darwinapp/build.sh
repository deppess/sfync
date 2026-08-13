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

echo "Compiling main.swift..."
swiftc -O -whole-module-optimization main.swift -o "$BIN"
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
