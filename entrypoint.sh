#!/bin/sh
set -e

# ==============================================================================
# Smalux Speedtest Docker 启动入口脚本
# 支持环境变量模式控制：MODE=server（默认）或 MODE=client
# 参考文档：https://smagicalk.github.io/smalux-speedtest/#quick-start
# ==============================================================================

if [ "$MODE" = "client" ]; then
    # --------------------------------------------------------------------------
    # 客户端模式 (Client Mode)
    # --------------------------------------------------------------------------
    TARGET_SERVER="${SMALUX_SERVER_URL:-${SMALUX_SERVER_ADDR:-$SMALUX_SERVER}}"

    if [ -z "$TARGET_SERVER" ]; then
        echo "======================================================================"
        echo "[Smalux Client 错误] 必须设置服务端 WebSocket 地址环境变量 SMALUX_SERVER_URL！"
        echo "运行示例："
        echo "  docker run --rm \\"
        echo "    -e MODE=client \\"
        echo "    -e SMALUX_SERVER_URL=wss://speed.example.com/ws/client \\"
        echo "    -e SMALUX_CLIENT_TOKEN=your_client_token \\"
        echo "    ghcr.io/smagicalk/smalux-speedtest:latest"
        echo "======================================================================"
        exit 1
    fi

    # 规范化服务端通信协议与端点路径
    case "$TARGET_SERVER" in
        http://*)
            TARGET_SERVER="ws://${TARGET_SERVER#http://}"
            ;;
        https://*)
            TARGET_SERVER="wss://${TARGET_SERVER#https://}"
            ;;
        ws://*|wss://*)
            ;;
        *)
            case "$TARGET_SERVER" in
                *:443|*:443/*)
                    TARGET_SERVER="wss://${TARGET_SERVER}"
                    ;;
                *)
                    TARGET_SERVER="ws://${TARGET_SERVER}"
                    ;;
            esac
            ;;
    esac

    case "$TARGET_SERVER" in
        */ws/client)
            ;;
        */)
            TARGET_SERVER="${TARGET_SERVER}ws/client"
            ;;
        *)
            TARGET_SERVER="${TARGET_SERVER}/ws/client"
            ;;
    esac

    if [ -z "$SMALUX_CLIENT_TOKEN" ]; then
        echo "======================================================================"
        echo "[Smalux Client 错误] 必须设置客户端鉴权 Token 环境变量 SMALUX_CLIENT_TOKEN！"
        echo "说明：请在服务端 Web 控制台创建 Client 并获取 Token。"
        echo "运行示例："
        echo "  docker run --rm \\"
        echo "    -e MODE=client \\"
        echo "    -e SMALUX_SERVER_URL=${TARGET_SERVER} \\"
        echo "    -e SMALUX_CLIENT_TOKEN=your_client_token \\"
        echo "    ghcr.io/smagicalk/smalux-speedtest:latest"
        echo "======================================================================"
        exit 1
    fi

    export SMALUX_SERVER_URL="$TARGET_SERVER"
    export SMALUX_CLIENT_TOKEN="$SMALUX_CLIENT_TOKEN"

    echo "[Smalux Client] 正在启动测速客户端..."
    echo "[Smalux Client] 连接服务端: $TARGET_SERVER"
    exec smalux-client -server "$TARGET_SERVER" "$@"
fi

# ------------------------------------------------------------------------------
# 服务端模式 (Server Mode，默认)
# ------------------------------------------------------------------------------
LISTEN_ADDR="${SMALUX_LISTEN:-0.0.0.0:8080}"
DB_PATH="${SMALUX_DATABASE:-/data/smalux-speedtest.db}"

# 首次启动必须初始化管理员密码；若数据库文件已存在则不强制要求密码环境变量
if [ ! -f "$DB_PATH" ] && [ -z "$SMALUX_ADMIN_PASSWORD" ]; then
    echo "======================================================================"
    echo "[Smalux Server 错误] 首次启动必须设置管理员密码环境变量 SMALUX_ADMIN_PASSWORD！"
    echo "运行示例："
    echo "  docker run -d --name smalux-server -p 8080:8080 \\"
    echo "    -v ./data:/data \\"
    echo "    -e MODE=server \\"
    echo "    -e SMALUX_ADMIN_PASSWORD=your_secure_password \\"
    echo "    ghcr.io/smagicalk/smalux-speedtest:latest"
    echo "======================================================================"
    exit 1
fi

SERVER_ARGS="-listen $LISTEN_ADDR -db $DB_PATH"

if [ "$SMALUX_ALLOW_INSECURE_WS" = "true" ] || [ "$SMALUX_ALLOW_INSECURE_WS" = "1" ]; then
    SERVER_ARGS="$SERVER_ARGS -allow-insecure-ws"
fi

export SMALUX_LISTEN="$LISTEN_ADDR"
export SMALUX_DATABASE="$DB_PATH"

echo "[Smalux Server] 正在启动测速服务端..."
echo "[Smalux Server] 监听地址: $LISTEN_ADDR"
echo "[Smalux Server] 数据库路径: $DB_PATH"
exec smalux-server $SERVER_ARGS "$@"
