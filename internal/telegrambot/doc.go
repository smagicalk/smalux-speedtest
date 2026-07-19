// Package telegrambot 实现 Smalux Speedtest 的 Telegram Bot 传输层与任务编排。
//
// 本包刻意不依赖 serverapp、store 或具体图片实现。Bot 通过四个小接口协作：API 封装
// Telegram Bot HTTP API，AuthorizationManager 判断发送者权限并管理授权名单，Runner 把代理或订阅
// 请求接入现有任务系统并等待终态，Renderer 将已完成任务转换为可发送的 PNG 等图片。
// 这种边界允许服务端在组合根中提供适配器，也允许单元测试完全使用内存假实现。
//
// HTTPAPI 使用 getUpdates 长轮询接收 message 更新，使用 sendMessage 和 sendPhoto 回传。
// Bot Token 只用于构造 Telegram API URL，不会进入业务消息或日志。用户提交的代理文本
// 可能包含密码、UUID、私钥等秘密，本包只把它交给 Runner，不记录、不回显，也不会存入
// Telegram 确认消息；其持久化策略由 Runner 适配的服务端任务系统继续负责。
package telegrambot
