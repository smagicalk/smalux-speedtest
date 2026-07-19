// Package store 封装服务端的 SQLite 持久化层。
//
// 数据库只保存控制面运行所需的信息：管理员密码哈希、Client 元数据与令牌哈希、
// 任务状态、目标 Client 状态，以及已经脱敏的测速结果。代理分享链接、协议凭据和
// sing-box outbound JSON 不应进入该包，任务中的敏感配置只在服务端内存与受认证的
// WebSocket 下发链路中流转。
//
// Store 将 SQLite 连接池限制为一个连接。这既避免同一进程内的高并发写入争锁，也
// 保证 foreign_keys、busy_timeout 等连接级 PRAGMA 对所有查询一致生效。跨多条 SQL
// 且要求原子性的操作（例如同时创建任务与任务目标）仍显式使用事务。
package store
