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

## Telegram Bot (optional)

Create a bot with BotFather, keep its token out of the command line, and start the server with the numeric Telegram user ID that will own authorization management:

```bash
SMALUX_ADMIN_PASSWORD='change-this-password' \
SMALUX_TELEGRAM_BOT_TOKEN='123456:bot-token' \
go run ./server \
  -listen :8080 \
  -db smalux-speedtest.db \
  -telegram-owner-id 123456789
```

Both the token and owner ID must be configured, otherwise the server refuses to start. The owner is synchronized into SQLite on every start and is the only user allowed to change the Bot authorization list. The Bot processes private text chats only.

| Command | Access | Purpose |
| --- | --- | --- |
| `/id` | Everyone | Show the user and chat IDs needed for authorization |
| `/authorize USER_ID` | Owner | Authorize a Telegram user |
| `/revoke USER_ID` | Owner | Revoke a user; the configured owner cannot be revoked |
| `/users` | Owner | List authorized users |
| `/test PROXY_TEXT` | Authorized | Submit one proxy, multiple lines, or Base64 subscription content |
| `/sub URL` | Authorized | Fetch and submit an HTTP(S) subscription URL |
| `/cancel` | Authorized | Cancel the sender's active task |

Authorized users may also send proxy text directly without `/test`. A plain `http://` or `https://` line is treated as an HTTP proxy, so remote subscriptions must use `/sub`. Each Telegram user can have one active task; Bot tasks target all currently online and enabled clients and use 10 candidates, Top 3 transfer servers, and 4 speedtest threads. Completed, partially completed, and failed tasks return a PNG report to the originating private chat. Transient Telegram upload failures are retried with a fixed bound.

Remote subscription bodies are limited to 5 MiB. The final encoded WebSocket assignment is limited to 2 MiB for protocol-version-1 client compatibility and is checked before a task is stored; split unusually large batches when the server reports that the benchmark batch is too large.

The authorization list and the acknowledged `getUpdates` offset are stored in SQLite. The offset is committed before processing each update, so a server restart does not replay an already acknowledged high-bandwidth test request; if the process exits in that narrow window, the sender can submit the message again.

The server uses an embedded portable font for reports. Set `SMALUX_REPORT_FONT` to a local TTF, OTF, or TTC file when CJK node names must render with their original glyphs on a minimal Linux host.

`SMALUX_TELEGRAM_API_BASE_URL` may point to a self-hosted Bot API Server. Remote endpoints must use HTTPS; plaintext HTTP is accepted only for loopback addresses because the Bot Token is part of the API request path.

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

## Source layout

| Module | Responsibility |
| --- | --- |
| `client/`, `server/` | Small executable entry points and process lifecycle |
| `internal/clientapp` | WebSocket connection, task worker, speedtest execution and sing-box runtime |
| `internal/importer` | Subscription decoding and protocol-specific URI normalization |
| `internal/model`, `internal/wire` | Shared domain models and WebSocket envelopes |
| `internal/serverapp` | HTTP routes, authentication, task APIs, WebSocket hub and SSE events |
| `internal/store` | SQLite migration, Client credentials, tasks and result persistence |
| `internal/subscription` | SSRF-protected remote subscription fetching |
| `internal/telegrambot` | Telegram Bot API transport, authorization commands and task replies |
| `internal/reportpng` | Bounded server-side PNG report rendering |
| `internal/serverapp/web` | Embedded dashboard, live task view and PNG report module |
