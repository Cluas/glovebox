# Runs when Magisk installs the module
SKIPUNZIP=0
set_perm_recursive "$MODPATH" 0 0 0755 0644
set_perm "$MODPATH/bin/glovebox" 0 0 0755
set_perm "$MODPATH/service.sh" 0 0 0755
[ -d "$MODPATH/system" ] && set_perm_recursive "$MODPATH/system" 0 0 0755 0644
mkdir -p /data/adb/glovebox
chmod 700 /data/adb/glovebox
ui_print "- Config and logs: /data/adb/glovebox/"
ui_print "- Set a console password before first use:"
ui_print "    echo <password> | /data/adb/modules/glovebox/bin/glovebox passwd"
