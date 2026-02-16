# 架构设计

## 1. 总览

`Orchids-2api` 是一个三通道代理服务：

- `orchids` 通道：Claude Messages / OpenAI 兼容入口
- `warp` 通道：Claude Messages / OpenAI 兼容入口
- `grok` 通道：OpenAI Chat + 图片生成/编辑 + 媒体本地缓存

核心目标：统一接口、统一账号池、统一模型管理，并在上游异常时自动重试和切换账号。

## 2. 目录结构（当前）

```text
Orchids-2api/
├── cmd/server/main.go          # 程序入口与路由注册
├── internal/
│   ├── api/                    # 管理端 REST API
│   ├── auth/                   # 管理会话 token
│   ├── clerk/                  # Orchids 账号鉴权辅助
│   ├── config/                 # 配置加载与默认值
│   ├── debug/                  # 调试日志
│   ├── grok/                   # Grok 通道（chat/images/files/admin）
│   ├── handler/                # Orchids/Warp 主处理器
│   ├── loadbalancer/           # 账号选择、状态更新、限流切换
│   ├── middleware/             # trace/log/session/concurrency
│   ├── orchids/                # Orchids 上游客户端
│   ├── store/                  # Redis 存储层（账号/模型/配置/API key）
│   ├── template/               # 管理页面模板渲染
│   ├── tokencache/             # 输入 token 估算缓存
│   ├── upstream/               # 通用上游协议结构
│   ├── util/                   # 工具方法
│   └── warp/                   # Warp 上游客户端
├── web/                        # 嵌入式前端资源（admin）
└── docs/
```

## 3. 请求入口与职责划分

### 3.1 `internal/handler`

负责 `orchids` 与 `warp` 的主流程：

- 解析 Claude/OpenAI 请求
- 会话 workdir 解析与会话状态维护
- 输入去重（短时间重复请求抑制）
- 模型可用性校验（基于 Redis 模型表）
- 账号选择、失败重试、账号切换
- SSE/JSON 格式化输出

### 3.2 `internal/grok`

负责 `grok` 专用流程：

- `/grok/v1/chat/completions`
- `/grok/v1/images/generations`
- `/grok/v1/images/edits`
- `/grok/v1/files/*`
- 管理端 imagine / cache / voice 接口

关键增强：

- 图片链接优先本地化（`/grok/v1/files/image/*`）
- 优先过滤低质量缩略图（如 `-part-0`）
- 403/429 时自动切换 grok 账号继续请求

### 3.3 `internal/loadbalancer`

- 按通道筛选账号
- 排除失败账号并切换
- 记录账号状态（如 401/403/429）
- 维护连接计数与缓存 TTL

### 3.4 `internal/store`（Redis）

统一存储：

- 账号（`Account`）
- 模型（`Model`）
- API Keys
- 配置快照（`config`）

启动时会自动种子模型（Orchids/Warp/Grok）。

## 4. 关键数据流

### 4.1 Orchids/Warp 消息流

```text
Client
  -> /orchids|/warp 路由
  -> ConcurrencyLimiter
  -> Handler.HandleMessages
  -> validate model
  -> select account (loadbalancer)
  -> upstream request (orchids/warp client)
  -> stream_handler 转换输出
  -> Client
```

### 4.2 Grok 图片流

```text
Client
  -> /grok/v1/images/generations or /edits
  -> Grok Handler
  -> upstream grok
  -> 提取图片 URL
  -> 下载并缓存到 data/tmp/image
  -> 返回 /grok/v1/files/image/{name}
  -> Client
```

## 5. 媒体缓存设计（Grok）

- 缓存根目录：`data/tmp`
- 图片目录：`data/tmp/image`
- 视频目录：`data/tmp/video`
- 对外读取：`GET /grok/v1/files/{image|video}/{name}`

目的：

- 避免前端直连 `assets.grok.com` 失败导致“图片无法显示”
- 保证返回链接在服务所在网络可访问

## 6. 可观测性

- 健康检查：`/health`
- 指标：`/metrics`
- pprof：`/debug/pprof/`（仅 `debug_enabled=true` 时注册，且需管理认证）
- 运行日志：JSON slog（默认 stdout，可重定向到 `server.log`）
