# Smalux Speedtest

Distributed proxy speed testing with a central server and remote clients. The server dispatches one-time proxy configurations over WebSocket; clients test public Speedtest.net endpoints through an embedded sing-box outbound.

## Build

```bash
make build
```

Windows client:

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -buildvcs=false -tags with_utls -o smalux-client.exe ./client
```

The client must be built with the `with_utls` tag. Reality and TLS fingerprint
outbounds require this sing-box feature; `make build` enables it by default.

## Run the server

Set the administrator password on the first start. The initial username is `admin`. Passwords are stored only as bcrypt hashes in SQLite, so later starts do not require the environment variable.

```bash
SMALUX_ADMIN_PASSWORD='change-this-password' go run ./server -listen 127.0.0.1:8080 -db smalux-speedtest.db
```

Open `http://127.0.0.1:8080`, log in, create a client, and retain the token shown once. The dashboard can create, disable, re-enable, and delete additional administrator accounts. It prevents the current session from disabling or deleting itself and always preserves at least one enabled administrator.

Each task can use 1-32 concurrent speedtest connections (4 by default). Completed results can be exported as CSV or as a branded PNG report from the task detail page.

## Telegram Bot (optional)

Create a bot with BotFather, keep its token out of the command line, and start the server with the numeric Telegram user ID that will own authorization management:

```bash
SMALUX_ADMIN_PASSWORD='change-this-password' \
SMALUX_TELEGRAM_BOT_TOKEN='123456:bot-token' \
go run ./server \
  -listen 127.0.0.1:8080 \
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

Authorized users may also send proxy text directly without `/test`. A plain `http://` or `https://` line is treated as an HTTP proxy, so remote subscriptions must use `/sub`. Each Telegram user can have one active task. Guided tasks provide a paginated multi-select for currently online and enabled Clients; quick submissions target all available Clients and use 10 candidates, Top 3 transfer servers, and 4 speedtest threads. Completed, partially completed, and failed tasks return a PNG report to the originating private chat. Transient Telegram upload failures are retried with a fixed bound.

On startup the Bot registers its command menu with Telegram. `/start`, `/help`, and the inline menu expose a guided flow for proxy text or subscription URLs, paginated Client multi-selection, candidate server count, Top N transfer servers, and speedtest threads. Direct proxy text, `/test <text>`, and `/sub <URL>` remain available as default-parameter shortcuts. One control message replies to the original proxy/subscription request and is edited in place through configuration and throttled aggregate/per-Client progress. After the final PNG is sent as a reply to the same request, the temporary control message is removed.

Remote subscription bodies are limited to 5 MiB. The final encoded WebSocket assignment is limited to 2 MiB for protocol-version-1 client compatibility and is checked before a task is stored; split unusually large batches when the server reports that the benchmark batch is too large.

The authorization list and the acknowledged `getUpdates` offset are stored in SQLite. The offset is committed before processing each update, so a server restart does not replay an already acknowledged high-bandwidth test request; if the process exits in that narrow window, the sender can submit the message again.

The server uses an embedded portable font for reports. Set `SMALUX_REPORT_FONT` to a local TTF, OTF, or TTC file when CJK node names must render with their original glyphs on a minimal Linux host.

`SMALUX_TELEGRAM_API_BASE_URL` may point to a self-hosted Bot API Server. Remote endpoints must use HTTPS; plaintext HTTP is accepted only for loopback addresses because the Bot Token is part of the API request path.

## Run a client

```bash
SMALUX_CLIENT_TOKEN='CLIENT_TOKEN' go run -tags with_utls ./client \
  -server ws://127.0.0.1:8080/ws/client \
  -name shanghai-01 \
  -labels region=cn-east,provider=example
```

`SMALUX_CLIENT_TOKEN` is the preferred token source and takes precedence when it is non-empty. The `-token` flag remains only as a compatibility fallback; command-line secrets may be visible in process listings and should not be used for new deployments.

For production, terminate TLS at a same-host reverse proxy and configure clients with a `wss://` URL. Both sides reject remote plaintext WebSocket: the Client accepts `ws://` only for `localhost` or literal loopback IP addresses, and the Server upgrades a non-TLS connection only when its direct peer is loopback. A reverse proxy on another host must use a TLS or loopback tunnel for its backend connection.

Supported URI families are Shadowsocks, VMess, VLESS, Trojan, SOCKS5, HTTP, SSH, AnyTLS, Hysteria, Hysteria2 and TUIC. Naive is intentionally excluded because sing-box embeds large per-platform Cronet libraries for that outbound.

## Privacy boundary

Raw share links, subscription URLs and bodies, and normalized sing-box outbound JSON are never written to SQLite. They remain in task memory only while dispatching and running a test (at most 10 minutes), then their writable outbound buffers are overwritten on a best-effort basis. Client tokens are stored only as hashes.

Historical results retain an ordinary user-provided node display name, protocol, a fully redacted address with only its port, selected public Speedtest.net server, measurements, and fixed error categories. Names shaped like a URI, IP/host, path, JSON, UUID, token, or credential are replaced with an anonymous protocol label; links without a display name use the same fallback. Human-readable names such as a region, provider, or site identifier remain unchanged.

Logs omit proxy configuration, request bodies, subscription URLs, raw connection errors, client names, remote client addresses, listener addresses, and local database paths. Existing databases are scrubbed once on upgrade: normal node names remain, sensitive-looking names become anonymous protocol labels, and old address fragments, invalid timestamps, untrusted Speedtest metadata, and arbitrary task/result errors are removed. The legacy single-admin hash is migrated into the administrator table and its duplicate setting is deleted. The upgrade then rewrites SQLite and truncates its WAL to purge obsolete local pages; separately managed backups are outside this process.

The complete outbound must still travel from Server to an authorized Client. Public deployments must use HTTPS/WSS and protect administrator and Client credentials. Proxy links sent to the optional Telegram Bot also remain subject to Telegram's own message-retention policy; use the authenticated web interface when that is unsuitable.

## Release builds

`.github/workflows/release.yml` runs automatically for pushes to `main` and for `v*` tag pushes. A `main` push builds downloadable Actions artifacts for integration testing. A tag such as `v1.0.0` builds Server and the uTLS-enabled Client for Linux, Windows, and macOS on amd64 and arm64, creates the matching GitHub Release when necessary, then uploads six archives plus `SHA256SUMS`. Publishing a Release from the GitHub UI is also supported.

The same workflow can be started manually from GitHub Actions with a branch, tag, or commit and a version. With an empty `release_tag`, packages are kept only as a downloadable Actions artifact for testing. Supplying an existing `release_tag` builds that exact tested tag and uploads to the matching Release. All third-party Actions are pinned to full commits.

## Source layout

| Module | Responsibility |
| --- | --- |
| `client/`, `server/` | Small executable entry points and process lifecycle |
| `internal/clientapp` | WebSocket connection, task worker, speedtest execution and sing-box runtime |
| `internal/importer` | Subscription decoding and protocol-specific URI normalization |
| `internal/model`, `internal/wire`, `internal/logsafe` | Shared domain models, WebSocket envelopes and message-free error log fields |
| `internal/serverapp` | HTTP routes, authentication, task APIs, WebSocket hub and SSE events |
| `internal/store` | SQLite migration, Client credentials, tasks and result persistence |
| `internal/subscription` | SSRF-protected remote subscription fetching |
| `internal/telegrambot` | Telegram Bot API transport, authorization commands and task replies |
| `internal/reportpng` | Bounded server-side PNG report rendering |
| `internal/serverapp/web` | Embedded dashboard, live task view and PNG report module |
