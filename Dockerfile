FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata

COPY smalux-server /usr/local/bin/smalux-server
COPY smalux-client /usr/local/bin/smalux-client
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

RUN chmod +x /usr/local/bin/smalux-server \
             /usr/local/bin/smalux-client \
             /usr/local/bin/entrypoint.sh

WORKDIR /data
VOLUME ["/data"]

# 预设默认环境变量
ENV MODE=server \
    TZ=Asia/Shanghai \
    SMALUX_LISTEN=0.0.0.0:8080 \
    SMALUX_DATABASE=/data/smalux-speedtest.db

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
