#!/system/bin/sh
# Runs at Magisk late_start; can also be run by hand (exits if already running)
MODDIR=${0%/*}
DATA=/data/adb/glovebox
PIDFILE=$DATA/supervisor.pid
UPD=$DATA/update

mkdir -p "$DATA"
chmod 700 "$DATA"

# a stale pid may be reused after reboot, so also check the command line
pid=$(cat "$PIDFILE" 2>/dev/null)
if [ -n "$pid" ] && grep -q glovebox/service.sh "/proc/$pid/cmdline" 2>/dev/null; then
  exit 0
fi

until [ "$(getprop sys.boot_completed)" = 1 ]; do sleep 2; done

(
  crashes=0
  while true; do
    [ -f "$MODDIR/disable" ] && break
    out=$DATA/stdout.log
    [ -f "$out" ] && [ "$(stat -c %s "$out" 2>/dev/null || echo 0)" -gt 262144 ] && mv -f "$out" "$out.1"
    started=$(date +%s)
    "$MODDIR/bin/glovebox" -config "$DATA/config.json" >> "$out" 2>&1
    echo "$(date '+%F %T') glovebox exited ($?), restarting in 5 s" >> "$out"
    # A remote upgrade (update.go) is undone by the previous binary when the new one does not stay
    # up. After a power cut that watcher is gone, so a new binary that keeps crashing at start is
    # undone here.
    if [ -f "$UPD/pending" ] && [ $(($(date +%s) - started)) -lt 60 ]; then
      crashes=$((crashes + 1))
    else
      crashes=0
    fi
    if [ "$crashes" -ge 3 ] && [ -x "$UPD/prev/bin/glovebox" ]; then
      for f in module.prop service.sh uninstall.sh customize.sh bin/glovebox; do
        [ -f "$UPD/prev/$f" ] && cp -f "$UPD/prev/$f" "$MODDIR/$f.new" && mv -f "$MODDIR/$f.new" "$MODDIR/$f"
      done
      echo "the new version crashed $crashes times at start" > "$UPD/rolled_back"
      rm -f "$UPD/pending"
      echo "$(date '+%F %T') upgrade undone, the previous version is back" >> "$out"
      crashes=0
    fi
    sleep 5
  done
  rm -f "$PIDFILE"
) &
echo $! > "$PIDFILE"
