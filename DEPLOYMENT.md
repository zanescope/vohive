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
startup:
  worker_bootstrap_concurrency: 2
  state_sync_concurrency: 2
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
| `startup` | 见下方 | 启动阶段的并发限制；两个值可独立配置，允许范围均为 `1`–`8`。 |
| `host_network_failover` | 候选列表为空 | Linux 主机默认出口故障切换；候选设备通过 Web 设备配置勾选，全部未勾选即关闭。 |
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
| `startup.worker_bootstrap_concurrency` | `2` | 服务冷启动时并行构建 Worker、启动控制面的数量；不控制热插拔 rescan。 |
| `startup.state_sync_concurrency` | `2` | 并行读取设备/SIM 状态并应用启动策略的数量；提高该值也会提高并发数据拨号量。 |

`config_schema` 是更新器和二进制之间的兼容契约。当前版本会把旧的 schema `0` 迁移到 `1`；配置版本高于当前二进制或低于最低支持版本时会拒绝启动，防止旧版本误读新配置。不要通过手工改数字绕过兼容检查。

### 6.3 公网 IP 探测参数

前端展示的公网 IPv4/IPv6 由服务端经对应模组网卡探测，避免浏览器出口与模组数据承载不一致：

```yaml
public_ip_probe:
  ipv4_urls:
    - https://4.ipw.cn
    - https://ipv4.ddnspod.com
    - https://ip.3322.net
  ipv6_urls:
    - https://6.ipw.cn
    - https://ipv6.ddnspod.com
```

| 字段 | 内置默认值 | 约束 |
| --- | --- | --- |
| `public_ip_probe.ipv4_urls` | `https://api.ipify.org`、`https://4.ident.me` | 最多 4 个，只接受返回单一公网 IPv4 文本的绝对 HTTPS URL。 |
| `public_ip_probe.ipv6_urls` | `https://api6.ipify.org`、`https://6.ident.me` | 最多 4 个，只接受返回单一公网 IPv6 文本的绝对 HTTPS URL。 |

同一地址族的端点按顺序回退；非空列表会完整替换该地址族的内置默认源，字段缺失或空列表继续使用默认源。URL 不允许用户信息和片段，禁止重定向；域名解析结果和响应内容都不能是私网、回环、链路本地或其他特殊用途地址。该节故意不支持 `VOHIVE_` 环境变量覆盖，修改 YAML 后需要重启服务。

HTTP 请求与 DNS 查询都绑定设备网卡。单个连接周期连续探测失败时先指数退避，最多进行 6 次快速尝试；之后进入降频状态，改为标称每 2 小时检查一次，并按设备加入稳定的 ±10% 抖动，避免 SIM 欠费、停机或网络不可用时持续请求和刷屏。未发生地址变化的健康轮询和 QMI/MBIM 事件不会绕过已排定的重试；数据承载真实重连、私网地址或地址族变化、Worker 替换以及 eSIM 轮换会开始新的快速探测周期。正常承载仍每 10 分钟巡检；降频状态只保存在内存中，服务重启后会重新快速确认。

上例列出中国大陆网络常用的第三方双栈 HTTPS 回显端点，但不承诺其可用性、限流策略或长期兼容性；生产环境建议优先使用受信任的自建端点。`ip.3322.net` 官方文档要求同一出口的两次请求间隔不少于 1 分钟，因此只建议将其放在列表末位作为回退源。HTTPS 探测依赖系统 CA 根证书；官方容器镜像已内置证书，OpenWrt 或直接运行发布二进制时需安装 `ca-bundle` 或系统等价的 `ca-certificates`，不能关闭 TLS 校验规避错误。

#### 6.3.1 主机网络故障切换

该功能只适用于直接管理 Linux 主机路由的原生部署，无需预先配置主网接口。候选设备在 Web 的“设备管理 → 配置 → 作为主机备用网络”中勾选；VoHive 会从非候选设备的 IPv4 主路由中自动识别主机当前出口。第一台被勾选的设备优先级最高，之后勾选的设备依次追加；取消后再次勾选会排到末尾。全部设备均未勾选时，监控停止探测并清理 VoHive 创建的备用路由。

