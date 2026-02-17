# Grok2API Parity Checklist (Round 2)

Date: 2026-02-17
Reference: `chenyme/grok2api` README + API behavior (`/v1/chat/completions`, `/v1/images/generations`, `/v1/images/edits`, `/v1/models`, `/v1/files/*`)

## Endpoint Compatibility

- [x] Added alias: `POST /v1/chat/completions` -> Grok handler
- [x] Added alias: `POST /v1/images/generations` -> Grok handler
- [x] Added alias: `POST /v1/images/edits` -> Grok handler
- [x] Added alias: `GET /v1/files/*` -> Grok files handler
- [x] File path parser now accepts both `/grok/v1/files/*` and `/v1/files/*`

## Chat Request Parity

- [x] `reasoning_effort` validation aligned (`none|minimal|low|medium|high|xhigh`)
- [x] `temperature` range validation aligned (0~2)
- [x] `top_p` range validation aligned (0~1)
- [x] default `temperature=0.8` when omitted
- [x] default `top_p=0.95` when omitted
- [x] multimodal block validation aligned (`text|image_url|input_audio|file`)
- [x] media payload validation aligned (URL/Data URI, reject bare base64)
- [x] image edit in chat requires `image_url`
- [x] image edit in chat now uses last provided image (matches grok2api behavior)
- [x] video model now supports image-to-video via `image_url` attachment

## Images API Parity

- [x] `/images/generations` enforces model semantic:
  - `The model \`grok-imagine-1.0\` is required for image generation.`
- [x] `/images/edits` enforces model semantic:
  - `The model \`grok-imagine-1.0-edit\` is required for image edits.`
- [x] size validation aligned (`1280x720|720x1280|1792x1024|1024x1792|1024x1024`)
- [x] stream + n validation aligned (`stream` only when `n=1|2`)
- [x] image edit file constraints aligned (required, max 16, max 50MB, mime whitelist)
- [x] response format alias aligned (`base64` -> `b64_json`)

## Error Semantics

- [x] model-not-found messaging aligned for chat/model checks:
  - `The model \`<id>\` does not exist or you do not have access to it.`
- [x] image-generation / image-edit model error messages aligned with grok2api text style

## Admin NSFW Parity

- [x] Added `POST /api/v1/admin/tokens/nsfw/enable` (sync)
- [x] Added `POST /api/v1/admin/tokens/nsfw/enable/async` (async task start)
- [x] Added `GET /api/v1/admin/batch/{task_id}/stream` (SSE progress stream)
- [x] Added `POST /api/v1/admin/batch/{task_id}/cancel` (task cancel)
- [x] NSFW results now carry grok2api-style fields:
  - `success`, `http_status`, `grpc_status`, `grpc_message`, `error`
- [x] SSE snapshot payload now includes explicit `type: "snapshot"` marker
- [x] Batch snapshot payload now includes `processed` alias (for grok2api cache/token JS compatibility)

## Admin Cache Parity

- [x] Added `POST /api/v1/admin/cache/online/clear/async` (and `/v1/admin/...` alias)
- [x] Added `POST /api/v1/admin/cache/online/load/async` (and `/v1/admin/...` alias)
- [x] Async cache tasks now expose final `result` payload in batch SSE final event
- [x] `/cache/list` default type now aligns to `image` when no type is provided
- [x] `/cache/clear` default type now aligns to `image` when no type is provided
- [x] `/cache/online/clear` behavior now aligns:
  - `tokens[]` -> batch clear response (`status: success`, `results`)
  - `token` or no-token fallback -> single clear semantics
  - no-token fallback now selects one available token (not clear-all)
  - single-token failure now returns `status: error` with `error`

## Admin Frontend Cache Parity

- [x] `grok-tools` online cache actions now use async batch APIs:
  - load selected/all -> `/api/v1/admin/cache/online/load/async`
  - clear selected -> `/api/v1/admin/cache/online/clear/async`
- [x] Added batch progress UI and cancel button in `grok-tools`:
  - stream `/api/v1/admin/batch/{task_id}/stream`
  - cancel `/api/v1/admin/batch/{task_id}/cancel`

## Admin Config Alias Parity

- [x] Added `/api/v1/admin/config` and `/v1/admin/config` aliases to existing config GET/POST handler

## Admin Token Refresh Parity

- [x] Added `POST /api/v1/admin/tokens/refresh` (sync)
- [x] Added `POST /api/v1/admin/tokens/refresh/async` (async task start)
- [x] Sync refresh response aligned to `{"status":"success","results":{token:boolean}}`
- [x] Async refresh task reuses `/api/v1/admin/batch/{task_id}/stream|cancel`

## Admin Token CRUD Parity

- [x] Added `GET /api/v1/admin/tokens` (token pool view)
- [x] Added `POST /api/v1/admin/tokens` (token pool upsert/replace)
- [x] Added `/v1/admin/*` aliases for tokens/refresh/nsfw/batch endpoints
- [x] Added `GET /v1/admin/verify` and `GET /v1/admin/storage` compatibility endpoints
- [x] Session auth now accepts grok2api-style `Authorization: Bearer <app_key>` and `?app_key=` (SSE)

## Public API Parity

