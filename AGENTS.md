# AGENTS.md

This file provides guidance to WARP (warp.dev) when working with code in this repository.

## Project Overview

Orchids-2api is a Go API proxy server providing multi-account management and load balancing for Claude API compatible requests. It proxies requests to the Orchids backend service with features like weighted load balancing, failover, SSE streaming, and tool call support.

## Build & Run Commands

```bash
# Install dependencies
go mod download

# Development mode (hot reload not supported - must restart manually)
go run ./cmd/server/main.go -config ./config_dev.json

# Production build (embeds web/* static files and templates)
go build -o orchids-server ./cmd/server

# Run production binary
./orchids-server -config ./config.json

# Run tests
go test ./...

# Run single test file
go test ./internal/tiktoken/...

# Run specific test
go test ./internal/handler/... -run TestHandlerName
```

## Critical Build Note

**Static files (`web/static/`, `web/templates/`) are embedded at compile time via Go embed.** After modifying any frontend files, you MUST rebuild the binary:

```bash
go build -o orchids-server ./cmd/server && ./orchids-server -config ./config.json
```

## Architecture

### Request Flow
```
Client (Claude API format) → Handler → LoadBalancer → PromptBuilder → Clerk Auth → Upstream Client → Orchids Server
                                                                                              ↓
                                                                     SSE Response ← Format Conversion
```

### Key Packages

- `cmd/server/main.go` - Application entry point, HTTP server setup, route registration
- `internal/handler/` - Main request handlers for `/orchids/v1/messages` and `/warp/v1/messages`
- `internal/loadbalancer/` - Weighted random account selection with failover support
- `internal/orchids/` - Orchids upstream client (SSE and WebSocket modes)
- `internal/warp/` - Warp upstream client
- `internal/upstream/` - Common upstream components (connection pool, circuit breaker, reliability)
- `internal/clerk/` - Clerk authentication service for JWT token generation
- `internal/store/` - Redis-based account and settings storage
- `internal/prompt/` - Converts Claude API messages to Markdown format for upstream
- `internal/api/` - Admin REST API for account management
- `internal/middleware/` - Auth middleware, concurrency limiter
- `internal/tiktoken/` - Token counting estimation
- `internal/summarycache/` - Conversation summary caching (memory or Redis)
- `web/` - Embedded static files and templates for admin UI

### Data Storage

Storage is Redis-only (`store_mode: "redis"`). Key entities:
- **Account**: Contains Clerk credentials (ClientCookie, SessionID), load balancing weight, request counts
- **Settings**: Key-value configuration store

### Configuration

Config is loaded from `config.json` or `config.yaml` (flat structure only). Use `-config` flag to specify path. Key settings:
- `port` - Server port (default: 3002)
- `redis_addr` / `redis_prefix` - Redis connection
- `admin_path` - Admin UI path (e.g., `/admin`)
- `upstream_mode` - `sse` or `ws` for upstream communication
- `orchids_ws_url` - WebSocket endpoint for upstream
- `orchids_allow_run_command` / `orchids_run_allowlist` - Command execution controls

## API Endpoints

- `POST /orchids/v1/messages` - Claude API proxy (main endpoint)
- `POST /warp/v1/messages` - Warp-specific Claude API proxy
- `GET/POST /api/accounts` - Account management (requires session auth)
- `GET /health` - Health check
- `GET /metrics` - Prometheus metrics

## Code Patterns

- HTTP handlers use standard `net/http` patterns (no framework)
- Logging via `log/slog` with JSON handler
- Context propagation for cancellation and timeouts
- Graceful shutdown handling in main.go

## Language

This codebase uses Chinese for documentation, comments, and log messages. Continue this convention.
