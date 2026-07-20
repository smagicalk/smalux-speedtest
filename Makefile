.PHONY: build build-server build-client test clean

# VERSION 通过链接参数写入两个可执行文件；发布流水线可覆盖为标签或提交号。
VERSION ?= dev
# BUILD_DIR 和 CGO_ENABLED 均可由命令行覆盖，默认生成便于分发的纯 Go 二进制。
BUILD_DIR ?= dist
CGO_ENABLED ?= 0
# Reality 和带 fingerprint 的 TLS 出站依赖 sing-box 的 uTLS 实现。该实现受构建标签
# 控制；遗漏标签时配置可以正常解析，却会在 Client 执行任务时才失败。
CLIENT_BUILD_TAGS ?= with_utls
LDFLAGS := -s -w -X main.version=$(VERSION)

# 同一 Go module 的两个入口分别生成服务端和远程 Client。
build: build-server build-client

build-server:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/smalux-server ./server

build-client:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build -buildvcs=false -trimpath -tags "$(CLIENT_BUILD_TAGS)" -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/smalux-client ./client

# 测试使用与发布 Client 相同的标签，确保 uTLS/Reality 实现本身也参与编译。
# -buildvcs=false 允许在源码归档或独立工作树中执行测试。
test:
	go test -buildvcs=false -tags "$(CLIENT_BUILD_TAGS)" ./...

# 只删除可再生的构建产物，不触碰 SQLite 运行数据。
clean:
	rm -rf $(BUILD_DIR)
