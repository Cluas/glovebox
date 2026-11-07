#!/system/bin/sh
# Glovebox installs this file into /data/adb/service.d. Magisk runs that folder at every boot that
# is not in safe mode, also while modules are disabled.
#
# When the power supply sags while the device boots, Magisk can end up in safe mode, which leaves
# a "disable" file in every module: Glovebox and Box then stay off at the following boots. Magisk
# creates those files with mode 000 and the switch in the Magisk app does not, so a module turned
# off by hand stays off. This removes the files safe mode left in Glovebox and Box, starts them
# and leaves a note that Glovebox reports.

ADB=${GLOVEBOX_ADB:-/data/adb} # tests use a scratch folder
MODS=$ADB/modules
BOX=$ADB/box

# Glovebox was uninstalled
if [ ! -d "$MODS/glovebox" ]; then
  rm -f "$0"
  exit 0
fi

left_by_safe_mode() {
  case "$(ls -l "$1" 2>/dev/null)" in
  ----------*) return 0 ;;
  esac
  return 1
}

box_running() {
  pid=$(cat "$BOX/run/box.pid" 2>/dev/null)
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

rescue() {
  if [ -z "$GLOVEBOX_NOW" ]; then
    until [ "$(getprop sys.boot_completed)" = 1 ]; do sleep 2; done
  fi
  restored=
  for m in glovebox box_for_root; do
    if left_by_safe_mode "$MODS/$m/disable" && rm -f "$MODS/$m/disable"; then
      restored="$restored $m"
    fi
  done
  [ -n "$restored" ] || return 0
  mkdir -p "$ADB/glovebox"
  echo "$restored" >>"$ADB/glovebox/rescued"
  case "$restored" in
  *glovebox*) sh "$MODS/glovebox/service.sh" ;; # Magisk skipped it at this boot
  esac
  case "$restored" in
  *box_for_root*)
    # Box's own boot script starts it once its disable file is gone, unless that happened just
    # before it began to watch the file: then start it the way its watcher does
    sleep "${GLOVEBOX_BOX_WAIT:-60}"
    if [ -x "$BOX/scripts/box.service" ] && ! box_running; then
      "$BOX/scripts/box.service" start && "$BOX/scripts/box.iptables" enable
    fi
    ;;
  esac
}

if [ -n "$GLOVEBOX_NOW" ]; then
  rescue
else
  (rescue >/dev/null 2>&1 &)
fi
