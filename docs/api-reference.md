# API 接口文档

本文档以 `cmd/server/main.go` 当前路由为准。

## 1. 公开接口

| 路径 | 方法 | 说明 |
|---|---|---|
| `/orchids/v1/messages` | POST | Claude Messages 代理（Orchids 通道） |
| `/orchids/v1/messages/count_tokens` | POST | 输入 Token 估算（Orchids 通道） |
| `/warp/v1/messages` | POST | Claude Messages 代理（Warp 通道） |
| `/warp/v1/messages/count_tokens` | POST | 输入 Token 估算（Warp 通道） |
| `/orchids/v1/chat/completions` | POST | OpenAI Chat Completions 兼容（Orchids） |
| `/warp/v1/chat/completions` | POST | OpenAI Chat Completions 兼容（Warp） |
| `/grok/v1/chat/completions` | POST | OpenAI Chat Completions 兼容（Grok） |
| `/grok/v1/images/generations` | POST | Grok 图片生成 |
| `/grok/v1/images/edits` | POST | Grok 图片编辑（multipart） |
| `/grok/v1/files/{image|video}/{name}` | GET | 读取本地缓存的图片/视频 |
| `/v1/models` | GET | 全通道可用模型列表 |
| `/v1/models/{id}` | GET | 查询单模型 |
| `/orchids/v1/models` | GET | Orchids 可用模型 |
| `/warp/v1/models` | GET | Warp 可用模型 |
| `/grok/v1/models` | GET | Grok 可用模型 |
| `/health` | GET | 健康检查 |
| `/metrics` | GET | Prometheus 指标 |

## 2. 管理接口（需认证）

| 路径 | 方法 | 说明 |
|---|---|---|
| `/api/login` | POST | 管理端登录，写入 `session_token` cookie |
| `/api/logout` | POST | 管理端退出 |
| `/api/accounts` | GET/POST | 账号列表 / 创建账号 |
| `/api/accounts/{id}` | GET/PUT/DELETE | 单账号查询 / 更新 / 删除 |
| `/api/accounts/{id}/check` | GET | 账号连通性与状态检查 |
| `/api/accounts/{id}/usage` | GET | 账号用量信息 |
| `/api/keys` | GET/POST | API Key 列表 / 创建 |
| `/api/keys/{id}` | GET/PUT/DELETE | API Key 详情 / 启停 / 删除 |
| `/api/models` | GET/POST | 模型配置列表 / 创建 |
| `/api/models/{id}` | GET/PUT/DELETE | 模型配置详情 / 更新 / 删除 |
| `/api/export` | GET | 导出账号与模型配置 |
| `/api/import` | POST | 导入账号与模型配置 |
| `/api/config` | GET/POST | 查看 / 更新运行配置（保存到 Redis） |
| `/api/config/cache/stats` | GET | Token 缓存统计 |
| `/api/config/cache/clear` | POST | 清空 Token 缓存 |
| `/api/v1/admin/voice/token` | GET | 获取 Grok 语音 token |
| `/api/v1/admin/imagine/start` | POST | 启动 imagine 连续生图 |
| `/api/v1/admin/imagine/stop` | POST | 停止 imagine 任务 |
| `/api/v1/admin/imagine/sse` | GET | imagine SSE 推送 |
| `/api/v1/admin/imagine/ws` | GET | imagine WebSocket 推送 |
| `/api/v1/admin/cache` | GET | Grok 媒体缓存摘要 |
| `/api/v1/admin/cache/list` | GET | Grok 缓存列表 |
| `/api/v1/admin/cache/clear` | POST | 清空 Grok 缓存 |
| `/api/v1/admin/cache/item/delete` | POST | 删除单个缓存文件 |

## 3. 认证方式

### 3.1 公开接口

默认不强制鉴权（通常由网关/反向代理做外层鉴权）。

### 3.2 管理接口

满足以下任一条件即可：

1. `session_token` cookie（通过 `/api/login` 获取）
2. `Authorization: Bearer <admin_token>` 或直接 `Authorization: <admin_token>`
3. `X-Admin-Token: <admin_token>`
4. Basic Auth，且密码等于 `admin_pass`（用户名不参与校验）

## 4. 常用请求示例

### 4.1 Claude Messages

```bash
curl -s http://127.0.0.1:3002/orchids/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-sonnet-4-5",
    "messages": [{"role":"user","content":"hello"}],
    "stream": false
  }'
```

### 4.2 OpenAI Chat Completions（Grok）

```bash
curl -s http://127.0.0.1:3002/grok/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"grok-3",
    "messages":[{"role":"user","content":"介绍一下你自己"}],
    "stream":false
  }'
```

### 4.3 图片生成

```bash
curl -s http://127.0.0.1:3002/grok/v1/images/generations \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"grok-imagine-1.0",
    "prompt":"中国高速返程堵车，航拍，纪实风",
    "n":2,
    "response_format":"url",
    "stream":false
  }'
```

### 4.4 图片编辑（multipart）

```bash
curl -s http://127.0.0.1:3002/grok/v1/images/edits \
  -F 'model=grok-imagine-1.0-edit' \
  -F 'prompt=把天空改成黄昏' \
  -F 'image=@./input.jpg' \
  -F 'n=1' \
  -F 'response_format=url'
```

## 5. 返回与错误约定

- `4xx`：请求参数/模型/方法错误（例如 `model not found`）
- `502`：上游 Grok/Warp/Orchids 异常或解析失败
- `503`：账号池不可用（无可用账号或无可用 token）

常见错误：

- `model not found`：模型名错误或模型未启用（例如 `gork-3`）
- `image model not supported`：图像接口使用了非图像模型
- `no image generated`：上游成功但未产出可用图片
