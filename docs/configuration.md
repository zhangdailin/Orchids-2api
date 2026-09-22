# 配置说明

## 1. 加载规则

首次运行先复制安全示例；生成的 `config.json` 不纳入 Git：

```bash
cp config.example.json config.json
```

配置加载顺序：

1. 启动参数 `-config` 指定文件
2. 若未指定，则按顺序查找 `config.json` -> `config.yaml` -> `config.yml`
3. 读取文件并应用默认值
4. 若 Redis 中存在 `settings:config`，则以 Redis 中保存的配置覆盖文件配置
5. 最后始终执行 `config.ApplyHardcoded()`，把一批运行时固定值重新写入

说明：

- YAML 仅支持扁平 `key: value`
- 不是所有历史字段都还能通过配置文件生效

## 2. 配置文件可设置的字段

下面这些字段会从配置文件或管理接口持久化到 Redis。

### 2.1 服务与管理端

| 字段 | 默认值 | 说明 |
|---|---|---|
| `port` | `3002` | 服务监听端口 |
| `debug_enabled` | `false` | 采集新推理请求的诊断内容，在请求日志详情中查看 |
| `verbose_diagnostics` | `false` | 详细诊断日志 |
| `admin_user` | `admin` | 管理端用户名 |
| `admin_pass` | 自动生成 | 管理端密码，建议显式设置 |
| `admin_path` | `/admin` | 管理端路径 |
| `admin_token` | 空 | 管理端静态 token |
| `inference_auth_enabled` | `true` | 模型和推理接口是否要求管理 API Key |
| `trusted_proxies` | `[]` | 允许提供转发头的反向代理 IP/CIDR；为空时忽略并剥离所有外部 `Forwarded`/`X-Forwarded-*`/`X-Real-IP` |
| `credential_encryption_key_file` | `data/credential.key` | Redis 账号凭据 AES-GCM 主密钥文件；只持久化路径，不持久化密钥内容 |
| `response_store_ttl_hours` | `720` | Build stored Response 账号归属记录的 Redis TTL（小时） |
| `grok_console_base_url` | `https://console.x.ai/v1` | Grok Console Responses、标准视频、TTS、STT 和 Realtime 的 DPoP 上游基址 |
| `grok_cli_fallback_base_url` | `https://api.x.ai/v1` | Build 视频主路由返回确认的 403 后使用的 XAI fallback 基址 |

### 2.2 Redis

| 字段 | 默认值 | 说明 |
|---|---|---|
| `store_mode` | `redis` | 当前仅支持 `redis` |
| `redis_addr` | 空 | Redis 地址，例如 `127.0.0.1:6379` |
| `redis_password` | 空 | Redis 密码 |
| `redis_db` | `0` | Redis DB |
| `redis_prefix` | `orchids:` | Redis key 前缀 |

### 2.3 媒体与多实例

| 字段 | 默认值 | 说明 |
|---|---|---|
| `media_dir` | `data/tmp` | 图片、视频成品和临时媒体输入目录；修改后需要重启 |
| `deployment_replicas` | `1` | 当前部署的服务副本数 |
| `deployment_instance_id` | 空 | 多副本时必填，并且每个副本必须使用不同的稳定值 |
| `deployment_cluster_id` | `orchids` | 同一 Redis 和共享媒体目录所属的集群标识 |
| `shared_media` | `false` | 多副本时必须显式设为 `true`，表示 `media_dir` 已挂载为所有副本共享的可读写目录 |

当 `deployment_replicas > 1` 时，启动检查要求使用 Redis、填写实例 ID 并确认共享媒体。服务会在媒体根目录创建 `.orchids-cluster` 标记，并执行临时文件写入与回读；目录属于其他集群、不可写或无法回读时会拒绝启动。所有副本应使用相同的 `deployment_cluster_id` 和媒体目录布局，但使用不同的 `deployment_instance_id`。

### 2.4 缓存

| 字段 | 默认值 | 说明 |
|---|---|---|
| `cache_token_count` | `false` | 是否缓存 token 计数 |
| `cache_ttl` | `5` | 通用缓存 TTL（分钟） |
| `cache_strategy` | `mix` | 缓存策略 |
| `enable_token_cache` | `false` | 是否启用 token cache |
| `token_cache_ttl` | `300` | token cache TTL（秒） |
| `token_cache_strategy` | `1` | token cache 策略 |

### 2.5 代理

