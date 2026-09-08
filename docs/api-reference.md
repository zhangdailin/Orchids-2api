# API 参考

本文档以 [routes.go](../cmd/server/routes.go) 和 [model_refresh.go](../cmd/server/model_refresh.go) 当前实现为准。

## 1. 公开接口

### 1.1 Claude Messages 风格

| 路径 | 方法 | 说明 |
|---|---|---|
| `/warp/v1/messages` | POST | Warp 通道 Claude Messages 代理 |
| `/puter/v1/messages` | POST | Puter 通道 Claude Messages 代理 |
| `/grok/v1/messages` | POST | Grok 通道 Anthropic Messages 兼容入口 |
| `/v1/messages` | POST | Grok Messages 兼容别名 |
| `/*/v1/messages/count_tokens` | POST | 输入 token 估算 |

### 1.2 OpenAI Chat Completions 风格

| 路径 | 方法 | 说明 |
|---|---|---|
| `/warp/v1/chat/completions` | POST | Warp OpenAI 兼容入口 |
| `/puter/v1/chat/completions` | POST | Puter OpenAI 兼容入口 |
| `/grok/v1/chat/completions` | POST | Grok OpenAI 兼容入口 |
| `/v1/chat/completions` | POST | Grok 兼容别名 |

### 1.3 OpenAI Responses 风格

| 路径 | 方法 | 说明 |
|---|---|---|
| `/grok/v1/responses`、`/v1/responses` | POST | 创建 Response；Build 原生支持 `store`、`previous_response_id` |
| `/grok/v1/responses/compact`、`/v1/responses/compact` | POST | Build 原生上下文压缩，强制非流式 |
| `/grok/v1/responses/{response_id}`、`/v1/responses/{response_id}` | GET | 查询 stored Response |
| 同上 | DELETE | 删除 stored Response |

stored Response 归属记录按客户端 API Key 隔离。连续请求和资源管理会固定使用创建该 Response 的 Build OAuth 账号；归属记录过期或账号不可用时不会切换到其他账号。

Build 原生 `context_management`、压缩输入和推理密文保留转发；`/responses/compact` 不可用时直接返回上游错误，不再调用模型生成本地摘要。中转层不按模型白名单降级 `reasoning.effort`，也不补写未指定的 `temperature` / `top_p`；具体值是否支持由上游决定。会话缓存键仍按租户隔离。

中转层不再执行请求去重，`Idempotency-Key` 也不会触发中转层拦截；相同内容或相同键的并发、连续请求均正常转发。工具参数相同、文本重复、连续重复输出不再被自动抑制或中断。上游如自行实施去重，其行为不由本网关控制。切换工作目录会重建上游会话，但保留客户端完整消息历史。

### 1.4 Grok 图片与文件

| 路径 | 方法 | 说明 |
|---|---|---|
| `/grok/v1/images/generations` | POST | 图片生成 |
| `/grok/v1/images/edits` | POST | 图片编辑 |
| `/v1/images/generations` | POST | Grok 图片生成别名 |
| `/v1/images/edits` | POST | Grok 图片编辑别名 |
| `/grok/v1/files/{image\|video}/{name}` | GET | 本地缓存媒体文件 |
| `/v1/files/{image\|video}/{name}` | GET | Grok 文件别名 |

### 1.5 Grok 视频与语音

