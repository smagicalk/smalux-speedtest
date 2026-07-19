// Package model 定义客户端、服务端、导入器、持久化层和 WebSocket 协议共同使用的
// 领域数据结构。
//
// 这里的类型以跨进程 JSON 传输为主要约束：字段使用明确的 JSON 名称，时间戳使用
// RFC 3339 字符串，速率统一使用 bit/s，延迟统一使用毫秒。ProxySpec.Outbound 是
// sing-box 原始出站配置，可能包含认证信息，只应存在于受信任的任务下发链路中；
// 持久化或展示测速结果时应使用 MaskedAddress，而不是泄露原始节点地址。
package model