```yaml
host_network_failover:
  # primary_interface: eth0  # 可选；仅用于覆盖自动发现结果
  probe_interval_seconds: 5
  probe_timeout_seconds: 8
  failure_threshold: 3
  recovery_threshold: 5
  minimum_backup_seconds: 30
  maximum_route_metric: 5
```

保存设备配置后无需重启服务，候选顺序与启停状态会在下一轮监控（默认最多 5 秒）内生效。候选顺序仍持久化为 `candidate_device_ids`，但应由 Web 维护，避免手工编辑与勾选顺序不一致。

| 字段 | 默认值 | 约束与说明 |
| --- | --- | --- |
| `primary_interface` | 空 | 可选覆盖值，例如 `eth0`、`enp1s0`；留空时自动选择 metric 最低的非候选 IPv4 主表默认出口，并把故障与恢复探测绑定到该接口。 |
| `candidate_device_ids` | `[]` | Web 自动维护的稳定设备 ID 列表；空列表即关闭功能，列表顺序就是选择优先级。 |
| `probe_interval_seconds` | `5` | `2`–`300`；每轮主网/备用网健康检查间隔，也是 Web 候选变更的最长生效等待时间。 |
| `probe_timeout_seconds` | `8` | `1`–`60`；单次绑定网卡公网探测超时。 |
| `failure_threshold` | `3` | `1`–`20`；主网连续失败次数，默认约 15 秒后开始切换。 |
| `recovery_threshold` | `5` | `1`–`20`；主网连续恢复次数，防止短暂成功导致来回切换。 |
| `minimum_backup_seconds` | `30` | `0`–`3600`；备用出口启用后的最短保持时间。 |
| `maximum_route_metric` | `5` | `1`–`4096`；备用路由允许使用的最大 metric；程序会在必要时自动取比主路由更小的安全值。 |

安全边界：

- 当前只接管主机的 IPv4 默认出口，不改变 IPv6，也不影响局域网直连路由。
- “设备已连接”不等于“设备能上网”。候选必须同时存在运行中的 Worker、已建立数据承载，并通过绑定其运行时网卡的公网 IPv4 探测。
- 自动发现会排除所有已勾选候选设备的网卡；主网故障与恢复 HTTPS 探测始终绑定到识别出的非候选接口，切换后不会把蜂窝备用出口误判为主网恢复。
- 任一时刻只提升一台设备。程序复制该设备已存在的 QMI/MBIM 默认路由，以专用协议标记创建临时路由；主路由 metric 大于 `0` 时创建更低 metric 的默认路由，metric 为 `0` 时创建覆盖公网地址空间的两条 `/1` 路由，局域网更具体的直连路由仍然优先。
- 主网恢复、服务正常停止或服务重启时只删除 VoHive 专用协议标记的默认路由或 `/1` 路由，绝不执行全局 route flush。
- 当前不会重写 `/etc/resolv.conf` 或接管 systemd-resolved。主机 DNS 服务器必须能同时经主网和蜂窝出口访问（例如部署者认可的公共 DNS）；否则路由切换后 IP 访问可用，但域名解析仍可能失败。
- 该开关会改变整台主机的新建出站连接，可能消耗 SIM 流量。不要把全部模组自动加入候选；只列出允许承担主机流量、套餐与稳定性合适的设备。

启用前可用以下命令确认自动发现所依据的默认出口并观察切换日志：

```sh
ip -4 route show default
sudo systemctl restart vohive.service
journalctl -u vohive.service -f | grep -E 'host network failover|primary host network'
```

