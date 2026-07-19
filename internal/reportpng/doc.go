// Package reportpng 在服务端把测速任务及其结果渲染为可下载的 PNG 表格报告。
//
// 渲染器不依赖浏览器、CGO 或操作系统图形接口。它使用固定列宽、受限行数和纯 Go
// OpenType 字体绘制，因此适合静态编译后的服务端进程。报告中的代理地址来自已经脱敏的
// model.SpeedResult.MaskedAddress；调用方不应把原始代理配置传入本包。
//
// 默认内嵌 Go 字体保证任何平台都能生成图片。为改善中文及其他 Unicode 字符覆盖率，
// 渲染器会优先读取 SMALUX_REPORT_FONT 指定的 TTF/OTF/TTC 文件，并尝试常见系统字体；
// 缺失字形最终以问号替代。所有截断均按 Unicode rune 而不是 UTF-8 字节执行，不会生成
// 损坏的文本。
package reportpng
