# 部署指南

## 1. 前置条件

- Go 1.22+
- Redis 7+
- 已准备好 `config.json`

## 2. 本地开发启动

```bash
go mod download
go run ./cmd/server/main.go -config ./config.json
```

## 3. 生产编译与启动

```bash
go build -o orchids-server ./cmd/server
./orchids-server -config ./config.json
```

后台方式：

```bash
nohup ./orchids-server -config ./config.json > server.log 2>&1 &
```

## 4. 重启流程（推荐）

```bash
pkill -f "./orchids-server -config ./config.json" || true
go build -o orchids-server ./cmd/server
nohup ./orchids-server -config ./config.json > server.log 2>&1 &
```

## 5. 健康与可观测性

```bash
curl -s http://127.0.0.1:3002/health
curl -s http://127.0.0.1:3002/metrics | head
lsof -iTCP:3002 -sTCP:LISTEN -n -P
```

若启用了 `debug_enabled=true`，可使用：

- `/debug/pprof/`（需管理认证）

## 6. 日志排查

```bash
tail -n 200 server.log
```

关注以下关键字：

- `model not found`：模型名错误或未启用
- `no available grok token`：grok 账号不可用
- `Bad Gateway` / `stream parse error`：上游返回异常

## 7. 升级建议

每次升级后至少执行：

```bash
go test ./...
go build -o orchids-server ./cmd/server
```

然后按第 4 节流程重启。