| 字段 | 默认值 | 说明 |
|---|---|---|
| `proxy_http` | 空 | HTTP 代理 |
| `proxy_https` | 空 | HTTPS 代理 |
| `proxy_user` | 空 | 代理用户名 |
| `proxy_pass` | 空 | 代理密码 |
| `proxy_bypass` | 空数组 | 直连域名或网段 |

`grok_egress_enabled=true` 时，HTTP、SOCKS5/SOCKS5H 节点同时用于普通请求和 Console Voice WebSocket；节点选择、UA、Cloudflare cookies 与连接租约保持同一绑定。公网反向代理必须把自身地址加入 `trusted_proxies`，不要填写任意客户端可达的地址段。

### 2.6 上游保真

本网关是 API 中转站，默认**逐字透传**客户端内容、不对报文内容做任何改写。以下字段可显式开启历史改写行为：

| 字段 | 默认值 | 说明 |
|---|---|---|
| `cc_entrypoint_mode` | `keep` | `keep`=系统内容原样透传（默认）；`auto`=仅剥离 `claude-vscode`/`claude-code` 入口值；`strip`=剥离全部 `cc_entrypoint=` 并过滤 Claude Code 系统/环境项 |

这些字段会被持久化，不会被 `ApplyHardcoded()` 覆盖。

## 3. 运行时限制与固定默认值

### 3.1 可配置的重试、超时与 Grok 限速

以下字段可通过配置文件/Redis 持久化；`ApplyHardcoded()` 不再覆盖有效值。整数设置省略或小于等于 0 时使用默认值，超过上限时钳制到上限。配置修改后重启服务，使 HTTP 客户端与入口中间件一致更新。

| 字段 | 默认值 | 上限/含义 |
|---|---|---|
| `max_retries` | `3` | 单次 HTTP 请求最大重试次数，上限 20 |
| `retry_delay` | `1000` | 重试基准延迟，毫秒，上限 60000 |
| `account_switch_count` | `5` | Grok 账号尝试次数，包含首轮，上限 20；不再误作秒数 |
| `qoder_oauth_base_url` | `https://qoder.com` | Qoder 设备授权页面地址；配置的 host 会自动加入授权页允许列表 |
| `qoder_openapi_base_url` | `https://openapi.qoder.sh` | Qoder 设备 token 与 userinfo 控制面地址 |
| `qoder_inference_base_url` | `https://api2.qoder.sh` | Qoder 聊天 SSE 地址（CN 网关可用 `https://gateway.qoder.com.cn`） |
| `qoder_client_id` | 内置 CLI 公共 client id | 设备授权 client id；非机密，可随 CLI 升级替换 |
| `qoder_client_version` | `1.1.34` | 发送的 `Cosy-Version` / `User-Agent` 版本号 |
| `cline_api_base_url` | `https://api.cline.bot/api/v1` | Cline API 地址（登录交换、模型目录、聊天） |
| `cline_workos_client_id` | 内置 CLI 公共 client id | WorkOS 设备授权 client id；非机密 |
| `cline_workos_authorize_url` | `https://api.workos.com/user_management/authorize/device` | WorkOS 设备授权地址；配置的 host 会自动加入授权页允许列表 |
| `cline_workos_token_url` | `https://api.workos.com/user_management/authenticate` | WorkOS 设备 token 轮询地址 |
| `request_timeout` | `600` | 通用请求超时，秒，上限 86400 |
| `concurrency_timeout` | 跟随 `request_timeout` | 入口请求执行超时，秒，上限 86400；不是单纯排队等待时间 |
| `retry_429_interval` | `60` | 无精确 reset 信息时的 Web 429 重试间隔，秒，上限 3600 |
| `grok_web_timeout_seconds` | 跟随 `request_timeout` | Web HTTP 总超时，含响应体读取，上限 86400 秒 |
| `grok_console_timeout_seconds` | 跟随 `request_timeout` | Console HTTP 总超时，含响应体读取，上限 86400 秒 |
| `grok_build_timeout_seconds` | 跟随 `request_timeout` | Build HTTP 总超时，含响应体读取，上限 86400 秒 |
| `grok_stream_idle_seconds` | `120` | Build/Console SSE 有效输出空闲超时，上限 3600 秒；keepalive 不重置计时 |
| `warp_stream_idle_seconds` | `300` | Warp 响应体连续无字节空闲超时，上限 3600 秒；有持续输出的长任务不受影响 |
| `puter_stream_idle_seconds` | `120` | Puter NDJSON 响应体连续无字节空闲超时，上限 3600 秒；非法或未知事件会按协议错误记录 |
| `grok_web_rps` / `grok_console_rps` / `grok_build_rps` | `0` | 0 关闭主动限速；正数按账号/团队限速，范围 0.01–1000，每个桶 burst=1 |

