# Session Handoff

更新时间：2026-07-26（Asia/Shanghai）

本文用于在另一台电脑恢复仓库和开发上下文。它记录当前架构、代码分布、关键约束、最近改动和验证方法，不包含任何真实 Token、密码、代理链接、订阅 URL 或数据库路径。

## 恢复仓库

- 远端：`git@github.com:smagicalk/smalux-speedtest.git`
- 当前开发分支：`vibe-dev`
- 编写本文时远端分支基线：`a73c372`
- `vibe-dev` 已推送，本地与 `origin/vibe-dev` 在编写本文前一致。
- Go module 要求 Go 1.26，具体依赖以 `go.mod` 和 `go.sum` 为准。

在新电脑执行：

    git clone git@github.com:smagicalk/smalux-speedtest.git
    cd smalux-speedtest
    git switch vibe-dev
    git pull --ff-only origin vibe-dev
    git status --short --branch
    go version
    go mod download
    make test
    make build

没有配置 SSH Key 时可改用：

    git clone https://github.com/smagicalk/smalux-speedtest.git

恢复后先阅读本文、`README.md`、`docs/index.html`，再检查 `git log -5 --oneline`。分支可能已包含更新本文的新提交，因此不要强制 checkout 到旧哈希；`a73c372` 只用于识别本轮之前的远端基线。

## 系统目标

Smalux Speedtest 是分布式代理测速系统：中心 Server 管理用户、Client、任务和结果；远程 Client 接收单代理工作单元，使用内嵌 sing-box outbound 转发 speedtest-go 的 HTTP/HTTPS 流量，然后回传进度和脱敏结果。

    浏览器 / Telegram
            |
            v
    Server HTTP API + importer + SQLite
            |
            v  WebSocket protocol v2
       Hub scheduler
            |
            v
    Remote Client -> sing-box outbound -> Speedtest.net
            |
            +---- progress / result ----> Server

核心原则：Server 可以短暂持有完整代理配置用于调度，但不得把原始分享链接、订阅正文或完整 outbound 写入 SQLite、日志、报告或管理 API。

## 运行链路

1. 网页或 Telegram 提交代理文本，或由 `internal/subscription` 安全抓取订阅。
2. `internal/importer` 将单条、多行或整体 Base64 订阅转换为 `model.ProxySpec`。
3. `serverapp.startTask` 校验任务参数和 Client，数据库仅保存任务摘要，完整代理只进入 Hub 内存。
4. Hub 将批次拆成单代理工作单元。同一 Client 同时只跑一个工作；相同 outbound 在所有活动任务间全局排他。
5. Client 收到 `task.assign` 后发送 ACK，为当前工作建立超时 context，并调用 Executor。
6. Executor 为每个代理启动独立 sing-box 实例，使用其 `DialContext` 承载 speedtest-go 请求。
7. Client 回传进度、每个测速节点的结果和完成/失败消息。
8. Server 验证结果身份和数量，持久化脱敏结果；任务完成、取消、超时、断线或撤销 Token 都会释放 lease 和敏感内存。

## 应用层协议

- 协议版本：`internal/model/model.go` 中 `ProtocolVersion = 2`。
- 消息封装：`internal/wire/wire.go` 的 `Envelope`。
- 双向单消息上限：2 MiB。
- 主要消息：`client.hello`、`server.welcome`、`task.assign`、`task.ack`、`task.progress`、`task.result`、`task.complete`、`task.failed`、`task.cancel`、`ping`、`pong`。
- Server 和 Client 必须同步升级；协议版本不一致会拒绝连接。
- 一个 Assignment 默认总超时 600 秒。

## 代码分布

### 可执行入口与构建

| 路径 | 职责 |
| --- | --- |
| `server/main.go` | Server 参数/环境变量、日志、信号和优雅关闭 |
| `client/main.go` | Client URL/Token、版本、日志、信号和运行入口 |
| `Makefile` | `make build`、`make test`；Client 默认使用 `with_utls` |
| `.github/workflows/release.yml` | 测试、六平台构建、Artifacts、Release 和 SHA256SUMS |
| `scripts/smalux.sh` | Linux Release 下载、校验和 systemd 安装/升级管理 |

### Server

