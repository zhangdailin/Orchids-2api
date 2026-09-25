# Orchids-2api

[中文](README.md) | [English](README_EN.md)

一个基于 Go 的多通道代理服务，统一暴露 Claude Messages 风格与 OpenAI 兼容接口，当前支持 `workbuddy`、`qoder`、`cline`、`grok` 四类通道。

## 当前状态

- `internal/handler` 统一处理 `workbuddy` / `qoder` / `cline` 的 `/v1/messages` 与 `/v1/chat/completions`
- `internal/grok` 仅通过 Build OAuth CLI 上游处理 `grok` 的 Messages、Responses 与 Chat
- 模型管理支持按通道刷新：`/api/models/refresh`
- WorkBuddy 通道对接国际版 `www.workbuddy.ai`，账号级模型目录从 `GET /v3/config` 同步，refreshToken 自动轮换并回写
- Qoder 通道对接 `qoder.com` CLI 设备授权流（**只支持 OAuth 登录，不提供 PAT 入口**），模型目录由 `GET /algo/api/v2/model/list` 读取（复用聊天链路的 COSY 签名，无内置回退），设备 refreshToken 自动轮换并回写
- Cline 通道对接 `api.cline.bot`：WorkOS 设备授权（**只支持 OAuth 登录，不提供手填凭证**）换取 Cline access/refresh token，请求凭据是 `Bearer workos:<accessToken>`，模型目录由 `GET /ai/cline/recommended-models` 读取（无内置回退），refreshToken 自动轮换并回写

## 核心能力

- 多账号池与按通道负载均衡
- Claude Messages 兼容接口
- OpenAI Chat Completions 兼容接口
- 通道级模型管理、默认模型与排序
- 管理后台与管理 API
- Redis 持久化存储
- Prometheus 指标与可选 `pprof`
- Grok Build OAuth 设备登录、模型发现、账单/限速状态与 stored Responses
- 推理 API Key 默认鉴权、模型白名单/RPM/到期策略与 Redis 账号凭据 AES-GCM 加密

## 支持通道

| 通道 | 对外入口 |
|---|---|
| `workbuddy` | `/workbuddy/v1/messages`、`/workbuddy/v1/chat/completions` |
| `qoder` | `/qoder/v1/messages`、`/qoder/v1/chat/completions` |
| `cline` | `/cline/v1/messages`、`/cline/v1/chat/completions` |
| `grok` | `/grok/v1/messages`、`/grok/v1/responses`、`/grok/v1/chat/completions`（仅 Build OAuth CLI） |

统一模型查询入口：

- `GET /v1/models`
- `GET /v1/models/{id}`

## 文档目录

- [架构设计](docs/architecture.md)
- [API 参考](docs/api-reference.md)
- [配置说明](docs/configuration.md)
- [部署指南](docs/deployment.md)

历史复核与修复记录统一通过 Git 历史查阅；当前行为以以上专题文档为准。

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

从安全示例创建本地配置（`config.json` 不纳入 Git）：

```bash
cp config.example.json config.json
```

最小可用配置：

```json
{
  "port": "3002",
  "store_mode": "redis",
  "redis_addr": "127.0.0.1:6379",
  "admin_user": "admin",
  "admin_pass": "",
  "admin_path": "/admin",
  "inference_auth_enabled": true,
  "credential_encryption_key_file": "data/credential.key",
  "response_store_ttl_hours": 720,
  "debug_enabled": false
}
```

说明：

- 未设置 `admin_pass` 时，程序会在启动时自动生成随机密码并打印日志
- 生产部署建议显式设置高强度 `admin_pass`，并保持 `debug_enabled` 为 `false`
- 运行后若 Redis 中存在 `settings:config`，会覆盖文件配置
- 首次启动会生成 `data/credential.key`；该文件必须和 Redis 数据一起持久化、备份，丢失后无法解密账号凭据
- 登录管理端创建 API Key 后，使用 `Authorization: Bearer <API Key>` 调用模型和推理接口；Anthropic SDK 也可使用 `x-api-key`

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

重新编译：

```bash
go build -o orchids-server ./cmd/server
```

查看健康状态：

```bash
curl -s http://127.0.0.1:3002/health
curl -s http://127.0.0.1:3002/v1/models -H 'Authorization: Bearer sk-...'
```

## 安全检查（CI 与本地复现）

CI 在 `.github/workflows/ci.yml` 中把三类检查拆成独立任务，本地逐条复现：

