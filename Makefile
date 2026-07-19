.PHONY: build test clean

VERSION ?= dev
BUILD_DIR ?= dist
CGO_ENABLED ?= 0
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/smalux-server ./server
	CGO_ENABLED=$(CGO_ENABLED) go build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/smalux-client ./client

test:
	go test -buildvcs=false ./...

clean:
	rm -rf $(BUILD_DIR)
