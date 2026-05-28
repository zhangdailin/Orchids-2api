# Orchids-2api

[中文](README.md) | [English](README_EN.md)

一个基于 Go 的多通道代理服务，统一暴露 Claude Messages 风格与 OpenAI 兼容接口，当前支持 `orchids`、`warp`、`bolt`、`puter`、`grok` 五类通道。

## 当前状态

- `internal/handler` 统一处理 `orchids` / `warp` / `bolt` / `puter` 的 `/v1/messages` 与 `/v1/chat/completions`
- `internal/grok` 独立处理 `grok` 的 `/v1/chat/completions`、`/v1/images/*`、`/v1/files/*`
- 模型管理支持按通道刷新：`/api/models/refresh`
- Puter 非流式 Claude Messages 已覆盖 `Read`、`Write`、`Edit`、`Delete`、长上下文、多轮 `tool_result` 回归

## 核心能力

- 多账号池与按通道负载均衡
- Claude Messages 兼容接口
- OpenAI Chat Completions 兼容接口
- 通道级模型管理、默认模型与排序
- 管理后台与管理 API
- Redis 持久化存储
- Prometheus 指标与可选 `pprof`
- Grok 图片生成、编辑和本地媒体缓存

## 支持通道

| 通道 | 对外入口 |
|---|---|
| `orchids` | `/orchids/v1/messages`、`/orchids/v1/chat/completions` |
| `warp` | `/warp/v1/messages`、`/warp/v1/chat/completions` |
| `bolt` | `/bolt/v1/messages`、`/bolt/v1/chat/completions` |
| `puter` | `/puter/v1/messages`、`/puter/v1/chat/completions` |
| `grok` | `/grok/v1/chat/completions`、`/grok/v1/images/*`、`/grok/v1/files/*` |

统一模型查询入口：

- `GET /v1/models`
- `GET /v1/models/{id}`

## 文档目录

- [架构设计](docs/architecture.md)
- [架构复核](docs/architecture-review.md)
- [API 参考](docs/api-reference.md)
- [配置说明](docs/configuration.md)
- [部署指南](docs/deployment.md)
- [请求流程](docs/ORCHIDS_API_FLOW.md)
- [Grok 与 grok2api 对齐清单](docs/grok2api-parity-checklist.md)

## 环境要求

- Go `1.24+`
- Redis `7+`
- Windows / Linux / macOS

## 快速开始

### 1. 启动 Redis

```bash
docker run -d --name orchids-redis -p 6379:6379 redis:7
```

### 2. 准备配置

最小可用 `config.json`：

```json
{
  "port": "3002",
  "store_mode": "redis",
  "redis_addr": "127.0.0.1:6379",
  "admin_user": "admin",
  "admin_pass": "change-me",
  "admin_path": "/admin",
  "debug_enabled": true
}
```

说明：

- 未设置 `admin_pass` 时，程序会在启动时自动生成随机密码并打印日志
- 运行后若 Redis 中存在 `settings:config`，会覆盖文件配置

### 3. 启动服务

开发模式：

```bash
go run ./cmd/server -config ./config.json
```

生产模式：

```bash
go build -o orchids-server ./cmd/server
./orchids-server -config ./config.json
```

Windows：

```powershell
go build -o server.exe ./cmd/server
.\server.exe -config .\config.json
```

## 常用命令

运行全部测试：

```bash
go test ./...
```

只跑 Puter 相关回归：

```bash
go test ./internal/handler -run "Puter_"
```

重新编译：

```bash
go build -o orchids-server ./cmd/server
```

查看健康状态：

```bash
curl -s http://127.0.0.1:3002/health
curl -s http://127.0.0.1:3002/v1/models
```

## 模型管理说明

- 管理接口：`POST /api/models/refresh`
- 请求体示例：`{"channel":"puter"}`
- 当前刷新策略是“按来源同步”
- 会把来源中新增的模型写入本地模型表
- 会把本地已存在但来源暂时缺失的模型标记为离线并保留记录
- 不再对每个模型单独做逐个测活；`verified` 表示本轮成功纳入同步集合的数量

当前各通道模型来源：

- `orchids`：账号上游 `/v1/models`，失败时退回公开页面/内置列表
- `warp`：账号 GraphQL 发现结果，失败时退回内置种子
- `bolt`：Bolt 前端 bundle 解析，失败时退回内置种子
- `puter`：Puter 公开模型列表 + 账号 test_mode 保守验证
- `grok`：内置支持列表 + 现存模型 + 账号 console 探测

## Puter 当前对齐点

- `/puter/v1/messages` 非流式响应会保留 `tool_use` content block，不再返回 `content: null`
- `tool_result` follow-up 可以继续返回新的 `tool_use`，也可以正常收敛成最终文本
- 已有回归测试覆盖 `Read`、`Write`、`Edit`、`Delete`、长上下文、多轮 `tool_result`

## 主要公开端点

### Claude Messages 风格

- `POST /orchids/v1/messages`
- `POST /warp/v1/messages`
- `POST /bolt/v1/messages`
- `POST /puter/v1/messages`

### OpenAI Chat Completions 风格

- `POST /orchids/v1/chat/completions`
- `POST /warp/v1/chat/completions`
- `POST /bolt/v1/chat/completions`
- `POST /puter/v1/chat/completions`
- `POST /grok/v1/chat/completions`

### Grok 图片与文件

- `POST /grok/v1/images/generations`
- `POST /grok/v1/images/edits`
- `GET /grok/v1/files/{image|video}/{name}`

## 管理端

- UI：`{admin_path}/`
- 登录：`POST /api/login`
- 账号管理：`/api/accounts*`
- 模型管理：`/api/models*`
- 配置管理：`/api/config*`
- Token 缓存：`/api/token-cache/*`

管理接口认证方式：

- `session_token` cookie
- `Authorization: Bearer <admin_token>`
- `X-Admin-Token: <admin_token>`
- Basic Auth，密码等于 `admin_pass`

## 许可证

本仓库遵循仓库内现有许可策略。
