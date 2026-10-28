# 镜像发布、部署与恢复

正式设备只需 Linux x86_64、Bash、Docker Engine 与 Compose v2，以及能拉取 GHCR、访问 Telegram 和真实 ABS 的网络。开发和 AI 均留在 Mac。正式 Bot 使用 SQLite，保持单实例；更新允许短暂停机。

## GitHub 首次配置与发布

将项目改动提交到 GitHub。仓库 Actions 开启后，`Check and build candidate` 会先执行普通测试、竞态检查、静态检查和部署脚本演练。PR 只检查；主分支检查通过后才有权限发布 GHCR 候选镜像，再在独立运行器上分别执行 amd64、arm64 容器维护冒烟测试，避免同一 Docker 镜像存储切换架构时发生 digest 冲突。两种架构均通过后才生成并上传部署包。

镜像位置为 `ghcr.io/huliyoudiangou/yueer_abs_bot`。候选首次创建后，在 GitHub Packages 中确认可见性为 **Private**，授予仓库 Actions 对该包的写入权限。工作流使用自带 `GITHUB_TOKEN`，无需配置正式设备的 SSH 或 Bot/ABS 密钥。

在 Mac 用独立测试 Bot 验收完整 SHA。然后在 GitHub Actions 手动运行 **Promote tested candidate**，选择 main，填写该 40 位 SHA、全新的 `v1.0.0` 等版本号，并确认已完成私聊验收、当前结构首次建库和重启验证。

发布工作流只接受主分支成功候选，从该次构建下载 digest 和部署包，复用镜像 manifest，不重新构建。已存在的版本拒绝覆盖。Release 附件包括：

- `deployment.tar.gz`：部署脚本、Compose、配置模板和操作文档，不含应用源码。
- `deployment.tar.gz.sha256`：部署包校验值。
- `release.json`：版本、源码提交、固定镜像 digest、数据库兼容标识与验证边界。

二进制内嵌的是候选 SHA；正式版本和 digest 记录在部署设备的 `.release.env` 与部署历史中。修改仓库名称或转移仓库时，同时调整脚本默认 `IMAGE_REPOSITORY`，或在运维环境显式设置该变量。

## 正式设备首次安装

从 Release 下载部署包和校验文件，放到同一目录，核对校验并解压到固定位置（例如 `/opt/ccoff-abs-bot`）。不要把新包覆盖到运行数据目录。

部署目录需要当前运维用户具有写权限；创建 `/opt` 下的目录时，按设备权限使用 sudo 并授予该用户目录所有权。

```sh
sha256sum -c deployment.tar.gz.sha256
mkdir -p /opt/ccoff-abs-bot
tar -xzf deployment.tar.gz -C /opt/ccoff-abs-bot
cd /opt/ccoff-abs-bot
cp .env.example .env
chmod 600 .env
```

填写正式 `.env`，保持 `APP_ENV=production`、`ABS_MODE=real`、`DATABASE_URL=data/bot_data.db`。部署脚本目前约定这个数据库路径；自定义路径需要同时调整脚本和挂载。`SECURITY_PEPPER` 与 `BACKUP_ENCRYPT_KEY` 使用两个不同的长期随机密钥，单独安全保存，后续更新保留原值。

Compose 从宿主 `.env` 注入配置，应用容器以 UID 10001 运行；维护容器仅在执行维护命令时以 root 离线访问挂载文件。不要把 Bot/ABS 密钥写进镜像、版本控制或镜像仓库凭据中。

私有镜像使用有 `read:packages` 权限的 GitHub 凭据登录一次：

```sh
docker login ghcr.io
```

首次上线时选定设备外副本目录。它可以是另一台设备提供的已挂载网络目录，备份工具本身不需要额外 Telegram 群：

```sh
./deploy.sh set-backup-destination /mnt/off-device-backup
./deploy.sh update v1.0.0
./deploy.sh status
```

指定目录必须已存在且可写。脚本要求它在部署目录以外；是否确实位于另一台设备由挂载配置保证。加密密钥及 `SECURITY_PEPPER` 也必须有独立安全副本，数据库备份不包含这些配置。

## 日常更新与状态

```sh
./deploy.sh update v1.0.1
./deploy.sh status
./deploy.sh logs
```

