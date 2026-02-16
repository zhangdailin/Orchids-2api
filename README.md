# Orchids-2api

一个基于 Go 的多通道代理服务，统一暴露兼容 Claude / OpenAI 风格接口，支持 `orchids`、`warp`、`grok` 三类上游账号池与自动切换。

## 核心能力

- 多账号池 + 负载均衡（按通道选账号，失败自动切换）
- 统一模型管理（`/v1/models` + 通道模型路由）
- Claude Messages 风格接口（`/orchids/v1/messages`、`/warp/v1/messages`）
- OpenAI Chat Completions 兼容接口（含 grok）
- Grok 图像生成/编辑与本地媒体缓存（解决外链不可达）
- Web 管理界面 + 管理 API
- Prometheus 指标、可选 pprof

## 文档目录

- [架构设计](docs/architecture.md)
- [API 参考](docs/api-reference.md)
- [配置说明](docs/configuration.md)
- [部署指南](docs/deployment.md)
- [请求流程](docs/ORCHIDS_API_FLOW.md)

## 环境要求

- Go 1.22+
- Redis（必需，当前仅支持 Redis 存储）

## 快速开始

### 1) 准备 Redis

```bash
docker run -d --name orchids-redis -p 6379:6379 redis:7
```

### 2) 准备配置

最小可用 `config.json` 示例：

```json
{
  "port": "3002",
  "store_mode": "redis",
  "redis_addr": "127.0.0.1:6379",
  "admin_user": "admin",
  "admin_pass": "admin123",
  "admin_path": "/admin"
}
```

### 3) 启动

开发模式：

```bash
go run ./cmd/server/main.go -config ./config.json
```

生产模式：

```bash
go build -o orchids-server ./cmd/server
./orchids-server -config ./config.json
```

后台运行：

```bash
nohup ./orchids-server -config ./config.json > server.log 2>&1 &
```

## 常用命令

重新编译并重启：

```bash
pkill -f "./orchids-server -config ./config.json" || true
go build -o orchids-server ./cmd/server
nohup ./orchids-server -config ./config.json > server.log 2>&1 &
```

查看日志：

```bash
tail -n 200 server.log
```

运行测试：

```bash
go test ./...
```

## 主要公开端点

- `POST /orchids/v1/messages`
- `POST /warp/v1/messages`
- `POST /orchids/v1/chat/completions`
- `POST /warp/v1/chat/completions`
- `POST /grok/v1/chat/completions`
- `POST /grok/v1/images/generations`
- `POST /grok/v1/images/edits`
- `GET /grok/v1/files/{image|video}/{name}`
- `GET /v1/models`
- `GET /health`
- `GET /metrics`

详细请求/响应见 [API 参考](docs/api-reference.md)。

## 管理端

- UI：`{admin_path}/`（默认 `/admin`）
- 登录接口：`POST /api/login`
- 账号、模型、配置、缓存管理接口：`/api/*`

> 管理接口默认走 session cookie，也支持 `admin_token`（`Authorization: Bearer <token>` 或 `X-Admin-Token`）。

## Grok 图片链路说明

- 生成/编辑结果会优先转成本地可访问地址：`/grok/v1/files/image/*`
- 缓存目录：`data/tmp/image`、`data/tmp/video`
- 外部 `assets.grok.com` 无法直连时，仍可通过本地缓存链接展示

## 常见问题

1. `model not found`
- 先调用 `GET /grok/v1/models` 或 `GET /v1/models` 确认模型名。
- 常见输入错误：`gork-3`（拼写错误）应为 `grok-3`。

2. grok 图片不显示
- 检查返回是否为 `/grok/v1/files/image/...`
- 检查本地文件是否存在：`data/tmp/image`

3. 启动后端口未监听
- 检查配置端口和进程：`lsof -iTCP:3002 -sTCP:LISTEN -n -P`

## 许可证

本仓库遵循仓库内现有许可策略。
