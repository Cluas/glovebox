#!/usr/bin/env bash
# Cross-compile glovebox for arm64 Android (static) and package it as a Magisk module.
#
#   ./build.sh                  -> dist/stage/ (module tree) and dist/glovebox-<version>.zip
#   VERSION=1.2.3 ./build.sh    -> explicit version (default: date-based)
#   NO_OVERLAY=1 ./build.sh     -> skip the SoftAP client-limit overlay APK
set -euo pipefail
cd "$(dirname "$0")"

ver=${VERSION:-$(date +%Y%m%d.%H%M)}
stage=dist/stage
rm -rf "$stage"
mkdir -p "$stage/bin"

unformatted=$(gofmt -l .)
if [[ -n $unformatted ]]; then
  echo "run gofmt on: $unformatted" >&2
  exit 1
fi
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$ver" -o "$stage/bin/glovebox" .
cp -R module/. "$stage/"

# Runtime resource overlay that raises the SoftAP client limit. Magisk mounts the module's
# system/vendor onto /vendor, so it lands in /vendor/overlay.
if [[ -z ${NO_OVERLAY:-} ]]; then
  [[ -f overlay/GloveboxWifiOverlay.apk ]] || overlay/build.sh
  mkdir -p "$stage/system/vendor/overlay/GloveboxWifiOverlay"
  cp overlay/GloveboxWifiOverlay.apk "$stage/system/vendor/overlay/GloveboxWifiOverlay/"
  chmod 644 "$stage/system/vendor/overlay/GloveboxWifiOverlay/GloveboxWifiOverlay.apk"
fi

sed -i.bak -e "s/^version=.*/version=$ver/" -e "s/^versionCode=.*/versionCode=$(date +%y%m%d%H)/" "$stage/module.prop"
rm -f "$stage/module.prop.bak"
chmod 755 "$stage/bin/glovebox" "$stage/service.sh"

rm -f dist/glovebox-*.zip
(cd "$stage" && zip -qr -X "../glovebox-$ver.zip" .)
ls -l "$stage/bin/glovebox" "dist/glovebox-$ver.zip"
