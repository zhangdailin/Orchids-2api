# 部署指南

本文档以当前代码实现为准，适用于 `orchids`、`warp`、`puter`、`grok` 四类通道。

## 1. 前置条件

- Go `1.24+`
- Redis `7+`
- 已准备好 `config.json`

最小配置示例见 [README.md](/D:/Code/Orchids-2api/README.md) 与 [configuration.md](/D:/Code/Orchids-2api/docs/configuration.md)。

注意：

- 启动后若 Redis 中已有 `settings:config`，会覆盖文件配置
- 未设置 `admin_pass` 时，程序会自动生成随机密码并写入启动日志

## 2. 本地开发启动

```bash
go mod download
go run ./cmd/server -config ./config.json
```

## 3. 生产编译与启动

### 3.1 Linux / macOS

```bash
go build -o orchids-server ./cmd/server
./orchids-server -config ./config.json
```

后台运行：

```bash
nohup ./orchids-server -config ./config.json > server.log 2>&1 &
```

### 3.2 Windows

```powershell
go build -o server.exe ./cmd/server
.\server.exe -config .\config.json
```

后台运行：

```powershell
Start-Process -FilePath .\server.exe -ArgumentList '-config','.\config.json'
```

## 4. 重启流程

### 4.1 Linux / macOS

```bash
pkill -f "./orchids-server -config ./config.json" || true
go build -o orchids-server ./cmd/server
nohup ./orchids-server -config ./config.json > server.log 2>&1 &
```

### 4.2 Windows

```powershell
Get-Process server -ErrorAction SilentlyContinue | Stop-Process -Force
go build -o server.exe ./cmd/server
Start-Process -FilePath .\server.exe -ArgumentList '-config','.\config.json'
```

## 5. 启动后验证

基础检查：

```bash
curl -s http://127.0.0.1:3002/health
curl -s http://127.0.0.1:3002/v1/models
curl -s http://127.0.0.1:3002/metrics
```

端口检查：

```bash
lsof -iTCP:3002 -sTCP:LISTEN -n -P
```

Windows：

```powershell
Get-NetTCPConnection -LocalPort 3002 -ErrorAction SilentlyContinue
```

模型同步验证：

- 登录管理端后调用 `POST /api/models/refresh`
- 当前刷新是“按来源同步”：新增即写入，来源消失即删除，不再逐个模型测活

建议回归：

```bash
go test ./...
go test ./internal/handler -run "Puter_"
```

## 6. 可观测性与排障

调试入口：

- `GET /health`
- `GET /metrics`
- `GET /debug/pprof/`，仅 `debug_enabled=true` 且管理认证通过时可访问

Linux / macOS 日志：

```bash
tail -n 200 server.log
```

Windows 日志通常取决于你的启动方式；若前台启动，直接查看控制台输出即可。

重点关注：

- `model not found`
- `no available grok token`
- `Bad Gateway`
- `stream parse error`

## 7. 升级建议

每次升级后至少执行：

```bash
go test ./...
go build -o orchids-server ./cmd/server
```

若当前版本重点涉及 Puter 或模型刷新逻辑，建议额外执行：

```bash
go test ./internal/handler -run "Puter_"
```
