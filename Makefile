.PHONY: build test clean

# VERSION 通过链接参数写入两个可执行文件；发布流水线可覆盖为标签或提交号。
VERSION ?= dev
# BUILD_DIR 和 CGO_ENABLED 均可由命令行覆盖，默认生成便于分发的纯 Go 二进制。
BUILD_DIR ?= dist
CGO_ENABLED ?= 0
LDFLAGS := -s -w -X main.version=$(VERSION)

# 同一 Go module 的两个入口分别生成服务端和远程 Client。
build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/smalux-server ./server
	CGO_ENABLED=$(CGO_ENABLED) go build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/smalux-client ./client

# -buildvcs=false 允许在源码归档或独立工作树中执行测试。
test:
	go test -buildvcs=false ./...

# 只删除可再生的构建产物，不触碰 SQLite 运行数据。
clean:
	rm -rf $(BUILD_DIR)
