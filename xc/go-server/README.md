# syncclipboard-server-xc (Go edition)

A from-scratch, single-binary **Go re-implementation of the SyncClipboard server**
(`SyncClipboard.Server`), built for very low memory usage. It is a drop-in
replacement: same HTTP API, same SignalR hub protocol (WebSockets), same SQLite
schema, same on-disk layout — official clients cannot tell the difference.

## Why

The official server is an ASP.NET Core app (~150-160 MB RSS at idle). On a 1 GB
VPS hosting other services this is a significant share of RAM. This rewrite runs
in ~5-10 MB RSS and uses a fraction of the CPU.

## Layout

```
go-server/
  main.go            entrypoint, router, cleanup loop
  config_auth.go     config loading + HTTP Basic auth
  model.go           protocol models (DTOs, enums, time formats)
  store.go           SQLite access (modernc.org/sqlite, no CGO)
  sync.go            /SyncClipboard.json, /file/* handlers
  history.go         /api/history/* handlers (multipart, query, data, patch)
  hub.go             minimal SignalR JSON-protocol server (negotiate + WS)
  cmd/synccheck/     protocol validation battery (http | hub | write modes)
```

## Build

```
go build -o syncclipboard-server-xc .
```

## Run

```
syncclipboard-server-xc \
  -data /path/to/data/server \
  -config /path/to/data/appsettings.json \
  -listen 127.0.0.1:5033
```

`-data` points at the server data root (the directory that contains
`SyncClipboard.json`, `file/`, `history/`), i.e. the same directory the official
container mounts as `/app/data/server`.

## Protocol notes (validated against official master / v3.3.1)

- Basic auth on every route; 401 with `WWW-Authenticate: Basic realm="SyncClipboard"`.
- `/api/version` returns the bare version string; `/api/time` returns a quoted
  DateTimeOffset string.
- `profileId` format `Type-Hash` (case-sensitive type parsing).
- `TransferDataFile` / `FilePaths` are stored relative to the record's working
  directory (`history/{Type}_{Hash}/`).
- `PUT /SyncClipboard.json` creates history records with `Version = 0`;
  `POST /api/history` update bumping matches official `HistoryHelper.ShouldUpdate`.
- MVC-style `ProblemDetails` bodies for 404 (with W3C-style traceId) and 422
  (`history_data_invalid`), byte-compatible with the official server.
- `/api/history/{id}/data` emits `X-SyncClipboard-Transfer-Data-Hash` with a
  freshly computed SHA-256 (exact header casing preserved).
- SignalR hub: `negotiate` + WebSockets transport, `{}` handshake, type-6
  keepalive pings every 10 s, `RemoteProfileChanged`/`RemoteHistoryChanged`
  broadcasts, 0x1e record separator.

Set `XcServerVersion` in appsettings.json (or `SYNCCLIPBOARD_SERVER_VERSION`
env) to override the reported `/api/version`.

## synccheck

```
synccheck -mode http  -base http://127.0.0.1:5033 -user U -pass P   # read-only battery
synccheck -mode hub   -base http://127.0.0.1:5033 -user U -pass P   # SignalR connect test
synccheck -mode write -base http://127.0.0.1:5033 -user U -pass P   # write paths (test copies only!)
```

`http` mode is safe against production; `write` mode creates/patch records and
must only be used against throwaway copies.

## Container image

CI (`.github/workflows/xc-go-server-image.yml`) builds and publishes the image
on every push under `xc/go-server/`:

```
ghcr.io/xiaochen301/syncclipboard-server-xc:latest
ghcr.io/xiaochen301/syncclipboard-server-xc:sha-<commit12>
```

Static binary on alpine: ~39 MB pulled, ~10 MB RSS at runtime. Pull-and-run
(mirrors the official compose layout — `/app/data`, `/app/appsettings.json`):

```
docker run -d \
  --name syncclipboard-server \
  -p 127.0.0.1:5033:5033 \
  -v ./data/appsettings.json:/app/appsettings.json \
  -v ./data:/app/data \
  ghcr.io/xiaochen301/syncclipboard-server-xc:latest
```

Default container args: `-data /app/data/server -config /app/appsettings.json
-listen 0.0.0.0:5033` (override via `command:`). Deploy-side upgrade is just
`docker compose pull && docker compose up -d`.
