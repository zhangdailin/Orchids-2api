# 部署指南

本文档以当前代码实现为准，适用于 `warp`、`puter`、`grok` 三类通道。

## 1. 前置条件

- Go `1.24+`
- Redis `7+`
- 已通过 `cp config.example.json config.json` 准备好本地配置

最小配置示例见 [README.md](../README.md) 与 [configuration.md](../docs/configuration.md)。

注意：

- 启动后若 Redis 中已有 `settings:config`，会覆盖文件配置
- 未设置 `admin_pass` 时，程序会自动生成随机密码并写入启动日志
- 首次启动会创建 `data/credential.key` 并迁移 Redis 中的账号凭据；生产环境必须持久化和备份该文件

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
curl -s http://127.0.0.1:3002/v1/models -H 'Authorization: Bearer sk-...'
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
- 当前刷新是“按来源同步”：新增即写入、来源消失即删除；Puter 还会执行账号 `test_mode` 逐模型验证

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

凭据解密报错通常表示 `data/credential.key` 没有随 Redis 一起恢复，或启动时使用了不同的 `ORCHIDS_CREDENTIAL_ENCRYPTION_KEY`。不要生成新密钥覆盖旧文件。

### 6.1 运维总览的统计口径

`/admin/` 的运维总览每分钟落一个 Redis 桶（保留 8 天，趋势查询上限 24 小时），数字的口径如下，避免误读：

| 项目 | 口径 |
|---|---|
| 请求数 / 成功率 | 只统计渠道前缀（`warp`/`puter`/`workbuddy`/`grok`）。`http`（管理页、健康检查、扫描器）与 `probe`（自己的探测）被计入聚合但排除在矩阵与总数之外，页面用 `excluded_aggregates` 说明 |
| 速率（RPM） | 按所选**窗口长度**计算，而不是按存在数据的桶数；60 分钟里 1 次请求显示 1/60 而不是 1 |
| 延迟 P95 | 合并视图会收集各渠道的原始样本后统一计算（百分位不可相加）；`samples` 为 0 表示没有样本，页面显示“暂无样本”而不是健康的 0 |
| 首字延迟 | 从**首个有效载荷字节**算起，提交响应头与 SSE keepalive 注释都不计入；非流式响应等于整个耗时 |
| 失败判定 | HTTP 状态类之外，已提交 2xx 之后中途失败的流记为 `stream_error` 并计为失败（客户端看到的仍是 200） |

告警按 30 分钟窗口每分钟评估一次：成功率低于 90% 需要窗口内至少 3 次失败才会告警（低于 50% 的严重故障不受该次数下限约束），低于 5 次请求不告警；恢复线为阈值 +3 个百分点，避免在阈値附近反复“告警/恢复”。`http` 与 `probe` 永不参与告警。

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