实机验收至少覆盖：不设置 `primary_interface` 时能识别真实主出口、断开主网后只出现 VoHive 拥有的临时路由（普通 metric 为一条默认路由，主路由 metric 0 为两条 `/1`）、主机能经所选模组访问公网、其他模组不被提升、主网恢复后临时路由消失，以及 VoHive 在备用期间重启后不残留旧路由。

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
| `qmi_use_proxy` | `false` | 明确选择 QMI 控制口打开方式：`false` 为直连，`true` 为 libqmi `qmi-proxy`。运行时不会根据瞬时占用自动切换，两种方式都不能替代下方的 ModemManager 所有权隔离。 |
| `qmi_proxy_path` | 空 | 自定义 qmi-proxy abstract socket 名称。 |
| `qmi_proxy_executable` | 空 | 自定义 qmi-proxy 可执行文件路径。 |
| `esim_transport` | `at`；可选 `at`、`qmi`、`mbim` | 兼容字段；设置 `device_backend` 时会优先从后端推导。 |
| `usbnet_mode` | 空 | 兼容字段；当前主要依赖自动发现，通常不要手工设置。 |
| `operator_selection_mode` | 空/自动；可选 `automatic`、`manual` | 驻网方式；手动模式必须同时提供 PLMN。 |
| `operator_selection_plmn` | 空 | 手动驻网的 5 或 6 位 MCC+MNC。 |
| `operator_selection_rat` | 空；可选 `gsm`、`wcdma`、`lte`、`nr5g` | 手动驻网希望使用的无线制式。 |
| `baud_rate`、`data_bits`、`stop_bits`、`parity` | 设备管理页面设置 | AT 串口参数；`parity` 使用 `N`、`O` 或 `E`。 |

`usb_path`、`at_port`、`manage_port`、`interface`、`qmi_device`、`control_device` 和 `audio_device` 是运行时发现结果，不会从配置文件加载，也不应手工持久化。APN、IP 版本、网络开关、飞行模式、VoWiFi 和短信策略当前按 ICCID 保存在数据库的卡策略中，不再从 `devices` 节点读取。

#### ModemManager 所有权隔离

同一个 QMI 控制口只能由一套设备管理栈负责。`qmi_use_proxy: true` 可以让多个 QMI 客户端共享 `qmi-proxy`，但不能让 VoHive 与 ModemManager 安全地共管同一台设备。如果 `qmi-proxy` 由 `ModemManager.service` 启动，ModemManager 崩溃、停止或重启时，systemd 可能同时清理该 proxy，导致共享它的全部 VoHive QMI 客户端收到 EOF。

VoHive 会在每次启动 QMI 控制面、紧邻实际打开控制口前重新检查占用进程的 cgroup 与父进程链。占用扫描失败或结果不完整时会拒绝打开；直连模式只允许无人占用的控制口，proxy 模式只允许无人占用或仅由 `qmi-proxy` 占用的控制口。运行时不会因为一次瞬时扫描结果在直连与 proxy 之间自动切换。

确认占用者属于 ModemManager 时，VoHive 会记录 `qmi_modemmanager_conflict=true`、`action=isolate_modemmanager_from_vohive_devices` 以及启动预检错误，并拒绝本次 QMI 启动。它不会自动停止系统服务或修改主机规则；完成下述隔离后，后续启动重试会重新扫描。

**专用 VoHive 主机**

如果这台主机上的蜂窝设备全部由 VoHive 管理，在维护窗口中停用并 mask ModemManager：

```bash
sudo systemctl stop vohive.service
sudo systemctl disable --now ModemManager.service
sudo systemctl mask ModemManager.service
sudo systemctl start vohive.service
```

`mask` 用于阻止 ModemManager 被 D-Bus 或其他服务重新激活。需要恢复 ModemManager 时执行：

```bash
sudo systemctl stop vohive.service
sudo systemctl unmask ModemManager.service
sudo systemctl enable --now ModemManager.service
```

**混合用途主机**

如果主机还需要 ModemManager 管理其他蜂窝设备，不要全局停用它。应只让 ModemManager 忽略交给 VoHive 的设备：

原生 systemd 安装可以在“系统设置 → ModemManager 混合使用”中预览并安装隔离配置。网页会检查所有由 VoHive 配置为 QMI 后端的设备；只有这些设备当前全部在线，并且都能解析出唯一 USB 序列号或稳定物理端口后，才会启用安装按钮。安装和卸载都需要再次输入当前管理员密码。

从 v1.6.5 等旧版本原地升级后，如果页面提示缺少宿主机配置 helper，请重新下载当前版本的签名安装器并执行 `sudo sh vohive-install.sh --repair`，补装并刷新受控 systemd 单元，然后返回页面重新检查。

