# Orchids-2api

[中文](README.md) | [English](README_EN.md)

A Go-based multi-channel proxy that exposes Claude Messages style and OpenAI-compatible APIs across three upstream channels: `warp`, `puter`, and `grok`.

## Current Status

- `internal/handler` serves `warp` / `puter` for both `/v1/messages` and `/v1/chat/completions`
- `internal/grok` handles Grok Messages, Responses, Chat, image, video, speech, and local media endpoints
- per-channel model sync is available through `POST /api/models/refresh`
- Puter non-stream Claude Messages regressions are covered for `Read`, `Write`, `Edit`, `Delete`, long-context, and multi-round `tool_result`

## Core Features

- multi-account pools with per-channel load balancing
- Claude Messages compatible endpoints
- OpenAI Chat Completions compatible endpoints
- model management, default model selection, and sorting
- admin UI and admin API
- Redis-backed persistence
- Prometheus metrics and optional `pprof`
- Grok image generation/editing, Console video generation/edit/extension, Console TTS/STT/Realtime, and local media caching
- API-key authentication by default, per-key model/RPM/expiration policies, and AES-GCM encryption for persisted account credentials

## Supported Channels

| Channel | Public routes |
|---|---|
| `warp` | `/warp/v1/messages`, `/warp/v1/chat/completions` |
| `puter` | `/puter/v1/messages`, `/puter/v1/chat/completions` |
| `grok` | `/grok/v1/messages`, `/grok/v1/responses`, `/grok/v1/chat/completions`, image, video, speech, and file routes |

Unified model lookup:

- `GET /v1/models`
- `GET /v1/models/{id}`

Standard Grok video routes include `/v1/videos/generations`, `/v1/videos/edits`, and `/v1/videos/extensions`; the legacy Web app-chat generator remains available at `/v1/videos`. Video jobs are isolated by API key and their metadata is persisted in Redis for one hour. Standard Console jobs that already obtained an upstream `request_id` resume polling with the original account after restart. A renewable 30-second Redis lease allows only one worker across instances and permits takeover after expiry. `media_dir` can be a shared filesystem mount. Multi-replica mode requires Redis, a unique `deployment_instance_id` per replica, one common `deployment_cluster_id`, and `shared_media=true`; startup validates a cluster marker and read/write access before workers start. `/v1/media/inputs` accepts temporary image/video uploads up to 20 MiB and returns an API-key-scoped `file_id` valid for 24 hours. Grok speech routes include native `/v1/tts`, `/v1/stt`, `/v1/realtime`, plus OpenAI-compatible `/v1/audio/speech`, `/v1/audio/tasks`, and `/v1/audio/transcriptions`. Transcriptions support `json`, `verbose_json`, and `text`; parameters that cannot be represented by Console STT are rejected explicitly.

## Documentation

- [Architecture](docs/architecture.md)
- [API Reference](docs/api-reference.md)
- [Configuration](docs/configuration.md)
- [Deployment](docs/deployment.md)
- [Grok parity checklist](docs/grok2api-parity-checklist.md)

Historical reviews and fix logs remain available in Git history. The topic guides above describe current behavior.

## Requirements

- Go `1.24+`
- Redis `7+`
- Windows / Linux / macOS

## Quick Start

### 1. Start Redis

```bash
docker run -d --name orchids-redis -p 6379:6379 redis:7
```

### 2. Create `config.json`

Copy the safe example first (`config.json` is not tracked by Git):

```bash
cp config.example.json config.json
```

```json
{
  "port": "3002",
  "store_mode": "redis",
  "redis_addr": "127.0.0.1:6379",
  "admin_user": "admin",
  "admin_pass": "",
  "admin_path": "/admin",
  "inference_auth_enabled": true,
  "credential_encryption_key_file": "data/credential.key",
  "response_store_ttl_hours": 720,
  "debug_enabled": false
}
```

Notes:

- if `admin_pass` is omitted, the server generates a random password at startup and prints it to logs
- in production, set a strong `admin_pass` explicitly and keep `debug_enabled` set to `false`
- if Redis already contains `settings:config`, that stored config overrides the file on boot
- the first start creates `data/credential.key`; persist and back it up with Redis, because losing it makes stored account credentials unreadable
- create an API key in the admin UI and send it as `Authorization: Bearer <API Key>`; Anthropic SDKs may use `x-api-key`

### 3. Start the server

Development:

```bash
go run ./cmd/server -config ./config.json
```

Production:

```bash
go build -o orchids-server ./cmd/server
./orchids-server -config ./config.json
```

Windows:

```powershell
go build -o server.exe ./cmd/server
.\server.exe -config .\config.json
```

## Common Commands

Run all tests:

```bash
go test ./...
```

Run Puter-specific regressions:

```bash
go test ./internal/handler -run "Puter_"
```

Rebuild:

```bash
go build -o orchids-server ./cmd/server
```

Basic health checks:

```bash
curl -s http://127.0.0.1:3002/health
curl -s http://127.0.0.1:3002/v1/models -H 'Authorization: Bearer sk-...'
```

## Model Sync Behavior

- endpoint: `POST /api/models/refresh`
- example body: `{"channel":"puter"}`
- sync is source-driven; Puter additionally verifies official-catalog candidates with account `test_mode`
- newly discovered models are inserted
- locally stored models missing from the source are deleted
- `verified` reports the number of models accepted into the synced set for that run

## Puter Notes

- requests use native `tools`, assistant `tool_calls`, and `role: tool` history instead of prompt-level `<tool_call>` emulation
- streams are decoded by native `text`, `reasoning`, `tool_use`, `usage`, and `error` event type
- `/puter/v1/messages` non-stream responses preserve `tool_use` content blocks
- `tool_result` follow-ups can either continue the tool chain or converge to final text
- regressions are covered for `Read`, `Write`, `Edit`, `Delete`, long context, and multi-round `tool_result`

## Admin

- UI: `{admin_path}/`
- login: `POST /api/login`
- account management: `/api/accounts*`
- model management: `/api/models*`
- config management: `/api/config*`
- token cache: `/api/token-cache/*`

Auth methods:

- `session_token` cookie
- `Authorization: Bearer <admin_token>`
- `X-Admin-Token: <admin_token>`
- Basic Auth with password equal to `admin_pass`

Model and inference endpoints require a managed API key by default. Each key can restrict models, requests per minute, and expiration; existing keys remain unrestricted unless configured. Set `inference_auth_enabled=false` only behind a trusted authenticating gateway.

Grok Build Responses also supports `POST /v1/responses/compact` and `GET`/`DELETE /v1/responses/{response_id}`. Stored response ownership is isolated by client API key, pinned to the creating OAuth account, and retained for 720 hours by default.

## License

This repository follows the existing license policy already used in the repo.
