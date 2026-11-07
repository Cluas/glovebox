# Remove the alias address from br0 on uninstall; config and logs stay in /data/adb/glovebox
ip addr del 192.168.0.254/32 dev br0 2>/dev/null
rm -f /data/adb/glovebox/supervisor.pid
# the boot script that turns the modules back on after Magisk's safe mode (rescue.go)
rm -f /data/adb/service.d/glovebox-rescue.sh