网页管理的规则固定写入 `/etc/udev/rules.d/78-mm-vohive-managed.rules`。VoHive 不会接管、覆盖或删除下面手工方式使用的 `78-mm-vohive.rules`，也不会接受浏览器传入的规则文本或文件路径。容器、OpenWrt、portable 部署，以及无法解析稳定 USB 身份的设备，仍需在宿主机按下面步骤手工处理。

网页操作只会 reload udev 规则。为了避免影响主机上由 ModemManager 管理的其他设备，VoHive 不会自动执行全局 `udevadm trigger`，也不会自动重启 ModemManager；请在维护窗口逐台重新插拔目标设备。

如果页面进入“需要人工确认”状态，表示规则文件可能已经变更，但系统没有确认 udev 规则已成功 reload。此时网页会锁定后续安装和卸载，避免在未知状态上继续覆盖。最稳妥的恢复方式是重启宿主机，重启后页面会自动重新检查并解除这项临时锁。

无法立即重启时，必须由 root 先 reload 并核对规则，再清除同一开机周期的恢复证据：

```bash
sudo udevadm control --reload-rules
# 核对 /etc/udev/rules.d/78-mm-vohive-managed.rules 与目标设备后再继续
sudo rm -f /var/lib/vohive/host-config/request.json /var/lib/vohive/host-config/result.json /var/lib/vohive/host-config/manual-attention.json
sudo systemctl restart vohive.service
```

不要在未 reload、未核对规则时单独删除这些恢复证据。

手工配置步骤如下：

1. 查询目标设备的唯一 USB 序列号和物理路径：

   ```bash
   sudo udevadm info --attribute-walk --name=/dev/cdc-wdm0
   ```

2. 创建 `/etc/udev/rules.d/78-mm-vohive.rules`。优先按唯一 `serial` 匹配；没有唯一序列号时，改用稳定的 USB 物理端口 `KERNELS`：

   ```udev
   ACTION!="add|change|move|bind", GOTO="mm_vohive_end"

   # 首选：唯一 USB 序列号
   SUBSYSTEMS=="usb", ATTRS{idVendor}=="2c7c", ATTRS{serial}=="REPLACE_WITH_UNIQUE_SERIAL", ENV{ID_MM_DEVICE_IGNORE}="1"

   # 备选：固定 USB 物理端口；启用时删除上面的 serial 规则
   # SUBSYSTEMS=="usb", ATTRS{idVendor}=="2c7c", KERNELS=="1-2.3", ENV{ID_MM_DEVICE_IGNORE}="1"

   LABEL="mm_vohive_end"
   ```

   不要只按 `ATTRS{idVendor}=="2c7c"` 屏蔽，否则 ModemManager 会忽略主机上的全部 Quectel 设备。每台交给 VoHive 的设备都应有一条精确规则；规则需要对该设备关联的 `cdc-wdm` 与 `tty` 端口都生效。

3. 在维护窗口应用规则。为了避免触发所有 USB 设备，优先逐台重新插拔目标设备，不要直接对全系统执行无条件 `udevadm trigger`：

   ```bash
   sudo systemctl stop vohive.service
   sudo udevadm control --reload-rules
   # 逐台重新插拔交给 VoHive 的设备
   sudo systemctl start vohive.service
   ```

4. 验证目标控制口已有 ignore 标记，并确认 `mmcli -L` 不再列出这些设备：

   ```bash
   sudo udevadm info -q property -n /dev/cdc-wdm0
   mmcli -L
   ```

   `udevadm info` 输出中应包含 `ID_MM_DEVICE_IGNORE=1`。关联的 AT/诊断端口也应检查同一标记。

容器部署同样必须在宿主机完成隔离；容器内设置 `qmi_use_proxy` 无法改变宿主机的 ModemManager 所有权。规则文件命名、动作范围与设备级 ignore 标签遵循 [ModemManager 官方端口与设备检测文档](https://modemmanager.org/docs/modemmanager/port-and-device-detection/)。

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