- [x] Added `GET /v1/public/verify` (and `/api/v1/public/verify`) compatibility endpoint
- [x] Added `GET /v1/public/voice/token` aliases to voice token handler
- [x] Added `imagine` public aliases:
  - `GET /v1/public/imagine/config`
  - `POST /v1/public/imagine/start`
  - `POST /v1/public/imagine/stop`
  - `GET /v1/public/imagine/sse`
  - `GET /v1/public/imagine/ws`
- [x] `public/imagine/start` now accepts optional `nsfw` and keeps it in session for SSE/WS generation flow
- [x] `public/imagine/config` values are now config-driven (not hardcoded):
  - `final_min_bytes` <- `image_final_min_bytes`
  - `medium_min_bytes` <- `image_medium_min_bytes`
  - `nsfw` <- `image_nsfw`
- [x] Added `video` public session endpoints:
  - `POST /v1/public/video/start`
  - `GET /v1/public/video/sse`
  - `POST /v1/public/video/stop`
- [x] `public/video/start` default `preset` now aligns to `normal`
- [x] Public auth semantics aligned to `public_key/public_enabled`:
  - added config key compatibility: `public_key` / `app_public_key` / `app.public_key`
  - added config key compatibility: `public_enabled` / `app_public_enabled` / `app.public_enabled`
  - `verify` / `voice/token` / `imagine start|stop` / `video start|stop` now use public-key auth (not admin session auth)
  - `imagine/config` and `video/sse` are no longer forced through admin/session auth
  - `imagine/sse|ws` support query `public_key` auth with `task_id` bypass behavior
  - project override: if `public_key` is empty, public API auth is disabled (no auth check)
- [x] Added public web entry routes aligned to grok2api behavior:
  - `/` redirects to `/login` when `public_enabled=true`, otherwise redirects to admin login
  - `/login` `/imagine` `/voice` `/video` pages are exposed only when public is enabled
  - static assets are available at `/static/public/*`

## Admin Page Path Parity

- [x] Added grok2api-style admin page aliases:
  - `/admin` -> `/admin/`
  - `/admin/login` -> `/admin/login.html`
  - `/admin/config` `/admin/cache` `/admin/token` -> authenticated admin index render

## Route Coverage Scan (Upstream `5936099`)

- [x] Re-scanned FastAPI route set from upstream and compared to `cmd/server/main.go`.
- [x] No missing API endpoints found at route layer (after prefix/wildcard normalization).

## Public Frontend Parity (Imagine/Video)

- [x] `public/imagine` interaction depth enhanced:
  - mode switch upgraded to `AUTO/WS/SSE` button group
  - added runtime status stats (`images`, `active`, `avg latency`, runtime mode)
  - added behavior toggles (`auto scroll`, `auto download`, `auto filter`, `reverse insert`) with local persistence
  - added optional File System API folder picker for save destination (`Select Save Folder`)
  - added batch selection workflow (`Batch`, `Select All/Clear All`, selected counter)
  - added batch download with ZIP packaging when `JSZip` is available (fallback to per-image download)
  - added image lightbox navigation (`prev/next/escape`) and keyboard support
  - added config-driven defaults bootstrap from `/v1/public/imagine/config` (`nsfw`, `final_min_bytes`)
  - running-session updates now auto-apply on key setting changes (`mode`, `ratio`, `nsfw`, `concurrent`) via controlled restart
- [x] `public/video` interaction depth enhanced:
  - added progress bar + indeterminate state + elapsed timer
  - added runtime metadata panel (`aspect`, `length`, `resolution`, `preset`)
  - added reference image local upload/clear flow (data URI conversion)
  - added preview card list (`open`/`download`) and raw stream panel separation
  - improved SSE delta parsing for progress text and video URL extraction (`<video>`, markdown link, file URL)
  - aligned SSE URL behavior to include `public_key` query when public key exists locally
- [x] `public/voice` interaction depth enhanced:
  - upgraded from token-only page to direct LiveKit session controls (`start`, `stop`)
  - added runtime status/meta panel, session logs, and audio track mount area
  - added activity visualizer for live session feedback
  - retained token/url visibility for diagnostics (`Fetch Token`)

## Request Decoding Compatibility

- [x] `chat/completions` now accepts loose JSON types for common fields:
  - `stream`: `true/false` and `"true"/"false"/"1"/"0"`
  - `temperature` / `top_p`: numeric string accepted
  - `video_config.video_length` and `image_config.n`: numeric string accepted
- [x] `images/generations` now accepts loose JSON types for:
  - `n` (numeric string)
  - `stream` (`"true"/"false"` style string)
  - `nsfw` (`true/false` and `"true"/"false"` style string)

## Stream Default Parity

- [x] Added global default stream behavior aligned to grok2api `app.stream`:
  - config keys: `stream` (default true), optional override `app_stream`
  - applies when `chat/completions` request omits `stream`
  - `stream: null` is treated as omitted (falls back to default)
  - explicit request `stream` always takes precedence
- [x] Added dot-key compatibility for grok2api-style config payloads:
  - `app.stream` (higher priority than `app_stream` / `stream`)
  - `image.nsfw`
  - `image.final_min_bytes`
  - `image.medium_min_bytes`

## Remaining Gaps (Intentional / Deferred)

- [ ] `/v1/models` still uses this project’s unified model listing logic (cross-channel capable), not strictly grok2api-only semantics.
- [ ] Error body format is still plain `http.Error` text in many paths (not fully migrated to a unified JSON error schema).
- [ ] Public frontend still has selective UX simplifications vs upstream (no toast system, lighter visual card system, fewer advanced toolbar interactions).
