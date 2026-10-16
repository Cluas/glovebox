#!/usr/bin/env bash
# Build GloveboxWifiOverlay.apk, a static runtime resource overlay that raises the SoftAP client
# limit. Needs Android SDK build-tools and a platform android.jar. Overlays under /vendor/overlay
# are not checked against a specific signer, so a throwaway key is generated if none is given.
#
#   BUILD_TOOLS=<sdk>/build-tools/35.0.0  ANDROID_JAR=<sdk>/platforms/android-34/android.jar
#   KEYSTORE=path KEYSTORE_PASS=secret
set -euo pipefail
cd "$(dirname "$0")"

sdk=${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$HOME/Library/Android/sdk}}
bt=${BUILD_TOOLS:-$(ls -d "$sdk"/build-tools/* 2>/dev/null | sort -V | tail -1)}
jar=${ANDROID_JAR:-$(ls "$sdk"/platforms/android-*/android.jar 2>/dev/null | sort -V | tail -1)}
ks=${KEYSTORE:-overlay.keystore}
pass=${KEYSTORE_PASS:-android}
[[ -x $bt/aapt2 && -f $jar ]] || { echo "Android SDK build-tools / android.jar not found; set BUILD_TOOLS and ANDROID_JAR" >&2; exit 1; }

if [[ ! -f $ks ]]; then
  keytool -genkeypair -keystore "$ks" -storepass "$pass" -keypass "$pass" -alias overlay \
    -keyalg EC -groupname secp256r1 -validity 36500 -dname "CN=Glovebox Overlay" >/dev/null
fi

rm -rf build && mkdir build
"$bt/aapt2" compile --dir res -o build/res.zip
"$bt/aapt2" link -I "$jar" --manifest AndroidManifest.xml -o build/unsigned.apk build/res.zip
"$bt/zipalign" -f 4 build/unsigned.apk build/aligned.apk
"$bt/apksigner" sign --ks "$ks" --ks-pass "pass:$pass" --key-pass "pass:$pass" --out GloveboxWifiOverlay.apk build/aligned.apk
rm -rf build GloveboxWifiOverlay.apk.idsig
"$bt/aapt2" dump resources GloveboxWifiOverlay.apk | grep -A1 SoftapMaxClient
