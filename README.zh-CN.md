# Glovebox

**给已 Root 的 Android 移动路由器用的网页管理后台，定位类似 OpenWrt 的 LuCI。** Glovebox 以 root
身份跑在随身 5G/4G Wi-Fi 的原厂系统上，把蜂窝网络状态、热点管理、[mihomo](https://github.com/MetaCubeX/mihomo)
代理和各类日志集中到一个页面里，不需要刷 OpenWrt。

[English](README.md) · 简体中文

![概览](docs/screenshots/zh-CN/overview.png)

- **单个静态二进制**，网页内嵌，没有运行时依赖，打包成 Magisk 模块。
- **基于原厂系统**：读写原厂网页接口（ZTE goform）、mihomo REST API 和 `/proc`、`/sys`，
  原厂网页和应用照常可用。
- **可安装为应用（PWA）**，桌面、平板、手机（横竖屏）和车机等嵌入式浏览器都有对应布局；
  HTTPS 由自动生成、带名称约束的本机 CA 提供。
- **多语言**（English、简体中文），加一种语言只需要加一个 JSON 文件。
- **演示模式**：用假后端在任何电脑上跑起完整控制台。

## 功能

| 页面 | 内容 |
| --- | --- |
| 概览 | WAN 实时吞吐曲线、信号（RSRP/SINR/频段）及历史、出口节点和代理链、Tailscale 连通性探测、本月流量、电量/CPU/内存/温度、已连接设备 |
| 代理 | mihomo 分组和节点、切换节点、测速、规则/全局/直连模式、更新订阅和规则集、实时连接（可筛选、一键断开） |
| 设备 | 每个频段的热点名称/密码/隐藏/开关、最大连接数（不超过系统上限）、已连接设备、HTTPS 与安装指引、重启代理、重启设备、校正设备时间、控制台密码、主题和语言 |
| 日志 | mihomo、Box for Root、Glovebox、Android logcat；级别筛选、grep 高亮、跟随/暂停、自动换行 |

键盘：`1`–`4` 切换页面，`/` 聚焦筛选框。

<p>
<img src="docs/screenshots/zh-CN/proxy.png" width="49%" alt="代理">
<img src="docs/screenshots/zh-CN/logs.png" width="49%" alt="日志">
</p>
<p>
<img src="docs/screenshots/zh-CN/device.png" width="66%" alt="设备">
<img src="docs/screenshots/zh-CN/phone.png" width="22%" alt="手机布局">
</p>

## 支持的设备

目前适配 **ZTE F50**（展锐平台，Android 13，B09 固件）及其原厂软件栈。和硬件打交道的部分都封装在
几个小客户端里（`zte.go`、`mihomo.go`、`sys.go`），适配其它基于 Android 的路由器只需要补对应实现，欢迎贡献。

设备需要：

- 用 [Magisk](https://github.com/topjohnwu/Magisk) Root
- 可选：[Box for Root](https://github.com/taamarin/box_for_magisk) 运行 mihomo，external-controller
  监听 `127.0.0.1:9091`（没有也能用，代理页和相关图表会降级显示）

## 安装

1. 从 Releases 页面下载 `glovebox-<版本>.zip`（或[自己编译](#编译)），在 Magisk 里安装后重启。
2. 设置控制台密码（只需一次）：

   ```sh
   adb shell su -c "'echo 你的密码 | /data/adb/modules/glovebox/bin/glovebox passwd'"
   ```

3. 连上热点，打开 **http://192.168.0.254**。Glovebox 会把这个地址加到 `br0` 上（原厂 DHCP
   地址池只到 .253），不会和 192.168.0.1 上的原厂网页冲突。

想用域名访问，就把 `glovebox.lan` 解析到 `192.168.0.254`，比如在 mihomo 配置里：

```yaml
hosts:
  glovebox.lan: 192.168.0.254
```

### HTTPS 与安装为应用

浏览器只允许从安全来源安装 PWA。Glovebox 第一次启动时在 `/data/adb/glovebox/tls/` 生成本机根证书，
并在 `192.168.0.254:443` 提供 HTTPS。根证书带**名称约束**，只能给配置的域名和监听 IP 签发证书，
即使私钥泄露也不能用来冒充其它网站。「设备」页有下载按钮和 iOS、Android、macOS、Windows 的分步说明，
信任之前请核对页面上显示的 SHA-256 指纹。

想用自己的证书？设置 `tls_cert` / `tls_key` 即可。

### 热点连接数上限

厂商的资源覆盖包把热点上限写死为 10 台（`config_wifiHardwareSoftapMaxClientCount`）。模块里带了一个
静态运行时资源覆盖包（`com.glovebox.wifi.overlay`，优先级 900），把上限改为 32；「设备」页的最大连接数
选项以系统实际报告的上限（`dumpsys wifi`）为准。真正能连多少台还取决于 Wi-Fi 固件。编译时加
`NO_OVERLAY=1` 可以保留厂商上限。如果热点异常，删除 `/data/adb/modules/glovebox/system` 后重启即可恢复。

## 配置

`/data/adb/glovebox/config.json` 只保存和默认值不同的字段。用 `glovebox config` 查看当前生效的配置，
用 `glovebox config import` 合并修改，然后重启程序（`su -c 'pkill -f bin/glovebox'`，守护脚本会自动拉起）：

```sh
echo '{"tailscale_proxy":"Tailscale","tailscale_probe":"http://100.64.0.1/"}' \
  | su -c '/data/adb/modules/glovebox/bin/glovebox config import'
```

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `["192.168.0.254:80"]` | HTTP 监听地址 |
| `tls_listen` | `["192.168.0.254:443"]` | HTTPS 监听地址；为空则关闭 HTTPS |
| `tls_cert`, `tls_key` | | 用自己的证书和私钥（PEM），不用本机 CA |
| `hostnames` | `["glovebox.lan"]` | 签发证书时包含的域名；第一个用于 HTTPS 链接 |
| `alias_addr`, `alias_dev` | `192.168.0.254/32`, `br0` | 加到局域网网桥上的地址；为空则不加 |
| `allow_cidrs` | `["192.168.0.0/24","127.0.0.0/8"]` | 允许连接的来源网段 |
| `zte_base`, `zte_db` | `http://192.168.0.1:8080`, 原厂数据库路径 | 原厂网页接口，以及存放其登录哈希的数据库 |
| `mihomo_api`, `mihomo_secret` | `http://127.0.0.1:9091` | mihomo external controller |
| `main_group` | 第一个可见的 Selector | 概览页显示的代理分组 |
| `box_dir` | `/data/adb/box` | Box for Root 目录（日志、重启脚本） |
| `tailscale_proxy`, `tailscale_probe` | | mihomo 出站名和经它访问的 tailnet 地址；两者都设置后才显示 Tailnet 卡片 |
| `delay_url` | `https://www.gstatic.com/generate_204` | 测速使用的地址 |
| `wan_prefixes`, `lan_dev` | `sipa_eth`, `rmnet`, `ccmni` / `br0` | 统计流量速率用的网卡 |

`/data/adb/glovebox/` 下的文件：`config.json`、`glovebox.log`（超过 1 MiB 轮转）、`stdout.log`
（`service.sh` 守护进程的输出）和 `tls/`（本机 CA）。升级或卸载模块都不会删除它们。

## 工作原理

```
 browser ──HTTP/HTTPS──▶ glovebox (root, 192.168.0.254)
                           ├─ vendor web API   192.168.0.1:8080  (signal, SIM, usage, clients, Wi-Fi)
                           ├─ mihomo REST API   127.0.0.1:9091   (groups, nodes, providers, connections)
                           ├─ /proc, /sys                       (rates, CPU, memory, battery, thermal)
                           └─ shell                              (box.service, logcat, reboot, date)
```

原厂登录哈希直接从原厂数据库读取，不需要保存管理员密码；某个哈希登录失败一次后就不再重试，避免触发原厂的锁定。
写操作会带上原厂要求的请求签名（`AD`）。原厂防火墙只允许 192.168.0.0/24 访问 8080 端口，所以 Glovebox
访问的是 192.168.0.1 而不是回环地址。

## 安全

- 控制台只接受来自 `allow_cidrs` 的连接（默认是热点局域网）。
- 密码用 PBKDF2-SHA256 保存；会话是 HMAC 签名的 Cookie，修改密码后全部失效。登录失败过多会锁定，等待时间按指数增长。
- 修改状态的请求必须带 `X-Glovebox: 1` 请求头（防 CSRF）；响应带严格的 Content-Security-Policy。
- Glovebox 以 root 运行，可以重启设备、修改 Wi-Fi 设置，请妥善保管控制台密码。报告安全问题见
  [SECURITY.md](SECURITY.md)。

## 编译

需要 Go 1.24+。覆盖包还需要 Android SDK 的 build-tools 和平台 `android.jar`（`overlay/build.sh`
通过 `ANDROID_HOME` 查找）。

```sh
./build.sh                    # 生成 dist/stage/ 和 dist/glovebox-<版本>.zip
NO_OVERLAY=1 ./build.sh       # 不带连接数覆盖包
VERSION=1.0.0 ./build.sh      # 指定版本号
GLOVEBOX_PASSWORD_FILE=pw.txt ./deploy.sh   # 通过 adb 推到设备并重启程序，不用重启设备
```

### 演示模式

```sh
go run . demo                 # http://127.0.0.1:8080，密码 glovebox
```

演示模式用进程内的原厂接口和 mihomo 假实现驱动真实界面，并生成模拟流量，方便开发界面、截图和测试。

### 测试

```sh
go test ./...
```

除了单元测试和接口测试，还会检查每种语言的翻译是否齐全、有没有多余的翻译，以及源代码是否只含英文。

## 翻译

界面文字都在 [`web/i18n`](web/i18n)。新增语言：复制 `en.json` 为 `<语言代码>.json` 并翻译，再在
`index.json` 里加上语言代码和它的本地名称。接口错误通过 `err.<code>` 翻译，没有对应翻译时显示英文原文。
界面默认跟随浏览器语言，也可以在「设备」页切换。

本说明也有 `README.<语言代码>.md` 版本；各语言版本保持相同的章节、表格、代码块和截图
（`docs/screenshots/<语言代码>/`），测试会检查这一点。

## 致谢

设计参考了 OpenWrt LuCI、[zashboard](https://github.com/Zephyruso/zashboard)、
[metacubexd](https://github.com/MetaCubeX/metacubexd)、[WebTUI](https://github.com/webtui/webtui)
以及 [daisyUI](https://trends.daisyui.com/futuristic-speculative-ui/) 收集的未来感界面作品。
内置字体：[JetBrains Mono](https://www.jetbrains.com/lp/mono/)（SIL Open Font License 1.1，
见 `web/fonts/OFL.txt`）。

## 许可证

[MIT](LICENSE)。本项目与 ZTE、展锐、mihomo 项目和 Tailscale 均无关联。
