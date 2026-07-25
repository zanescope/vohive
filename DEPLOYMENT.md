# VoHive 部署、配置与更新指南

## 1. 目标与范围

本文面向 VoHive 部署者，说明原生 Linux、Docker Compose 和 OpenWrt 的安装边界，以及配置、更新、备份、回滚和发布安全规则。普通用户不需要安装 Go、Node.js、Git 或手动编辑源码；默认原生安装只需下载并运行安装器，日常更新可使用 `vohivectl` 或 Web 页面。

本文覆盖：

- 默认快速安装与可选的严格来源验证，脚本自动识别 `amd64`、`arm64` 或 `armv7`。
- 安装完成后给出本机访问地址；仅首次创建配置时显示一次随机初始账号密码。
- 日常更新只需要执行 `vohivectl update` 或在 Web 页面点击“更新”。
- 更新前自动备份，更新后自动健康检查，失败时自动回滚。
- 配置、数据库和日志与程序版本分离，升级和回滚不会误删用户数据。
- `config.yaml` 的字段、默认值、生效方式和敏感信息保护要求。
- 同时覆盖普通 Linux、Docker Compose 和 OpenWrt，但文档只向普通用户推荐一种首选路径。

本文默认 VoHive 运行在连接 4G/5G USB 模组的 Linux 主机上。由于程序需要访问 `/dev`、TUN 和主机网络，**普通 Linux 原生安装是首选方案**；Docker 和 OpenWrt 是按环境选择的替代方案。

## 2. 当前基础与主要缺口

仓库已经具备以下基础：

- GitHub Actions 可发布 `amd64`、`arm64`、`armv7` Linux 二进制。
- GHCR 可发布 `amd64`、`arm64` 多架构镜像。
- 已有 Dockerfile、Compose 示例、OpenWrt procd init 脚本。
- Web 设置页已经可以检查 Release，并对非容器环境发起二进制自更新。
- 程序数据位于 `data/`，配置由 `-c` 指定，适合做版本与数据分离。

当前实现状态与剩余边界：

| 环节 | 已落地状态 | 剩余边界 |
| --- | --- | --- |
| 首次安装 | 签名安装器自动识别架构、建目录、生成配置并安装 systemd/procd 服务 | 离线安装镜像和更多发行版验收 |
| 安全校验 | Minisign 签名清单与归档 SHA-256 均为强制校验，失败即停止 | 发布密钥轮换演练 |
| 服务托管 | 已提供 systemd 与 OpenWrt procd 定义及恢复服务 | 继续按真实设备矩阵收敛权限 |
| 健康检查 | 已提供最小信息的 `/healthz` 和 `/readyz`，容器与服务共用 | 增加更多端到端故障注入测试 |
| 更新回滚 | 原生安装已使用双槽、快照、锁、就绪观察和失败回切 | 更多断电与发行版兼容测试 |
| Docker 更新 | Compose 已固定 digest 并有 healthcheck；当前仍由宿主机手工更新 | 受限宿主机助手落地后再开放一键更新 |
| OpenWrt | 只有配置和 init 文件，没有完整包定义与软件源 | 可安装、可升级、保留配置的正式包 |
| 默认安全 | 首次启动生成随机管理密码，配置文件使用 `0600` | 首次登录强制修改的完整交互验收 |

## 3. 部署方式决策

| 场景 | 推荐方式 | 用户入口 | 更新入口 |
| --- | --- | --- | --- |
| Debian、Ubuntu、树莓派 OS 等 systemd Linux | **原生安装，默认推荐** | Release 的 `vohive-install.sh` | `vohivectl update` 或 Web |
| 已经使用 Docker 的 NAS/服务器 | Docker Compose | `docker-compose.yml` + `.env` | 当前按 `CONTAINER.md` 手工切换 digest |
| OpenWrt 路由器 | 原生安装器（当前）；`.ipk`/`.apk` 软件包（规划） | `vohive-install.sh` | `vohivectl update` |
| 开发者 | 源码构建 | Makefile/CI | Git 工作流 |

不建议把 Docker 作为默认教程。VoHive 需要主机网络和较高的设备权限，容器部署仍然要使用 `network_mode: host`、设备映射或 `privileged`，对新手并没有明显降低理解成本。

## 4. 统一的目录与版本模型

原生 Linux 使用以下布局：

```text
/opt/vohive/
├── current -> releases/v1.6.0
├── last-good -> releases/v1.5.5
├── releases/
│   ├── v1.5.5/vohive
│   └── v1.6.0/vohive
└── control/
    └── vohivectl

/usr/local/sbin/vohivectl -> /opt/vohive/control/vohivectl

/etc/vohive/
├── config.yaml
├── deployment.json
└── update.pub

/var/lib/vohive/
├── data/
├── logs/
├── backups/
└── update/
    ├── state.json
    ├── request.json
    └── update.lock
```