限流状态按 provider、账号/已知团队、模型隔离；真实 429 冷却不随主动限速关闭。优先使用 `Retry-After`，再使用响应中的 reset 信息；信息缺失时只冷却受影响的账号/模型，不再全局停顿。配置 Redis 时，Grok 主动 pacing 和团队/模型冷却会跨副本共享；Redis 暂时不可用时退化到进程内 pacing。

总超时与空闲超时是不同边界。长回答需要同时满足入口 `concurrency_timeout` 和目标 provider HTTP 超时。中转层已删除思考质量门控、额外质量重试和缺失思考惩罚；历史 `grok_quality_*` / `grok_missing_thinking_cooldown_seconds` 配置不再生效。存储会话仍保留账号绑定。

未显式设置账号 `max_concurrent` 时，所有 provider（WorkBuddy、Warp、Puter、Qoder、Grok）默认每账号 10 路；显式正数会覆盖默认值，未知账号类型不受限。单一账号的渠道因此不再因为只有 1 路而把并发请求判成过载。Redis 部署使用带过期与续租的分布式连接租约，进程异常退出后遗留计数会自动回收。

Grok 直连与托管 egress 均使用以上 provider 超时，不再受 egress 固定 120 秒总超时或共享客户端 HTTP/1 固定 120 秒响应头上限影响；等待响应头仍受 HTTP 总超时约束。其他模型继续使用原有共享客户端策略。

### 3.2 仍然固定的默认值

这些值由 [config.go](../internal/config/config.go) 里的 `ApplyHardcoded()` 强制覆盖，不能指望仅靠配置文件改变。

| 字段 | 当前值 | 说明 |
|---|---|---|
| `output_token_mode` | `final` | 输出 token 统计模式 |
| `context_max_tokens` | 不生效 | 旧兼容字段；不在中转层截断或自动压缩上下文 |
| `context_summary_max_tokens` | 不生效 | 旧兼容字段；不生成中转层摘要 |
| `context_keep_turns` | 不生效 | 旧兼容字段；不按轮数删除请求历史 |
| `grok_api_base_url` | `https://grok.com` | Grok 基础地址 |
| `warp_disable_tools` | `false` | Warp 工具默认开启 |
| `stream` | `true` | Chat 默认流式 |
| `image_nsfw` | `true` | 公共 imagine 默认 NSFW 开启 |
| `public_enabled` | `true` | 公共页面默认开启 |
| `image_final_min_bytes` | `100000` | imagine 最终图阈值 |
| `image_medium_min_bytes` | `30000` | imagine 中间图阈值 |
| `token_refresh_interval` | `1` | token 自动刷新间隔（分钟） |
| `auto_refresh_token` | `true` | 自动刷新账号 token |
| `load_balancer_cache_ttl` | `5` | 负载均衡缓存 TTL（秒） |
| `concurrency_limit` | `100` | 并发上限 |
| `adaptive_timeout` | `true` | 自适应超时 |

### 3.3 上下文与长会话相关字段

| 字段 | 默认值 | 说明 |
|---|---|---|
| `session_ttl_minutes` | `720`（12 小时） | 客户端会话可空闲多久仍续用上游会话绑定。过短会在轮次之间断开绑定，下一轮只能重发整段 transcript。上限 43200（30 天） |
| `warp_stateless_history_max_chars` | `8388608`（8 MiB） | 无服务端会话 ID 的 Warp 请求所渲染 transcript 的**传输**上限，不是上下文策略：上游按模型自身窗口处理收到的内容。旧值 48 KiB（约 12k token）会让 1M 窗口的模型表现得像 16k。上限 67108864 |

模型窗口是**观测值**，不是配置项：`GET /v1/models` 与 `GET /v1/models/{id}` 会按渠道已经观测到的目录回报
`context_length` / `max_input_tokens` / `max_output_tokens`；未观测到的模型**不输出**这些字段（而不是输出 0），
以免客户端按一个编造的数字做预算。数据来源：

| 渠道 | 来源 |
|---|---|
| Qoder | 账号快照 `qoder_model_ids[]` 的 `max_input_tokens` |
| Cline | 暂无可信来源，不输出（推荐模型目录只声明 id） |
| WorkBuddy | 账号快照 `workbuddy_model_ids[]` 的 `max_input_tokens` / `max_output_tokens` |
| Warp | 账号模型发现缓存的 `context_windows`（即上游 `contextWindow.max`） |
| Grok | Codex catalog 的静态窗口表 |
| Puter | 暂无可信来源，不输出 |