| 路径 | 职责 |
| --- | --- |
| `internal/serverapp/app.go` | App 装配、内嵌 Web UI、HTTP 路由、Shutdown 顺序 |
| `internal/serverapp/auth.go` / `internal/serverapp/sessions.go` | 管理员认证、Cookie Session 和 CSRF |
| `internal/serverapp/clients.go` | Client 创建、元数据、撤销和权限边界 |
| `internal/serverapp/task_service.go` | 导入、任务参数校验、单工作单元大小检查和下发 |
| `internal/serverapp/hub.go` | Client WebSocket、Peer 和 Hub 共享状态 |
| `internal/serverapp/hub_dispatch.go` | Client/proxy lease、轮转、公平调度和工作分配 |
| `internal/serverapp/hub_tasks.go` / `internal/serverapp/hub_results.go` | ACK、完成/失败、结果保存和状态迁移 |
| `internal/serverapp/hub_result_validation.go` | 不信任 Client 回传，按 Assignment 重建结果身份 |
| `internal/serverapp/telegram*.go` | Bot 动态绑定、加密配置、授权、任务与报告适配 |
| `internal/serverapp/web/*` | 内嵌管理页面、JS 和 CSS |

### Client

| 路径 | 职责 |
| --- | --- |
| `internal/clientapp/client.go` | 配置校验、WebSocket 重连和握手生命周期 |
| `internal/clientapp/connection.go` | 串行发送、读限制、当前任务取消状态 |
| `internal/clientapp/worker.go` | Assignment 执行、进度/结果回传、心跳 |
| `internal/clientapp/executor.go` | sing-box + speedtest-go 测速流程和结果归一化 |
| `internal/clientapp/singbox.go` | 最小 sing-box 配置与协议 Outbound 注册表 |
| `internal/clientapp/errors.go` | 对外固定错误类别，避免泄露底层网络细节 |

### 导入、模型与持久化

| 路径 | 职责 |
| --- | --- |
| `internal/importer/importer.go` | 订阅识别、逐行导入、去重和安全错误 |
| `internal/importer/uri.go` | 标准 URI 协议映射和 Hysteria 端口跳跃 |
| `internal/importer/vmess.go` | VMess Base64(JSON) 转换 |
| `internal/importer/shadowsocks.go` | SIP002/旧格式和插件参数 |
| `internal/importer/transport.go` | V2Ray transport、TLS/uTLS 和 Reality |
| `internal/model/*` | Assignment、ProxySpec、Progress、SpeedResult 和隐私归一化 |
| `internal/wire/*` | WebSocket Envelope、版本和消息大小限制 |
| `internal/store/*` | SQLite schema、迁移、账户、Client、任务和脱敏结果 |
| `internal/subscription/*` | 5 MiB 限制、SSRF 防护、DNS/重定向复验和超时 |
| `internal/logsafe/*` | 日志错误类型归一化 |
| `internal/reportpng/*` | 有界 PNG 报告布局、字体和渲染 |
| `internal/telegrambot/*` | Telegram API、命令、向导、进度和投递队列 |

## 持久化与隐私边界

SQLite 表由 `internal/store/migration.go` 管理，主要包括管理员、邀请、Client、任务、目标状态、结果、Telegram 配置/用户/offset 和 settings。

允许持久化：

- 管理员密码 bcrypt 哈希、Client Token 哈希。
- Client 的受限名称/标签和启用状态。
- 任务参数、创建管理员 ID、成功/跳过计数、状态和固定错误类别。
- 脱敏代理地址、规范化节点名/协议、公开 Speedtest 节点元数据和测量值。
- 加密后的 Telegram Bot Token；本地加密 key 与数据库分开保存。

禁止持久化或记录：

- 原始代理链接、订阅 URL/正文、完整 outbound、代理密码/UUID/密钥。
- Client Token 明文、Bot Token 明文、远端 Client 地址和底层连接错误。
- 任意不受信任错误文本或可能带凭据的请求正文。

隐私迁移位于 `internal/store/privacy_migrations.go` 与 `internal/store/privacy_timestamp_migration.go`。新增字段时要同时审查数据库、API、日志、CSV/PNG 和迁移路径。

## 当前协议支持

| 协议 | 输入格式 |
| --- | --- |
| Shadowsocks | `ss://` |
| VMess | `vmess://Base64(JSON)` |
| VLESS | `vless://` |
| Trojan | `trojan://` |
| SOCKS5 | `socks://`、`socks5://` |
| HTTP 代理 | `http://`、`https://` |
| SSH | `ssh://` |
| AnyTLS | `anytls://` |
| Hysteria v1 | `hysteria://` |
| Hysteria2 | `hysteria2://`、`hy2://` |
| TUIC | `tuic://` |

VMess、VLESS、Trojan 的 V2Ray transport 支持默认 TCP（省略、`tcp`、`raw`、`none`）、WebSocket、gRPC、HTTP/H2 和 HTTPUpgrade。WebSocket 支持 `ed` / `eh`，gRPC 支持 `serviceName` / `service_name`。VLESS/VMess 支持 packet encoding，VLESS 支持 Reality。