| 路径 | 方法 | 说明 |
|---|---|---|
| `/grok/v1/videos`、`/v1/videos` | POST | Web app-chat 旧版异步视频生成入口 |
| `/grok/v1/videos/generations`、`/v1/videos/generations` | POST | Console DPoP 标准异步视频生成；基础模型与 1.5 |
| `/grok/v1/videos/edits`、`/v1/videos/edits` | POST | Console DPoP 视频编辑；当前仅基础模型 |
| `/grok/v1/videos/extensions`、`/v1/videos/extensions` | POST | Console DPoP 视频延长；当前仅基础模型 |
| `/grok/v1/videos/{video_id}`、`/v1/videos/{video_id}` | GET | 查询视频任务；按创建任务的 API Key 隔离 |
| `/grok/v1/videos/{video_id}/content`、`/v1/videos/{video_id}/content` | GET | 读取视频内容 |
| `/grok/v1/media/inputs`、`/v1/media/inputs` | POST | 上传临时图片或视频；multipart 字段 `file`，单文件最大 20 MiB |
| `/grok/v1/media/inputs/{file_id}`、`/v1/media/inputs/{file_id}` | GET / DELETE | 查询或删除调用方拥有的临时媒体输入 |
| `/grok/v1/tts`、`/v1/tts` | POST | Console 原生 TTS；支持流式音频响应 |
| `/grok/v1/tts/voices`、`/v1/tts/voices` | GET | 查询可用 Voice；路径后追加 `{voice_id}` 可查询单项 |
| `/grok/v1/stt`、`/v1/stt` | POST / WebSocket | Console 原生 HTTP 或流式 STT |
| `/grok/v1/realtime`、`/v1/realtime` | WebSocket | Console Realtime 语音双向代理 |
| `/grok/v1/audio/speech`、`/v1/audio/speech` | POST | OpenAI speech 请求转换到 Console TTS |
| `/grok/v1/audio/tasks`、`/v1/audio/tasks` | POST | `audio/speech` 兼容别名 |
| `/grok/v1/audio/transcriptions`、`/v1/audio/transcriptions` | POST | OpenAI 音频转录兼容；支持 `json`、`verbose_json`、`text` |

标准视频、TTS、STT 和 Realtime 只使用显式的 Grok Console SSO 账号，并通过 DPoP 请求上游。标准视频创建返回 `{"request_id":"video_..."}`，随后通过统一查询和内容端点读取结果。视频任务元数据按 API Key 所有者写入 Redis，TTL 为一小时；标准 Console 任务一旦取得上游 `request_id`，服务重启后会固定回原账号继续轮询和下载。每个运行任务持有可续期的 30 秒 Redis 原子租约，其他实例不会重复轮询或写结果；持有者失联后，租约过期即可由其他实例接管。`media_dir` 支持挂载共享文件系统，多副本启动时会验证 Redis、稳定实例 ID、集群标记和共享目录读写，因此完成视频和临时 `file_id` 可由任一副本读取。若进程在取得上游 ID 前中断，任务会明确变为 `video_resume_unavailable`，不会永久停留在 pending；旧 Web 分段视频任务不进行不安全重放。生成支持 `duration`（1–15）、`aspect_ratio`、`resolution`、`image`、`reference_images` 和 `reference_audios`；编辑要求 `prompt` 与 `video`；延长额外支持 2–10 秒的 `duration`。

媒体输入上传返回 `file_id`、类型、MIME、字节数和过期时间。ID 使用 192-bit 随机值，元数据在 Redis 中保留 24 小时并按 API Key 所有者隔离；支持 jpeg、png、webp、gif、mp4、webm 和 quicktime。`image`、`reference_images` 与 `video` 的结构化输入可在 `url` 和 `file_id` 中二选一；一次标准视频请求解析的本地媒体总量不得超过 32 MiB。

静态 HTTP/SOCKS 代理可用于语音 WebSocket；若启用托管 Grok 出口池，WebSocket 会在出口管理器支持租约式拨号前安全拒绝，避免绕过出口策略。

`audio/transcriptions` 会明确拒绝无法无损转换到 Console STT 的 `prompt`、非零 `temperature` 和 `timestamp_granularities`，也不提供 `/audio/translations`。

### 1.6 模型、健康与指标

| 路径 | 方法 | 说明 |
|---|---|---|
| `/v1/models` | GET | 全通道模型列表 |
| `/v1/models/{id}` | GET | 查询单个模型 |
| `/warp/v1/models` | GET | Warp 模型列表 |
| `/puter/v1/models` | GET | Puter 模型列表 |
| `/grok/v1/models` | GET | Grok 模型列表 |
| `/health` | GET | 健康检查 |
| `/metrics` | GET | Prometheus 指标 |

## 2. 管理接口

### 2.1 `/api/*`

