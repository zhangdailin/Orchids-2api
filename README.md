# Orchids-2api 文档

## 项目简介

**Orchids-2api** (orchids-api) 是一个 Go 语言编写的 API 代理服务器，提供多账号管理与负载均衡代理功能，兼容 Claude API 格式的请求转发。

### 核心功能

- 多账号管理与负载均衡代理
- 兼容 Claude API 格式的请求转发
- 将请求代理到 Orchids 后端服务
- 提供 Web 管理界面


## 文档目录

| 文档 | 描述 |
|------|------|
| [架构设计](./docs/architecture.md) | 目录结构、核心组件、请求流程、数据模型 |
| [API 接口](./docs/api-reference.md) | 所有端点列表、请求/响应格式、认证说明 |
| [部署指南](./docs/deployment.md) | 本地开发、生产部署 |
| [配置说明](./docs/configuration.md) | 配置文件格式 |

## 快速开始

### 开发环境运行

```bash
# 1. 下载依赖
go mod download

# 2. 直接运行（开发模式）
go run ./cmd/server/main.go -config ./config.json
```

### 生产环境编译和运行

**重要提示**：本项目使用 Go embed 将静态文件（web/static）和模板文件（web/templates）嵌入到二进制文件中。因此，修改这些文件后必须重新编译才能生效。

```bash
# 1. 编译服务器（将静态文件和模板嵌入到二进制文件）
go build -o orchids-server ./cmd/server

# 2. 运行编译后的服务器
./orchids-server -config ./config.json

# 或者后台运行
nohup ./orchids-server -config ./config.json > server.log 2>&1 &
```

### 修改前端文件后的步骤

如果您修改了以下文件：
- `web/static/` 目录下的任何文件（JS、CSS、HTML等）
- `web/templates/` 目录下的任何模板文件

**必须执行以下步骤**：

```bash
# 1. 停止正在运行的服务器
pkill -f orchids-server

# 2. 重新编译（嵌入更新后的文件）
go build -o orchids-server ./cmd/server

# 3. 重新启动服务器
./orchids-server -config ./config.json
```

### 快速重启脚本

```bash
# 一键重新编译并启动
(pkill -f orchids-server || true) && go build -o orchids-server ./cmd/server && ./orchids-server
```

## 主要特性

1. **多账号管理** - 支持添加、编辑、删除多个 Orchids 账号
2. **负载均衡** - 加权随机算法分配请求
3. **故障转移** - 账号失败时自动切换
4. **模型映射** - 透明映射 Claude 模型到上游模型
5. **工具调用** - 完整支持 Claude Tool Use
6. **流式响应** - SSE 实时响应
7. **Token 计数** - 估算输入/输出 Token
8. **调试日志** - 详细的请求/响应日志
9. **管理界面** - Web UI 管理账号
10. **导入导出** - 账号配置备份恢复

## 项目架构

```
orchids-api/
├── cmd/server/          # 应用入口
│   └── main.go
├── internal/
│   ├── api/             # Admin REST API
│   ├── auth/            # 认证服务
│   ├── clerk/           # Clerk JWT 认证
│   ├── config/          # 配置加载
│   ├── errors/          # 统一错误处理
│   ├── handler/         # HTTP 请求处理器
│   │   ├── handler.go   # 核心处理逻辑
│   │   ├── stream_handler.go  # SSE 流处理
│   │   ├── tool_exec.go       # 工具执行
│   │   └── tools.go           # 工具映射
│   ├── loadbalancer/    # 加权负载均衡
│   ├── middleware/      # HTTP 中间件
│   │   ├── auth.go      # 认证中间件
│   │   ├── concurrency.go  # 并发限制
│   │   └── session.go   # 会话管理
│   ├── orchids/         # Orchids 上游客户端
│   │   ├── client.go    # SSE 客户端
│   │   ├── ws_aiclient.go  # WebSocket 客户端
│   │   ├── fs.go        # 文件系统操作
│   │   └── tool_mapping.go # 工具名称映射
│   ├── upstream/        # 通用上游组件
│   │   ├── wspool.go    # WebSocket 连接池
│   │   ├── breaker.go   # 熔断器
│   │   └── reliability.go # 重试与可靠性
│   ├── warp/            # Warp 上游客户端
│   │   ├── client.go    # Warp API 客户端
│   │   └── session.go   # Warp 会话管理
│   ├── perf/            # 性能优化 (对象池)
│   ├── prompt/          # Prompt 构建与压缩
│   ├── store/           # Redis 数据存储
│   ├── summarycache/    # 会话摘要缓存
│   ├── tiktoken/        # Token 估算
│   └── util/            # 通用工具函数
├── web/                 # 嵌入式静态资源
│   ├── static/          # CSS, JS
│   └── templates/       # HTML 模板
└── docs/                # 文档
```

### 请求流程

```
Client Request
     ↓
[Middleware] → Trace ID → Concurrency Limit → Auth
     ↓
[Handler] → Validate → Select Account (LoadBalancer)
     ↓
[Prompt Builder] → Build Markdown Prompt → Compress History
     ↓
[Upstream Client] → WebSocket/SSE → Orchids Server
     ↓
[Stream Handler] → Parse Events → Build Response → SSE to Client
```

### 核心模块说明

| 模块 | 职责 |
|------|------|
| `orchids/` | Orchids 上游客户端，SSE/WebSocket 通信 |
| `warp/` | Warp 上游客户端 |
| `upstream/` | 通用上游组件：连接池、熔断器、重试 |
| `errors/` | 统一错误码和结构化错误处理 |
| `util/` | 并行处理、重试、可取消休眠等工具 |
| `perf/` | 对象池复用，减少 GC 压力 |

## 运行测试

```bash
# 运行所有测试
go test ./...

# 运行特定模块测试
go test ./internal/errors/...
go test ./internal/util/...
go test ./internal/middleware/...

# 查看覆盖率
go test ./... -cover
```