限制：

- KCP 和 V2Ray QUIC transport 在导入阶段拒绝。
- 正式 Client 只用 `with_utls`，没有注册 `with_quic` 的 V2Ray QUIC 构造器。
- Hysteria、Hysteria2、TUIC 使用各自已注册的原生 QUIC，不受上一条限制。
- SSR 已从 sing-box 1.13 移除；Naive 因 Cronet 体积未注册。

## 最近完成的改动

- 当前工作区（未提交）
  - 管理后台改为紧凑响应式布局，并完成桌面/390px 浏览器验证。
  - 任务按创建管理员隔离；Owner 可管理全部任务，旧任务只对 Owner 可见。
  - 任务详情展示安全的导入跳过计数与每个目标 Client 状态；target SSE 实时同步运行、终态和断线重排。
  - 失败、部分完成和取消任务按实际结果显示进度，REST 请求具有超时且后台标签暂停轮询。

- `5357055 feat: improve proxy share link compatibility`
  - 扩展 VLESS/VMess/Trojan transport 字段。
  - 补齐 Hysteria v1/v2 认证、带宽、混淆和端口跳跃。
  - 修正 Shadowsocks 明文百分号转义与 Base64 密码双重解码。
  - 未支持 transport 改为明确拒绝。
  - 增加 importer 转换测试和真实 sing-box 初始化测试。
- `96f33d9 docs: document supported proxy protocols`
  - 更新 README 和 docs 协议矩阵及限制。
- `a73c372 docs: save development session handoff`
  - 创建最初的 `session.md`。

## 构建、测试与运行

常用检查：

    make test
    make build
    go test -p=1 -count=1 ./...
    go test -p=1 -count=1 -tags with_utls ./...
    go test -race ./internal/importer ./internal/clientapp
    go vet ./...
    git diff --check

本地启动示例只使用占位秘密：

    SMALUX_ADMIN_PASSWORD='replace-me' go run ./server \
      -listen 127.0.0.1:8080 -db smalux-speedtest.db

    SMALUX_CLIENT_TOKEN='replace-me' go run -tags with_utls ./client \
      -server ws://127.0.0.1:8080/ws/client

Server 默认拒绝远程明文 Client WebSocket。公网必须由 HTTPS/WSS 反向代理保护；`-allow-insecure-ws` 仅限可信内网临时测试。

任务默认值和范围：Candidate 10（1..50）、Top N 3（1..3 且不超过 Candidate）、Threads 4（1..32）、总超时 600 秒。远程订阅正文上限 5 MiB，单代理 wire 工作单元上限 2 MiB。

## 测试覆盖与已知问题

- `internal/importer/protocol_conversion_test.go` 覆盖字段转换、非法 transport、Hysteria 带宽/端口和 Shadowsocks 编码边界。
- `internal/clientapp/imported_outbounds_test.go` 使用正式 `with_utls` 注册表初始化所有支持协议族。
- `TestTelegramLifecycleSynchronizesOwnerAndStopsPolling` 存在既有低概率时序失败：Shutdown 后请求数偶尔多 1。该失败已在不包含 importer 改动的旧 `HEAD` 临时副本中复现，不是 `5357055` 引入。
- 最近完整验证通过：普通/uTLS 全仓测试、importer/client race、build、vet 和 diff check；关键转换/初始化测试曾连续运行 10 轮。

## 修改检查清单

新增或调整协议时必须同步：

1. 修改 importer 白名单和协议转换。
2. 确认 `minimalBoxContext` 已注册生产运行所需实现及构建标签。
3. 增加字段级转换测试和真实 `startBox` 初始化测试。
4. 检查单消息 2 MiB 边界与错误脱敏。
5. 更新 `README.md`、`docs/index.html` 和本文。
6. 运行普通/uTLS 测试、race、build、vet、diff check。

修改 Server 状态机时还要检查 Store 迁移、Hub lease 释放、断线/撤销/取消/超时，以及 Client 回传不可信边界。修改任何持久化或报告字段时，先证明不会泄露代理、订阅、Token、地址或任意错误文本。

## 下一步候选

- 单独修复 Telegram polling Shutdown 的 flaky test，不与 importer 改动混合。
- 如需 V2Ray QUIC，必须同时改变发布构建标签、导入构造器注册包并验证六个平台，不能只允许 URI。
- 新机器继续开发前先确认 `git status` 干净、`vibe-dev` 与远端同步，并运行 `make test`。
