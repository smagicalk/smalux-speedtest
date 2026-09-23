# Smalux Speedtest

[English](README_EN.md) | [中文说明](README.md)

Distributed proxy speed testing with a central server and remote clients. The server dispatches one-time proxy configurations over WebSocket; clients test public Speedtest.net endpoints through an embedded sing-box outbound.

Project documentation: [`docs/index.html`](docs/index.html). It includes the [runtime call flow](docs/index.html#call-flow), [extension recipes](docs/index.html#extension), and [local development and test workflow](docs/index.html#testing). When GitHub Pages is configured to publish from the branch's `/docs` directory, the same file is the documentation home page.

## Build

```bash
make build
```

Windows client:

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -buildvcs=false -tags with_utls -o smalux-client.exe ./client
```

The client must be built with the `with_utls` tag. Reality and TLS fingerprint outbounds require this sing-box feature; `make build` enables it by default.

## Run the server

Set the administrator password on the first start. The initial username is `admin`. Passwords are stored only as bcrypt hashes in SQLite, so later starts do not require the environment variable.

```bash
SMALUX_ADMIN_PASSWORD='change-this-password' go run ./server -listen 0.0.0.0:8080 -db smalux-speedtest.db
```

Open `http://127.0.0.1:8080`, log in, create a client, and retain the token shown once. The first `admin` account is the unique highest-privilege administrator. Instead of assigning passwords to other people, the Owner generates single-use, expiring invitation codes in the dashboard; a recipient chooses their own username and password on the invitation registration page. The Owner can disable, re-enable, and delete additional administrator accounts.

Ordinary administrators can see every Client as read-only operational status, but can edit, revoke, and use only the Clients they created; the highest-privilege administrator can manage all Clients. Task lists, details, live events, exports, and cancellation follow the same ownership boundary: ordinary administrators can operate only their own tasks, while the Owner can operate all tasks. Client display names and labels can be edited from the dashboard without rotating their tokens. Every administrator can open **Account security** from their username in the top bar and change their own password after confirming the current password. A successful password change immediately invalidates that account's other browser sessions.

Task candidate count, Top N, and thread count use bounded dashboard selectors. Each task can use 1-32 concurrent speedtest connections (4 by default). The detail page shows each target Client's execution state and a count-only summary when invalid proxy lines were skipped; proxy text and import details are not persisted. Completed results can be exported as CSV or as a branded PNG report from the task detail page.

### Docker Deployment

The project provides a multi-mode Alpine container image published to GitHub Container Registry (`ghcr.io/smagicalk/smalux-speedtest`).

#### 1. Run Server (Server Mode)

```bash
docker run -d \
  --name smalux-server \
  --restart unless-stopped \
  -p 8080:8080 \
  -v ./data:/data \
  -e MODE=server \
  -e SMALUX_ADMIN_PASSWORD='change-this-password' \
  ghcr.io/smagicalk/smalux-speedtest:latest
```

* `SMALUX_ADMIN_PASSWORD` is required on the first start to bootstrap the `admin` account. After SQLite database is initialized, restarting the container does not require this variable.
* The container listens on `0.0.0.0:8080` by default and stores database at `/data/smalux-speedtest.db`.
* In trusted internal test environments where clients connect via unencrypted `ws://`, add `-e SMALUX_ALLOW_INSECURE_WS=true`.

#### 2. Run Client (Client Mode)

```bash
docker run -d \
  --name smalux-client \
  --restart unless-stopped \
  -e MODE=client \
  -e SMALUX_SERVER_URL='wss://speed.example.com/ws/client' \
  -e SMALUX_CLIENT_TOKEN='CLIENT_TOKEN' \
  ghcr.io/smagicalk/smalux-speedtest:latest
```

* `SMALUX_CLIENT_TOKEN` is mandatory and obtained from the server dashboard when creating a client.
* `SMALUX_SERVER_URL` is the WebSocket endpoint URL (`wss://.../ws/client` or `ws://.../ws/client`). If an `http://<ip>:8080` address is supplied, the entrypoint script will automatically convert it.

#### Environment Variables

| Variable | Default | Mode | Description |
| --- | --- | --- | --- |
| `MODE` | `server` | Universal | Running mode: `server` or `client` |
| `SMALUX_ADMIN_PASSWORD` | None | Server | Admin password (required on initial start only) |
| `SMALUX_LISTEN` | `0.0.0.0:8080` | Server | HTTP/WebSocket listen address |
| `SMALUX_DATABASE` | `/data/smalux-speedtest.db` | Server | SQLite database path |
| `SMALUX_ALLOW_INSECURE_WS` | `false` | Server | Allow remote plaintext `ws://` client connections |
| `SMALUX_SERVER_URL` | None | Client | Server WebSocket URL (mandatory for client, alias `SMALUX_SERVER_ADDR`) |
| `SMALUX_CLIENT_TOKEN` | None | Client | Client authentication token (mandatory for client) |
| `TZ` | `Asia/Shanghai` | Universal | Container timezone |

## Telegram Bot (optional)

Create a bot with BotFather, open **System settings** as the highest-privilege web administrator, and bind it with the Bot Token and numeric Telegram owner ID. The server validates the token with `getMe`, sends a six-digit code through that Bot, and saves the binding only after the code is confirmed in the web page. Send `/start` to the new Bot before requesting the code.

The settings page starts, stops, rebinds, and removes the Bot without restarting the Server. Only the unique highest-privilege administrator can access these controls. The Token is encrypted in SQLite with a randomly generated local key stored beside the database with owner-only permissions; neither the Token nor key is returned to the browser or written to logs. Back up the database and its `.bot-key` file together. If the key is missing or invalid, the speedtest service still starts and the settings page allows the Owner to bind the Bot again.

The Telegram owner is synchronized into SQLite when the binding is confirmed and is the only Telegram user allowed to change the Bot authorization list. The Bot processes private text chats only. Legacy `SMALUX_TELEGRAM_BOT_TOKEN` plus `-telegram-owner-id` startup configuration remains available for existing deployments, but is not needed for a web-managed Bot.

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

Remote subscription bodies are limited to 5 MiB. Under protocol version 2, each encoded single-proxy WebSocket work unit is limited to 2 MiB and is checked before a task is stored. The aggregate imported batch may be larger because it remains only in Server memory and is dispatched one proxy at a time.

The authorization list and the acknowledged `getUpdates` offset are stored in SQLite. The offset is committed before processing each update, so a server restart does not replay an already acknowledged high-bandwidth test request; if the process exits in that narrow window, the sender can submit the message again.

The server uses an embedded portable font for reports. Set `SMALUX_REPORT_FONT` to a local TTF, OTF, or TTC file when CJK node names must render with their original glyphs on a minimal Linux host.

`SMALUX_TELEGRAM_API_BASE_URL` may point to a self-hosted Bot API Server. Remote endpoints must use HTTPS; plaintext HTTP is accepted only for loopback addresses because the Bot Token is part of the API request path.

## Run a client

```bash
SMALUX_CLIENT_TOKEN='CLIENT_TOKEN' go run -tags with_utls ./client \
  -server ws://127.0.0.1:8080/ws/client
```

`SMALUX_CLIENT_TOKEN` is the preferred token source and takes precedence when it is non-empty. The `-token` flag remains only as a compatibility fallback; command-line secrets may be visible in process listings and should not be used for new deployments.

The Client only needs the Server WebSocket URL and its one-time token. Its display name and labels are maintained in the Server dashboard; task candidate count, transfer-server selection, and thread count are sent with each assignment.

Client tokens cannot be read back because the Server stores only their hashes. When moving a Client to another host or replacing a lost token, use **Re-authorize** in the dashboard. The replacement token is displayed once, the old token becomes invalid immediately, and the Client ID, labels, task history, and result associations remain unchanged. Update the new host's `SMALUX_CLIENT_TOKEN` and restart that Client.

For a Linux systemd installation, run `scripts/smalux.sh` as root. The interactive menu can install or update a Server/Client, show service status, and uninstall while optionally preserving the database; the `start` and `stop` subcommands control service state without changing boot enablement. It detects amd64/arm64, downloads the matching latest Release archive, verifies `SHA256SUMS`, stores service configuration under `/etc/smalux-speedtest`, and never puts the Client Token or administrator password in an `ExecStart` argument.

## Work scheduling

Protocol version 2 dispatches one proxy work unit to each Client at a time. The Server keeps an in-memory lease for both the Client and an ephemeral HMAC fingerprint of the proxy outbound: an idle Client immediately receives another available proxy, while the same proxy is never tested by two Clients concurrently. Every selected Client still tests every proxy, so regional result comparison remains complete. Server and Client must be upgraded together when moving from protocol version 1.

The scheduler applies across all active tasks, not only within one task. A Client lease prevents overlapping speed tests from competing for that machine's bandwidth, while a proxy lease prevents different Clients from testing the same outbound simultaneously. Different available proxies can still run in parallel on different Clients. Work completion, failure, cancellation, timeout, token revocation, disconnect, and connection replacement all release their leases; a reconnect resumes only unfinished proxy units for that Client.

The scheduler and proxy fingerprints are process-local. Raw outbounds and fingerprints are not persisted, and restarting the Server does not reconstruct active work because tasks containing proxy credentials intentionally exist only in memory.

The Server rejects remote plaintext `ws://` Clients by default. For temporary use on a trusted network, start it with `-allow-insecure-ws`; the Client will then authenticate with its Bearer Token over plaintext WebSocket. Public deployments must use WSS because plain WS exposes the Token, assignments, results, packet sizes, timing, and endpoint addresses to the network path.

## Supported proxy protocols

The importer and the production Client currently support these 11 share-link families:

| Protocol | Accepted URI schemes/formats | Imported features and notes |
| --- | --- | --- |
| Shadowsocks | `ss://` | SIP002 Base64 or percent-encoded plain credentials, legacy whole-authority Base64, and `plugin` options |
| VMess | `vmess://Base64(JSON)` | Security/alter ID, packet encoding, TLS/uTLS, and the V2Ray transports listed below |
| VLESS | `vless://` | Flow, packet encoding, TLS/uTLS, Reality, and the V2Ray transports listed below |
| Trojan | `trojan://` | TLS by default, TLS/uTLS, and the V2Ray transports listed below |
| SOCKS5 | `socks://`, `socks5://` | Optional username/password authentication; both schemes become SOCKS version 5 |
| HTTP proxy | `http://`, `https://` | Optional username/password; `https://` enables TLS to the upstream proxy |
| SSH | `ssh://` | Username/password authentication |
| AnyTLS | `anytls://` | Password authentication and mandatory TLS |
| Hysteria v1 | `hysteria://` | Auth, required upload/download bandwidth, XPlus obfuscation, TLS, and port hopping |
| Hysteria2 | `hysteria2://`, `hy2://` | Password, optional bandwidth, Salamander-style obfuscation, TLS, and port hopping |
| TUIC | `tuic://` | UUID/password, TLS, congestion control, and UDP relay mode |

For VMess, VLESS, and Trojan, supported V2Ray transports are default TCP (`tcp`, `raw`, `none`, or omitted), WebSocket, gRPC, HTTP/H2, and HTTPUpgrade. Unsupported transports are rejected during import instead of silently falling back to TCP. In particular, KCP and V2Ray QUIC are not enabled by the production Client build. This does not affect Hysteria, Hysteria2, or TUIC, whose native QUIC implementations are registered separately.

SSR is rejected because support was removed from sing-box 1.13. Naive is intentionally excluded because sing-box embeds large per-platform Cronet libraries for that outbound. Other URI families are reported as unsupported.

## Privacy boundary

Raw share links, subscription URLs and bodies, and normalized sing-box outbound JSON are never written to SQLite. They remain in task memory only while dispatching and running a test (at most 10 minutes), then their writable outbound buffers are overwritten on a best-effort basis. Client tokens are stored only as hashes.

Historical results retain an ordinary user-provided node display name, protocol, a fully redacted address with only its port, selected public Speedtest.net server, measurements, and fixed error categories. Names shaped like a URI, IP/host, path, JSON, UUID, token, or credential are replaced with an anonymous protocol label; links without a display name use the same fallback. Human-readable names such as a region, provider, or site identifier remain unchanged.

Structured error logs are automatically sanitized by `logsafe`: sensitive proxy configurations, tokens, passwords, query credentials, and local system paths are masked, while essential network error reasons, socket states, HTTP/WebSocket status codes, and configuration hints are preserved for reliable troubleshooting. Existing databases are scrubbed once on upgrade: normal node names remain, sensitive-looking names become anonymous protocol labels, and old address fragments, invalid timestamps, untrusted Speedtest metadata, and arbitrary task/result errors are removed. The legacy single-admin hash is migrated into the administrator table and its duplicate setting is deleted. The upgrade then rewrites SQLite and truncates its WAL to purge obsolete local pages; separately managed backups are outside this process.

The complete outbound must still travel from Server to an authorized Client. Public deployments should use HTTPS/WSS and separately protect the administrator web interface. Proxy links sent to the optional Telegram Bot also remain subject to Telegram's own message-retention policy; use the authenticated web interface when that is unsuitable.

For temporary trusted-network testing only, start the Server with `-allow-insecure-ws` (or `SMALUX_ALLOW_INSECURE_WS=true`) to accept remote `ws://` Clients. This sends the Client Token and all task traffic without transport encryption. Never enable it across an untrusted or public network; use WSS instead.

## Release builds

* **Release Pipeline** (`.github/workflows/release.yml`): Triggers only on `v*` tag pushes or published GitHub Releases. Automatically runs unit tests, compiles Server and the uTLS-enabled Client for six platform targets (Linux, Windows, macOS on amd64 / arm64), generates `SHA256SUMS`, and publishes to GitHub Releases.
* **Docker Build Pipeline** (`.github/workflows/docker-publish.yml`): Triggers automatically after `release.yml` completes successfully (or via manual `workflow_dispatch`), downloads the published Linux binary, packages the multi-mode Alpine Docker image, and pushes to GitHub Container Registry (`ghcr.io/smagicalk/smalux-speedtest`).
* **Test & Build Pipeline** (`.github/workflows/test.yml`): Triggered manually (`workflow_dispatch`) to test any branch or commit, optionally run tests, cross-compile for all or specific operating systems and architectures, and produce downloadable Actions artifacts without polluting Releases.

## Source layout

| Module | Responsibility |
| --- | --- |
| `client/`, `server/` | Small executable entry points and process lifecycle |
| `internal/clientapp` | WebSocket connection, task worker, speedtest execution and sing-box runtime |
| `internal/importer` | Subscription decoding and protocol-specific URI normalization |
| `internal/model`, `internal/wire`, `internal/logsafe` | Shared domain models, WebSocket envelopes and sanitized diagnostic error log fields |
| `internal/serverapp` | HTTP routes, authentication, task APIs, WebSocket hub and SSE events |
| `internal/store` | SQLite migration, Client credentials, tasks and result persistence |
| `internal/subscription` | SSRF-protected remote subscription fetching |
| `internal/telegrambot` | Telegram Bot API transport, authorization commands and task replies |
| `internal/reportpng` | Bounded server-side PNG report rendering |
| `internal/serverapp/web` | Embedded dashboard, live task view and PNG report module |
| `Dockerfile`, `entrypoint.sh` | Multi-mode Alpine container packaging, environment configuration and startup parameter normalization |
