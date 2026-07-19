// Package serverapp 实现 Smalux Speedtest 服务端的控制平面。
//
// 服务端同时面向两类调用方：
//   - 管理员通过 HTML 页面和 JSON API 创建 Client、下发任务并查看结果；
//   - 分布在各地的 Client 通过带 Bearer Token 的 WebSocket 长连接领取任务、
//     上报进度和测速结果。
//
// App 负责 HTTP 路由、管理员会话、CSRF 防护、订阅抓取和持久化入口；Hub
// 负责 WebSocket 连接、运行中任务状态机以及向浏览器 SSE 订阅者广播实时事件。
// 两者共同使用 store.Store 持久化 Client 元数据、任务摘要、目标状态和测速结果。
//
// 代理链接中可能包含密码、UUID、私钥等敏感字段。createTask 解析出的完整
// model.Assignment 只交给 Hub 保存在进程内存，并只发送给被选中的 Client；数据库
// 只记录任务统计信息和脱敏后的测速结果。任务终态聚合后，Hub 会清空代理切片并删除
// 对应运行态。因此服务重启会丢失尚未完成任务的内存配置，这正是当前“不持久化代理
// 凭据”安全边界带来的明确取舍。
package serverapp