服务端从不按 token 裁剪请求历史：`puter` / `workbuddy` / `qoder` / `cline` 全量透传客户端 `messages`。

### 3.4 工具定义保真

客户端声明的工具定义按原样转发，中转层只做必要的结构解析，不改写内容：

| 位置 | 行为 | 说明 |
|---|---|---|
| 工具 schema | 逐字透传 | 不再按内置工具白名单删字段、不再丢弃 `additionalProperties` / `oneOf` / `$schema` 等关键字、不再因超过 4 KiB 就替换成空对象 |
| 工具描述 | 逐字透传（仅去首尾空白） | 上限 64 KiB 仅作传输保护，达到上限才是病态请求 |
| 工具数量 | 上限 256 | 仅作传输保护；旧值 32 会静默丢弃第 32 个之后的工具 |
| 工具 token 估算 | 按实际发送的定义计算 | `count_tokens` 与前置 usage 不再按"压缩后的投影"少报（旧实现：24 个工具 / 128 字符描述 / 4 KiB schema） |

历史里唯一被丢弃过的内容是**旧版**的 48 KiB 无状态 transcript 上限，现已提高为
`warp_stateless_history_max_chars`（见 3.3）。

## 4. 最小可用配置

```json
{
  "port": "3002",
  "store_mode": "redis",
  "redis_addr": "127.0.0.1:6379",
  "admin_user": "admin",
  "admin_pass": "change-me",
  "admin_path": "/admin",
  "inference_auth_enabled": true,
  "credential_encryption_key_file": "data/credential.key",
  "response_store_ttl_hours": 720,
  "debug_enabled": true
}
```

## 5. 配置保存入口

管理端有两套常用配置接口：

| 路径 | 方法 | 说明 |
|---|---|---|
| `/api/config` | GET/POST | 直接读取 / 覆盖整个配置对象 |
| `/api/config/list` | GET | 读取管理端表单配置 |
| `/api/config/save` | POST | 以 patch 方式保存管理端表单配置 |

## 6. 注意事项

- `admin_pass` 若留空，会在启动时自动生成随机密码并写日志
- 配置保存在 Redis 后，后续重启会优先使用 Redis 版本
- 可用 `ORCHIDS_CREDENTIAL_ENCRYPTION_KEY` 提供 Base64、Hex 或 32 字节原始主密钥；环境变量优先于密钥文件
- Warp 登录与账号 token 刷新需要设置 `ORCHIDS_WARP_FIREBASE_API_KEY`；该值只从进程环境读取，不写入配置文件、Redis 或管理 API
- `/health` 会报告 Warp 配置状态；`/ready/warp` 在 Firebase API key 缺失时返回 503，便于部署系统在接流量前发现登录能力不可用
- 首次启动自动创建主密钥文件，并把已有账号明文凭据迁移为 `enc:v1:` 密文
- 主密钥不会写入 Redis 或管理 API；必须和 Redis 数据共同备份，切勿在已有账号后更换或删除
- `data/tmp`、`debug-logs` 等目录是运行期产物，不是配置项
- 许多历史字段即使仍出现在旧配置里，也不会改变当前运行行为

## 7. 建议清理的历史字段

以下旧字段不建议继续保留在配置文件中：

- `summary_cache_*`
- `tool_call_mode`
- `warp_tool_call_mode`
- `disable_tool_filter`

## 8. 请求诊断与运维指标

- 启用 `debug_enabled` 后，新 JSON 推理请求的输入、上游交换、响应及请求事件会通过 request ID 关联到请求日志详情。凭据字段经过脱敏；请求正文和模型输出仍属于诊断内容。
- 诊断内容保存在 Redis，最多保留 24 小时、512 个请求；每个请求最多 16 个片段，每段最多 64 KiB，超出后标记截断。关闭采集不影响已保留内容的查询，重启服务也不会主动清空诊断。未采集或已过期的历史正文无法补回。
- 运维页面每 15 秒刷新。1min / 5min 使用服务端时间对齐分钟桶；空闲分钟计为零，单个有流量的分钟也会绘制。用量未上报时 TPS 显示“未采集”。新增错误分类、重试和用量统计从更新后的新请求开始积累，旧桶会提示覆盖不完整。
- CPU、主机内存和进程 RSS 从 Linux `/proc` 读取；CPU 需要两次采样，首次打开可能等待一次刷新。容器内主机指标反映 `/proc` 所见的主机范围，不代表容器配额。其他系统会显示采集不可用，Go 堆内存和协程数仍可查看。
