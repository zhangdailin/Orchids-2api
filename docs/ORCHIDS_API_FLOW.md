# Orchids-2api 请求流程

文件名沿用历史命名，但当前内容覆盖 `orchids`、`warp`、`puter`、`grok` 四类通道。

## 1. 启动流程

```text
main.go
  -> 读取配置文件
  -> 应用默认值与硬编码运行时配置
  -> 初始化日志
  -> 初始化 Redis Store
  -> 若 Redis 中存在 settings:config，则覆盖文件配置
  -> 初始化 LoadBalancer
  -> 初始化 Handler 与 Grok Handler
  -> 初始化 token cache / prompt cache
  -> 初始化 session / dedup / audit（Redis 可用时）
  -> 注册 provider registry
  -> 注册路由
  -> 启动后台 token refresh / auth cleanup
  -> 监听端口
```

关键点：

- 配置最终会经过 `ApplyHardcoded()`，不是所有字段都可由配置文件覆盖
- 模型、账号、配置、API Key 都由 Redis 持久化
- `settings:config` 存在时会优先于本地 `config.json`

## 2. 对外入口

### 2.1 Claude Messages

- `/orchids/v1/messages`
- `/warp/v1/messages`
- `/puter/v1/messages`

### 2.2 OpenAI Chat Completions

- `/orchids/v1/chat/completions`
- `/warp/v1/chat/completions`
- `/puter/v1/chat/completions`
- `/grok/v1/chat/completions`
- `/v1/chat/completions`，Grok 兼容别名

### 2.3 模型、图片、文件与公共能力

- `/v1/models`
- `/health`
- `/metrics`
- `/grok/v1/images/generations`
- `/grok/v1/images/edits`
- `/grok/v1/files/*`
- `/api/*`
- `/api/v1/admin/*`、`/v1/admin/*`
- `/api/v1/public/*`、`/v1/public/*`

## 3. `orchids` / `warp` / `puter` 处理流

这些通道统一走 [handler.go](/D:/Code/Orchids-2api/internal/handler/handler.go) 和 [stream_handler.go](/D:/Code/Orchids-2api/internal/handler/stream_handler.go)。

```text
HTTP Request
  -> SecurityHeaders / Trace / Logging / ConcurrencyLimiter
  -> HandleMessages
  -> 解析 Claude/OpenAI 请求
  -> 请求去重
  -> 根据路径识别 channel
  -> 校验模型是否在本地模型表中可用
  -> 读取或创建会话状态
  -> LoadBalancer 选账号
  -> provider registry 创建对应上游 client
  -> 组装 upstream payload
  -> 请求上游
  -> stream_handler 转换事件
  -> 输出 Claude/OpenAI 兼容响应
  -> 更新账号状态、会话、缓存和统计
```

### 3.1 失败重试

当错误属于可重试类型时：

- 标记当前账号状态
- 排除失败账号
- 重新选账号
- 达到重试上限后返回错误

### 3.2 Puter 特殊点

Puter 虽然也走统一 handler，但上游特征不同：

- 上游返回的工具调用需要重新组装为 Claude Messages `tool_use`
- 非流式响应要保留 `tool_use` block
- `tool_result` follow-up 可以继续发起下一轮工具调用，也可以收敛成文本
- 当前已有 `Read`、`Write`、`Edit`、`Delete`、长上下文、多轮 `tool_result` 回归测试

## 4. `grok` 处理流

Grok 走独立的 [handler.go](/D:/Code/Orchids-2api/internal/grok/handler.go)。

### 4.1 Chat Completions

```text
/grok/v1/chat/completions
  -> 校验请求与模型
  -> 选 grok 账号
  -> 提取文本与附件
  -> 请求 grok upstream
  -> 解析流式/非流式事件
  -> 转换为 OpenAI Chat Completions
```

### 4.2 图片生成与编辑

```text
/grok/v1/images/generations | /edits
  -> 校验模型与参数
  -> 请求 grok upstream
  -> 提取候选图片 URL
  -> 过滤无效候选
  -> 下载并缓存到 data/tmp/image
  -> 返回本地文件 URL 或 b64_json
```

### 4.3 本地文件服务

`GET /grok/v1/files/{image|video}/{name}` 和 `/v1/files/*`：

- 仅允许 `image` 或 `video`
- 做路径安全校验
- 从 `data/tmp` 读取缓存文件

## 5. 模型刷新流程

刷新入口：`POST /api/models/refresh`

```text
Admin Request
  -> SessionAuth
  -> makeModelRefreshHandler
  -> 按 channel 选择来源
  -> 拉取候选模型列表
  -> 选默认模型
  -> 新模型写入本地表
  -> 已存在模型更新状态/排序/默认标记
  -> 来源中已消失的本地模型直接删除
  -> 返回 added / updated / deleted / verified
```

当前来源：

- `orchids`：上游公开模型选择列表
- `warp`：账号 GraphQL 发现，失败时回退种子模型
- `puter`：公开模型列表
- `grok`：内置支持模型 + 现存模型 + 公共文档探测

## 6. 管理端与公共接口流程

### 6.1 管理端

```text
/api/login
  -> 校验 admin_user/admin_pass
  -> 写入 session_token cookie

/api/*
  -> SessionAuth
  -> 账号 / 模型 / 配置 / token cache 管理
```

### 6.2 Public / Admin Alias

- `SessionAuth` 支持 cookie、Bearer、`X-Admin-Token`、Basic Auth
- Public API 使用 `public_key` 鉴权
- 若 `public_key` 为空，则 public API 默认不鉴权

## 7. 可观测性

- `/health`
- `/metrics`
- `/debug/pprof/`，仅 debug 模式且需管理认证

常见排障关键字：

- `model not found`
- `no available grok token`
- `stream parse error`
- `Bad Gateway`
