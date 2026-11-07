# Glovebox

[![CI](https://github.com/Cluas/glovebox/actions/workflows/ci.yml/badge.svg)](https://github.com/Cluas/glovebox/actions/workflows/ci.yml)

**A LuCI-style web console for rooted Android mobile routers.** Glovebox runs as root on the
stock firmware of a pocket 5G/4G Wi-Fi router and gives you one place to watch the cellular link,
manage the hotspot, drive a [mihomo](https://github.com/MetaCubeX/mihomo) proxy and read logs —
without flashing OpenWrt.

English · [简体中文](README.zh-CN.md)

![Overview](docs/screenshots/en/overview.png)

- **Single static binary**, web UI embedded, no runtime dependencies. Packaged as a Magisk module.
- **Works with the stock firmware**: talks to the vendor web API (ZTE goform), mihomo's REST API
  and `/proc`, `/sys` — the vendor UI and apps keep working.
- **Installable app (PWA)** with a responsive layout for desktop, tablets, phones (portrait and
  landscape) and in-vehicle / embedded browsers. HTTPS via a generated, name-constrained local CA.
- **Themes**: iOS and Material You looks that follow the system's light or dark mode, plus seven
  terminal palettes. The first visit picks one for the device: iOS on iPhone and iPad, Material You
  on Android, the terminal look elsewhere.
- **Internationalized** (English, 简体中文); adding a language is adding one JSON file.
- **Demo mode** runs the whole console on any computer with fake backends.

## Features

| Page | What you get |
| --- | --- |
| Overview | Quick actions (proxy mode, exit node, speed test, subscription update, connections, Wi-Fi band, restart proxy, reboot), cards that open the matching detail, live WAN throughput chart, signal (RSRP/SINR/band) with history, egress node and proxy chain, Tailscale reachability probe, monthly data usage, battery/CPU/memory/temperature gauges, connected clients, health checks and their events |
| Proxy | mihomo groups and nodes, switch nodes, latency tests, rule/global/direct mode, update subscriptions and rule sets, live connections with filter and "close all" |
| Device | Hotspot band (2.4 GHz or 5 GHz — the F50 runs one at a time) or off, SSID/password/visibility per band, max clients (up to the system limit), connected devices, alert and log shipping status, software update, remote diagnostics results, HTTPS & app install guide, restart proxy, reboot, sync the device clock, console password, theme, light/dark and language |
| Logs | mihomo, Box for Root, Glovebox and Android logcat — level filter, grep with highlight, follow/pause, wrap |

Keyboard: `1`–`4` switch pages, `/` focuses the filter.

<p>
<img src="docs/screenshots/en/proxy.png" width="49%" alt="Proxy">
<img src="docs/screenshots/en/logs.png" width="49%" alt="Logs">
</p>
<p>
<img src="docs/screenshots/en/device.png" width="66%" alt="Device">
<img src="docs/screenshots/en/phone.png" width="22%" alt="Phone layout">
</p>
<p>
<img src="docs/screenshots/en/themes.png" width="100%" alt="Themes: iOS light, iOS dark, Material You, Dracula">
</p>

## Supported devices

Glovebox currently targets the **ZTE F50** (Unisoc platform, Android 13, firmware B09) and the
vendor stack it ships with. Anything that talks to the hardware is behind small clients
(`zte.go`, `mihomo.go`, `sys.go`), so other Android-based routers can be added — contributions are
welcome.

Requirements on the device:

- Root with [Magisk](https://github.com/topjohnwu/Magisk)
- Optional: [Box for Root](https://github.com/taamarin/box_for_magisk) running mihomo with the
  external controller on `127.0.0.1:9091` (the proxy page and charts degrade gracefully without it)

## Install

1. Download `glovebox-<version>.zip` from the [releases page](https://github.com/Cluas/glovebox/releases) (or [build it](#build)) and install it
   in the Magisk app, then reboot.
2. Set the console password (once):

   ```sh
   adb shell su -c "'echo YOUR-PASSWORD | /data/adb/modules/glovebox/bin/glovebox passwd'"
   ```

3. Join the hotspot and open **http://192.168.0.254**. Glovebox adds this address to `br0` (the
   vendor DHCP pool ends at .253) so it never collides with the vendor UI on 192.168.0.1.

For a friendly name, resolve `glovebox.lan` to `192.168.0.254`, e.g. with mihomo:

```yaml
hosts:
  glovebox.lan: 192.168.0.254
```

### HTTPS and installing as an app

Browsers only install PWAs from secure origins. On first start Glovebox creates a local root CA in
`/data/adb/glovebox/tls/` and serves HTTPS on `192.168.0.254:443`. The CA is **name-constrained**
to the configured hostnames and listen IPs, so it cannot sign certificates for any other site even
if its key leaks. The Device page has a download button and step-by-step instructions for iOS,
Android, macOS and Windows; compare the SHA-256 fingerprint shown there before trusting it.

Prefer your own certificate? Set `tls_cert` / `tls_key`.

### Hotspot client limit

Vendor resource overlays cap the SoftAP at 10 clients
(`config_wifiHardwareSoftapMaxClientCount`). The module ships a static runtime resource overlay
(`com.glovebox.wifi.overlay`, priority 900) that raises it to 32; the Device page offers values
up to the limit the system reports (`dumpsys wifi`). The real ceiling also depends on the Wi-Fi
firmware. Build with `NO_OVERLAY=1` to leave the vendor limit alone. If the hotspot misbehaves,
delete `/data/adb/modules/glovebox/system` and reboot.

## Configuration

`/data/adb/glovebox/config.json` stores only values that differ from the defaults. Inspect the
effective configuration with `glovebox config`, merge changes with `glovebox config import`, then
restart (`su -c 'pkill -f bin/glovebox'`; the supervisor starts it again):

```sh
echo '{"tailscale_proxy":"Tailscale","tailscale_probe":"http://100.64.0.1/"}' \
  | su -c '/data/adb/modules/glovebox/bin/glovebox config import'
```

| Key | Default | Description |
| --- | --- | --- |
| `listen` | `["192.168.0.254:80"]` | HTTP listen addresses |
| `tls_listen` | `["192.168.0.254:443"]` | HTTPS listen addresses; empty disables HTTPS |
| `tls_cert`, `tls_key` | | Custom certificate and key (PEM) instead of the local CA |
| `hostnames` | `["glovebox.lan"]` | Names in issued certificates; the first is used for the HTTPS link |
| `alias_addr`, `alias_dev` | `192.168.0.254/32`, `br0` | Address Glovebox adds to the LAN bridge; empty disables it |
| `allow_cidrs` | `["192.168.0.0/24","127.0.0.0/8"]` | Source networks allowed to connect |
| `zte_base`, `zte_db` | `http://192.168.0.1:8080`, vendor DB path | Vendor web API and the database holding its login hash |
| `mihomo_api`, `mihomo_secret` | `http://127.0.0.1:9091` | mihomo external controller |
| `main_group` | first visible selector | Proxy group shown on the overview |
| `box_dir` | `/data/adb/box` | Box for Root directory (logs, restart script) |
| `tailscale_proxy`, `tailscale_probe` | | mihomo outbound and a tailnet URL to probe through it; both enable the Tailnet card |
| `delay_url` | `https://www.gstatic.com/generate_204` | URL for latency tests |
| `wan_prefixes`, `lan_dev` | `sipa_eth`, `rmnet`, `ccmni` / `br0` | Interfaces used for traffic rates |
| `device_name` | `glovebox` | Name in alerts and the `host` label of shipped logs |
| `report_proxy` | | `http://` or `socks5://` proxy for alerts and log shipping |
| `report_lang` | `en` | Language of alerts, one of the `web/i18n` locales |
| `telegram_token`, `telegram_chat` | | Telegram bot token and chat ID; both enable Telegram alerts |
| `webhook_url` | | Receives every health event as a JSON POST |
| `log_push_url` | | Loki push endpoint for logs and events; `user:password@` in the URL is sent as basic auth |
| `dns_probe` | `127.0.0.1:1053` | mihomo DNS listener asked by the DNS check; empty skips that part |
| `update_url` | `update.json` of the latest GitHub release | Signed release manifest checked for [remote upgrades](#remote-upgrade); empty turns them off |
| `update_mode` | `auto` | `auto` installs new versions, `notify` only announces them, `off` stops checking |
| `update_interval` | `1h` | Time between checks; the first runs 2 minutes after start |
| `update_pubkeys` | | Extra ed25519 public keys (base64) trusted to sign releases |
| `diag_url` | `auto` | Signed [diagnostics job](#remote-diagnostics) to poll; `auto` is `diag/<device_name>.json` next to a self-hosted `update_url`, empty turns it off |
| `diag_interval` | `1m` | Time between polls, at least `15s`; every 15 seconds for 10 minutes after a job |

Files in `/data/adb/glovebox/`: `config.json`, `glovebox.log` (rotated at 1 MiB),
`stdout.log` (output of the supervisor in `service.sh`), `tls/` (the local CA), `events.jsonl`
(health events), `clock` (the last trusted time), `notify.json` / `ship.json` (how far alerts
and log shipping got), `update/` (downloads, the previous version and the upgrade state),
`diag.log` (diagnostics results, rotated at 256 KiB) and `diag/` (the jobs that ran). They survive module upgrades and uninstalls.

## Health checks and reporting

Every 30 seconds Glovebox checks mihomo, its DNS (the listener hotspot clients are redirected to,
and the upstream servers behind it), the main proxy group, Tailscale, cellular data, the hotspot,
the clock and free memory. A check counts as failing after two bad rounds (cellular data: after
5 minutes, so short coverage gaps stay quiet) and recovers after one good round. Checks that
depend on a failing one are skipped, so an alert names the cause: no DNS or proxy alerts while
mihomo is down, no proxy alerts while cellular data is. The Overview page lists the checks and
their state changes.

The F50 has no RTC battery: after a power cut it boots at an old date until the cellular network
sets the time. Glovebox notices that the clock is behind the last time it saw, holds alerts and
log shipping until it is corrected, and then fixes the timestamps of the events in between.

Three optional channels report the events, each enabled by its keys in the configuration:

- **Telegram**: create a bot with [@BotFather](https://t.me/BotFather), send it a message, and
  read the chat ID from `https://api.telegram.org/bot<token>/getUpdates`. Failure alerts include
  the latest mihomo warnings.
- **Webhook**: every event is POSTed as JSON (`device`, `time`, `check`, `state`, `msg`, `dur`,
  `text`).
- **Logs**: the mihomo, Box for Root and Glovebox logs and the events are pushed with the Loki
  push protocol, which Grafana Loki, VictoriaLogs and Grafana Alloy accept. The log files are the
  buffer: lines count as sent only once the server accepted them, so whatever was logged without
  coverage is sent later. An event is a JSON line with `check`, `state`, `msg` and `dur`, and a
  summary such as `dns fail: timeout` in `_msg`, the message field of VictoriaLogs.

Undelivered alerts and logs are retried with back-off, also after a restart or a power cut. The
Device page shows the state of each channel and sends a test message on request.

Box for Root lets the device's own traffic to private addresses bypass mihomo. To reach a server
at home through the tailnet, set `report_proxy` to mihomo's mixed port and route the server's
address in the mihomo rules:

```sh
echo '{"device_name":"router","report_proxy":"http://127.0.0.1:7890",
  "telegram_token":"123456:ABC-DEF","telegram_chat":"42",
  "log_push_url":"http://192.168.1.10:9428/insert/loki/api/v1/push"}' \
  | su -c '/data/adb/modules/glovebox/bin/glovebox config import'
```

A VictoriaLogs server that keeps 30 days; its web UI is at `http://<host>:9428/select/vmui`,
where `source:mihomo` shows the mihomo log and `source:events` the health events:

```sh
docker run -d --name victorialogs --restart unless-stopped -p 9428:9428 \
  -v victorialogs:/vlogs victoriametrics/victoria-logs:v1.53.0 \
  -storageDataPath=/vlogs -retentionPeriod=30d
```

## Remote upgrade

Glovebox checks `update_url` for a newer release and installs it on its own, so a fix reaches a
device on the road without adb. The device pulls the release; nothing has to reach it, which suits
cellular networks behind CGNAT.

1. Every `update_interval`, Glovebox fetches `update.json` through `report_proxy`. It names the
   version, the SHA-256 and size of the module zip, and carries an ed25519 signature over them.
   Manifests that are unsigned, signed by an unknown key or not newer than the running build are
   ignored.
2. The zip is downloaded to `/data/adb/glovebox/update/` and checked against the signed SHA-256 and
   size. Only `bin/glovebox`, `module.prop` and the module scripts are taken from it, and the new
   binary has to run and report the expected version.
3. The current files are copied to `update/prev/`, the new ones take their place and the console
   restarts. That takes about 5 seconds; the proxy and Wi-Fi keep running.
4. The new version commits itself after a minute of running. If it has not done so within
   3 minutes (it crashes, hangs or never starts), the previous binary, which stays behind as a
   watchdog, restores the old files. If the device loses power in between, `service.sh` restores
   them after three quick crashes at the next boot. Automatic upgrades skip a version that was
   rolled back; installing it by hand retries it.

Each outcome is a health event, so Telegram and the webhook report "Updated A → B" and "Update
failed" with the reason, and in `notify` mode "Update X is available". The software update panel on
the Device page shows the state and can check and install by hand.

The client-limit overlay (`system/`) is not updated this way because it only applies after a
reboot; install the module zip or run `deploy.sh` for releases that change it.

### Publishing your own builds

Official releases are signed in CI with the key built into Glovebox. To push your own builds, create
a key once, trust it on the device, and publish the signed manifest next to the zip on any HTTP
server the device can reach (through `report_proxy` if one is set):

```sh
go run . update keygen -out update-signing.key    # prints the public key; keep the file private
# on the device: trust the key and point it at your server
echo '{"update_pubkeys":["<public key>"],"update_url":"http://192.0.2.10:8088/update.json"}' \
  | su -c '/data/adb/modules/glovebox/bin/glovebox config import'
./build.sh
go run . update sign -key update-signing.key -zip dist/glovebox-<version>.zip \
  -notes "Fix log shipping" -out update.json
# upload the zip first, then update.json
```

`zipUrl` in the manifest may be relative to `update.json`. A fork can sign its GitHub releases the
same way by storing the key file's contents in the `UPDATE_SIGNING_KEY` secret.

## Remote diagnostics

When something fails on the road, such as a site that does not resolve, you can ask the device what
it sees without reaching it: sign a job at home and put it next to `update.json`; the device runs it
and ships the results with its logs. As with the remote upgrade, the device only pulls.

1. Every `diag_interval`, Glovebox fetches `diag_url` through `report_proxy`. The default `auto` is
   `diag/<device_name>.json` next to a self-hosted `update_url`; it stays off while `update_url`
   points at GitHub. A missing file means there is no job.
2. A job runs once, and only if it is signed by a trusted release key, names this device and has
   not expired (`not_after` at most 24 hours ahead). Its probes run in order: at most 40, and
   3 minutes per job.
3. Each result is a JSON line in `/data/adb/glovebox/diag.log` with a summary in `_msg`, followed by
   a line for the whole job. With `log_push_url` set they are shipped right away with
   `source="diag"`; the Device page shows the last job.
4. For 10 minutes after a job the device polls every 15 seconds, so a follow-up runs within seconds.

| Probe | What it does |
| --- | --- |
| `dns` | Resolves `name` (`type` `A`, `AAAA` or `HTTPS`) at the DNS listener hotspot clients use (`dns_probe`) and through mihomo's resolver, which asks the upstream servers |
| `http` | Requests `url` through mihomo's mixed port, as a hotspot client would: status, redirect target, TLS version and certificate, timings, and the rule and proxy chain that carried it, or the error. `follow` follows up to 5 redirects, `ua` sets the User-Agent, `body` adds the first 512 bytes |
| `delay` | Latency test of `proxy` (a node or group) against `url` (default `delay_url`) |
| `proxies` | The node each group has selected |
| `connections` | mihomo's connections from hotspot clients, those containing `match` if set, up to 200 |
| `health` | The state of every health check |
| `log` | Raises mihomo's log level to `level` (`info` or `debug`) for `minutes` (default 10, at most 30), so its connection and DNS log is shipped; the level goes back on its own, also after a restart |

```sh
cat > probes.json <<'EOF'
[{"kind":"dns","name":"accounts.google.com","type":"HTTPS"},
 {"kind":"http","url":"https://accounts.google.com/","follow":true},
 {"kind":"connections","match":"google"}]
EOF
go run . diag sign -key update-signing.key -device car -ttl 30m -in probes.json -out car.json
# upload it as diag/car.json next to update.json; the results, in VictoriaLogs:
curl http://192.0.2.10:9428/select/logsql/query --data-urlencode 'query=_time:1h source:diag job_id:"<id>"'
```

`-id` names the job (default: the current time); a device runs each id once. Jobs cannot run
commands, and `http` refuses the device's own addresses (loopback, link-local, its interface
addresses, the vendor interface and the console hostnames, also when a name resolves to one) and
`.lan` / `.local` names, which keeps the console, mihomo's API and the vendor interface out of reach. The only change a job can make is the temporary log
level. Keep in mind that `connections` and `log` send the names of the sites your hotspot clients
visit to your log server.

## How it works

```
 browser ──HTTP/HTTPS──▶ glovebox (root, 192.168.0.254)
                           ├─ vendor web API   192.168.0.1:8080  (signal, SIM, usage, clients, Wi-Fi)
                           ├─ mihomo REST API   127.0.0.1:9091   (groups, nodes, providers, connections)
                           ├─ /proc, /sys                       (rates, CPU, memory, battery, thermal)
                           ├─ shell                              (box.service, logcat, reboot, date)
                           └─ report_proxy ──▶ Telegram, webhook, Loki push (alerts, logs)
```

The vendor login hash is read from the vendor database instead of storing the admin password; a
hash that failed once is never retried, to avoid tripping the vendor lockout. Writes carry the
vendor's request signature (`AD`). The vendor firewall only admits 192.168.0.0/24 to port 8080,
which is why Glovebox talks to 192.168.0.1 rather than loopback.

## Security

- The console only accepts connections from `allow_cidrs` (the hotspot LAN by default).
- Passwords are stored as PBKDF2-SHA256; sessions are HMAC-signed cookies that become invalid
  when the password changes. Failed logins lock out with exponential back-off.
- State-changing requests require an `X-Glovebox: 1` header (CSRF protection); responses carry a
  strict Content-Security-Policy.
- Remote upgrades only install releases signed by a trusted ed25519 key whose zip matches the
  signed SHA-256 and size, and roll back a version that does not start.
- Diagnostics jobs need a signature by the same keys, name one device, expire within 24 hours and
  run once; their probes only read, apart from a temporary mihomo log level.
- Glovebox runs as root and can reboot the device and change Wi-Fi settings — treat the console
  password accordingly. See [SECURITY.md](SECURITY.md) to report issues.

## Build

Requires Go 1.24+. The overlay additionally needs Android SDK build-tools and a platform
`android.jar` (`overlay/build.sh` finds them via `ANDROID_HOME`).

```sh
./build.sh                    # dist/stage/ and dist/glovebox-<version>.zip
NO_OVERLAY=1 ./build.sh       # without the client-limit overlay
VERSION=1.0.0 ./build.sh      # explicit version
GLOVEBOX_PASSWORD_FILE=pw.txt ./deploy.sh   # push to a device over adb and restart, no reboot
```

### Demo mode

```sh
go run . demo                 # http://127.0.0.1:8080, password "glovebox"
```

Demo mode serves the real UI against in-process fakes of the vendor API and mihomo with synthetic
traffic, which is handy for UI work, screenshots and tests.

### Tests

```sh
go test ./...
```

Besides unit and API tests, the suite checks that every locale has every key and no unused ones,
that source files stay English-only, and that README translations match this file's structure.

## Translations

UI strings live in [`web/i18n`](web/i18n). To add a language, copy `en.json` to `<code>.json`,
translate the values, and add the code with its native name to `index.json`. API errors are
translated through `err.<code>` keys; unknown codes fall back to the English message. The UI
picks the browser language and can be switched on the Device page.

This README is also available as `README.<code>.md`; translations keep the same sections,
tables, code blocks and screenshots (`docs/screenshots/<code>/`), which the tests check.

## Acknowledgements

Design inspired by OpenWrt LuCI, [zashboard](https://github.com/Zephyruso/zashboard),
[metacubexd](https://github.com/MetaCubeX/metacubexd), [WebTUI](https://github.com/webtui/webtui)
and futuristic UI work collected by [daisyUI](https://trends.daisyui.com/futuristic-speculative-ui/).
Bundled font: [JetBrains Mono](https://www.jetbrains.com/lp/mono/) (SIL Open Font License 1.1,
see `web/fonts/OFL.txt`).

## License

[MIT](LICENSE). Not affiliated with ZTE, Unisoc, the mihomo project or Tailscale.
