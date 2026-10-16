#!/usr/bin/env bash
# Install dist/stage into /data/adb/modules/glovebox on a device connected over adb and
# (re)start the console without rebooting. Requires root (su) on the device.
#
#   GLOVEBOX_PASSWORD_FILE=path   set the console password from this file on first install
#   ADB_SERIAL=serial             pick a device when several are connected
set -euo pipefail
cd "$(dirname "$0")"

stage=dist/stage
[[ -x $stage/bin/glovebox ]] || { echo "run ./build.sh first" >&2; exit 1; }
adb=(adb)
[[ -n ${ADB_SERIAL:-} ]] && adb+=(-s "$ADB_SERIAL")
pwfile=${GLOVEBOX_PASSWORD_FILE:-}
tmp=/data/local/tmp/glovebox-stage

"${adb[@]}" shell "rm -rf $tmp" >/dev/null
"${adb[@]}" push "$stage" "$tmp" >/dev/null
if [[ -n $pwfile ]]; then
  "${adb[@]}" push "$pwfile" "$tmp/initial-password" >/dev/null
fi

"${adb[@]}" shell "su -c 'sh -s'" <<SH
set -e
MOD=/data/adb/modules/glovebox
DATA=/data/adb/glovebox
mkdir -p \$MOD \$DATA
chmod 700 \$DATA
cp -f $tmp/module.prop $tmp/service.sh $tmp/uninstall.sh $tmp/customize.sh \$MOD/
# files under system/ are mounted by Magisk at boot, so changes need a reboot
if [ -d $tmp/system ]; then
  if ! diff -r $tmp/system \$MOD/system >/dev/null 2>&1; then
    rm -rf \$MOD/system && cp -r $tmp/system \$MOD/system
    echo "note: system/ changed, reboot the device to apply"
  fi
fi
mkdir -p \$MOD/bin
# copy then rename so a running binary is never overwritten in place
cp -f $tmp/bin/glovebox \$MOD/bin/glovebox.new
chmod 755 \$MOD/bin/glovebox.new \$MOD/service.sh
mv -f \$MOD/bin/glovebox.new \$MOD/bin/glovebox
chown -R 0:0 \$MOD
if ! grep -q pbkdf2 \$DATA/config.json 2>/dev/null; then
  if [ -f $tmp/initial-password ]; then
    \$MOD/bin/glovebox -config \$DATA/config.json passwd < $tmp/initial-password
  else
    echo "warning: no console password set; run: echo <password> | \$MOD/bin/glovebox passwd"
  fi
fi
rm -rf $tmp
# if the supervisor is running, kill the binary and let it restart the new one
pid=\$(cat \$DATA/supervisor.pid 2>/dev/null || true)
if [ -n "\$pid" ] && grep -q glovebox/service.sh /proc/\$pid/cmdline 2>/dev/null; then
  pkill -f "\$MOD/bin/glovebox" || true
  echo "restarted glovebox (supervisor \$pid)"
else
  setsid sh \$MOD/service.sh </dev/null >/dev/null 2>&1 &
  echo "started supervisor"
fi
SH
