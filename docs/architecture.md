# 架构设计

## 1. 总览

`Orchids-2api` 当前由两条主处理链组成：

- `internal/handler`：处理 `warp`、`puter`、`workbuddy`
- `internal/grok`：处理 `grok`

整体目标：

- 对外暴露统一的 Claude Messages 与 OpenAI 兼容接口
- 对内按通道维护账号池、模型表、失败切换和会话状态
- 用 Redis 保存账号、模型、配置、API Key 与缓存相关状态

## 2. 当前目录结构

```text
Orchids-2api/
├── cmd/server/                  # 程序入口、路由、模型刷新
├── internal/
│   ├── api/                     # 管理端 REST API
│   ├── auth/                    # 管理会话
│   ├── config/                  # 配置加载与默认值
│   ├── debug/                   # 调试日志
│   ├── errors/                  # 错误分类
│   ├── grok/                    # Grok chat/images/files/admin
│   ├── handler/                 # Warp/Puter/WorkBuddy 主处理器
│   ├── loadbalancer/            # 账号选择与状态管理
│   ├── middleware/              # trace/log/session/concurrency
│   ├── provider/                # 通道到 client 的注册表
│   ├── prompt/                  # 共享消息结构
│   ├── puter/                   # Puter 上游客户端
│   ├── store/                   # Redis 存储
│   ├── template/                # 管理页面模板
│   ├── tokencache/              # token / prompt cache
│   ├── upstream/                # 统一上游事件结构
│   ├── util/                    # 通用工具
│   ├── warp/                    # Warp 上游客户端
│   └── workbuddy/               # WorkBuddy 国际版上游客户端
├── web/                         # 嵌入式前端资源
└── docs/
```

## 3. 路由与职责分层

### 3.1 `cmd/server/routes.go`

负责统一注册：

- `/*/v1/messages`
- `/*/v1/chat/completions`
- `/*/v1/models`
- `/api/*`
- `/api/v1/admin/*` / `/v1/admin/*`
- `/api/v1/public/*` / `/v1/public/*`

### 3.2 `internal/handler`

负责 `warp` / `puter`：

- 解析 Claude/OpenAI 请求
- 识别通道与目标模型
- 维护会话状态、workdir、去重和 token 统计
- 选择账号、切换失败账号并重试
- 把上游 SSE / 直出事件转换为 Claude 或 OpenAI 兼容响应

### 3.3 `internal/grok`

负责 `grok`：

- Chat Completions
- 图片生成与编辑
- 本地媒体缓存文件
- Grok 管理接口与公共 imagine/video/voice 能力

### 3.4 `internal/loadbalancer`

核心职责：

- 按通道筛选可用账号
- 记录连接数与失败状态
- 触发冷却与恢复
- 为请求选择当前最合适的账号

### 3.5 `internal/store`

统一存储：

- 账号
- 模型
- API Key
- 配置快照

## 4. 主请求流

### 4.1 `warp` / `puter`

```text
HTTP Request
  -> middleware chain
  -> Handler.HandleMessages
  -> parse request + dedup
  -> resolve channel + model
  -> load session/workdir state
  -> select account from LoadBalancer
  -> build UpstreamRequest
  -> send to channel client
  -> stream_handler converts upstream events
  -> write Claude/OpenAI compatible response
  -> sync account/session/cache stats
```

### 4.2 `grok`

```text
HTTP Request
  -> middleware chain
  -> grok.Handler
  -> validate request/model
  -> select grok account
  -> call upstream
  -> normalize stream / image / file result
  -> write OpenAI-compatible response
```

## 5. 模型管理流

模型刷新入口：`POST /api/models/refresh`

按通道的来源：

- `warp`：账号 GraphQL 发现结果，失败时回退内置种子
- `puter`：Puter 官方模型目录与本地当前代策略的交集，再经账号 `test_mode` 验证
- `grok`：内置支持表 + 现存模型 + 公共文档探测

当前策略：

- 新发现模型写入本地表
- 来源缺失模型从本地表删除
- Puter 做无额度消耗的 `test_mode` 逐模型验证；其他通道按各自来源能力验证

## 6. Puter 当前实现要点

Puter 走 `internal/puter`，特点是：

- 请求直接发送原生 `tools`、assistant `tool_calls` 与 `role: tool` 消息
- 上游流按 `text`、`reasoning`、`tool_use`、`usage`、`error` 类型严格解码
- `tool_use` 被转换为统一 `model.tool-call` 事件，再由 handler 组装成 Claude Messages 或 OpenAI 响应
- 非流式 `tool_use` 与 `tool_result` follow-up 已有回归测试覆盖

## 7. 运行时状态

### 7.1 Redis 中保存

- 账号、模型、API Key、配置
- 可选 token cache / prompt cache
- 在可用时，handler 会优先用 Redis 做会话与去重存储

### 7.2 本地目录

- `debug-logs/`：调试日志
- `data/tmp/image`：Grok 图片缓存
- `data/tmp/video`：Grok 视频缓存

## 8. 当前已知设计边界

- 很多运行时默认值由 `config.ApplyHardcoded` 强制写入，不是所有字段都能靠配置文件覆盖
- `/metrics` 默认公开，生产环境建议放到内网或额外网关后面
- 运行期会在 `data/tmp` 产生缓存文件，不应提交到 Git
