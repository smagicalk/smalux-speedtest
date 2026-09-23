# Smalux Speedtest

[中文说明](README.md) | [English](README_EN.md)

分布式代理测速系统：由中心控制端（Server）与多地远程执行端（Client）组成。服务端通过 WebSocket 调度并下发单代理工作单元；客户端接收后拉起内置 sing-box 独立出站实例，经由被测代理对公网 Speedtest.net 测速节点执行全方位网络评估（延迟、抖动、下载、上传、丢包），并回传脱敏数据与实时进度。

项目在线完整文档：[`docs/index.html`](docs/index.html)。包含[运行时调用流程](docs/index.html#call-flow)、[二次开发与扩展配方](docs/index.html#extension)以及[本地调试与测试指南](docs/index.html#testing)。

## 项目核心特性

* **中心调度与分布式执行**：服务端集中管控账户、Client、测速任务与历史快照；客户端多节点跨地域部署。
* **协议 v2 租约调度（Lease）**：单客户端单工作单元，相同 Outbound 节点跨任务全局排他测试，兼顾测速效率与多地域横向对比完整性。
* **严格的隐私与安全边界**：原始代理链接、订阅内容、sing-box 出站配置**仅驻留内存，绝不落盘持久化**；历史记录与结构化日志全方位脱敏。
* **多渠道任务下发**：提供内嵌 Web 控制台与 Telegram Bot 双入口；支持多管理员邀请注册与基于所有权的任务隔离。
* **丰富的结果呈现**：实时 SSE 进度同步，支持一键导出 CSV 数据报表或生成带品牌标识的清晰 PNG 测速结果长图。
* **一键容器化与运维脚本**：支持 Docker 多模式轻量镜像与 Linux systemd 一键安装管理脚本。

## 编译与构建

```bash
make build
```

编译产物位于 `dist/` 目录，包含 `smalux-server` 和 `smalux-client`。

Windows 客户端构建：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -buildvcs=false -tags with_utls -o smalux-client.exe ./client
```

> [!IMPORTANT]
> 客户端构建**必须**携带 `-tags with_utls` 标签。Reality 协议及带 TLS 指纹的出站必须依赖 sing-box 的 uTLS 实现支持；`make build` 默认已启用该标签。

## 运行服务端（Server）

首次启动必须指定初始管理员密码（默认用户名为 `admin`）。密码以 bcrypt 哈希形式安全存储于 SQLite 数据库中，后续启动无需再传入该环境变量。

```bash
SMALUX_ADMIN_PASSWORD='change-this-password' go run ./server -listen 0.0.0.0:8080 -db smalux-speedtest.db
```

1. 浏览器访问 `http://127.0.0.1:8080`，使用初始账号登录。
2. 进入管理控制台创建 Client，并妥善保存仅显示一次的 Client Token。
3. **多管理员体系**：首个 `admin` 账户为唯一的最高权限管理员（Owner）。Owner 可在控制台生成一次性、有时效的邀请码，其他管理员通过邀请注册页自主设置用户名与密码。普通管理员仅可查看、编辑与使用自己创建的 Client 和测速任务；Owner 享有全局管理权限。
4. **测速配置**：支持设置候选测速节点数、Top N 筛选以及测速并发线程数（1~32 线程，默认 4）。测速完成后可在任务详情页导出 CSV 数据或渲染下载精美 PNG 报告图。

## Docker 容器运行

项目提供发布至 GitHub Container Registry（`ghcr.io/smagicalk/smalux-speedtest`）的 Alpine 轻量容器镜像，通过 `MODE` 环境变量自由切换服务端与客户端。

### 1. 服务端模式 (Server)

```bash
docker run -d \
  --name smalux-server \
  --restart unless-stopped \
  -p 8080:8080 \
  -v ./data:/data \
  -e MODE=server \
  -e SMALUX_ADMIN_PASSWORD='change-this-password' \
  ghcr.io/smagicalk/smalux-speedtest:latest
```

* 首次启动需设置 `SMALUX_ADMIN_PASSWORD` 初始化管理员账户；后续数据库文件存在时无需再次提供。
* 容器默认监听 `0.0.0.0:8080`，数据库持久化保存在 `/data/smalux-speedtest.db`。
* 若在测试环境需允许远程明文 `ws://` 客户端，可添加环境变量 `-e SMALUX_ALLOW_INSECURE_WS=true`。

### 2. 客户端模式 (Client)

```bash
docker run -d \
  --name smalux-client \
  --restart unless-stopped \
  -e MODE=client \
  -e SMALUX_SERVER_URL='wss://speed.example.com/ws/client' \
  -e SMALUX_CLIENT_TOKEN='CLIENT_TOKEN' \
  ghcr.io/smagicalk/smalux-speedtest:latest
```

* `SMALUX_CLIENT_TOKEN` 为客户端必填项，由服务端控制台创建 Client 后生成。
* `SMALUX_SERVER_URL` 为服务端 WebSocket 通信地址（支持 `wss://.../ws/client` 或 `ws://.../ws/client`；若输入形如 `http://<IP>:8080`，启动脚本会自动补全并转换为对应 `ws://` 端点）。

### 容器环境变量说明

| 环境变量 | 默认值 | 适用模式 | 说明 |
| --- | --- | --- | --- |
| `MODE` | `server` | 通用 | 运行模式：`server`（服务端）或 `client`（客户端） |
| `SMALUX_ADMIN_PASSWORD` | 无 | 服务端 | 初始管理员登录密码（仅首次建库启动必填） |
| `SMALUX_LISTEN` | `0.0.0.0:8080` | 服务端 | 服务端 HTTP/WebSocket 监听地址 |
| `SMALUX_DATABASE` | `/data/smalux-speedtest.db` | 服务端 | SQLite 数据库存储路径 |
| `SMALUX_ALLOW_INSECURE_WS` | `false` | 服务端 | 是否允许远程明文 `ws://` 客户端连接（公网建议保持 false） |
| `SMALUX_SERVER_URL` | 无 | 客户端 | 服务端 WebSocket 通信地址（必填，兼容 `SMALUX_SERVER_ADDR`） |
| `SMALUX_CLIENT_TOKEN` | 无 | 客户端 | 客户端测速鉴权 Token（客户端必填） |
| `TZ` | `Asia/Shanghai` | 通用 | 容器时区 |

## 运行客户端（Client）

```bash
SMALUX_CLIENT_TOKEN='CLIENT_TOKEN' go run -tags with_utls ./client \
  -server ws://127.0.0.1:8080/ws/client
```

* **凭据安全**：优先从环境变量 `SMALUX_CLIENT_TOKEN` 读取，避免敏感 Token 暴露在进程命令行或系统进程列表中。
* **配置轻量**：客户端仅需提供服务端 WebSocket 地址与 Token，客户端名称、标签、并发线程与测速参数均由服务端统一维护与按任务动态下发。
* **重新授权（Re-authorize）**：由于服务端仅存储 Token 哈希，当迁移机器或遗失 Token 时，可在 Web 控制台点击“重新授权”生成新 Token，旧 Token 立即失效，原有 Client ID、历史测速数据保持不变。
* **Linux systemd 一键安装**：Linux 服务器可直接运行 `scripts/smalux.sh`，支持交互式安装/升级、架构自动识别、SHA256 校验与 systemd 服务管理。

## Telegram Bot 联动（可选）

通过 Telegram @BotFather 创建机器人后，使用最高权限管理员（Owner）登录 Web 控制台的**系统设置**页面，输入 Bot Token 与纯数字 Telegram Owner ID 进行绑定。服务端自动向机器人发送 6 位验证码，在网页端输入确认后即可完成动态绑定，整个过程无需重启服务端，亦无需将明文 Token 写入启动参数。

Bot Token 采用本地随机生成的强密钥加密保存在 SQLite 中（密钥保存在同级 `.bot-key` 文件中且权限受限），既不在日志中输出，亦不回显到前端。

### Bot 命令支持

| 命令 | 权限 | 用途说明 |
| --- | --- | --- |
| `/id` | 所有人 | 查看发送者 User ID 与当前 Chat ID |
| `/authorize USER_ID` | Owner | 授权指定的 Telegram 用户使用测速能力 |
| `/revoke USER_ID` | Owner | 撤销用户的测速授权（Owner 不可被撤销） |
| `/users` | Owner | 列出当前所有已授权用户 |
| `/test PROXY_TEXT` | 已授权 | 提交单条、多行代理链接或 Base64 订阅文本 |
| `/sub URL` | 已授权 | 安全拉取 HTTP(S) 订阅链接并提交测速 |
| `/cancel` | 已授权 | 取消当前用户正在运行中的测速任务 |

已授权用户亦可直接在私聊窗口发送节点链接。机器人支持交互式向导，支持分页多选在线客户端、选择候选节点数、Top N 与线程数，测速期间实时编辑进度消息，测速结束后直接回复精美的 PNG 图片报告。

## 任务调度机制

* **协议版本 2（Protocol Version 2）**：将任务批次拆分为单个代理工作单元。
* **双重租约（Lease）公平调度**：
  * **客户端租约（Client Lease）**：单台客户端同一时刻只执行一个代理的测速，避免本地带宽竞争干扰测试准确度。
  * **代理租约（Proxy Lease）**：同一代理 Outbound 在所有活动任务间全局排他，防止不同客户端并发测速把同一个节点带宽打满。
* **内存态调度**：任务 Outbound 配置仅保留在内存调度器中，调度完成后自动执行内存擦除，服务端重启后不会恢复包含明文凭据的活动任务。
* **传输安全**：服务端默认拒绝未经 TLS 加密的远程明文 `ws://` 客户端连接；公网部署务必搭配反向代理配置 WSS。

## 支持的代理协议

解析导入器与客户端原生支持以下 11 类代理分享链接：

| 协议族 | 识别协议 Scheme / 格式 | 核心特性支持与说明 |
| --- | --- | --- |
| **Shadowsocks** | `ss://` | SIP002 Base64 或 URL 百分号编码明文凭据、旧版全段 Base64、`plugin` 插件选项 |
| **VMess** | `vmess://Base64(JSON)` | Security/alterId、Packet Encoding、TLS/uTLS，以及下列 V2Ray 传输层 |
| **VLESS** | `vless://` | Flow (XTLS)、Packet Encoding、TLS/uTLS、Reality，以及下列 V2Ray 传输层 |
| **Trojan** | `trojan://` | 默认 TLS、TLS/uTLS，以及下列 V2Ray 传输层 |
| **SOCKS5** | `socks://`, `socks5://` | 支持可选的用户名/密码认证；两者均映射为标准 SOCKS5 |
| **HTTP(S) 代理** | `http://`, `https://` | 支持可选的用户名/密码；`https://` 启用上游 TLS 传输安全 |
| **SSH** | `ssh://` | 用户名/密码认证 |
| **AnyTLS** | `anytls://` | 密码认证与强制 TLS 传输 |
| **Hysteria v1** | `hysteria://` | Auth 凭据、带宽上下行声明、XPlus 混淆、TLS 与端口跳跃（Port Hopping） |
| **Hysteria2** | `hysteria2://`, `hy2://` | 密码、带宽限制、Salamander 混淆、TLS 与端口跳跃 |
| **TUIC** | `tuic://` | UUID/密码、TLS、拥塞控制算法与 UDP Relay 模式 |

* **传输层（Transport）**：VMess、VLESS、Trojan 支持标准 TCP（默认）、WebSocket（支持 `ed`/`eh`）、gRPC（支持 `serviceName`）、HTTP/H2 与 HTTPUpgrade。不支持的传输方式会在导入阶段明确拦截并提示，杜绝静默降级。
* **设计限制**：SSR 因已被 sing-box 1.13 移除故不支持；Naive 因内置 Cronet 库体积过大未引入；KCP 与 V2Ray QUIC 在客户端中未预置。Hysteria、Hysteria2、TUIC 使用各自的原生 QUIC 实现，不受此限。

## 隐私与安全边界

1. **零凭据落盘**：原始分享链接、订阅链接与正文、转换后的完整 sing-box outbound 绝不写入 SQLite 数据库，仅在测速期间留存于内存中，并在测试完毕后尽快覆写清空。
2. **脱敏存储**：历史测速结果仅保留脱敏后的网络节点信息（脱敏主机名与端口）、清洗后的节点显示名称、协议类型、公开测速点信息与测速指标。
3. **结构化日志安全脱敏（`logsafe`）**：引入日志清洗管道，自动过滤报错信息中包含的代理链接、Telegram Bot Token、Bearer Token、URL 查询参数凭据及操作系统本地私有路径；同时**完整保留网络超时、连接被拒、端口冲突、HTTP 状态码等关键根因诊断文本**，兼顾隐私安全与排障可用性。
4. **防 SSRF 保护**：远程订阅获取模块内嵌严格的私网 IP/回环 IP 过滤与 DNS 复验机制，单次订阅正文硬上限限制为 5 MiB。

## 构建发布流水线

* **多平台二进制发布**：`.github/workflows/release.yml` 在推送 `v*` Tag 时自动运行单元测试，为 Linux、Windows、macOS（amd64 / arm64）编译发布六个平台的完整二进制包与 `SHA256SUMS` 校验清单。
* **Docker 镜像自动打包**：`.github/workflows/docker-publish.yml` 在 Release 发布或手动触发时自动打包 Alpine 轻量多模式镜像，并推送至 GitHub Container Registry（`ghcr.io/smagicalk/smalux-speedtest`）。

## 代码架构布局

| 目录 / 文件 | 模块职责说明 |
| --- | --- |
| `server/`、`client/` | 服务端与客户端可执行入口、命令行参数解析与进程信号生命周期管理 |
| `internal/clientapp` | 客户端 WebSocket 长连接维护、工作单元 Worker、sing-box 实例生命周期与测速执行器 |
| `internal/importer` | 订阅安全解码与各类代理协议 URI 的标准化映射与解析 |
| `internal/model` | 跨端业务模型、Assignment 实体、脱敏规则与敏感内存覆写 |
| `internal/wire` | WebSocket 应用层 Envelope 封包协议与消息尺寸边界限制 |
| `internal/logsafe` | 日志结构化错误安全脱敏：清洗代理节点与凭据，保留真实诊断根因 |
| `internal/serverapp` | HTTP API 路由、Web 认证、Session/CSRF、WebSocket Hub 任务调度与 SSE 事件总线 |
| `internal/serverapp/web` | 内嵌前端管理后台页面（原生 ES Module）、实时测速视图与客户端管理面板 |
| `internal/store` | SQLite 数据库驱动、版本迁移事务、Client/用户凭据哈希与脱敏结果持久化 |
| `internal/subscription` | 防 SSRF 安全远程订阅抓取客户端（带 DNS 复验与 5 MiB 严格限流） |
| `internal/telegrambot` | Telegram Bot API 交互、交互向导、命令分发与消息编辑重试队列 |
| `internal/reportpng` | 服务端轻量有界 PNG 测速结果报表渲染引擎 |
| `Dockerfile`、`entrypoint.sh` | Alpine 轻量容器镜像构建脚本与智能参数标准化装配启动脚本 |
| `scripts/smalux.sh` | Linux systemd 一键安装、服务启停与在线平滑升级运维脚本 |
