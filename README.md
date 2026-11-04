# 昔风听书助手

基于 Go 的 Telegram Bot，连接 Audiobookshelf（ABS），提供账号管理、听书统计和社群活动。本仓库包含 Bot、SQLite 数据模型、模拟 ABS、自动测试，以及镜像发布和部署工具。普通用户的命令与玩法见[用户指南](docs/user-guide.md)。

## 开发环境

- Go 1.26.10；SQLite 驱动使用 CGO，需要 C 编译器。
- Python 3 用于部署脚本回归测试；Docker 用于镜像构建和可选的集成验收。
- 从 [.env.example](.env.example) 准备正式配置；开发测试使用独立的 `.env.test`、测试 Bot Token 和内置模拟 ABS。配置校验在 `internal/config/`。

```sh
./scripts/test-bot.sh init
./scripts/test-bot.sh run
```

`init` 交互读取独立测试 Bot 的 Token、用户名和管理员 Telegram 数字 ID，生成 `.env.test` 与随机密钥，不覆盖现有配置。测试 Bot 需先在 BotFather 创建。

`run` 从源码前台启动，按 `Ctrl+C` 停止；也可运行 `go run . test-bot`。它只读取 `.env.test`，不读取正式 `.env` 或继承终端中的正式 Bot/ABS 配置。脚本识别 `.tools/go/bin/go` 工具链。测试模式通过用户名校验独立 Bot，仅接受配置管理员的私聊，禁止群消息和向群、频道及其他用户发送请求。内置 ABS 模拟器没有真实网络回退。

## 代码结构

| 路径 | 职责 |
| --- | --- |
| `main.go`、`internal/app/` | 启动、数据库初始化、Telegram 路由、管理流程和定时任务 |
| `internal/account/` | 注册、绑定、续期、签到和邀请关系 |
| `internal/books/` | 求书工单、书库事件和公告 |
| `internal/game/` | 听书修为、斗气、宗门、秘境、世界 Boss 和药园 |
| `internal/activity/` | 抽奖、红包、盲盒和寻宝 |
| `internal/market/` | 交易行和卡密回收 |
| `internal/model/`、`internal/storage/` | GORM 模型、SQLite 索引、钱包事务与审计 |
| `internal/telegram/` | 发送、自动清理、回调、会话和群权限 |
| `internal/abs/`、`internal/config/` | ABS API 客户端与配置校验 |
| `internal/operations/`、`internal/backup/` | 离线维护、健康检查和加密备份 |
| `internal/security/`、`internal/textfmt/` | 密钥与随机数、消息格式化及脱敏 |

详细入口见[项目目录说明](docs/project-layout.md)。`app` 组装业务模块；跨模块能力放在职责对应的公共包，避免业务包相互导入。新增消息或回调时，复用路由的身份与权限检查、Telegram 发送封装和业务事务边界。

## 数据与外部服务约束

- 项目尚未正式发布。首次部署按当前模型和索引创建数据库，不支持开发期旧表结构或旧数据的自动升级。结构或语义调整后，用新建的测试库验收。部署兼容标识用于将来发布版本的备份、更新和回退检查，不负责转换开发期数据库。
- 积分、卡密、贡献和奖励等资产变更应在事务内完成，并以唯一约束或持久化记录防止重复处理。外部 ABS 或 Telegram 调用失败后的恢复需保持幂等；不能把数据库读取失败当作记录不存在。
- 听书统计以 ABS 官方 `listening-stats.days` 为总量来源。历史会话用于跨日修正和可证明的播放凭据，不作为实时播放总量。后台分页扫描使用持久化游标。
- Telegram 文本经 `internal/telegram` 发送；动态用户内容应由 `internal/textfmt` 格式化。新增菜单或指令后运行 `go run ./scripts/generate-command-catalog`，更新群关键词回复的指令冲突词表。
- 加密密钥、Bot Token 和运行数据不进仓库。`.env`、`data/`、`data-test/` 由 Git 忽略。

## 验证

```sh
./scripts/check.sh
go build -o ccoff-abs-bot .
```

