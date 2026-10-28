# 项目目录说明

根目录的 `main.go` 只负责调用 `app.Run()`。业务代码放在 `internal/` 下，以 Go 包隔离依赖；从项目根目录运行和构建即可，无需逐个指定 `.go` 文件。

## 功能入口

| 功能 | 代码位置 |
| --- | --- |
| 启动和数据库初始化 | `internal/app/runtime.go`、`internal/app/database.go` |
| Telegram 消息路由、管理菜单 | `internal/app/router.go`、`internal/app/*callbacks.go` |
| 定时调度、账号到期处理 | `internal/app/scheduler.go` |
| 开放注册、用户补偿和管理操作 | `internal/app/admin_*.go` |
| 注册、绑定和账号状态 | `internal/account/` |
| 邀请拉新、签到、财富榜 | `internal/account/` |
| 求书工单、入库事件和公告投递 | `internal/books/` |
| 斗气配置、突破和材料兑换 | `internal/game/cultivation*.go` |
| 药园和背包 | `internal/game/garden.go` |
| 宗门、科技、喇叭、宗门抽奖和秘境 | `internal/game/sect*.go` |
| 听书统计、跨日修正、违规检测和排行榜 | `internal/game/*listening*.go`、`internal/game/abs_cross_day_*.go`、`internal/game/leaderboard.go` |
| 世界 Boss | `internal/game/world_boss.go` |
| 抽奖、红包、盲盒和寻宝 | `internal/activity/` |
| 交易行和卡密回收 | `internal/market/` |

斗气、宗门和听书修为共用结算规则，暂时保留在同一个 `game` 包中，避免这些玩法互相导入而产生循环依赖。

## 公共能力

| 包 | 职责 |
| --- | --- |
| `model` | 按账号、钱包、宗门、活动等分类的 GORM 模型与表名定义 |
| `storage` | 数据库连接状态、索引、系统配置、账号资格、钱包事务、卡密、审计和安全校验 |
| `telegram` | 消息发送队列、自动删除、回调应答、菜单、群成员校验、会话和用户锁 |
| `abs` | Audiobookshelf 请求、账号管理和统计 API；听书报告的修为计算在 `game` 中 |
| `config` | `.env` 与环境变量加载、必填配置校验 |
| `security` | 安全令牌哈希、校验和密码学随机数 |
| `backup` | 数据库快照、备份加解密和文件分片 |
| `metrics` | 消息队列、API 请求和慢处理的运行指标 |
| `textfmt` | Markdown 转义、敏感信息脱敏、文本与数字解析 |

`app` 负责组装业务模块。公共包不导入业务包；需要新增跨业务能力时，优先放入职责对应的公共包，保持依赖单向。模型集中声明，首次部署按当前模型和索引定义创建数据库；开发期旧结构不作为兼容目标。

## 配置、部署与测试

- `.env.example` 是配置模板；实际 `.env` 与运行数据 `data/` 保留在根目录，并由 Git 忽略。
- `Dockerfile` 构建多架构镜像；`docker-compose.yml` 用于固定 digest 的正式部署，`compose.test.yml` 用于 Mac 测试 Bot；入口脚本在 `scripts/docker-entrypoint.sh`。
- `go run .`、`go test ./...`、`go vet ./...` 和 `go build -o ccoff-abs-bot .` 都从根目录执行。
- 包内测试随代码保留；`internal/app/` 的集成测试覆盖数据库新建与重启、备份、资产事务、并发领取和 Telegram 回调。
- 用户操作说明见 [用户使用指南](user-guide.md)。
- 自动回归、内置 ABS 模拟器和测试 Bot 操作见 [测试指南](testing.md)，镜像发布与运维见 [部署指南](deployment.md)。`internal/operations/` 提供不启动 Bot 的健康检查、SQLite 加密备份和恢复；`internal/buildinfo/` 保存构建与数据库兼容标识。
