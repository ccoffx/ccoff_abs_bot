# 开发与测试

日常流程：**Mac 修改代码并补测试 → 自动检查 → 独立测试 Bot 私聊验收 → 发布指定候选镜像 → 正式设备更新。**

## 首次准备：只创建一个测试 Bot

在 BotFather 创建独立 Bot，准备它的 Token、用户名及你自己的 Telegram 数字 ID。本地开发可以直接运行 Go 源码，无需 Docker；准备 Go 1.26.10 与 C 编译器（Mac 的 Xcode Command Line Tools）。脚本也会识别项目 `.tools/go/bin/go` 工具链。

```sh
./scripts/test-bot.sh init
./scripts/test-bot.sh run
```

`run` 在当前终端前台编译运行，日志直接显示在终端，按 `Ctrl+C` 停止。修改代码后重新执行即可。已经生成 `.env.test` 的用户直接运行 `run`，无需重新 `init`。如果 Go 已在 PATH，也可以在项目根目录执行 `go run . test-bot`。

源码启动只读取 `.env.test` 中的应用配置，不读取正式 `.env`，也不继承终端中导出的正式 Bot/ABS 配置。普通 `go run .` 仍读取正式环境配置，请使用带 `test-bot` 的入口来启动测试 Bot。

如需验收 Docker 镜像，在 Mac 安装并启动 Docker Desktop 或 OrbStack，确认 `docker compose version` 可用后执行 `./scripts/test-bot.sh up`。后文的 `up`、`down`、`logs`、`status`、`reset` 命令用于 Docker 测试容器；源码启动使用当前终端查看日志和停止程序。两种方式使用同一个 `data-test/`，切换前先停止另一种方式，避免同一 Bot Token 被两个实例使用。

`init` 交互读取信息，自动生成 `.env.test` 与两个独立随机密钥，不覆盖现有文件。无需填写真实 ABS 地址和密钥，也无需创建群聊、备份群或第二个服务。测试配置模板是根目录 `.env.test.example`。

打开这个测试 Bot，先发送 `/start`，再发送 `/test`。首次启动时，Bot 可能因你尚未主动私聊而无法发送启动通知；不影响你之后使用。测试身份由 `TEST_BOT_USERNAME` 校验，不匹配时在清理 webhook、接收消息之前退出。必须填写独立 Bot 的用户名，不要填写正式 Bot。

测试模式仅接受 `ADMIN_TELEGRAM_IDS` 中用户的私聊与私聊按钮；群消息、非管理员消息和跨用户按钮被忽略。发送请求也检查目标用户，后台任务不能借测试 Bot 向群或其他人发消息。

## 准备数据与验收功能

在私聊中使用：

```text
/test seed
/test listening 2026-10-28 2.5
/test failure 500
/test failure off
```

- `seed` 在没有本地档案时，为你创建 1000 积分、30 天有效期的虚拟正式账号；已有档案保持原样，重复调用不会反复加分。模拟用户名为 `demo你的ID`，模拟 ABS 密码为 `TestPassword123!`，安全码为 `TestSecurity123!`。这些凭据只用于本地虚拟账号。
- `listening 日期 小时` 设置指定自然日的模拟时长，范围 0～24 小时；将示例日期替换为你需要测试的日期，再打开听书报告刷新。数据持久保存，重启不会清空。
- `failure` 支持 `timeout`、`500`、`invalid-json` 和 `off`，作用于后续 ABS 请求，直到关闭。它不会阻断 `/test` 命令本身或 Telegram 消息。
- 验收注册流程时，使用新测试数据库，不先执行 `seed`；测试配置默认免邀请码，直接走注册步骤。需要积分验收时再准备测试档案。绑定可使用 `/test seed` 创建的模拟账号；先在 Bot 中完成解绑，再验证绑定流程。

```sh
./scripts/test-bot.sh logs
./scripts/test-bot.sh status
./scripts/test-bot.sh down
./scripts/test-bot.sh reset --confirm
```

`reset` 先停止测试容器，仅清理带测试标记的 `data-test/`，保留 `.env.test` 和密钥。需要测试容器仍存在，以便确定其镜像；如果已经执行 `down`，先 `up` 再 `reset`。正式 `.env` 和 `data/` 不参与这个流程。不要直接复制正式数据库来做测试。

模拟器覆盖账号增删改、登录验证、停用恢复、听书统计，以及后台所需的空会话和空媒体库查询。它不模拟真实播放过程、书库内容或封面。未知接口返回 501；模拟传输没有真实网络回退，即使客户端 URL 被改成真实地址也会拒绝请求。

## 每次修改的自动检查

原生 Go 开发使用 Go 1.26.10 和 C 编译器；Mac 可通过 Xcode Command Line Tools 提供 C 编译器。统一入口：

```sh
./scripts/check.sh
```

它执行 `go test ./...`、`go test -race ./...`、`go vet ./...`，再执行部署演练。没有本地 Go 时，Go 检查使用固定工具链的 Docker 镜像；部署演练仍需 Python 3.9+，并通过同一个 Docker 工具链编译维护程序。无需 AI 即可运行检查。

功能变更至少补充成功与失败场景。涉及积分、库存或卡密时检查重复回调、并发和事务失败回滚；定时任务使用明确时间驱动检查，不等待真实的一天过去。修复 Bug 时先写复现测试。使用包级全局数据库或配置的测试不要直接加 `t.Parallel()`。

已有测试覆盖多种资产操作、活动重启、模拟 Telegram 按钮和权限。新增测试覆盖模拟 ABS 的账号生命周期与并发、测试环境隔离、旧结构升级、WAL 备份、加密恢复和健康心跳。`internal/app/testdata/legacy-compatible.sql` 是固定的合成旧结构样例，不能随当前模型更新而重写；发布后应追加对应版本的脱敏样例来保留升级路径。

`scripts/test_deploy.py` 使用真实维护程序和 SQLite/加密代码演练更新失败、兼容版本回退、显式恢复与外部副本；Docker 命令和 Telegram 可用性在这里被模拟。它不等于实际 Docker 引擎的部署验证。候选镜像 CI 另在 amd64、arm64 容器中执行维护命令的实际冒烟检查。

## 验收 GitHub 候选镜像

主分支检查通过后发布 `ghcr.io/huliyoudiangou/yueer_abs_bot:sha-完整40位提交SHA`，支持 Mac ARM 与 Linux x86。私有镜像先执行一次 `docker login ghcr.io`，使用具备 `read:packages` 权限的凭据。

```sh
./scripts/test-bot.sh up sha-完整40位提交SHA
```

这里填实际 SHA，例如从 GitHub Actions 对应候选构建中复制。命令拉取镜像并替换测试容器，保留测试数据库。若需要从零验收，使用重置命令后再次启动相同候选。

私聊验收本次改动及其相邻功能，通过后按部署指南发布该 SHA。SHA 标签在候选重试时复用原镜像，正式版本直接复用其 manifest，避免验收之后重新编译。

## 验证边界

不建立测试群时，群消息、成员权限、多人点击和失败响应靠自动模拟测试覆盖；真实 Telegram 群权限、置顶行为与视觉展示仍未完整验收。内置 ABS 模拟器检验程序流程，无法证明真实 ABS 的接口版本兼容。首次上线及升级 ABS 版本时，需要在正式服务上用明确的专用账号做基本连通性与状态核查，不能把模拟测试结果视为真实接口验证。
