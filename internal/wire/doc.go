// Package wire 定义 Smalux Speedtest 客户端与服务端之间的 WebSocket 消息封装。
//
// 每个 WebSocket 文本帧承载一个 Envelope。Envelope 提供协议版本、消息类型、消息 ID
// 和可选任务 ID，具体业务对象则延迟编码在 Payload 中。接收方应先检查 Version 和
// Type，再使用 Decode 解出该消息类型对应的 model 类型。
//
// 典型连接生命周期为：client.hello -> server.welcome；连接建立后双方通过 ping/pong
// 保活，服务端通过 task.assign/task.cancel 控制任务，客户端依次回传 task.ack、
// task.progress、task.result，并最终发送 task.complete 或 task.failed。
package wire
