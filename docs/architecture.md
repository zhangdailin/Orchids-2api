# 架构设计

## 目录结构

```
Orchids-2api/
├── cmd/
│   └── server/
│       └── main.go              # 应用入口点
├── internal/                     # 核心业务逻辑
│   ├── api/api.go               # 账号管理 REST API
│   ├── handler/                  # 主请求处理器
│   │   ├── handler.go           # /orchids/v1/messages, /warp/v1/messages
│   │   ├── stream_handler.go    # SSE 流处理
│   │   ├── tool_exec.go         # 本地工具执行
│   │   └── tools.go             # 工具名称映射
│   ├── loadbalancer/            # 加权负载均衡
│   ├── store/store.go           # Redis 账号/配置存储层
│   ├── config/config.go         # 配置管理
│   ├── orchids/                  # Orchids 上游客户端
│   │   ├── client.go            # SSE 模式客户端
│   │   ├── ws_aiclient.go       # WebSocket 模式客户端
│   │   ├── fs.go                # 文件系统操作
│   │   └── tool_mapping.go      # 上游工具名称映射
│   ├── warp/                     # Warp 上游客户端
│   │   ├── client.go            # Warp API 客户端
│   │   └── session.go           # Warp 会话/Token 管理
│   ├── upstream/                 # 通用上游组件
│   │   ├── wspool.go            # WebSocket 连接池
│   │   ├── breaker.go           # 熔断器
│   │   └── reliability.go       # 重试与可靠性
│   ├── middleware/auth.go       # 认证中间件
│   ├── clerk/clerk.go           # Clerk 认证服务
│   ├── prompt/                   # 提示词处理
│   ├── tiktoken/                 # Token 计数
│   ├── debug/logger.go          # 调试日志
│   └── perf/                     # 性能优化 (对象池)
├── web/                          # 嵌入式静态资源
│   ├── static/                   # CSS, JS
│   └── templates/                # HTML 模板
└── go.mod                        # Go 模块定义
```

## 核心组件

### 负载均衡器 (LoadBalancer)

**位置**: `internal/loadbalancer/loadbalancer.go`

- 加权随机选择算法
- 支持账号排除 (故障转移)
- 自动递增请求计数
- 仅选择已启用的账号

### 请求处理器 (Handler)

**位置**: `internal/handler/handler.go`

- 解析 Claude API 格式请求
- 调用负载均衡器选择账号
- 构建上游请求提示词
- 处理 SSE 流式响应
- 转换响应格式为 Claude API 格式
- 处理工具调用 (Tool Calls)

### Orchids 上游客户端

**位置**: `internal/orchids/`

**上游服务器**:
```
https://orchids-server.calmstone-6964e08a.westeurope.azurecontainerapps.io/agent/coding-agent
```

- `client.go` - SSE 模式请求处理
- `ws_aiclient.go` - WebSocket 模式请求处理
- `fs.go` - 文件系统操作（用于 fs_operation 事件）
- `tool_mapping.go` - 上游与 Claude 工具名称映射
- 通过 Clerk 获取 JWT Token
- 发送请求到上游服务器
- 解析 SSE/WebSocket 响应流

### Warp 上游客户端

**位置**: `internal/warp/`

- `client.go` - Warp API 请求处理
- `session.go` - 会话管理与 Token 刷新
- 使用 Protobuf 格式通信

### 通用上游组件

**位置**: `internal/upstream/`

- `wspool.go` - WebSocket 连接池管理
- `breaker.go` - 熔断器（防止级联故障）
- `reliability.go` - 重试策略与可靠性配置

### Clerk 认证服务

**位置**: `internal/clerk/clerk.go`

**Clerk API**: `https://clerk.orchids.app/v1/client`

- 从 ClientCookie 获取账号信息
- 生成 JWT Token 用于上游认证
- 提取 SessionID、UserID、Email

### 提示词构建器 (Prompt Builder)

**位置**: `internal/prompt/prompt_v2.go`

- 将 Claude API 消息转换为 Markdown 格式
- 构建结构化提示词

## 请求流程

```
客户端请求 (Claude API 格式)
    ↓
POST /orchids/v1/messages / /warp/v1/messages (Handler)
    ↓
解析请求 → 提取 model, messages, tools
    ↓
负载均衡器 → 选择账号 (加权随机)
    ↓
提示词构建器 → 转换为 Markdown 格式
    ↓
Clerk 服务 → 获取 JWT Token
    ↓
上游客户端 → 发送到 Orchids 服务器
    ↓
接收 SSE 流式响应
    ↓
转换为 Claude API SSE 格式
    ↓
流式返回给客户端
    ↓
记录调试日志 (如启用)
```

## 数据模型

### Account 账号表

```go
type Account struct {
    ID           int64     // 主键
    Name         string    // 账号名称
    SessionID    string    // Clerk 会话 ID
    ClientCookie string    // 认证 Cookie (JWT)
    ClientUat    string    // 客户端 UAT 时间戳
    ProjectID    string    // 项目 UUID
    UserID       string    // 用户 ID
    AgentMode    string    // 模型类型 (默认: claude-opus-4.5)
    Email        string    // 用户邮箱
    Weight       int       // 负载均衡权重
    Enabled      bool      // 是否启用
    RequestCount int64     // 请求计数
    LastUsedAt   time.Time // 最后使用时间
    CreatedAt    time.Time // 创建时间
    UpdatedAt    time.Time // 更新时间
}
```

### Settings 设置表

```go
type Settings struct {
    ID    int64  // 主键
    Key   string // 设置键 (唯一)
    Value string // 设置值
}
```
