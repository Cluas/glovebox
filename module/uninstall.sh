# Remove the alias address from br0 on uninstall; config and logs stay in /data/adb/glovebox
ip addr del 192.168.0.254/32 dev br0 2>/dev/null
rm -f /data/adb/glovebox/supervisor.pid
