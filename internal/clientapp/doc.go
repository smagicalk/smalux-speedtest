// Package clientapp 实现 Smalux Speedtest 的常驻测速客户端。
//
// 客户端包含两条相互独立的数据路径：控制面通过 WebSocket 完成身份认证、任务下发、
// 取消、心跳和结果回传；测速数据面则为每个代理启动一个最小 sing-box 实例，并让
// speedtest-go 的 HTTP 请求经该代理出站。这样服务端只负责调度和展示，不接触实际
// 测速流量。
//
// 每条 WebSocket 连接只有一个读取循环和一个串行任务 worker。worker 同一时间最多
// 执行一个任务，任务内的代理也按顺序测试，从而限制单个客户端的资源占用。心跳与
// worker 可能并发写连接，因此所有 WebSocket 写入都由 connection.send 统一串行化。
// 上层 context 被取消、连接断开或服务端发送 task.cancel 时，当前测速 context 会被
// 取消并继续向 sing-box、HTTP 请求及 speedtest-go 传播。
package clientapp