该入口运行全量 Go 测试、竞态测试、`go vet`、部署与发布演练，以及 Shell 语法检查。只验证某个包时可运行 `go test ./internal/包名`。没有本地 Go 时，检查脚本使用 Docker 工具链；部署演练仍需 Python 3.9+。

业务回归测试与代码放在同一包；`internal/app/` 包含跨模块和模拟 Telegram 的测试。涉及积分、库存、卡密或外部调用的修复，验证重复请求、失败回滚和重启恢复；定时任务使用明确时间驱动。使用包级数据库或配置的测试不直接调用 `t.Parallel()`。保留能发现实际业务回归的测试，避免另写逐条复制断言的测试文档。

### 本地测试 Bot

启动后在独立测试 Bot 私聊发送 `/start`，再按需使用以下开发命令：

```text
/test
/test seed
/test listening 2026-10-28 2.5
/test failure 500
/test failure off
```

`seed` 在没有档案时创建 1000 积分、30 天有效期的虚拟正式账号，已有档案保持原样。模拟用户名为 `demo你的ID`，密码为 `TestPassword123!`，安全码为 `TestSecurity123!`。验收注册时从空测试库开始，不先执行 `seed`；模拟配置默认免邀请码。

`listening 日期 小时` 设置指定日期的模拟听书时长，范围 0–24 小时；将示例日期替换为测试日期，再刷新听书报告。`failure` 支持 `timeout`、`500`、`invalid-json`、`off`，持续作用于后续 ABS 请求，直到关闭。模拟数据保存在 `data-test/`，重启不清空。

停止源码实例后，可用 `./scripts/test-bot.sh reset-native --confirm` 重建测试数据；脚本用 `lsof` 检查目录未被进程占用，只清理带测试标记的 `data-test/`，保留配置与密钥。

### 容器与候选镜像

准备 Docker Desktop 或 OrbStack，并确认 `docker compose version` 可用：

```sh
./scripts/test-bot.sh up
./scripts/test-bot.sh logs
./scripts/test-bot.sh status
./scripts/test-bot.sh reset --confirm
./scripts/test-bot.sh down
```

`up` 构建本地测试镜像；`reset` 停止测试容器后清理数据，须在 `down` 删除容器之前执行。容器与源码模式共用 `data-test/`，切换前停止现有实例，避免同一 Token 启动两个 Bot。正式 `.env` 和 `data/` 不参与测试流程。

发版前使用本地源码或 `./scripts/test-bot.sh up` 验收待发布提交。版本 tag 触发构建后，也可用 `./scripts/test-bot.sh up sha-完整40位提交SHA` 验收对应镜像；私有镜像需先登录 GHCR，镜像支持 amd64 和 arm64。模型变更后重新创建测试库，不复制正式数据库来测试。

### 验证边界

模拟 ABS 覆盖账号、登录、状态和听书统计，媒体库与会话查询返回空集合，未知接口返回 501；它不能证明真实 ABS 的接口行为。首次上线及升级 ABS 时，使用专用账号核对真实接口。`scripts/test_deploy.py` 演练维护程序、SQLite、加密备份、更新失败和回退，但模拟 Docker 与 Telegram；CI 在两个架构的真实容器中另做维护命令冒烟检查。

群权限、入群验证、频道公告、多人回调、置顶及消息展示先由模拟测试覆盖，再在独立测试群和频道验收。重点核对 Bot 权限、本人验证、超时处理、退订与重新加入、重启恢复、公告链接、私聊入口和提醒清理。公告发送结果不确定时，先核对频道实际内容，再处理重试。

## 发布与部署

普通提交和 PR 不触发 GitHub Actions。将代码推送到 `main` 后，运行 `./scripts/release.sh` 自动选择下一补丁版本，或用 `./scripts/release.sh v1.1.0` 指定版本；推送 `vX.Y.Z` tag 后，Actions 执行检查、双架构镜像构建和容器冒烟测试，全部成功后自动创建同名 GitHub Release，附上部署包、校验文件和 `release.json`。`scripts/package-release.sh` 生成部署包。正式设备使用固定镜像 digest、加密备份和健康检查；更新、回退及数据恢复步骤见[部署指南](docs/deployment.md)。