设计要点：

- 二进制按版本存放，`current` 使用原子软链接切换。
- 配置和业务数据不放在版本目录内。
- systemd 的 `WorkingDirectory` 固定为 `/var/lib/vohive`，兼容程序当前的 `data/` 和 `logs/` 相对路径。
- 当前实现维护 `current` 与 `last-good` 指针，并为事务创建备份；尚未实现按份数或时间自动清理，运维需监控 `releases/` 与 `backups/` 的磁盘占用。
- 配置目录权限为 `0700`，配置文件权限为 `0600`。
- 当前程序需要直接控制模组、网络和可能占用串口的进程，第一阶段服务仍以 root 运行；后续再根据真实设备矩阵收敛 capability，不能先假设一组过窄权限。

## 5. 原生安装

### 5.1 快速安装

README 面向普通用户保留两条快速安装命令：

```sh
curl -fsSLO https://github.com/zanescope/vohive/releases/latest/download/vohive-install.sh
sudo sh vohive-install.sh
```

该方式通过 HTTPS 从 VoHive 官方 GitHub Release 下载当前安装器，适合个人使用和受信任环境。安装脚本将以 root 权限运行；生产环境、共享主机或需要确认构建来源时，应使用下一节的严格验证安装。

### 5.2 严格验证安装

严格模式固定下载 `v1.6.0` 信任基线，并在交给 root 前验证 GitHub 构建来源：

```sh
VOHIVE_BOOTSTRAP_VERSION=v1.6.0
curl --proto '=https' --tlsv1.2 -fsSLO \
  "https://github.com/zanescope/vohive/releases/download/${VOHIVE_BOOTSTRAP_VERSION}/vohive-install.sh"
gh attestation verify vohive-install.sh \
  --repo zanescope/vohive \
  --signer-workflow zanescope/vohive/.github/workflows/binary-release.yml \
  --source-ref "refs/tags/${VOHIVE_BOOTSTRAP_VERSION}" \
  --deny-self-hosted-runners
sudo sh vohive-install.sh
```

