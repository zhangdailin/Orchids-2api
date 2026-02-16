# Orchids-2api 请求流程

## 1. 启动流程

```text
main.go
  -> 读取配置文件
  -> 初始化日志
  -> 初始化 Redis Store
  -> 从 Redis 读取 config 覆盖
  -> 初始化 LoadBalancer
  -> 初始化 Handler(orchids/warp) + Grok Handler
  -> 注册路由
  -> 启动自动刷新 token（可选）
  -> 监听端口
```

关键点：

- 模型列表由 Redis 维护，启动时会自动种子。
- 若 Redis 中存在 `config`，会覆盖文件配置。

## 2. 公共请求入口

`main.go` 里三类主入口：

1. Orchids/Warp
- `/orchids/v1/messages`
- `/warp/v1/messages`
- `/orchids/v1/chat/completions`
- `/warp/v1/chat/completions`

2. Grok
- `/grok/v1/chat/completions`
- `/grok/v1/images/generations`
- `/grok/v1/images/edits`
- `/grok/v1/files/*`

3. 公共能力
- `/v1/models`
- `/health`
- `/metrics`

## 3. Orchids/Warp 处理流（`internal/handler`）

```text
HTTP Request
  -> ConcurrencyLimiter
  -> HandleMessages
  -> 解析 JSON + 去重（短时间重复请求抑制）
  -> 识别通道（orchids/warp）
  -> 校验模型可用性（Redis model status）
  -> 解析 workdir / conversation key
  -> LoadBalancer 选账号
  -> 组装上游 payload
  -> 调用 upstream 客户端
  -> stream_handler 转换为 Claude/OpenAI 响应
  -> 更新账号状态/用量
```

### 3.1 重试与切换

当上游错误属于可重试类型（如 429/5xx/网络超时）时：

- 标记当前账号状态
- 排除失败账号
- 重新选账号并重试
- 达到重试上限后返回错误

### 3.2 会话状态

按 `conversation_id`/header/metadata 识别会话，保存：

- workdir
- warp 会话 id
- 最后访问时间

并定期清理过期会话项。

## 4. Grok 处理流（`internal/grok`）

## 4.1 Chat Completions

```text
/grok/v1/chat/completions
  -> 校验请求与模型
  -> 提取文本与附件
  -> 选 grok 账号并获取 token
  -> 上传附件（如有）
  -> 请求 grok upstream
  -> 解析流式 token/modelResponse
  -> 清洗工具卡片/渲染标签
  -> 产出 OpenAI chat 响应
```

特性：

- 对图片请求可走更稳定的图像生成路径。
- 自动提取图片 URL 并优先返回可访问链接。

## 4.2 图片生成与编辑

```text
/grok/v1/images/generations | /edits
  -> 校验参数（model/prompt/n/stream）
  -> 请求 grok upstream
  -> 提取候选图片 URL
  -> 过滤低质量/缩略图候选
  -> 下载并缓存到 data/tmp/image
  -> 返回 /grok/v1/files/image/{name} 或 b64_json
```

编辑接口额外步骤：

- 读取 multipart 图片
- 校验类型（png/jpeg/webp）与大小
- 先上传输入图，再发编辑请求

## 4.3 本地文件服务

`GET /grok/v1/files/{image|video}/{name}`：

- 仅允许 `image` 或 `video`
- 路径安全校验后读取 `data/tmp/*`
- 返回静态文件并附缓存头

## 5. 管理端流程

```text
/api/login
  -> 校验 admin_user/admin_pass
  -> 下发 session_token cookie

/api/*
  -> SessionAuth
  -> 账号/API key/模型/配置管理
```

认证支持：

- session cookie
- `admin_token`（Bearer 或 X-Admin-Token）
- Basic Auth（密码匹配 `admin_pass`）

## 6. 可观测性与排障

- `GET /health`：基础可用性
- `GET /metrics`：Prometheus
- `GET /debug/pprof/`：仅 debug 模式 + 管理认证
- 日志中常见关键字：
  - `model not found`
  - `no available ... token`
  - `stream parse error`
  - `Bad Gateway`