| 路径 | 方法 | 说明 |
|---|---|---|
| `/api/login` | POST | 管理端登录 |
| `/api/logout` | POST | 管理端退出 |
| `/api/accounts` | GET/POST | 账号列表 / 创建账号 |
| `/api/accounts/{id}` | GET/PUT/DELETE | 查询 / 更新 / 删除账号 |
| `/api/accounts/{id}/check` | GET | 账号检查 |
| `/api/accounts/{id}/usage` | GET | 账号用量 |
| `/api/warp/device-auth` | POST | 启动 Warp 官方网页登录 |
| `/api/warp/device-auth/{id}` | GET/DELETE | 查询授权状态 / 取消授权 |
| `/api/puter/web-login` | POST | 试验：验证 Puter 官方弹窗授权并保存账号；需管理认证和同源 JSON 请求 |
| `/api/keys` | GET/POST | API Key 列表 / 创建 |
| `/api/keys/{id}` | PATCH/DELETE | 更新 API Key 状态或访问策略 / 删除 |
| `/api/models` | GET/POST | 模型列表 / 创建模型 |
| `/api/models/{id}` | GET/PUT/DELETE | 模型详情 / 更新 / 删除 |
| `/api/models/refresh` | POST | 按通道刷新模型列表 |
| `/api/export` | GET | 导出账号与模型 |
| `/api/import` | POST | 导入账号与模型 |
| `/api/config` | GET/POST | 查看 / 更新配置 |
| `/api/config/list` | GET | 管理端表单配置读取 |
| `/api/config/save` | POST | 管理端表单配置保存 |
| `/api/config/cache/clear` | POST | 清空 prompt/token 缓存 |
| `/api/token-cache/stats` | GET | Token 缓存统计 |
| `/api/token-cache/clear` | POST | 清空 Token 缓存 |

Warp 账号只能通过官方网页登录添加。`POST /api/accounts` 拒绝创建 Warp，
`PUT /api/accounts/{id}` 仅允许编辑设置，不能提交 Token 或转换为其他账号类型。
账号查询返回 `warp_authenticated` 表示服务器是否保存了登录凭据（不代表实时认证成功），不返回会话密钥。
`/api/import` 跳过 Warp 并计入 `skipped`；`/api/export` 不包含 Warp 账号。
迁移服务器后需重新进行 Warp 官方网页登录。已有账号及内部自动续期机制保留。

### 2.2 `/api/v1/admin/*` 和 `/v1/admin/*`

这些路径是 Grok 管理能力和 grok2api 对齐别名，两个前缀都可用。

| 路径 | 方法 | 说明 |
|---|---|---|
| `/config` | GET/POST | 管理配置 |
| `/verify` | GET | Grok 管理验证 |
| `/storage` | GET | Grok 存储信息 |
| `/tokens` | GET/POST | Grok token 池 |
| `/tokens/refresh` | POST | 同步刷新 token |
| `/tokens/refresh/async` | POST | 异步刷新 token |
| `/tokens/nsfw/enable` | POST | 同步启用 NSFW |
| `/tokens/nsfw/enable/async` | POST | 异步启用 NSFW |
| `/batch/{task}` | GET/POST | 批任务流与取消 |
| `/cache` | GET | 缓存摘要 |
| `/cache/list` | GET | 缓存列表 |
| `/cache/clear` | POST | 清空缓存 |
| `/cache/item/delete` | POST | 删除单项缓存 |
| `/cache/online/clear` | POST | 远端缓存清理 |
| `/cache/online/clear/async` | POST | 远端缓存异步清理 |
| `/cache/online/load/async` | POST | 远端缓存异步加载 |
| `/voice/token` | GET | 语音 token |
| `/imagine/start` | POST | imagine 开始 |
| `/imagine/stop` | POST | imagine 停止 |
| `/imagine/sse` | GET | imagine SSE |
| `/imagine/ws` | GET | imagine WebSocket |
| `/video/start` | POST | 视频任务开始 |
| `/video/stop` | POST | 视频任务停止 |
| `/video/sse` | GET | 视频 SSE |

### 2.3 `/api/v1/public/*` 和 `/v1/public/*`

| 路径 | 方法 | 说明 |
|---|---|---|
| `/verify` | GET | 公共验证接口 |
| `/voice/token` | GET | 公共语音 token |
| `/imagine/config` | GET | imagine 配置 |
| `/imagine/start` | POST | imagine 开始 |
| `/imagine/stop` | POST | imagine 停止 |
| `/imagine/sse` | GET | imagine SSE |
| `/imagine/ws` | GET | imagine WebSocket |
| `/video/start` | POST | 视频任务开始 |
| `/video/stop` | POST | 视频任务停止 |
| `/video/sse` | GET | 视频 SSE |

## 3. 认证方式

### 3.1 管理接口

满足以下任一条件即可：

1. `session_token` cookie
2. `Authorization: Bearer <admin_token>`
3. `X-Admin-Token: <admin_token>`
4. Basic Auth，密码等于 `admin_pass`

