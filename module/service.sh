#!/system/bin/sh
# Runs at Magisk late_start; can also be run by hand (exits if already running)
MODDIR=${0%/*}
DATA=/data/adb/glovebox
PIDFILE=$DATA/supervisor.pid

mkdir -p "$DATA"
chmod 700 "$DATA"

# a stale pid may be reused after reboot, so also check the command line
pid=$(cat "$PIDFILE" 2>/dev/null)
if [ -n "$pid" ] && grep -q glovebox/service.sh "/proc/$pid/cmdline" 2>/dev/null; then
  exit 0
fi

until [ "$(getprop sys.boot_completed)" = 1 ]; do sleep 2; done

(
  while true; do
    [ -f "$MODDIR/disable" ] && break
    out=$DATA/stdout.log
    [ -f "$out" ] && [ "$(stat -c %s "$out" 2>/dev/null || echo 0)" -gt 262144 ] && mv -f "$out" "$out.1"
    "$MODDIR/bin/glovebox" -config "$DATA/config.json" >> "$out" 2>&1
    echo "$(date '+%F %T') glovebox exited ($?), restarting in 5 s" >> "$out"
    sleep 5
  done
  rm -f "$PIDFILE"
) &
echo $! > "$PIDFILE"