```bash
# 数据竞争检测（需要 gcc，CGO_ENABLED=1）
go test -race -count=1 -p 1 -timeout 15m ./...

# 可达漏洞扫描：只报告代码真正调用到的漏洞，命中即失败
go install golang.org/x/vuln/cmd/govulncheck@v1.8.0
govulncheck -scan=symbol ./...

# 依赖审计
go mod verify                                    # 模块缓存与 go.sum 哈希一致
go mod tidy -diff                                # 有差异即退出码非 0（不修改文件）
go list -m -retracted all | grep -i retracted    # 命中被撤回版本即失败
go list -m -u all                                # 可升级清单，仅信息、不阻断
```

说明：

- `govulncheck` 在 push/定时任务上还会输出 SARIF 上传到 GitHub Security 标签页；fork PR 只有只读令牌，上传失败不会让 CI 变红。
- 漏洞是在代码发布**之后**才被披露的，所以这几个安全任务除 push/PR 外，每周一 03:37 也会在 `main` 上重跑一次。
- 依赖升级由 Dependabot（`.github/dependabot.yml`）分组提 PR，CI 只负责「当前版本现在有没有问题」，不阻断升级评审。

## 模型管理说明

- 管理接口：`POST /api/models/refresh`
- 请求体示例：`{"channel":"workbuddy"}`
- 当前刷新策略是“按来源同步”，不同通道按各自上游能力验证
- `verified` 表示本轮通过通道验证并纳入同步集合的数量

当前各通道模型来源：

- `workbuddy`：账号级 `GET /v3/config` 的 `cli` agent 白名单（鉴权成功即视为验证通过，不额外消耗额度）
- `grok`：Build OAuth 账号的 `GET /v1/models` 上游发现结果

## WorkBuddy 当前对齐点

- 对接国际版 `www.workbuddy.ai`（`isOversea=true`），基址 `/v2/chat/completions`
- **官方浏览器登录**：管理页面 WorkBuddy 平台 → 添加账号 → **手动点击**「使用 WorkBuddy 官方网页登录」。打开弹窗不会自动发起登录，也不会自动跳转；编辑已有账号时同样保留该按钮，用于重新授权（无需删除账号）。服务端只申请上游登录事务并轮询换取 token，不接触密码；授权后自动同步该账号模型目录与额度，读不到目录就不落库
- 上游强制 `stream: true`，且要求 `messages[0]` 为 system（否则 `400 code=11128`）；客户端会自动按此组包，并把 `developer` 归一到 `system`
- `tool_choice` 只接受字符串，工具调用以 OpenAI 增量 `tool_calls` 形式回流并聚合为完整调用
- 业务错误在 200 信封内返回：`6004` 为该模型频率限制（账号其它模型仍可用），`12153` 为会话失效需重新登录
- 账号凭据只存放于 `workbuddy_access_token` / `workbuddy_refresh_token` 专用字段，refreshToken 不下发到管理页面；Keycloak 每次刷新都会轮换 refreshToken，服务器自动持久化新值
- **额度（真实计量）**：账号状态同步会调用 `POST /v2/billing/meter/get-user-resource`（`p_tcaca`）。账号表格里的「等级」显示上游计量包名（如 `Free Plan Subscription` / `Bonus Pack`），「配额」显示当前周期剩余/上限（如 `147.28 / 350`，上游支持小数），并给出周期重置时间；「调用」在无请求计数的该通道下显示计量已消耗额度。多个计量包会按同一周期聚合
- `quota_*` 字段合并进所有账号响应（列表/创建/编辑/检查）
- **额度用尽不摘除调度**：计量包/积分用完后 WorkBuddy 仍可正常使用免费模型，所以账号同步不再把「额度用尽」写成 `402`（旧行为会让整个账号进入支付冷却 24 小时，免费模型跟着一起下线）。剩余额度仍照常写进配额列（显示 `0 / 350` 与周期重置时间）；若上游对某个付费模型返回 `402`，只对该模型做短冷却，账号继续留在调度池里。历史遗留的 `402` 标记会在下一次账号检查或调度选取时自动释放
- **账号页手动同步**：打开/刷新账号管理页只读取现有账号状态，不会自动调用上游检查接口或逐行重绘。需要更新额度、状态或模型信息时，使用账号行上的刷新操作；后台服务仍会按配置执行凭据与健康状态维护
- 「账号 / 邮箱」列对 WorkBuddy 显示登录邮箱（由 accessToken 的 Keycloak claims 推导，官方登录与手填会话 JSON 一致）
- 手填方式仅保留在 API 层（用于迁移/脚本化导入）：`POST /api/accounts` 仍接受 `client_cookie` 里的会话 `refreshToken` 或整段 auth JSON；管理页面不再暴露该通道的凭证输入框