### 3.2 模型与推理接口

- 默认要求 `Authorization: Bearer <API Key>`；Anthropic Messages 客户端也可发送 `x-api-key: <API Key>`
- API Key 通过管理端 `/api/keys` 创建、禁用、设置访问策略和删除
- 模型列表、Messages、Chat、Responses、图片、视频和语音任务均执行该校验
- 只有在可信上游网关已经完成认证时，才应设置 `inference_auth_enabled=false`

API Key 创建和更新支持以下策略字段：

| 字段 | 语义 |
|---|---|
| `allowed_models` | 精确、忽略大小写的模型 ID 列表；空数组或省略表示不限，`["*"]` 也表示不限 |
| `rpm_limit` | 固定分钟窗口内允许的请求数；`0` 或省略表示不限 |
| `expires_at` | RFC3339 到期时间；创建时省略表示永不过期，PATCH 时传 `null` 可清除 |

```json
{
  "name": "production-client",
  "allowed_models": ["grok-4.6", "grok-imagine-image"],
  "rpm_limit": 60,
  "expires_at": "2026-12-31T16:00:00Z"
}
```

RPM 在 Redis 中原子计数，并覆盖所有受 API Key 保护的模型与推理请求；超限返回 `429` 和 `Retry-After: 60`。模型列表会按白名单过滤，白名单外的推理请求返回 `403 model_not_allowed`。已有 Key 缺少这些字段时保持不限模型、不限 RPM、永不过期。

### 3.3 公共工具接口

- `/api/v1/public/*` 与 `/v1/public/*` 会按当前 `public_key` / `public_enabled` 逻辑鉴权

## 4. 请求语义说明

### 4.1 Claude Messages 非流式工具调用

当模型要调用工具时，非流式响应会直接返回 `content` 数组中的 `tool_use` block，而不是空内容。

当前已做回归覆盖的 Puter 场景：

- `Read`
- `Write`
- `Edit`
- `Delete`
- 长上下文
- 多轮 `tool_result`

### 4.2 `tool_result` follow-up

带有 `tool_result` 的 follow-up 请求有两种正常结果：

1. 继续返回新的 `tool_use`
2. 收敛为最终 `text`

当前实现不会因为上游 usage token 已产生就把“空内容”误判成有效输出。

### 4.3 模型刷新

`POST /api/models/refresh` 示例：

```bash
curl -s http://127.0.0.1:3002/api/models/refresh \
  -H 'Content-Type: application/json' \
  -d '{"channel":"puter"}'
```

返回字段：

| 字段 | 说明 |
|---|---|
| `channel` | 当前刷新通道 |
| `source` | 模型发现来源 |
| `discovered` | 来源中发现的模型数 |
| `verified` | 纳入同步集合的模型数 |
| `added` | 本轮新增数量 |
| `updated` | 本轮更新数量 |
| `deleted` | 本轮删除数量 |
| `default_model_id` | 当前默认模型 |
| `added_model_ids` | 新增模型 ID 列表 |
| `deleted_model_ids` | 删除模型 ID 列表 |

注意：

- 当前刷新是“来源同步”；Puter 会额外使用账号 `test_mode` 逐模型验证
- 来源拿不到的模型会被删除

## 5. 常用请求示例

### 5.1 Puter Claude Messages 工具首轮

```bash
curl -s http://127.0.0.1:3002/puter/v1/messages \
	-H 'Authorization: Bearer sk-...' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-opus-5",
    "messages": [{"role":"user","content":"Read README.md"}],
    "tools": [{
      "name": "Read",
      "input_schema": {
        "type": "object",
        "properties": {
          "file_path": {"type": "string"}
        },
        "required": ["file_path"]
      }
    }],
    "stream": false
  }'
```

### 5.2 Grok Chat Completions

```bash
curl -s http://127.0.0.1:3002/grok/v1/chat/completions \
	-H 'Authorization: Bearer sk-...' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "grok-4.6",
    "messages": [{"role":"user","content":"介绍一下你自己"}],
    "stream": false
  }'
```

## 6. 错误约定

- `400`：请求参数错误、模型错误、方法错误
- `401` / `403` / `429`：账号状态或鉴权状态错误
- `502`：上游请求失败或流解析失败
- `503`：当前通道无可用账号

常见错误：

- `model not found`
- `puter API error: ...`
- `Bad Gateway`
- `stream parse error`
