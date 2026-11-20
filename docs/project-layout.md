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
| 境界、称号、榜首荣誉和群标签 | `internal/realm/`、`internal/titles/` |
| 抽奖、红包、盲盒和寻宝 | `internal/activity/` |
| 交易行和卡密回收 | `internal/market/` |

斗气、宗门和听书修为共用结算规则，暂时保留在同一个 `game` 包中，避免这些玩法互相导入而产生循环依赖。

## 公共能力

| 包 | 职责 |
| --- | --- |
| `model` | 按账号、钱包、宗门、活动等分类的 GORM 模型与表名定义 |
| `storage` | 数据库连接状态、索引、系统配置、账号资格、钱包事务、卡密、审计和安全校验 |
| `rankings` | 财富、斗气、宗门榜与榜首称号共用的资格、排序和并列规则 |
| `lifecycle` | 跨业务的 ABS 账号恢复、播放冻结解除与持久化生命周期互斥保护 |
| `telegram` | 消息发送队列、自动删除、回调应答、菜单、群成员校验、会话和用户锁 |
| `abs` | Audiobookshelf 请求、账号管理和统计 API；听书报告的修为计算在 `game` 中 |
| `config` | `.env` 与环境变量加载、必填配置校验 |
| `security` | 安全令牌哈希、校验和密码学随机数 |
| `backup` | 数据库快照、备份加解密和文件分片 |
| `metrics` | 消息队列、API 请求和慢处理的运行指标 |
| `textfmt` | Markdown 转义、敏感信息脱敏、文本与数字解析 |

`app` 负责组装业务模块。公共包不导入业务包；需要新增跨业务能力时，优先放入职责对应的公共包，保持依赖单向。模型集中声明，首次部署按当前模型和索引定义创建完整数据库。业务代码统一使用当前结构；异火相关表缺失、活动配置或公告预览快照不完整时返回错误，不提供开发期旧结构和旧数据的补迁入口。

### 累计数值与听书同步

药草收获数、成丹数存放在 `garden_production_totals`，宗门商店累计贡献消费存放在 `sect_shop_contribution_totals`。`storage/business_totals.go` 在首次升级时按现有明细回填一次，后续通过 SQLite 触发器在原业务事务内维护差额，覆盖数量修正、软删除、恢复、硬删除与换绑。称号榜和累计贡献资格查询直接读取累计值；明细继续保留用于审计。重复启动不会重扫历史；累计表缺失时会从明细重建。

听书修为仍按每日新旧已结算金额的差额更新。`ListeningReportSnapshot.OfficialInputJSON` 保存上次成功同步的官方按日输入与已计入秒数，普通同步仅加载新增或发生变化的日期，以及今天的完整日记录。跨日时重新检查上次拉取日期的时间上限；首次同步、换绑、缓存失效时按输入日期分批加载。`storage/listening_input_cache.go` 用触发器在日记录被修正、删除、恢复或转移时使缓存失效。源记录、修为结算与新快照同事务提交，失败后重试不会漏算或重复增加。

ABS 的 `listening-stats` 仍返回整份 `days`，因此接收、校验和快照比对需要遍历返回日期；本地数据库不再为每次同步加载全部历史日记录。宗门秘境复用增量结算后的修为总值，仍校验闭关证据，避免损坏证据被当作零加成。

## 配置、部署与测试

- `.env.example` 是配置模板；实际 `.env` 与运行数据 `data/` 保留在根目录，并由 Git 忽略。
- `Dockerfile` 构建多架构镜像；`docker-compose.yml` 用于固定 digest 的正式部署，`compose.test.yml` 用于 Mac 测试 Bot；入口脚本在 `scripts/docker-entrypoint.sh`。
- `go run .`、`go test ./...`、`go vet ./...` 和 `go build -o ccoff-abs-bot .` 都从根目录执行。
- 包内测试随代码保留；`internal/app/` 的集成测试覆盖数据库新建与重启、备份、资产事务、并发领取和 Telegram 回调。
- 用户操作说明见 [用户使用指南](user-guide.md)。
- 自动回归、内置 ABS 模拟器和测试 Bot 操作见 [README](../README.md#验证)，镜像发布与运维见 [部署指南](deployment.md)。`internal/operations/` 提供不启动 Bot 的健康检查、SQLite 加密备份和恢复；`internal/buildinfo/` 保存构建与数据库兼容标识。


`internal/joinverify/`：关注频道入群验证，包含 Telegram 管理接口、持久化验证状态机、成员事件队列、验证按钮和超时恢复；通过 `internal/app` 接入消息与按钮业务门禁。对应持久化模型位于 `internal/model/join_verification.go`。


`internal/announcements/`：频道公告持久化投递、消息链接、群提醒、发送中断恢复和人工处置。`internal/app/channel_announcements.go` 接入注册/补偿状态及服务公告向导；`internal/activity/lottery_entry.go` 提供抽奖私聊深链接和二次确认。
