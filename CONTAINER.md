# VoHive GHCR 容器部署

正式容器镜像只发布到 `ghcr.io/zanescope/vohive`。Docker Hub 不是发布源，也不在容器内执行自更新。

选择 GHCR 是因为源码、Actions 权限、镜像来源链接、SBOM 和 provenance 都在同一个 GitHub 信任域内，发布无需再维护 Docker Hub 专用账号和长期令牌，也避免两个 Registry 的标签或 digest 漂移。Docker Hub 并非技术上不可用；本项目只保留一个官方源，降低小白部署和维护成本。

仓库维护者在首次发布后必须在 GitHub Package 设置中关联 `zanescope/vohive` 的 Actions 访问权限，并把 `vohive` 容器包设为 **Public**；公开部署无需登录。若包仍处于私有状态，部署者需要先用仅含 `read:packages` 权限的令牌登录：

```sh
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u YOUR_GITHUB_USER --password-stdin
```

VoHive 需要访问蜂窝模组、`/dev/net/tun` 和主机网络。容器部署适合已经熟悉 Docker 的用户；普通 Linux 主机优先使用原生安装。

## 准备不可变镜像地址

正式部署使用镜像 digest。版本标签只用于发现目标版本：

```sh
docker pull ghcr.io/zanescope/vohive:v1.6.0
docker image inspect \
  --format='{{index .RepoDigests 0}}' \
  ghcr.io/zanescope/vohive:v1.6.0
```

复制环境文件，把输出的完整 `ghcr.io/zanescope/vohive@sha256:...` 写入 `VOHIVE_IMAGE`：

```sh
cp .env.example .env
```

`docker-compose.yml` 会在 `VOHIVE_IMAGE` 缺失时直接报错，避免意外使用可移动的 `latest`。

## 启动

```sh
mkdir -p config data logs
docker compose up -d
docker compose ps
```

Compose 使用 `network_mode: host`，因此不能再配置 `ports`。容器需要硬件访问权限，默认启用 `privileged` 并挂载 `/dev`。

首次启动会生成随机管理员密码，不使用 `admin/admin` 或 `admin/admin123`。只在可信终端查看一次初始凭据，并在首次登录后立即修改：

```sh
docker compose logs vohive
```

配置、数据和日志分别保存在 `./config`、`./data`、`./logs`。可在 `.env` 中通过 `VOHIVE_CONFIG_DIR`、`VOHIVE_DATA_DIR`、`VOHIVE_LOG_DIR` 改为绝对路径。

## 健康检查

镜像和 Compose 都使用镜像内的 `vohivectl probe --liveness`，从持久化配置解析实际 `server.port` 后请求无需登录的 `/healthz`：

```sh
docker inspect --format='{{json .State.Health}}' vohive
```

没有插入蜂窝模组不会令 HTTP 健康检查失败。业务就绪和更新后的稳定观察由更新管理器另行判断。

## 更新与回滚

主容器没有 Docker Socket，也不会替换自身二进制。未安装受限宿主机更新代理时，前端更新必须 fail closed，只显示目标版本和宿主机操作提示。

更新时先备份数据并保存旧 digest，再解析目标版本的新 digest：

```sh
cp .env .env.previous
docker pull ghcr.io/zanescope/vohive:v1.6.1
docker image inspect \
  --format='{{index .RepoDigests 0}}' \
  ghcr.io/zanescope/vohive:v1.6.1
# 将上一步输出写入 .env 的 VOHIVE_IMAGE
docker compose up -d
docker compose ps
```

健康检查失败时恢复旧环境文件并重建：

```sh
cp .env.previous .env
docker compose up -d
```

正式的 `vohivectl update` 会包装备份、拉取、切换、健康检查和自动回滚；在该宿主机能力落地前，不把多条 Docker 命令伪装成前端“一键更新”。

## 镜像通道

- stable：`v1.6.0`、`1.6.0`、`latest`；生产记录 digest。
- beta：`v1.6.1-beta.1`、`1.6.1-beta.1`、`beta`；不会覆盖 `latest`。
- dev：`edge-<git-sha>-<run-id>-<attempt>`；每次 Actions 构建使用唯一标签，只供测试，不能进入正式 Web 更新流程。

`latest` 和 `beta` 只在候选 SemVer 严格高于 GHCR 中该通道的最高精确版本标签时前进。补发较旧版本时只生成 `vX.Y.Z`/`X.Y.Z` 精确标签，不会把 moving alias 回退。
正式镜像只能由同 tag 的已发布、不可变 GitHub Release 触发；工作流会验证 Minisign 清单、源码 revision 和远端 tag 绑定。GHCR 中任一 `vX.Y.Z` 或 `X.Y.Z` 精确标签已存在时发布失败，不允许覆盖。


正式多架构镜像包含 `linux/amd64` 和 `linux/arm64`。armv7 使用原生 Release 或 OpenWrt 包。

基础镜像的 Alpine minor 集中固定在 Dockerfile 的 `ALPINE_VERSION`，每次升级都必须经过 CI、SBOM 和构建来源证明。发布工作流附带 OCI source、revision、version 标签及 provenance；运行部署仍以最终 GHCR digest 为准。