当前种子型号（`cli` 白名单，20 个）：`default-model`、`fast-model`、`balanced-model`、`primary-model`、`deep-model`、`deepseek-v4.1-flash`、`gpt-6-astra`、`hy4-preview-f`、`hy3`、`gpt-5.6-sol`、`gpt-5.6-terra`、`gpt-5.6-luna`、`gpt-5.5`、`gpt-5.4`、`gpt-5.3-codex`、`gemini-3.5-flash`、`glm-5.3`、`glm-5.2`、`kimi-k3`、`kimi-k2.6`。

可选的在线联调（模型目录与对话会真实消耗账号额度，默认跳过）：

```bash
# 只需出网（无凭证）：验证官方登录事务引导
go test -count=1 -v -run TestLive_StartAuthLogin ./internal/workbuddy/live/

# 需要凭证：验证模型目录、流式对话与工具调用
WB_LIVE=1 WB_AUTH_FILE=/path/to/auths/workbuddy-<uid>.json go test ./internal/workbuddy/live/ -v
```

## 主要公开端点

### Claude Messages 风格

- `POST /workbuddy/v1/messages`
- `POST /grok/v1/messages`

### OpenAI Chat Completions 风格

- `POST /workbuddy/v1/chat/completions`
- `POST /grok/v1/chat/completions`

### OpenAI Responses 风格

- `POST /grok/v1/responses`
- `POST /grok/v1/responses/compact`
- `GET /grok/v1/responses/{response_id}`
- `DELETE /grok/v1/responses/{response_id}`
- `POST /responses/{response_id}/cancel`
- `GET /responses/{response_id}/input_items`

统一前缀 `/v1` 下以上端点对所有渠道的模型可用：Grok 模型走原生实现，WorkBuddy / Qoder / Cline 由 Responses→Chat 桥接提供；`cancel` 与 `input_items` 在四个前缀（含 `/grok/v1`）下均已注册，读取同一个 response store。未配置 Redis 时桥接回退到进程内存储并输出 WARN 日志，多副本部署必须配置 Redis。

Build stored Responses 会按客户端 API Key 隔离，并固定回创建该 Response 的 OAuth 账号；归属记录默认保留 720 小时。详见 [docs/api-reference.md](docs/api-reference.md#13-openai-responses-风格)。

## Grok Build OAuth CLI

Grok 只使用 `cli-chat-proxy.grok.com/v1` Build 上游。账号必须通过管理端设备授权创建，服务端保存并自动刷新 OAuth access/refresh token；不接受 Cookie 或手填 token。

设备登录：

- `POST /api/grok/device-auth`：创建事务，返回 `verification_uri`、`verification_uri_complete`、`user_code` 与过期时间
- `GET /api/grok/device-auth/{id}`：轮询，完成后返回新建或更新的账号 ID
- `DELETE /api/grok/device-auth/{id}`：取消事务

以上端点要求管理会话并遵循同源保护。模型由 Build 账号的 `GET /v1/models` 实际发现。

### Build 配置（config.json / Redis）

- `grok_cli_base_url`
- `grok_cli_user_agent` / `grok_cli_client_version` / `grok_cli_client_identifier`
- `grok_cli_oauth_client_id` / `grok_cli_oauth_device_url` / `grok_cli_oauth_token_url`
- `grok_cli_model_ids`（可选的 CLI 模型路由补充）
- `grok_build_timeout_seconds` / `grok_build_stream_idle_seconds` / `grok_build_rps`
- `response_store_ttl_hours`（stored Response 账号归属记录 TTL，默认 720 小时）

这些字段可由配置文件或管理端持久化。通用 `proxy_http` / `proxy_https`、prompt/token 缓存和渠道配置仍适用于 Build 请求。

## 管理端

- UI：`{admin_path}/`
- 登录：`POST /api/login`
- 账号管理：`/api/accounts*`
- 模型管理：`/api/models*`
- 配置管理：`/api/config*`
- Token 缓存：`/api/token-cache/*`

管理接口认证方式：

- `session_token` cookie
- Basic Auth，密码等于 `admin_pass`

模型与推理接口默认要求管理端创建的 API Key。管理端可为每个 Key 设置允许模型、每分钟请求数和到期时间；旧 Key 默认不限制这些策略。仅在已有可信上游网关负责认证时，才设置 `inference_auth_enabled=false`。

### Grok 模型来源

`/api/models` 与 `/grok/v1/models` 均来自 Build OAuth 账号的上游能力目录。刷新读取账号自己的 `GET /v1/models`，返回 `source=grok_build_models`；上游新增或撤回会在下次成功刷新时同步。没有启用的 Build OAuth 账号时不会发布 Grok 模型。

## 许可证

本仓库遵循仓库内现有许可策略。