更新先拉取镜像并固定 digest，再检查配置、数据库兼容标识及备份空间，随后停止旧实例、创建并校验加密一致性备份、启动新版本。备份会复制到已配置的外部目录；副本失败时返回错误，保留本地备份。

最多等待 180 秒，要求进程存在、SQLite 可读并且最近 120 秒内成功完成过 Telegram 轮询。启动阶段文件不作为运行健康证明。网络持续不可用也会导致健康检查失败；Docker 的 `unless-stopped` 不会仅因为 unhealthy 自动重启容器。

拉取或兼容检查失败时，原实例继续运行。停止旧实例之后失败，脚本保持现场并退出；启动检查失败时还会保存日志并停止新实例。它不会自动恢复数据库，也不会在新旧实例之间反复切换。查看 `status`、`deploy-state/history.tsv`、失败日志和最近备份，再选择恢复操作。

操作通过目录锁互斥。进程被强制终止可能留下 `deploy-state/.lock`，确认没有维护任务运行后再手动移除。不要并行执行额外的 `docker compose up` 绕过此锁。

## 每日加密备份

```sh
./deploy.sh backup
```

日常备份使用在线 `VACUUM INTO`，完整包含已提交的 WAL 数据，不要求停止 Bot。快照进行 SQLite 完整性校验后压缩、加密并解密自校验，持久保存在 `deploy-state/backups/`，再复制到已设置的副本位置。升级及恢复前也会额外备份。此方式不依赖 Telegram 备份群；已有群备份能力仍可继续使用。

在 NAS 的任务计划或宿主机 crontab 配置每日执行。若宿主机时区为 Asia/Shanghai，可用以下示例（路径按实际修改）：

```cron
15 3 * * * cd /opt/ccoff-abs-bot && ./deploy.sh backup >> deploy-state/daily-backup.log 2>&1
```

这一步需要在设备首次安装时完成，更新镜像不会自动修改宿主机定时任务。核对任务日志与外部副本是否到达；定期在隔离目录演练解密恢复。脚本默认不自动删除历史备份，根据磁盘容量制定保留周期，日志也需由宿主机轮转。

## 程序回退与数据恢复

仅回退程序：

```sh
./deploy.sh rollback v1.0.0
```

目标必须是本设备曾成功运行过的版本，使用记录的原 digest，并与当前版本的数据库兼容标识一致。优先使用本地缓存镜像，无需联系镜像仓库；本地镜像已被清理时才重新拉取。回退前仍会备份；**它保留当前数据库，不抹掉升级后新增的积分、订单等数据**。兼容标识只能表示发布者确认的兼容组，必须由升级回归测试与发布检查共同保证。

明确需要恢复历史数据时，从 `deploy-state/backups/` 选择文件名：

```sh
./deploy.sh restore 20261028T031500Z-daily-1234.enc --confirm-data-loss
```

命令先停止 Bot，保存当前数据库的新备份，再解密并校验选定备份、检查兼容标识、替换数据库并检查启动。没有 `--confirm-data-loss` 不执行恢复；密钥错误、文件损坏或不兼容时拒绝替换。

恢复会丢失选定备份之后的本地数据；已经发生的真实 ABS 修改或 Telegram 通知无法通过 SQLite 恢复撤销，需要另行核对。恢复前备份失败时，脚本保留当前数据库且保持停机；若当前库已损坏，应先将整个 `data/` 留存到隔离目录，再走人工灾难恢复，不能删除现场硬跳过检查。

当前项目尚未上线，数据库兼容组为 `1`，首次部署以当前模型创建数据库，不支持开发期旧结构及旧数据升级。开发期模型发生删除字段或语义调整后，使用新建测试数据库验收。兼容标识仍用于部署、备份恢复和版本回退检查；正式发布后，涉及已发布结构的破坏性变更再提供独立迁移方案，并同步调整兼容标识、升级测试和发布门禁。

## 实际上线验证边界

自动模拟测试不证明真实 ABS 的接口兼容性，也不完整证明 Telegram 群权限与展示。首次正式启动后检查 `/start`、管理面板、真实 ABS 连通性、基本账号状态和每日备份。涉及真实写入的验收使用专用测试账号，避免拿普通用户试操作。

本项目脚本演练通过真实维护程序验证数据库和加密恢复；Docker 编排边界被模拟。实际镜像启动、双架构构建和容器冒烟检查由 GitHub 候选流水线执行，必须看到相应构建成功后才发布。
