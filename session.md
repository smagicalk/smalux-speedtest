# Session Handoff

更新时间：2026-07-26（Asia/Shanghai）

## 当前状态

- 分支：`vibe-dev`
- 本轮开始时工作树干净。
- 最近完成了代理分享链接兼容性扩展、运行时边界测试和协议文档更新。
- Server/Client 应用层协议仍为 v2；生产 Client 使用 `with_utls` 构建标签。

## 已完成工作

### 分享链接导入

- VLESS 和 VMess 支持 `packet_encoding`。
- VMess、VLESS、Trojan 共用的 V2Ray transport 支持：
  - 默认 TCP：省略 `type`，或使用 `tcp`、`raw`、`none`。
  - WebSocket，包括 early data 的 `ed` / `eh` 别名。
  - gRPC，包括 `serviceName` / `service_name` 别名。
  - HTTP/H2 和 HTTPUpgrade。
- 未实现的 transport 会在导入阶段明确拒绝，不再静默退化成 TCP。
- Hysteria v1 支持查询参数认证、必填上下行带宽、XPlus 混淆和端口跳跃。
- Hysteria2 支持认证别名、可选带宽、混淆和端口跳跃。
- Hysteria 分享链接的 `start-end` 端口范围会转换成 sing-box 所需的 `start:end`，并在导入时验证。
- Shadowsocks 支持 SIP002 明文百分号转义，同时避免对 Base64 解码后的密码再次做百分号解码。

### 生产能力边界

当前支持 11 类分享链接：

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

限制：

- KCP 和 V2Ray QUIC transport 当前不支持，并在导入时拒绝。
- 生产 Client 仅使用 `with_utls`，没有注册需要 `with_quic` 的 V2Ray QUIC 构造器。
- Hysteria、Hysteria2、TUIC 使用各自已注册的原生 QUIC 实现，不受上述限制。
- SSR 已被 sing-box 1.13 移除，因此明确拒绝。
- Naive 未注册，原因是会引入体积较大的多平台 Cronet 运行库。

### 测试与文档

- `internal/importer/protocol_conversion_test.go` 覆盖协议字段转换、非法 transport、Hysteria 带宽/端口范围及 Shadowsocks 编码边界。
- `internal/clientapp/imported_outbounds_test.go` 使用项目真实 sing-box 注册表初始化导入结果，验证 importer 到 Client 的边界。
- `README.md` 与 `docs/index.html` 已加入完整协议矩阵、transport 能力和不支持项。

## 验证记录

本轮通过的主要检查：

    go test -p=1 -count=1 ./...
    go test -p=1 -count=1 -tags with_utls ./...
    go test -race -count=3 ./internal/importer ./internal/clientapp
    go build ./...
    go vet ./...
    git diff --check

此外，importer 和关键 sing-box outbound 初始化测试曾连续运行 10 轮通过。

## 已知事项

`TestTelegramLifecycleSynchronizesOwnerAndStopsPolling` 存在既有低概率时序失败：Shutdown 后请求计数偶尔多 1。该问题已在不包含本轮 importer 修改的原始 `HEAD` 临时副本中复现，因此不是协议导入改动引入。最终提交前的常规和 `with_utls` 全仓测试均通过。

## 相关提交

- `5357055 feat: improve proxy share link compatibility`
- `96f33d9 docs: document supported proxy protocols`

## 后续建议

- 若要支持 V2Ray QUIC，需要同时修改生产构建标签、注册 `transport/v2rayquic`，并评估所有发布平台的体积与运行兼容性；不能只放宽 importer。
- 若处理 Telegram flaky test，应围绕 polling goroutine 的退出确认和 Shutdown 后请求计数同步单独修复，不与 importer 变更混合。
- 新增协议或 transport 时，应同步更新 importer 白名单、`minimalBoxContext` 注册表、真实 outbound 初始化测试、README 和 docs。