运行前需要按 [GitHub CLI 官方说明](https://cli.github.com/)安装 `gh`；如果公共证明查询要求登录，先执行 `gh auth login`。验证同时匹配唯一仓库、固定发布工作流、`v1.6.0` tag ref，并拒绝 self-hosted runner。任何验证错误都应停止，不能继续执行安装器。

这里固定下载 `v1.6.0` 是为了固定 bootstrap 信任基线，而不是把目标版本固定在 `v1.6.0`。验证后的安装器内置 Minisign 公钥和 bootstrap verifier 摘要，默认仍从签名清单安装最新 stable；`--channel beta` 会选择 beta。

安装器支持非交互参数，方便高级用户和自动化：

```sh
sudo sh vohive-install.sh --version v1.6.0 --channel stable
```

严格模式不使用 `curl | sh`，并把下载、验证、执行分开，让网络中断可重试，也让验证错误停在任何系统修改之前。

默认安装只接受正在运行的 systemd 或 OpenWrt procd；检测不到受支持的服务管理器时会在下载和系统写入前失败。`--no-service` 是明确的高级模式，只适合已经准备好自行启动、监控和重启 VoHive 的用户；安装器不会自动降级到未托管模式并报告普通安装成功。

### 5.3 安装器职责

安装器应按以下顺序执行：

1. 检查 Linux、root 权限、`amd64`/`arm64`/`armv7` 架构、服务管理器及必需的下载、摘要和解压工具。
2. 校验现有部署路径、事务状态和锁；有未解决事务或路径越界时 fail closed。
3. 获取目标 Release 的 `release-manifest.json`、Minisign 签名、对应架构归档及固定 SHA 的 bootstrap verifier。
4. 必须先验证清单签名，再按签名清单验证归档 SHA-256、大小、平台和归档成员；签名缺失、密钥不可信或任何校验失败都立即停止。
5. 建立安装事务并备份现有部署；停止服务后把归档提升到新的版本目录，再原子切换 `current`。
6. 只在配置不存在时创建最小配置并生成高强度随机管理密码，不写入固定默认密码、不覆盖现有配置。
7. 安装 `vohive.service`、`vohive-update.service`、`vohive-recover.service` 和 `/opt/vohive/control/vohivectl`，启用主服务与开机恢复服务，再启动主服务。
8. 等待 `/readyz` 成功；任何后续步骤失败都按同一事务恢复旧二进制、配置、数据和服务定义。
9. 输出本机 Web 地址，并仅在首次创建配置时显示一次初始管理员密码。

安装脚本必须幂等：重复执行时不得重置密码或覆盖配置，而是识别为修复或升级。

安装器继承标准 `HTTPS_PROXY` 环境变量。当前尚未提供 `--from` 离线目录或 `--download-base` 镜像覆盖参数；在这些入口完成相同的签名校验与事务测试前，文档不把它们列为可用能力。

### 5.4 systemd 服务

正式 unit 的恢复与启动顺序如下；完整权限配置以 `packaging/systemd/` 为准：

```ini
# vohive-recover.service
[Unit]
Description=Recover an interrupted VoHive update
DefaultDependencies=no
After=local-fs.target
Before=vohive.service

[Service]
Type=oneshot
WorkingDirectory=/var/lib/vohive
ExecStart=/opt/vohive/control/vohivectl recover --boot

[Install]
WantedBy=multi-user.target

# vohive.service
[Unit]
Description=VoHive cellular modem manager
Wants=network-online.target
After=network-online.target vohive-recover.service

[Service]
Type=simple
WorkingDirectory=/var/lib/vohive
ExecStartPre=/opt/vohive/control/vohivectl guard-start
ExecStart=/opt/vohive/current/vohive -c /etc/vohive/config.yaml
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

`vohive-recover.service` 在开机时先恢复被中断的事务，主服务的 `guard-start` 再阻止半切换或需人工恢复的状态启动。两者都不能省略。不要使用 `Restart=always`；正常停止、卸载或维护时不应被立即拉起，异常退出仍由 `on-failure` 恢复。

## 6. `config.yaml` 配置参考

### 6.1 文件位置与修改方式

原生安装和 OpenWrt 默认读取 `/etc/vohive/config.yaml`；Docker 读取容器内 `/app/config/config.yaml`，对应宿主机 `${VOHIVE_CONFIG_DIR:-./config}/config.yaml`。安装器首次创建配置时使用 `0600` 权限并生成随机管理员密码，不会在重复安装或升级时覆盖现有文件。

配置文件包含管理员密码、通知密钥和代理凭据，不能上传到公开仓库或直接粘贴到工单。手工修改前建议运行 `sudo vohivectl backup`。设备、代理、通知和登录凭据优先通过 Web 页面修改；直接修改 YAML 后应重启服务：

```sh
sudo systemctl restart vohive.service
sudo vohivectl doctor
```

安装器生成的最小配置类似下面内容；密码仅作结构示意，实际值由安装器随机生成：

```yaml
config_schema: 1
server:
  port: 7575
  debug: false
web:
  username: admin
  password: "由安装器生成的随机密码"
free_device_limit: 5
vowifi:
  enabled: false
```

根节点字段如下：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `config_schema` | `1` | 配置结构版本，由程序迁移器维护，不要手工降级或提前修改。 |
| `server` | 见下文 | Web/API 监听设置。 |
| `web` | 首次安装生成 | 管理后台账号和密码；当前 schema 不允许留空。 |
| `free_device_limit` | `5` | 可配置设备数量上限；`0` 表示不限制，负数会拒绝启动。 |
| `devices` | `[]` | 设备身份和后端设置，建议通过设备管理页面维护。 |
| `proxy.instances` | `[]` | SOCKS5/HTTP 代理实例，建议通过代理管理页面维护。 |
| `vowifi` | `enabled: false` | VoWiFi 和可选 SIP 语音网关设置。 |
| `public_ip_probe` | 内置双栈源 | 公网 IP 探测源；只从 YAML 读取，修改后必须重启。 |
| `telegram`、`feishu`、`qq`、`webhook`、`bark`、`email`、`pushplus` | 均关闭 | 通知渠道配置，启用前必须填写对应凭据和目标。 |

### 6.2 服务与管理员参数

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `server.port` | `7575` | Web/API 监听端口；`7575` 和 `:7575` 都可使用。 |
| `server.debug` | `false` | Gin 调试模式；常规部署保持关闭。 |
| `web.username` | 首次安装为 `admin` | 管理后台用户名，不能为空。 |
| `web.password` | 随机生成 | 管理后台密码，不能为空；首次登录后应立即修改。 |
| `free_device_limit` | `5` | 新增设备时执行并发安全的数量检查；`0` 表示不限制。 |

`config_schema` 是更新器和二进制之间的兼容契约。当前版本会把旧的 schema `0` 迁移到 `1`；配置版本高于当前二进制或低于最低支持版本时会拒绝启动，防止旧版本误读新配置。不要通过手工改数字绕过兼容检查。

### 6.3 公网 IP 探测参数

前端展示的公网 IPv4/IPv6 由服务端经对应模组网卡探测，避免浏览器出口与模组数据承载不一致：

```yaml
public_ip_probe:
  ipv4_urls:
    - https://your-ipv4-endpoint.example
  ipv6_urls:
    - https://your-ipv6-endpoint.example
```

| 字段 | 内置默认值 | 约束 |
| --- | --- | --- |
| `public_ip_probe.ipv4_urls` | `https://api.ipify.org`、`https://4.ident.me` | 最多 4 个，只接受返回单一公网 IPv4 文本的绝对 HTTPS URL。 |
| `public_ip_probe.ipv6_urls` | `https://api6.ipify.org`、`https://6.ident.me` | 最多 4 个，只接受返回单一公网 IPv6 文本的绝对 HTTPS URL。 |

同一地址族的端点按顺序回退；非空列表会完整替换该地址族的内置默认源，字段缺失或空列表继续使用默认源。URL 不允许用户信息和片段，禁止重定向；域名解析结果和响应内容都不能是私网、回环、链路本地或其他特殊用途地址。该节故意不支持 `VOHIVE_` 环境变量覆盖，修改 YAML 后需要重启服务。

HTTP 请求与 DNS 查询都绑定设备网卡。单个承载连续探测失败时先指数退避，最多进行 6 次快速尝试；之后改为每 10 分钟低频检查，避免 SIM 欠费或网络不可用时持续请求和刷屏。数据承载重连、地址变化或手动刷新会立即重新探测。

中国大陆网络可能无法稳定访问内置国际探测源，可把受信任的境内双栈 HTTPS 回显端点放在各自列表首位。HTTPS 探测依赖系统 CA 根证书；官方容器镜像已内置证书，OpenWrt 或直接运行发布二进制时需安装 `ca-bundle` 或系统等价的 `ca-certificates`，不能关闭 TLS 校验规避错误。

### 6.4 设备参数

设备项建议由 Web 页面创建和更新。下面示例展示 YAML 结构，不代表所有设备都需要这些字段：

```yaml
devices:
  - id: modem-1
    name: 主卡
    modem_imei: "866069053194211"
    device_backend: qmi
    module_vendor: quectel
    qmi_use_proxy: true
    proxy_port: 1080
    esim_switch:
      event_gated_converge: false
      reinit_window_ms: 0
      radio_cycle: false
      nas_attach_timeout_ms: 0
```

| 字段 | 默认值/可选值 | 说明 |
| --- | --- | --- |
| `id` | 无 | 配置内唯一的设备 ID。 |
| `name` | 空 | Web 页面显示名称。 |
| `modem_imei` | 自动探测 | 稳定硬件身份，用于设备拔插后的重新绑定。 |
| `device_backend` | `at`；可选 `at`、`qmi`、`mbim` | 设备控制后端。 |
| `module_vendor` | `quectel`；可选 `quectel`、`simcom` | AT 指令方言。 |
| `proxy_port` | `0` | 兼容的设备代理端口；新代理实例优先配置在 `proxy.instances`。 |
| `mbim_transport` | `auto`；可选 `auto`、`proxy`、`direct` | MBIM 控制通道打开方式。 |
| `qmi_use_proxy` | `false` | 是否通过 libqmi `qmi-proxy` 打开 QMI 控制口。 |
| `qmi_proxy_path` | 空 | 自定义 qmi-proxy abstract socket 名称。 |
| `qmi_proxy_executable` | 空 | 自定义 qmi-proxy 可执行文件路径。 |
| `esim_transport` | `at`；可选 `at`、`qmi`、`mbim` | 兼容字段；设置 `device_backend` 时会优先从后端推导。 |
| `usbnet_mode` | 空 | 兼容字段；当前主要依赖自动发现，通常不要手工设置。 |
| `operator_selection_mode` | 空/自动；可选 `automatic`、`manual` | 驻网方式；手动模式必须同时提供 PLMN。 |
| `operator_selection_plmn` | 空 | 手动驻网的 5 或 6 位 MCC+MNC。 |
| `operator_selection_rat` | 空；可选 `gsm`、`wcdma`、`lte`、`nr5g` | 手动驻网希望使用的无线制式。 |
| `baud_rate`、`data_bits`、`stop_bits`、`parity` | 设备管理页面设置 | AT 串口参数；`parity` 使用 `N`、`O` 或 `E`。 |

`usb_path`、`at_port`、`manage_port`、`interface`、`qmi_device`、`control_device` 和 `audio_device` 是运行时发现结果，不会从配置文件加载，也不应手工持久化。APN、IP 版本、网络开关、飞行模式、VoWiFi 和短信策略当前按 ICCID 保存在数据库的卡策略中，不再从 `devices` 节点读取。

`devices[].esim_switch` 是高级兼容参数，零值保持原有行为：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `use_refresh_true` | `false` | eSIM 主切换路径是否请求 `refresh=true`。 |
| `event_gated_converge` | `false` | 是否等待 UIM indication 再执行切换后的收敛。 |
| `radio_cycle` | `false` | 切换期间是否执行 LowPower → Online 无线电循环。 |
| `reinit_window_ms` | `0` | UIM 重新初始化窗口；仅在 `event_gated_converge=true` 时有效，`0` 表示关闭。 |
| `nas_attach_timeout_ms` | `0` | 恢复 Online 后等待 NAS 附着的最长毫秒数；`0` 表示不阻塞等待。 |

### 6.5 代理参数

```yaml
proxy:
  instances:
    - id: proxy-1
      name: 主卡 SOCKS5
      device_id: modem-1
      enabled: true
      mode: socks5
      listen_addr: 0.0.0.0
      listen_port: 1080
      auth_enabled: true
      username: proxy-user
      password: "替换为强密码"
```

每个 `proxy.instances[]` 支持 `id`、`name`、`device_id`、`enabled`、`mode`（`socks5` 或 `http`）、`listen_addr`、`listen_port`、`auth_enabled`、`username` 和 `password`。监听到非回环地址时应启用认证并配置防火墙，不要直接把代理端口暴露到公网。

### 6.6 VoWiFi 与语音网关参数

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `vowifi.enabled` | `false` | VoWiFi 全局开关。 |
| `vowifi.device_id` | 空 | 指定设备；留空时选择首个可用设备。 |
| `vowifi.mode` | `vowifi` | 当前使用 `vowifi`；`volte` 目前会回退到 `vowifi`。 |
| `vowifi.voice_gateway.sip.listen` | 空 | SIP 监听地址，例如 `0.0.0.0:5060`；非空时启用语音网关。 |
| `vowifi.voice_gateway.sip.transport` | `udp` | SIP 传输：`udp`、`tcp` 或 `tls`。 |
| `vowifi.voice_gateway.sip.realm` | `vohive.local` | SIP 认证域。 |
| `vowifi.voice_gateway.sip.external_ip` | 空 | NAT 场景使用的外部地址。 |
| `vowifi.voice_gateway.users[]` | `[]` | SIP 用户；字段为 `username`、`password`、`display_name`、`device_id`。 |
| `vowifi.voice_gateway.media.rtp_port_min` | `10000` | RTP 起始端口，必须为偶数。 |
| `vowifi.voice_gateway.media.rtp_port_max` | `20000` | RTP 结束端口，必须大于起始端口加 1。 |
| `vowifi.voice_gateway.media.codecs` | 空 | 编解码器列表，例如 `PCMU/8000`、`PCMA/8000`。 |
| `vowifi.voice_gateway.linphone_push.*` | 空 | `linphone_user` 和 `linphone_password` 推送凭据。 |

### 6.7 通知参数

所有通知渠道默认关闭，敏感字段会明文保存在受限配置文件中：

| 节点 | 字段说明 |
| --- | --- |
| `telegram` | `enabled`、`bot_token`、`chat_id`、`admin_id`、`base_url`、`proxy`。 |
| `feishu` | `enabled`、`app_id`、`app_secret`、`chat_ids`；`chat_id` 仅用于兼容旧版单目标配置。 |
| `qq` | `enabled`、`app_id`、`app_secret`、`group_ids`、`direct_ids`；多个 ID 使用逗号分隔。 |
| `webhook` | `enabled`、`urls`、`secret`、`headers`、`timeout_ms`（默认 `5000`）、`retry_max`（默认 `3`）、`text_template`（默认 `{{device_label}} {{text}}`）。 |
| `bark` | `enabled`、`urls`、`group`（默认 `vohive`）、`icon`、`level`（默认 `active`）。 |
| `email` | `enabled`、`use_ssl`、`smtp_host`、`smtp_port`、`username`、`password`、`from_address`、`to_addresses`。 |
| `pushplus` | `enabled`、`token`、`topic`、`channel`。 |

### 6.8 环境变量覆盖

除 `config_schema` 和 `public_ip_probe` 外，标量配置可使用大写、下划线形式的环境变量覆盖。`VOHIVE_` 前缀优先，旧的 `PROXY_` 前缀仅作兼容：

```sh
VOHIVE_SERVER_PORT=9000
VOHIVE_WEB_USERNAME=operator
VOHIVE_WEB_PASSWORD='replace-with-a-secret'
VOHIVE_FREE_DEVICE_LIMIT=0
```

环境变量只改变当前进程的有效值，不会回写 YAML。数组和复杂对象建议继续写入配置文件或通过 Web 页面维护。

## 7. `vohivectl`：原生更新与诊断入口

`vohivectl` 负责签名更新、事务恢复、备份和部署诊断；日志与卸载使用各自的系统入口：

```text
vohivectl status                         输出部署、能力和事务状态
vohivectl check                          检查当前通道的签名更新候选
vohivectl check --channel beta           检查 beta 通道
vohivectl update                         更新到当前通道的最新版本
vohivectl update --version v1.6.0        更新到指定签名版本
vohivectl rollback                       回到 last-good 并恢复配套备份
vohivectl backup                         手动创建配置和数据备份
vohivectl doctor                         检查部署路径、权限、服务和就绪状态
vohivectl recover                        恢复被中断的事务
```

这些命令当前输出 JSON 并使用非零退出码表示失败，便于 Web 更新任务和自动化调用。`guard-start` 是服务 unit 使用的内部保护命令，不作为日常用户入口。

查看 systemd 日志使用 `journalctl -u vohive.service -f`。卸载使用同一正式 Release 中的 `vohive-uninstall.sh`：默认只删除服务和程序版本；只有显式传入 `--purge` 并确认时才会先备份再删除 `/etc/vohive` 与 `/var/lib/vohive`。

## 8. 安全更新与自动回滚

### 8.1 发布通道

- `stable`：正式 SemVer 标签，默认通道，只接收非 prerelease Release。
- `beta`：显式选择后才接收预发布版本。
- `dev`：只用于开发环境，不允许在 Web 中一键切换。
- Docker 的生产部署保存精确版本或镜像 digest；`latest` 只用于发现新版，不能作为唯一回滚依据。
- GitHub `latest`、GHCR `latest` 与 `beta` 都执行严格 SemVer 单调门禁：较旧版本仍可发布新的精确版本资产/标签，但不会把 moving alias 回退；同标签 Release 禁止重发。

更新器信任基线固定为 `v1.6.0`：

- `v1.5.x` 及更早版本不得继续使用旧的 Web 文件热替换；首次迁移必须重新运行 `v1.6.0` Release 内的签名安装器，由安装器内置的公钥和固定 SHA-256 bootstrap verifier 完成受控迁移。
- 从 `v1.6.0` 起，正式二进制同时内置事务更新器和发布公钥，才开放 Web 一键更新；清单中的 `min_updater_version` 固定为 `v1.6.0`，更旧更新器必须 fail closed。
- 密钥轮换期间同时保留旧、新公钥；只有所有受支持版本都已包含新公钥后，后续 Release 才能移除旧公钥。

### 8.2 原生更新流程

```text
获取发布清单
  → 下载到临时目录
  → 校验架构、SHA-256 和签名
  → 停止服务并创建一致性备份
  → 安装到新版本目录
  → 原子切换 current
  → 启动服务
  → 在限定时间内轮询 /readyz，并经过稳定观察期
      ├─ 成功：记录 last-good 和事务完成状态
      └─ 失败：停止新版本，切回旧版本，恢复更新前数据，重新启动并报告原因
```

关键约束：

- 下载和校验阶段不停止现有服务。
- 更新使用 `/var/lib/vohive/update/update.lock` 防止 Web 与命令行同时更新。
- 停止服务后再复制 SQLite 数据库及其 `-wal`、`-shm`，确保备份一致。
- 数据库变更必须改为有版本号、可测试的显式迁移。仅依赖 `AutoMigrate` 会让旧二进制回滚后的兼容性不可判断。
- `/readyz` 只有在配置、存储和 API 初始化完成后才可访问；响应包含运行版本，更新器还会发送随机挑战并验证 root-only 密钥生成的证明，确认端口、目标版本和受管服务身份一致。更新器要求连续就绪并经过稳定观察期；没有模组时仍可视为服务就绪，设备状态单独展示。
- 更新失败保留事务错误和备份；自动回滚会清理未被 `current` 或 `last-good` 引用的失败版本槽，确保同版本可安全重试。
- 默认不启用无人值守自动更新。蜂窝网络和语音业务可能被升级中断，用户可自行配置维护窗口。

### 8.3 Web 更新

Web 页面不再直接让进程替换自身。推荐流程是：

1. API 创建更新任务，返回任务 ID。
2. 后台通过受控 helper 调用与命令行相同的 `vohivectl update`。
3. 页面持续展示“下载、校验、备份、重启、验证、完成/已回滚”的状态。
4. 页面断线后自动重连；服务恢复时显示实际版本和最终结果。

容器环境的更新 API 必须在服务端返回“不支持容器内自替换”，不能只靠前端拦截。

## 9. 健康检查设计

当前提供两个无需登录、只返回最小信息的接口：

- `GET /healthz`：进程事件循环和 HTTP 服务可响应即返回 200，用于 systemd/Docker 存活检查。
- `GET /readyz`：配置已加载、数据库可用、核心服务完成初始化时返回 200；响应包含运行版本，更新器传入随机挑战时还返回基于 root-only 密钥的证明，用于排除错误端口上的其他进程。

响应不包含设备标识、配置路径或其他敏感信息。就绪证明不能脱离每次随机挑战重放。

Dockerfile 和 Compose 都已声明 healthcheck，当前使用镜像内的 `vohivectl` 从配置解析实际 `server.port` 后请求 `/healthz`。

## 10. Docker Compose 方案

当前仓库已经提供一份官方 `docker-compose.yml` 和 `.env.example`。用户可放在任意受控目录，例如 `/opt/vohive-compose`：

```text
docker-compose.yml
.env                 # VOHIVE_IMAGE=ghcr.io/zanescope/vohive@sha256:...
config/
data/
logs/
```

当前约束：

- 复制 `.env.example` 为 `.env` 后必须填入不可变镜像 digest；缺失时 Compose 直接报错。
- 使用 `network_mode: host`，不再声明无效的 `ports` 映射，也不硬编码代理。
- 主容器不挂载 Docker Socket、不替换自身二进制；没有受限宿主机更新代理时，Web 更新 fail closed。
- Dockerfile 与 Compose 都通过 `vohivectl` 按配置端口检查 `/healthz`；正式镜像只发布 `amd64`、`arm64`，armv7 走原生安装或 OpenWrt。
- 配置、数据和日志均由宿主机 bind mount 持久化，镜像升级不得覆盖配置。

当前版本按 `CONTAINER.md` 手工保存旧 digest、拉取并解析新 digest、重建、检查 healthy，失败时恢复旧 `.env`。仓库尚未提供 `install-docker.sh` 或可控制宿主 Docker 的 `vohivectl update`，前端不会把多条宿主机命令伪装成“一键更新”。

后续若落地受限宿主机助手，再由它包装备份、拉取、原子修改 `.env`、`docker compose up -d`、健康观察和自动回滚；该助手必须使用最小权限，不能把 Docker Socket 暴露给 Web 容器。

## 11. OpenWrt 方案

把现有 procd init 和示例配置补全为正式软件包：

- 为支持的 OpenWrt 版本分别构建 `.ipk` 或 `.apk`。
- 包含架构、版本、依赖、校验和、安装/升级脚本。
- 使用 `/etc/vohive/config.yaml` 作为 conffile，升级不得覆盖。
- 数据固定在 `/var/lib/vohive`，安装前检查 overlay 可用空间。
- procd 使用 `respawn`，升级脚本负责有序停止、备份、安装和启动。
- 建立签名软件源后，用户只执行包管理器的安装或升级命令。

OpenWrt 包不应假设所有路由器都有足够存储或相同的 USB/QMI/MBIM 内核模块。安装后由 `vohivectl doctor` 输出缺失的设备相关依赖，而不是静默启动失败。

## 12. Release 产物与供应链

每个正式 Release 建议一次性发布：

```text
vohive_v1.6.0_linux_amd64.tar.gz
vohive_v1.6.0_linux_arm64.tar.gz
vohive_v1.6.0_linux_armv7.tar.gz
vohive-verify_v1.6.0_linux_amd64
vohive-verify_v1.6.0_linux_arm64
vohive-verify_v1.6.0_linux_armv7
vohive-install.sh
vohive-uninstall.sh
release-manifest.json
release-manifest.json.minisig
SHA256SUMS
SHA256SUMS.minisig
```

每份归档根目录只包含 `vohive`、`vohivectl` 和 `LICENSE`；安装器按目标平台生成 systemd 或 procd 服务定义，并只在首次安装时创建配置。Release 发布前同时签名 manifest 和完整 `SHA256SUMS`；发布后禁止补传或覆盖资产，修复必须使用新标签。

首次正式发布的运维准备：

- 在仓库 `Settings` 的 Releases 区域启用 **Release immutability**；未启用时发布工作流会删除本次生成的可变 Release 并失败。该设置只影响启用后的新 Release。
- 创建名为 `release` 的 GitHub Environment，限制只允许受保护的正式版本标签使用；正式二进制和容器发布 job 都绑定该环境。
- 离线生成 Minisign 密钥；把分号分隔的公钥写入仓库变量 `VOHIVE_MINISIGN_PUBLIC_KEYS`，把私钥文件和口令作为 `release` Environment secrets 写入 `MINISIGN_PRIVATE_KEY`、`MINISIGN_PASSWORD`。
- 用 tag ruleset 保护 `v*` 正式版本标签，禁止强推、更新和删除且不配置人工绕过；发布工作流还会在上传前重新解析远端标签，并要求它仍精确指向签名清单中的源码 revision。只给发布 job 最小的 `contents`、`packages`、`id-token` 与 `attestations` 权限。
- 首次推送 GHCR 后，在 GitHub Package 设置中把 `vohive` 容器包设为 Public；生产部署始终记录 `ghcr.io/zanescope/vohive@sha256:...`。
- GitHub CLI 按“创建草稿、上传全部资产、发布”顺序生成 Release；签名私钥临时文件在签名步骤结束前删除。
- 发布工作流发现同标签 Release 已存在时必须失败，不允许重跑后补传资产；任何修复都提升 SemVer 并重新发布。
- 正式容器发布必须先验证同 tag 的不可变 Release、Minisign 清单和源码 revision；GHCR 的 `vX.Y.Z`/`X.Y.Z` 精确标签已存在时拒绝覆盖。

CI 需要增加：

- 在同一个受保护工作流中构建全部产物并生成校验和。
- 生成签名或 GitHub artifact attestation。
- 对全新 Linux VM 执行安装、重复安装、升级和卸载测试。
- 制造启动失败版本，验证自动回滚和数据恢复。
- 对 Compose 执行 config 校验、启动、healthcheck、升级和回滚测试。
- 在发布前验证归档内版本号、目标架构和 Release 标签完全一致。

## 13. 安全与运维默认值

- 安装时生成随机初始密码并只显示一次；当前尚未强制首次登录修改，部署后应立即修改。禁止发布固定通用密码。
- 默认只建议在可信局域网访问 7575，不把管理端口直接暴露到公网。
- 如需公网访问，另行提供反向代理、TLS、访问控制和可信代理配置教程。
- 更新下载只允许 HTTPS，并限制重定向目标。
- API 更新属于高风险操作，需要重新验证管理员密码或短时二次令牌，并记录审计日志；卸载脚本的 `--purge` 使用交互确认或显式 `--yes`。
- 备份中含配置密钥与通信数据，必须限制权限；导出时明确提示敏感性。
- 当前版本尚未自动清理日志、备份和旧版本；部署者必须监控磁盘空间，后续保留策略落地前不能宣称自动限额。

## 14. 实施状态与后续路线

### P0：已形成原生闭环

1. 已提供 `/healthz`、`/readyz` 和 `vohivectl doctor`。
2. 发布工作流生成三架构归档、完整 `SHA256SUMS`、Minisign 签名和 attestation。
3. 已实现签名 `vohive-install.sh`、systemd/procd unit 和基础 `vohivectl`。
4. 已实现版本目录切换、更新前备份、更新后稳定就绪观察和自动回滚。
5. README 已展示原生签名安装入口，并把 Docker 指向独立的 digest 部署文档。

### P1：继续收敛 Web 与 Docker 体验

1. Web 已接入任务式原生更新；继续补充真实主机上的断线重连和逐阶段交互验收。
2. 容器端已经禁止自替换；受限宿主机更新 helper 尚未实现。
3. Compose 已合并并固定 digest、加入 healthcheck；宿主机备份和回滚当前仍按 `CONTAINER.md` 手工执行。
4. `doctor` 与 `backup` 已落地；备份导入仍需单独设计和故障测试，卸载继续使用 `vohive-uninstall.sh`。

### P2：发行版与路由器生态

1. 完整 OpenWrt 包和签名 feed。
2. 根据用户量决定是否维护 Debian/Ubuntu APT 仓库。
3. 增加首次启动 Web 引导，完成密码、设备发现和网络设置。
4. 建立真实 `amd64`、`arm64`、`armv7` 主机与模组的发布验证矩阵。

## 15. 验收标准

- 全新支持系统上，用户不安装编译工具即可完成部署。
- 首次安装只需“下载脚本、sudo 执行”两步，结束时可打开安装器输出的本机 Web 地址。
- 重复运行安装器不会覆盖配置、密码和数据。
- 更新操作只有一个入口；断网、校验失败不影响当前运行版本。
- 新版本在 90 秒内未就绪时自动恢复旧二进制与更新前数据，并重新提供服务。
- 断电或更新进程被杀后，系统下次启动能根据事务状态恢复到一个完整版本。
- `vohivectl doctor` 用结构化 JSON 报告部署路径、签名信任、更新锁、目录和就绪状态。
- Docker 与原生安装共享通道和版本规则；Docker 的宿主机备份与回滚仍按 `CONTAINER.md` 手工执行。
- 普通升级保留配置和数据库；普通卸载默认保留数据。

## 16. 参考资料

- Docker Compose `pull`：<https://docs.docker.com/reference/cli/docker/compose/pull/>
- Docker Compose healthcheck：<https://docs.docker.com/reference/compose-file/services/#healthcheck>
- GitHub artifact attestations：<https://docs.github.com/en/actions/concepts/security/artifact-attestations>
- GitHub immutable releases：<https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases>
