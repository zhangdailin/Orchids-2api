# API 参考

本文档以 [routes.go](/D:/Code/Orchids-2api/cmd/server/routes.go) 和 [model_refresh.go](/D:/Code/Orchids-2api/cmd/server/model_refresh.go) 当前实现为准。

## 1. 公开接口

### 1.1 Claude Messages 风格

| 路径 | 方法 | 说明 |
|---|---|---|
| `/orchids/v1/messages` | POST | Orchids 通道 Claude Messages 代理 |
| `/warp/v1/messages` | POST | Warp 通道 Claude Messages 代理 |
| `/puter/v1/messages` | POST | Puter 通道 Claude Messages 代理 |
| `/*/v1/messages/count_tokens` | POST | 输入 token 估算 |

### 1.2 OpenAI Chat Completions 风格

| 路径 | 方法 | 说明 |
|---|---|---|
| `/orchids/v1/chat/completions` | POST | Orchids OpenAI 兼容入口 |
| `/warp/v1/chat/completions` | POST | Warp OpenAI 兼容入口 |
| `/puter/v1/chat/completions` | POST | Puter OpenAI 兼容入口 |
| `/grok/v1/chat/completions` | POST | Grok OpenAI 兼容入口 |
| `/v1/chat/completions` | POST | Grok 兼容别名 |

### 1.3 Grok 图片与文件

| 路径 | 方法 | 说明 |
|---|---|---|
| `/grok/v1/images/generations` | POST | 图片生成 |
| `/grok/v1/images/edits` | POST | 图片编辑 |
| `/v1/images/generations` | POST | Grok 图片生成别名 |
| `/v1/images/edits` | POST | Grok 图片编辑别名 |
| `/grok/v1/files/{image\|video}/{name}` | GET | 本地缓存媒体文件 |
| `/v1/files/{image\|video}/{name}` | GET | Grok 文件别名 |

### 1.4 模型、健康与指标

| 路径 | 方法 | 说明 |
|---|---|---|
| `/v1/models` | GET | 全通道模型列表 |
| `/v1/models/{id}` | GET | 查询单个模型 |
| `/orchids/v1/models` | GET | Orchids 模型列表 |
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
| `/api/keys` | GET/POST | API Key 列表 / 创建 |
| `/api/keys/{id}` | GET/PUT/DELETE | API Key 详情 / 更新 / 删除 |
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

### 3.2 公共接口

- 普通代理接口默认不强制鉴权，通常由上层网关控制
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

- 当前刷新是“来源同步”，不是逐个模型测活
- 来源拿不到的模型会被删除

## 5. 常用请求示例

### 5.1 Orchids Claude Messages

```bash
curl -s http://127.0.0.1:3002/orchids/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-sonnet-4-6",
    "messages": [{"role":"user","content":"hello"}],
    "stream": false
  }'
```

### 5.2 Puter Claude Messages 工具首轮

```bash
curl -s http://127.0.0.1:3002/puter/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-opus-4-5",
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

### 5.3 Grok Chat Completions

```bash
curl -s http://127.0.0.1:3002/grok/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "grok-4",
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
