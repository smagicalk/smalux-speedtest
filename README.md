# Smalux Speedtest

Distributed proxy speed testing with a central server and remote clients. The server dispatches one-time proxy configurations over WebSocket; clients test public Speedtest.net endpoints through an embedded sing-box outbound.

## Build

```bash
make build
```

Windows client:

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -buildvcs=false -o smalux-client.exe ./client
```

## Run the server

Set the administrator password on the first start. The password hash is kept in SQLite, so later starts do not require the environment variable.

```bash
SMALUX_ADMIN_PASSWORD='change-this-password' go run ./server -listen :8080 -db smalux-speedtest.db
```

Open `http://127.0.0.1:8080`, create a client, and retain the token shown once.

Each task can use 1-32 concurrent speedtest connections (4 by default). Completed results can be exported as CSV or as a branded PNG report from the task detail page.

## Run a client

```bash
go run ./client \
  -server ws://127.0.0.1:8080/ws/client \
  -token CLIENT_TOKEN \
  -name shanghai-01 \
  -labels region=cn-east,provider=example
```

For production, terminate TLS at a reverse proxy and configure clients with a `wss://` URL.

Supported URI families are Shadowsocks, VMess, VLESS, Trojan, SOCKS5, HTTP, SSH, AnyTLS, Hysteria, Hysteria2 and TUIC. Naive is intentionally excluded because sing-box embeds large per-platform Cronet libraries for that outbound.
